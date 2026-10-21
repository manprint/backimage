// Package server implements the stateless remote backup receiver.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/manprint/backimage/pkg/protocol"
	backregistry "github.com/manprint/backimage/pkg/registry"
	"github.com/manprint/backimage/pkg/transport"
)

const (
	ErrorGeneric   uint32 = 1
	ErrorUsage     uint32 = 2
	ErrorAuth      uint32 = 3
	ErrorIntegrity uint32 = 5
	ErrorNetwork   uint32 = 6
)

// BlobWriter receives exactly one layer. Commit makes it visible atomically;
// Abort releases the registry upload session without persisting local data.
type BlobWriter interface {
	io.Writer
	Commit(context.Context) error
	Abort(context.Context) error
}

// Sink is the registry-facing half of a session. Implementations must stream;
// session never requires a seekable file or stores layer bytes itself.
type Sink interface {
	KnownBlobs(context.Context, string) ([]string, error)
	BlobExists(context.Context, string, string) (bool, error)
	OpenBlob(context.Context, string, string, int64) (BlobWriter, error)
	CommitBackup(context.Context, Backup) (string, error)
}

// Layer describes a committed data layer.
type Layer struct {
	Index     uint32
	Size      int64
	Digest    string
	DiffID    string
	MediaType string
}

// Backup contains the client-built metadata needed to publish the final OCI
// index after all data layers are already present in the registry.
type Backup struct {
	SessionID string
	Start     *protocol.BackupStart
	Layers    []Layer
}

// SessionConfig contains policy, authentication, and wire timeouts.
type SessionConfig struct {
	Version      string
	AuthToken    []byte
	AllowNoAuth  bool
	AllowedRepos []string
	MaxBytes     uint64
	RateLimit    uint64
	IdleTimeout  time.Duration
	Now          func() time.Time
	Metrics      *Metrics
	// TempDir holds the per-layer spool of streaming (v2) sessions. Only the
	// layer currently being assembled is written there.
	TempDir string
	// ProgressInterval is how often a streaming session reports progress. It
	// doubles as the server keepalive, so it also bounds how long a client can
	// hear nothing while the pipeline is busy. Values below 50ms are raised to
	// it; zero selects 2s.
	ProgressInterval time.Duration
}

// Session receives one strict protocol stream.
type Session struct {
	cfg  SessionConfig
	sink Sink
}

func NewSession(cfg SessionConfig, sink Sink) (*Session, error) {
	if sink == nil {
		return nil, errors.New("server sink is required")
	}
	if len(cfg.AuthToken) == 0 && !cfg.AllowNoAuth {
		return nil, errors.New("client authentication is required")
	}
	if err := ValidateAllowedRepos(cfg.AllowedRepos); err != nil {
		return nil, err
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 120 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ProgressInterval <= 0 {
		cfg.ProgressInterval = 2 * time.Second
	}
	return &Session{cfg: cfg, sink: sink}, nil
}

// Streaming reports whether this server can run protocol v2 sessions, where
// the whole pipeline lives on the server.
func (s *Session) Streaming() bool {
	_, ok := s.sink.(StreamCommitter)
	return ok
}

type sessionState uint8

const (
	stateNew sessionState = iota
	stateGreeted
	stateReady
	stateReceiving
	stateStreaming
	stateClosed
)

type runState struct {
	state      sessionState
	version    uint32
	sessionID  string
	start      *protocol.BackupStart
	reference  string
	layers     []Layer
	current    *protocol.LayerStart
	receiver   BlobWriter
	hash       hash.Hash
	received   int64
	totalBytes uint64
	started    time.Time
	skipped    bool
	skipCount  uint32

	// streaming (protocol v2) state
	ingest    *ingest
	heartbeat *heartbeat

	// cancel stops the session context, and with it a publication the
	// client gave up on.
	cancel context.CancelFunc
}

// sessionStream serialises everything the server writes. The session loop is
// not the only writer any more: while the pipeline holds it, a heartbeat keeps
// the wire warm beside it, and a protocol message must never be interleaved
// with another halfway through its frame.
//
// It also owns the stream's deadline. While the loop waits for the client,
// the deadline it armed is authoritative: a client silent for the idle window
// is gone, whatever the heartbeat says. While the loop is busy — pushing back
// on the client, publishing — nothing is read, and every write arms its own
// idle window: the deadline left over from the last read would otherwise
// expire under a long publish and fail the heartbeat and the final BackupEnd
// of a backup already pushed. TCP streams rearm on every write and hid this;
// streams with plain deadlines, as QUIC's are, did not.
type sessionStream struct {
	transport.Stream
	idle time.Duration
	now  func() time.Time

	mu      sync.Mutex
	reading bool
}

func (s *sessionStream) send(msg *protocol.ServerMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reading && s.idle > 0 {
		if err := s.Stream.SetDeadline(s.now().Add(s.idle)); err != nil {
			return fmt.Errorf("set session deadline: %w", err)
		}
	}
	return protocol.WriteServerMessage(s.Stream, msg)
}

// armRead starts a wait for the client's next frame, which must arrive by t.
func (s *sessionStream) armRead(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reading = true
	return s.Stream.SetDeadline(t)
}

// readDone ends the wait armRead started.
func (s *sessionStream) readDone() {
	s.mu.Lock()
	s.reading = false
	s.mu.Unlock()
}

// extend arms one more idle window, as a write does, unless the loop is
// waiting for the client: that deadline stays authoritative.
func (s *sessionStream) extend() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reading || s.idle <= 0 {
		return nil
	}
	return s.Stream.SetDeadline(s.now().Add(s.idle))
}

// heartbeat reports progress on a timer while a streaming session runs. A
// layer upload can hold the receive path for longer than the client's idle
// timeout, and a client that hears nothing for that long drops the connection
// on a backup that is in fact progressing.
type heartbeat struct {
	stop chan struct{}
	done chan struct{}
}

// minHeartbeatInterval floors the timer. ProgressInterval used to throttle a
// report driven by arriving frames, where a tiny value simply meant "every
// frame"; a timer with the same value would instead flood the session.
const minHeartbeatInterval = 50 * time.Millisecond

func startHeartbeat(stream *sessionStream, in *ingest, every time.Duration) *heartbeat {
	if every < minHeartbeatInterval {
		every = minHeartbeatInterval
	}
	h := &heartbeat{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		// The first report is one interval away on purpose: a client that has
		// not entered its read loop yet must not meet an unsolicited message
		// on a transport with no buffer to park it in.
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-ticker.C:
				// A failed send is not this goroutine's to report: the
				// session loop is about to see the same broken stream.
				if err := stream.send(&protocol.ServerMessage{
					Msg: &protocol.ServerMessage_StreamProgress{StreamProgress: in.progress()},
				}); err != nil {
					_ = err
				}
			}
		}
	}()
	return h
}

func (h *heartbeat) Stop() {
	if h == nil {
		return
	}
	close(h.stop)
	<-h.done
}

// maxKeepaliveOnly bounds how long a session may consist of nothing but
// keepalives. The idle deadline cannot catch that on its own: a peer sending a
// keepalive every 30 seconds is never idle, and would hold one of the
// --max-sessions slots for as long as it cared to.
const maxKeepaliveOnly = 15 * time.Minute

// Run serves the stream until the backup commits or a protocol error occurs.
func (s *Session) Run(ctx context.Context, raw transport.Stream) error {
	stream := &sessionStream{Stream: raw, idle: s.cfg.IdleTimeout, now: s.cfg.Now}
	defer stream.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rs := &runState{state: stateNew, cancel: cancel}
	// The pipeline of a session belongs to the session, and nothing outside
	// this function will free it: the ingest goroutine holds the spool of the
	// layer it is assembling, and the blob receiver holds an upload the
	// registry is waiting to be told about. Every return used to release them
	// by hand, which held until one return did not — the rate limiter waits on
	// the context, and a cancelled context is not a protocol failure to report
	// to a client that is already gone, so it returns without going through
	// fail(). A shutdown that lands in that sleep therefore left a spool
	// behind in --work-dir, which is a directory the next server reuses.
	//
	// abort clears what it releases, so calling it here as well as on the
	// paths that already do costs nothing, and a session that completed has
	// handed both away long before this runs.
	defer func() { s.abort(ctx, rs) }()
	buf := make([]byte, 0, 64<<10)
	lastProgress := s.cfg.Now()
	for rs.state != stateClosed {
		if err := ctx.Err(); err != nil {
			s.abort(ctx, rs)
			return err
		}
		if err := stream.armRead(s.cfg.Now().Add(s.cfg.IdleTimeout)); err != nil {
			s.abort(ctx, rs)
			return fmt.Errorf("set session deadline: %w", err)
		}
		typ, payload, err := protocol.ReadFrame(stream, buf)
		stream.readDone()
		if err != nil {
			s.abort(ctx, rs)
			return err
		}
		buf = payload[:0]
		switch typ {
		case protocol.FrameKeepalive:
			if len(payload) != 0 {
				return s.fail(ctx, stream, rs, ErrorUsage, "keepalive frame must be empty", "")
			}
			if s.cfg.Now().Sub(lastProgress) > maxKeepaliveOnly {
				return s.fail(ctx, stream, rs, ErrorNetwork,
					fmt.Sprintf("no protocol progress for %s: keepalives alone do not hold a session", maxKeepaliveOnly), "")
			}
		case protocol.FrameData:
			lastProgress = s.cfg.Now()
			if err := s.data(ctx, stream, rs, payload); err != nil {
				return err
			}
		case protocol.FrameControl:
			lastProgress = s.cfg.Now()
			msg, decErr := protocol.DecodeClientMessage(payload)
			if decErr != nil {
				return s.fail(ctx, stream, rs, ErrorUsage, decErr.Error(), "")
			}
			if err := s.control(ctx, stream, rs, msg); err != nil {
				return err
			}
		default:
			return s.fail(ctx, stream, rs, ErrorUsage, "unknown frame type", "")
		}
	}
	return nil
}

func (s *Session) control(ctx context.Context, stream *sessionStream, rs *runState, msg *protocol.ClientMessage) error {
	switch m := msg.Msg.(type) {
	case *protocol.ClientMessage_Hello:
		return s.hello(ctx, stream, rs, m.Hello)
	case *protocol.ClientMessage_BackupStart:
		return s.backupStart(ctx, stream, rs, m.BackupStart)
	case *protocol.ClientMessage_LayerStart:
		return s.layerStart(ctx, stream, rs, m.LayerStart)
	case *protocol.ClientMessage_LayerEnd:
		return s.layerEnd(ctx, stream, rs, m.LayerEnd)
	case *protocol.ClientMessage_StreamStart:
		return s.streamStart(ctx, stream, rs, m.StreamStart)
	case *protocol.ClientMessage_StreamEnd:
		return s.streamEnd(ctx, stream, rs, m.StreamEnd)
	case *protocol.ClientMessage_Token:
		if rs.state != stateReady && rs.state != stateReceiving && rs.state != stateStreaming {
			return s.unexpected(ctx, stream, rs, "Token")
		}
		// The registry sink may implement TokenConsumer. Session never logs or stores it.
		if consumer, ok := s.sink.(TokenConsumer); ok {
			consumer.ProvideToken(m.Token)
		}
		return nil
	case *protocol.ClientMessage_Cancel:
		s.abort(ctx, rs)
		rs.state = stateClosed
		return fmt.Errorf("client canceled session: %s", m.Cancel.GetReason())
	case *protocol.ClientMessage_Error:
		s.abort(ctx, rs)
		rs.state = stateClosed
		return fmt.Errorf("client error: %s", m.Error.GetMessage())
	default:
		return s.fail(ctx, stream, rs, ErrorUsage, "empty client control message", "")
	}
}

// TokenConsumer accepts short-lived registry tokens without persisting them.
type TokenConsumer interface{ ProvideToken(*protocol.Token) }

// TokenRequestSource tells Session which scoped token its sink needs.
type TokenRequestSource interface {
	TokenScope(reference string) (repository string, actions []string, err error)
}

func (s *Session) hello(ctx context.Context, stream *sessionStream, rs *runState, hello *protocol.Hello) error {
	if rs.state != stateNew {
		return s.unexpected(ctx, stream, rs, "Hello")
	}
	if hello == nil || strings.TrimSpace(hello.SessionId) == "" {
		return s.fail(ctx, stream, rs, ErrorUsage, "Hello.session_id is required", "")
	}
	if !protocol.Supported(hello.ProtocolVersion) {
		return s.fail(ctx, stream, rs, ErrorUsage,
			fmt.Sprintf("incompatible protocol version: client=%d server=%d..%d", hello.ProtocolVersion, protocol.MinVersion, protocol.Version),
			"upgrade client or server")
	}
	if len(s.cfg.AuthToken) != 0 && subtle.ConstantTimeCompare([]byte(hello.AuthToken), s.cfg.AuthToken) != 1 {
		return s.fail(ctx, stream, rs, ErrorAuth, "client authentication failed", "check --auth-token")
	}
	known, err := s.sink.KnownBlobs(ctx, hello.SessionId)
	if err != nil {
		return s.fail(ctx, stream, rs, ErrorNetwork, "cannot inspect resumable blobs", err.Error())
	}
	rs.sessionID = hello.SessionId
	rs.version = hello.ProtocolVersion
	rs.state = stateGreeted
	return stream.send(&protocol.ServerMessage{Msg: &protocol.ServerMessage_HelloAck{HelloAck: &protocol.HelloAck{
		ServerVersion:       s.cfg.Version,
		ProtocolVersion:     rs.version,
		Streaming:           rs.version >= 2 && s.Streaming(),
		MaxBytes:            s.cfg.MaxBytes,
		AllowedRepoPrefixes: append([]string(nil), s.cfg.AllowedRepos...),
		Resumable:           true,
		KnownBlobDigests:    known,
	}}})
}

func (s *Session) backupStart(ctx context.Context, stream *sessionStream, rs *runState, start *protocol.BackupStart) error {
	if rs.state != stateGreeted {
		return s.unexpected(ctx, stream, rs, "BackupStart")
	}
	if start == nil || start.Reference == "" || start.LayerCount == 0 {
		return s.fail(ctx, stream, rs, ErrorUsage, "BackupStart requires reference and layer_count", "")
	}
	if !repoAllowed(start.Reference, s.cfg.AllowedRepos) {
		return s.fail(ctx, stream, rs, ErrorAuth, fmt.Sprintf("repository %q is not allowed", start.Reference), "check --allow-repo")
	}
	if s.cfg.MaxBytes > 0 && start.EstimatedBytes > s.cfg.MaxBytes {
		return s.fail(ctx, stream, rs, ErrorUsage,
			fmt.Sprintf("estimated backup size %d exceeds session quota %d", start.EstimatedBytes, s.cfg.MaxBytes), "")
	}
	// The announced layer count is the capacity of the slice two lines below,
	// and it arrives as a uint32 in a message a few dozen bytes long: a peer
	// that says 4294967295 asks this process for some 270 GB of capacity
	// before it has sent a single layer. The ceiling is not invented here —
	// it is the overlayfs budget of a runnable image, the same maxDataLayers
	// the streaming pipeline enforces while it assembles layers, and the same
	// number the client's own planner never exceeds. All that changes is
	// where it is checked: in front of the allocation instead of after it.
	if start.LayerCount > maxDataLayers {
		return s.fail(ctx, stream, rs, ErrorUsage,
			fmt.Sprintf("BackupStart announces %d layers, an image holds at most %d", start.LayerCount, maxDataLayers), "")
	}
	rs.start = start
	rs.reference = start.Reference
	rs.layers = make([]Layer, 0, start.LayerCount)
	rs.started = s.cfg.Now()
	rs.state = stateReady
	ack := &protocol.BackupAck{Ready: true}
	if source, ok := s.sink.(TokenRequestSource); ok {
		repository, actions, err := source.TokenScope(start.Reference)
		if err != nil {
			return s.fail(ctx, stream, rs, ErrorUsage, "invalid registry reference", err.Error())
		}
		ack.TokenRequest = &protocol.TokenRequest{Repository: repository, Actions: actions}
	}
	return stream.send(&protocol.ServerMessage{Msg: &protocol.ServerMessage_BackupAck{BackupAck: ack}})
}

func (s *Session) layerStart(ctx context.Context, stream *sessionStream, rs *runState, start *protocol.LayerStart) error {
	if rs.state != stateReady {
		return s.unexpected(ctx, stream, rs, "LayerStart")
	}
	if start == nil || start.Index != uint32(len(rs.layers)) || start.Size == 0 || !validDigest(start.Sha256) {
		return s.fail(ctx, stream, rs, ErrorUsage, "invalid or out-of-order LayerStart", "")
	}
	if s.cfg.MaxBytes > 0 && rs.totalBytes+start.Size > s.cfg.MaxBytes {
		return s.fail(ctx, stream, rs, ErrorUsage,
			fmt.Sprintf("session quota exceeded: %d > %d", rs.totalBytes+start.Size, s.cfg.MaxBytes), "")
	}
	exists, err := s.sink.BlobExists(ctx, rs.reference, start.Sha256)
	if err != nil {
		return s.fail(ctx, stream, rs, ErrorNetwork, "registry blob check failed", err.Error())
	}
	rs.current = start
	rs.received = 0
	rs.skipped = exists
	rs.hash = sha256.New()
	rs.state = stateReceiving
	if exists {
		rs.skipCount++
		if s.cfg.Metrics != nil {
			s.cfg.Metrics.addSkipped()
		}
	} else {
		rs.receiver, err = s.sink.OpenBlob(ctx, rs.reference, start.Sha256, int64(start.Size))
		if err != nil {
			return s.fail(ctx, stream, rs, ErrorNetwork, "registry upload start failed", err.Error())
		}
	}
	return stream.send(&protocol.ServerMessage{Msg: &protocol.ServerMessage_LayerAck{LayerAck: &protocol.LayerAck{
		Index: start.Index, Skipped: exists,
	}}})
}

func (s *Session) data(ctx context.Context, stream *sessionStream, rs *runState, payload []byte) error {
	if rs.state == stateStreaming {
		return s.streamData(ctx, stream, rs, payload)
	}
	if rs.state != stateReceiving || rs.current == nil || rs.skipped {
		return s.fail(ctx, stream, rs, ErrorUsage, "unexpected Data frame", "send LayerStart and wait for LayerAck")
	}
	remaining := int64(rs.current.Size) - rs.received
	if int64(len(payload)) > remaining {
		return s.fail(ctx, stream, rs, ErrorUsage, "layer data exceeds declared size", "")
	}
	if s.cfg.MaxBytes > 0 && rs.totalBytes+uint64(len(payload)) > s.cfg.MaxBytes {
		return s.fail(ctx, stream, rs, ErrorUsage, "session quota exceeded while receiving layer", "")
	}
	if len(payload) > 0 {
		n, err := rs.receiver.Write(payload)
		if err != nil {
			return s.fail(ctx, stream, rs, ErrorNetwork, "registry upload failed", err.Error())
		}
		if n != len(payload) {
			return s.fail(ctx, stream, rs, ErrorNetwork, "registry upload returned a short write", "")
		}
		if _, err := rs.hash.Write(payload); err != nil {
			return s.fail(ctx, stream, rs, ErrorNetwork, "layer digest update failed", err.Error())
		}
		if s.cfg.Metrics != nil {
			s.cfg.Metrics.addReceived(len(payload))
		}
	}
	rs.received += int64(len(payload))
	rs.totalBytes += uint64(len(payload))
	return s.throttle(ctx, rs)
}

// throttle enforces --rate-limit on the bytes accepted so far.
func (s *Session) throttle(ctx context.Context, rs *runState) error {
	if s.cfg.RateLimit == 0 {
		return nil
	}
	target := rs.started.Add(time.Duration(float64(rs.totalBytes) / float64(s.cfg.RateLimit) * float64(time.Second)))
	delay := target.Sub(s.cfg.Now())
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) layerEnd(ctx context.Context, stream *sessionStream, rs *runState, end *protocol.LayerEnd) error {
	if rs.state != stateReceiving || rs.current == nil {
		return s.unexpected(ctx, stream, rs, "LayerEnd")
	}
	if end == nil || end.Index != rs.current.Index || end.Digest != rs.current.Sha256 {
		return s.fail(ctx, stream, rs, ErrorUsage, "LayerEnd does not match LayerStart", "")
	}
	if !rs.skipped && rs.received != int64(rs.current.Size) {
		return s.fail(ctx, stream, rs, ErrorUsage,
			fmt.Sprintf("layer size mismatch: received=%d declared=%d", rs.received, rs.current.Size), "")
	}
	if !rs.skipped {
		got := "sha256:" + hex.EncodeToString(rs.hash.Sum(nil))
		if got != rs.current.Sha256 {
			return s.fail(ctx, stream, rs, ErrorIntegrity, "layer digest mismatch", "")
		}
	}
	if uint32(len(rs.layers))+1 == rs.start.LayerCount {
		return s.publishLastLayer(ctx, stream, rs)
	}
	if !rs.skipped {
		if err := rs.receiver.Commit(ctx); err != nil {
			return s.fail(ctx, stream, rs, ErrorNetwork, "registry upload commit failed", err.Error())
		}
		if s.cfg.Metrics != nil {
			s.cfg.Metrics.addUploaded(rs.current.Size)
		}
	}
	rs.layers = append(rs.layers, Layer{
		Index: rs.current.Index, Size: int64(rs.current.Size), Digest: rs.current.Sha256,
		DiffID: rs.current.DiffId, MediaType: rs.current.MediaType,
	})
	rs.current, rs.receiver, rs.hash = nil, nil, nil
	rs.state = stateReady
	if err := stream.send(&protocol.ServerMessage{Msg: &protocol.ServerMessage_Progress{Progress: &protocol.Progress{
		Layer: uint32(len(rs.layers) - 1), Uploaded: rs.totalBytes, Skipped: rs.skipped,
	}}}); err != nil {
		return err
	}
	return nil
}

// publishLastLayer commits the last layer and the index of a v1 session. The
// client waits for the answer, so the step runs while the session keeps
// reading it: the registry token the commit needs may be the one the client
// sends meanwhile. Of the session state the step only clears the receiver it
// committed, which the loop does not touch until the step has returned.
func (s *Session) publishLastLayer(ctx context.Context, stream *sessionStream, rs *runState) error {
	receiver, skipped := rs.receiver, rs.skipped
	size := rs.current.Size
	layers := append(append([]Layer(nil), rs.layers...), Layer{
		Index: rs.current.Index, Size: int64(rs.current.Size), Digest: rs.current.Sha256,
		DiffID: rs.current.DiffId, MediaType: rs.current.MediaType,
	})
	progress := &protocol.Progress{Layer: uint32(len(layers) - 1), Uploaded: rs.totalBytes, Skipped: skipped}
	backup := Backup{SessionID: rs.sessionID, Start: rs.start, Layers: layers}
	var digest string
	publishErr, sessionErr := s.publishWhileReading(ctx, stream, rs, func() error {
		if !skipped {
			if err := receiver.Commit(ctx); err != nil {
				return &publishFailure{kind: ErrorNetwork, message: "registry upload commit failed", err: err}
			}
			// Committed: a later failure must not abort an upload the
			// registry has already completed.
			rs.receiver = nil
			if s.cfg.Metrics != nil {
				s.cfg.Metrics.addUploaded(size)
			}
		}
		if err := stream.send(&protocol.ServerMessage{Msg: &protocol.ServerMessage_Progress{Progress: progress}}); err != nil {
			return err
		}
		var err error
		if digest, err = s.sink.CommitBackup(ctx, backup); err != nil {
			return &publishFailure{kind: commitErrorKind(err), message: "publishing OCI index failed", err: err}
		}
		return nil
	})
	if sessionErr != nil {
		return sessionErr
	}
	var failure *publishFailure
	if errors.As(publishErr, &failure) {
		return s.fail(ctx, stream, rs, failure.kind, failure.message, failure.err.Error())
	}
	if publishErr != nil {
		return publishErr
	}
	rs.layers = layers
	rs.current, rs.receiver, rs.hash = nil, nil, nil
	rs.state = stateClosed
	return stream.send(&protocol.ServerMessage{Msg: &protocol.ServerMessage_BackupEnd{BackupEnd: &protocol.BackupEnd{
		Digest: digest, BytesUploaded: rs.totalBytes, BlobsSkipped: rs.skipCount,
	}}})
}

// publishFailure is a failed publication step with the kind and message the
// session reports for it.
type publishFailure struct {
	kind    uint32
	message string
	err     error
}

func (f *publishFailure) Error() string { return f.message + ": " + f.err.Error() }
func (f *publishFailure) Unwrap() error { return f.err }

// commitErrorKind classifies a failed publish: a read-back that found the
// registry holding something else is an integrity failure, anything else is
// the registry being unreachable or refusing.
func commitErrorKind(err error) uint32 {
	if errors.Is(err, backregistry.ErrPublishedMismatch) {
		return ErrorIntegrity
	}
	return ErrorNetwork
}

func (s *Session) unexpected(ctx context.Context, stream *sessionStream, rs *runState, message string) error {
	return s.fail(ctx, stream, rs, ErrorUsage, fmt.Sprintf("unexpected %s in state %d", message, rs.state), "")
}

func (s *Session) fail(ctx context.Context, stream *sessionStream, rs *runState, kind uint32, message, hint string) error {
	s.abort(ctx, rs)
	rs.state = stateClosed
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.addError(kind)
	}
	writeErr := stream.send(&protocol.ServerMessage{Msg: &protocol.ServerMessage_Error{Error: &protocol.Error{
		Kind: kind, Message: message, Hint: hint,
	}}})
	sessionErr := &SessionError{Kind: kind, Message: message}
	if writeErr != nil {
		return errors.Join(sessionErr, fmt.Errorf("report error to client: %w", writeErr))
	}
	return sessionErr
}

func (s *Session) abort(ctx context.Context, rs *runState) {
	// The heartbeat reads the ingest: stop it before the ingest goes away.
	rs.heartbeat.Stop()
	rs.heartbeat = nil
	if rs.ingest != nil {
		rs.ingest.Abort()
		rs.ingest = nil
	}
	if rs.receiver != nil {
		if err := rs.receiver.Abort(ctx); err != nil && s.cfg.Metrics != nil {
			s.cfg.Metrics.addError(ErrorNetwork)
		}
		rs.receiver = nil
	}
}

type SessionError struct {
	Kind    uint32
	Message string
}

func (e *SessionError) Error() string { return e.Message }

// repoAllowed reports whether the reference ref names a repository under one
// of prefixes; no prefixes allow everything. A prefix covers whole path
// components: "ghcr.io/team/a" admits ghcr.io/team/a, its tags, digests and
// sub-repositories, never ghcr.io/team/abc. A prefix without a slash names a
// registry, so only a slash may follow it: "localhost" is not
// "localhost:5000".
func repoAllowed(ref string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		if prefixCovers(prefix, ref) {
			return true
		}
	}
	return false
}

func prefixCovers(prefix, ref string) bool {
	if prefix == "" || !strings.HasPrefix(ref, prefix) {
		return false
	}
	if len(ref) == len(prefix) || strings.HasSuffix(prefix, "/") {
		return true
	}
	switch ref[len(prefix)] {
	case '/':
		return true
	case ':', '@':
		// A tag or a digest only follows a repository path.
		return strings.Contains(prefix, "/")
	}
	return false
}

// ValidateAllowedRepos checks repository prefixes for --allow-repo. They are
// compared literally, so a glob would match nothing and refuse every backup,
// and an empty entry would match nothing either while reading as "any".
func ValidateAllowedRepos(prefixes []string) error {
	for _, p := range prefixes {
		if strings.TrimSpace(p) == "" {
			return errors.New("empty repository prefix")
		}
		if strings.ContainsAny(p, "*?[") {
			return fmt.Errorf("repository prefix %q contains a glob: prefixes are literal, e.g. ghcr.io/team/", p)
		}
	}
	return nil
}

func validDigest(s string) bool {
	if !strings.HasPrefix(s, "sha256:") || len(s) != len("sha256:")+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(s, "sha256:"))
	return err == nil
}

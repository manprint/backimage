// Package remote implements the backup-side protocol client.
package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/fpierri/backimage/pkg/protocol"
	"github.com/fpierri/backimage/pkg/registry"
	"github.com/fpierri/backimage/pkg/transport"
	"github.com/google/go-containerregistry/pkg/v1"
)

type Config struct {
	Dialer    transport.Dialer
	Address   string
	Version   string
	AuthToken string
	Provider  registry.Provider
	Backoffs  []time.Duration
	Keepalive time.Duration
	Now       func() time.Time
}

type Backup struct {
	Start          *protocol.BackupStart
	Layers         []v1.Layer
	ExpectedDigest string // local OCI index digest; never sent over the wire
}

type Result struct {
	Digest        string
	BytesUploaded uint64
	BlobsSkipped  uint32
	Attempts      int
}

type Error struct {
	Kind    uint32
	Message string
	Hint    string
}

func (e *Error) Error() string {
	if e.Hint != "" {
		return e.Message + ": " + e.Hint
	}
	return e.Message
}

func New(cfg Config) (*Client, error) {
	if cfg.Dialer == nil {
		return nil, errors.New("remote dialer is required")
	}
	if cfg.Address == "" {
		return nil, errors.New("remote address is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Keepalive <= 0 {
		cfg.Keepalive = 30 * time.Second
	}
	if cfg.Backoffs == nil {
		cfg.Backoffs = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	}
	return &Client{cfg: cfg}, nil
}

type Client struct{ cfg Config }

// SessionID is deterministic and contains no credentials or source paths.
func SessionID(reference string, manifestJSON []byte) string {
	payload := make([]byte, 0, len(reference)+1+len(manifestJSON))
	payload = append(payload, reference...)
	payload = append(payload, 0)
	payload = append(payload, manifestJSON...)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (c *Client) Upload(ctx context.Context, backup Backup) (Result, error) {
	var result Result
	if err := validateBackup(backup); err != nil {
		return result, err
	}
	attempts := len(c.cfg.Backoffs) + 1
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		result, last = c.uploadOnce(ctx, backup)
		result.Attempts = attempt + 1
		if last == nil {
			return result, nil
		}
		var remoteErr *Error
		if errors.As(last, &remoteErr) && remoteErr.Kind != 6 {
			return result, last
		}
		if attempt == len(c.cfg.Backoffs) {
			break
		}
		if err := sleepContext(ctx, c.cfg.Backoffs[attempt]); err != nil {
			return result, err
		}
	}
	failure := fmt.Errorf("remote backup failed after %d attempts: %w", attempts, last)
	switch c.cfg.Dialer.Name() {
	case "quic":
		return result, fmt.Errorf("%w; hint: the server may be using TCP, retry without --udp", failure)
	case "tcp":
		return result, fmt.Errorf("%w; hint: the server may be listening with --udp, retry adding --udp", failure)
	default:
		return result, failure
	}
}

type connection struct {
	client    *Client
	stream    transport.Stream
	writeMu   sync.Mutex
	asyncErr  chan error
	cancel    context.CancelFunc
	refreshMu sync.Mutex
	refresh   map[string]context.CancelFunc
}

func (c *Client) uploadOnce(ctx context.Context, backup Backup) (Result, error) {
	var result Result
	stream, err := c.cfg.Dialer.Dial(ctx, c.cfg.Address)
	if err != nil {
		return result, err
	}
	attemptCtx, cancel := context.WithCancel(ctx)
	conn := &connection{
		client: c, stream: stream, asyncErr: make(chan error, 1),
		cancel: cancel, refresh: make(map[string]context.CancelFunc),
	}
	defer conn.close()
	go conn.keepalive(attemptCtx)

	start := backup.Start
	if err := conn.writeClient(&protocol.ClientMessage{Msg: &protocol.ClientMessage_Hello{Hello: &protocol.Hello{
		ClientVersion: c.cfg.Version, ProtocolVersion: protocol.Version,
		SessionId: SessionID(start.Reference, start.ManifestJson), AuthToken: c.cfg.AuthToken,
	}}}); err != nil {
		return result, err
	}
	msg, err := conn.readServer()
	if err != nil {
		return result, err
	}
	if remoteErr := responseError(msg); remoteErr != nil {
		return result, remoteErr
	}
	ack := msg.GetHelloAck()
	if ack == nil || ack.ProtocolVersion != protocol.Version {
		return result, errors.New("remote server returned an invalid HelloAck")
	}
	if ack.MaxBytes > 0 && start.EstimatedBytes > ack.MaxBytes {
		return result, fmt.Errorf("remote quota exceeded: %d > %d", start.EstimatedBytes, ack.MaxBytes)
	}
	if err := conn.writeClient(&protocol.ClientMessage{Msg: &protocol.ClientMessage_BackupStart{BackupStart: start}}); err != nil {
		return result, err
	}
	if err := conn.waitForBackupAck(attemptCtx); err != nil {
		return result, err
	}

	for i, layer := range backup.Layers {
		digest, diffID, size, mediaType, err := layerDescriptor(layer)
		if err != nil {
			return result, fmt.Errorf("layer %d: %w", i, err)
		}
		if err := conn.writeClient(&protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerStart{LayerStart: &protocol.LayerStart{
			Index: uint32(i), Size: uint64(size), Sha256: digest,
			DiffId: diffID, MediaType: mediaType,
		}}}); err != nil {
			return result, err
		}
		layerAck, err := conn.waitForLayerAck(attemptCtx, uint32(i))
		if err != nil {
			return result, err
		}
		if !layerAck.Skipped {
			if err := conn.streamLayer(layer); err != nil {
				return result, err
			}
		}
		if err := conn.writeClient(&protocol.ClientMessage{Msg: &protocol.ClientMessage_LayerEnd{LayerEnd: &protocol.LayerEnd{
			Index: uint32(i), Digest: digest,
		}}}); err != nil {
			return result, err
		}
		if err := conn.waitForProgress(attemptCtx, uint32(i)); err != nil {
			return result, err
		}
	}
	for {
		msg, err := conn.readServer()
		if err != nil {
			return result, err
		}
		if err := conn.handleAux(attemptCtx, msg); err != nil {
			return result, err
		}
		if end := msg.GetBackupEnd(); end != nil {
			result.Digest = end.Digest
			result.BytesUploaded = end.BytesUploaded
			result.BlobsSkipped = end.BlobsSkipped
			return result, nil
		}
	}
}

func (c *connection) waitForBackupAck(ctx context.Context) error {
	for {
		msg, err := c.readServer()
		if err != nil {
			return err
		}
		if err := c.handleAux(ctx, msg); err != nil {
			return err
		}
		if ack := msg.GetBackupAck(); ack != nil {
			if !ack.Ready {
				return errors.New("remote server is not ready for layer data")
			}
			if ack.TokenRequest != nil {
				return c.provideToken(ctx, ack.TokenRequest)
			}
			return nil
		}
	}
}

func (c *connection) waitForLayerAck(ctx context.Context, index uint32) (*protocol.LayerAck, error) {
	for {
		msg, err := c.readServer()
		if err != nil {
			return nil, err
		}
		if err := c.handleAux(ctx, msg); err != nil {
			return nil, err
		}
		if ack := msg.GetLayerAck(); ack != nil {
			if ack.Index != index {
				return nil, fmt.Errorf("remote LayerAck index %d, want %d", ack.Index, index)
			}
			return ack, nil
		}
	}
}

func (c *connection) waitForProgress(ctx context.Context, index uint32) error {
	for {
		msg, err := c.readServer()
		if err != nil {
			return err
		}
		if err := c.handleAux(ctx, msg); err != nil {
			return err
		}
		if progress := msg.GetProgress(); progress != nil {
			if progress.Layer != index {
				return fmt.Errorf("remote progress layer %d, want %d", progress.Layer, index)
			}
			return nil
		}
	}
}

func (c *connection) handleAux(ctx context.Context, msg *protocol.ServerMessage) error {
	if remoteErr := responseError(msg); remoteErr != nil {
		return remoteErr
	}
	if request := msg.GetTokenRequest(); request != nil {
		return c.provideToken(ctx, request)
	}
	select {
	case err := <-c.asyncErr:
		return err
	default:
		return nil
	}
}

func (c *connection) provideToken(ctx context.Context, request *protocol.TokenRequest) error {
	if c.client.cfg.Provider == nil {
		return errors.New("remote server requested registry credentials, but no token provider is configured")
	}
	scope := registry.Scope{Repository: request.Repository, Actions: append([]string(nil), request.Actions...)}
	token, err := c.client.cfg.Provider.Get(ctx, scope)
	if err != nil {
		return err
	}
	if err := c.sendToken(token, scope); err != nil {
		return err
	}
	key := scope.String()
	c.refreshMu.Lock()
	if old := c.refresh[key]; old != nil {
		old()
	}
	refreshCtx, cancel := context.WithCancel(ctx)
	c.refresh[key] = cancel
	c.refreshMu.Unlock()
	go c.refreshToken(refreshCtx, scope, token)
	return nil
}

func (c *connection) refreshToken(ctx context.Context, scope registry.Scope, token *registry.Token) {
	for {
		remaining := token.ExpiresAt.Sub(c.client.cfg.Now())
		if remaining <= 0 {
			c.sendAsync(errors.New("registry token expired before refresh"))
			return
		}
		if err := sleepContext(ctx, remaining*3/5); err != nil {
			return
		}
		c.client.cfg.Provider.Invalidate(scope)
		fresh, err := c.client.cfg.Provider.Get(ctx, scope)
		if err != nil {
			c.sendAsync(err)
			return
		}
		if err := c.sendToken(fresh, scope); err != nil {
			c.sendAsync(err)
			return
		}
		token = fresh
	}
}

func (c *connection) sendToken(token *registry.Token, scope registry.Scope) error {
	if token == nil || (token.Value == "" && !token.Anonymous()) || token.ExpiresAt.IsZero() {
		return errors.New("registry provider returned an invalid token")
	}
	return c.writeClient(&protocol.ClientMessage{Msg: &protocol.ClientMessage_Token{Token: &protocol.Token{
		Value: token.Value, ExpiresAtUnix: token.ExpiresAt.Unix(),
		Repository: scope.Repository, Actions: append([]string(nil), scope.Actions...), Anonymous: token.Anonymous(),
	}}})
}

func (c *connection) streamLayer(layer v1.Layer) error {
	r, err := layer.Compressed()
	if err != nil {
		return err
	}
	defer r.Close()
	buf := make([]byte, protocol.MaxFrameSize)
	for {
		n, readErr := io.ReadFull(r, buf)
		if n > 0 {
			c.writeMu.Lock()
			err = protocol.WriteFrame(c.stream, protocol.FrameData, buf[:n])
			c.writeMu.Unlock()
			if err != nil {
				return err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				return nil
			}
			return readErr
		}
	}
}

func (c *connection) keepalive(ctx context.Context) {
	ticker := time.NewTicker(c.client.cfg.Keepalive)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.writeMu.Lock()
			err := protocol.WriteFrame(c.stream, protocol.FrameKeepalive, nil)
			c.writeMu.Unlock()
			if err != nil {
				c.sendAsync(err)
				return
			}
		}
	}
}

func (c *connection) writeClient(msg *protocol.ClientMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return protocol.WriteClientMessage(c.stream, msg)
}

func (c *connection) readServer() (*protocol.ServerMessage, error) {
	typ, payload, err := protocol.ReadFrame(c.stream, nil)
	if err != nil {
		return nil, err
	}
	if typ != protocol.FrameControl {
		return nil, fmt.Errorf("unexpected server frame type %d", typ)
	}
	return protocol.DecodeServerMessage(payload)
}

func (c *connection) sendAsync(err error) {
	select {
	case c.asyncErr <- err:
	default:
	}
}

func (c *connection) close() {
	c.cancel()
	c.refreshMu.Lock()
	for _, cancel := range c.refresh {
		cancel()
	}
	c.refreshMu.Unlock()
	if err := c.stream.Close(); err != nil {
		c.sendAsync(err)
	}
	if closer, ok := c.stream.(transport.ConnectionCloser); ok {
		if err := closer.CloseConnection(); err != nil {
			c.sendAsync(err)
		}
	}
}

func responseError(msg *protocol.ServerMessage) error {
	if wire := msg.GetError(); wire != nil {
		return &Error{Kind: wire.Kind, Message: wire.Message, Hint: wire.Hint}
	}
	return nil
}

func layerDescriptor(layer v1.Layer) (string, string, int64, string, error) {
	if layer == nil {
		return "", "", 0, "", errors.New("nil layer")
	}
	digest, err := layer.Digest()
	if err != nil {
		return "", "", 0, "", err
	}
	diffID, err := layer.DiffID()
	if err != nil {
		return "", "", 0, "", err
	}
	size, err := layer.Size()
	if err != nil {
		return "", "", 0, "", err
	}
	mediaType, err := layer.MediaType()
	if err != nil {
		return "", "", 0, "", err
	}
	return digest.String(), diffID.String(), size, string(mediaType), nil
}

func validateBackup(backup Backup) error {
	if backup.Start == nil || backup.Start.Reference == "" || len(backup.Start.ManifestJson) == 0 {
		return errors.New("remote backup requires reference and manifest")
	}
	if len(backup.Layers) == 0 || backup.Start.LayerCount != uint32(len(backup.Layers)) {
		return errors.New("remote layer_count does not match layers")
	}
	return nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
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

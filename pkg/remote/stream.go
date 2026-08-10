package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/fpierri/backimage/pkg/protocol"
)

// StreamFrameSize is the payload of one data frame. It bounds client memory:
// a streaming backup never buffers more than this plus the archiver window.
const StreamFrameSize = 1 << 20

// StreamBackup is a protocol v2 upload: the client ships the raw archive and
// the server performs chunking, compression, encryption and the push.
type StreamBackup struct {
	Start *protocol.StreamStart
	// Source writes the raw tar stream. It may be called more than once, one
	// call per connection attempt, and must produce the archive from scratch.
	Source func(context.Context, io.Writer) error
	// Progress receives the server-side stage updates, if set.
	Progress func(*protocol.StreamProgress)
}

// ErrNoStreaming reports a peer that only speaks the layer-by-layer protocol.
var ErrNoStreaming = errors.New("the remote server does not support server-side streaming (protocol v2)")

// UploadStream runs one streaming backup, retrying the whole stream on
// network failures. Nothing is spooled locally between attempts.
func (c *Client) UploadStream(ctx context.Context, backup StreamBackup) (Result, error) {
	if backup.Start == nil || backup.Start.Reference == "" {
		return Result{}, errors.New("remote stream requires a reference")
	}
	if backup.Source == nil {
		return Result{}, errors.New("remote stream requires an archive source")
	}
	return c.withRetries(ctx, func(attemptCtx context.Context) (Result, error) {
		return c.uploadStreamOnce(attemptCtx, backup)
	})
}

func (c *Client) uploadStreamOnce(ctx context.Context, backup StreamBackup) (Result, error) {
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
		SessionId: SessionID(start.Reference, []byte(start.GetToolVersion())), AuthToken: c.cfg.AuthToken,
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
	if ack == nil {
		return result, errors.New("remote server returned an invalid HelloAck")
	}
	if !ack.Streaming || ack.ProtocolVersion < 2 {
		// Not a transient condition: fail without burning the retry budget.
		return result, &Error{Kind: 2, Message: ErrNoStreaming.Error(), Hint: "upgrade the server or use --remote-mode layers"}
	}
	if ack.MaxBytes > 0 && start.EstimatedBytes > ack.MaxBytes {
		return result, &Error{Kind: 2, Message: fmt.Sprintf("remote quota exceeded: %d > %d", start.EstimatedBytes, ack.MaxBytes)}
	}
	if err := conn.writeClient(&protocol.ClientMessage{Msg: &protocol.ClientMessage_StreamStart{StreamStart: start}}); err != nil {
		return result, err
	}

	reader := newStreamReader(conn, attemptCtx, backup.Progress)
	defer reader.stop()
	if err := reader.waitReady(); err != nil {
		return result, err
	}

	writer := &streamWriter{conn: conn, reader: reader}
	if err := backup.Source(attemptCtx, writer); err != nil {
		// A server-side failure surfaces here as a broken pipe: prefer the
		// remote cause when the reader already captured one.
		if remoteErr := reader.failure(); remoteErr != nil {
			return result, remoteErr
		}
		return result, err
	}
	if err := conn.writeClient(&protocol.ClientMessage{Msg: &protocol.ClientMessage_StreamEnd{StreamEnd: &protocol.StreamEnd{
		RawBytes: writer.written,
	}}}); err != nil {
		if remoteErr := reader.failure(); remoteErr != nil {
			return result, remoteErr
		}
		return result, err
	}
	end, err := reader.waitEnd()
	if err != nil {
		return result, err
	}
	return Result{
		Digest:        end.Digest,
		BytesUploaded: end.BytesUploaded,
		BlobsSkipped:  end.BlobsSkipped,
		RawBytes:      end.RawBytes,
		StoredBytes:   end.StoredBytes,
		Layers:        end.Layers,
		Chunks:        end.Chunks,
		Files:         end.Files,
	}, nil
}

// withRetries repeats fn while the failure looks transient. The caller must be
// able to regenerate its payload: nothing is kept between attempts.
func (c *Client) withRetries(ctx context.Context, fn func(context.Context) (Result, error)) (Result, error) {
	attempts := len(c.cfg.Backoffs) + 1
	var result Result
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		result, last = fn(ctx)
		result.Attempts = attempt + 1
		if last == nil {
			return result, nil
		}
		var remoteErr *Error
		if errors.As(last, &remoteErr) && remoteErr.Kind != 6 {
			return result, last
		}
		// Only the caller's cancellation stops the loop. A transport-internal
		// deadline (a QUIC handshake against a TCP-only server, for instance)
		// is exactly the failure the retry and the crossed-transport hint
		// exist for.
		if ctx.Err() != nil {
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

// streamReader consumes server messages while the archive is being sent:
// progress, token requests, errors and the final result.
type streamReader struct {
	conn     *connection
	ctx      context.Context
	progress func(*protocol.StreamProgress)

	ready chan struct{}
	end   chan *protocol.BackupEnd
	done  chan struct{}

	mu  sync.Mutex
	err error
}

func newStreamReader(conn *connection, ctx context.Context, progress func(*protocol.StreamProgress)) *streamReader {
	r := &streamReader{
		conn: conn, ctx: ctx, progress: progress,
		ready: make(chan struct{}), end: make(chan *protocol.BackupEnd, 1), done: make(chan struct{}),
	}
	go r.run()
	return r
}

func (r *streamReader) run() {
	defer close(r.done)
	readyClosed := false
	for {
		msg, err := r.conn.readServer()
		if err != nil {
			r.setErr(err)
			if !readyClosed {
				close(r.ready)
			}
			return
		}
		if remoteErr := responseError(msg); remoteErr != nil {
			r.setErr(remoteErr)
			if !readyClosed {
				close(r.ready)
			}
			return
		}
		switch {
		case msg.GetStreamAck() != nil:
			ack := msg.GetStreamAck()
			if !ack.Ready {
				r.setErr(errors.New("remote server is not ready to receive the stream"))
			} else if ack.TokenRequest != nil {
				if err := r.conn.provideToken(r.ctx, ack.TokenRequest); err != nil {
					r.setErr(err)
				}
			}
			if !readyClosed {
				readyClosed = true
				close(r.ready)
			}
			if r.failure() != nil {
				return
			}
		case msg.GetTokenRequest() != nil:
			if err := r.conn.provideToken(r.ctx, msg.GetTokenRequest()); err != nil {
				r.setErr(err)
				return
			}
		case msg.GetStreamProgress() != nil:
			if r.progress != nil {
				r.progress(msg.GetStreamProgress())
			}
		case msg.GetBackupEnd() != nil:
			r.end <- msg.GetBackupEnd()
			return
		}
	}
}

func (r *streamReader) waitReady() error {
	select {
	case <-r.ready:
		return r.failure()
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

func (r *streamReader) waitEnd() (*protocol.BackupEnd, error) {
	select {
	case end := <-r.end:
		return end, nil
	case <-r.done:
		if err := r.failure(); err != nil {
			return nil, err
		}
		select {
		case end := <-r.end:
			return end, nil
		default:
			return nil, errors.New("remote server closed the session without a result")
		}
	case <-r.ctx.Done():
		return nil, r.ctx.Err()
	}
}

func (r *streamReader) setErr(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
}

func (r *streamReader) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *streamReader) stop() { r.conn.cancel() }

// streamWriter frames the archive bytes produced by the local archiver.
type streamWriter struct {
	conn    *connection
	reader  *streamReader
	written uint64
}

func (w *streamWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		if err := w.reader.failure(); err != nil {
			return total, err
		}
		n := len(p)
		if n > StreamFrameSize {
			n = StreamFrameSize
		}
		w.conn.writeMu.Lock()
		err := protocol.WriteFrame(w.conn.stream, protocol.FrameData, p[:n])
		w.conn.writeMu.Unlock()
		if err != nil {
			if remoteErr := w.reader.failure(); remoteErr != nil {
				return total, remoteErr
			}
			return total, err
		}
		w.written += uint64(n)
		total += n
		p = p[n:]
	}
	return total, nil
}

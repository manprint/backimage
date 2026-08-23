package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/manprint/backimage/pkg/protocol"
)

// streamStart opens a protocol v2 session. From here on the client only sends
// raw archive bytes: chunking, compression, encryption, layer assembly and the
// registry push all happen on this side.
func (s *Session) streamStart(ctx context.Context, stream *sessionStream, rs *runState, start *protocol.StreamStart) error {
	if rs.state != stateGreeted {
		return s.unexpected(ctx, stream, rs, "StreamStart")
	}
	if rs.version < 2 {
		return s.fail(ctx, stream, rs, ErrorUsage, "StreamStart requires protocol version 2", "upgrade the client")
	}
	if !s.Streaming() {
		return s.fail(ctx, stream, rs, ErrorUsage, "this server cannot publish streamed backups", "")
	}
	if start == nil || strings.TrimSpace(start.Reference) == "" {
		return s.fail(ctx, stream, rs, ErrorUsage, "StreamStart requires a reference", "")
	}
	if !repoAllowed(start.Reference, s.cfg.AllowedRepos) {
		return s.fail(ctx, stream, rs, ErrorAuth, fmt.Sprintf("repository %q is not allowed", start.Reference), "check --allow-repo")
	}
	if s.cfg.MaxBytes > 0 && start.EstimatedBytes > s.cfg.MaxBytes {
		return s.fail(ctx, stream, rs, ErrorUsage,
			fmt.Sprintf("estimated backup size %d exceeds session quota %d", start.EstimatedBytes, s.cfg.MaxBytes), "")
	}
	rs.reference = start.Reference
	rs.started = s.cfg.Now()
	ack := &protocol.StreamAck{Ready: true}
	if source, ok := s.sink.(TokenRequestSource); ok {
		repository, actions, err := source.TokenScope(start.Reference)
		if err != nil {
			return s.fail(ctx, stream, rs, ErrorUsage, "invalid registry reference", err.Error())
		}
		ack.TokenRequest = &protocol.TokenRequest{Repository: repository, Actions: actions}
	}
	in, err := startIngest(ctx, ingestConfig{
		Start:     start,
		SessionID: rs.sessionID,
		Reference: start.Reference,
		TempDir:   s.cfg.TempDir,
		MaxBytes:  s.cfg.MaxBytes,
		Sink:      s.sink,
		Metrics:   s.cfg.Metrics,
		Now:       s.cfg.Now,
	})
	if err != nil {
		return s.fail(ctx, stream, rs, ErrorUsage, "cannot start the server pipeline", err.Error())
	}
	rs.ingest = in
	rs.state = stateStreaming
	if err := stream.send(&protocol.ServerMessage{Msg: &protocol.ServerMessage_StreamAck{StreamAck: ack}}); err != nil {
		return err
	}
	// Only now, after the ack: the first report is one interval away, so a
	// client that has not started reading yet meets no unsolicited message.
	rs.heartbeat = startHeartbeat(stream, in, s.cfg.ProgressInterval)
	return nil
}

// streamData feeds one received frame into the pipeline. The write blocks
// while the pipeline is busy, which is exactly the back-pressure that keeps
// server memory bounded.
func (s *Session) streamData(ctx context.Context, stream *sessionStream, rs *runState, payload []byte) error {
	if rs.ingest == nil {
		return s.fail(ctx, stream, rs, ErrorUsage, "unexpected Data frame", "send StreamStart first")
	}
	if s.cfg.MaxBytes > 0 && rs.totalBytes+uint64(len(payload)) > s.cfg.MaxBytes {
		return s.fail(ctx, stream, rs, ErrorUsage, "session quota exceeded while receiving the stream", "")
	}
	if len(payload) > 0 {
		if err := rs.ingest.Write(payload); err != nil {
			return s.fail(ctx, stream, rs, pipelineErrorKind(err), "server pipeline failed", err.Error())
		}
		if s.cfg.Metrics != nil {
			s.cfg.Metrics.addReceived(len(payload))
		}
	}
	rs.totalBytes += uint64(len(payload))
	// Progress is reported by the heartbeat, on a timer: driven from here it
	// would go silent exactly when the pipeline is slow and the client most
	// needs to hear that the session is alive.
	return s.throttle(ctx, rs)
}

// streamEnd waits for the pipeline to drain and publishes the image.
func (s *Session) streamEnd(ctx context.Context, stream *sessionStream, rs *runState, end *protocol.StreamEnd) error {
	if rs.state != stateStreaming || rs.ingest == nil {
		return s.unexpected(ctx, stream, rs, "StreamEnd")
	}
	if end != nil && end.RawBytes > 0 && end.RawBytes != rs.totalBytes {
		return s.fail(ctx, stream, rs, ErrorIntegrity,
			fmt.Sprintf("stream size mismatch: received=%d declared=%d", rs.totalBytes, end.RawBytes), "")
	}
	in := rs.ingest
	// Finish waits for the pipeline to drain, so the heartbeat has to outlive
	// it: publishing a large backup is the longest silence of the session.
	result, err := in.Finish()
	rs.heartbeat.Stop()
	rs.heartbeat = nil
	rs.ingest = nil
	if err != nil {
		return s.fail(ctx, stream, rs, pipelineErrorKind(err), "server pipeline failed", err.Error())
	}
	// Uploaded bytes are already counted per layer by the pipeline.
	rs.state = stateClosed
	// A closing report, so a session shorter than one heartbeat interval still
	// tells the client what the server did. Safe to send unsolicited here, and
	// only here: the client is already blocked reading for BackupEnd.
	if err := stream.send(&protocol.ServerMessage{
		Msg: &protocol.ServerMessage_StreamProgress{StreamProgress: in.progress()},
	}); err != nil {
		return err
	}
	return stream.send(&protocol.ServerMessage{Msg: &protocol.ServerMessage_BackupEnd{BackupEnd: &protocol.BackupEnd{
		Digest:        result.Digest,
		BytesUploaded: result.UploadedBytes,
		BlobsSkipped:  result.LayersSkipped,
		RawBytes:      result.RawBytes,
		StoredBytes:   result.StoredBytes,
		Layers:        result.Layers,
		Chunks:        result.Chunks,
		Files:         result.Files,
	}}})
}

func pipelineErrorKind(err error) uint32 {
	switch {
	case errors.Is(err, errStreamAborted):
		return ErrorNetwork
	case strings.Contains(err.Error(), "registry"):
		return ErrorNetwork
	case strings.Contains(err.Error(), "quota"):
		return ErrorUsage
	default:
		return ErrorGeneric
	}
}

package protocol

import (
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

// WriteClientMessage encodes a client control message in a FrameControl frame.
func WriteClientMessage(w io.Writer, msg *ClientMessage) error {
	return writeControl(w, msg)
}

// WriteServerMessage encodes a server control message in a FrameControl frame.
func WriteServerMessage(w io.Writer, msg *ServerMessage) error {
	return writeControl(w, msg)
}

func writeControl(w io.Writer, msg proto.Message) error {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal control message: %w", err)
	}
	return WriteFrame(w, FrameControl, payload)
}

// DecodeClientMessage decodes a previously read FrameControl payload.
func DecodeClientMessage(payload []byte) (*ClientMessage, error) {
	msg := new(ClientMessage)
	if err := proto.Unmarshal(payload, msg); err != nil {
		return nil, fmt.Errorf("decode client message: %w", err)
	}
	return msg, nil
}

// DecodeServerMessage decodes a previously read FrameControl payload.
func DecodeServerMessage(payload []byte) (*ServerMessage, error) {
	msg := new(ServerMessage)
	if err := proto.Unmarshal(payload, msg); err != nil {
		return nil, fmt.Errorf("decode server message: %w", err)
	}
	return msg, nil
}

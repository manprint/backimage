package protocol

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestControlMessageRoundTrip(t *testing.T) {
	client := &ClientMessage{Msg: &ClientMessage_Hello{Hello: &Hello{
		ClientVersion:   "v1",
		ProtocolVersion: Version,
		SessionId:       "session",
		AuthToken:       "secret",
	}}}
	var wire bytes.Buffer
	if err := WriteClientMessage(&wire, client); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := ReadFrame(&wire, nil)
	if err != nil {
		t.Fatal(err)
	}
	if typ != FrameControl {
		t.Fatalf("type = %d", typ)
	}
	gotClient, err := DecodeClientMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(gotClient, client) {
		t.Fatalf("client round trip = %v", gotClient)
	}

	server := &ServerMessage{Msg: &ServerMessage_HelloAck{HelloAck: &HelloAck{
		ServerVersion:    "v1",
		ProtocolVersion:  Version,
		Resumable:        true,
		KnownBlobDigests: []string{"sha256:a"},
	}}}
	wire.Reset()
	if err := WriteServerMessage(&wire, server); err != nil {
		t.Fatal(err)
	}
	_, payload, err = ReadFrame(&wire, payload[:0])
	if err != nil {
		t.Fatal(err)
	}
	gotServer, err := DecodeServerMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(gotServer, server) {
		t.Fatalf("server round trip = %v", gotServer)
	}
}

func TestControlRejectsMalformedProtobuf(t *testing.T) {
	if _, err := DecodeClientMessage([]byte{0xff}); err == nil {
		t.Fatal("malformed client message accepted")
	}
	if _, err := DecodeServerMessage([]byte{0xff}); err == nil {
		t.Fatal("malformed server message accepted")
	}
}

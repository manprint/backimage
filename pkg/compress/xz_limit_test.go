package compress

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"math/rand"
	"runtime"
	"strings"
	"testing"
)

func xzEncode(t *testing.T, data []byte) []byte {
	t.Helper()
	c, err := ByID(Xz)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := c.NewWriter(&buf, 6)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withDictionaryByte rewrites the LZMA2 dictionary-size byte of the first
// block header and fixes the header CRC, so the stream stays valid.
func withDictionaryByte(t *testing.T, stream []byte, dict byte) []byte {
	t.Helper()
	b := append([]byte(nil), stream...)
	hs := (int(b[12]) + 1) * 4
	h := b[12 : 12+hs]
	i := bytes.Index(h, []byte{0x21, 0x01})
	if i < 0 {
		t.Fatalf("no LZMA2 filter in % x", h)
	}
	h[i+2] = dict
	binary.LittleEndian.PutUint32(h[hs-4:], crc32.ChecksumIEEE(h[:hs-4]))
	return b
}

func xzDecode(stream []byte) ([]byte, error) {
	c, err := ByID(Xz)
	if err != nil {
		return nil, err
	}
	r, err := c.NewReader(bytes.NewReader(stream))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// TestXzRefusesADictionaryNoWriterOfOursAsksFor is the regression for the
// memory bomb: the decoder sized its dictionary from the block header, up to
// 4 GiB, and allocated it before producing a byte. A 72-byte chunk of an
// unencrypted backup — anyone can publish one — cost 4 GiB per decode, per
// worker; the plaintext cap on the output comes too late to help.
func TestXzRefusesADictionaryNoWriterOfOursAsksFor(t *testing.T) {
	hostile := withDictionaryByte(t, xzEncode(t, []byte("hello hello hello")), 40) // 4 GiB - 1
	if len(hostile) > 128 {
		t.Fatalf("the fixture must be cheap to publish, it is %d bytes", len(hostile))
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := xzDecode(hostile)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "dictionary") {
		t.Fatalf("decode of a 4 GiB dictionary = %v, want a refusal naming the dictionary", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Fatalf("the refusal still allocated %d MiB", grew>>20)
	}
}

// TestXzDictionaryLimitIsInclusive: the largest dictionary any xz preset
// uses still decodes; one step above it does not.
func TestXzDictionaryLimitIsInclusive(t *testing.T) {
	stream := xzEncode(t, []byte("payload"))
	// 28 encodes 64 MiB, 29 encodes 96 MiB.
	if out, err := xzDecode(withDictionaryByte(t, stream, 28)); err != nil || string(out) != "payload" {
		t.Fatalf("64 MiB dictionary: %q, %v", out, err)
	}
	if _, err := xzDecode(withDictionaryByte(t, stream, 29)); err == nil {
		t.Fatal("a 96 MiB dictionary was accepted")
	}
}

// TestXzGuardKeepsEveryStreamOurWriterProduces: the guard parses the framing
// the decoder is about to read, so it must agree with the writer on every
// shape it emits — empty, a single literal chunk, incompressible data split
// into many uncompressed LZMA2 chunks, compressible data across many LZMA
// chunks — and on any read pattern of the decoder.
func TestXzGuardKeepsEveryStreamOurWriterProduces(t *testing.T) {
	random := make([]byte, 3<<20)
	rand.New(rand.NewSource(1)).Read(random)
	mixed := append(bytes.Repeat([]byte("abcdefgh"), 1<<18), random[:1<<20]...)
	for name, data := range map[string][]byte{
		"empty":          {},
		"one byte":       {7},
		"text":           []byte(strings.Repeat("the quick brown fox ", 1000)),
		"incompressible": random,
		"mixed":          mixed,
	} {
		t.Run(name, func(t *testing.T) {
			stream := xzEncode(t, data)
			out, err := xzDecode(stream)
			if err != nil || !bytes.Equal(out, data) {
				t.Fatalf("round trip: %d bytes back of %d, %v", len(out), len(data), err)
			}
			// The same stream read one byte at a time: the guard sees every
			// field split across reads.
			c, _ := ByID(Xz)
			r, err := c.NewReader(oneByteReader{bytes.NewReader(stream)})
			if err != nil {
				t.Fatal(err)
			}
			out, err = io.ReadAll(r)
			if err != nil || !bytes.Equal(out, data) {
				t.Fatalf("byte-at-a-time: %d bytes back of %d, %v", len(out), len(data), err)
			}
			// Two concatenated streams with stream padding, as xz allows.
			double := append(append(append([]byte(nil), stream...), 0, 0, 0, 0), stream...)
			out, err = xzDecode(double)
			if err != nil || !bytes.Equal(out, append(append([]byte(nil), data...), data...)) {
				t.Fatalf("two streams: %d bytes back, %v", len(out), err)
			}
		})
	}
}

type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return o.r.Read(p)
}

// FuzzXzGuard: whatever the input, the guarded decoder fails cleanly or
// decodes; it never panics.
func FuzzXzGuard(f *testing.F) {
	c, _ := ByID(Xz)
	var buf bytes.Buffer
	w, _ := c.NewWriter(&buf, 6)
	_, _ = w.Write([]byte("seed seed seed"))
	_ = w.Close()
	f.Add(buf.Bytes())
	f.Add([]byte{0xfd, '7', 'z', 'X', 'Z', 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := c.NewReader(bytes.NewReader(data))
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(r, 1<<20))
	})
}

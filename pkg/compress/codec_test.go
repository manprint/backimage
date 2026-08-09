package compress

import (
	"bytes"
	"io"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestRegistryGetUnknown(t *testing.T) {
	_, err := Get("nonesiste")
	if err == nil {
		t.Fatal("unknown codec must error")
	}
	if !strings.Contains(err.Error(), "zstd") {
		t.Fatalf("error must list available names: %v", err)
	}
	if !errorsAsUsage(err) {
		t.Fatalf("unknown codec must be a usage error: %v", err)
	}
}

func errorsAsUsage(err error) bool {
	u, ok := err.(interface{ UsageError() bool })
	return ok && u.UsageError()
}

func TestRegistryByID(t *testing.T) {
	if _, err := ByID(42); err == nil {
		t.Fatal("unknown id must error")
	}
	for _, name := range Names() {
		c, err := Get(name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ByID(c.ID())
		if err != nil {
			t.Fatal(err)
		}
		if got.Name() != name {
			t.Fatalf("ByID(%d) = %s, want %s", c.ID(), got.Name(), name)
		}
	}
}

func TestRegistryNamesSortedAndComplete(t *testing.T) {
	names := Names()
	want := []string{"gzip", "lz4", "store", "xz", "zstd"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("Names() = %v, want %v", names, want)
	}
}

func TestRegistryDuplicatePanics(t *testing.T) {
	c, err := Get("store")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register must panic")
		}
	}()
	Register(c)
}

func TestMediaTypeSuffixes(t *testing.T) {
	if got := mustGet(t, "gzip").MediaTypeSuffix(); got != "gzip" {
		t.Fatalf("gzip suffix = %q", got)
	}
	if got := mustGet(t, "zstd").MediaTypeSuffix(); got != "zstd" {
		t.Fatalf("zstd suffix = %q", got)
	}
	if got := mustGet(t, "xz").MediaTypeSuffix(); got != "" {
		t.Fatalf("xz suffix must be empty, got %q", got)
	}
	if got := mustGet(t, "lz4").MediaTypeSuffix(); got != "" {
		t.Fatalf("lz4 suffix must be empty, got %q", got)
	}
	if got := mustGet(t, "store").MediaTypeSuffix(); got != "none" {
		t.Fatalf("store suffix must be \"none\", got %q", got)
	}
}

func mustGet(t *testing.T, name string) Codec {
	t.Helper()
	c, err := Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func archive(t *testing.T, c Codec, level int, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := c.NewWriter(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

func unarchive(t *testing.T, c Codec, compressed []byte) []byte {
	t.Helper()
	r, err := c.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func makeCorpus(seed int64, size int) []byte {
	r := rand.New(rand.NewSource(seed))
	data := make([]byte, size)
	r.Read(data)
	return data
}

func TestRoundTripTable(t *testing.T) {
	inputs := map[string][]byte{
		"empty":         {},
		"one-byte":      {0x61},
		"random-1MiB":   makeCorpus(1, 1<<20),
		"zeros-1MiB":    make([]byte, 1<<20),
		"repeated-8MiB": bytes.Repeat([]byte("Lorem ipsum dolor sit amet, consectetur adipiscing elit. "), 1<<17),
	}
	for _, name := range Names() {
		c := mustGet(t, name)
		min, max, def := c.Levels()
		levels := []int{min, max}
		if min != max {
			levels = append(levels, def)
		}
		for _, level := range levels {
			for inName, in := range inputs {
				t.Run(name+"-l"+strconv.Itoa(level)+"-"+inName, func(t *testing.T) {
					compressed := archive(t, c, level, in)
					out := unarchive(t, c, compressed)
					if !bytes.Equal(out, in) {
						t.Fatalf("round-trip mismatch: %d bytes in, %d out", len(in), len(out))
					}
				})
			}
		}
	}
}

func TestLevelOutOfRange(t *testing.T) {
	for _, name := range Names() {
		c := mustGet(t, name)
		_, max, _ := c.Levels()
		if _, err := c.NewWriter(io.Discard, max+1); err == nil {
			t.Fatalf("%s: level %d must error", name, max+1)
		} else if !strings.Contains(err.Error(), strconv.Itoa(max)) {
			t.Fatalf("%s: error must mention the range: %v", name, err)
		}
		if min, _, _ := c.Levels(); min > 0 {
			if _, err := c.NewWriter(io.Discard, min-1); err == nil {
				t.Fatalf("%s: level %d must error", name, min-1)
			}
		}
	}
}

type closingTracker struct {
	bytes.Buffer
	closed bool
}

func (ct *closingTracker) Close() error {
	ct.closed = true
	return nil
}

func TestCloseDoesNotCloseUnderlying(t *testing.T) {
	for _, name := range []string{"gzip", "zstd", "xz", "lz4", "store"} {
		c := mustGet(t, name)
		_, _, def := c.Levels()
		if name == "store" {
			def = 0
		}
		ct := &closingTracker{}
		w, err := c.NewWriter(ct, def)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("x"))
		if err := w.Close(); err != nil {
			t.Fatalf("%s: close: %v", name, err)
		}
		if ct.closed {
			t.Fatalf("%s: Close closed the underlying writer", name)
		}
		if name != "store" {
			if got := unarchive(t, c, ct.Bytes()); !bytes.Equal(got, []byte("x")) {
				t.Fatalf("%s: closed stream not readable", name)
			}
		}
	}
}

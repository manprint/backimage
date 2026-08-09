package compress

import (
	"bytes"
	"io"
	"math/rand"
	"sort"
	"sync"
	"testing"
)

// The three corpora are synthesized once and shared by all benchmarks. Seeds
// are fixed so numbers are reproducible across machines.
const corpusSize = 256 << 20 // 256 MiB each

var corporaOnce sync.Once
var corpora map[string][]byte

func benchCorpora() map[string][]byte {
	corporaOnce.Do(func() {
		corpora = buildCorpora()
	})
	return corpora
}

func buildCorpora() map[string][]byte {
	cm := map[string][]byte{}
	text := make([]byte, corpusSize)
	for i := 0; i < len(text); i += 80 {
		copy(text[i:], []byte("2026-07-08 12:41:07 INFO node=7 op=chunk layer=12 took=184ms sha=6f1c8f2b\n"))
	}
	cm["texto"] = text

	mix := make([]byte, corpusSize)
	r := rand.New(rand.NewSource(2))
	for i := 0; i < len(mix); i += 4096 {
		r.Read(mix[i : i+2048])
		copy(mix[i+2048:], make([]byte, 2048))
	}
	cm["binario"] = mix

	rr := rand.New(rand.NewSource(3))
	raw := make([]byte, corpusSize)
	rr.Read(raw)
	cm["incomprimibile"] = raw
	return cm
}

func levelSet(c Codec) []int {
	min, max, def := c.Levels()
	set := map[int]bool{min: true, max: true, def: true}
	out := make([]int, 0, len(set))
	for l := range set {
		out = append(out, l)
	}
	sort.Ints(out)
	return out
}

func BenchmarkCompress(b *testing.B) {
	corpora := benchCorpora()
	for _, name := range Names() {
		c, err := Get(name)
		if err != nil {
			b.Fatal(err)
		}
		for _, level := range levelSet(c) {
			for corpus, data := range corpora {
				b.Run(c.Name()+"-l"+itoa0(level)+"-"+corpus, func(b *testing.B) {
					src := data
					b.SetBytes(int64(len(src)))
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						w, err := c.NewWriter(io.Discard, level)
						if err != nil {
							b.Fatal(err)
						}
						if _, err := w.Write(src); err != nil {
							b.Fatal(err)
						}
						if err := w.Close(); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

var compressCache map[string][]byte // codec-level -> compressed corpus

func compressedCorpus(b *testing.B, c Codec, level int, corpus string) []byte {
	if compressCache == nil {
		compressCache = map[string][]byte{}
	}
	key := c.Name() + itoa0(level) + corpus
	if out, ok := compressCache[key]; ok {
		return out
	}
	var buf bytes.Buffer
	w, err := c.NewWriter(&buf, level)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := w.Write(benchCorpora()[corpus]); err != nil {
		b.Fatal(err)
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}
	compressCache[key] = buf.Bytes()
	return buf.Bytes()
}

func BenchmarkDecompress(b *testing.B) {
	corpora := benchCorpora()
	for _, name := range Names() {
		c, err := Get(name)
		if err != nil {
			b.Fatal(err)
		}
		for _, level := range levelSet(c) {
			for corpus := range corpora {
				b.Run(c.Name()+"-l"+itoa0(level)+"-"+corpus, func(b *testing.B) {
					compressed := compressedCorpus(b, c, level, corpus)
					b.SetBytes(int64(len(compressed)))
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						r, err := c.NewReader(bytes.NewReader(compressed))
						if err != nil {
							b.Fatal(err)
						}
						if _, err := io.Copy(io.Discard, r); err != nil {
							b.Fatal(err)
						}
						r.Close()
					}
				})
			}
		}
	}
}

func itoa0(n int) string {
	if n < 0 {
		return "m" + itoa0(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

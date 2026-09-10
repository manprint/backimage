package index

import "fmt"

// The public metadata of a backup decides how much memory a reader is about
// to allocate: chunks.json says how many bytes one stored chunk holds, and a
// reader that believes it allocates that much before a single byte has been
// authenticated. manifest.json and chunks.json are both public and neither
// carries a signature, so "believes it" is the whole problem.
//
// Nothing here invents a limit. Every bound comes from a value the backup
// itself declares, and the checks are about the declarations agreeing with
// each other: a chunk cannot be larger than the layer that holds it, the
// chunks of a layer must add up to exactly the size of that layer, and no
// chunk may exceed the chunk size the same manifest says the run used. A
// table that disagrees with the manifest is a format error before it is an
// allocation.
//
// The one bound with no counterpart in the file is the compression
// allowance below, and it is deliberately loose: it exists so that an
// absurd declaration is refused, not to second-guess a codec.

const (
	// expansionDenominator gives back plainCap/64 on top of the declared
	// chunk size. Every codec this project ships stores incompressible input
	// almost verbatim, with a few bytes of block framing; 1.5% is far more
	// than any of them needs and still bounds the allocation.
	expansionDenominator = 64
	// expansionFloor covers the fixed overhead of a codec frame plus the
	// envelope (24 byte header + 16 byte GCM tag) on small chunks.
	expansionFloor = 4096
)

// MaxStoredChunkBytes is the largest stored size one chunk of this backup can
// plausibly have, derived from the chunk size the manifest declares. It
// returns 0 when the manifest declares nothing to derive it from, and the
// caller then has only the per-layer bounds — which are the tighter ones
// anyway.
func MaxStoredChunkBytes(m *Manifest) int64 {
	if m == nil {
		return 0
	}
	plain := m.Chunking.MaxChunkBytes
	if plain <= 0 {
		plain = m.Chunking.TargetChunkBytes
	}
	if plain <= 0 {
		return 0
	}
	return plain + plain/expansionDenominator + expansionFloor
}

// ValidateChunkTable checks the public chunk table against the public
// manifest, before any reader allocates from either.
//
// It is called once, when a backup is opened, so the rest of the reader can
// use the stored sizes and the per-layer offsets without re-deriving whether
// they make sense. Every failure is ErrBadSchema, which the CLI classifies as
// an integrity answer.
func ValidateChunkTable(m *Manifest, t *ChunkTable) error {
	if m == nil || t == nil {
		return fmt.Errorf("%w: missing manifest or chunk table", ErrBadSchema)
	}
	if m.Chunking.Count != len(t.Chunks) {
		return fmt.Errorf("%w: manifest declares %d chunks, the table holds %d",
			ErrBadSchema, m.Chunking.Count, len(t.Chunks))
	}
	for i, c := range t.Chunks {
		if c.I != i {
			return fmt.Errorf("%w: chunk at position %d says it is chunk %d", ErrBadSchema, i, c.I)
		}
		if c.Sb <= 0 {
			return fmt.Errorf("%w: chunk %d declares %d stored bytes", ErrBadSchema, i, c.Sb)
		}
		if c.P == "" {
			return fmt.Errorf("%w: chunk %d names no blob", ErrBadSchema, i)
		}
	}
	if len(t.Chunks) == 0 {
		for _, layer := range m.Layers {
			if layer.ChunkTo >= layer.ChunkFrom {
				return fmt.Errorf("%w: layer %d claims chunks %d-%d of an empty table",
					ErrBadSchema, layer.Index, layer.ChunkFrom, layer.ChunkTo)
			}
		}
		return nil
	}
	return validateLayers(m, t)
}

func validateLayers(m *Manifest, t *ChunkTable) error {
	maxStored := MaxStoredChunkBytes(m)
	next := 0
	// A layer is one blob file holding the stored chunks of a contiguous
	// range, so two layers sharing a path would make the offset of the second
	// one point past the end of the file. It has never been produced; saying
	// so here turns a truncated read into a refusal that names the cause.
	byPath := make(map[string]int, len(m.Layers))
	for _, layer := range m.Layers {
		if layer.ChunkFrom != next {
			return fmt.Errorf("%w: layer %d starts at chunk %d, the previous layer ended at %d",
				ErrBadSchema, layer.Index, layer.ChunkFrom, next-1)
		}
		if layer.ChunkTo < layer.ChunkFrom || layer.ChunkTo >= len(t.Chunks) {
			return fmt.Errorf("%w: layer %d claims chunks %d-%d of %d",
				ErrBadSchema, layer.Index, layer.ChunkFrom, layer.ChunkTo, len(t.Chunks))
		}
		if layer.StoredBytes <= 0 {
			return fmt.Errorf("%w: layer %d declares %d stored bytes", ErrBadSchema, layer.Index, layer.StoredBytes)
		}
		path := t.Chunks[layer.ChunkFrom].P
		if other, seen := byPath[path]; seen {
			return fmt.Errorf("%w: layers %d and %d both read %s", ErrBadSchema, other, layer.Index, path)
		}
		byPath[path] = layer.Index
		var sum int64
		for i := layer.ChunkFrom; i <= layer.ChunkTo; i++ {
			c := t.Chunks[i]
			if c.P != path {
				return fmt.Errorf("%w: chunk %d is in layer %d but names %s instead of %s",
					ErrBadSchema, i, layer.Index, c.P, path)
			}
			if c.Sb > layer.StoredBytes {
				return fmt.Errorf("%w: chunk %d declares %d stored bytes, more than the %d of layer %d",
					ErrBadSchema, i, c.Sb, layer.StoredBytes, layer.Index)
			}
			if maxStored > 0 && c.Sb > maxStored {
				return fmt.Errorf("%w: chunk %d declares %d stored bytes, more than the %d a chunk of this backup can hold",
					ErrBadSchema, i, c.Sb, maxStored)
			}
			sum += c.Sb
			if sum > layer.StoredBytes {
				return fmt.Errorf("%w: the chunks of layer %d add up past its %d stored bytes",
					ErrBadSchema, layer.Index, layer.StoredBytes)
			}
		}
		if sum != layer.StoredBytes {
			return fmt.Errorf("%w: the chunks of layer %d add up to %d, the layer declares %d",
				ErrBadSchema, layer.Index, sum, layer.StoredBytes)
		}
		next = layer.ChunkTo + 1
	}
	if next != len(t.Chunks) {
		return fmt.Errorf("%w: chunks %d-%d belong to no layer", ErrBadSchema, next, len(t.Chunks)-1)
	}
	return nil
}

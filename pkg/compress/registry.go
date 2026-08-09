package compress

import (
	"fmt"
	"sort"
	"strings"
)

var byName = make(map[string]Codec)
var byID = make(map[ID]Codec)

// builtin registers every bundled codec. This is the single place where
// init() is allowed (harness rule 9 exception).
var builtin = []Codec{
	&storeCodec{},
	&gzipCodec{},
	&zstdCodec{},
	&xzCodec{},
	&lz4Codec{},
}

func init() {
	for _, c := range builtin {
		Register(c)
	}
}

// Register makes a codec available by name and wire ID. It panics on
// duplicates and must only be called during package initialisation.
func Register(c Codec) {
	if _, dup := byName[c.Name()]; dup {
		panic(fmt.Sprintf("compress: duplicate codec name %q", c.Name()))
	}
	if _, dup := byID[c.ID()]; dup {
		panic(fmt.Sprintf("compress: duplicate codec id %d", c.ID()))
	}
	byName[c.Name()] = c
	byID[c.ID()] = c
}

// Get returns the codec registered under name.
func Get(name string) (Codec, error) {
	if c, ok := byName[name]; ok {
		return c, nil
	}
	return nil, UsageErrorf("unknown codec %q: available %s", name, strings.Join(Names(), ", "))
}

// ByID returns the codec for a wire identifier.
func ByID(id ID) (Codec, error) {
	if c, ok := byID[id]; ok {
		return c, nil
	}
	return nil, fmt.Errorf("unknown codec id %d", id)
}

// Names returns the registered names, sorted.
func Names() []string {
	out := make([]string, 0, len(byName))
	for name := range byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

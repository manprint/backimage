package restore

import (
	"errors"
	"fmt"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// ErrDigestMismatch reports that the image a source resolved is not the one
// the caller expected. It is an integrity answer and not a network one: the
// bytes arrived, they are simply not the bytes that were asked for.
var ErrDigestMismatch = errors.New("image digest mismatch")

// ExpectedDigest is an image digest the caller obtained **out of band** — a
// signed release note, a ticket, another machine — and wants the source to be
// measured against before anything is read from it.
//
// It is a distinct type from v1.Hash on purpose. A digest computed on the
// image that is being opened proves nothing about its provenance: whoever can
// substitute the image can substitute its digest too. Keeping the expectation
// in its own type, constructed only by parsing text the operator supplied,
// means a value read out of the image cannot reach the comparison by
// accident.
type ExpectedDigest struct {
	hash v1.Hash
	set  bool
}

// ParseExpectedDigest accepts the canonical "sha256:<hex>" form. An empty
// string is not an error: it means the caller asked for no anchor.
func ParseExpectedDigest(text string) (ExpectedDigest, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return ExpectedDigest{}, nil
	}
	h, err := v1.NewHash(text)
	if err != nil {
		return ExpectedDigest{}, fmt.Errorf("%q is not a digest of the form sha256:<hex>: %w", text, err)
	}
	return ExpectedDigest{hash: h, set: true}, nil
}

// Set reports whether an anchor was asked for at all.
func (e ExpectedDigest) Set() bool { return e.set }

func (e ExpectedDigest) String() string {
	if !e.set {
		return ""
	}
	return e.hash.String()
}

// matchResolved compares the expectation with the digests the source reports
// for the object the reference resolved to.
//
// The expectation is the receiver and the resolved digests are the argument:
// two values from two places, one from outside the image and one from the
// source. No call site can satisfy the check by handing the same object
// twice, which is the entire point of the anchor.
func (e ExpectedDigest) matchResolved(source string, resolved ...v1.Hash) error {
	if !e.set {
		return nil
	}
	names := make([]string, 0, len(resolved))
	for _, got := range resolved {
		if got == e.hash {
			return nil
		}
		names = append(names, got.String())
	}
	found := "nothing"
	if len(names) > 0 {
		found = strings.Join(names, ", ")
	}
	return fmt.Errorf("%w: %s resolves to %s, expected %s", ErrDigestMismatch, source, found, e.hash)
}

package archive

import "fmt"

// xattrLossError marks a metadata read that obtained everything about an
// entry except its extended attributes. The writer turns it into a counted
// degradation instead of dropping the entry: losing an attribute is a
// fidelity report, losing the file is data loss.
type xattrLossError struct {
	Path string
	Err  error
}

func (e *xattrLossError) Error() string {
	return fmt.Sprintf("extended attributes of %q: %v", e.Path, e.Err)
}

func (e *xattrLossError) Unwrap() error { return e.Err }

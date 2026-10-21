// Package fixtures builds filesystem trees used by archive round-trip tests.
//
// The builders and the comparator use Unix semantics (modes, owners, xattrs,
// device nodes), so they are compiled on Unix only.
package fixtures

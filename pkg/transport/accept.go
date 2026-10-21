package transport

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// The pause after a transient accept error doubles from acceptRetryMin up to
// acceptRetryMax, as net/http does: retrying at once would spin on a
// condition, such as descriptor exhaustion, that only time clears.
const (
	acceptRetryMin = 5 * time.Millisecond
	acceptRetryMax = time.Second
)

func nextAcceptDelay(d time.Duration) time.Duration {
	if d == 0 {
		return acceptRetryMin
	}
	if d *= 2; d > acceptRetryMax {
		return acceptRetryMax
	}
	return d
}

// transientAcceptError reports whether an accept error may clear on its own.
// Only errors that say the listening socket itself is unusable are final;
// everything else — descriptor or buffer exhaustion, a peer that reset before
// the accept, the pending network errors Linux reports through accept(2), a
// firewall refusal — is retried, since a server that gives up on them stops
// serving for a condition that time clears.
func transientAcceptError(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && fatalAcceptErrno(errno) {
		return false
	}
	return true
}

// closedError is what Accept returns once the listener is closed. A loop
// stopped by an accept failure reports it, still as a closed listener: the
// server leaves its loop on net.ErrClosed instead of retrying a listener that
// will never accept again.
func closedError(cause error) error {
	if cause == nil {
		return net.ErrClosed
	}
	return fmt.Errorf("listener stopped: %w: %w", cause, net.ErrClosed)
}

func isClosed(closed <-chan struct{}) bool {
	select {
	case <-closed:
		return true
	default:
		return false
	}
}

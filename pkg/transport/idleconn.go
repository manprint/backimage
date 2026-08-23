package transport

import (
	"crypto/tls"
	"sync"
	"time"
)

// idleConn makes Config.IdleTimeout mean what it says: the deadline is pushed
// forward by every byte that moves. Armed once at dial or accept time it would
// instead be an absolute cap on the whole session, and a backup stream
// outlives any sensible idle window by design.
type idleConn struct {
	*tls.Conn
	idle time.Duration

	mu       sync.Mutex
	deadline time.Time
}

func newIdleConn(conn *tls.Conn, idle time.Duration) (Stream, error) {
	if idle <= 0 {
		return conn, nil
	}
	c := &idleConn{Conn: conn, idle: idle}
	if err := c.SetDeadline(time.Now().Add(idle)); err != nil {
		return nil, err
	}
	return c, nil
}

// SetDeadline keeps an explicit deadline authoritative: the session loop sets
// one per frame and touch must not fight it.
func (c *idleConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Conn.SetDeadline(t); err != nil {
		return err
	}
	c.deadline = t
	return nil
}

// touch rearms the deadline once less than half the idle window is left.
// Rearming on every frame would cost a timer update per megabyte for nothing.
func (c *idleConn) touch() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.deadline.IsZero() && c.deadline.Sub(now) > c.idle/2 {
		return
	}
	next := now.Add(c.idle)
	if err := c.Conn.SetDeadline(next); err != nil {
		return
	}
	c.deadline = next
}

func (c *idleConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.touch()
	}
	return n, err
}

func (c *idleConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.touch()
	}
	return n, err
}

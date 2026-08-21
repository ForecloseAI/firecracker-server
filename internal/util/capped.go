// Package util holds small helpers shared across the control plane.
package util

import (
	"os"
	"sync"
)

// CappedWriter writes to a file until a byte limit is reached, then silently
// discards. The guest drives firecracker's stdout, so it must never be unbounded.
type CappedWriter struct {
	mu      sync.Mutex
	f       *os.File
	written int64
	limit   int64
}

// NewCappedWriter opens path for append and caps total bytes written at limit.
func NewCappedWriter(path string, limit int64) (*CappedWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return nil, err
	}
	return &CappedWriter{f: f, limit: limit}, nil
}

// Write stores bytes up to the limit and reports the full length so the
// producer never sees a short-write error.
func (c *CappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.limit - c.written
	if room <= 0 {
		return len(p), nil
	}
	if int64(len(p)) < room {
		room = int64(len(p))
	}
	n, err := c.f.Write(p[:room])
	c.written += int64(n)
	return len(p), err
}

// Close releases the underlying file.
func (c *CappedWriter) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.f.Close()
}

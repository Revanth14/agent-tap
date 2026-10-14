package claudecode

import (
	"context"
	"errors"
	"io"
	"sync"
)

// streamTap is an unbounded in-memory pipe. Writes never block, so the
// response stream to Claude Code never waits on the parser; memory is bounded
// by the size of one response.
type streamTap struct {
	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	closed  bool  // No more writes are coming
	stopped bool  // The reader is gone; writes are dropped
	err     error // Why the upstream body ended: io.EOF when it completed
}

func newStreamTap() *streamTap {
	t := &streamTap{}
	t.cond = sync.NewCond(&t.mu)
	return t
}

func (t *streamTap) write(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed && !t.stopped {
		t.buf = append(t.buf, p...)
		t.cond.Signal()
	}
}

// close records why the body ended; the first reason wins.
func (t *streamTap) close(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed, t.err = true, err
	}
	t.cond.Signal()
}

// canceled reports whether the client went away before the body completed.
func (t *streamTap) canceled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err == context.Canceled
}

// stop tells writers the reader is gone, releasing buffered bytes.
func (t *streamTap) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	t.buf = nil
}

func (t *streamTap) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(t.buf) == 0 && !t.closed {
		t.cond.Wait()
	}
	if len(t.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, t.buf)
	t.buf = t.buf[n:]
	return n, nil
}

// teeBody copies every byte the proxy reads from the upstream response into
// a tap, unchanged.
type teeBody struct {
	io.ReadCloser
	tap *streamTap
}

func (b *teeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.tap.write(p[:n])
	}
	if err != nil {
		cause := err
		if errors.Is(err, context.Canceled) {
			cause = context.Canceled
		}
		b.tap.close(cause)
	}
	return n, err
}

// Close before the body ended means the proxy stopped copying it: the client
// went away.
func (b *teeBody) Close() error {
	b.tap.close(context.Canceled)
	return b.ReadCloser.Close()
}

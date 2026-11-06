// Package ratelimit provides bandwidth-limited io.Reader and io.Writer wrappers.
package ratelimit

import (
	"context"
	"io"

	"golang.org/x/time/rate"
)

// maxChunk is the maximum number of bytes accounted per limiter wait.
const maxChunk = 256 * 1024

// NewLimiter returns a limiter allowing bytesPerSecond bytes per second.
// bytesPerSecond <= 0 means unlimited.
func NewLimiter(bytesPerSecond int64) *rate.Limiter {
	if bytesPerSecond <= 0 {
		return rate.NewLimiter(rate.Inf, maxChunk)
	}
	burst := int(min(bytesPerSecond, maxChunk))
	return rate.NewLimiter(rate.Limit(bytesPerSecond), burst)
}

type writer struct {
	ctx context.Context
	w   io.Writer
	l   *rate.Limiter
}

// NewWriter wraps w so that writes do not exceed bytesPerSecond.
// Waiting is interrupted when ctx is canceled.
func NewWriter(ctx context.Context, w io.Writer, bytesPerSecond int64) io.Writer {
	return &writer{ctx: ctx, w: w, l: NewLimiter(bytesPerSecond)}
}

func (lw *writer) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := min(len(p), lw.l.Burst())
		if err := lw.l.WaitN(lw.ctx, chunk); err != nil {
			return written, err
		}
		n, err := lw.w.Write(p[:chunk])
		written += n
		if err != nil {
			return written, err
		}
		p = p[chunk:]
	}
	return written, nil
}

type reader struct {
	ctx context.Context
	r   io.Reader
	l   *rate.Limiter
}

// NewReader wraps r so that reads do not exceed bytesPerSecond.
// Waiting is interrupted when ctx is canceled.
func NewReader(ctx context.Context, r io.Reader, bytesPerSecond int64) io.Reader {
	return &reader{ctx: ctx, r: r, l: NewLimiter(bytesPerSecond)}
}

func (lr *reader) Read(p []byte) (int, error) {
	if len(p) > lr.l.Burst() {
		p = p[:lr.l.Burst()]
	}
	n, err := lr.r.Read(p)
	if n > 0 {
		if werr := lr.l.WaitN(lr.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

package restore

import (
	"context"
	"sync"
	"time"
)

// gate tracks in-flight proxied requests and blocks new ones while the
// VictoriaLogs process is being restarted.
type gate struct {
	mu     sync.Mutex
	active int
	closed bool
	// openCh is closed when the gate is open.
	openCh chan struct{}
	// drainCh is closed when active drops to zero while the gate is closed.
	drainCh chan struct{}
}

func newGate() *gate {
	ch := make(chan struct{})
	close(ch)
	return &gate{openCh: ch}
}

// enter registers a new in-flight request. It waits while the gate is closed.
// It returns false if ctx is done or the gate is not opened within timeout.
func (g *gate) enter(ctx context.Context, timeout time.Duration) bool {
	var timer <-chan time.Time
	for {
		g.mu.Lock()
		if !g.closed {
			g.active++
			g.mu.Unlock()
			return true
		}
		ch := g.openCh
		g.mu.Unlock()
		if timer == nil {
			t := time.NewTimer(timeout)
			defer t.Stop()
			timer = t.C
		}
		select {
		case <-ch:
		case <-timer:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// leave unregisters an in-flight request registered with enter.
func (g *gate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.active < 0 {
		panic("BUG: gate.leave called without enter")
	}
	if g.active == 0 && g.drainCh != nil {
		close(g.drainCh)
		g.drainCh = nil
	}
}

// close blocks new requests and waits until in-flight requests finish or timeout expires.
// It returns the number of requests still in flight when it returns.
// The caller must call open afterwards.
func (g *gate) close(timeout time.Duration) int {
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		g.openCh = make(chan struct{})
	}
	if g.active == 0 {
		g.mu.Unlock()
		return 0
	}
	if g.drainCh == nil {
		g.drainCh = make(chan struct{})
	}
	ch := g.drainCh
	g.mu.Unlock()

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

// open lets new requests in.
func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		return
	}
	g.closed = false
	close(g.openCh)
	g.drainCh = nil
}

// inFlight returns the number of in-flight requests.
func (g *gate) inFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

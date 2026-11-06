package restore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingRestarter counts restarts; each restart blocks until release is called
// when block is set. onRestart is called at the start of every restart.
type blockingRestarter struct {
	calls     atomic.Int64
	block     bool
	releaseCh chan struct{}
	onRestart func()
}

func newBlockingRestarter(block bool) *blockingRestarter {
	return &blockingRestarter{block: block, releaseCh: make(chan struct{}, 100)}
}

func (r *blockingRestarter) Restart(ctx context.Context, _ string) error {
	r.calls.Add(1)
	if r.onRestart != nil {
		r.onRestart()
	}
	if r.block {
		select {
		case <-r.releaseCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *blockingRestarter) release() { r.releaseCh <- struct{}{} }

func TestPartitionLoaderRestartIsNotSharedWithEarlierRestart(t *testing.T) {
	env := newTestEnv(t, "")
	restarter := newBlockingRestarter(true)
	l := &partitionLoader{proxy: env.proxy, restarter: restarter, drainTimeout: time.Second}

	// The first restart is in progress (e.g. it already scanned the partitions directory).
	first := make(chan error, 1)
	go func() { first <- l.restart(context.Background(), "first") }()
	waitFor(t, func() bool { return restarter.calls.Load() == 1 })

	// Requests made while it is running must not be satisfied by it.
	const late = 3
	var wg sync.WaitGroup
	lateErrs := make(chan error, late)
	for range late {
		wg.Go(func() { lateErrs <- l.restart(context.Background(), "late") })
	}
	waitFor(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.requestedSeq == 1+late
	})

	restarter.release()
	if err := <-first; err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	// A second restart starts for the late requests and is shared by all of them.
	waitFor(t, func() bool { return restarter.calls.Load() == 2 })
	select {
	case err := <-lateErrs:
		t.Fatalf("late requests must wait for the second restart; got %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	restarter.release()
	wg.Wait()
	close(lateErrs)
	for err := range lateErrs {
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	}
	if n := restarter.calls.Load(); n != 2 {
		t.Fatalf("expecting exactly 2 restarts; got %d", n)
	}

	// A request made after all restarts finished triggers a new restart.
	restarter.release()
	if err := l.restart(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	if n := restarter.calls.Load(); n != 3 {
		t.Fatalf("expecting 3 restarts; got %d", n)
	}
}

func TestPartitionLoaderRestartWaiterCanceled(t *testing.T) {
	env := newTestEnv(t, "")
	restarter := newBlockingRestarter(true)
	l := &partitionLoader{proxy: env.proxy, restarter: restarter, drainTimeout: time.Second}
	go func() { _ = l.restart(context.Background(), "first") }()
	waitFor(t, func() bool { return restarter.calls.Load() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.restart(ctx, "canceled"); err == nil {
		t.Fatalf("expecting an error for a canceled waiter")
	}
	restarter.release()
	restarter.release()
}

func TestPartitionLoaderFailsWhenPartitionMissingAfterRestart(t *testing.T) {
	env := newTestEnv(t, "")
	env.failAtt.Store(true)
	// The restart does not pick up the partition, and the partition list is available.
	restarter := newBlockingRestarter(false)
	l := &partitionLoader{proxy: env.proxy, restarter: restarter, drainTimeout: time.Second}
	if err := l.Load(context.Background(), "20261104"); err == nil {
		t.Fatalf("expecting an error when the partition is not attached after restart")
	}
	if n := restarter.calls.Load(); n != 1 {
		t.Fatalf("expecting 1 restart; got %d", n)
	}

	// If attach succeeds after the restart, the partition is loaded.
	restarter.onRestart = func() { env.failAtt.Store(false) }
	env.failAtt.Store(true)
	if err := l.Load(context.Background(), "20261103"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
}

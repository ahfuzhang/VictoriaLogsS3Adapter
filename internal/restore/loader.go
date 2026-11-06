package restore

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// attachTimeout bounds a single /internal/partition/attach call. Opening a big
// partition may take a while.
const attachTimeout = 5 * time.Minute

// Restarter restarts the VictoriaLogs process.
type Restarter interface {
	Restart(ctx context.Context, reason string) error
}

// partitionLoader loads restored partitions into VictoriaLogs.
//
// It first tries to hot-load the partition via /internal/partition/attach.
// If that fails and a Restarter is configured (managed mode), VictoriaLogs is
// restarted, so it opens all the partitions found in its storage directory.
// New proxied requests are blocked during the restart and in-flight requests
// are given drainTimeout to finish.
type partitionLoader struct {
	proxy        *Proxy
	restarter    Restarter
	drainTimeout time.Duration

	// Restart coordination. Every restart request gets a sequence number.
	// A request is satisfied only by a restart which started after the request
	// was made, since only such a restart is guaranteed to see the partition
	// directory placed before the request.
	mu           sync.Mutex
	requestedSeq uint64
	// completedSeq is the highest request sequence number covered by a finished restart.
	completedSeq uint64
	// lastErr is the result of the restart which set completedSeq.
	lastErr error
	running bool
	// doneCh is closed when the running restart finishes.
	doneCh chan struct{}
}

// Load implements Loader.
func (l *partitionLoader) Load(ctx context.Context, day string) error {
	be := l.proxy.currentBackend()
	if be == nil {
		return fmt.Errorf("VictoriaLogs is not available")
	}
	startTime := time.Now()
	err := l.attach(ctx, be, day)
	if err == nil {
		slog.Info("partition attached to VictoriaLogs without restart", "date", day, "method", "attach",
			"duration_seconds", time.Since(startTime).Seconds())
		return nil
	}
	if attached, _ := l.isAttached(ctx, be, day); attached {
		slog.Info("partition is already attached to VictoriaLogs", "date", day, "attach_error", err)
		return nil
	}
	if l.restarter == nil {
		return fmt.Errorf("cannot attach partition %s: %w", day, err)
	}
	slog.Warn("cannot attach partition; restarting VictoriaLogs to load it", "date", day, "error", err)
	if err := l.restart(ctx, "cannot attach partition "+day); err != nil {
		return fmt.Errorf("cannot attach partition %s and cannot restart VictoriaLogs: %w", day, err)
	}
	be = l.proxy.currentBackend()
	if be == nil {
		return fmt.Errorf("VictoriaLogs is not available after restart")
	}
	attached, listErr := l.isAttached(ctx, be, day)
	if !attached {
		// VictoriaLogs opens all the partition directories on startup, so the partition
		// is expected to be attached now. Try attaching it explicitly just in case.
		err = l.attach(ctx, be, day)
		if err == nil {
			attached = true
		} else {
			attached, listErr = l.isAttached(ctx, be, day)
		}
	}
	if !attached {
		if listErr == nil {
			return fmt.Errorf("partition %s is not attached after restarting VictoriaLogs; attach error: %v", day, err)
		}
		// The partition list is unavailable (e.g. auth errors), so the result cannot be verified.
		// VictoriaLogs opens all the partition directories on startup, so the partition is most likely loaded.
		slog.Warn("cannot verify that the partition is loaded after restart", "date", day, "error", listErr)
	}
	slog.Info("partition loaded by restarting VictoriaLogs", "date", day, "method", "restart",
		"duration_seconds", time.Since(startTime).Seconds())
	return nil
}

func (l *partitionLoader) attach(ctx context.Context, be *backend, day string) error {
	actx, cancel := context.WithTimeout(ctx, attachTimeout)
	defer cancel()
	return be.vl.PartitionAttach(actx, day)
}

// isAttached reports whether day is in the list of attached partitions.
// A non-nil error means the list could not be obtained.
func (l *partitionLoader) isAttached(ctx context.Context, be *backend, day string) (bool, error) {
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	names, err := be.vl.PartitionList(lctx)
	if err != nil {
		slog.Debug("cannot list partitions", "error", err)
		return false, err
	}
	return slices.Contains(names, day), nil
}

// restart restarts VictoriaLogs while new proxied requests wait.
//
// Concurrent calls share a restart only if it started after they were made;
// otherwise they wait for it to finish and start a new one (shared by all the
// callers waiting at that moment).
func (l *partitionLoader) restart(ctx context.Context, reason string) error {
	l.mu.Lock()
	l.requestedSeq++
	seq := l.requestedSeq
	for {
		if l.completedSeq >= seq {
			err := l.lastErr
			l.mu.Unlock()
			return err
		}
		if !l.running {
			// Start a restart covering all the requests made so far.
			covers := l.requestedSeq
			done := make(chan struct{})
			l.running = true
			l.doneCh = done
			l.mu.Unlock()

			err := l.doRestart(ctx, reason)

			l.mu.Lock()
			l.running = false
			l.completedSeq = covers
			l.lastErr = err
			close(done)
			continue
		}
		done := l.doneCh
		l.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		l.mu.Lock()
	}
}

func (l *partitionLoader) doRestart(ctx context.Context, reason string) error {
	startTime := time.Now()
	remaining := l.proxy.gate.close(l.drainTimeout)
	defer l.proxy.gate.open()
	if remaining > 0 {
		slog.Warn("restarting VictoriaLogs while requests are still in flight", "in_flight", remaining,
			"drain_timeout", l.drainTimeout.String())
	}
	// Interrupt live tail streams and responses still in flight after the drain timeout,
	// so VictoriaLogs stops quickly instead of waiting for them.
	if n := l.proxy.closeStreams(false); n > 0 {
		slog.Info("interrupted response streams before restarting VictoriaLogs", "streams", n)
	}
	err := l.restarter.Restart(ctx, reason)
	if err == nil {
		slog.Info("VictoriaLogs restarted", "reason", reason, "duration_seconds", time.Since(startTime).Seconds())
	}
	return err
}

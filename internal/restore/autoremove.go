package restore

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// maxWaitStep bounds a single sleep while waiting for the next day, so the wall
// clock is re-checked regularly (system suspend, clock adjustments).
const maxWaitStep = time.Minute

// autoRemover removes partitions downloaded at least maxAge ago (-auto.remove.days).
//
// Partitions may be removed only while VictoriaLogs is stopped, so removal happens
// in removeExpired, which is called before every start of VictoriaLogs. Every day
// after 00:00 UTC, run restarts VictoriaLogs if there are expired partitions.
type autoRemover struct {
	restorer *Restorer
	maxAge   time.Duration
	// restart restarts VictoriaLogs; removeExpired is called while it is stopped.
	restart  func(ctx context.Context, reason string) error
	now      func() time.Time
	waitStep time.Duration
}

func newAutoRemover(r *Restorer, days int, restart func(ctx context.Context, reason string) error) *autoRemover {
	return &autoRemover{
		restorer: r,
		maxAge:   time.Duration(days) * 24 * time.Hour,
		restart:  restart,
		now:      time.Now,
		waitStep: maxWaitStep,
	}
}

func (a *autoRemover) cutoff() time.Time {
	return a.now().Add(-a.maxAge)
}

// removeExpired removes the expired partitions. It must be called only while VictoriaLogs is stopped.
func (a *autoRemover) removeExpired() {
	if days := a.restorer.RemoveDownloadedBefore(a.cutoff()); len(days) > 0 {
		slog.Info("removed partitions downloaded long ago", "days", days, "auto_remove_days", a.days())
	}
}

func (a *autoRemover) days() int {
	return int(a.maxAge / (24 * time.Hour))
}

// run checks for expired partitions every day after 00:00 UTC until ctx is canceled.
func (a *autoRemover) run(ctx context.Context) {
	for {
		next := nextDayStart(a.now())
		slog.Info("next check for partitions to remove automatically", "at", next.Format(time.RFC3339),
			"auto_remove_days", a.days())
		if err := waitUntil(ctx, next, a.now, a.waitStep); err != nil {
			return
		}
		a.check(ctx)
	}
}

// check restarts VictoriaLogs if there are expired partitions, so they are removed while it is stopped.
func (a *autoRemover) check(ctx context.Context) {
	days := a.restorer.DownloadedBefore(a.cutoff())
	if len(days) == 0 {
		slog.Info("no partitions to remove automatically", "auto_remove_days", a.days(),
			"local_partitions", len(a.restorer.DownloadTimes()))
		return
	}
	slog.Info("restarting VictoriaLogs to remove partitions downloaded long ago", "days", days,
		"auto_remove_days", a.days())
	reason := fmt.Sprintf("remove partitions downloaded at least %d days ago: %s", a.days(), strings.Join(days, ", "))
	if err := a.restart(ctx, reason); err != nil {
		if ctx.Err() == nil {
			slog.Error("cannot restart VictoriaLogs to remove partitions", "days", days, "error", err)
		}
	}
}

// nextDayStart returns the start of the UTC day after t.
func nextDayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

// waitUntil blocks until the wall clock reported by now reaches t or ctx is canceled.
func waitUntil(ctx context.Context, t time.Time, now func() time.Time, step time.Duration) error {
	for {
		d := t.Sub(now())
		if d <= 0 {
			return ctx.Err()
		}
		timer := time.NewTimer(min(d, step))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

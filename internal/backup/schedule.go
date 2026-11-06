package backup

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// DefaultRetryDelays are the delays between attempts of a scheduled backup.
// A scheduled backup is attempted len(DefaultRetryDelays)+1 times.
var DefaultRetryDelays = []time.Duration{time.Minute, 2 * time.Minute}

// maxSleepStep bounds a single sleep while waiting for the next run, so the
// wall clock is re-checked regularly (system suspend, clock adjustments).
const maxSleepStep = time.Minute

// nextRun returns the first moment strictly after now at hour:minute UTC.
func nextRun(now time.Time, hour, minute int) time.Time {
	now = now.UTC()
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.UTC)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// previousDay returns the partition name (YYYYMMDD) of the UTC day before t.
func previousDay(t time.Time) string {
	return t.UTC().AddDate(0, 0, -1).Format(DateLayout)
}

// sleepCtx sleeps for d or until ctx is canceled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// waitUntil blocks until the wall clock reported by now reaches t or ctx is canceled.
func waitUntil(ctx context.Context, t time.Time, now func() time.Time, step time.Duration) error {
	for {
		d := t.Sub(now())
		if d <= 0 {
			return ctx.Err()
		}
		if err := sleepCtx(ctx, min(d, step)); err != nil {
			return err
		}
	}
}

// BackupFunc backs up a single partition date.
type BackupFunc func(ctx context.Context, date string) error

// scheduler runs a BackupFunc every day at a fixed UTC time.
type scheduler struct {
	backup      BackupFunc
	hour        int
	minute      int
	retryDelays []time.Duration
	log         *slog.Logger
	now         func() time.Time
	sleepStep   time.Duration
}

// run backs up the previous UTC day every day at hour:minute UTC until ctx is canceled.
// It returns nil on cancellation.
func (s *scheduler) run(ctx context.Context) error {
	for {
		next := nextRun(s.now(), s.hour, s.minute)
		date := previousDay(next)
		s.log.Info("next backup scheduled", "at", next.Format(time.RFC3339), "date", date,
			"wait_seconds", int64(next.Sub(s.now()).Seconds()))
		if err := waitUntil(ctx, next, s.now, s.sleepStep); err != nil {
			s.log.Info("scheduler stopped")
			return nil
		}
		s.runWithRetries(ctx, date)
		if ctx.Err() != nil {
			s.log.Info("scheduler stopped")
			return nil
		}
	}
}

// runWithRetries runs the backup for date, retrying failures after retryDelays.
// It reports whether the backup succeeded.
func (s *scheduler) runWithRetries(ctx context.Context, date string) bool {
	attempts := len(s.retryDelays) + 1
	for attempt := 1; ; attempt++ {
		log := s.log.With("date", date, "attempt", attempt, "max_attempts", attempts)
		log.Info("starting scheduled backup")
		err := s.backup(ctx, date)
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			log.Warn("scheduled backup interrupted by shutdown", "error", err)
			return false
		}
		if errors.Is(err, ErrPartitionNotFound) {
			log.Error("scheduled backup failed; not retrying since the partition does not exist", "error", err)
			return false
		}
		if attempt >= attempts {
			log.Error("scheduled backup failed; giving up until the next scheduled run", "error", err)
			return false
		}
		delay := s.retryDelays[attempt-1]
		log.Error("scheduled backup failed; will retry", "error", err, "retry_in_seconds", delay.Seconds())
		if err := sleepCtx(ctx, delay); err != nil {
			return false
		}
	}
}

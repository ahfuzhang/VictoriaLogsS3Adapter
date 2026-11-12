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

// lastRun returns the latest moment at or before now at hour:minute UTC.
func lastRun(now time.Time, hour, minute int) time.Time {
	return nextRun(now, hour, minute).AddDate(0, 0, -1)
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

// ExistsFunc reports whether a backup of the partition date already exists.
type ExistsFunc func(ctx context.Context, date string) (bool, error)

// scheduler runs a BackupFunc every day at a fixed UTC time.
type scheduler struct {
	backup BackupFunc
	// exists is used at startup to detect a missed run; nil disables the check.
	exists      ExistsFunc
	hour        int
	minute      int
	retryDelays []time.Duration
	log         *slog.Logger
	now         func() time.Time
	sleepStep   time.Duration
}

// run backs up the previous UTC day every day at hour:minute UTC until ctx is canceled.
// It returns nil on cancellation.
//
// At startup the most recent scheduled run that has already passed is caught
// up if its backup is missing in S3, so a process (re)started after the
// scheduled time, or killed during a backup, does not lose that day.
//
// If a run ends after the following scheduled time has passed (a long backup,
// system suspend, clock jump), that run starts immediately instead of being
// skipped. At most one passed run is caught up this way.
func (s *scheduler) run(ctx context.Context) error {
	last := lastRun(s.now(), s.hour, s.minute)
	s.catchUp(ctx, last)
	for ctx.Err() == nil {
		next := last.AddDate(0, 0, 1)
		if latest := lastRun(s.now(), s.hour, s.minute); latest.After(next) {
			next = latest
		}
		date := previousDay(next)
		s.log.Info("next backup scheduled", "at", next.Format(time.RFC3339), "date", date,
			"wait_seconds", max(0, int64(next.Sub(s.now()).Seconds())))
		if err := waitUntil(ctx, next, s.now, s.sleepStep); err != nil {
			break
		}
		s.runWithRetries(ctx, date)
		last = next
	}
	s.log.Info("scheduler stopped")
	return nil
}

// catchUp backs up the partition of the scheduled run at slot unless its
// backup already exists. If the check fails, the backup runs anyway: a
// duplicate backup is better than a missing one.
func (s *scheduler) catchUp(ctx context.Context, slot time.Time) {
	if s.exists == nil {
		return
	}
	date := previousDay(slot)
	log := s.log.With("date", date, "scheduled_at", slot.Format(time.RFC3339))
	found, err := s.exists(ctx, date)
	switch {
	case ctx.Err() != nil:
		return
	case err != nil:
		log.Warn("cannot check whether the last scheduled backup exists; running it now", "error", err)
	case found:
		log.Info("the last scheduled backup exists; nothing to catch up")
		return
	default:
		log.Info("the last scheduled backup is missing; running it now")
	}
	s.runWithRetries(ctx, date)
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

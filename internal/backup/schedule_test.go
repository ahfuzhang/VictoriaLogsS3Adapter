package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"
)

var discardLogger = slog.New(slog.DiscardHandler)

func TestNextRun(t *testing.T) {
	utc := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		now          string
		hour, minute int
		want         string
		wantDate     string
	}{
		{"2026-11-05T00:00:00Z", 0, 15, "2026-11-05T00:15:00Z", "20261104"},
		{"2026-11-05T00:14:59.999Z", 0, 15, "2026-11-05T00:15:00Z", "20261104"},
		{"2026-11-05T00:15:00Z", 0, 15, "2026-11-06T00:15:00Z", "20261105"}, // exactly at the run time: next day
		{"2026-11-05T00:15:00.001Z", 0, 15, "2026-11-06T00:15:00Z", "20261105"},
		{"2026-11-05T23:59:00Z", 23, 59, "2026-11-06T23:59:00Z", "20261105"},
		{"2026-12-31T12:00:00Z", 0, 5, "2027-01-01T00:05:00Z", "20261231"},           // year boundary
		{"2028-02-28T23:00:00Z", 1, 0, "2028-02-29T01:00:00Z", "20280228"},           // leap day
		{"2026-03-01T00:30:00Z", 0, 15, "2026-03-02T00:15:00Z", "20260301"},          // after today's run
		{"2026-11-05T08:00:00+08:00", 0, 15, "2026-11-05T00:15:00Z", "20261104"},     // input in another zone (00:00 UTC)
		{"2026-11-04T23:30:00-02:00", 0, 15, "2026-11-06T00:15:00Z", "20261105"},     // 01:30 UTC on 11-05
		{"2026-11-05T00:10:00+00:00", 0, 15, "2026-11-05T00:15:00Z", "20261104"},     // explicit offset
		{"2026-10-25T00:59:00Z", 1, 0, "2026-10-25T01:00:00Z", "20261024"},           // DST change days are irrelevant in UTC
		{"2026-11-05T09:46:00Z", 9, 45, "2026-11-06T09:45:00Z", "20261105"},          // just missed
		{"2026-11-05T09:44:00Z", 9, 45, "2026-11-05T09:45:00Z", "20261104"},          // just before
		{"2026-01-31T23:59:59.999999999Z", 0, 0, "2026-02-01T00:00:00Z", "20260131"}, // month boundary
	}
	for _, tt := range tests {
		got := nextRun(utc(tt.now), tt.hour, tt.minute)
		if !got.Equal(utc(tt.want)) || got.Location() != time.UTC {
			t.Errorf("nextRun(%s, %02d%02d) = %s; want %s", tt.now, tt.hour, tt.minute, got, tt.want)
		}
		if d := previousDay(got); d != tt.wantDate {
			t.Errorf("previousDay(%s) = %s; want %s", got, d, tt.wantDate)
		}
	}
}

func TestWaitUntil(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	if err := waitUntil(ctx, start.Add(80*time.Millisecond), time.Now, 20*time.Millisecond); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if el := time.Since(start); el < 80*time.Millisecond {
		t.Fatalf("waitUntil returned too early after %s", el)
	}

	// The past returns immediately.
	if err := waitUntil(ctx, start.Add(-time.Hour), time.Now, time.Minute); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(30*time.Millisecond, cancel)
	start = time.Now()
	if err := waitUntil(cctx, start.Add(time.Hour), time.Now, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled; got %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("cancellation took too long: %s", el)
	}
}

func TestRunWithRetries(t *testing.T) {
	delays := []time.Duration{5 * time.Millisecond, 5 * time.Millisecond}

	t.Run("succeeds after failures", func(t *testing.T) {
		calls := 0
		s := &scheduler{log: discardLogger, retryDelays: delays, backup: func(ctx context.Context, date string) error {
			calls++
			if date != "20261104" {
				t.Errorf("unexpected date %q", date)
			}
			if calls < 3 {
				return fmt.Errorf("transient failure %d", calls)
			}
			return nil
		}}
		if !s.runWithRetries(context.Background(), "20261104") || calls != 3 {
			t.Fatalf("expected success after 3 calls; got %d calls", calls)
		}
	})

	t.Run("gives up after max attempts", func(t *testing.T) {
		calls := 0
		s := &scheduler{log: discardLogger, retryDelays: delays, backup: func(ctx context.Context, date string) error {
			calls++
			return errors.New("permanent failure")
		}}
		if s.runWithRetries(context.Background(), "20261104") || calls != 3 {
			t.Fatalf("expected failure after 3 calls; got %d calls", calls)
		}
	})

	t.Run("missing partition is not retried", func(t *testing.T) {
		calls := 0
		s := &scheduler{log: discardLogger, retryDelays: delays, backup: func(ctx context.Context, date string) error {
			calls++
			return fmt.Errorf("%w: /x", ErrPartitionNotFound)
		}}
		if s.runWithRetries(context.Background(), "20261104") || calls != 1 {
			t.Fatalf("expected a single call; got %d calls", calls)
		}
	})

	t.Run("cancellation interrupts the retry delay", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		s := &scheduler{log: discardLogger, retryDelays: []time.Duration{time.Hour, time.Hour}, backup: func(ctx context.Context, date string) error {
			calls++
			time.AfterFunc(20*time.Millisecond, cancel)
			return errors.New("failure")
		}}
		start := time.Now()
		if s.runWithRetries(ctx, "20261104") || calls != 1 {
			t.Fatalf("expected failure after 1 call; got %d calls", calls)
		}
		if el := time.Since(start); el > 2*time.Second {
			t.Fatalf("cancellation took too long: %s", el)
		}
	})
}

func TestSchedulerRun(t *testing.T) {
	// Pretend the wall clock is 100ms before 00:15 UTC on 2026-11-05.
	fakeStart := time.Date(2026, 11, 5, 0, 14, 59, 900_000_000, time.UTC)
	offset := time.Until(fakeStart)
	now := func() time.Time { return time.Now().Add(offset) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dates []string
	s := &scheduler{
		hour:        0,
		minute:      15,
		retryDelays: []time.Duration{time.Millisecond},
		log:         discardLogger,
		now:         now,
		sleepStep:   20 * time.Millisecond,
		backup: func(ctx context.Context, date string) error {
			if n := now(); n.Before(time.Date(2026, 11, 5, 0, 15, 0, 0, time.UTC)) {
				t.Errorf("backup started too early at %s", n)
			}
			dates = append(dates, date)
			cancel() // simulate a shutdown signal during the backup
			return ctx.Err()
		},
	}
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error on shutdown: %s", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not stop")
	}
	if len(dates) != 1 || dates[0] != "20261104" {
		t.Fatalf("backed up dates %q; want [20261104]", dates)
	}
}

func TestSchedulerRunStopsWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &scheduler{
		hour: 0, minute: 15,
		log:       discardLogger,
		now:       time.Now,
		sleepStep: time.Minute,
		backup: func(ctx context.Context, date string) error {
			t.Errorf("unexpected backup of %s", date)
			return nil
		},
	}
	// Make sure the next run is far enough in the future.
	if time.Until(nextRun(time.Now(), 0, 15)) < time.Second {
		t.Skip("too close to 00:15 UTC")
	}
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	if err := s.run(ctx); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("shutdown took too long: %s", el)
	}
}

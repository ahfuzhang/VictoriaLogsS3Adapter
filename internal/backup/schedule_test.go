package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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

func TestLastRun(t *testing.T) {
	tests := []struct {
		now          time.Time
		hour, minute int
		want         time.Time
	}{
		{time.Date(2026, 11, 12, 6, 31, 48, 0, time.UTC), 0, 15, time.Date(2026, 11, 12, 0, 15, 0, 0, time.UTC)},
		{time.Date(2026, 11, 12, 0, 15, 0, 0, time.UTC), 0, 15, time.Date(2026, 11, 12, 0, 15, 0, 0, time.UTC)}, // exactly at the run time
		{time.Date(2026, 11, 12, 0, 14, 59, 0, time.UTC), 0, 15, time.Date(2026, 11, 11, 0, 15, 0, 0, time.UTC)},
		{time.Date(2027, 1, 1, 0, 1, 0, 0, time.UTC), 0, 5, time.Date(2026, 12, 31, 0, 5, 0, 0, time.UTC)}, // year boundary
	}
	for _, tt := range tests {
		if got := lastRun(tt.now, tt.hour, tt.minute); !got.Equal(tt.want) {
			t.Errorf("lastRun(%s, %02d%02d) = %s; want %s", tt.now, tt.hour, tt.minute, got, tt.want)
		}
	}
}

// fakeClock is a manually advanced wall clock safe for concurrent use.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// runScheduler runs s until it stops and fails the test if that takes too long.
func runScheduler(t *testing.T, ctx context.Context, s *scheduler) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %s", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not stop")
	}
}

func TestSchedulerCatchUp(t *testing.T) {
	// The process starts at 06:31 UTC, after the 00:15 UTC run for 20261111 has passed.
	start := time.Date(2026, 11, 12, 6, 31, 48, 0, time.UTC)
	nextSlot := time.Date(2026, 11, 13, 0, 15, 0, 0, time.UTC)

	newScheduler := func(clock *fakeClock, dates *[]string, cancelAfter int, cancel context.CancelFunc) *scheduler {
		return &scheduler{
			hour: 0, minute: 15,
			retryDelays: []time.Duration{time.Millisecond},
			log:         discardLogger,
			now:         clock.Now,
			sleepStep:   time.Millisecond,
			backup: func(ctx context.Context, date string) error {
				*dates = append(*dates, date)
				if len(*dates) >= cancelAfter {
					cancel()
				}
				return nil
			},
		}
	}

	t.Run("missing backup is run at startup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clock := &fakeClock{t: start}
		var dates, checked []string
		s := newScheduler(clock, &dates, 1, cancel)
		s.exists = func(ctx context.Context, date string) (bool, error) {
			checked = append(checked, date)
			return false, nil
		}
		runScheduler(t, ctx, s)
		if fmt.Sprint(checked) != "[20261111]" || fmt.Sprint(dates) != "[20261111]" {
			t.Fatalf("checked %q, backed up %q; want [20261111] for both", checked, dates)
		}
	})

	t.Run("existing backup is not run again", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clock := &fakeClock{t: start}
		var dates []string
		s := newScheduler(clock, &dates, 1, cancel)
		s.exists = func(ctx context.Context, date string) (bool, error) {
			clock.Set(nextSlot) // let the next regular run start right away
			return true, nil
		}
		runScheduler(t, ctx, s)
		if fmt.Sprint(dates) != "[20261112]" {
			t.Fatalf("backed up %q; want only the next regular run [20261112]", dates)
		}
	})

	t.Run("failed check still runs the backup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clock := &fakeClock{t: start}
		var dates []string
		s := newScheduler(clock, &dates, 1, cancel)
		s.exists = func(ctx context.Context, date string) (bool, error) {
			return false, errors.New("s3 unavailable")
		}
		runScheduler(t, ctx, s)
		if fmt.Sprint(dates) != "[20261111]" {
			t.Fatalf("backed up %q; want [20261111]", dates)
		}
	})

	t.Run("cancellation during the check skips the backup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clock := &fakeClock{t: start}
		var dates []string
		s := newScheduler(clock, &dates, 1, cancel)
		s.exists = func(ctx context.Context, date string) (bool, error) {
			cancel()
			return false, ctx.Err()
		}
		runScheduler(t, ctx, s)
		if len(dates) != 0 {
			t.Fatalf("backed up %q; want nothing", dates)
		}
	})
}

func TestSchedulerRunOverrun(t *testing.T) {
	t.Run("run that passed during a long backup starts immediately", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clock := &fakeClock{t: time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)}
		var dates []string
		s := &scheduler{
			hour: 0, minute: 15,
			log:       discardLogger,
			now:       clock.Now,
			sleepStep: time.Millisecond,
			exists:    func(ctx context.Context, date string) (bool, error) { return false, nil },
			backup: func(ctx context.Context, date string) error {
				dates = append(dates, date)
				if len(dates) == 1 {
					// The catch-up backup of 20261111 ends after the 00:15 run for 20261112.
					clock.Set(time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC))
				} else {
					cancel()
				}
				return nil
			},
		}
		runScheduler(t, ctx, s)
		if fmt.Sprint(dates) != "[20261111 20261112]" {
			t.Fatalf("backed up %q; want [20261111 20261112]", dates)
		}
	})

	t.Run("only the latest passed run is caught up", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clock := &fakeClock{t: time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)}
		var dates []string
		s := &scheduler{
			hour: 0, minute: 15,
			log:       discardLogger,
			now:       clock.Now,
			sleepStep: time.Millisecond,
			exists: func(ctx context.Context, date string) (bool, error) {
				clock.Set(time.Date(2026, 11, 13, 0, 15, 0, 0, time.UTC)) // the regular run for 20261112 is due
				return true, nil
			},
			backup: func(ctx context.Context, date string) error {
				dates = append(dates, date)
				if len(dates) == 1 {
					// Simulate a system suspend of several days.
					clock.Set(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
				} else {
					cancel()
				}
				return nil
			},
		}
		runScheduler(t, ctx, s)
		if fmt.Sprint(dates) != "[20261112 20261114]" {
			t.Fatalf("backed up %q; want [20261112 20261114]", dates)
		}
	})
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

package restore

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/s3store"
)

// restoreAt restores day with the clock set to t.
func restoreAt(t *testing.T, r *Restorer, clock *fakeClock, day string, at time.Time) {
	t.Helper()
	clock.mu.Lock()
	clock.t = at
	clock.mu.Unlock()
	res, err := r.EnsureDay(context.Background(), day)
	if err != nil || res != ResultRestored {
		t.Fatalf("cannot restore %s: %q, %v", day, res, err)
	}
}

func TestRestorerRecordsDownloadTime(t *testing.T) {
	store := newFakeStore()
	store.put("20261101", makeSnapshotTar(t, "snap"))
	clock := &fakeClock{}
	r, storageDir := newTestRestorer(t, store, &fakeLoader{}, func(c *RestorerConfig) { c.Now = clock.Now })

	downloadedAt := time.Date(2026, 11, 2, 3, 4, 5, 0, time.UTC)
	restoreAt(t, r, clock, "20261101", downloadedAt)
	if got := r.DownloadTimes(); len(got) != 1 || !got["20261101"].Equal(downloadedAt) {
		t.Fatalf("unexpected download times: %v", got)
	}
	fi, err := os.Stat(filepath.Join(storageDir, partitionsDirname, "20261101"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(downloadedAt) {
		t.Fatalf("the modification time of the partition must be the download time %s; got %s", downloadedAt, fi.ModTime())
	}

	// A partition directory placed by somebody else, with a modification time in the future.
	future := filepath.Join(storageDir, partitionsDirname, "20261103")
	if err := os.MkdirAll(future, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(future, downloadedAt.AddDate(1, 0, 0), downloadedAt.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	// Entries which aren't partition directories are ignored.
	if err := os.WriteFile(filepath.Join(storageDir, partitionsDirname, "20261104"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(storageDir, partitionsDirname, "not-a-day"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The download times are read back on startup.
	startedAt := downloadedAt.Add(time.Hour)
	r2, err := NewRestorer(context.Background(), RestorerConfig{
		StorageDir: storageDir,
		Store:      store,
		Now:        func() time.Time { return startedAt },
	})
	if err != nil {
		t.Fatal(err)
	}
	got := r2.DownloadTimes()
	if len(got) != 2 || !got["20261101"].Equal(downloadedAt) || !got["20261103"].Equal(startedAt) {
		t.Fatalf("unexpected download times after restart: %v", got)
	}
}

func TestRestorerRemoveDownloadedBefore(t *testing.T) {
	store := newFakeStore()
	for _, day := range []string{"20261101", "20261102", "20261103"} {
		store.put(day, makeSnapshotTar(t, "snap"))
	}
	clock := &fakeClock{}
	r, storageDir := newTestRestorer(t, store, &fakeLoader{}, func(c *RestorerConfig) { c.Now = clock.Now })
	base := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	restoreAt(t, r, clock, "20261101", base)
	restoreAt(t, r, clock, "20261102", base.Add(time.Hour))
	restoreAt(t, r, clock, "20261103", base.Add(2*time.Hour))

	cutoff := base.Add(time.Hour)
	if got := r.DownloadedBefore(cutoff); !slices.Equal(got, []string{"20261101", "20261102"}) {
		t.Fatalf("unexpected expired days: %v", got)
	}
	if got := r.RemoveDownloadedBefore(cutoff); !slices.Equal(got, []string{"20261101", "20261102"}) {
		t.Fatalf("unexpected removed days: %v", got)
	}
	assertNoPartition(t, storageDir, "20261101")
	assertNoPartition(t, storageDir, "20261102")
	if _, err := os.Stat(filepath.Join(storageDir, partitionsDirname, "20261103")); err != nil {
		t.Fatalf("the partition downloaded after the cutoff must be kept: %s", err)
	}
	if got := r.DownloadTimes(); len(got) != 1 || !got["20261103"].Equal(base.Add(2*time.Hour)) {
		t.Fatalf("unexpected download times: %v", got)
	}
	// The removed directories are deleted in the background.
	waitFor(t, func() bool {
		des, err := os.ReadDir(filepath.Join(storageDir, restoreTmpDirname))
		return err == nil && len(des) == 0
	})

	// A removed partition is downloaded again when requested.
	restoreAt(t, r, clock, "20261101", base.Add(3*time.Hour))
	if n := store.openCalls.Load(); n != 4 {
		t.Fatalf("expecting the removed partition to be downloaded again; Open calls: %d", n)
	}
	if got := r.RemoveDownloadedBefore(cutoff); len(got) != 0 {
		t.Fatalf("nothing must be removed; got %v", got)
	}
}

func TestRestorerRemoveDownloadedBeforeMissingDir(t *testing.T) {
	store := newFakeStore()
	store.put("20261101", makeSnapshotTar(t, "snap"))
	clock := &fakeClock{t: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	r, storageDir := newTestRestorer(t, store, &fakeLoader{}, func(c *RestorerConfig) { c.Now = clock.Now })
	restoreAt(t, r, clock, "20261101", clock.Now())
	// The directory is deleted by somebody else; it is forgotten.
	if err := os.RemoveAll(filepath.Join(storageDir, partitionsDirname, "20261101")); err != nil {
		t.Fatal(err)
	}
	if got := r.RemoveDownloadedBefore(clock.Now()); !slices.Equal(got, []string{"20261101"}) {
		t.Fatalf("unexpected removed days: %v", got)
	}
	if got := r.DownloadTimes(); len(got) != 0 {
		t.Fatalf("unexpected download times: %v", got)
	}
}

func TestRestorerForgetsQuarantinedPartition(t *testing.T) {
	store := newFakeStore()
	store.put("20261101", makeSnapshotTar(t, "snap"))
	r, _ := newTestRestorer(t, store, &fakeLoader{err: context.DeadlineExceeded}, func(c *RestorerConfig) {
		c.RemoveOnLoadFailure = true
	})
	if _, err := r.EnsureDay(context.Background(), "20261101"); err == nil {
		t.Fatalf("expecting a load error")
	}
	if got := r.DownloadTimes(); len(got) != 0 {
		t.Fatalf("a removed partition must be forgotten; got %v", got)
	}
}

// fakeVLRestarter emulates the supervisor: removeExpired is called while VictoriaLogs is stopped.
type fakeVLRestarter struct {
	mu          sync.Mutex
	reasons     []string
	beforeStart func()
}

func (f *fakeVLRestarter) restart(_ context.Context, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reasons = append(f.reasons, reason)
	f.beforeStart()
	return nil
}

func (f *fakeVLRestarter) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reasons)
}

func TestAutoRemoverCheck(t *testing.T) {
	store := newFakeStore()
	store.put("20261101", makeSnapshotTar(t, "snap"))
	store.put("20261102", makeSnapshotTar(t, "snap"))
	clock := &fakeClock{}
	r, storageDir := newTestRestorer(t, store, &fakeLoader{}, func(c *RestorerConfig) { c.Now = clock.Now })
	base := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	restoreAt(t, r, clock, "20261101", base)
	restoreAt(t, r, clock, "20261102", base.Add(12*time.Hour))

	vl := &fakeVLRestarter{}
	a := newAutoRemover(r, 3, vl.restart)
	a.now = clock.Now
	vl.beforeStart = a.removeExpired

	// Nothing has expired yet: VictoriaLogs is not restarted.
	clock.Add(3*24*time.Hour - 12*time.Hour - time.Second)
	a.check(context.Background())
	if got := vl.calls(); len(got) != 0 {
		t.Fatalf("VictoriaLogs must not be restarted; got restarts %q", got)
	}

	// 3 days after the first download: only the first partition is removed.
	clock.mu.Lock()
	clock.t = base.Add(3 * 24 * time.Hour)
	clock.mu.Unlock()
	a.check(context.Background())
	if got := vl.calls(); len(got) != 1 || !strings.Contains(got[0], "20261101") || strings.Contains(got[0], "20261102") {
		t.Fatalf("unexpected restarts: %q", got)
	}
	assertNoPartition(t, storageDir, "20261101")
	if _, err := os.Stat(filepath.Join(storageDir, partitionsDirname, "20261102")); err != nil {
		t.Fatalf("the partition downloaded less than 3 days ago must be kept: %s", err)
	}

	// Nothing to remove until the second partition expires.
	a.check(context.Background())
	if got := vl.calls(); len(got) != 1 {
		t.Fatalf("unexpected restarts: %q", got)
	}
}

func TestAutoRemoverRunsAfterDayStart(t *testing.T) {
	store := newFakeStore()
	store.put("20261101", makeSnapshotTar(t, "snap"))
	clock := &fakeClock{}
	r, storageDir := newTestRestorer(t, store, &fakeLoader{}, func(c *RestorerConfig) { c.Now = clock.Now })
	restoreAt(t, r, clock, "20261101", time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))

	vl := &fakeVLRestarter{}
	a := newAutoRemover(r, 1, vl.restart)
	a.now = clock.Now
	a.waitStep = time.Millisecond
	vl.beforeStart = a.removeExpired

	clock.mu.Lock()
	clock.t = time.Date(2026, 11, 2, 23, 59, 59, 0, time.UTC)
	clock.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.run(ctx)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	if got := vl.calls(); len(got) != 0 {
		t.Fatalf("nothing must happen before the day ends; got restarts %q", got)
	}
	clock.Add(2 * time.Second)
	waitFor(t, func() bool { return len(vl.calls()) == 1 })
	assertNoPartition(t, storageDir, "20261101")
	cancel()
	<-done
}

func TestNextDayStart(t *testing.T) {
	f := func(now, want time.Time) {
		t.Helper()
		if got := nextDayStart(now); !got.Equal(want) {
			t.Fatalf("nextDayStart(%s): got %s; want %s", now, got, want)
		}
	}
	f(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC))
	f(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC))
	f(time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	// The UTC day is used regardless of the time zone of now.
	f(time.Date(2026, 11, 6, 7, 0, 0, 0, time.FixedZone("UTC+8", 8*3600)), time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC))
}

func TestConfigValidateAutoRemoveDays(t *testing.T) {
	newCfg := func(managed bool, days int) *Config {
		c := &Config{
			S3:             &s3store.Config{Endpoint: "http://127.0.0.1:9000", Bucket: "b"},
			StorageDir:     "/tmp/",
			MaxDays:        31,
			Addr:           ":8093",
			AutoRemoveDays: days,
		}
		if managed {
			c.VLBin = os.Args[0] // any executable file
		} else {
			c.VLAddr = "http://127.0.0.1:9428"
		}
		return c
	}
	for _, tc := range []struct {
		managed bool
		days    int
		ok      bool
	}{
		{managed: true, days: 0, ok: true},
		{managed: true, days: 3, ok: true},
		{managed: false, days: 0, ok: true},
		{managed: false, days: 3, ok: false},
		{managed: true, days: -1, ok: false},
	} {
		err := newCfg(tc.managed, tc.days).Validate()
		if (err == nil) != tc.ok {
			t.Fatalf("managed=%v, days=%d: unexpected error %v", tc.managed, tc.days, err)
		}
	}
}

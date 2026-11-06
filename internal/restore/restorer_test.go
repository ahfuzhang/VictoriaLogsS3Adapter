package restore

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore is an in-memory Store.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte // day -> tar data

	findCalls atomic.Int64
	openCalls atomic.Int64
	findErr   error
	// truncate makes Open return fewer bytes than the reported size.
	truncate bool

	// openGate, when set, blocks Open until it is closed.
	openGate chan struct{}
	// concurrency tracking for Open/read.
	active    atomic.Int64
	maxActive atomic.Int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: make(map[string][]byte)}
}

func (s *fakeStore) put(day string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[day] = data
}

func (s *fakeStore) FindBackup(_ context.Context, day string) (string, bool, error) {
	s.findCalls.Add(1)
	if s.findErr != nil {
		return "", false, s.findErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[day]; !ok {
		return "", false, nil
	}
	return "backups/" + day + ".tar", true, nil
}

func (s *fakeStore) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	s.openCalls.Add(1)
	n := s.active.Add(1)
	for {
		m := s.maxActive.Load()
		if n <= m || s.maxActive.CompareAndSwap(m, n) {
			break
		}
	}
	if s.openGate != nil {
		select {
		case <-s.openGate:
		case <-ctx.Done():
			s.active.Add(-1)
			return nil, 0, ctx.Err()
		}
	}
	day := filepath.Base(key)
	day = day[:len(day)-len(".tar")]
	s.mu.Lock()
	data, ok := s.objects[day]
	s.mu.Unlock()
	if !ok {
		s.active.Add(-1)
		return nil, 0, errors.New("no such key")
	}
	size := int64(len(data))
	if s.truncate {
		data = data[:len(data)-512]
	}
	return &trackedReader{r: bytes.NewReader(data), s: s}, size, nil
}

type trackedReader struct {
	r    io.Reader
	s    *fakeStore
	once sync.Once
}

func (t *trackedReader) Read(p []byte) (int, error) { return t.r.Read(p) }
func (t *trackedReader) Close() error {
	t.once.Do(func() { t.s.active.Add(-1) })
	return nil
}

// fakeLoader records Load calls.
type fakeLoader struct {
	mu    sync.Mutex
	days  []string
	err   error
	check func(day string)
}

func (l *fakeLoader) Load(_ context.Context, day string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.days = append(l.days, day)
	if l.check != nil {
		l.check(day)
	}
	return l.err
}

func (l *fakeLoader) calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.days...)
}

func makeSnapshotTar(t *testing.T, snapshotName string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name string, typ byte, body string) {
		hdr := &tar.Header{Name: name, Typeflag: typ, Mode: 0o644, Size: int64(len(body))}
		if typ == tar.TypeDir {
			hdr.Mode, hdr.Size = 0o755, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("./", tar.TypeDir, "")
	write("./"+snapshotName+"/", tar.TypeDir, "")
	write("./"+snapshotName+"/indexdb/", tar.TypeDir, "")
	write("./"+snapshotName+"/indexdb/parts.json", tar.TypeReg, "[]")
	write("./"+snapshotName+"/datadb/", tar.TypeDir, "")
	write("./"+snapshotName+"/datadb/parts.json", tar.TypeReg, `["part1"]`)
	write("./"+snapshotName+"/datadb/part1/timestamps.bin", tar.TypeReg, "0123456789")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestRestorer(t *testing.T, store Store, loader Loader, mutate func(*RestorerConfig)) (*Restorer, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := RestorerConfig{
		StorageDir: dir,
		Store:      store,
		Loader:     loader,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	r, err := NewRestorer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewRestorer: %s", err)
	}
	return r, dir
}

func assertTmpEmpty(t *testing.T, storageDir string) {
	t.Helper()
	des, err := os.ReadDir(filepath.Join(storageDir, restoreTmpDirname))
	if err != nil {
		t.Fatal(err)
	}
	if len(des) != 0 {
		names := make([]string, len(des))
		for i, de := range des {
			names[i] = de.Name()
		}
		t.Fatalf("temporary directory must be empty; got %v", names)
	}
}

func assertNoPartition(t *testing.T, storageDir, day string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(storageDir, partitionsDirname, day)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partition directory for %s must not exist; stat error: %v", day, err)
	}
}

func TestRestorerRestoresPartition(t *testing.T) {
	store := newFakeStore()
	store.put("20261104", makeSnapshotTar(t, "20261104091211-18C876EFEB03599B"))
	var storageDir string
	loader := &fakeLoader{}
	r, storageDir := newTestRestorer(t, store, loader, nil)
	loader.check = func(day string) {
		// The partition must be in place before it is loaded.
		if _, err := os.Stat(filepath.Join(storageDir, "partitions", day, "indexdb")); err != nil {
			t.Errorf("partition must exist when Load is called: %s", err)
		}
	}

	res, err := r.EnsureDay(context.Background(), "20261104")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if res != ResultRestored {
		t.Fatalf("unexpected result %q", res)
	}
	p := filepath.Join(storageDir, "partitions", "20261104")
	b, err := os.ReadFile(filepath.Join(p, "datadb", "part1", "timestamps.bin"))
	if err != nil || string(b) != "0123456789" {
		t.Fatalf("unexpected restored file: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(p, "indexdb", "parts.json")); err != nil {
		t.Fatal(err)
	}
	assertTmpEmpty(t, storageDir)
	if got := loader.calls(); len(got) != 1 || got[0] != "20261104" {
		t.Fatalf("unexpected Load calls: %v", got)
	}

	// The partition exists now, so the store must not be accessed again.
	res, err = r.EnsureDay(context.Background(), "20261104")
	if err != nil || res != ResultExists {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
	if n := store.findCalls.Load(); n != 1 {
		t.Fatalf("unexpected FindBackup calls: %d", n)
	}
	if n := store.openCalls.Load(); n != 1 {
		t.Fatalf("unexpected Open calls: %d", n)
	}
	if got := loader.calls(); len(got) != 1 {
		t.Fatalf("unexpected Load calls: %v", got)
	}
}

func TestRestorerSingleflight(t *testing.T) {
	store := newFakeStore()
	store.put("20261104", makeSnapshotTar(t, "snap"))
	store.openGate = make(chan struct{})
	loader := &fakeLoader{}
	r, storageDir := newTestRestorer(t, store, loader, nil)

	const n = 20
	results := make(chan Result, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			res, err := r.EnsureDay(context.Background(), "20261104")
			if err != nil {
				t.Errorf("unexpected error: %s", err)
			}
			results <- res
		})
	}
	// Wait until the single download is blocked in Open, then let it proceed.
	waitFor(t, func() bool { return store.openCalls.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	close(store.openGate)
	wg.Wait()
	close(results)
	restored, exists := 0, 0
	for res := range results {
		switch res {
		case ResultRestored:
			restored++
		case ResultExists:
			exists++
		default:
			t.Fatalf("unexpected result %q", res)
		}
	}
	if restored == 0 {
		t.Fatalf("expecting at least one restored result")
	}
	if store.findCalls.Load() != 1 || store.openCalls.Load() != 1 {
		t.Fatalf("expecting a single lookup and download; got find=%d open=%d", store.findCalls.Load(), store.openCalls.Load())
	}
	if got := loader.calls(); len(got) != 1 {
		t.Fatalf("expecting a single Load call; got %v", got)
	}
	assertTmpEmpty(t, storageDir)
}

func TestRestorerNegativeCache(t *testing.T) {
	store := newFakeStore()
	clock := &fakeClock{t: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	loader := &fakeLoader{}
	r, storageDir := newTestRestorer(t, store, loader, func(c *RestorerConfig) {
		c.Now = clock.Now
		c.NotFoundTTL = 5 * time.Minute
	})

	res, err := r.EnsureDay(context.Background(), "20261105")
	if err != nil || res != ResultNotFound {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
	for range 10 {
		res, err = r.EnsureDay(context.Background(), "20261105")
		if err != nil || res != ResultCachedNotFound {
			t.Fatalf("unexpected result %q, %v", res, err)
		}
	}
	if n := store.findCalls.Load(); n != 1 {
		t.Fatalf("the store must be queried once while the negative cache is valid; got %d calls", n)
	}

	// The cache expires.
	clock.Add(5*time.Minute + time.Second)
	res, err = r.EnsureDay(context.Background(), "20261105")
	if err != nil || res != ResultNotFound {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
	if n := store.findCalls.Load(); n != 2 {
		t.Fatalf("the store must be queried again after the cache expires; got %d calls", n)
	}

	// A backup uploaded later is picked up after the cache expires.
	store.put("20261105", makeSnapshotTar(t, "snap"))
	res, _ = r.EnsureDay(context.Background(), "20261105")
	if res != ResultCachedNotFound {
		t.Fatalf("unexpected result %q", res)
	}
	clock.Add(6 * time.Minute)
	res, err = r.EnsureDay(context.Background(), "20261105")
	if err != nil || res != ResultRestored {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
	if len(loader.calls()) != 1 {
		t.Fatalf("unexpected Load calls: %v", loader.calls())
	}
	assertTmpEmpty(t, storageDir)
}

func TestRestorerLookupErrorIsCachedBriefly(t *testing.T) {
	store := newFakeStore()
	store.findErr = errors.New("S3 is down")
	clock := &fakeClock{t: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	r, _ := newTestRestorer(t, store, nil, func(c *RestorerConfig) {
		c.Now = clock.Now
		c.FailureTTL = time.Minute
	})
	if res, err := r.EnsureDay(context.Background(), "20261104"); err == nil || res != ResultFailed {
		t.Fatalf("expecting failure; got %q, %v", res, err)
	}
	if res, _ := r.EnsureDay(context.Background(), "20261104"); res != ResultCachedFailure {
		t.Fatalf("unexpected result %q", res)
	}
	clock.Add(time.Minute + time.Second)
	if res, _ := r.EnsureDay(context.Background(), "20261104"); res != ResultFailed {
		t.Fatalf("unexpected result %q", res)
	}
	if n := store.findCalls.Load(); n != 2 {
		t.Fatalf("unexpected FindBackup calls: %d", n)
	}
}

func TestRestorerMaxConcurrentDownloads(t *testing.T) {
	store := newFakeStore()
	days := []string{"20261101", "20261102", "20261103", "20261104", "20261105", "20261106"}
	for _, d := range days {
		store.put(d, makeSnapshotTar(t, "snap-"+d))
	}
	store.openGate = make(chan struct{})
	loader := &fakeLoader{}
	r, storageDir := newTestRestorer(t, store, loader, func(c *RestorerConfig) {
		c.MaxConcurrentDownloads = 2
	})
	done := make(chan struct{})
	go func() {
		r.EnsureDays(context.Background(), days)
		close(done)
	}()
	waitFor(t, func() bool { return store.openCalls.Load() == 2 })
	time.Sleep(100 * time.Millisecond)
	if n := store.openCalls.Load(); n != 2 {
		t.Fatalf("expecting 2 concurrent downloads; got %d", n)
	}
	close(store.openGate)
	<-done
	if m := store.maxActive.Load(); m > 2 {
		t.Fatalf("too many concurrent downloads: %d", m)
	}
	for _, d := range days {
		if _, err := os.Stat(filepath.Join(storageDir, "partitions", d, "datadb")); err != nil {
			t.Fatalf("partition %s must be restored: %s", d, err)
		}
	}
	if len(loader.calls()) != len(days) {
		t.Fatalf("unexpected Load calls: %v", loader.calls())
	}
}

func TestRestorerCorruptBackupLeavesNothing(t *testing.T) {
	store := newFakeStore()
	data := makeSnapshotTar(t, "snap")
	// Corrupt a header checksum in the middle of the archive.
	corrupt := append([]byte(nil), data...)
	copy(corrupt[1024+148:], "garbage!")
	store.put("20261104", corrupt)
	loader := &fakeLoader{}
	r, storageDir := newTestRestorer(t, store, loader, nil)
	res, err := r.EnsureDay(context.Background(), "20261104")
	if err == nil || res != ResultFailed {
		t.Fatalf("expecting failure; got %q, %v", res, err)
	}
	assertNoPartition(t, storageDir, "20261104")
	assertTmpEmpty(t, storageDir)
	if len(loader.calls()) != 0 {
		t.Fatalf("Load must not be called")
	}
}

func TestRestorerPathTraversalBackupLeavesNothing(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"snap/indexdb/", "snap/datadb/"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "../../evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("evil"))
	_ = tw.Close()

	store := newFakeStore()
	store.put("20261104", buf.Bytes())
	r, storageDir := newTestRestorer(t, store, &fakeLoader{}, nil)
	if res, err := r.EnsureDay(context.Background(), "20261104"); err == nil || res != ResultFailed {
		t.Fatalf("expecting failure; got %q, %v", res, err)
	}
	assertNoPartition(t, storageDir, "20261104")
	assertTmpEmpty(t, storageDir)
	if _, err := os.Stat(filepath.Join(filepath.Dir(storageDir), "evil")); err == nil {
		t.Fatalf("archive entry escaped the storage directory")
	}
}

func TestRestorerTruncatedDownload(t *testing.T) {
	store := newFakeStore()
	store.put("20261104", makeSnapshotTar(t, "snap"))
	store.truncate = true
	r, storageDir := newTestRestorer(t, store, &fakeLoader{}, nil)
	if res, err := r.EnsureDay(context.Background(), "20261104"); err == nil || res != ResultFailed {
		t.Fatalf("expecting failure; got %q, %v", res, err)
	}
	assertNoPartition(t, storageDir, "20261104")
	assertTmpEmpty(t, storageDir)
}

func TestRestorerUnexpectedLayout(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "foo/bar.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
	_, _ = tw.Write([]byte("bar"))
	_ = tw.Close()
	store := newFakeStore()
	store.put("20261104", buf.Bytes())
	r, storageDir := newTestRestorer(t, store, &fakeLoader{}, nil)
	if res, err := r.EnsureDay(context.Background(), "20261104"); err == nil || res != ResultFailed {
		t.Fatalf("expecting failure; got %q, %v", res, err)
	}
	assertNoPartition(t, storageDir, "20261104")
	assertTmpEmpty(t, storageDir)
}

func TestRestorerLoadFailure(t *testing.T) {
	for _, remove := range []bool{true, false} {
		store := newFakeStore()
		store.put("20261104", makeSnapshotTar(t, "snap"))
		loader := &fakeLoader{err: errors.New("cannot attach")}
		r, storageDir := newTestRestorer(t, store, loader, func(c *RestorerConfig) {
			c.RemoveOnLoadFailure = remove
		})
		res, err := r.EnsureDay(context.Background(), "20261104")
		if err == nil || res != ResultFailed {
			t.Fatalf("expecting failure; got %q, %v", res, err)
		}
		_, statErr := os.Stat(filepath.Join(storageDir, "partitions", "20261104"))
		if remove && !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("partition must be removed after load failure; stat error: %v", statErr)
		}
		if !remove && statErr != nil {
			t.Fatalf("partition must be kept after load failure: %v", statErr)
		}
		assertTmpEmpty(t, storageDir)
	}
}

func TestRestorerCleansTmpOnStartup(t *testing.T) {
	dir := t.TempDir()
	leftover := filepath.Join(dir, restoreTmpDirname, "20261104-123", "snap", "indexdb")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRestorer(context.Background(), RestorerConfig{StorageDir: dir, Store: newFakeStore()}); err != nil {
		t.Fatal(err)
	}
	assertTmpEmpty(t, dir)
	if fi, err := os.Stat(filepath.Join(dir, partitionsDirname)); err != nil || !fi.IsDir() {
		t.Fatalf("partitions directory must be created: %v", err)
	}
}

func TestRestorerRejectsInvalidDay(t *testing.T) {
	r, _ := newTestRestorer(t, newFakeStore(), nil, nil)
	for _, day := range []string{"", "../../x", "2026-11-04", "20261332"} {
		if _, err := r.EnsureDay(context.Background(), day); err == nil {
			t.Fatalf("expecting error for day %q", day)
		}
	}
}

func TestRestorerCallerCancel(t *testing.T) {
	store := newFakeStore()
	store.put("20261104", makeSnapshotTar(t, "snap"))
	store.openGate = make(chan struct{})
	loader := &fakeLoader{}
	r, storageDir := newTestRestorer(t, store, loader, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := r.EnsureDay(ctx, "20261104")
		errCh <- err
	}()
	waitFor(t, func() bool { return store.openCalls.Load() == 1 })
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("expecting context.Canceled; got %v", err)
	}
	// The restore continues in the background for other callers.
	close(store.openGate)
	res, err := r.EnsureDay(context.Background(), "20261104")
	if err != nil || (res != ResultRestored && res != ResultExists) {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(storageDir, "partitions", "20261104")); err != nil {
		t.Fatal(err)
	}
	if store.openCalls.Load() != 1 {
		t.Fatalf("the restore must not be repeated")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for condition")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRestorerRetriesLoadOfKeptPartition(t *testing.T) {
	store := newFakeStore()
	store.put("20261104", makeSnapshotTar(t, "snap"))
	clock := &fakeClock{t: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	loader := &fakeLoader{err: errors.New("attach failed")}
	r, storageDir := newTestRestorer(t, store, loader, func(c *RestorerConfig) {
		c.Now = clock.Now
		c.FailureTTL = time.Minute
		c.RemoveOnLoadFailure = false
	})

	if res, err := r.EnsureDay(context.Background(), "20261104"); err == nil || res != ResultFailed {
		t.Fatalf("expecting failure; got %q, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(storageDir, "partitions", "20261104")); err != nil {
		t.Fatalf("the partition must be kept: %s", err)
	}
	// Within FailureTTL the load is not retried.
	if res, _ := r.EnsureDay(context.Background(), "20261104"); res != ResultCachedFailure {
		t.Fatalf("unexpected result %q", res)
	}
	if n := len(loader.calls()); n != 1 {
		t.Fatalf("unexpected Load calls: %d", n)
	}

	// After FailureTTL the load is retried; it fails again.
	clock.Add(time.Minute + time.Second)
	if res, err := r.EnsureDay(context.Background(), "20261104"); err == nil || res != ResultFailed {
		t.Fatalf("expecting failure; got %q, %v", res, err)
	}
	if n := len(loader.calls()); n != 2 {
		t.Fatalf("unexpected Load calls: %d", n)
	}

	// Now it succeeds.
	clock.Add(time.Minute + time.Second)
	loader.mu.Lock()
	loader.err = nil
	loader.mu.Unlock()
	if res, err := r.EnsureDay(context.Background(), "20261104"); err != nil || res != ResultLoaded {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
	// Afterwards the partition is just present.
	if res, err := r.EnsureDay(context.Background(), "20261104"); err != nil || res != ResultExists {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
	if n := len(loader.calls()); n != 3 {
		t.Fatalf("unexpected Load calls: %d", n)
	}
	if n := store.openCalls.Load(); n != 1 {
		t.Fatalf("the backup must be downloaded once; got %d downloads", n)
	}
}

func TestRestorerMarkUnloaded(t *testing.T) {
	loader := &fakeLoader{}
	r, storageDir := newTestRestorer(t, newFakeStore(), loader, nil)
	for _, day := range []string{"20261103", "20261104"} {
		if err := os.MkdirAll(filepath.Join(storageDir, "partitions", day, "indexdb"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(storageDir, "partitions", "not-a-partition"), 0o755); err != nil {
		t.Fatal(err)
	}
	local, err := r.LocalPartitions()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(local, []string{"20261103", "20261104"}) {
		t.Fatalf("unexpected local partitions: %v", local)
	}
	r.MarkUnloaded([]string{"20261103"})
	if res, err := r.EnsureDay(context.Background(), "20261103"); err != nil || res != ResultLoaded {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
	if res, err := r.EnsureDay(context.Background(), "20261104"); err != nil || res != ResultExists {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
	if got := loader.calls(); !slices.Equal(got, []string{"20261103"}) {
		t.Fatalf("unexpected Load calls: %v", got)
	}
}

func TestRestorerSkipsDaysOutsideRetention(t *testing.T) {
	store := newFakeStore()
	store.put("20261020", makeSnapshotTar(t, "snap"))
	store.put("20261104", makeSnapshotTar(t, "snap"))
	clock := &fakeClock{t: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	r, _ := newTestRestorer(t, store, &fakeLoader{}, func(c *RestorerConfig) { c.Now = clock.Now })
	r.SetRetention(7 * 24 * time.Hour)
	if res, err := r.EnsureDay(context.Background(), "20261020"); err != nil || res != ResultOutsideRetention {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
	if res, _ := r.EnsureDay(context.Background(), "20261020"); res != ResultOutsideRetention {
		t.Fatalf("unexpected cached result %q", res)
	}
	if res, err := r.EnsureDay(context.Background(), "20261104"); err != nil || res != ResultRestored {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
	if n := store.findCalls.Load(); n != 1 {
		t.Fatalf("days outside the retention must not be looked up; got %d lookups", n)
	}
	// Without a known retention every day is restored.
	r2, _ := newTestRestorer(t, store, &fakeLoader{}, func(c *RestorerConfig) { c.Now = clock.Now })
	if res, err := r2.EnsureDay(context.Background(), "20261020"); err != nil || res != ResultRestored {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
}

func TestRestorerRootLayoutPermissions(t *testing.T) {
	// The partition contents are at the archive root, so the temporary directory itself is moved into place.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"./", "indexdb/", "datadb/"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "datadb/parts.json", Typeflag: tar.TypeReg, Mode: 0o600, Size: 2}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("[]"))
	_ = tw.Close()
	store := newFakeStore()
	store.put("20261104", buf.Bytes())
	r, storageDir := newTestRestorer(t, store, &fakeLoader{}, nil)
	if res, err := r.EnsureDay(context.Background(), "20261104"); err != nil || res != ResultRestored {
		t.Fatalf("unexpected result %q, %v", res, err)
	}
	fi, err := os.Stat(filepath.Join(storageDir, "partitions", "20261104"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Fatalf("unexpected partition directory permissions %o; want 755", perm)
	}
	assertTmpEmpty(t, storageDir)
}

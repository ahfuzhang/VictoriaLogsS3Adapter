package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/pathtmpl"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/s3store"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/vlclient"
)

// fakeVL emulates the VictoriaLogs snapshot API on top of a local storage
// directory, while reporting paths under a different root (as VictoriaLogs
// running in a container would).
type fakeVL struct {
	mu         sync.Mutex
	storageDir string
	vlRoot     string
	counter    int

	forceFlushErr error
	createErr     error
	createNothing bool
	deleteErr     error
	afterCreate   func(localDir string)

	deleted []string
}

func (f *fakeVL) ForceFlush(ctx context.Context) error { return f.forceFlushErr }

func (f *fakeVL) SnapshotList(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dirs, _ := filepath.Glob(filepath.Join(f.storageDir, "partitions", "*", "snapshots", "*"))
	var out []string
	for _, d := range dirs {
		rel, _ := filepath.Rel(f.storageDir, d)
		out = append(out, f.vlRoot+"/"+filepath.ToSlash(rel))
	}
	return out, nil
}

func (f *fakeVL) SnapshotCreate(ctx context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createNothing {
		return nil, nil
	}
	ptDir := filepath.Join(f.storageDir, "partitions", prefix)
	if _, err := os.Stat(ptDir); err != nil {
		return []string{}, nil
	}
	f.counter++
	name := fmt.Sprintf("20261105094918-%016X", f.counter)
	dst := filepath.Join(ptDir, "snapshots", name)
	for _, sub := range []string{"datadb", "indexdb"} {
		if err := os.CopyFS(filepath.Join(dst, sub), os.DirFS(filepath.Join(ptDir, sub))); err != nil {
			return nil, err
		}
	}
	if f.afterCreate != nil {
		f.afterCreate(dst)
	}
	return []string{f.vlRoot + "/partitions/" + prefix + "/snapshots/" + name}, nil
}

func (f *fakeVL) SnapshotDelete(ctx context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	rel, ok := strings.CutPrefix(path, f.vlRoot+"/")
	if !ok {
		return &vlclient.HTTPError{StatusCode: 400, Body: "bad path"}
	}
	local := filepath.Join(f.storageDir, filepath.FromSlash(rel))
	if _, err := os.Stat(local); err != nil {
		return &vlclient.HTTPError{StatusCode: 400, Body: "snapshot doesn't exist"}
	}
	f.deleted = append(f.deleted, path)
	return os.RemoveAll(local)
}

// fakeS3 stores uploaded objects in memory.
type fakeS3 struct {
	mu        sync.Mutex
	objects   map[string][]byte
	failAfter int64 // > 0: fail after reading this many bytes without draining the reader
	statSize  int64 // > 0: report this size from Stat instead of the real one
}

var errSimulatedUpload = errors.New("simulated upload failure")

func (s *fakeS3) Upload(ctx context.Context, key string, r io.Reader, contentType string) (int64, error) {
	if contentType != TarContentType {
		return 0, fmt.Errorf("unexpected content type %q", contentType)
	}
	var buf bytes.Buffer
	if s.failAfter > 0 {
		_, _ = io.CopyN(&buf, r, s.failAfter)
		return 0, fmt.Errorf("cannot upload %s: %w", key, errSimulatedUpload)
	}
	n, err := io.Copy(&buf, r)
	if err != nil {
		return 0, fmt.Errorf("cannot upload %s: %w", key, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = map[string][]byte{}
	}
	s.objects[key] = buf.Bytes()
	return n, nil
}

func (s *fakeS3) Stat(ctx context.Context, key string) (s3store.ObjectInfo, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	if !ok {
		return s3store.ObjectInfo{}, false, nil
	}
	size := int64(len(b))
	if s.statSize > 0 {
		size = s.statSize
	}
	return s3store.ObjectInfo{Key: key, Size: size}, true, nil
}

type testEnv struct {
	storage string
	vl      *fakeVL
	s3      *fakeS3
	b       *Backuper
}

const testDate = "20261104"

func newTestEnv(t *testing.T, prefix string, bigFileSize int) *testEnv {
	t.Helper()
	storage := t.TempDir()
	pt := filepath.Join(storage, "partitions", testDate)
	writeFile(t, filepath.Join(pt, "datadb", "parts.json"), `["P1"]`, 0o644)
	writeFile(t, filepath.Join(pt, "datadb", "P1", "metadata.json"), `{"rows":3}`, 0o644)
	writeFile(t, filepath.Join(pt, "datadb", "P1", "big.bin"), strings.Repeat("x", bigFileSize), 0o644)
	writeFile(t, filepath.Join(pt, "indexdb", "parts.json"), `[]`, 0o644)
	// A pre-existing snapshot must be left untouched.
	writeFile(t, filepath.Join(pt, "snapshots", "20261101000000-0000000000000001", "datadb", "parts.json"), `[]`, 0o644)
	// Another partition.
	writeFile(t, filepath.Join(storage, "partitions", "20261103", "datadb", "parts.json"), `[]`, 0o644)

	tmpl, err := pathtmpl.Parse(prefix)
	if err != nil {
		t.Fatal(err)
	}
	vl := &fakeVL{storageDir: storage, vlRoot: "/data"}
	s3 := &fakeS3{}
	return &testEnv{
		storage: storage,
		vl:      vl,
		s3:      s3,
		b: &Backuper{
			VL:         vl,
			S3:         s3,
			Bucket:     "test",
			Prefix:     tmpl,
			StorageDir: storage,
			Logger:     discardLogger,
			now:        func() time.Time { return time.Date(2026, 11, 5, 0, 15, 0, 0, time.UTC) },
		},
	}
}

func (e *testEnv) snapshots(t *testing.T) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(e.storage, "partitions", "*", "snapshots", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func (e *testEnv) assertOnlyOldSnapshotLeft(t *testing.T) {
	t.Helper()
	snaps := e.snapshots(t)
	if len(snaps) != 1 || filepath.Base(snaps[0]) != "20261101000000-0000000000000001" {
		t.Fatalf("unexpected snapshots left: %q", snaps)
	}
}

func waitForGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutine leak: %d goroutines, %d before\n%s", runtime.NumGoroutine(), base, buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBackupSuccess(t *testing.T) {
	t.Setenv("BACKUP_TEST_ENV", "prod")
	e := newTestEnv(t, "/victoria_logs/{BACKUP_TEST_ENV}/", 100_000)
	base := runtime.NumGoroutine()

	res, err := e.b.Backup(context.Background(), testDate)
	if err != nil {
		t.Fatalf("Backup: %s", err)
	}
	waitForGoroutines(t, base)

	if res.Key != "victoria_logs/prod/20261104.tar" {
		t.Fatalf("key = %q", res.Key)
	}
	if res.Snapshot != "/data/partitions/20261104/snapshots/20261105094918-0000000000000001" {
		t.Fatalf("snapshot = %q", res.Snapshot)
	}
	if want := filepath.Join(e.storage, "partitions", testDate, "snapshots", "20261105094918-0000000000000001"); res.LocalDir != want {
		t.Fatalf("local dir = %q; want %q", res.LocalDir, want)
	}
	obj := e.s3.objects[res.Key]
	if int64(len(obj)) != res.TarBytes || res.TarBytes == 0 {
		t.Fatalf("object size %d; result bytes %d", len(obj), res.TarBytes)
	}
	if res.Files != 4 || res.Dirs != 4 || res.DataBytes != int64(len(`["P1"]`)+len(`{"rows":3}`)+100_000+2) {
		t.Fatalf("unexpected result: %+v", res)
	}

	entries := readTar(t, obj)
	var names []string
	for _, en := range entries {
		names = append(names, en.name)
		if en.name == "20261105094918-0000000000000001/datadb/P1/big.bin" && en.content != strings.Repeat("x", 100_000) {
			t.Fatalf("big.bin content mismatch")
		}
	}
	want := []string{
		"20261105094918-0000000000000001/",
		"20261105094918-0000000000000001/datadb/",
		"20261105094918-0000000000000001/datadb/P1/",
		"20261105094918-0000000000000001/datadb/P1/big.bin",
		"20261105094918-0000000000000001/datadb/P1/metadata.json",
		"20261105094918-0000000000000001/datadb/parts.json",
		"20261105094918-0000000000000001/indexdb/",
		"20261105094918-0000000000000001/indexdb/parts.json",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tar entries:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}

	if len(e.vl.deleted) != 1 || e.vl.deleted[0] != res.Snapshot {
		t.Fatalf("deleted snapshots via API: %q", e.vl.deleted)
	}
	e.assertOnlyOldSnapshotLeft(t)
}

func TestBackupTimePrefix(t *testing.T) {
	e := newTestEnv(t, "bk/${YYYYMMDD_HHMISS}", 10)
	res, err := e.b.Backup(context.Background(), testDate)
	if err != nil {
		t.Fatalf("Backup: %s", err)
	}
	if res.Key != "bk/20261105_001500/20261104.tar" {
		t.Fatalf("key = %q", res.Key)
	}
}

func TestBackupDeleteFallback(t *testing.T) {
	e := newTestEnv(t, "", 10)
	e.vl.deleteErr = &vlclient.HTTPError{StatusCode: 404, Body: "unsupported path requested"}
	e.vl.forceFlushErr = &vlclient.HTTPError{StatusCode: 404}
	res, err := e.b.Backup(context.Background(), testDate)
	if err != nil {
		t.Fatalf("Backup: %s", err)
	}
	if res.Key != "20261104.tar" {
		t.Fatalf("key = %q", res.Key)
	}
	if len(e.vl.deleted) != 0 {
		t.Fatalf("unexpected API deletes: %q", e.vl.deleted)
	}
	e.assertOnlyOldSnapshotLeft(t)
}

func TestBackupUploadFailure(t *testing.T) {
	e := newTestEnv(t, "p", 4<<20)
	e.s3.failAfter = 10_000
	base := runtime.NumGoroutine()

	_, err := e.b.Backup(context.Background(), testDate)
	if !errors.Is(err, errSimulatedUpload) {
		t.Fatalf("expected the upload error; got %v", err)
	}
	waitForGoroutines(t, base)
	if len(e.s3.objects) != 0 {
		t.Fatalf("unexpected objects: %d", len(e.s3.objects))
	}
	e.assertOnlyOldSnapshotLeft(t)
}

func TestBackupTarFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions are not enforced for root")
	}
	e := newTestEnv(t, "p", 1000)
	e.vl.afterCreate = func(dir string) {
		if err := os.Chmod(filepath.Join(dir, "datadb", "P1", "metadata.json"), 0); err != nil {
			t.Error(err)
		}
	}
	base := runtime.NumGoroutine()
	_, err := e.b.Backup(context.Background(), testDate)
	if err == nil || !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "cannot build tar stream") {
		t.Fatalf("expected a tar permission error; got %v", err)
	}
	waitForGoroutines(t, base)
	if len(e.s3.objects) != 0 {
		t.Fatalf("an incomplete object was stored")
	}
	e.assertOnlyOldSnapshotLeft(t)
}

func TestBackupSizeMismatch(t *testing.T) {
	e := newTestEnv(t, "p", 1000)
	e.s3.statSize = 1
	_, err := e.b.Backup(context.Background(), testDate)
	if err == nil || !strings.Contains(err.Error(), "has size 1") {
		t.Fatalf("expected a size verification error; got %v", err)
	}
	e.assertOnlyOldSnapshotLeft(t)
}

func TestBackupPartitionNotFound(t *testing.T) {
	e := newTestEnv(t, "p", 10)
	_, err := e.b.Backup(context.Background(), "20261001")
	if !errors.Is(err, ErrPartitionNotFound) {
		t.Fatalf("expected ErrPartitionNotFound; got %v", err)
	}
}

func TestBackupNoSnapshot(t *testing.T) {
	e := newTestEnv(t, "p", 10)
	e.vl.createNothing = true
	_, err := e.b.Backup(context.Background(), testDate)
	if !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("expected ErrNoSnapshot; got %v", err)
	}
}

func TestBackupCreateError(t *testing.T) {
	e := newTestEnv(t, "p", 10)
	e.vl.createErr = &vlclient.HTTPError{StatusCode: 401, Body: "unauthorized"}
	_, err := e.b.Backup(context.Background(), testDate)
	if !vlclient.IsStatus(err, 401) {
		t.Fatalf("expected the create error; got %v", err)
	}
}

func TestBackupSnapshotNotFoundLocally(t *testing.T) {
	e := newTestEnv(t, "p", 10)
	// VictoriaLogs reports a snapshot that is not visible under the local storage dir.
	e.vl.afterCreate = func(dir string) {
		if err := os.Rename(dir, filepath.Join(e.storage, "moved")); err != nil {
			t.Error(err)
		}
	}
	e.vl.deleteErr = &vlclient.HTTPError{StatusCode: 400}
	_, err := e.b.Backup(context.Background(), testDate)
	if err == nil || !strings.Contains(err.Error(), "not found locally") {
		t.Fatalf("expected a 'not found locally' error; got %v", err)
	}
}

func TestBackupRateLimit(t *testing.T) {
	// The limiter allows a burst of up to 256KiB, so ~768KiB at 512KiB/s takes ~1s.
	e := newTestEnv(t, "p", 768<<10)
	e.b.BytesPerSecond = 512 << 10
	e.b.now = nil
	start := time.Now()
	res, err := e.b.Backup(context.Background(), testDate)
	if err != nil {
		t.Fatalf("Backup: %s", err)
	}
	el := time.Since(start)
	want := time.Duration(float64(res.TarBytes-256<<10) / float64(512<<10) * float64(time.Second))
	if el < want*8/10 || el > want*3+time.Second {
		t.Fatalf("uploading %d bytes at 512KiB/s took %s; expected about %s", res.TarBytes, el, want)
	}
	t.Logf("uploaded %d bytes in %s (expected about %s)", res.TarBytes, el, want)
}

func TestBackupCanceledDuringUpload(t *testing.T) {
	e := newTestEnv(t, "p", 4<<20)
	e.b.BytesPerSecond = 64 << 10
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := e.b.Backup(ctx, testDate)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled; got %v", err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("cancellation took %s", el)
	}
	waitForGoroutines(t, base)
	// The snapshot is removed even though ctx is canceled.
	e.assertOnlyOldSnapshotLeft(t)
}

func TestFormatBytes(t *testing.T) {
	tests := map[float64]string{
		0:                 "0 B",
		1023:              "1023 B",
		1024:              "1.00 KiB",
		1536:              "1.50 KiB",
		100 * 1024 * 1024: "100.00 MiB",
		3 << 30:           "3.00 GiB",
	}
	for n, want := range tests {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%v) = %q; want %q", n, got, want)
		}
	}
}

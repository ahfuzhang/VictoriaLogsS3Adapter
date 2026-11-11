package restore

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/ratelimit"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/restore/untar"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/restore/vltime"
)

// Store locates and opens partition backups.
type Store interface {
	// FindBackup returns the object key of the backup for the partition day (YYYYMMDD).
	FindBackup(ctx context.Context, day string) (key string, found bool, err error)
	// Open opens the object for reading and returns its size.
	Open(ctx context.Context, key string) (io.ReadCloser, int64, error)
}

// Loader makes VictoriaLogs serve a partition directory which has just been
// placed at <storage>/partitions/<day>.
type Loader interface {
	Load(ctx context.Context, day string) error
}

// Directory names under the storage dir.
const (
	partitionsDirname = "partitions"
	restoreTmpDirname = ".restore_tmp"
)

// RestorerConfig configures a Restorer.
type RestorerConfig struct {
	// StorageDir is the VictoriaLogs -storageDataPath.
	StorageDir string
	Store      Store
	Loader     Loader
	// BytesPerSecond limits the download speed of every backup; <= 0 means unlimited.
	BytesPerSecond int64
	// MaxConcurrentDownloads limits the number of concurrent backup downloads. Defaults to 2.
	MaxConcurrentDownloads int
	// MaxConcurrentLookups limits the number of concurrent backup lookups in the store. Defaults to 8.
	MaxConcurrentLookups int
	// NotFoundTTL is how long "no backup for this day" results are cached. Defaults to 5 minutes.
	NotFoundTTL time.Duration
	// FailureTTL is how long a failed restore is not retried. Defaults to 1 minute.
	FailureTTL time.Duration
	// RemoveOnLoadFailure removes the restored partition directory when Loader fails,
	// so VictoriaLogs never sees a partition it cannot open.
	RemoveOnLoadFailure bool
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

// Restorer downloads partition backups from the store on demand.
type Restorer struct {
	cfg           RestorerConfig
	partitionsDir string
	tmpDir        string

	// baseCtx is used for restores, so they aren't canceled when the client that triggered them goes away.
	baseCtx context.Context

	sf        singleflight.Group
	downloads chan struct{}
	lookups   chan struct{}

	mu       sync.Mutex
	negative map[string]negativeEntry
	// unloaded holds days whose partition directory is in place, but Loader failed for it.
	// Loading is retried when such a day is requested again (after FailureTTL).
	unloaded map[string]struct{}
	// downloadedAt maps days whose partition directory is in place to the time it was downloaded.
	// The time is persisted as the modification time of the directory, so it is
	// read back from the partitions directory on startup.
	downloadedAt map[string]time.Time

	// retention is the retention of the VictoriaLogs instance in nanoseconds; 0 means unknown.
	// Days VictoriaLogs would drop because of retention are not restored.
	retention atomic.Int64

	// Counters for observability and tests.
	lookupsTotal   atomic.Int64
	downloadsTotal atomic.Int64
}

type negativeEntry struct {
	expires time.Time
	reason  string
}

// Result describes what EnsureDay did.
type Result string

// Possible results of EnsureDay.
const (
	ResultExists   Result = "exists"
	ResultRestored Result = "restored"
	// ResultLoaded means the partition directory existed, but was not loaded into VictoriaLogs yet; now it is.
	ResultLoaded         Result = "loaded"
	ResultNotFound       Result = "not_found"
	ResultCachedNotFound Result = "cached_not_found"
	ResultCachedFailure  Result = "cached_failure"
	// ResultOutsideRetention means the day is older than the VictoriaLogs retention, so it is not restored.
	ResultOutsideRetention Result = "outside_retention"
	ResultFailed           Result = "failed"
)

// NewRestorer creates the partitions and temporary directories and removes
// leftovers of interrupted restores. baseCtx bounds the lifetime of restores.
func NewRestorer(baseCtx context.Context, cfg RestorerConfig) (*Restorer, error) {
	if cfg.Store == nil {
		return nil, errors.New("BUG: Store must be set")
	}
	if cfg.MaxConcurrentDownloads <= 0 {
		cfg.MaxConcurrentDownloads = 2
	}
	if cfg.MaxConcurrentLookups <= 0 {
		cfg.MaxConcurrentLookups = 8
	}
	if cfg.NotFoundTTL <= 0 {
		cfg.NotFoundTTL = 5 * time.Minute
	}
	if cfg.FailureTTL <= 0 {
		cfg.FailureTTL = time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	r := &Restorer{
		cfg:           cfg,
		partitionsDir: filepath.Join(cfg.StorageDir, partitionsDirname),
		tmpDir:        filepath.Join(cfg.StorageDir, restoreTmpDirname),
		baseCtx:       baseCtx,
		downloads:     make(chan struct{}, cfg.MaxConcurrentDownloads),
		lookups:       make(chan struct{}, cfg.MaxConcurrentLookups),
		negative:      make(map[string]negativeEntry),
		unloaded:      make(map[string]struct{}),
		downloadedAt:  make(map[string]time.Time),
	}
	if err := os.MkdirAll(r.partitionsDir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create partitions directory: %w", err)
	}
	if err := os.RemoveAll(r.tmpDir); err != nil {
		return nil, fmt.Errorf("cannot clean up temporary restore directory: %w", err)
	}
	if err := os.MkdirAll(r.tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create temporary restore directory: %w", err)
	}
	if err := r.scanDownloadTimes(); err != nil {
		return nil, fmt.Errorf("cannot scan partitions directory: %w", err)
	}
	return r, nil
}

// scanDownloadTimes reads the download times of the partition directories in
// place from their modification times, which restore sets to the download time.
// Times in the future (e.g. clock changes) are clamped to now.
func (r *Restorer) scanDownloadTimes() error {
	des, err := os.ReadDir(r.partitionsDir)
	if err != nil {
		return err
	}
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, de := range des {
		day := de.Name()
		if !de.IsDir() || !vltime.IsPartitionName(day) {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		t := fi.ModTime()
		if t.After(now) {
			t = now
		}
		r.downloadedAt[day] = t
	}
	if len(r.downloadedAt) > 0 {
		slog.Info("found local partitions", "partitions", len(r.downloadedAt))
	}
	return nil
}

// DownloadTimes returns the download times of the partition directories in place, keyed by day (YYYYMMDD).
func (r *Restorer) DownloadTimes() map[string]time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.downloadedAt)
}

// DownloadedBefore returns the days (in ascending order) whose partitions were
// downloaded at or before cutoff.
func (r *Restorer) DownloadedBefore(cutoff time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.downloadedBeforeLocked(cutoff)
}

func (r *Restorer) downloadedBeforeLocked(cutoff time.Time) []string {
	var days []string
	for day, t := range r.downloadedAt {
		if !t.After(cutoff) {
			days = append(days, day)
		}
	}
	slices.Sort(days)
	return days
}

// RemoveDownloadedBefore removes the partitions downloaded at or before cutoff
// and returns their days. It must be called only while VictoriaLogs is stopped.
//
// The partition directories are moved into the temporary directory and deleted
// in the background, so the call is fast even for big partitions. Leftovers of
// interrupted deletions are removed on the next startup.
func (r *Restorer) RemoveDownloadedBefore(cutoff time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed, trash []string
	for _, day := range r.downloadedBeforeLocked(cutoff) {
		downloadedAt := r.downloadedAt[day]
		dst := filepath.Join(r.tmpDir, "expired-"+day+"-"+randomSuffix())
		if err := os.Rename(r.PartitionPath(day), dst); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				slog.Error("cannot remove expired partition; will retry on the next removal", "date", day, "error", err)
				continue
			}
			// The directory has already been removed by somebody else.
		} else {
			trash = append(trash, dst)
		}
		delete(r.downloadedAt, day)
		delete(r.unloaded, day)
		delete(r.negative, day)
		removed = append(removed, day)
		slog.Info("removed expired partition", "date", day, "downloaded_at", downloadedAt.Format(time.RFC3339),
			"age", r.cfg.Now().Sub(downloadedAt).Round(time.Second).String())
	}
	if len(trash) > 0 {
		if err := untar.SyncDir(r.partitionsDir); err != nil {
			slog.Warn("cannot sync partitions directory", "path", r.partitionsDir, "error", err)
		}
		go func() {
			for _, p := range trash {
				if err := os.RemoveAll(p); err != nil {
					slog.Warn("cannot delete expired partition directory; it is deleted on the next startup", "path", p, "error", err)
				}
			}
		}()
	}
	return removed
}

func (r *Restorer) setDownloadedAt(day string, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.downloadedAt[day] = t
}

func (r *Restorer) forgetDownload(day string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.downloadedAt, day)
}

// SetLoader sets the loader. It must be called before serving requests.
func (r *Restorer) SetLoader(l Loader) { r.cfg.Loader = l }

// SetRetention sets the retention of the VictoriaLogs instance. Days older than
// the retention are not restored, since VictoriaLogs would drop them.
// Zero disables the check.
func (r *Restorer) SetRetention(d time.Duration) {
	r.retention.Store(int64(d))
}

// outsideRetention reports whether VictoriaLogs would drop the partition for day.
func (r *Restorer) outsideRetention(day string) bool {
	retention := time.Duration(r.retention.Load())
	if retention <= 0 {
		return false
	}
	n, err := dayNumber(day)
	if err != nil {
		return false
	}
	return n < minRetainedDay(r.cfg.Now(), retention)
}

// LocalPartitions returns the names of the partition directories under the partitions directory.
func (r *Restorer) LocalPartitions() ([]string, error) {
	des, err := os.ReadDir(r.partitionsDir)
	if err != nil {
		return nil, err
	}
	var days []string
	for _, de := range des {
		if de.IsDir() && vltime.IsPartitionName(de.Name()) {
			days = append(days, de.Name())
		}
	}
	return days, nil
}

// MarkUnloaded records that the partition directories for days exist, but are
// not loaded into VictoriaLogs, so they are loaded when requested.
func (r *Restorer) MarkUnloaded(days []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, day := range days {
		r.unloaded[day] = struct{}{}
	}
}

func (r *Restorer) isUnloaded(day string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.unloaded[day]
	return ok
}

func (r *Restorer) setUnloaded(day string, unloaded bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if unloaded {
		r.unloaded[day] = struct{}{}
	} else {
		delete(r.unloaded, day)
	}
}

// PartitionPath returns the directory of the partition day.
func (r *Restorer) PartitionPath(day string) string {
	return filepath.Join(r.partitionsDir, day)
}

// EnsureDays makes sure the partitions for days are available locally, restoring
// missing ones from the store in parallel. Errors are logged; the caller should
// forward the request anyway.
func (r *Restorer) EnsureDays(ctx context.Context, days []string) {
	if len(days) == 1 {
		_, _ = r.EnsureDay(ctx, days[0])
		return
	}
	var wg sync.WaitGroup
	for _, day := range days {
		wg.Go(func() {
			_, _ = r.EnsureDay(ctx, day)
		})
	}
	wg.Wait()
}

// EnsureDay makes sure the partition for day is available locally.
//
// Concurrent calls for the same day share a single restore. The restore keeps
// running in the background if ctx is canceled.
func (r *Restorer) EnsureDay(ctx context.Context, day string) (Result, error) {
	if !vltime.IsPartitionName(day) {
		return ResultFailed, fmt.Errorf("invalid partition name %q", day)
	}
	ch := r.sf.DoChan(day, func() (any, error) {
		return r.ensureDay(day)
	})
	select {
	case res := <-ch:
		result, _ := res.Val.(Result)
		return result, res.Err
	case <-ctx.Done():
		return ResultFailed, ctx.Err()
	}
}

func (r *Restorer) ensureDay(day string) (Result, error) {
	exists, err := dirExists(r.PartitionPath(day))
	if err != nil {
		slog.Error("cannot check partition directory", "date", day, "error", err)
		return ResultFailed, err
	}
	if exists {
		if !r.isUnloaded(day) {
			return ResultExists, nil
		}
		// The partition was restored earlier, but loading it into VictoriaLogs failed. Retry.
		if res, ok := r.checkNegative(day); ok {
			return res, nil
		}
		if err := r.load(day); err != nil {
			return ResultFailed, err
		}
		return ResultLoaded, nil
	}
	if res, ok := r.checkNegative(day); ok {
		return res, nil
	}
	if r.outsideRetention(day) {
		slog.Warn("the requested day is outside the VictoriaLogs retention; not restoring it, since VictoriaLogs would drop it",
			"date", day, "retention", time.Duration(r.retention.Load()).String())
		r.setNegative(day, r.cfg.NotFoundTTL, "outside_retention")
		return ResultOutsideRetention, nil
	}

	key, found, err := r.findBackup(day)
	if err != nil {
		slog.Error("cannot look up backup", "date", day, "error", err)
		r.setNegative(day, r.cfg.FailureTTL, "lookup_failed")
		return ResultFailed, err
	}
	if !found {
		slog.Info("no backup found for the requested day; forwarding the request as is", "date", day,
			"cache_ttl", r.cfg.NotFoundTTL.String())
		r.setNegative(day, r.cfg.NotFoundTTL, "not_found")
		return ResultNotFound, nil
	}

	if err := r.restore(day, key); err != nil {
		slog.Error("cannot restore partition", "date", day, "key", key, "error", err)
		r.setNegative(day, r.cfg.FailureTTL, "restore_failed")
		return ResultFailed, err
	}

	if err := r.load(day); err != nil {
		return ResultFailed, err
	}
	return ResultRestored, nil
}

// load loads the partition directory of day into VictoriaLogs via Loader.
//
// On failure the directory is either removed (RemoveOnLoadFailure) or kept and
// remembered as unloaded, so loading is retried on a later request. In both
// cases the day is not retried for FailureTTL.
func (r *Restorer) load(day string) error {
	if r.cfg.Loader == nil {
		return nil
	}
	err := r.cfg.Loader.Load(r.baseCtx, day)
	if err == nil {
		r.setUnloaded(day, false)
		return nil
	}
	if r.cfg.RemoveOnLoadFailure {
		slog.Error("cannot load restored partition into VictoriaLogs; removing it", "date", day, "error", err)
		if rerr := r.quarantine(day); rerr != nil {
			slog.Error("cannot remove partition which failed to load", "date", day, "error", rerr)
		}
		r.setUnloaded(day, false)
	} else {
		slog.Error("cannot load restored partition into VictoriaLogs; loading will be retried on a later request",
			"date", day, "retry_after", r.cfg.FailureTTL.String(), "error", err)
		r.setUnloaded(day, true)
	}
	r.setNegative(day, r.cfg.FailureTTL, "load_failed")
	return err
}

func (r *Restorer) findBackup(day string) (string, bool, error) {
	select {
	case r.lookups <- struct{}{}:
	case <-r.baseCtx.Done():
		return "", false, r.baseCtx.Err()
	}
	defer func() { <-r.lookups }()
	r.lookupsTotal.Add(1)
	slog.Debug("looking up backup", "date", day)
	ctx, cancel := context.WithTimeout(r.baseCtx, 2*time.Minute)
	defer cancel()
	return r.cfg.Store.FindBackup(ctx, day)
}

// restore downloads and extracts the backup into a temporary directory and
// atomically moves the partition data to <storage>/partitions/<day>.
func (r *Restorer) restore(day, key string) error {
	select {
	case r.downloads <- struct{}{}:
	case <-r.baseCtx.Done():
		return r.baseCtx.Err()
	}
	defer func() { <-r.downloads }()
	r.downloadsTotal.Add(1)

	startTime := time.Now()
	tmp, err := os.MkdirTemp(r.tmpDir, day+"-")
	if err != nil {
		return fmt.Errorf("cannot create temporary directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmp); err != nil {
			slog.Warn("cannot remove temporary restore directory", "path", tmp, "error", err)
		}
	}()

	ctx, cancel := context.WithCancel(r.baseCtx)
	defer cancel()
	body, size, err := r.cfg.Store.Open(ctx, key)
	if err != nil {
		return err
	}
	defer body.Close()
	slog.Info("restore started", "date", day, "key", key, "size_bytes", size, "max_bytes_per_second", r.cfg.BytesPerSecond)

	cr := &countingReader{r: body}
	stopProgress := r.logProgress(day, key, size, cr, startTime)
	defer stopProgress()

	limited := ratelimit.NewReader(ctx, cr, r.cfg.BytesPerSecond)
	br := bufio.NewReaderSize(limited, 1<<20)
	st, err := untar.Extract(ctx, br, tmp)
	if err != nil {
		return fmt.Errorf("cannot extract backup after downloading %d bytes: %w", cr.n.Load(), err)
	}
	// Read the rest of the object (tar end-of-archive padding) and verify that the
	// whole object has been received, so a silently truncated download is detected.
	if _, err := io.Copy(io.Discard, br); err != nil {
		return fmt.Errorf("cannot read the end of the backup: %w", err)
	}
	if n := cr.n.Load(); size >= 0 && n != size {
		return fmt.Errorf("downloaded %d bytes; expected %d bytes", n, size)
	}

	root, err := untar.FindDataRoot(tmp)
	if err != nil {
		return fmt.Errorf("unexpected backup layout: %w", err)
	}
	dst := r.PartitionPath(day)
	if exists, err := dirExists(dst); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("partition directory %q appeared during the restore", dst)
	}
	// The data root may be the temporary directory itself, which is created with 0700 permissions.
	if err := os.Chmod(root, 0o755); err != nil {
		return fmt.Errorf("cannot set permissions on restored partition: %w", err)
	}
	if err := os.Rename(root, dst); err != nil {
		return fmt.Errorf("cannot move restored partition into place: %w", err)
	}
	// Remember the download time. It is persisted as the modification time of the
	// partition directory (VictoriaLogs doesn't add entries to it), so it survives restarts.
	downloadedAt := r.cfg.Now()
	if err := os.Chtimes(dst, downloadedAt, downloadedAt); err != nil {
		slog.Warn("cannot set the download time on the restored partition", "path", dst, "error", err)
	}
	r.setDownloadedAt(day, downloadedAt)
	// Persist the new directory entry. The partition contents were synced by untar.Extract.
	if err := untar.SyncDir(r.partitionsDir); err != nil {
		slog.Warn("cannot sync partitions directory", "path", r.partitionsDir, "error", err)
	}

	slog.Info("restore finished", "date", day, "key", key, "bytes", cr.n.Load(), "files", st.Files, "dirs", st.Dirs,
		"data_bytes", st.DataBytes, "skipped_entries", st.Skipped, "duration_seconds", time.Since(startTime).Seconds(),
		"path", dst)
	return nil
}

// quarantine removes the partition directory of day. It is renamed into the
// temporary directory first, so VictoriaLogs never sees a partially removed partition.
func (r *Restorer) quarantine(day string) error {
	src := r.PartitionPath(day)
	dst := filepath.Join(r.tmpDir, "failed-"+day+"-"+randomSuffix())
	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			r.forgetDownload(day)
			return nil
		}
		return err
	}
	r.forgetDownload(day)
	slog.Warn("removed restored partition which could not be loaded", "date", day)
	return os.RemoveAll(dst)
}

func (r *Restorer) logProgress(day, key string, size int64, cr *countingReader, startTime time.Time) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				slog.Info("restore in progress", "date", day, "key", key, "bytes", cr.n.Load(), "size_bytes", size,
					"elapsed_seconds", time.Since(startTime).Seconds())
			}
		}
	}()
	return func() { close(done) }
}

func (r *Restorer) checkNegative(day string) (Result, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.negative[day]
	if !ok {
		return "", false
	}
	if r.cfg.Now().After(e.expires) {
		delete(r.negative, day)
		return "", false
	}
	switch e.reason {
	case "not_found":
		return ResultCachedNotFound, true
	case "outside_retention":
		return ResultOutsideRetention, true
	}
	return ResultCachedFailure, true
}

func (r *Restorer) setNegative(day string, ttl time.Duration, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.cfg.Now()
	for d, e := range r.negative {
		if now.After(e.expires) {
			delete(r.negative, d)
		}
	}
	r.negative[day] = negativeEntry{expires: now.Add(ttl), reason: reason}
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n.Add(int64(n))
	return n, err
}

func dirExists(p string) (bool, error) {
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("%q exists, but it is not a directory", p)
	}
	return true, nil
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

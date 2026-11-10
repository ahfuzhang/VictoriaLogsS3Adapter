package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/pathtmpl"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/ratelimit"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/s3store"
)

// TarContentType is the content type of uploaded backup objects.
const TarContentType = "application/x-tar"

var (
	// ErrPartitionNotFound is returned when the partition directory does not exist locally.
	// Retrying does not help in this case.
	ErrPartitionNotFound = errors.New("partition directory not found")
	// ErrNoSnapshot is returned when VictoriaLogs did not create a snapshot for the partition.
	ErrNoSnapshot = errors.New("no new snapshot found")

	errUploadFinished = errors.New("upload finished before the tar stream was fully consumed")
)

// VLClient is the subset of the VictoriaLogs API used for backups.
type VLClient interface {
	ForceFlush(ctx context.Context) error
	SnapshotList(ctx context.Context) ([]string, error)
	SnapshotCreate(ctx context.Context, partitionPrefix string) ([]string, error)
	SnapshotDelete(ctx context.Context, path string) error
}

// Uploader is the subset of the S3 client used for backups.
type Uploader interface {
	Upload(ctx context.Context, key string, r io.Reader, contentType string) (int64, error)
	Stat(ctx context.Context, key string) (s3store.ObjectInfo, bool, error)
}

// Backuper backs up VictoriaLogs partitions into S3.
type Backuper struct {
	VL         VLClient
	S3         Uploader
	Bucket     string // used for logging only
	Prefix     *pathtmpl.Template
	StorageDir string
	// BytesPerSecond limits the upload rate; <= 0 means unlimited.
	BytesPerSecond int64
	Logger         *slog.Logger
	// ProgressInterval is the interval between upload progress log messages; <= 0 disables them.
	ProgressInterval time.Duration
	// CleanupTimeout bounds snapshot deletion. Defaults to one minute.
	CleanupTimeout time.Duration

	// now is used for tests; defaults to time.Now.
	now func() time.Time
}

// Result describes a completed backup.
type Result struct {
	Date           string
	Snapshot       string // path reported by VictoriaLogs
	LocalDir       string
	Key            string
	Files          int
	Dirs           int
	Skipped        int
	DataBytes      int64 // sum of file sizes
	TarBytes       int64 // size of the uploaded object
	Duration       time.Duration
	UploadDuration time.Duration
}

func (b *Backuper) log() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

func (b *Backuper) timeNow() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// Backup snapshots the partition date (YYYYMMDD), streams the snapshot as a tar
// archive to S3 and deletes the snapshot afterwards.
func (b *Backuper) Backup(ctx context.Context, date string) (*Result, error) {
	start := b.timeNow()
	log := b.log().With("date", date)

	partDir := filepath.Join(b.StorageDir, "partitions", date)
	if fi, err := os.Stat(partDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: %q; check that -victoria.logs.storage.dir points to the VictoriaLogs -storageDataPath "+
			"and that logs exist for %s", ErrPartitionNotFound, partDir, date)
	}

	if err := b.VL.ForceFlush(ctx); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Warn("cannot force flush VictoriaLogs; recently ingested logs may be missing from the snapshot", "error", err)
	}

	before, err := b.VL.SnapshotList(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot list snapshots: %w", err)
	}
	created, err := b.VL.SnapshotCreate(ctx, date)
	if err != nil {
		return nil, fmt.Errorf("cannot create snapshot for partition %s: %w", date, err)
	}
	after, err := b.VL.SnapshotList(ctx)
	if err != nil {
		if len(created) == 0 {
			return nil, fmt.Errorf("cannot list snapshots after creating a snapshot for partition %s "+
				"(the snapshot may need manual removal): %w", date, err)
		}
		log.Warn("cannot list snapshots after creating a snapshot; relying on the create response", "error", err)
	}
	snapshot, others, err := selectSnapshot(before, after, created, date)
	if err != nil {
		return nil, err
	}
	if len(others) > 0 {
		log.Warn("several new snapshots found for the partition; using the newest one and leaving the others untouched",
			"snapshot", snapshot, "others", others)
	}
	log = log.With("snapshot", snapshot)
	log.Info("snapshot created")

	localDir, _, mapErr := localSnapshotDir(b.StorageDir, snapshot, date)
	defer b.deleteSnapshot(ctx, log, snapshot, localDir)
	if mapErr != nil {
		return nil, mapErr
	}
	if fi, err := os.Stat(localDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("snapshot directory %q not found locally (reported by VictoriaLogs as %q); "+
			"check that -victoria.logs.storage.dir points to the VictoriaLogs -storageDataPath", localDir, snapshot)
	}

	key, err := s3store.BackupKey(b.Prefix, start, date)
	if err != nil {
		return nil, err
	}
	log = log.With("key", key)
	log.Info("uploading snapshot", "local_dir", localDir, "bucket", b.Bucket, "max_upload_bytes_per_second", b.BytesPerSecond)

	uploadStart := b.timeNow()
	st, tarBytes, err := b.upload(ctx, log, key, localDir, date)
	if err != nil {
		if ctx.Err() != nil {
			log.Debug("upload interrupted", "error", err)
			return nil, fmt.Errorf("backup of partition %s interrupted: %w", date, ctx.Err())
		}
		return nil, err
	}
	if err := b.verify(ctx, key, tarBytes); err != nil {
		return nil, err
	}
	end := b.timeNow()

	res := &Result{
		Date:           date,
		Snapshot:       snapshot,
		LocalDir:       localDir,
		Key:            key,
		Files:          st.Files,
		Dirs:           st.Dirs,
		Skipped:        st.Skipped,
		DataBytes:      st.DataBytes,
		TarBytes:       tarBytes,
		Duration:       end.Sub(start),
		UploadDuration: end.Sub(uploadStart),
	}
	rate := bytesPerSecond(res.TarBytes, res.UploadDuration)
	log.Info("backup completed",
		"bucket", b.Bucket,
		"files", res.Files,
		"dirs", res.Dirs,
		"skipped", res.Skipped,
		"data_bytes", res.DataBytes,
		"bytes", res.TarBytes,
		"size", formatBytes(float64(res.TarBytes)),
		"duration_seconds", res.Duration.Seconds(),
		"upload_duration_seconds", res.UploadDuration.Seconds(),
		"throughput_bytes_per_second", int64(rate),
		"throughput", formatBytes(rate)+"/s",
	)
	return res, nil
}

// upload streams the tar archive of srcDir into key.
//
// The tar archive is produced by a goroutine writing into a pipe through the
// rate limiter while the upload consumes the other end of the pipe. Both ends
// are always closed, so neither side can block forever when the other fails.
func (b *Backuper) upload(ctx context.Context, log *slog.Logger, key, srcDir, rootName string) (tarStats, int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var written atomic.Int64
	pr, pw := io.Pipe()
	type tarResult struct {
		st  tarStats
		err error
	}
	tarDone := make(chan tarResult, 1)
	go func() {
		w := ratelimit.NewWriter(ctx, &countingWriter{w: pw, n: &written}, b.BytesPerSecond)
		st, err := writeTar(ctx, w, srcDir, rootName, log)
		// A nil error makes the reader see io.EOF after the tar trailer.
		_ = pw.CloseWithError(err)
		tarDone <- tarResult{st, err}
	}()

	stopProgress := b.startProgress(log, &written)
	n, upErr := b.S3.Upload(ctx, key, pr, TarContentType)
	stopProgress()
	if upErr != nil {
		_ = pr.CloseWithError(upErr)
	} else {
		_ = pr.CloseWithError(errUploadFinished)
	}
	cancel() // interrupt a writer waiting in the rate limiter
	tr := <-tarDone

	switch {
	case tr.err != nil && upErr != nil && errors.Is(tr.err, upErr):
		// The tar writer failed only because the upload had failed.
		return tr.st, 0, upErr
	case tr.err != nil:
		if upErr != nil && !errors.Is(upErr, tr.err) {
			return tr.st, 0, fmt.Errorf("cannot build tar stream from %q: %w (upload error: %v)", srcDir, tr.err, upErr)
		}
		return tr.st, 0, fmt.Errorf("cannot build tar stream from %q: %w", srcDir, tr.err)
	case upErr != nil:
		return tr.st, 0, upErr
	}
	if total := written.Load(); n != total {
		return tr.st, 0, fmt.Errorf("uploaded size mismatch for %q: uploaded %d bytes, tar stream has %d bytes", key, n, total)
	}
	return tr.st, n, nil
}

// verify checks that the uploaded object exists and has the expected size.
func (b *Backuper) verify(ctx context.Context, key string, size int64) error {
	info, found, err := b.S3.Stat(ctx, key)
	if err != nil {
		return fmt.Errorf("cannot verify uploaded object: %w", err)
	}
	if !found {
		return fmt.Errorf("uploaded object %q not found", key)
	}
	if info.Size != size {
		return fmt.Errorf("uploaded object %q has size %d; expected %d", key, info.Size, size)
	}
	return nil
}

func (b *Backuper) startProgress(log *slog.Logger, written *atomic.Int64) (stop func()) {
	if b.ProgressInterval <= 0 {
		return func() {}
	}
	start := b.timeNow()
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(b.ProgressInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				n := written.Load()
				rate := bytesPerSecond(n, b.timeNow().Sub(start))
				log.Info("upload in progress", "bytes", n, "size", formatBytes(float64(n)),
					"throughput_bytes_per_second", int64(rate), "throughput", formatBytes(rate)+"/s")
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// deleteSnapshot removes the snapshot via the VictoriaLogs API. If that fails
// (e.g. older VictoriaLogs versions without the delete endpoint), it removes the
// local snapshot directory instead. Snapshots are plain directories of hard
// links, and VictoriaLogs lists them by scanning the filesystem, so removing the
// directory is safe.
//
// It runs even if ctx is canceled, so snapshots are not leaked on shutdown.
func (b *Backuper) deleteSnapshot(ctx context.Context, log *slog.Logger, snapshot, localDir string) {
	timeout := b.CleanupTimeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	err := b.VL.SnapshotDelete(ctx, snapshot)
	if err == nil {
		log.Info("snapshot deleted")
		return
	}
	if localDir == "" {
		log.Error("cannot delete snapshot via VictoriaLogs API and its local directory is unknown; remove it manually", "error", err)
		return
	}
	log.Warn("cannot delete snapshot via VictoriaLogs API; removing the local snapshot directory instead",
		"error", err, "local_dir", localDir)
	if rerr := os.RemoveAll(localDir); rerr != nil {
		log.Error("cannot remove local snapshot directory; remove it manually", "local_dir", localDir, "error", rerr)
		return
	}
	log.Info("local snapshot directory removed", "local_dir", localDir)
}

func bytesPerSecond(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / d.Seconds()
}

func formatBytes(n float64) string {
	const unit = 1024.0
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for n >= unit && i < len(units)-1 {
		n /= unit
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", n, units[i])
	}
	return fmt.Sprintf("%.2f %s", n, units[i])
}

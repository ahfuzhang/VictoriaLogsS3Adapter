package restore

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/valyala/fasthttp"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/pathtmpl"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/restore/supervisor"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/restore/vltime"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/s3store"
)

const (
	// drainTimeout is how long a restart waits for in-flight requests.
	drainTimeout = 30 * time.Second
	// shutdownTimeout is how long the server waits for open connections on shutdown.
	shutdownTimeout = 30 * time.Second
	// maxRequestBodySize limits request bodies; select requests are small.
	maxRequestBodySize = 64 << 20
)

// s3Backups adapts s3store.Client to Store.
type s3Backups struct {
	c    *s3store.Client
	tmpl *pathtmpl.Template
}

func (s *s3Backups) FindBackup(ctx context.Context, day string) (string, bool, error) {
	info, found, err := s.c.FindBackup(ctx, s.tmpl, day)
	return info.Key, found, err
}

func (s *s3Backups) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	return s.c.Open(ctx, key)
}

// Run runs the restore server until ctx is canceled.
func Run(ctx context.Context, cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	tmpl, err := pathtmpl.Parse(cfg.S3.Prefix)
	if err != nil {
		return fmt.Errorf("invalid -s3.prefix: %w", err)
	}
	storageDir, err := filepath.Abs(cfg.StorageDir)
	if err != nil {
		return fmt.Errorf("invalid -victoria.logs.storage.dir: %w", err)
	}
	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		return fmt.Errorf("cannot create -victoria.logs.storage.dir: %w", err)
	}

	s3c, err := s3store.New(ctx, cfg.S3)
	if err != nil {
		return err
	}
	slog.Info("S3 bucket is accessible", "endpoint", cfg.S3.Endpoint, "bucket", cfg.S3.Bucket,
		"prefix_template", cfg.S3.Prefix, "static_prefix", tmpl.StaticPrefix())

	// Restores are not bound to client requests; they are canceled on shutdown only.
	restoreCtx, cancelRestores := context.WithCancel(context.Background())
	defer cancelRestores()
	restorer, err := NewRestorer(restoreCtx, RestorerConfig{
		StorageDir:          storageDir,
		Store:               &s3Backups{c: s3c, tmpl: tmpl},
		BytesPerSecond:      cfg.MaxDownloadBytesPerSecond.N,
		RemoveOnLoadFailure: cfg.Managed(),
	})
	if err != nil {
		return err
	}

	prefix := NormalizePrefix(cfg.HTTPPrefix)
	proxy := NewProxy(ProxyConfig{
		Prefix:   prefix,
		Restorer: restorer,
		MaxDays:  cfg.MaxDays,
	})
	loader := &partitionLoader{proxy: proxy, drainTimeout: drainTimeout}
	restorer.SetLoader(loader)

	if cfg.Managed() {
		sup := supervisor.New(supervisor.Config{
			Bin:        cfg.VLBin,
			StorageDir: storageDir,
			AuthKey:    cfg.AuthKey,
			DropEnv:    []string{s3store.EnvAccessKeyID, s3store.EnvSecretAccessKey},
			OnReady: func(baseURL string) {
				be, err := newBackend(baseURL, cfg.AuthKey)
				if err != nil {
					slog.Error("BUG: cannot create backend for VictoriaLogs child process", "addr", baseURL, "error", err)
					return
				}
				proxy.setBackend(be)
			},
		})
		defer sup.Stop()
		if err := sup.Start(ctx); err != nil {
			return err
		}
		loader.restarter = sup
		slog.Info("VictoriaLogs runs as a child process", "bin", cfg.VLBin, "storage_dir", storageDir,
			"retention", supervisor.MaxRetention)
	} else {
		be, err := newBackend(cfg.VLAddr, cfg.AuthKey)
		if err != nil {
			return err
		}
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = be.vl.Ping(pctx)
		cancel()
		if err != nil {
			return fmt.Errorf("cannot access VictoriaLogs at -victoria.logs.addr=%q: %w", cfg.VLAddr, err)
		}
		proxy.setBackend(be)
		if _, err := os.Stat(filepath.Join(storageDir, "flock.lock")); err != nil {
			slog.Warn("-victoria.logs.storage.dir doesn't look like the -storageDataPath of the running VictoriaLogs; "+
				"restored partitions may be invisible to it", "storage_dir", storageDir)
		}
		slog.Info("proxying to an external VictoriaLogs; restored partitions are loaded via attach only",
			"addr", be.baseURL, "storage_dir", storageDir)
		watchExternalRetention(restoreCtx, be.baseURL, restorer)
		markUnattachedPartitions(ctx, be, restorer)
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("cannot listen on -addr=%q: %w", cfg.Addr, err)
	}
	srv := &fasthttp.Server{
		Handler:                      proxy.Handler,
		NoDefaultServerHeader:        true,
		NoDefaultContentType:         true,
		DisablePreParseMultipartForm: true,
		ReadBufferSize:               64 * 1024,
		WriteBufferSize:              64 * 1024,
		MaxRequestBodySize:           maxRequestBodySize,
		// Restores and big queries may take minutes, and /select/logsql/tail streams
		// forever, so there is no write timeout. ReadTimeout limits reading of a request only.
		ReadTimeout:     5 * time.Minute,
		IdleTimeout:     2 * time.Minute,
		CloseOnShutdown: true,
		TCPKeepalive:    true,
		Logger:          fasthttpLogger{},
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()
	slog.Info("restore server started", "addr", ln.Addr().String(), "http_prefix", prefix, "s3_bucket", cfg.S3.Bucket,
		"max_download_bytes_per_second", cfg.MaxDownloadBytesPerSecond.N, "restore_max_days", cfg.MaxDays)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return fmt.Errorf("HTTP server failed: %w", err)
	}

	slog.Info("shutting down")
	cancelRestores()
	// Live tail streams never finish on their own; other requests may complete gracefully.
	if n := proxy.closeStreams(true); n > 0 {
		slog.Info("interrupted live tail streams", "streams", n)
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.ShutdownWithContext(sctx); err != nil {
		slog.Warn("HTTP server did not shut down gracefully", "error", err)
	}
	// The deferred sup.Stop() stops VictoriaLogs.
	return nil
}

// watchExternalRetention reads -retentionPeriod of an external VictoriaLogs at startup
// and every retentionRefreshInterval, so days it would drop are not restored.
func watchExternalRetention(ctx context.Context, baseURL string, r *Restorer) {
	update := func(initial bool) {
		d, err := fetchRetention(ctx, baseURL)
		if err != nil {
			if initial {
				slog.Warn("cannot detect -retentionPeriod of the external VictoriaLogs from its /metrics page; "+
					"make sure it covers all the days to restore, otherwise VictoriaLogs drops restored partitions "+
					"and they are downloaded again on every request", "error", err)
			} else {
				slog.Debug("cannot refresh -retentionPeriod of the external VictoriaLogs; keeping the last known value", "error", err)
			}
			return
		}
		if time.Duration(r.retention.Load()) != d || initial {
			slog.Warn("the external VictoriaLogs drops partitions older than its -retentionPeriod; older days are not restored",
				"retention", d.String(), "oldest_restorable_day", vltime.DayName(minRetainedDay(time.Now(), d)))
		}
		r.SetRetention(d)
	}
	update(true)
	go func() {
		t := time.NewTicker(retentionRefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				update(false)
			}
		}
	}()
}

// markUnattachedPartitions finds partition directories which the external VictoriaLogs
// hasn't attached (e.g. attach failed before this process restarted), so they are
// attached when requested.
func markUnattachedPartitions(ctx context.Context, be *backend, r *Restorer) {
	local, err := r.LocalPartitions()
	if err != nil {
		slog.Warn("cannot list local partitions", "error", err)
		return
	}
	if len(local) == 0 {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	attached, err := be.vl.PartitionList(lctx)
	if err != nil {
		slog.Warn("cannot list partitions attached to the external VictoriaLogs; unattached local partitions won't be attached",
			"error", err)
		return
	}
	var missing []string
	for _, day := range local {
		if !slices.Contains(attached, day) {
			missing = append(missing, day)
		}
	}
	if len(missing) > 0 {
		slog.Warn("found local partitions not attached to the external VictoriaLogs; they will be attached when requested",
			"days", missing)
		r.MarkUnloaded(missing)
	}
}

// fasthttpLogger routes fasthttp error logs to slog.
type fasthttpLogger struct{}

func (fasthttpLogger) Printf(format string, args ...any) {
	slog.Warn(fmt.Sprintf(format, args...), "component", "fasthttp")
}

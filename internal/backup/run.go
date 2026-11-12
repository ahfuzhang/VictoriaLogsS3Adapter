package backup

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/pathtmpl"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/s3store"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/vlclient"
)

const (
	pingTimeout      = 10 * time.Second
	progressInterval = 30 * time.Second
)

// Run validates cfg, checks access to S3, VictoriaLogs and the storage
// directory, and then either backs up cfg.Date once or runs the daily
// scheduler until ctx is canceled.
//
// In one-shot mode a non-nil error means the backup failed. In scheduled mode
// Run returns nil after ctx is canceled; failed scheduled backups are logged.
func Run(ctx context.Context, cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	tmpl, err := pathtmpl.Parse(cfg.S3.Prefix)
	if err != nil {
		return fmt.Errorf("invalid -s3.prefix: %w", err)
	}
	log := slog.Default()

	s3c, err := s3store.New(ctx, cfg.S3)
	if err != nil {
		return err
	}
	log.Info("S3 bucket is accessible", "endpoint", cfg.S3.Endpoint, "region", cfg.S3.Region, "bucket", cfg.S3.Bucket,
		"prefix", tmpl.String())

	vl, err := vlclient.New(cfg.VLAddr, cfg.VLAuthKey)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	err = vl.Ping(pctx)
	cancel()
	if err != nil {
		return fmt.Errorf("VictoriaLogs at -victoria.logs.addr=%q is not reachable: %w", cfg.VLAddr, err)
	}
	log.Info("VictoriaLogs is reachable", "addr", vl.BaseURL())

	if err := checkStorageDir(cfg.StorageDir); err != nil {
		return err
	}
	if fi, err := os.Stat(filepath.Join(cfg.StorageDir, "partitions")); err != nil || !fi.IsDir() {
		log.Warn("storage directory has no partitions subdirectory; make sure it is the VictoriaLogs -storageDataPath",
			"storage_dir", cfg.StorageDir)
	}

	b := &Backuper{
		VL:               vl,
		S3:               s3c,
		Bucket:           s3c.Bucket(),
		Prefix:           tmpl,
		StorageDir:       cfg.StorageDir,
		BytesPerSecond:   cfg.MaxUploadBytesPerSecond.N,
		Logger:           log,
		ProgressInterval: progressInterval,
	}

	if cfg.Date != "" {
		d, _ := ParseDate(cfg.Date)
		if today := time.Now().UTC().Truncate(24 * time.Hour); !d.Before(today) {
			log.Warn("backing up a partition that may still receive logs; the backup will contain only logs ingested so far",
				"date", cfg.Date)
		}
		_, err := b.Backup(ctx, cfg.Date)
		return err
	}

	hour, minute, _ := ParseHHMM(cfg.YesterdayAt)
	log.Info("running in scheduled mode", "at_utc", fmt.Sprintf("%02d:%02d", hour, minute))
	s := &scheduler{
		backup: func(ctx context.Context, date string) error {
			_, err := b.Backup(ctx, date)
			return err
		},
		exists: func(ctx context.Context, date string) (bool, error) {
			_, found, err := s3c.FindBackup(ctx, tmpl, date)
			return found, err
		},
		hour:        hour,
		minute:      minute,
		retryDelays: DefaultRetryDelays,
		log:         log,
		now:         time.Now,
		sleepStep:   maxSleepStep,
	}
	return s.run(ctx)
}

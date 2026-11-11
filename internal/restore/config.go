// Package restore implements victoria_logs_s3_restore_svr: an HTTP proxy in
// front of VictoriaLogs which restores per-day partitions from S3 backups on
// demand before forwarding select requests.
package restore

import (
	"errors"
	"flag"
	"fmt"
	"os/exec"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/bytesize"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/s3store"
)

// Config holds command-line settings of the restore server.
type Config struct {
	S3 *s3store.Config

	// VLBin is the path to the VictoriaLogs binary (managed mode).
	VLBin string
	// VLAddr is the address of an already running VictoriaLogs (external mode).
	VLAddr string
	// StorageDir is the VictoriaLogs -storageDataPath.
	StorageDir string
	// AuthKey is the VictoriaLogs -partitionManageAuthKey.
	AuthKey string

	MaxDownloadBytesPerSecond *bytesize.Flag

	// Addr is the HTTP listen address.
	Addr string
	// HTTPPrefix is the path prefix the service is mounted under.
	HTTPPrefix string
	// MaxDays is the maximum number of days a single request may restore.
	MaxDays int
	// AutoRemoveDays is the number of days after which downloaded partitions are removed; 0 disables removal.
	AutoRemoveDays int

	LogLevel string
}

// RegisterFlags registers the restore server flags on fs.
func RegisterFlags(fs *flag.FlagSet) *Config {
	c := &Config{
		S3:                        s3store.RegisterFlags(fs),
		MaxDownloadBytesPerSecond: bytesize.NewFlag("100mb"),
	}
	fs.StringVar(&c.VLBin, "victoria.logs.bin", "", "Path to the VictoriaLogs binary. When set, VictoriaLogs is started as a child process "+
		"on a free 127.0.0.1 port with -storageDataPath=<-victoria.logs.storage.dir> and the maximum -retentionPeriod, "+
		"and it is restarted automatically if it exits. Mutually exclusive with -victoria.logs.addr")
	fs.StringVar(&c.VLAddr, "victoria.logs.addr", "", "Address of an already running VictoriaLogs, e.g. http://127.0.0.1:9428. "+
		"Used only when -victoria.logs.bin is empty; its -storageDataPath must be -victoria.logs.storage.dir. "+
		"Restored partitions are loaded via /internal/partition/attach. Its -retentionPeriod must cover the days to restore, "+
		"since VictoriaLogs drops older partitions (days outside the retention read from its /metrics page are not restored); "+
		"-retention.maxDiskSpaceUsageBytes and -retention.maxDiskUsagePercent may drop restored partitions too")
	fs.StringVar(&c.StorageDir, "victoria.logs.storage.dir", "/tmp/", "VictoriaLogs data directory (-storageDataPath). "+
		"Restored partitions are placed at <dir>/partitions/<YYYYMMDD>")
	fs.StringVar(&c.AuthKey, "victoria.logs.auth.key", "", "Optional VictoriaLogs -partitionManageAuthKey for /internal/partition/* APIs; "+
		"it is also passed to the child process when -victoria.logs.bin is set")
	fs.Var(c.MaxDownloadBytesPerSecond, "max.download.bytes.per.second", "Download speed limit per backup; units: b, kb, mb, gb (case-insensitive)")
	fs.StringVar(&c.Addr, "addr", "0.0.0.0:8093", "HTTP listen address")
	fs.StringVar(&c.HTTPPrefix, "http.prefix", "", "Path prefix the service is mounted under, e.g. /admin/. "+
		"Requests outside the prefix get 404; the prefix is stripped before proxying to VictoriaLogs")
	fs.IntVar(&c.MaxDays, "restore.max.days", 31, "Maximum number of days a single request may restore. "+
		"For longer time ranges only the most recent days are restored")
	fs.IntVar(&c.AutoRemoveDays, "auto.remove.days", 0, "Remove partitions downloaded at least this many days ago, so they don't "+
		"use disk space forever; they are downloaded again when requested. Every day after 00:00 UTC VictoriaLogs is restarted "+
		"if there are such partitions, and they are removed while it is stopped. The download time of the partitions found "+
		"on startup is the modification time of their directories. Requires -victoria.logs.bin. 0 disables automatic removal")
	fs.StringVar(&c.LogLevel, "loglevel", "info", "Log level: debug, info, warn, error")
	return c
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	if err := c.S3.Validate(); err != nil {
		return err
	}
	switch {
	case c.VLBin == "" && c.VLAddr == "":
		return errors.New("either -victoria.logs.bin or -victoria.logs.addr must be set")
	case c.VLBin != "" && c.VLAddr != "":
		return errors.New("-victoria.logs.bin and -victoria.logs.addr are mutually exclusive")
	}
	if c.VLBin != "" {
		p, err := exec.LookPath(c.VLBin)
		if err != nil {
			return fmt.Errorf("invalid -victoria.logs.bin=%q: %w", c.VLBin, err)
		}
		c.VLBin = p
	}
	if c.StorageDir == "" {
		return errors.New("-victoria.logs.storage.dir must be set")
	}
	if c.MaxDays < 1 {
		return fmt.Errorf("-restore.max.days must be at least 1; got %d", c.MaxDays)
	}
	if c.AutoRemoveDays < 0 {
		return fmt.Errorf("-auto.remove.days cannot be negative; got %d", c.AutoRemoveDays)
	}
	if c.AutoRemoveDays > 0 && !c.Managed() {
		// Removing partitions requires restarting VictoriaLogs. Besides, the storage
		// directory of an external VictoriaLogs holds partitions which weren't downloaded.
		return errors.New("-auto.remove.days requires -victoria.logs.bin, since VictoriaLogs must be restarted in order to remove partitions")
	}
	if c.Addr == "" {
		return errors.New("-addr must be set")
	}
	return nil
}

// Managed reports whether VictoriaLogs runs as a child process.
func (c *Config) Managed() bool { return c.VLBin != "" }

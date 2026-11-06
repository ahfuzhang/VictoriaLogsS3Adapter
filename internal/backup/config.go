// Package backup implements victoria_logs_s3_backup: it snapshots a VictoriaLogs
// daily partition and streams it as a tar archive into an S3 object.
package backup

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/bytesize"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/pathtmpl"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/s3store"
)

// DateLayout is the layout of VictoriaLogs partition names (UTC days).
const DateLayout = "20060102"

// Config holds command-line configuration of the backup tool.
type Config struct {
	S3 *s3store.Config

	VLAddr     string
	VLAuthKey  string
	StorageDir string

	// Date is the partition (YYYYMMDD) to back up once. Mutually exclusive with YesterdayAt.
	Date string
	// YesterdayAt is the daily UTC time (HHMM) at which the previous UTC day is backed up.
	YesterdayAt string

	MaxUploadBytesPerSecond *bytesize.Flag

	LogLevel string
}

// RegisterFlags registers all backup tool flags on fs.
func RegisterFlags(fs *flag.FlagSet) *Config {
	c := &Config{
		S3:                      s3store.RegisterFlags(fs),
		MaxUploadBytesPerSecond: bytesize.NewFlag("100mb"),
	}
	fs.StringVar(&c.VLAddr, "victoria.logs.addr", "", "VictoriaLogs HTTP address, e.g. http://127.0.0.1:9428 (required)")
	fs.StringVar(&c.VLAuthKey, "victoria.logs.auth.key", "", "Optional auth key for /internal/partition/* endpoints "+
		"(the value of -partitionManageAuthKey in VictoriaLogs)")
	fs.StringVar(&c.StorageDir, "victoria.logs.storage.dir", "", "Local path of the VictoriaLogs -storageDataPath directory (required)")
	fs.StringVar(&c.Date, "backup.date", "", "Back up the partition for this UTC day (YYYYMMDD) and exit. "+
		"Mutually exclusive with -backup.yesterday.at")
	fs.StringVar(&c.YesterdayAt, "backup.yesterday.at", "", "Run forever and back up the previous UTC day every day at this UTC time (HHMM). "+
		"Mutually exclusive with -backup.date")
	fs.Var(c.MaxUploadBytesPerSecond, "max.upload.bytes.per.second", "Upload rate limit; units: b, kb, mb, gb (case-insensitive, 1024-based)")
	fs.StringVar(&c.LogLevel, "loglevel", "info", "Log level: debug, info, warn, error")
	return c
}

// Validate checks flag values without touching the network or the filesystem.
func (c *Config) Validate() error {
	if err := c.S3.Validate(); err != nil {
		return err
	}
	if _, err := pathtmpl.Parse(c.S3.Prefix); err != nil {
		return fmt.Errorf("invalid -s3.prefix: %w", err)
	}
	if c.VLAddr == "" {
		return errors.New("-victoria.logs.addr must be set")
	}
	if c.StorageDir == "" {
		return errors.New("-victoria.logs.storage.dir must be set")
	}
	switch {
	case c.Date == "" && c.YesterdayAt == "":
		return errors.New("either -backup.date or -backup.yesterday.at must be set")
	case c.Date != "" && c.YesterdayAt != "":
		return errors.New("-backup.date and -backup.yesterday.at are mutually exclusive")
	case c.Date != "":
		if _, err := ParseDate(c.Date); err != nil {
			return fmt.Errorf("invalid -backup.date: %w", err)
		}
	default:
		if _, _, err := ParseHHMM(c.YesterdayAt); err != nil {
			return fmt.Errorf("invalid -backup.yesterday.at: %w", err)
		}
	}
	if c.MaxUploadBytesPerSecond == nil || c.MaxUploadBytesPerSecond.N <= 0 {
		return errors.New("-max.upload.bytes.per.second must be positive")
	}
	return nil
}

// ParseDate parses a partition date in YYYYMMDD format.
func ParseDate(s string) (time.Time, error) {
	if len(s) != len(DateLayout) || !allDigits(s) {
		return time.Time{}, fmt.Errorf("%q must be in YYYYMMDD format", s)
	}
	t, err := time.ParseInLocation(DateLayout, s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a valid YYYYMMDD date: %w", s, err)
	}
	return t, nil
}

// ParseHHMM parses a time of day in HHMM format (24-hour clock).
func ParseHHMM(s string) (hour, minute int, err error) {
	if len(s) != 4 || !allDigits(s) {
		return 0, 0, fmt.Errorf("%q must be in HHMM format, e.g. 0015", s)
	}
	hour = int(s[0]-'0')*10 + int(s[1]-'0')
	minute = int(s[2]-'0')*10 + int(s[3]-'0')
	if hour > 23 {
		return 0, 0, fmt.Errorf("hour in %q must be in the range 00..23", s)
	}
	if minute > 59 {
		return 0, 0, fmt.Errorf("minute in %q must be in the range 00..59", s)
	}
	return hour, minute, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// checkStorageDir verifies that dir exists and is a directory.
func checkStorageDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("cannot access -victoria.logs.storage.dir=%q: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("-victoria.logs.storage.dir=%q is not a directory", dir)
	}
	return nil
}

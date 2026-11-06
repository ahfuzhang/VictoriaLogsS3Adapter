// Command victoria_logs_s3_restore_svr is an HTTP/1.1 proxy in front of
// VictoriaLogs which restores per-day partitions from S3 backups on demand.
//
// Before a /select/logsql/* request with start/end args is forwarded, every
// day of the requested time range missing at <storage dir>/partitions/<YYYYMMDD>
// is downloaded from <s3.prefix>/<YYYYMMDD>.tar and loaded into VictoriaLogs.
//
// S3 credentials are read from the environment variables access_key_id
// and secret_access_key. All other settings come from command-line flags.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/logger"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/restore"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/s3store"
)

func main() {
	cfg := restore.RegisterFlags(flag.CommandLine)
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Usage: %s [flags]\n\n", os.Args[0])
		fmt.Fprintf(out, "Proxies requests to VictoriaLogs and restores missing daily partitions from <s3.prefix>/<YYYYMMDD>.tar on demand.\n")
		fmt.Fprintf(out, "S3 credentials are read from the environment variables %s and %s.\n\n",
			s3store.EnvAccessKeyID, s3store.EnvSecretAccessKey)
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected positional arguments: %q\n", flag.Args())
		flag.Usage()
		os.Exit(2)
	}
	if _, err := logger.Init(cfg.LogLevel); err != nil {
		fmt.Fprintf(os.Stderr, "invalid -loglevel: %s\n", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Restore default signal handling, so a second signal terminates the process immediately.
		stop()
	}()

	if err := restore.Run(ctx, cfg); err != nil {
		logger.Fatal("restore server failed", "error", err)
	}
}

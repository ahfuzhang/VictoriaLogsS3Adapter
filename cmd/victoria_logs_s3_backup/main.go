// Command victoria_logs_s3_backup backs up VictoriaLogs daily partitions to S3.
//
// Each backup snapshots a partition via the VictoriaLogs snapshot API and
// streams it as <s3.prefix>/<YYYYMMDD>.tar into an S3 bucket.
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

	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/backup"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/logger"
	"github.com/ahfuzhang/VictoriaLogsS3Adapter/internal/s3store"
)

func main() {
	cfg := backup.RegisterFlags(flag.CommandLine)
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Usage: %s [flags]\n\n", os.Args[0])
		fmt.Fprintf(out, "Backs up VictoriaLogs daily partitions to S3 as <s3.prefix>/<YYYYMMDD>.tar.\n")
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

	if err := backup.Run(ctx, cfg); err != nil {
		logger.Fatal("backup failed", "error", err)
	}
}

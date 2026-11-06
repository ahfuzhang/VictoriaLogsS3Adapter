**English** | [简体中文](README_cn.md)

# VictoriaLogsS3Adapter

Back up VictoriaLogs logs to S3 by day, and restore them from S3 on demand at query time.

> docker pull ahfuzhang/victoria-logs-s3-adapter:v0.1.0

## Goal

VictoriaLogs stores logs on local disk in per-day UTC partitions (`<storageDataPath>/partitions/<YYYYMMDD>`). When log volume is large, local disk is only suitable for keeping recent data. Older data is cheaper on S3, but you still need to get it back when querying historical logs.

This project provides two command-line tools to solve this:

- **`victoria_logs_s3_backup`**: the backup tool. It creates a snapshot of a VictoriaLogs partition for one day and uploads it as a tar stream to the S3 object `<s3.prefix>/<YYYYMMDD>.tar`. It can back up specified dates once, or run as a daemon that backs up the previous day's data on a daily schedule.
- **`victoria_logs_s3_restore_svr`**: the restore server. An HTTP proxy placed in front of VictoriaLogs. When the query's time range includes a day whose data is missing locally, it automatically downloads that day's backup from S3, loads it into VictoriaLogs, and then returns the query result. To clients, it looks like a VictoriaLogs that can query all historical data.

```
VictoriaLogs ──► victoria_logs_s3_backup ──► S3: <s3.prefix>/<YYYYMMDD>.tar
                                             │
                                             │ days missing locally are downloaded on demand
                                             ▼
client ──► victoria_logs_s3_restore_svr ◄────┘
                        │
                        ▼ forward the query after restore
                  VictoriaLogs (historical data)
```

## Usage

### 1. Build

```bash
make build    # output goes to build/
make docker   # or build a Docker image containing VictoriaLogs and both tools
```

### 2. Prepare S3 credentials

Both tools read S3 credentials from the environment variables `access_key_id` and `secret_access_key`. Passing them via `env` is recommended. All other configuration comes from command-line flags.

### 3. Backup

Run on the machine where VictoriaLogs runs (the tool needs to read VictoriaLogs' data directory). The command below uploads the previous day's partition to `s3://my-bucket/victoria_logs/prod/<YYYYMMDD>.tar` every day at 00:15 (UTC):

```bash
env 'access_key_id=xxx' 'secret_access_key=yyy' env=prod \
  ./build/victoria_logs_s3_backup \
  -s3.endpoint=https://oss-cn-hongkong.aliyuncs.com \
  -s3.region=cn-hongkong \
  -s3.bucket=my-bucket \
  -s3.prefix='victoria_logs/{env}' \
  -victoria.logs.addr=http://127.0.0.1:9428 \
  -victoria.logs.storage.dir=/data/victoria-logs \
  -backup.yesterday.at=0015
```

Replace `-backup.yesterday.at=0015` with `-backup.date=20261104` to back up the specified date once and then exit.

### 4. Restore and query

Start the restore server; it manages a VictoriaLogs child process. `-s3.prefix` must be the same as used for backup:

```bash
env 'access_key_id=xxx' 'secret_access_key=yyy' env=prod \
  ./build/victoria_logs_s3_restore_svr \
  -s3.endpoint=https://oss-cn-hongkong.aliyuncs.com \
  -s3.region=cn-hongkong \
  -s3.bucket=my-bucket \
  -s3.prefix='victoria_logs/{env}' \
  -victoria.logs.bin=/usr/local/bin/victoria-logs-prod \
  -victoria.logs.storage.dir=/data/vl_restore \
  -addr=0.0.0.0:8093
```

Then query `:8093` as you would VictoriaLogs. Days not available locally are first restored from S3, and then the result is returned:

```bash
curl http://127.0.0.1:8093/select/logsql/query \
  -d 'query=error' -d 'start=2026-11-04T00:00:00Z' -d 'end=2026-11-05T00:00:00Z'
```

Run `<tool> -h` to see all command-line flags.

## Documentation

- [victoria_logs_s3_backup](docs/victoria_logs_s3_backup.md): all flags, run modes, backup flow, tar format, scheduling and retries, exit codes of the backup tool.
- [victoria_logs_s3_restore_svr](docs/victoria_logs_s3_restore_svr.md): all flags, managed and external modes, requests that trigger a restore, restore flow, caching and limits of the restore server.
- [-s3.prefix prefix template](docs/s3_prefix.md): how to write `{ENV_VAR}` and `${time format}` placeholders, examples, incorrect usages, and how backups are located during restore.
- [Build and deployment](docs/build.md): make targets, cross-compilation, Docker image, tests.
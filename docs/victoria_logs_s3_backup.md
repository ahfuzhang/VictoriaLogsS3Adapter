# victoria_logs_s3_backup

Takes a snapshot of a VictoriaLogs partition for one day (UTC) and uploads it as a tar stream to the S3 object `<s3.prefix>/<YYYYMMDD>.tar`.

There are two run modes; exactly one must be chosen:

* **One-time backup** (`-backup.date=YYYYMMDD`): back up the partition for the specified date, then exit.
* **Scheduled backup** (`-backup.yesterday.at=HHMM`): run as a daemon and, at the specified UTC time each day, back up the previous UTC day's partition.

> The tool reads the snapshot files in VictoriaLogs' data directory directly, so it must run on the same machine as VictoriaLogs (or mount the same data directory).

## Command-line flags

| Flag | Default | Description |
|---|---|---|
| `-s3.endpoint` | | **Required**. S3 endpoint, e.g. `https://oss-cn-hongkong.aliyuncs.com`. |
| `-s3.region` | | S3 region, e.g. `cn-hongkong`. |
| `-s3.bucket` | | **Required**. S3 bucket name. |
| `-s3.prefix` | | Object key prefix template; supports `{ENV_VAR}` and `${time format}` placeholders. See [-s3.prefix prefix template](s3_prefix.md). |
| `-victoria.logs.addr` | | **Required**. HTTP address of VictoriaLogs, e.g. `http://127.0.0.1:9428`. `/health` is requested at startup to check reachability. |
| `-victoria.logs.auth.key` | | Auth key used when calling endpoints such as `/internal/partition/*`, i.e. VictoriaLogs' `-partitionManageAuthKey`. Leave empty if VictoriaLogs doesn't set it. |
| `-victoria.logs.storage.dir` | | **Required**. Local path of VictoriaLogs' `-storageDataPath`. The directory's existence is checked at startup. |
| `-backup.date` | | Back up the partition for this day (UTC) once, format `YYYYMMDD`, then exit. Mutually exclusive with `-backup.yesterday.at`. |
| `-backup.yesterday.at` | | Run as a daemon and, at this UTC time each day, back up the previous UTC day's partition. Format `HHMM` (24-hour clock), e.g. `0015`. Mutually exclusive with `-backup.date`. |
| `-max.upload.bytes.per.second` | `100mb` | Upload rate limit. Units `b`, `kb`, `mb`, `gb`, case-insensitive, powers of 1024; no unit means bytes; decimals are allowed, e.g. `1.5mb`. Must be greater than 0. |
| `-loglevel` | `info` | Log level: `debug`, `info`, `warn`, `error`. |

Run `victoria_logs_s3_backup -h` to see the same flag descriptions.

## Environment variables

| Environment variable | Description |
|---|---|
| `access_key_id` | **Required**. S3 access key id. |
| `secret_access_key` | **Required**. S3 secret access key. |
| `{VAR}` referenced in `-s3.prefix` | e.g. `{namespace}`, `{env}`; startup fails if unset or empty. |

You can pass them with `env`:

```bash
env 'access_key_id=xxx' 'secret_access_key=yyy' victoria_logs_s3_backup ...
```

## Examples

One-time backup of the `20261104` partition:

```bash
env 'access_key_id=xxx' 'secret_access_key=yyy' namespace=ns1 env=prod \
  victoria_logs_s3_backup \
  -s3.endpoint=https://oss-cn-hongkong.aliyuncs.com \
  -s3.region=cn-hongkong \
  -s3.bucket=my-bucket \
  -s3.prefix='victoria_logs/{namespace}/{env}' \
  -victoria.logs.addr=http://127.0.0.1:9428 \
  -victoria.logs.storage.dir=/data/victoria-logs \
  -backup.date=20261104
# uploads to s3://my-bucket/victoria_logs/ns1/prod/20261104.tar
```

Back up the previous day's partition every day at 00:15 (UTC), rate-limited to 50 MiB/s, running as a daemon:

```bash
env 'access_key_id=xxx' 'secret_access_key=yyy' namespace=ns1 env=prod \
  victoria_logs_s3_backup \
  -s3.endpoint=https://oss-cn-hongkong.aliyuncs.com \
  -s3.region=cn-hongkong \
  -s3.bucket=my-bucket \
  -s3.prefix='victoria_logs/{namespace}/{env}/${YYYYMMDD_HHMISS}' \
  -victoria.logs.addr=http://127.0.0.1:9428 \
  -victoria.logs.storage.dir=/data/victoria-logs \
  -backup.yesterday.at=0015 \
  -max.upload.bytes.per.second=50mb
```

## Backup flow

1. Validate flags; check that the S3 bucket is accessible, VictoriaLogs is reachable, and `-victoria.logs.storage.dir` exists. If any check fails, exit immediately.
2. Check that `<storage.dir>/partitions/<YYYYMMDD>` exists. If not, there is no data for that day, or `-victoria.logs.storage.dir` is misconfigured.
3. Call `/internal/force_flush` so that recently written logs are flushed to disk (a failure only logs a warning).
4. Call `/internal/partition/snapshot/create?partition_prefix=<YYYYMMDD>` to create a snapshot, calling `/internal/partition/snapshot/list` once before and once after to find the newly created snapshot.
5. Append the part of the snapshot path returned by VictoriaLogs that starts at `partitions/` to `-victoria.logs.storage.dir` to get the local snapshot directory. So it works even if VictoriaLogs runs in a container with a mount point different from the local path.
6. Pack the snapshot directory into a tar stream, rate-limit it, and upload it via multipart to `<expanded s3.prefix>/<YYYYMMDD>.tar`. Everything is streamed; nothing is written to local disk.
7. After the upload completes, verify that the S3 object's size equals the tar stream's size.
8. Delete the snapshot. This happens whether the backup succeeded or failed; for older VictoriaLogs versions without a delete endpoint, the local snapshot directory is deleted directly.

Progress is logged every 30 seconds during the upload.

### tar file format

The tar is uncompressed, and its content is the same as running `tar -cf <YYYYMMDD>.tar ./<snapshot name>` in the `partitions/<YYYYMMDD>/snapshots/` directory, i.e. `<snapshot name>/indexdb/...` and `<snapshot name>/datadb/...`. Each tar is a self-contained single-day partition that can be restored by hand without this project: extract the snapshot directory as `partitions/<YYYYMMDD>` under VictoriaLogs' data directory:

```bash
mkdir -p /data/victoria-logs/partitions/20261104
tar -xf 20261104.tar -C /data/victoria-logs/partitions/20261104 --strip-components=1
# then restart VictoriaLogs, or call /internal/partition/attach?name=20261104 to hot-load it
```

Non-regular files such as symlinks and device files are skipped, with a warning logged.

Each object's multipart part size is 64 MiB with at most 10000 parts, so a single partition's tar can be at most 640 GiB. An S3 object only becomes visible after all parts are uploaded successfully, so a mid-way failure never leaves a partial object.

### Backing up the current day's partition

When `-backup.date` specifies today's or a future date, the tool prints a warning: the partition may still be written to, and the backup only contains logs already written.

## Scheduled mode

* All times are computed in UTC: when the UTC time given by `-backup.yesterday.at` is reached each day, the previous UTC day's partition is backed up (VictoriaLogs partitions are themselves split by UTC day).
* A failed backup is retried, up to 3 attempts in total, with intervals of 1 minute and 2 minutes. If the partition directory doesn't exist, no retry is made.
* If all 3 attempts fail, an error is logged; the process does not exit and continues with the next day's schedule.
* While waiting, the system time is checked every minute, so a backup isn't missed after system sleep or a clock adjustment.

## Exit and signals

* On `SIGINT` / `SIGTERM`, the backup in progress is interrupted (the snapshot is deleted and no partial S3 object is left) and the process exits; sending the signal again terminates the process immediately.
* Exit codes:

| Exit code | Meaning |
|---|---|
| `0` | One-time backup succeeded; or scheduled mode exited normally on a signal. |
| `1` | Flag validation failed, S3 / VictoriaLogs unreachable, one-time backup failed or was interrupted by a signal. |
| `2` | Command-line flags could not be parsed, extra positional arguments were given, or `-loglevel` is invalid. |

## Logs

Logs are in JSON format, written to stderr. After a successful backup, one `backup completed` entry is logged, containing the object key, file count, tar size, duration, average speed, etc.

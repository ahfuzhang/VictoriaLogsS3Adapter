# victoria_logs_s3_restore_svr

An HTTP/1.1 proxy placed in front of VictoriaLogs. All requests are forwarded to VictoriaLogs; for queries with a time range, if the partition for a day in the range doesn't exist locally, it first downloads the `<s3.prefix>/<YYYYMMDD>.tar` produced by [victoria_logs_s3_backup](victoria_logs_s3_backup.md) from S3, extracts it and loads it into VictoriaLogs, and then forwards the request. To clients, it looks like a VictoriaLogs that can query historical data.

## Two run modes

Exactly one must be chosen:

| Mode | Flag | Description |
|---|---|---|
| Managed mode | `-victoria.logs.bin` | This service starts and manages a VictoriaLogs child process. **Recommended**, suitable for instances dedicated to querying historical logs. |
| External mode | `-victoria.logs.addr` | Proxies to an already-running VictoriaLogs. This service must be able to write directly to its data directory. |

### Managed mode

* The child process listens on a free port on `127.0.0.1`, accessed only by this service; clients only access `-addr`.
* The child process is started with `-storageDataPath=<-victoria.logs.storage.dir> -httpListenAddr=127.0.0.1:<port> -retentionPeriod=100y -loggerFormat=json`, plus `-partitionManageAuthKey` when `-victoria.logs.auth.key` is set. `100y` is the maximum retention period VictoriaLogs allows, so restored partitions are not deleted for expiring.
* The S3 credential environment variables are not passed to the child process.
* If the child process exits unexpectedly it is restarted automatically (backoff grows from 1 second up to 30 seconds).
* Restored partitions are preferably hot-loaded via `/internal/partition/attach`; if hot-loading fails, the child process is restarted so it opens all partitions at startup. During the restart, new requests wait (up to 5 minutes, then 503 on timeout), and in-flight requests have 30 seconds to complete.

### External mode

* At startup, `-victoria.logs.addr` is contacted to check that VictoriaLogs is reachable.
* `-victoria.logs.storage.dir` must be this VictoriaLogs' `-storageDataPath`, and this service must be able to read and write it. If there is no `flock.lock` in the directory, a warning is logged: restored partitions may be invisible to VictoriaLogs.
* Restored partitions can only be hot-loaded via `/internal/partition/attach`; VictoriaLogs is not restarted. If loading fails, the partition directory is kept and retried the next time that day is requested.
* At startup and every 10 minutes after, `-retentionPeriod` is read from VictoriaLogs' `/metrics`. Dates older than the retention period are not restored, because VictoriaLogs would delete them immediately. `-retention.maxDiskSpaceUsageBytes` and `-retention.maxDiskUsagePercent` may also delete restored partitions.
* At startup, the directories under local `partitions/` are compared with the list of partitions VictoriaLogs has loaded; partitions not yet loaded are loaded when requested.

## Command-line flags

| Flag | Default | Description |
|---|---|---|
| `-s3.endpoint` | | **Required**. S3 endpoint, e.g. `https://oss-cn-hongkong.aliyuncs.com`. |
| `-s3.region` | | S3 region, e.g. `cn-hongkong`. |
| `-s3.bucket` | | **Required**. S3 bucket name. |
| `-s3.prefix` | | Object key prefix template; **must be exactly the same as used for backup**. See [-s3.prefix prefix template](s3_prefix.md). |
| `-victoria.logs.bin` | | Path of the VictoriaLogs executable (a bare file name is looked up in `PATH`). Setting it enables managed mode. Mutually exclusive with `-victoria.logs.addr`. |
| `-victoria.logs.addr` | | Address of a running VictoriaLogs, e.g. `http://127.0.0.1:9428`. Setting it enables external mode. Mutually exclusive with `-victoria.logs.bin`. |
| `-victoria.logs.storage.dir` | `/tmp/` | VictoriaLogs' data directory (`-storageDataPath`); created automatically if it doesn't exist. Restored partitions are placed at `<dir>/partitions/<YYYYMMDD>`. |
| `-victoria.logs.auth.key` | | VictoriaLogs' `-partitionManageAuthKey`, used when calling `/internal/partition/*` endpoints; in managed mode it is also passed to the child process. |
| `-max.download.bytes.per.second` | `100mb` | Download rate limit per backup. Units `b`, `kb`, `mb`, `gb`, case-insensitive, powers of 1024. At most 2 backups are downloaded concurrently, so total bandwidth is at most twice this value. |
| `-addr` | `0.0.0.0:8093` | HTTP listen address. |
| `-http.prefix` | | Path prefix the service is mounted under, e.g. `/admin/`. Paths outside the prefix return 404; the prefix is stripped before forwarding to VictoriaLogs. `/admin`, `/admin/` and `admin/` are equivalent. |
| `-restore.max.days` | `31` | Maximum number of days restored per request. For a longer time range, only the most recent this-many days are restored. Minimum 1. |
| `-loglevel` | `info` | Log level: `debug`, `info`, `warn`, `error`. |

Run `victoria_logs_s3_restore_svr -h` to see the same flag descriptions.

## Environment variables

| Environment variable | Description |
|---|---|
| `access_key_id` | **Required**. S3 access key id. |
| `secret_access_key` | **Required**. S3 secret access key. |
| `{VAR}` referenced in `-s3.prefix` | e.g. `{namespace}`, `{env}`; startup fails if unset or empty. |

You can pass them with `env`:

```bash
env 'access_key_id=xxx' 'secret_access_key=yyy' victoria_logs_s3_restore_svr ...
```

## Examples

Managed mode, with the service mounted under `/admin/`:

```bash
env 'access_key_id=xxx' 'secret_access_key=yyy' namespace=ns1 env=prod \
  victoria_logs_s3_restore_svr \
  -s3.endpoint=https://oss-cn-hongkong.aliyuncs.com \
  -s3.region=cn-hongkong \
  -s3.bucket=my-bucket \
  -s3.prefix='victoria_logs/{namespace}/{env}' \
  -victoria.logs.bin=/usr/local/bin/victoria-logs-prod \
  -victoria.logs.storage.dir=/data/vl_restore \
  -addr=0.0.0.0:8093 \
  -http.prefix=/admin/
```

Query all logs for 2026-11-04. If the partition for that day isn't available locally, the request waits until the restore from S3 completes before returning results:

```bash
curl http://127.0.0.1:8093/admin/select/logsql/query \
  -d 'query=error' \
  -d 'start=2026-11-04T00:00:00Z' \
  -d 'end=2026-11-05T00:00:00Z'
```

External mode, proxying to a VictoriaLogs already running on this machine:

```bash
env 'access_key_id=xxx' 'secret_access_key=yyy' namespace=ns1 env=prod \
  victoria_logs_s3_restore_svr \
  -s3.endpoint=https://oss-cn-hongkong.aliyuncs.com \
  -s3.bucket=my-bucket \
  -s3.prefix='victoria_logs/{namespace}/{env}' \
  -victoria.logs.addr=http://127.0.0.1:9428 \
  -victoria.logs.storage.dir=/data/victoria-logs
```

## Which requests trigger a restore

After removing `-http.prefix`, requests whose path starts with `/select/logsql/` (e.g. `query`, `hits`, `field_names`, `stream_field_names`, etc.) and that carry a `start` or `end` parameter. The parameters can be in the URL query string or in an `application/x-www-form-urlencoded` request body (the body takes precedence), the same way VictoriaLogs reads them. All other requests are forwarded directly.

The time range is computed the same way as in VictoriaLogs:

* Supported formats for `start` and `end`: `now`; relative times such as `5m`, `1h30m`, `now-1h`; Unix timestamps in seconds / milliseconds / microseconds / nanoseconds; `YYYY`, `YYYY-MM`, `YYYY-MM-DD`, `YYYY-MM-DDTHH`, `YYYY-MM-DDTHH:MM`, `YYYY-MM-DDTHH:MM:SS` (optionally with `Z` or `±HH:MM`); RFC3339.
* Times without a time zone are interpreted in the local time zone of the machine this service runs on (same as VictoriaLogs).
* The range is `[start, end)`; `end` is exclusive. With only `start`, `end` is the current time; with only `end`, only the day containing `end` is restored.
* Converted by UTC day to the partition name `YYYYMMDD`; dates later than today (UTC) are ignored.
* If the range covers more days than `-restore.max.days`, only the most recent `-restore.max.days` days are restored and a warning is logged.
* If the parameters can't be parsed, nothing is restored and the request is forwarded directly, letting VictoriaLogs report the error to the client.

## Restore flow

For each day in the time range (multiple days are processed in parallel):

1. `<storage.dir>/partitions/<YYYYMMDD>` already exists: do nothing.
2. Look up that day's backup in S3; for the lookup rules see [How backups are located during restore](s3_prefix.md#how-backups-are-located-during-restore). If none is found, forward the request directly.
3. Download the tar and stream-extract it into `<storage.dir>/.restore_tmp/`, verifying that the amount of data received equals the S3 object's size.
4. After successful extraction, atomically rename it to `<storage.dir>/partitions/<YYYYMMDD>`. A mid-way failure never leaves a partial partition.
5. Have VictoriaLogs load the partition (see the descriptions of the two modes above), then forward the request.

Other details:

* The request waits until the restore completes before being forwarded. Concurrent requests for the same day share a single restore; if the client disconnects, the restore continues in the background.
* At most 2 backups are downloaded at once and 8 S3 queries run at once. Progress is logged every 30 seconds during download.
* Result caching: dates with no backup in S3 and dates beyond the retention period are cached for 5 minutes; a date whose restore failed is not retried for 1 minute. Requests in the meantime are forwarded directly.
* At startup, `<storage.dir>/.restore_tmp/` is cleared to remove temporary files left by an interrupted restore.
* A restore failure only logs an error; the request is still forwarded, and the client gets a result missing that day's data.
* **Restored partitions are never deleted automatically**; watch the free space on the disk holding `-victoria.logs.storage.dir`.

## Exit and signals

* On `SIGINT` / `SIGTERM`: in-progress restores are cancelled, the `/select/logsql/tail` live streams are interrupted, up to 30 seconds are allowed for other requests to finish, then the VictoriaLogs child process is stopped (managed mode) and the process exits; sending the signal again terminates the process immediately.
* Exit codes:

| Exit code | Meaning |
|---|---|
| `0` | Exited normally after a signal. |
| `1` | Flag validation failed, S3 / VictoriaLogs unreachable, the VictoriaLogs child process could not start, listening on the port failed, etc. |
| `2` | Command-line flags could not be parsed, extra positional arguments were given, or `-loglevel` is invalid. |

## Logs

Logs are in JSON format, written to stderr. In managed mode, the VictoriaLogs child process' logs (also JSON) are likewise written to stderr.

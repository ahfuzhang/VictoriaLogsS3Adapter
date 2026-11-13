# victoria_logs_s3_restore_svr

挡在 VictoriaLogs 前面的 HTTP/1.1 代理。所有请求都会转发给 VictoriaLogs；对于带时间范围的查询请求，如果范围内某一天的分区本地不存在，就先从 S3 下载 [victoria_logs_s3_backup](victoria_logs_s3_backup_cn.md) 生成的 `<s3.prefix>/<YYYYMMDD>.tar`，解包并加载到 VictoriaLogs，再转发请求。对客户端来说，它就是一个能查到历史数据的 VictoriaLogs。

## 两种运行模式

必须且只能选一种：

| 模式 | 参数 | 说明 |
|---|---|---|
| 托管模式 | `-victoria.logs.bin` | 由本服务启动并管理一个 VictoriaLogs 子进程。**推荐**，适合专门用来查询历史日志的实例。 |
| 外部模式 | `-victoria.logs.addr` | 代理到一个已经在运行的 VictoriaLogs。本服务必须能直接写入它的数据目录。 |

### 托管模式

* 子进程监听 `127.0.0.1` 上的一个空闲端口，只有本服务访问它；客户端只访问 `-addr`。
* 子进程的启动参数为 `-storageDataPath=<-victoria.logs.storage.dir> -httpListenAddr=127.0.0.1:<端口> -retentionPeriod=100y -loggerFormat=json`，设置了 `-victoria.logs.auth.key` 时再加上 `-partitionManageAuthKey`。`100y` 是 VictoriaLogs 允许的最大保留期，恢复的分区不会因为过期被删除。
* S3 凭证的环境变量不会传给子进程。
* 子进程意外退出时自动重启（退避间隔从 1 秒逐步增加到 30 秒）。
* 恢复的分区优先通过 `/internal/partition/attach` 热加载；热加载失败时重启子进程，让它在启动时打开所有分区。重启期间新的请求会等待（最长 5 分钟，超时返回 503），已在处理中的请求有 30 秒时间完成。
* 可以用 `-auto.remove.days` 自动删除下载已满 N 天的分区，见 [自动删除恢复的分区](#自动删除恢复的分区)。

### 外部模式

* 启动时访问 `-victoria.logs.addr` 检查 VictoriaLogs 是否可达。
* `-victoria.logs.storage.dir` 必须是这个 VictoriaLogs 的 `-storageDataPath`，并且本服务要能读写它。目录下没有 `flock.lock` 时会输出警告：恢复的分区可能对 VictoriaLogs 不可见。
* 恢复的分区只能通过 `/internal/partition/attach` 热加载，不会重启 VictoriaLogs。加载失败时分区目录保留，下次请求这一天时重试。
* 启动时以及之后每 10 分钟，从 VictoriaLogs 的 `/metrics` 读取 `-retentionPeriod`。早于保留期的日期不会被恢复，因为 VictoriaLogs 会立即删除它们。`-retention.maxDiskSpaceUsageBytes`、`-retention.maxDiskUsagePercent` 也可能删除恢复的分区。
* 启动时会对比本地 `partitions/` 下的目录与 VictoriaLogs 已加载的分区列表，未加载的分区在被请求时再加载。
* 不支持 `-auto.remove.days`：删除分区需要重启 VictoriaLogs，而且它的数据目录中还有不是从 S3 下载的分区。

## 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-s3.endpoint` | | **必填**。S3 endpoint，例如 `https://oss-cn-hongkong.aliyuncs.com`。 |
| `-s3.region` | | S3 region，例如 `cn-hongkong`。 |
| `-s3.bucket` | | **必填**。S3 bucket 名。 |
| `-s3.prefix` | | 对象 key 的前缀模板，**必须与备份时使用的完全相同**，详见 [-s3.prefix 前缀模板](s3_prefix_cn.md)。 |
| `-victoria.logs.bin` | | VictoriaLogs 可执行文件的路径（可以只写文件名，从 `PATH` 中查找）。设置后进入托管模式。与 `-victoria.logs.addr` 互斥。 |
| `-victoria.logs.addr` | | 已运行的 VictoriaLogs 的地址，例如 `http://127.0.0.1:9428`。设置后进入外部模式。与 `-victoria.logs.bin` 互斥。 |
| `-victoria.logs.storage.dir` | `/tmp/` | VictoriaLogs 的数据目录（`-storageDataPath`），不存在时自动创建。恢复的分区放在 `<dir>/partitions/<YYYYMMDD>`。 |
| `-victoria.logs.auth.key` | | VictoriaLogs 的 `-partitionManageAuthKey`，访问 `/internal/partition/*` 接口时使用；托管模式下也会传给子进程。 |
| `-max.download.bytes.per.second` | `100mb` | 每个备份的下载限速。单位 `b`、`kb`、`mb`、`gb`，不区分大小写，按 1024 进位。最多同时下载 2 个备份，所以总带宽最多为该值的 2 倍。 |
| `-addr` | `0.0.0.0:8093` | HTTP 监听地址。 |
| `-http.prefix` | | 服务挂载的路径前缀，例如 `/admin/`。前缀之外的路径返回 404；转发给 VictoriaLogs 之前会去掉前缀。`/admin`、`/admin/`、`admin/` 是等价的写法。 |
| `-restore.max.days` | `31` | 单个请求最多恢复多少天。时间范围更长时只恢复最近的这么多天。最小为 1。 |
| `-auto.remove.days` | `0` | 自动删除下载已满这么多天的分区，之后再查询这些日期时重新下载。只支持托管模式。`0` 表示不自动删除。详见 [自动删除恢复的分区](#自动删除恢复的分区)。 |
| `-loglevel` | `info` | 日志级别：`debug`、`info`、`warn`、`error`。 |

运行 `victoria_logs_s3_restore_svr -h` 可查看同样的参数说明。

## 环境变量

| 环境变量 | 说明 |
|---|---|
| `access_key_id` | **必填**。S3 的 access key id。 |
| `secret_access_key` | **必填**。S3 的 secret access key。 |
| `-s3.prefix` 中引用的 `{VAR}` | 例如 `{namespace}`、`{env}`；未设置或为空时启动报错。 |

可以用 `env` 传入：

```bash
env 'access_key_id=xxx' 'secret_access_key=yyy' victoria_logs_s3_restore_svr ...
```

## 示例

托管模式，服务挂在 `/admin/` 下：

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

查询 2026-11-04 全天的日志。如果本地还没有这一天的分区，请求会等到从 S3 恢复完成后再返回结果：

```bash
curl http://127.0.0.1:8093/admin/select/logsql/query \
  -d 'query=error' \
  -d 'start=2026-11-04T00:00:00Z' \
  -d 'end=2026-11-05T00:00:00Z'
```

外部模式，代理到本机已运行的 VictoriaLogs：

```bash
env 'access_key_id=xxx' 'secret_access_key=yyy' namespace=ns1 env=prod \
  victoria_logs_s3_restore_svr \
  -s3.endpoint=https://oss-cn-hongkong.aliyuncs.com \
  -s3.bucket=my-bucket \
  -s3.prefix='victoria_logs/{namespace}/{env}' \
  -victoria.logs.addr=http://127.0.0.1:9428 \
  -victoria.logs.storage.dir=/data/victoria-logs
```

## 哪些请求会触发恢复

去掉 `-http.prefix` 后，路径以 `/select/logsql/` 开头（例如 `query`、`hits`、`field_names`、`stream_field_names` 等），并且带有 `start` 或 `end` 参数的请求。参数可以在 URL 查询串中，也可以在 `application/x-www-form-urlencoded` 的请求体中（请求体优先），与 VictoriaLogs 的取值方式相同。其他请求直接转发。

时间范围的计算与 VictoriaLogs 一致：

* `start`、`end` 支持的格式：`now`；相对时间如 `5m`、`1h30m`、`now-1h`；秒 / 毫秒 / 微秒 / 纳秒级 Unix 时间戳；`YYYY`、`YYYY-MM`、`YYYY-MM-DD`、`YYYY-MM-DDTHH`、`YYYY-MM-DDTHH:MM`、`YYYY-MM-DDTHH:MM:SS`（可带 `Z` 或 `±HH:MM`）；RFC3339。
* 不带时区的时间按本服务所在机器的本地时区解释（与 VictoriaLogs 的行为相同）。
* 范围是 `[start, end)`，`end` 不包含在内。只有 `start` 时，`end` 取当前时间；只有 `end` 时，只恢复 `end` 所在的那一天。
* 按 UTC 日换算成分区名 `YYYYMMDD`；晚于今天（UTC）的日期忽略。
* 覆盖的天数超过 `-restore.max.days` 时，只恢复最近的 `-restore.max.days` 天，并输出警告日志。
* 参数无法解析时不恢复，直接转发，由 VictoriaLogs 向客户端报错。

## 恢复流程

对时间范围内的每一天（多天并行处理）：

1. `<storage.dir>/partitions/<YYYYMMDD>` 已存在：不做任何事。
2. 在 S3 中查找这一天的备份，查找规则见 [恢复时如何查找备份](s3_prefix_cn.md#恢复时如何查找备份)。找不到时直接转发请求。
3. 下载 tar 并流式解包到 `<storage.dir>/.restore_tmp/`，同时校验收到的数据量与 S3 对象大小一致。
4. 解包成功后，原子地 rename 为 `<storage.dir>/partitions/<YYYYMMDD>`。中途失败不会留下半个分区。
5. 让 VictoriaLogs 加载这个分区（见上文两种模式的说明），然后转发请求。

其他细节：

* 请求会一直等到恢复完成再转发。同一天的多个并发请求共用一次恢复；客户端断开时，恢复仍在后台继续。
* 最多同时下载 2 个备份、同时查询 S3 8 次。下载过程中每 30 秒输出一次进度日志。
* 结果缓存：S3 中没有备份的日期、超出保留期的日期缓存 5 分钟；恢复失败的日期 1 分钟内不重试。期间的请求直接转发。
* 启动时清空 `<storage.dir>/.restore_tmp/`，清理上次中断的恢复留下的临时文件。
* 恢复失败只记录错误日志，请求仍然会转发，客户端得到的是缺少这一天数据的结果。
* 下载前检查磁盘空间：拿到备份 tar 的大小后，与 `-victoria.logs.storage.dir` 所在磁盘的剩余空间（扣除其他正在进行的下载尚未写入的部分）比较。空间不足时不下载，记录错误日志，请求**不会转发**，客户端收到 `507 Insufficient Storage`；1 分钟内同一天的请求同样返回 507，之后重新检查。
* 默认**不会自动删除**恢复的分区，请关注 `-victoria.logs.storage.dir` 所在磁盘的空间；托管模式下可以设置 `-auto.remove.days`，见下文。

## 自动删除恢复的分区

托管模式下设置 `-auto.remove.days=N`（N ≥ 1）后，下载时间距今已满 N 天的分区会被自动删除，节约磁盘空间。之后再查询这些日期时，会重新从 S3 下载。不设置（默认 `0`）时不删除任何分区。

* **记录下载时间**：每次从 S3 恢复一个分区后，记录它的日期 `YYYYMMDD` 和下载时间，并把下载时间写成分区目录 `<storage.dir>/partitions/<YYYYMMDD>` 本身的修改时间（mtime）。VictoriaLogs 不会修改分区目录本身的 mtime（查询、写入、合并只会修改其下的 `datadb/`、`indexdb/`），所以下载时间在重启后仍然有效。
* **启动时扫描**：启动时扫描 `<storage.dir>/partitions/`，以每个分区目录的 mtime 作为它的下载时间（晚于当前时间的按当前时间算）。所以启用该参数之前恢复的分区、手动放进去的分区也会按各自的 mtime 被删除。
* **何时删除**：分区只在 VictoriaLogs 子进程停止时删除：
  * 每次启动子进程之前（本服务启动时、热加载失败触发的重启、意外退出后的自动重启），删除已满 N 天的分区；
  * 每天 UTC 00:00 之后检查一次，如果有已满 N 天的分区，就重启子进程，在它停止期间删除这些分区。重启过程与热加载失败时相同：新请求等待，处理中的请求有 30 秒时间完成，实时流被中断。没有要删除的分区时不重启。
* 分区目录先被移动到 `<storage.dir>/.restore_tmp/`，然后在后台删除，所以删除大分区不会拖慢重启。删除中途退出时，剩余的文件在下次启动时清理。
* 因为每天只在 UTC 0 点之后检查，一个分区实际保留的时间在 N 天到 N+1 天之间。
* 删除的依据是**下载时间**，不是最后一次查询的时间：经常被查询的日期同样会在 N 天后被删除，下次查询时重新下载。

## 退出与信号

* 收到 `SIGINT` / `SIGTERM` 时：取消正在进行的恢复，中断 `/select/logsql/tail` 实时流，最多等待 30 秒让其他请求完成，然后停止 VictoriaLogs 子进程（托管模式）并退出；再发一次信号会立即终止进程。
* 退出码：

| 退出码 | 含义 |
|---|---|
| `0` | 收到信号后正常退出。 |
| `1` | 参数检查不通过、S3 / VictoriaLogs 无法访问、VictoriaLogs 子进程无法启动、端口监听失败等。 |
| `2` | 命令行参数无法解析、带有多余的位置参数、`-loglevel` 无效。 |

## 日志

日志为 JSON 格式，输出到 stderr。托管模式下 VictoriaLogs 子进程的日志（也是 JSON 格式）同样输出到 stderr。

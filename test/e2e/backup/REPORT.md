# victoria_logs_s3_backup 端到端试验报告

* 试验日期：2026-11-05（UTC 13:29 – 13:33，一次完整的 `run_all.sh` 从零运行）
* 被测对象：`cmd/victoria_logs_s3_backup`，由当前工作区代码 `make build` 构建（仓库尚无提交）
* 试验脚本：本目录 `test/e2e/backup/`，可一键复现（见第 7 节）

## 1. 结论

备份工具有效。具体证据：

| # | 验证项 | 结果 |
|---|---|---|
| 1 | 3 个分区（前天、昨天两个已封闭分区 + 今天的活跃分区）一次性备份到 RustFS | 全部成功；S3 对象大小 = 工具报告的 tar 大小 |
| 2 | **不使用本项目任何代码**（只用 curl + tar + 官方 VictoriaLogs 镜像）下载备份、解包、启动全新 VictoriaLogs | 每天 300,000 条日志的**全量内容 SHA-256 与写入时的基准完全一致**；服务端 LogsQL 聚合、stream 过滤结果一致 |
| 3 | 只恢复一个分区 | 该分区数据完整，其他日期为空：每个 tar 都是自包含的单日分区 |
| 4 | 大分区（2,000,000 条，tar 91.8 MiB > 64 MiB part size） | 走 S3 multipart（2 个 part），恢复后内容 SHA-256 一致 |
| 5 | 上传限速 `-max.upload.bytes.per.second` | 1 MiB/s 实测 1.011 倍，4 MiB/s 实测 0.987 倍 |
| 6 | 定时模式 `-backup.yesterday.at` | 准点触发，备份前一天，进程常驻并排好下一次；SIGTERM 优雅退出（exit 0） |
| 7 | 11 个异常场景（凭证、桶、VictoriaLogs、参数、上传中途 SIGTERM、上传中途 S3 宕机） | 全部非 0 退出，错误信息清楚；**S3 中没有残留（半个）对象；没有泄漏快照** |
| 8 | 快照清理 | 每次备份后 `snapshot/list` 为 `[]`，`snapshots/` 目录为空 |

## 2. 试验环境

| 项 | 值 |
|---|---|
| 主机 | Apple M5，macOS 27.0.1 |
| Docker | Docker Desktop，Engine 28.3.3（linuxkit VM：10 CPU，7.65 GiB） |
| VictoriaLogs | `victoriametrics/victoria-logs:v1.50.0` |
| S3 存储 | RustFS 1.0.3（`rustfs/rustfs:latest`），容器 `vl-e2e-rustfs`，S3 端口 19000，控制台 19001，桶 `vl-backup-e2e` |
| 备份工具 | `build/victoria_logs_s3_backup`，Go 1.27.0，darwin/arm64，在宿主机上运行 |
| S3 前缀 | `-s3.prefix='victoria_logs/{namespace}/{env}'`，`namespace=e2e env=test` → `victoria_logs/e2e/test/<YYYYMMDD>.tar` |

拓扑：

```
                         宿主机 (macOS)
 vldata.py ingest ──► vl-e2e-source :9428 ──bind mount── .e2e/backup/vl-source-data/
                          ▲  snapshot / force_flush API          │ (快照 = 硬链接目录)
                          │                                      ▼
              victoria_logs_s3_backup ── 读取快照目录 ── tar 流 ── 限速 ──► vl-e2e-rustfs :19000 (S3)
                                                                                │
 curl (SigV4) 下载 ◄─────────────────────────────────────────────────────────────┘
      │ tar -x --strip-components=1 → partitions/<YYYYMMDD>/
      ▼
 vl-e2e-restore :9429 (全新 VictoriaLogs，-retentionPeriod=100y) ◄── vldata.py verify
```

### 与生产启动命令的差异

源 VictoriaLogs 的参数与生产命令一致（`--cpuset-cpus=4,5,6,7 -m 2048m -e GOMAXPROCS=4 -storageDataPath=/data/ -inmemoryDataFlushInterval=30s -memory.allowedPercent=80 -retentionPeriod=3d`），仅有以下改动：

* `--network=host` 改为 `-p 9428:9428`：Docker Desktop for Mac 默认不支持 host 网络。
* 数据目录改为 bind mount `.e2e/backup/vl-source-data/`，宿主机上的备份工具通过它读取快照。VictoriaLogs 报告的快照路径是容器内的 `/data/partitions/...`，工具把它映射到 `-victoria.logs.storage.dir`。这正是生产环境中容器路径与宿主机路径不同的场景。
* 去掉 `--rm`，便于保留容器日志排查问题。

## 3. 试验方法

### 3.1 测试数据（`vldata.py ingest`）

* 3 个 UTC 日期：20261103、20261104、20261105（今天）。每天 300,000 条，均在 3 天保留期内。今天的数据时间戳截止到写入时刻前 2 分钟，所以今天的基准值随运行时间变化，每次运行都会重新计算。
* 数据是确定性生成的（随机种子 = 日期），字段包括 `_time`（纳秒）、`service`、`host`（作为 `_stream_fields`）、`level`、`trace_id`（128 bit 随机）、`seq`、`latency_ms`、`user_id`、`_msg`。
* 写入时记录每天的**基准（ground truth）**：条数、各 level 条数、`sum(latency_ms)`、`trace_id` 去重数、`{service="payment"} level:error` 条数，以及**内容摘要**：按 `<_time 纳秒>\t<service>\t<host>\t<level>\t<trace_id>\t<seq>\t<latency_ms>\t<_msg>` 拼成规范行，排序后整体计算 SHA-256。

### 3.2 校验（`vldata.py verify`）

* 从被测 VictoriaLogs 导出某一天 `_time:[day, day+1)` 的**全部日志**，重新计算上面所有统计值和 SHA-256。
* 另外在服务端执行 LogsQL：`stats count(), sum(latency_ms), count_uniq(trace_id)`，以及带 stream 过滤的 `{service="payment"} level:error | stats count()`。后者依赖 indexdb，用来证明索引也被完整备份。
* 所有值都与基准相等才算通过。SHA-256 相等意味着两边的日志内容逐条一致。

### 3.3 恢复方式（刻意不使用本项目代码）

1. `curl --aws-sigv4` 从 RustFS 下载 `<prefix>/<YYYYMMDD>.tar`；
2. `tar -tvf` 检查：只有一个顶层目录（快照名），包含 `datadb/parts.json` 和 `indexdb/parts.json`，普通文件数等于工具日志中的 `files`；
3. `tar -xf ... --strip-components=1` 解包到空数据目录的 `partitions/<YYYYMMDD>/`；
4. 用官方镜像启动一个全新的 VictoriaLogs 指向该目录，再执行 3.2 的校验。

## 4. 试验结果

### 4.1 数据写入与基线（源实例）

| 日期 | 条数 | 基准 SHA-256（前 16 位） | 源实例校验 |
|---|---|---|---|
| 20261103 | 300,000 | `62099ef8c5c4ddd3` | 通过 |
| 20261104 | 300,000 | `7d5141b08df16e19` | 通过 |
| 20261105 | 300,000 | `9610a48b6c90d7e4` | 通过 |

备份开始前，每个分区目录在磁盘上只有 8 KiB（只有 `parts.json`），数据全部还在内存中（`-inmemoryDataFlushInterval=30s`）。

### 4.2 一次性备份（`-backup.date`，`04_backup.sh`）

| 日期 | exit | S3 key | 文件数 | 数据字节 | tar 字节 | S3 对象大小 | 上传耗时 | 备份后快照 |
|---|---|---|---|---|---|---|---|---|
| 20261103 | 0 | `victoria_logs/e2e/test/20261103.tar` | 27 | 15,016,602 | 15,042,048 | 15,042,048 | 0.26 s | API `[]`，目录 0 |
| 20261104 | 0 | `victoria_logs/e2e/test/20261104.tar` | 27 | 15,015,438 | 15,041,024 | 15,041,024 | 0.33 s | API `[]`，目录 0 |
| 20261105 | 0 | `victoria_logs/e2e/test/20261105.tar` | 27 | 14,957,511 | 14,982,656 | 14,982,656 | 0.44 s | API `[]`，目录 0 |

* 备份今天的分区（20261105）时，工具输出 WARN：`backing up a partition that may still receive logs; the backup will contain only logs ingested so far`，行为符合设计。
* `{namespace}`、`{env}` 环境变量在 S3 前缀中被正确展开。

### 4.3 恢复校验（核心证据，`05_restore_verify.sh`）

**(a) 只恢复 20261104 一个分区**：新实例 `partition/list` = `["20261104"]`。

| 日期 | 恢复实例条数 | SHA-256 | 结果 |
|---|---|---|---|
| 20261103 | 0 | – | 通过（预期为空） |
| 20261104 | 300,000 | `7d5141b08df16e19` = 基准 | 通过（10/10 项一致） |
| 20261105 | 0 | – | 通过（预期为空） |

**(b) 恢复全部 3 个分区**：新实例 `partition/list` = `["20261103","20261104","20261105"]`。

| 日期 | tar 顶层目录（快照名） | tar 文件数 / 工具报告 | 恢复实例条数 | 恢复实例 SHA-256 | 基准 SHA-256 | 服务端查询 | 结果 |
|---|---|---|---|---|---|---|---|
| 20261103 | `20261105132944-18E5293BFBC0CCE6` | 27 / 27 | 300,000 | `62099ef8c5c4ddd3` | `62099ef8c5c4ddd3` | 一致 | 通过 |
| 20261104 | `20261105132944-18E5293BFBC0CCE7` | 27 / 27 | 300,000 | `7d5141b08df16e19` | `7d5141b08df16e19` | 一致 | 通过 |
| 20261105 | `20261105132945-18E5293BFBC0CCE8` | 27 / 27 | 300,000 | `9610a48b6c90d7e4` | `9610a48b6c90d7e4` | 一致 | 通过 |

以 20261104 为例，10 项校验全部一致：

```
count=300000  by_level={debug:15084, error:29992, info:210152, warn:44772}
sum_latency_ms=299599327  uniq_trace_id=300000  payment_errors=4956
server: count=300000 sum_latency_ms=299599327 uniq_trace_id=300000 payment_errors=4956
sha256=7d5141b08df16e19946381e2b00ada64a531162b9c3c58d81f9cf14fe464928e
```

### 4.4 大分区 / multipart 上传（`09_large_partition.sh`）

工具的 multipart part size 为 64 MiB，4.2 中的 tar 都小于一个 part。为覆盖生产中常见的多 part 上传路径，单独启动一个 VictoriaLogs 实例，写入 20261104 的 2,000,000 条日志：

| 条数 | tar 字节 | S3 ETag | 文件数 | 上传耗时 | 源 SHA-256 | 恢复后 SHA-256 |
|---|---|---|---|---|---|---|
| 2,000,000 | 96,224,768（91.8 MiB） | `4dea67d4…-2`（2 个 part） | 47 | 2.36 s | `9d8f69682563b4f8` | `9d8f69682563b4f8` |

### 4.5 上传限速（`06_rate_limit.sh`）

同一个 tar（15,041,024 字节）分别用三个限速值上传：

| 限速 | 限速值 (B/s) | 上传耗时 | 实测 (B/s) | 实测 / 限速 |
|---|---|---|---|---|
| `1mb` | 1,048,576 | 14.19 s | 1,059,783 | 1.011 |
| `4mb` | 4,194,304 | 3.63 s | 4,138,536 | 0.987 |
| `100mb` | 104,857,600 | 0.21 s | 70,636,208 | 0.674（未触顶，受本机 I/O 限制） |

限速生效，误差约 1%。单位按 1024 进制解释，与 README 一致。

### 4.6 定时模式（`07_scheduled.sh`）

参数为 `-backup.yesterday.at=1331`，前缀为 `victoria_logs/{namespace}/{env}/scheduled/${YYYYMMDD_HHMISS}`。工具日志的时间为本地时区 +08:00，21:31 即 13:31 UTC：

```
21:30:33 running in scheduled mode
21:30:33 next backup scheduled  at=2026-11-05T13:31:00Z date=20261104 wait_seconds=26
21:31:00 starting scheduled backup  date=20261104 attempt=1
21:31:00 backup completed  key=victoria_logs/e2e/test/scheduled/20261105_133100/20261104.tar bytes=15041024
21:31:00 snapshot deleted
21:31:00 next backup scheduled  at=2026-11-06T13:31:00Z date=20261105 wait_seconds=86399
21:31:03 scheduler stopped        (收到 SIGTERM，exit code 0)
```

* 准点（13:31:00 UTC）触发，备份的是前一个 UTC 日 20261104；
* `${YYYYMMDD_HHMISS}` 被展开为备份开始时间 `20261105_133100`；
* 备份完成后进程不退出，并安排了第二天同一时刻的下一次备份；
* SIGTERM 后优雅退出，退出码为 0。

### 4.7 异常场景（`08_failure_cases.sh`）

每个场景都检查四点：退出码非 0、错误信息符合预期、S3 中没有对应对象、没有遗留快照。

| 场景 | exit | 错误信息（节选） | 残留对象 | 遗留快照 | 结果 |
|---|---|---|---|---|---|
| 缺少 S3 凭证环境变量 | 1 | `environment variables "auth.access_key_id" and "auth.secret_access_key" must be set` | 无 | 0 | 通过 |
| secret key 错误 | 1 | `cannot access S3 bucket "vl-backup-e2e": Access Denied.` | 无 | 0 | 通过 |
| 桶不存在 | 1 | `S3 bucket "no-such-bucket-e2e" does not exist` | 无 | 0 | 通过 |
| 前缀引用的环境变量未设置 | 1 | `environment variable "namespace" referenced by template ... is not set or empty` | 无 | 0 | 通过 |
| VictoriaLogs 不可达 | 1 | `VictoriaLogs at -victoria.logs.addr="http://127.0.0.1:9" is not reachable` | 无 | 0 | 通过 |
| 存储目录不存在 | 1 | `cannot access -victoria.logs.storage.dir="/nonexistent/vl"` | 无 | 0 | 通过 |
| 分区不存在（20200101） | 1 | `partition directory not found: ".../partitions/20200101"` | 无 | 0 | 通过 |
| 非法日期（20261332） | 1 | `invalid -backup.date: "20261332" is not a valid YYYYMMDD date` | 无 | 0 | 通过 |
| `-backup.date` 与 `-backup.yesterday.at` 同时指定 | 1 | `-backup.date and -backup.yesterday.at are mutually exclusive` | 无 | 0 | 通过 |
| 上传中途 SIGTERM（限速 1 MiB/s，第 4 秒） | 1 | `backup of partition 20261104 interrupted: context canceled`，随后 `snapshot deleted` | 无 | 0 | 通过 |
| 上传中途 RustFS 宕机（`docker stop`） | 1 | `cannot upload s3://vl-backup-e2e/...: Put "http://127.0.0.1:19000/...": ...`，随后 `snapshot deleted` | 无 | 0 | 通过 |

最后两项说明：备份中断时不会在 S3 中留下半个 tar，也不会遗留快照，即使在收到 SIGTERM 的情况下快照仍会被清理。

## 5. 观察与说明

* **快照与 force_flush**：备份前数据全部在内存中，备份结果却是完整的。为了确认原因，另做了一个单独实验：在 v1.50.0 上写入 30 万条后，**不调用** `force_flush` 直接执行 `snapshot/create`，得到的快照同样完整（14.7 MB，27 个文件）。也就是说，v1.50.0 的快照接口自身会先落盘内存数据。工具在快照前调用 `/internal/force_flush` 是无害的保险措施，但在 v1.50.0 上不是数据完整的必要条件。
* **硬链接快照在 Docker Desktop 的 bind mount 上工作正常**：快照创建、宿主机读取、通过 API 删除都没有问题。
* **今天的分区可以备份**：备份内容为截至备份时刻已写入的数据，工具会给出 WARN。生产中应使用 `-backup.yesterday.at` 备份已封闭的前一天。
* `100mb` 限速下实测约 67 MiB/s，没有达到限速值，瓶颈是本机磁盘与回环网络，不是工具问题。

## 6. 未覆盖的范围

* 真实云存储：未测试阿里云 OSS（https、virtual-hosted style 地址）。RustFS 使用 http + path style。
* 备份进行中同时向**同一分区**持续写入的一致性未专门测试。今天分区的测试是在写入停止后进行的。
* 规模：最大测试分区为 2,000,000 条 / 91.8 MiB（2 个 part），GB 级分区未测，其代码路径与本次相同，只是 part 数更多。
* 定时模式的失败重试（1 分钟、2 分钟后重试）未在端到端中触发，只有单元测试覆盖。
* `-victoria.logs.auth.key`（`-partitionManageAuthKey`）未测试。
* 恢复服务器 `victoria_logs_s3_restore_svr` 不在本报告范围内。
* 运行环境为 macOS + Docker Desktop，生产环境为 Linux + host 网络。路径映射逻辑（容器内 `/data` → 宿主机目录）已覆盖。

## 7. 复现

前置条件：Docker、Go、python3（只用标准库）、curl（需支持 `--aws-sigv4`，7.75+）。

```bash
./test/e2e/backup/run_all.sh     # 从零运行全部步骤，约 3.5 分钟；任一步失败即中止
./test/e2e/backup/99_cleanup.sh  # 删除容器和 .e2e/backup/ 工作目录
```

运行结束后容器保持运行，方便手工检查：

* RustFS 控制台：<http://127.0.0.1:19001>，账号密码见 `env.sh` 中的 `S3_ACCESS_KEY` / `S3_SECRET_KEY`
* 源 VictoriaLogs：<http://127.0.0.1:9428/select/vmui/>
* 恢复出来的 VictoriaLogs：<http://127.0.0.1:9429/select/vmui/>
* 所有日志、tar 清单、校验 JSON：`.e2e/backup/logs/`

| 脚本 | 作用 |
|---|---|
| `env.sh` | 公共配置（端口、容器名、凭证、日期）和辅助函数；所有值都可以用环境变量覆盖 |
| `01_start_rustfs.sh` | 启动 RustFS，创建桶 |
| `02_start_victorialogs.sh` | 按生产参数启动源 VictoriaLogs |
| `03_ingest_logs.sh` | 写入 3 天测试数据，记录基准，校验源实例 |
| `04_backup.sh` | 一次性备份 3 个分区，检查对象大小和快照清理 |
| `05_restore_verify.sh` | 用 curl + tar + 官方镜像恢复并做全量校验（`RESTORE_DAYS` 可指定只恢复部分日期） |
| `06_rate_limit.sh` | 限速测试 |
| `07_scheduled.sh` | 定时模式与 SIGTERM 退出 |
| `08_failure_cases.sh` | 11 个异常场景 |
| `09_large_partition.sh` | 大分区 multipart 测试 |
| `vldata.py` | 确定性数据生成（`ingest`）与全量校验（`verify`） |
| `99_cleanup.sh` | 清理 |

常用可调参数：`LOGS_PER_DAY`（默认 300000）、`LARGE_LOGS`（默认 2000000）、`RATE_LIMITS`（默认 `"1mb 4mb 100mb"`）、`E2E_WORK_DIR`（默认 `<repo>/.e2e/backup`）、`RUSTFS_IMAGE`、`VL_IMAGE`、`VL_CPUSET`。

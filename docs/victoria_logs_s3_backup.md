# victoria_logs_s3_backup

把 VictoriaLogs 某一天（UTC）的分区做成快照，以 tar 流的形式上传到 S3 对象 `<s3.prefix>/<YYYYMMDD>.tar`。

有两种运行模式，必须且只能选一种：

* **一次性备份**（`-backup.date=YYYYMMDD`）：备份指定日期的分区后退出。
* **定时备份**（`-backup.yesterday.at=HHMM`）：常驻运行，每天在指定的 UTC 时刻备份前一个 UTC 日的分区。

> 工具直接读取 VictoriaLogs 数据目录下的快照文件，所以必须和 VictoriaLogs 运行在同一台机器上（或挂载同一个数据目录）。

## 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-s3.endpoint` | | **必填**。S3 endpoint，例如 `https://oss-cn-hongkong.aliyuncs.com`。 |
| `-s3.region` | | S3 region，例如 `cn-hongkong`。 |
| `-s3.bucket` | | **必填**。S3 bucket 名。 |
| `-s3.prefix` | | 对象 key 的前缀模板，支持 `{环境变量}` 和 `${时间格式}` 占位符，详见 [-s3.prefix 前缀模板](s3_prefix.md)。 |
| `-victoria.logs.addr` | | **必填**。VictoriaLogs 的 HTTP 地址，例如 `http://127.0.0.1:9428`。启动时会访问 `/health` 检查是否可达。 |
| `-victoria.logs.auth.key` | | 访问 `/internal/partition/*` 等接口时使用的 auth key，即 VictoriaLogs 的 `-partitionManageAuthKey`。VictoriaLogs 未设置时留空。 |
| `-victoria.logs.storage.dir` | | **必填**。VictoriaLogs 的 `-storageDataPath` 在本机上的路径。启动时检查目录是否存在。 |
| `-backup.date` | | 一次性备份这一天（UTC）的分区，格式 `YYYYMMDD`，完成后退出。与 `-backup.yesterday.at` 互斥。 |
| `-backup.yesterday.at` | | 常驻运行，每天在这个 UTC 时刻备份前一个 UTC 日的分区，格式 `HHMM`（24 小时制），例如 `0015`。与 `-backup.date` 互斥。 |
| `-max.upload.bytes.per.second` | `100mb` | 上传限速。单位 `b`、`kb`、`mb`、`gb`，不区分大小写，按 1024 进位；不带单位按字节计；可以写小数，例如 `1.5mb`。必须大于 0。 |
| `-loglevel` | `info` | 日志级别：`debug`、`info`、`warn`、`error`。 |

运行 `victoria_logs_s3_backup -h` 可查看同样的参数说明。

## 环境变量

| 环境变量 | 说明 |
|---|---|
| `auth.access_key_id` | **必填**。S3 的 access key id。 |
| `auth.secret_access_key` | **必填**。S3 的 secret access key。 |
| `-s3.prefix` 中引用的 `{VAR}` | 例如 `{namespace}`、`{env}`；未设置或为空时启动报错。 |

变量名里带点号，shell 的 `export` 无法设置，请用 `env` 传入：

```bash
env 'auth.access_key_id=xxx' 'auth.secret_access_key=yyy' victoria_logs_s3_backup ...
```

## 示例

一次性备份 `20261104` 分区：

```bash
env 'auth.access_key_id=xxx' 'auth.secret_access_key=yyy' namespace=ns1 env=prod \
  victoria_logs_s3_backup \
  -s3.endpoint=https://oss-cn-hongkong.aliyuncs.com \
  -s3.region=cn-hongkong \
  -s3.bucket=my-bucket \
  -s3.prefix='victoria_logs/{namespace}/{env}' \
  -victoria.logs.addr=http://127.0.0.1:9428 \
  -victoria.logs.storage.dir=/data/victoria-logs \
  -backup.date=20261104
# 上传到 s3://my-bucket/victoria_logs/ns1/prod/20261104.tar
```

每天 00:15（UTC）备份前一天的分区，限速 50 MiB/s，常驻运行：

```bash
env 'auth.access_key_id=xxx' 'auth.secret_access_key=yyy' namespace=ns1 env=prod \
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

## 备份流程

1. 检查参数；检查 S3 bucket 能否访问、VictoriaLogs 能否访问、`-victoria.logs.storage.dir` 是否存在。任何一项失败都直接退出。
2. 检查 `<storage.dir>/partitions/<YYYYMMDD>` 是否存在。不存在说明这一天没有数据，或者 `-victoria.logs.storage.dir` 配错了。
3. 调用 `/internal/force_flush`，让刚写入的日志落盘（失败只记警告）。
4. 调用 `/internal/partition/snapshot/create?partition_prefix=<YYYYMMDD>` 创建快照，前后各调用一次 `/internal/partition/snapshot/list`，找出新建的快照。
5. 把 VictoriaLogs 返回的快照路径中从 `partitions/` 开始的部分拼到 `-victoria.logs.storage.dir` 后面，得到本地快照目录。因此 VictoriaLogs 跑在容器里、挂载点和本机路径不同也没关系。
6. 把快照目录打成 tar 流，经过限速后以 multipart 方式上传到 `<展开后的 s3.prefix>/<YYYYMMDD>.tar`。全程流式处理，不在本地落盘。
7. 上传完成后检查 S3 对象的大小与 tar 流大小一致。
8. 删除快照。无论备份成功还是失败都会删除；VictoriaLogs 版本较旧、没有删除接口时，直接删除本地快照目录。

上传过程中每 30 秒输出一次进度日志。

### tar 文件格式

tar 不压缩，内容与在 `partitions/<YYYYMMDD>/snapshots/` 目录下执行 `tar -cf <YYYYMMDD>.tar ./<快照名>` 相同，即 `<快照名>/indexdb/...` 和 `<快照名>/datadb/...`。每个 tar 都是自包含的单日分区，不依赖本项目也能手工恢复，把快照目录解包成 VictoriaLogs 数据目录下的 `partitions/<YYYYMMDD>` 即可：

```bash
mkdir -p /data/victoria-logs/partitions/20261104
tar -xf 20261104.tar -C /data/victoria-logs/partitions/20261104 --strip-components=1
# 然后重启 VictoriaLogs，或调用 /internal/partition/attach?name=20261104 热加载
```

符号链接、设备文件等非普通文件会被跳过，并输出警告日志。

单个对象的 multipart 分片大小为 64 MiB，最多 10000 片，所以单个分区的 tar 最大 640 GiB。S3 对象只有在全部上传成功后才可见，中途失败不会留下半个对象。

### 备份当天的分区

`-backup.date` 指定今天或未来的日期时，工具会输出警告：这个分区可能还在写入，备份只包含已经写入的日志。

## 定时模式

* 时间一律按 UTC 计算：每天到达 `-backup.yesterday.at` 指定的 UTC 时刻时，备份前一个 UTC 日的分区（VictoriaLogs 的分区本身就是按 UTC 日划分的）。
* 一次备份失败时会重试，最多尝试 3 次，间隔分别为 1 分钟、2 分钟。分区目录不存在时不重试。
* 3 次都失败时记录错误日志，进程不退出，等第二天的定时继续。
* 等待期间每分钟检查一次系统时间，系统休眠或时钟调整后不会错过备份。

## 退出与信号

* 收到 `SIGINT` / `SIGTERM` 时中断正在进行的备份（会删除快照，不会留下半个 S3 对象）并退出；再发一次信号会立即终止进程。
* 退出码：

| 退出码 | 含义 |
|---|---|
| `0` | 一次性备份成功；或定时模式收到信号正常退出。 |
| `1` | 参数检查不通过、S3 / VictoriaLogs 无法访问、一次性备份失败或被信号中断。 |
| `2` | 命令行参数无法解析、带有多余的位置参数、`-loglevel` 无效。 |

## 日志

日志为 JSON 格式，输出到 stderr。一次备份成功后会输出一条 `backup completed`，包含对象 key、文件数、tar 大小、耗时、平均速度等信息。

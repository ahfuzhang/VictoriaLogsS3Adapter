# VictoriaLogsS3Adapter

把 VictoriaLogs 的日志按天备份到 S3，并在查询时按需从 S3 恢复。

## 目标

VictoriaLogs 把日志按 UTC 日存放在本地磁盘的分区里（`<storageDataPath>/partitions/<YYYYMMDD>`）。日志量大时，本地磁盘只适合保留最近一段时间的数据，更早的数据放到 S3 更便宜，但查询历史日志时又需要把数据找回来。

本项目提供两个命令行工具来解决这个问题：

* **`victoria_logs_s3_backup`**：备份工具。为 VictoriaLogs 的某一天分区创建快照，以 tar 流的形式上传到 S3 对象 `<s3.prefix>/<YYYYMMDD>.tar`。可以一次性备份指定日期，也可以常驻运行，每天定时备份前一天的数据。
* **`victoria_logs_s3_restore_svr`**：恢复服务器。挡在 VictoriaLogs 前面的 HTTP 代理；查询的时间范围内缺少某一天的数据时，自动从 S3 下载这一天的备份并加载到 VictoriaLogs，然后返回查询结果。对客户端来说，它就是一个能查到全部历史数据的 VictoriaLogs。

```
VictoriaLogs ──► victoria_logs_s3_backup ──► S3: <s3.prefix>/<YYYYMMDD>.tar
                                             │
                                             │ 本地缺少的日期按需下载
                                             ▼
client ──► victoria_logs_s3_restore_svr ◄────┘
                        │
                        ▼ 恢复后转发查询
                  VictoriaLogs（历史数据）
```

## 用法

### 1. 构建

```bash
make build    # 输出到 build/
make docker   # 或构建包含 VictoriaLogs 和两个工具的 Docker 镜像
```

### 2. 准备 S3 凭证

两个工具都从环境变量 `auth.access_key_id` 和 `auth.secret_access_key` 读取 S3 凭证。变量名带点号，不能用 `export` 设置，请用 `env` 传入。其他配置全部来自命令行参数。

### 3. 备份

在 VictoriaLogs 所在的机器上运行（工具需要读取 VictoriaLogs 的数据目录）。下面的命令每天 00:15（UTC）把前一天的分区上传到 `s3://my-bucket/victoria_logs/prod/<YYYYMMDD>.tar`：

```bash
env 'auth.access_key_id=xxx' 'auth.secret_access_key=yyy' env=prod \
  ./build/victoria_logs_s3_backup \
  -s3.endpoint=https://oss-cn-hongkong.aliyuncs.com \
  -s3.region=cn-hongkong \
  -s3.bucket=my-bucket \
  -s3.prefix='victoria_logs/{env}' \
  -victoria.logs.addr=http://127.0.0.1:9428 \
  -victoria.logs.storage.dir=/data/victoria-logs \
  -backup.yesterday.at=0015
```

把 `-backup.yesterday.at=0015` 换成 `-backup.date=20261104`，就是一次性备份指定日期后退出。

### 4. 恢复与查询

启动恢复服务器，由它管理一个 VictoriaLogs 子进程。`-s3.prefix` 必须与备份时相同：

```bash
env 'auth.access_key_id=xxx' 'auth.secret_access_key=yyy' env=prod \
  ./build/victoria_logs_s3_restore_svr \
  -s3.endpoint=https://oss-cn-hongkong.aliyuncs.com \
  -s3.region=cn-hongkong \
  -s3.bucket=my-bucket \
  -s3.prefix='victoria_logs/{env}' \
  -victoria.logs.bin=/usr/local/bin/victoria-logs-prod \
  -victoria.logs.storage.dir=/data/vl_restore \
  -addr=0.0.0.0:8093
```

然后像访问 VictoriaLogs 一样访问 `:8093`。本地没有的日期会先从 S3 恢复，再返回结果：

```bash
curl http://127.0.0.1:8093/select/logsql/query \
  -d 'query=error' -d 'start=2026-11-04T00:00:00Z' -d 'end=2026-11-05T00:00:00Z'
```

运行 `<工具> -h` 可查看全部命令行参数。

## 文档

* [victoria_logs_s3_backup](docs/victoria_logs_s3_backup.md)：备份工具的全部参数、运行模式、备份流程、tar 格式、定时与重试、退出码。
* [victoria_logs_s3_restore_svr](docs/victoria_logs_s3_restore_svr.md)：恢复服务器的全部参数、托管模式与外部模式、触发恢复的请求、恢复流程、缓存与限制。
* [-s3.prefix 前缀模板](docs/s3_prefix.md)：`{环境变量}` 和 `${时间格式}` 占位符的写法、示例、错误写法，以及恢复时如何查找备份。
* [构建与部署](docs/build.md)：make 目标、交叉编译、Docker 镜像、测试。

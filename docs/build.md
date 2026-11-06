# 构建与部署

## 用 make 构建

需要 Go 1.27 或更高版本。编译参数为 `CGO_ENABLED=0 -trimpath -ldflags '-s -w'`，产物是静态链接的二进制文件。

| 命令 | 说明 |
|---|---|
| `make build` | 编译本机平台，输出到 `build/` |
| `make linux` | 编译 linux/amd64 和 linux/arm64，输出到 `build/linux_amd64/`、`build/linux_arm64/` |
| `make cross` | 编译 linux、darwin 的 amd64 / arm64 全部平台，输出到 `build/<os>_<arch>/` |
| `make linux/amd64` | 只编译某一个平台 |
| `make docker` | 构建 Docker 镜像 `victoria-logs-s3-adapter:latest` |
| `make test` | 运行单元测试 |
| `make vet` | 运行 `go vet` |
| `make fmt` | 格式化代码 |
| `make clean` | 删除 `build/` |
| `make help` | 列出所有目标 |

可以通过变量覆盖默认值，例如：

```bash
make build BUILD_DIR=out LDFLAGS=
make cross PLATFORMS="linux/amd64 windows/amd64"
make docker TAG=v1.0.0 DOCKER_BUILD_FLAGS='--platform linux/amd64'
```

## Docker 镜像

镜像基于 Alpine，包含三个可执行文件：

| 路径 | 说明 |
|---|---|
| `/usr/local/bin/victoria-logs-prod` | VictoriaLogs，取自官方镜像 `victoriametrics/victoria-logs` |
| `/usr/local/bin/victoria_logs_s3_backup` | 备份工具 |
| `/usr/local/bin/victoria_logs_s3_restore_svr` | 恢复服务器 |

镜像没有设置 `ENTRYPOINT`，每个容器按需选择要运行的程序。暴露端口 `8093`（恢复服务器的默认 `-addr`）和 `9428`（VictoriaLogs 的默认 `-httpListenAddr`）。

```bash
# 构建
docker build -t victoria-logs-s3-adapter .

# 多架构构建并推送
docker buildx build --platform linux/amd64,linux/arm64 -t <repo>:<tag> --push .

# 指定依赖版本（以下为默认值）
docker build \
  --build-arg GO_VERSION=1.27 \
  --build-arg VICTORIA_LOGS_VERSION=v1.50.0 \
  --build-arg ALPINE_VERSION=3.23 \
  -t victoria-logs-s3-adapter .
```

运行恢复服务器（托管模式，VictoriaLogs 作为子进程运行在同一个容器里）：

```bash
docker run -d -p 8093:8093 \
  -e 'auth.access_key_id=xxx' -e 'auth.secret_access_key=yyy' \
  -e namespace=ns1 -e env=prod \
  -v /data/vl_restore:/data \
  victoria-logs-s3-adapter \
  victoria_logs_s3_restore_svr \
  -s3.endpoint=https://oss-cn-hongkong.aliyuncs.com \
  -s3.bucket=my-bucket \
  -s3.prefix='victoria_logs/{namespace}/{env}' \
  -victoria.logs.bin=/usr/local/bin/victoria-logs-prod \
  -victoria.logs.storage.dir=/data
```

备份工具需要读取 VictoriaLogs 的数据目录，在容器中运行时要挂载与 VictoriaLogs 相同的数据卷。

## 测试

```bash
go test ./...

# s3store 的集成测试需要一个 S3 兼容服务
S3STORE_TEST_ENDPOINT=http://127.0.0.1:19000 S3STORE_TEST_BUCKET=e2e-test S3STORE_TEST_LARGE=1 go test ./internal/s3store/
```

备份工具的端到端测试脚本和报告见 [test/e2e/backup/](../test/e2e/backup/REPORT.md)。

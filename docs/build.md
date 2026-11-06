# Build and Deployment

## Build with make

Requires Go 1.27 or later. The build flags are `CGO_ENABLED=0 -trimpath -ldflags '-s -w'`, producing statically linked binaries.

| Command | Description |
|---|---|
| `make build` | Build for the current platform, output to `build/` |
| `make linux` | Build linux/amd64 and linux/arm64, output to `build/linux_amd64/` and `build/linux_arm64/` |
| `make cross` | Build all linux and darwin platforms (amd64 / arm64), output to `build/<os>_<arch>/` |
| `make linux/amd64` | Build a single platform only |
| `make docker` | Build the Docker image `victoria-logs-s3-adapter:latest` |
| `make test` | Run unit tests |
| `make vet` | Run `go vet` |
| `make fmt` | Format the code |
| `make clean` | Delete `build/` |
| `make help` | List all targets |

Defaults can be overridden with variables, for example:

```bash
make build BUILD_DIR=out LDFLAGS=
make cross PLATFORMS="linux/amd64 windows/amd64"
make docker TAG=v1.0.0 DOCKER_BUILD_FLAGS='--platform linux/amd64'
```

## Docker image

The image is based on Alpine and contains three executables:

| Path | Description |
|---|---|
| `/usr/local/bin/victoria-logs-prod` | VictoriaLogs, taken from the official image `victoriametrics/victoria-logs` |
| `/usr/local/bin/victoria_logs_s3_backup` | Backup tool |
| `/usr/local/bin/victoria_logs_s3_restore_svr` | Restore server |

The image sets no `ENTRYPOINT`; each container chooses which program to run. Ports `8093` (the restore server's default `-addr`) and `9428` (VictoriaLogs' default `-httpListenAddr`) are exposed.

```bash
# Build
docker build -t victoria-logs-s3-adapter .

# Multi-arch build and push
docker buildx build --platform linux/amd64,linux/arm64 -t <repo>:<tag> --push .

# Specify dependency versions (defaults shown below)
docker build \
  --build-arg GO_VERSION=1.27 \
  --build-arg VICTORIA_LOGS_VERSION=v1.50.0 \
  --build-arg ALPINE_VERSION=3.23 \
  -t victoria-logs-s3-adapter .
```

Run the restore server (managed mode, with VictoriaLogs running as a child process in the same container):

```bash
docker run -d -p 8093:8093 \
  -e 'access_key_id=xxx' -e 'secret_access_key=yyy' \
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

The backup tool needs to read VictoriaLogs' data directory, so when running in a container, mount the same data volume as VictoriaLogs.

## Testing

```bash
go test ./...

# The s3store integration tests need an S3-compatible service
S3STORE_TEST_ENDPOINT=http://127.0.0.1:19000 S3STORE_TEST_BUCKET=e2e-test S3STORE_TEST_LARGE=1 go test ./internal/s3store/
```


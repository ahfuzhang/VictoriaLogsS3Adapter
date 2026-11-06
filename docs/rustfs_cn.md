# RustFS：本地 S3 模拟环境

[RustFS](https://github.com/rustfs/rustfs) 是用 Rust 编写、兼容 S3 协议的对象存储。
本项目用它在开发/测试机上模拟 S3，以便在不依赖真实云存储的情况下调试
`victoria_logs_s3_backup` 与 `victoria_logs_s3_restore_svr`。

本文记录使用 Docker 安装、启停、验证，以及与本项目配合的方法。
本机已按本文部署并验证（镜像 `rustfs/rustfs:1.0.4`）。

## 1. 本机当前部署

| 项目 | 值 |
| --- | --- |
| 容器名 | `rustfs`（`--restart unless-stopped`，Docker 启动后自动拉起） |
| S3 endpoint | `http://<主机IP>:9100`（监听 `0.0.0.0`，本机也可用 `http://127.0.0.1:9100`） |
| 控制台 | `http://<主机IP>:9101/rustfs/console/` |
| Access Key / Secret Key | `rustfsadmin` / `rustfsadmin` |
| Region | `us-east-1`（任意值均可，客户端需与签名一致） |
| 数据目录 | 宿主机 `~/rustfs/data`（挂载到容器 `/data`） |
| 日志目录 | 宿主机 `~/rustfs/logs`（挂载到容器 `/logs`） |

> 端口说明：9000/9001 常被 MinIO/QuestDB 占用（本机 9000 已被 QuestDB 使用），
> 因此宿主机映射为 9100/9101；`test/e2e/backup` 使用 19000/19001，互不冲突。
> 容器内部 S3 端口为 9000、控制台为 9001。

> 安全提醒：监听 `0.0.0.0` 意味着同网络内任何机器都能访问，且默认密钥是公开的 `rustfsadmin`。
> 仅限内网/测试环境使用；需要时改用强密钥，或用防火墙限制来源。

## 2. 安装与启动

```bash
mkdir -p ~/rustfs/data ~/rustfs/logs

docker run -d --name rustfs --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -p 0.0.0.0:9100:9000 -p 0.0.0.0:9101:9001 \
  -v ~/rustfs/data:/data -v ~/rustfs/logs:/logs \
  -e RUSTFS_ACCESS_KEY=rustfsadmin -e RUSTFS_SECRET_KEY=rustfsadmin \
  rustfs/rustfs:1.0.4
```

要点：

- `-p 0.0.0.0:9100:9000`：宿主机在所有网卡上监听（要仅本机访问，改为 `127.0.0.1:9100:9000`）。
- `--user "$(id -u):$(id -g)"`：镜像默认以内置用户 `rustfs` 运行，对挂载的宿主机目录没有写权限，
  会报 `[FATAL] Server runtime failed: Io error: Permission denied (os error 13)` 并不断重启；
  用当前用户身份运行即可。也可改用 Docker 命名卷（`-v rustfs-data:/data`）规避权限问题。
- 镜像版本固定为 `1.0.4`，如需升级见第 6 节。
- 容器内监听地址默认就是 `0.0.0.0`，对外是否可达由 `-p` 决定。

### 常用命令

```bash
docker ps --filter name=rustfs     # 状态
docker logs -f rustfs              # 日志
docker stop rustfs                 # 停止
docker start rustfs                # 启动
docker rm -f rustfs                # 删除容器（数据仍在 ~/rustfs/data）
```

### 环境变量

| 变量 | 含义 |
| --- | --- |
| `RUSTFS_ACCESS_KEY` / `RUSTFS_SECRET_KEY` | 管理员凭证（使用默认值时日志会有警告，仅测试可忽略） |
| `RUSTFS_VOLUMES` | 数据目录，镜像内默认 `/data` |
| `RUSTFS_ADDRESS` | 容器内 S3 监听地址（默认 `:9000`） |
| `RUSTFS_CONSOLE_ENABLE` / `RUSTFS_CONSOLE_ADDRESS` | 控制台开关与监听地址 |

## 3. 验证（仅需 curl，无需 aws cli / mc）

curl ≥ 7.75 支持 `--aws-sigv4` 直接签名。注意 bucket 名至少 3 个字符（`InvalidBucketName`）。

```bash
s3() { curl -sS --aws-sigv4 "aws:amz:us-east-1:s3" --user rustfsadmin:rustfsadmin "$@"; }
B=http://127.0.0.1:9100          # 远程机器上用 http://<主机IP>:9100

s3 -X PUT $B/test-bucket                                   # 创建 bucket（200）
echo hello > /tmp/h.txt
s3 -X PUT --data-binary @/tmp/h.txt $B/test-bucket/a/h.txt # 上传对象（200）
s3 $B/test-bucket/a/h.txt                                  # 下载：hello
s3 "$B/test-bucket?list-type=2&prefix=a/"                  # ListObjectsV2
s3 -X DELETE $B/test-bucket/a/h.txt                        # 删除对象（204）
s3 -X DELETE $B/test-bucket                                # 删除 bucket（204，需为空）
```

控制台：浏览器访问 `http://<主机IP>:9101/rustfs/console/`，用 Access/Secret Key 登录，
可图形化管理 bucket 与对象。

如果安装了 `aws` CLI：

```bash
export AWS_ACCESS_KEY_ID=rustfsadmin AWS_SECRET_ACCESS_KEY=rustfsadmin AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://127.0.0.1:9100 s3 ls
```

## 4. 与本项目配合

先创建 bucket，再把 endpoint 与凭证传给备份/恢复程序。RustFS 使用 path-style 访问
（地址形如 `http://127.0.0.1:9100/<bucket>/<key>`），需确保程序按 path-style 访问。

```bash
curl -sS --aws-sigv4 "aws:amz:us-east-1:s3" --user rustfsadmin:rustfsadmin \
  -X PUT http://127.0.0.1:9100/vl-backup
```

具体命令行参数（endpoint、bucket、access key、prefix 模板等）见：

- [victoria_logs_s3_backup_cn.md](victoria_logs_s3_backup_cn.md)
- [victoria_logs_s3_restore_svr_cn.md](victoria_logs_s3_restore_svr_cn.md)
- [s3_prefix_cn.md](s3_prefix_cn.md)

端到端脚本见 `test/e2e/backup/`，它会自行用 Docker 拉起独立的 RustFS 实例。

## 5. 如果备份程序也跑在容器里

容器内的 `127.0.0.1` 指向容器自己。此时 endpoint 应使用主机的局域网 IP
（`http://<主机IP>:9100`），或让容器加入同一 Docker 网络后用 `http://rustfs:9000`。

## 6. 日常维护与排错

| 场景 | 做法 |
| --- | --- |
| 清空所有数据 | `docker rm -f rustfs && rm -rf ~/rustfs/data/* && ` 重新执行第 2 节的 `docker run` |
| 升级 | `docker pull rustfs/rustfs:<新版本>`，`docker rm -f rustfs` 后用新 tag 重新 `docker run`；数据目录不变 |
| 容器反复重启 | `docker logs rustfs`；多为数据/日志目录权限问题，见第 2 节 `--user` |
| 端口被占用 | `ss -ltnp \| grep 9100`，改 `-p` 左侧的宿主机端口 |
| 其他机器连不上 | 检查防火墙（`ufw status`）是否放行 9100/9101 |
| `SignatureDoesNotMatch` | 检查 Access/Secret Key 与 region 是否与客户端一致 |

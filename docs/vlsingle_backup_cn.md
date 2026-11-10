# 在 VLSingle 上部署定时备份

本文说明如何在 [VictoriaMetrics Operator](https://docs.victoriametrics.com/operator/) 的 `VLSingle` CRD 上，以 sidecar 容器的方式运行 [victoria_logs_s3_backup](victoria_logs_s3_backup_cn.md)，每天定时把前一天的日志分区备份到 S3。

> 本文中的 S3 endpoint、region、bucket、环境名、Secret 名等都是示例值，请替换成你自己的配置。

## 原理

`VLSingle` 的 `spec.containers` 字段可以往 VictoriaLogs 的 Pod 里追加额外的容器。把备份工具作为 sidecar 加进去之后：

* sidecar 和 VictoriaLogs 在同一个 Pod 里，共享网络，可以通过 `http://127.0.0.1:9428` 访问 VictoriaLogs；
* sidecar 挂载 Operator 为 VictoriaLogs 创建的数据卷 `data`，可以直接读取快照文件；
* sidecar 以定时模式（`-backup.yesterday.at`）常驻运行，每天在指定的 UTC 时刻备份前一个 UTC 日的分区。

```
┌──────────────── Pod: vlsingle-<name> ────────────────┐
│                                                      │
│  VictoriaLogs ◄── 127.0.0.1:9428 ── s3-adapter       │──► S3: <s3.prefix>/<YYYYMMDD>.tar
│       │                               │              │
│       └──────── volume "data" ────────┘              │
│            /victoria-logs-data                       │
└──────────────────────────────────────────────────────┘
```

## 前提条件

* 集群里已经安装 VictoriaMetrics Operator，可以创建 `VLSingle` 资源。
* 集群能拉取镜像 `ahfuzhang/victoria-logs-s3-adapter:v0.3.0`。
* 已有一个 S3（或兼容 S3 的对象存储，例如阿里云 OSS、MinIO）bucket，以及一组访问凭证。建议为备份单独创建一个子账号，只授予下面这些权限：

  | 用途 | AWS S3 | 阿里云 OSS |
  |---|---|---|
  | 启动时检查 bucket 是否可访问 | `s3:ListBucket` | `oss:ListObjects` |
  | 分片上传备份文件 | `s3:PutObject` | `oss:PutObject` |
  | 上传失败时清理分片 | `s3:AbortMultipartUpload` | `oss:AbortMultipartUpload` |
  | 上传完成后校验对象大小 | `s3:GetObject` | `oss:GetObject` |

* `VLSingle` 的 `retentionPeriod` 至少为 `2d`，保证备份时前一天的分区还在本地，并给重试和排错留出余量。
* **数据卷建议使用 PVC。** `VLSingle` 没有配置 `spec.storage` 时，数据卷 `data` 是 `emptyDir`，Pod 重建后本地数据会丢失；而修改 `VLSingle`（包括加上本文的 sidecar）会让 Operator 重建 Pod。生产环境建议配置 `spec.storage`，例如：

  ```yaml
  spec:
    storage:
      accessModes:
      - ReadWriteOnce
      resources:
        requests:
          storage: 100Gi
  ```

## 第 1 步：创建 S3 凭证 Secret

在 `VLSingle` 所在的 namespace 中创建 Secret，保存 access key。不要把凭证直接写在 `VLSingle` 的 YAML 里。

```bash
kubectl -n logging create secret generic vlogs-backup-s3-secret \
  --from-literal=access_key_id='<YOUR_ACCESS_KEY_ID>' \
  --from-literal=secret_access_key='<YOUR_SECRET_ACCESS_KEY>'
```

## 第 2 步：在 VLSingle 中加入备份 sidecar

在已有的 `VLSingle` 的 `spec` 下加入 `containers`。下面是一个完整的示例，`# ---- 备份 sidecar ----` 以下是新增的部分，其余字段保持你原来的配置即可：

```yaml
apiVersion: operator.victoriametrics.com/v1
kind: VLSingle
metadata:
  name: example
  namespace: logging
spec:
  replicaCount: 1
  retentionPeriod: 3d
  image:
    repository: victoriametrics/victoria-logs
    pullPolicy: IfNotPresent
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      cpu: "1"
      memory: 4Gi
  extraArgs:
    loggerFormat: json

  # ---- 备份 sidecar ----
  containers:
  - name: s3-adapter
    image: ahfuzhang/victoria-logs-s3-adapter:v0.3.0
    imagePullPolicy: IfNotPresent
    command:
    - /usr/local/bin/victoria_logs_s3_backup
    args:
    - -s3.endpoint=https://s3.example.com
    - -s3.region=us-east-1
    - -s3.bucket=my-bucket
    - -s3.prefix=victoria_logs_backup/{env}/${YYYY}/${MM}/${DD}
    - -victoria.logs.addr=http://127.0.0.1:9428
    - -victoria.logs.storage.dir=/victoria-logs-data
    - -backup.yesterday.at=0015
    - -max.upload.bytes.per.second=1mb
    env:
    - name: env               # 被 -s3.prefix 中的 {env} 引用
      value: prod
    - name: access_key_id
      valueFrom:
        secretKeyRef:
          name: vlogs-backup-s3-secret
          key: access_key_id
    - name: secret_access_key
      valueFrom:
        secretKeyRef:
          name: vlogs-backup-s3-secret
          key: secret_access_key
    resources:
      requests:
        cpu: 500m
        memory: 512Mi
      limits:
        cpu: 500m
        memory: 512Mi
    volumeMounts:
    - name: data              # Operator 为 VictoriaLogs 创建的数据卷
      mountPath: /victoria-logs-data
```

> 如果 YAML 是从 `kubectl get vlsingle -o yaml` 导出后修改的，`kubectl apply` 前请删掉 `status`、`metadata.finalizers`、`metadata.resourceVersion` 等由集群维护的字段。

### 配置说明

**S3 参数**

| 参数 | 说明 |
|---|---|
| `-s3.endpoint` | 对象存储的 endpoint。AWS S3 形如 `https://s3.<region>.amazonaws.com`，阿里云 OSS 形如 `https://oss-<region>.aliyuncs.com`。 |
| `-s3.region` | bucket 所在的 region。 |
| `-s3.bucket` | bucket 名。 |
| `-s3.prefix` | 对象 key 的前缀模板。上面的示例会把 2026-11-04 的分区上传到 `victoria_logs_backup/prod/2026/11/04/20261104.tar`。写法详见 [-s3.prefix 前缀模板](s3_prefix_cn.md)。 |

在 Kubernetes 的 `args` 里写 `-s3.prefix` 时要注意：

* **不要加引号。** 容器的 `args` 不经过 shell，`-s3.prefix='...'` 中的引号会被当成前缀的一部分。
* `${YYYY}` 这类时间占位符可以直接写。Kubernetes 只展开 `$(VAR)` 形式的变量，不会处理 `${...}`。
* `{env}` 引用的是容器的环境变量 `env`，必须在 `env` 中定义且不为空，否则启动报错。用它区分不同集群/环境的备份，避免互相覆盖。

**VictoriaLogs 连接与数据目录**

| 参数 | 说明 |
|---|---|
| `-victoria.logs.addr` | 同一个 Pod 内访问 VictoriaLogs，用 `http://127.0.0.1:<端口>`。默认端口是 `9428`；如果改了 `VLSingle` 的 `spec.port`，这里要一起改。 |
| `-victoria.logs.storage.dir` | 必须等于 sidecar 中 `data` 卷的 `mountPath`，并且这个 `mountPath` 要和 VictoriaLogs 主容器的 `-storageDataPath` 一致。Operator 默认使用 `/victoria-logs-data`。 |

可以用下面的命令确认 VictoriaLogs 主容器实际使用的数据目录（Deployment 名为 `vlsingle-<VLSingle 名>`）：

```bash
kubectl -n logging get deploy vlsingle-example -o yaml | grep -E 'storageDataPath|mountPath'
```

如果 VictoriaLogs 设置了 `-partitionManageAuthKey`（例如 `extraArgs.partitionManageAuthKey`），还需要把同一个 key 传给 sidecar。建议也放在 Secret 里，再通过 Kubernetes 的 `$(VAR)` 展开传入：

```yaml
    args:
    # ...
    - -victoria.logs.auth.key=$(VL_PARTITION_AUTH_KEY)
    env:
    # ...
    - name: VL_PARTITION_AUTH_KEY
      valueFrom:
        secretKeyRef:
          name: vlogs-partition-auth-secret
          key: auth_key
```

**定时与限速**

| 参数 | 说明 |
|---|---|
| `-backup.yesterday.at=0015` | 每天 00:15（**UTC**，即北京时间 08:15）备份前一个 UTC 日的分区。不要设成 `0000`，留几分钟让前一天迟到的日志写完。备份失败会再重试 2 次（间隔 1 分钟、2 分钟），3 次都失败则等第二天。 |
| `-max.upload.bytes.per.second=1mb` | 上传限速，用来避免备份占满网络带宽、影响日志写入。`1mb` 即 1 MiB/s，约 3.5 GiB/小时、84 GiB/天。 |

限速要按单日分区的大小来定：分区大小除以限速得到的上传时间应该远小于 24 小时。可以这样查看某一天分区的大小：

```bash
kubectl -n logging exec deploy/vlsingle-example -c s3-adapter -- \
  du -sh /victoria-logs-data/partitions/20261109
```

**资源**

备份是流式的 tar + 分片上传（单片 64 MiB，单并发），不在本地落盘，`512Mi` 内存足够。sidecar 的 `resources` 会计入 Pod 的总资源，调度前确认节点有余量。

## 第 3 步：应用并检查

```bash
kubectl apply -f vlsingle.yaml
```

Operator 会更新 Deployment 并重建 Pod。确认 Pod 中有两个容器，且都在运行：

```bash
kubectl -n logging get pods | grep vlsingle-example
# vlsingle-example-xxxxxxxxxx-xxxxx   2/2   Running   0   1m
```

查看 sidecar 日志：

```bash
kubectl -n logging logs deploy/vlsingle-example -c s3-adapter -f
```

正常启动时依次输出（JSON 格式）：

* `S3 bucket is accessible`：S3 配置和凭证可用；
* `VictoriaLogs is reachable`：能访问 VictoriaLogs；
* `running in scheduled mode`，`at_utc` 为 `00:15`；
* `next backup scheduled`：下一次备份的时间和要备份的日期。

到达定时时刻后会输出 `starting scheduled backup`，上传过程中每 30 秒输出一次进度，完成后输出 `backup completed`，其中包含对象 key、tar 大小、耗时、平均速度。

> **注意：** sidecar 和 VictoriaLogs 在同一个 Pod 里。sidecar 启动失败、反复重启时，Pod 会变成 NotReady，并从 `vlsingle-example` Service 的 endpoints 中摘除，经由 Service 的日志写入和查询都会受影响。所以应用后一定要检查 sidecar 是否正常启动。
>
> sidecar 启动时如果 VictoriaLogs 还没就绪，sidecar 会退出并由 kubelet 自动重启，`RESTARTS` 出现 1～2 次属于正常现象。

## 第 4 步（可选）：立即做一次备份验证

不想等到第二天，可以在 sidecar 里手工执行一次性备份（`-backup.date`），验证整个链路。容器里已有 `access_key_id`、`secret_access_key`、`env` 这些环境变量，所以只需要带上命令行参数。这条命令在本地 shell 中执行，`-s3.prefix` 要用单引号括起来：

```bash
# 把 20261109 换成昨天的 UTC 日期
kubectl -n logging exec deploy/vlsingle-example -c s3-adapter -- \
  /usr/local/bin/victoria_logs_s3_backup \
  -s3.endpoint=https://s3.example.com \
  -s3.region=us-east-1 \
  -s3.bucket=my-bucket \
  -s3.prefix='victoria_logs_backup/{env}/${YYYY}/${MM}/${DD}' \
  -victoria.logs.addr=http://127.0.0.1:9428 \
  -victoria.logs.storage.dir=/victoria-logs-data \
  -max.upload.bytes.per.second=1mb \
  -backup.date=20261109
```

成功后退出码为 `0`，并输出 `backup completed`。这时可以在对象存储控制台或用其他 S3 客户端看到 `victoria_logs_backup/prod/2026/11/09/20261109.tar`。与定时备份写的是同一个 key，会被覆盖，没有副作用。

## 常见问题

| 日志 / 现象 | 原因与处理 |
|---|---|
| `cannot access S3 bucket "..."` 或 `S3 bucket "..." does not exist` | `-s3.endpoint`、`-s3.region`、`-s3.bucket` 配错，或凭证没有 List 权限。检查 Secret 的内容和子账号授权。 |
| `environment variable "env" referenced by template ... is not set or empty` | `-s3.prefix` 引用了 `{env}`，但容器的 `env` 里没有定义或值为空。 |
| `invalid -s3.prefix: ...` | 前缀模板写错，例如加了引号、把分钟写成了 `MM`。见 [-s3.prefix 前缀模板](s3_prefix_cn.md)。 |
| `VictoriaLogs at -victoria.logs.addr=... is not reachable` | VictoriaLogs 还没启动完成（会自动重启重试），或者 `spec.port` 改过而 `-victoria.logs.addr` 没跟着改。 |
| `cannot access -victoria.logs.storage.dir=...` | sidecar 没有挂载 `data` 卷，或 `mountPath` 与 `-victoria.logs.storage.dir` 不一致。 |
| 警告 `storage directory has no partitions subdirectory` | 挂载的目录不是 VictoriaLogs 的 `-storageDataPath`，或者 VictoriaLogs 还没有写入过数据。 |
| `partition directory not found`，随后 `scheduled backup failed; not retrying since the partition does not exist` | 前一天没有日志，或数据目录配错。没有数据的日期不会重试。 |
| `scheduled backup failed; giving up until the next scheduled run` | 3 次尝试都失败。查看前面的错误日志，修复后可以按第 4 步手工补备份这一天。 |
| Pod 重建后本地日志没了 | 没有配置 `spec.storage`，数据卷是 `emptyDir`。见“前提条件”。 |

## 从备份中查询历史日志

备份上传后，可以用 [victoria_logs_s3_restore_svr](victoria_logs_s3_restore_svr_cn.md) 按需从 S3 恢复并查询历史日志，在 Kubernetes 上的部署方法见 [在 Kubernetes 上部署恢复服务器](k8s_restore_svr_cn.md)。恢复服务器的 `-s3.endpoint`、`-s3.bucket`、`-s3.prefix` 以及 `{env}` 等环境变量必须与备份时一致。

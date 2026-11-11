# 在 Kubernetes 上部署历史日志恢复服务器

本文说明如何在 Kubernetes 上用 Deployment 部署 [victoria_logs_s3_restore_svr](victoria_logs_s3_restore_svr_cn.md)，查询已经备份到 S3 的历史日志。备份的部署方法见 [在 VLSingle 上部署定时备份](vlsingle_backup_cn.md)。

> 本文中的 S3 endpoint、region、bucket、环境名、Secret 名等都是示例值，请替换成你自己的配置。

## 原理

恢复服务器以**托管模式**（`-victoria.logs.bin`）运行：容器里同时跑着恢复服务器和一个由它管理的 VictoriaLogs 子进程，客户端只访问恢复服务器的 `:8093` 端口。

* 所有请求都转发给 VictoriaLogs 子进程，用法与直接访问 VictoriaLogs 一样，包括 Web UI（vmui）。
* 带 `start` / `end` 的 `/select/logsql/*` 查询，如果时间范围内某一天的分区本地没有，就先从 S3 下载这一天的备份并加载，再转发查询。
* 已经恢复的分区留在本地数据目录，之后再查这一天不需要重新下载。

```
client / vmui / Grafana
   │  HTTP :8093
   ▼
Service vlogs-s3-restore
   │
   ▼
Pod vlogs-s3-restore
 ├─ victoria_logs_s3_restore_svr  :8093  ◄──── S3: <s3.prefix>/<YYYYMMDD>.tar
 │     │                                       （本地缺少的日期先下载）
 │     ▼
 └─ VictoriaLogs 子进程  127.0.0.1:<随机端口>
       数据目录：/victoria-logs-data/partitions/<YYYYMMDD>
```

这个实例**只用来查询历史日志**，不接收日志写入。最近的日志（还没有备份到 S3 的日期）仍然在线上的 VictoriaLogs 中查询。

## 前提条件

* S3 中已经有 [victoria_logs_s3_backup](victoria_logs_s3_backup_cn.md) 生成的备份。
* 集群能拉取镜像 `ahfuzhang/victoria-logs-s3-adapter:v0.3.0`。镜像中已包含恢复服务器和 VictoriaLogs（`/usr/local/bin/victoria-logs-prod`）。
* 一组能读取备份的 S3 凭证。恢复服务器只需要读权限，建议使用单独的只读子账号：

  | 用途 | AWS S3 | 阿里云 OSS |
  |---|---|---|
  | 启动时检查 bucket 是否可访问；按前缀模板查找备份 | `s3:ListBucket` | `oss:ListObjects` |
  | 下载备份、读取对象大小 | `s3:GetObject` | `oss:GetObject` |

## 第 1 步：创建 S3 凭证 Secret

```bash
kubectl -n logging create secret generic vlogs-restore-s3-secret \
  --from-literal=access_key_id='<YOUR_ACCESS_KEY_ID>' \
  --from-literal=secret_access_key='<YOUR_SECRET_ACCESS_KEY>'
```

也可以直接复用备份用的 Secret（例如 `vlogs-backup-s3-secret`），把下面 Deployment 中的 Secret 名改掉即可。

> 不要把 Secret 和下面的 ConfigMap、Deployment 写在同一个 YAML 文件里。否则一旦文件里的 Secret 是空值或占位符，`kubectl apply` 时就会覆盖掉集群里真实的凭证。

## 第 2 步：创建 S3 配置 ConfigMap

把 S3 配置放在 ConfigMap 里，Deployment 通过环境变量引用。这些值**必须与备份时完全一致**：

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: vlogs-restore-s3-config
  namespace: logging
data:
  S3_ENDPOINT: "https://s3.example.com"
  S3_REGION: "us-east-1"
  S3_BUCKET: "my-bucket"
  # 必须与 victoria_logs_s3_backup 的 -s3.prefix 完全相同
  S3_PREFIX: "victoria_logs_backup/{env}/${YYYY}/${MM}/${DD}"
  # S3_PREFIX 中 {env} 的值，必须与备份时相同
  env: "prod"
```

`S3_PREFIX` 是前缀模板，`{env}` 和 `${YYYY}` 等占位符由恢复服务器自己展开，写法详见 [-s3.prefix 前缀模板](s3_prefix_cn.md)。模板里有时间占位符时，恢复服务器会先列出第一个时间占位符之前的固定前缀（上例为 `victoria_logs_backup/prod/`）下的对象，再找出匹配的备份。

## 第 3 步：创建 Deployment 和 Service

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vlogs-s3-restore
  namespace: logging
  labels:
    app: vlogs-s3-restore
spec:
  replicas: 1
  selector:
    matchLabels:
      app: vlogs-s3-restore
  template:
    metadata:
      labels:
        app: vlogs-s3-restore
    spec:
      containers:
      - name: restore-svr
        image: ahfuzhang/victoria-logs-s3-adapter:v0.3.0
        imagePullPolicy: IfNotPresent
        command:
        - /usr/local/bin/victoria_logs_s3_restore_svr
        args:
        # $(VAR) 由 kubelet 用下面 env 中的值展开
        - -s3.endpoint=$(S3_ENDPOINT)
        - -s3.region=$(S3_REGION)
        - -s3.bucket=$(S3_BUCKET)
        - -s3.prefix=$(S3_PREFIX)
        - -victoria.logs.bin=/usr/local/bin/victoria-logs-prod
        - -victoria.logs.storage.dir=/victoria-logs-data
        - -addr=0.0.0.0:8093
        - -max.download.bytes.per.second=100mb
        - -restore.max.days=31
        - -loglevel=info
        env:
        - name: S3_ENDPOINT
          valueFrom:
            configMapKeyRef:
              name: vlogs-restore-s3-config
              key: S3_ENDPOINT
        - name: S3_REGION
          valueFrom:
            configMapKeyRef:
              name: vlogs-restore-s3-config
              key: S3_REGION
        - name: S3_BUCKET
          valueFrom:
            configMapKeyRef:
              name: vlogs-restore-s3-config
              key: S3_BUCKET
        - name: S3_PREFIX
          valueFrom:
            configMapKeyRef:
              name: vlogs-restore-s3-config
              key: S3_PREFIX
        - name: env               # 被 S3_PREFIX 中的 {env} 引用
          valueFrom:
            configMapKeyRef:
              name: vlogs-restore-s3-config
              key: env
        - name: access_key_id
          valueFrom:
            secretKeyRef:
              name: vlogs-restore-s3-secret
              key: access_key_id
        - name: secret_access_key
          valueFrom:
            secretKeyRef:
              name: vlogs-restore-s3-secret
              key: secret_access_key
        ports:
        - name: http
          containerPort: 8093
          protocol: TCP
        resources:
          requests:
            cpu: "1"
            memory: 2Gi
          limits:
            cpu: "1"
            memory: 2Gi
        volumeMounts:
        - name: data
          mountPath: /victoria-logs-data
      # 收到 SIGTERM 后，恢复服务器给处理中的请求 30 秒，然后停止 VictoriaLogs
      terminationGracePeriodSeconds: 60
      volumes:
      - name: data
        emptyDir:
          sizeLimit: 200Gi
---
apiVersion: v1
kind: Service
metadata:
  name: vlogs-s3-restore
  namespace: logging
spec:
  selector:
    app: vlogs-s3-restore
  ports:
  - name: http
    protocol: TCP
    port: 8093
    targetPort: http
```

需要从集群外访问时，可以把 Service 改成 `type: NodePort`（`nodePort` 可以不写，由集群分配），或者配置 Ingress。恢复服务器本身没有鉴权，暴露到集群外之前请在 Ingress / 网关上加访问控制。

### 配置说明

**参数与环境变量**

* kubelet 会把 `args` 中的 `$(S3_ENDPOINT)` 等替换成同名环境变量的值，替换后的内容不会再展开，所以 `S3_PREFIX` 中的 `${YYYY}` 会原样传给恢复服务器。如果引用的变量没有在 `env` 中定义（例如名字拼错），kubelet 不做替换，参数里会留下字面的 `$(S3_ENDPOINT)`。可以对照启动日志 `S3 bucket is accessible` 中的 `endpoint`、`bucket`、`prefix_template` 检查实际生效的值。
* `args` 不经过 shell，参数值不要加引号。
* `env` 这个环境变量被 `S3_PREFIX` 中的 `{env}` 引用，必须存在且不为空。

**运行参数**

| 参数 | 说明 |
|---|---|
| `-victoria.logs.bin` | 镜像中 VictoriaLogs 的路径。设置后进入托管模式，子进程监听 `127.0.0.1` 上的随机端口，保留期为 `100y`，恢复的分区不会因过期被删除。 |
| `-victoria.logs.storage.dir` | VictoriaLogs 子进程的数据目录，恢复的分区放在 `<dir>/partitions/<YYYYMMDD>`。要与 `volumeMounts` 的 `mountPath` 一致。 |
| `-addr` | 恢复服务器的监听地址，要与 `containerPort` 一致。 |
| `-max.download.bytes.per.second` | 每个备份的下载限速，默认 `100mb`。最多同时下载 2 个备份，所以总带宽最多为该值的 2 倍。 |
| `-restore.max.days` | 单个请求最多恢复多少天，默认 `31`。时间范围更长时只恢复最近的这么多天。 |
| `-auto.remove.days` | 可选。自动删除下载已满这么多天的分区，例如 `-auto.remove.days=3`。默认 `0`，不删除。详见 [自动删除恢复的分区](victoria_logs_s3_restore_svr_cn.md#自动删除恢复的分区)。 |

如果要把服务挂在某个路径下（例如通过 Ingress 暴露在 `/vlogs-history/`），加上 `-http.prefix=/vlogs-history/`，之后所有的访问路径都要带上这个前缀。

**数据卷**

默认情况下，恢复服务器**不会自动删除**已经恢复的分区；设置 `-auto.remove.days` 后，下载已满这么多天的分区会被自动删除。本地数据目录只是 S3 备份的缓存，丢了可以按需重新下载，所以推荐用 `emptyDir`：

* Pod 重建后缓存清空，之后的查询会重新从 S3 下载；
* `sizeLimit` 限制缓存最多占用多少节点磁盘。超出后 kubelet 会驱逐 Pod，Deployment 重建出的新 Pod 缓存是空的，正在进行的查询会中断。`sizeLimit` 应大于“单日分区大小 × 常查询的天数”。

如果希望缓存在 Pod 重建后仍然保留，可以把 `data` 换成 PVC，但需要自己关注磁盘空间。不要不挂卷直接把数据目录写在容器的可写层里（例如 `/tmp/`）：既不受 `sizeLimit` 限制，又会占用节点上容器运行时的磁盘。

**资源**

单日分区越大、查询越复杂，VictoriaLogs 子进程需要的内存越多。`2Gi` 是一个起点，按实际查询量调整。

**副本数与探针**

* 建议 `replicas: 1`。多个副本各自维护自己的缓存，同一天的数据会被每个副本各下载一次。
* 示例没有配置探针。恢复的分区热加载失败时，恢复服务器会重启 VictoriaLogs 子进程，期间的新请求会排队等待（最长 5 分钟）。如果配置了以 `/health` 为目标的 readinessProbe，这段时间 Pod 会被标为 NotReady 并从 Service 中摘除，请求反而直接失败。

## 第 4 步：应用并检查

```bash
kubectl apply -f vlogs-restore-config.yaml   # 第 2 步的 ConfigMap
kubectl apply -f vlogs-restore.yaml          # 第 3 步的 Deployment 和 Service
kubectl -n logging get pods -l app=vlogs-s3-restore
kubectl -n logging logs deploy/vlogs-s3-restore -f
```

正常启动时依次输出（JSON 格式）：

* `S3 bucket is accessible`：S3 配置和凭证可用，日志中有展开后的固定前缀 `static_prefix`，可以核对是否与备份的路径一致；
* `started VictoriaLogs process`、`VictoriaLogs is ready`：子进程已启动；
* `VictoriaLogs runs as a child process`；
* `restore server started`：开始监听 `:8093`。

修改 ConfigMap 后，环境变量不会自动更新，需要重启 Deployment：

```bash
kubectl -n logging rollout restart deploy/vlogs-s3-restore
```

## 查询历史日志

### 访问地址

* 集群内：`http://vlogs-s3-restore.logging.svc:8093`
* 本地调试：先做端口转发，然后访问 `http://127.0.0.1:8093`

  ```bash
  kubectl -n logging port-forward svc/vlogs-s3-restore 8093:8093
  ```

下面的例子都以 `http://127.0.0.1:8093` 为例。

### 用 curl 查询

查询 2026-11-04 全天（UTC）包含 `error` 的日志：

```bash
curl --max-time 600 http://127.0.0.1:8093/select/logsql/query \
  -d 'query=error' \
  -d 'start=2026-11-04T00:00:00Z' \
  -d 'end=2026-11-05T00:00:00Z' \
  -d 'limit=100'
```

如果本地还没有这一天的分区，请求会等到从 S3 恢复完成后再返回，所以要把客户端超时设得足够长。

按小时统计 2026-11-03 到 2026-11-04 两天的日志条数：

```bash
curl --max-time 600 http://127.0.0.1:8093/select/logsql/hits \
  -d 'query=*' \
  -d 'start=2026-11-03T00:00:00Z' \
  -d 'end=2026-11-05T00:00:00Z' \
  -d 'step=1h'
```

LogsQL 的语法和其他查询接口见 VictoriaLogs 官方文档：[LogsQL](https://docs.victoriametrics.com/victorialogs/logsql/)、[Querying](https://docs.victoriametrics.com/victorialogs/querying/)。

### 用 Web UI（vmui）查询

浏览器打开 `http://127.0.0.1:8093/select/vmui/`，在时间选择器中选择要查询的历史时间段即可。vmui 发出的查询带有时间范围，同样会触发恢复。

### 在 Grafana 中查询

安装 VictoriaLogs 数据源插件，把数据源的 URL 设为 `http://vlogs-s3-restore.logging.svc:8093`。Grafana 面板的时间范围会作为 `start` / `end` 传给查询，选到历史日期时自动触发恢复。

### 查询时的注意事项

* **时间请带时区。** 不带时区的时间按容器的本地时区解释（镜像默认为 UTC）。建议统一写成 `2026-11-04T00:00:00Z` 这种 UTC 格式。分区按 UTC 日划分。
* **第一次查询某一天会比较慢**，要等下载和加载完成，耗时取决于分区大小和 `-max.download.bytes.per.second`。如果客户端（例如 Grafana）先超时了，恢复仍会在后台继续，稍后重试即可；也可以适当调大客户端的超时时间。
* **单个请求最多恢复 `-restore.max.days` 天**（默认 31 天），超出时只恢复最近的这么多天，日志中会有警告。
* **S3 中没有备份的日期**（例如还没到备份时间的昨天、今天）不会报错，查询结果中只是没有这些天的数据。这部分日志请到线上的 VictoriaLogs 中查询。
* **恢复失败时**请求仍然会转发，返回的结果缺少失败的那一天。1 分钟内不会重试同一天，失败原因见日志。

查看当前已经恢复到本地的日期：

```bash
kubectl -n logging exec deploy/vlogs-s3-restore -- ls /victoria-logs-data/partitions
```

## 日志与常见问题

查询触发恢复时，日志中依次出现 `restore started`、`restore in progress`（每 30 秒一次）、`restore finished`。

| 日志 / 现象 | 原因与处理 |
|---|---|
| `cannot access S3 bucket "..."` 或 `S3 bucket "..." does not exist` | ConfigMap 中的 endpoint、region、bucket 配错，或凭证没有 List 权限。 |
| `invalid -s3.prefix: environment variable "env" referenced by template ... is not set or empty` | 容器的 `env` 中没有定义 `env`，或值为空。 |
| `invalid -s3.prefix: ...`（其他原因） | 前缀模板写错，例如把分钟写成了 `MM`。见 [-s3.prefix 前缀模板](s3_prefix_cn.md)。 |
| `no backup found for the requested day; forwarding the request as is` | S3 中找不到这一天的备份。检查 `S3_PREFIX`、`env` 是否与备份时完全一致（对照启动日志中的 `static_prefix`、`prefix_template`），以及这一天是否已经备份。 |
| Pod 处于 `CreateContainerConfigError` | 引用的 ConfigMap / Secret 或其中的 key 不存在。用 `kubectl -n logging describe pod` 查看具体是哪一个。 |
| `cannot restore partition` | 下载或解包失败，例如网络中断、磁盘空间不足、凭证没有 `GetObject` 权限。 |
| `the requested time range exceeds -restore.max.days; ...` | 查询的时间范围超过 `-restore.max.days`，只恢复了最近的那些天。缩小时间范围或调大该参数。 |
| Pod 被驱逐（`Evicted`） | 恢复的数据超过了 `emptyDir` 的 `sizeLimit`。调大 `sizeLimit`，或者减少一次查询的天数。 |
| 查询超时，但过一会儿再查就有结果 | 第一次查询在等待恢复。调大客户端超时，或者先用较小的时间范围预热。 |

参数、恢复流程和缓存的完整说明见 [victoria_logs_s3_restore_svr](victoria_logs_s3_restore_svr_cn.md)。

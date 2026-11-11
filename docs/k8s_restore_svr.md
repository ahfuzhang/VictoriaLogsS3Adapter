# Deploying the Restore Server on Kubernetes

This document shows how to deploy [victoria_logs_s3_restore_svr](victoria_logs_s3_restore_svr.md) on Kubernetes as a Deployment, so you can query historical logs that were backed up to S3. For deploying the backups themselves, see [Scheduled backup on VLSingle](vlsingle_backup.md).

> The S3 endpoint, region, bucket, environment name, Secret name, etc. in this document are examples. Replace them with your own configuration.

## How it works

The restore server runs in **managed mode** (`-victoria.logs.bin`): the container runs the restore server plus a VictoriaLogs child process managed by it, and clients only talk to the restore server on port `:8093`.

* Every request is forwarded to the VictoriaLogs child process, so you use it exactly like VictoriaLogs, including its Web UI (vmui).
* For `/select/logsql/*` queries with `start` / `end`, if the partition for a day in the time range isn't available locally, that day's backup is first downloaded from S3 and loaded, then the query is forwarded.
* Restored partitions stay in the local data directory, so later queries for the same day don't download it again.

```
client / vmui / Grafana
   │  HTTP :8093
   ▼
Service vlogs-s3-restore
   │
   ▼
Pod vlogs-s3-restore
 ├─ victoria_logs_s3_restore_svr  :8093  ◄──── S3: <s3.prefix>/<YYYYMMDD>.tar
 │     │                                       (missing days are downloaded first)
 │     ▼
 └─ VictoriaLogs child process  127.0.0.1:<random port>
       data directory: /victoria-logs-data/partitions/<YYYYMMDD>
```

This instance is **for querying historical logs only** and doesn't ingest logs. Recent logs (days not yet backed up to S3) are still queried on the live VictoriaLogs.

## Prerequisites

* S3 already holds backups produced by [victoria_logs_s3_backup](victoria_logs_s3_backup.md).
* The cluster can pull the image `ahfuzhang/victoria-logs-s3-adapter:v0.3.0`. The image contains both the restore server and VictoriaLogs (`/usr/local/bin/victoria-logs-prod`).
* A set of S3 credentials that can read the backups. The restore server only needs read access, so a dedicated read-only sub-account is recommended:

  | Purpose | AWS S3 | Aliyun OSS |
  |---|---|---|
  | Check at startup that the bucket is accessible; find backups by the prefix template | `s3:ListBucket` | `oss:ListObjects` |
  | Download backups and read object sizes | `s3:GetObject` | `oss:GetObject` |

## Step 1: Create a Secret with the S3 credentials

```bash
kubectl -n logging create secret generic vlogs-restore-s3-secret \
  --from-literal=access_key_id='<YOUR_ACCESS_KEY_ID>' \
  --from-literal=secret_access_key='<YOUR_SECRET_ACCESS_KEY>'
```

You can also reuse the Secret used for backups (e.g. `vlogs-backup-s3-secret`); just change the Secret name in the Deployment below.

> Don't put the Secret in the same YAML file as the ConfigMap and Deployment below. If the Secret in that file has empty or placeholder values, `kubectl apply` overwrites the real credentials in the cluster.

## Step 2: Create a ConfigMap with the S3 configuration

Keep the S3 configuration in a ConfigMap and reference it from the Deployment through environment variables. These values **must be exactly the same as the backup's**:

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
  # Must be exactly the same as -s3.prefix of victoria_logs_s3_backup
  S3_PREFIX: "victoria_logs_backup/{env}/${YYYY}/${MM}/${DD}"
  # Value of {env} in S3_PREFIX; must be the same as for the backup
  env: "prod"
```

`S3_PREFIX` is a prefix template. Placeholders such as `{env}` and `${YYYY}` are expanded by the restore server itself; see [-s3.prefix prefix template](s3_prefix.md). When the template has time placeholders, the restore server lists objects under the fixed prefix before the first time placeholder (`victoria_logs_backup/prod/` in the example above), then picks the matching backup.

## Step 3: Create the Deployment and Service

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
        # $(VAR) is expanded by kubelet from the env below
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
        - name: env               # referenced by {env} in S3_PREFIX
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
      # On SIGTERM the restore server gives in-flight requests 30s, then stops VictoriaLogs
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

To access it from outside the cluster, change the Service to `type: NodePort` (`nodePort` can be omitted and allocated by the cluster), or set up an Ingress. The restore server has no authentication of its own, so add access control on the Ingress / gateway before exposing it outside the cluster.

### Configuration notes

**Flags and environment variables**

* kubelet replaces `$(S3_ENDPOINT)` and the like in `args` with the value of the environment variable of the same name. The replaced content isn't expanded again, so `${YYYY}` in `S3_PREFIX` reaches the restore server unchanged. If a referenced variable isn't defined in `env` (e.g. a typo in its name), kubelet leaves it as is and the flag keeps the literal `$(S3_ENDPOINT)`. Check the effective values in the `endpoint`, `bucket` and `prefix_template` fields of the `S3 bucket is accessible` startup log.
* `args` don't go through a shell; don't put quotes around flag values.
* The `env` environment variable is referenced by `{env}` in `S3_PREFIX`; it must exist and not be empty.

**Runtime flags**

| Flag | Description |
|---|---|
| `-victoria.logs.bin` | Path of VictoriaLogs in the image. Setting it selects managed mode: the child process listens on a random port on `127.0.0.1` with a retention of `100y`, so restored partitions are never dropped for being too old. |
| `-victoria.logs.storage.dir` | Data directory of the VictoriaLogs child process; restored partitions go to `<dir>/partitions/<YYYYMMDD>`. Must match the `mountPath` in `volumeMounts`. |
| `-addr` | Listen address of the restore server; must match `containerPort`. |
| `-max.download.bytes.per.second` | Download rate limit per backup, `100mb` by default. At most 2 backups are downloaded at once, so total bandwidth is at most twice this value. |
| `-restore.max.days` | Maximum number of days restored for a single request, `31` by default. For longer time ranges only the most recent days are restored. |
| `-auto.remove.days` | Optional. Automatically remove partitions downloaded at least this many days ago, e.g. `-auto.remove.days=3`. `0` by default: nothing is removed. See [Automatic removal of restored partitions](victoria_logs_s3_restore_svr.md#automatic-removal-of-restored-partitions). |

To serve under a path (e.g. exposed by an Ingress at `/vlogs-history/`), add `-http.prefix=/vlogs-history/`; every request path must then include this prefix.

**Data volume**

By default the restore server **never deletes** restored partitions on its own; with `-auto.remove.days`, partitions downloaded at least that many days ago are removed automatically. The local data directory is just a cache of the S3 backups and can always be downloaded again on demand, so `emptyDir` is recommended:

* when the Pod is recreated, the cache is cleared and later queries download from S3 again;
* `sizeLimit` caps how much node disk the cache may use. When it is exceeded, kubelet evicts the Pod; the Pod the Deployment creates in its place starts with an empty cache, and in-flight queries are interrupted. Make `sizeLimit` larger than "size of one day's partition × number of days usually queried".

If you want the cache to survive Pod recreation, replace `data` with a PVC, but then you have to watch the disk space yourself. Don't skip the volume and put the data directory in the container's writable layer (e.g. `/tmp/`): it isn't bounded by `sizeLimit` and it fills the container runtime's disk on the node.

**Resources**

The bigger one day's partition and the heavier the queries, the more memory the VictoriaLogs child process needs. `2Gi` is a starting point; adjust it to your query load.

**Replicas and probes**

* `replicas: 1` is recommended. Each replica keeps its own cache, so every replica would download the same day separately.
* The example sets no probes. If hot-loading a restored partition fails, the restore server restarts the VictoriaLogs child process, and new requests queue up meanwhile (for up to 5 minutes). With a readinessProbe on `/health`, the Pod would be marked NotReady and removed from the Service during that time, making requests fail instead.

## Step 4: Apply and check

```bash
kubectl apply -f vlogs-restore-config.yaml   # ConfigMap from Step 2
kubectl apply -f vlogs-restore.yaml          # Deployment and Service from Step 3
kubectl -n logging get pods -l app=vlogs-s3-restore
kubectl -n logging logs deploy/vlogs-s3-restore -f
```

On a normal start it logs, in order (JSON format):

* `S3 bucket is accessible`: the S3 configuration and credentials work. Its `static_prefix` field is the expanded fixed prefix, which you can compare with the backup path;
* `started VictoriaLogs process`, `VictoriaLogs is ready`: the child process is up;
* `VictoriaLogs runs as a child process`;
* `restore server started`: it is listening on `:8093`.

Environment variables aren't updated when the ConfigMap changes; restart the Deployment afterwards:

```bash
kubectl -n logging rollout restart deploy/vlogs-s3-restore
```

## Querying historical logs

### Addresses

* Inside the cluster: `http://vlogs-s3-restore.logging.svc:8093`
* Local debugging: set up port forwarding first, then use `http://127.0.0.1:8093`

  ```bash
  kubectl -n logging port-forward svc/vlogs-s3-restore 8093:8093
  ```

The examples below use `http://127.0.0.1:8093`.

### Querying with curl

Query logs containing `error` for the whole day of 2026-11-04 (UTC):

```bash
curl --max-time 600 http://127.0.0.1:8093/select/logsql/query \
  -d 'query=error' \
  -d 'start=2026-11-04T00:00:00Z' \
  -d 'end=2026-11-05T00:00:00Z' \
  -d 'limit=100'
```

If that day's partition isn't available locally yet, the request waits until the restore from S3 finishes, so give the client a long enough timeout.

Count logs per hour over 2026-11-03 and 2026-11-04:

```bash
curl --max-time 600 http://127.0.0.1:8093/select/logsql/hits \
  -d 'query=*' \
  -d 'start=2026-11-03T00:00:00Z' \
  -d 'end=2026-11-05T00:00:00Z' \
  -d 'step=1h'
```

For LogsQL syntax and the other query endpoints, see the official VictoriaLogs docs: [LogsQL](https://docs.victoriametrics.com/victorialogs/logsql/), [Querying](https://docs.victoriametrics.com/victorialogs/querying/).

### Querying with the Web UI (vmui)

Open `http://127.0.0.1:8093/select/vmui/` in a browser and pick the historical time range in the time picker. vmui's queries carry a time range, so they trigger restores too.

### Querying from Grafana

Install the VictoriaLogs data source plugin and set the data source URL to `http://vlogs-s3-restore.logging.svc:8093`. The panel's time range is passed to the query as `start` / `end`, so picking historical dates triggers restores automatically.

### Things to keep in mind

* **Always include a time zone.** Times without a time zone are interpreted in the container's local time zone (UTC by default in this image). Prefer UTC times such as `2026-11-04T00:00:00Z`. Partitions are split by UTC day.
* **The first query for a day is slow**, as it waits for the download and load. How long depends on the partition size and `-max.download.bytes.per.second`. If the client (e.g. Grafana) times out first, the restore keeps going in the background; just retry later, or raise the client's timeout.
* **A single request restores at most `-restore.max.days` days** (31 by default). Beyond that only the most recent days are restored, and a warning is logged.
* **Days with no backup in S3** (e.g. yesterday before the backup ran, or today) don't cause an error; the result simply has no data for them. Query those logs on the live VictoriaLogs.
* **If a restore fails**, the request is still forwarded and the result lacks the failed day. The same day isn't retried for 1 minute; the logs show the reason.

To see which days have been restored locally:

```bash
kubectl -n logging exec deploy/vlogs-s3-restore -- ls /victoria-logs-data/partitions
```

## Logs and troubleshooting

When a query triggers a restore, the logs show `restore started`, then `restore in progress` (every 30 seconds), then `restore finished`.

| Log / symptom | Cause and fix |
|---|---|
| `cannot access S3 bucket "..."` or `S3 bucket "..." does not exist` | The endpoint, region or bucket in the ConfigMap is wrong, or the credentials lack the List permission. |
| `invalid -s3.prefix: environment variable "env" referenced by template ... is not set or empty` | `env` is not defined, or is empty, in the container's `env`. |
| `invalid -s3.prefix: ...` (other causes) | The prefix template is wrong, e.g. it uses `MM` for minutes. See [-s3.prefix prefix template](s3_prefix.md). |
| `no backup found for the requested day; forwarding the request as is` | No backup for that day was found in S3. Check that `S3_PREFIX` and `env` are exactly the same as for the backup (compare with `static_prefix` and `prefix_template` in the startup log), and that the day has been backed up. |
| Pod stuck in `CreateContainerConfigError` | A referenced ConfigMap / Secret, or a key in it, doesn't exist. Run `kubectl -n logging describe pod` to see which one. |
| `cannot restore partition` | Download or extraction failed, e.g. network interruption, not enough disk space, or credentials without the `GetObject` permission. |
| `the requested time range exceeds -restore.max.days; ...` | The query's time range is longer than `-restore.max.days`, so only the most recent days were restored. Narrow the time range or raise the flag. |
| Pod `Evicted` | Restored data exceeded the `emptyDir` `sizeLimit`. Raise `sizeLimit`, or query fewer days at a time. |
| A query times out, but returns results when retried later | The first query was waiting for the restore. Raise the client's timeout, or warm up with a smaller time range first. |

For the full description of flags, the restore flow and caching, see [victoria_logs_s3_restore_svr](victoria_logs_s3_restore_svr.md).

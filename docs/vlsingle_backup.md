# Scheduled Backup on VLSingle

This document shows how to run [victoria_logs_s3_backup](victoria_logs_s3_backup.md) as a sidecar container of a `VLSingle` resource managed by the [VictoriaMetrics Operator](https://docs.victoriametrics.com/operator/), so that the previous day's log partition is backed up to S3 every day.

> The S3 endpoint, region, bucket, environment name, Secret name, etc. in this document are examples. Replace them with your own configuration.

## How it works

The `spec.containers` field of `VLSingle` appends extra containers to the VictoriaLogs Pod. With the backup tool added as a sidecar:

* the sidecar is in the same Pod as VictoriaLogs and shares its network, so it reaches VictoriaLogs at `http://127.0.0.1:9428`;
* the sidecar mounts the `data` volume the Operator creates for VictoriaLogs, so it can read the snapshot files directly;
* the sidecar runs as a daemon in scheduled mode (`-backup.yesterday.at`) and, at the given UTC time each day, backs up the previous UTC day's partition.

```
┌──────────────── Pod: vlsingle-<name> ────────────────┐
│                                                      │
│  VictoriaLogs ◄── 127.0.0.1:9428 ── s3-adapter       │──► S3: <s3.prefix>/<YYYYMMDD>.tar
│       │                               │              │
│       └──────── volume "data" ────────┘              │
│            /victoria-logs-data                       │
└──────────────────────────────────────────────────────┘
```

## Prerequisites

* The VictoriaMetrics Operator is installed in the cluster and `VLSingle` resources can be created.
* The cluster can pull the image `ahfuzhang/victoria-logs-s3-adapter:v0.3.0`.
* An S3 bucket (or a bucket on an S3-compatible store such as Aliyun OSS or MinIO) and a set of credentials. It is recommended to create a dedicated sub-account for backups and grant it only these permissions:

  | Purpose | AWS S3 | Aliyun OSS |
  |---|---|---|
  | Check at startup that the bucket is accessible | `s3:ListBucket` | `oss:ListObjects` |
  | Upload the backup with multipart upload | `s3:PutObject` | `oss:PutObject` |
  | Clean up parts when an upload fails | `s3:AbortMultipartUpload` | `oss:AbortMultipartUpload` |
  | Verify the object size after upload | `s3:GetObject` | `oss:GetObject` |

* The `retentionPeriod` of `VLSingle` is at least `2d`, so that the previous day's partition is still on local disk at backup time, with room left for retries and troubleshooting.
* **A PVC is recommended for the data volume.** When `spec.storage` is not set on `VLSingle`, the `data` volume is an `emptyDir` and local data is lost when the Pod is recreated, and modifying `VLSingle` (including adding the sidecar described here) makes the Operator recreate the Pod. In production, set `spec.storage`, for example:

  ```yaml
  spec:
    storage:
      accessModes:
      - ReadWriteOnce
      resources:
        requests:
          storage: 100Gi
  ```

## Step 1: Create a Secret with the S3 credentials

Create a Secret holding the access key in the namespace of the `VLSingle`. Do not put the credentials directly in the `VLSingle` YAML.

```bash
kubectl -n logging create secret generic vlogs-backup-s3-secret \
  --from-literal=access_key_id='<YOUR_ACCESS_KEY_ID>' \
  --from-literal=secret_access_key='<YOUR_SECRET_ACCESS_KEY>'
```

## Step 2: Add the backup sidecar to VLSingle

Add `containers` under `spec` of the existing `VLSingle`. Below is a complete example. Everything after `# ---- backup sidecar ----` is new; keep the rest of your existing configuration as is:

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

  # ---- backup sidecar ----
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
    - name: env               # referenced by {env} in -s3.prefix
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
    - name: data              # the data volume the Operator creates for VictoriaLogs
      mountPath: /victoria-logs-data
```

> If you edited YAML exported with `kubectl get vlsingle -o yaml`, remove cluster-managed fields such as `status`, `metadata.finalizers` and `metadata.resourceVersion` before running `kubectl apply`.

### Configuration notes

**S3 flags**

| Flag | Description |
|---|---|
| `-s3.endpoint` | Endpoint of the object store. For AWS S3 it looks like `https://s3.<region>.amazonaws.com`; for Aliyun OSS, `https://oss-<region>.aliyuncs.com`. |
| `-s3.region` | Region of the bucket. |
| `-s3.bucket` | Bucket name. |
| `-s3.prefix` | Object key prefix template. With the example above, the partition for 2026-11-04 is uploaded to `victoria_logs_backup/prod/2026/11/04/20261104.tar`. See [-s3.prefix prefix template](s3_prefix.md). |

When writing `-s3.prefix` in Kubernetes `args`:

* **Do not add quotes.** Container `args` don't go through a shell, so the quotes in `-s3.prefix='...'` would become part of the prefix.
* Time placeholders such as `${YYYY}` can be written as is. Kubernetes only expands variables of the form `$(VAR)` and leaves `${...}` alone.
* `{env}` refers to the container's environment variable `env`. It must be defined in `env` and not empty, or startup fails. Use it to tell backups of different clusters/environments apart so they don't overwrite each other.

**VictoriaLogs address and data directory**

| Flag | Description |
|---|---|
| `-victoria.logs.addr` | VictoriaLogs is in the same Pod, so use `http://127.0.0.1:<port>`. The default port is `9428`; if you changed `spec.port` of the `VLSingle`, change this too. |
| `-victoria.logs.storage.dir` | Must equal the `mountPath` of the `data` volume in the sidecar, and that `mountPath` must match the `-storageDataPath` of the VictoriaLogs container. The Operator uses `/victoria-logs-data` by default. |

To check the data directory the VictoriaLogs container actually uses (the Deployment is named `vlsingle-<VLSingle name>`):

```bash
kubectl -n logging get deploy vlsingle-example -o yaml | grep -E 'storageDataPath|mountPath'
```

If VictoriaLogs sets `-partitionManageAuthKey` (for example through `extraArgs.partitionManageAuthKey`), pass the same key to the sidecar. Keep it in a Secret as well, and pass it in with Kubernetes' `$(VAR)` expansion:

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

**Schedule and rate limit**

| Flag | Description |
|---|---|
| `-backup.yesterday.at=0015` | Back up the previous UTC day's partition every day at 00:15 **UTC** (08:15 in UTC+8). Avoid `0000`; leave a few minutes for late logs of the previous day to arrive. A failed backup is retried twice more (after 1 and 2 minutes); if all 3 attempts fail, it waits for the next day. |
| `-max.upload.bytes.per.second=1mb` | Upload rate limit, so that the backup doesn't saturate the network and slow down log ingestion. `1mb` is 1 MiB/s, about 3.5 GiB per hour or 84 GiB per day. |

Pick the rate limit based on the size of one day's partition: the partition size divided by the rate should be well below 24 hours. To see the size of a day's partition:

```bash
kubectl -n logging exec deploy/vlsingle-example -c s3-adapter -- \
  du -sh /victoria-logs-data/partitions/20261109
```

**Resources**

The backup is a streamed tar plus multipart upload (64 MiB parts, one at a time) and writes nothing to local disk, so `512Mi` of memory is enough. The sidecar's `resources` add to the Pod's total, so make sure the node has room.

## Step 3: Apply and check

```bash
kubectl apply -f vlsingle.yaml
```

The Operator updates the Deployment and recreates the Pod. Check that the Pod has two containers, both running:

```bash
kubectl -n logging get pods | grep vlsingle-example
# vlsingle-example-xxxxxxxxxx-xxxxx   2/2   Running   0   1m
```

View the sidecar's logs:

```bash
kubectl -n logging logs deploy/vlsingle-example -c s3-adapter -f
```

On a normal start it logs, in order (JSON format):

* `S3 bucket is accessible`: the S3 configuration and credentials work;
* `VictoriaLogs is reachable`: VictoriaLogs can be reached;
* `running in scheduled mode`, with `at_utc` set to `00:15`;
* `next backup scheduled`: when the next backup runs and which date it backs up.

At the scheduled time it logs `starting scheduled backup`, logs progress every 30 seconds during the upload, and logs `backup completed` at the end, including the object key, tar size, duration and average speed.

> **Note:** the sidecar is in the same Pod as VictoriaLogs. If the sidecar fails to start or keeps restarting, the Pod becomes NotReady and is removed from the endpoints of the `vlsingle-example` Service, which affects log ingestion and queries that go through the Service. Always check that the sidecar started correctly after applying.
>
> If VictoriaLogs isn't ready yet when the sidecar starts, the sidecar exits and kubelet restarts it automatically; 1–2 `RESTARTS` are normal.

## Step 4 (optional): Run a backup now to verify

To avoid waiting until the next day, run a one-time backup (`-backup.date`) inside the sidecar to verify the whole path. The container already has the `access_key_id`, `secret_access_key` and `env` environment variables, so only the command-line flags are needed. This command runs in your local shell, so wrap `-s3.prefix` in single quotes:

```bash
# replace 20261109 with yesterday's UTC date
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

On success the exit code is `0` and `backup completed` is logged. You can then see `victoria_logs_backup/prod/2026/11/09/20261109.tar` in the object store console or with any S3 client. It is the same key the scheduled backup writes to, so it simply gets overwritten, with no side effects.

## Troubleshooting

| Log / symptom | Cause and fix |
|---|---|
| `cannot access S3 bucket "..."` or `S3 bucket "..." does not exist` | `-s3.endpoint`, `-s3.region` or `-s3.bucket` is wrong, or the credentials lack the List permission. Check the Secret and the sub-account's permissions. |
| `environment variable "env" referenced by template ... is not set or empty` | `-s3.prefix` references `{env}`, but `env` is not defined, or is empty, in the container's `env`. |
| `invalid -s3.prefix: ...` | The prefix template is wrong, e.g. it has quotes or uses `MM` for minutes. See [-s3.prefix prefix template](s3_prefix.md). |
| `VictoriaLogs at -victoria.logs.addr=... is not reachable` | VictoriaLogs hasn't finished starting (the sidecar restarts and retries automatically), or `spec.port` was changed but `-victoria.logs.addr` wasn't. |
| `cannot access -victoria.logs.storage.dir=...` | The sidecar doesn't mount the `data` volume, or the `mountPath` doesn't match `-victoria.logs.storage.dir`. |
| Warning `storage directory has no partitions subdirectory` | The mounted directory isn't VictoriaLogs' `-storageDataPath`, or VictoriaLogs hasn't written any data yet. |
| `partition directory not found`, then `scheduled backup failed; not retrying since the partition does not exist` | There were no logs on the previous day, or the data directory is misconfigured. Dates without data are not retried. |
| `scheduled backup failed; giving up until the next scheduled run` | All 3 attempts failed. Check the earlier error logs, fix the cause, then back up that day by hand as in Step 4. |
| Local logs are gone after the Pod is recreated | `spec.storage` is not set, so the data volume is an `emptyDir`. See "Prerequisites". |

## Querying historical logs from backups

Once backups are in S3, use [victoria_logs_s3_restore_svr](victoria_logs_s3_restore_svr.md) to restore and query historical logs on demand; for deploying it on Kubernetes, see [Restore server on Kubernetes](k8s_restore_svr.md). The restore server's `-s3.endpoint`, `-s3.bucket`, `-s3.prefix` and environment variables such as `{env}` must match the backup's.

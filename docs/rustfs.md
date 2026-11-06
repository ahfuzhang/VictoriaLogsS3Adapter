# RustFS: A Local S3 Simulation Environment

[RustFS](https://github.com/rustfs/rustfs) is an S3-compatible object store written in Rust.
This project uses it to simulate S3 on development/test machines, so that
`victoria_logs_s3_backup` and `victoria_logs_s3_restore_svr` can be debugged without relying on real cloud storage.

This document covers installing with Docker, starting/stopping, verifying, and using it with this project.
This machine has been deployed and verified as described here (image `rustfs/rustfs:1.0.4`).

## 1. Current deployment on this machine

| Item | Value |
| --- | --- |
| Container name | `rustfs` (`--restart unless-stopped`, started automatically after Docker starts) |
| S3 endpoint | `http://<host IP>:9100` (listens on `0.0.0.0`; `http://127.0.0.1:9100` also works locally) |
| Console | `http://<host IP>:9101/rustfs/console/` |
| Access Key / Secret Key | `rustfsadmin` / `rustfsadmin` |
| Region | `us-east-1` (any value works; the client must match the signature) |
| Data directory | `~/rustfs/data` on the host (mounted at `/data` in the container) |
| Log directory | `~/rustfs/logs` on the host (mounted at `/logs` in the container) |

> Port note: 9000/9001 are often taken by MinIO/QuestDB (9000 is already used by QuestDB on this machine),
> so the host maps to 9100/9101; `test/e2e/backup` uses 19000/19001, so there is no conflict.
> Inside the container, the S3 port is 9000 and the console is 9001.

> Security warning: listening on `0.0.0.0` means any machine on the network can access it, and the default key is the public `rustfsadmin`.
> Use only in internal/test environments; when needed, switch to strong keys or restrict sources with a firewall.

## 2. Install and start

```bash
mkdir -p ~/rustfs/data ~/rustfs/logs

docker run -d --name rustfs --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -p 0.0.0.0:9100:9000 -p 0.0.0.0:9101:9001 \
  -v ~/rustfs/data:/data -v ~/rustfs/logs:/logs \
  -e RUSTFS_ACCESS_KEY=rustfsadmin -e RUSTFS_SECRET_KEY=rustfsadmin \
  rustfs/rustfs:1.0.4
```

Key points:

- `-p 0.0.0.0:9100:9000`: the host listens on all network interfaces (to allow local access only, change to `127.0.0.1:9100:9000`).
- `--user "$(id -u):$(id -g)"`: by default the image runs as the built-in user `rustfs`, which has no write permission on the mounted host directories;
  it fails with `[FATAL] Server runtime failed: Io error: Permission denied (os error 13)` and keeps restarting.
  Running as the current user fixes this. You can also use a Docker named volume (`-v rustfs-data:/data`) to avoid permission problems.
- The image version is pinned to `1.0.4`; see section 6 for upgrading.
- The listen address inside the container is `0.0.0.0` by default; whether it is reachable from outside is determined by `-p`.

### Common commands

```bash
docker ps --filter name=rustfs     # status
docker logs -f rustfs              # logs
docker stop rustfs                 # stop
docker start rustfs                # start
docker rm -f rustfs                # remove the container (data remains in ~/rustfs/data)
```

### Environment variables

| Variable | Meaning |
| --- | --- |
| `RUSTFS_ACCESS_KEY` / `RUSTFS_SECRET_KEY` | Admin credentials (using the defaults produces a warning in the logs; ignorable for testing) |
| `RUSTFS_VOLUMES` | Data directory, default `/data` in the image |
| `RUSTFS_ADDRESS` | S3 listen address inside the container (default `:9000`) |
| `RUSTFS_CONSOLE_ENABLE` / `RUSTFS_CONSOLE_ADDRESS` | Console switch and listen address |

## 3. Verify (only curl needed; no aws cli / mc)

curl ≥ 7.75 supports signing directly with `--aws-sigv4`. Note that bucket names must be at least 3 characters (`InvalidBucketName`).

```bash
s3() { curl -sS --aws-sigv4 "aws:amz:us-east-1:s3" --user rustfsadmin:rustfsadmin "$@"; }
B=http://127.0.0.1:9100          # on a remote machine use http://<host IP>:9100

s3 -X PUT $B/test-bucket                                   # create bucket (200)
echo hello > /tmp/h.txt
s3 -X PUT --data-binary @/tmp/h.txt $B/test-bucket/a/h.txt # upload object (200)
s3 $B/test-bucket/a/h.txt                                  # download: hello
s3 "$B/test-bucket?list-type=2&prefix=a/"                  # ListObjectsV2
s3 -X DELETE $B/test-bucket/a/h.txt                        # delete object (204)
s3 -X DELETE $B/test-bucket                                # delete bucket (204, must be empty)
```

Console: open `http://<host IP>:9101/rustfs/console/` in a browser and log in with the Access/Secret Key
to manage buckets and objects graphically.

If the `aws` CLI is installed:

```bash
export AWS_ACCESS_KEY_ID=rustfsadmin AWS_SECRET_ACCESS_KEY=rustfsadmin AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://127.0.0.1:9100 s3 ls
```

## 4. Using it with this project

Create the bucket first, then pass the endpoint and credentials to the backup/restore programs. RustFS uses path-style access
(addresses look like `http://127.0.0.1:9100/<bucket>/<key>`), so make sure the program uses path-style access.

```bash
curl -sS --aws-sigv4 "aws:amz:us-east-1:s3" --user rustfsadmin:rustfsadmin \
  -X PUT http://127.0.0.1:9100/vl-backup
```

For the specific command-line flags (endpoint, bucket, access key, prefix template, etc.), see:

- [victoria_logs_s3_backup.md](victoria_logs_s3_backup.md)
- [victoria_logs_s3_restore_svr.md](victoria_logs_s3_restore_svr.md)
- [s3_prefix.md](s3_prefix.md)

For end-to-end scripts, see `test/e2e/backup/`; they start their own separate RustFS instance with Docker.

## 5. If the backup program also runs in a container

`127.0.0.1` inside a container refers to the container itself. In that case, use the host's LAN IP as the endpoint
(`http://<host IP>:9100`), or put the container on the same Docker network and use `http://rustfs:9000`.

## 6. Maintenance and troubleshooting

| Scenario | What to do |
| --- | --- |
| Wipe all data | `docker rm -f rustfs && rm -rf ~/rustfs/data/*`, then run the `docker run` from section 2 again |
| Upgrade | `docker pull rustfs/rustfs:<new version>`, `docker rm -f rustfs`, then `docker run` again with the new tag; the data directory is unchanged |
| Container restarts repeatedly | `docker logs rustfs`; usually a data/log directory permission problem, see `--user` in section 2 |
| Port in use | `ss -ltnp \| grep 9100`; change the host port on the left of `-p` |
| Other machines can't connect | Check whether the firewall (`ufw status`) allows 9100/9101 |
| `SignatureDoesNotMatch` | Check that the Access/Secret Key and region match the client |

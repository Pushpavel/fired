# Operating fired on a shared KVM workstation

These notes target one bare-metal Linux machine that also runs other
workloads (in our case ClickHouse and Coder). The sandbox budget is
enforced with hard limits, so sandboxes and the other services can't starve
each other.

## 1. Host prerequisites

```bash
egrep -c '(vmx|svm)' /proc/cpuinfo     # > 0, else enable VT-x/AMD-V in BIOS
ls -l /dev/kvm                          # must exist
stat -fc %T /sys/fs/cgroup              # cgroup2fs
```

Docker must be using cgroup v2, which is the default on current distributions.

## 2. Storage layout

| Path on host | Mounted at | Medium | Holds |
|---|---|---|---|
| `/srv/fired/hot` | `/data/hot` | **tmpfs** (or SSD) | running sandbox disks, hot memory snapshots, jail roots, VM logs |
| `/mnt/hdd/fired/cold` | `/data/cold` | HDD | `<id>.fired.zst` archives (sparse-aware, zstd) |
| `/mnt/hdd/fired/templates` | `/data/templates` | HDD | `rootfs.ext4`, base snapshot (`base/`), build logs |
| `/mnt/hdd/fired/postgres` | (the `db` container) | HDD or SSD | Postgres: sandbox, template, host and quota records |
| `/mnt/hdd/fired/kernels` | `/data/kernels` | HDD | `vmlinux` |

Create the hot tier as a host tmpfs, not a container tmpfs. A host tmpfs
survives container restarts and upgrades, so suspended sandboxes stay hot.
It must allow exec, because the jailer copies the Firecracker binary into
each jail.

```bash
mkdir -p /srv/fired/hot
echo 'tmpfs /srv/fired/hot tmpfs size=48g,mode=0755 0 0' >> /etc/fstab
mount /srv/fired/hot
mkdir -p /mnt/hdd/fired/{cold,templates,postgres,kernels}
```

**What a host reboot does:** it wipes the tmpfs. Any sandbox that was only
on the hot tier (`running`, `suspended`, `stopped`) is lost; archived
sandboxes survive. To protect them:

- Use `shutdown_action: archive` so a clean shutdown archives everything,
  and/or
- keep `ttl.archive_after` short.

On startup fired reconciles state and marks lost sandboxes as `error`.

**Template reads:** templates live on the HDD, but every sandbox reads the
same `rootfs.ext4` and `base/memory`, so after the first boot they are
served from the page cache.

## 3. Memory budget

Everything the sandboxes use is charged to the container's memory cgroup:

- guest RAM that has actually been touched
- Firecracker overhead (about 5–30 MB per VM)
- tmpfs pages: sandbox disks, plus a full memory file per suspended sandbox

Split the container limit along these lines:

```
mem_limit (56g)
 ├─ capacity.max_running_memory_mb   (32g)   sum of memory_mb of running VMs
 ├─ hot snapshots                    (~12g)  suspended sandboxes awaiting archive
 └─ sandbox disks + overhead         (~12g)
```

- **Hot snapshots can't fill the hot tier:** `capacity.min_free_hot_mb`
  (2048) is kept free there. When a start, a memory snapshot or the reaper
  would leave less, fired archives the least recently used suspended or
  stopped sandboxes to the HDD first; only if nothing can be archived does
  a create or resume fail, with `503 no_space`. Size the hot tier for the
  memory of the sandboxes you want suspended there plus this; shorten
  `ttl.archive_after` to move idle ones sooner.
- **`capacity.evict_idle`** suspends the least recently used idle sandbox
  when a create or resume would exceed the budget. Without it, the call
  fails with `503 capacity`.

Also cap the other tenants so they can't grow into the sandbox budget:

- ClickHouse: set `max_server_memory_usage`. The default is 90% of RAM.
- Coder: set memory and CPU limits in its workspace template.

## 4. Kernel

The image already includes a guest kernel at `/data/kernels/vmlinux`. It is
built by the Dockerfile's `kernel` stage. To pin or customize the kernel
outside the image, build it into the mounted kernels directory. The mount
hides the built-in one:

```bash
scripts/build-kernel.sh /mnt/hdd/fired/kernels     # ~10–20 min, needs Docker
```

If you would rather use the kernel in the image, drop the `/data/kernels`
volume from `deploy/docker-compose.yml`.

The script starts from Firecracker's CI guest config and builds in overlayfs,
vsock, ext4, devtmpfs and virtio-rng. **overlayfs is required** for a
writable root. Without it only `/workspace`, `/root`, `/home` and `/tmp` are
writable, and template builds log a warning.

## 5. Run

```bash
cd deploy
export FIRED_API_TOKEN=$(openssl rand -hex 32)      # store it in your backend's secrets
export FIRED_DB_PASSWORD=$(openssl rand -hex 16)
FIRED_BIND=10.0.0.5 docker compose up -d --build
docker compose exec fired fired check
```

Restrict the API to your cluster nodes. Docker-published ports bypass
ufw and INPUT-chain rules, so add the rule to `DOCKER-USER`:

```bash
iptables -I DOCKER-USER -p tcp --dport 8443 ! -s 10.0.0.0/24 -j DROP
```

For mTLS, set `tls.cert`, `tls.key` and `tls.client_ca`, and give your
backend a client certificate.

## 6. Templates

```bash
curl -H "X-Fired-Token: $T" -H 'Content-Type: application/json' $U/v1/templates -d '{
  "name": "node",
  "image": "node:22-bookworm",
  "memory_mb": 1536, "vcpus": 2, "disk_mb": 10240,
  "run": ["apt-get update && apt-get install -y git curl python3 && rm -rf /var/lib/apt/lists/*",
          "corepack enable"]
}'
curl -H "X-Fired-Token: $T" $U/v1/templates/node/logs
```

- **Rebuilding:** building the same name again creates a new build. New
  sandboxes use the newest `ready` build; existing sandboxes keep the build
  they were created from.
- **Private registry:** pull from a local registry (Zot or Harbor) so builds
  don't depend on the internet. Set `firecracker.insecure_registries` if it
  serves plain HTTP. Registry credentials come from the Docker config
  (`~/.docker/config.json`) inside the container.
- **Local images:** `docker save img -o /mnt/hdd/fired/templates/img.tar`,
  then use `"image": "tar:/data/templates/img.tar"`.

## 7. Upgrades

The database schema is migrated on startup (or run `fired migrate` first).
With several hosts, see [cluster.md](cluster.md) for rolling upgrades.

`docker compose up -d --build` replaces the container. On SIGTERM, fired
snapshots every running sandbox (`shutdown_action: suspend`). The new
container finds them on the host tmpfs and resumes them on demand.

`stop_grace_period` must cover snapshotting all running VMs. Snapshots go
to tmpfs, so this is usually a few seconds.

**Firecracker version changes:** memory snapshots are tied to the
Firecracker version. If restoring a snapshot fails after an upgrade, fired
discards it and cold-boots the sandbox from its disk instead. Files are kept
(subject to ext4 journal recovery), but running processes are lost. Template
base snapshots fall back to cold boot the same way; rebuild the templates to
make creates fast again.

## 8. Troubleshooting

| Symptom | Look at |
|---|---|
| Template build fails | `GET /v1/templates/{id}/logs`, and `vm.log` in the template directory |
| Sandbox won't boot or resume | `GET /v1/sandboxes/{id}/logs` (Firecracker and serial console) |
| `firecracker exited during startup` | KVM permissions; whether the jailer can create its cgroup (`fired check`, daemon logs); kernel path |
| Egress denied | daemon log line `egress denied`, with sandbox, host and port; the sandbox's `egress.allow` |
| Commands can't resolve DNS | By design the VM has no DNS. Tools must honor `HTTPS_PROXY` (curl, git, npm, pip, and Node's fetch with `NODE_USE_ENV_PROXY=1`, which is set for you) |
| A process ignores the proxy | Add an allowlisted mirror, or run that step in the template build |

Inside a sandbox, `/etc/fired/env` holds the template environment, and the
proxy variables point at the in-guest forwarder at `127.0.0.1:3128`.

## 9. Security notes

- **Isolation:** each sandbox is a separate KVM guest with its own kernel.
  The jailer runs each Firecracker process as an unprivileged UID, in a
  chroot, with seccomp and its own cgroup.
- **Container privileges:** the fired container is privileged because it
  needs to set up jails and mounts. No sandbox code runs in it.
- **Network:** the VMs have no network interface. Egress goes through the
  proxy with per-sandbox allowlists, and private, loopback and link-local
  destinations are blocked, so sandboxes can't reach ClickHouse, Coder or
  the LAN.
- **Ingress:** inbound traffic only reaches a VM through fired's proxy, over
  vsock, to explicitly exposed ports.
- **Secrets:** `secret_env` and `secret_files` are held in memory only.
  However, a running process that holds a secret ends up in that sandbox's
  memory snapshots, so use short-lived tokens. The cold directory is
  readable only by root.

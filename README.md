# fired

**fired** is a self-hosted sandbox daemon for AI agents. Every sandbox is a
[Firecracker](https://firecracker-microvm.github.io/) microVM with its own
kernel. Sandboxes boot from template snapshots in milliseconds, suspend to a
memory snapshot when idle, move to cold storage when idle for longer, and
wake up again on the next command or preview request, with running processes
intact.

It is a single Docker container that your backend controls over an HTTP API.
Your backend owns users, auth, preview domains and MCP tools; fired only
knows about VMs, commands, files and ports.

```
your backend (K8s)                          KVM host (Docker)
──────────────────                          ─────────────────────────────────────────
MCP tools  ─┐                               fired container  (cpuset / mem_limit)
previews   ─┼── HTTP + token / mTLS ──────▶   API ─ manager ─ reaper ─ egress proxy
PR flows   ─┘                                   │
                                                ├─ jailer ▶ Firecracker VM ─ fired-agent (PID 1)
                                                │      vsock: API, port tunnels, egress
                                                ├─ hot tier  (tmpfs):  running disks, snapshots
                                                └─ cold tier (HDD):    zstd archives, templates
```

## Status

The API, lifecycle state machine, tiering, egress proxy, redaction and guest
agent are covered by integration tests that run against the **fake driver**
(the real guest agent as a host process). The Firecracker driver follows the
Firecracker v1.x API and is unit-tested at the protocol level, but it has
**not yet been run on real KVM hardware**. Run `fired check` and
`scripts/smoke-test.sh` on the target host first; see
[docs/operations.md](docs/operations.md).

## Features

- **Lifecycle:**
  - `running` → `suspended` (hot memory snapshot) → `archived` (compressed on the cold disk) → deleted.
  - Driven by per-sandbox idle TTLs, plus explicit API calls.
  - Any exec, file call or preview request auto-resumes the sandbox.
  - The states, timers and what counts as activity are covered in
    [docs/lifecycle.md](docs/lifecycle.md).
- **Fast creates:** each template is booted once and snapshotted. New
  sandboxes restore that snapshot with a copy-on-write disk instead of
  cold-booting.
- **Memory budget:** caps the total RAM of running sandboxes. When full, the
  least recently used idle sandbox is suspended to make room.
- **Exec:**
  - Foreground commands with captured head and tail output.
  - Background processes with incremental, long-pollable output.
  - Interactive WebSocket streams, with an optional PTY.
- **Per-exec credentials:** pass `secret_env` / `secret_files` on a single
  command. They are never written to disk or logs, and their values (and
  common encodings) are redacted from output.
- **Files:** read, write, list, move and delete files, and upload or
  download tar.gz archives.
- **Port previews:**
  - Expose a port and fired proxies HTTP and WebSocket traffic to it over
    vsock. The VM has no network interface and nothing listens on the host.
  - Ports are deny-by-default.
  - Add `X-Fired-No-Wake` to a request to stop it from waking a suspended sandbox.
- **Egress allowlist:**
  - Sandboxes reach the internet only through a per-sandbox HTTP(S) proxy,
    with domain and port patterns.
  - Private and loopback destinations (your host, your LAN) are blocked
    unless explicitly allowed.
- **Templates:** built from any OCI image (a registry reference or a
  `docker save` tarball), with optional setup commands that are baked into
  the snapshot.
- **Operations:** survives restarts (snapshot on SIGTERM, reconcile on
  start). Also provides an SSE event stream, Prometheus metrics,
  per-sandbox VMM logs and a Go client (`pkg/client`).
- **Artifacts (separate service):** `fired-artifacts` stores versioned file
  trees that agents publish from sandboxes or from inline content, serves
  them, and runs their scheduled jobs in fired sandboxes, writing the results
  back as new versions. It is a separate binary meant to run next to your
  backend on durable storage; see [docs/artifacts.md](docs/artifacts.md).
  It has its own console (`make dev-artifacts`, then
  http://localhost:8444/ui/) with example artifacts to start from.

## Quick start (development, no KVM)

The fake driver runs the guest agent as a normal process. **There is no
isolation**, so use it only for developing against the API.

```bash
make dev        # builds bin/fired + bin/fired-agent, serves on 127.0.0.1:8443, token "dev"

T=dev; U=http://127.0.0.1:8443
curl -s -H "X-Fired-Token: $T" -d '{"name":"base"}' $U/v1/templates
curl -s -H "X-Fired-Token: $T" -d '{"template":"base"}' $U/v1/sandboxes          # → {"id":"sb-…"}
curl -s -H "X-Fired-Token: $T" -d '{"cmd":"echo hi"}' $U/v1/sandboxes/sb-…/exec
```

## Try it on a KVM machine (disposable)

Runs the production image with real Firecracker VMs and nothing mounted.
All state lives inside the container (the hot tier is a container tmpfs), so
stopping it deletes everything.

```bash
make try            # builds fired:latest, then: http://localhost:8443/ui/  (token: dev)
```

The image includes the guest kernel. The first build compiles it, which
takes 10–20 minutes, and later builds reuse it from the cache. `ui: true`
(or `FIRED_UI=1`) turns on the browser console at `/ui/`. It has a terminal,
commands, secrets, files, port previews, lifecycle controls and a live
event/API log, and you sign in with the API token.

Console port previews get their own origin, at
`p<port>-<sandbox>-<sig>.<preview_domain>`, so apps run at `/` the same way
they do behind your backend's preview domain. Without `preview_domain`
(`FIRED_PREVIEW_DOMAIN`), the console uses nip.io for the IP you opened it on,
for example `p3000-web-1a2b3c4d5e6f.127-0-0-1.nip.io:8443`. Some routers block
public DNS names that resolve to private IPs. If nip.io doesn't resolve on your
network, set `preview_domain` to a wildcard DNS name that points at fired.

## Production (the KVM workstation)

The steps are summarized here; the full guide is
[docs/operations.md](docs/operations.md).

1. The image includes a guest kernel at `/data/kernels/vmlinux`. To use your
   own, build it with `scripts/build-kernel.sh /mnt/hdd/fired/kernels` and mount it there.
2. Create a host tmpfs for the hot tier, e.g. `/srv/fired/hot` (size ≈ the
   container memory limit).
3. Edit `deploy/config.yaml` and `deploy/docker-compose.yml` (cpuset,
   mem_limit, bind address, paths).
4. Start it:
   ```bash
   FIRED_API_TOKEN=$(openssl rand -hex 32) docker compose -f deploy/docker-compose.yml up -d
   ```
5. Check the host:
   ```bash
   docker compose exec fired fired check
   ```
6. Restrict the API port to your cluster:
   ```bash
   iptables -I DOCKER-USER -p tcp --dport 8443 ! -s <k8s-cidr> -j DROP
   ```
7. Build a template:
   ```bash
   curl -H "X-Fired-Token: $T" -d '{"name":"node","image":"node:22-bookworm"}' $U/v1/templates
   ```
8. Run the smoke test: `FIRED_URL=… FIRED_API_TOKEN=… scripts/smoke-test.sh`.

## API

All endpoints are under `/v1` and authenticated with `X-Fired-Token: <token>`
or `Authorization: Bearer <token>`. The full reference is
[docs/api.md](docs/api.md).

| | |
|---|---|
| Templates | `GET/POST /templates`, `GET/DELETE /templates/{id-or-name}`, `GET /templates/{id}/logs` |
| Sandboxes | `POST /sandboxes`, `GET /sandboxes?label=k=v&state=`, `GET/PATCH/DELETE /sandboxes/{id}` |
| Lifecycle | `POST /sandboxes/{id}/resume`, `/suspend`, `/archive`; `GET /sandboxes/{id}/logs` |
| Exec | `POST /sandboxes/{id}/exec`, `GET /sandboxes/{id}/exec/stream` (WebSocket) |
| Processes | `GET /sandboxes/{id}/processes[/{pid}[/output]]`, `DELETE /sandboxes/{id}/processes/{pid}` |
| Files | `GET/PUT/DELETE /sandboxes/{id}/files?path=`, `POST /files/mkdir`, `POST /files/move`, `GET/PUT /files/archive`, `GET /files/manifest` |
| Ports | `GET/POST /sandboxes/{id}/ports`, `DELETE /ports/{port}`, `ANY /ports/{port}/proxy/{path}` |
| Ops | `GET /host`, `GET /events` (SSE), `GET /metrics`, `GET /healthz` |

## Integrating with your backend

See [docs/integration.md](docs/integration.md) for:

- Mapping MCP tools (`run_command`, `read_file`, `start_dev_server`, …) onto the API.
- Preview subdomains: auth tickets, `X-Forwarded-Host`, when to wake.
- The credential-bundle pattern: `creds: ["gh", "p4"]` resolved to
  `secret_env` / `secret_files` for a single exec.
- PR workflows.

## Layout

```
cmd/fired          daemon (serve, check, version)
cmd/fired-agent    guest agent: PID 1 init (overlay root) + API server on vsock
cmd/fired-artifacts  artifact service (versions, serving, scheduled jobs)
internal/sandbox   lifecycle manager, reaper, memory budget, templates
internal/server    HTTP API, port proxy, exec stream bridge
internal/vmm       driver interface; firecracker/ (jailer, snapshots); fake/
internal/agent     guest agent implementation
internal/egress    allowlisting egress proxy
internal/rootfs    OCI image → ext4 root filesystem
internal/fsutil    sparse copies and the cold-tier archive format
internal/redact    secret redaction
internal/artifacts artifact service: blob store, versions, jobs, runner
pkg/api, pkg/client  wire types and Go client
pkg/artifacts      artifact service wire types and Go client
```

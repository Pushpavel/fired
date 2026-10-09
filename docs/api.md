# fired API reference

**Base path:** `/v1`.

**Authentication:** send `X-Fired-Token: <token>` or
`Authorization: Bearer <token>` with every request. `/healthz` is the only
endpoint that skips this.

**Request and response bodies:** JSON, unless an endpoint below says
otherwise. Unknown fields in request bodies are rejected.

**Errors:** every error response has this shape:

```json
{"error": {"code": "not_found", "message": "sandbox not found"}}
```

| Status | Codes |
|---|---|
| 400 | `invalid_request` |
| 401 | `unauthorized` |
| 403 | `port_not_exposed` |
| 404 | `not_found`, `template_not_found` |
| 409 | `conflict`, `unrecoverable` |
| 502 | `agent_unreachable`, `port_not_listening`, `proxy_error` |
| 503 | `capacity`, `not_running` |

**Durations:** given as strings like `"10m"` or `"2h"`, or as a number of
seconds. `0` or omitted means "use the daemon default"; a negative value
disables that behavior.

## Sandbox states

```
creating → running ⇄ suspending → suspended → archiving → archived
                ▲                     │                       │
                └────── resuming ◀────┴───────────────────────┘
stopped: the VM exited without a memory snapshot. The disk is kept; the next use cold-boots it.
error:   creation failed or the data is gone (the `error` field says why).
```

| State | Tier | Resume |
|---|---|---|
| `running` | — | — |
| `suspended` | memory and disk on the hot tier | restores the snapshot (processes survive) |
| `archived` | compressed on the cold tier | unpacks, then restores (processes survive) |
| `stopped` | disk on the hot tier | cold boot (processes are lost) |

Any exec, file, process or port-proxy call on a sandbox that isn't running
resumes it first. For the port proxy only, `X-Fired-No-Wake: 1` makes the
call fail with `503 not_running` instead of resuming.

---

## Templates

### `POST /templates` → `202`

Builds asynchronously. Poll `GET /templates/{id}` or watch the event stream.

```json
{
  "name": "node",                       // [a-z0-9-], max 32 characters
  "image": "node:22-bookworm",          // registry ref, or "tar:/path/image.tar"
  "rootfs_mb": 0,                       // default: content size + 30% + 256 MiB
  "disk_mb": 10240,                     // default writable disk per sandbox
  "vcpus": 2, "memory_mb": 1536,        // shape of the base snapshot
  "snapshot": true,                     // boot once and snapshot (fast creates)
  "env": {"NPM_CONFIG_UPDATE_NOTIFIER": "false"},
  "workdir": "/workspace",
  "run": ["corepack enable", "npm i -g pnpm@9"]   // runs before the snapshot
}
```

Each build gets an id of the form `<name>-<random>`. Sandboxes reference a
template by name, which resolves to the newest `ready` build, or by exact
id.

Sandboxes created with the template's own `vcpus` and `memory_mb` clone the
snapshot. Any other shape cold-boots, because a snapshot fixes the vCPU
count and memory size.

### Other template endpoints

- `GET /templates` → `{"templates": [Template]}`
- `GET /templates/{id-or-name}` → `Template` with fields `status`
  (`building|ready|failed`), `error`, `has_snapshot`, …
- `GET /templates/{id-or-name}/logs?bytes=65536` → the build log, as text.
- `DELETE /templates/{id-or-name}`:
  - By id, deletes that build. By name, deletes every unused build of that name.
  - Returns `409` if a sandbox still uses the template.

---

## Sandboxes

### `POST /sandboxes` → `201 Sandbox`

```json
{
  "template": "node",
  "id": "optional-client-id",           // [a-z0-9-], 3–40 characters
  "vcpus": 2, "memory_mb": 1536, "disk_mb": 10240,
  "env": {"CI": "1"},                   // added to every exec
  "labels": {"user": "42", "session": "abc"},
  "ttl": {"idle_suspend": "5m", "archive_after": "1h", "delete_after": "168h"},
  "egress": {"allow": ["github.com", "*.githubusercontent.com", "registry.npmjs.org"]},
  "redact": true
}
```

A `Sandbox` looks like this:

```json
{
  "id": "sb-…", "template": "node", "template_id": "node-ab12cd34",
  "state": "running", "vcpus": 2, "memory_mb": 1536, "disk_mb": 10240,
  "env": {}, "labels": {}, "ttl": {}, "egress": {"allow": []}, "redact": true,
  "ports": [3000], "created_at": "…", "last_active_at": "…",
  "state_changed_at": "…", "error": ""
}
```

**Egress patterns:**

| Pattern | Matches |
|---|---|
| `github.com` | exactly that host |
| `*.github.com` | subdomains only |
| `host:8443` | that host, only on port 8443 |
| `*` | any host |

A pattern without a port allows only the configured `egress.allowed_ports`
(default 443 and 80). Private and loopback addresses are refused unless they
are listed in `egress.allow_private_cidrs`. The entries in
`egress.global_allow` are added to every sandbox's list.

### Other sandbox endpoints

- `GET /sandboxes?label=user=42&label=session=abc&state=running` →
  `{"sandboxes": [...]}`
- `GET /sandboxes/{id}`
- `PATCH /sandboxes/{id}`: send any of `labels`, `env`, `ttl`, `egress`,
  `redact`. Each one you send replaces the whole field. Changes take effect
  immediately, including egress.
- `DELETE /sandboxes/{id}` → `204`. Stops the VM and deletes the hot data,
  the archive and the logs.
- `POST /sandboxes/{id}/resume | /suspend | /archive` → `Sandbox`. All three
  are idempotent.
- `GET /sandboxes/{id}/logs?bytes=65536`: the Firecracker and serial console
  log, useful when a boot fails.

---

## Exec

### `POST /sandboxes/{id}/exec`

```json
{
  "cmd": "npm test",                    // run with /bin/sh -c …
  "args": ["git", "status"],            // …or exec'd directly (one of cmd/args)
  "cwd": "/workspace/repo",             // default: template workdir
  "env": {"FORCE_COLOR": "0"},
  "secret_env": {"GH_TOKEN": "ghs_…"},  // never logged or persisted; redacted from output
  "secret_files": {"p4tickets": "…"},   // written to $SECRET_DIR/<name> (mode 0600)
  "user": "node",                       // default root
  "stdin": "…",
  "timeout_s": 300,                     // foreground only; capped by exec.max_timeout
  "background": false
}
```

**Foreground** runs return:

```json
{"exit_code": 0, "stdout": "…", "stderr": "…", "truncated": false, "timed_out": false, "duration_ms": 1234}
```

- Output beyond `exec.max_output_bytes` per stream keeps the head and the
  tail, with a `[N bytes truncated]` marker in between.
- When the timeout expires, the process group is killed and `timed_out` is set.

**Background** runs return `{"pid": 123}` immediately. The process keeps
running after the call, keeps its environment (including secrets), and
stays alive across suspend and resume.

**`$SECRET_DIR`:** a per-exec directory under `/run/fired/secrets/`. It
stays in place for the sandbox's lifetime so that background processes can
keep reading it. The literal strings `$SECRET_DIR` and `${SECRET_DIR}` in
`env` and `secret_env` values are expanded to its path.

### `GET /sandboxes/{id}/exec/stream` (WebSocket)

1. The client sends an `ExecRequest` as a text frame. Add `"pty": true,
   "cols": 120, "rows": 30` for a terminal.
2. The server sends `{"type":"started","pid":…}` as a text frame.
3. Output arrives as binary frames: the first byte is the channel (`1` =
   stdout, `2` = stderr) and the rest is data. With a PTY, all output is on
   channel 1.
4. The client can send:
   - binary frames: written to stdin
   - text frames:
     - `{"type":"resize","cols":…,"rows":…}`
     - `{"type":"signal","signal":"INT"}`
     - `{"type":"eof"}`
5. The stream ends with `{"type":"exit","exit_code":…}`, or
   `{"type":"error","message":…}` on failure.

If the WebSocket closes, the process group is killed. While a stream is
open, the sandbox counts as busy and won't idle-suspend.

### Processes

- `GET /sandboxes/{id}/processes` → `{"processes": [{pid, cmd, background, running, exit_code, started_at, exited_at}]}`
- `GET /sandboxes/{id}/processes/{pid}`
- `GET /sandboxes/{id}/processes/{pid}/output?since_stdout=0&since_stderr=0&wait_ms=5000`
  returns:
  ```json
  {"pid": 1, "running": true, "stdout": "…", "stderr": "…", "stdout_offset": 1234, "stderr_offset": 0, "dropped": false}
  ```
  - Pass the returned offsets back as `since_*` to receive only new output.
  - `wait_ms` long-polls until new output arrives or the process exits.
  - The last 1 MiB of each stream is retained; `dropped` is true when the
    requested offset has already been discarded.
- `DELETE /sandboxes/{id}/processes/{pid}?signal=TERM` signals the process
  group. Supported signals: TERM, KILL, INT, HUP, QUIT, USR1, USR2.

---

## Files

Relative paths are resolved against the template workdir (`/workspace`).

| Request | Response |
|---|---|
| `GET /files?path=src/app.ts` | Raw bytes, with headers `X-Fired-Type: file` and `X-Fired-Mode: 0644` |
| `GET /files?path=src` (a directory) | `X-Fired-Type: dir` and a `DirListing`: `{"path", "entries": [{name, path, size, mode, is_dir, is_link, mod_time}]}` |
| `GET /files?path=…&stat=1` | `FileInfo` |
| `PUT /files?path=…&mode=0644`, body = raw bytes | `FileInfo`. Parent directories are created and the write is atomic. |
| `DELETE /files?path=…&recursive=1` | `204` |
| `POST /files/mkdir?path=…` | `FileInfo` (creates parents) |
| `POST /files/move`, body `{"from":"a","to":"b"}` | `FileInfo` |
| `GET /files/archive?path=dist&exclude=node_modules&exclude=.git` | A tar.gz with entries relative to `path` |
| `PUT /files/archive?path=dest`, body = tar.gz | Extracted into `dest` |

---

## Ports

- `POST /sandboxes/{id}/ports` with `{"port": 3000}` → `{"ports": [3000]}`.
  Only exposed ports are reachable through the proxy.
- `GET /sandboxes/{id}/ports`
- `DELETE /sandboxes/{id}/ports/{port}`
- `ANY /sandboxes/{id}/ports/{port}/proxy/{path…}`: HTTP and WebSocket proxy
  to `127.0.0.1:{port}` inside the VM.
  - **Host header:** the app sees `X-Forwarded-Host` if the request has one,
    otherwise the incoming `Host`. `X-Forwarded-For` and `X-Forwarded-Proto`
    are passed through.
  - **Stripped before forwarding:** `X-Fired-Token`, `X-Fired-No-Wake`, and
    an `Authorization` header that carries the fired token.
  - **Waking:** the sandbox is resumed if needed. Send `X-Fired-No-Wake: 1`
    to get `503 not_running` instead.
  - **Errors:** `502 port_not_listening` if nothing is listening on the port.
  - **Activity:** proxied traffic counts as activity. An open WebSocket does
    not keep the sandbox busy on its own.
- **Header routing, the same proxy without path rewriting:** send any
  request, with its original path and query, to fired with
  `X-Fired-Sandbox: <id>` and `X-Fired-Port: <port>` plus the API token. This
  is for a front proxy that owns the wildcard preview domain: it maps
  `p3000-<id>.preview.example.com/some/page` to a sandbox and port and
  forwards `/some/page` as it is. All the rules above apply. Both headers are
  removed before forwarding. The front proxy must set these headers itself
  and drop any `X-Fired-*` headers that come from clients.

---

## Operations

- `GET /host` returns `driver`, `version`, `memory_budget_mb`,
  `memory_running_mb`, `counts` by state, and the used/free bytes of the hot
  and cold directories.
- `GET /events?sandbox=<id>` is a Server-Sent Events stream:
  - event types: `sandbox.state`, `sandbox.deleted`, `template.status`
  - data: `{"type", "sandbox_id", "state", "prev_state", "template", "status", "error", "at"}`
- `GET /metrics` is in Prometheus text format.
- `GET /healthz` needs no authentication.

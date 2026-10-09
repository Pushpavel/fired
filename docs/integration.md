# Integrating fired with your agent platform

fired is deliberately generic. Your backend is its only client and keeps
everything user-facing: users and sessions, MCP tools, preview domains,
credential storage, and GitHub.

## Sandbox per session

```go
c := client.New(os.Getenv("FIRED_URL"), os.Getenv("FIRED_API_TOKEN"))

// Find or create the sandbox for this chat/session.
list, _ := c.ListSandboxes(ctx, map[string]string{"session": sessionID}, "")
var sb *api.Sandbox
if len(list) > 0 {
    sb = &list[0]
} else {
    sb, err = c.CreateSandbox(ctx, api.CreateSandboxRequest{
        Template: "node",
        Labels:   map[string]string{"session": sessionID, "user": userID},
        Egress:   &api.EgressPolicy{Allow: []string{"github.com", "*.githubusercontent.com", "registry.npmjs.org"}},
    })
}
```

You don't have to resume anything yourself. Every call resumes the sandbox
if needed, including from cold storage.

## MCP tools

| Tool | fired call |
|---|---|
| `run_command(cmd, cwd?, creds?)` | `POST /exec` (foreground). Return `stdout`/`stderr`/`exit_code`, and flag `truncated` / `timed_out`. |
| `start_process(cmd, creds?)` | `POST /exec` with `background: true` → `pid` |
| `read_process_output(pid)` | `GET /processes/{pid}/output?since_…&wait_ms=…`. Keep the offsets per pid in the tool state. |
| `stop_process(pid)` | `DELETE /processes/{pid}?signal=TERM` |
| `read_file` / `write_file` / `list_dir` | `GET` / `PUT` / `GET /files` |
| `expose_port(port)` → preview URL | `POST /ports`, then return `https://p{port}-{sandbox}.preview.example.com` |
| `download_artifact(path)` | `GET /files/archive?path=dist` → store the bundle in object storage |

Keep foreground timeouts below your HTTP client timeout. Use
`timeout_s` ≤ 300 for agent tool calls, and background processes for
anything longer, such as dev servers and long builds.

## Credentials (`creds: ["gh", "p4"]`)

The agent names credential bundles. Your backend resolves them and passes
the values to that one exec:

```go
bundles := map[string]func(ctx context.Context, s Session) (env, files map[string]string, err error){
    "gh": func(ctx context.Context, s Session) (map[string]string, map[string]string, error) {
        tok, err := githubApp.InstallationToken(ctx, s.Repo) // short-lived, repo-scoped
        return map[string]string{"GH_TOKEN": tok, "GITHUB_TOKEN": tok}, nil, err
    },
    "p4": func(ctx context.Context, s Session) (map[string]string, map[string]string, error) {
        ticket, err := p4.LoginTicket(ctx, s.User) // ticket with a group timeout
        return map[string]string{
            "P4PORT": "ssl:perforce.internal:1666", "P4USER": s.User,
            "P4TICKETS": "$SECRET_DIR/p4tickets",
        }, map[string]string{"p4tickets": ticket}, err
    },
}

req := api.ExecRequest{Cmd: cmd, SecretEnv: map[string]string{}, SecretFiles: map[string]string{}}
for _, name := range creds {
    if !session.Granted(name) { return errNotGranted(name) }
    env, files, err := bundles[name](ctx, session)
    if err != nil { return err }
    maps.Copy(req.SecretEnv, env)
    maps.Copy(req.SecretFiles, files)
}
res, err := c.Exec(ctx, sb.ID, req)
```

**Making git use the token only when `gh` is requested:** bake a credential
helper into the template, so git reads the token from the environment at
call time:

```
git config --system credential.helper '!f() { test -n "$GH_TOKEN" && echo username=x-access-token && echo password=$GH_TOKEN; }; f'
git config --system url."https://github.com/".insteadOf git@github.com:
```

**What fired does with the secrets:**

- It never writes secret values to SQLite or to its logs.
- It redacts the values (raw, base64 and URL-encoded) from exec output,
  stream output and process output.
- Background processes keep their secrets, and `$SECRET_DIR` stays for the
  sandbox's lifetime. That is why every bundle should mint short-lived
  tokens.

## Preview subdomains

The browser hits `https://p3000-<sandbox>.preview.example.com`. Your backend
then:

1. **Parses** the host into `port` and `sandbox`, and looks up which user
   owns that sandbox.
2. **Authenticates** with a short-lived ticket. Use a separate registrable
   domain for previews, so preview pages can never read your app's cookies:
   - `/__auth?t=<jwt>` sets a host-only, `HttpOnly` cookie and redirects.
   - Later requests check that cookie.
   - **Remove the cookie** before forwarding.
3. **Decides whether to wake** the sandbox:
   - Top-level navigations (`Sec-Fetch-Mode: navigate`) may wake it.
   - Other requests (HMR pings, assets, XHR) carry `X-Fired-No-Wake: 1`. On
     `503 not_running`, serve a small "starting…" page that calls
     `POST /resume` and reloads.

   This stops a background tab's HMR reconnect loop from keeping a sandbox
   awake forever.
4. **Forwards** to `{FIRED_URL}/v1/sandboxes/{id}/ports/{port}/proxy{path}`,
   adding `X-Fired-Token` and `X-Forwarded-Host: p3000-<id>.preview.example.com`
   and passing WebSocket upgrades through.

   Alternatively, forward the request **unchanged** (same path and query) to
   `{FIRED_URL}` with `X-Fired-Sandbox: <id>` and `X-Fired-Port: 3000` added.
   No path rewriting is needed, so a plain nginx, Caddy or Traefik route works:

   ```nginx
   server {
     server_name ~^p(?<fport>\d+)-(?<fsb>[a-z0-9-]+)\.preview\.example\.com$;
     location / {
       # after your auth check
       proxy_set_header X-Fired-Sandbox $fsb;
       proxy_set_header X-Fired-Port    $fport;
       proxy_set_header X-Fired-Token   $fired_token;
       proxy_set_header X-Forwarded-Host $host;
       proxy_set_header Upgrade $http_upgrade;
       proxy_set_header Connection $connection_upgrade;
       proxy_http_version 1.1;
       proxy_pass http://fired-host:8443;
     }
   }
   ```

   `proxy_set_header` replaces anything a client sent under those names. With
   other proxies, make sure client-supplied `X-Fired-*` headers are dropped.

For trying things by hand, fired's console (`ui: true`) can also serve
previews itself on `p<port>-<sandbox>-<sig>.<preview_domain>`, or on nip.io
when no domain is configured. The signature in the hostname is a capability
for that one port. Your product should still use the flow above, which adds
per-user auth and wake control.

**Dev server settings for the template.** Vite rejects unknown Host headers
and needs to know where HMR connects back to:

```js
// vite.config.js
export default {
  server: {
    host: "127.0.0.1",
    allowedHosts: [".preview.example.com"],
    hmr: { clientPort: 443, protocol: "wss" },
  },
};
```

**Static artifacts** (slides, docs, built React apps) don't need a VM to be
viewed. Download `GET /files/archive?path=dist`, store the bundle in MinIO,
and serve it from the preview domain directly.

## Pull requests

- **Inside the sandbox:** `git clone` / `fetch` / `push` with `creds: ["gh"]`,
  plus the build and test commands.
- **In your backend, never inside the sandbox:** opening PRs, review
  comments and merges, using the GitHub API with your app's credentials.
  High-impact actions such as merging or deleting branches should go
  through an approval step.

## Events

`GET /v1/events` (SSE) streams state changes. Use it to show
"sleeping / waking / running" in your UI without polling.

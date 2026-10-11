# 10. Credentials

A stub broker on the host (`make dev-artifacts BROKER_URL=http://127.0.0.1:8445
BROKER_TOKEN=broker-dev`, new): for quota key `u10` it answers `API_TOKEN`
(`tok-<run id>-s3cretvalue`) and a secret file `key.pem` (a PEM-style key);
`u10-refused` gets `403 owner_gone`; `u10-slow` answers after 15s (the
broker timeout is 10s). Artifact `u10-creds` prints its credentials in
several encodings and tries to write each to the db. Then a fired sandbox
with a secret, published with `from_sandbox`. Real Firecracker runners;
deleted afterwards, and dev runs without a broker again.

## Saw

- **Secret files are names, not paths.** The stub's `/run/secrets/key.pem`
  failed every run: "fired: 400 invalid secret file name". They land in
  `$SECRET_DIR/<name>`, mode 0600, which docs/artifacts.md didn't say.
- The broker got `{artifact, quota_key, version, run_id}` with its bearer
  token, once per run.
- **Logs:** the token raw, base64 and URL-encoded were
  `[REDACTED:API_TOKEN]`, on stdout and stderr. Reversed, a prefix or a
  token split in two get through (expected: it catches accidents).
- **Secret files leaked (first try):** a line of `key.pem` printed in the
  logs wasn't redacted, and the whole file written to the db was
  accepted: a multi-line secret was matched only whole, and in JSON its
  newlines are `\n`.
- **db writes:** the token raw, base64 and URL-encoded were refused with
  `422 secret` ("leak/raw contains the value of credential API_TOKEN");
  reversed, prefix and split were written.
- **Refused:** `failed`, "credentials: 403 owner_gone: the owner
  disconnected their account".
- **Slow broker:** `pending`, "waiting: broker unavailable: … Client.Timeout
  exceeded", retried. Another user's run meanwhile started at once, but
  only because it was dispatched while the slow call was in flight: the
  backoff after a broker failure held back every user's runs.
- **from_sandbox:** a folder with `config.js` holding the token, then one
  with the key file and one line of it, then only the line: each refused
  with `422 secret_found` naming the file and the secret ("(and 1 more
  files)"); nothing was created.

## Fixed

- **Multi-line secrets** (fired and fired-artifacts share `redact`): every
  line of 16+ characters counts on its own (PEM armor lines don't), and
  the whole value is matched JSON-escaped too. Now a line of the key is
  `[REDACTED:key.pem]` in logs, the file and the line are refused as db
  writes, and `from_sandbox` refuses a file holding just the line.
- **A broker failure backs off only its quota key** (15s): the broker may
  fail for one owner's accounts, so other users' runs go on. fired being
  unavailable still holds back every run.
- `make dev-artifacts` takes `BROKER_URL` / `BROKER_TOKEN`.
- Docs: secret files are keyed by name and found in `$SECRET_DIR`; what
  the secret check matches.

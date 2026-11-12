# Optional UI child

`fae serve --ui` makes the daemon supervise one UI child process
(FAE-API-030..033). It is disabled by default. When it is disabled, nothing is
created: no `run/ui` directory and no registry, and status reports
`"ui": {"enabled": false}`.

The operator UI is not part of the core: non-goal 6 of FAE-GEN-010 keeps it
out of the `fae` binary. The daemon carries only the generic child
supervision; this package (`ui`) holds the launch contract (bind and port
rules, secret files, parent-death signal), and the reference UI is the
separate `fae-ui` executable (`cmd/fae-ui`, server in `ui/server`). Build it
beside `fae`:

```sh
go build -o bin/fae ./cmd/fae && go build -o bin/fae-ui ./cmd/fae-ui
```

Without `--ui-bin`, the daemon runs `fae-ui` from the directory of its own
executable. A missing UI binary is a warning, at `serve` and in the daemon
log; the daemon starts without the UI (status `ui.health` is
`missing-binary`). `TestCoreBinaryExcludesTheOperatorUI` fails if
`go list -deps ./cmd/fae` ever includes `ui/server`.

```sh
fae serve --workspace "$PWD" --ui [--ui-port N] [--ui-bind 127.0.0.1] \
  [--ui-bin /abs/ui] [--ui-dist /abs/assets] [--ui-allow-writes] [--ui-rpc-log /abs/ui-rpc.log]
fae ui token          # {"token", "url", "login_url", "writes_enabled"}; instance operator only
```

Open the `login_url`. The one-time `?token=` is exchanged for an `HttpOnly`,
`SameSite=Strict` session cookie, and the browser is redirected to a URL
without the token. Scripts send `Authorization: Bearer <token>` instead. To
reach the UI from another machine, use a port forward over an authenticated
channel (for example `ssh -L`). A wider bind is never accepted.

## Listener rules

- The bind must be a literal loopback address: `127.0.0.0/8`, `::1`, or
  `localhost` as an alias for `127.0.0.1`. Anything else, including `0.0.0.0`,
  `::`, a LAN address or a hostname, is refused at `serve`, in the daemon and in
  the child, with a message naming the value. In each case the refusal happens
  before any file or listener exists.
- The token is required on every request, static or API. It is compared in
  constant time on SHA-256 digests, so neither length nor a matching prefix
  ends the comparison early.
- Requests must come from a loopback peer and must address the listener by its
  own host (`127.0.0.1:PORT`, `localhost:PORT` or `[::1]:PORT`), which defeats
  DNS rebinding.
- Responses carry `Cache-Control: no-store`, a CSP that allows only same-origin
  scripts and styles, `X-Frame-Options: DENY` and `Referrer-Policy: no-referrer`.
- `--ui-port 0` (the default) derives a stable per-instance port in
  18658..22753 from the instance root. A pinned port must be 1024..65535 and
  must differ from Dolt's port.

## Authority

The daemon issues the UI its own RPC credentials when it starts. They are
registered as delegated principals before the socket listens:

| Principal | When | May |
|---|---|---|
| `ui` | always | read RPCs only |
| `ui-writer` | only with `--ui-allow-writes` | mutating RPCs other than `ProphetChat`. The cited message must explicitly direct the verb (`authorize.verbs`), as for any delegated non-controller actor. Operator-only handlers (backups, cleanup, `Message`, `UIToken`, ...) still refuse it. The audit row names `ui-writer`. |

Operator principal configuration cannot declare either actor. The reference
UI has a single write: alert acknowledgement (`POST /api/alerts/ack` with
`{"id", "authorizing_message"}`). It accepts only same-origin JSON, and only
when the write capability was issued. Status reports `ui.writes_enabled`.

## Child contract (for `--ui-bin`)

The daemon starts the child after it is ready, in `<root>/run/ui` (0700),
under the owned-process registry `<root>/run/ui-processes`, with a
parent-death signal on Linux. The environment carries only `PATH`, `HOME`,
`TMPDIR`, `LANG`, `LC_ALL` and `TZ`. Only paths travel on argv:

```
<bin> --bind ADDR --port N --socket <root>/run/rpc.sock --secrets-dir <root>/run/ui \
      --instance NAME [--dist DIR] [--rpc-log FILE]
```

The reference `fae-ui` takes exactly these flags.

Before each launch, the daemon writes these owner-only files into the secrets
directory. The child must read each one and remove it before it listens, as
the resident worker does with its key:

| File | Content |
|---|---|
| `token` | 64 hex characters: the HTTP token |
| `rpc.credential` | 32 raw bytes: the `ui` RPC credential (send it hex-encoded in `Authorization`) |
| `write.credential` | 32 raw bytes: the `ui-writer` capability; present only with `--ui-allow-writes` |

The child must serve `GET /healthz` behind the token and answer 200 when
healthy. The daemon probes it over loopback, every 100 ms until the child is
first healthy (within 30 s), then every 30 s. The child must exit on SIGTERM.
The daemon waits 5 s, then kills it.

## Supervision

Health is reported as one of `starting`, `healthy`, `unhealthy`, `restarting`,
`exhausted`, `missing-binary`, `unavailable` or `stopped`. Status `ui` also
carries `url`, `pid`, `restarts`, `writes_enabled`, `binary`, `last_exit` and
`checked_at`. It never carries the token.

- **Crashes:** the child is restarted with exponential backoff from 1 s,
  doubling to 60 s. The backoff resets after a run that lasts 60 s.
- **Restart limit:** at most 10 restarts in any hour. After that the child is
  left stopped and one `ui-child` alert is raised.
- **Events:** each launch and crash records a `child-started`,
  `child-restarted` or `child-crashed` event.
- **Missing binary:** logged as a warning; the daemon starts without the UI.
- **Shutdown:** the child is stopped before the RPC socket closes.
  Unconsumed secrets are removed.
- **Secrets in output:** the child's output goes to the supervision log under
  component `ui`, with the token and credentials redacted.

The reference UI shows status, the board, task history and alerts. It refreshes
every 5 s and makes no external network calls. Its assets are embedded and
write untrusted text with `textContent` only. `--ui-dist` serves an operator
directory through `os.Root`: only regular, non-hidden files, with no
directory listing and no symlink escape. `--ui-rpc-log` appends one JSON line
per daemon call, holding the method and outcome but never the parameters or
secrets.

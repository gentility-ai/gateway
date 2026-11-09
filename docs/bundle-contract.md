# Gateway bundle contract

The interface between the three parts of gateway slice 2:
- the public packaging repo `gentility-ai/gateway`, which builds and signs
  bundles;
- the gentility agent (`gentility-ai/agent`) and its root helper, which
  install them;
- the Gentility server, which lists them and asks for a move.

A change here is a change to all three, made in this file first.

## The bundle

Each green build publishes one GitHub release in `gentility-ai/gateway`,
tagged `gateway-<upstream version>-<plugin version>`, e.g.
`gateway-8.0.60-0.2.0`. The pair is the bundle's id.

Its assets:

| asset | what |
|---|---|
| `bundle.tar` | the bundle itself: `manifest.json`, `manifest.sig`, upstream's archive and `gentility-usage.so`, flat, no directories |
| `manifest.json` | the same bytes as in the tar, so the server can verify without downloading the bundle |
| `manifest.sig` | the same bytes as in the tar |

### manifest.json

```json
{
  "kind": "gateway",
  "format": 1,
  "bundle": "8.0.60-0.2.0",
  "upstream": {
    "repo": "router-for-me/CLIProxyAPI",
    "version": "8.0.60",
    "archive": "CLIProxyAPI_8.0.60_linux_amd64.tar.gz",
    "sha256": "<hex, equal to upstream's checksums.txt>"
  },
  "plugin": {
    "id": "gentility-usage",
    "version": "0.2.0",
    "commit": "<gentility-ai/gateway commit>",
    "file": "gentility-usage.so",
    "sha256": "<hex>"
  },
  "built_at": "<RFC 3339 UTC>",
  "build": "<URL of the workflow run>",
  "changelog": [
    {"version": "8.0.59", "lines": [{"text": "fix(pluginhost): …", "flagged": true}]}
  ]
}
```

Rules for the manifest:
- **Readers refuse:**
  - any `kind` other than `gateway`;
  - any `format` they don't know;
  - a file whose sha256 differs from the manifest's.
- **Unknown fields** are ignored.
- **`changelog`** covers the upstream releases since the previous bundle's
  upstream version. It is informational: nothing verifies it.

### manifest.sig

```json
{"key_id": "<16 hex chars: the first 8 bytes of the public key>", "signature": "<base64 Ed25519 signature over the exact bytes of manifest.json>"}
```

## The keys

- Each signing key is Ed25519. Its private half is kept offline by the
  maintainers. The current key also lives in this repo's `publish`
  environment, which only `main` can use.
- Agent, helper and server each pin the same **set** of public keys:
  `{"keys": {"<key_id>": "<base64 32-byte public key>"}}`, holding the current
  key and the next one.
- A signature verifies when its `key_id` is in the set and the signature
  checks under that key.
- **The helper's set** is root-owned, at `/etc/gentility-gateway/keys.json`
  (mode 0644, owner `root:root`), shipped with the agent package. It is never
  read from a path or key the caller passes.
- **The server's set** is in its config.

## The command

The server sends it as a `command.request` on the agent's existing channel,
with type `gateway_install`:

```json
{"move_id": "<uuidv7>", "bundle_url": "<release asset URL of bundle.tar>", "sha256": "<hex of bundle.tar>"}
```

The answer:

```json
{
  "move_id": "<uuidv7>",
  "result": "moved | rolled_back | refused",
  "reason": "<short words; empty when moved>",
  "running": {"bundle": "8.0.60-0.2.0", "upstream_version": "8.0.60", "plugin_version": "0.2.0"},
  "previous": {"bundle": "8.0.47-0.2.0", "upstream_version": "8.0.47", "plugin_version": "0.2.0"}
}
```

- **`running`** is what runs after the attempt, read from the host, never
  from the request.
- **`previous`** is null when the host had no bundle record (the first move
  from a hand install).
- **Readers accept an unknown `result` word leniently.** The server records
  it as is.

**A repeated `move_id`** is answered from the first attempt's record and does
not install twice. Records live root-owned in
`/var/lib/gentility-gateway/moves/<move_id>.json`.

**What runs** is root-owned in `/var/lib/gentility-gateway/current.json`,
which has the shape of `running`, and the helper writes it. The agent reports
that object as `gateway` in its `agent.status` frame (sent on connect and
after security changes), or `null` when the file is absent. A frame without
the key changes nothing.

## The helper

`/usr/libexec/gentility/gentility-gateway-install`, owner `root:root`, mode
0755. The agent package ships it with
`/etc/sudoers.d/gentility-gateway`:

```
gentility ALL=(root) NOPASSWD: /usr/libexec/gentility/gentility-gateway-install
Defaults!/usr/libexec/gentility/gentility-gateway-install env_reset, !setenv
```

The package checks that file with `visudo -c` before installing it.

**Invocation:** the agent passes only the move id, the URL and the sha256, as
arguments. The helper downloads the bundle itself, into a root-owned staging
dir. It takes no paths.

**Steps, each failure ending the attempt with a reason:**

1. **Repeat move.** If `moves/<move_id>.json` exists, print it and stop.
2. **Fetch.** Download `bundle.tar` into
   `/var/lib/gentility-gateway/staging/<move_id>/` (root, 0700). Refuse
   unless its sha256 equals the argument.
3. **Verify.** Extract into the same staging dir. Then check, in order:
   `manifest.sig` against `/etc/gentility-gateway/keys.json`; `kind`;
   `format`; each file's sha256. A failure here ends in `refused`, and
   nothing on the host has changed.
4. **Install beside.**
   - The proxy goes to `/opt/cliproxyapi/<upstream version>/cli-proxy-api`.
   - Before the plugin is replaced, keep the running plugin, `config.yaml`
     and the drop-in under `/var/lib/gentility-gateway/previous/`.
   - The plugin goes to `/var/lib/cliproxyapi/plugins/linux/amd64/`.
5. **Switch.** Point the drop-in
   (`/etc/systemd/system/cliproxyapi.service.d/omp-gateway.conf`) at the new
   binary by writing a temp file and renaming it. Then `daemon-reload` and
   restart `cliproxyapi`.
6. **Check, within 60 s of the restart:**
   - the journal since the restart shows
     `plugin registered plugin_id=gentility-usage … version=<plugin.version>`
     and `API server started successfully`;
   - `GET /v1/models` on the configured listen address answers, 401 or 200.
7. **On a failed check, roll back.** Restore the previous drop-in, plugin and
   config, then `daemon-reload` and restart. Re-run the check against the
   previous versions. Answer `rolled_back`, with the reason the new bundle
   failed.
8. **Record.** Write `current.json` from what passed its check, and
   `moves/<move_id>.json` with the answer. Remove the staging dir.

**A move cut off midway** leaves the old version running. The drop-in is the
only switch, and it changes in one rename after the files are in place.

**Accepted risk:** a compromised `gentility` user can reinstall any older
signed bundle. Only signed bundles install, and `current.json` reports what
runs.

## The server

- **Listing.** It reads `gentility-ai/gateway` releases through the existing
  GitHub App token, cached briefly. It keeps no table of releases.
- **Verifying.** For each release it fetches `manifest.json` and
  `manifest.sig` and verifies them with its key set and kind. A release that
  fails is listed as refused, with the reason, and is never offered.
- **Sending.** It sends `gateway_install` only to a machine marked as a
  gateway host, and only from a person's press.
- **Facts.** Each move appends two facts under its `move_id`: `requested`
  (machine, from, to, bundle sha256, actor), then the outcome (`moved`,
  `rolled_back`, `refused`, or `no_answer`).
  - A timeout job writes `no_answer` when the agent never answers.
  - The projection on the machine holds what the host reports: the answer's
    `running`, or `agent.status`'s `gateway`.

## The management calls

`contract/calls.json` in the gateway repo lists the management calls the
Gentility server makes: method, path, query, headers and body shape. The
contract test sends each one as written. The server pins a copy, and a server
test fails when its calls differ from the copy. Today's calls:

| method | path | notes |
|---|---|---|
| PATCH | `/v0/management/api-keys` | body `{"old": k, "new": k}`, an idempotent add |
| DELETE | `/v0/management/api-keys` | query `value=k`; a 404 counts as done |
| GET | `/v8/management/config/access/api-keys` | answers with an `ETag` |
| PUT | `/v8/management/config/access/api-keys` | `If-Match: <etag>`, body is the list; a 412 means a stale read, so read again |
| GET | `/v1/models` | `Authorization: Bearer k`; a 200 means the key is accepted |

## The key prefixes

`contract/key_prefixes.json` maps the prefix of every client key the server
mints or admits (`omp_`, `claude_`, `codex_`, `rk_live_`, `rk_test_`) to the
harness it names. From plugin
0.5.0 the plugin refuses, with a `key_scope` 403 before any upstream call, a
request whose key's harness is not the harness of the login serving it; an
unprefixed key gets no inference. A plugin test holds the plugin's table to
this file, and the plugin does not read it at runtime. The server pins a copy
of the file, and its own test holds that copy. The refusal depends on the plugin being loaded and not fused:
upstream serves without it, so the README's key scoping section says when
that happens.

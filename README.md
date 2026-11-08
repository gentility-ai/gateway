# Gentility gateway

Gentility's model gateway is a **stock [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
release** plus one native plugin, `gentility-usage`. This repo packages the
two as a signed **gateway bundle**, builds one for every upstream release, and
publishes it here as a GitHub release.

It is not a fork. Upstream's `linux_amd64` archive goes into each bundle
byte for byte, checked against upstream's `checksums.txt`. The plugin is
built against the same release through the plugin ABI.

## The plugin

`cliproxy-usage/` builds `gentility-usage.so`. Inside the proxy it:

- **meters** every completion and posts a usage record to the Gentility
  server, spooling to disk while the server is unreachable;
- keeps a **quota record** per Codex and Claude login:
  - the 5-hour and weekly windows with their reset times, the resets held
    and the credits;
  - fed by polling each login's usage endpoint every ten minutes (jittered,
    never faster than once a minute, backing off on 429, stopping on 401 or
    403 until the login changes), and by the quota headers on real replies;
  - published through the proxy's `quota.describe` / `quota.fetch`
    (`GET /v0/management/plugins/gentility-usage/quota?auth_index=…`), and
    for every login at once through the plugin's own management route,
    `GET /v0/management/gentility-usage/record` (from plugin 0.4.0). Both
    take the management key.

No bearer, refresh token, id token, account id or email reaches the record,
its file or the log; a test seeds fake ones to prove it.

From plugin 0.5.0 it also **scopes keys**. The prefix of a client key names
its harness (`contract/key_prefixes.json`); a request is served only by a
login of that harness: `claude` logins for `claude_` keys, `codex` logins
for `codex_` keys, openai-compatibility logins for `omp_` keys. Anything
else, including a key with no prefix, a login of another provider or a check
that cannot decide, gets a `key_scope` 403 before any upstream call. On the
Responses websocket upstream drops the 403's body and closes the socket.

The scope holds only while the plugin is loaded and not fused. Paths are in
upstream's `internal/pluginhost/` at v8.0.61:
- upstream keeps serving when the plugin fails to load or register, and has
  no option to require a plugin (`host.go:355-370` and `412-420`);
- a panic the host recovers in any of the plugin's adapters fuses the
  plugin until its file is replaced or removed, so in practice for the
  process's lifetime, and a fused plugin's interceptors are skipped
  (`adapters_usage_translation.go:97-106`, `adapters_interceptors.go:22-25`
  and `144-147`);
- the host takes an interceptor's error as a no-op
  (`adapters_interceptors.go:33-37`), so the check never returns one: it
  answers a refusal, and recovers its own panics into one.
- the host also skips interceptors when it holds no current plugin record
  (a failed record, a disable, an unload or shutdown, or the plugin gone in
  the middle of a request: `host.go:1001-1021`, and upstream's
  `sdk/api/handlers/handlers_interceptors.go:620`). Whether a hot-reload swap
  leaves a gap has not been checked.

So something must watch it: a check after each install that a `claude_` key
or the sentinel asking for a compat model gets the `key_scope` 403, and an
alert on the proxy's "pluginhost: plugin panic recovered" log line.

Two more paths, from upstream's `sdk/cliproxy/auth/` at v8.0.60:
- **The Antigravity credits fallback** (non-streaming,
  `conductor_execution.go:176-182` → `conductor_home.go:1492`) runs on another
  login without the hook. It is live only with
  `quota-exceeded.antigravity-credits` on and an Antigravity login present.
  Keep both off where the scope matters.
- **A streaming refusal** passes through the request-scoped error rules
  (`conductor_request_scoped_errors.go:187-217`). A rule with action
  `continue` or `continue-and-cooldown` that matches a 403 tries the next
  login. The hook runs again for that login, so this is no bypass, but keep
  such rules out of the config.

The routes that never reach the hook (Codex alpha search, live and realtime)
are listed in gentility-core's task T-375.

The prefix table is compiled into the plugin. A test holds it to
`contract/key_prefixes.json`; the file is not read at runtime.

## The contract test

`contract/` runs the real proxy release with the built plugin. It sends every
management call the Gentility server makes, exactly as `contract/calls.json`
lists them, plus one metered streamed completion. It sends every key class
to every login type, each behind a fake upstream, on every route a client can
reach a model by: chat and text completions, Claude's messages and
count_tokens, Responses (plain, streamed, compact, Codex's own path and the
two-turn websocket) and Gemini's generateContent and countTokens. It checks
that only a key's own harness is served and that no refused request reaches
an upstream.
It checks that the record route refuses a missing or wrong management key. A release that answers any
of those differently fails here, not in production.

## Bundles

`.github/workflows/release.yml` runs hourly, on every push to `main`, and by
hand. When upstream's latest release has no bundle yet, it:
1. builds one with `scripts/build`;
2. runs the plugin and contract tests;
3. signs the manifest in a separate job that only `main` can reach;
4. attests the bundle's build provenance;
5. publishes `gateway-<upstream>-<plugin>`.

When a build fails, nothing is published and an issue is opened. A plugin
change is published when its version (in `cliproxy-usage/main.go`) moves.

[docs/bundle-contract.md](docs/bundle-contract.md) describes:
- the manifest;
- the signature;
- the pinned key set (`keys.json`);
- the `gateway_install` command;
- the root helper that installs a bundle and rolls back by itself.

To verify a downloaded bundle:

```bash
mkdir b && tar -xf bundle.tar -C b
(cd bundle && go run . verify -dir ../b -keys ../keys.json)
gh attestation verify bundle.tar -R gentility-ai/gateway
```

## Licence

MIT. Upstream CLIProxyAPI is MIT too; its licence ships inside its archive.

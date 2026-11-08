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

## The contract test

`contract/` runs the real proxy release with the built plugin. It sends every
management call the Gentility server makes, exactly as `contract/calls.json`
lists them, plus one metered streamed completion, and checks that the record
route refuses a missing or wrong management key. A release that answers any
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

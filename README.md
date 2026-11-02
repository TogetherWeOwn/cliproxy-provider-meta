# Meta provider for CLIProxyAPI

Read-only Meta subscription quota cards for stock CLIProxyAPI. Built on
[`cliproxy-provider-kit`](https://github.com/TogetherWeOwn/cliproxy-provider-kit)
at `31d92c0` (v8.0.49 plugin ABI: answers RPC schema 10, refuses hosts older than schema 10)
and tested with the **real CLIProxyAPI v8.0.49 C-ABI loader** (ABI 1, RPC schema 10).
Requires CLIProxyAPI v8.0.49 or newer.

This is a **quota-only** plugin: it declares no auth, model or executor
capability, so the host keeps serving Meta inference itself (including the
core `meta` auths created by Meta OAuth login). The plugin only adds
subscription quota cards. There is **no Scheduler**, preemptive
quota-percentage avoidance, or automatic quota reset. Quota is display-only.

## How it works

Each poll POSTs an empty JSON object to the Meta key-envelope read
(`https://api.meta.ai/muse-code/key`) through the credential's proxy
(auth `proxy_url`, else the host's global proxy). The DCA token travels in
the `Authorization` header only. The response envelope is decoded for the
two subscription usage windows (`subs_usage.window`, `subs_usage.weekly`)
and the tier — nothing else is retained:

- 5-hour bucket: `1 - window.used_percent / 100`, reset from `window.resets_at`.
- Weekly bucket: `1 - weekly.used_percent / 100`, reset from `weekly.resets_at`.
- Tier chip: `subs_tier_name`, falling back to `subs_usage.tier`.
- `rotated` summary flag when the envelope key differs from the stored
  `api_key` (boolean only — no key is ever reported).

Successful polls are cached per auth for 30 minutes with single-flight
fetching; errors are never cached. Windows without a usable signal drop
from the card; a tier-only card means quota-blind, never fabricated.
Accounts with no subscription (`is_subs_active:false`, no tier, no
`subs_usage`) read as unknown — no windows, never fabricated headroom.

## Credential handling

- Only `type: "meta"` credentials with a `dca_token` matching `^dca:\S+$`
  (max 4096 characters) are polled. Anything else is rejected without a request.
- The DCA token is never logged, persisted or included in errors, and the
  normalized card never contains the envelope key, email or name.

## Auth files

No new auth files are needed: the plugin reads the `dca_token` from the
existing core `meta` auths (host metadata over object storage). Never put
real auth files in this repository.

## Provenance

See [provenance](docs/PROVENANCE.md) for where the envelope shape and the
request parameters come from.

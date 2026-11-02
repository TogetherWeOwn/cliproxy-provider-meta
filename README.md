# Meta provider for CLIProxyAPI

Full Meta (Muse) provider plugin for stock CLIProxyAPI: auth ownership,
namespaced models, request execution, and subscription quota cards. Built on
[`cliproxy-provider-kit`](https://github.com/TogetherWeOwn/cliproxy-provider-kit)
at `31d92c0` (v8.0.49 plugin ABI: answers RPC schema 10, refuses hosts older than schema 10)
and tested with the **real CLIProxyAPI v8.0.49 C-ABI loader** (ABI 1, RPC schema 10).
Requires CLIProxyAPI v8.0.49 or newer.

## What it owns

- **Auth** (`auth.go`): first-crack `ParseAuth` claim of the `meta-muse` key
  plus the legacy `meta` key (takeover without file renames; rollback is a
  persisted `type` string flip). Resolves API keys with attribute >
  metadata > storage precedence (`dca:` tokens never count as API keys),
  mints LLM keys from DCA tokens on refresh, and implements the stock
  device-code login flow (`StartLogin`/`PollLogin`).
- **Models** (`models.go`): static and per-auth models under the `meta-muse/`
  namespace.
- **Executor** (`executor.go`, `replay.go`, `prefilter.go`,
  `promptcachekey.go`, `sanitize.go`): serves `openai-response`, `claude`,
  `gemini` and `openai` client formats by translating to upstream Codex
  `/responses`. It carries the four Meta fork patches so they can be dropped
  from the fork: account-bound reasoning replay with a one-shot retry,
  per-session `prompt_cache_key`, the fail-closed reasoning sanitizer, and
  stream bootstrap peek with `response.subscription_usage` filtering.
- **Quota** (`quota.go`, `quota_provider.go`, `subscription.go`): converging
  subscription quota cards for both `meta` and `meta-muse` keys, fed passively
  by usage events the executor observes on inference traffic (plus the
  key-envelope poll). There is **no Scheduler**, preemptive quota-percentage
  avoidance, or automatic quota reset. Quota is display-only.

## How quota works

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

## Credential handling

- Only `type: "meta"` credentials with a `dca_token` matching `^dca:\S+$`
  (max 4096 characters) are polled. Anything else is rejected without a request.
- Raw executor HTTP requests are gated to `https` URLs on the credential's
  own base-URL host (plus the hardcoded Meta auth/mint hosts) before any
  bearer token is attached; client-supplied identity headers are dropped
  case-insensitively so per-request session isolation holds.
- The DCA token is persisted as the credential storage of record (the quota
  poll reads it back from storage) and is never logged or included in errors,
  and the normalized card never contains the envelope key, email or name.

## Auth files

No new auth files are needed for existing users: the plugin takes over the
existing core `meta` auths in place (host metadata over object storage).
Rollback is flipping the persisted `type` back to `"meta"`. Never put
real auth files in this repository.

## Provenance

See [provenance](docs/PROVENANCE.md) for where the envelope shape and the
request parameters come from.

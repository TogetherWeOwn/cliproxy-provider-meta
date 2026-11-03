# Provenance

Where the Meta envelope shape and request parameters come from. No account
data, tokens or keys are recorded here — only public protocol facts.

## Envelope read

- Endpoint `POST https://api.meta.ai/muse-code/key` with an empty JSON body,
  `Accept: application/json`, `Content-Type: application/json`,
  `Authorization: Bearer <dca-token>` and `X-Api-Version: 1.0.0`. This is the
  same key-envelope read the CLIProxyAPI core Meta executor mints with
  (`MintAPIKey` in `internal/auth/meta`), minus the mint: the quota poll
  sends `{}` so no new key is issued and nothing is rotated.
- Response envelope fields used: `subs_usage.window.used_percent`,
  `subs_usage.window.resets_at`, `subs_usage.weekly.used_percent`,
  `subs_usage.weekly.resets_at`, `subs_tier_name` (fallback
  `subs_usage.tier`), and `api_key` for the rotated comparison only.
  `used_percent` is capped at 100 before conversion; unix `resets_at`
  values render as RFC 3339, anything else renders no reset time.
- The `^dca:\S+$` token shape and 4096-character cap match the connector's
  published `DCA_TOKEN_PATTERN` and `DCA_TOKEN_MAX_LENGTH`.

## Subscription usage signals

- The `subscription_usage` server-sent event (5-hour `window` plus `weekly`
  usage with `used_percent` and `resets_at`) is the same signal the core
  Meta executor surfaces as `X-Meta-*` response headers. This plugin reads
  it directly from the envelope instead, so it works on stock CLIProxy
  without any core patches.
- Accounts without a subscription return no `subs_usage` and no tier
  (`is_subs_active:false`, `require_payment:true` on the operator's
  mint-test accounts): the provider reports unknown (no windows, an
  uncached error) rather than fabricating headroom. Neither
  `is_subs_active` nor `require_payment` is decoded — only the absence of
  a usable window plus tier decides unknown.

## Later work

- v2 adds the executor that retires the carried Meta core patches in this
  same plugin, with auth ownership (single `meta-muse` identifier; the core
  `meta` key is taken over through the credential-provider bridge) and
  namespaced models. v1 was quota-only on purpose so quota cards landed
  without taking over the core `meta` key.

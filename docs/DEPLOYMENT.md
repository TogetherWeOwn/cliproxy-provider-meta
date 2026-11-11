# Plugin deployment runbook — detached swap controller

Canonical procedure for hot-swapping a CLIProxy provider plugin on the
production host. It replaces the ad-hoc per-install copies of the detached
deploy controller: derive each per-install controller (`deploy.sh`) from
this document and copy the functions in §“Controller functions” verbatim.
This applies to every CLIProxy provider plugin installed through this
controller — Meta, Hopper, and any other provider plugin — because the
constraints below come from the host loader, not from one plugin.

Host baseline: the behaviour in §1 was reproduced on the stock CLIProxy
image used for isolated probes (v8.0.63). Re-verify the same rules against
the host image in use at install time; do not assume a newer or older host
still behaves differently.

## 1. Hot rollback to a previously-loaded build is impossible

CLIProxy loads plugin images from content-addressed shadow copies and never
unloads them (`internal/pluginhost/shadow.go:21-25`,
`internal/pluginhost/loader_unix.go:188`). A build the host process has
already loaded is therefore resident for the life of the process: a re-load
of the same bytes reuses the resident image and `cliproxy_plugin_init`
returns 1. The host keeps the outgoing plugin when a re-load fails, so a
failed hot rollback is *safe* — but it is not a rollback.

Rules, for every plugin that uses this controller:

- **Never attempt or promise a hot rollback to a build this host process
  already ran.** Every rollback plan is file restore + container restart
  (§2). Any install plan, probe script, or release note that assumes a hot
  reload back is wrong and must be corrected before use.
- **One exception:** if the new build *never loaded* in this process (the
  outgoing build is still the resident one), restoring the outgoing bytes
  to disk needs no restart — verify with the outgoing version loaded and
  route probes green. The controller must record which case it is in; it
  may not guess.
- The outgoing plugin for the *forward* swap may hot-load freely: the
  incoming build has never been resident, so the file swap + one
  config-hash reload trigger (see §4) is the correct forward path.

## 2. Restart rollback: procedure and blast radius

The way back once the new build has loaded:

1. Restore the recorded outgoing bytes to the plugin path and verify the
   sha256 matches the pre-install record.
2. Restore `config.yaml` to the recorded original bytes (byte-identical).
3. Restart the CLIProxy container (`docker compose restart` for the CLIProxy
   service, falling back to `docker restart`).
4. Verify in the fresh process: `plugin loaded` and `plugin registered`
   lines name the *outgoing* version, the post-restart `/v1/models` list
   matches the retained pre-install list (§5), and one stream probe passes.

**Blast radius — state it in every rollback plan:** a container restart
cancels in-flight work across **all** providers served by that host
process, not only the plugin being rolled back. Count
`499 / operation canceled` failures across the board after any restart,
not just for the swapped plugin. A restart rollback is a host-wide
interruption; prefer the §1 exception (no restart) whenever it applies.

## 3. Swap timing: first confirmed lowest in-flight reading

The forward swap waits for in-flight requests on the outgoing plugin. The
old rule — wait out a fixed bound (900 s + 300 s grace) and then swap at
whatever count the bound lands on — is retired. It once sampled a low of 3
in-flight runs, waited out the bound, and swapped at 8, canceling 6 runs
under a non-quiescing outgoing plugin.

The rule now: **swap on the first confirmed lowest in-flight reading.**

- Sample the in-flight basis every 15 s and log every sample.
- Track the running minimum. Swap when the current reading is at the
  running minimum and the following sample does not rise above it (two
  consecutive samples at the floor), or immediately at zero in flight.
- Never swap at a count above the running minimum, and never wait out a
  fixed bound hoping the count improves further. A hard wall-clock cap
  remains only as a last resort and must be logged as such when taken.
- Record the swap basis in the state log: count at swap, minimum seen, and
  elapsed time.
- Why the count matters: if the *outgoing* plugin does not implement
  `plugin.quiesce`, a swap unloads it with in-flight requests still running,
  and the in-flight count at swap is an upper bound on canceled runs. If
  the outgoing plugin does quiesce, in-flight requests drain and the count
  only affects drain duration — but the controller cannot know the
  outgoing build's quiesce behaviour unless it records it, so the
  first-confirmed-minimum rule applies either way.

An exact global-minimum swap is impossible online (the global minimum is
only known in hindsight); the rule guarantees the swap lands at the first
confirmed floor the measurement reaches, never above it.

## 4. Forward swap mechanics (unchanged, restated for completeness)

- A plugin file swap alone is never picked up: the watcher runs pluginhost
  `ApplyConfig` only when `config.yaml` sha256 changes
  (`internal/watcher/config_reload.go:54-80`,
  `internal/pluginhost/host.go:221`). Append one comment line to fire one
  reload, wait for `plugin registered` naming the new version, then write
  the original config bytes back so `config.yaml` ends byte-identical.
- Detached and self-testing: one session leader, a watchdog that kills the
  main session and rolls back if the run exceeds its bound, and a rollback
  lock so watchdog and foreground cannot roll back twice.
- Auto-rollback on any red signal (failed load, missing required models,
  unhealthy route, auth-file set or disabled flags changed, config sha
  changed, panic in container logs).

## 5. Evidence: retain full model lists

- In preflight, retain the **full** `/v1/models` response (every id, not
  just the required-present check) as `models.before.json` in the evidence
  dir. After the swap, retain the full response again as
  `models.after.json`.
- Diff the two lists and name every added and removed id in the install
  record. A count delta without named ids is not evidence — it leaves the
  change unexplained (a 575 → 572 post-install count delta with no retained
  preflight list cannot be named, and the next install must re-derive it).
- Retain the in-flight sample log and the `499` count taken after the swap.

## 6. Restart-rollback isolated probe

Before any install relies on §2, exercise it end-to-end in an isolated
probe on a throwaway container from the prod image — scratch API key,
copied API-key logins only (no refresh tokens, so no token-rotation risk),
torn down afterwards with zero containers left:

1. Boot the probe on the outgoing build; record its model list.
2. Forward-swap to the new build through the §4 path; verify load and
   registration.
3. Restore the outgoing bytes to disk and **restart the probe container**.
4. In the fresh process, verify the outgoing version loaded and
   registered, the post-restart model list matches the outgoing list, and
   one stream probe passes at 200.
5. Record the evidence (hashes, loaded/registered lines, model lists,
   stream result, teardown confirmation) in the probe evidence dir and
   cite it from the install card.

## Controller functions

Copy these into each per-install `deploy.sh`, parameterised by that
install's plugin id, versions, file names, and hashes. Full per-install
scripts remain frozen evidence; this document is the living spec.

```sh
# swap on the first confirmed lowest in-flight reading (sampled every 15 s);
# never wait out a fixed bound, never swap above the running minimum.
wait_low_inflight() {  # $1 = in-flight count command (plugin-scoped basis)
  local t0 n min=999 floor=0; t0=$(date -u +%s)
  while :; do
    n=$("$1"); [ -n "$n" ] || n=999
    echo "$(date -u +%FT%TZ) in_flight=$n" >> "$SD/inflight.log"
    [ "$n" -lt "$min" ] && min=$n
    [ "$n" -eq 0 ] && break
    if [ "$n" -le "$min" ]; then floor=$((floor + 1)); else floor=0; fi
    [ "$floor" -ge 2 ] && break
    [ $(( $(date -u +%s) - t0 )) -ge 1200 ] && { log "hard cap: swapping at running minimum"; break; }
    sleep 15
  done
  log "swap in-flight basis: in_flight=$n (min seen $min over $(( $(date -u +%s) - t0 ))s)"
}

# retain the FULL preflight payload before any change; the post-install
# list is written by the verify step to the same call below.
capture_models() {  # $1 = output file (full response, all ids)
  probe models-all > "$1" 2>/dev/null
}

# rollback is file restore + container restart once the new build has
# loaded; only the never-loaded case verifies without a restart (see §1).
# A restart cancels in-flight work across ALL providers — state that
# blast radius in every rollback plan.
rollback() {
  exec 9>"$SD/rollback.lock"; flock -w 600 9 || { log "rollback lock timeout"; state rollback-FAILED; return 1; }
  grep -qx rolledback "$SD/state" 2>/dev/null && return 0
  state rolling-back
  rm -f "$SO_DST"; [ -e "$SO_OLD" ] || install -m 755 "$SD/rollback.so.orig" "$SO_OLD"
  cmp -s "$SD/config.yaml.orig" "$S/config.yaml" || cp "$SD/config.yaml.orig" "$S/config.yaml"
  log "plugin restored ($(sha "$SO_OLD"))"
  if new_build_never_loaded "$RB" && version_loaded "$OLD_VERSION" "$RB" && route_ok; then
    state rolledback; log "rollback verified without restart (new build never loaded; outgoing still active)"; return 0
  fi
  log "restarting container: this cancels in-flight work across all providers"
  (cd "$S" && timeout 150 docker compose restart "$SVC") || docker restart "$C" || true
  for _ in $(seq 1 40); do sleep 3; models_ok "$REQUIRED_MODELS" >/dev/null 2>&1 && break; done
  if version_loaded "$OLD_VERSION" "$RB" && models_match "$SD/models.before.json" "$SD/models.after.json" && stream_ok; then
    state rolledback; log "rollback verified after restart ($OLD_VERSION loaded)"; return 0
  fi
  state rollback-FAILED; return 1
}
```

# Source-bound replay diagnostics

The producer contract is `replay-event-v1`; the surrounding integration contract
is `replay-evidence-v1`. This plugin produces decisions, **not replay acceptance**.
It does not implement HTTP ingress, a probe, container inspection, collection,
recovery, or a production rollout. Those need separately reviewed sources and
artifact identities. This change does not install anything or replace any
existing release pin. Version 0.2.4 identifies new source/package bytes; it is
not the historical 0.2.2 artifact and is not published by these build tools.

## Existing metadata ABI

The existing SDK/RPC `pluginapi.ExecutorRequest.Metadata` extension bag carries:

```json
{
  "requested_model": "public-model-alias(high)",
  "cpa_recovery_v1": {
    "attempt_id": "attempt-1",
    "probe_id": "probe-1",
    "request_id": "request-2",
    "parent_request_id": "request-1",
    "turn": 2
  }
}
```

- `cpa_recovery_v1` must be an object with **exactly these five keys**. All four
  IDs are strings, 1–128 ASCII bytes matching
  `[A-Za-z0-9][A-Za-z0-9._:-]*`, except turn 1's parent must be the empty string.
  Turn is the numeric integer 1 or 2, not a string/bool. Turn 2 requires a
  nonempty parent different from its own request ID. RPC-decoded exact float64
  1/2, native Go int 1/2, and json.Number `1`/`2` are supported.
- No identifiers are read from credential storage, `AuthMetadata`, headers,
  timestamps, environment/container names, or another request. No authorization
  or model entitlement is inferred from any diagnostic value.
- `Metadata.requested_model`, when supplied, is the exact host-recorded pre-alias
  model. If absent, the original request's string `model`, then payload's string
  `model`, then executor `Model` is used. Explicit malformed values never fall
  back to a plausible model. Model strings are 1–256 ASCII bytes starting with
  an alphanumeric and continuing with alphanumerics or `._:/()+-`.
- `resolved_model` is the actual `baseModel` written into the prepared upstream
  body, not a name supplied in diagnostic metadata. Model names are never
  truncated or rewritten for evidence. A collector must compare the exact
  requested/resolved mapping with the probe/attempt manifest.
- Missing/malformed/unbounded/unknown-field correlation suppresses the correlated
  record; inference and its existing auth/model/replay behavior continue unchanged.
  A missing record is **not** successful evidence. Values are rejected rather
  than sanitized into another identity. Surrounding unrelated metadata is not
  serialized into events.
- All case variants of `X-CPA-Recovery-*`, including unknown future names, are
  removed at the provider HTTP header boundary on both execution lanes and raw
  `HttpRequest`. This plugin does not ingest those headers: a separately reviewed
  ingress bridge must map the five explicit carriers to request metadata. There
  is no SDK/ABI change and no assumption that arbitrary headers already map.

## Production sites and counter semantics

`makePlugin` creates a producer for each plugin runtime and first queues a
`replay-producer-startup-v1` marker. `Execute` and `ExecuteStream` queue a
`replay-event-v1` immediately before each upstream call, after preparation and
credential resolution. Token counting and rejected/failed preparation do not
emit upstream decision records. HTTP/transport errors can follow a decision;
**the event never claims that the HTTP call or tool round trip succeeded**.

- `kept`: number of reasoning `encrypted_content` envelopes actually surviving
  the translated-body/native-shape sanitizer into that upstream attempt. A
  prefilter approval later removed by translation/sanitization is not a hit.
- `dropped_foreign`: actual account-policy drops in that attempt, including tags
  rejected while replay is disabled. Initial foreign drops remain visible even
  when a later retry has zero drops. No account identifier is logged.
- `dropped_untagged`: actual unattributed envelope drops in that attempt.
- `replay_enabled`: the policy's effective enabled state, not a collector guess.
- `replay_attempt`: 0 initially; 1 for the existing once-only retry without all
  reasoning. That retry retains all request/turn/model IDs, gets a **new event
  identity**, and records its own zero kept count and actual drop counts. Host
  retries/re-executions also get new event identities, never reused IDs.

All counters are JSON integer tokens, nonnegative. Zero-count decisions are
emitted for valid diagnostics, including turn 1. Diagnostic data does not alter
foreign reasoning/tool isolation, request bodies, prompt-cache keys, credential
selection/refresh, entitlement, upstream errors, or retry eligibility. The
legacy uncorrelated summary remains counters-only: it has no auth filename/ID,
model string, or arbitrary metadata and cannot be used as correlated evidence.

## Identity and raw log transport

Each runtime uses a crypto-random UUIDv4 `producer_instance_id`; every record's
`event_id` is `<producer_instance_id>:<sequence>` (canonical decimal uint64).
The startup sequence is 1 and subsequent decisions increase under one mutex.
No wrap is allowed; sequence exhaustion stops evidence emission, not inference.
Identical copies in stdout/file capture retain byte-identical identities and
content; distinct decisions and retries do not. The collector deduplicates only
identical copies and rejects conflicting duplicates.

Records are standalone ASCII NDJSON admitted nonblockingly to a **128-record
FIFO**, with one dedicated writer per runtime and at most one in-flight record.
Each record is bounded (<4096 bytes); the queue, in-flight record and reserved
terminal failure record bound retained output to less than 520 KiB per runtime.
Only admission/identity assignment holds the producer mutex. No startup,
execution, retry or shutdown caller waits for a sink write. This does not make
other host/logrus output nonblocking: those pre-existing logs are independent.
A permanently stalled sink can retain the single writer until process exit;
there is no per-request worker, I/O retry loop or growing backlog. Shutdown seals
admission but does not wait on I/O; pending output is not guaranteed at exit.

The writer preserves startup-before-decisions and admission order, one write per
record to the process stdout descriptor, independently of logrus formatting
or level. The Go writer uses a raw write syscall, not `os.Stdout.Write`'s
process-terminating descriptor-1 EPIPE policy. A closed pipe returns a sink error
and permanently disables evidence; it does not raise a software SIGPIPE, change
the host's signal disposition, close/duplicate stdout, or retry the record.
Other host log lines may share stdout and are not producer records; their signal
and blocking behavior is not changed.
The plugin opens **no diagnostic file**, declares `sources: ["stdout"]` and
`file_logging: "not_configured"` at startup, and has no file-output configuration.
If the host duplicates stdout into a file, that is an externally declared
collector source, not a second producer.

Queue saturation permanently seals admission for this runtime: no later green
decision or retry may replace a lost record. If the sink resumes, the writer
finishes the admitted prefix then writes a terminal `replay-producer-failure-v1`
record with reason `queue_overflow` and the next contiguous identity. Sequence
exhaustion reserves the final identity for the same failure type with reason
`sequence_exhausted`; encoding failure uses reason `encoding`. A collector must
reject **all acceptance from that producer** on any failure marker, not merely
the last request. A partial/failed sink write terminates the writer without
retrying ambiguous bytes or attempting a healthy tail. A failure marker itself
is best effort, not guaranteed delivery.

Admission is not delivery. An HTTP response, an empty read, a quiet interval, or
the absence of a failure marker **does not prove the queue drained**. Required
collection must account for delayed initial/retry records and complete source
coverage; any missing expected decision, truncation, timeout, overflow, or
unproven drain/coverage fails acceptance closed. Do not accept a finite prefix
of a stalled producer's output as complete. The host/collector integration must
establish its drain/completeness boundary; this plugin provides no synchronous
flush RPC, health attestation or production acceptance. Unknown record schemas
must fail closed, not be silently discarded. These limits also apply at process
exit. Inference remains available even when its diagnostics are unusable.

Startup also includes the plugin version and package-builder-stamped
`source_revision` and `source_modified` (unstamped is conservative: empty
revision and modified true). The builder reads Git from its explicit worktree
root, **requires its canonical physical path to equal Git's worktree root before
creating output or building**, and disables automatic Go VCS discovery, which
can otherwise report an enclosing checkout's unrelated revision. A tracked
plugin subdirectory inside another repository is refused; standalone repositories,
managed worktrees and symlinked paths to the same worktree remain supported. It writes
`build-provenance.json` alongside the archive/checksums, binding source revision,
HEAD tree, modified status, archive hash, extracted-library hash and schema hash.
Harness-only `.claude` configuration is excluded from the modified check; all
other nonignored tracked/untracked files count. A dirty build is component
output, not a clean source artifact. Both pre-build and post-build Git status
commands must succeed, including for an initially dirty build. An unreadable or
corrupt index aborts before build at the first check, or before publishing any
archive/checksums/provenance at the second check. Clean source must remain clean
through the build. These are correspondence hints, **not a signed build attestation or
reproducible rebuild**. Bind the actual source archive and candidate image hashes
in the integration package too. A version string alone proves no identity.
Test examples are unstamped and deliberately not acceptable as source-bound
production evidence.

A UUID/hostname/short container label is not container provenance. The collector
must bind the startup marker through real daemon/container inspection and
opened source handles to the immutable full container ID and exact candidate /
plugin artifacts. It must retain raw digests, source offsets/cursors/rotation
identity, coverage of all required sources (including genuinely empty ones),
and reject truncation, conflicts, overflow, unreadable sources or cursor gaps.
It may add observed provenance, never fabricate any request/model/turn/attempt
field. No timestamp-window relabeling is permitted.

## Schema, examples, validation

- [Exact JSON Schema](replay-event-v1.schema.json), draft 2020-12. The root
  validates startup, decision or terminal failure records; `$defs.correlation` describes the
  metadata object. Cross-field constraints also require parent != self on turn
  2, event ID prefix == producer instance, canonical nonwrapping sequence, and
  startup-before-decisions. Consumers must enforce these, not just JSON types.
- [Actual producer output](replay-events.example.ndjson) was captured by
  `TestReplayEvidenceTwoTurnToolLinkage/false`, running the real plugin
  `Execute`/preparation/response-tag/replay path with a synthetic HTTP client.
  It is not manually relabeled, a live request, or production acceptance.
- CI generates fresh producer output during the race suite and validates both
  fresh output and the saved example, plus strict-type/injection schema
  negatives. The example checker is a schema/identity check, **not a collector**.

```sh
# Use a new output path; do not overwrite retained evidence.
META_REPLAY_EXAMPLES_OUT=/tmp/new-replay-events.ndjson \
  go test -race -count=1 -run '^TestReplayEvidenceTwoTurnToolLinkage$' .
# Dev-only python3-jsonschema prerequisite; no runtime Python dependency.
python3 -I scripts/check-replay-examples.py \
  docs/replay-event-v1.schema.json /tmp/new-replay-events.ndjson
```

A real probe must separately prove both successful HTTP turns and actual tool-use
linkage. For its exact second-turn request/attempt/probe/model/container mapping,
require kept >= 1 and dropped_foreign == 0, and reject **any** correlated foreign
drop, conflicting duplicate, incomplete source collection or wrong mapping. A
later green retry/unrelated event cannot override a correlated bad event.
Stale/concurrent/wrong-model/wrong-turn IDs remain exactly as supplied (or are
rejected when malformed); this producer does not claim freshness/ownership from
them. Collector rejection of those cases, real ingress/RPC integration, container
mapping, rotation/failure handling, and host recovery/timing remain integration
prerequisites. No production probe or production acceptance is claimed here.

REA angle: this uses the inspected existing SDK extension bag, per-request replay
policy and final sanitizer, rather than a new ABI or opaque log reconstruction.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This file ports the account-bound Muse reasoning replay policy from stock
// CLIProxy (internal/runtime/executor/meta_reasoning_replay.go). The native
// policy runs on the translated Codex body because the fork's translator keeps
// the proxy's meta# provenance tag intact; the pinned stock translators this
// plugin builds on drop the tag, so this port applies the same account policy
// to the client-format payload BEFORE translation (see prefilter.go) and tags
// egress envelopes afterwards. The decision, retry and telemetry semantics
// below are otherwise unchanged.
//
// Muse returns reasoning as an opaque encrypted_content envelope, and Meta binds
// each envelope to the account that issued it: replaying one through any other
// account is rejected with 400 "reasoning encrypted_content was not issued to
// this caller". A client such as Claude Code resends the whole history on every
// turn, and that history mixes envelopes from every pool account that served an
// earlier turn, so forwarding it verbatim fails in production even though a
// single-account test passes.
//
// The feature therefore carries provenance end to end:
//
//   - Responses leaving this executor have each reasoning envelope wrapped as
//     meta#<account-key>#<envelope>, whether the client reads it as an
//     Anthropic thinking signature or as a Responses encrypted_content,
//     streaming or not.
//   - Requests entering this executor keep an envelope only when its tag names
//     the credential selected for this very request. Everything else (another
//     account's tag, an untagged legacy envelope) is dropped. With the feature
//     off, untagged envelopes are forwarded as is.
//   - If Meta still rejects a replayed envelope, the request is retried once
//     with every envelope removed, so the caller never sees that 400.

// metaReasoningNotIssuedMessage is the stable fragment of the 400 Meta returns for
// an envelope replayed through an account that did not issue it.
const metaReasoningNotIssuedMessage = "not issued to this caller"

// metaReasoningAccountDomain separates the replay account key from every other
// hash in the proxy (notably the prompt cache key): neither can collide with
// the other.
const metaReasoningAccountDomain = "cliproxy/meta-reasoning-account/v1"

// metaReasoningReplayStats counts what happened to the reasoning envelopes of
// one request. The executor logs it so the replay hit rate can be measured.
type metaReasoningReplayStats struct {
	kept            int
	droppedForeign  int
	droppedUntagged int
}

// metaReasoningReplay is the per-request replay policy.
type metaReasoningReplay struct {
	// enabled is true when the feature is on for this model and the selected
	// credential can be identified.
	enabled bool
	// accountKey names the credential selected for this request.
	accountKey string
	// baseURL is the selected credential's upstream, which decides how strictly
	// the native sanitizer gates Meta envelopes.
	baseURL string
	stats   *metaReasoningReplayStats
	// approved carries the stripped native envelopes the client-format prefilter
	// already attributed to this account. The prefilter strips the tag before
	// the stock translators (which would drop the tagged value outright), so by
	// the time the translated-body barrier runs, an approved envelope looks
	// untagged. The barrier trusts exactly these values; every other untagged
	// envelope is still dropped. Entries are envelope values, never keys, and
	// live only for this request: the one-shot retry builds a fresh policy
	// without them.
	approved map[string]struct{}
}

// newMetaReasoningReplay builds the per-request replay policy. requestModel is
// the host-supplied model (suffix-free, possibly namespaced), baseModel the
// upstream model name, originalPayload the client-format request (used to
// recover the requested name for canary matching), metadata the merged
// executor metadata, and authID the selected credential identity.
func newMetaReasoningReplay(cfg metaReasoningReplayConfig, requestModel, baseModel string, originalPayload []byte, metadata map[string]any, authID, baseURL string) metaReasoningReplay {
	replay := metaReasoningReplay{baseURL: baseURL, stats: &metaReasoningReplayStats{}, approved: map[string]struct{}{}}
	if !cfg.appliesToModel(replayModelNames(requestModel, baseModel, originalPayload, metadata)...) {
		return replay
	}
	replay.accountKey = metaReasoningAccountKey(authID)
	replay.enabled = replay.accountKey != ""
	return replay
}

// replayModelNames collects the candidate names the replay canary matches
// against: the host-supplied model, the upstream model, the client's own model
// field, and the host-recorded requested model. Empty candidates are dropped.
func replayModelNames(requestModel, baseModel string, originalPayload []byte, metadata map[string]any) []string {
	var out []string
	add := func(name string) {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	add(requestModel)
	add(stripMetaMuseNamespace(requestModel))
	add(parseMetaReasoningModelSuffix(requestModel))
	add(baseModel)
	if len(originalPayload) > 0 {
		add(gjson.GetBytes(originalPayload, "model").String())
	}
	if metadata != nil {
		if name, _ := metadata["requested_model"].(string); strings.TrimSpace(name) != "" {
			add(name)
		}
	}
	return out
}

// stripMetaMuseNamespace removes this provider's model namespace prefix, so
// canary patterns written against the upstream model name keep matching.
func stripMetaMuseNamespace(model string) string {
	if rest, ok := strings.CutPrefix(model, metaMuseNamespace+"/"); ok {
		return rest
	}
	return model
}

// parseMetaReasoningModelSuffix mirrors the trailing-parenthesis suffix the
// host strips before the plugin sees the model (thinking.ParseSuffix), so a
// suffixed requested name still matches canary patterns on the base name.
// Only the parentheses form is handled; anything else is returned unchanged.
func parseMetaReasoningModelSuffix(model string) string {
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen < 0 || !strings.HasSuffix(model, ")") {
		return model
	}
	return model[:lastOpen]
}

// metaReasoningAccountKey derives a stable, pseudonymous key for the credential.
// It has to survive token re-minting and restarts, so it is derived from the
// credential's identity (auth ID), never from the short-lived API key. A
// credential without an ID gets no key, which leaves replay off for it.
func metaReasoningAccountKey(authID string) string {
	id := strings.TrimSpace(authID)
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(metaReasoningAccountDomain + "\x00" + id))
	return hex.EncodeToString(sum[:8])
}

// metaReasoningReplayDecision is what the account policy decides for one
// reasoning item's encrypted_content.
type metaReasoningReplayDecision struct {
	// replay is the encrypted_content to forward. Used only when drop is false.
	replay string
	// drop removes the encrypted_content; reason explains why.
	drop   bool
	reason string
}

// approve records a stripped native envelope the prefilter attributed to this
// account, so the translated-body barrier (which sees it untagged) keeps it.
// The map is shared by value copies within one request; the one-shot retry
// builds a fresh policy without approvals.
func (p metaReasoningReplay) approve(envelope string) {
	if envelope == "" || p.approved == nil {
		return
	}
	p.approved[envelope] = struct{}{}
}

// isApproved reports whether the prefilter attributed this untagged envelope
// to the selected account.
func (p metaReasoningReplay) isApproved(envelope string) bool {
	if envelope == "" || p.approved == nil {
		return false
	}
	_, ok := p.approved[envelope]
	return ok
}

// decide applies the account policy to one string encrypted_content.
func (p metaReasoningReplay) decide(raw string) metaReasoningReplayDecision {
	accountKey, envelope, tagged := ParseMetaReasoningTag(raw)
	if !p.enabled {
		if tagged {
			// The feature was on earlier and is off now: nothing may reach Meta
			// that did not reach it before the feature existed.
			p.stats.droppedForeign++
			return metaReasoningReplayDecision{drop: true, reason: "Muse reasoning replay is disabled"}
		}
		// Without provenance, an untagged Muse envelope is forwarded as is (upstream
		// behaviour, still subject to the native-only shape check). It may belong to
		// another pool account, so it counts as kept: if Meta rejects it, the request
		// is retried once without reasoning.
		if IsValidMetaReasoningSignature(raw) {
			p.stats.kept++
		}
		return metaReasoningReplayDecision{replay: raw}
	}
	switch {
	case !tagged:
		p.stats.droppedUntagged++
		return metaReasoningReplayDecision{drop: true, reason: "untagged Muse reasoning envelope has no provable account"}
	case p.accountKey == "" || accountKey != p.accountKey:
		p.stats.droppedForeign++
		return metaReasoningReplayDecision{drop: true, reason: "Muse reasoning envelope was issued by another account"}
	default:
		p.stats.kept++
		return metaReasoningReplayDecision{replay: envelope}
	}
}

// withoutReplay is the policy for the single retry: it is enabled but names no
// account, so every envelope is dropped.
func (p metaReasoningReplay) withoutReplay() metaReasoningReplay {
	return metaReasoningReplay{enabled: true, baseURL: p.baseURL, stats: &metaReasoningReplayStats{}}
}

// shouldTag reports whether responses must carry the provenance tag.
func (p metaReasoningReplay) shouldTag() bool {
	return p.enabled && p.accountKey != ""
}

// canRetryWithoutReplay reports whether this attempt forwarded at least one
// envelope, making the one-shot retry worthwhile. Whether the retry already ran
// is tracked by the prepared request, not the policy.
func (p metaReasoningReplay) canRetryWithoutReplay() bool {
	return p.stats != nil && p.stats.kept > 0
}

// retryWithoutReplay decides whether a rejected request should be sent once more
// with every envelope removed, and prepares that body. It only fires when
// envelopes were actually forwarded and Meta rejected them as foreign.
func (p preparedMetaRequest) retryWithoutReplay(statusCode int, errBody []byte) (*preparedMetaRequest, bool) {
	if p.replayRetried || !p.replay.canRetryWithoutReplay() || !isMetaReasoningNotIssued(statusCode, errBody) {
		return nil, false
	}
	next := p
	next.replayRetried = true
	next.replay = p.replay.withoutReplay()
	next.body = next.replay.sanitizeCodexRequest(next.body)
	log.Warnf("meta-muse executor: meta reasoning replay: Meta rejected %d replayed reasoning envelope(s) as not issued to this caller; retrying once without reasoning", p.replay.stats.kept)
	return &next, true
}

// sanitizeCodexRequest applies the account policy to a translated Codex-format
// request body and then the native-only Meta sanitizer, which still strips
// anything that is not a recognized Meta envelope (fail-closed). A reasoning
// item that loses its envelope and has no summary or content of its own is
// removed entirely.
func (p metaReasoningReplay) sanitizeCodexRequest(body []byte) []byte {
	body = p.applyAccountPolicy(body)
	return sanitizeMetaReasoningEnvelopes(body, p.baseURL)
}

// applyAccountPolicy applies the keep-or-drop decision to every reasoning item
// of a Codex-format input array.
func (p metaReasoningReplay) applyAccountPolicy(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	items := input.Array()
	rebuilt := make([]string, 0, len(items))
	changed := false
	for index, item := range items {
		encrypted := item.Get("encrypted_content")
		if item.Get("type").String() != "reasoning" || encrypted.Type != gjson.String {
			rebuilt = append(rebuilt, item.Raw)
			continue
		}
		if _, _, tagged := ParseMetaReasoningTag(encrypted.String()); !tagged && p.isApproved(encrypted.String()) {
			// The prefilter already attributed this envelope to this account
			// and stripped its tag before translation. Keep it without
			// restating stats; the native sanitizer still shape-checks it.
			rebuilt = append(rebuilt, item.Raw)
			continue
		}
		decision := p.decide(encrypted.String())
		if !decision.drop {
			if decision.replay == encrypted.String() {
				rebuilt = append(rebuilt, item.Raw)
				continue
			}
			next, errSet := sjson.Set(item.Raw, "encrypted_content", decision.replay)
			if errSet == nil {
				rebuilt = append(rebuilt, next)
				changed = true
				continue
			}
			// Forwarding the tagged value would send Meta a string it never issued,
			// so fall back to dropping it like any other unusable envelope.
			decision = metaReasoningReplayDecision{drop: true, reason: "failed to strip provenance tag: " + errSet.Error()}
		}
		changed = true
		log.Debugf("meta-muse executor: dropped reasoning encrypted_content at input[%d] reason=%s", index, decision.reason)
		if metaReasoningItemIsEmpty(item) {
			continue
		}
		next, errDelete := sjson.Delete(item.Raw, "encrypted_content")
		if errDelete != nil {
			continue
		}
		rebuilt = append(rebuilt, next)
	}
	if !changed {
		return body
	}
	out, errSet := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(rebuilt, ",")+"]"))
	if errSet != nil {
		log.Debugf("meta-muse executor: failed to rebuild input after reasoning replay policy: %v", errSet)
		return body
	}
	return out
}

// metaReasoningItemIsEmpty reports whether a reasoning item carries no summary
// and no content, so that nothing is left of it once encrypted_content is gone.
func metaReasoningItemIsEmpty(item gjson.Result) bool {
	if !openaiResponsesReasoningSummaryIsEmpty(item.Get("summary")) {
		return false
	}
	content := item.Get("content")
	return !content.Exists() || content.Type == gjson.Null || (content.IsArray() && len(content.Array()) == 0)
}

// tagResponseEvent wraps the reasoning envelopes in one upstream Codex event
// (or a bare non-streaming response object) with this account's provenance tag.
func (p metaReasoningReplay) tagResponseEvent(eventData []byte) []byte {
	if !p.shouldTag() || len(eventData) == 0 {
		return eventData
	}
	switch gjson.GetBytes(eventData, "type").String() {
	case "response.output_item.added", "response.output_item.done":
		return p.tagReasoningItem(eventData, "item")
	case "response.completed", "response.incomplete", "response.failed":
		return p.tagOutputItems(eventData, "response.output")
	case "":
		return p.tagOutputItems(eventData, "output")
	}
	return eventData
}

func (p metaReasoningReplay) tagOutputItems(data []byte, outputPath string) []byte {
	output := gjson.GetBytes(data, outputPath)
	if !output.IsArray() {
		return data
	}
	for index := range output.Array() {
		data = p.tagReasoningItem(data, outputPath+"."+strconv.Itoa(index))
	}
	return data
}

func (p metaReasoningReplay) tagReasoningItem(data []byte, itemPath string) []byte {
	item := gjson.GetBytes(data, itemPath)
	if item.Get("type").String() != "reasoning" {
		return data
	}
	encrypted := item.Get("encrypted_content")
	if encrypted.Type != gjson.String || encrypted.String() == "" {
		return data
	}
	tagged := TagMetaReasoning(p.accountKey, encrypted.String())
	if tagged == encrypted.String() {
		return data
	}
	out, errSet := sjson.SetBytes(data, itemPath+".encrypted_content", tagged)
	if errSet != nil {
		// An untagged envelope is dropped on replay, so failing to tag only costs the
		// replay; it must never fail the response.
		return data
	}
	return out
}

// recordRequest logs what happened to the reasoning envelopes of one request, so the
// replay hit rate (kept / (kept + dropped_foreign)) can be measured from the logs.
func (p metaReasoningReplay) recordRequest(model, authID string) {
	if p.stats == nil {
		return
	}
	s := p.stats
	if s.kept+s.droppedForeign+s.droppedUntagged == 0 {
		return
	}
	log.Infof("meta-muse executor: meta reasoning replay: model=%s auth=%s kept=%d dropped_foreign=%d dropped_untagged=%d", model, authID, s.kept, s.droppedForeign, s.droppedUntagged)
}

// isMetaReasoningNotIssued reports whether an upstream error is Meta rejecting an
// envelope replayed through the wrong account.
func isMetaReasoningNotIssued(statusCode int, body []byte) bool {
	// An error event inside a 200 stream carries no HTTP status of its own, and
	// wrapMetaUpstreamError reports those as 502 unless the event names a code.
	if statusCode < 400 || statusCode >= 600 || len(body) == 0 {
		return false
	}
	return strings.Contains(strings.ToLower(string(body)), metaReasoningNotIssuedMessage)
}

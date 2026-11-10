package main

// Behavior-parity tests for the four Meta CLIProxy fork patches this plugin
// carries so they can be dropped from the fork:
//
//  1. 4a6fcb08 account-bound reasoning replay.
//  2. 2c99035c per-session prompt_cache_key.
//  3. e86b751 reasoning-replay review fixes (fail-closed sanitizer, disabled
//     tag stripping, empty-item removal).
//  4. b4c6bf0e stream bootstrap peek plus subscription-usage skip.
//
// Vectors mirror the host fork tests they replace
// (internal/runtime/executor/meta_reasoning_replay_test.go,
// meta_prompt_cache_key_test.go, the meta executor stream/quota tests, and
// internal/signature/meta_validation_test.go). Where the plugin necessarily
// adapts (session resolution via CanonicalSessionID instead of the host request
// context, client-format prefilter before the stock translators that drop the
// meta# tag), the test names the adaptation and asserts the adapted behavior.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// parityEnvelope builds a shape-valid Meta reasoning envelope: 45 bytes of
// envelope overhead (opening with the observed five-byte header) plus a
// 1024-byte power-of-two payload bucket, for 1069 decoded bytes total.
func parityEnvelope(t *testing.T, seeds ...byte) string {
	t.Helper()
	var seed byte = 7
	if len(seeds) > 0 {
		seed = seeds[0]
	}
	raw := append([]byte{0x43, 0xe3, 0xda, 0x0e, 0x01}, make([]byte, 1064)...)
	for i := range raw[5:] {
		raw[5+i] = byte(i*31) + seed
	}
	env := base64.RawURLEncoding.EncodeToString(raw)
	if !IsValidMetaReasoningSignature(env) {
		t.Fatalf("test envelope failed validation: %q", env)
	}
	return env
}

// parityReplay builds an enabled replay policy for authID on the given model.
func parityReplay(authID, model string) metaReasoningReplay {
	if model == "" {
		model = "muse-spark-1.3"
	}
	return newMetaReasoningReplay(
		metaReasoningReplayConfig{Enabled: true},
		model, model,
		[]byte(`{"model":"`+model+`"}`),
		map[string]any{"requested_model": model},
		authID, metaDefaultAPIBaseURL,
	)
}

func TestParityReplayAccountKey(t *testing.T) {
	a, b := metaReasoningAccountKey("auth-a"), metaReasoningAccountKey("auth-a")
	if a == "" || len(a) != 16 {
		t.Fatalf("account key = %q, want 16 hex chars", a)
	}
	for _, r := range a {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("account key = %q, want lowercase hex", a)
		}
	}
	if a != b {
		t.Fatal("same auth derived different account keys")
	}
	if c := metaReasoningAccountKey("auth-b"); a == c {
		t.Fatal("different auths derived the same account key")
	}
	if got := metaReasoningAccountKey(""); got != "" {
		t.Fatalf("empty auth derived %q, want empty (replay off)", got)
	}
	if got := metaReasoningAccountKey("   "); got != "" {
		t.Fatalf("blank auth derived %q, want empty (replay off)", got)
	}
}

func TestParityReplayDecide(t *testing.T) {
	env := parityEnvelope(t)
	own := parityReplay("auth-a", "")
	ownKey := own.accountKey
	if ownKey == "" || !own.enabled {
		t.Fatal("enabled config with identified auth must enable replay")
	}
	foreign := TagMetaReasoning(metaReasoningAccountKey("auth-b"), env)

	decided := own.decide(TagMetaReasoning(ownKey, env))
	if decided.drop || decided.replay != env {
		t.Fatalf("own-account tag not kept stripped: %+v", decided)
	}
	if decided := own.decide(foreign); !decided.drop {
		t.Fatal("foreign-account tag not dropped")
	}
	if decided := own.decide(env); !decided.drop {
		t.Fatal("untagged envelope not dropped while enabled")
	}
	if own.stats.kept != 1 || own.stats.droppedForeign != 1 || own.stats.droppedUntagged != 1 {
		t.Fatalf("stats = %+v, want 1/1/1", own.stats)
	}

	// Disabled feature: untagged envelopes forward as-is; tags from an earlier
	// enabled era are stripped so nothing the feature minted reaches Meta.
	off := newMetaReasoningReplay(metaReasoningReplayConfig{}, "muse-spark-1.3", "muse-spark-1.3", nil, nil, "auth-a", metaDefaultAPIBaseURL)
	if off.enabled {
		t.Fatal("empty config must leave replay disabled")
	}
	if decided := off.decide(env); decided.drop || decided.replay != env {
		t.Fatalf("disabled replay must forward untagged envelopes: %+v", decided)
	}
	if decided := off.decide(TagMetaReasoning(ownKey, env)); !decided.drop {
		t.Fatal("disabled replay must drop tagged envelopes")
	}
}

func TestParityReplayModelScope(t *testing.T) {
	canary := metaReasoningReplayConfig{Enabled: true, Models: []string{"muse-spark-*"}}
	if r := newMetaReasoningReplay(canary, "muse-spark-1.3", "muse-spark-1.3", nil, nil, "a", metaDefaultAPIBaseURL); !r.enabled {
		t.Fatal("canary must match muse-spark-1.3")
	}
	if r := newMetaReasoningReplay(canary, "meta-muse/muse-wonder-1.0", "muse-wonder-1.0", nil, nil, "a", metaDefaultAPIBaseURL); r.enabled {
		t.Fatal("canary must not match namespaced muse-wonder-1.0")
	}
	off := metaReasoningReplayConfig{Enabled: false}
	if r := newMetaReasoningReplay(off, "muse-spark-1.3", "muse-spark-1.3", nil, nil, "a", metaDefaultAPIBaseURL); r.enabled {
		t.Fatal("disabled config must never enable replay")
	}
	if r := newMetaReasoningReplay(metaReasoningReplayConfig{Enabled: true}, "muse-spark-1.3", "muse-spark-1.3", nil, nil, "", metaDefaultAPIBaseURL); r.enabled {
		t.Fatal("unidentified auth must leave replay off")
	}
}

func TestParityReplayRetryOnce(t *testing.T) {
	env := parityEnvelope(t)
	own := parityReplay("auth-a", "")
	base := []byte(`{"model":"muse-spark-1.3","input":[{"type":"reasoning","encrypted_content":` + strconv.Quote(TagMetaReasoning(own.accountKey, env)) + `}]}`)
	prepared := preparedMetaRequest{body: own.sanitizeCodexRequest(base), replay: own}
	if !prepared.replay.canRetryWithoutReplay() {
		t.Fatal("forwarded envelope must make the retry worthwhile")
	}
	next, ok := prepared.retryWithoutReplay(400, []byte(`reasoning encrypted_content was not issued to this caller`))
	if !ok {
		t.Fatal("not-issued 400 must trigger one retry without reasoning")
	}
	if !next.replayRetried || gjson.GetBytes(next.body, "input.0.encrypted_content").Exists() {
		t.Fatalf("retry must strip every envelope: %s", next.body)
	}
	if _, ok := next.retryWithoutReplay(400, []byte(`not issued to this caller`)); ok {
		t.Fatal("retry must happen exactly once")
	}
	if _, ok := prepared.retryWithoutReplay(429, []byte(`rate limited`)); ok {
		t.Fatal("non-replay errors must not trigger the reasoning retry")
	}
	if _, ok := prepared.retryWithoutReplay(400, []byte(`some other 400`)); ok {
		t.Fatal("unrelated 400s must not trigger the reasoning retry")
	}
	empty := preparedMetaRequest{replay: parityReplay("auth-a", "")}
	if _, ok := empty.retryWithoutReplay(400, []byte(`not issued to this caller`)); ok {
		t.Fatal("no forwarded envelope means no retry")
	}
}

func TestParityReplayTagEgress(t *testing.T) {
	env := parityEnvelope(t)
	own := parityReplay("auth-a", "")
	key := own.accountKey
	completed := []byte(`{"type":"response.completed","response":{"output":[{"type":"reasoning","encrypted_content":` + strconv.Quote(env) + `}]}}`)
	tagged := own.tagResponseEvent(completed)
	if got := gjson.GetBytes(tagged, "response.output.0.encrypted_content").String(); got != TagMetaReasoning(key, env) {
		t.Fatalf("completed output not tagged: %s", tagged)
	}
	added := []byte(`{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":` + strconv.Quote(env) + `}}`)
	if got := gjson.GetBytes(own.tagResponseEvent(added), "item.encrypted_content").String(); got != TagMetaReasoning(key, env) {
		t.Fatalf("output_item.added not tagged: %s", added)
	}
	bare := []byte(`{"object":"response","output":[{"type":"reasoning","encrypted_content":` + strconv.Quote(env) + `}]}`)
	if got := gjson.GetBytes(own.tagResponseEvent(bare), "output.0.encrypted_content").String(); got != TagMetaReasoning(key, env) {
		t.Fatalf("bare response object not tagged: %s", bare)
	}
	other := []byte(`{"type":"response.output_text.delta","delta":"hi"}`)
	if got := string(own.tagResponseEvent(other)); got != string(other) {
		t.Fatalf("non-reasoning event mutated: %s", got)
	}
	off := newMetaReasoningReplay(metaReasoningReplayConfig{}, "muse-spark-1.3", "muse-spark-1.3", nil, nil, "auth-a", metaDefaultAPIBaseURL)
	if got := string(off.tagResponseEvent(completed)); got != string(completed) {
		t.Fatal("disabled replay must leave egress untagged")
	}
}

func TestParityReplayLongConversationAcrossAccounts(t *testing.T) {
	env := parityEnvelope(t)
	first := parityReplay("auth-a", "")
	second := parityReplay("auth-b", "")
	firstTag, secondTag := TagMetaReasoning(first.accountKey, env), TagMetaReasoning(second.accountKey, env)
	history := []byte(`{"input":[{"type":"reasoning","encrypted_content":` + strconv.Quote(firstTag) + `},{"type":"reasoning","encrypted_content":` + strconv.Quote(secondTag) + `},{"type":"reasoning","encrypted_content":` + strconv.Quote(env) + `}]}`)
	// Second account serves the turn: only its own envelope survives.
	got := second.sanitizeCodexRequest(history)
	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 1 {
		t.Fatalf("cross-account history kept %d items, want 1: %s", len(items), got)
	}
	if kept := items[0].Get("encrypted_content").String(); kept != env {
		t.Fatalf("kept envelope = %q, want stripped native %q", kept, env)
	}
	// Single account replays the whole history.
	single := parityReplay("auth-a", "")
	ownHistory := []byte(`{"input":[{"type":"reasoning","encrypted_content":` + strconv.Quote(firstTag) + `},{"type":"reasoning","encrypted_content":` + strconv.Quote(TagMetaReasoning(first.accountKey, parityEnvelope(t))) + `}]}`)
	if kept := gjson.GetBytes(single.sanitizeCodexRequest(ownHistory), "input").Array(); len(kept) != 2 {
		t.Fatalf("single-account history kept %d items, want 2", len(kept))
	}
}

func TestParitySanitizeFailClosed(t *testing.T) {
	env := parityEnvelope(t)
	valid := []byte(`{"model":"muse-spark-1.3","input":[{"type":"reasoning","id":"rs_1","encrypted_content":` + strconv.Quote(env) + `}],"store":false}`)
	if got := sanitizeMetaReasoningEnvelopes(valid, "https://api.meta.ai"); string(got) != string(valid) {
		t.Fatalf("valid native envelope mutated: %s", got)
	}
	garbage := []byte(`{"model":"muse-spark-1.3","input":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gpt-envelope"},{"type":"message","content":"hi"}],"store":false}`)
	got := sanitizeMetaReasoningEnvelopes(garbage, "https://api.meta.ai")
	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("foreign envelope not stripped: %s", got)
	}
	if gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatalf("orphan reasoning id not stripped with store=false: %s", got)
	}
	if gjson.GetBytes(got, "input.1.content").String() != "hi" {
		t.Fatalf("non-reasoning item mutated: %s", got)
	}
	// Router suffix: native upstream refuses it, routed upstream replays it.
	suffixed := []byte(`{"model":"muse-spark-1.3","input":[{"type":"reasoning","encrypted_content":` + strconv.Quote(env+".abc123") + `}],"store":false}`)
	if out := sanitizeMetaReasoningEnvelopes(suffixed, "https://api.meta.ai"); gjson.GetBytes(out, "input.0.encrypted_content").Exists() {
		t.Fatalf("router-suffixed envelope kept for native upstream: %s", out)
	}
	if out := sanitizeMetaReasoningEnvelopes(suffixed, "https://openrouter.ai"); gjson.GetBytes(out, "input.0.encrypted_content").String() != env+".abc123" {
		t.Fatalf("router-suffixed envelope not replayed verbatim on router: %s", out)
	}
}

func TestParityPromptCacheKey(t *testing.T) {
	a, b := metaPromptCacheKeyForSession("sess-1"), metaPromptCacheKeyForSession("sess-1")
	if a == "" || len(a) != 32 {
		t.Fatalf("key = %q, want 32 hex chars", a)
	}
	if a != b || a == metaPromptCacheKeyForSession("sess-2") {
		t.Fatal("key derivation not stable/distinct")
	}
	if got := metaPromptCacheKeyForSession(""); got != "" {
		t.Fatalf("empty session derived %q, want empty", got)
	}
	body := []byte(`{"model":"muse-spark-1.3","input":[]}`)
	headers := http.Header{"X-Claude-Code-Session-Id": []string{"sess-1"}}

	// Translated-body key wins over everything.
	withKey := []byte(`{"model":"muse-spark-1.3","prompt_cache_key":"body-key","input":[]}`)
	if got := applyMetaPromptCacheKey(withKey, headers, nil, nil, nil, nil); gjson.GetBytes(got, "prompt_cache_key").String() != "body-key" {
		t.Fatalf("body key overwritten: %s", got)
	}
	// Client-payload key is carried over (client payload before original request).
	client := []byte(`{"model":"muse-spark-1.3","prompt_cache_key":"client-key-1"}`)
	if got := applyMetaPromptCacheKey(body, headers, nil, client, nil, nil); gjson.GetBytes(got, "prompt_cache_key").String() != "client-key-1" {
		t.Fatalf("client key not carried: %s", got)
	}
	original := []byte(`{"prompt_cache_key":"client-key-3"}`)
	if got := applyMetaPromptCacheKey(body, headers, nil, original, nil, nil); gjson.GetBytes(got, "prompt_cache_key").String() != "client-key-3" {
		t.Fatalf("original-request key not carried: %s", got)
	}
	// Derived session key otherwise. The plugin resolves the session through
	// CanonicalSessionID (bound identity, not the raw header), so the
	// expectation is derived the same way.
	session := resolveMetaPromptCacheKeySession(headers, nil, []byte(`{"model":"muse-spark-1.3"}`), nil)
	if session == "" {
		t.Fatal("no session resolved from the session header")
	}
	want := metaPromptCacheKeyForSession(session)
	if got := applyMetaPromptCacheKey(body, headers, nil, []byte(`{"model":"muse-spark-1.3"}`), nil, nil); gjson.GetBytes(got, "prompt_cache_key").String() != want {
		t.Fatalf("key = %s, want derived %q", got, want)
	}
	// No resolvable session sets nothing.
	if got := applyMetaPromptCacheKey(body, nil, nil, []byte(`{"model":"muse-spark-1.3"}`), nil, nil); gjson.GetBytes(got, "prompt_cache_key").Exists() {
		t.Fatalf("sessionless request gained a key: %s", got)
	}
}

func TestParityPrefilterClientPayload(t *testing.T) {
	env := parityEnvelope(t)
	stranger := parityEnvelope(t, 42)
	if stranger == env {
		t.Fatal("test envelopes must differ")
	}
	own := parityReplay("auth-a", "")
	otherTag := TagMetaReasoning(metaReasoningAccountKey("auth-b"), env)
	ownTag := TagMetaReasoning(own.accountKey, env)

	in := []byte(`{"model":"muse-spark-1.3","input":[{"type":"reasoning","encrypted_content":` + strconv.Quote(ownTag) + `},{"type":"reasoning","encrypted_content":` + strconv.Quote(otherTag) + `},{"type":"reasoning","encrypted_content":` + strconv.Quote(stranger) + `},{"type":"message","content":"hi"}]}`)
	got := prefilterClientPayload(in, sdktranslator.FormatOpenAIResponse, own)
	items := gjson.GetBytes(got, "input").Array()
	// Own tag stripped to native, foreign tag dropped, untagged native left for
	// the translated-body policy (second barrier), message untouched. The
	// untagged item uses a distinct envelope so it is not confused with the
	// approved own envelope.
	if len(items) != 3 {
		t.Fatalf("prefilter kept %d items, want own + untagged + message: %s", len(items), got)
	}
	if kept := items[0].Get("encrypted_content").String(); kept != env {
		t.Fatalf("own tag not stripped to native: %q", kept)
	}
	if kept := items[1].Get("encrypted_content").String(); kept != stranger {
		t.Fatalf("untagged envelope not passed through to second barrier: %q", kept)
	}
	second := own.sanitizeCodexRequest(got)
	if kept := gjson.GetBytes(second, "input").Array(); len(kept) != 2 {
		t.Fatalf("second barrier kept %d items, want stripped own + message: %s", len(kept), second)
	}

	claude := []byte(`{"model":"muse-spark-1.3","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"deep","signature":` + strconv.Quote(otherTag) + `},{"type":"thinking","thinking":"kept","signature":` + strconv.Quote(ownTag) + `}]}]}`)
	filtered := prefilterClientPayload(claude, sdktranslator.FormatClaude, own)
	parts := gjson.GetBytes(filtered, "messages.0.content").Array()
	if len(parts) != 2 || parts[0].Get("signature").Exists() {
		t.Fatalf("foreign claude signature not dropped: %s", filtered)
	}
	if parts[1].Get("signature").String() != env || parts[1].Get("thinking").String() != "kept" {
		t.Fatalf("own claude signature not stripped: %s", filtered)
	}

	gemini := []byte(`{"contents":[{"parts":[{"text":"hi","thoughtSignature":` + strconv.Quote(otherTag) + `},{"text":"yo","thought_signature":` + strconv.Quote(ownTag) + `}]}]}`)
	gfiltered := prefilterClientPayload(gemini, sdktranslator.FormatGemini, own)
	if gjson.GetBytes(gfiltered, "contents.0.parts.0.thoughtSignature").Exists() {
		t.Fatalf("foreign gemini signature not dropped: %s", gfiltered)
	}
	if gjson.GetBytes(gfiltered, "contents.0.parts.1.thought_signature").String() != env {
		t.Fatalf("own gemini signature not stripped: %s", gfiltered)
	}
}

func TestParityStreamBootstrapPeek(t *testing.T) {
	ctx := context.Background()
	streamChunks := func(payloads ...string) <-chan pluginapi.HTTPStreamChunk {
		ch := make(chan pluginapi.HTTPStreamChunk, len(payloads))
		for _, p := range payloads {
			ch <- pluginapi.HTTPStreamChunk{Payload: []byte(p)}
		}
		close(ch)
		return ch
	}

	// A foreign-envelope rejection arriving as the first 200-stream event
	// triggers the once-only retry; peeked bytes are rewound, not consumed.
	errEvent := "data: {\"type\":\"error\",\"error\":{\"code\":400,\"message\":\"reasoning encrypted_content was not issued to this caller\"}}\n"
	rewound, retry, err := peekMetaStreamBootstrap(ctx, streamChunks(errEvent))
	if err != nil || !retry {
		t.Fatalf("peek retry=%v err=%v, want retry on first-event rejection", retry, err)
	}
	var rebuilt strings.Builder
	for c := range rewound {
		rebuilt.Write(c.Payload)
	}
	if !strings.Contains(rebuilt.String(), "not issued to this caller") {
		t.Fatalf("peek dropped bytes instead of rewinding: %q", rebuilt.String())
	}

	// A healthy first event peeks clean: no retry, bytes preserved.
	rewound, retry, err = peekMetaStreamBootstrap(ctx, streamChunks("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r_1\"}}\n"))
	if err != nil || retry {
		t.Fatalf("healthy first event caused retry=%v err=%v", retry, err)
	}
	rebuilt.Reset()
	for c := range rewound {
		rebuilt.Write(c.Payload)
	}
	if !strings.Contains(rebuilt.String(), "response.created") {
		t.Fatalf("peek lost bytes: %q", rebuilt.String())
	}

	// Subscription usage before the rejection is skipped, not mistaken for it.
	usage := "data: {\"type\":\"response.subscription_usage\",\"subscription\":{\"tier\":\"plus\",\"window\":{\"used_percent\":25,\"resets_at\":1893456000}}}\n"
	rewound, retry, err = peekMetaStreamBootstrap(ctx, streamChunks(usage, "data: {\"type\":\"response.created\"}\n"))
	if err != nil || retry {
		t.Fatalf("usage event caused retry=%v err=%v, want skip", retry, err)
	}
	rebuilt.Reset()
	for c := range rewound {
		rebuilt.Write(c.Payload)
	}
	if !strings.Contains(rebuilt.String(), "subscription_usage") || !strings.Contains(rebuilt.String(), "response.created") {
		t.Fatalf("peek lost bytes: %q", rebuilt.String())
	}
}

func TestParitySubscriptionUsageFilteredAndObserved(t *testing.T) {
	usageEvent := []byte(`{"type":"response.subscription_usage","subscription":{"tier":"plus","window":{"used_percent":25,"window_duration_mins":300,"resets_at":1893456000},"weekly":{"used_percent":10,"resets_at":1893456000}}}`)
	stream := []byte("data: {\"type\":\"response.created\"}\n\ndata: " + string(usageEvent) + "\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n")

	prepared := &preparedMetaRequest{replay: parityReplay("auth-a", "")}
	completed, errEvent := translateMetaCompleted(context.Background(), prepared, pluginapi.ExecutorRequest{}, stream)
	if completed == nil || gjson.GetBytes(completed, "response").Exists() == false && gjson.GetBytes(completed, "type").String() != "response.completed" {
		t.Fatalf("buffered completed not collected: %s errEvent=%s", completed, errEvent)
	}
	if strings.Contains(string(completed), "subscription_usage") {
		t.Fatalf("usage event leaked into completed: %s", completed)
	}

	headers := parseMetaSubscriptionUsageHeaders(usageEvent)
	if headers.Get(metaQuotaTierHeader) != "plus" || headers.Get(metaQuotaWindowUsedHeader) != "25" {
		t.Fatalf("usage headers = %v, want tier plus and 25%% window", headers)
	}
	out, ok := subscriptionHeadersToQuota(headers)
	if !ok || out.Subscription == nil || out.Subscription.TierName != "plus" || len(out.Groups) != 1 || len(out.Groups[0].Buckets) != 2 {
		t.Fatalf("quota card = %+v, want plus card with 2 buckets", out)
	}
	if out.Groups[0].Buckets[0].RemainingFraction != 0.75 || out.Groups[0].Buckets[1].RemainingFraction != 0.9 {
		t.Fatalf("bucket fractions = %+v, want 0.75/0.9", out.Groups[0].Buckets)
	}

	filtered := filterMetaEgressHeaders(http.Header{"X-Meta-Window-Used-Percent": {"25"}, "Content-Type": {"text/event-stream"}})
	if filtered.Get(metaQuotaWindowUsedHeader) != "" || filtered.Get("Content-Type") == "" {
		t.Fatalf("egress headers = %v, want X-Meta-* dropped and content kept", filtered)
	}
}

func TestParityQuotaDualProvider(t *testing.T) {
	q := newQuotaProvider(providerkit.NewQuotaHTTPClient(nil))
	desc, err := q.DescribeQuota(context.Background(), pluginapi.QuotaDescribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.SupportedProviders) != 2 || desc.SupportedProviders[0] != providerID || desc.SupportedProviders[1] != metaMuseNamespace {
		t.Fatalf("describe = %+v, want [meta meta-muse]", desc)
	}
	if q.Identifier() != providerID {
		t.Fatalf("routing key = %q, want meta", q.Identifier())
	}
}

func TestParityAuthTakeover(t *testing.T) {
	auth := &metaAuthProvider{}
	if auth.Identifier() != metaMuseNamespace {
		t.Fatalf("identifier = %q, want meta-muse", auth.Identifier())
	}
	// Single auth identifier (the kit rejects MultiAuthProvider); the legacy
	// key is claimed through the credential-provider bridge instead.
	if creds := auth.CredentialProviders(); len(creds) != 1 || creds[0] != providerID {
		t.Fatalf("credential providers = %v, want [meta]", creds)
	}
	for _, typ := range []string{providerID, metaMuseNamespace} {
		resp, err := auth.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"` + typ + `","api_key":"k"}`)})
		if err != nil || !resp.Handled || resp.Auth.Provider != metaMuseNamespace {
			t.Fatalf("type %q not claimed: %+v %v", typ, resp, err)
		}
	}
	resp, err := auth.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"openrouter","api_key":"k"}`)})
	if err != nil || resp.Handled {
		t.Fatalf("foreign type claimed: %+v %v", resp, err)
	}
	if !isMetaAuthType(providerID) || !isMetaAuthType(metaMuseNamespace) || isMetaAuthType("openrouter") {
		t.Fatal("isMetaAuthType mismatch")
	}

	// Credential precedence: attributes beat metadata beat storage; DCA never an API key.
	storage := []byte(`{"type":"meta","api_key":"stored-key","dca_token":"dca:stored"}`)
	_, token := metaCreds(storage, map[string]any{"api_key": "meta-key"}, map[string]string{"api_key": "attr-key"})
	if token != "attr-key" {
		t.Fatalf("attribute key lost: %q", token)
	}
	if _, token := metaCreds(storage, map[string]any{"api_key": "dca:only"}, nil); token != "" && strings.HasPrefix(token, "dca:") {
		t.Fatalf("DCA used as API key: %q", token)
	}
	if dca := metaDCAToken(storage, nil, nil); dca != "dca:stored" {
		t.Fatalf("dca = %q", dca)
	}
}

// TestParityAuthIDLeftEmpty proves ParseAuth leaves ID empty so the host
// derives the same path-relative ID core uses (pluginAuthDataToCoreAuth falls
// back to authIDForPath). A plugin-invented ID would fork the replay account
// key across the cutover, rollback, and restarts.
func TestParityAuthIDLeftEmpty(t *testing.T) {
	auth := &metaAuthProvider{}
	for _, typ := range []string{providerID, metaMuseNamespace} {
		resp, err := auth.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
			RawJSON:  []byte(`{"type":"` + typ + `","api_key":"k"}`),
			Path:     "/auths/meta-user.json",
			FileName: "meta-user.json",
		})
		if err != nil || !resp.Handled {
			t.Fatalf("type %q not claimed: %+v %v", typ, resp, err)
		}
		if resp.Auth.ID != "" {
			t.Fatalf("type %q: ParseAuth ID = %q, want empty so the host derives the path-relative ID", typ, resp.Auth.ID)
		}
		if resp.Auth.FileName != "meta-user.json" {
			t.Fatalf("type %q: FileName = %q, want the host-supplied name", typ, resp.Auth.FileName)
		}
	}
	// A file carrying an explicit id field must not pin the runtime ID either:
	// core file auths have no id field; the ID is path-derived.
	resp, err := auth.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		RawJSON:  []byte(`{"type":"meta","id":"meta-muse:abc123","api_key":"k"}`),
		Path:     "/auths/meta-user.json",
		FileName: "meta-user.json",
	})
	if err != nil || !resp.Handled {
		t.Fatalf("legacy file not claimed: %+v %v", resp, err)
	}
	if resp.Auth.ID != "" {
		t.Fatalf("explicit file id leaked into runtime ID = %q, want empty", resp.Auth.ID)
	}
}

// TestParityPollLoginLeavesIDEmpty proves a successful device login returns an
// empty ID with the FileName already set, so the host login-save path derives
// the path ID from CredentialFileName — the same ID a restart derives, and
// never a token hash that changes on every re-mint.
func TestParityPollLoginLeavesIDEmpty(t *testing.T) {
	fake := &fakeSecurityHTTPClient{}
	fake.do = func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.Contains(req.URL, "device/token") {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"access_token":"dca:test-token","token_type":"bearer","expires_in":3600}`)}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"api_key":"sk-test-key","base_url":"https://api.meta.ai/v1","user_email":"test-user","user_full_name":"Test User"}`)}, nil
	}
	state := encodeMetaLoginState(metaLoginState{
		DeviceCode:      "dc-1",
		TokenEndpoint:   metaTokenEndpoint,
		IntervalSeconds: 5,
		ExpiresAtUnix:   time.Now().Add(time.Hour).Unix(),
	})
	resp, err := (&metaAuthProvider{}).PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{
		State: state, HTTPClient: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success", resp.Status)
	}
	if resp.Auth.ID != "" {
		t.Fatalf("PollLogin ID = %q, want empty so the host derives the path-relative ID", resp.Auth.ID)
	}
	if !strings.HasPrefix(resp.Auth.FileName, "meta-test-user-") {
		t.Fatalf("FileName = %q, want the CredentialFileName for the minted account", resp.Auth.FileName)
	}
	if len(fake.doCalls) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (token poll + mint)", len(fake.doCalls))
	}
}

// TestParityPollLoginMintFailureStillSaves proves an approved device login
// with a failed mint still succeeds with a DCA-derived filename (core parity:
// meta.CredentialFileName(email, dcaToken)), so the host can save the
// credential and derive an ID instead of failing the commit with no filename
// and no ID. The DCA token is retained for a later mint.
func TestParityPollLoginMintFailureStillSaves(t *testing.T) {
	fake := &fakeSecurityHTTPClient{}
	fake.do = func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.Contains(req.URL, "device/token") {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"access_token":"dca:unminted-token","token_type":"bearer","expires_in":3600}`)}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 503, Body: []byte(`unavailable`)}, nil
	}
	state := encodeMetaLoginState(metaLoginState{
		DeviceCode:      "dc-1",
		TokenEndpoint:   metaTokenEndpoint,
		IntervalSeconds: 5,
		ExpiresAtUnix:   time.Now().Add(time.Hour).Unix(),
	})
	resp, err := (&metaAuthProvider{}).PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{
		State: state, HTTPClient: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success (mint failure must not fail an approved login)", resp.Status)
	}
	if resp.Auth.ID != "" {
		t.Fatalf("PollLogin ID = %q, want empty", resp.Auth.ID)
	}
	wantFile := CredentialFileName("", "dca:unminted-token")
	if resp.Auth.FileName != wantFile {
		t.Fatalf("FileName = %q, want DCA-derived %q", resp.Auth.FileName, wantFile)
	}
	var storage map[string]any
	if err := json.Unmarshal(resp.Auth.StorageJSON, &storage); err != nil {
		t.Fatal(err)
	}
	if storage["dca_token"] != "dca:unminted-token" {
		t.Fatalf("storage lost the DCA token: %v", storage)
	}
	if _, hasKey := storage["api_key"]; hasKey {
		t.Fatalf("storage has api_key despite failed mint: %v", storage)
	}
}

// TestParityAccountKeyPathRelative proves the replay account key is keyed on a
// path-relative auth ID (e.g. meta-<file>.json): stable across a plugin-only
// restart, distinct per account file, and able to verify a fork-era tag minted
// under the same path-relative ID (cutover and rollback round trip).
func TestParityAccountKeyPathRelative(t *testing.T) {
	const authID = "meta-user_example.com-a1b2c3d4e5f60718.json"
	first := metaReasoningAccountKey(authID)
	if first == "" || len(first) != 16 {
		t.Fatalf("account key = %q, want 16 hex chars", first)
	}
	// Restart: the same file derives the same key.
	if again := metaReasoningAccountKey(authID); again != first {
		t.Fatalf("restart derived %q, want %q", again, first)
	}
	// Another account file derives a different key.
	if other := metaReasoningAccountKey("meta-other_example.com-0011223344556677.json"); other == first {
		t.Fatalf("different file derived the same account key %q", other)
	}
	// A fork-era tag minted under the same path-relative ID verifies.
	env := parityEnvelope(t)
	policy := parityReplay(authID, "")
	if !policy.enabled || policy.accountKey != first {
		t.Fatalf("policy not enabled for path-relative ID (enabled=%v key=%q)", policy.enabled, policy.accountKey)
	}
	if decided := policy.decide(TagMetaReasoning(first, env)); decided.drop || decided.replay != env {
		t.Fatalf("path-relative tag not kept stripped: %+v", decided)
	}
}

// parityCompactHTTPClient records upstream calls on both lanes so the compact
// tests can prove none is made.
type parityCompactHTTPClient struct {
	doCalls     int
	streamCalls int
}

func (f *parityCompactHTTPClient) Do(_ context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.doCalls++
	return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}, nil
}

func (f *parityCompactHTTPClient) DoStream(_ context.Context, _ pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	f.streamCalls++
	return pluginapi.HTTPStreamResponse{}, nil
}

func parityCompactError(t *testing.T, err error) {
	t.Helper()
	perr, ok := err.(*pluginabi.Error)
	if !ok {
		t.Fatalf("compact error type = %T, want *pluginabi.Error", err)
	}
	if perr.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("compact HTTPStatus = %d, want 501", perr.HTTPStatus)
	}
}

// TestParityResponsesCompactNotImplemented proves responses/compact is rejected
// with 501 before any upstream call on both lanes, as core does — a compact
// request must never execute as a normal /responses call. CountTokens parity:
// the fork has no Alt guard there, so counting with Alt set still succeeds.
func TestParityResponsesCompactNotImplemented(t *testing.T) {
	ctx := context.Background()
	e := newMetaExecutor(nil, &metaAuthProvider{})
	body := []byte(`{"model":"muse-spark-1.3","input":[]}`)
	newReq := func(fake *parityCompactHTTPClient) pluginapi.ExecutorRequest {
		return pluginapi.ExecutorRequest{
			Alt:             "responses/compact",
			Model:           metaMuseNamespace + "/muse-spark-1.3",
			Format:          "openai-response",
			SourceFormat:    "openai-response",
			Payload:         body,
			OriginalRequest: body,
			Headers:         http.Header{},
			HTTPClient:      fake,
		}
	}

	fake := &parityCompactHTTPClient{}
	if _, err := e.Execute(ctx, newReq(fake)); err == nil {
		t.Fatal("Execute accepted responses/compact, want 501")
	} else {
		parityCompactError(t, err)
	}
	if fake.doCalls != 0 || fake.streamCalls != 0 {
		t.Fatalf("Execute made %d+%d upstream calls, want none", fake.doCalls, fake.streamCalls)
	}

	fake = &parityCompactHTTPClient{}
	if _, err := e.ExecuteStream(ctx, newReq(fake)); err == nil {
		t.Fatal("ExecuteStream accepted responses/compact, want 501")
	} else {
		parityCompactError(t, err)
	}
	if fake.doCalls != 0 || fake.streamCalls != 0 {
		t.Fatalf("ExecuteStream made %d+%d upstream calls, want none", fake.doCalls, fake.streamCalls)
	}

	counted, err := e.CountTokens(ctx, newReq(&parityCompactHTTPClient{}))
	if err != nil {
		t.Fatalf("CountTokens with Alt set failed: %v (fork has no Alt guard there)", err)
	}
	if len(counted.Payload) == 0 {
		t.Fatal("CountTokens with Alt set returned no payload")
	}
}

func TestParityModelsNamespaced(t *testing.T) {
	provider := newMetaModelCatalog()
	resp, err := provider.StaticModels(context.Background(), pluginapi.StaticModelRequest{Plugin: pluginapi.Metadata{Name: metaMuseNamespace}})
	if err != nil || resp.Provider != metaMuseNamespace || len(resp.Models) == 0 {
		t.Fatalf("static models = %+v %v", resp, err)
	}
	for _, m := range resp.Models {
		if !strings.HasPrefix(m.ID, metaMuseNamespace+"/") {
			t.Fatalf("model %q missing namespace", m.ID)
		}
	}
	if got := stripMetaMuseModelID(metaMuseNamespace + "/muse-spark-1.3"); got != "muse-spark-1.3" {
		t.Fatalf("strip = %q", got)
	}
}

func TestStaticModelsIncludeMuseEmber(t *testing.T) {
	resp, err := newMetaModelCatalog().StaticModels(context.Background(), pluginapi.StaticModelRequest{Plugin: pluginapi.Metadata{Name: metaMuseNamespace}})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range resp.Models {
		switch m.Name {
		case "muse-ember-1.0":
			found = true
			if m.ID != metaMuseNamespace+"/muse-ember-1.0" || m.ContextLength != 1048576 || m.MaxCompletionTokens != 524288 {
				t.Fatalf("muse-ember-1.0 = %+v", m)
			}
		case "muse-ember-1.0-contributor":
			t.Fatal("Muse Ember has no Contributor-mode variant")
		}
	}
	if !found {
		t.Fatal("muse-ember-1.0 missing from static catalog")
	}
}

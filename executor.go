package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"

	log "github.com/sirupsen/logrus"
)

// This file is the Meta Muse executor. It runs the four carried fork patches
// on stock CLIProxy so they can be dropped from the fork:
//
//  1. Account-bound reasoning replay (4a6fcb08): provenance tag + keep/drop
//     policy in prefilter.go and replay.go, one-shot retry without reasoning.
//  2. Per-session prompt_cache_key (2c99035c): promptcachekey.go injection.
//  3. Reasoning-replay review fixes (e86b751): fail-closed sanitizer,
//     disabled-feature tag stripping, empty-item removal.
//  4. Stream bootstrap peek + subscription-usage skip (b4c6bf0e): first-event
//     peek with rewind before streaming, subscription_usage filtering on both
//     paths, passive quota observation via the kit CachedQuota store.
//
// Pipeline per request: resolve credential -> build replay policy -> prefilter
// client payload (tag-aware, before the stock translators that drop tags) ->
// pinned SDK translate to Codex -> Codex-level replay sanitize + tool hygiene
// -> prompt_cache_key injection -> upstream /responses -> tag egress envelopes
// -> pinned SDK translate back to the client format. Payload configuration is
// host-side and stays the final semantic barrier: the host applies thinking
// and payload rules before RPC, and this executor performs no user-payload
// mutation after translation.

// metaClientIdentity headers mirror Muse Code 1.4.14 on linux-x86_64 so
// subscription-key attestation accepts tool-bearing requests.
const (
	metaClientVersion = "1.4.14"
	metaClientBuild   = "dec7d7497fecdad135ec5835a8161dbc92e55b58"
	metaClientSurface = "tbh:exec"
)

// metaSubscriptionUsageEventType is the Responses stream event Meta sends after
// response.completed carrying the account's subscription usage. It is observed
// passively into the quota cache and never reaches the client.
const metaSubscriptionUsageEventType = "response.subscription_usage"

// metaExecutor serves Meta Muse models over the plugin RPC boundary.
type metaExecutor struct {
	quota *quotaProvider
	auth  *metaAuthProvider
}

func newMetaExecutor(quota *quotaProvider, auth *metaAuthProvider) *metaExecutor {
	return &metaExecutor{quota: quota, auth: auth}
}

func (e *metaExecutor) Identifier() string { return metaMuseNamespace }

func (e *metaExecutor) config() pluginConfig { return pluginConfigNow() }

// preparedMetaRequest is one translated upstream request with its replay state.
type preparedMetaRequest struct {
	baseModel       string
	from            sdktranslator.Format
	responseFormat  sdktranslator.Format
	originalPayload []byte
	body            []byte
	replay          metaReasoningReplay
	replayRetried   bool
}

// prepare builds the upstream Codex body for one executor request.
func (e *metaExecutor) prepare(req pluginapi.ExecutorRequest) (*preparedMetaRequest, error) {
	from := sdktranslator.FromString(req.SourceFormat)
	if req.SourceFormat != "" && from.String() == "" {
		from = sdktranslator.FormatOpenAIResponse
	}
	responseFormat := sdktranslator.FromString(req.Format)
	if req.Format != "" && responseFormat.String() == "" {
		responseFormat = from
	}
	if responseFormat.String() == "" {
		responseFormat = sdktranslator.FormatOpenAIResponse
	}
	if from.String() == "" {
		from = responseFormat
	}
	baseModel := stripMetaMuseModelID(req.Model)
	if baseModel == "" {
		return nil, providerkit.MapError(fmt.Errorf("meta-muse executor: missing model"))
	}
	clientPayload := bytes.Clone(req.Payload)
	originalPayload := bytes.Clone(req.OriginalRequest)
	if len(originalPayload) == 0 {
		originalPayload = bytes.Clone(req.Payload)
	}
	baseURL, _ := metaCreds(req.StorageJSON, mergeMetaFields(req), req.AuthAttributes)
	replay := newMetaReasoningReplay(
		e.config().ReasoningReplay,
		req.Model, baseModel, originalPayload, mergeMetaFields(req),
		req.AuthID, baseURL,
	)
	filtered := prefilterClientPayload(clientPayload, from, replay)
	stream := req.Stream
	translated := sdktranslator.TranslateRequest(from, sdktranslator.FormatCodex, baseModel, filtered, stream)
	if len(bytes.TrimSpace(translated)) == 0 {
		return nil, providerkit.MapError(fmt.Errorf("meta-muse executor: translation produced no upstream body"))
	}
	translated = setMetaStringIfDifferent(translated, "model", baseModel)
	translated = setMetaBoolIfDifferent(translated, "stream", stream)
	translated = setMetaBoolIfDifferent(translated, "store", false)
	for _, path := range []string{"generate", "prompt_cache_retention", "safety_identifier", "stream_options", "client_metadata"} {
		translated, _ = sjson.DeleteBytes(translated, path)
	}
	if instructionsMissing(translated) {
		translated, _ = sjson.SetBytes(translated, "instructions", "")
	}
	translated = replay.sanitizeCodexRequest(translated)
	translated = sanitizeMetaWebSearchTools(translated)
	translated = ensureMetaMuseToolNamespace(translated)
	translated = applyMetaPromptCacheKey(translated, req.Headers, req.Query, originalPayload, clientPayload, mergeMetaFields(req))
	return &preparedMetaRequest{
		baseModel:       baseModel,
		from:            from,
		responseFormat:  responseFormat,
		originalPayload: originalPayload,
		body:            translated,
		replay:          replay,
	}, nil
}

// mergeMetaFields returns the merged credential fields for replay canary
// matching: host metadata over object storage.
func mergeMetaFields(req pluginapi.ExecutorRequest) map[string]any {
	fields, _ := providerkit.AuthFields(req.StorageJSON, req.AuthMetadata)
	if fields == nil {
		fields = map[string]any{}
	}
	return fields
}

// Execute runs one non-streaming request: POST, collect, filter, retry, tag,
// translate back to the client format.
func (e *metaExecutor) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	prepared, err := e.prepare(req)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	prepared.replay.recordRequest(prepared.baseModel, req.AuthID)
	baseURL, token, err := e.resolveCreds(ctx, req)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	for {
		resp, err := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{
			Method:  http.MethodPost,
			URL:     url,
			Headers: metaUpstreamHeaders(token, false, req.Headers),
			Body:    prepared.body,
		})
		if err != nil {
			return pluginapi.ExecutorResponse{}, providerkit.MapError(err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if next, ok := prepared.retryWithoutReplay(resp.StatusCode, resp.Body); ok {
				prepared = next
				continue
			}
			return pluginapi.ExecutorResponse{}, wrapMetaUpstreamError(resp.StatusCode, resp.Body)
		}
		e.observeSubscriptionUsage(ctx, req, resp.Body)
		completed, ok := translateMetaCompleted(ctx, prepared, req, resp.Body)
		if !ok {
			return pluginapi.ExecutorResponse{}, providerkit.MapError(fmt.Errorf("meta-muse executor: stream disconnected before response.completed"))
		}
		if isMetaReasoningNotIssuedFailure(completed) {
			if next, ok := prepared.retryWithoutReplay(400, []byte(metaReasoningNotIssuedMessage)); ok {
				prepared = next
				continue
			}
		}
		out := completed
		translated := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatCodex, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, out, nil)
		if len(bytes.TrimSpace(translated)) == 0 {
			return pluginapi.ExecutorResponse{}, providerkit.MapError(fmt.Errorf("meta-muse executor: response translation produced no payload"))
		}
		return pluginapi.ExecutorResponse{Payload: translated, Headers: filterMetaEgressHeaders(resp.Headers)}, nil
	}
}

// ExecuteStream runs one streaming request with the bootstrap peek: the first
// SSE data event is inspected before any chunk reaches the client, so a
// foreign-envelope rejection Cham arriving as a 200-stream error event still
// retries once without reasoning. Anything peeked is rewound onto the stream.
func (e *metaExecutor) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	prepared, err := e.prepare(req)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, err
	}
	prepared.replay.recordRequest(prepared.baseModel, req.AuthID)
	baseURL, token, err := e.resolveCreds(ctx, req)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, err
	}
	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	for {
		stream, err := req.HTTPClient.DoStream(ctx, pluginapi.HTTPRequest{
			Method:  http.MethodPost,
			URL:     url,
			Headers: metaUpstreamHeaders(token, true, req.Headers),
			Body:    prepared.body,
		})
		if err != nil {
			return pluginapi.ExecutorStreamResponse{}, providerkit.MapError(err)
		}
		if stream.StatusCode < 200 || stream.StatusCode >= 300 {
			var body []byte
			for chunk := range stream.Chunks {
				if len(chunk.Payload) > 0 {
					body = append(body, chunk.Payload...)
				}
			}
			if next, ok := prepared.retryWithoutReplay(stream.StatusCode, body); ok {
				prepared = next
				continue
			}
			return pluginapi.ExecutorStreamResponse{}, wrapMetaUpstreamError(stream.StatusCode, body)
		}
		peeked, retry, fatal := peekMetaStreamBootstrap(ctx, stream.Chunks)
		if fatal != nil {
			return pluginapi.ExecutorStreamResponse{}, fatal
		}
		if retry {
			if next, ok := prepared.retryWithoutReplay(400, []byte(metaReasoningNotIssuedMessage)); ok {
				prepared = next
				continue
			}
		}
		out := make(chan pluginapi.ExecutorStreamChunk)
		go pumpMetaStream(ctx, e, req, prepared, peeked, out)
		headers := filterMetaEgressHeaders(stream.Headers).Clone()
		if headers == nil {
			headers = make(http.Header)
		}
		headers.Set("Content-Type", "text/event-stream")
		return pluginapi.ExecutorStreamResponse{Headers: headers, Chunks: out}, nil
	}
}

// pumpMetaStream filters subscription usage, tags egress envelopes, translates
// Codex events back to the client format, and forwards chunks until the
// upstream closes.
func pumpMetaStream(ctx context.Context, e *metaExecutor, req pluginapi.ExecutorRequest, prepared *preparedMetaRequest, in <-chan pluginapi.HTTPStreamChunk, out chan<- pluginapi.ExecutorStreamChunk) {
	defer close(out)
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	var pendingEventLine []byte
	filteringSubscriptionUsage := false
	emit := func(line []byte) bool {
		if len(bytes.TrimSpace(line)) == 0 {
			return true
		}
		chunks := sdktranslator.TranslateStream(ctx, sdktranslator.FormatCodex, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, line, nil)
		for _, chunk := range chunks {
			if len(bytes.TrimSpace(chunk)) == 0 {
				continue
			}
			select {
			case out <- pluginapi.ExecutorStreamChunk{Payload: chunk}:
			case <-ctx.Done():
				return false
			}
		}
		return true
	}
	flushPending := func() bool {
		if pendingEventLine == nil {
			return true
		}
		line := pendingEventLine
		pendingEventLine = nil
		return emit(line)
	}
	for chunk := range in {
		if chunk.Err != nil {
			select {
			case out <- pluginapi.ExecutorStreamChunk{Err: providerkit.MapError(chunk.Err)}:
			case <-ctx.Done():
			}
			return
		}
		for _, line := range bytes.Split(chunk.Payload, []byte("\n")) {
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("event:")) {
				eventName := bytes.TrimSpace(trimmed[len("event:"):])
				if string(eventName) == metaSubscriptionUsageEventType {
					if !flushPending() {
						return
					}
					filteringSubscriptionUsage = true
					continue
				}
				filteringSubscriptionUsage = false
				if !flushPending() {
					return
				}
				pendingEventLine = bytes.Clone(line)
				continue
			}
			if !bytes.HasPrefix(trimmed, []byte("data:")) {
				if len(trimmed) == 0 {
					filteringSubscriptionUsage = false
				} else if filteringSubscriptionUsage {
					continue
				}
				if !flushPending() {
					return
				}
				if !emit(bytes.Clone(line)) {
					return
				}
				continue
			}
			eventData := bytes.TrimSpace(trimmed[len("data:"):])
			eventType := gjson.GetBytes(eventData, "type").String()
			if isMetaSubscriptionUsageEvent(eventType, eventData) {
				e.observeSubscriptionUsageEvent(ctx, req, eventData)
				pendingEventLine = nil
				filteringSubscriptionUsage = false
				continue
			}
			if filteringSubscriptionUsage {
				if eventType == "" {
					pendingEventLine = nil
					if gjson.ValidBytes(eventData) {
						filteringSubscriptionUsage = false
					}
					continue
				}
				filteringSubscriptionUsage = false
			}
			if streamEventErr := metaStreamEventError(eventData); streamEventErr != nil {
				select {
				case out <- pluginapi.ExecutorStreamChunk{Err: streamEventErr}:
				case <-ctx.Done():
				}
				return
			}
			eventData = prepared.replay.tagResponseEvent(eventData)
			switch eventType {
			case "response.output_item.done":
				collectMetaOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
			case "response.completed", "response.incomplete":
				eventData = patchMetaCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
			}
			if !flushPending() {
				return
			}
			if !emit(append([]byte("data: "), eventData...)) {
				return
			}
		}
	}
	flushPending()
}

// peekMetaStreamBootstrap reads chunks until the first SSE data event,
// reporting whether it carries a foreign-envelope rejection. Peeked bytes are
// replayed back through a rewound channel so the stream scanner consumes
// nothing.
func peekMetaStreamBootstrap(ctx context.Context, in <-chan pluginapi.HTTPStreamChunk) (<-chan pluginapi.HTTPStreamChunk, bool, error) {
	var peeked []pluginapi.HTTPStreamChunk
	var buf bytes.Buffer
	for {
		select {
		case <-ctx.Done():
			rewound := make(chan pluginapi.HTTPStreamChunk, len(peeked)+1)
			for _, c := range peeked {
				rewound <- c
			}
			close(rewound)
			return rewound, false, providerkit.MapError(ctx.Err())
		case chunk, ok := <-in:
			if !ok {
				rewound := make(chan pluginapi.HTTPStreamChunk, len(peeked)+1)
				for _, c := range peeked {
					rewound <- c
				}
				close(rewound)
				return rewound, false, nil
			}
			if chunk.Err != nil {
				rewound := make(chan pluginapi.HTTPStreamChunk, len(peeked)+1)
				for _, c := range peeked {
					rewound <- c
				}
				close(rewound)
				return rewound, false, providerkit.MapError(chunk.Err)
			}
			peeked = append(peeked, chunk)
			buf.Write(chunk.Payload)
			data := buf.Bytes()
			if idx := bytes.Index(data, []byte("data:")); idx >= 0 {
				rest := data[idx+len("data:"):]
				if end := bytes.IndexByte(rest, '\n'); end >= 0 {
					eventData := bytes.TrimSpace(rest[:end])
					if isMetaSubscriptionUsageEvent(gjson.GetBytes(eventData, "type").String(), eventData) {
						buf.Next(idx + len("data:") + end + 1)
						continue
					}
					retry := metaStreamEventError(eventData) != nil &&
						isMetaReasoningNotIssued(400, []byte(gjson.GetBytes(eventData, "error.message").String()))
					rewound := make(chan pluginapi.HTTPStreamChunk, len(peeked)+8)
					for _, c := range peeked {
						rewound <- c
					}
					go drainToChannel(in, rewound)
					return rewound, retry, nil
				}
			}
			if buf.Len() > 52_428_800 {
				rewound := make(chan pluginapi.HTTPStreamChunk, len(peeked)+8)
				for _, c := range peeked {
					rewound <- c
				}
				go drainToChannel(in, rewound)
				return rewound, false, nil
			}
		}
	}
}

func drainToChannel(in <-chan pluginapi.HTTPStreamChunk, out chan<- pluginapi.HTTPStreamChunk) {
	defer close(out)
	for chunk := range in {
		out <- chunk
	}
}

// translateMetaCompleted collects buffered SSE (non-streaming POST returns an
// event stream) into the terminal completed event, filtering subscription
// usage and tagging egress envelopes.
func translateMetaCompleted(_ context.Context, prepared *preparedMetaRequest, _ pluginapi.ExecutorRequest, data []byte) ([]byte, bool) {
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	var completed []byte
	var filteringSubscriptionUsage bool
	for _, line := range bytes.Split(data, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			filteringSubscriptionUsage = false
			continue
		}
		if bytes.HasPrefix(trimmed, []byte("event:")) {
			eventName := bytes.TrimSpace(trimmed[len("event:"):])
			filteringSubscriptionUsage = string(eventName) == metaSubscriptionUsageEventType
			continue
		}
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		eventData := bytes.TrimSpace(trimmed[len("data:"):])
		eventType := gjson.GetBytes(eventData, "type").String()
		if isMetaSubscriptionUsageEvent(eventType, eventData) {
			filteringSubscriptionUsage = false
			continue
		}
		if filteringSubscriptionUsage {
			if eventType == "" {
				if gjson.ValidBytes(eventData) {
					filteringSubscriptionUsage = false
				}
				continue
			}
			filteringSubscriptionUsage = false
		}
		if streamEventErr := metaStreamEventError(eventData); streamEventErr != nil {
			continue
		}
		eventData = prepared.replay.tagResponseEvent(eventData)
		switch eventType {
		case "response.output_item.done":
			collectMetaOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
		case "response.completed", "response.incomplete":
			eventData = patchMetaCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
			completed = eventData
		}
	}
	if completed != nil {
		return completed, true
	}
	if completed, ok := metaAsCompletedEvent(data); ok {
		completed = prepared.replay.tagResponseEvent(completed)
		completed = patchMetaCompletedOutput(completed, outputItemsByIndex, outputItemsFallback)
		return completed, true
	}
	return nil, false
}

// CountTokens counts Codex input tokens for the prepared body and translates
// the usage envelope back to the client format.
func (e *metaExecutor) CountTokens(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	prepared, err := e.prepare(req)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	enc, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		return pluginapi.ExecutorResponse{}, providerkit.MapError(fmt.Errorf("meta-muse executor: tokenizer init failed"))
	}
	count, err := countMetaCodexInputTokens(enc, prepared.body)
	if err != nil {
		return pluginapi.ExecutorResponse{}, providerkit.MapError(fmt.Errorf("meta-muse executor: token counting failed"))
	}
	usageJSON := []byte(fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count))
	translated := sdktranslator.TranslateTokenCount(ctx, sdktranslator.FormatCodex, prepared.responseFormat, count, usageJSON)
	return pluginapi.ExecutorResponse{Payload: translated}, nil
}

// HttpRequest injects Meta credentials and executes one raw upstream request
// through the host client.
func (e *metaExecutor) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	baseURL, token := metaCreds(req.StorageJSON, req.Metadata, req.Attributes)
	if baseURL == "" {
		baseURL = metaDefaultAPIBaseURL
	}
	headers := http.Header{}
	for k, v := range req.Headers {
		headers[k] = v
	}
	headers.Set("Content-Type", "application/json")
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	applyMetaClientIdentity(headers)
	resp, err := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{
		Method:  req.Method,
		URL:     req.URL,
		Headers: headers,
		Body:    req.Body,
	})
	if err != nil {
		return pluginapi.ExecutorHTTPResponse{}, providerkit.MapError(err)
	}
	return pluginapi.ExecutorHTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Body: resp.Body}, nil
}

// resolveCreds ensures a usable API key: mint from DCA inline when the auth
// has no key yet (the host-driven RefreshAuth path covers the rest).
func (e *metaExecutor) resolveCreds(ctx context.Context, req pluginapi.ExecutorRequest) (string, string, error) {
	baseURL, token := metaCreds(req.StorageJSON, req.AuthMetadata, req.AuthAttributes)
	if token != "" {
		return baseURL, token, nil
	}
	dca := metaDCAToken(req.StorageJSON, req.AuthMetadata, req.AuthAttributes)
	if dca == "" {
		return "", "", providerkit.MapError(fmt.Errorf("meta-muse executor: missing API key or access token"))
	}
	minted, err := mintMetaAPIKey(ctx, req.HTTPClient, dca)
	if err != nil || minted == nil {
		return "", "", providerkit.MapError(fmt.Errorf("meta-muse executor: mint API key failed"))
	}
	if strings.TrimSpace(minted.BaseURL) != "" {
		baseURL = strings.TrimSpace(minted.BaseURL)
	}
	return baseURL, minted.APIKey, nil
}

// observeSubscriptionUsage records buffered-SSE subscription usage into the
// passive quota cache keyed by auth identity.
func (e *metaExecutor) observeSubscriptionUsage(_ context.Context, req pluginapi.ExecutorRequest, body []byte) {
	if e.quota == nil {
		return
	}
	headers := parseMetaSubscriptionUsageSSE(body)
	if len(headers) == 0 {
		return
	}
	e.quota.observe(req.AuthID, headers)
}

func (e *metaExecutor) observeSubscriptionUsageEvent(_ context.Context, req pluginapi.ExecutorRequest, eventData []byte) {
	if e.quota == nil {
		return
	}
	headers := parseMetaSubscriptionUsageHeaders(eventData)
	if len(headers) == 0 {
		return
	}
	e.quota.observe(req.AuthID, headers)
}

// metaUpstreamHeaders builds the Muse Code identity headers for one upstream call.
func metaUpstreamHeaders(token string, stream bool, clientHeaders http.Header) http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	if strings.TrimSpace(token) != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	applyMetaClientIdentity(headers)
	if stream {
		headers.Set("Accept", "text/event-stream")
		headers.Set("Cache-Control", "no-cache")
	} else {
		headers.Set("Accept", "application/json")
	}
	for k, v := range clientHeaders {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "x-meta-") || strings.HasPrefix(lower, "x-muse-") {
			continue
		}
		if _, exists := headers[k]; !exists {
			headers[k] = v
		}
	}
	return headers
}

// applyMetaClientIdentity stamps Muse Code 1.4.14 identity headers. Session ids
// are a fresh UUID-7 per request so proxy traffic never reuses one Muse
// session across unrelated clients.
func applyMetaClientIdentity(headers http.Header) {
	headers.Set("User-Agent", fmt.Sprintf("muse-build/%s (non-interactive; %s-%s; build %s)", metaClientVersion, goosName(), goarchName(), metaClientBuild))
	headers.Set("X-Client-Id", metaClientSurface)
	if strings.TrimSpace(headers.Get("X-Tbh-Session-Id")) != "" {
		return
	}
	sessionID := uuid.Must(uuid.NewV7()).String()
	headers.Set("X-Tbh-Session-Id", sessionID)
	headers.Set("X-Meta-Ai-Gateway-Session-Id", sessionID)
	headers.Set("X-Tbh-Thread-Id", sessionID)
	headers.Set("X-Tbh-Root-Session-Id", sessionID)
	headers.Set("X-Tbh-Agent-Role", "main")
	headers.Set("X-Tbh-Contributor-Mode", "1")
}

// filterMetaEgressHeaders drops upstream X-Meta-* subscription headers before
// they can leak to the client; quota travels via the passive cache instead.
func filterMetaEgressHeaders(headers http.Header) http.Header {
	if len(headers) == 0 {
		return headers
	}
	out := make(http.Header, len(headers))
	for k, v := range headers {
		if strings.HasPrefix(strings.ToLower(k), "x-meta-") {
			continue
		}
		out[k] = v
	}
	return out
}

func setMetaStringIfDifferent(payload []byte, path, value string) []byte {
	if gjson.GetBytes(payload, path).Type == gjson.String && gjson.GetBytes(payload, path).String() == value {
		return payload
	}
	updated, err := sjson.SetBytes(payload, path, value)
	if err != nil {
		return payload
	}
	return updated
}

func setMetaBoolIfDifferent(payload []byte, path string, value bool) []byte {
	current := gjson.GetBytes(payload, path)
	if (value && current.Type == gjson.True) || (!value && current.Type == gjson.False) {
		return payload
	}
	updated, err := sjson.SetBytes(payload, path, value)
	if err != nil {
		return payload
	}
	return updated
}

func instructionsMissing(body []byte) bool {
	instructions := gjson.GetBytes(body, "instructions")
	return !instructions.Exists() || instructions.Type == gjson.Null
}

// countMetaCodexInputTokens counts input text tokens of a Codex body.
func countMetaCodexInputTokens(enc tokenizer.Codec, body []byte) (int64, error) {
	var total int64
	input := gjson.GetBytes(body, "input")
	if instructions := gjson.GetBytes(body, "instructions"); instructions.Type == gjson.String && instructions.String() != "" {
		n, err := enc.Count(instructions.String())
		if err != nil {
			return 0, err
		}
		total += int64(n)
	}
	if !input.IsArray() {
		return total, nil
	}
	for _, item := range input.Array() {
		for _, path := range []string{"content", "summary"} {
			value := item.Get(path)
			appendTokens := func(text string) error {
				if text == "" {
					return nil
				}
				n, err := enc.Count(text)
				if err != nil {
					return err
				}
				total += int64(n)
				return nil
			}
			switch {
			case value.Type == gjson.String:
				if err := appendTokens(value.String()); err != nil {
					return 0, err
				}
			case value.IsArray():
				for _, part := range value.Array() {
					text := part.Get("text").String()
					if text == "" {
						text = part.String()
					}
					if err := appendTokens(text); err != nil {
						return 0, err
					}
				}
			}
		}
	}
	return total, nil
}

// collectMetaOutputItemDone remembers output items by index for completed
// hydration, mirroring the native xaiCollectOutputItemDone helper.
func collectMetaOutputItemDone(eventData []byte, byIndex map[int64][]byte, fallback *[][]byte) {
	item := gjson.GetBytes(eventData, "item")
	if !item.Exists() || item.Type != gjson.JSON {
		return
	}
	if index := gjson.GetBytes(eventData, "output_index"); index.Exists() {
		byIndex[index.Int()] = []byte(item.Raw)
		return
	}
	*fallback = append(*fallback, []byte(item.Raw))
}

// patchMetaCompletedOutput hydrates an empty completed output from collected
// output items, mirroring the native patchCodexCompletedOutput helper.
func patchMetaCompletedOutput(eventData []byte, byIndex map[int64][]byte, fallback [][]byte) []byte {
	output := gjson.GetBytes(eventData, "response.output")
	if output.Exists() && output.IsArray() && len(output.Array()) > 0 {
		return eventData
	}
	if (!output.Exists() || !output.IsArray() || len(output.Array()) == 0) && len(byIndex) == 0 && len(fallback) == 0 {
		return eventData
	}
	indexes := make([]int64, 0, len(byIndex))
	for idx := range byIndex {
		indexes = append(indexes, idx)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	items := make([][]byte, 0, len(byIndex)+len(fallback))
	for _, idx := range indexes {
		items = append(items, byIndex[idx])
	}
	items = append(items, fallback...)
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(item)
	}
	buf.WriteByte(']')
	patched, err := sjson.SetRawBytes(eventData, "response.output", buf.Bytes())
	if err != nil {
		return eventData
	}
	return patched
}

func metaAsCompletedEvent(data []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(data)
	if !gjson.ValidBytes(trimmed) {
		return nil, false
	}
	root := gjson.ParseBytes(trimmed)
	switch root.Get("type").String() {
	case "response.completed", "response.incomplete":
		return trimmed, true
	}
	if root.Get("object").String() == "response" || root.Get("output").Exists() {
		wrapped, err := sjson.SetRawBytes([]byte(`{"type":"response.completed"}`), "response", trimmed)
		if err != nil {
			return nil, false
		}
		return wrapped, true
	}
	return nil, false
}

func isMetaReasoningNotIssuedFailure(completedEvent []byte) bool {
	msg := gjson.GetBytes(completedEvent, "response.error.message").String()
	if msg == "" {
		msg = gjson.GetBytes(completedEvent, "error.message").String()
	}
	if msg == "" {
		return false
	}
	return isMetaReasoningNotIssued(400, []byte(msg))
}

func isMetaSubscriptionUsageEvent(eventType string, eventData []byte) bool {
	if eventType == metaSubscriptionUsageEventType {
		return true
	}
	if eventType == "" && len(eventData) > 0 && gjson.GetBytes(eventData, "subscription").IsObject() {
		return true
	}
	return false
}

func metaStreamEventError(eventData []byte) error {
	eventType := gjson.GetBytes(eventData, "type").String()
	if eventType != "error" && eventType != "response.failed" {
		return nil
	}
	statusCode := http.StatusBadGateway
	if code := int(gjson.GetBytes(eventData, "error.code").Int()); code >= 400 && code <= 599 {
		statusCode = code
	}
	return wrapMetaUpstreamError(statusCode, eventData)
}

func wrapMetaUpstreamError(statusCode int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = fmt.Sprintf("meta-muse executor: upstream error (HTTP %d)", statusCode)
	}
	if len(msg) > 4096 {
		msg = msg[:4096]
	}
	return providerkit.MapError(&metaUpstreamError{statusCode: statusCode, message: msg})
}

type metaUpstreamError struct {
	statusCode int
	message    string
	retryAfter int64
}

func (e *metaUpstreamError) Error() string { return e.message }

func goosName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	default:
		return runtime.GOOS
	}
}

func goarchName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

var _ = log.Debugf

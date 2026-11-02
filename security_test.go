package main

// Regression tests for the Security-stage findings on the meta-muse executor
// PR: unsigned login-state SSRF, HttpRequest credential exfiltration to
// arbitrary URLs, case-sensitive client-header merge defeating session
// isolation, explicit default base_url losing to storage, fragmented SSE
// lines, the drain goroutine leak, swallowed buffered stream errors, and the
// stale quota-only README (covered by README content, not code).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// fakeSecurityHTTPClient records upstream calls and replays scripted answers.
type fakeSecurityHTTPClient struct {
	doCalls []pluginapi.HTTPRequest
	do      func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

func (f *fakeSecurityHTTPClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.doCalls = append(f.doCalls, req)
	if f.do != nil {
		return f.do(req)
	}
	return pluginapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"error":"authorization_pending"}`)}, nil
}

func (f *fakeSecurityHTTPClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, nil
}

// TestSecurityPollLoginIgnoresTamperedEndpoint proves a login state whose
// token_endpoint was rewritten by the caller still polls the hardcoded Meta
// endpoint, so the device_code cannot be exfiltrated to an arbitrary URL.
func TestSecurityPollLoginIgnoresTamperedEndpoint(t *testing.T) {
	evil := "https://evil.example.invalid/collect"
	tampered := encodeMetaLoginState(metaLoginState{
		DeviceCode:      "dc-1",
		TokenEndpoint:   evil,
		IntervalSeconds: 5,
		ExpiresAtUnix:   time.Now().Add(time.Hour).Unix(),
	})
	var raw metaLoginState
	decoded, err := base64.RawURLEncoding.DecodeString(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(decoded, &raw); err != nil || raw.TokenEndpoint != evil {
		t.Fatalf("test setup broken: tampered state = %+v err=%v", raw, err)
	}

	fake := &fakeSecurityHTTPClient{}
	resp, err := (&metaAuthProvider{}).PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{
		State: tampered, HTTPClient: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending (tampered poll must still reach Meta)", resp.Status)
	}
	if len(fake.doCalls) != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", len(fake.doCalls))
	}
	got := fake.doCalls[0]
	if got.URL != metaTokenEndpoint {
		t.Fatalf("poll URL = %q, want hardcoded %q", got.URL, metaTokenEndpoint)
	}
	if !strings.Contains(string(got.Body), "dc-1") {
		t.Fatalf("poll body lost the device_code: %q", got.Body)
	}
}

// TestSecurityPollLoginRejectsExpiredState proves an expired login state is
// rejected without any upstream request.
func TestSecurityPollLoginRejectsExpiredState(t *testing.T) {
	state := encodeMetaLoginState(metaLoginState{
		DeviceCode:      "dc-1",
		TokenEndpoint:   metaTokenEndpoint,
		IntervalSeconds: 5,
		ExpiresAtUnix:   time.Now().Add(-time.Minute).Unix(),
	})
	fake := &fakeSecurityHTTPClient{}
	resp, err := (&metaAuthProvider{}).PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{
		State: state, HTTPClient: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != pluginapi.AuthLoginStatusError || !strings.Contains(resp.Message, "expired") {
		t.Fatalf("status/message = %q/%q, want expired error", resp.Status, resp.Message)
	}
	if len(fake.doCalls) != 0 {
		t.Fatalf("upstream calls = %d, want 0 for expired state", len(fake.doCalls))
	}
}

// TestSecurityHttpRequestGatesURL proves the credential bearer token only
// leaves for https URLs on the resolved base-URL host (or the hardcoded Meta
// auth/mint hosts); off-host and non-https targets are rejected with no call.
func TestSecurityHttpRequestGatesURL(t *testing.T) {
	e := newMetaExecutor(nil, &metaAuthProvider{})
	attrs := map[string]string{"api_key": "test-key-1"}
	call := func(url string) (pluginapi.ExecutorHTTPResponse, error, *fakeSecurityHTTPClient) {
		fake := &fakeSecurityHTTPClient{
			do: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{}, Body: []byte(`{}`)}, nil
			},
		}
		resp, err := e.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{
			Method: "GET", URL: url, Headers: http.Header{},
			Attributes: attrs, HTTPClient: fake,
		})
		return resp, err, fake
	}

	if _, err, fake := call("https://evil.example.invalid/x"); err == nil || len(fake.doCalls) != 0 {
		t.Fatalf("off-host URL accepted (err=%v calls=%d), want rejection with no call", err, len(fake.doCalls))
	}
	if _, err, fake := call("http://api.meta.ai/v1/responses"); err == nil || len(fake.doCalls) != 0 {
		t.Fatalf("plain-http URL accepted (err=%v calls=%d), want rejection with no call", err, len(fake.doCalls))
	}
	resp, err, fake := call("https://api.meta.ai/v1/responses")
	if err != nil {
		t.Fatalf("on-host URL rejected: %v", err)
	}
	if len(fake.doCalls) != 1 || fake.doCalls[0].Headers.Get("Authorization") != "Bearer test-key-1" {
		t.Fatalf("on-host call = %+v, want one call carrying the credential", fake.doCalls)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestSecurityUpstreamHeadersDropClientDuplicates proves client headers that
// differ only in case from executor-owned identity/framing headers are
// dropped, so the per-request session stamp cannot be shadowed.
func TestSecurityUpstreamHeadersDropClientDuplicates(t *testing.T) {
	client := http.Header{
		"Authorization":    {"Bearer client-token"},
		"authorization":    {"Bearer client-token-lower"},
		"Content-Type":     {"text/plain"},
		"X-Tbh-Session-Id": {"client-session"},
		"x-tbh-session-id": {"client-session-lower"},
		"X-Meta-Foo":       {"client-meta"},
		"x-muse-bar":       {"client-muse"},
		"X-Client-Id":      {"client-id"},
		"User-Agent":       {"client-ua"},
		"X-Custom-Keep":    {"yes"},
		"X-Another-Custom": {"also-yes"},
	}
	first := metaUpstreamHeaders("server-token", true, client)
	second := metaUpstreamHeaders("server-token", true, client)

	count := func(h http.Header, name string) int {
		n := 0
		for k := range h {
			if strings.EqualFold(k, name) {
				n++
			}
		}
		return n
	}
	if got := first.Get("Authorization"); got != "Bearer server-token" {
		t.Fatalf("Authorization = %q, want server token", got)
	}
	if n := count(first, "authorization"); n != 1 {
		t.Fatalf("authorization header count = %d, want exactly 1: %v", n, first)
	}
	if got := first.Get("X-Tbh-Session-Id"); got == "" || got == "client-session" || got == "client-session-lower" {
		t.Fatalf("X-Tbh-Session-Id = %q, want a fresh executor-stamped session", got)
	}
	if n := count(first, "x-tbh-session-id"); n != 1 {
		t.Fatalf("x-tbh-session-id count = %d, want exactly 1: %v", n, first)
	}
	if first.Get("X-Meta-Foo") != "" || first.Get("X-Muse-Bar") != "" {
		t.Fatalf("client X-Meta-*/X-Muse-* leaked: %v", first)
	}
	if got := first.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := first.Get("X-Custom-Keep"); got != "yes" {
		t.Fatalf("unrelated client header dropped: %v", first)
	}
	if first.Get("X-Tbh-Session-Id") == second.Get("X-Tbh-Session-Id") {
		t.Fatalf("session id reused across requests: %q", first.Get("X-Tbh-Session-Id"))
	}
}

// TestSecurityHttpRequestDropsClientIdentity proves the same header isolation
// on the raw HttpRequest path: a smuggled lowercase authorization and session
// id never reach upstream.
func TestSecurityHttpRequestDropsClientIdentity(t *testing.T) {
	e := newMetaExecutor(nil, &metaAuthProvider{})
	fake := &fakeSecurityHTTPClient{
		do: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{}, Body: []byte(`{}`)}, nil
		},
	}
	_, err := e.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{
		Method: "GET", URL: "https://api.meta.ai/v1/responses",
		Headers:    http.Header{"authorization": {"Bearer client-token"}, "x-tbh-session-id": {"client-session"}},
		Attributes: map[string]string{"api_key": "test-key-1"},
		HTTPClient: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.doCalls) != 1 {
		t.Fatalf("calls = %d, want 1", len(fake.doCalls))
	}
	sent := fake.doCalls[0].Headers
	if got := sent.Get("Authorization"); got != "Bearer test-key-1" {
		t.Fatalf("upstream Authorization = %q, want credential token", got)
	}
	if got := sent.Get("X-Tbh-Session-Id"); got == "client-session" || got == "" {
		t.Fatalf("upstream X-Tbh-Session-Id = %q, want fresh stamp", got)
	}
}

// TestSecurityMetaCredsExplicitDefaultBaseURL proves an explicitly configured
// attribute base_url equal to the default is honored instead of losing to the
// stored base_url.
func TestSecurityMetaCredsExplicitDefaultBaseURL(t *testing.T) {
	storage := []byte(`{"type":"meta","base_url":"https://stored.example.invalid/v1","api_key":"stored-key"}`)
	base, _ := metaCreds(storage, nil, map[string]string{"base_url": metaDefaultAPIBaseURL})
	if base != metaDefaultAPIBaseURL {
		t.Fatalf("explicit default base_url lost to storage: %q", base)
	}
	base, _ = metaCreds(storage, nil, nil)
	if base != "https://stored.example.invalid/v1" {
		t.Fatalf("storage fallback broken without attributes: %q", base)
	}
}

// TestSecurityPumpCarriesPartialLine proves a data: line split across host
// bridge reads is reassembled before translation: one emitted chunk carries
// the whole marker and no error is reported.
func TestSecurityPumpCarriesPartialLine(t *testing.T) {
	ctx := context.Background()
	in := make(chan pluginapi.HTTPStreamChunk, 4)
	full := `data: {"type":"response.output_text.delta","item_id":"it_1","output_index":0,"content_index":0,"delta":"hello-split-marker-xyz"}` + "\n"
	split := strings.Index(full, "split-marker")
	if split < 0 {
		t.Fatal("test setup broken: marker missing")
	}
	in <- pluginapi.HTTPStreamChunk{Payload: []byte(full[:split])}
	in <- pluginapi.HTTPStreamChunk{Payload: []byte(full[split:])}
	close(in)
	out := make(chan pluginapi.ExecutorStreamChunk, 16)
	prepared := &preparedMetaRequest{replay: parityReplay("auth-a", ""), responseFormat: sdktranslator.FormatCodex}
	req := pluginapi.ExecutorRequest{Model: "muse-spark-1.3", OriginalRequest: []byte(`{"model":"muse-spark-1.3"}`)}
	pumpMetaStream(ctx, &metaExecutor{}, req, prepared, in, out)
	reassembled := false
	for c := range out {
		if c.Err != nil {
			t.Fatalf("split line produced stream error: %v", c.Err)
		}
		if strings.Contains(string(c.Payload), "hello-split-marker-xyz") {
			reassembled = true
		}
	}
	if !reassembled {
		t.Fatalf("split data: line never reassembled into one emitted chunk")
	}
}

// TestSecurityPumpFlushesUnterminatedTail proves a final line without a
// trailing newline is still processed at stream end instead of dropped.
func TestSecurityPumpFlushesUnterminatedTail(t *testing.T) {
	ctx := context.Background()
	in := make(chan pluginapi.HTTPStreamChunk, 2)
	in <- pluginapi.HTTPStreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r_tail\",\"output\":[]}}")}
	close(in)
	out := make(chan pluginapi.ExecutorStreamChunk, 16)
	prepared := &preparedMetaRequest{replay: parityReplay("auth-a", ""), responseFormat: sdktranslator.FormatCodex}
	req := pluginapi.ExecutorRequest{Model: "muse-spark-1.3", OriginalRequest: []byte(`{"model":"muse-spark-1.3"}`)}
	pumpMetaStream(ctx, &metaExecutor{}, req, prepared, in, out)
	found := false
	for c := range out {
		if c.Err != nil {
			t.Fatalf("tail produced stream error: %v", c.Err)
		}
		if strings.Contains(string(c.Payload), "r_tail") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unterminated trailing line dropped at stream end")
	}
}

// TestSecurityBufferedStreamErrorSurfaced proves buffered SSE error events are
// preserved: a genuine upstream failure surfaces with its message and status,
// a not-issued rejection stays recognizable so Execute can retry without
// reasoning, and an error-free disconnect reports no error event.
func TestSecurityBufferedStreamErrorSurfaced(t *testing.T) {
	ctx := context.Background()
	prepared := &preparedMetaRequest{replay: parityReplay("auth-a", "")}
	req := pluginapi.ExecutorRequest{}

	boom := []byte("data: {\"type\":\"error\",\"error\":{\"code\":500,\"message\":\"upstream boom\"}}\n")
	completed, errEvent := translateMetaCompleted(ctx, prepared, req, boom)
	if completed != nil || errEvent == nil || !strings.Contains(string(errEvent), "upstream boom") {
		t.Fatalf("upstream failure swallowed (completed=%s errEvent=%s), want event preserved", completed, errEvent)
	}
	if status := metaStreamEventStatus(errEvent); status != 500 {
		t.Fatalf("error status = %d, want 500", status)
	}

	notIssued := []byte("data: {\"type\":\"error\",\"error\":{\"code\":400,\"message\":\"reasoning encrypted_content was not issued to this caller\"}}\n")
	completed, errEvent = translateMetaCompleted(ctx, prepared, req, notIssued)
	if completed != nil || errEvent == nil {
		t.Fatalf("not-issued rejection lost (completed=%s errEvent=%s)", completed, errEvent)
	}
	if !isMetaReasoningNotIssued(metaStreamEventStatus(errEvent), metaStreamEventMessage(errEvent)) {
		t.Fatalf("not-issued rejection not recognizable (%s), want retry signal", errEvent)
	}

	bare := []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r_1\"}}\n")
	completed, errEvent = translateMetaCompleted(ctx, prepared, req, bare)
	if completed != nil || errEvent != nil {
		t.Fatalf("error-free disconnect misreported (completed=%s errEvent=%s)", completed, errEvent)
	}
}

// TestSecurityDrainCancellable proves the peek drain terminates on client
// disconnect instead of blocking forever on channels nobody reads.
func TestSecurityDrainCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan pluginapi.HTTPStreamChunk)
	out := make(chan pluginapi.HTTPStreamChunk)
	done := make(chan struct{})
	go func() { drainToChannel(ctx, in, out); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("drainToChannel blocked after context cancellation")
	}
}

// TestSecurityDrainForwardsThenCloses proves the cancellable drain still
// forwards every byte and closes out on a healthy stream.
func TestSecurityDrainForwardsThenCloses(t *testing.T) {
	ctx := context.Background()
	in := make(chan pluginapi.HTTPStreamChunk, 2)
	in <- pluginapi.HTTPStreamChunk{Payload: []byte("a")}
	in <- pluginapi.HTTPStreamChunk{Payload: []byte("b")}
	close(in)
	out := make(chan pluginapi.HTTPStreamChunk, 4)
	drainToChannel(ctx, in, out)
	var got strings.Builder
	for c := range out {
		got.Write(c.Payload)
	}
	if got.String() != "ab" {
		t.Fatalf("drained = %q, want %q", got.String(), "ab")
	}
}

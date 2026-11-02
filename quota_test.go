package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const testDCAToken = "dca:test-token-for-unit-tests"

func metaStorage(token string) []byte {
	raw, _ := json.Marshal(map[string]any{"type": "meta", "dca_token": token, "api_key": "stored-key", "label": "Example"})
	return raw
}

func envelopeFixture() string {
	return `{"api_key":"envelope-key","base_url":"https://api.meta.ai","user_email":"identity-address","full_name":"Identity Name","subs_tier_name":"Plus","subs_usage":{"tier":"plus","window":{"used_percent":25,"window_duration_mins":300,"resets_at":1893456000},"weekly":{"used_percent":10,"resets_at":1893456000}}}`
}

func TestExtractDCAToken(t *testing.T) {
	token, err := extractDCAToken(metaStorage(testDCAToken), nil)
	if err != nil || token != testDCAToken {
		t.Fatalf("storage token %q %v", token, err)
	}
	// Host metadata takes precedence over storage.
	token, err = extractDCAToken(metaStorage("dca:old-token"), map[string]any{"dca_token": testDCAToken})
	if err != nil || token != testDCAToken {
		t.Fatalf("metadata token %q %v", token, err)
	}
	for name, tc := range map[string]struct {
		storage  []byte
		metadata map[string]any
	}{
		"wrong type":     {[]byte(`{"type":"openrouter","dca_token":"` + testDCAToken + `"}`), nil},
		"missing token":  {[]byte(`{"type":"meta"}`), nil},
		"empty token":    {[]byte(`{"type":"meta","dca_token":""}`), nil},
		"bad prefix":     {[]byte(`{"type":"meta","dca_token":"bearer abc"}`), nil},
		"whitespace":     {[]byte(`{"type":"meta","dca_token":"dca:has space"}`), nil},
		"too long":       {[]byte(`{"type":"meta","dca_token":"dca:` + strings.Repeat("x", 4096) + `"}`), nil},
		"malformed JSON": {[]byte(`{"type":`), nil},
	} {
		if token, err := extractDCAToken(tc.storage, tc.metadata); err == nil || token != "" {
			t.Fatalf("%s accepted %q", name, token)
		} else if strings.Contains(err.Error(), testDCAToken) || strings.Contains(err.Error(), "old-token") {
			t.Fatalf("%s leaked credential: %v", name, err)
		}
	}
}

func TestNormalizeEnvelope(t *testing.T) {
	var env metaKeyEnvelope
	decoder := json.NewDecoder(bytes.NewReader([]byte(envelopeFixture())))
	decoder.UseNumber()
	if err := decoder.Decode(&env); err != nil {
		t.Fatal(err)
	}
	out := normalizeEnvelope(env, "envelope-key")
	if out.Subscription == nil || out.Subscription.Plan != "Meta" || out.Subscription.TierName != "Plus" {
		t.Fatalf("subscription %+v", out.Subscription)
	}
	if len(out.Groups) != 1 || len(out.Groups[0].Buckets) != 2 {
		t.Fatalf("groups %+v", out.Groups)
	}
	fiveHour, weekly := out.Groups[0].Buckets[0], out.Groups[0].Buckets[1]
	if fiveHour.Window != providerkit.CardWindowFiveHour || fiveHour.RemainingFraction != 0.75 || fiveHour.ResetTime != "2030-01-01T00:00:00Z" {
		t.Fatalf("5h bucket %+v", fiveHour)
	}
	if weekly.Window != providerkit.CardWindowWeekly || weekly.RemainingFraction != 0.9 || weekly.ResetTime != "2030-01-01T00:00:00Z" {
		t.Fatalf("weekly bucket %+v", weekly)
	}
	if len(out.Summary) != 0 {
		t.Fatalf("unexpected summary %+v", out.Summary)
	}
	// Same stored key: no warning. Differing stored key: boolean warning only.
	if out := normalizeEnvelope(env, "other-key"); len(out.Summary) != 1 || out.Summary[0].Key != "rotated" || out.Summary[0].Value != 1 || out.Summary[0].Format != "number" {
		t.Fatalf("rotated summary %+v", out.Summary)
	}
	// Tier falls back to the usage tier when the envelope names no plan tier.
	env.SubsTierName = ""
	if out := normalizeEnvelope(env, ""); out.Subscription.TierName != "plus" {
		t.Fatalf("tier fallback %+v", out.Subscription)
	}
	// Overuse caps at empty instead of going negative.
	env.SubsUsage.Window.UsedPercent = ptr(150)
	if out := normalizeEnvelope(env, ""); out.Groups[0].Buckets[0].RemainingFraction != 0 {
		t.Fatalf("uncapped overuse %+v", out.Groups[0].Buckets[0])
	}
	// Windows without a signal drop; a tier-only card stays valid (quota-blind).
	env.SubsUsage.Window.UsedPercent = nil
	env.SubsUsage.Weekly.UsedPercent = nil
	if out := normalizeEnvelope(env, ""); len(out.Groups) != 0 || out.Subscription.TierName != "plus" {
		t.Fatalf("tier-only card %+v", out)
	}
	// The normalized card never retains envelope secrets: neither values
	// nor identity keys survive normalization.
	raw, _ := json.Marshal(out)
	for _, secret := range []string{"envelope-key", "identity-address", "Identity Name", "user_email", "full_name"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("normalized card retains %q", secret)
		}
	}
}

func TestResetTime(t *testing.T) {
	if got := resetTime(float64(1893456000)); got != "2030-01-01T00:00:00Z" {
		t.Fatalf("unix seconds %q", got)
	}
	if got := resetTime("1893456000"); got != "2030-01-01T00:00:00Z" {
		t.Fatalf("unix string %q", got)
	}
	for _, raw := range []any{nil, "", "  ", "not-a-time", float64(-5), float64(0), 42} {
		if got := resetTime(raw); got != "" {
			t.Fatalf("unusable resets_at %v -> %q", raw, got)
		}
	}
}

func TestFetchMetaQuota(t *testing.T) {
	var method, auth, apiVersion, contentType string
	var body []byte
	var polls int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		method, auth, apiVersion, contentType = r.Method, r.Header.Get("Authorization"), r.Header.Get("X-Api-Version"), r.Header.Get("Content-Type")
		raw := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(raw)
		body = raw
		switch r.URL.Path {
		case "/muse-code/key":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(envelopeFixture()))
		case "/denied":
			w.WriteHeader(http.StatusUnauthorized)
		case "/limited":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/tier-only":
			_, _ = w.Write([]byte(`{"subs_tier_name":"Plus"}`))
		case "/empty":
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer s.Close()
	client := providerkit.NewQuotaHTTPClient(nil)
	defer client.CloseIdleConnections()

	out, err := fetchMetaQuota(context.Background(), client, s.URL+"/muse-code/key", testDCAToken, "envelope-key")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || auth != "Bearer "+testDCAToken || apiVersion != "1.0.0" || contentType != "application/json" || string(body) != "{}" {
		t.Fatalf("request %s auth=%q version=%q content=%q body=%q", method, auth, apiVersion, contentType, body)
	}
	if len(out.Groups) != 1 || len(out.Groups[0].Buckets) != 2 || polls != 1 {
		t.Fatalf("quota %+v polls=%d", out, polls)
	}
	if _, err := fetchMetaQuota(context.Background(), client, s.URL+"/denied", testDCAToken, ""); providerkit.MapError(err).Code != "invalid_credentials" {
		t.Fatalf("denied mapped %+v", providerkit.MapError(err))
	}
	if _, err := fetchMetaQuota(context.Background(), client, s.URL+"/limited", testDCAToken, ""); true {
		e := providerkit.MapError(err)
		if e.Code != "quota_exhausted" || !e.Retryable || e.RetryAfterSeconds != 7 {
			t.Fatalf("limited mapped %+v", e)
		}
		if strings.Contains(err.Error(), testDCAToken) {
			t.Fatalf("error leaked credential: %v", err)
		}
	}
	// Tier-only envelopes stay valid with no buckets; fully empty ones fail.
	if out, err := fetchMetaQuota(context.Background(), client, s.URL+"/tier-only", testDCAToken, ""); err != nil || len(out.Groups) != 0 || out.Subscription.TierName != "Plus" {
		t.Fatalf("tier-only %+v %v", out, err)
	}
	if _, err := fetchMetaQuota(context.Background(), client, s.URL+"/empty", testDCAToken, ""); err == nil {
		t.Fatal("empty envelope accepted")
	}
}

func TestQuotaProviderCache(t *testing.T) {
	var polls int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		_, _ = w.Write([]byte(envelopeFixture()))
	}))
	defer s.Close()
	client := providerkit.NewQuotaHTTPClient(nil)
	defer client.CloseIdleConnections()
	q := newQuotaProvider(client)
	q.endpoint = s.URL + "/muse-code/key"

	req := pluginapi.QuotaFetchRequest{AuthID: "a", Provider: "meta", StorageJSON: metaStorage(testDCAToken)}
	one, err := q.cache.FetchQuota(context.Background(), req)
	if err != nil || len(one.Groups) != 1 || polls != 1 {
		t.Fatalf("fetch %+v polls=%d %v", one, polls, err)
	}
	if _, err := q.cache.FetchQuota(context.Background(), req); err != nil || polls != 1 {
		t.Fatalf("cache missed, polls=%d %v", polls, err)
	}
	other := req
	other.AuthID = "b"
	if _, err := q.cache.FetchQuota(context.Background(), other); err != nil || polls != 2 {
		t.Fatalf("cross-account cache, polls=%d %v", polls, err)
	}
	// Non-Meta credentials are rejected without polling.
	bad := req
	bad.StorageJSON = []byte(`{"type":"openrouter","api_key":"example-key"}`)
	if _, err := q.cache.FetchQuota(context.Background(), bad); err == nil || polls != 2 {
		t.Fatalf("non-meta accepted, polls=%d %v", polls, err)
	}
	desc, err := q.cache.DescribeQuota(context.Background(), pluginapi.QuotaDescribeRequest{})
	if err != nil || desc.SupportsReset || len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != "meta" {
		t.Fatalf("describe %+v %v", desc, err)
	}
	if _, err := q.cache.ResetQuota(context.Background(), pluginapi.QuotaResetRequest{}); err == nil {
		t.Fatal("reset accepted")
	}
}

// The 30-minute TTL value is asserted directly: expiry mechanics of
// NewCachedQuotaWithTTL are covered by the kit's own clock-controlled tests,
// so this guards the wiring (the constant the provider passes) without
// wall-clock sleeps.
func TestQuotaCacheTTLValue(t *testing.T) {
	if quotaCacheTTL != 30*time.Minute {
		t.Fatalf("quota cache TTL = %v, want 30m", quotaCacheTTL)
	}
}

func ptr(v float64) *float64 { return &v }

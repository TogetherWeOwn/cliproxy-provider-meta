package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/tidwall/gjson"
)

func unwrapMetaRPCError(t *testing.T, err error) *pluginabi.Error {
	t.Helper()
	var rpcErr *pluginabi.Error
	if !errors.As(err, &rpcErr) || rpcErr == nil {
		t.Fatalf("error is %T (%v), want *pluginabi.Error", err, err)
	}
	return rpcErr
}

// TestWrapMetaUpstreamErrorSurfacesCode proves an attestation-shaped upstream
// body surfaces Meta's error token instead of the sanitized kit message, with
// the upstream status preserved.
func TestWrapMetaUpstreamErrorSurfacesCode(t *testing.T) {
	body := []byte(`{"error":{"code":"client_attestation_denied","message":"attestation failed"}}`)
	rpcErr := unwrapMetaRPCError(t, wrapMetaUpstreamError(403, body))
	if rpcErr.Code != "client_attestation_denied" {
		t.Fatalf("Code = %q, want client_attestation_denied", rpcErr.Code)
	}
	if want := "Meta request failed (HTTP 403: client_attestation_denied)"; rpcErr.Message != want {
		t.Fatalf("Message = %q, want %q", rpcErr.Message, want)
	}
	if rpcErr.HTTPStatus != 403 {
		t.Fatalf("HTTPStatus = %d, want 403", rpcErr.HTTPStatus)
	}
	if strings.Contains(rpcErr.Message, "attestation failed") || strings.Contains(rpcErr.Message, string(body)) {
		t.Fatalf("client error leaks upstream content: %q", rpcErr.Message)
	}
}

// TestWrapMetaUpstreamErrorFallsBack proves malformed or absent codes keep the
// current sanitized behavior with the upstream status preserved.
func TestWrapMetaUpstreamErrorFallsBack(t *testing.T) {
	longCode := strings.Repeat("a", 65)
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"malformed json", 403, `not json`},
		{"empty body", 502, ``},
		{"absent message", 400, `{"error":{"type":"invalid_request_error"}}`},
		{"numeric code", 500, `{"type":"error","error":{"code":500,"message":"upstream boom"}}`},
		{"uppercase start", 403, `{"error":{"code":"Client_Denied"}}`},
		{"bad chars", 403, `{"error":{"code":"denied; rm -rf"}}`},
		{"too long", 403, `{"error":{"code":"` + longCode + `"}}`},
		{"empty code", 403, `{"error":{"code":""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rpcErr := unwrapMetaRPCError(t, wrapMetaUpstreamError(tc.status, []byte(tc.body)))
			if rpcErr.Code != "plugin_error" {
				t.Fatalf("Code = %q, want plugin_error", rpcErr.Code)
			}
			if rpcErr.Message != "plugin operation failed" {
				t.Fatalf("Message = %q, want sanitized fallback", rpcErr.Message)
			}
			if rpcErr.HTTPStatus != tc.status {
				t.Fatalf("HTTPStatus = %d, want %d", rpcErr.HTTPStatus, tc.status)
			}
		})
	}
}

// TestWrapMetaUpstreamErrorPassesThroughToolChoiceRejection covers the
// forced-tool_choice repro: Meta rejects it with a 400 whose actionable
// message (plus type and param) must reach the client instead of the generic
// "plugin operation failed". The payload is rebuilt field by field, so the
// host renders it in both the OpenAI and Claude error shapes.
func TestWrapMetaUpstreamErrorPassesThroughToolChoiceRejection(t *testing.T) {
	const upstreamMessage = `only "auto" is supported for tool_choice. "none", "required", and named function choices are not currently supported`
	body := []byte(`{"error":{"message":"only \"auto\" is supported for tool_choice. \"none\", \"required\", and named function choices are not currently supported","type":"invalid_request_error","param":"tool_choice","code":null}}`)
	rpcErr := unwrapMetaRPCError(t, wrapMetaUpstreamError(400, body))
	if rpcErr.HTTPStatus != 400 {
		t.Fatalf("HTTPStatus = %d, want 400", rpcErr.HTTPStatus)
	}
	if rpcErr.Code != "invalid_request_error" {
		t.Fatalf("Code = %q, want invalid_request_error (from upstream type)", rpcErr.Code)
	}
	if got := gjson.Get(rpcErr.Message, "error.message").String(); got != upstreamMessage {
		t.Fatalf("error.message = %q, want %q", got, upstreamMessage)
	}
	if got := gjson.Get(rpcErr.Message, "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error", got)
	}
	if got := gjson.Get(rpcErr.Message, "error.param").String(); got != "tool_choice" {
		t.Fatalf("error.param = %q, want tool_choice", got)
	}
	if code := gjson.Get(rpcErr.Message, "error.code"); code.Exists() {
		t.Fatalf("error.code = %v, want absent for a null upstream code", code)
	}
}

// TestWrapMetaUpstreamErrorPassesThroughCodeAndMessage proves a rejection
// carrying both a code token and a message surfaces both, in the RPC code
// and in the payload.
func TestWrapMetaUpstreamErrorPassesThroughCodeAndMessage(t *testing.T) {
	body := []byte(`{"error":{"type":"invalid_request_error","code":"model_not_found","message":"model not found"}}`)
	rpcErr := unwrapMetaRPCError(t, wrapMetaUpstreamError(404, body))
	if rpcErr.Code != "model_not_found" {
		t.Fatalf("Code = %q, want model_not_found", rpcErr.Code)
	}
	if got := gjson.Get(rpcErr.Message, "error.code").String(); got != "model_not_found" {
		t.Fatalf("error.code = %q, want model_not_found", got)
	}
	if got := gjson.Get(rpcErr.Message, "error.message").String(); got != "model not found" {
		t.Fatalf("error.message = %q, want model not found", got)
	}
	if rpcErr.HTTPStatus != 404 {
		t.Fatalf("HTTPStatus = %d, want 404", rpcErr.HTTPStatus)
	}
}

// TestWrapMetaUpstreamErrorPassesThroughResponseFailedShape proves the
// response.failed stream shape (response.error.*) passes through the same
// fields as the plain error shape.
func TestWrapMetaUpstreamErrorPassesThroughResponseFailedShape(t *testing.T) {
	body := []byte(`{"type":"response.failed","response":{"error":{"message":"bad input shape","type":"invalid_request_error","param":"input[3]","code":"invalid_input"}}}`)
	rpcErr := unwrapMetaRPCError(t, wrapMetaUpstreamError(400, body))
	if rpcErr.Code != "invalid_input" {
		t.Fatalf("Code = %q, want invalid_input", rpcErr.Code)
	}
	if got := gjson.Get(rpcErr.Message, "error.message").String(); got != "bad input shape" {
		t.Fatalf("error.message = %q, want bad input shape", got)
	}
	if got := gjson.Get(rpcErr.Message, "error.param").String(); got != "input[3]" {
		t.Fatalf("error.param = %q, want input[3]", got)
	}
}

// TestWrapMetaUpstreamErrorPassthroughGates proves the message passthrough
// stays closed where upstream text must not surface verbatim: credential and
// attestation statuses (401/403) and server errors keep the sanitized
// mapping, and unusable messages fall back the same way.
func TestWrapMetaUpstreamErrorPassthroughGates(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		secret string
	}{
		{"unauthorized", 401, `{"error":{"message":"api key abc123 is invalid","type":"authentication_error"}}`, "abc123"},
		{"forbidden without code", 403, `{"error":{"message":"attestation detail xyz"}}`, "xyz"},
		{"server error", 500, `{"error":{"message":"internal stack trace here"}}`, "stack trace"},
		{"bad gateway", 502, `{"error":{"message":"upstream connect detail"}}`, "connect detail"},
		{"non-string message", 400, `{"error":{"message":{"nested":"nope"}}}`, "nope"},
		{"whitespace message", 400, `{"error":{"message":"  \n\t "}}`, ""},
		{"malformed json at 400", 400, `not json`, "not json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rpcErr := unwrapMetaRPCError(t, wrapMetaUpstreamError(tc.status, []byte(tc.body)))
			if rpcErr.Message != "plugin operation failed" {
				t.Fatalf("Message = %q, want sanitized fallback", rpcErr.Message)
			}
			if tc.secret != "" && strings.Contains(rpcErr.Message, tc.secret) {
				t.Fatalf("client error leaks upstream content: %q", rpcErr.Message)
			}
			if rpcErr.HTTPStatus != tc.status {
				t.Fatalf("HTTPStatus = %d, want %d", rpcErr.HTTPStatus, tc.status)
			}
		})
	}
}

// TestWrapMetaUpstreamErrorPassthroughSanitizes proves the rebuilt payload
// carries only the extracted fields: control characters and newlines are
// collapsed, overlong text is capped, and unrelated body content (including
// anything credential-shaped) never reaches the client.
func TestWrapMetaUpstreamErrorPassthroughSanitizes(t *testing.T) {
	long := strings.Repeat("word ", 300)
	body := []byte(`{"error":{"message":"  first line\nsecond\u0001line ` + long + `","type":"invalid_request_error","debug":"sekret-debug"},"authorization":"Bearer sekret-token"}`)
	rpcErr := unwrapMetaRPCError(t, wrapMetaUpstreamError(400, body))
	if strings.Contains(rpcErr.Message, "sekret") {
		t.Fatalf("client error leaks non-error body content: %q", rpcErr.Message)
	}
	message := gjson.Get(rpcErr.Message, "error.message").String()
	if message == "" {
		t.Fatalf("error.message missing from payload %q", rpcErr.Message)
	}
	for _, r := range message {
		if unicode.IsControl(r) {
			t.Fatalf("error.message contains control rune %q: %q", r, message)
		}
	}
	if strings.Contains(message, "  ") || strings.HasPrefix(message, " ") {
		t.Fatalf("error.message whitespace not collapsed: %q", message)
	}
	if !strings.HasPrefix(message, "first line second line word") {
		t.Fatalf("error.message = %q, want collapsed prefix", message)
	}
	if n := utf8.RuneCountInString(message); n > 512 {
		t.Fatalf("error.message is %d runes, want <= 512", n)
	}
}

// TestWrapMetaUpstreamErrorAcceptsTokenChars proves dotted/dashed tokens
// within the length limit surface as codes.
func TestWrapMetaUpstreamErrorAcceptsTokenChars(t *testing.T) {
	body := []byte(`{"error":{"code":"quota.exhausted-high"}}`)
	rpcErr := unwrapMetaRPCError(t, wrapMetaUpstreamError(429, body))
	if rpcErr.Code != "quota.exhausted-high" {
		t.Fatalf("Code = %q, want quota.exhausted-high", rpcErr.Code)
	}
	if want := "Meta request failed (HTTP 429: quota.exhausted-high)"; rpcErr.Message != want {
		t.Fatalf("Message = %q, want %q", rpcErr.Message, want)
	}
}

// TestWrapMetaUpstreamErrorQuotaTopLevelBody proves the observed account-wide
// quota body — top-level code/message, not nested under error — maps to a
// credential-scoped 429. Classification reads the top-level shape; the
// sanitized-message rules are untouched, so the client still sees the generic
// mapping rather than upstream text.
func TestWrapMetaUpstreamErrorQuotaTopLevelBody(t *testing.T) {
	body := []byte(`{"code":"rate_limit_exceeded","message":"Subscription quota exhausted. Your usage window resets at 2026-11-11T02:59:37Z."}`)
	rpcErr := unwrapMetaRPCError(t, wrapMetaUpstreamError(429, body))
	if rpcErr.HTTPStatus != 429 {
		t.Fatalf("HTTPStatus = %d, want 429", rpcErr.HTTPStatus)
	}
	if !rpcErr.CredentialScoped || !rpcErr.IsCredentialScoped() {
		t.Fatalf("CredentialScoped = %v, want true for an account-wide quota 429", rpcErr.CredentialScoped)
	}
	if rpcErr.Code != "plugin_error" || rpcErr.Message != "plugin operation failed" {
		t.Fatalf("Code/Message = %q/%q, want the sanitized fallback for a top-level body", rpcErr.Code, rpcErr.Message)
	}
}

// TestWrapMetaUpstreamErrorQuotaNestedShape proves the nested shape (and its
// numeric resets_at) maps to credential-scoped with a reset-time retry-after.
func TestWrapMetaUpstreamErrorQuotaNestedShape(t *testing.T) {
	resetsAt := time.Now().Add(time.Hour).Unix()
	body := []byte(fmt.Sprintf(`{"error":{"code":"rate_limit_exceeded","message":"Subscription quota exhausted.","resets_at":%d}}`, resetsAt))
	rpcErr := unwrapMetaRPCError(t, wrapMetaUpstreamError(429, body))
	if !rpcErr.CredentialScoped {
		t.Fatalf("CredentialScoped = false, want true for a quota 429 with resets_at")
	}
	if rpcErr.RetryAfterSeconds <= 0 || rpcErr.RetryAfterSeconds > 3601 {
		t.Fatalf("RetryAfterSeconds = %d, want roughly one hour until the reset", rpcErr.RetryAfterSeconds)
	}
}

// TestWrapMetaUpstreamErrorQuotaMessageResetCarriesSeconds proves a
// "resets at <RFC3339>" message (no resets_at field) still yields a
// retry-after hint on the wrapped error.
func TestWrapMetaUpstreamErrorQuotaMessageResetCarriesSeconds(t *testing.T) {
	reset := time.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339)
	body := []byte(fmt.Sprintf(`{"code":"rate_limit_exceeded","message":"Subscription quota exhausted. Your usage window resets at %s."}`, reset))
	rpcErr := unwrapMetaRPCError(t, wrapMetaUpstreamError(429, body))
	if !rpcErr.CredentialScoped {
		t.Fatalf("CredentialScoped = false, want true for a quota 429")
	}
	if rpcErr.RetryAfterSeconds <= 2690 || rpcErr.RetryAfterSeconds > 2701 {
		t.Fatalf("RetryAfterSeconds = %d, want roughly 45 minutes until the reset", rpcErr.RetryAfterSeconds)
	}
}

// TestWrapMetaUpstreamErrorPlain429NotScoped proves an ordinary per-model
// rate limit (no quota phrasing, no resets_at) stays model-scoped with no
// retry-after hint, so the host keeps its per-model 429 failover.
func TestWrapMetaUpstreamErrorPlain429NotScoped(t *testing.T) {
	body := []byte(`{"error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded, slow down."}}`)
	rpcErr := unwrapMetaRPCError(t, wrapMetaUpstreamError(429, body))
	if rpcErr.CredentialScoped {
		t.Fatalf("CredentialScoped = true, want false for a plain 429")
	}
	if rpcErr.RetryAfterSeconds != 0 {
		t.Fatalf("RetryAfterSeconds = %d, want 0 without a reset", rpcErr.RetryAfterSeconds)
	}
}

// TestIsMetaSubscriptionQuota pins the classification arms and its gates:
// quota phrasing in a message (any scope), or a quota-shaped code with a
// resets_at field; non-429s never classify.
func TestIsMetaSubscriptionQuota(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"message arm top-level", 429, `{"code":"rate_limit_exceeded","message":"Subscription quota exhausted. Your usage window resets at 2026-11-11T02:59:37Z."}`, true},
		{"message arm nested", 429, `{"error":{"message":"Your subscription quota is exhausted"}}`, true},
		{"message arm response.failed", 429, `{"type":"response.failed","response":{"error":{"message":"quota exhausted"}}}`, true},
		{"code arm nested resets_at", 429, `{"error":{"code":"rate_limit_exceeded","resets_at":1893456000}}`, true},
		{"code arm top-level resets_at", 429, `{"code":"rate_limit_exceeded","resets_at":1893456000}`, true},
		{"quota code without resets_at", 429, `{"error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded"}}`, false},
		{"plain 429", 429, `{"error":{"code":"rate_limit_exceeded","message":"Too many requests"}}`, false},
		{"non-429 with quota text", 500, `{"error":{"message":"Subscription quota exhausted"}}`, false},
		{"404 with quota text", 404, `{"error":{"message":"Subscription quota exhausted","resets_at":1893456000}}`, false},
		{"empty body", 429, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMetaSubscriptionQuota(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("isMetaSubscriptionQuota(%d, %s) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestMetaResetRetryAfterSeconds pins the exact reset arithmetic with a fixed
// now: the real incident body yields 3577s from 02:00:00Z to the 02:59:37Z
// reset, and only 429/404 statuses read a reset at all.
func TestMetaResetRetryAfterSeconds(t *testing.T) {
	now := time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"real incident body", 429, `{"code":"rate_limit_exceeded","message":"Subscription quota exhausted. Your usage window resets at 2026-11-11T02:59:37Z."}`, 3577},
		{"numeric resets_at", 429, `{"error":{"code":"rate_limit_exceeded","resets_at":1893456000}}`, int(int64(1893456000) - now.Unix())},
		{"404 reads a reset", 404, `{"error":{"resets_at":1893456000}}`, int(int64(1893456000) - now.Unix())},
		{"500 ignores a reset", 500, `{"error":{"resets_at":1893456000}}`, 0},
		{"past reset", 429, `{"code":"rate_limit_exceeded","message":"Subscription quota exhausted. Your usage window resets at 2026-11-11T01:59:37Z."}`, 0},
		{"missing resets_at", 429, `{"error":{"message":"Rate limit exceeded"}}`, 0},
		{"empty body", 429, ``, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := metaResetRetryAfterSeconds(tc.status, []byte(tc.body), now); got != tc.want {
				t.Fatalf("metaResetRetryAfterSeconds(%d, %s) = %d, want %d", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestMetaStreamEventErrorQuotaCarriesPair proves stream error events carry
// the same credential-scoped pair as the HTTP path: a numeric 429 code on an
// error (or response.failed) event maps scoped with a retry-after, and
// non-error events are not errors.
func TestMetaStreamEventErrorQuotaCarriesPair(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	for _, tc := range []struct{ name, event string }{
		{"error event", fmt.Sprintf(`{"type":"error","error":{"code":429,"message":"Subscription quota exhausted. Your usage window resets at %s."}}`, reset)},
		{"response.failed event", fmt.Sprintf(`{"type":"response.failed","response":{"error":{"code":429,"message":"quota exhausted, resets at %s"}}}`, reset)},
		{"string-code error event", fmt.Sprintf(`{"type":"error","error":{"code":"rate_limit_exceeded","message":"Subscription quota exhausted. Your usage window resets at %s."}}`, reset)},
		{"string-code response.failed event", fmt.Sprintf(`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Subscription quota exhausted. Your usage window resets at %s."}}}`, reset)},
		{"top-level string-code event", fmt.Sprintf(`{"type":"error","code":"rate_limit_exceeded","message":"Subscription quota exhausted. Your usage window resets at %s."}`, reset)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpcErr := unwrapMetaRPCError(t, metaStreamEventError([]byte(tc.event)))
			if rpcErr.HTTPStatus != 429 {
				t.Fatalf("HTTPStatus = %d, want 429", rpcErr.HTTPStatus)
			}
			if !rpcErr.CredentialScoped {
				t.Fatalf("CredentialScoped = false, want true on a quota stream error")
			}
			if rpcErr.RetryAfterSeconds <= 1790 || rpcErr.RetryAfterSeconds > 1801 {
				t.Fatalf("RetryAfterSeconds = %d, want roughly 30 minutes until the reset", rpcErr.RetryAfterSeconds)
			}
		})
	}
	// A plain (non-quota) rate limit still infers 429 on the wire but must not
	// cool the whole credential, and an unrelated string code stays 502.
	plain := unwrapMetaRPCError(t, metaStreamEventError([]byte(`{"type":"error","error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded, slow down."}}`)))
	if plain.HTTPStatus != 429 {
		t.Fatalf("plain rate-limit HTTPStatus = %d, want 429", plain.HTTPStatus)
	}
	if plain.CredentialScoped {
		t.Fatalf("plain rate-limit CredentialScoped = true, want false")
	}
	unrelated := unwrapMetaRPCError(t, metaStreamEventError([]byte(`{"type":"error","error":{"code":"client_attestation_denied","message":"bad credential"}}`)))
	if unrelated.HTTPStatus != 502 {
		t.Fatalf("unrelated-code HTTPStatus = %d, want 502", unrelated.HTTPStatus)
	}
	if err := metaStreamEventError([]byte(`{"type":"response.completed"}`)); err != nil {
		t.Fatalf("non-error event err = %v, want nil", err)
	}
}

// TestMapErrorKeepsScopedPairForErrorDetail proves the stream-close carrier:
// the kit runtime sends chunk errors as error_detail through MapError, and a
// mapped *pluginabi.Error passes through with the scoped pair intact.
func TestMapErrorKeepsScopedPairForErrorDetail(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339)
	body := []byte(fmt.Sprintf(`{"code":"rate_limit_exceeded","message":"Subscription quota exhausted. Your usage window resets at %s."}`, reset))
	wrapped := unwrapMetaRPCError(t, wrapMetaUpstreamError(429, body))
	remapped := providerkit.MapError(wrapped)
	if !remapped.CredentialScoped {
		t.Fatalf("remapped CredentialScoped = false, want true")
	}
	if remapped.RetryAfterSeconds != wrapped.RetryAfterSeconds || remapped.RetryAfterSeconds <= 1190 {
		t.Fatalf("remapped RetryAfterSeconds = %d, want wrapped %d", remapped.RetryAfterSeconds, wrapped.RetryAfterSeconds)
	}
}

package main

import (
	"errors"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

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

// TestWrapMetaUpstreamErrorPassesThroughToolChoiceRejection is the TWO-1797
// repro: Meta rejects a forced tool_choice with a 400 whose actionable
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

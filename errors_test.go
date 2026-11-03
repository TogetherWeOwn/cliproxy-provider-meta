package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
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
		{"absent code", 400, `{"error":{"message":"bad request"}}`},
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

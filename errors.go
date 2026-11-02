package main

import (
	"net/http"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// classifyPollError maps a quota-poll failure to a safe public error. The
// kit helper already redacts URLs, headers, bodies and transport text; this
// layer only relabels statuses the host and panel act on. No credential or
// response content is ever copied into an error.
func classifyPollError(err error) *pluginabi.Error {
	e := providerkit.MapError(err)
	switch e.HTTPStatus {
	case http.StatusUnauthorized, http.StatusForbidden:
		e.Code, e.Message, e.Retryable, e.RetryAfterSeconds =
			"invalid_credentials", "Meta rejected the credential", false, 0
	case http.StatusTooManyRequests:
		e.Code, e.Message, e.Retryable =
			"quota_exhausted", "Meta quota or rate limit reached", true
		if e.RetryAfterSeconds <= 0 {
			e.RetryAfterSeconds = 60
		}
	case http.StatusRequestTimeout:
		e.Code, e.Message, e.Retryable =
			"upstream_error", "Meta request failed", true
	default:
		if e.HTTPStatus >= 500 {
			e.Code, e.Message, e.Retryable =
				"upstream_error", "Meta request failed", true
		}
	}
	return e
}

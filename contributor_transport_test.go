package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// This file intentionally uses only the released executor API and fixtures, so
// Actions can run the same transport regression against the pre-fix release.
type contributorHTTPClient struct {
	calls         []pluginapi.HTTPRequest
	rejectFirst   bool
	bootstrap     bool
	nestedFailure bool
}

func (f *contributorHTTPClient) record(req pluginapi.HTTPRequest) {
	req.Headers = req.Headers.Clone()
	req.Body = bytes.Clone(req.Body)
	f.calls = append(f.calls, req)
}

func (f *contributorHTTPClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.record(req)
	if f.rejectFirst && len(f.calls) == 1 {
		return pluginapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"error":{"message":"reasoning encrypted_content was not issued to this caller"}}`)}, nil
	}
	return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(translatePathUpstreamSSE)}, nil
}

func (f *contributorHTTPClient) DoStream(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	f.record(req)
	status, body := 200, translatePathUpstreamSSE
	if f.rejectFirst && len(f.calls) == 1 {
		body = `{"error":{"message":"reasoning encrypted_content was not issued to this caller"}}`
		status = 400
		if f.bootstrap {
			status = 200
			body = "data: {\"type\":\"error\",\"error\":{\"message\":\"reasoning encrypted_content was not issued to this caller\"}}\n\n"
			if f.nestedFailure {
				body = "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"reasoning encrypted_content was not issued to this caller\"}}}\n\n"
			}
		}
	}
	ch := make(chan pluginapi.HTTPStreamChunk, len(body))
	// Deterministic one-byte reads cover every possible SSE line split.
	for i := range body {
		ch <- pluginapi.HTTPStreamChunk{Payload: []byte(body[i : i+1])}
	}
	close(ch)
	return pluginapi.HTTPStreamResponse{StatusCode: status, Chunks: ch}, nil
}

func assertContributorHeader(t *testing.T, headers http.Header, want bool) {
	t.Helper()
	keys, values := 0, 0
	for key, vs := range headers {
		if strings.EqualFold(strings.TrimSpace(key), "X-Tbh-Contributor-Mode") {
			keys++
			values += len(vs)
			if len(vs) != 1 || vs[0] != "1" {
				t.Fatalf("invalid contributor values: %v", vs)
			}
		}
	}
	if want && (keys != 1 || values != 1) || !want && keys != 0 {
		t.Fatalf("contributor keys/values = %d/%d, want opt-in=%t", keys, values, want)
	}
}

func executeContributorRequest(t *testing.T, req pluginapi.ExecutorRequest, stream bool) {
	t.Helper()
	e := newMetaExecutor(nil, &metaAuthProvider{})
	if !stream {
		if _, err := e.Execute(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := e.ExecuteStream(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range resp.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
}

func TestContributorTransportMatrix(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, family := range []string{"muse-spark-1.3", "muse-wonder-1.0", "muse-wonder-1.0-full"} {
			for _, variant := range []struct {
				suffix string
				want   bool
			}{
				{"", false}, {"-contributor", true}, {"-CONTRIBUTOR(high)", true},
				{"(8192)", false}, {"-contributor(auto)", true}, {"-contributor(none)", true},
				{"-contributor-extra", false}, {"-contributor-middle(high)", false},
			} {
				for _, namespace := range []string{"", "meta-muse/"} {
					model := namespace + family + variant.suffix
					t.Run(fmt.Sprintf("%t/%s", stream, model), func(t *testing.T) {
						client := &contributorHTTPClient{}
						// Conflicting payload, original request and auth metadata must
						// not overrule the authoritative prepared req.Model.
						payloadModel := "muse-spark-1.3-contributor"
						if variant.want {
							payloadModel = "muse-spark-1.3"
						}
						payload := []byte(`{"model":` + strconv.Quote(payloadModel) + `,"input":[]}`)
						req := translatePathRequest("codex", payload, stream, client)
						req.Model = " " + model + " "
						req.AuthMetadata = map[string]any{"requested_model": payloadModel, "model": payloadModel}
						req.AuthAttributes["model"] = payloadModel
						req.Headers = http.Header{"X-Tbh-Contributor-Mode": {"0", "1"}, "x-tbh-contributor-mode": {"1"}, "x-tbh-session-id": {"spoof"}, "X-Custom-Keep": {"yes"}}
						executeContributorRequest(t, req, stream)
						if len(client.calls) != 1 {
							t.Fatalf("calls=%d", len(client.calls))
						}
						call := client.calls[0]
						if got, want := gjson.GetBytes(call.Body, "model").String(), family+variant.suffix; got != want {
							t.Fatalf("wire model=%q want=%q", got, want)
						}
						assertContributorHeader(t, call.Headers, variant.want)
						if call.Headers.Get("Authorization") != "Bearer test-key" || call.Headers.Get("X-Custom-Keep") != "yes" || call.Headers.Get("X-Tbh-Session-Id") == "spoof" {
							t.Fatal("credential/header isolation changed")
						}
						if req.Headers.Get("X-Tbh-Contributor-Mode") != "0" {
							t.Fatal("client headers mutated")
						}
					})
				}
			}
		}
	}
}

func TestContributorRetryTransport(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, bootstrap := range []string{"http", "error", "failed"} {
			for _, contributor := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/bootstrap=%s/contributor=%t", stream, bootstrap, contributor), func(t *testing.T) {
					model := "muse-spark-1.3"
					if contributor {
						model += "-contributor"
					}
					client := &contributorHTTPClient{rejectFirst: true, bootstrap: bootstrap != "http", nestedFailure: bootstrap == "failed"}
					payload := []byte(`{"model":` + strconv.Quote(model) + `,"input":[{"type":"reasoning","encrypted_content":` + strconv.Quote(parityEnvelope(t)) + `}]}`)
					req := translatePathRequest("codex", payload, stream, client)
					req.Model = model
					executeContributorRequest(t, req, stream)
					if len(client.calls) != 2 {
						t.Fatalf("calls=%d, want one retry", len(client.calls))
					}
					for i, call := range client.calls {
						assertContributorHeader(t, call.Headers, contributor)
						if gjson.GetBytes(call.Body, "model").String() != model {
							t.Fatal("retry changed model")
						}
						if got := strings.Contains(string(call.Body), "encrypted_content"); got != (i == 0) {
							t.Fatalf("attempt %d reasoning=%t", i, got)
						}
					}
				})
			}
		}
	}
}

func TestContributorRawTransport(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		want                     bool
	}{
		{"contributor", "POST", "/v1/responses", `{"model":"muse-spark-1.3-contributor(high)"}`, true},
		{"plain", "POST", "/v1/responses", `{"model":"muse-spark-1.3"}`, false},
		{"missing", "POST", "/v1/responses", `{}`, false},
		{"blank", "POST", "/v1/responses", `{"model":" "}`, false},
		{"number", "POST", "/v1/responses", `{"model":123}`, false},
		{"duplicate", "POST", "/v1/responses", `{"model":"muse-spark-1.3","model":"muse-spark-1.3-contributor"}`, false},
		{"escaped-duplicate", "POST", "/v1/responses", `{"model":"muse-spark-1.3-contributor","model":"muse-spark-1.3"}`, false},
		{"malformed", "POST", "/v1/responses", `{"model":"muse-spark-1.3-contributor"`, false},
		{"array", "POST", "/v1/responses", `[{"model":"muse-spark-1.3-contributor"}]`, false},
		{"get", "GET", "/v1/responses", `{"model":"muse-spark-1.3-contributor"}`, false},
		{"query", "POST", "/v1/responses?model=other", `{"model":"muse-spark-1.3-contributor"}`, false},
		{"unknown-route", "POST", "/v1/other", `{"model":"muse-spark-1.3-contributor"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &contributorHTTPClient{}
			_, err := newMetaExecutor(nil, nil).HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{
				Method: tc.method, URL: "https://api.meta.ai" + tc.path, Body: []byte(tc.body),
				Attributes: map[string]string{"api_key": "test-key", "base_url": "https://api.meta.ai/v1"},
				Headers:    http.Header{"x-tbh-contributor-mode": {"1", "1"}}, HTTPClient: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(client.calls) != 1 {
				t.Fatalf("calls=%d", len(client.calls))
			}
			assertContributorHeader(t, client.calls[0].Headers, tc.want)
			if string(client.calls[0].Body) != tc.body {
				t.Fatal("raw body mutated")
			}
		})
	}
}

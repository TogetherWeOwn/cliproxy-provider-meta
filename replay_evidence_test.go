package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

type evidenceHTTPClient struct {
	requests []pluginapi.HTTPRequest
	bodies   []string
	statuses []int
	fail     bool
}

func (f *evidenceHTTPClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	i := len(f.requests)
	f.requests = append(f.requests, req)
	if f.fail {
		return pluginapi.HTTPResponse{}, errors.New("synthetic transport failure")
	}
	status, body := 200, translatePathUpstreamSSE
	if i < len(f.statuses) {
		status = f.statuses[i]
	}
	if i < len(f.bodies) {
		body = f.bodies[i]
	}
	return pluginapi.HTTPResponse{StatusCode: status, Body: []byte(body)}, nil
}

func (f *evidenceHTTPClient) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	resp, err := f.Do(ctx, req)
	if err != nil {
		return pluginapi.HTTPStreamResponse{}, err
	}
	chunks := make(chan pluginapi.HTTPStreamChunk, 1)
	chunks <- pluginapi.HTTPStreamChunk{Payload: resp.Body}
	close(chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Chunks: chunks}, nil
}

func evidenceExecutor(t *testing.T, out io.Writer) *metaExecutor {
	t.Helper()
	if err := configureBase(context.Background(), []byte("reasoning-replay:\n  enabled: true\n")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = configureBase(context.Background(), nil) })
	e := newMetaExecutor(nil, nil)
	var err error
	e.evidence, err = newReplayProducer(out)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func evidenceMetadata(turn int, request, parent string) map[string]any {
	return map[string]any{
		requestedModelMetadataKey: "public-model-alias(high)",
		recoveryMetadataKey: map[string]any{
			"attempt_id": "attempt-1", "probe_id": "probe-1", "request_id": request,
			"parent_request_id": parent, "turn": float64(turn),
		},
	}
}

func evidenceRequest(payload []byte, stream bool, client pluginapi.HostHTTPClient) pluginapi.ExecutorRequest {
	req := translatePathRequest("openai-response", payload, stream, client)
	req.Metadata = evidenceMetadata(2, "request-2", "request-1")
	return req
}

func runEvidenceRequest(t *testing.T, e *metaExecutor, req pluginapi.ExecutorRequest) []byte {
	t.Helper()
	if !req.Stream {
		resp, err := e.Execute(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Payload
	}
	resp, err := e.ExecuteStream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for c := range resp.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		out.Write(c.Payload)
	}
	return out.Bytes()
}

func evidenceRecords(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("not standalone NDJSON: %v: %s", err, line)
		}
		records = append(records, event)
	}
	return records
}

func TestReplayEvidenceTwoTurnToolLinkage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(strconv.FormatBool(stream), func(t *testing.T) {
			var out bytes.Buffer
			e := evidenceExecutor(t, &out)
			env := parityEnvelope(t)
			firstOutput := fmt.Sprintf(`[{"type":"reasoning","encrypted_content":%q,"summary":[]},{"type":"function_call","call_id":"call-probe-1","name":"probe_tool","arguments":"{}"}]`, env)
			first := &evidenceHTTPClient{bodies: []string{`data: {"type":"response.completed","response":{"id":"resp-probe-1","status":"completed","output":` + firstOutput + `}}` + "\n\n"}}
			req := evidenceRequest([]byte(`{"model":"public-model-alias(high)","input":[]}`), stream, first)
			req.Metadata = evidenceMetadata(1, "request-1", "")
			response := runEvidenceRequest(t, e, req)
			var output gjson.Result
			if stream {
				for _, line := range bytes.Split(response, []byte("\n")) {
					if bytes.HasPrefix(line, []byte("data:")) {
						event := gjson.ParseBytes(bytes.TrimSpace(line[5:]))
						if event.Get("type").String() == "response.completed" {
							output = event.Get("response.output")
						}
					}
				}
			} else {
				output = gjson.GetBytes(response, "output")
			}
			if !output.IsArray() || output.Get("1.call_id").String() != "call-probe-1" || output.Get("1.name").String() != "probe_tool" {
				t.Fatalf("missing actual tool linkage: %s", response)
			}
			if output.Get("0.encrypted_content").String() != TagMetaReasoning(metaReasoningAccountKey(req.AuthID), env) {
				t.Fatalf("response lost account provenance: %s", response)
			}
			secondInput := strings.TrimSuffix(output.Raw, "]") + `,{"type":"function_call_output","call_id":"call-probe-1","output":"synthetic-tool-result"}]`
			second := &evidenceHTTPClient{}
			req = evidenceRequest([]byte(`{"model":"public-model-alias(high)","input":`+secondInput+`}`), stream, second)
			runEvidenceRequest(t, e, req)
			if got := gjson.GetBytes(second.requests[0].Body, "input.0.encrypted_content").String(); got != env {
				t.Fatalf("actual second request did not replay native envelope: %q", got)
			}
			if !bytes.Contains(second.requests[0].Body, []byte("call-probe-1")) {
				t.Fatal("second request lost tool result linkage")
			}
			records := evidenceRecords(t, &out)
			if len(records) != 3 || records[1]["turn"] != float64(1) || records[1]["kept"] != float64(0) || records[2]["kept"] != float64(1) || records[2]["dropped_foreign"] != float64(0) {
				t.Fatalf("actual production-site events: %+v", records)
			}
			if records[2]["parent_request_id"] != records[1]["request_id"] || records[2]["requested_model"] != "public-model-alias(high)" || records[2]["resolved_model"] != "muse-spark-1.3" {
				t.Fatalf("correlation/model mapping lost: %+v", records)
			}
			if path := os.Getenv("META_REPLAY_EXAMPLES_OUT"); path != "" && !stream {
				if err := os.WriteFile(path, out.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestReplayEvidenceForeignDropsAndRetry(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, lane := range []string{"http", "event", "transport"} {
			t.Run(fmt.Sprintf("%v/%s", stream, lane), func(t *testing.T) {
				var out bytes.Buffer
				e := evidenceExecutor(t, &out)
				env := parityEnvelope(t)
				payload := []byte(fmt.Sprintf(`{"input":[{"type":"reasoning","encrypted_content":%q},{"type":"reasoning","encrypted_content":%q}]}`, TagMetaReasoning(metaReasoningAccountKey("auth-a"), env), TagMetaReasoning(metaReasoningAccountKey("auth-b"), env)))
				client := &evidenceHTTPClient{bodies: []string{`{"error":{"message":"not issued to this caller"}}`}, statuses: []int{400}}
				if lane == "event" {
					client.statuses[0] = 200
					client.bodies[0] = "data: {\"type\":\"error\",\"error\":{\"code\":400,\"message\":\"not issued to this caller\"}}\n\n"
				}
				if lane == "transport" {
					client.fail = true
					req := evidenceRequest(payload, stream, client)
					var err error
					if stream {
						_, err = e.ExecuteStream(context.Background(), req)
					} else {
						_, err = e.Execute(context.Background(), req)
					}
					if err == nil {
						t.Fatal("transport error was swallowed")
					}
				} else {
					runEvidenceRequest(t, e, evidenceRequest(payload, stream, client))
				}
				records := evidenceRecords(t, &out)
				if records[1]["kept"] != float64(1) || records[1]["dropped_foreign"] != float64(1) {
					t.Fatalf("foreign drop hidden: %+v", records)
				}
				if lane == "transport" {
					if len(records) != 2 || len(client.requests) != 1 {
						t.Fatal("transport failure must not invent a retry")
					}
					return
				}
				if len(records) != 3 || len(client.requests) != 2 || records[2]["replay_attempt"] != float64(1) || records[2]["kept"] != float64(0) || records[2]["dropped_untagged"] != float64(1) {
					t.Fatalf("retry decision lost: %+v", records)
				}
				if records[1]["event_id"] == records[2]["event_id"] || records[1]["request_id"] != records[2]["request_id"] || bytes.Contains(client.requests[1].Body, []byte(env)) {
					t.Fatal("retry identity/policy changed")
				}
			})
		}
	}
}

func TestReplayEvidenceValidation(t *testing.T) {
	cases := map[string]func(*pluginapi.ExecutorRequest){
		"absent":         func(r *pluginapi.ExecutorRequest) { r.Metadata = nil },
		"string carrier": func(r *pluginapi.ExecutorRequest) { r.Metadata[recoveryMetadataKey] = "raw-arbitrary-metadata" },
		"extra field": func(r *pluginapi.ExecutorRequest) {
			r.Metadata[recoveryMetadataKey].(map[string]any)["secret"] = "never-log"
		},
		"missing parent": func(r *pluginapi.ExecutorRequest) {
			delete(r.Metadata[recoveryMetadataKey].(map[string]any), "parent_request_id")
		},
		"wrong type": func(r *pluginapi.ExecutorRequest) {
			r.Metadata[recoveryMetadataKey].(map[string]any)["request_id"] = 123
		},
		"injected": func(r *pluginapi.ExecutorRequest) {
			r.Metadata[recoveryMetadataKey].(map[string]any)["request_id"] = "id\n{\"kept\":1}"
		},
		"oversized": func(r *pluginapi.ExecutorRequest) {
			r.Metadata[recoveryMetadataKey].(map[string]any)["probe_id"] = strings.Repeat("a", 129)
		},
		"unicode": func(r *pluginapi.ExecutorRequest) {
			r.Metadata[recoveryMetadataKey].(map[string]any)["attempt_id"] = "id‮"
		},
		"fraction": func(r *pluginapi.ExecutorRequest) { r.Metadata[recoveryMetadataKey].(map[string]any)["turn"] = 1.5 },
		"NaN": func(r *pluginapi.ExecutorRequest) {
			r.Metadata[recoveryMetadataKey].(map[string]any)["turn"] = math.NaN()
		},
		"infinite": func(r *pluginapi.ExecutorRequest) {
			r.Metadata[recoveryMetadataKey].(map[string]any)["turn"] = math.Inf(1)
		},
		"string turn": func(r *pluginapi.ExecutorRequest) { r.Metadata[recoveryMetadataKey].(map[string]any)["turn"] = "2" },
		"bool turn":   func(r *pluginapi.ExecutorRequest) { r.Metadata[recoveryMetadataKey].(map[string]any)["turn"] = true },
		"zero turn":   func(r *pluginapi.ExecutorRequest) { r.Metadata[recoveryMetadataKey].(map[string]any)["turn"] = 0 },
		"wrong turn":  func(r *pluginapi.ExecutorRequest) { r.Metadata[recoveryMetadataKey].(map[string]any)["turn"] = 3 },
		"same parent": func(r *pluginapi.ExecutorRequest) {
			r.Metadata[recoveryMetadataKey].(map[string]any)["parent_request_id"] = "request-2"
		},
		"turn1 parent": func(r *pluginapi.ExecutorRequest) { r.Metadata[recoveryMetadataKey].(map[string]any)["turn"] = 1 },
		"empty parent": func(r *pluginapi.ExecutorRequest) {
			r.Metadata[recoveryMetadataKey].(map[string]any)["parent_request_id"] = ""
		},
		"model injection":    func(r *pluginapi.ExecutorRequest) { r.Metadata[requestedModelMetadataKey] = "alias\nforged-event" },
		"model unbounded":    func(r *pluginapi.ExecutorRequest) { r.Metadata[requestedModelMetadataKey] = strings.Repeat("a", 257) },
		"model nonstring":    func(r *pluginapi.ExecutorRequest) { r.Metadata[requestedModelMetadataKey] = 123 },
		"auth metadata only": func(r *pluginapi.ExecutorRequest) { r.AuthMetadata, r.Metadata = r.Metadata, nil },
		"headers only": func(r *pluginapi.ExecutorRequest) {
			r.Metadata = nil
			r.Headers.Set("X-CPA-Recovery-Request-Id", "request-2")
		},
	}
	var out bytes.Buffer
	e := evidenceExecutor(t, &out)
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			client := &evidenceHTTPClient{}
			r := evidenceRequest([]byte(`{"input":[]}`), false, client)
			mutate(&r)
			before := out.Len()
			runEvidenceRequest(t, e, r)
			if out.Len() != before || len(client.requests) != 1 {
				t.Fatal("invalid diagnostics must not emit, fail, or change inference")
			}
		})
	}
	for _, turn := range []any{2, float64(2), json.Number("2")} {
		r := evidenceRequest([]byte(`{"input":[]}`), false, &evidenceHTTPClient{})
		r.Metadata[recoveryMetadataKey].(map[string]any)["turn"] = turn
		if c, _ := recoveryCorrelationFrom(r); c == nil {
			t.Fatalf("valid RPC/native turn rejected: %T", turn)
		}
	}
}

func TestReplayEvidenceConcurrentIdentityAndIsolation(t *testing.T) {
	var out bytes.Buffer
	e := evidenceExecutor(t, &out)
	const count = 64
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := evidenceRequest([]byte(`{"input":[]}`), i%2 == 0, &evidenceHTTPClient{})
			r.Metadata = evidenceMetadata(2, fmt.Sprintf("stale-or-concurrent-%d", i), fmt.Sprintf("parent-%d", i))
			r.Metadata[requestedModelMetadataKey] = fmt.Sprintf("wrong-or-concurrent-model-%d", i)
			runEvidenceRequest(t, e, r)
		}()
	}
	wg.Wait()
	records := evidenceRecords(t, &out)
	if len(records) != count+1 {
		t.Fatalf("lost events: %d", len(records))
	}
	seen := map[string]bool{}
	for i, event := range records {
		id := event["event_id"].(string)
		if seen[id] || id != e.evidence.instance+":"+strconv.Itoa(i+1) || event["producer_instance_id"] != e.evidence.instance {
			t.Fatal("event identity collided or sequence is not serialized")
		}
		seen[id] = true
		if i == 0 {
			continue
		}
		n := strings.TrimPrefix(event["request_id"].(string), "stale-or-concurrent-")
		if event["parent_request_id"] != "parent-"+n || event["requested_model"] != "wrong-or-concurrent-model-"+n {
			t.Fatal("concurrent IDs/model were relabeled or crossed requests")
		}
	}
	var second bytes.Buffer
	p, err := newReplayProducer(&second)
	if err != nil || p.instance == e.evidence.instance || uuid.Validate(p.instance) != nil {
		t.Fatal("startup instance collision or invalid UUID")
	}
	if records[0]["schema"] != "replay-producer-startup-v1" || records[0]["file_logging"] != "not_configured" || records[0]["sources"].([]any)[0] != "stdout" {
		t.Fatal("startup/source declaration changed")
	}
}

func TestReplayEvidenceMirroredSinkDedupIdentity(t *testing.T) {
	var stdout, file bytes.Buffer
	e := evidenceExecutor(t, io.MultiWriter(&stdout, &file))
	runEvidenceRequest(t, e, evidenceRequest([]byte(`{"input":[]}`), false, &evidenceHTTPClient{}))
	if !bytes.Equal(stdout.Bytes(), file.Bytes()) {
		t.Fatal("mirrored capture changed producer bytes or identity")
	}
	records := evidenceRecords(t, &stdout)
	if len(records) != 2 || records[0]["event_id"] == records[1]["event_id"] {
		t.Fatal("distinct startup/decision events collapsed")
	}
}

func TestReplayEvidenceRedactionAndFinalKeptCount(t *testing.T) {
	var out bytes.Buffer
	e := evidenceExecutor(t, &out)
	client := &evidenceHTTPClient{}
	// A policy-approved but malformed native envelope must not manufacture a
	// kept hit after the final native sanitizer has removed it.
	payload := []byte(fmt.Sprintf(`{"input":[{"type":"reasoning","encrypted_content":%q}],"instructions":"private-prompt"}`, TagMetaReasoning(metaReasoningAccountKey("private-auth.json"), "not-a-native-envelope")))
	r := evidenceRequest(payload, false, client)
	r.AuthID = "private-auth.json"
	r.AuthAttributes["api_key"] = "private-credential"
	r.Metadata["unrelated"] = "private-arbitrary-metadata"
	runEvidenceRequest(t, e, r)
	records := evidenceRecords(t, &out)
	if records[1]["kept"] != float64(0) || bytes.Contains(client.requests[0].Body, []byte("not-a-native-envelope")) {
		t.Fatal("prefilter approval was mislabeled as a surviving replay")
	}
	for _, secret := range []string{"private-auth.json", "private-credential", "private-prompt", "private-arbitrary-metadata", "not-a-native-envelope"} {
		if bytes.Contains(out.Bytes(), []byte(secret)) {
			t.Fatalf("producer leaked %q", secret)
		}
	}
}

func TestReplayEvidenceNoUpstreamCarriers(t *testing.T) {
	for _, stream := range []bool{false, true} {
		var out bytes.Buffer
		e := evidenceExecutor(t, &out)
		client := &evidenceHTTPClient{}
		r := evidenceRequest([]byte(`{"input":[]}`), stream, client)
		for _, key := range []string{"X-CPA-Recovery-Request-Id", "x-cpa-recovery-turn", "X-CpA-ReCoVeRy-Future"} {
			r.Headers[key] = []string{"diagnostic-only"}
		}
		runEvidenceRequest(t, e, r)
		// Raw executor HTTP uses the same protected-header boundary.
		_, err := e.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{
			Method: "POST", URL: "https://api.meta.ai/v1/responses", Headers: r.Headers,
			Attributes: r.AuthAttributes, HTTPClient: client,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, request := range client.requests {
			for key := range request.Headers {
				if strings.HasPrefix(strings.ToLower(key), "x-cpa-recovery-") {
					t.Fatal("diagnostic header forwarded to provider")
				}
			}
			for _, id := range []string{"attempt-1", "probe-1", "request-2", recoveryMetadataKey} {
				if bytes.Contains(request.Body, []byte(id)) {
					t.Fatal("diagnostic metadata copied into provider body")
				}
			}
		}
	}
}

func TestReplayEvidenceDiagnosticsDoNotChangePolicyOrPayload(t *testing.T) {
	var out bytes.Buffer
	e := evidenceExecutor(t, &out)
	env := parityEnvelope(t)
	payload := []byte(fmt.Sprintf(`{"input":[{"type":"reasoning","encrypted_content":%q},{"type":"reasoning","encrypted_content":%q}]}`, TagMetaReasoning(metaReasoningAccountKey("auth-a"), env), TagMetaReasoning(metaReasoningAccountKey("auth-b"), env)))
	var bodies [][]byte
	for _, enabled := range []bool{true, false} {
		for _, diagnostics := range []bool{false, true} {
			if err := configureBase(context.Background(), []byte(fmt.Sprintf("reasoning-replay:\n  enabled: %v\n", enabled))); err != nil {
				t.Fatal(err)
			}
			client := &evidenceHTTPClient{}
			req := evidenceRequest(payload, false, client)
			if !diagnostics {
				req.Metadata = nil
			}
			runEvidenceRequest(t, e, req)
			bodies = append(bodies, bytes.Clone(client.requests[0].Body))
		}
	}
	if !bytes.Equal(bodies[0], bodies[1]) || !bytes.Equal(bodies[2], bodies[3]) {
		t.Fatal("diagnostic metadata changed actual upstream payload/policy")
	}
	records := evidenceRecords(t, &out)
	if len(records) != 3 || records[1]["replay_enabled"] != true || records[2]["replay_enabled"] != false || records[2]["kept"] != float64(0) || records[2]["dropped_foreign"] != float64(2) {
		t.Fatalf("diagnostics changed disabled behavior: %+v", records)
	}
}

func TestReplayEvidenceRetryFailureAndBounds(t *testing.T) {
	var out bytes.Buffer
	e := evidenceExecutor(t, &out)
	env := parityEnvelope(t)
	payload := []byte(fmt.Sprintf(`{"input":[{"type":"reasoning","encrypted_content":%q}]}`, TagMetaReasoning(metaReasoningAccountKey("auth-a"), env)))
	client := &evidenceHTTPClient{
		statuses: []int{400, 400},
		bodies:   []string{`{"error":{"message":"not issued to this caller"}}`, `{"error":{"message":"not issued to this caller"}}`},
	}
	req := evidenceRequest(payload, false, client)
	c := req.Metadata[recoveryMetadataKey].(map[string]any)
	for _, key := range []string{"attempt_id", "probe_id", "request_id"} {
		c[key] = strings.Repeat("a", 128)
	}
	c["parent_request_id"] = strings.Repeat("b", 128)
	req.Metadata[requestedModelMetadataKey] = strings.Repeat("m", 256)
	if _, err := e.Execute(context.Background(), req); err == nil {
		t.Fatal("second rejection must surface, not retry indefinitely")
	}
	records := evidenceRecords(t, &out)
	if len(records) != 3 || len(client.requests) != 2 {
		t.Fatal("retry must emit exactly two decisions")
	}
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		if len(line) >= 4096 {
			t.Fatal("bounded record exceeded atomic pipe write size")
		}
	}
	if records[1]["requested_model"] != req.Metadata[requestedModelMetadataKey] || records[1]["attempt_id"] != c["attempt_id"] {
		t.Fatal("boundary values were truncated or relabeled")
	}
	// JSON RPC serializes the extension bag, including numeric turn, without
	// requiring SDK/ABI changes or copying it through credential metadata.
	raw, err := json.Marshal(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &req.Metadata); err != nil {
		t.Fatal(err)
	}
	if correlation, _ := recoveryCorrelationFrom(req); correlation == nil || correlation.RequestID != c["request_id"] {
		t.Fatal("RPC JSON roundtrip lost correlation")
	}
}

type shortEvidenceWriter struct{}

func (shortEvidenceWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestReplayEvidenceFailedSinkAndSequenceExhaustion(t *testing.T) {
	e := evidenceExecutor(t, shortEvidenceWriter{})
	if !e.evidence.failed {
		t.Fatal("partial startup write must disable evidence")
	}
	runEvidenceRequest(t, e, evidenceRequest([]byte(`{"input":[]}`), false, &evidenceHTTPClient{}))
	var out bytes.Buffer
	e = evidenceExecutor(t, &out)
	e.evidence.sequence = math.MaxUint64
	before := out.Len()
	runEvidenceRequest(t, e, evidenceRequest([]byte(`{"input":[]}`), false, &evidenceHTTPClient{}))
	if out.Len() != before {
		t.Fatal("sequence exhaustion wrapped and reused identity")
	}
}

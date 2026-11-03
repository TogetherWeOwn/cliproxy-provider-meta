package main

// Full translate-path tests: a client request in a non-Responses format goes
// through Execute/ExecuteStream, reaches the upstream as a Codex /responses
// body, and the upstream reply comes back in the client's format. These fail
// when the built-in translators are not linked into the plugin.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const translatePathUpstreamSSE = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"muse-spark-1.3\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
	"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n" +
	"data: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"delta\":\"pong\"}\n\n" +
	"data: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_1\",\"text\":\"pong\"}\n\n" +
	"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"pong\"}]}}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"muse-spark-1.3\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"pong\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n"

// translatePathHTTPClient captures the upstream body and answers with a
// scripted Responses SSE stream on both lanes.
type translatePathHTTPClient struct {
	bodies [][]byte
}

func (f *translatePathHTTPClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.bodies = append(f.bodies, req.Body)
	return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, Body: []byte(translatePathUpstreamSSE)}, nil
}

func (f *translatePathHTTPClient) DoStream(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	f.bodies = append(f.bodies, req.Body)
	events := strings.SplitAfter(translatePathUpstreamSSE, "\n\n")
	ch := make(chan pluginapi.HTTPStreamChunk, len(events))
	for _, ev := range events {
		if ev != "" {
			ch <- pluginapi.HTTPStreamChunk{Payload: []byte(ev)}
		}
	}
	close(ch)
	return pluginapi.HTTPStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, Chunks: ch}, nil
}

func translatePathRequest(format string, payload []byte, stream bool, client pluginapi.HostHTTPClient) pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{
		Model:           metaMuseNamespace + "/muse-spark-1.3",
		Format:          format,
		SourceFormat:    format,
		Payload:         payload,
		OriginalRequest: payload,
		Stream:          stream,
		Headers:         http.Header{},
		AuthID:          "auth-a",
		AuthAttributes:  map[string]string{"api_key": "test-key", "base_url": "https://api.meta.ai/v1"},
		HTTPClient:      client,
	}
}

// assertTranslatedUpstream proves the client body was translated to Codex
// Responses: an input array carrying the user text, no client-format messages.
func assertTranslatedUpstream(t *testing.T, client *translatePathHTTPClient) {
	t.Helper()
	if len(client.bodies) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(client.bodies))
	}
	body := client.bodies[0]
	if gjson.GetBytes(body, "messages").Exists() {
		t.Fatalf("upstream body still carries client messages (untranslated): %s", body)
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() || !strings.Contains(input.Raw, "ping") {
		t.Fatalf("upstream body has no translated input with the user text: %s", body)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "muse-spark-1.3" {
		t.Fatalf("upstream model = %q, want muse-spark-1.3", got)
	}
}

func TestTranslatePathClaudeMessages(t *testing.T) {
	ctx := context.Background()
	e := newMetaExecutor(nil, &metaAuthProvider{})
	payload := []byte(`{"model":"meta-muse/muse-spark-1.3","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`)

	client := &translatePathHTTPClient{}
	resp, err := e.Execute(ctx, translatePathRequest("claude", payload, false, client))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertTranslatedUpstream(t, client)
	if gjson.GetBytes(resp.Payload, "type").String() != "message" || gjson.GetBytes(resp.Payload, "role").String() != "assistant" {
		t.Fatalf("response is not a Claude message: %s", resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "pong" {
		t.Fatalf("Claude content text = %q, want pong: %s", got, resp.Payload)
	}

	client = &translatePathHTTPClient{}
	streamResp, err := e.ExecuteStream(ctx, translatePathRequest("claude", payload, true, client))
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	var out strings.Builder
	for chunk := range streamResp.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		out.Write(chunk.Payload)
	}
	assertTranslatedUpstream(t, client)
	for _, want := range []string{"message_start", "content_block_delta", "pong", "message_stop"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("Claude stream missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "response.output_text.delta") {
		t.Fatalf("Claude stream carries raw Responses events (untranslated): %s", out.String())
	}
}

func TestTranslatePathOpenAIChat(t *testing.T) {
	ctx := context.Background()
	e := newMetaExecutor(nil, &metaAuthProvider{})
	payload := []byte(`{"model":"meta-muse/muse-spark-1.3","messages":[{"role":"user","content":"ping"}]}`)

	client := &translatePathHTTPClient{}
	resp, err := e.Execute(ctx, translatePathRequest("openai", payload, false, client))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertTranslatedUpstream(t, client)
	if gjson.GetBytes(resp.Payload, "object").String() != "chat.completion" {
		t.Fatalf("response is not an OpenAI chat completion: %s", resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "pong" {
		t.Fatalf("chat content = %q, want pong: %s", got, resp.Payload)
	}

	client = &translatePathHTTPClient{}
	streamResp, err := e.ExecuteStream(ctx, translatePathRequest("openai", payload, true, client))
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	var out strings.Builder
	for chunk := range streamResp.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		out.Write(chunk.Payload)
	}
	assertTranslatedUpstream(t, client)
	if !strings.Contains(out.String(), "chat.completion.chunk") || !strings.Contains(out.String(), "pong") {
		t.Fatalf("OpenAI chat stream not translated: %s", out.String())
	}
}

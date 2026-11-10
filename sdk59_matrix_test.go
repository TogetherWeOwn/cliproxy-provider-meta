package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// These exercise the embedded stock registry through the provider. Importing
// builtin registers many more routes; it is not evidence those routes ran.
func TestSDK59DeferredToolSearchOrdering(t *testing.T) {
	const tools = `[{"name":"ToolSearch","input_schema":{"type":"object"}},{"name":"Write","input_schema":{"type":"object"}},{"name":"WebFetch","defer_loading":true,"input_schema":{"type":"object"}},{"name":"WebSearch","defer_loading":true,"input_schema":{"type":"object"}}]`
	messages := []string{
		`{"role":"user","content":"load tools"}`,
		`{"role":"assistant","content":[{"type":"tool_use","id":"search_1","name":"ToolSearch","input":{"query":"select:WebSearch,WebFetch"}}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"search_1","content":[{"type":"tool_reference","tool_name":"WebSearch"},{"type":"tool_reference","tool_name":"WebFetch"},{"type":"tool_reference","tool_name":"WebSearch"},{"type":"tool_reference","tool_name":"Unknown"}]}]}`,
		`{"role":"assistant","content":[{"type":"tool_use","id":"fetch_1","name":"WebFetch","input":{"url":"https://example.invalid"}}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"fetch_1","content":"fetched"}]}`,
		`{"role":"assistant","content":[{"type":"tool_use","id":"search_2","name":"ToolSearch","input":{"query":"select:WebFetch"}}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"search_2","content":[{"type":"tool_reference","tool_name":"WebFetch"}]}]}`,
	}
	for _, stream := range []bool{false, true} {
		var prefix []gjson.Result
		var topTools string
		for turn := 2; turn <= len(messages); turn++ {
			t.Run(fmt.Sprintf("stream=%t/turn=%d", stream, turn), func(t *testing.T) {
				payload := []byte(`{"model":"muse-spark-1.3","system":"Stable instructions","tools":` + tools + `,"messages":[` + strings.Join(messages[:turn], ",") + `]}`)
				client := &contributorHTTPClient{}
				req := translatePathRequest("claude", payload, stream, client)
				req.Headers.Set("X-Claude-Code-Session-Id", "sdk59-session")
				executeContributorRequest(t, req, stream)
				body := client.calls[0].Body
				items := gjson.GetBytes(body, "input").Array()
				if turn == 2 {
					prefix = items
					topTools = gjson.GetBytes(body, "tools").Raw
				}
				if gjson.GetBytes(body, "tools").Raw != topTools {
					t.Fatal("tool discovery changed eager/cache prefix")
				}
				if len(items) < len(prefix) {
					t.Fatalf("lost prefix: %s", body)
				}
				for i, item := range prefix {
					if items[i].Raw != item.Raw {
						t.Fatalf("prefix item %d changed: %s", i, body)
					}
				}
				additional, searchResult, use := -1, -1, -1
				declarations := 0
				for i, item := range items {
					if item.Get("type").String() == "additional_tools" {
						additional = i
						declarations++
						loaded := item.Get("tools").Array()
						if len(loaded) != 2 || loaded[0].Get("name").String() != "WebSearch" || loaded[1].Get("name").String() != "WebFetch" {
							t.Fatalf("discovery order/dedup: %s", item.Raw)
						}
					}
					if item.Get("type").String() == "function_call_output" && item.Get("call_id").String() == "search_1" {
						searchResult = i
					}
					if item.Get("type").String() == "function_call" && item.Get("call_id").String() == "fetch_1" {
						use = i
					}
				}
				if turn >= 3 && (declarations != 1 || searchResult < 0 || additional <= searchResult) {
					t.Fatalf("declaration/result ordering: %s", body)
				}
				if turn >= 4 && (use <= additional || items[use].Get("name").String() != "WebFetch") {
					t.Fatalf("subsequent use lost or reordered: %s", body)
				}
			})
		}
	}
}

type sdk59HTTPClient struct {
	calls    []pluginapi.HTTPRequest
	response func(pluginapi.HTTPRequest) string
}

func (f *sdk59HTTPClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.calls = append(f.calls, req)
	return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(f.response(req))}, nil
}
func (f *sdk59HTTPClient) DoStream(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	f.calls = append(f.calls, req)
	body := f.response(req)
	ch := make(chan pluginapi.HTTPStreamChunk, len(body))
	for i := range body {
		ch <- pluginapi.HTTPStreamChunk{Payload: []byte(body[i : i+1])}
	}
	close(ch)
	return pluginapi.HTTPStreamResponse{StatusCode: 200, Chunks: ch}, nil
}

func TestSDK59CustomToolRoundTrips(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, name := range []string{"apply_patch", strings.Repeat("custom_tool_", 7)} {
			for _, grammar := range []bool{false, true} {
				t.Run(fmt.Sprintf("nested=%t/long=%t/grammar=%t", nested, len(name) > 64, grammar), func(t *testing.T) {
					format := `{"type":"text"}`
					if grammar {
						format = `{"type":"grammar","grammar":{"syntax":"lark","definition":"start: patch"}}`
					}
					custom := `{"name":` + strconv.Quote(name) + `,"description":"Edit text","format":` + format + `}`
					tool := `{"type":"custom","name":` + strconv.Quote(name) + `,"description":"Edit text","format":` + format + `}`
					choice := `{"type":"custom","name":` + strconv.Quote(name) + `}`
					if nested {
						tool = `{"type":"custom","custom":` + custom + `}`
						choice = `{"type":"custom","custom":{"name":` + strconv.Quote(name) + `}}`
					}
					payload := []byte(`{"model":"muse-spark-1.3","messages":[{"role":"user","content":"edit"}],"tools":[` + tool + `],"tool_choice":` + choice + `}`)
					for _, stream := range []bool{false, true} {
						client := &sdk59HTTPClient{response: func(req pluginapi.HTTPRequest) string {
							short := gjson.GetBytes(req.Body, "tools.0.name").String()
							if short == "" || len(short) > 64 || gjson.GetBytes(req.Body, "tool_choice.name").String() != short {
								t.Fatalf("declaration/choice mismatch: %s", req.Body)
							}
							item := `{"type":"custom_tool_call","id":"item_patch","call_id":"call_patch","name":` + strconv.Quote(short) + `,"input":"patch text"}`
							return "data: " + `{"type":"response.created","response":{"id":"resp_custom","model":"muse-spark-1.3"}}` + "\n\n" +
								"data: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"item_patch","call_id":"call_patch","name":` + strconv.Quote(short) + `,"input":""}}` + "\n\n" +
								"data: " + `{"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"item_patch","delta":"patch "}` + "\n\n" +
								"data: " + `{"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"item_patch","delta":"text"}` + "\n\n" +
								"data: " + `{"type":"response.custom_tool_call_input.done","output_index":0,"item_id":"item_patch","input":"patch text"}` + "\n\n" +
								"data: " + `{"type":"response.output_item.done","output_index":0,"item":` + item + `}` + "\n\n" +
								"data: " + `{"type":"response.completed","response":{"id":"resp_custom","model":"muse-spark-1.3","status":"completed","output":[` + item + `],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}` + "\n\n"
						}}
						req := translatePathRequest("openai", payload, stream, client)
						e := newMetaExecutor(nil, nil)
						var callID, callName, args string
						if !stream {
							resp, err := e.Execute(context.Background(), req)
							if err != nil {
								t.Fatal(err)
							}
							call := gjson.GetBytes(resp.Payload, "choices.0.message.tool_calls.0")
							callID = call.Get("id").String()
							callName = call.Get("function.name").String()
							args = call.Get("function.arguments").String()
						} else {
							resp, err := e.ExecuteStream(context.Background(), req)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range resp.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								line := strings.TrimSpace(strings.TrimPrefix(string(chunk.Payload), "data: "))
								call := gjson.Get(line, "choices.0.delta.tool_calls.0")
								if v := call.Get("id").String(); v != "" {
									callID = v
								}
								if v := call.Get("function.name").String(); v != "" {
									callName = v
								}
								args += call.Get("function.arguments").String()
							}
						}
						wantArgs := "patch text"
						if name == "apply_patch" {
							// Stock .59 exposes this custom tool as a JSON-input
							// function to Chat clients, then unwraps it on replay.
							wantArgs = `{"input":"patch text"}`
						}
						if callID != "call_patch" || callName != name || args != wantArgs {
							t.Fatalf("stream=%t restored %q/%q/%q", stream, callID, callName, args)
						}
						// Feed the restored response and result into the next request.
						var follow map[string]any
						if err := json.Unmarshal(payload, &follow); err != nil {
							t.Fatal(err)
						}
						follow["messages"] = []any{map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": callName, "arguments": args}}}}, map[string]any{"role": "tool", "tool_call_id": callID, "content": "done"}}
						next, _ := json.Marshal(follow)
						prepared, err := e.prepare(translatePathRequest("openai", next, false, client))
						if err != nil {
							t.Fatal(err)
						}
						if gjson.GetBytes(prepared.body, "input.0.type").String() != "custom_tool_call" || gjson.GetBytes(prepared.body, "input.0.input").String() != "patch text" || gjson.GetBytes(prepared.body, "input.1.type").String() != "custom_tool_call_output" || gjson.GetBytes(prepared.body, "input.1.call_id").String() != callID {
							t.Fatalf("follow-up lost custom identity/result: %s", prepared.body)
						}
					}
				})
			}
		}
	}
}

func TestSDK59DeferredEagerLongNameCollision(t *testing.T) {
	eager := "mcp__shared-long-server-11111111-2222-3333-4444-555555555555_deliver_final_subagent_handback_report"
	deferred := "mcp__shared-long-server-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee_deliver_final_subagent_handback_report"
	tool := func(name string, deferLoading bool) string {
		return fmt.Sprintf(`{"name":%q,"defer_loading":%t,"input_schema":{"type":"object"}}`, name, deferLoading)
	}
	messages := fmt.Sprintf(`[{"role":"user","content":"discover"},{"role":"assistant","content":[{"type":"tool_use","id":"search","name":"ToolSearch","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"search","content":[{"type":"tool_reference","tool_name":%q},{"type":"tool_reference","tool_name":%q}]}]},{"role":"assistant","content":[{"type":"tool_use","id":"later","name":%q,"input":{}}]}]`, deferred, deferred, deferred)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var eagerName string
			for _, tools := range []string{tool("ToolSearch", false) + "," + tool(eager, false), tool("ToolSearch", false) + "," + tool(deferred, true) + "," + tool(eager, false)} {
				payload := []byte(`{"tools":[` + tools + `],"messages":` + messages + `}`)
				client := &contributorHTTPClient{}
				executeContributorRequest(t, translatePathRequest("claude", payload, stream, client), stream)
				body := client.calls[0].Body
				got := gjson.GetBytes(body, "tools.1.name").String()
				if eagerName == "" {
					eagerName = got
					continue
				}
				if got == "" || got != eagerName {
					t.Fatalf("discovery renamed eager tool %q to %q: %s", eagerName, got, body)
				}
				var short, called string
				declarations := 0
				for _, item := range gjson.GetBytes(body, "input").Array() {
					if item.Get("type").String() == "additional_tools" {
						declarations++
						if len(item.Get("tools").Array()) != 1 {
							t.Fatalf("duplicate discovered declaration: %s", item.Raw)
						}
						short = item.Get("tools.0.name").String()
					}
					if item.Get("call_id").String() == "later" {
						called = item.Get("name").String()
					}
				}
				if declarations != 1 || short == "" || short == eagerName || len(short) > 64 || called != short {
					t.Fatalf("collision mapping lost: eager=%q deferred=%q called=%q: %s", eagerName, short, called, body)
				}
			}
		})
	}
}

func TestSDK59GeminiProviderPath(t *testing.T) {
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"ping"}]}]}`)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			client := &sdk59HTTPClient{response: func(req pluginapi.HTTPRequest) string { return translatePathUpstreamSSE }}
			req := translatePathRequest("gemini", payload, stream, client)
			e := newMetaExecutor(nil, nil)
			var text strings.Builder
			if stream {
				resp, err := e.ExecuteStream(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range resp.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					line := strings.TrimSpace(strings.TrimPrefix(string(chunk.Payload), "data: "))
					for _, part := range gjson.Get(line, "candidates.0.content.parts").Array() {
						text.WriteString(part.Get("text").String())
					}
				}
			} else {
				resp, err := e.Execute(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				for _, part := range gjson.GetBytes(resp.Payload, "candidates.0.content.parts").Array() {
					text.WriteString(part.Get("text").String())
				}
			}
			if text.String() != "pong" || len(client.calls) != 1 || !strings.Contains(gjson.GetBytes(client.calls[0].Body, "input").Raw, "ping") {
				t.Fatalf("Gemini provider round trip: text=%q calls=%v", text.String(), client.calls)
			}
		})
	}
}

func TestSDK59RegisteredMultipartRoutes(t *testing.T) {
	// Registry-only routes are not Meta's Codex upstream path. Exercise the .59
	// multipart fix explicitly, without claiming provider inference coverage.
	for _, from := range []sdktranslator.Format{sdktranslator.FromString("gemini"), sdktranslator.FromString("antigravity")} {
		t.Run(from.String(), func(t *testing.T) {
			payload := `{"candidates":[{"content":{"role":"model","parts":[{"text":"first"},{"text":"second"},{"thought":true,"text":"reason-one"},{"thought":true,"text":"reason-two"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
			if from.String() == "antigravity" {
				payload = `{"response":` + payload + `}`
			}
			var state any
			out := sdktranslator.TranslateNonStream(context.Background(), from, sdktranslator.FromString("openai"), "muse-spark-1.3", nil, nil, []byte(payload), &state)
			if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "firstsecond" {
				t.Fatalf("multipart text=%q: %s", got, out)
			}
			if got := gjson.GetBytes(out, "choices.0.message.reasoning_content").String(); got != "reason-onereason-two" {
				t.Fatalf("multipart reasoning=%q: %s", got, out)
			}
			for _, split := range []bool{false, true} {
				events := []string{payload}
				if split {
					events = nil
					for _, parts := range []string{`[{"text":"first"},{"thought":true,"text":"reason-one"}]`, `[{"text":"second"},{"thought":true,"text":"reason-two"}]`} {
						event := `{"candidates":[{"content":{"role":"model","parts":` + parts + `}}]}`
						if from.String() == "antigravity" {
							event = `{"response":` + event + `}`
						}
						events = append(events, event)
					}
				}
				var streamState any
				var text, reasoning strings.Builder
				for _, event := range events {
					for _, chunk := range sdktranslator.TranslateStream(context.Background(), from, sdktranslator.FromString("openai"), "muse-spark-1.3", nil, nil, []byte("data: "+event), &streamState) {
						line := strings.TrimSpace(strings.TrimPrefix(string(chunk), "data: "))
						text.WriteString(gjson.Get(line, "choices.0.delta.content").String())
						reasoning.WriteString(gjson.Get(line, "choices.0.delta.reasoning_content").String())
					}
				}
				if text.String() != gjson.GetBytes(out, "choices.0.message.content").String() || reasoning.String() != gjson.GetBytes(out, "choices.0.message.reasoning_content").String() {
					t.Fatalf("split=%t stream differs: text=%q reasoning=%q", split, text.String(), reasoning.String())
				}
			}
		})
	}
}

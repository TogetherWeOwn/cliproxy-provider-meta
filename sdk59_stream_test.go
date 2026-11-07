package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func sdk59ParallelEvents(marker string) []string {
	return []string{
		fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_%s","model":"muse-spark-1.3"}}`, marker),
		fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"item_%s_0","call_id":"call_%s_0","name":"first","arguments":""}}`, marker, marker),
		fmt.Sprintf(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"item_%s_1","call_id":"call_%s_1","name":"second","arguments":""}}`, marker, marker),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"item_%s_0","delta":"{\"value\":\"%s"}`, marker, marker),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"item_%s_1","delta":"{\"value\":\"%s"}`, marker, marker),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"item_%s_0","delta":"-first\"}"}`, marker),
		fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"item_%s_1","delta":"-second\"}"}`, marker),
		`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
	}
}

func TestSDK59ConcurrentFragmentedInterleaving(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := newMetaExecutor(nil, nil)
	inputs := []chan pluginapi.HTTPStreamChunk{make(chan pluginapi.HTTPStreamChunk), make(chan pluginapi.HTTPStreamChunk)}
	outputs := []chan pluginapi.ExecutorStreamChunk{make(chan pluginapi.ExecutorStreamChunk, 100), make(chan pluginapi.ExecutorStreamChunk, 100)}
	for i, marker := range []string{"alpha", "bravo"} {
		prepared := &preparedMetaRequest{responseFormat: sdktranslator.FromString("openai"), originalPayload: []byte(`{"tools":[{"type":"function","function":{"name":"first"}},{"type":"function","function":{"name":"second"}}]}`), replay: parityReplay("auth-"+marker, "")}
		go pumpMetaStream(ctx, e, pluginapi.ExecutorRequest{Model: "muse-spark-1.3"}, prepared, inputs[i], outputs[i])
	}
	// Alternate every byte across two independent auth/translator states. Within
	// each stream, tool 0 and tool 1 arguments are also deliberately interleaved.
	wire := []string{strings.Join(sdk59ParallelEvents("alpha"), "\n\ndata: "), strings.Join(sdk59ParallelEvents("bravo"), "\n\ndata: ")}
	for i := range wire {
		wire[i] = "data: " + wire[i] + "\n\n"
	}
	for offset := 0; offset < len(wire[0]) || offset < len(wire[1]); offset++ {
		for i := range inputs {
			if offset < len(wire[i]) {
				select {
				case inputs[i] <- pluginapi.HTTPStreamChunk{Payload: []byte(wire[i][offset : offset+1])}:
				case <-ctx.Done():
					t.Fatal("interleaved pump timed out")
				}
			}
		}
	}
	for _, input := range inputs {
		close(input)
	}
	for i, marker := range []string{"alpha", "bravo"} {
		args := map[int64]string{}
		ids := map[int64]string{}
		finishes := 0
		for chunk := range outputs[i] {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
			line := strings.TrimSpace(strings.TrimPrefix(string(chunk.Payload), "data: "))
			for _, call := range gjson.Get(line, "choices.0.delta.tool_calls").Array() {
				index := call.Get("index").Int()
				if id := call.Get("id").String(); id != "" {
					ids[index] = id
				}
				args[index] += call.Get("function.arguments").String()
			}
			if gjson.Get(line, "choices.0.finish_reason").String() == "tool_calls" {
				finishes++
			}
		}
		for index, suffix := range []string{"first", "second"} {
			if got, want := args[int64(index)], fmt.Sprintf(`{"value":"%s-%s"}`, marker, suffix); got != want {
				t.Fatalf("stream %d/tool %d args=%q want=%q", i, index, got, want)
			}
			if got, want := ids[int64(index)], fmt.Sprintf("call_%s_%d", marker, index); got != want {
				t.Fatalf("stream %d/tool %d id=%q want=%q", i, index, got, want)
			}
		}
		if finishes != 1 {
			t.Fatalf("stream %d terminal count=%d", i, finishes)
		}
	}
}

func TestSDK59PumpCancellation(t *testing.T) {
	for _, blockedOutput := range []bool{false, true} {
		t.Run(fmt.Sprint(blockedOutput), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			in := make(chan pluginapi.HTTPStreamChunk)
			defer close(in)
			out := make(chan pluginapi.ExecutorStreamChunk, 1)
			done := make(chan struct{})
			go func() {
				pumpMetaStream(ctx, newMetaExecutor(nil, nil), pluginapi.ExecutorRequest{}, &preparedMetaRequest{responseFormat: sdktranslator.FormatCodex, replay: parityReplay("auth-cancel", "")}, in, out)
				close(done)
			}()
			if blockedOutput {
				// The second unbuffered receive proves the first event was
				// emitted into the one-slot output. Processing the second
				// event must now cancel its send, not its input receive.
				for i := 0; i < 2; i++ {
					select {
					case in <- pluginapi.HTTPStreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"unread\"}\n")}:
					case <-time.After(2 * time.Second):
						t.Fatal("pump did not receive event before cancellation")
					}
				}
			}
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("pump did not stop on cancellation with upstream still open")
			}
		})
	}
}

func TestSDK59PumpTerminalErrors(t *testing.T) {
	for _, kind := range []string{"completed", "incomplete", "failed", "error", "server", "transport"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			in := make(chan pluginapi.HTTPStreamChunk, 3)
			out := make(chan pluginapi.ExecutorStreamChunk, 10)
			isError := kind == "failed" || kind == "error" || kind == "server" || kind == "transport"
			switch kind {
			case "failed":
				in <- pluginapi.HTTPStreamChunk{Payload: []byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":400,\"type\":\"invalid_request_error\",\"message\":\"terminal boom\"}}}\n")}
			case "error":
				in <- pluginapi.HTTPStreamChunk{Payload: []byte("data: {\"type\":\"error\",\"error\":{\"code\":400,\"type\":\"invalid_request_error\",\"message\":\"terminal boom\"}}\n")}
			case "server":
				in <- pluginapi.HTTPStreamChunk{Payload: []byte("data: {\"type\":\"error\",\"error\":{\"code\":500,\"message\":\"terminal boom\"}}\n")}
			case "transport":
				in <- pluginapi.HTTPStreamChunk{Err: errors.New("terminal boom")}
			default:
				in <- pluginapi.HTTPStreamChunk{Payload: []byte(fmt.Sprintf("data: {\"type\":\"response.%s\",\"response\":{\"status\":%q,\"output\":[]}}\n", kind, kind))}
			}
			if isError {
				in <- pluginapi.HTTPStreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"must-not-escape\"}\n")}
			}
			close(in)
			go pumpMetaStream(ctx, newMetaExecutor(nil, nil), pluginapi.ExecutorRequest{}, &preparedMetaRequest{responseFormat: sdktranslator.FormatCodex, replay: parityReplay("auth-terminal", "")}, in, out)
			chunks, failures := 0, 0
			for {
				select {
				case chunk, ok := <-out:
					if !ok {
						if chunks != 1 || (isError && failures != 1) || (!isError && failures != 0) {
							t.Fatalf("chunks=%d errors=%d", chunks, failures)
						}
						return
					}
					chunks++
					if chunk.Err != nil {
						failures++
						rpcErr := unwrapMetaRPCError(t, chunk.Err)
						if kind == "failed" || kind == "error" {
							if rpcErr.HTTPStatus != 400 || gjson.Get(rpcErr.Message, "error.message").String() != "terminal boom" {
								t.Fatalf("actionable rejection lost: %+v", rpcErr)
							}
						} else if rpcErr.Message != "plugin operation failed" {
							t.Fatalf("server/transport details were not sanitized: %+v", rpcErr)
						}
						if kind == "server" && rpcErr.HTTPStatus != 500 {
							t.Fatalf("server status lost: %+v", rpcErr)
						}
					}
				case <-ctx.Done():
					t.Fatal("terminal pump did not close")
				}
			}
		})
	}
}

func TestSDK59FlatResponsesCustomHistory(t *testing.T) {
	payload := []byte(`{"model":"muse-spark-1.3","tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}}],"tool_choice":{"type":"custom","name":"apply_patch"},"input":[{"type":"custom_tool_call","call_id":"call_old","name":"apply_patch","input":"old patch"},{"type":"custom_tool_call_output","call_id":"call_old","output":"done"},{"role":"user","content":"next edit"}]}`)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			client := &sdk59HTTPClient{response: func(req pluginapi.HTTPRequest) string {
				if gjson.GetBytes(req.Body, "tools.0.type").String() != "custom" || gjson.GetBytes(req.Body, "tool_choice.name").String() != "apply_patch" || gjson.GetBytes(req.Body, "input.0.input").String() != "old patch" || gjson.GetBytes(req.Body, "input.1.call_id").String() != "call_old" {
					t.Fatalf("flat custom history/choice lost: %s", req.Body)
				}
				return "data: " + `{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","call_id":"call_new","name":"apply_patch","input":"new patch"}}` + "\n\n" +
					"data: " + `{"type":"response.completed","response":{"id":"resp_flat","output":[],"status":"completed"}}` + "\n\n"
			}}
			req := translatePathRequest("openai-response", payload, stream, client)
			e := newMetaExecutor(nil, nil)
			var completed []byte
			if !stream {
				resp, err := e.Execute(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				completed = resp.Payload
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
					if gjson.Get(line, "type").String() == "response.completed" {
						completed = []byte(gjson.Get(line, "response").Raw)
					}
				}
			}
			item := gjson.GetBytes(completed, "output.0")
			if item.Get("type").String() != "custom_tool_call" || item.Get("call_id").String() != "call_new" || item.Get("name").String() != "apply_patch" || item.Get("input").String() != "new patch" {
				t.Fatalf("flat custom round trip: %s", completed)
			}
		})
	}
}

func TestSDK59CustomFunctionCollision(t *testing.T) {
	const custom = "mcp__server-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee_apply_patch"
	for _, name := range []string{"apply_patch", custom} {
		payload := []byte(`{"messages":[{"role":"user","content":"test"}],"tools":[{"type":"function","function":{"name":` + fmt.Sprintf("%q", name) + `,"parameters":{"type":"object"}}},{"type":"custom","custom":{"name":` + fmt.Sprintf("%q", name) + `}}]}`)
		prepared, err := newMetaExecutor(nil, nil).prepare(translatePathRequest("openai", payload, false, nil))
		if err != nil {
			t.Fatal(err)
		}
		tools := gjson.GetBytes(prepared.body, "tools").Array()
		if len(tools) < 2 || tools[0].Get("type").String() != "function" || tools[1].Get("type").String() != "custom" {
			t.Fatalf("tool families lost in collision: %s", prepared.body)
		}
		// Same-name declarations must never turn a function history into custom
		// history; stock defaults to the function family for this ambiguity.
		var request = string(payload)
		request = strings.Replace(request, `{"role":"user","content":"test"}`, `{"role":"assistant","tool_calls":[{"id":"call_fn","type":"function","function":{"name":`+fmt.Sprintf("%q", name)+`,"arguments":"{}"}}]}`, 1)
		prepared, err = newMetaExecutor(nil, nil).prepare(translatePathRequest("openai", []byte(request), false, nil))
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(prepared.body, "input.0.type").String() != "function_call" {
			t.Fatalf("collision changed function family: %s", prepared.body)
		}
	}
}

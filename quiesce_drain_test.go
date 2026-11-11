package main

// Plugin-level quiesce drain coverage through the real kit Runtime: the Meta
// plugin is built with makePlugin against a fake host Caller, registered, and
// a synthetic request is held in flight while plugin.quiesce runs. Quiesce
// must wait for the in-flight work (not cancel it), refuse new work with
// not_ready during and after the drain, and only then return.
//
// The drain-timeout case (a stuck in-flight call failing quiesce bounded
// with drain_timeout) is intentionally not repeated here: kit pins it in its
// own TestQuiesceDrainTimeoutCancelsStraggler, and makePlugin does not
// expose a DrainTimeout knob, so a provider-level variant would need an
// unexported seam. The default 60s drain bound is left untouched per the kit
// contract.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quiesceHost fakes the host Caller boundary. host.http.do is gated so a sync
// execute can be held in flight; host.http.stream_read serves the synthetic
// SSE stream read-by-read, with every read after the first gated, so the
// stream (peek, then pump) can be held in flight across quiesce.
type quiesceHost struct {
	enterOnce sync.Once
	entered   chan struct{}
	DoGate    chan struct{} // closed by the test to release the sync Do
	reads     int
	readGate  chan struct{} // closed by the test to release gated stream reads
	mu        sync.Mutex
	emitted   [][]byte
	closes    int
}

func newQuiesceHost() *quiesceHost {
	return &quiesceHost{
		entered:  make(chan struct{}),
		DoGate:   make(chan struct{}),
		readGate: make(chan struct{}),
	}
}

func quiesceOKResult(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func (h *quiesceHost) enter() { h.enterOnce.Do(func() { close(h.entered) }) }

func (h *quiesceHost) Call(ctx context.Context, method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostHTTPOperationOpen:
		return quiesceOKResult(map[string]string{"operation_id": "op-quiesce"}), nil
	case pluginabi.MethodHostHTTPDo:
		h.enter()
		<-h.DoGate
		return quiesceOKResult(pluginapi.HTTPResponse{
			StatusCode: 200,
			Headers:    http.Header{"Content-Type": {"text/event-stream"}},
			Body:       []byte(translatePathUpstreamSSE),
		}), nil
	case pluginabi.MethodHostHTTPDoStream:
		return quiesceOKResult(map[string]any{
			"status_code": 200,
			"headers":     map[string][]string{"Content-Type": {"text/event-stream"}},
			"stream_id":   "stream-quiesce",
		}), nil
	case pluginabi.MethodHostHTTPStreamRead:
		h.mu.Lock()
		h.reads++
		n := h.reads
		h.mu.Unlock()
		h.enter()
		events := strings.SplitAfter(translatePathUpstreamSSE, "\n\n")
		if n-1 < len(events) {
			if n >= 2 {
				<-h.readGate
			}
			if ev := events[n-1]; ev != "" {
				return quiesceOKResult(map[string]any{"payload": []byte(ev)}), nil
			}
		}
		return quiesceOKResult(map[string]any{"done": true}), nil
	case pluginabi.MethodHostStreamEmit:
		var req struct {
			Payload []byte `json:"payload"`
		}
		_ = json.Unmarshal(raw, &req)
		h.mu.Lock()
		h.emitted = append(h.emitted, req.Payload)
		h.mu.Unlock()
		return quiesceOKResult(map[string]any{}), nil
	case pluginabi.MethodHostStreamClose:
		h.mu.Lock()
		h.closes++
		h.mu.Unlock()
		return quiesceOKResult(map[string]any{}), nil
	default:
		return quiesceOKResult(map[string]any{}), nil
	}
}

func (h *quiesceHost) snapshot() (emitted, closes int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.emitted), h.closes
}

type quiesceRuntime interface {
	Call(context.Context, string, []byte) []byte
}

func quiesceRegister(t *testing.T, rt quiesceRuntime) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"schema_version": pluginabi.SchemaVersion})
	var env pluginabi.Envelope
	if err := json.Unmarshal(rt.Call(context.Background(), pluginabi.MethodPluginRegister, raw), &env); err != nil || !env.OK {
		t.Fatalf("register: %+v %v", env, err)
	}
}

func quiesceCallEnv(rt quiesceRuntime, method string, req any) pluginabi.Envelope {
	raw, _ := json.Marshal(req)
	var env pluginabi.Envelope
	_ = json.Unmarshal(rt.Call(context.Background(), method, raw), &env)
	return env
}

func quiesceExecuteReq(stream bool) map[string]any {
	payload := []byte(`{"model":"meta-muse/muse-spark-1.3","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`)
	return map[string]any{
		"Model":           "meta-muse/muse-spark-1.3",
		"Format":          "claude",
		"SourceFormat":    "claude",
		"Payload":         payload,
		"OriginalRequest": payload,
		"Stream":          stream,
		"AuthID":          "auth-a",
		"AuthAttributes":  map[string]string{"api_key": "test-key", "base_url": "https://api.meta.ai/v1"},
	}
}

func waitForNotReadyQuiesce(t *testing.T, rt quiesceRuntime) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		env := quiesceCallEnv(rt, pluginabi.MethodExecutorIdentifier, nil)
		if !env.OK && env.Error != nil && env.Error.Code == "not_ready" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission never stopped: %+v", env)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestQuiesceDrainsInFlightExecute(t *testing.T) {
	host := newQuiesceHost()
	rt, err := makePlugin(host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Shutdown(context.Background()) })
	quiesceRegister(t, rt)

	execDone := make(chan pluginabi.Envelope, 1)
	go func() { execDone <- quiesceCallEnv(rt, pluginabi.MethodExecutorExecute, quiesceExecuteReq(false)) }()
	select {
	case <-host.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("execute never reached upstream")
	}

	quiesceDone := make(chan pluginabi.Envelope, 1)
	go func() { quiesceDone <- quiesceCallEnv(rt, pluginabi.MethodPluginQuiesce, nil) }()
	waitForNotReadyQuiesce(t, rt)
	select {
	case env := <-quiesceDone:
		t.Fatalf("quiesce returned while work in flight: %+v", env)
	case <-time.After(50 * time.Millisecond):
	}

	// New work during quiesce is refused not_ready, and the in-flight call
	// has not finished.
	if env := quiesceCallEnv(rt, pluginabi.MethodExecutorExecute, quiesceExecuteReq(false)); env.OK || env.Error == nil || env.Error.Code != "not_ready" {
		t.Fatalf("execute during quiesce: %+v", env)
	}
	select {
	case env := <-execDone:
		t.Fatalf("in-flight execute finished early: %+v", env)
	default:
	}

	close(host.DoGate)
	select {
	case env := <-execDone:
		if !env.OK {
			t.Fatalf("drained execute failed: %+v", env.Error)
		}
		var resp struct {
			Payload []byte `json:"Payload"`
		}
		if err := json.Unmarshal(env.Result, &resp); err != nil || len(resp.Payload) == 0 {
			t.Fatalf("execute result: %s %v", env.Result, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight execute never finished")
	}

	select {
	case env := <-quiesceDone:
		if !env.OK {
			t.Fatalf("quiesce: %+v", env.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quiesce never finished")
	}

	if env := quiesceCallEnv(rt, pluginabi.MethodExecutorExecute, quiesceExecuteReq(false)); env.OK || env.Error == nil || env.Error.Code != "not_ready" {
		t.Fatalf("execute after quiesce: %+v", env)
	}
}

func TestQuiesceDrainsInFlightStream(t *testing.T) {
	host := newQuiesceHost()
	rt, err := makePlugin(host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Shutdown(context.Background()) })
	quiesceRegister(t, rt)

	req := quiesceExecuteReq(true)
	req["stream_id"] = "stream-quiesce"
	execDone := make(chan pluginabi.Envelope, 1)
	go func() { execDone <- quiesceCallEnv(rt, pluginabi.MethodExecutorExecuteStream, req) }()
	select {
	case <-host.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never reached upstream")
	}

	quiesceDone := make(chan pluginabi.Envelope, 1)
	go func() { quiesceDone <- quiesceCallEnv(rt, pluginabi.MethodPluginQuiesce, nil) }()
	waitForNotReadyQuiesce(t, rt)
	select {
	case env := <-quiesceDone:
		t.Fatalf("quiesce returned while stream in flight: %+v", env)
	case <-time.After(50 * time.Millisecond):
	}

	if env := quiesceCallEnv(rt, pluginabi.MethodExecutorExecute, quiesceExecuteReq(false)); env.OK || env.Error == nil || env.Error.Code != "not_ready" {
		t.Fatalf("execute during quiesce: %+v", env)
	}

	close(host.readGate)
	select {
	case env := <-execDone:
		if !env.OK {
			t.Fatalf("execute_stream: %+v", env.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execute_stream never returned")
	}
	select {
	case env := <-quiesceDone:
		if !env.OK {
			t.Fatalf("quiesce: %+v", env.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quiesce never finished")
	}

	emitted, closes := host.snapshot()
	if closes != 1 {
		t.Fatalf("stream closes = %d, want exactly 1", closes)
	}
	if emitted == 0 {
		t.Fatal("pump emitted nothing during drain")
	}

	if env := quiesceCallEnv(rt, pluginabi.MethodExecutorExecute, quiesceExecuteReq(false)); env.OK || env.Error == nil || env.Error.Code != "not_ready" {
		t.Fatalf("execute after quiesce: %+v", env)
	}
}

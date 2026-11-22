package main

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const replayFlowConfig = "enabled: true\npriority: 0\nreasoning-replay: {enabled: true, models: [spark, wonder]}\n"
const replayBlockConfig = "# unchanged replay selectors\nreasoning-replay:\n  models:\n    - spark\n    - 'wonder'\n  enabled: true\nenabled: true\n"

func TestPluginConfigEquivalent(t *testing.T) {
	for _, tc := range []struct {
		name, previous, proposed string
		want                     bool
	}{
		{"flow to block with reordered keys and default priority", replayFlowConfig, replayBlockConfig, true},
		{"explicit defaults", "", "{priority: 0, enabled: false, reasoning-replay: {enabled: false, models: []}}", true},
		{"changed selector", replayFlowConfig, "enabled: true\nreasoning-replay: {enabled: true, models: [spark, other]}", false},
		{"changed replay enabled", replayFlowConfig, "enabled: true\nreasoning-replay: {enabled: false, models: [spark, wonder]}", false},
		{"changed host enabled", replayFlowConfig, "enabled: false\nreasoning-replay: {enabled: true, models: [spark, wonder]}", false},
		{"changed host priority", replayFlowConfig, "enabled: true\npriority: 1\nreasoning-replay: {enabled: true, models: [spark, wonder]}", false},
		{"malformed", "", "reasoning-replay: [", false},
		{"wrong root type", "", "null", false},
		{"invalid priority", "", "priority: high", false},
		{"unknown root field", replayFlowConfig, replayFlowConfig + "unknown: true\n", false},
		{"unknown nested field", "", "reasoning-replay: {future-setting: true}", false},
		{"test endpoint falls back", "", "hostcompat_endpoint: http://127.0.0.1:12/muse-code/key", false},
		{"duplicate host field", "", "enabled: false\nenabled: true\n", false},
		{"duplicate nested field", "", "reasoning-replay: {enabled: true, enabled: false}", false},
		{"extra document", replayFlowConfig, replayFlowConfig + "---\n{}\n", false},
		{"malformed trailing document", replayFlowConfig, replayFlowConfig + "---\n[", false},
		{"partial decode", "reasoning-replay: {enabled: true}", "reasoning-replay: {enabled: true, models: {wrong: type}}", false},
		{"partially decoded sequence", replayFlowConfig, "enabled: true\nreasoning-replay: {enabled: true, models: [spark, null, wonder]}", false},
		{"coerced selector", "reasoning-replay: {models: ['42']}", "reasoning-replay: {models: [42]}", false},
		{"alias", "reasoning-replay: {models: [spark, spark]}", "reasoning-replay: {models: [&model spark, *model]}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := []byte(tc.previous), []byte(tc.proposed)
			if pluginConfigEquivalent(left, right) != tc.want || pluginConfigEquivalent(right, left) != tc.want {
				t.Fatalf("equivalence in both directions must be %t", tc.want)
			}
		})
	}
}

func TestConfigurePreservesPermissiveDecode(t *testing.T) {
	t.Cleanup(func() { _ = configureBase(context.Background(), nil) })
	raw := []byte("reasoning-replay: {enabled: true, models: {wrong: type}, unknown: true}\n---\n[")
	if err := configureBase(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if cfg := pluginConfigNow().ReasoningReplay; !cfg.Enabled || len(cfg.Models) != 0 {
		t.Fatalf("ordinary Configure lost the partially decoded settings: %+v", cfg)
	}
}

func TestConfigEquivalentKeepsActiveMetaStream(t *testing.T) {
	host := newQuiesceHost()
	rt, err := makePlugin(host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		rt.Shutdown(context.Background())
		_ = configureBase(context.Background(), nil)
	})
	defer func() {
		select {
		case <-host.readGate:
		default:
			close(host.readGate)
		}
	}()
	apply := func(method, raw string) pluginabi.Envelope {
		return quiesceCallEnv(rt, method, map[string]any{"schema_version": pluginabi.SchemaVersion, "config_yaml": []byte(raw)})
	}
	if env := apply(pluginabi.MethodPluginRegister, replayFlowConfig); !env.OK {
		t.Fatalf("register: %+v", env.Error)
	}

	req := quiesceExecuteReq(true)
	req["stream_id"] = "config-stream"
	done := make(chan pluginabi.Envelope, 1)
	go func() { done <- quiesceCallEnv(rt, pluginabi.MethodExecutorExecuteStream, req) }()
	select {
	case <-host.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never reached upstream")
	}
	reconfigured := make(chan pluginabi.Envelope, 1)
	go func() { reconfigured <- apply(pluginabi.MethodPluginReconfigure, replayBlockConfig) }()
	select {
	case env := <-reconfigured:
		if !env.OK {
			t.Fatalf("reconfigure: %+v", env.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("equivalent reconfigure waited for the active stream")
	}
	close(host.readGate)
	select {
	case env := <-done:
		if !env.OK {
			t.Fatalf("active stream: %+v", env.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active stream never returned")
	}
	select {
	case <-host.streamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("active stream never closed")
	}
	host.mu.Lock()
	payload, closes := bytes.Join(host.emitted, nil), host.closes
	host.mu.Unlock()
	if closes != 1 || !bytes.Contains(payload, []byte("pong")) || !bytes.Contains(payload, []byte("message_stop")) {
		t.Fatalf("active stream missed content or terminal: closes=%d, payload=%s", closes, payload)
	}
	if env := apply(pluginabi.MethodPluginReconfigure, "enabled: true\nreasoning-replay: {enabled: false, models: [other]}"); !env.OK {
		t.Fatalf("effective change: %+v", env.Error)
	}
	if cfg := pluginConfigNow().ReasoningReplay; cfg.Enabled || !slices.Equal(cfg.Models, []string{"other"}) {
		t.Fatalf("effective change did not apply: %+v", cfg)
	}
}

package main

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// stockMetaMuseNamespaceTool is a byte-exact copy of stock CLIProxy v8.0.51
// metaMuseNamespace (internal/runtime/executor/helps/meta_muse_tools.go in
// TogetherWeOwn/CLIProxyAPI, byte-identical to stock). Meta subscription-key
// attestation rejects tool-bearing requests whose Muse namespace drifts from
// stock (403 client_attestation_denied), so this golden pins the plugin
// constant: refresh it by copying stock again, never by editing by hand.
const stockMetaMuseNamespaceTool = `{"type":"namespace","name":"muse","description":"Muse Code tool set.","tools":[{"type":"function","name":"submit_reminder_decision","description":"Submit this reminder child's explicit decision.","parameters":{"additionalProperties":false,"properties":{"advisory_text":{"description":"Short text to deliver to the main agent when reminding.","type":["null","string"]},"confidence":{"description":"Confidence in the decision.","enum":["low","medium","high"],"type":["null","string"]},"decision":{"enum":["remind","none"],"type":"string"},"priority":{"description":"Requested reminder priority.","enum":["low","normal","high"],"type":["null","string"]},"reason":{"description":"Brief reason for reminding or staying silent.","type":"string"},"skill_id":{"description":"The listed skill id to remind when needs_reminder is true.","type":["null","string"]},"visible_for_steps":{"description":"Requested ephemeral visibility length in main model steps.","maximum":8,"minimum":1,"type":["null","integer"]}},"required":["advisory_text","confidence","decision","priority","reason","skill_id","visible_for_steps"],"type":"object"},"strict":true}]}`

// TestMetaMuseNamespaceToolMatchesStock pins the plugin namespace tool to the
// stock literal above: any drift breaks Meta attestation for tool use.
func TestMetaMuseNamespaceToolMatchesStock(t *testing.T) {
	if metaMuseNamespaceTool != stockMetaMuseNamespaceTool {
		t.Fatalf("metaMuseNamespaceTool drifted from stock (%d bytes vs %d stock bytes)",
			len(metaMuseNamespaceTool), len(stockMetaMuseNamespaceTool))
	}
	var v any
	if err := json.Unmarshal([]byte(metaMuseNamespaceTool), &v); err != nil {
		t.Fatalf("metaMuseNamespaceTool is not valid JSON: %v", err)
	}
}

// TestEnsureMetaMuseToolNamespaceAppendsStockTool proves the helper appends
// exactly the pinned stock tool to tool-bearing requests and leaves tool-less
// requests and existing Muse namespaces untouched.
func TestEnsureMetaMuseToolNamespaceAppendsStockTool(t *testing.T) {
	body := []byte(`{"model":"muse","tools":[{"type":"function","name":"get_time"}]}`)
	out := ensureMetaMuseToolNamespace(body)
	tools := gjson.GetBytes(out, "tools")
	if !tools.IsArray() || len(tools.Array()) != 2 {
		t.Fatalf("tools = %s, want 2 entries", tools.Raw)
	}
	got := tools.Array()[1].Raw
	if got != stockMetaMuseNamespaceTool {
		t.Fatalf("appended tool drifted from stock:\ngot  %.120s...\nwant %.120s...", got, stockMetaMuseNamespaceTool)
	}
	if gjson.GetBytes(out, "tools.0.name").String() != "get_time" {
		t.Fatalf("caller tool moved: %s", gjson.GetBytes(out, "tools").Raw)
	}

	toolLess := []byte(`{"model":"muse"}`)
	if out := ensureMetaMuseToolNamespace(toolLess); string(out) != string(toolLess) {
		t.Fatalf("tool-less body changed: %s", out)
	}
	existing := []byte(`{"tools":[` + stockMetaMuseNamespaceTool + `]}`)
	if out := ensureMetaMuseToolNamespace(existing); string(out) != string(existing) {
		t.Fatalf("existing namespace body changed: %s", out)
	}
}

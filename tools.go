package main

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Ports of the Meta tool-hygiene helpers from stock CLIProxy
// (internal/runtime/executor/helps/meta_muse_tools.go and meta_tools.go).
// They run after translation and before the upstream call, well before any
// user payload rules would run host-side, so ordering with payload config is
// preserved by construction (the host already applied payload rules before RPC).

// metaMuseNamespaceTool is the canonical Muse tool namespace appended to
// tool-bearing Responses requests. Subscription-key attestation can require a
// canonical Muse namespace even with current client headers.
const metaMuseNamespaceTool = `{"type":"namespace","name":"muse","description":"Muse Code tool set.","tools":[{"type":"function","name":"submit_reminder_decision","description":"Submit this reminder child's explicit decision.","parameters":{"additionalProperties":false,"properties":{"advisory_text":{"description":"Short text to deliver to the main agent when reminding.","type":["null","string"]},"confidence":{"description":"Confidence in the decision.","enum":["low","medium","high"],"type":["null","string"]},"decision":{"enum":["remind","none"],"type":"string"},"priority":{"description":"Requested reminder priority.","enum":["low","normal","high"],"type":["null","string"]},"reason":{"description":"Brief reason for reminding or staying silent.","type":"string"},"skill_id":{"description":"The listed skill id to remind when needs_reminder is true.","type":["null","string"]},"visible_for_steps":{"description":"Requested ephemeral visibility length in main model steps.","maximum":8,"minimum":1,"type":["null","integer"]}},"required":["advisory_text","confidence","decision","priority","reason","skill_id","visible_for_steps"],"type":"object"},"strict":true}]}`

// ensureMetaMuseToolNamespace appends the canonical namespace to tool-bearing
// Responses requests, preserving caller tool indexes. It leaves tool-less
// requests and existing Muse namespaces untouched.
func ensureMetaMuseToolNamespace(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	items := tools.Array()
	if len(items) == 0 {
		return body
	}
	for _, item := range items {
		if item.Get("type").String() == "namespace" && item.Get("name").String() == "muse" {
			return body
		}
	}
	raw := make([]byte, 0, len(metaMuseNamespaceTool)+len(tools.Raw)+1)
	raw = append(raw, '[')
	for i, item := range items {
		if i > 0 {
			raw = append(raw, ',')
		}
		raw = append(raw, item.Raw...)
	}
	raw = append(raw, ',')
	raw = append(raw, metaMuseNamespaceTool...)
	raw = append(raw, ']')
	updated, err := sjson.SetRawBytes(body, "tools", raw)
	if err != nil {
		return body
	}
	return updated
}

// sanitizeMetaWebSearchTools strips unsupported fields from web_search tool
// definitions: Meta rejects search_content_types on type "web_search" (it is
// only accepted on "web_search_preview" tools).
func sanitizeMetaWebSearchTools(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body
	}
	var pathsToDelete []string
	tools.ForEach(func(index, tool gjson.Result) bool {
		if tool.Get("type").String() == "web_search" && tool.Get("search_content_types").Exists() {
			pathsToDelete = append(pathsToDelete, fmt.Sprintf("tools.%d.search_content_types", index.Int()))
		}
		if tool.Get("type").String() == "namespace" {
			if subtools := tool.Get("tools"); subtools.Exists() && subtools.IsArray() {
				subtools.ForEach(func(subIndex, subtool gjson.Result) bool {
					if subtool.Get("type").String() == "web_search" && subtool.Get("search_content_types").Exists() {
						pathsToDelete = append(pathsToDelete, fmt.Sprintf("tools.%d.tools.%d.search_content_types", index.Int(), subIndex.Int()))
					}
					return true
				})
			}
		}
		return true
	})
	if len(pathsToDelete) == 0 {
		return body
	}
	out := body
	for _, p := range pathsToDelete {
		if updated, err := sjson.DeleteBytes(out, p); err == nil {
			out = updated
		}
	}
	return out
}

package main

import (
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This file is the plugin's answer to the stock-translator drift documented in
// replay.go: the pinned SDK translators drop the proxy's meta# provenance tag
// (CodexReplaySignatureForTarget has no Meta-tag branch), so the tag would be
// lost if the plugin translated first and filtered afterwards. The plugin
// therefore applies the same account policy to the client-format payload BEFORE
// translation, then translates the cleaned payload and applies the Codex-level
// policy plus the native-only sanitizer to the translated body as a second
// barrier. A kept envelope loses its tag in the prefilter, so each kept value
// is recorded via replay.approve for the second barrier, which would otherwise
// mistake it for an unattributed envelope and drop it. Response tagging happens
// on the Codex egress events, which the SDK response translators copy verbatim
// into client signatures, so no per-format egress work is needed.

// prefilterClientPayload applies the replay account policy to tagged envelopes
// carried in the client-format request. Only formats that can carry reasoning
// provenance are handled; anything else passes through and the stock
// translator's fail-closed drop remains the safety net (replay is lost, but a
// foreign envelope can never reach Meta).
func prefilterClientPayload(payload []byte, format sdktranslator.Format, replay metaReasoningReplay) []byte {
	if len(payload) == 0 {
		return payload
	}
	switch format {
	case sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex:
		return prefilterResponsesPayload(payload, replay)
	case sdktranslator.FormatClaude:
		return prefilterClaudePayload(payload, replay)
	case sdktranslator.FormatGemini:
		return prefilterGeminiPayload(payload, replay)
	default:
		return payload
	}
}

// prefilterResponsesPayload filters reasoning encrypted_content items of a
// Responses/Codex-format client payload. Kept envelopes are stripped to their
// native form; dropped ones lose encrypted_content, and items left with
// neither summary nor content are removed.
func prefilterResponsesPayload(payload []byte, replay metaReasoningReplay) []byte {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload
	}
	items := input.Array()
	rebuilt := make([]string, 0, len(items))
	changed := false
	for _, item := range items {
		encrypted := item.Get("encrypted_content")
		if item.Get("type").String() != "reasoning" || encrypted.Type != gjson.String {
			rebuilt = append(rebuilt, item.Raw)
			continue
		}
		if _, _, tagged := ParseMetaReasoningTag(encrypted.String()); !tagged {
			// Untagged client envelopes are left for the translated-body policy,
			// which knows the enabled/disabled distinction.
			rebuilt = append(rebuilt, item.Raw)
			continue
		}
		decision := replay.decide(encrypted.String())
		if !decision.drop {
			if decision.replay == encrypted.String() {
				rebuilt = append(rebuilt, item.Raw)
				continue
			}
			next, err := sjson.Set(item.Raw, "encrypted_content", decision.replay)
			if err == nil {
				// The tag is gone after translation, so record the stripped
				// envelope for the translated-body barrier (see replay.go).
				replay.approve(decision.replay)
				rebuilt = append(rebuilt, next)
				changed = true
				continue
			}
			decision = metaReasoningReplayDecision{drop: true, reason: "failed to strip provenance tag: " + err.Error()}
		}
		changed = true
		if metaReasoningItemIsEmpty(item) {
			continue
		}
		next, err := sjson.Delete(item.Raw, "encrypted_content")
		if err != nil {
			continue
		}
		rebuilt = append(rebuilt, next)
	}
	if !changed {
		return payload
	}
	out, err := sjson.SetRawBytes(payload, "input", []byte("["+strings.Join(rebuilt, ",")+"]"))
	if err != nil {
		return payload
	}
	return out
}

// prefilterClaudePayload filters thinking-block signatures of a Claude-format
// client payload. Kept envelopes are stripped; dropped ones lose the signature
// field while keeping the thinking text, which stays valid Claude input.
func prefilterClaudePayload(payload []byte, replay metaReasoningReplay) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}
	out := payload
	changed := false
	for i, msg := range messages.Array() {
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}
		for j, part := range content.Array() {
			ptype := part.Get("type").String()
			if ptype != "thinking" && ptype != "redacted_thinking" {
				continue
			}
			sig := part.Get("signature")
			if sig.Type != gjson.String || sig.String() == "" {
				continue
			}
			if _, _, tagged := ParseMetaReasoningTag(sig.String()); !tagged {
				continue
			}
			decision := replay.decide(sig.String())
			path := "messages." + itoa(i) + ".content." + itoa(j) + ".signature"
			if !decision.drop {
				if decision.replay == sig.String() {
					continue
				}
				next, err := sjson.SetBytes(out, path, decision.replay)
				if err != nil {
					continue
				}
				replay.approve(decision.replay)
				out, changed = next, true
				continue
			}
			next, err := sjson.DeleteBytes(out, path)
			if err != nil {
				continue
			}
			out, changed = next, true
		}
	}
	_ = changed
	return out
}

// prefilterGeminiPayload filters Gemini thought signatures. Both spellings the
// SDK reads (thoughtSignature and thought_signature) are handled; dropped ones
// lose the field.
func prefilterGeminiPayload(payload []byte, replay metaReasoningReplay) []byte {
	out := payload
	changed := false
	for _, path := range geminiThoughtSignaturePaths(payload) {
		raw := gjson.GetBytes(out, path).String()
		if _, _, tagged := ParseMetaReasoningTag(raw); !tagged {
			continue
		}
		decision := replay.decide(raw)
		if !decision.drop {
			if decision.replay == raw {
				continue
			}
			next, err := sjson.SetBytes(out, path, decision.replay)
			if err != nil {
				continue
			}
			replay.approve(decision.replay)
			out, changed = next, true
			continue
		}
		next, err := sjson.DeleteBytes(out, path)
		if err != nil {
			continue
		}
		out, changed = next, true
	}
	_ = changed
	return out
}

// geminiThoughtSignaturePaths enumerates thought-signature JSON paths in a
// Gemini payload: contents[].parts[] under either spelling.
func geminiThoughtSignaturePaths(payload []byte) []string {
	var paths []string
	contents := gjson.GetBytes(payload, "contents")
	if !contents.IsArray() {
		return nil
	}
	for i, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for j, part := range parts.Array() {
			base := "contents." + itoa(i) + ".parts." + itoa(j)
			if part.Get("thoughtSignature").Type == gjson.String && part.Get("thoughtSignature").String() != "" {
				paths = append(paths, base+".thoughtSignature")
			}
			if part.Get("thought_signature").Type == gjson.String && part.Get("thought_signature").String() != "" {
				paths = append(paths, base+".thought_signature")
			}
		}
	}
	return paths
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

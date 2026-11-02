package main

import (
	"fmt"
	"net/url"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This file ports the Meta-target reasoning sanitizer from stock CLIProxy
// (internal/runtime/executor/openai_responses_signature.go). Only the
// Meta-target path is ported: this provider serves Meta Muse models
// exclusively, so the GPT/Grok/Gemini/Claude/Kimi/SWE branches of the native
// sanitizer are unreachable. Where the native code would consult those
// branches, this port strips fail-closed with a logged reason instead.
//
// The sanitizer still strips anything that is not a recognized Meta envelope
// (fail-closed): a future Meta envelope revision will be stripped until the
// inspector in signature.go learns it.

type metaSignaturePolicy int

const (
	// metaSignatureForeign: target is not Meta; a recognized Meta envelope is foreign.
	metaSignatureForeign metaSignaturePolicy = iota
	// metaSignatureNativeOnly: native Meta upstream (api.meta.ai); only unsuffixed envelopes replay.
	metaSignatureNativeOnly
	// metaSignatureAllowRouted: Meta-family model served by a router (e.g. OpenRouter meta/muse-*); native or router-suffixed envelopes replay verbatim.
	metaSignatureAllowRouted
)

// sanitizeMetaReasoningEnvelopes is the Meta-target variant of the native
// sanitizer. It is deliberately native-only for api.meta.ai (suffix acceptance
// there is unverified); a third-party Responses provider configured under a
// custom base URL may emit and consume router-suffixed Meta Muse envelopes,
// which replay verbatim there (or with the OpenRouter suffix stripped for
// other providers).
func sanitizeMetaReasoningEnvelopes(body []byte, baseURL string) []byte {
	if isNativeMetaUpstreamURL(baseURL) {
		return sanitizeMetaReasoningWithOptions(body, metaSignatureNativeOnly, false)
	}
	return sanitizeMetaReasoningWithOptions(body, metaSignatureAllowRouted, isOpenRouterUpstreamURL(baseURL))
}

func isNativeMetaUpstreamURL(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return true
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "" || host == "api.meta.ai" || strings.HasSuffix(host, ".meta.ai")
}

func isOpenRouterUpstreamURL(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai")
}

func stripMetaRouterSuffix(rawSignature, suffix string) string {
	if suffix == "" || len(rawSignature) <= len(suffix)+1 {
		return rawSignature
	}
	return rawSignature[:len(rawSignature)-len(suffix)-1]
}

// openaiResponsesReasoningSummaryIsEmpty reports whether a reasoning item
// carries no summary of its own.
func openaiResponsesReasoningSummaryIsEmpty(summary gjson.Result) bool {
	if !summary.Exists() || summary.Type == gjson.Null {
		return true
	}
	return summary.IsArray() && len(summary.Array()) == 0
}

func promoteOpenAIResponsesReasoningTextToSummary(itemRaw string, content gjson.Result) (string, error) {
	var b strings.Builder
	b.WriteByte('[')
	n := 0
	for _, part := range content.Array() {
		if strings.TrimSpace(part.Get("type").String()) != "reasoning_text" {
			continue
		}
		text := part.Get("text").String()
		if text == "" {
			continue
		}
		partJSON, err := sjson.Set(`{"type":"summary_text"}`, "text", text)
		if err != nil {
			return itemRaw, err
		}
		if n > 0 {
			b.WriteByte(',')
		}
		b.WriteString(partJSON)
		n++
	}
	b.WriteByte(']')
	if n == 0 {
		return itemRaw, nil
	}
	return sjson.SetRaw(itemRaw, "summary", b.String())
}

// sanitizeMetaReasoningWithOptions applies the native-only Meta envelope
// policy to every reasoning item in a Responses request body. Items without
// usable encrypted_content keep their summary and content; orphan reasoning
// ids are stripped unless the request explicitly opts into store=true.
func sanitizeMetaReasoningWithOptions(body []byte, metaPolicy metaSignaturePolicy, allowRouterSuffix bool) []byte {
	const provider = "meta-muse executor"
	inputResult := gjson.GetBytes(body, "input")
	if !inputResult.Exists() || !inputResult.IsArray() {
		return body
	}

	// Codex backend rejects store=true and does not persist items when store=false.
	// A reasoning item that still carries an id without usable encrypted_content is
	// treated as a store lookup and fails with:
	//   Item with id '...' not found. Items are not persisted when `store` is set to false.
	// Strip those orphan ids unless the request explicitly opts into store=true.
	stripOrphanReasoningIDs := !gjson.GetBytes(body, "store").Bool()

	items := inputResult.Array()

	// rebuilt accumulates the edited "input" array as JSON array bytes. It
	// stays nil while no item needs editing so the common case (nothing to
	// sanitize) does no allocation or rebuilding. Edits are applied directly
	// to each item's own raw JSON rather than re-parsing the whole body,
	// keeping the cost proportional to the item being edited.
	var rebuilt []byte
	itemsWritten := 0
	keep := func(raw string) {
		if rebuilt == nil {
			return
		}
		if itemsWritten > 0 {
			rebuilt = append(rebuilt, ',')
		}
		rebuilt = append(rebuilt, raw...)
		itemsWritten++
	}
	startRebuild := func(index int) {
		if rebuilt != nil {
			return
		}
		// First item that needs editing: start the buffer and backfill
		// it with the raw JSON of every preceding item.
		rebuilt = make([]byte, 0, len(inputResult.Raw))
		rebuilt = append(rebuilt, '[')
		for i := range index {
			keep(items[i].Raw)
		}
	}

	for index, item := range items {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			keep(item.Raw)
			continue
		}

		encryptedContent := item.Get("encrypted_content")
		itemID := strings.TrimSpace(item.Get("id").String())
		if itemID == "" {
			itemID = fmt.Sprintf("input[%d]", index)
		}

		nextItem := item.Raw
		changed := false

		// Official Codex schema sets maxItems: 0 on reasoning.content. Third-party
		// channels replay cleartext thinking there; promote it into summary when
		// summary is empty, then force content to [].
		content := item.Get("content")
		if content.IsArray() && len(content.Array()) > 0 {
			if openaiResponsesReasoningSummaryIsEmpty(item.Get("summary")) {
				promoted, errPromote := promoteOpenAIResponsesReasoningTextToSummary(nextItem, content)
				if errPromote != nil {
					log.Debugf("%s: failed to promote reasoning_text into summary at input[%d]: %v", provider, index, errPromote)
				} else {
					nextItem = promoted
				}
			}
			cleared, errClear := sjson.SetRaw(nextItem, "content", "[]")
			if errClear != nil {
				log.Debugf("%s: failed to clear reasoning content at input[%d]: %v", provider, index, errClear)
			} else {
				nextItem = cleared
				changed = true
				log.Debugf("%s: cleared reasoning content at input[%d] item_id=%q", provider, index, itemID)
			}
		}

		if !encryptedContent.Exists() {
			if stripOrphanReasoningIDs && item.Get("id").Exists() {
				dropped, err := sjson.Delete(nextItem, "id")
				if err != nil {
					log.Debugf("%s: failed to drop orphan reasoning id at input[%d]: %v", provider, index, err)
				} else {
					nextItem = dropped
					changed = true
					log.Debugf("%s: dropped orphan reasoning id at input[%d] item_id=%q reason=missing encrypted_content with store disabled", provider, index, itemID)
				}
			}
			if !changed {
				keep(item.Raw)
				continue
			}
			startRebuild(index)
			keep(nextItem)
			continue
		}

		reason := ""
		rewrittenSignature := ""
		switch encryptedContent.Type {
		case gjson.String:
			rawSignature := encryptedContent.String()
			if rawSignature != strings.TrimSpace(rawSignature) {
				reason = "encrypted_content has leading or trailing whitespace"
			} else if metaPolicy == metaSignatureForeign {
				// Unreachable for this provider (it only serves Meta
				// targets); strip fail-closed rather than risk a wrong
				// signature reaching the upstream.
				reason = "meta-muse executor serves Meta targets only"
			} else {
				// Meta targets never reach a provider-first gate: a wrong
				// signature must not replay to Meta. Only a positively
				// recognized Meta envelope survives, and the native Meta
				// upstream additionally refuses the router suffix a router
				// such as OpenRouter appends.
				if info, err := InspectMetaReasoningSignature(rawSignature); err != nil {
					reason = err.Error()
				} else if info.RouterSuffix != "" {
					if metaPolicy == metaSignatureNativeOnly {
						reason = "router-suffixed Meta reasoning signature is not accepted by the native Meta upstream"
					} else if !allowRouterSuffix {
						rewrittenSignature = stripMetaRouterSuffix(rawSignature, info.RouterSuffix)
					}
				}
			}
		case gjson.Null:
			reason = "encrypted_content is null"
		default:
			reason = fmt.Sprintf("encrypted_content must be a string, got %s", encryptedContent.Type.String())
		}
		if reason == "" {
			if rewrittenSignature != "" {
				if updatedItem, errSet := sjson.Set(nextItem, "encrypted_content", rewrittenSignature); errSet == nil {
					nextItem = updatedItem
					changed = true
					log.Debugf("%s: stripped router suffix from reasoning encrypted_content at input[%d] item_id=%q", provider, index, itemID)
				}
			}
			if !changed {
				keep(item.Raw)
				continue
			}
			startRebuild(index)
			keep(nextItem)
			continue
		}

		dropped, err := sjson.Delete(nextItem, "encrypted_content")
		if err != nil {
			log.Debugf("%s: failed to drop invalid reasoning encrypted_content at input[%d]: %v", provider, index, err)
			if !changed {
				keep(item.Raw)
				continue
			}
			startRebuild(index)
			keep(nextItem)
			continue
		}
		nextItem = dropped
		changed = true
		if stripOrphanReasoningIDs && item.Get("id").Exists() {
			if nextID, errID := sjson.Delete(nextItem, "id"); errID != nil {
				log.Debugf("%s: failed to drop reasoning id after invalid encrypted_content at input[%d]: %v", provider, index, errID)
			} else {
				nextItem = nextID
			}
		}

		startRebuild(index)
		keep(nextItem)

		log.Debugf("%s: dropped invalid reasoning encrypted_content at input[%d] item_id=%q reason=%s", provider, index, itemID, reason)
	}

	if rebuilt == nil {
		return body
	}
	rebuilt = append(rebuilt, ']')

	updated, err := sjson.SetRawBytes(body, "input", rebuilt)
	if err != nil {
		log.Debugf("%s: failed to rebuild input array while sanitizing reasoning encrypted_content: %v", provider, err)
		return body
	}
	return updated
}

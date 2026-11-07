package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestSDK59GeminiCarrierFailClosed(t *testing.T) {
	envelope := parityEnvelope(t)
	tagged := TagMetaReasoning(metaReasoningAccountKey("auth-a"), envelope)
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	for _, value := range []string{
		"not base64!",
		encode("unrecognized-carrier:" + tagged),
		encode("cpa-codex-gemini-carrier-v1:" + encode("cpa-codex-gemini-carrier-v1:"+tagged)),
		encode("cpa-codex-gemini-carrier-v1:" + TagMetaReasoning(metaReasoningAccountKey("auth-a"), "invalid-envelope")),
		encode("cpa-codex-gemini-carrier-v1:" + envelope),
	} {
		replay := parityReplay("auth-a", "")
		payload := []byte(`{"contents":[{"role":"model","parts":[{"thought":true,"text":"hidden","thoughtSignature":` + strconv.Quote(value) + `}]},{"role":"user","parts":[{"text":"continue"}]}]}`)
		filtered := prefilterClientPayload(payload, sdktranslator.FormatGemini, replay)
		body := replay.sanitizeCodexRequest(sdktranslator.TranslateRequest(sdktranslator.FormatGemini, sdktranslator.FormatCodex, "muse-spark-1.3", filtered, false))
		for _, item := range gjson.GetBytes(body, "input").Array() {
			if item.Get("encrypted_content").Exists() {
				t.Fatalf("malformed/unattributed carrier gained replay authority: %s", body)
			}
		}
		if !strings.Contains(string(body), "continue") {
			t.Fatalf("carrier filtering lost user input: %s", body)
		}
	}
}

func TestSDK59GeminiCarrierReplay(t *testing.T) {
	envelope := parityEnvelope(t)
	item := `{"type":"reasoning","id":"rs_carrier","encrypted_content":` + strconv.Quote(envelope) + `,"summary":[{"type":"summary_text","text":"deep"}]}`
	sse := "data: " + `{"type":"response.output_item.done","output_index":0,"item":` + item + `}` + "\n\n" +
		"data: " + `{"type":"response.output_text.delta","delta":"answer"}` + "\n\n" +
		"data: " + `{"type":"response.completed","response":{"id":"resp_carrier","model":"muse-spark-1.3","status":"completed","output":[]}}` + "\n\n"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			prepared := &preparedMetaRequest{responseFormat: sdktranslator.FormatGemini, replay: parityReplay("auth-a", "")}
			var parts []string
			collect := func(payload []byte) {
				line := strings.TrimSpace(strings.TrimPrefix(string(payload), "data: "))
				for _, part := range gjson.Get(line, "candidates.0.content.parts").Array() {
					parts = append(parts, part.Raw)
				}
			}
			if stream {
				in := make(chan pluginapi.HTTPStreamChunk, len(sse))
				for i := range sse {
					in <- pluginapi.HTTPStreamChunk{Payload: []byte(sse[i : i+1])}
				}
				close(in)
				out := make(chan pluginapi.ExecutorStreamChunk, 20)
				go pumpMetaStream(ctx, newMetaExecutor(nil, nil), pluginapi.ExecutorRequest{Model: "muse-spark-1.3"}, prepared, in, out)
				for chunk := range out {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					collect(chunk.Payload)
				}
			} else {
				completed, errEvent := translateMetaCompleted(ctx, prepared, pluginapi.ExecutorRequest{}, []byte(sse))
				if completed == nil || errEvent != nil {
					t.Fatalf("buffered carrier response failed: %s", errEvent)
				}
				var state any
				collect(sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatCodex, sdktranslator.FormatGemini, "muse-spark-1.3", nil, nil, completed, &state))
			}
			carrierCount := 0
			for _, part := range parts {
				if signature := gjson.Get(part, "thoughtSignature").String(); signature != "" {
					carrierCount++
					decoded, err := base64.StdEncoding.DecodeString(signature)
					want := "cpa-codex-gemini-carrier-v1:" + TagMetaReasoning(metaReasoningAccountKey("auth-a"), envelope)
					if err != nil || string(decoded) != want {
						t.Fatalf("stock SDK carrier lost provenance: %q/%v", decoded, err)
					}
				}
			}
			if carrierCount != 1 {
				t.Fatalf("carrier count=%d parts=%v", carrierCount, parts)
			}
			for _, spelling := range []string{"thoughtSignature", "thought_signature"} {
				for _, principal := range []string{"auth-a", "auth-b", "", "disabled"} {
					t.Run(spelling+"/"+principal, func(t *testing.T) {
						replay := parityReplay(principal, "")
						if principal == "disabled" {
							replay = newMetaReasoningReplay(metaReasoningReplayConfig{}, "muse-spark-1.3", "muse-spark-1.3", nil, nil, "auth-a", metaDefaultAPIBaseURL)
						}
						payload := []byte(`{"contents":[{"role":"model","parts":[` + strings.Join(parts, ",") + `]},{"role":"user","parts":[{"text":"continue"}]}]}`)
						payload = []byte(strings.ReplaceAll(string(payload), "thoughtSignature", spelling))
						filtered := prefilterClientPayload(payload, sdktranslator.FormatGemini, replay)
						body := replay.sanitizeCodexRequest(sdktranslator.TranslateRequest(sdktranslator.FormatGemini, sdktranslator.FormatCodex, "muse-spark-1.3", filtered, stream))
						kept := 0
						for _, input := range gjson.GetBytes(body, "input").Array() {
							if encrypted := input.Get("encrypted_content"); encrypted.Exists() {
								kept++
								if encrypted.String() != envelope {
									t.Fatalf("non-native carrier/tag reached upstream: %s", body)
								}
							}
						}
						want := 0
						if principal == "auth-a" {
							want = 1
						} else if replay.stats.droppedForeign != 1 {
							t.Fatalf("foreign/unknown/disabled drop not counted: %+v", replay.stats)
						}
						if kept != want || !strings.Contains(string(body), "continue") || (want == 1 && !strings.Contains(string(body), "deep")) {
							t.Fatalf("principal=%q kept=%d want=%d, content=%s", principal, kept, want, body)
						}
					})
				}
			}
		})
	}
}

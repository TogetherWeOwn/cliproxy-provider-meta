package main

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// Passive subscription-usage observation, ported from stock CLIProxy
// (internal/runtime/executor/helps/meta_quota.go). Meta sends one
// response.subscription_usage event after response.completed per response:
//
//	{"type":"response.subscription_usage","subscription":{"tier":"…",
//	 "window":{"used_percent":0,"window_duration_mins":300,"resets_at":…},
//	 "weekly":{"used_percent":5,"resets_at":…}}}
//
// The event is filtered from both the buffered and streaming paths so it never
// reaches the client, and converted here to the bounded header representation
// the quota cache stores. Header names follow the X-Codex-* pattern so the
// passive snapshot and the management API treat both alike.

// Quota signal header names for Meta subscription usage.
const (
	metaQuotaTierHeader          = "X-Meta-Tier"
	metaQuotaWindowUsedHeader    = "X-Meta-Window-Used-Percent"
	metaQuotaWindowMinutesHeader = "X-Meta-Window-Minutes"
	metaQuotaWindowResetHeader   = "X-Meta-Window-Reset-At"
	metaQuotaWeeklyUsedHeader    = "X-Meta-Weekly-Used-Percent"
	metaQuotaWeeklyResetHeader   = "X-Meta-Weekly-Reset-At"
)

// parseMetaSubscriptionUsageHeaders converts one subscription usage event to
// headers, or nil when the event carries no usable window.
func parseMetaSubscriptionUsageHeaders(payload []byte) http.Header {
	if len(payload) == 0 || gjson.GetBytes(payload, "type").String() != metaSubscriptionUsageEventType {
		return nil
	}
	return metaSubscriptionUsageHeaders(gjson.GetBytes(payload, "subscription"))
}

// parseMetaSubscriptionUsageSSE finds the last subscription usage event in a
// buffered Responses event stream.
func parseMetaSubscriptionUsageSSE(body []byte) http.Header {
	var headers http.Header
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if parsed := parseMetaSubscriptionUsageHeaders(bytes.TrimSpace(line[len("data:"):])); parsed != nil {
			headers = parsed
		}
	}
	return headers
}

func metaSubscriptionUsageHeaders(subscription gjson.Result) http.Header {
	if !subscription.IsObject() {
		return nil
	}
	headers := make(http.Header)
	window := subscription.Get("window")
	minutes := window.Get("window_duration_mins")
	if used, reset, ok := metaQuotaWindow(window); ok && minutes.Type == gjson.Number && minutes.Int() > 0 {
		headers.Set(metaQuotaWindowUsedHeader, used)
		headers.Set(metaQuotaWindowMinutesHeader, strconv.FormatInt(minutes.Int(), 10))
		headers.Set(metaQuotaWindowResetHeader, reset)
	}
	if used, reset, ok := metaQuotaWindow(subscription.Get("weekly")); ok {
		headers.Set(metaQuotaWeeklyUsedHeader, used)
		headers.Set(metaQuotaWeeklyResetHeader, reset)
	}
	if len(headers) == 0 {
		return nil
	}
	if tier := strings.TrimSpace(subscription.Get("tier").String()); tier != "" && len(tier) <= 128 && !strings.ContainsAny(tier, "\r\n") {
		headers.Set(metaQuotaTierHeader, tier)
	}
	return headers
}

func metaQuotaWindow(window gjson.Result) (used, reset string, ok bool) {
	if !window.IsObject() {
		return "", "", false
	}
	usedPercent := window.Get("used_percent")
	resetsAt := window.Get("resets_at")
	if usedPercent.Type != gjson.Number || usedPercent.Float() < 0 || resetsAt.Type != gjson.Number || resetsAt.Int() <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(usedPercent.Raw), strconv.FormatInt(resetsAt.Int(), 10), true
}

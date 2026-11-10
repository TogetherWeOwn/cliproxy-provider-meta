package main

import (
	"net/http"
	"strconv"
	"testing"
)

func TestContributorFinalizerReusedSession(t *testing.T) {
	headers := http.Header{
		"X-Tbh-Session-Id":       {"retained-session"},
		"X-Tbh-Contributor-Mode": {"1", "0"},
		"x-tbh-contributor-mode": {"1"},
		"X-TBH-CONTRIBUTOR-MODE": {"0"},
	}
	for _, model := range []string{"plain", "muse-spark-1.3-contributor", "muse-spark-1.3-contributor", "plain"} {
		applyMetaClientIdentity(headers)
		applyMetaContributorMode(headers, []byte(`{"model":`+strconv.Quote(model)+`}`))
		assertContributorHeader(t, headers, model != "plain")
		if headers.Get("X-Tbh-Session-Id") != "retained-session" {
			t.Fatal("reused session changed")
		}
	}
}

func TestContributorFinalModelParsing(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{" MUSE-SPARK-1.3-CoNtRiBuToR(HIGH) ", true},
		{"muse-spark-1.3-contributor(0)", true},
		{"muse-spark-1.3-contributor(-1)", true},
		{"muse-spark-1.3-contributor(xhigh)", true},
		{"muse-spark-1.3-contributor(16384)", true},
		{"muse-spark-1.3-contributor(high)(low)", false},
		{"muse-spark-1.3-contributor(high", false},
		{"muse-spark-1.3-contributor)", false},
		{"muse-spark-1.3-contributor-extra(high)", false},
		{"muse-contributor-spark-1.3(high)", false},
		{"muse-spark-1.3(high)", false},
		{"", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			headers := http.Header{"x-tbh-contributor-mode": {"spoof"}}
			applyMetaContributorMode(headers, []byte(`{"model":`+strconv.Quote(tc.model)+`}`))
			assertContributorHeader(t, headers, tc.want)
		})
	}
	for _, body := range []string{
		`{"model":"muse-spark-1.3-contributor","model":"plain"}`,
		`{"model":"plain","model":"muse-spark-1.3-contributor"}`,
		`{"model":null}`, `{"model":{}}`, `{"model":true}`, `null`,
		`{"model":"muse-spark-1.3-contributor"} {"model":"plain"}`,
	} {
		headers := http.Header{"X-Tbh-Contributor-Mode": {"1"}}
		applyMetaContributorMode(headers, []byte(body))
		assertContributorHeader(t, headers, false)
	}
}

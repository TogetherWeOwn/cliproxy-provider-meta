//go:build hostcompat

package main

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

var testEndpoint atomic.Value

func configureBase(_ context.Context, raw []byte) error {
	var cfg struct {
		Endpoint string `yaml:"hostcompat_endpoint"`
	}
	if yaml.Unmarshal(raw, &cfg) != nil {
		return errors.New("invalid test configuration")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		return errors.New("test upstream must be loopback")
	}
	if !strings.HasSuffix(u.Path, "/muse-code/key") {
		return errors.New("test upstream must serve the key envelope path")
	}
	testEndpoint.Store(strings.TrimSuffix(cfg.Endpoint, "/"))
	return nil
}

// Host compatibility binaries are test-only and refuse non-loopback upstreams.
func selectedEndpoint(_ string) string {
	if endpoint := testEndpoint.Load(); endpoint != nil {
		return endpoint.(string)
	}
	return "http://127.0.0.1:0/muse-code/key"
}

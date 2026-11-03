package main

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestRefreshAuthPassThroughKeepsDisabled proves a refresh of a credential
// that already carries a key preserves the disabled flag: the host backfills
// identity fields on refresh but not Disabled, so dropping it here would
// re-enable a disabled credential (disk disabled:false, status active,
// inference served).
func TestRefreshAuthPassThroughKeepsDisabled(t *testing.T) {
	cases := []struct {
		name     string
		storage  string
		metadata map[string]any
		want     bool
	}{
		{
			name:     "metadata disabled stays disabled",
			storage:  `{"type":"meta-muse","api_key":"sk-test"}`,
			metadata: map[string]any{"disabled": true},
			want:     true,
		},
		{
			name:     "storage disabled stays disabled",
			storage:  `{"type":"meta-muse","api_key":"sk-test","disabled":true}`,
			metadata: map[string]any{},
			want:     true,
		},
		{
			name:     "absent flag stays enabled",
			storage:  `{"type":"meta-muse","api_key":"sk-test"}`,
			metadata: map[string]any{},
			want:     false,
		},
		{
			name:     "explicit false stays enabled",
			storage:  `{"type":"meta-muse","api_key":"sk-test"}`,
			metadata: map[string]any{"disabled": false},
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := (&metaAuthProvider{}).RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{
				AuthID:      "auth-1",
				StorageJSON: []byte(tc.storage),
				Metadata:    tc.metadata,
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Auth.Disabled != tc.want {
				t.Fatalf("Disabled = %v, want %v", resp.Auth.Disabled, tc.want)
			}
		})
	}
}

// TestRefreshAuthMintKeepsDisabled proves the DCA-mint refresh path also
// preserves the disabled flag and never lets merged storage clobber the
// metadata flag ParseAuth records.
func TestRefreshAuthMintKeepsDisabled(t *testing.T) {
	fake := &fakeSecurityHTTPClient{}
	fake.do = func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.Contains(req.URL, "muse-code/key") {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"api_key":"sk-minted","base_url":"https://api.meta.ai/v1"}`)}, nil
		}
		t.Fatalf("unexpected upstream call %s", req.URL)
		return pluginapi.HTTPResponse{StatusCode: 500}, nil
	}
	resp, err := (&metaAuthProvider{}).RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{
		AuthID:      "auth-1",
		StorageJSON: []byte(`{"type":"meta-muse","dca_token":"dca:test"}`),
		Metadata:    map[string]any{"disabled": true},
		HTTPClient:  fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Auth.Disabled {
		t.Fatalf("Disabled = false, want true after mint refresh")
	}
	if disabled, _ := resp.Auth.Metadata["disabled"].(bool); !disabled {
		t.Fatalf("metadata disabled = %v, want true (merged storage clobbered it)", resp.Auth.Metadata["disabled"])
	}
}

// TestRefreshAuthMintEnabledStaysEnabled proves the mint path leaves an
// enabled credential enabled.
func TestRefreshAuthMintEnabledStaysEnabled(t *testing.T) {
	fake := &fakeSecurityHTTPClient{}
	fake.do = func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"api_key":"sk-minted","base_url":"https://api.meta.ai/v1"}`)}, nil
	}
	resp, err := (&metaAuthProvider{}).RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{
		AuthID:      "auth-1",
		StorageJSON: []byte(`{"type":"meta-muse","dca_token":"dca:test"}`),
		Metadata:    map[string]any{},
		HTTPClient:  fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Auth.Disabled {
		t.Fatalf("Disabled = true, want false for an enabled credential")
	}
}

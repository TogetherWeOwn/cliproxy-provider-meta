package main

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// metaMuseNamespace is the plugin's vendor model namespace. Every model this
// plugin registers or serves is meta-muse/<id>, so Meta Muse models can never
// clash with a future official provider.
const metaMuseNamespace = "meta-muse"

// requestedModelMetadataKey carries the host-recorded client-requested model
// name through executor metadata for replay canary matching.
const requestedModelMetadataKey = "requested_model"

// metaReasoningReplayConfig mirrors the stock host knob
// (config.MetaReasoningReplayConfig): account-bound Muse reasoning replay with
// an optional canary model list. Patterns are case-insensitive "*" globs;
// empty means every Meta model when enabled.
type metaReasoningReplayConfig struct {
	Enabled bool     `yaml:"enabled" json:"enabled"`
	Models  []string `yaml:"models,omitempty" json:"models,omitempty"`
}

// appliesToModel reports whether replay is on for any of the candidate names.
func (c metaReasoningReplayConfig) appliesToModel(modelNames ...string) bool {
	if !c.Enabled {
		return false
	}
	var patterns []string
	for _, pattern := range c.Models {
		if pattern = strings.ToLower(strings.TrimSpace(pattern)); pattern != "" {
			patterns = append(patterns, pattern)
		}
	}
	if len(patterns) == 0 {
		return true
	}
	for _, name := range modelNames {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		for _, pattern := range patterns {
			if matchModelGlob(pattern, name) {
				return true
			}
		}
	}
	return false
}

// matchModelGlob matches value against a pattern whose only metacharacter is "*".
func matchModelGlob(pattern, value string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	last := parts[len(parts)-1]
	if last != "" && !strings.HasSuffix(value, last) {
		return false
	}
	if last != "" {
		value = value[:len(value)-len(last)]
	}
	for _, middle := range parts[1 : len(parts)-1] {
		if middle == "" {
			continue
		}
		index := strings.Index(value, middle)
		if index < 0 {
			return false
		}
		value = value[index+len(middle):]
	}
	return true
}

// pluginConfig is this plugin's configuration subtree: replay canary only.
// Quota behavior is unchanged (30-minute per-auth envelope polls).
type pluginConfig struct {
	ReasoningReplay metaReasoningReplayConfig `yaml:"reasoning-replay"`
}

func parsePluginConfig(raw []byte) pluginConfig {
	var cfg pluginConfig
	if len(raw) == 0 {
		return cfg
	}
	_ = yaml.Unmarshal(raw, &cfg)
	return cfg
}

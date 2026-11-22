package main

import (
	"bytes"
	"io"
	"reflect"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
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
	cfg, _, _ := decodePluginConfig(yaml.NewDecoder(bytes.NewReader(raw)))
	return cfg
}

func decodePluginConfig(decoder *yaml.Decoder) (pluginConfig, yaml.Node, error) {
	var cfg pluginConfig
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		return cfg, doc, err
	}
	// Keep any partially decoded settings for the ordinary Configure path.
	err := doc.Decode(&cfg)
	return cfg, doc, err
}

type effectivePluginConfig struct {
	pluginConfig
	enabled  bool
	priority int
	store    *pluginstore.Manifest
}

func pluginConfigEquivalent(previous, proposed []byte) bool {
	left, valid := validatedPluginConfig(previous)
	if !valid {
		return false
	}
	right, valid := validatedPluginConfig(proposed)
	return valid && reflect.DeepEqual(left, right)
}

func validatedPluginConfig(raw []byte) (effectivePluginConfig, bool) {
	var effective effectivePluginConfig
	// Bound the work done by the optional comparison during reconfigure.
	if len(raw) > 64*1024 {
		return effective, false
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	cfg, doc, err := decodePluginConfig(decoder)
	if err == io.EOF {
		return effective, true
	}
	var extra yaml.Node
	if err != nil || decoder.Decode(&extra) != io.EOF {
		return effective, false
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode || root.Tag != "!!map" {
		return effective, false
	}
	for i := 0; i < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Tag != "!!str" {
			return effective, false
		}
		switch key.Value {
		case "enabled":
			if value.Tag != "!!bool" || value.Decode(&effective.enabled) != nil {
				return effective, false
			}
		case "priority":
			if value.Tag != "!!int" || value.Decode(&effective.priority) != nil {
				return effective, false
			}
		case "reasoning-replay":
			if !validReplayConfig(value) {
				return effective, false
			}
		case "store":
			effective.store = validatedStoreManifest(value)
			if effective.store == nil {
				return effective, false
			}
		default:
			return effective, false
		}
	}
	if len(cfg.ReasoningReplay.Models) == 0 {
		cfg.ReasoningReplay.Models = nil
	}
	effective.pluginConfig = cfg
	return effective, true
}

func validReplayConfig(node *yaml.Node) bool {
	if node.Kind != yaml.MappingNode || node.Tag != "!!map" {
		return false
	}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Tag != "!!str" {
			return false
		}
		switch key.Value {
		case "enabled":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
				return false
			}
		case "models":
			if value.Kind != yaml.SequenceNode || value.Tag != "!!seq" {
				return false
			}
			for _, model := range value.Content {
				if model.Kind != yaml.ScalarNode || model.Tag != "!!str" {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

func validatedStoreManifest(node *yaml.Node) *pluginstore.Manifest {
	var manifest pluginstore.Manifest
	if node.Decode(&manifest) != nil || manifest.Validate() != nil {
		return nil
	}
	// The host writes this same SDK type as YAML. Checking its encoded shape
	// excludes unknown fields, aliases and scalar coercions during decoding.
	var canonical yaml.Node
	if canonical.Encode(manifest) != nil || !matchesYAMLShape(node, &canonical) {
		return nil
	}
	return &manifest
}

func matchesYAMLShape(node, canonical *yaml.Node) bool {
	if node.Kind != canonical.Kind || node.Tag != canonical.Tag || len(node.Content) != len(canonical.Content) {
		return false
	}
	if node.Kind == yaml.MappingNode {
		fields := make(map[string]*yaml.Node, len(canonical.Content)/2)
		for i := 0; i < len(canonical.Content); i += 2 {
			fields[canonical.Content[i].Value] = canonical.Content[i+1]
		}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			value := fields[key.Value]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || value == nil || !matchesYAMLShape(node.Content[i+1], value) {
				return false
			}
			delete(fields, key.Value)
		}
		return true
	}
	for i, child := range node.Content {
		if !matchesYAMLShape(child, canonical.Content[i]) {
			return false
		}
	}
	return true
}

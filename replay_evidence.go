package main

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const recoveryMetadataKey = "cpa_recovery_v1"

// Stamped by the package builder from its explicit source root. Go's automatic
// VCS discovery can select an enclosing checkout for nested worktrees, so it
// must not supply a plausible but unrelated revision. Unstamped builds are unknown.
var replayBuildRevision string
var replayBuildModified = "true"

// recoveryCorrelation is diagnostic data only. It never participates in auth,
// model routing, replay policy or prompt-cache keys. Copy only this allowlist
// from request metadata, never credential storage/metadata or HTTP headers.
type recoveryCorrelation struct {
	AttemptID       string `json:"attempt_id"`
	ProbeID         string `json:"probe_id"`
	RequestID       string `json:"request_id"`
	ParentRequestID string `json:"parent_request_id"`
	Turn            int    `json:"turn"`
}

func recoveryCorrelationFrom(req pluginapi.ExecutorRequest) (*recoveryCorrelation, string) {
	fields, ok := req.Metadata[recoveryMetadataKey].(map[string]any)
	if !ok || len(fields) != 5 {
		return nil, ""
	}
	var c recoveryCorrelation
	var valid bool
	readID := func(key string, empty bool) (string, bool) {
		s, ok := fields[key].(string)
		return s, ok && ((empty && s == "") || boundedEvidenceToken(s, 128, false))
	}
	if c.AttemptID, valid = readID("attempt_id", false); !valid {
		return nil, ""
	}
	if c.ProbeID, valid = readID("probe_id", false); !valid {
		return nil, ""
	}
	if c.RequestID, valid = readID("request_id", false); !valid {
		return nil, ""
	}
	if c.ParentRequestID, valid = readID("parent_request_id", true); !valid {
		return nil, ""
	}
	// RPC JSON decoding produces float64; native callers may supply int or
	// json.Number. Strings, booleans, fractions and other turns are invalid.
	switch turn := fields["turn"].(type) {
	case int:
		c.Turn = turn
	case float64:
		if turn != 1 && turn != 2 {
			return nil, ""
		}
		c.Turn = int(turn)
	case json.Number:
		if turn != "1" && turn != "2" {
			return nil, ""
		}
		c.Turn, _ = strconv.Atoi(string(turn))
	default:
		return nil, ""
	}
	if (c.Turn != 1 && c.Turn != 2) ||
		(c.Turn == 1 && c.ParentRequestID != "") ||
		(c.Turn == 2 && (c.ParentRequestID == "" || c.ParentRequestID == c.RequestID)) {
		return nil, ""
	}
	// requested_model is the host's pre-alias name, when supplied. Never
	// replace a malformed explicit value with a plausible-looking fallback.
	var requested string
	if value, exists := req.Metadata[requestedModelMetadataKey]; exists {
		requested, ok = value.(string)
		if !ok {
			return nil, ""
		}
	} else {
		model := gjson.GetBytes(req.OriginalRequest, "model")
		if !model.Exists() {
			model = gjson.GetBytes(req.Payload, "model")
		}
		if model.Exists() {
			if model.Type != gjson.String {
				return nil, ""
			}
			requested = model.String()
		} else {
			requested = req.Model
		}
	}
	if !boundedEvidenceToken(requested, 256, true) || !boundedEvidenceToken(stripMetaMuseModelID(req.Model), 256, true) {
		return nil, ""
	}
	return &c, requested
}

// The ASCII allowlist excludes whitespace, controls, quotes, escapes and
// arbitrary Unicode. Values are rejected, never trimmed, truncated or relabeled.
func boundedEvidenceToken(s string, max int, model bool) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for i := range len(s) {
		c := s[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if alnum {
			continue
		}
		if i == 0 || !(c == '.' || c == '_' || c == ':' || c == '-' || (model && (c == '/' || c == '(' || c == ')' || c == '+'))) {
			return false
		}
	}
	return true
}

type replayEventIdentity struct {
	Schema             string `json:"schema"`
	ProducerInstanceID string `json:"producer_instance_id"`
	EventID            string `json:"event_id"`
	Timestamp          string `json:"timestamp"`
}

type replayStartupEvent struct {
	replayEventIdentity
	ProducerVersion string   `json:"producer_version"`
	SourceRevision  string   `json:"source_revision"`
	SourceModified  bool     `json:"source_modified"`
	Sources         []string `json:"sources"`
	FileLogging     string   `json:"file_logging"`
}

type replayDecisionEvent struct {
	replayEventIdentity
	recoveryCorrelation
	RequestedModel  string `json:"requested_model"`
	ResolvedModel   string `json:"resolved_model"`
	ReplayAttempt   int    `json:"replay_attempt"`
	ReplayEnabled   bool   `json:"replay_enabled"`
	Kept            int    `json:"kept"`
	DroppedForeign  int    `json:"dropped_foreign"`
	DroppedUntagged int    `json:"dropped_untagged"`
}

const replayQueueCapacity = 128

type replayFailureEvent struct {
	replayEventIdentity
	Reason string `json:"reason"`
}

// One producer belongs to one plugin runtime. Its single NDJSON stdout sink is
// independent of logrus formatting/levels and never opens files. A collector
// must bind this startup identity to an actually inspected container/log handle;
// neither a hostname nor a diagnostic ID proves container provenance.
type replayProducer struct {
	mu       sync.Mutex
	instance string
	sequence uint64
	out      io.Writer
	queue    chan []byte
	done     chan struct{}
	failed   bool
	closed   bool
	failure  []byte
}

func newReplayProducer(out io.Writer) (*replayProducer, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	p := &replayProducer{instance: id.String(), out: out, queue: make(chan []byte, replayQueueCapacity), done: make(chan struct{})}
	startup := replayStartupEvent{ProducerVersion: version, SourceModified: true, Sources: []string{"stdout"}, FileLogging: "not_configured"}
	if _, err := hex.DecodeString(replayBuildRevision); err == nil && len(replayBuildRevision) == 40 {
		startup.SourceRevision = replayBuildRevision
		startup.SourceModified = replayBuildModified != "false"
	}
	p.mu.Lock()
	startup.replayEventIdentity = p.identity("replay-producer-startup-v1")
	p.admit(startup)
	p.mu.Unlock()
	// Startup is queued before this sole writer starts, and before any caller
	// can admit a decision. No startup or inference caller ever writes the sink.
	go p.run()
	return p, nil
}

func (p *replayProducer) identity(schema string) replayEventIdentity {
	p.sequence++
	return replayEventIdentity{
		Schema: schema, ProducerInstanceID: p.instance,
		EventID:   p.instance + ":" + strconv.FormatUint(p.sequence, 10),
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// Admission is serialized, bounded and independent of sink progress. Overflow
// is sticky: never allow a later green decision to replace lost evidence.
// The caller holds mu; the writer never holds it during an output operation.
func (p *replayProducer) admit(event any) {
	data, err := json.Marshal(event)
	if err != nil || len(data) >= 4095 {
		p.sequence--
		p.fail("encoding")
		return
	}
	select {
	case p.queue <- append(data, '\n'):
	default:
		// The rejected decision already allocated the next identity. Reuse it
		// for the terminal failure, keeping the admitted prefix contiguous.
		p.sequence--
		p.fail("queue_overflow")
	}
}

func (p *replayProducer) fail(reason string) {
	p.failed = true
	if p.sequence < math.MaxUint64 {
		p.failure, _ = json.Marshal(replayFailureEvent{p.identity("replay-producer-failure-v1"), reason})
		p.failure = append(p.failure, '\n')
	}
	p.closeQueue()
}

func (p *replayProducer) closeQueue() {
	if !p.closed {
		p.closed = true
		close(p.queue)
	}
}

// stop seals admission without waiting for I/O. A stalled writer is at most one
// worker and one in-flight record, not one goroutine per request or retry.
func (p *replayProducer) stop() {
	p.mu.Lock()
	p.closeQueue()
	p.mu.Unlock()
}

func (p *replayProducer) run() {
	defer close(p.done)
	for data := range p.queue {
		n, err := p.out.Write(data)
		if err != nil || n != len(data) {
			p.mu.Lock()
			p.failed = true
			p.closeQueue()
			p.mu.Unlock()
			return // Never retry an ambiguous/partial write or write a healthy tail.
		}
	}
	p.mu.Lock()
	failure := p.failure
	p.mu.Unlock()
	if len(failure) != 0 {
		// Best effort only, after the admitted prefix, if a stalled sink resumes.
		// Missing/truncated output still requires fail-closed collection.
		_, _ = p.out.Write(failure)
	}
}

func (p *replayProducer) record(prepared *preparedMetaRequest) {
	if p == nil || prepared.correlation == nil {
		return
	}
	s := prepared.replay.stats
	event := replayDecisionEvent{
		recoveryCorrelation: *prepared.correlation,
		RequestedModel:      prepared.requestedModel, ResolvedModel: prepared.baseModel,
		ReplayEnabled:  prepared.replay.enabled,
		DroppedForeign: s.droppedForeign, DroppedUntagged: s.droppedUntagged,
	}
	if prepared.replayRetried {
		event.ReplayAttempt = 1
	}
	// Count actual surviving envelopes, not prefilter approvals that a
	// translator or the final native-shape sanitizer may subsequently remove.
	for _, item := range gjson.GetBytes(prepared.body, "input").Array() {
		if item.Get("type").String() == "reasoning" && item.Get("encrypted_content").Type == gjson.String && item.Get("encrypted_content").String() != "" {
			event.Kept++
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	// Reserve the last sequence for a terminal failure, never wrap identity.
	if p.sequence >= math.MaxUint64-1 {
		p.fail("sequence_exhausted")
		return
	}
	event.replayEventIdentity = p.identity("replay-event-v1")
	p.admit(event)
}

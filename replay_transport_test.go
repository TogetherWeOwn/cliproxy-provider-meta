package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Release and join every stalled writer, even when the regression fails.
type stalledEvidenceWriter struct {
	bytes.Buffer
	blockAt int
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (w *stalledEvidenceWriter) Write(data []byte) (int, error) {
	w.calls++
	if w.calls == w.blockAt {
		close(w.entered)
		<-w.release
	}
	return w.Buffer.Write(data)
}

func awaitEvidence(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestReplayEvidenceStalledStartupDoesNotBlock(t *testing.T) {
	w := &stalledEvidenceWriter{blockAt: 1, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	var p *replayProducer
	var err error
	go func() { p, err = newReplayProducer(w); close(done) }()
	defer func() {
		close(w.release)
		<-done
		if p != nil {
			finishEvidence(t, p)
		}
	}()
	awaitEvidence(t, done, "producer construction independent of stalled startup sink")
	if err != nil || p == nil {
		t.Fatalf("producer: %v", err)
	}
}

func TestReplayEvidenceStalledSinkDoesNotBlockExecution(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(strconv.FormatBool(stream), func(t *testing.T) {
			w := &stalledEvidenceWriter{blockAt: 2, entered: make(chan struct{}), release: make(chan struct{})}
			e := evidenceExecutor(t, w)
			done := make(chan struct{})
			var err error
			go func() {
				req := evidenceRequest([]byte(`{"input":[]}`), stream, &evidenceHTTPClient{})
				if stream {
					resp, executeErr := e.ExecuteStream(context.Background(), req)
					err = executeErr
					if err == nil {
						for range resp.Chunks {
						}
					}
				} else {
					_, err = e.Execute(context.Background(), req)
				}
				close(done)
			}()
			defer func() { close(w.release); <-done }()
			awaitEvidence(t, w.entered, "stalled decision write")
			awaitEvidence(t, done, "inference independent of stalled decision sink")
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReplayEvidenceQueueOverflowIsBoundedAndTerminal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(strconv.FormatBool(stream), func(t *testing.T) {
			w := &stalledEvidenceWriter{blockAt: 1, entered: make(chan struct{}), release: make(chan struct{})}
			e := evidenceExecutor(t, w)
			var release sync.Once
			defer release.Do(func() { close(w.release) })
			awaitEvidence(t, w.entered, "stalled startup writer")
			for i := range replayQueueCapacity + 10 {
				payload := []byte(`{"input":[]}`)
				if i == 0 {
					payload = []byte(fmt.Sprintf(`{"input":[{"type":"reasoning","encrypted_content":%q}]}`, TagMetaReasoning(metaReasoningAccountKey("auth-b"), parityEnvelope(t))))
				}
				runEvidenceRequest(t, e, evidenceRequest(payload, stream, &evidenceHTTPClient{}))
			}
			p := e.evidence
			p.mu.Lock()
			failed, queued, sequence := p.failed, len(p.queue), p.sequence
			p.mu.Unlock()
			if !failed || queued != replayQueueCapacity || sequence != replayQueueCapacity+2 {
				t.Fatalf("overflow must seal bounded admission: failed=%v queued=%d sequence=%d", failed, queued, sequence)
			}
			release.Do(func() { close(w.release) })
			records := evidenceRecords(t, &w.Buffer, p)
			if len(records) != replayQueueCapacity+2 || records[0]["schema"] != "replay-producer-startup-v1" {
				t.Fatalf("startup/queued prefix lost: %d", len(records))
			}
			for i, record := range records {
				if record["event_id"] != p.instance+":"+strconv.Itoa(i+1) {
					t.Fatal("queue reordered or reused identities")
				}
			}
			if records[1]["dropped_foreign"] != float64(1) {
				t.Fatal("overflow concealed an earlier admitted foreign drop")
			}
			last := records[len(records)-1]
			if last["schema"] != "replay-producer-failure-v1" || last["reason"] != "queue_overflow" {
				t.Fatal("overflow must invalidate the producer, not emit a healthy tail")
			}
			if path := os.Getenv("META_REPLAY_FAILURE_EXAMPLES_OUT"); path != "" && !stream {
				if err := os.WriteFile(path, w.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

type failedEvidenceWriter struct {
	calls int
	short bool
}

func (w *failedEvidenceWriter) Write(data []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		return len(data), nil
	}
	if w.short {
		return len(data) / 2, nil
	}
	return 0, errors.New("synthetic sink failure")
}

func TestReplayEvidenceFailedDecisionSinkRemainsDisabled(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, short := range []bool{false, true} {
			t.Run(strconv.FormatBool(stream)+"/short="+strconv.FormatBool(short), func(t *testing.T) {
				w := &failedEvidenceWriter{short: short}
				e := evidenceExecutor(t, w)
				runEvidenceRequest(t, e, evidenceRequest([]byte(`{"input":[]}`), stream, &evidenceHTTPClient{}))
				awaitEvidence(t, e.evidence.done, "failed decision writer")
				for range 10 {
					runEvidenceRequest(t, e, evidenceRequest([]byte(`{"input":[]}`), stream, &evidenceHTTPClient{}))
				}
				if !e.evidence.failed || w.calls != 2 {
					t.Fatal("failed sink restarted or inference failed")
				}
			})
		}
	}
}

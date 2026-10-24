package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/observation/wire"
	"github.com/windshare/windshare/internal/testrun"
)

const (
	observationPollInterval       = 20 * time.Millisecond
	maximumObservationRecordBytes = 4 << 20
)

// Test milestones are interpretations of the same facts available to a client.
// No test identity, transport, or recorder is injected into the CLI.
type observationMilestones struct {
	path          string
	command       string
	done          <-chan struct{}
	file          *os.File
	reader        *bufio.Reader
	pending       []byte
	summary       bool
	receiveJoined bool
}

type observationRecord struct {
	SchemaVersion int             `json:"schema_version"`
	Event         string          `json:"event"`
	Command       string          `json:"command"`
	Payload       json.RawMessage `json:"payload"`
}

type observationMilestone struct {
	name    string
	outcome testrun.Outcome
	payload json.RawMessage
}

func (stream *observationMilestones) close() {
	if stream.file != nil {
		_ = stream.file.Close()
	}
}

func (process *v2Process) nextObservationMilestone(ctx context.Context) (testrun.Event, error) {
	if process.milestones == nil {
		return testrun.Event{}, errors.New("process has no observation trace")
	}
	fact, err := process.milestones.next(ctx)
	if err != nil {
		return testrun.Event{}, err
	}
	return testrun.Event{
		SchemaVersion: testrun.EventSchemaVersion,
		Identity:      process.scenario.operation.EventIdentity(),
		Component:     process.component,
		Milestone:     fact.name, Outcome: string(fact.outcome), Payload: fact.payload,
	}, nil
}

func (stream *observationMilestones) next(ctx context.Context) (observationMilestone, error) {
	ticker := time.NewTicker(observationPollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return observationMilestone{}, err
		}
		finished := false
		select {
		case <-stream.done:
			finished = true
		default:
		}
		if stream.file == nil {
			file, err := os.Open(stream.path)
			switch {
			case err == nil:
				stream.file, stream.reader = file, bufio.NewReader(file)
			case !errors.Is(err, os.ErrNotExist) || finished:
				return observationMilestone{}, fmt.Errorf("open observation trace: %w", err)
			}
		}
		if stream.reader != nil {
			part, err := stream.reader.ReadSlice('\n')
			if len(stream.pending)+len(part) > maximumObservationRecordBytes {
				return observationMilestone{}, errors.New("observation record exceeds test reader bound")
			}
			stream.pending = append(stream.pending, part...)
			switch {
			case err == nil:
				var record observationRecord
				if err := json.Unmarshal(stream.pending, &record); err != nil {
					return observationMilestone{}, fmt.Errorf("decode observation: %w", err)
				}
				stream.pending = stream.pending[:0]
				if record.SchemaVersion != wire.SchemaVersion || record.Command != stream.command {
					return observationMilestone{}, fmt.Errorf("unexpected observation schema/command: %d/%s", record.SchemaVersion, record.Command)
				}
				if stream.summary {
					return observationMilestone{}, errors.New("observation follows terminal summary")
				}
				stream.summary = record.Event == "trace_summary"
				milestone, relevant, err := stream.derive(record)
				if err != nil || relevant {
					return milestone, err
				}
				continue
			case errors.Is(err, bufio.ErrBufferFull):
				continue
			case !errors.Is(err, io.EOF):
				return observationMilestone{}, err
			case finished:
				if len(stream.pending) != 0 || !stream.summary {
					return observationMilestone{}, errors.New("observation trace ended without a complete terminal summary")
				}
				return observationMilestone{}, io.EOF
			}
		}
		select {
		case <-ctx.Done():
			return observationMilestone{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (stream *observationMilestones) derive(record observationRecord) (observationMilestone, bool, error) {
	fact := observationMilestone{outcome: testrun.OutcomeSucceeded}
	decode := func(value any) error { return json.Unmarshal(record.Payload, value) }
	switch record.Event {
	case "ready":
		if record.Command == "share" {
			fact.name = "sender_ready"
		}
	case "peer_attempt":
		var payload wire.PeerAttemptPayload
		if err := decode(&payload); err != nil {
			return fact, false, err
		}
		if record.Command == "share" && payload.Stage == "admitted" && payload.Route == "direct" {
			fact.name = "sender_direct_lane"
		}
	case "lane_adopted":
		var payload wire.LaneAdoptedPayload
		if err := decode(&payload); err != nil {
			return fact, false, err
		}
		if record.Command == "get" && payload.Transport == "webrtc" {
			fact.name = "receiver_direct_lane"
		}
	case "engine_task_observed":
		var payload wire.EngineTaskPayload
		if err := decode(&payload); err != nil {
			return fact, false, err
		}
		if record.Command == "share" {
			switch payload.Stage {
			case "share_stopping":
				fact.name, fact.outcome = "sender_stop", testrun.OutcomeStarted
			case "share_closed":
				fact.name = "sender_stop"
				if payload.Failure != nil {
					fact.outcome = testrun.OutcomeFailed
				}
			case "session_retired":
				fact.name = "sender_session_retired"
			}
		}
		if record.Command == "get" {
			switch payload.Stage {
			case "generation_changed":
				stream.receiveJoined = true
			case "relay_content_claimed":
				fact.name = "receiver_relay_content"
			case "task_settled":
				// Only settlement before the first admitted generation means
				// initial join failed; a later stopped session is a different fact.
				if !stream.receiveJoined && payload.Outcome == appobs.EngineOutcomeFailed && payload.Failure != nil && payload.Failure.Code == "relay_stopped" {
					fact.name, fact.outcome = "receiver_join_stopped", testrun.OutcomeFailed
				}
			}
		}
	case "relay_recovering":
		var payload wire.RelayRecoveringPayload
		if err := decode(&payload); err != nil {
			return fact, false, err
		}
		if record.Command != "share" || payload.Details == nil {
			break
		}
		generation, err := strconv.ParseUint(payload.Details.Generation, 10, 64)
		if err != nil {
			return fact, false, err
		}
		switch {
		case payload.State == "started" && payload.Attempt == 1 && generation > 0:
			fact.outcome = testrun.OutcomeStarted
		case payload.State == "succeeded" && generation > 1:
		case payload.State == "failed" && payload.Details.Terminal && generation > 0:
			fact.outcome = testrun.OutcomeFailed
		default:
			return fact, false, nil
		}
		fact.name = "sender_relay_recovery"
		fact.payload, err = json.Marshal(map[string]uint64{"connection_generation": generation})
		if err != nil {
			return fact, false, err
		}
	case "trace_summary":
		var payload struct {
			ContractRejected string `json:"contract_rejected"`
			LifecycleDropped string `json:"lifecycle_dropped"`
			WriterFailed     bool   `json:"writer_failed"`
			FlushFailed      bool   `json:"flush_failed"`
			SchemaLimited    bool   `json:"schema_limited"`
		}
		if err := decode(&payload); err != nil {
			return fact, false, err
		}
		if payload.ContractRejected != "0" || payload.LifecycleDropped != "0" || payload.WriterFailed || payload.FlushFailed || payload.SchemaLimited {
			return fact, false, fmt.Errorf("observation trace lost lifecycle evidence: %+v", payload)
		}
	}
	return fact, fact.name != "", nil
}

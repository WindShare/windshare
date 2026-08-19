package runtrace

import (
	"encoding/base64"
	"errors"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

var ErrInvalidCorrelation = errors.New("diagnostic correlation is invalid")

// LaneCorrelation uses explicit pointer presence because zero is a valid
// uint32 lane value and therefore cannot double as an absence sentinel.
type LaneCorrelation struct {
	ID    uint32
	Epoch uint32
}

// CorrelationInput keeps protocol identities strongly typed until the record
// boundary, preventing unrelated 16-byte identities from being interchanged.
type CorrelationInput struct {
	ProtocolSessionID   clievent.ProtocolSessionID
	ProtocolOperationID clievent.ProtocolOperationID
	PeerPathID          clievent.PeerPathID
	PeerAttemptID       clievent.PeerAttemptID
	Lane                *LaneCorrelation
}

// CorrelationV1 is the cross-runtime JSON projection shared with browser
// diagnostics. Runtime-local and output identities intentionally do not fit.
type CorrelationV1 struct {
	ProtocolSessionID   string  `json:"protocol_session_id,omitempty"`
	ProtocolOperationID string  `json:"protocol_operation_id,omitempty"`
	PeerPathID          string  `json:"peer_path_id,omitempty"`
	PeerAttemptID       string  `json:"peer_attempt_id,omitempty"`
	LaneID              *uint32 `json:"lane_id,omitempty"`
	LaneEpoch           *uint32 `json:"lane_epoch,omitempty"`
}

// ProjectCorrelationV1 converts typed internal identities only at the JSON
// boundary. A nil result represents pre-session or local-only work.
func ProjectCorrelationV1(input CorrelationInput) (*CorrelationV1, error) {
	hasSession := input.ProtocolSessionID.Valid()
	hasOperation := input.ProtocolOperationID.Valid()
	hasPath := input.PeerPathID.Valid()
	hasAttempt := input.PeerAttemptID.Valid()
	hasLane := input.Lane != nil

	if hasOperation && !hasSession || hasAttempt && !hasPath {
		return nil, ErrInvalidCorrelation
	}
	if !hasSession && !hasOperation && !hasPath && !hasAttempt && !hasLane {
		return nil, nil
	}

	projected := CorrelationV1{}
	if hasSession {
		projected.ProtocolSessionID = encodeCorrelationIdentity(input.ProtocolSessionID.Bytes())
	}
	if hasOperation {
		projected.ProtocolOperationID = encodeCorrelationIdentity(input.ProtocolOperationID.Bytes())
	}
	if hasPath {
		projected.PeerPathID = encodeCorrelationIdentity(input.PeerPathID.Bytes())
	}
	if hasAttempt {
		projected.PeerAttemptID = encodeCorrelationIdentity(input.PeerAttemptID.Bytes())
	}
	if hasLane {
		laneID := input.Lane.ID
		laneEpoch := input.Lane.Epoch
		projected.LaneID = &laneID
		projected.LaneEpoch = &laneEpoch
	}
	return &projected, nil
}

func encodeCorrelationIdentity(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

package diagnostics

import (
	"encoding/base64"
	"errors"
)

const IdentityBytes = 16

var ErrInvalidCorrelation = errors.New("diagnostic correlation is invalid")

// LaneCorrelation uses explicit pointer presence because zero is a valid
// uint32 lane value and therefore cannot double as an absence sentinel.
type LaneCorrelation struct {
	ID    uint32
	Epoch uint32
}

// CorrelationInput carries raw 16-byte protocol/path identities prior to JSON
// projection. Empty or all-zero slices represent absent identities.
type CorrelationInput struct {
	ProtocolSessionID   []byte
	ProtocolOperationID []byte
	PeerPathID          []byte
	PeerAttemptID       []byte
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

// ProjectCorrelationV1 converts 16-byte identities into canonical unpadded
// base64url at the JSON boundary. A nil result represents local-only work.
func ProjectCorrelationV1(input CorrelationInput) (*CorrelationV1, error) {
	session, hasSession, err := encodeOptionalIdentity(input.ProtocolSessionID)
	if err != nil {
		return nil, err
	}
	operation, hasOperation, err := encodeOptionalIdentity(input.ProtocolOperationID)
	if err != nil {
		return nil, err
	}
	path, hasPath, err := encodeOptionalIdentity(input.PeerPathID)
	if err != nil {
		return nil, err
	}
	attempt, hasAttempt, err := encodeOptionalIdentity(input.PeerAttemptID)
	if err != nil {
		return nil, err
	}
	hasLane := input.Lane != nil
	if hasOperation && !hasSession || hasAttempt && !hasPath {
		return nil, ErrInvalidCorrelation
	}
	if !hasSession && !hasOperation && !hasPath && !hasAttempt && !hasLane {
		return nil, nil
	}
	projected := CorrelationV1{
		ProtocolSessionID:   session,
		ProtocolOperationID: operation,
		PeerPathID:          path,
		PeerAttemptID:       attempt,
	}
	if hasLane {
		laneID := input.Lane.ID
		laneEpoch := input.Lane.Epoch
		projected.LaneID = &laneID
		projected.LaneEpoch = &laneEpoch
	}
	return &projected, nil
}

func EncodeIdentity(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

func encodeOptionalIdentity(raw []byte) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if len(raw) != IdentityBytes {
		return "", false, ErrInvalidCorrelation
	}
	allZero := true
	for _, b := range raw {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return "", false, nil
	}
	return EncodeIdentity(raw), true, nil
}

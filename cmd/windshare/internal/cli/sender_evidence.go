package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

const senderEvidenceSchemaVersion = 1

type senderEvidenceProjector struct {
	writer  io.Writer
	onError func(error)
	mu      sync.Mutex
}

func newSenderEvidenceProjector(writer io.Writer, onError func(error)) v2peer.SenderAttemptObserver {
	if writer == nil {
		return nil
	}
	return &senderEvidenceProjector{writer: writer, onError: onError}
}

func (projector *senderEvidenceProjector) ObserveSenderAttempt(
	observation v2peer.SenderAttemptObservation,
) {
	encoded, err := marshalSenderAttemptEvidence(observation)
	if err != nil {
		projector.report(err)
		return
	}
	encoded = append(encoded, '\n')
	projector.mu.Lock()
	written, writeErr := projector.writer.Write(encoded)
	projector.mu.Unlock()
	if writeErr != nil {
		projector.report(fmt.Errorf("write sender attempt JSONL: %w", writeErr))
	} else if written != len(encoded) {
		projector.report(io.ErrShortWrite)
	}
}

func (projector *senderEvidenceProjector) report(err error) {
	if projector == nil || projector.onError == nil || err == nil {
		return
	}
	projector.onError(err)
}

type senderEvidenceEnvelope struct {
	SchemaVersion    int                       `json:"schemaVersion"`
	SessionID        string                    `json:"sessionId"`
	PeerPathID       string                    `json:"peerPathId"`
	AttemptID        string                    `json:"attemptId"`
	Side             string                    `json:"side"`
	SideSequence     uint64                    `json:"sideSequence"`
	AttemptElapsedMS uint64                    `json:"attemptElapsedMs"`
	Stage            v2peer.SenderAttemptStage `json:"stage"`
}

type senderCandidateCountsJSON struct {
	LocalEmitted   uint32 `json:"localEmitted"`
	RemoteAccepted uint32 `json:"remoteAccepted"`
}

type senderLaneJSON struct {
	LaneID    uint32 `json:"laneId"`
	LaneEpoch uint32 `json:"laneEpoch"`
}

type senderCandidateJSON struct {
	CandidateType string `json:"candidateType"`
	Protocol      string `json:"protocol"`
	Address       string `json:"address"`
	Port          uint16 `json:"port"`
	AddressFamily string `json:"addressFamily"`
}

type senderSelectedPairJSON struct {
	Local  senderCandidateJSON `json:"local"`
	Remote senderCandidateJSON `json:"remote"`
}

type senderCandidateStageJSON struct {
	senderEvidenceEnvelope
	CandidateCounts senderCandidateCountsJSON `json:"candidateCounts"`
}

type senderLaneStageJSON struct {
	senderCandidateStageJSON
	Lane senderLaneJSON `json:"lane"`
}

type senderAdmittedJSON struct {
	senderLaneStageJSON
	SelectedPair *senderSelectedPairJSON `json:"selectedPair"`
}

type senderFailedJSON struct {
	senderEvidenceEnvelope
	FailedAtStage   v2peer.SenderAttemptStage  `json:"failedAtStage"`
	FailureScope    v2peer.AttemptFailureScope `json:"failureScope"`
	TypedErrorCode  v2peer.TypedPeerErrorCode  `json:"typedErrorCode"`
	FailureMessage  string                     `json:"failureMessage"`
	CandidateCounts *senderCandidateCountsJSON `json:"candidateCounts,omitempty"`
	Lane            *senderLaneJSON            `json:"lane,omitempty"`
	SelectedPair    *senderSelectedPairJSON    `json:"selectedPair,omitempty"`
}

func marshalSenderAttemptEvidence(observation v2peer.SenderAttemptObservation) ([]byte, error) {
	envelope := senderEvidenceEnvelope{
		SchemaVersion: senderEvidenceSchemaVersion,
		SessionID:     base64.RawURLEncoding.EncodeToString(observation.SessionID.Bytes()),
		PeerPathID:    base64.RawURLEncoding.EncodeToString(observation.PeerPathID[:]),
		AttemptID:     base64.RawURLEncoding.EncodeToString(observation.AttemptID[:]),
		Side:          "sender", SideSequence: observation.SideSequence,
		AttemptElapsedMS: observation.AttemptElapsedMillis, Stage: observation.Stage,
	}
	switch observation.Stage {
	case v2peer.SenderAttemptStarted, v2peer.SenderAttemptOfferReceived:
		return json.Marshal(envelope)
	case v2peer.SenderAttemptAnswerCreated, v2peer.SenderAttemptAnswerSent,
		v2peer.SenderAttemptDataChannelOpen:
		counts, err := senderCountsJSON(observation.CandidateCounts)
		if err != nil {
			return nil, err
		}
		return json.Marshal(senderCandidateStageJSON{
			senderEvidenceEnvelope: envelope, CandidateCounts: counts,
		})
	case v2peer.SenderAttemptLaneAdmissionStarted:
		return marshalSenderLaneStage(envelope, observation)
	case v2peer.SenderAttemptAdmitted:
		counts, lane, err := senderLanePayload(observation)
		if err != nil {
			return nil, err
		}
		return json.Marshal(senderAdmittedJSON{
			senderLaneStageJSON: senderLaneStageJSON{
				senderCandidateStageJSON: senderCandidateStageJSON{
					senderEvidenceEnvelope: envelope, CandidateCounts: counts,
				},
				Lane: lane,
			},
			SelectedPair: senderPairJSON(observation.SelectedPair),
		})
	case v2peer.SenderAttemptFailed:
		return marshalSenderFailure(envelope, observation)
	default:
		return nil, fmt.Errorf("project sender attempt stage %q: unsupported stage", observation.Stage)
	}
}

func marshalSenderLaneStage(
	envelope senderEvidenceEnvelope,
	observation v2peer.SenderAttemptObservation,
) ([]byte, error) {
	counts, lane, err := senderLanePayload(observation)
	if err != nil {
		return nil, err
	}
	return json.Marshal(senderLaneStageJSON{
		senderCandidateStageJSON: senderCandidateStageJSON{
			senderEvidenceEnvelope: envelope, CandidateCounts: counts,
		},
		Lane: lane,
	})
}

func marshalSenderFailure(
	envelope senderEvidenceEnvelope,
	observation v2peer.SenderAttemptObservation,
) ([]byte, error) {
	if observation.Failure == nil {
		return nil, errors.New("failed sender attempt lacks failure data")
	}
	record := senderFailedJSON{
		senderEvidenceEnvelope: envelope,
		FailedAtStage:          observation.Failure.FailedAtStage,
		FailureScope:           observation.Failure.Scope,
		TypedErrorCode:         observation.Failure.TypedCode,
		FailureMessage:         observation.Failure.Message,
	}
	if observation.CandidateCounts != nil {
		counts, err := senderCountsJSON(observation.CandidateCounts)
		if err != nil {
			return nil, err
		}
		record.CandidateCounts = &counts
	}
	if observation.Lane != nil {
		lane, err := senderLaneIdentityJSON(observation.Lane)
		if err != nil {
			return nil, err
		}
		record.Lane = &lane
	}
	record.SelectedPair = senderPairJSON(observation.SelectedPair)
	return json.Marshal(record)
}

func senderLanePayload(
	observation v2peer.SenderAttemptObservation,
) (senderCandidateCountsJSON, senderLaneJSON, error) {
	counts, err := senderCountsJSON(observation.CandidateCounts)
	if err != nil {
		return senderCandidateCountsJSON{}, senderLaneJSON{}, err
	}
	lane, err := senderLaneIdentityJSON(observation.Lane)
	return counts, lane, err
}

func senderCountsJSON(counts *v2peer.SenderCandidateCounts) (senderCandidateCountsJSON, error) {
	if counts == nil {
		return senderCandidateCountsJSON{}, errors.New("sender attempt stage lacks candidate counts")
	}
	return senderCandidateCountsJSON{
		LocalEmitted: counts.LocalEmitted, RemoteAccepted: counts.RemoteAccepted,
	}, nil
}

func senderLaneIdentityJSON(lane *sessionruntime.LaneIdentity) (senderLaneJSON, error) {
	if lane == nil || lane.ID == 0 || lane.Epoch == 0 {
		return senderLaneJSON{}, errors.New("sender attempt stage lacks a nonzero lane identity")
	}
	return senderLaneJSON{LaneID: lane.ID, LaneEpoch: lane.Epoch}, nil
}

func senderPairJSON(pair *v2peer.PionSelectedPairEvidence) *senderSelectedPairJSON {
	if pair == nil {
		return nil
	}
	return &senderSelectedPairJSON{
		Local:  senderCandidateEvidenceJSON(pair.Local),
		Remote: senderCandidateEvidenceJSON(pair.Remote),
	}
}

func senderCandidateEvidenceJSON(candidate v2peer.PionCandidateEvidence) senderCandidateJSON {
	return senderCandidateJSON{
		CandidateType: candidate.CandidateType, Protocol: candidate.Protocol,
		Address: candidate.Address, Port: candidate.Port, AddressFamily: candidate.AddressFamily,
	}
}

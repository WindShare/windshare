package resumeauthority

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/transfer"
)

const attentionReferenceBytes = sha256.Size

var ErrInvalidContract = errors.New("resume state authority contract is invalid")

// AttentionReason is deliberately smaller than repository diagnostics. These
// are the only uncertainty classes that may cross the discard settlement
// boundary without accidentally turning a storage detail into deletion policy.
type AttentionReason string

const (
	AttentionMissingOwnership     AttentionReason = "missing-ownership"
	AttentionReplacement          AttentionReason = "replacement"
	AttentionUnknownChildren      AttentionReason = "unknown-children"
	AttentionCorruptBinding       AttentionReason = "corrupt-binding"
	AttentionAmbiguousPublication AttentionReason = "ambiguous-publication"
)

func (reason AttentionReason) Valid() bool {
	switch reason {
	case AttentionMissingOwnership, AttentionReplacement, AttentionUnknownChildren,
		AttentionCorruptBinding, AttentionAmbiguousPublication:
		return true
	default:
		return false
	}
}

// Attention carries only a stable correlation token. Native identities,
// checkpoint names, and user paths remain inside the adapter that pinned them.
type Attention struct {
	reason    AttentionReason
	reference string
}

func NewAttention(reason AttentionReason, reference string) (Attention, error) {
	if !reason.Valid() || !validAttentionReference(reference) {
		return Attention{}, ErrInvalidContract
	}
	return Attention{reason: reason, reference: reference}, nil
}

func (attention Attention) Reason() AttentionReason { return attention.reason }
func (attention Attention) Reference() string       { return attention.reference }

func (attention Attention) Valid() bool {
	return attention.reason.Valid() && validAttentionReference(attention.reference)
}

func validAttentionReference(reference string) bool {
	if len(reference) != hex.EncodedLen(attentionReferenceBytes) {
		return false
	}
	decoded, err := hex.DecodeString(reference)
	return err == nil && hex.EncodeToString(decoded) == reference
}

type ListStatus uint8

const (
	ListAvailable ListStatus = iota + 1
	ListNeedsAttention
)

func (status ListStatus) Valid() bool {
	return status == ListAvailable || status == ListNeedsAttention
}

// ListedState is the immutable semantic projection returned by a pinned
// repository inventory. Its ordinal is intentionally absent: only Inventory
// may bind an ordinal to a live, single-use Reference.
type ListedState struct {
	status                ListStatus
	intent                transfer.TransferIntentDigest
	backend               transfer.OutputBackendID
	checkpointRecordCount uint64
	recoveryArtifactBytes uint64
	attention             []Attention
}

type ListedStateSpec struct {
	Status                ListStatus
	Intent                transfer.TransferIntentDigest
	Backend               transfer.OutputBackendID
	CheckpointRecordCount uint64
	RecoveryArtifactBytes uint64
	Attention             []Attention
}

func NewListedState(spec ListedStateSpec) (ListedState, error) {
	state := ListedState{
		status:                spec.Status,
		intent:                spec.Intent,
		backend:               spec.Backend,
		checkpointRecordCount: spec.CheckpointRecordCount,
		recoveryArtifactBytes: spec.RecoveryArtifactBytes,
		attention:             slices.Clone(spec.Attention),
	}
	if !state.valid() {
		return ListedState{}, ErrInvalidContract
	}
	return state, nil
}

func (state ListedState) valid() bool {
	if !state.status.Valid() {
		return false
	}
	for _, attention := range state.attention {
		if !attention.Valid() {
			return false
		}
	}
	if state.status == ListAvailable {
		if state.intent.IsZero() || len(state.attention) != 0 {
			return false
		}
		_, err := transfer.NewOutputBackendID(string(state.backend))
		return err == nil
	}
	// Opaque unsafe state may not have a trustworthy intent or backend. When
	// either is present it must still be canonical, and attention is mandatory.
	if len(state.attention) == 0 {
		return false
	}
	if state.backend != "" {
		if _, err := transfer.NewOutputBackendID(string(state.backend)); err != nil {
			return false
		}
	}
	return true
}

type Summary struct {
	state     ListedState
	reference Reference
}

func (summary Summary) Status() ListStatus                    { return summary.state.status }
func (summary Summary) Intent() transfer.TransferIntentDigest { return summary.state.intent }
func (summary Summary) Backend() transfer.OutputBackendID     { return summary.state.backend }
func (summary Summary) CheckpointRecordCount() uint64 {
	return summary.state.checkpointRecordCount
}
func (summary Summary) RecoveryArtifactBytes() uint64 {
	return summary.state.recoveryArtifactBytes
}
func (summary Summary) Attention() []Attention { return slices.Clone(summary.state.attention) }
func (summary Summary) Reference() Reference   { return summary.reference }
func (summary Summary) NeedsAttention() bool   { return summary.state.status == ListNeedsAttention }

type DiscardStatus uint8

const (
	Discarded DiscardStatus = iota + 1
	AlreadyAbsent
	DiscardNeedsAttention
)

func (status DiscardStatus) Valid() bool {
	return status == Discarded || status == AlreadyAbsent || status == DiscardNeedsAttention
}

// DiscardResult is a settlement, not an error. Needs-attention means every
// object lacking exact deletion proof was retained.
type DiscardResult struct {
	status           DiscardStatus
	removedArtifacts uint64
	attention        []Attention
}

func NewDiscardResult(
	status DiscardStatus,
	removedArtifacts uint64,
	attention []Attention,
) (DiscardResult, error) {
	result := DiscardResult{
		status: status, removedArtifacts: removedArtifacts, attention: slices.Clone(attention),
	}
	if !result.valid() {
		return DiscardResult{}, ErrInvalidContract
	}
	return result, nil
}

func (result DiscardResult) Status() DiscardStatus    { return result.status }
func (result DiscardResult) RemovedArtifacts() uint64 { return result.removedArtifacts }
func (result DiscardResult) Attention() []Attention   { return slices.Clone(result.attention) }
func (result DiscardResult) NeedsAttention() bool     { return result.status == DiscardNeedsAttention }

func (result DiscardResult) valid() bool {
	if !result.status.Valid() {
		return false
	}
	for _, attention := range result.attention {
		if !attention.Valid() {
			return false
		}
	}
	switch result.status {
	case Discarded:
		return result.removedArtifacts > 0 && len(result.attention) == 0
	case AlreadyAbsent:
		return result.removedArtifacts == 0 && len(result.attention) == 0
	case DiscardNeedsAttention:
		return len(result.attention) > 0
	default:
		return false
	}
}

func validateListedStates(states []ListedState) error {
	seen := make(map[transfer.TransferIntentDigest]struct{}, len(states))
	for _, state := range states {
		if !state.valid() {
			return ErrInvalidContract
		}
		if state.intent.IsZero() {
			continue
		}
		if _, exists := seen[state.intent]; exists {
			return fmt.Errorf("%w: duplicate transfer intent", ErrInvalidContract)
		}
		seen[state.intent] = struct{}{}
	}
	return nil
}

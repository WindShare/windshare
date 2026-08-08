package resumeauthority

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestPublicValueConstructorsKeepListAndDiscardSumsClosed(t *testing.T) {
	attention := derivedAttention(AttentionReplacement, []byte("replacement"))
	if !attention.Valid() || attention.Reason() != AttentionReplacement || attention.Reference() == "" {
		t.Fatalf("attention = %+v", attention)
	}
	if _, err := NewAttention(AttentionReason("foreign"), attention.Reference()); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("open attention reason error = %v", err)
	}
	if _, err := NewAttention(AttentionReplacement, "native/path"); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("unsafe attention reference error = %v", err)
	}

	intent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0x71}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := transfer.NewOutputBackendID("resumeauthority-test")
	if err != nil {
		t.Fatal(err)
	}
	available, err := NewListedState(ListedStateSpec{
		Status: ListAvailable, Intent: intent, Backend: backend, CheckpointRecordCount: 1,
	})
	if err != nil || !available.valid() {
		t.Fatalf("available state = %+v, %v", available, err)
	}
	if _, err := NewListedState(ListedStateSpec{
		Status: ListAvailable, Intent: intent, Backend: backend, Attention: []Attention{attention},
	}); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("available attention error = %v", err)
	}
	if _, err := NewListedState(ListedStateSpec{Status: ListNeedsAttention}); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("attention-free unsafe state error = %v", err)
	}

	for _, test := range []struct {
		status    DiscardStatus
		removed   uint64
		attention []Attention
		valid     bool
	}{
		{Discarded, 1, nil, true},
		{AlreadyAbsent, 0, nil, true},
		{DiscardNeedsAttention, 0, []Attention{attention}, true},
		{Discarded, 0, nil, false},
		{AlreadyAbsent, 1, nil, false},
		{DiscardNeedsAttention, 0, nil, false},
	} {
		result, err := NewDiscardResult(test.status, test.removed, test.attention)
		if test.valid {
			if err != nil || !result.valid() || result.Status() != test.status ||
				result.RemovedArtifacts() != test.removed ||
				result.NeedsAttention() != (test.status == DiscardNeedsAttention) {
				t.Fatalf("valid discard result = %+v, %v", result, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("invalid discard result error = %v", err)
		}
	}
}

func TestRepositoryObservationAndApplyContractsAreClosed(t *testing.T) {
	for _, invalid := range []Evidence{0, 99} {
		if invalid.Valid() {
			t.Fatalf("invalid evidence %d is valid", invalid)
		}
		if _, err := NewPublicationObservation([32]byte{1}, invalid); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("publication error = %v", err)
		}
	}
	attention := derivedAttention(AttentionCorruptBinding, []byte("apply"))
	for _, test := range []struct {
		status    ApplyStatus
		attention []Attention
		valid     bool
	}{
		{ApplyCompleted, nil, true},
		{ApplyAlreadySatisfied, nil, true},
		{ApplyNeedsAttention, []Attention{attention}, true},
		{ApplyCompleted, []Attention{attention}, false},
		{ApplyNeedsAttention, nil, false},
	} {
		result, err := NewApplyResult(test.status, test.attention)
		if test.valid {
			if err != nil || !result.valid() || result.Status() != test.status {
				t.Fatalf("apply result = %+v, %v", result, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("invalid apply result error = %v", err)
		}
	}
}

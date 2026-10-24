package checkpointmodel

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLiveCleanupTicketV1FreezesMinimalCodec(t *testing.T) {
	if !LiveCleanupLinuxExt4V1.Valid() || LiveCleanupLinuxExt4V1.String() != "linux/ext4/v1" ||
		!LiveCleanupWindowsNTFSV1.Valid() || LiveCleanupWindowsNTFSV1.String() != "windows/ntfs/v1" ||
		LiveCleanupNativeProfile(0).Valid() || LiveCleanupNativeProfile(3).Valid() {
		t.Fatal("live cleanup native profile union drifted")
	}
	ticket, err := NewLiveCleanupTicket(LiveCleanupTicketSpec{
		Nonce: bytes.Repeat([]byte{0x4a}, LiveCleanupNonceBytesV1), ExactSize: 42,
		Profile: LiveCleanupWindowsNTFSV1, Generation: 1, State: LiveCleanupTicketCommitted,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeLiveCleanupTicket(ticket)
	if err != nil {
		t.Fatal(err)
	}
	if LiveCleanupNamespaceV1 != "cleanup-v1" ||
		LiveCleanupTicketDomainV1 != "windshare/live-cleanup-ticket/v1" ||
		LiveCleanupStageDomainV1 != "windshare/live-cleanup-stage/v1" ||
		LiveCleanupTicketVersionV1 != 1 || LiveCleanupNonceBytesV1 != 16 ||
		MaximumLiveCleanupTicketBytesV1 != 256 {
		t.Fatal("live cleanup domains, version, or limits drifted")
	}
	if len(encoded) > MaximumLiveCleanupTicketBytesV1 || ticket.StageName() != "stage-c6acc006d1a9a2d0901657836d3fa5b2.part" {
		t.Fatalf("encoded length/stage = %d/%q", len(encoded), ticket.StageName())
	}
	restored, err := DecodeLiveCleanupTicket(encoded)
	if err != nil || restored.ExactSize() != 42 || restored.Profile() != LiveCleanupWindowsNTFSV1 ||
		restored.Generation() != 1 || restored.State() != LiveCleanupTicketCommitted ||
		!bytes.Equal(restored.Nonce(), ticket.Nonce()) {
		t.Fatalf("restored ticket = %+v, %v", restored, err)
	}
	for _, forbidden := range []string{"intent", "range", "revision", "path", "selection", "checkpoint"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("cleanup ticket encoded forbidden semantic %q", forbidden)
		}
	}
	for structField := range reflect.TypeFor[LiveCleanupTicket]().Fields() {
		field := strings.ToLower(structField.Name)
		for _, forbidden := range []string{"intent", "range", "revision", "path", "selection", "checkpoint"} {
			if strings.Contains(field, forbidden) {
				t.Fatalf("cleanup ticket field %q can carry %s", field, forbidden)
			}
		}
	}
}

func TestLiveCleanupTicketReducerEnforcesRecordBeforeStage(t *testing.T) {
	committed, err := NewLiveCleanupTicket(LiveCleanupTicketSpec{
		Nonce: bytes.Repeat([]byte{0x5b}, LiveCleanupNonceBytesV1), ExactSize: 7,
		Profile: LiveCleanupLinuxExt4V1, Generation: 1, State: LiveCleanupTicketCommitted,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := ReduceLiveCleanupTicket(committed, LiveCleanupRecordStageCreated)
	if err != nil || created.State() != LiveCleanupStageCreated || created.Generation() != 2 {
		t.Fatalf("created transition = %+v, %v", created, err)
	}
	removed, err := ReduceLiveCleanupTicket(created, LiveCleanupRecordStageRemoved)
	if err != nil || removed.State() != LiveCleanupStageRemoved || removed.Generation() != 3 {
		t.Fatalf("removed transition = %+v, %v", removed, err)
	}
	if _, err := ReduceLiveCleanupTicket(committed, LiveCleanupRecordStageRemoved); !errors.Is(err, ErrInvalidLiveCleanupTicket) {
		t.Fatalf("stage removed before create = %v", err)
	}
	if _, err := ReduceLiveCleanupTicket(removed, LiveCleanupRecordStageCreated); !errors.Is(err, ErrInvalidLiveCleanupTicket) {
		t.Fatalf("retired ticket reopened = %v", err)
	}
}

func TestLiveCleanupTicketRejectsNonCanonicalInputs(t *testing.T) {
	if _, err := NewLiveCleanupTicket(LiveCleanupTicketSpec{
		Nonce: make([]byte, LiveCleanupNonceBytesV1), ExactSize: 1,
		Profile: LiveCleanupLinuxExt4V1, Generation: 1, State: LiveCleanupTicketCommitted,
	}); !errors.Is(err, ErrInvalidLiveCleanupTicket) {
		t.Fatalf("zero nonce error = %v", err)
	}
	ticket, _ := NewLiveCleanupTicket(LiveCleanupTicketSpec{
		Nonce: bytes.Repeat([]byte{1}, LiveCleanupNonceBytesV1), ExactSize: 1,
		Profile: LiveCleanupLinuxExt4V1, Generation: 1, State: LiveCleanupTicketCommitted,
	})
	encoded, _ := EncodeLiveCleanupTicket(ticket)
	for name, value := range map[string][]byte{
		"empty":     nil,
		"truncated": encoded[:len(encoded)-1],
		"trailing":  append(append([]byte(nil), encoded...), 0),
		"foreign":   append([]byte("foreign"), encoded...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeLiveCleanupTicket(value); !errors.Is(err, ErrInvalidLiveCleanupTicket) {
				t.Fatalf("decode error = %v", err)
			}
		})
	}
}

package protocolsession

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestOperationFailureSchemaSpansAllFrozenServiceScopes(t *testing.T) {
	for _, failure := range []OperationFailure{
		{Scope: OperationScopeDirectory, Code: directoryOperationCodeFirst + 5, Message: "Catalog operation failed"},
		{Scope: OperationScopeRevision, Code: revisionOperationCodeFirst + 6, Message: "Revision drifted"},
		{Scope: OperationScopeBlock, Code: blockOperationCodeFirst + 3, Retryable: true, RetryAfter: 250 * time.Millisecond, Message: "Block timed out"},
		{Scope: OperationScopePeer, Code: PeerOperationCodeAdmission, Message: "Peer channel admission failed"},
	} {
		encoded, err := EncodeOperationFailure(failure)
		if err != nil {
			t.Fatalf("encode scope %d: %v", failure.Scope, err)
		}
		canonical, err := EncodeBody(map[uint64]any{
			0: operationFailureSchemaVersion, 1: uint64(failure.Scope), 2: uint64(failure.Code),
			3: failure.Retryable, 4: operationFailureRetryValue(failure), 5: failure.Message,
		})
		if err != nil || !bytes.Equal(encoded, canonical) {
			t.Fatalf("scope %d changed canonical body: %x / %v", failure.Scope, encoded, err)
		}
		decoded, err := DecodeOperationFailure(encoded)
		if err != nil || decoded != failure {
			t.Fatalf("decode scope %d = %+v, want %+v, error %v", failure.Scope, decoded, failure, err)
		}
	}
}

func TestPeerOperationFailureIsPermanentlyBoundToOneNegotiationIdentity(t *testing.T) {
	failure := OperationFailure{
		Scope: OperationScopePeer, Code: PeerOperationCodeTimeout,
		Retryable: true, RetryAfter: time.Second, Message: "Peer negotiation timed out",
	}
	if _, err := EncodeOperationFailure(failure); err == nil {
		t.Fatal("retryable peer failure was encoded")
	}
	encoded, err := messageEncMode.Marshal(map[uint64]any{
		0: operationFailureSchemaVersion, 1: uint64(OperationScopePeer),
		2: uint64(PeerOperationCodeTimeout), 3: true, 4: uint64(1_000),
		5: "Peer negotiation timed out",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeOperationFailure(encoded); !errors.Is(err, ErrInvalidOperationFailure) {
		t.Fatalf("retryable signed peer failure = %v", err)
	}
}

func TestOperationFailureRejectsCrossScopeAndAmbiguousRetrySemantics(t *testing.T) {
	valid := OperationFailure{Scope: OperationScopeBlock, Code: blockOperationCodeFirst + 3, Message: "failed"}
	for name, mutate := range map[string]func(*OperationFailure){
		"unknown scope":    func(failure *OperationFailure) { failure.Scope = 1 },
		"cross-scope code": func(failure *OperationFailure) { failure.Code = revisionOperationCodeFirst + 3 },
		"empty message":    func(failure *OperationFailure) { failure.Message = "" },
		"non-NFC message":  func(failure *OperationFailure) { failure.Message = "e\u0301" },
		"oversized message": func(failure *OperationFailure) {
			failure.Message = string(bytes.Repeat([]byte{'x'}, MaxOperationFailureMessageBytes+1))
		},
		"fractional retry": func(failure *OperationFailure) { failure.Retryable, failure.RetryAfter = true, time.Nanosecond },
		"zero retry":       func(failure *OperationFailure) { failure.Retryable = true },
		"permanent delay":  func(failure *OperationFailure) { failure.RetryAfter = time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			failure := valid
			mutate(&failure)
			if _, err := EncodeOperationFailure(failure); err == nil {
				t.Fatal("invalid operation failure was encoded")
			}
		})
	}
}

func TestOperationFailureRetryDelayBounds(t *testing.T) {
	base := OperationFailure{
		Scope: OperationScopeBlock, Code: blockOperationCodeFirst,
		Retryable: true, Message: "retry later",
	}
	for _, test := range []struct {
		name       string
		retryAfter time.Duration
		wantError  bool
	}{
		{name: "zero", retryAfter: 0, wantError: true},
		{name: "minimum", retryAfter: MinOperationFailureRetryAfter},
		{name: "maximum", retryAfter: MaxOperationFailureRetryAfter},
		{name: "above maximum", retryAfter: MaxOperationFailureRetryAfter + time.Millisecond, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := base
			failure.RetryAfter = test.retryAfter
			_, err := EncodeOperationFailure(failure)
			if (err != nil) != test.wantError {
				t.Fatalf("EncodeOperationFailure() error = %v, want error %v", err, test.wantError)
			}
		})
	}
}

func TestDecodeOperationFailureRejectsHostileSignedSemantics(t *testing.T) {
	valid := map[uint64]any{
		0: operationFailureSchemaVersion,
		1: uint64(OperationScopeBlock),
		2: uint64(blockOperationCodeFirst),
		3: true,
		4: uint64(MaxOperationFailureRetryAfter / time.Millisecond),
		5: "retry later",
	}
	for name, mutate := range map[string]func(map[uint64]any){
		"schema":        func(fields map[uint64]any) { fields[0] = uint64(2) },
		"scope":         func(fields map[uint64]any) { fields[1] = uint64(1) },
		"cross-scope":   func(fields map[uint64]any) { fields[2] = uint64(revisionOperationCodeFirst) },
		"zero-retry":    func(fields map[uint64]any) { fields[4] = uint64(0) },
		"large-retry":   func(fields map[uint64]any) { fields[4] = uint64(30_001) },
		"empty-message": func(fields map[uint64]any) { fields[5] = "" },
		"non-NFC":       func(fields map[uint64]any) { fields[5] = "e\u0301" },
		"extra-field":   func(fields map[uint64]any) { fields[6] = uint64(0) },
	} {
		t.Run(name, func(t *testing.T) {
			fields := make(map[uint64]any, len(valid)+1)
			for key, value := range valid {
				fields[key] = value
			}
			mutate(fields)
			encoded, err := messageEncMode.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeOperationFailure(encoded); !errors.Is(err, ErrInvalidOperationFailure) {
				t.Fatalf("hostile operation failure error = %v", err)
			}
		})
	}
}

func operationFailureRetryValue(failure OperationFailure) any {
	if !failure.Retryable {
		return nil
	}
	return uint64(failure.RetryAfter / time.Millisecond)
}

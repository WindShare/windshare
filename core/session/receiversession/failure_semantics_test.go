package receiversession

import (
	"errors"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"maps"
	"testing"
)

func TestRemoteOperationErrorRejectsAuthenticatedMalformedSemantics(t *testing.T) {
	operationID := protocolsession.OperationID(201)
	valid := map[uint64]any{
		0: uint64(1),
		1: uint64(protocolsession.OperationScopeBlock),
		2: uint64(contentflow.BlockCodeInvalidRef),
		3: true,
		4: uint64(1),
		5: "retry later",
	}
	for name, mutate := range map[string]func(map[uint64]any){
		"scope":         func(fields map[uint64]any) { fields[1] = uint64(1) },
		"cross-scope":   func(fields map[uint64]any) { fields[2] = uint64(0x3001) },
		"zero-retry":    func(fields map[uint64]any) { fields[4] = uint64(0) },
		"large-retry":   func(fields map[uint64]any) { fields[4] = uint64(30_001) },
		"empty-message": func(fields map[uint64]any) { fields[5] = "" },
		"extra-field":   func(fields map[uint64]any) { fields[6] = uint64(0) },
	} {
		t.Run(name, func(t *testing.T) {
			fields := make(map[uint64]any, len(valid)+1)
			maps.Copy(fields, valid)
			mutate(fields)
			semantic, err := protocolsession.EncodeBody(fields)
			if err != nil {
				t.Fatal(err)
			}
			message, err := protocolsession.NewMessage(
				protocolsession.MessageOperationError, &operationID, semantic,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := remoteOperationError(message); !errors.Is(err, protocolsession.ErrInvalidOperationFailure) {
				t.Fatalf("authenticated malformed operation error = %v", err)
			}
		})
	}
}

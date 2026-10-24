package wire_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/observation/wire"
)

func identityBytes(first byte) []byte {
	raw := make([]byte, appobs.IdentityBytes)
	raw[0] = first
	return raw
}

func testIdentity(t *testing.T, marker byte) []byte {
	t.Helper()
	return bytes.Repeat([]byte{marker}, appobs.IdentityBytes)
}

func TestEncodeOwnsApplicationMetadataAndRejectsUnknownFacts(t *testing.T) {
	record, err := wire.Encode(appobs.NewReady())
	if err != nil || record.SchemaVersion != wire.SchemaVersion || record.Workflow != "share" || record.Event != "ready" || record.Level != "info" {
		t.Fatalf("application metadata: %+v, %v", record, err)
	}
	if _, err := json.Marshal(record); err != nil {
		t.Fatal(err)
	}
	for _, event := range []appobs.Event{nil, appobs.TransferProgress{}} {
		if _, err := wire.Encode(event); err == nil {
			t.Fatalf("accepted invalid event: %T", event)
		}
	}
	if _, err := wire.Encode(unknownFact{}); !errors.Is(err, wire.ErrUnsupportedEvent) {
		t.Fatalf("unsupported fact: %v", err)
	}
}

type unknownFact struct{}

func (unknownFact) EngineEvent()              {}
func (unknownFact) Workflow() appobs.Workflow { return appobs.WorkflowShare }
func (unknownFact) Level() appobs.Level       { return appobs.LevelDebug }
func (unknownFact) Validate() error           { return nil }

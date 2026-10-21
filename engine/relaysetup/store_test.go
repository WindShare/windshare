package relaysetup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turn"
)

func TestCheckpointDoesNotRewriteSettingsAndCorruptionIsIsolated(t *testing.T) {
	settings := relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
	initial := prior()
	if err := settings.Save(initial.Config); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(settings.Path)
	store := FileStore{Settings: settings}
	pending := expiredPending(time.Now())
	if _, err := store.Update(t.Context(), func(snapshot *Snapshot) error { snapshot.Pending = pending; return nil }); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(settings.Path)
	if string(before) != string(after) {
		t.Fatal("checkpoint rewrote settings")
	}
	journal := filepath.Join(settings.Path+".state", "setup", pending.OperationID+".json")
	if _, err := os.Stat(journal); err != nil {
		t.Fatal("operation has no independent record", err)
	}
	imported := initial.Config
	imported.ManagedTURNPolicies = []turn.ManagedPolicy{{RelayURL: initial.Profiles[0].PublicURL, LocalBudgetBytes: 99}}
	if err := settings.Save(imported); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load()
	if err != nil || snapshot.Pending == nil || snapshot.Pending.OperationID != pending.OperationID ||
		snapshot.SettingsRevision() != imported.SettingsRevision() {
		t.Fatal("import lost operation or settings", snapshot, err)
	}
	if snapshot.Pending.SettingsRevision == snapshot.SettingsRevision() {
		t.Fatal("budget edits were excluded from revision")
	}
	if err := os.WriteFile(journal, []byte("broken checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("corrupt checkpoint accepted")
	}
	if config, err := settings.Load(); err != nil || config.SettingsRevision() != imported.SettingsRevision() {
		t.Fatal("workflow corruption blocked sharing settings", err)
	}
}

func TestCheckpointDecoderRejectsIllegalStagePayloads(t *testing.T) {
	pending := expiredPending(time.Now())
	for _, stage := range []setupPhase{
		requesting{}, pending.phase, installing{Enrollment: pending.enrollment(), Credential: credential{ID: "credential", AccessKey: "key"}},
		cancelling{EnrollmentID: pending.enrollment().ID},
	} {
		value := pending.withPhase(stage)
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var restored PendingSetup
		if err := json.Unmarshal(encoded, &restored); err != nil || restored.phase.stage() != stage.stage() {
			t.Fatal("checkpoint did not round-trip", err)
		}
	}
	encoded, _ := json.Marshal(pending)
	var document checkpointDocument
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct{ stage, data string }{
		{"installing", `{"enrollment":{}}`},
		{"requesting", `{"credential":{"id":"credential","access_key":"key"}}`},
		{"cancelling", `{"enrollment_id":"enrollment","credential":{"id":"credential","access_key":"key"}}`},
		{"requesting", "null"},
		{"unknown", "{}"},
	} {
		document.Stage, document.Data = input.stage, json.RawMessage(input.data)
		data, _ := json.Marshal(document)
		var result PendingSetup
		if err := json.Unmarshal(data, &result); err == nil {
			t.Fatalf("accepted invalid %s checkpoint", input.stage)
		}
	}
	for _, field := range []string{"pending_setup", "managed_turn"} {
		if _, err := relayconfig.Decode(strings.NewReader(`{"profiles":[],"` + field + `":{}}`)); err == nil {
			t.Fatal("runtime state accepted as settings", field)
		}
	}
}

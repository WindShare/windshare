package e2e

import "testing"

func TestUserTraceV3FilesystemCheckpointDecisionVocabulary(t *testing.T) {
	tests := []struct {
		name     string
		event    string
		decision string
		want     bool
	}{
		{"absent", "filesystem_output", "absent", true},
		{"exact", "filesystem_output", "exact", true},
		{"revision conflict", "filesystem_output", "revision_conflict", true},
		{"ownership conflict", "filesystem_output", "ownership_conflict", true},
		{"invalid", "filesystem_output", "invalid", true},
		{"empty", "filesystem_output", "", false},
		{"case variant", "filesystem_output", "RevisionConflict", false},
		{"display spelling", "filesystem_output", "revision-conflict", false},
		{"sensitive payload", "filesystem_output", `checkpoint/path/owned-object-id`, false},
		{"wrong event", "transfer_lifecycle", "exact", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := v3ValidFilesystemCheckpointDecision(test.event, test.decision); got != test.want {
				t.Fatalf("valid=%t want=%t", got, test.want)
			}
		})
	}
}

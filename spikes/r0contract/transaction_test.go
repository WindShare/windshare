package r0contract

import "testing"

type commitStep string

const (
	dataWrite     commitStep = "data-write"
	dataFlush     commitStep = "data-flush"
	journalWrite  commitStep = "journal-write"
	journalFlush  commitStep = "journal-flush"
	atomicInstall commitStep = "atomic-install"
	reopenVerify  commitStep = "reopen-verify"
)

var checkpointOrder = []commitStep{
	dataWrite,
	dataFlush,
	journalWrite,
	journalFlush,
	atomicInstall,
	reopenVerify,
}

func TestOutputCheckpointCrashCutsPublishOnlyReopenedState(t *testing.T) {
	for cut := range checkpointOrder {
		applied := checkpointOrder[:cut+1]
		published := checkpointPublished(applied)
		wantPublished := cut == len(checkpointOrder)-1
		if published != wantPublished {
			t.Fatalf("cut after %q: published = %t, want %t", checkpointOrder[cut], published, wantPublished)
		}
	}
}

func checkpointPublished(applied []commitStep) bool {
	if len(applied) != len(checkpointOrder) {
		return false
	}
	for index := range checkpointOrder {
		if applied[index] != checkpointOrder[index] {
			return false
		}
	}
	return true
}

type rootCommit struct {
	pages         bool
	nodes         bool
	terminal      bool
	budgetCharged bool
	spillFlushed  bool
	installed     bool
}

func TestCatalogRootTransactionRejectsEveryPartialCommit(t *testing.T) {
	steps := []func(*rootCommit){
		func(state *rootCommit) { state.pages = true },
		func(state *rootCommit) { state.nodes = true },
		func(state *rootCommit) { state.terminal = true },
		func(state *rootCommit) { state.budgetCharged = true },
		func(state *rootCommit) { state.spillFlushed = true },
		func(state *rootCommit) { state.installed = true },
	}
	for cut := range steps {
		var state rootCommit
		for _, apply := range steps[:cut+1] {
			apply(&state)
		}
		visible := rootVisible(state)
		wantVisible := cut == len(steps)-1
		if visible != wantVisible {
			t.Fatalf("catalog cut %d: visible = %t, want %t", cut, visible, wantVisible)
		}
	}
}

func rootVisible(state rootCommit) bool {
	return state.pages && state.nodes && state.terminal && state.budgetCharged && state.spillFlushed && state.installed
}

func TestExplicitStopNeverUsesCrashGrace(t *testing.T) {
	const crashGraceSeconds = 60
	if stoppedAt(1_000, true) != 1_000 {
		t.Fatal("explicit stop must revoke immediately")
	}
	if stoppedAt(1_000, false) != 1_000+crashGraceSeconds {
		t.Fatal("an unclean disconnect must retain the bounded crash grace")
	}
}

func stoppedAt(disconnectedAt int, explicit bool) int {
	if explicit {
		return disconnectedAt
	}
	return disconnectedAt + 60
}

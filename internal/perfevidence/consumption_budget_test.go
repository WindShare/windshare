package perfevidence

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

func TestConsumptionUniverseRejectsBudgetOverrunsBeforeAuthority(t *testing.T) {
	t.Parallel()
	t.Run("single-directory-n-plus-one", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"a", "b"} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := inventoryConsumptionUniverseWithBudget(
			map[string]string{"root": root},
			snapshotInputBudget{MaximumObjects: 2, MaximumBytes: 10, MaximumDepth: 4, MaximumFileBytes: 10},
		)
		if err == nil || !strings.Contains(err.Error(), "object count") {
			t.Fatalf("object overrun = %v", err)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"a", "b"} {
			if err := os.WriteFile(filepath.Join(root, name), []byte("123"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := inventoryConsumptionUniverseWithBudget(
			map[string]string{"root": root},
			snapshotInputBudget{MaximumObjects: 4, MaximumBytes: 5, MaximumDepth: 4, MaximumFileBytes: 3},
		)
		if err == nil || !strings.Contains(err.Error(), "total bytes") {
			t.Fatalf("byte overrun = %v", err)
		}
	})
	t.Run("empty-deep-tree", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "one", "two", "three"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := inventoryConsumptionUniverseWithBudget(
			map[string]string{"root": root},
			snapshotInputBudget{MaximumObjects: 4, MaximumBytes: 10, MaximumDepth: 2, MaximumFileBytes: 10},
		)
		if err == nil || !strings.Contains(err.Error(), "maximum depth") {
			t.Fatalf("depth overrun = %v", err)
		}
	})
	t.Run("single-file-bytes", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "oversize"), []byte("1234"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := inventoryConsumptionUniverseWithBudget(
			map[string]string{"root": root},
			snapshotInputBudget{MaximumObjects: 2, MaximumBytes: 10, MaximumDepth: 2, MaximumFileBytes: 3},
		)
		if err == nil || !strings.Contains(err.Error(), "maximum file bytes") {
			t.Fatalf("single-file overrun = %v", err)
		}
	})
}

func TestFinalModuleVerificationIsBracketedByByteAuthority(t *testing.T) {
	t.Parallel()
	authority := &transitioningTestAuthority{}
	runner := &moduleMutationRunner{authority: authority}
	err := verifyDownloadedModulesUnderAuthority(
		context.Background(), runner,
		controlledGoEnvironment{GoExecutable: "go"},
		t.TempDir(), []Workload{{ModuleDir: "."}}, authority,
	)
	if err == nil || !strings.Contains(err.Error(), "after final module verification") {
		t.Fatalf("module mutation crossed final authority verification: %v", err)
	}
	if runner.calls != 1 || authority.verifications != 2 {
		t.Fatalf("verification order: runner calls=%d authority verifications=%d", runner.calls, authority.verifications)
	}
}

type transitioningTestAuthority struct {
	changed       bool
	verifications int
}

func (authority *transitioningTestAuthority) Verify() error {
	authority.verifications++
	if authority.changed {
		return errors.New("module bytes changed")
	}
	return nil
}

func (*transitioningTestAuthority) VerifyProcessStart(protocol.StartEvidence, string) (bool, error) {
	return true, nil
}

func (*transitioningTestAuthority) Close() error { return nil }

type moduleMutationRunner struct {
	authority *transitioningTestAuthority
	calls     int
}

func (runner *moduleMutationRunner) Run(context.Context, Command) (CommandResult, error) {
	runner.calls++
	runner.authority.changed = true
	return CommandResult{ExitCode: 0}, nil
}

func TestToolchainMaterializationCopiesOnlyInventoriedGeneration(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	inventoried := filepath.Join(source, "bin", "go")
	if err := os.MkdirAll(filepath.Dir(inventoried), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inventoried, []byte("generation-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	files, err := inventoryConsumptionUniverseWithBudget(
		map[string]string{"source": source}, defaultSnapshotInputBudget(),
	)
	if err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(source, "late", "unbounded")
	if err := os.MkdirAll(filepath.Dir(late), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(late, []byte("generation-b"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "copy")
	if err := copyAuthorityInventory(source, destination, files); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(destination, "bin", "go")); err != nil || string(content) != "generation-a" {
		t.Fatalf("inventoried generation = %q, err = %v", content, err)
	}
	if _, err := os.Lstat(filepath.Join(destination, "late", "unbounded")); !os.IsNotExist(err) {
		t.Fatalf("late unmetered input entered materialized generation: %v", err)
	}
}

func TestExactCopyRejectsNPlusOneBeforeUnboundedTransfer(t *testing.T) {
	t.Parallel()
	var destination bytes.Buffer
	err := copyExactIdentity(
		&destination, strings.NewReader("ab"), "growing-input", 1, hashBytes([]byte("a")),
	)
	if err == nil || !strings.Contains(err.Error(), "produced 2 bytes, expected 1") {
		t.Fatalf("N+1 transfer = %v", err)
	}
	if destination.Len() != 2 {
		t.Fatalf("bounded transfer observed %d bytes, want the single N+1 sentinel", destination.Len())
	}
}

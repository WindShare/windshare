//go:build linux

package perfevidence

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLinuxRejectsGroupWritableOutputRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "evidence")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := openOutputRootAuthority(root); err == nil || !strings.Contains(err.Error(), "not group/world writable") {
		t.Fatalf("writable output authority was accepted: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected output authority was mutated: %v", entries)
	}
}

func TestPhysicalContainmentDetectsProspectiveBindAlias(t *testing.T) {
	repository := t.TempDir()
	alias := t.TempDir()
	if err := unix.Mount(repository, alias, "", unix.MS_BIND, ""); err != nil {
		t.Skipf("bind-mount adversary requires mount permission: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(alias, 0) })
	runner := commandFunc(func(context.Context, Command) (CommandResult, error) {
		return CommandResult{ExitCode: 1}, nil
	})
	err := validateEvidenceOutputRoot(
		context.Background(), runner, repository,
		filepath.Join(alias, "prospective", "evidence"), "bind-alias",
	)
	if err == nil || !strings.Contains(err.Error(), "must be Git-ignored") {
		t.Fatalf("physical bind alias bypassed containment policy: %v", err)
	}
}

func TestPhysicalContainmentDetectsRepositorySubdirectoryBindAlias(t *testing.T) {
	repository := t.TempDir()
	subdirectory := filepath.Join(repository, "nested", "tracked")
	if err := os.MkdirAll(subdirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := t.TempDir()
	if err := unix.Mount(subdirectory, alias, "", unix.MS_BIND, ""); err != nil {
		t.Skipf("bind-mount adversary requires mount permission: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(alias, 0) })
	runner := commandFunc(func(context.Context, Command) (CommandResult, error) {
		return CommandResult{ExitCode: 1}, nil
	})
	err := validateEvidenceOutputRoot(
		context.Background(), runner, repository,
		filepath.Join(alias, "prospective", "evidence"), "subdir-bind-alias",
	)
	if err == nil || !strings.Contains(err.Error(), "must be Git-ignored") {
		t.Fatalf("repository-subdirectory bind alias bypassed containment policy: %v", err)
	}
}

func TestLinuxSealedMutationOutputRemainsRollbackCapableAfterAdoption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.bin")
	sink, err := prepareMutationOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("rollback-capable")
	if written, err := sink.WriteContext(context.Background(), content); err != nil || written != len(content) {
		t.Fatalf("write sealed output: wrote %d: %v", written, err)
	}
	if err := sink.Seal(context.Background(), int64(len(content)), hashBytes(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.adopt(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rolled-back sealed output survives: %v", err)
	}
}

func TestLinuxRecoveryCannotUseForgedRuntimeToDeleteLiveArtifact(t *testing.T) {
	root := testOutputRoot(t)
	stage, err := NewStage(root, "forged-runtime")
	if err != nil {
		t.Fatal(err)
	}
	directRuntime := filepath.Join(root, stage.runtimeName)
	movedRuntime := directRuntime + "-retained"
	if err := os.Rename(directRuntime, movedRuntime); err != nil {
		_ = stage.Abort()
		t.Fatal(err)
	}
	restored := false
	defer func() {
		if !restored {
			_ = os.RemoveAll(directRuntime)
			_ = os.Rename(movedRuntime, directRuntime)
		}
		if err := stage.Abort(); err != nil {
			t.Error(err)
		}
	}()
	if err := os.Mkdir(directRuntime, 0o700); err != nil {
		t.Fatal(err)
	}
	forgedOwner, err := json.Marshal(stageOwner{
		SchemaVersion: SchemaVersion,
		RunID:         stage.runID,
		ProcessID:     1 << 30,
		ProcessToken:  "dead-forged-owner",
		CreatedAt:     time.Unix(1, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directRuntime, stageOwnerName), forgedOwner, 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(stage.ArtifactRoot, "must-survive.txt")
	if err := writeExclusive(sentinel, []byte("live-artifact")); err != nil {
		t.Fatal(err)
	}

	if err := recoverAbandonedStages(root, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "live-artifact" {
		t.Fatalf("forged runtime authorized deletion of a live artifact: content=%q err=%v", content, err)
	}
	if err := os.RemoveAll(directRuntime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(movedRuntime, directRuntime); err != nil {
		t.Fatal(err)
	}
	restored = true
}

func TestLinuxStagePathsRemainBoundWhenRootIsReplaced(t *testing.T) {
	root := filepath.Join(t.TempDir(), "evidence")
	stage, err := NewStage(root, "root-replacement")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	if err := os.WriteFile(filepath.Join(stage.ArtifactRoot, "before.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "outside-sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage.ArtifactRoot, "after.txt"), []byte("after"), 0o600); err != nil {
		t.Fatalf("retained stage path lost its object after root rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, stage.artifactName, "after.txt")); err != nil {
		t.Fatalf("external-write path escaped retained stage object: %v", err)
	}
	if _, err := stage.Commit(Evidence{SchemaVersion: SchemaVersion, Kind: EvidenceKind}, nil, nil); err == nil {
		t.Fatal("replaced output pathname remained publishable")
	}
	if err := stage.Abort(); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "replacement" {
		t.Fatalf("abort touched replacement root: content=%q err=%v", content, err)
	}
	for _, name := range []string{stage.artifactName, stage.runtimeName} {
		if _, err := os.Lstat(filepath.Join(moved, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retained-root child survived abort: %s (%v)", name, err)
		}
	}
}

func TestLinuxPostRenameRootReplacementRollsBackWithinRetainedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "evidence")
	moved := root + "-moved"
	sentinel := filepath.Join(root, "outside-sentinel.txt")
	var stage *Stage
	replaced := false
	transition := func(name string) error {
		if replaced || name != "after-rename" {
			return nil
		}
		replaced = true
		if err := os.Rename(root, moved); err != nil {
			return err
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			return err
		}
		return os.WriteFile(sentinel, []byte("replacement"), 0o600)
	}
	var err error
	stage, err = newStage(root, "rename-authority", time.Now(), transition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage.ArtifactRoot, "sample.log"), []byte("sample"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stage.Commit(Evidence{SchemaVersion: SchemaVersion, Kind: EvidenceKind}, nil, nil); err == nil {
		t.Fatal("publication accepted a replaced output pathname")
	}
	if !replaced {
		t.Fatal("root-replacement adversary did not run")
	}
	if err := stage.Abort(); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "replacement" {
		t.Fatalf("publication touched replacement root: content=%q err=%v", content, err)
	}
	replacementEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(replacementEntries) != 1 || replacementEntries[0].Name() != filepath.Base(sentinel) {
		t.Fatalf("publication created children in replacement root: %v", replacementEntries)
	}
	retainedEntries, err := os.ReadDir(moved)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range retainedEntries {
		if len(entry.Name()) == 64 && entry.IsDir() {
			t.Fatalf("failed publication remained visible in retained root: %v", retainedEntries)
		}
	}
}

func TestLinuxSourceSwapAfterNameVerificationQuarantinesSubstitute(t *testing.T) {
	root := filepath.Join(t.TempDir(), "evidence")
	stage, err := NewStage(root, "rename-source-swap")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	if err := os.WriteFile(filepath.Join(stage.ArtifactRoot, "sample.log"), []byte("sample"), 0o600); err != nil {
		t.Fatal(err)
	}

	directStage := filepath.Join(root, stage.artifactName)
	retainedOriginal := filepath.Join(root, ".staging-source-retained")
	swapped := false
	stage.artifactDir.transition = func(relative, phase string) error {
		if swapped || relative != "" || phase != "rename-source-verified" {
			return nil
		}
		swapped = true
		if err := os.Rename(directStage, retainedOriginal); err != nil {
			return err
		}
		if err := os.Mkdir(directStage, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(directStage, "invalid.txt"), []byte("invalid"), 0o600)
	}

	if _, err := stage.Commit(Evidence{SchemaVersion: SchemaVersion, Kind: EvidenceKind}, nil, nil); err == nil {
		t.Fatal("publication accepted an unverified source substituted immediately before rename")
	}
	if !swapped {
		t.Fatal("source-substitution adversary did not run")
	}
	assertNoPublishedEvidence(t, root)
	if content, err := os.ReadFile(filepath.Join(retainedOriginal, "sample.log")); err != nil || string(content) != "sample" {
		t.Fatalf("retained verified source changed during failed publication: content=%q err=%v", content, err)
	}

	if err := stage.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedTree(root, retainedOriginal); err != nil {
		t.Fatal(err)
	}
}

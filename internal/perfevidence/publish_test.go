package perfevidence

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublicationIsVerifiedContentAddressedAndCreateOnly(t *testing.T) {
	root := testOutputRoot(t)
	evidence := Evidence{SchemaVersion: SchemaVersion, Kind: EvidenceKind, RunID: "same", Status: "succeeded"}
	stage, err := NewStage(root, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	if err := writeExclusive(filepath.Join(stage.ArtifactRoot, "logs", "sample.log"), []byte("sample")); err != nil {
		t.Fatal(err)
	}
	publication, err := stage.Commit(evidence, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(publication.Path) != publication.EvidenceID {
		t.Fatalf("publication = %+v", publication)
	}
	if err := VerifyPublication(publication.Path, publication.EvidenceID); err != nil {
		t.Fatal(err)
	}
	secondStage, err := NewStage(root, "second")
	if err != nil {
		t.Fatal(err)
	}
	defer secondStage.Abort()
	if err := writeExclusive(filepath.Join(secondStage.ArtifactRoot, "logs", "sample.log"), []byte("sample")); err != nil {
		t.Fatal(err)
	}
	secondPublication, err := secondStage.Commit(evidence, nil, nil)
	if err != nil || secondPublication != publication {
		t.Fatalf("idempotent publication = %+v, err = %v", secondPublication, err)
	}
	if err := os.WriteFile(filepath.Join(publication.Path, "logs", "sample.log"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPublication(publication.Path, publication.EvidenceID); err == nil {
		t.Fatal("tampered artifact was accepted")
	}
}

func TestPublicationRejectsUnsupportedArtifactsAndPayloads(t *testing.T) {
	root := testOutputRoot(t)
	if _, err := NewStage(root, "../escape"); err == nil {
		t.Fatal("unsafe performance run ID was accepted")
	}
	outside := t.TempDir()
	if _, _, err := publicationPaths(root, outside); err == nil {
		t.Fatal("stage outside its publication root was accepted")
	}
	stage, err := NewStage(root, "unsupported")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	link := filepath.Join(stage.ArtifactRoot, "link")
	if err := os.Symlink("missing", link); err == nil {
		if _, err := inspectArtifacts(stage.ArtifactRoot); err == nil {
			t.Fatal("symlink artifact was accepted")
		}
	}
	bad := filepath.Join(root, "bad")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, manifestName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, payloadName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPublication(bad, "wrong"); err == nil {
		t.Fatal("unsupported payload was accepted")
	}
}

func TestStageAbortAndCrashRecoveryOwnOnlyPerformanceDirectories(t *testing.T) {
	root := testOutputRoot(t)
	stage, err := NewStage(root, "abort")
	if err != nil {
		t.Fatal(err)
	}
	artifact := stage.ArtifactRoot
	runtimeRoot := stage.RuntimeRoot
	if err := stage.Abort(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{artifact, runtimeRoot} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned path survived abort: %s (%v)", path, err)
		}
	}

	abandonedArtifact := filepath.Join(root, ".staging-abandoned")
	abandonedRuntime := filepath.Join(root, ".runtime-abandoned")
	orphanArtifact := filepath.Join(root, ".staging-orphan")
	for _, path := range []string{abandonedArtifact, abandonedRuntime, orphanArtifact} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	owner, _ := json.Marshal(stageOwner{
		SchemaVersion: SchemaVersion, RunID: "abandoned", ProcessID: 1 << 30,
		ProcessToken: "dead-process-instance", CreatedAt: time.Unix(1, 0),
	})
	if err := os.WriteFile(filepath.Join(abandonedRuntime, stageOwnerName), owner, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverAbandonedStages(root, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{abandonedArtifact, abandonedRuntime, orphanArtifact} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("abandoned path survived recovery: %s (%v)", path, err)
		}
	}

	// A reused PID is not ownership. A current PID paired with a different
	// process-instance token must be reclaimed even when the stage is young.
	reusedArtifact := filepath.Join(root, ".staging-reused")
	reusedRuntime := filepath.Join(root, ".runtime-reused")
	for _, path := range []string{reusedArtifact, reusedRuntime} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	reusedOwner, err := json.Marshal(stageOwner{
		SchemaVersion: SchemaVersion, RunID: "reused", ProcessID: os.Getpid(),
		ProcessToken: "different-process-instance", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reusedRuntime, stageOwnerName), reusedOwner, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverAbandonedStages(root, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{reusedArtifact, reusedRuntime} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("PID-reuse stage survived recovery: %s (%v)", path, err)
		}
	}

	// Conversely, a matching run and process-instance token is live authority
	// and must never be reclaimed merely because timestamps are old.
	liveArtifact := filepath.Join(root, ".staging-live")
	liveRuntime := filepath.Join(root, ".runtime-live")
	for _, path := range []string{liveArtifact, liveRuntime} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	liveToken, err := currentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	liveOwner, err := json.Marshal(stageOwner{
		SchemaVersion: SchemaVersion, RunID: "live", ProcessID: os.Getpid(),
		ProcessToken: liveToken, CreatedAt: time.Unix(1, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveRuntime, stageOwnerName), liveOwner, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverAbandonedStages(root, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{liveArtifact, liveRuntime} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("live stage was reclaimed: %s (%v)", path, err)
		}
		if err := removeOwnedTree(root, path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCrashRecoveryDefersToLiveDirectoryLeases(t *testing.T) {
	root := testOutputRoot(t)
	stage, err := NewStage(root, "leased")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stage.Abort(); err != nil {
			t.Error(err)
		}
	}()
	sentinel := filepath.Join(stage.ArtifactRoot, "live.txt")
	if err := writeExclusive(sentinel, []byte("live")); err != nil {
		t.Fatal(err)
	}

	if err := recoverAbandonedStages(root, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "live" {
		t.Fatalf("recovery reclaimed a live leased stage: content=%q err=%v", content, err)
	}
}

func TestStageAbortNeverTraversesSymlinkedChildren(t *testing.T) {
	root := testOutputRoot(t)
	stage, err := NewStage(root, "symlink-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "must-survive.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o400); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(stage.ArtifactRoot, "linked-outside")
	if err := os.Symlink(outside, link); err != nil {
		_ = stage.Abort()
		t.Skipf("filesystem cannot create directory symlink: %v", err)
	}
	if err := stage.Abort(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(outsideFile)
	if err != nil || string(content) != "outside" {
		t.Fatalf("cleanup traversed outside the owned stage: content=%q err=%v", content, err)
	}
}

func TestHandleRelativeCleanupSurvivesDirectoryToSymlinkSwap(t *testing.T) {
	root := testOutputRoot(t)
	stage, err := NewStage(root, "swap-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	directStage := filepath.Join(root, stage.artifactName)
	nested := filepath.Join(directStage, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "owned.txt"), []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "must-survive.txt")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stage.artifactDir.close(); err != nil {
		t.Fatal(err)
	}
	stage.artifactDir = nil
	swapped := false
	err = removeOwnedTreeAuthority(stage.authority, stage.artifactName, func(relative string) error {
		if swapped || filepath.ToSlash(relative) != filepath.ToSlash(filepath.Join(stage.artifactName, "nested")) {
			return nil
		}
		swapped = true
		if err := os.Rename(nested, nested+"-moved"); err != nil {
			return err
		}
		return os.Symlink(outside, nested)
	})
	if err != nil {
		if !swapped {
			t.Skipf("filesystem cannot execute identity-swap adversary: %v", err)
		}
		t.Fatal(err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "outside" {
		t.Fatalf("handle-relative cleanup traversed replacement link: content=%q err=%v", content, err)
	}
	if _, err := os.Lstat(directStage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned stage survived cleanup swap: %v", err)
	}
}

func TestHandleRelativeReadAndSyncNeverTraverseDirectorySwap(t *testing.T) {
	operations := map[string]func(*stageDirectoryAuthority) error{
		"read": func(authority *stageDirectoryAuthority) error {
			_, err := inspectArtifactsAuthority(authority)
			return err
		},
		"sync": func(authority *stageDirectoryAuthority) error {
			return authority.syncContents()
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			root := testOutputRoot(t)
			stage, err := NewStage(root, "traversal-swap-"+name)
			if err != nil {
				t.Fatal(err)
			}
			defer stage.Abort()
			directStage := filepath.Join(root, stage.artifactName)
			nested := filepath.Join(directStage, "nested")
			if err := os.MkdirAll(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(nested, "owned.txt"), []byte("owned"), 0o600); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "outside-only.txt")
			if err := os.WriteFile(sentinel, []byte("outside"), 0o400); err != nil {
				t.Fatal(err)
			}
			var opened []string
			var linkErr error
			swapped := false
			stage.artifactDir.transition = func(relative, phase string) error {
				if phase == "file-opened" {
					opened = append(opened, filepath.ToSlash(relative))
				}
				if swapped || phase != "directory-opened" || filepath.ToSlash(relative) != "nested" {
					return nil
				}
				swapped = true
				if err := os.Rename(nested, nested+"-moved"); err != nil {
					return err
				}
				linkErr = os.Symlink(outside, nested)
				return linkErr
			}
			err = operation(stage.artifactDir)
			if linkErr != nil {
				t.Skipf("filesystem cannot create traversal-swap link: %v", linkErr)
			}
			if !swapped || err == nil {
				t.Fatalf("%s traversal accepted a directory identity swap: swapped=%v err=%v", name, swapped, err)
			}
			if containsString(opened, "nested/outside-only.txt") {
				t.Fatalf("%s traversal opened outside sentinel through replacement link: %v", name, opened)
			}
			if content, err := os.ReadFile(sentinel); err != nil || string(content) != "outside" {
				t.Fatalf("%s traversal changed outside sentinel: content=%q err=%v", name, content, err)
			}
		})
	}
}

func TestPublicationBindsReplacedBinaryAndProfilesToPayloadIdentities(t *testing.T) {
	for _, boundary := range []string{"before-artifact-inspection", "before-rename"} {
		t.Run(boundary, func(t *testing.T) {
			root := testOutputRoot(t)
			var stage *Stage
			transition := func(name string) error {
				if name != boundary {
					return nil
				}
				for path, content := range map[string][]byte{
					"binaries/unit.test":         []byte("binary-B"),
					"profiles/unit/cpu.pprof":    []byte("cpu-B"),
					"profiles/unit/memory.pprof": []byte("memory-B"),
				} {
					if err := os.WriteFile(filepath.Join(stage.ArtifactRoot, filepath.FromSlash(path)), content, 0o600); err != nil {
						return err
					}
				}
				return nil
			}
			var err error
			stage, err = newStage(root, "profile-binding-"+strings.ReplaceAll(boundary, "before-", ""), time.Now(), transition)
			if err != nil {
				t.Fatal(err)
			}
			defer stage.Abort()
			evidence := writeBoundProfileEvidence(t, stage.ArtifactRoot)
			if _, err := stage.Commit(evidence, nil, nil); err == nil {
				t.Fatalf("simultaneous binary/profile replacement at %s was published", boundary)
			}
		})
	}
}

func writeBoundProfileEvidence(t *testing.T, stageRoot string) Evidence {
	t.Helper()
	files := map[string][]byte{
		"binaries/unit.test":         []byte("binary-A"),
		"profiles/unit/cpu.pprof":    []byte("cpu-A"),
		"profiles/unit/memory.pprof": []byte("memory-A"),
	}
	identities := make(map[string]ArtifactFile, len(files))
	for path, content := range files {
		if err := writeExclusive(filepath.Join(stageRoot, filepath.FromSlash(path)), content); err != nil {
			t.Fatal(err)
		}
		identity, err := inspectArtifactIdentity(stageRoot, path)
		if err != nil {
			t.Fatal(err)
		}
		identities[path] = identity
	}
	binary := identities["binaries/unit.test"]
	return Evidence{
		SchemaVersion: SchemaVersion, Kind: EvidenceKind,
		Workloads: []WorkloadEvidence{{
			Definition: Workload{ID: "unit"},
			Binary:     BinaryEvidence{Path: binary.Path, Bytes: binary.Bytes, SHA256: binary.SHA256},
			Profile: &ProfileEvidence{
				Binary: binary,
				CPU:    identities["profiles/unit/cpu.pprof"],
				Memory: identities["profiles/unit/memory.pprof"],
			},
		}},
	}
}

func TestCommitRevalidatesImmediatelyAfterStagedVerification(t *testing.T) {
	root := testOutputRoot(t)
	valid := true
	validationCalls := 0
	stage, err := newStage(root, "final-validation", time.Now(), func(name string) error {
		if name == "before-final-source-validation" {
			valid = false
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	if err := writeExclusive(filepath.Join(stage.ArtifactRoot, "sample.log"), []byte("sample")); err != nil {
		t.Fatal(err)
	}
	_, err = stage.Commit(
		Evidence{SchemaVersion: SchemaVersion, Kind: EvidenceKind},
		func() error {
			validationCalls++
			if !valid {
				return errors.New("source changed during staged verification")
			}
			return nil
		},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "immediately-before-rename") {
		t.Fatalf("mutation at final publication boundary was accepted: %v", err)
	}
	if validationCalls != 3 {
		t.Fatalf("publication validation calls = %d, want pre/post inspection plus final boundary", validationCalls)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("failed final validation published %s", entry.Name())
		}
	}
}

func TestPublicationTransitionFailureLeavesStageAbortable(t *testing.T) {
	root := testOutputRoot(t)
	want := errors.New("injected")
	stage, err := newStage(root, "transition", time.Now(), func(name string) error {
		if name == "before-rename" {
			return want
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeExclusive(filepath.Join(stage.ArtifactRoot, "sample.log"), []byte("sample")); err != nil {
		t.Fatal(err)
	}
	if _, err := stage.Commit(Evidence{SchemaVersion: SchemaVersion, Kind: EvidenceKind}, nil, nil); !errors.Is(err, want) {
		t.Fatalf("transition error = %v", err)
	}
	if err := stage.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage.ArtifactRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed stage survived abort: %v", err)
	}
}

func TestPostRenameFailuresQuarantineExactPublication(t *testing.T) {
	for _, boundary := range []string{"after-rename", "after-verification"} {
		t.Run(boundary, func(t *testing.T) {
			root := testOutputRoot(t)
			injected := errors.New("injected post-rename failure")
			stage, err := newStage(root, "rollback-"+boundary, time.Now(), func(name string) error {
				if name == boundary {
					return injected
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stage.Abort()
			if err := writeExclusive(filepath.Join(stage.ArtifactRoot, "sample.log"), []byte("sample")); err != nil {
				t.Fatal(err)
			}
			if _, err := stage.Commit(
				Evidence{SchemaVersion: SchemaVersion, Kind: EvidenceKind}, nil, nil,
			); !errors.Is(err, injected) {
				t.Fatalf("post-rename failure = %v", err)
			}
			assertNoPublishedEvidence(t, root)
		})
	}
}

func TestExistingPublicationRemainsRetainedThroughReconciliation(t *testing.T) {
	for _, boundary := range []string{
		"existing-after-verification",
		"existing-after-stage-removal",
		"existing-after-root-sync",
	} {
		t.Run(boundary, func(t *testing.T) {
			root := testOutputRoot(t)
			evidence := Evidence{SchemaVersion: SchemaVersion, Kind: EvidenceKind, Status: "succeeded"}
			first, err := NewStage(root, "existing-first")
			if err != nil {
				t.Fatal(err)
			}
			defer first.Abort()
			if err := writeExclusive(filepath.Join(first.ArtifactRoot, "sample.log"), []byte("sample")); err != nil {
				t.Fatal(err)
			}
			publication, err := first.Commit(evidence, nil, nil)
			if err != nil {
				t.Fatal(err)
			}

			moved := publication.Path + "-retained-" + boundary
			swapped := false
			var swapErr error
			second, err := newStage(root, "existing-second-"+boundary, time.Now(), func(name string) error {
				if name != boundary {
					return nil
				}
				swapErr = os.Rename(publication.Path, moved)
				if swapErr != nil {
					return nil
				}
				swapped = true
				if err := os.Mkdir(publication.Path, 0o700); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(publication.Path, "foreign.txt"), []byte("foreign"), 0o600)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer second.Abort()
			t.Cleanup(func() {
				_ = os.RemoveAll(publication.Path)
				_ = os.RemoveAll(moved)
			})
			if err := writeExclusive(filepath.Join(second.ArtifactRoot, "sample.log"), []byte("sample")); err != nil {
				t.Fatal(err)
			}
			result, commitErr := second.Commit(evidence, nil, nil)
			if swapped {
				if commitErr == nil {
					t.Fatalf("reconciliation returned a foreign same-name destination: %+v", result)
				}
				if content, err := os.ReadFile(filepath.Join(publication.Path, "foreign.txt")); err != nil || string(content) != "foreign" {
					t.Fatalf("reconciliation touched foreign replacement: content=%q err=%v", content, err)
				}
				return
			}
			if swapErr == nil {
				t.Fatal("reconciliation swap neither succeeded nor reported an error")
			}
			if commitErr != nil || result != publication {
				t.Fatalf("kernel-retained reconciliation = %+v, err = %v (swap err %v)", result, commitErr, swapErr)
			}
			if err := VerifyPublication(publication.Path, publication.EvidenceID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRollbackNeverDeletesSameContentForeignReplacement(t *testing.T) {
	root := testOutputRoot(t)
	injected := errors.New("injected after foreign replacement")
	var publicationPath, retainedPath, evidenceID string
	swapped := false
	stage, err := newStage(root, "foreign-rollback", time.Now(), func(name string) error {
		if name != "after-rename" {
			return nil
		}
		publicationPath = findPublishedEvidence(t, root)
		if publicationPath == "" {
			return errors.New("published directory was not visible")
		}
		evidenceID = filepath.Base(publicationPath)
		retainedPath = publicationPath + "-exact-retained"
		if err := os.Rename(publicationPath, retainedPath); err != nil {
			return err
		}
		if err := copyTestDirectory(retainedPath, publicationPath); err != nil {
			return err
		}
		swapped = true
		return injected
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	t.Cleanup(func() {
		if publicationPath != "" {
			_ = os.RemoveAll(publicationPath)
		}
		if retainedPath != "" {
			_ = os.RemoveAll(retainedPath)
		}
	})
	if err := writeExclusive(filepath.Join(stage.ArtifactRoot, "sample.log"), []byte("sample")); err != nil {
		t.Fatal(err)
	}
	_, commitErr := stage.Commit(
		Evidence{SchemaVersion: SchemaVersion, Kind: EvidenceKind, Status: "succeeded"}, nil, nil,
	)
	if !swapped {
		t.Skipf("filesystem denied the retained-object swap adversary: %v", commitErr)
	}
	if !errors.Is(commitErr, injected) {
		t.Fatalf("foreign replacement failure = %v", commitErr)
	}
	if err := VerifyPublication(publicationPath, evidenceID); err != nil {
		t.Fatalf("rollback deleted or changed the byte-identical foreign replacement: %v", err)
	}
}

func TestPublicationSealDetectsPostRenameNamespaceMutation(t *testing.T) {
	root := testOutputRoot(t)
	mutated := false
	stage, err := newStage(root, "namespace-seal", time.Now(), func(name string) error {
		if name != "after-rename" {
			return nil
		}
		publication := findPublishedEvidence(t, root)
		if publication == "" {
			return errors.New("published directory was not visible at the post-rename boundary")
		}
		mutated = true
		return os.Rename(
			filepath.Join(publication, "sample.log"),
			filepath.Join(publication, "renamed.log"),
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	if err := writeExclusive(filepath.Join(stage.ArtifactRoot, "sample.log"), []byte("sample")); err != nil {
		t.Fatal(err)
	}
	if _, err := stage.Commit(
		Evidence{SchemaVersion: SchemaVersion, Kind: EvidenceKind}, nil, nil,
	); err == nil {
		t.Fatal("post-rename namespace mutation was published")
	}
	if !mutated {
		t.Fatal("post-rename namespace adversary did not run")
	}
	assertNoPublishedEvidence(t, root)
}

func TestEvidenceArtifactBindingsRequireCommandProvenance(t *testing.T) {
	artifact := func(path, content string) ArtifactFile {
		return ArtifactFile{Path: path, Bytes: int64(len(content)), SHA256: hashBytes([]byte(content))}
	}
	valid := func() (Evidence, map[string]ArtifactFile) {
		buildLog := artifact("logs/unit/build.log", "build")
		binary := artifact("binaries/unit.test", "binary")
		sampleLog := artifact("logs/unit/sample-01.log", "sample")
		profileLog := artifact("logs/unit/profile.log", "profile")
		cpu := artifact("profiles/unit/cpu.pprof", "cpu")
		memory := artifact("profiles/unit/memory.pprof", "memory")
		verificationLog := artifact("logs/unit/profile-cpu-verification.log", "verification")
		sourceBound := artifact("source/snapshot.json", "source")
		all := []ArtifactFile{buildLog, binary, sampleLog, profileLog, cpu, memory, verificationLog, sourceBound}
		manifest := make(map[string]ArtifactFile, len(all))
		for _, identity := range all {
			manifest[identity.Path] = identity
		}
		return Evidence{
			Status: string(EvidenceOutcomeSucceeded),
			Workloads: []WorkloadEvidence{{
				Definition: Workload{ID: "unit"},
				Build: CommandEvidence{
					Phase: EvidencePhaseBuild, Outcome: EvidenceOutcomeSucceeded,
					Artifacts: []ArtifactFile{buildLog, binary},
				},
				Binary: BinaryEvidence{
					Path: binary.Path, Bytes: binary.Bytes, SHA256: binary.SHA256,
					GoBuildID: "build-id", GoVersionMetadata: "go-version",
					BuildGraphSHA256: hashBytes([]byte("graph")),
				},
				Samples: []BenchmarkSample{{
					Command: CommandEvidence{
						Phase: EvidencePhaseSample, Outcome: EvidenceOutcomeSucceeded,
						Artifacts: []ArtifactFile{sampleLog},
					},
				}},
				Profile: &ProfileEvidence{
					Command: CommandEvidence{
						Phase: EvidencePhaseProfile, Outcome: EvidenceOutcomeSucceeded,
						Artifacts: []ArtifactFile{profileLog, cpu, memory},
					},
					Verification: []CommandEvidence{{
						Phase: EvidencePhaseProfileVerification, Outcome: EvidenceOutcomeSucceeded,
						Artifacts: []ArtifactFile{verificationLog},
					}},
					Binary: binary, CPU: cpu, Memory: memory,
				},
			}},
		}, manifest
	}

	evidence, manifest := valid()
	if err := verifyEvidenceArtifactBindings(evidence, manifest); err != nil {
		t.Fatalf("valid command provenance was rejected: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*Evidence, map[string]ArtifactFile)
	}{
		{
			name: "artifact-not-in-manifest",
			mutate: func(evidence *Evidence, _ map[string]ArtifactFile) {
				evidence.Workloads[0].Samples[0].Command.Artifacts = append(
					evidence.Workloads[0].Samples[0].Command.Artifacts,
					artifact("logs/unit/forged.log", "forged"),
				)
			},
		},
		{
			name: "duplicate-produced-claim",
			mutate: func(evidence *Evidence, _ map[string]ArtifactFile) {
				evidence.Workloads[0].Samples[0].Command.Artifacts = []ArtifactFile{
					evidence.Workloads[0].Build.Artifacts[0],
				}
			},
		},
		{
			name: "illegal-phase",
			mutate: func(evidence *Evidence, _ map[string]ArtifactFile) {
				evidence.Workloads[0].Build.Phase = EvidencePhaseSample
			},
		},
		{
			name: "successful-build-omits-binary-claim",
			mutate: func(evidence *Evidence, _ map[string]ArtifactFile) {
				evidence.Workloads[0].Build.Artifacts = evidence.Workloads[0].Build.Artifacts[:1]
			},
		},
		{
			name: "successful-profile-omits-profile-claim",
			mutate: func(evidence *Evidence, _ map[string]ArtifactFile) {
				evidence.Workloads[0].Profile.Command.Artifacts = evidence.Workloads[0].Profile.Command.Artifacts[:2]
			},
		},
		{
			name: "failed-command-in-successful-evidence",
			mutate: func(evidence *Evidence, _ map[string]ArtifactFile) {
				evidence.Workloads[0].Samples[0].Command.Outcome = EvidenceOutcomeFailed
				evidence.Workloads[0].Samples[0].Command.Error = "failed"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence, manifest := valid()
			test.mutate(&evidence, manifest)
			if err := verifyEvidenceArtifactBindings(evidence, manifest); err == nil {
				t.Fatal("invalid command artifact provenance was accepted")
			}
		})
	}

	failedBuildLog := artifact("logs/failed/build.log", "failed")
	failedEvidence := Evidence{
		Status: string(EvidenceOutcomeFailed),
		Workloads: []WorkloadEvidence{{
			Definition: Workload{ID: "failed"},
			Build: CommandEvidence{
				Phase: EvidencePhaseBuild, Outcome: EvidenceOutcomeFailed, Error: "build failed",
				Artifacts: []ArtifactFile{failedBuildLog},
			},
		}},
	}
	if err := verifyEvidenceArtifactBindings(
		failedEvidence, map[string]ArtifactFile{failedBuildLog.Path: failedBuildLog},
	); err != nil {
		t.Fatalf("failed build without a rich binary identity was rejected: %v", err)
	}
}

func TestEvidenceStoreBudgetRejectsRecoveryBeforeDeletingSentinel(t *testing.T) {
	baseBudget := func() EvidenceStoreBudget {
		return EvidenceStoreBudget{
			MaxRootEntries: 8, MaxObjects: 8, MaxDepth: 8,
			MaxMetadataBytes: 64, MaxPayloadBytes: 64, MaxTotalBytes: 1 << 10,
		}
	}
	tests := []struct {
		name    string
		budget  EvidenceStoreBudget
		prepare func(*testing.T, string)
	}{
		{
			name: "root-n-plus-one",
			budget: func() EvidenceStoreBudget {
				budget := baseBudget()
				budget.MaxRootEntries = 2
				return budget
			}(),
			prepare: func(t *testing.T, root string) {
				for _, name := range []string{".staging-z-one", ".staging-z-two"} {
					if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "object-n-plus-one",
			budget: func() EvidenceStoreBudget {
				budget := baseBudget()
				budget.MaxObjects = 2
				return budget
			}(),
			prepare: func(t *testing.T, root string) {
				for _, name := range []string{"one", "two"} {
					if err := writeExclusive(filepath.Join(root, ".staging-z-hostile", name), []byte(name)); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "directory-only-n-plus-one",
			budget: func() EvidenceStoreBudget {
				budget := baseBudget()
				budget.MaxObjects = 3
				return budget
			}(),
			prepare: func(t *testing.T, root string) {
				for _, name := range []string{"one", "two", "three"} {
					if err := os.MkdirAll(filepath.Join(root, ".staging-z-hostile", name), 0o700); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "cross-candidate-aggregate-n-plus-one",
			budget: func() EvidenceStoreBudget {
				budget := baseBudget()
				budget.MaxObjects = 2
				return budget
			}(),
			prepare: func(t *testing.T, root string) {
				for _, name := range []string{".staging-z-one", ".staging-z-two"} {
					if err := writeExclusive(filepath.Join(root, name, "payload"), []byte("x")); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "deep-tree",
			budget: func() EvidenceStoreBudget {
				budget := baseBudget()
				budget.MaxDepth = 2
				return budget
			}(),
			prepare: func(t *testing.T, root string) {
				if err := writeExclusive(
					filepath.Join(root, ".staging-z-hostile", "one", "two", "payload"), []byte("deep"),
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "empty-deep-tree",
			budget: func() EvidenceStoreBudget {
				budget := baseBudget()
				budget.MaxDepth = 2
				return budget
			}(),
			prepare: func(t *testing.T, root string) {
				if err := os.MkdirAll(
					filepath.Join(root, ".staging-z-hostile", "one", "two", "three"), 0o700,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "total-bytes",
			budget: EvidenceStoreBudget{
				MaxRootEntries: 8, MaxObjects: 8, MaxDepth: 8,
				MaxMetadataBytes: 8, MaxPayloadBytes: 8, MaxTotalBytes: 8,
			},
			prepare: func(t *testing.T, root string) {
				if err := writeExclusive(
					filepath.Join(root, ".staging-z-hostile", "payload"), []byte("12345"),
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversized-owner-metadata",
			budget: EvidenceStoreBudget{
				MaxRootEntries: 8, MaxObjects: 8, MaxDepth: 8,
				MaxMetadataBytes: 8, MaxPayloadBytes: 64, MaxTotalBytes: 64,
			},
			prepare: func(t *testing.T, root string) {
				if err := writeExclusive(
					filepath.Join(root, ".runtime-z-hostile", stageOwnerName), []byte("123456789"),
				); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := testOutputRoot(t)
			victim := filepath.Join(root, ".staging-a-victim")
			sentinel := filepath.Join(victim, "must-survive.txt")
			if err := writeExclusive(sentinel, []byte("safe")); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-48 * time.Hour)
			if err := os.Chtimes(victim, old, old); err != nil {
				t.Fatal(err)
			}
			test.prepare(t, root)
			if err := recoverAbandonedStagesWithBudget(root, time.Now(), test.budget); err == nil {
				t.Fatal("hostile evidence store was recovered without enforcing its budget")
			}
			content, err := os.ReadFile(sentinel)
			if err != nil || string(content) != "safe" {
				t.Fatalf("budget failure touched recovery sentinel: content=%q err=%v", content, err)
			}
		})
	}
}

func TestEvidenceStoreBudgetBoundsMetadataAndPayloadReads(t *testing.T) {
	for _, test := range []struct {
		name     string
		budget   EvidenceStoreBudget
		manifest []byte
		payload  []byte
	}{
		{
			name: "metadata",
			budget: EvidenceStoreBudget{
				MaxRootEntries: 4, MaxObjects: 4, MaxDepth: 4,
				MaxMetadataBytes: 8, MaxPayloadBytes: 64, MaxTotalBytes: 64,
			},
			manifest: []byte("123456789"), payload: []byte("{}"),
		},
		{
			name: "payload",
			budget: EvidenceStoreBudget{
				MaxRootEntries: 4, MaxObjects: 4, MaxDepth: 4,
				MaxMetadataBytes: 64, MaxPayloadBytes: 8, MaxTotalBytes: 64,
			},
			manifest: []byte("{}"), payload: []byte("123456789"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "publication")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string][]byte{
				manifestName: test.manifest,
				payloadName:  test.payload,
			} {
				if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sentinel := filepath.Join(t.TempDir(), "must-survive.txt")
			if err := os.WriteFile(sentinel, []byte("safe"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := VerifyPublicationWithBudget(root, "", test.budget); err == nil {
				t.Fatal("oversized evidence document was accepted")
			}
			if content, err := os.ReadFile(sentinel); err != nil || string(content) != "safe" {
				t.Fatalf("bounded read touched sentinel: content=%q err=%v", content, err)
			}
		})
	}
}

func assertNoPublishedEvidence(t *testing.T, root string) {
	t.Helper()
	if publication := findPublishedEvidence(t, root); publication != "" {
		t.Fatalf("failed publication remained visible at %s", publication)
	}
}

func findPublishedEvidence(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) == 64 && entry.IsDir() {
			return filepath.Join(root, entry.Name())
		}
	}
	return ""
}

func copyTestDirectory(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return writeExclusive(target, content)
	})
}

func testOutputRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "evidence")
}

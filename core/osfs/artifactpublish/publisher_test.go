package artifactpublish

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPublishDirectoryReturnsDurableRereadBytes(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	request := directoryFixture(parent, "generation", ".stage-success")
	result, err := PublishDirectory(request)
	if err != nil {
		t.Fatalf("PublishDirectory: %v", err)
	}
	if len(result.Artifacts) != len(request.Artifacts) {
		t.Fatalf("published artifact count = %d, want %d", len(result.Artifacts), len(request.Artifacts))
	}
	for index, artifact := range result.Artifacts {
		expected := request.Artifacts[index]
		if artifact.Name != expected.Name || artifact.SHA256 != expected.SHA256 ||
			!slices.Equal(artifact.Bytes, expected.Bytes) {
			t.Fatalf("published artifact %d differs from request", index)
		}
		onDisk, readErr := os.ReadFile(filepath.Join(parent, request.OutputName, expected.Name))
		if readErr != nil || !slices.Equal(onDisk, expected.Bytes) {
			t.Fatalf("read published artifact %d: bytes=%q err=%v", index, onDisk, readErr)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, request.StagingName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory remained after commit: %v", err)
	}
}

func TestPublishDirectoryNeverReplacesRaceCreatedDestinations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		create func(*testing.T, string)
		verify func(*testing.T, string)
	}{
		{
			name: "empty directory",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("create race directory: %v", err)
				}
			},
			verify: func(t *testing.T, path string) {
				t.Helper()
				entries, err := os.ReadDir(path)
				if err != nil || len(entries) != 0 {
					t.Fatalf("race directory changed: entries=%v err=%v", entries, err)
				}
			},
		},
		{
			name: "regular file",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("foreign\n"), 0o600); err != nil {
					t.Fatalf("create race file: %v", err)
				}
			},
			verify: func(t *testing.T, path string) {
				t.Helper()
				content, err := os.ReadFile(path)
				if err != nil || string(content) != "foreign\n" {
					t.Fatalf("race file changed: content=%q err=%v", content, err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			request := directoryFixture(parent, "generation", ".stage-race")
			owner := publisher{open: openNativePlatform, hook: func(
				boundary publicationBoundary,
				_ *transactionState,
			) error {
				if boundary == boundaryBeforeCommit {
					test.create(t, filepath.Join(parent, request.OutputName))
				}
				return nil
			}}
			if _, err := owner.publishDirectory(request); err == nil {
				t.Fatal("race-created destination was replaced")
			}
			test.verify(t, filepath.Join(parent, request.OutputName))
		})
	}
}

func TestPublishDirectoryRejectsRaceCreatedSymlink(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	foreign := filepath.Join(parent, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatalf("create foreign directory: %v", err)
	}
	request := directoryFixture(parent, "generation", ".stage-symlink")
	link := filepath.Join(parent, request.OutputName)
	owner := publisher{open: openNativePlatform, hook: func(
		boundary publicationBoundary,
		_ *transactionState,
	) error {
		if boundary != boundaryBeforeCommit {
			return nil
		}
		if err := os.Symlink(foreign, link); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		return nil
	}}
	if _, err := owner.publishDirectory(request); err == nil {
		t.Fatal("race-created symlink was replaced")
	}
	metadata, err := os.Lstat(link)
	if err != nil || metadata.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("race-created symlink changed: metadata=%v err=%v", metadata, err)
	}
}

func TestPublishDirectoryRejectsStagedMutationBeforeCommit(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	request := directoryFixture(parent, "generation", ".stage-mutation")
	owner := publisher{open: openNativePlatform, hook: func(
		boundary publicationBoundary,
		state *transactionState,
	) error {
		if boundary == boundaryBeforeCommit {
			_, err := state.stagedFiles[0].file.WriteAt([]byte("evil\n"), 0)
			return err
		}
		return nil
	}}
	if _, err := owner.publishDirectory(request); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("staged mutation error = %v, want ErrUnsafe", err)
	}
	if _, err := os.Stat(filepath.Join(parent, request.OutputName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mutated stage reached final name: %v", err)
	}
}

func TestPublishDirectoryDetectsDescendantSwapAtNativeCommit(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	request := directoryFixture(parent, "generation", ".stage-descendant-swap")
	displaced := filepath.Join(parent, ".displaced-run")
	owner := publisher{open: openNativePlatform, hook: func(
		boundary publicationBoundary,
		_ *transactionState,
	) error {
		if boundary != boundaryBeforeNativeCommit {
			return nil
		}
		staged := filepath.Join(parent, request.StagingName, request.Artifacts[0].Name)
		if err := os.Rename(staged, displaced); err != nil {
			return err
		}
		return os.WriteFile(staged, []byte("evil\n"), 0o600)
	}}
	if _, err := owner.publishDirectory(request); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("descendant swap error = %v, want ErrUnsafe", err)
	}
	content, err := os.ReadFile(displaced)
	if err != nil || string(content) != "good\n" {
		t.Fatalf("publisher changed displaced owned file: content=%q err=%v", content, err)
	}
}

func TestPublishDirectoryFailsClosedOnPostCommitDrift(t *testing.T) {
	for _, boundary := range []publicationBoundary{boundaryAfterCommit, boundaryAfterDurability} {
		t.Run(fmt.Sprintf("boundary-%d", boundary), func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			request := directoryFixture(parent, "generation", ".stage-post-commit")
			owner := publisher{open: openNativePlatform, hook: func(
				actual publicationBoundary,
				state *transactionState,
			) error {
				if actual == boundary {
					file, err := state.installed.OpenFile(state.stagedFiles[0].expected.Name, true, true)
					if err != nil {
						return err
					}
					_, writeErr := file.WriteAt([]byte("evil\n"), 0)
					return errors.Join(writeErr, file.Close())
				}
				return nil
			}}
			if _, err := owner.publishDirectory(request); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("post-commit drift error = %v, want ErrUnsafe", err)
			}
			content, err := os.ReadFile(filepath.Join(parent, request.OutputName, request.Artifacts[0].Name))
			if err != nil || string(content) != "evil\n" {
				t.Fatalf("post-commit fixture did not reach expected hostile state: content=%q err=%v", content, err)
			}
		})
	}
}

func TestPublishDirectoryDetectsStagingPathSwap(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	request := directoryFixture(parent, "generation", ".stage-swap")
	owner := publisher{open: openNativePlatform, hook: func(
		boundary publicationBoundary,
		_ *transactionState,
	) error {
		if boundary != boundaryBeforeCommit {
			return nil
		}
		stage := filepath.Join(parent, request.StagingName)
		moved := filepath.Join(parent, ".stage-moved")
		if err := os.Rename(stage, moved); err != nil {
			// A platform placement guard that blocks the swap is also exact proof.
			return err
		}
		return os.Mkdir(stage, 0o700)
	}}
	if _, err := owner.publishDirectory(request); err == nil {
		t.Fatal("staging path swap was accepted")
	}
	if _, err := os.Stat(filepath.Join(parent, request.OutputName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("swapped staging path reached final name: %v", err)
	}
}

func TestPublishFileUsesNoReplaceLinkAndRetiresProvenStage(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	artifact := artifactFixture("aggregate.json", "aggregate\n")
	request := FileRequest{
		ParentPath: parent, OutputName: "aggregate.json", StagingName: ".aggregate.stage", Artifact: artifact,
	}
	result, err := PublishFile(request)
	if err != nil {
		t.Fatalf("PublishFile: %v", err)
	}
	if len(result.Artifacts) != 1 || !slices.Equal(result.Artifacts[0].Bytes, artifact.Bytes) {
		t.Fatalf("unexpected file publication result: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(parent, request.StagingName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proven file stage remained: %v", err)
	}
	foreign := []byte("foreign\n")
	second := FileRequest{
		ParentPath: parent, OutputName: "foreign.json", StagingName: ".foreign.stage", Artifact: artifact,
	}
	owner := publisher{open: openNativePlatform, hook: func(
		boundary publicationBoundary,
		_ *transactionState,
	) error {
		if boundary == boundaryBeforeCommit {
			return os.WriteFile(filepath.Join(parent, second.OutputName), foreign, 0o600)
		}
		return nil
	}}
	if _, err := owner.publishFile(second); err == nil {
		t.Fatal("file publisher replaced race-created destination")
	}
	content, err := os.ReadFile(filepath.Join(parent, second.OutputName))
	if err != nil || !slices.Equal(content, foreign) {
		t.Fatalf("race-created file changed: content=%q err=%v", content, err)
	}
}

func TestPublicationRequestValidation(t *testing.T) {
	t.Parallel()
	valid := directoryFixture(t.TempDir(), "generation", ".stage-validation")
	invalid := []DirectoryRequest{
		{},
		{ParentPath: "relative", OutputName: "out", StagingName: ".stage", Artifacts: valid.Artifacts},
		{ParentPath: valid.ParentPath, OutputName: "../out", StagingName: ".stage", Artifacts: valid.Artifacts},
		{ParentPath: valid.ParentPath, OutputName: "out", StagingName: "out", Artifacts: valid.Artifacts},
		{ParentPath: valid.ParentPath, OutputName: "out", StagingName: ".stage", Artifacts: []Artifact{
			{Name: "same", Bytes: []byte("a"), SHA256: digest([]byte("a"))},
			{Name: "same", Bytes: []byte("b"), SHA256: digest([]byte("b"))},
		}},
		{ParentPath: valid.ParentPath, OutputName: "out", StagingName: ".stage", Artifacts: []Artifact{
			{Name: "bad", Bytes: []byte("a"), SHA256: stringsOf("0", 64)},
		}},
	}
	for index, request := range invalid {
		if _, err := PublishDirectory(request); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("invalid request %d error = %v, want ErrUnsafe", index, err)
		}
	}
}

func TestPublicationNamesAllowHexButRejectPathAuthority(t *testing.T) {
	t.Parallel()
	for _, accepted := range []string{"x0", ".network-matrix-stage-0x"} {
		if err := requireName(accepted); err != nil {
			t.Fatalf("requireName(%q): %v", accepted, err)
		}
	}
	for _, rejected := range []string{"with\x00nul", "child/name", `child\name`} {
		if err := requireName(rejected); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("requireName(%q) error = %v, want ErrUnsafe", rejected, err)
		}
	}
}

func directoryFixture(parent, output, stage string) DirectoryRequest {
	return DirectoryRequest{
		ParentPath:  parent,
		OutputName:  output,
		StagingName: stage,
		Artifacts: []Artifact{
			artifactFixture("run.json", "good\n"),
			artifactFixture("aggregate.json", "aggregate\n"),
		},
	}
}

func artifactFixture(name, content string) Artifact {
	bytes := []byte(content)
	return Artifact{Name: name, Bytes: bytes, SHA256: digest(bytes)}
}

func digest(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func stringsOf(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}

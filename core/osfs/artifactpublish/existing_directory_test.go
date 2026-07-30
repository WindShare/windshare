package artifactpublish

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestExistingDirectoryPublishesAndReverifiesExactFinalSnapshots(t *testing.T) {
	t.Parallel()
	fixture := prepareExistingDirectoryFixture(t)
	result, err := PublishExistingDirectory(fixture.publishRequest())
	if err != nil {
		t.Fatalf("publish existing directory: %v", err)
	}
	if result.ManifestSHA256 != fixture.manifestSHA256 || len(result.Snapshots) != 1 ||
		result.Snapshots[0].RelativePath != "samples/chromium/result.json" ||
		string(result.Snapshots[0].Bytes) != "result\n" {
		t.Fatalf("unexpected authenticated result: %#v", result)
	}
	if _, err := os.Lstat(filepath.Join(fixture.parent, fixture.stagingName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging name survived no-replace install: %v", err)
	}
	verified, err := VerifyExistingDirectory(fixture.verifyRequest())
	if err != nil || verified.ManifestSHA256 != fixture.manifestSHA256 ||
		!slices.Equal(verified.Snapshots[0].Bytes, result.Snapshots[0].Bytes) {
		t.Fatalf("verify existing directory: result=%#v err=%v", verified, err)
	}
	if _, err := PrepareExistingDirectoryStaging(ExistingDirectoryStagingRequest{
		ParentPath:             fixture.parent,
		StagingName:            ".browser-evidence-upload-11111111111111111111111111111111",
		Inventory:              fixture.inventory,
		ManifestPath:           existingDirectoryManifestPath,
		ExpectedManifestSHA256: fixture.manifestSHA256,
	}); !errors.Is(err, ErrCollision) {
		t.Fatalf("second authority did not collide with sealed final: %v", err)
	}
}

func TestExistingDirectoryRejectsUnexpectedAndSymlinkEntries(t *testing.T) {
	t.Parallel()
	t.Run("unexpected file", func(t *testing.T) {
		t.Parallel()
		fixture := prepareExistingDirectoryFixture(t)
		if err := os.WriteFile(filepath.Join(fixture.stagePath, "unexpected.txt"), []byte("foreign\n"), 0o600); err != nil {
			t.Fatalf("write unexpected file: %v", err)
		}
		if _, err := PublishExistingDirectory(fixture.publishRequest()); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("unexpected file was not rejected: %v", err)
		}
		assertPathAbsent(t, filepath.Join(fixture.parent, ExistingDirectoryOutputName))
	})
	t.Run("symbolic link", func(t *testing.T) {
		t.Parallel()
		fixture := prepareExistingDirectoryFixture(t)
		link := filepath.Join(fixture.stagePath, "samples", "chromium", "link.json")
		if err := os.Symlink(filepath.Join(fixture.stagePath, existingDirectoryManifestPath), link); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("Windows symlink privilege is unavailable: %v", err)
			}
			t.Fatalf("create symlink: %v", err)
		}
		request := fixture.publishRequest()
		request.Inventory.Files = append(request.Inventory.Files, existingFile(t, fixture.stagePath, "samples/chromium/link.json"))
		slices.SortFunc(request.Inventory.Files, compareExistingFiles)
		if _, err := PublishExistingDirectory(request); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("symbolic link was not rejected: %v", err)
		}
		assertPathAbsent(t, filepath.Join(fixture.parent, ExistingDirectoryOutputName))
	})
}

func TestExistingDirectoryRevalidatesAtEveryCommitBoundary(t *testing.T) {
	t.Parallel()
	t.Run("pre-commit mutation", func(t *testing.T) {
		t.Parallel()
		fixture := prepareExistingDirectoryFixture(t)
		owner := publisher{openPrivate: openPrivateNativePlatform, hook: func(boundary publicationBoundary, _ *transactionState) error {
			if boundary == boundaryBeforeCommit {
				return os.WriteFile(filepath.Join(fixture.stagePath, "samples", "chromium", "result.json"), []byte("changed\n"), 0o600)
			}
			return nil
		}}
		if _, err := owner.publishExistingDirectory(fixture.publishRequest()); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("pre-commit mutation was not rejected: %v", err)
		}
		assertPathAbsent(t, filepath.Join(fixture.parent, ExistingDirectoryOutputName))
	})
	t.Run("post-commit mutation", func(t *testing.T) {
		t.Parallel()
		fixture := prepareExistingDirectoryFixture(t)
		owner := publisher{openPrivate: openPrivateNativePlatform, hook: func(boundary publicationBoundary, _ *transactionState) error {
			if boundary == boundaryAfterCommit {
				return os.WriteFile(filepath.Join(
					fixture.parent, ExistingDirectoryOutputName, "samples", "chromium", "result.json",
				), []byte("changed\n"), 0o600)
			}
			return nil
		}}
		if _, err := owner.publishExistingDirectory(fixture.publishRequest()); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("post-commit mutation was not rejected: %v", err)
		}
	})
	t.Run("post-commit ambiguity is recoverable only by exact authority", func(t *testing.T) {
		t.Parallel()
		fixture := prepareExistingDirectoryFixture(t)
		owner := publisher{openPrivate: openPrivateNativePlatform, hook: func(boundary publicationBoundary, _ *transactionState) error {
			if boundary == boundaryAfterDurability {
				return errors.New("injected response ambiguity")
			}
			return nil
		}}
		if _, err := owner.publishExistingDirectory(fixture.publishRequest()); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("post-commit ambiguity did not fail closed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(fixture.parent, ExistingDirectoryOutputName)); err != nil {
			t.Fatalf("ambiguous durable final is absent: %v", err)
		}
		if _, err := VerifyExistingDirectory(fixture.verifyRequest()); err != nil {
			t.Fatalf("exact expected manifest authority did not recover durable final: %v", err)
		}
		wrong := fixture.verifyRequest()
		wrong.ExpectedManifestSHA256 = strings.Repeat("0", sha256.Size*2)
		if _, err := VerifyExistingDirectory(wrong); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("foreign manifest authority recovered ambiguous final: %v", err)
		}
	})
}

func TestExistingDirectoryPortablePathContract(t *testing.T) {
	t.Parallel()
	encoded, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "testdata", "browser-evidence", "portable-path-vectors.json",
	))
	if err != nil {
		t.Fatalf("read shared portable path vectors: %v", err)
	}
	var vectors struct {
		Accepted   []string    `json:"accepted"`
		Rejected   []string    `json:"rejected"`
		Unordered  []string    `json:"unordered"`
		Ordered    []string    `json:"ordered"`
		Collisions [][2]string `json:"collisions"`
	}
	if err := json.Unmarshal(encoded, &vectors); err != nil {
		t.Fatalf("decode shared portable path vectors: %v", err)
	}
	for _, relativePath := range vectors.Accepted {
		if err := requirePortableExistingPath(relativePath); err != nil {
			t.Errorf("portable path rejected: %q err=%v", relativePath, err)
		}
	}
	for _, relativePath := range vectors.Rejected {
		if err := requirePortableExistingPath(relativePath); !errors.Is(err, ErrUnsafe) {
			t.Errorf("non-portable path accepted: %q err=%v", relativePath, err)
		}
	}
	ordered := slices.Clone(vectors.Unordered)
	sort.Strings(ordered)
	if !slices.Equal(ordered, vectors.Ordered) {
		t.Errorf("UTF-8 byte order drifted: got=%q want=%q", ordered, vectors.Ordered)
	}
	for _, collision := range vectors.Collisions {
		if portableExistingPathKey(collision[0]) != portableExistingPathKey(collision[1]) {
			t.Errorf("portable collision key did not fold shared pair %q", collision)
		}
	}
}

func TestExistingDirectoryCleanupRequiresExactPersistentReceipt(t *testing.T) {
	t.Parallel()
	t.Run("failed copy cleanup", func(t *testing.T) {
		t.Parallel()
		fixture := prepareExistingDirectoryFixture(t)
		if err := os.WriteFile(filepath.Join(fixture.stagePath, "samples", "chromium", "result.json"), []byte("partial"), 0o600); err != nil {
			t.Fatalf("write partial staged file: %v", err)
		}
		outcome, err := CleanupExistingDirectoryStaging(fixture.cleanupRequest())
		if err != nil || outcome != ExistingDirectoryCleanupCompleted {
			t.Fatalf("cleanup failed copy: outcome=%s err=%v", outcome, err)
		}
		assertPathAbsent(t, fixture.stagePath)
	})
	t.Run("unexpected entry is ambiguous", func(t *testing.T) {
		t.Parallel()
		fixture := prepareExistingDirectoryFixture(t)
		if err := os.WriteFile(filepath.Join(fixture.stagePath, "unexpected"), []byte("foreign"), 0o600); err != nil {
			t.Fatalf("write unexpected entry: %v", err)
		}
		outcome, err := CleanupExistingDirectoryStaging(fixture.cleanupRequest())
		if !errors.Is(err, ErrUnsafe) || outcome != ExistingDirectoryCleanupAmbiguous {
			t.Fatalf("unexpected entry cleanup was not ambiguous: outcome=%s err=%v", outcome, err)
		}
		if _, err := os.Lstat(filepath.Join(fixture.stagePath, "unexpected")); err != nil {
			t.Fatalf("ambiguous cleanup mutated unexpected entry: %v", err)
		}
	})
	t.Run("swapped stage is ambiguous", func(t *testing.T) {
		t.Parallel()
		fixture := prepareExistingDirectoryFixture(t)
		original := fixture.stagePath + "-original"
		if err := os.Rename(fixture.stagePath, original); err != nil {
			t.Fatalf("move original staging for swap: %v", err)
		}
		foreignReceipt, err := PrepareExistingDirectoryStaging(ExistingDirectoryStagingRequest{
			ParentPath: fixture.parent, StagingName: fixture.stagingName,
			Inventory: fixture.inventory, ManifestPath: existingDirectoryManifestPath,
			ExpectedManifestSHA256: fixture.manifestSHA256,
		})
		if err != nil || foreignReceipt.IsZero() {
			t.Fatalf("prepare swapped staging: receipt=%#v err=%v", foreignReceipt, err)
		}
		outcome, err := CleanupExistingDirectoryStaging(fixture.cleanupRequest())
		if !errors.Is(err, ErrUnsafe) || outcome != ExistingDirectoryCleanupAmbiguous {
			t.Fatalf("swapped cleanup was not ambiguous: outcome=%s err=%v", outcome, err)
		}
		if _, err := os.Lstat(fixture.stagePath); err != nil {
			t.Fatalf("swapped stage was deleted: %v", err)
		}
	})
	t.Run("committed stage is absent and sealed remains", func(t *testing.T) {
		t.Parallel()
		fixture := prepareExistingDirectoryFixture(t)
		if _, err := PublishExistingDirectory(fixture.publishRequest()); err != nil {
			t.Fatalf("publish before cleanup: %v", err)
		}
		outcome, err := CleanupExistingDirectoryStaging(fixture.cleanupRequest())
		if err != nil || outcome != ExistingDirectoryCleanupAbsent {
			t.Fatalf("committed cleanup outcome=%s err=%v", outcome, err)
		}
		if _, err := VerifyExistingDirectory(fixture.verifyRequest()); err != nil {
			t.Fatalf("cleanup touched sealed final: %v", err)
		}
	})
}

func TestExistingDirectoryPublicationRequiresPreparedPersistentReceipt(t *testing.T) {
	t.Parallel()
	t.Run("same-shape staging swap", func(t *testing.T) {
		t.Parallel()
		fixture := prepareExistingDirectoryFixture(t)
		original := fixture.stagePath + "-original"
		if err := os.Rename(fixture.stagePath, original); err != nil {
			t.Fatalf("move original staging for publication swap: %v", err)
		}
		foreignReceipt, err := PrepareExistingDirectoryStaging(ExistingDirectoryStagingRequest{
			ParentPath: fixture.parent, StagingName: fixture.stagingName,
			Inventory: fixture.inventory, ManifestPath: existingDirectoryManifestPath,
			ExpectedManifestSHA256: fixture.manifestSHA256,
		})
		if err != nil || foreignReceipt.IsZero() {
			t.Fatalf("prepare foreign same-shape staging: receipt=%#v err=%v", foreignReceipt, err)
		}
		writeExistingFile(t, fixture.stagePath, existingDirectoryManifestPath, []byte("{\"manifest\":true}\n"))
		writeExistingFile(t, fixture.stagePath, "samples/chromium/result.json", []byte("result\n"))

		if _, err := PublishExistingDirectory(fixture.publishRequest()); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("foreign same-shape staging was not rejected: %v", err)
		}
		assertPathAbsent(t, filepath.Join(fixture.parent, ExistingDirectoryOutputName))
		if bytes, err := os.ReadFile(filepath.Join(fixture.stagePath, existingDirectoryManifestPath)); err != nil || string(bytes) != "{\"manifest\":true}\n" {
			t.Fatalf("rejected foreign staging was mutated: bytes=%q err=%v", bytes, err)
		}
	})

	t.Run("empty and wrong receipts", func(t *testing.T) {
		t.Parallel()
		for name, receipt := range map[string]ExistingDirectoryStagingReceipt{
			"empty": {},
			"wrong": NewExistingDirectoryStagingReceipt([]byte("not-the-prepared-directory")),
		} {
			name, receipt := name, receipt
			t.Run(name, func(t *testing.T) {
				fixture := prepareExistingDirectoryFixture(t)
				request := fixture.publishRequest()
				request.Receipt = receipt
				if _, err := PublishExistingDirectory(request); !errors.Is(err, ErrUnsafe) {
					t.Fatalf("%s receipt was not rejected: %v", name, err)
				}
				assertPathAbsent(t, filepath.Join(fixture.parent, ExistingDirectoryOutputName))
				if _, err := os.Stat(fixture.stagePath); err != nil {
					t.Fatalf("rejected staging was touched: %v", err)
				}
			})
		}
	})
}

func TestExistingDirectoryPrepareReturnsReceiptOnHandleSettlementAmbiguity(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "native-owned-publication-root")
	manifestBytes := []byte("{\"receipt\":true}\n")
	digest := sha256.Sum256(manifestBytes)
	inventory := ExistingDirectoryInventory{Directories: []string{}, Files: []ExistingDirectoryFile{{
		RelativePath: existingDirectoryManifestPath,
		ByteLength:   uint64(len(manifestBytes)),
		SHA256:       hex.EncodeToString(digest[:]),
	}}}
	stagingName := ".browser-evidence-upload-22222222222222222222222222222222"
	owner := publisher{
		openPrivate:           openPrivateNativePlatform,
		prepareSettlementHook: func() error { return errors.New("injected close settlement failure") },
	}
	receipt, err := owner.prepareExistingDirectoryStaging(ExistingDirectoryStagingRequest{
		ParentPath: parent, StagingName: stagingName, Inventory: inventory,
		ManifestPath:           existingDirectoryManifestPath,
		ExpectedManifestSHA256: hex.EncodeToString(digest[:]),
	})
	if !errors.Is(err, ErrUnsafe) || receipt.IsZero() {
		t.Fatalf("ambiguous prepare did not retain receipt: receipt=%#v err=%v", receipt, err)
	}
	outcome, cleanupErr := CleanupExistingDirectoryStaging(ExistingDirectoryCleanupRequest{
		ParentPath: parent, StagingName: stagingName, Inventory: inventory,
		ManifestPath:           existingDirectoryManifestPath,
		ExpectedManifestSHA256: hex.EncodeToString(digest[:]),
		Receipt:                receipt,
	})
	if cleanupErr != nil || outcome != ExistingDirectoryCleanupCompleted {
		t.Fatalf("receipt did not recover ambiguous preparation: outcome=%s err=%v", outcome, cleanupErr)
	}
}

func TestExistingDirectoryPrepareOwnsMissingPublicationParent(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "native-owned-publication-root")
	fixture := prepareExistingDirectoryFixtureAt(t, parent)

	outcome, err := CleanupExistingDirectoryStaging(fixture.cleanupRequest())
	if err != nil || outcome != ExistingDirectoryCleanupCompleted {
		t.Fatalf("cleanup native-owned publication staging: outcome=%s err=%v", outcome, err)
	}
	assertPathAbsent(t, fixture.stagePath)
	if metadata, err := os.Stat(parent); err != nil || !metadata.IsDir() {
		t.Fatalf("native-owned publication root is unavailable after cleanup: metadata=%v err=%v", metadata, err)
	}
}

type existingDirectoryFixture struct {
	parent         string
	stagePath      string
	stagingName    string
	manifestSHA256 string
	inventory      ExistingDirectoryInventory
	receipt        ExistingDirectoryStagingReceipt
}

func prepareExistingDirectoryFixture(t *testing.T) existingDirectoryFixture {
	t.Helper()
	return prepareExistingDirectoryFixtureAt(
		t,
		filepath.Join(t.TempDir(), "native-owned-publication-root"),
	)
}

func prepareExistingDirectoryFixtureAt(t *testing.T, parent string) existingDirectoryFixture {
	t.Helper()
	stagingName := ".browser-evidence-upload-0123456789abcdef0123456789abcdef"
	directories := []string{"samples", "samples/chromium"}
	manifestBytes := []byte("{\"manifest\":true}\n")
	resultBytes := []byte("result\n")
	manifestDigest := sha256.Sum256(manifestBytes)
	resultDigest := sha256.Sum256(resultBytes)
	files := []ExistingDirectoryFile{
		{RelativePath: existingDirectoryManifestPath, ByteLength: uint64(len(manifestBytes)), SHA256: hex.EncodeToString(manifestDigest[:])},
		{RelativePath: "samples/chromium/result.json", ByteLength: uint64(len(resultBytes)), SHA256: hex.EncodeToString(resultDigest[:])},
	}
	slices.SortFunc(files, compareExistingFiles)
	inventory := ExistingDirectoryInventory{Directories: directories, Files: files}
	receipt, err := PrepareExistingDirectoryStaging(ExistingDirectoryStagingRequest{
		ParentPath: parent, StagingName: stagingName, Inventory: inventory,
		ManifestPath:           existingDirectoryManifestPath,
		ExpectedManifestSHA256: hex.EncodeToString(manifestDigest[:]),
	})
	if err != nil {
		t.Fatalf("prepare native private staging: %v", err)
	}
	stagePath := filepath.Join(parent, stagingName)
	writeExistingFile(t, stagePath, existingDirectoryManifestPath, manifestBytes)
	writeExistingFile(t, stagePath, "samples/chromium/result.json", resultBytes)
	manifest := findExistingFile(files, existingDirectoryManifestPath)
	return existingDirectoryFixture{
		parent: parent, stagePath: stagePath, stagingName: stagingName,
		manifestSHA256: manifest.SHA256,
		inventory:      inventory,
		receipt:        receipt,
	}
}

func (fixture existingDirectoryFixture) cleanupRequest() ExistingDirectoryCleanupRequest {
	return ExistingDirectoryCleanupRequest{
		ParentPath:             fixture.parent,
		StagingName:            fixture.stagingName,
		Inventory:              fixture.inventory,
		ManifestPath:           existingDirectoryManifestPath,
		ExpectedManifestSHA256: fixture.manifestSHA256,
		Receipt:                fixture.receipt,
	}
}

func (fixture existingDirectoryFixture) publishRequest() ExistingDirectoryRequest {
	return ExistingDirectoryRequest{
		ParentPath:             fixture.parent,
		OutputName:             ExistingDirectoryOutputName,
		StagingName:            fixture.stagingName,
		Inventory:              fixture.inventory,
		ManifestPath:           existingDirectoryManifestPath,
		ExpectedManifestSHA256: fixture.manifestSHA256,
		SnapshotPaths:          []string{"samples/chromium/result.json"},
		Receipt:                fixture.receipt,
	}
}

func (fixture existingDirectoryFixture) verifyRequest() ExistingDirectoryVerificationRequest {
	return ExistingDirectoryVerificationRequest{
		ParentPath:             fixture.parent,
		OutputName:             ExistingDirectoryOutputName,
		Inventory:              fixture.inventory,
		ManifestPath:           existingDirectoryManifestPath,
		ExpectedManifestSHA256: fixture.manifestSHA256,
		SnapshotPaths:          []string{"samples/chromium/result.json"},
	}
}

func writeExistingFile(t *testing.T, root, relative string, bytes []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(relative)), bytes, 0o600); err != nil {
		t.Fatalf("write %s: %v", relative, err)
	}
}

func existingFile(t *testing.T, root, relative string) ExistingDirectoryFile {
	t.Helper()
	bytes, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	digest := sha256.Sum256(bytes)
	return ExistingDirectoryFile{RelativePath: relative, ByteLength: uint64(len(bytes)), SHA256: hex.EncodeToString(digest[:])}
}

func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %s is present or ambiguous: %v", path, err)
	}
}

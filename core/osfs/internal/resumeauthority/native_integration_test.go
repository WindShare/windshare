//go:build windows || linux

package resumeauthority_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer"
)

const (
	nativeResumeFileSize         = 64
	nativeResumeCheckpointSuffix = ".checkpoint"
)

func TestNativeResumeAuthorityListsFreshRootWithoutCreatingStateOrRunningFeatureProbe(t *testing.T) {
	rootFixture := testoutputroot.New(t)
	if err := os.Mkdir(rootFixture.RootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := openNativeTestPlatform(rootFixture.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	rootBinding, err := platform.RootBinding()
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	certification, err := checkpointmodel.NewCertificationID(string(platform.Certification()))
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	store, err := resumeauthority.NewNativeResumeRepository(resumeauthority.NativeResumeConfig{
		Root: platform.Root(), BackendID: transfer.NativeFilesystemOutputBackendID,
		Certification: certification, RootIdentity: rootBinding.Bytes(),
	})
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	observer, err := directoryauthority.NewPublicationObserver(platform)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	native, err := resumeauthority.NewNativeRepository(store, observer, platform.Close)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	inventory, err := resumeauthority.List(context.Background(), native)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Summaries()) != 0 {
		t.Fatalf("fresh-root summaries = %+v", inventory.Summaries())
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(rootFixture.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("read-only inventory created root entries: %+v", entries)
	}
}

func TestNativeResumeAuthorityPreservesPublishedFinalAndRetiresOwnedState(t *testing.T) {
	fixture := createNativeResumeFixture(t, true)
	inventory := fixture.list(t)

	result, err := resumeauthority.Discard(
		context.Background(), inventory.Summaries()[0].Reference(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status() != resumeauthority.Discarded || result.RemovedArtifacts() != 3 {
		t.Fatalf("discard result = %v/%d", result.Status(), result.RemovedArtifacts())
	}
	if actual, err := os.ReadFile(fixture.finalPath); err != nil || !bytes.Equal(actual, fixture.payload) {
		t.Fatalf("published final changed: bytes=%x err=%v", actual, err)
	}
	fixture.assertOwnedArtifacts(t, false)
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}

	// The intent shell is still listed, but a second live capability observes no
	// records and settles idempotently without manufacturing deletion authority.
	inventory = fixture.list(t)
	result, err = resumeauthority.Discard(
		context.Background(), inventory.Summaries()[0].Reference(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status() != resumeauthority.AlreadyAbsent || result.RemovedArtifacts() != 0 {
		t.Fatalf("second discard result = %v/%d", result.Status(), result.RemovedArtifacts())
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeResumeAuthorityPreservesReplacementAndAllUncertainState(t *testing.T) {
	fixture := createNativeResumeFixture(t, true)
	inventory := fixture.list(t)
	replacement := bytes.Repeat([]byte{0x9d}, nativeResumeFileSize)
	if err := os.Remove(fixture.finalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.finalPath, replacement, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := resumeauthority.Discard(
		context.Background(), inventory.Summaries()[0].Reference(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status() != resumeauthority.DiscardNeedsAttention || result.RemovedArtifacts() != 0 ||
		!hasNativeAttention(result.Attention(), resumeauthority.AttentionReplacement) {
		t.Fatalf("replacement discard result = %+v", result)
	}
	if actual, err := os.ReadFile(fixture.finalPath); err != nil || !bytes.Equal(actual, replacement) {
		t.Fatalf("replacement final changed: bytes=%x err=%v", actual, err)
	}
	fixture.assertOwnedArtifacts(t, true)
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeResumeAuthorityNeverAdoptsOrReplacesForeignFinal(t *testing.T) {
	fixture := createNativeResumeFixture(t, false)
	inventory := fixture.list(t)
	foreign := bytes.Repeat([]byte{0x8c}, nativeResumeFileSize)
	if err := os.Mkdir(filepath.Dir(fixture.finalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.finalPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := resumeauthority.Discard(
		context.Background(), inventory.Summaries()[0].Reference(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status() != resumeauthority.DiscardNeedsAttention || result.RemovedArtifacts() != 0 ||
		!hasNativeAttention(result.Attention(), resumeauthority.AttentionAmbiguousPublication) {
		t.Fatalf("foreign-final discard result = %+v", result)
	}
	if actual, err := os.ReadFile(fixture.finalPath); err != nil || !bytes.Equal(actual, foreign) {
		t.Fatalf("foreign final changed: bytes=%x err=%v", actual, err)
	}
	fixture.assertOwnedArtifacts(t, true)
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeResumeAuthorityDoesNotFollowPublicParentOutsideRoot(t *testing.T) {
	fixture := createNativeResumeFixture(t, true)
	inventory := fixture.list(t)
	outside := t.TempDir()
	moved := filepath.Join(outside, "moved-publication")
	if err := os.Rename(filepath.Dir(fixture.finalPath), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Dir(fixture.finalPath)); err != nil {
		// A replaced in-root directory still exercises the same fail-closed parent
		// boundary on hosts that deny unprivileged Windows symlink creation.
		if err := os.Mkdir(filepath.Dir(fixture.finalPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixture.finalPath, []byte("foreign"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result, err := resumeauthority.Discard(
		context.Background(), inventory.Summaries()[0].Reference(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status() != resumeauthority.DiscardNeedsAttention || result.RemovedArtifacts() != 0 {
		t.Fatalf("outside-parent discard result = %+v", result)
	}
	if actual, err := os.ReadFile(filepath.Join(moved, filepath.Base(fixture.finalPath))); err != nil || !bytes.Equal(actual, fixture.payload) {
		t.Fatalf("outside publication changed: bytes=%x err=%v", actual, err)
	}
	fixture.assertOwnedArtifacts(t, true)
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeResumeAuthorityBusyDoesNotMutateOwnedState(t *testing.T) {
	fixture := createNativeResumeFixture(t, false)
	inventory := fixture.list(t)
	platform, config := fixture.openCheckpointPlatform(t)
	namespace, err := checkpointstore.OpenNamespace(config)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	lease, err := namespace.AcquireIntent(fixture.intent)
	if err != nil {
		_ = errors.Join(namespace.Close(), platform.Close())
		t.Fatal(err)
	}
	repository, err := lease.OpenExistingRepository()
	if err != nil {
		_ = errors.Join(lease.Close(), namespace.Close(), platform.Close())
		t.Fatal(err)
	}

	_, err = resumeauthority.Discard(context.Background(), inventory.Summaries()[0].Reference())
	if !errors.Is(err, resumeauthority.ErrBusy) {
		t.Fatalf("busy discard error = %v", err)
	}
	fixture.assertOwnedArtifacts(t, true)
	if err := errors.Join(
		repository.Close(), lease.Close(), namespace.Close(), platform.Close(), inventory.Close(),
	); err != nil {
		t.Fatal(err)
	}
}

type nativeResumeFixture struct {
	rootPath   string
	intent     transfer.TransferIntentDigest
	payload    []byte
	stagePath  string
	anchorPath string
	recordPath string
	finalPath  string
}

func createNativeResumeFixture(t *testing.T, published bool) nativeResumeFixture {
	t.Helper()
	rootFixture := testoutputroot.New(t)
	rootPath := rootFixture.RootPath
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := openNativeTestPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	platformOwned := true
	defer func() {
		if platformOwned {
			_ = platform.Close()
		}
	}()
	intent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0x31}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	config, err := certifiedNativeCheckpointConfig(platform)
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := checkpointstore.Initialize(config)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := namespace.AcquireIntent(intent)
	if err != nil {
		t.Fatal(errors.Join(err, namespace.Close()))
	}
	repository, err := lease.OpenOrCreateRepository()
	if err != nil {
		t.Fatal(errors.Join(err, lease.Close(), namespace.Close()))
	}
	store, err := checkpointstore.NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(errors.Join(err, repository.Close(), lease.Close(), namespace.Close()))
	}

	record := nativeResumeRecord(t, intent, config.Ownership.RootIdentity().Bytes(), published)
	payload := bytes.Repeat([]byte{0x4a}, nativeResumeFileSize)
	objectName := hex.EncodeToString(record.OwnedOutputObject().Bytes())
	owned, _, err := store.CreateOwnedFile(
		context.Background(), record.OwnedOutputObject(), nativeResumeFileSize,
	)
	if err != nil || owned == nil {
		t.Fatal(errors.Join(err, repository.Close(), lease.Close(), namespace.Close()))
	}
	stageName := objectName + ".stage"
	if written, err := owned.WriteAt(payload, 0); err != nil || written != len(payload) {
		t.Fatal(errors.Join(err, owned.Close(), repository.Close(), lease.Close(), namespace.Close()))
	}
	if err := owned.Sync(); err != nil {
		t.Fatal(errors.Join(err, owned.Close(), repository.Close(), lease.Close(), namespace.Close()))
	}
	if err := owned.Close(); err != nil {
		t.Fatal(errors.Join(err, repository.Close(), lease.Close(), namespace.Close()))
	}
	anchorName := objectName + ".anchor"

	finalPath := filepath.Join(rootPath, "folder", "final.bin")
	var final outputcap.File
	var publicParent outputcap.Directory
	if published {
		publicParent, err = platform.Root().CreateDirectory("folder", false)
		if err == nil {
			err = platform.Root().Sync()
		}
		if err == nil {
			final, err = store.PublishOwnedNoReplace(
				context.Background(), record.OwnedOutputObject(), nativeResumeFileSize, publicParent, "final.bin",
			)
		}
		if err == nil {
			err = publicParent.Sync()
		}
		if err != nil {
			t.Fatal(errors.Join(
				err, closeNativeFile(final), closeNativeDirectory(publicParent),
				repository.Close(), lease.Close(), namespace.Close(),
			))
		}
	}

	err = repository.Create(record)
	closeErr := errors.Join(
		closeNativeFile(final), closeNativeDirectory(publicParent),
		repository.Close(), lease.Close(), namespace.Close(),
	)
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}
	platformOwned = false

	recordHex := hex.EncodeToString(record.RecordID().Bytes())
	recordName := recordHex + nativeResumeCheckpointSuffix
	privateRoot := filepath.Join(rootPath, checkpointstore.ControlDirectory,
		checkpointstore.CheckpointDirectory, checkpointstore.IntentsDirectory, hex.EncodeToString(intent.Bytes()))
	return nativeResumeFixture{
		rootPath: rootPath, intent: intent, payload: payload,
		stagePath:  filepath.Join(privateRoot, checkpointstore.StagesDirectory, objectName[:2], stageName),
		anchorPath: filepath.Join(privateRoot, checkpointstore.AnchorsDirectory, objectName[:2], anchorName),
		recordPath: filepath.Join(privateRoot, checkpointstore.RecordsDirectory, recordHex[:2], recordName),
		finalPath:  finalPath,
	}
}

func nativeResumeRecord(
	t *testing.T,
	intent transfer.TransferIntentDigest,
	rootIdentity []byte,
	published bool,
) checkpointmodel.Record {
	t.Helper()
	var fileID catalog.FileID
	var revision content.FileRevision
	for index := range fileID {
		fileID[index] = byte(index + 1)
		revision[index] = byte(index + 2)
	}
	phase, commit := checkpointmodel.PhasePaused, checkpointmodel.CommitVerified
	if published {
		phase, commit = checkpointmodel.PhasePublished, checkpointmodel.CommitPublished
	}
	record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		TransferIntentDigest: intent,
		FileID:               fileID,
		FileRevision:         revision,
		CanonicalPath:        "folder/final.bin",
		ExactSize:            nativeResumeFileSize,
		BackendID:            string(transfer.NativeFilesystemOutputBackendID),
		RootIdentity:         rootIdentity,
		OwnedOutputObject:    bytes.Repeat([]byte{0x62}, sha256.Size),
		StateGeneration:      2,
		CheckpointGeneration: 1,
		VerifiedRanges:       []checkpointmodel.Range{{Offset: 0, End: 16}},
		Phase:                phase,
		CommitState:          commit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (fixture nativeResumeFixture) list(t *testing.T) *resumeauthority.Inventory {
	t.Helper()
	platform, config := fixture.openCheckpointPlatform(t)
	store, err := resumeauthority.NewNativeResumeRepository(resumeauthority.NativeResumeConfig{
		Root: config.Root, BackendID: config.Ownership.Backend(), Certification: config.Ownership.Certification(),
		RootIdentity: config.Ownership.RootIdentity().Bytes(),
	})
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	observer, err := directoryauthority.NewPublicationObserver(platform)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	native, err := resumeauthority.NewNativeRepository(store, observer, platform.Close)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	inventory, err := resumeauthority.List(context.Background(), native)
	if err != nil {
		t.Fatal(err)
	}
	if summaries := inventory.Summaries(); len(summaries) != 1 || summaries[0].NeedsAttention() {
		_ = inventory.Close()
		t.Fatalf("resume summaries = %+v", summaries)
	}
	return inventory
}

func (fixture nativeResumeFixture) openCheckpointPlatform(
	t *testing.T,
) (outputcap.Platform, checkpointstore.CertifiedConfig) {
	t.Helper()
	platform, err := openNativeTestPlatform(fixture.rootPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := certifiedNativeCheckpointConfig(platform)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	return platform, config
}

func certifiedNativeCheckpointConfig(platform outputcap.Platform) (checkpointstore.CertifiedConfig, error) {
	if platform == nil || platform.Root() == nil {
		return checkpointstore.CertifiedConfig{}, transfer.ErrInvalidOutputBinding
	}
	rootBinding, err := platform.RootBinding()
	if err != nil {
		return checkpointstore.CertifiedConfig{}, err
	}
	certification, err := checkpointmodel.NewCertificationID(string(platform.Certification()))
	if err != nil {
		return checkpointstore.CertifiedConfig{}, err
	}
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Backend:             transfer.NativeFilesystemOutputBackendID,
		Certification:       certification,
		RootIdentity:        rootBinding.Bytes(),
		RootOpenDisposition: platform.RootOpenDisposition(),
	})
	if err != nil {
		return checkpointstore.CertifiedConfig{}, err
	}
	return checkpointstore.CertifiedConfig{Root: platform.Root(), Ownership: ownership}, nil
}

func (fixture nativeResumeFixture) assertOwnedArtifacts(t *testing.T, present bool) {
	t.Helper()
	for _, path := range []string{fixture.stagePath, fixture.anchorPath, fixture.recordPath} {
		_, err := os.Lstat(path)
		if present && err != nil {
			t.Errorf("owned artifact %s: %v", filepath.Base(path), err)
		}
		if !present && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("owned artifact %s still exists: %v", filepath.Base(path), err)
		}
	}
}

func hasNativeAttention(attention []resumeauthority.Attention, reason resumeauthority.AttentionReason) bool {
	for _, item := range attention {
		if item.Reason() == reason {
			return true
		}
	}
	return false
}

func closeNativeDirectory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

func closeNativeFile(file outputcap.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

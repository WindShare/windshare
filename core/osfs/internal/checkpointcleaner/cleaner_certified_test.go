package checkpointcleaner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/osfs/internal/legacyresume"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func cleanOwnedNamespace(
	ctx context.Context,
	config OneShotCheckpointCleanerConfig,
) (CheckpointCleanupReport, error) {
	cleaner, err := NewOneShotCheckpointCleaner(config)
	if err != nil {
		return CheckpointCleanupReport{}, err
	}
	return cleaner.Run(ctx)
}

func TestCleanerRemovesOnlyOwnedLegacyControlState(t *testing.T) {
	platform, rootPath := newCleanerPlatform(t)
	installCurrentNamespaceSentinels(t, platform)
	legacy := installLegacyNamespace(t, platform)
	published := filepath.Join(rootPath, "published.txt")
	if err := os.WriteFile(published, []byte("published"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := cleanOwnedNamespace(context.Background(), cleanerConfig(platform))
	if err != nil || !report.Complete || report.NeedsAttention() || report.Removed == 0 {
		t.Fatalf("cleanup = %+v, %v", report, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, legacy.session)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy session remains: %v", err)
	}
	if _, err := os.Stat(published); err != nil {
		t.Fatalf("published output was removed: %v", err)
	}
	for _, relative := range []string{
		filepath.Join(legacyresume.ControlDirectory, legacyresume.CheckpointDirectory, legacyresume.CheckpointOwnership),
		filepath.Join(legacyresume.ControlDirectory, legacyresume.CheckpointDirectory, legacyresume.CheckpointLeases),
		filepath.Join(legacyresume.ControlDirectory, legacyresume.CheckpointDirectory, legacyresume.CheckpointIntents),
		filepath.Join(legacyresume.ControlDirectory, legacyresume.CheckpointDirectory, FileCheckpointCleanupState),
		filepath.Join(legacyresume.ControlDirectory, legacyresume.CheckpointDirectory, FileCheckpointCleanupLock),
	} {
		if _, err := os.Stat(filepath.Join(rootPath, relative)); err != nil {
			t.Fatalf("retained state %q = %v", relative, err)
		}
	}
	currentCheckpoint := filepath.Join(
		rootPath, legacyresume.ControlDirectory, legacyresume.CheckpointDirectory,
		legacyresume.CheckpointIntents, "current.checkpoint",
	)
	if _, err := os.Stat(currentCheckpoint); err != nil {
		t.Fatalf("current checkpoint payload was removed: %v", err)
	}
	if !reportHasDetail(report, cleanupDetailPublished) {
		t.Fatalf("published path was not reported: %+v", report.Entries)
	}
}

func TestCleanerRejectsUnknownAndConflictingLegacyPathsWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, outputcap.Platform, legacyFixture) string
		detail string
	}{
		{
			name: "unknown session child",
			mutate: func(t *testing.T, platform outputcap.Platform, fixture legacyFixture) string {
				session := openLegacySession(t, platform, fixture)
				defer session.Close()
				writeCapabilityFile(t, session, "foreign.bin", []byte("foreign"))
				return filepath.Join(fixture.session, "foreign.bin")
			},
			detail: cleanupDetailUnknown,
		},
		{
			name: "conflicting session lock kind",
			mutate: func(t *testing.T, platform outputcap.Platform, fixture legacyFixture) string {
				session := openLegacySession(t, platform, fixture)
				defer session.Close()
				lock, err := session.OpenObservedFile(legacyresume.SessionLock, true)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(session.RemoveFile(legacyresume.SessionLock, lock), lock.Close(), session.Sync()); err != nil {
					t.Fatal(err)
				}
				conflict, err := session.CreateDirectory(legacyresume.SessionLock, true)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(conflict.Sync(), conflict.Close(), session.Sync()); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(fixture.session, legacyresume.SessionLock)
			},
			detail: cleanupDetailConflict,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform, rootPath := newCleanerPlatform(t)
			legacy := installLegacyNamespace(t, platform)
			retained := test.mutate(t, platform, legacy)
			report, err := cleanOwnedNamespace(context.Background(), cleanerConfig(platform))
			if err != nil || !report.NeedsAttention() || report.Complete || report.Removed != 0 {
				t.Fatalf("ambiguous cleanup = %+v, %v", report, err)
			}
			if _, err := os.Stat(filepath.Join(rootPath, retained)); err != nil {
				t.Fatalf("ambiguous path was mutated: %v", err)
			}
			if !reportHasDetail(report, test.detail) {
				t.Fatalf("missing %q report: %+v", test.detail, report.Entries)
			}
		})
	}
}

func TestCleanerRejectsUnknownOwnerAndPreservesLegacyTree(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(*testing.T, outputcap.Platform, []byte) []byte
	}{
		{
			name: "corrupt checksum",
			replace: func(_ *testing.T, _ outputcap.Platform, encoded []byte) []byte {
				encoded[len(encoded)-1] ^= 0xff
				return encoded
			},
		},
		{
			name: "foreign certified root",
			replace: func(t *testing.T, platform outputcap.Platform, _ []byte) []byte {
				return encodeLegacyOwnershipFixture(t, legacyOwnershipFixture{
					Backend:       string(legacyresume.NativeFilesystemBackend),
					RootIdentity:  bytes.Repeat([]byte{0x91}, legacyresume.RootIdentityBytes),
					Certification: string(platform.Certification()), Durability: 1, Generation: 2,
				})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform, rootPath := newCleanerPlatform(t)
			legacy := installLegacyNamespace(t, platform)
			control := openLegacyControl(t, platform)
			encoded, err := readBoundedRecord(control, legacyresume.ControlRecord, legacyresume.MaxOwnershipRecordBytes)
			if err != nil {
				t.Fatal(err)
			}
			encoded = test.replace(t, platform, encoded)
			replaceCapabilityFile(t, control, legacyresume.ControlRecord, encoded)
			if err := control.Close(); err != nil {
				t.Fatal(err)
			}

			report, err := cleanOwnedNamespace(context.Background(), cleanerConfig(platform))
			if err != nil || !report.NeedsAttention() || report.Removed != 0 {
				t.Fatalf("unknown-owner cleanup = %+v, %v", report, err)
			}
			if _, err := os.Stat(filepath.Join(rootPath, legacy.payload)); err != nil {
				t.Fatalf("unknown-owner state was mutated: %v", err)
			}
		})
	}
}

func TestCleanerRefusesActiveLegacyAndMaintenanceLocks(t *testing.T) {
	for _, test := range []struct {
		name string
		lock func(*testing.T, outputcap.Platform, legacyFixture) outputcap.Lock
	}{
		{name: "coordinator", lock: lockLegacyCoordinator},
		{name: "session", lock: lockLegacySession},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform, rootPath := newCleanerPlatform(t)
			legacy := installLegacyNamespace(t, platform)
			lock := test.lock(t, platform, legacy)
			defer lock.Close()
			if _, err := cleanOwnedNamespace(context.Background(), cleanerConfig(platform)); !errors.Is(err, ErrCheckpointCleanerBusy) {
				t.Fatalf("cleanup with active %s = %v", test.name, err)
			}
			if _, err := os.Stat(filepath.Join(rootPath, legacy.payload)); err != nil {
				t.Fatalf("cleanup crossed active %s: %v", test.name, err)
			}
		})
	}

	t.Run("maintenance", func(t *testing.T) {
		platform, rootPath := newCleanerPlatform(t)
		legacy := installLegacyNamespace(t, platform)
		faulted := cleanerConfig(platform)
		faulted.Fault = func(CheckpointCleanupStep) error { return errors.New("pause after cleaner state") }
		_, _ = cleanOwnedNamespace(context.Background(), faulted)
		namespace := openCheckpointNamespace(t, platform)
		defer namespace.Close()
		lock, _, err := namespace.AcquireLock(FileCheckpointCleanupLock, true)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if _, err := cleanOwnedNamespace(context.Background(), cleanerConfig(platform)); !errors.Is(err, ErrCheckpointCleanerBusy) {
			t.Fatalf("cleanup with active maintenance lock = %v", err)
		}
		if _, err := os.Stat(filepath.Join(rootPath, legacy.payload)); err != nil {
			t.Fatalf("cleanup crossed maintenance lock: %v", err)
		}
	})
}

func TestCleanerResumesOnlyItsExactIncompleteState(t *testing.T) {
	platform, rootPath := newCleanerPlatform(t)
	legacy := installLegacyNamespace(t, platform)
	injected := errors.New("injected cleanup interruption")
	faulted := cleanerConfig(platform)
	faulted.Fault = func(step CheckpointCleanupStep) error {
		if step.Index == 0 {
			return injected
		}
		return nil
	}
	if _, err := cleanOwnedNamespace(context.Background(), faulted); !errors.Is(err, injected) {
		t.Fatalf("faulted cleanup = %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, legacy.payload)); err != nil {
		t.Fatalf("fault cut mutated payload: %v", err)
	}

	report, err := cleanOwnedNamespace(context.Background(), cleanerConfig(platform))
	if err != nil || !report.Complete || !report.Resumed || report.Removed == 0 {
		t.Fatalf("resumed cleanup = %+v, %v", report, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, legacy.session)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy session remains after resume: %v", err)
	}
}

func TestCleanerRejectsImpossibleAndNonCanonicalResumeState(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, outputcap.Platform)
	}{
		{
			name: "legacy tree without ownership record",
			mutate: func(t *testing.T, platform outputcap.Platform) {
				control := openLegacyControl(t, platform)
				defer control.Close()
				entry, err := control.OpenEntry(legacyresume.ControlRecord)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(control.RemoveEntry(legacyresume.ControlRecord, entry), entry.Close(), control.Sync()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "non-canonical cleaner state",
			mutate: func(t *testing.T, platform outputcap.Platform) {
				namespace := openCheckpointNamespace(t, platform)
				defer namespace.Close()
				encoded, err := readBoundedRecord(namespace, FileCheckpointCleanupState, maxCleanerStateBytes)
				if err != nil {
					t.Fatal(err)
				}
				replaceCapabilityFile(t, namespace, FileCheckpointCleanupState, append(encoded, ' '))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform, rootPath := newCleanerPlatform(t)
			legacy := installLegacyNamespace(t, platform)
			injected := errors.New("stop before first mutation")
			faulted := cleanerConfig(platform)
			faulted.Fault = func(CheckpointCleanupStep) error { return injected }
			if _, err := cleanOwnedNamespace(context.Background(), faulted); !errors.Is(err, injected) {
				t.Fatalf("create incomplete state = %v", err)
			}
			test.mutate(t, platform)

			report, err := cleanOwnedNamespace(context.Background(), cleanerConfig(platform))
			if test.name == "non-canonical cleaner state" {
				if !errors.Is(err, ErrCheckpointCleanerState) {
					t.Fatalf("non-canonical state error = %v", err)
				}
			} else if err != nil || !report.NeedsAttention() || report.Removed != 0 {
				t.Fatalf("impossible resume report = %+v, %v", report, err)
			}
			if _, err := os.Stat(filepath.Join(rootPath, legacy.payload)); err != nil {
				t.Fatalf("invalid resume state mutated legacy payload: %v", err)
			}
		})
	}
}

func TestCleanerRevalidatesOwnershipProofImmediatelyBeforeMutation(t *testing.T) {
	platform, rootPath := newCleanerPlatform(t)
	legacy := installLegacyNamespace(t, platform)
	faulted := cleanerConfig(platform)
	replaced := false
	faulted.Fault = func(CheckpointCleanupStep) error {
		if replaced {
			return nil
		}
		replaced = true
		control := openLegacyControl(t, platform)
		defer control.Close()
		foreign := encodeLegacyOwnershipFixture(t, legacyOwnershipFixture{
			Backend:       string(legacyresume.NativeFilesystemBackend),
			RootIdentity:  bytes.Repeat([]byte{0x72}, legacyresume.RootIdentityBytes),
			Certification: string(platform.Certification()), Durability: 1, Generation: 4,
		})
		replaceCapabilityFile(t, control, legacyresume.ControlRecord, foreign)
		return nil
	}
	report, err := cleanOwnedNamespace(context.Background(), faulted)
	if !errors.Is(err, ErrCheckpointCleanerOwnership) || report.Removed != 0 {
		t.Fatalf("replaced ownership proof cleanup = %+v, %v", report, err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, legacy.payload)); err != nil {
		t.Fatalf("ownership proof replacement mutated payload: %v", err)
	}
}

func TestCleanerNeverDeletesAPathIntroducedAfterClassification(t *testing.T) {
	platform, rootPath := newCleanerPlatform(t)
	legacy := installLegacyNamespace(t, platform)
	faulted := cleanerConfig(platform)
	introduced := false
	faulted.Fault = func(CheckpointCleanupStep) error {
		if introduced {
			return nil
		}
		introduced = true
		session := openLegacySession(t, platform, legacy)
		defer session.Close()
		writeCapabilityFile(t, session, "introduced-after-scan", []byte("must survive"))
		return nil
	}
	if _, err := cleanOwnedNamespace(context.Background(), faulted); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("changed cleanup tree error = %v", err)
	}
	introducedPath := filepath.Join(rootPath, legacy.session, "introduced-after-scan")
	if encoded, err := os.ReadFile(introducedPath); err != nil || string(encoded) != "must survive" {
		t.Fatalf("introduced path was not preserved: %q, %v", encoded, err)
	}
}

func TestCleanerReportsCurrentCheckpointStateInsteadOfDeletingIt(t *testing.T) {
	platform, rootPath := newCleanerPlatform(t)
	legacy := installLegacyNamespace(t, platform)
	intent := openLegacyIntent(t, platform, legacy)
	current, err := intent.CreateDirectory(legacyresume.CheckpointDirectory, true)
	if err != nil {
		t.Fatal(err)
	}
	writeCapabilityFile(t, current, "current.checkpoint", []byte("current"))
	if err := errors.Join(current.Close(), intent.Close()); err != nil {
		t.Fatal(err)
	}

	report, err := cleanOwnedNamespace(context.Background(), cleanerConfig(platform))
	if err != nil || !report.NeedsAttention() || report.Removed != 0 || !reportHasDetail(report, cleanupDetailSeparateOwnership) {
		t.Fatalf("current-state cleanup = %+v, %v", report, err)
	}
	currentPath := filepath.Join(rootPath, legacy.intent, legacyresume.CheckpointDirectory, "current.checkpoint")
	if _, err := os.Stat(currentPath); err != nil {
		t.Fatalf("current checkpoint was mutated: %v", err)
	}
}

type legacyFixture struct {
	intent  string
	session string
	payload string
}

func newCleanerPlatform(t *testing.T) (outputcap.Platform, string) {
	t.Helper()
	fixture := testoutputroot.New(t)
	platform, err := openCleanerTestPlatform(fixture.RootPath, fixture.CreateRoot)
	if err != nil {
		t.Skipf("native output platform unavailable: %v", err)
	}
	t.Cleanup(func() { _ = platform.Close() })
	return platform, fixture.RootPath
}

func cleanerConfig(platform outputcap.Platform) OneShotCheckpointCleanerConfig {
	return OneShotCheckpointCleanerConfig{
		Platform: platform, BackendID: legacyresume.NativeFilesystemBackend,
	}
}

func installCurrentNamespaceSentinels(t *testing.T, platform outputcap.Platform) {
	t.Helper()
	control := ensureTestDirectory(t, platform.Root(), legacyresume.ControlDirectory)
	defer control.Close()
	namespace := ensureTestDirectory(t, control, legacyresume.CheckpointDirectory)
	defer namespace.Close()
	writeCapabilityFile(t, namespace, legacyresume.CheckpointOwnership, []byte("current marker sentinel"))
	for _, name := range []string{legacyresume.CheckpointLeases, legacyresume.CheckpointIntents} {
		directory := ensureTestDirectory(t, namespace, name)
		if name == legacyresume.CheckpointIntents {
			writeCapabilityFile(t, directory, "current.checkpoint", []byte("current checkpoint sentinel"))
		}
		if err := directory.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func installLegacyNamespace(t *testing.T, platform outputcap.Platform) legacyFixture {
	t.Helper()
	control := ensureTestDirectory(t, platform.Root(), legacyresume.ControlDirectory)
	defer control.Close()
	binding, err := platform.RootBinding()
	if err != nil {
		t.Fatal(err)
	}
	encoded := encodeLegacyOwnershipFixture(t, legacyOwnershipFixture{
		Backend: string(legacyresume.NativeFilesystemBackend), RootIdentity: binding.Bytes(),
		Certification: string(platform.Certification()), Durability: 1, Generation: 1,
	})
	writeCapabilityFile(t, control, legacyresume.ControlRecord, encoded)
	installUnlockedLock(t, control, legacyresume.CoordinatorLock)
	sessions := ensureTestDirectory(t, control, legacyresume.SessionsDirectory)
	defer sessions.Close()
	intentName := strings.Repeat("a", 64)
	intent := ensureTestDirectory(t, sessions, intentName)
	defer intent.Close()
	sessionName := strings.Repeat("b", 32)
	session := ensureTestDirectory(t, intent, sessionName)
	defer session.Close()
	installUnlockedLock(t, session, legacyresume.SessionLock)
	writeCapabilityFile(t, session, legacyresume.HeaderRecord, []byte("opaque retired header"))
	for _, directoryName := range []string{
		legacyresume.FilesDirectory, legacyresume.AnchorsDirectory, legacyresume.StagesDirectory,
	} {
		directory := ensureTestDirectory(t, session, directoryName)
		shard := ensureTestDirectory(t, directory, "aa")
		base := strings.Repeat("a", 64)
		suffix := ".state"
		switch directoryName {
		case legacyresume.AnchorsDirectory:
			suffix = ".anchor"
		case legacyresume.StagesDirectory:
			suffix = ".stage"
		}
		writeCapabilityFile(t, shard, base+suffix, []byte("owned legacy state"))
		if err := errors.Join(shard.Close(), directory.Close()); err != nil {
			t.Fatal(err)
		}
	}
	base := filepath.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory, intentName)
	sessionPath := filepath.Join(base, sessionName)
	payload := filepath.Join(sessionPath, legacyresume.StagesDirectory, "aa", strings.Repeat("a", 64)+".stage")
	return legacyFixture{intent: base, session: sessionPath, payload: payload}
}

func openLegacyControl(t *testing.T, platform outputcap.Platform) outputcap.Directory {
	t.Helper()
	control, err := platform.Root().OpenDirectory(legacyresume.ControlDirectory, true)
	if err != nil {
		t.Fatal(err)
	}
	return control
}

func openLegacyIntent(t *testing.T, platform outputcap.Platform, fixture legacyFixture) outputcap.Directory {
	t.Helper()
	control := openLegacyControl(t, platform)
	defer control.Close()
	sessions, err := control.OpenDirectory(legacyresume.SessionsDirectory, true)
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	intent, err := sessions.OpenDirectory(filepath.Base(fixture.intent), true)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func openLegacySession(t *testing.T, platform outputcap.Platform, fixture legacyFixture) outputcap.Directory {
	t.Helper()
	intent := openLegacyIntent(t, platform, fixture)
	defer intent.Close()
	session, err := intent.OpenDirectory(filepath.Base(fixture.session), true)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func openCheckpointNamespace(t *testing.T, platform outputcap.Platform) outputcap.Directory {
	t.Helper()
	control := openLegacyControl(t, platform)
	defer control.Close()
	namespace, err := control.OpenDirectory(legacyresume.CheckpointDirectory, true)
	if err != nil {
		t.Fatal(err)
	}
	return namespace
}

func ensureTestDirectory(t *testing.T, parent outputcap.Directory, name string) outputcap.Directory {
	t.Helper()
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		t.Fatal(err)
	}
	if kind != outputcap.EntryAbsent {
		if !exact || kind != outputcap.EntryDirectory {
			t.Fatalf("test directory %q conflicts", name)
		}
		directory, err := parent.OpenDirectory(name, true)
		if err != nil {
			t.Fatal(err)
		}
		return directory
	}
	directory, err := parent.CreateDirectory(name, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(directory.Sync(), parent.Sync()); err != nil {
		t.Fatal(err)
	}
	return directory
}

func installUnlockedLock(t *testing.T, directory outputcap.Directory, name string) {
	t.Helper()
	lock, _, err := directory.AcquireLock(name, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(directory.Sync(), lock.Close()); err != nil {
		t.Fatal(err)
	}
}

func writeCapabilityFile(t *testing.T, directory outputcap.Directory, name string, payload []byte) {
	t.Helper()
	file, err := directory.CreateFile(name, true, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	written, writeErr := file.WriteAt(payload, 0)
	if writeErr == nil && written != len(payload) {
		writeErr = errors.New("short test fixture write")
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close(), directory.Sync()); err != nil {
		t.Fatal(err)
	}
}

func replaceCapabilityFile(t *testing.T, directory outputcap.Directory, name string, payload []byte) {
	t.Helper()
	file, err := directory.CreateFile("replacement", true, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	written, err := file.WriteAt(payload, 0)
	if err != nil || written != len(payload) {
		t.Fatalf("write replacement = %d, %v", written, err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := directory.ReplacePrivateFile(file, name); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(directory.Sync(), file.Close()); err != nil {
		t.Fatal(err)
	}
}

func lockLegacyCoordinator(t *testing.T, platform outputcap.Platform, _ legacyFixture) outputcap.Lock {
	t.Helper()
	control := openLegacyControl(t, platform)
	defer control.Close()
	lock, created, err := control.AcquireLock(legacyresume.CoordinatorLock, true)
	if err != nil || created {
		t.Fatalf("acquire coordinator fixture: created=%t err=%v", created, err)
	}
	return lock
}

func lockLegacySession(t *testing.T, platform outputcap.Platform, fixture legacyFixture) outputcap.Lock {
	t.Helper()
	session := openLegacySession(t, platform, fixture)
	defer session.Close()
	lock, created, err := session.AcquireLock(legacyresume.SessionLock, true)
	if err != nil || created {
		t.Fatalf("acquire session fixture: created=%t err=%v", created, err)
	}
	return lock
}

func reportHasDetail(report CheckpointCleanupReport, detail string) bool {
	for _, entry := range report.Entries {
		if entry.Detail == detail || strings.Contains(entry.Detail, detail) {
			return true
		}
	}
	return false
}

type legacyOwnershipFixture struct {
	Schema        uint32 `cbor:"0,keyasint"`
	Backend       string `cbor:"1,keyasint"`
	RootIdentity  []byte `cbor:"2,keyasint"`
	Certification string `cbor:"3,keyasint"`
	Durability    uint8  `cbor:"4,keyasint"`
	Generation    uint64 `cbor:"5,keyasint"`
}

func encodeLegacyOwnershipFixture(t *testing.T, fixture legacyOwnershipFixture) []byte {
	t.Helper()
	fixture.Schema = 1
	options := cbor.CoreDetEncOptions()
	options.NilContainers = cbor.NilContainerAsEmpty
	encoder, err := options.EncMode()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := encoder.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	magic := []byte{'W', 'S', 'O', 'C', 'T', 'L', '0', '1'}
	encoded := append([]byte(nil), magic...)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	encoded = append(encoded, length[:]...)
	encoded = append(encoded, payload...)
	checksum := sha256.Sum256(encoded)
	return append(encoded, checksum[:]...)
}

func TestLegacyOwnershipFixtureIsBounded(t *testing.T) {
	if len(encodeLegacyOwnershipFixture(t, legacyOwnershipFixture{
		Backend:       string(legacyresume.NativeFilesystemBackend),
		RootIdentity:  bytes.Repeat([]byte{1}, legacyresume.RootIdentityBytes),
		Certification: legacyresume.CertificationWindowsNTFSProcessRestart,
		Durability:    1, Generation: 1,
	})) > legacyresume.MaxOwnershipRecordBytes {
		t.Fatal("legacy ownership fixture exceeds decoder bound")
	}
}

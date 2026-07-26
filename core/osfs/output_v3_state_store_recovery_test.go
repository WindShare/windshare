package osfs

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3InitialStateRecordRecoversEveryAtomicCreationCut(t *testing.T) {
	encoded := []byte("authoritative state envelope")
	tests := []struct {
		name         string
		written      int
		fileSynced   bool
		targetLinked bool
		parentSynced bool
	}{
		{name: "temporary-created"},
		{name: "temporary-partially-written", written: len(encoded) / 2},
		{name: "temporary-completely-written", written: len(encoded)},
		{name: "temporary-synced", written: len(encoded), fileSynced: true},
		{name: "target-linked", written: len(encoded), fileSynced: true, targetLinked: true},
		{name: "target-parent-synced", written: len(encoded), fileSynced: true, targetLinked: true, parentSynced: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			platform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			directoryName := "state-record-cut"
			directory, err := platform.Root().CreateDirectory(directoryName, true)
			if err != nil {
				t.Fatal(err)
			}
			store := outputStateStore{random: bytes.NewReader(bytes.Repeat([]byte{0x91}, 256))}
			targetName := "header.state"
			temporaryName, err := store.temporaryName(targetName)
			if err != nil {
				t.Fatal(err)
			}
			temporary, err := directory.CreateFile(temporaryName, true, int64(len(encoded)))
			if err != nil {
				t.Fatal(err)
			}
			if test.written != 0 {
				written, err := temporary.WriteAt(encoded[:test.written], 0)
				if err != nil || written != test.written {
					t.Fatalf("write cut = (%d, %v), want %d", written, err, test.written)
				}
			}
			if test.fileSynced {
				if err := temporary.Sync(); err != nil {
					t.Fatal(err)
				}
			}
			if test.targetLinked {
				target, err := directory.LinkFileNoReplace(temporary, targetName)
				if err != nil {
					t.Fatal(err)
				}
				if err := target.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if test.parentSynced {
				if err := directory.Sync(); err != nil {
					t.Fatal(err)
				}
			}
			if err := errors.Join(temporary.Close(), directory.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}

			reopenedPlatform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := reopenedPlatform.Root().OpenDirectory(directoryName, true)
			if err != nil {
				t.Fatal(err)
			}
			recoveryStore := outputStateStore{random: bytes.NewReader(bytes.Repeat([]byte{0xa2}, 256))}
			if _, err := recoveryStore.ensureInitialRecord(reopened, targetName, encoded, len(encoded)); err != nil {
				t.Fatal(err)
			}
			actual, err := readStateRecord(reopened, targetName, len(encoded))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual, encoded) {
				t.Fatalf("recovered state = %q, want %q", actual, encoded)
			}
			names, err := reopened.Names(3)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(names, []string{targetName}) {
				t.Fatalf("state namespace after recovery = %v, want only %q", names, targetName)
			}
			if err := errors.Join(reopened.Close(), reopenedPlatform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3StateReplacementClassifiesEveryAtomicInstallCut(t *testing.T) {
	current, next, divergentRetry := stateStoreHeaderImages(t)
	tests := []struct {
		name        string
		fault       stateStoreFaultPoint
		wantOutcome outputStateReplaceOutcome
		wantBytes   []byte
		wantError   bool
	}{
		{name: "create", fault: stateStoreFaultCreate, wantOutcome: outputStateReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "write", fault: stateStoreFaultWrite, wantOutcome: outputStateReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "file-sync", fault: stateStoreFaultFileSync, wantOutcome: outputStateReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "temporary-reopen", fault: stateStoreFaultTemporaryReopen, wantOutcome: outputStateReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "temporary-byte-verify", fault: stateStoreFaultTemporaryRead, wantOutcome: outputStateReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "current-target-reopen", fault: stateStoreFaultCurrentReopen, wantOutcome: outputStateReplaceUncertain, wantBytes: current.encoded, wantError: true},
		{name: "replace-before-mutation", fault: stateStoreFaultReplaceBeforeMutation, wantOutcome: outputStateReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "replace-succeeded-before-error", fault: stateStoreFaultReplaceAfterMutation, wantOutcome: outputStateReplaceAdopted, wantBytes: next.encoded},
		{name: "parent-sync", fault: stateStoreFaultParentSync, wantOutcome: outputStateReplaceAdopted, wantBytes: next.encoded},
		{name: "installed-target-reopen", fault: stateStoreFaultInstalledReopen, wantOutcome: outputStateReplaceUncertain, wantBytes: next.encoded, wantError: true},
		{name: "installed-target-diverged", fault: stateStoreFaultInstalledDivergent, wantOutcome: outputStateReplaceUncertain, wantBytes: next.encoded, wantError: true},
		{name: "current-target-close", fault: stateStoreFaultCurrentClose, wantOutcome: outputStateReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "installed-target-close", fault: stateStoreFaultInstalledClose, wantOutcome: outputStateReplaceAdopted, wantBytes: next.encoded, wantError: true},
		{name: "complete", wantOutcome: outputStateReplaceAdopted, wantBytes: next.encoded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			platform, directory := stateStoreReplacementFixture(t, current.encoded)
			defer func() {
				if err := errors.Join(directory.Close(), platform.Close()); err != nil {
					t.Error(err)
				}
			}()
			faults := &stateStoreFaultDirectory{
				outputV3Directory: directory,
				fault:             test.fault,
				target:            resumestate.HeaderRecordName,
				divergent:         divergentRetry.encoded,
			}
			store := outputStateStore{random: bytes.NewReader(bytes.Repeat([]byte{0xb3}, 256))}
			outcome, err := store.replaceRecord(
				faults, resumestate.HeaderRecordName, current, next, resumestate.MaxSessionHeaderBytes,
			)
			if outcome != test.wantOutcome || (err != nil) != test.wantError {
				t.Fatalf("replace outcome = %d, %v; want %d, error=%t", outcome, err, test.wantOutcome, test.wantError)
			}
			actual, err := readStateRecord(directory, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
			if err != nil || !bytes.Equal(actual, test.wantBytes) {
				t.Fatalf("installed bytes = %q, %v; want %q", actual, err, test.wantBytes)
			}
		})
	}
}

func TestOutputV3StateReplacementRejectsSameGenerationAndStaleAuthority(t *testing.T) {
	current, next, divergentRetry := stateStoreHeaderImages(t)
	platform, directory := stateStoreReplacementFixture(t, current.encoded)
	defer func() {
		if err := errors.Join(directory.Close(), platform.Close()); err != nil {
			t.Error(err)
		}
	}()
	store := outputStateStore{random: bytes.NewReader(bytes.Repeat([]byte{0xc4}, 512))}

	sameGeneration := current
	outcome, err := store.replaceRecord(
		directory, resumestate.HeaderRecordName, current, sameGeneration, resumestate.MaxSessionHeaderBytes,
	)
	if outcome != outputStateReplaceUnchanged || !errors.Is(err, resumestate.ErrInvalidTransition) {
		t.Fatalf("same-generation replacement = %d, %v", outcome, err)
	}
	mislabeled := next
	mislabeled.generation++
	outcome, err = store.replaceRecord(
		directory, resumestate.HeaderRecordName, current, mislabeled, resumestate.MaxSessionHeaderBytes,
	)
	if outcome != outputStateReplaceUnchanged || !errors.Is(err, resumestate.ErrInvalidState) {
		t.Fatalf("mislabeled replacement image = %d, %v", outcome, err)
	}

	outcome, err = store.replaceRecord(
		directory, resumestate.HeaderRecordName, current, next, resumestate.MaxSessionHeaderBytes,
	)
	if outcome != outputStateReplaceAdopted || err != nil {
		t.Fatalf("first replacement = %d, %v", outcome, err)
	}
	outcome, err = store.replaceRecord(
		directory, resumestate.HeaderRecordName, current, divergentRetry, resumestate.MaxSessionHeaderBytes,
	)
	if outcome != outputStateReplaceUncertain || err == nil {
		t.Fatalf("stale-authority replacement = %d, %v", outcome, err)
	}
	actual, err := readStateRecord(directory, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
	if err != nil || !bytes.Equal(actual, next.encoded) {
		t.Fatalf("stale retry changed installed generation: %q, %v", actual, err)
	}
}

func TestOutputV3StateCreationFailureCutsPreserveARecoverableImage(t *testing.T) {
	encoded := []byte("authoritative state envelope")
	for _, test := range []struct {
		name       string
		fault      stateStoreFaultPoint
		badRandom  bool
		targetLive bool
		settled    bool
	}{
		{name: "nonce-source", badRandom: true},
		{name: "temporary-collision-budget", fault: stateStoreFaultCreateCollision},
		{name: "create", fault: stateStoreFaultCreate},
		{name: "write", fault: stateStoreFaultWrite},
		{name: "short-write", fault: stateStoreFaultShortWrite},
		{name: "file-sync", fault: stateStoreFaultFileSync},
		{name: "temporary-reopen", fault: stateStoreFaultTemporaryReopen},
		{name: "temporary-read", fault: stateStoreFaultTemporaryRead},
		{name: "link", fault: stateStoreFaultLink},
		{name: "parent-sync-after-link", fault: stateStoreFaultCreateParentSync, targetLive: true, settled: true},
		{name: "temporary-remove-after-link", fault: stateStoreFaultRemove, targetLive: true},
		{name: "parent-sync-after-temporary-remove", fault: stateStoreFaultCreateFinalSync, targetLive: true},
		{name: "returned-target-close", fault: stateStoreFaultCreateTargetClose, targetLive: true},
		{name: "fixed-target-reopen-close", fault: stateStoreFaultCreateFixedClose, targetLive: true},
		{name: "creation-temporary-close", fault: stateStoreFaultCreateTemporaryClose, targetLive: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			platform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			directory, err := platform.Root().CreateDirectory("state-creation-fault", true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := errors.Join(directory.Close(), platform.Close()); err != nil {
					t.Error(err)
				}
			}()

			faults := &stateStoreFaultDirectory{
				outputV3Directory: directory,
				fault:             test.fault,
				target:            resumestate.HeaderRecordName,
			}
			random := bytes.NewReader(bytes.Repeat([]byte{0xd5}, 2048))
			if test.badRandom {
				random = bytes.NewReader(nil)
			}
			store := outputStateStore{random: random}
			outcome, err := store.createRecord(
				faults, resumestate.HeaderRecordName, encoded, len(encoded),
			)
			wantOutcome := outputStateCreateNotInstalled
			if test.targetLive {
				wantOutcome = outputStateCreateAdopted
			}
			wantError := !test.settled
			if outcome != wantOutcome || (err != nil) != wantError {
				t.Fatalf("state creation outcome = (%v, %v), want (%v, error=%t)",
					outcome, err, wantOutcome, wantError)
			}

			kind, err := directory.ObserveEntry(resumestate.HeaderRecordName)
			if err != nil {
				t.Fatal(err)
			}
			if !test.targetLive {
				if kind != outputV3EntryAbsent {
					t.Fatalf("failed pre-link creation left target kind %v", kind)
				}
				return
			}
			if kind != outputV3EntryRegularFile {
				t.Fatalf("post-link failure lost authoritative target: kind=%v", kind)
			}
			actual, err := readStateRecord(directory, resumestate.HeaderRecordName, len(encoded))
			if err != nil || !bytes.Equal(actual, encoded) {
				t.Fatalf("post-link target = %q, %v; want %q", actual, err, encoded)
			}
		})
	}
}

func TestOutputV3HeaderTemporaryRecoveryReducesOnlyDeterministicCuts(t *testing.T) {
	for _, test := range []struct {
		name      string
		candidate func(*testing.T, *filesystemOutputSession) []byte
		wrongKind bool
		listedGap bool
		wantError bool
		wantEntry outputV3EntryKind
	}{
		{
			name:      "partial-write",
			candidate: func(*testing.T, *filesystemOutputSession) []byte { return []byte("partial") },
			wantEntry: outputV3EntryAbsent,
		},
		{
			name: "installed-generation",
			candidate: func(t *testing.T, session *filesystemOutputSession) []byte {
				encoded, err := resumestate.EncodeHeader(session.state.Header())
				if err != nil {
					t.Fatal(err)
				}
				return encoded
			},
			wantEntry: outputV3EntryAbsent,
		},
		{
			name: "next-generation",
			candidate: func(t *testing.T, session *filesystemOutputSession) []byte {
				updated, err := session.state.NamespaceAuthority().WithLifecycle(resumestate.SessionPausing)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := resumestate.EncodeHeader(updated.Header())
				if err != nil {
					t.Fatal(err)
				}
				return encoded
			},
			wantEntry: outputV3EntryAbsent,
		},
		{
			name: "foreign-session",
			candidate: func(t *testing.T, _ *filesystemOutputSession) []byte {
				foreign, _, _ := stateStoreHeaderImages(t)
				return foreign.encoded
			},
			wantError: true,
			wantEntry: outputV3EntryRegularFile,
		},
		{name: "wrong-entry-kind", wrongKind: true, wantError: true, wantEntry: outputV3EntryDirectory},
		{name: "listed-entry-disappeared", listedGap: true, wantEntry: outputV3EntryAbsent},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, v3RecoverySelection(t, false, 0)).Session
			defer v3RecoveryCloseSession(t, session)

			temporaryName, err := session.store.temporaryName(resumestate.HeaderRecordName)
			if err != nil {
				t.Fatal(err)
			}
			directory := outputV3Directory(session.sessionDir)
			switch {
			case test.listedGap:
				directory = &stateStoreReconcileFaultDirectory{
					outputV3Directory: session.sessionDir,
					listedMissing:     temporaryName,
				}
			case test.wrongKind:
				wrong, err := session.sessionDir.CreateDirectory(temporaryName, true)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(wrong.Sync(), session.sessionDir.Sync(), wrong.Close()); err != nil {
					t.Fatal(err)
				}
			default:
				writeStateStoreHeaderTemporary(t, session.sessionDir, temporaryName, test.candidate(t, session))
			}

			err = reconcileHeaderRecordTemporaries(
				directory,
				session.state.NamespaceAuthority(),
				func() error { return nil },
			)
			if (err != nil) != test.wantError {
				t.Fatalf("reconcile error = %v, want error=%t", err, test.wantError)
			}
			kind, observeErr := session.sessionDir.ObserveEntry(temporaryName)
			if observeErr != nil || kind != test.wantEntry {
				t.Fatalf("temporary after reconcile = (%v, %v), want %v", kind, observeErr, test.wantEntry)
			}
		})
	}
}

func TestOutputV3HeaderTemporaryRecoveryChecksAuthorityBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name        string
		verifyFault bool
		removeFault bool
		syncFault   bool
		wantPresent bool
	}{
		{name: "authority-changed", verifyFault: true, wantPresent: true},
		{name: "remove-failed", removeFault: true, wantPresent: true},
		{name: "sync-failed-after-remove", syncFault: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, v3RecoverySelection(t, false, 0)).Session
			defer v3RecoveryCloseSession(t, session)
			temporaryName, err := session.store.temporaryName(resumestate.HeaderRecordName)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := resumestate.EncodeHeader(session.state.Header())
			if err != nil {
				t.Fatal(err)
			}
			writeStateStoreHeaderTemporary(t, session.sessionDir, temporaryName, encoded)

			faults := &stateStoreReconcileFaultDirectory{outputV3Directory: session.sessionDir}
			if test.removeFault {
				faults.removeErr = errStateStoreInjected
			}
			if test.syncFault {
				faults.syncErr = errStateStoreInjected
			}
			verifyCalls := 0
			verify := func() error {
				verifyCalls++
				if test.verifyFault && verifyCalls == 2 {
					return errStateStoreInjected
				}
				return nil
			}
			if err := reconcileHeaderRecordTemporaries(
				faults, session.state.NamespaceAuthority(), verify,
			); !errors.Is(err, errStateStoreInjected) {
				t.Fatalf("reconcile fault error = %v, want injected failure", err)
			}
			kind, err := session.sessionDir.ObserveEntry(temporaryName)
			if err != nil {
				t.Fatal(err)
			}
			present := kind != outputV3EntryAbsent
			if present != test.wantPresent {
				t.Fatalf("temporary present=%t after failed cut, want %t", present, test.wantPresent)
			}
		})
	}
}

func TestOutputV3StateRecordDecodingBindsGenerationToTarget(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 8)
	session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
	defer v3RecoveryCloseSession(t, session)
	outputFile := v3RecoveryOutputFile(t, session, selection, 8)
	object, err := resumestate.OutputObjectIDFromBytes(bytes.Repeat([]byte{0xe6}, resumestate.OutputObjectIDBytes))
	if err != nil {
		t.Fatal(err)
	}
	record, err := resumestate.NewFileRecord(resumestate.FileRecordSpec{
		Session: session.state, Descriptor: outputFile.Descriptor,
		CanonicalLocator: outputFile.Path, OutputObject: object,
	})
	if err != nil {
		t.Fatal(err)
	}
	bound := record.Bound()
	encoded, err := resumestate.EncodeFileRecord(bound)
	if err != nil {
		t.Fatal(err)
	}
	name := resumestate.FileRecordName(bound.Record().LocatorDigest())
	if generation, err := decodeStateRecordGeneration(name.Name(), encoded); err != nil || generation != bound.Record().StateGeneration() {
		t.Fatalf("file generation = %d, %v; want %d", generation, err, bound.Record().StateGeneration())
	}
	wrongName := resumestate.FileRecordName(resumestate.DigestCanonicalLocator("different.bin"))
	for _, target := range []string{"x", "zz", wrongName.Name()} {
		if _, err := decodeStateRecordGeneration(target, encoded); err == nil {
			t.Fatalf("state record accepted mismatched target %q", target)
		}
	}
	if _, err := decodeStateRecordGeneration(name.Name(), []byte("invalid")); err == nil {
		t.Fatal("invalid file record generation decoded")
	}
	if _, err := decodeStateRecordGeneration(resumestate.HeaderRecordName, []byte("invalid")); err == nil {
		t.Fatal("invalid header generation decoded")
	}
}

func TestOutputV3StateRecordReadEnforcesExactBoundedImage(t *testing.T) {
	readErr := errors.New("read failed")
	for _, test := range []struct {
		name  string
		file  outputV3File
		limit int
		want  []byte
	}{
		{name: "nil-file", limit: 1},
		{name: "invalid-limit", file: &stateStoreReadFile{size: 1}, limit: 0},
		{name: "size-error", file: &stateStoreReadFile{sizeErr: errStateStoreInjected}, limit: 1},
		{name: "empty", file: &stateStoreReadFile{}, limit: 1},
		{name: "oversize", file: &stateStoreReadFile{size: 2}, limit: 1},
		{name: "read-error", file: &stateStoreReadFile{size: 1, readErr: readErr}, limit: 1},
		{name: "short-read", file: &stateStoreReadFile{size: 2, data: []byte{1}}, limit: 2},
		{name: "full-read-with-eof", file: &stateStoreReadFile{size: 2, data: []byte{1, 2}, readErr: io.EOF}, limit: 2, want: []byte{1, 2}},
		{name: "exact", file: &stateStoreReadFile{size: 2, data: []byte{1, 2}}, limit: 2, want: []byte{1, 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual, err := readStateFile(test.file, test.limit)
			if test.want == nil {
				if err == nil {
					t.Fatalf("read = %v, want error", actual)
				}
				return
			}
			if err != nil || !bytes.Equal(actual, test.want) {
				t.Fatalf("read = %v, %v; want %v", actual, err, test.want)
			}
		})
	}
}

func TestOutputV3StateReplacementRejectsMalformedImagesBeforeFilesystemMutation(t *testing.T) {
	current, next, _ := stateStoreHeaderImages(t)
	for _, test := range []struct {
		name    string
		current outputStateRecordImage
		next    outputStateRecordImage
		want    error
	}{
		{name: "empty-current", current: outputStateRecordImage{}, next: next, want: errOutputV3Unsafe},
		{name: "empty-next", current: current, next: outputStateRecordImage{}, want: errOutputV3Unsafe},
		{
			name: "mislabeled-current-generation",
			current: outputStateRecordImage{
				encoded: current.encoded, generation: current.generation + 1,
			},
			next: next,
			want: resumestate.ErrInvalidState,
		},
		{
			name:    "malformed-next-envelope",
			current: current,
			next:    outputStateRecordImage{encoded: []byte("not a state envelope"), generation: next.generation},
			want:    resumestate.ErrInvalidState,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome, err := (outputStateStore{}).replaceRecord(
				nil, resumestate.HeaderRecordName, test.current, test.next, resumestate.MaxSessionHeaderBytes,
			)
			if outcome != outputStateReplaceUnchanged || !errors.Is(err, test.want) {
				t.Fatalf("rejected replacement = (%d, %v), want unchanged and %v", outcome, err, test.want)
			}
		})
	}
}

func TestOutputV3InitialStateRecoveryFailsClosedBeforeCreatingAuthority(t *testing.T) {
	store := outputStateStore{random: bytes.NewReader(bytes.Repeat([]byte{0xf7}, 256))}
	if _, err := store.ensureInitialRecord(
		nil, resumestate.HeaderRecordName, nil, resumestate.MaxSessionHeaderBytes,
	); !errors.Is(err, errOutputV3Unsafe) {
		t.Fatalf("empty initial image error = %v, want unsafe", err)
	}

	t.Run("missing-target-with-ambiguous-temporary-enumeration", func(t *testing.T) {
		platform, directory := stateStoreEmptyDirectoryFixture(t)
		defer closeStateStoreFixture(t, platform, directory)
		faults := &stateStoreReconcileFaultDirectory{
			outputV3Directory: directory,
			namesErr:          errStateStoreInjected,
		}
		if _, err := store.ensureInitialRecord(
			faults, resumestate.HeaderRecordName, []byte("expected"), resumestate.MaxSessionHeaderBytes,
		); !errors.Is(err, errStateStoreInjected) {
			t.Fatalf("ambiguous temporary enumeration error = %v, want injected failure", err)
		}
		if kind, err := directory.ObserveEntry(resumestate.HeaderRecordName); err != nil || kind != outputV3EntryAbsent {
			t.Fatalf("initial target after blocked recovery = (%v, %v), want absent", kind, err)
		}
	})

	t.Run("existing-target-must-match-requested-image", func(t *testing.T) {
		platform, directory := stateStoreReplacementFixture(t, []byte("installed"))
		defer closeStateStoreFixture(t, platform, directory)
		if _, err := store.ensureInitialRecord(
			directory, resumestate.HeaderRecordName, []byte("different"), resumestate.MaxSessionHeaderBytes,
		); !errors.Is(err, errOutputV3Unsafe) {
			t.Fatalf("divergent initial target error = %v, want unsafe", err)
		}
	})

	t.Run("existing-target-requires-unambiguous-temporary-enumeration", func(t *testing.T) {
		platform, directory := stateStoreReplacementFixture(t, []byte("installed"))
		defer closeStateStoreFixture(t, platform, directory)
		faults := &stateStoreReconcileFaultDirectory{
			outputV3Directory: directory,
			namesErr:          errStateStoreInjected,
		}
		if _, err := store.ensureInitialRecord(
			faults, resumestate.HeaderRecordName, []byte("installed"), resumestate.MaxSessionHeaderBytes,
		); !errors.Is(err, errStateStoreInjected) {
			t.Fatalf("existing target enumeration error = %v, want injected failure", err)
		}
	})

	t.Run("wrong-kind-target-is-never-reinterpreted", func(t *testing.T) {
		platform, directory := stateStoreEmptyDirectoryFixture(t)
		defer closeStateStoreFixture(t, platform, directory)
		wrong, err := directory.CreateDirectory(resumestate.HeaderRecordName, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(wrong.Sync(), directory.Sync(), wrong.Close()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ensureInitialRecord(
			directory, resumestate.HeaderRecordName, []byte("expected"), resumestate.MaxSessionHeaderBytes,
		); !errors.Is(err, errOutputV3Unsafe) {
			t.Fatalf("wrong-kind initial target error = %v, want unsafe", err)
		}
	})
}

func TestOutputV3InitialTemporaryCleanupRequiresExactOwnedEvidence(t *testing.T) {
	if err := removeInitialRecordTemporaries(nil, "not-a-state-record", nil); !errors.Is(err, resumestate.ErrInvalidState) {
		t.Fatalf("invalid temporary target error = %v, want invalid state", err)
	}

	for _, test := range []struct {
		name             string
		createTemporary  bool
		namesErr         error
		malformedName    bool
		classifyOverride bool
		classifyKind     outputV3EntryKind
		classifyErr      error
		openErr          error
		verifyErr        error
		removeErr        error
		syncErr          error
		want             error
		wantPresent      bool
	}{
		{name: "enumeration-failed", namesErr: errStateStoreInjected, want: errStateStoreInjected},
		{name: "malformed-listed-name", malformedName: true, want: errOutputV3Unsafe},
		{
			name: "listed-entry-disappeared", classifyOverride: true,
			classifyKind: outputV3EntryAbsent, want: errOutputV3Unsafe,
		},
		{
			name: "listed-entry-changed-kind", classifyOverride: true,
			classifyKind: outputV3EntryDirectory, want: errOutputV3Unsafe,
		},
		{
			name: "entry-observation-failed", classifyOverride: true,
			classifyErr: errStateStoreInjected, want: errStateStoreInjected,
		},
		{
			name: "temporary-open-failed", createTemporary: true,
			openErr: errStateStoreInjected, want: errStateStoreInjected, wantPresent: true,
		},
		{
			name: "authority-changed-before-removal", createTemporary: true,
			verifyErr: errStateStoreInjected, want: errStateStoreInjected, wantPresent: true,
		},
		{
			name: "remove-failed", createTemporary: true,
			removeErr: errStateStoreInjected, want: errStateStoreInjected, wantPresent: true,
		},
		{
			name: "sync-failed-after-remove", createTemporary: true,
			syncErr: errStateStoreInjected, want: errStateStoreInjected,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform, directory := stateStoreEmptyDirectoryFixture(t)
			defer closeStateStoreFixture(t, platform, directory)
			store := outputStateStore{random: bytes.NewReader(bytes.Repeat([]byte{0x87}, 64))}
			temporaryName, err := store.temporaryName(resumestate.HeaderRecordName)
			if err != nil {
				t.Fatal(err)
			}
			if test.createTemporary {
				writeStateStoreHeaderTemporary(t, directory, temporaryName, []byte("candidate"))
			}
			listedName := temporaryName
			if test.malformedName {
				listedName = resumestate.HeaderUpdateTemporaryPrefix + "not-canonical"
			}
			faults := &stateStoreReconcileFaultDirectory{
				outputV3Directory: directory,
				namesOverride:     []string{listedName},
				namesErr:          test.namesErr,
				classifyOverride:  test.classifyOverride,
				classifyName:      temporaryName,
				classifyKind:      test.classifyKind,
				classifyErr:       test.classifyErr,
				openName:          temporaryName,
				openErr:           test.openErr,
				removeErr:         test.removeErr,
				syncErr:           test.syncErr,
			}
			var verify func() error
			if test.verifyErr != nil {
				verify = func() error { return test.verifyErr }
			}
			err = removeInitialRecordTemporaries(faults, resumestate.HeaderRecordName, verify)
			if !errors.Is(err, test.want) {
				t.Fatalf("temporary cleanup error = %v, want %v", err, test.want)
			}
			kind, observeErr := directory.ObserveEntry(temporaryName)
			if observeErr != nil {
				t.Fatal(observeErr)
			}
			present := kind != outputV3EntryAbsent
			if present != test.wantPresent {
				t.Fatalf("temporary present=%t after cleanup failure, want %t", present, test.wantPresent)
			}
		})
	}
}

func TestOutputV3ExactTemporaryRemovalRefusesAmbiguousIdentity(t *testing.T) {
	for _, test := range []struct {
		name string
		kind outputV3EntryKind
		err  error
		want error
	}{
		{name: "already-absent", kind: outputV3EntryAbsent},
		{name: "changed-kind", kind: outputV3EntryDirectory, want: errOutputV3Unsafe},
		{name: "observation-failed", err: errStateStoreInjected, want: errStateStoreInjected},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := &stateStoreReconcileFaultDirectory{
				classifyOverride: true,
				classifyName:     "temporary",
				classifyKind:     test.kind,
				classifyErr:      test.err,
			}
			err := removeExactStateTemporary(directory, "temporary", &stateStoreReadFile{})
			if !errors.Is(err, test.want) {
				t.Fatalf("exact temporary removal error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOutputV3DirectoryCreationSettlesNamespaceRacesBeforeReturningAuthority(t *testing.T) {
	for _, test := range []struct {
		name             string
		configure        func(*stateStoreDirectoryCreationFault)
		wantCreated      bool
		wantError        error
		wantDirectory    bool
		wantPersistedDir bool
	}{
		{name: "create-and-sync", wantCreated: true, wantDirectory: true, wantPersistedDir: true},
		{
			name: "concurrent-winner",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.createCollision = true
			},
			wantDirectory: true, wantPersistedDir: true,
		},
		{
			name: "classification-failed",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.classifyOverride = true
				faults.classifyErr = errStateStoreInjected
			},
			wantError: errStateStoreInjected,
		},
		{
			name: "aliased-existing-entry",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.classifyOverride = true
				faults.classifyKind = outputV3EntryDirectory
			},
			wantError: errOutputV3Unsafe,
		},
		{
			name: "creation-failed",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.createErr = errStateStoreInjected
			},
			wantError: errStateStoreInjected,
		},
		{
			name: "child-sync-failed",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.childSyncErr = errStateStoreInjected
			},
			wantError: errStateStoreInjected, wantPersistedDir: true,
		},
		{
			name: "parent-sync-failed",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.parentSyncErr = errStateStoreInjected
			},
			wantError: errStateStoreInjected, wantPersistedDir: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform, parent := stateStoreEmptyDirectoryFixture(t)
			defer closeStateStoreFixture(t, platform, parent)
			faults := &stateStoreDirectoryCreationFault{outputV3Directory: parent}
			if test.configure != nil {
				test.configure(faults)
			}
			directory, created, err := ensureOutputDirectory(faults, "child", true)
			if created != test.wantCreated || (directory != nil) != test.wantDirectory || !errors.Is(err, test.wantError) {
				t.Fatalf(
					"ensure directory = (%v, created=%t, %v), want directory=%t created=%t error=%v",
					directory, created, err, test.wantDirectory, test.wantCreated, test.wantError,
				)
			}
			if directory != nil {
				if err := directory.Close(); err != nil {
					t.Fatal(err)
				}
			}
			kind, observeErr := parent.ObserveEntry("child")
			if observeErr != nil {
				t.Fatal(observeErr)
			}
			persisted := kind == outputV3EntryDirectory
			if persisted != test.wantPersistedDir {
				t.Fatalf("persisted child directory=%t, want %t", persisted, test.wantPersistedDir)
			}
		})
	}
}

func TestOutputV3OptionalDirectoryOpeningRejectsAliasAndKindChanges(t *testing.T) {
	for _, test := range []struct {
		name        string
		prepare     func(*testing.T, outputV3Directory)
		configure   func(*stateStoreDirectoryCreationFault)
		wantPresent bool
		wantError   error
	}{
		{name: "absent"},
		{
			name: "exact-directory",
			prepare: func(t *testing.T, parent outputV3Directory) {
				child, err := parent.CreateDirectory("child", true)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(child.Sync(), parent.Sync(), child.Close()); err != nil {
					t.Fatal(err)
				}
			},
			wantPresent: true,
		},
		{
			name: "wrong-kind",
			prepare: func(t *testing.T, parent outputV3Directory) {
				file, err := parent.CreateFile("child", true, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(file.Sync(), parent.Sync(), file.Close()); err != nil {
					t.Fatal(err)
				}
			},
			wantError: errOutputV3Unsafe,
		},
		{
			name: "aliased-directory",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.classifyOverride = true
				faults.classifyKind = outputV3EntryDirectory
			},
			wantError: errOutputV3Unsafe,
		},
		{
			name: "classification-failed",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.classifyOverride = true
				faults.classifyErr = errStateStoreInjected
			},
			wantError: errStateStoreInjected,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform, parent := stateStoreEmptyDirectoryFixture(t)
			defer closeStateStoreFixture(t, platform, parent)
			if test.prepare != nil {
				test.prepare(t, parent)
			}
			faults := &stateStoreDirectoryCreationFault{outputV3Directory: parent}
			if test.configure != nil {
				test.configure(faults)
			}
			directory, present, err := openOptionalOutputDirectory(faults, "child", true)
			if present != test.wantPresent || !errors.Is(err, test.wantError) {
				t.Fatalf("optional directory = (present=%t, %v), want present=%t error=%v", present, err, test.wantPresent, test.wantError)
			}
			if directory != nil {
				if err := directory.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func writeStateStoreHeaderTemporary(
	t *testing.T,
	directory outputV3Directory,
	name string,
	encoded []byte,
) {
	t.Helper()
	temporary, err := directory.CreateFile(name, true, int64(len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	written, writeErr := temporary.WriteAt(encoded, 0)
	if writeErr == nil && written != len(encoded) {
		writeErr = errors.New("short temporary write")
	}
	if err := errors.Join(writeErr, temporary.Sync(), directory.Sync(), temporary.Close()); err != nil {
		t.Fatal(err)
	}
}

func stateStoreHeaderImages(t *testing.T) (
	outputStateRecordImage,
	outputStateRecordImage,
	outputStateRecordImage,
) {
	t.Helper()
	selection := v3RecoverySelection(t, false, 0)
	root, err := resumestate.NewOutputRootBinding(
		resumestate.CertificationWindowsNTFSProcessRestart,
		[]byte("state-store-test-volume"),
		[]byte("state-store-test-root"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := transfer.OutputSessionIDFromBytes(bytes.Repeat([]byte{0x61}, transfer.OutputSessionIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	header, err := resumestate.NewHeader(resumestate.HeaderSpec{
		Backend: filesystemOutputBackendID, SessionID: sessionID, Selection: selection, OutputRoot: root,
		OutputAncestry: v3RecoveryAncestryBinding(t, root, selection),
	})
	if err != nil {
		t.Fatal(err)
	}
	control, err := resumestate.NewControl(resumestate.ControlSpec{
		Backend: filesystemOutputBackendID, OutputRoot: root,
		Certification: resumestate.CertificationWindowsNTFSProcessRestart,
		Durability:    transfer.DurabilityProcessRestart,
		Generation:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	namespace, err := resumestate.BindSessionNamespaceAuthority(
		control,
		header,
		resumestate.ResumeNamespaceName(selection.ResumeIntent()),
		resumestate.SessionDirectoryName(sessionID),
	)
	if err != nil {
		t.Fatal(err)
	}
	next, err := namespace.WithLifecycle(resumestate.SessionPausing)
	if err != nil {
		t.Fatal(err)
	}
	divergent, err := namespace.WithLifecycle(resumestate.SessionCompleting)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(header resumestate.Header) outputStateRecordImage {
		encoded, encodeErr := resumestate.EncodeHeader(header)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		return outputStateRecordImage{encoded: encoded, generation: header.StateGeneration()}
	}
	return encode(namespace.Header()), encode(next.Header()), encode(divergent.Header())
}

func stateStoreReplacementFixture(
	t *testing.T,
	initial []byte,
) (outputV3Platform, outputV3Directory) {
	t.Helper()
	root := v3RecoveryRoot(t)
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := platform.Root().CreateDirectory("state-replacement", true)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	store := outputStateStore{random: bytes.NewReader(bytes.Repeat([]byte{0xa2}, 256))}
	if _, err := store.createRecord(
		directory, resumestate.HeaderRecordName, initial, resumestate.MaxSessionHeaderBytes,
	); err != nil {
		_ = directory.Close()
		_ = platform.Close()
		t.Fatal(err)
	}
	return platform, directory
}

func stateStoreEmptyDirectoryFixture(t *testing.T) (outputV3Platform, outputV3Directory) {
	t.Helper()
	root := v3RecoveryRoot(t)
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := platform.Root().CreateDirectory("state-store", true)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	return platform, directory
}

func closeStateStoreFixture(t *testing.T, platform outputV3Platform, directory outputV3Directory) {
	t.Helper()
	if err := errors.Join(directory.Close(), platform.Close()); err != nil {
		t.Error(err)
	}
}

type stateStoreFaultPoint uint8

const (
	stateStoreFaultNone stateStoreFaultPoint = iota
	stateStoreFaultCreate
	stateStoreFaultWrite
	stateStoreFaultFileSync
	stateStoreFaultTemporaryReopen
	stateStoreFaultTemporaryRead
	stateStoreFaultCurrentReopen
	stateStoreFaultReplaceBeforeMutation
	stateStoreFaultReplaceAfterMutation
	stateStoreFaultParentSync
	stateStoreFaultInstalledReopen
	stateStoreFaultInstalledDivergent
	stateStoreFaultCreateCollision
	stateStoreFaultShortWrite
	stateStoreFaultLink
	stateStoreFaultCreateParentSync
	stateStoreFaultRemove
	stateStoreFaultCreateFinalSync
	stateStoreFaultCurrentClose
	stateStoreFaultInstalledClose
	stateStoreFaultCreateTargetClose
	stateStoreFaultCreateFixedClose
	stateStoreFaultCreateTemporaryClose
)

var errStateStoreInjected = errors.New("injected state-store crash cut")

type stateStoreFaultDirectory struct {
	outputV3Directory
	fault            stateStoreFaultPoint
	target           string
	temporary        string
	replaceAttempted bool
	linkAttempted    bool
	linkSyncs        int
	divergent        []byte
}

func (directory *stateStoreFaultDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputV3File, error) {
	if directory.fault == stateStoreFaultCreateCollision {
		return nil, errOutputV3Collision
	}
	if directory.fault == stateStoreFaultCreate {
		return nil, errStateStoreInjected
	}
	file, err := directory.outputV3Directory.CreateFile(name, private, size)
	if err != nil {
		return nil, err
	}
	directory.temporary = name
	return &stateStoreFaultFile{outputV3File: file, owner: directory, name: name}, nil
}

func (directory *stateStoreFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputV3File, error) {
	if name == directory.temporary && directory.fault == stateStoreFaultTemporaryReopen {
		return nil, errStateStoreInjected
	}
	if name == directory.target && !directory.replaceAttempted && directory.fault == stateStoreFaultCurrentReopen {
		return nil, errStateStoreInjected
	}
	if name == directory.target && directory.replaceAttempted && directory.fault == stateStoreFaultInstalledReopen {
		return nil, errStateStoreInjected
	}
	file, err := directory.outputV3Directory.OpenFile(name, private, writable)
	if err != nil {
		return nil, err
	}
	wrapped := &stateStoreFaultFile{outputV3File: file, owner: directory, name: name, reopened: true}
	if name == directory.target && directory.replaceAttempted && directory.fault == stateStoreFaultInstalledDivergent {
		wrapped.readOverride = bytes.Clone(directory.divergent)
	}
	return wrapped, nil
}

func (directory *stateStoreFaultDirectory) LinkFileNoReplace(source outputV3File, name string) (outputV3File, error) {
	if directory.fault == stateStoreFaultLink {
		return nil, errStateStoreInjected
	}
	if wrapped, ok := source.(*stateStoreFaultFile); ok {
		source = wrapped.outputV3File
	}
	linked, err := directory.outputV3Directory.LinkFileNoReplace(source, name)
	if err != nil {
		return nil, err
	}
	directory.linkAttempted = true
	return &stateStoreFaultFile{outputV3File: linked, owner: directory, name: name, linkedTarget: true}, nil
}

func (directory *stateStoreFaultDirectory) ReplacePrivateFile(source outputV3File, name string) error {
	directory.replaceAttempted = true
	if directory.fault == stateStoreFaultReplaceBeforeMutation {
		return errStateStoreInjected
	}
	if wrapped, ok := source.(*stateStoreFaultFile); ok {
		source = wrapped.outputV3File
	}
	err := directory.outputV3Directory.ReplacePrivateFile(source, name)
	if err == nil && directory.fault == stateStoreFaultReplaceAfterMutation {
		return errStateStoreInjected
	}
	return err
}

func (directory *stateStoreFaultDirectory) RemoveFile(name string, expected outputV3File) error {
	if directory.linkAttempted && name == directory.temporary && directory.fault == stateStoreFaultRemove {
		return errStateStoreInjected
	}
	if wrapped, ok := expected.(*stateStoreFaultFile); ok {
		expected = wrapped.outputV3File
	}
	return directory.outputV3Directory.RemoveFile(name, expected)
}

func (directory *stateStoreFaultDirectory) Sync() error {
	if directory.replaceAttempted && directory.fault == stateStoreFaultParentSync {
		return errStateStoreInjected
	}
	if directory.linkAttempted {
		directory.linkSyncs++
		if directory.fault == stateStoreFaultCreateParentSync && directory.linkSyncs == 1 {
			return errStateStoreInjected
		}
		if directory.fault == stateStoreFaultCreateFinalSync && directory.linkSyncs == 2 {
			return errStateStoreInjected
		}
	}
	return directory.outputV3Directory.Sync()
}

type stateStoreFaultFile struct {
	outputV3File
	owner        *stateStoreFaultDirectory
	name         string
	reopened     bool
	linkedTarget bool
	readOverride []byte
}

func (file *stateStoreFaultFile) WriteAt(value []byte, offset int64) (int, error) {
	if file.name == file.owner.temporary && !file.reopened && file.owner.fault == stateStoreFaultWrite {
		return 0, errStateStoreInjected
	}
	if file.name == file.owner.temporary && !file.reopened && file.owner.fault == stateStoreFaultShortWrite {
		return len(value) - 1, nil
	}
	return file.outputV3File.WriteAt(value, offset)
}

func (file *stateStoreFaultFile) Sync() error {
	if file.name == file.owner.temporary && !file.reopened && file.owner.fault == stateStoreFaultFileSync {
		return errStateStoreInjected
	}
	return file.outputV3File.Sync()
}

func (file *stateStoreFaultFile) ReadAt(value []byte, offset int64) (int, error) {
	if file.readOverride != nil {
		if offset != 0 {
			return 0, errors.New("state-store override only supports offset zero")
		}
		return copy(value, file.readOverride), nil
	}
	if file.name == file.owner.temporary && file.reopened && file.owner.fault == stateStoreFaultTemporaryRead {
		return 0, errStateStoreInjected
	}
	return file.outputV3File.ReadAt(value, offset)
}

func (file *stateStoreFaultFile) Size() (uint64, error) {
	if file.readOverride != nil {
		return uint64(len(file.readOverride)), nil
	}
	return file.outputV3File.Size()
}

func (file *stateStoreFaultFile) SameFile(other outputV3File) (bool, error) {
	if wrapped, ok := other.(*stateStoreFaultFile); ok {
		other = wrapped.outputV3File
	}
	return file.outputV3File.SameFile(other)
}

func (file *stateStoreFaultFile) Close() error {
	injected := false
	switch file.owner.fault {
	case stateStoreFaultCurrentClose:
		injected = file.name == file.owner.target && file.reopened && !file.owner.replaceAttempted
	case stateStoreFaultInstalledClose:
		injected = file.name == file.owner.target && file.reopened && file.owner.replaceAttempted
	case stateStoreFaultCreateTargetClose:
		injected = file.name == file.owner.target && file.linkedTarget
	case stateStoreFaultCreateFixedClose:
		injected = file.name == file.owner.target && file.reopened && file.owner.linkAttempted
	case stateStoreFaultCreateTemporaryClose:
		injected = file.name == file.owner.temporary && !file.reopened && !file.linkedTarget && file.owner.linkAttempted
	}
	if injected {
		return errors.Join(file.outputV3File.Close(), errStateStoreInjected)
	}
	return file.outputV3File.Close()
}

type stateStoreReconcileFaultDirectory struct {
	outputV3Directory
	listedMissing    string
	namesOverride    []string
	namesErr         error
	classifyOverride bool
	classifyName     string
	classifyKind     outputV3EntryKind
	classifyErr      error
	openName         string
	openErr          error
	removeErr        error
	syncErr          error
}

func (directory *stateStoreReconcileFaultDirectory) NamesWithPrefix(prefix string, limit int) ([]string, error) {
	if directory.namesErr != nil {
		return nil, directory.namesErr
	}
	if directory.namesOverride != nil {
		return slices.Clone(directory.namesOverride), nil
	}
	if directory.listedMissing != "" {
		return []string{directory.listedMissing}, nil
	}
	return directory.outputV3Directory.NamesWithPrefix(prefix, limit)
}

func (directory *stateStoreReconcileFaultDirectory) ClassifyExactEntry(
	name string,
) (outputV3EntryKind, bool, error) {
	if directory.classifyOverride && name == directory.classifyName {
		return directory.classifyKind, true, directory.classifyErr
	}
	if name == directory.listedMissing {
		return outputV3EntryAbsent, true, nil
	}
	return directory.outputV3Directory.ClassifyExactEntry(name)
}

func (directory *stateStoreReconcileFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputV3File, error) {
	if name == directory.openName && directory.openErr != nil {
		return nil, directory.openErr
	}
	return directory.outputV3Directory.OpenFile(name, private, writable)
}

func (directory *stateStoreReconcileFaultDirectory) RemoveFile(name string, expected outputV3File) error {
	if directory.removeErr != nil {
		return directory.removeErr
	}
	return directory.outputV3Directory.RemoveFile(name, expected)
}

func (directory *stateStoreReconcileFaultDirectory) Sync() error {
	if directory.syncErr != nil {
		return directory.syncErr
	}
	return directory.outputV3Directory.Sync()
}

type stateStoreReadFile struct {
	outputV3File
	size    uint64
	sizeErr error
	data    []byte
	readErr error
}

func (file *stateStoreReadFile) Size() (uint64, error) { return file.size, file.sizeErr }

func (file *stateStoreReadFile) ReadAt(target []byte, _ int64) (int, error) {
	return copy(target, file.data), file.readErr
}

type stateStoreDirectoryCreationFault struct {
	outputV3Directory
	classifyOverride bool
	classifyKind     outputV3EntryKind
	classifyErr      error
	createCollision  bool
	createErr        error
	childSyncErr     error
	parentSyncErr    error
}

func (directory *stateStoreDirectoryCreationFault) ClassifyExactEntry(
	name string,
) (outputV3EntryKind, bool, error) {
	if directory.classifyOverride {
		return directory.classifyKind, false, directory.classifyErr
	}
	return directory.outputV3Directory.ClassifyExactEntry(name)
}

func (directory *stateStoreDirectoryCreationFault) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	if directory.createErr != nil {
		return nil, directory.createErr
	}
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	if err != nil {
		return nil, err
	}
	if directory.createCollision {
		if err := errors.Join(created.Sync(), directory.outputV3Directory.Sync(), created.Close()); err != nil {
			return nil, err
		}
		return nil, errOutputV3Collision
	}
	if directory.childSyncErr != nil {
		return &stateStoreDirectorySyncFault{
			outputV3Directory: created,
			syncErr:           directory.childSyncErr,
		}, nil
	}
	return created, nil
}

func (directory *stateStoreDirectoryCreationFault) Sync() error {
	if directory.parentSyncErr != nil {
		return directory.parentSyncErr
	}
	return directory.outputV3Directory.Sync()
}

type stateStoreDirectorySyncFault struct {
	outputV3Directory
	syncErr error
}

func (directory *stateStoreDirectorySyncFault) Sync() error { return directory.syncErr }

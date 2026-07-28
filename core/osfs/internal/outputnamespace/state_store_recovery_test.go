package outputnamespace

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
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
			store := Store{random: bytes.NewReader(bytes.Repeat([]byte{0x91}, 256))}
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
			recoveryStore := Store{random: bytes.NewReader(bytes.Repeat([]byte{0xa2}, 256))}
			if _, err := recoveryStore.EnsureInitialRecord(reopened, targetName, encoded, len(encoded)); err != nil {
				t.Fatal(err)
			}
			actual, err := ReadRecord(reopened, targetName, len(encoded))
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
		wantOutcome ReplaceOutcome
		wantBytes   []byte
		wantError   bool
	}{
		{name: "create", fault: stateStoreFaultCreate, wantOutcome: ReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "write", fault: stateStoreFaultWrite, wantOutcome: ReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "file-sync", fault: stateStoreFaultFileSync, wantOutcome: ReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "temporary-reopen", fault: stateStoreFaultTemporaryReopen, wantOutcome: ReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "temporary-byte-verify", fault: stateStoreFaultTemporaryRead, wantOutcome: ReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "current-target-reopen", fault: stateStoreFaultCurrentReopen, wantOutcome: ReplaceUncertain, wantBytes: current.encoded, wantError: true},
		{name: "replace-before-mutation", fault: stateStoreFaultReplaceBeforeMutation, wantOutcome: ReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "replace-succeeded-before-error", fault: stateStoreFaultReplaceAfterMutation, wantOutcome: ReplaceAdopted, wantBytes: next.encoded},
		{name: "parent-sync", fault: stateStoreFaultParentSync, wantOutcome: ReplaceAdopted, wantBytes: next.encoded},
		{name: "installed-target-reopen", fault: stateStoreFaultInstalledReopen, wantOutcome: ReplaceUncertain, wantBytes: next.encoded, wantError: true},
		{name: "installed-target-diverged", fault: stateStoreFaultInstalledDivergent, wantOutcome: ReplaceUncertain, wantBytes: next.encoded, wantError: true},
		{name: "current-target-close", fault: stateStoreFaultCurrentClose, wantOutcome: ReplaceUnchanged, wantBytes: current.encoded, wantError: true},
		{name: "installed-target-close", fault: stateStoreFaultInstalledClose, wantOutcome: ReplaceAdopted, wantBytes: next.encoded, wantError: true},
		{name: "complete", wantOutcome: ReplaceAdopted, wantBytes: next.encoded},
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
				Directory: directory,
				fault:     test.fault,
				target:    resumestate.HeaderRecordName,
				divergent: divergentRetry.encoded,
			}
			store := Store{random: bytes.NewReader(bytes.Repeat([]byte{0xb3}, 256))}
			outcome, err := store.ReplaceRecord(
				faults, resumestate.HeaderRecordName, current, next, resumestate.MaxSessionHeaderBytes,
			)
			if outcome != test.wantOutcome || (err != nil) != test.wantError {
				t.Fatalf("replace outcome = %d, %v; want %d, error=%t", outcome, err, test.wantOutcome, test.wantError)
			}
			actual, err := ReadRecord(directory, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
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
	store := Store{random: bytes.NewReader(bytes.Repeat([]byte{0xc4}, 512))}

	sameGeneration := current
	outcome, err := store.ReplaceRecord(
		directory, resumestate.HeaderRecordName, current, sameGeneration, resumestate.MaxSessionHeaderBytes,
	)
	if outcome != ReplaceUnchanged || !errors.Is(err, resumestate.ErrInvalidTransition) {
		t.Fatalf("same-generation replacement = %d, %v", outcome, err)
	}
	mislabeled := next
	mislabeled.generation++
	outcome, err = store.ReplaceRecord(
		directory, resumestate.HeaderRecordName, current, mislabeled, resumestate.MaxSessionHeaderBytes,
	)
	if outcome != ReplaceUnchanged || !errors.Is(err, resumestate.ErrInvalidState) {
		t.Fatalf("mislabeled replacement image = %d, %v", outcome, err)
	}

	outcome, err = store.ReplaceRecord(
		directory, resumestate.HeaderRecordName, current, next, resumestate.MaxSessionHeaderBytes,
	)
	if outcome != ReplaceAdopted || err != nil {
		t.Fatalf("first replacement = %d, %v", outcome, err)
	}
	outcome, err = store.ReplaceRecord(
		directory, resumestate.HeaderRecordName, current, divergentRetry, resumestate.MaxSessionHeaderBytes,
	)
	if outcome != ReplaceUncertain || err == nil {
		t.Fatalf("stale-authority replacement = %d, %v", outcome, err)
	}
	actual, err := ReadRecord(directory, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
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
				Directory: directory,
				fault:     test.fault,
				target:    resumestate.HeaderRecordName,
			}
			random := bytes.NewReader(bytes.Repeat([]byte{0xd5}, 2048))
			if test.badRandom {
				random = bytes.NewReader(nil)
			}
			store := Store{random: random}
			outcome, err := store.CreateRecord(
				faults, resumestate.HeaderRecordName, encoded, len(encoded),
			)
			wantOutcome := CreateNotInstalled
			if test.targetLive {
				wantOutcome = CreateAdopted
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
				if kind != outputcap.EntryAbsent {
					t.Fatalf("failed pre-link creation left target kind %v", kind)
				}
				return
			}
			if kind != outputcap.EntryRegularFile {
				t.Fatalf("post-link failure lost authoritative target: kind=%v", kind)
			}
			actual, err := ReadRecord(directory, resumestate.HeaderRecordName, len(encoded))
			if err != nil || !bytes.Equal(actual, encoded) {
				t.Fatalf("post-link target = %q, %v; want %q", actual, err, encoded)
			}
		})
	}
}

func TestOutputV3HeaderTemporaryRecoveryReducesOnlyDeterministicCuts(t *testing.T) {
	for _, test := range []struct {
		name      string
		candidate func(*testing.T, *testStateSession) []byte
		wrongKind bool
		listedGap bool
		wantError bool
		wantEntry outputcap.EntryKind
	}{
		{
			name:      "partial-write",
			candidate: func(*testing.T, *testStateSession) []byte { return []byte("partial") },
			wantEntry: outputcap.EntryAbsent,
		},
		{
			name: "installed-generation",
			candidate: func(t *testing.T, session *testStateSession) []byte {
				encoded, err := resumestate.EncodeHeader(session.state.Header())
				if err != nil {
					t.Fatal(err)
				}
				return encoded
			},
			wantEntry: outputcap.EntryAbsent,
		},
		{
			name: "next-generation",
			candidate: func(t *testing.T, session *testStateSession) []byte {
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
			wantEntry: outputcap.EntryAbsent,
		},
		{
			name: "foreign-session",
			candidate: func(t *testing.T, _ *testStateSession) []byte {
				foreign, _, _ := stateStoreHeaderImages(t)
				return foreign.encoded
			},
			wantError: true,
			wantEntry: outputcap.EntryRegularFile,
		},
		{name: "wrong-entry-kind", wrongKind: true, wantError: true, wantEntry: outputcap.EntryDirectory},
		{name: "listed-entry-disappeared", listedGap: true, wantEntry: outputcap.EntryAbsent},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := newTestStateSession(t, v3RecoverySelection(t, false, 0))
			defer session.close(t)

			temporaryName, err := session.store.temporaryName(resumestate.HeaderRecordName)
			if err != nil {
				t.Fatal(err)
			}
			directory := outputcap.Directory(session.sessionDir)
			switch {
			case test.listedGap:
				directory = &stateStoreReconcileFaultDirectory{
					Directory:     session.sessionDir,
					listedMissing: temporaryName,
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

			err = ReconcileHeaderRecordTemporaries(
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

func TestOutputV3StateReplacementRejectsMalformedImagesBeforeFilesystemMutation(t *testing.T) {
	current, next, _ := stateStoreHeaderImages(t)
	for _, test := range []struct {
		name    string
		current RecordImage
		next    RecordImage
		want    error
	}{
		{name: "empty-current", current: RecordImage{}, next: next, want: outputcap.ErrUnsafeNamespace},
		{name: "empty-next", current: current, next: RecordImage{}, want: outputcap.ErrUnsafeNamespace},
		{
			name: "mislabeled-current-generation",
			current: RecordImage{
				encoded: current.encoded, generation: current.generation + 1,
			},
			next: next,
			want: resumestate.ErrInvalidState,
		},
		{
			name:    "malformed-next-envelope",
			current: current,
			next:    RecordImage{encoded: []byte("not a state envelope"), generation: next.generation},
			want:    resumestate.ErrInvalidState,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome, err := (Store{}).ReplaceRecord(
				nil, resumestate.HeaderRecordName, test.current, test.next, resumestate.MaxSessionHeaderBytes,
			)
			if outcome != ReplaceUnchanged || !errors.Is(err, test.want) {
				t.Fatalf("rejected replacement = (%d, %v), want unchanged and %v", outcome, err, test.want)
			}
		})
	}
}

func TestOutputV3InitialStateRecoveryFailsClosedBeforeCreatingAuthority(t *testing.T) {
	store := Store{random: bytes.NewReader(bytes.Repeat([]byte{0xf7}, 256))}
	if _, err := store.EnsureInitialRecord(
		nil, resumestate.HeaderRecordName, nil, resumestate.MaxSessionHeaderBytes,
	); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("empty initial image error = %v, want unsafe", err)
	}

	t.Run("missing-target-with-ambiguous-temporary-enumeration", func(t *testing.T) {
		platform, directory := stateStoreEmptyDirectoryFixture(t)
		defer closeStateStoreFixture(t, platform, directory)
		faults := &stateStoreReconcileFaultDirectory{
			Directory: directory,
			namesErr:  errStateStoreInjected,
		}
		if _, err := store.EnsureInitialRecord(
			faults, resumestate.HeaderRecordName, []byte("expected"), resumestate.MaxSessionHeaderBytes,
		); !errors.Is(err, errStateStoreInjected) {
			t.Fatalf("ambiguous temporary enumeration error = %v, want injected failure", err)
		}
		if kind, err := directory.ObserveEntry(resumestate.HeaderRecordName); err != nil || kind != outputcap.EntryAbsent {
			t.Fatalf("initial target after blocked recovery = (%v, %v), want absent", kind, err)
		}
	})

	t.Run("existing-target-must-match-requested-image", func(t *testing.T) {
		platform, directory := stateStoreReplacementFixture(t, []byte("installed"))
		defer closeStateStoreFixture(t, platform, directory)
		if _, err := store.EnsureInitialRecord(
			directory, resumestate.HeaderRecordName, []byte("different"), resumestate.MaxSessionHeaderBytes,
		); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("divergent initial target error = %v, want unsafe", err)
		}
	})

	t.Run("existing-target-requires-unambiguous-temporary-enumeration", func(t *testing.T) {
		platform, directory := stateStoreReplacementFixture(t, []byte("installed"))
		defer closeStateStoreFixture(t, platform, directory)
		faults := &stateStoreReconcileFaultDirectory{
			Directory: directory,
			namesErr:  errStateStoreInjected,
		}
		if _, err := store.EnsureInitialRecord(
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
		if _, err := store.EnsureInitialRecord(
			directory, resumestate.HeaderRecordName, []byte("expected"), resumestate.MaxSessionHeaderBytes,
		); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("wrong-kind initial target error = %v, want unsafe", err)
		}
	})
}

func TestOutputV3InitialTemporaryCleanupRequiresExactOwnedEvidence(t *testing.T) {
	if err := RemoveInitialRecordTemporaries(nil, "not-a-state-record", nil); !errors.Is(err, resumestate.ErrInvalidState) {
		t.Fatalf("invalid temporary target error = %v, want invalid state", err)
	}

	for _, test := range []struct {
		name             string
		createTemporary  bool
		namesErr         error
		malformedName    bool
		classifyOverride bool
		classifyKind     outputcap.EntryKind
		classifyErr      error
		openErr          error
		verifyErr        error
		removeErr        error
		syncErr          error
		want             error
		wantPresent      bool
	}{
		{name: "enumeration-failed", namesErr: errStateStoreInjected, want: errStateStoreInjected},
		{name: "malformed-listed-name", malformedName: true, want: outputcap.ErrUnsafeNamespace},
		{
			name: "listed-entry-disappeared", classifyOverride: true,
			classifyKind: outputcap.EntryAbsent, want: outputcap.ErrUnsafeNamespace,
		},
		{
			name: "listed-entry-changed-kind", classifyOverride: true,
			classifyKind: outputcap.EntryDirectory, want: outputcap.ErrUnsafeNamespace,
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
			store := Store{random: bytes.NewReader(bytes.Repeat([]byte{0x87}, 64))}
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
				Directory:        directory,
				namesOverride:    []string{listedName},
				namesErr:         test.namesErr,
				classifyOverride: test.classifyOverride,
				classifyName:     temporaryName,
				classifyKind:     test.classifyKind,
				classifyErr:      test.classifyErr,
				openName:         temporaryName,
				openErr:          test.openErr,
				removeErr:        test.removeErr,
				syncErr:          test.syncErr,
			}
			var verify func() error
			if test.verifyErr != nil {
				verify = func() error { return test.verifyErr }
			}
			err = RemoveInitialRecordTemporaries(faults, resumestate.HeaderRecordName, verify)
			if !errors.Is(err, test.want) {
				t.Fatalf("temporary cleanup error = %v, want %v", err, test.want)
			}
			kind, observeErr := directory.ObserveEntry(temporaryName)
			if observeErr != nil {
				t.Fatal(observeErr)
			}
			present := kind != outputcap.EntryAbsent
			if present != test.wantPresent {
				t.Fatalf("temporary present=%t after cleanup failure, want %t", present, test.wantPresent)
			}
		})
	}
}

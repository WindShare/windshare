package outputnamespace

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

var errOutputV3ControlSessionInjected = errors.New("injected control/session failure")

func TestOutputV3ControlBootstrapFailureCutsRemainRecoverable(t *testing.T) {
	const nonceByte = byte(0xc7)
	candidateName := v3RecoveryBootstrapCandidateName(t, nonceByte)
	tests := []struct {
		name          string
		plan          *outputV3ControlSessionFaultPlan
		exhaustRandom bool
		wantCause     error
	}{
		{
			name: "inspect-installed-control",
			plan: outputV3ControlSessionFailure(
				outputV3CSClassifyEntry, "", resumestate.ControlDirectoryName,
			),
			wantCause: errOutputV3ControlSessionInjected,
		},
		{
			name: "enumerate-candidates",
			plan: outputV3ControlSessionFailure(
				outputV3CSNamesWithPrefix, "", resumestate.BootstrapCandidatePrefix,
			),
			wantCause: errOutputV3ControlSessionInjected,
		},
		{
			name:      "bind-root-identity",
			plan:      outputV3ControlSessionFailure(outputV3CSRootBinding, "", ""),
			wantCause: errOutputV3ControlSessionInjected,
		},
		{
			name:          "allocate-bootstrap-nonce",
			exhaustRandom: true,
			wantCause:     io.EOF,
		},
		{
			name: "create-candidate",
			plan: outputV3ControlSessionFailure(
				outputV3CSCreateDirectory, "", candidateName,
			),
			wantCause: errOutputV3ControlSessionInjected,
		},
		{
			name:      "build-candidate",
			plan:      outputV3ControlSessionFailure(outputV3CSNames, candidateName, ""),
			wantCause: errOutputV3ControlSessionInjected,
		},
		{
			name: "install-candidate",
			plan: outputV3ControlSessionFailure(
				outputV3CSInstallDirectory, "", resumestate.ControlDirectoryName,
			),
			wantCause: errOutputV3ControlSessionInjected,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, root, nil)
			authority.random = bytes.NewReader(bytes.Repeat([]byte{nonceByte}, 64*1024))
			if test.exhaustRandom {
				authority.random = bytes.NewReader(nil)
			}
			platform := openOutputV3ControlSessionFaultPlatform(t, root, test.plan)
			bootstrapResult, err := authority.OpenOrBootstrapControl(platform)
			namespace := bootstrapResult.Namespace
			if namespace != nil {
				_ = namespace.Close()
			}
			if !errors.Is(err, test.wantCause) {
				t.Fatalf("bootstrap failure = %v, want %v", err, test.wantCause)
			}
			outputV3ControlSessionRequireFault(
				t, err, transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe,
			)
			if test.plan != nil {
				test.plan.requireFired(t)
			}
			if err := platform.Close(); err != nil {
				t.Fatal(err)
			}

			// Every failed cut above is either pre-mutation, an empty candidate, or a
			// complete candidate. A plain restart must therefore converge without
			// deleting ambiguous metadata or requiring operator repair.
			authority.random = bytes.NewReader(bytes.Repeat([]byte{nonceByte + 1}, 64*1024))
			recoveryPlatform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			recoveryResult, err := authority.OpenOrBootstrapControl(recoveryPlatform)
			recovered := recoveryResult.Namespace
			created := recoveryResult.Disposition == ControlInstalled
			if err != nil || !created {
				_ = recoveryPlatform.Close()
				t.Fatalf("recover bootstrap cut = (created=%t, err=%v)", created, err)
			}
			candidates, listErr := recoveryPlatform.Root().NamesWithPrefix(
				resumestate.BootstrapCandidatePrefix, RootInspectionLimit,
			)
			if listErr != nil || len(candidates) != 0 {
				t.Fatalf("recovered candidates = %v, %v", candidates, listErr)
			}
			if err := errors.Join(recovered.Close(), recoveryPlatform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3BootstrapCleanupFailureCutsConvergeOnRestart(t *testing.T) {
	tests := []struct {
		name string
		plan func(string) *outputV3ControlSessionFaultPlan
	}{
		{
			name: "remove-sessions",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSRemoveDirectory, candidate, resumestate.SessionsDirectoryName,
				)
			},
		},
		{
			name: "sync-after-sessions",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSSync, candidate, "")
			},
		},
		{
			name: "remove-lock",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSRemoveFile, candidate, resumestate.CoordinatorLockName,
				)
			},
		},
		{
			name: "remove-envelope",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSRemoveFile, candidate, resumestate.ControlRecordName,
				)
			},
		},
		{
			name: "remove-empty-candidate",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSRemoveDirectory, "", candidate)
			},
		},
		{
			name: "sync-root-after-removal",
			plan: func(_ string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSSync, "", "")
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, root, nil)
			platform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			installedResult, err := authority.OpenOrBootstrapControl(platform)
			installed := installedResult.Namespace
			if err != nil {
				t.Fatal(err)
			}
			control := installed.control
			if err := installed.Close(); err != nil {
				t.Fatal(err)
			}
			candidateName := v3RecoveryBootstrapCandidateName(t, byte(0xd0+index))
			candidate, err := platform.Root().CreateDirectory(candidateName, true)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryBuildBootstrapPrefix(t, authority, candidate, control, 3)
			if err := errors.Join(candidate.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}

			plan := test.plan(candidateName)
			faulted := openOutputV3ControlSessionFaultPlatform(t, root, plan)
			faultResult, openErr := authority.OpenOrBootstrapControl(faulted)
			unexpected := faultResult.Namespace
			if unexpected != nil {
				_ = unexpected.Close()
			}
			if !errors.Is(openErr, errOutputV3ControlSessionInjected) {
				t.Fatalf("cleanup failure = %v, want injected failure", openErr)
			}
			outputV3ControlSessionRequireFault(
				t, openErr, transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe,
			)
			plan.requireFired(t)
			if err := faulted.Close(); err != nil {
				t.Fatal(err)
			}

			recoveryPlatform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			recoveryResult, err := authority.OpenOrBootstrapControl(recoveryPlatform)
			recovered := recoveryResult.Namespace
			created := recoveryResult.Disposition == ControlInstalled
			if err != nil || created || recovered.control != control {
				_ = recoveryPlatform.Close()
				t.Fatalf("recover cleanup cut = (created=%t, same=%t, err=%v)",
					created, recovered != nil && recovered.control == control, err)
			}
			kind, observeErr := recoveryPlatform.Root().ObserveEntry(candidateName)
			if observeErr != nil || kind != outputcap.EntryAbsent {
				t.Fatalf("candidate after cleanup recovery = (%v, %v)", kind, observeErr)
			}
			if err := errors.Join(recovered.Close(), recoveryPlatform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3DivergentValidControlCandidatePersistentlyBlocksRootBeforeInstallation(t *testing.T) {
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	firstControl, err := authority.newControl(platform)
	if err != nil {
		t.Fatal(err)
	}
	secondControl, err := resumestate.NewControl(resumestate.ControlSpec{
		Backend: firstControl.Backend(), OutputRoot: firstControl.OutputRoot(),
		Certification: firstControl.Certification(), Durability: firstControl.Durability(),
		Generation: firstControl.Generation() + 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{
		v3RecoveryBootstrapCandidateName(t, 0xf2),
		v3RecoveryBootstrapCandidateName(t, 0xf1),
	}
	slices.Sort(names)
	for index, control := range []resumestate.Control{firstControl, secondControl} {
		candidate, err := platform.Root().CreateDirectory(names[index], true)
		if err != nil {
			t.Fatal(err)
		}
		v3RecoveryBuildBootstrapPrefix(t, authority, candidate, control, 3)
		if err := candidate.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}

	for attempt := range 2 {
		platform, err = openOutputV3Platform(root, false)
		if err != nil {
			t.Fatal(err)
		}
		faultResult, openErr := authority.OpenOrBootstrapControl(platform)
		namespace := faultResult.Namespace
		if namespace != nil {
			_ = namespace.Close()
		}
		if !errors.Is(openErr, outputfault.ErrRootUnsafe) {
			t.Fatalf("ambiguous control attempt %d = %v, want root block", attempt, openErr)
		}
		outputV3ControlSessionRequireFault(
			t, openErr, transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe,
		)
		if kind, observeErr := platform.Root().ObserveEntry(resumestate.ControlDirectoryName); observeErr != nil ||
			kind != outputcap.EntryAbsent {
			t.Fatalf("divergent candidate attempt %d installed control = (%v, %v)", attempt, kind, observeErr)
		}
		for _, name := range names {
			kind, observeErr := platform.Root().ObserveEntry(name)
			if observeErr != nil || kind != outputcap.EntryDirectory {
				t.Fatalf("divergent candidate %q attempt %d = (%v, %v), want preserved", name, attempt, kind, observeErr)
			}
		}
		if err := platform.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOutputV3InstalledControlAuthorityFailuresBlockRootAndRecover(t *testing.T) {
	tests := []struct {
		name string
		plan *outputV3ControlSessionFaultPlan
	}{
		{
			name: "read-control-record",
			plan: outputV3ControlSessionFailure(
				outputV3CSOpenFile, resumestate.ControlDirectoryName, resumestate.ControlRecordName,
			),
		},
		{
			name: "enumerate-control-children",
			plan: outputV3ControlSessionFailure(outputV3CSNames, resumestate.ControlDirectoryName, ""),
		},
		{
			name: "open-coordinator-record",
			plan: outputV3ControlSessionFailure(
				outputV3CSOpenFile, resumestate.ControlDirectoryName, resumestate.CoordinatorLockName,
			),
		},
		{
			name: "open-sessions-directory",
			plan: outputV3ControlSessionFailure(
				outputV3CSOpenDirectory, resumestate.ControlDirectoryName, resumestate.SessionsDirectoryName,
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, root, nil)
			platform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			installedResult, err := authority.OpenOrBootstrapControl(platform)
			installed := installedResult.Namespace
			if err != nil {
				t.Fatal(err)
			}
			expectedControl := installed.control
			if err := errors.Join(installed.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}

			faulted := openOutputV3ControlSessionFaultPlatform(t, root, test.plan)
			faultResult, openErr := authority.OpenOrBootstrapControl(faulted)
			namespace := faultResult.Namespace
			if namespace != nil {
				_ = namespace.Close()
				t.Fatal("installed-control authority failure returned a namespace")
			}
			if !errors.Is(openErr, errOutputV3ControlSessionInjected) {
				t.Fatalf("installed-control authority failure = %v, want injected failure", openErr)
			}
			outputV3ControlSessionRequireFault(
				t, openErr, transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe,
			)
			test.plan.requireFired(t)
			if err := faulted.Close(); err != nil {
				t.Fatal(err)
			}

			recoveryPlatform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			recoveryResult, err := authority.OpenOrBootstrapControl(recoveryPlatform)
			recovered := recoveryResult.Namespace
			created := recoveryResult.Disposition == ControlInstalled
			if err != nil || created || recovered.control != expectedControl {
				_ = recoveryPlatform.Close()
				t.Fatalf("recover installed control = (created=%t, same=%t, err=%v)",
					created, recovered != nil && recovered.control == expectedControl, err)
			}
			if err := errors.Join(recovered.Close(), recoveryPlatform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("nonempty-coordinator-record", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		authority := v3RecoveryAuthority(t, root, nil)
		platform, err := openOutputV3Platform(root, false)
		if err != nil {
			t.Fatal(err)
		}
		installedResult, err := authority.OpenOrBootstrapControl(platform)
		installed := installedResult.Namespace
		if err != nil {
			t.Fatal(err)
		}
		expectedControl := installed.control
		if err := errors.Join(installed.Close(), platform.Close()); err != nil {
			t.Fatal(err)
		}
		writeControlFixtureFile(
			t, root, resumestate.ControlDirectoryName, resumestate.CoordinatorLockName, []byte{1},
		)

		faulted, err := openOutputV3Platform(root, false)
		if err != nil {
			t.Fatal(err)
		}
		faultResult, openErr := authority.OpenOrBootstrapControl(faulted)
		namespace := faultResult.Namespace
		if namespace != nil {
			_ = namespace.Close()
			t.Fatal("nonempty coordinator returned a namespace")
		}
		outputV3ControlSessionRequireFault(
			t, openErr, transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe,
		)
		if err := faulted.Close(); err != nil {
			t.Fatal(err)
		}
		writeControlFixtureFile(
			t, root, resumestate.ControlDirectoryName, resumestate.CoordinatorLockName, nil,
		)

		recoveryPlatform, err := openOutputV3Platform(root, false)
		if err != nil {
			t.Fatal(err)
		}
		recoveryResult, err := authority.OpenOrBootstrapControl(recoveryPlatform)
		recovered := recoveryResult.Namespace
		created := recoveryResult.Disposition == ControlInstalled
		if err != nil || created || recovered.control != expectedControl {
			_ = recoveryPlatform.Close()
			t.Fatalf("recover coordinator cut = (created=%t, same=%t, err=%v)",
				created, recovered != nil && recovered.control == expectedControl, err)
		}
		if err := errors.Join(recovered.Close(), recoveryPlatform.Close()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestOutputV3StateRecordDecodingBindsGenerationToTarget(t *testing.T) {
	selection := v3RecoverySelection(t, true, 8)
	session := newTestStateSession(t, selection)
	defer session.close(t)
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
	if generation, err := DecodeRecordGeneration(name.Name(), encoded); err != nil || generation != bound.Record().StateGeneration() {
		t.Fatalf("file generation = %d, %v; want %d", generation, err, bound.Record().StateGeneration())
	}
	wrongName := resumestate.FileRecordName(resumestate.DigestCanonicalLocator("different.bin"))
	for _, target := range []string{"x", "zz", wrongName.Name()} {
		if _, err := DecodeRecordGeneration(target, encoded); err == nil {
			t.Fatalf("state record accepted mismatched target %q", target)
		}
	}
	if _, err := DecodeRecordGeneration(name.Name(), []byte("invalid")); err == nil {
		t.Fatal("invalid file record generation decoded")
	}
	if _, err := DecodeRecordGeneration(resumestate.HeaderRecordName, []byte("invalid")); err == nil {
		t.Fatal("invalid header generation decoded")
	}
}

func TestOutputV3OptionalDirectoryOpeningRejectsAliasAndKindChanges(t *testing.T) {
	for _, test := range []struct {
		name        string
		prepare     func(*testing.T, outputcap.Directory)
		configure   func(*stateStoreDirectoryCreationFault)
		wantPresent bool
		wantError   error
	}{
		{name: "absent"},
		{
			name: "exact-directory",
			prepare: func(t *testing.T, parent outputcap.Directory) {
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
			prepare: func(t *testing.T, parent outputcap.Directory) {
				file, err := parent.CreateFile("child", true, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(file.Sync(), parent.Sync(), file.Close()); err != nil {
					t.Fatal(err)
				}
			},
			wantError: outputcap.ErrUnsafeNamespace,
		},
		{
			name: "aliased-directory",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.classifyOverride = true
				faults.classifyKind = outputcap.EntryDirectory
			},
			wantError: outputcap.ErrUnsafeNamespace,
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
			faults := &stateStoreDirectoryCreationFault{Directory: parent}
			if test.configure != nil {
				test.configure(faults)
			}
			result, err := OpenOptionalDirectory(faults, "child", true)
			directory := result.Directory
			present := result.Disposition == DirectoryExisting
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

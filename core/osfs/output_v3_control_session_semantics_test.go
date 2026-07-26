package osfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

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
			namespace, _, err := authority.openOrBootstrapControl(platform)
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
			recovered, created, err := authority.openOrBootstrapControl(recoveryPlatform)
			if err != nil || !created {
				_ = recoveryPlatform.Close()
				t.Fatalf("recover bootstrap cut = (created=%t, err=%v)", created, err)
			}
			candidates, listErr := recoveryPlatform.Root().NamesWithPrefix(
				resumestate.BootstrapCandidatePrefix, outputRootInspectionLimit,
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
			installed, _, err := authority.openOrBootstrapControl(platform)
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
			unexpected, _, openErr := authority.openOrBootstrapControl(faulted)
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
			recovered, created, err := authority.openOrBootstrapControl(recoveryPlatform)
			if err != nil || created || recovered.control != control {
				_ = recoveryPlatform.Close()
				t.Fatalf("recover cleanup cut = (created=%t, same=%t, err=%v)",
					created, recovered != nil && recovered.control == control, err)
			}
			kind, observeErr := recoveryPlatform.Root().ObserveEntry(candidateName)
			if observeErr != nil || kind != outputV3EntryAbsent {
				t.Fatalf("candidate after cleanup recovery = (%v, %v)", kind, observeErr)
			}
			if err := errors.Join(recovered.Close(), recoveryPlatform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3RecoveredBootstrapAuthorityCutsConvergeOnRestart(t *testing.T) {
	tests := []struct {
		name        string
		prefixCount int
		plan        func(string) *outputV3ControlSessionFaultPlan
	}{
		{
			name:        "remove-empty-candidate",
			prefixCount: 0,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSRemoveDirectory, "", candidate)
			},
		},
		{
			name:        "create-missing-coordinator",
			prefixCount: 1,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSCreateFile, candidate, resumestate.CoordinatorLockName,
				)
			},
		},
		{
			name:        "sync-created-coordinator",
			prefixCount: 1,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSSync, candidate, "")
			},
		},
		{
			name:        "create-missing-sessions",
			prefixCount: 2,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSCreateDirectory, candidate, resumestate.SessionsDirectoryName,
				)
			},
		},
		{
			name:        "sync-created-sessions",
			prefixCount: 2,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSSync, candidate, "")
			},
		},
		{
			name:        "install-complete-candidate",
			prefixCount: 3,
			plan: func(_ string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSInstallDirectory, "", resumestate.ControlDirectoryName,
				)
			},
		},
		{
			name:        "reopen-recovered-install",
			prefixCount: 3,
			plan: func(_ string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSOpenDirectory, "", resumestate.ControlDirectoryName,
				)
			},
		},
		{
			name:        "reopen-new-install",
			prefixCount: -1,
			plan: func(_ string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSOpenDirectory, "", resumestate.ControlDirectoryName,
				)
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, root, nil)
			candidateName := v3RecoveryBootstrapCandidateName(t, byte(0xb0+index))
			var expectedControl resumestate.Control
			if test.prefixCount >= 0 {
				platform, err := openOutputV3Platform(root, false)
				if err != nil {
					t.Fatal(err)
				}
				expectedControl, err = authority.newControl(platform)
				if err != nil {
					t.Fatal(err)
				}
				candidate, err := platform.Root().CreateDirectory(candidateName, true)
				if err != nil {
					t.Fatal(err)
				}
				v3RecoveryBuildBootstrapPrefix(t, authority, candidate, expectedControl, test.prefixCount)
				if err := errors.Join(candidate.Close(), platform.Close()); err != nil {
					t.Fatal(err)
				}
			}

			plan := test.plan(candidateName)
			faulted := openOutputV3ControlSessionFaultPlatform(t, root, plan)
			namespace, _, openErr := authority.openOrBootstrapControl(faulted)
			if namespace != nil {
				_ = namespace.Close()
				t.Fatal("faulted bootstrap returned a control namespace")
			}
			if !errors.Is(openErr, errOutputV3ControlSessionInjected) {
				t.Fatalf("bootstrap authority cut = %v, want injected failure", openErr)
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
			recovered, _, err := authority.openOrBootstrapControl(recoveryPlatform)
			if err != nil {
				_ = recoveryPlatform.Close()
				t.Fatalf("recover bootstrap authority cut: %v", err)
			}
			if test.prefixCount >= 1 && recovered.control != expectedControl {
				t.Fatalf("recovered control = %#v, want %#v", recovered.control, expectedControl)
			}
			candidates, listErr := recoveryPlatform.Root().NamesWithPrefix(
				resumestate.BootstrapCandidatePrefix, outputRootInspectionLimit,
			)
			if listErr != nil || len(candidates) != 0 {
				t.Fatalf("candidates after recovery = %v, %v", candidates, listErr)
			}
			if err := errors.Join(recovered.Close(), recoveryPlatform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3BootstrapCandidateInspectionFailuresPreserveRecoverableAuthority(t *testing.T) {
	tests := []struct {
		name        string
		prefixCount int
		plan        func(string) *outputV3ControlSessionFaultPlan
	}{
		{
			name:        "open-candidate",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSOpenDirectory, "", candidate)
			},
		},
		{
			name:        "classify-control-record",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSClassifyEntry, candidate, resumestate.ControlRecordName,
				)
			},
		},
		{
			name:        "enumerate-control-temporaries",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSNamesWithPrefix, candidate, resumestate.ControlUpdateTemporaryPrefix,
				)
			},
		},
		{
			name:        "enumerate-structural-cut",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSNames, candidate, "")
			},
		},
		{
			name:        "enumerate-completed-cut",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(outputV3CSNames, candidate, "")
				plan.atCall = 2
				return plan
			},
		},
		{
			name:        "revalidate-root-binding",
			prefixCount: 3,
			plan: func(_ string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(outputV3CSRootBinding, "", "")
				plan.atCall = 2
				return plan
			},
		},
		{
			name:        "open-coordinator-record",
			prefixCount: 2,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSOpenFile, candidate, resumestate.CoordinatorLockName,
				)
			},
		},
		{
			name:        "validate-complete-children",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(outputV3CSNames, candidate, "")
				plan.atCall = 3
				return plan
			},
		},
		{
			name:        "open-sessions-child",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSOpenDirectory, candidate, resumestate.SessionsDirectoryName,
				)
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, root, nil)
			candidateName := v3RecoveryBootstrapCandidateName(t, byte(0x80+index))
			platform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			expectedControl, err := authority.newControl(platform)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := platform.Root().CreateDirectory(candidateName, true)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryBuildBootstrapPrefix(t, authority, candidate, expectedControl, test.prefixCount)
			if err := errors.Join(candidate.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}

			plan := test.plan(candidateName)
			faulted := openOutputV3ControlSessionFaultPlatform(t, root, plan)
			inspected, _, observation, inspectErr := inspectBootstrapCandidate(
				authority, faulted.Root(), candidateName, faulted,
			)
			if inspected != nil {
				_ = inspected.Close()
			}
			if observation != resumestate.BootstrapCandidateUnsafe ||
				!errors.Is(inspectErr, errOutputV3ControlSessionInjected) {
				t.Fatalf("candidate inspection = (%v, %v), want unsafe injected failure", observation, inspectErr)
			}
			plan.requireFired(t)
			if err := faulted.Close(); err != nil {
				t.Fatal(err)
			}

			recoveryPlatform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			recovered, _, err := authority.openOrBootstrapControl(recoveryPlatform)
			if err != nil || recovered.control != expectedControl {
				_ = recoveryPlatform.Close()
				t.Fatalf("recover inspected candidate = (same=%t, err=%v)",
					recovered != nil && recovered.control == expectedControl, err)
			}
			candidates, listErr := recoveryPlatform.Root().NamesWithPrefix(
				resumestate.BootstrapCandidatePrefix, outputRootInspectionLimit,
			)
			if listErr != nil || len(candidates) != 0 {
				t.Fatalf("candidates after inspection recovery = %v, %v", candidates, listErr)
			}
			if err := errors.Join(recovered.Close(), recoveryPlatform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3BootstrapCleanupAuthorityRacesPreserveRestartableCuts(t *testing.T) {
	tests := []struct {
		name string
		plan func(string) *outputV3ControlSessionFaultPlan
	}{
		{
			name: "inspect-before-cleanup",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSOpenDirectory, "", candidate)
			},
		},
		{
			name: "enumerate-before-retirement",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(outputV3CSNames, candidate, "")
				plan.atCall = 4
				return plan
			},
		},
		{
			name: "pin-open-before-retirement",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(outputV3CSOpenDirectory, "", candidate)
				plan.atCall = 2
				return plan
			},
		},
		{
			name: "pin-replaced-before-retirement",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return &outputV3ControlSessionFaultPlan{
					operation: outputV3CSSameDirectory, path: candidate, forceDifferent: true,
				}
			},
		},
		{
			name: "pin-replaced-before-root-removal",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return &outputV3ControlSessionFaultPlan{
					operation: outputV3CSSameDirectory, path: candidate,
					atCall: 4, forceDifferent: true,
				}
			},
		},
		{
			name: "enumerate-before-root-removal",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(outputV3CSNames, candidate, "")
				plan.atCall = 5
				return plan
			},
		},
		{
			name: "open-sessions-for-retirement",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(
					outputV3CSOpenDirectory, candidate, resumestate.SessionsDirectoryName,
				)
				plan.atCall = 2
				return plan
			},
		},
		{
			name: "enumerate-sessions-for-retirement",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSNames, candidate+"/"+resumestate.SessionsDirectoryName, "",
				)
			},
		},
		{
			name: "open-coordinator-for-retirement",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(
					outputV3CSOpenFile, candidate, resumestate.CoordinatorLockName,
				)
				plan.atCall = 3
				return plan
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
			installed, _, err := authority.openOrBootstrapControl(platform)
			if err != nil {
				t.Fatal(err)
			}
			expectedControl := installed.control
			if err := installed.Close(); err != nil {
				t.Fatal(err)
			}
			candidateName := v3RecoveryBootstrapCandidateName(t, byte(0x60+index))
			candidate, err := platform.Root().CreateDirectory(candidateName, true)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryBuildBootstrapPrefix(t, authority, candidate, expectedControl, 3)
			if err := errors.Join(candidate.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}

			plan := test.plan(candidateName)
			faulted := openOutputV3ControlSessionFaultPlatform(t, root, plan)
			namespace, _, openErr := authority.openOrBootstrapControl(faulted)
			if namespace != nil {
				_ = namespace.Close()
				t.Fatal("cleanup authority race returned a control namespace")
			}
			if plan.failure != nil && !errors.Is(openErr, plan.failure) {
				t.Fatalf("cleanup authority race = %v, want %v", openErr, plan.failure)
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
			recovered, created, err := authority.openOrBootstrapControl(recoveryPlatform)
			if err != nil || created || recovered.control != expectedControl {
				_ = recoveryPlatform.Close()
				t.Fatalf("recover cleanup authority race = (created=%t, same=%t, err=%v)",
					created, recovered != nil && recovered.control == expectedControl, err)
			}
			candidates, listErr := recoveryPlatform.Root().NamesWithPrefix(
				resumestate.BootstrapCandidatePrefix, outputRootInspectionLimit,
			)
			if listErr != nil || len(candidates) != 0 {
				t.Fatalf("candidates after cleanup-race recovery = %v, %v", candidates, listErr)
			}
			if err := errors.Join(recovered.Close(), recoveryPlatform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3MultipleCompleteBootstrapCandidatesUseStableDirectoryWinner(t *testing.T) {
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	control, err := authority.newControl(platform)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{
		v3RecoveryBootstrapCandidateName(t, 0xe2),
		v3RecoveryBootstrapCandidateName(t, 0xe1),
	}
	slices.Sort(names)
	candidates := make(map[string]outputV3Directory, len(names))
	t.Cleanup(func() {
		var closeErr error
		for _, candidate := range candidates {
			closeErr = errors.Join(closeErr, candidate.Close())
		}
		if closeErr != nil {
			t.Errorf("close retained bootstrap candidates: %v", closeErr)
		}
	})
	for _, name := range names {
		candidate, err := platform.Root().CreateDirectory(name, true)
		if err != nil {
			t.Fatal(err)
		}
		v3RecoveryBuildBootstrapPrefix(t, authority, candidate, control, 3)
		candidates[name] = candidate
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}

	platform, err = openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	installed, created, err := authority.openOrBootstrapControl(platform)
	if err != nil || !created || installed.control != control {
		t.Fatalf("multi-candidate recovery = (created=%t, same=%t, err=%v)",
			created, installed != nil && installed.control == control, err)
	}
	stableWinner, err := installed.directory.SameDirectory(candidates[names[0]])
	if err != nil || !stableWinner {
		t.Fatalf("installed candidate matches stable winner = %t, %v; want true", stableWinner, err)
	}
	for _, name := range names {
		kind, err := platform.Root().ObserveEntry(name)
		if err != nil || kind != outputV3EntryAbsent {
			t.Fatalf("candidate %q after recovery = (%v, %v)", name, kind, err)
		}
	}
	if err := errors.Join(installed.Close(), platform.Close()); err != nil {
		t.Fatal(err)
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

	for attempt := 0; attempt < 2; attempt++ {
		platform, err = openOutputV3Platform(root, false)
		if err != nil {
			t.Fatal(err)
		}
		namespace, _, openErr := authority.openOrBootstrapControl(platform)
		if namespace != nil {
			_ = namespace.Close()
		}
		if !errors.Is(openErr, errOutputRootUnsafe) {
			t.Fatalf("ambiguous control attempt %d = %v, want root block", attempt, openErr)
		}
		outputV3ControlSessionRequireFault(
			t, openErr, transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe,
		)
		if kind, observeErr := platform.Root().ObserveEntry(resumestate.ControlDirectoryName); observeErr != nil ||
			kind != outputV3EntryAbsent {
			t.Fatalf("divergent candidate attempt %d installed control = (%v, %v)", attempt, kind, observeErr)
		}
		for _, name := range names {
			kind, observeErr := platform.Root().ObserveEntry(name)
			if observeErr != nil || kind != outputV3EntryDirectory {
				t.Fatalf("divergent candidate %q attempt %d = (%v, %v), want preserved", name, attempt, kind, observeErr)
			}
		}
		if err := platform.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOutputV3InstalledControlSchemaCorruptionBlocksWholeRootWithoutSessionMutation(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	sessionIDs := &v3RecoverySessionIDs{}
	authority := v3RecoveryAuthority(t, root, sessionIDs)
	opened := v3RecoveryOpen(t, authority, root, selection)
	headerPath := filepath.Join(
		v3RecoverySessionPath(root, selection, opened.Session.SessionID()),
		resumestate.HeaderRecordName,
	)
	headerBefore, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatal(err)
	}
	v3RecoveryCloseSession(t, opened.Session)
	extraPath := filepath.Join(root, resumestate.ControlDirectoryName, "unexpected-control-child")
	if err := os.WriteFile(extraPath, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range []transfer.OutputSelection{selection, v3RecoverySelection(t, true, 1)} {
		session, openErr := authority.OpenSelection(context.Background(), candidate)
		if session != nil {
			_ = session.(*filesystemOutputSession).closeHandles()
			t.Fatal("corrupt global control returned a session")
		}
		outputV3ControlSessionRequireFault(
			t, openErr, transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe,
		)
	}
	headerAfter, err := os.ReadFile(headerPath)
	if err != nil || !bytes.Equal(headerAfter, headerBefore) {
		t.Fatalf("global schema fault changed session header = %x, %v; want %x", headerAfter, err, headerBefore)
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
			installed, _, err := authority.openOrBootstrapControl(platform)
			if err != nil {
				t.Fatal(err)
			}
			expectedControl := installed.control
			if err := errors.Join(installed.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}

			faulted := openOutputV3ControlSessionFaultPlatform(t, root, test.plan)
			namespace, _, openErr := authority.openOrBootstrapControl(faulted)
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
			recovered, created, err := authority.openOrBootstrapControl(recoveryPlatform)
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
		installed, _, err := authority.openOrBootstrapControl(platform)
		if err != nil {
			t.Fatal(err)
		}
		expectedControl := installed.control
		if err := errors.Join(installed.Close(), platform.Close()); err != nil {
			t.Fatal(err)
		}
		lockPath := filepath.Join(root, resumestate.ControlDirectoryName, resumestate.CoordinatorLockName)
		if err := os.WriteFile(lockPath, []byte{1}, 0o600); err != nil {
			t.Fatal(err)
		}

		faulted, err := openOutputV3Platform(root, false)
		if err != nil {
			t.Fatal(err)
		}
		namespace, _, openErr := authority.openOrBootstrapControl(faulted)
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
		if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		recoveryPlatform, err := openOutputV3Platform(root, false)
		if err != nil {
			t.Fatal(err)
		}
		recovered, created, err := authority.openOrBootstrapControl(recoveryPlatform)
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

func TestOutputV3SessionSchemaCorruptionIsIntentScopedAndNonMutating(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "unexpected-child",
			mutate: func(t *testing.T, sessionPath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(sessionPath, "unexpected"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "files-wrong-type",
			mutate: outputV3ControlSessionReplaceDirectoryWithFile(resumestate.FilesDirectoryName),
		},
		{
			name:   "anchors-wrong-type",
			mutate: outputV3ControlSessionReplaceDirectoryWithFile(resumestate.AnchorsDirectoryName),
		},
		{
			name:   "stages-wrong-type",
			mutate: outputV3ControlSessionReplaceDirectoryWithFile(resumestate.StagesDirectoryName),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			sessionIDs := &v3RecoverySessionIDs{}
			authority := v3RecoveryAuthority(t, root, sessionIDs)
			opened := v3RecoveryOpen(t, authority, root, selection)
			sessionPath := v3RecoverySessionPath(root, selection, opened.Session.SessionID())
			headerPath := filepath.Join(sessionPath, resumestate.HeaderRecordName)
			headerBefore, err := os.ReadFile(headerPath)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryCloseSession(t, opened.Session)
			test.mutate(t, sessionPath)

			session, openErr := authority.OpenSelection(context.Background(), selection)
			if session != nil {
				_ = session.(*filesystemOutputSession).closeHandles()
				t.Fatal("corrupt session schema returned a session")
			}
			if !errors.Is(openErr, errOutputIntentUnsafe) {
				t.Fatalf("session schema error = %v, want intent unsafe", openErr)
			}
			outputV3ControlSessionRequireFault(
				t, openErr, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
			)
			headerAfter, err := os.ReadFile(headerPath)
			if err != nil || !bytes.Equal(headerAfter, headerBefore) {
				t.Fatalf("session schema fault changed header = %x, %v; want %x", headerAfter, err, headerBefore)
			}

			other := v3RecoverySelection(t, true, 1)
			unrelated := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, other)
			v3RecoveryCloseSession(t, unrelated.Session)
		})
	}
}

func TestOutputV3RestartResumesEveryNonterminalSessionCut(t *testing.T) {
	for _, lifecycle := range []resumestate.SessionLifecycle{
		resumestate.SessionPausing,
		resumestate.SessionPaused,
		resumestate.SessionPausedNeedsAttention,
	} {
		t.Run(lifecycle.String(), func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			sessionID := opened.Session.SessionID()
			if err := opened.Session.installLifecycle(resumestate.SessionPausing); err != nil {
				t.Fatal(err)
			}
			if lifecycle != resumestate.SessionPausing {
				if err := opened.Session.installLifecycle(lifecycle); err != nil {
					t.Fatal(err)
				}
			}
			v3RecoveryCloseSession(t, opened.Session)

			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			if reopened.Session.SessionID() != sessionID ||
				reopened.Session.stateSnapshot().Header().Lifecycle() != resumestate.SessionActive {
				t.Fatalf("resume %v = (session=%s, lifecycle=%v)", lifecycle,
					reopened.Session.SessionID(), reopened.Session.stateSnapshot().Header().Lifecycle())
			}
			v3RecoveryCloseSession(t, reopened.Session)
		})
	}
}

func TestOutputV3RestartSettlementFailurePreservesLifecycleCut(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	sessionIDs := &v3RecoverySessionIDs{}
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	if err := opened.Session.installLifecycle(resumestate.SessionPausing); err != nil {
		t.Fatal(err)
	}
	sessionID := opened.Session.SessionID()
	headerPath := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID), resumestate.HeaderRecordName,
	)
	headerBefore, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatal(err)
	}
	v3RecoveryCloseSession(t, opened.Session)
	sessionNamespacePath := outputV3ControlSessionSessionPath(selection, sessionID)
	plan := outputV3ControlSessionFailure(
		outputV3CSCreateFile, sessionNamespacePath, resumestate.HeaderUpdateTemporaryPrefix,
	)
	plan.namePrefix = true
	authority := v3RecoveryAuthority(t, root, sessionIDs)
	authority.platformFactory = outputV3ControlSessionFaultFactory(plan)
	session, openErr := authority.OpenSelection(context.Background(), selection)
	if session != nil {
		_ = session.(*filesystemOutputSession).closeHandles()
		t.Fatal("failed lifecycle settlement returned a session")
	}
	if !errors.Is(openErr, errOutputV3ControlSessionInjected) {
		t.Fatalf("restart settlement failure = %v, want injected failure", openErr)
	}
	outputV3ControlSessionRequireFault(
		t, openErr, transfer.OutputFaultSession, transfer.OutputFaultStateIO,
	)
	plan.requireFired(t)
	headerAfter, err := os.ReadFile(headerPath)
	if err != nil || !bytes.Equal(headerAfter, headerBefore) {
		t.Fatalf("failed restart settlement changed header = %x, %v; want %x", headerAfter, err, headerBefore)
	}

	reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	if reopened.Session.stateSnapshot().Header().Lifecycle() != resumestate.SessionActive {
		t.Fatalf("lifecycle after settlement recovery = %v", reopened.Session.stateSnapshot().Header().Lifecycle())
	}
	v3RecoveryCloseSession(t, reopened.Session)
}

func TestOutputV3LockedSessionRevalidationRejectsReplacedAncestryWithoutMutation(t *testing.T) {
	for _, target := range []string{"intent", "session"} {
		t.Run(target, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			sessionID := opened.Session.SessionID()
			headerPath := filepath.Join(
				v3RecoverySessionPath(root, selection, sessionID), resumestate.HeaderRecordName,
			)
			headerBefore, err := os.ReadFile(headerPath)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryCloseSession(t, opened.Session)

			intentPath := strings.Join([]string{
				resumestate.ControlDirectoryName,
				resumestate.SessionsDirectoryName,
				resumestate.ResumeNamespaceName(selection.ResumeIntent()),
			}, "/")
			path := intentPath
			if target == "session" {
				path += "/" + resumestate.SessionDirectoryName(sessionID)
			}
			plan := &outputV3ControlSessionFaultPlan{
				operation: outputV3CSSameDirectory, path: path, forceDifferent: true,
			}
			authority := v3RecoveryAuthority(t, root, sessionIDs)
			authority.platformFactory = outputV3ControlSessionFaultFactory(plan)
			session, openErr := authority.OpenSelection(context.Background(), selection)
			if session != nil {
				_ = session.(*filesystemOutputSession).closeHandles()
				t.Fatal("replaced session ancestry returned a session")
			}
			if !errors.Is(openErr, errOutputIntentUnsafe) {
				t.Fatalf("replaced %s ancestry error = %v", target, openErr)
			}
			outputV3ControlSessionRequireFault(
				t, openErr, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
			)
			plan.requireFired(t)
			headerAfter, err := os.ReadFile(headerPath)
			if err != nil || !bytes.Equal(headerAfter, headerBefore) {
				t.Fatalf("revalidation failure changed header = %x, %v; want %x", headerAfter, err, headerBefore)
			}

			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			v3RecoveryCloseSession(t, reopened.Session)
		})
	}
}

func TestOutputV3SessionBoundaryContractsRemainTotal(t *testing.T) {
	var session *filesystemOutputSession
	if !session.SessionID().IsZero() || session.Capabilities() != (transfer.OutputCapabilities{}) ||
		session.stateSnapshot().Header().Lifecycle() != 0 {
		t.Fatal("nil session exposed nonzero identity, capabilities, or state")
	}
	if err := session.beginOperation(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil session begin error = %v", err)
	}
	session.poisonState()
	if err := session.closeHandles(); err != nil {
		t.Fatalf("nil session close = %v", err)
	}
	for name, valid := range map[string]bool{
		"00": true, "9f": true, "af": true, "": false, "0": false, "000": false,
		"AF": false, "g0": false, "-1": false,
	} {
		if actual := validStateShard(name); actual != valid {
			t.Fatalf("state shard %q valid=%t, want %t", name, actual, valid)
		}
	}
}

func TestOutputV3SessionOpenAuthorityFailuresRemainPreciselyScoped(t *testing.T) {
	tests := []struct {
		name      string
		context   func() context.Context
		plan      *outputV3ControlSessionFaultPlan
		wantScope transfer.OutputFaultScope
		wantCode  transfer.OutputFaultCode
		wantCause error
	}{
		{
			name: "canceled-before-coordinator",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantCause: context.Canceled,
		},
		{
			name:    "coordinator-lock-io",
			context: context.Background,
			plan: outputV3ControlSessionFailure(
				outputV3CSAcquireLock, resumestate.ControlDirectoryName, resumestate.CoordinatorLockName,
			),
			wantScope: transfer.OutputFaultRoot,
			wantCode:  transfer.OutputFaultStateIO,
		},
		{
			name:    "coordinator-lock-recreated",
			context: context.Background,
			plan: &outputV3ControlSessionFaultPlan{
				operation: outputV3CSAcquireLock, path: resumestate.ControlDirectoryName,
				name: resumestate.CoordinatorLockName, forceCreated: true,
			},
			wantScope: transfer.OutputFaultRoot,
			wantCode:  transfer.OutputFaultNamespaceUnsafe,
		},
		{
			name:    "intent-enumeration",
			context: context.Background,
			plan: outputV3ControlSessionFailure(
				outputV3CSNames,
				resumestate.ControlDirectoryName+"/"+resumestate.SessionsDirectoryName,
				"",
			),
			wantScope: transfer.OutputFaultSession,
			wantCode:  transfer.OutputFaultNamespaceUnsafe,
		},
		{
			name:    "session-enumeration",
			context: context.Background,
			plan: outputV3ControlSessionFailure(
				outputV3CSNames,
				resumestate.ControlDirectoryName+"/"+resumestate.SessionsDirectoryName+"/{intent}",
				"",
			),
			wantScope: transfer.OutputFaultSession,
			wantCode:  transfer.OutputFaultNamespaceUnsafe,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			if test.plan != nil && strings.Contains(test.plan.path, "{intent}") {
				test.plan.path = strings.ReplaceAll(
					test.plan.path, "{intent}", resumestate.ResumeNamespaceName(selection.ResumeIntent()),
				)
			}
			authority := v3RecoveryAuthority(t, root, nil)
			platform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			control, _, err := authority.openOrBootstrapControl(platform)
			if err != nil {
				_ = platform.Close()
				t.Fatal(err)
			}
			admission, err := preflightOutputSelectionAdmission(platform, selection)
			if err != nil {
				_ = control.Close()
				_ = platform.Close()
				t.Fatal(err)
			}
			platform = wrapOutputV3ControlSessionFaultPlatform(platform, test.plan)
			validation, err := prepareOutputSelectionAncestry(platform, selection)
			if err != nil {
				_ = control.Close()
				_ = platform.Close()
				t.Fatal(err)
			}
			admission.ancestry = validation.snapshot
			admission.validation = validation
			control.directory = wrapOutputV3ControlSessionFaultDirectory(
				control.directory, test.plan, resumestate.ControlDirectoryName,
			)
			control.sessions = wrapOutputV3ControlSessionFaultDirectory(
				control.sessions, test.plan,
				resumestate.ControlDirectoryName+"/"+resumestate.SessionsDirectoryName,
			)

			session, _, _, openErr := authority.openOutputSession(test.context(), platform, control, admission)
			if session != nil {
				if err := errors.Join(
					session.closeHandles(), authority.closeOutputAdmissionAncestry(&admission),
				); err != nil {
					t.Fatal(err)
				}
				t.Fatal("authority-boundary failure returned a session")
			}
			if err := errors.Join(
				authority.closeOutputAdmissionAncestry(&admission), control.Close(), platform.Close(),
			); err != nil {
				t.Fatal(err)
			}
			if test.wantCause != nil {
				if !errors.Is(openErr, test.wantCause) {
					t.Fatalf("open error = %v, want %v", openErr, test.wantCause)
				}
				return
			}
			outputV3ControlSessionRequireFault(t, openErr, test.wantScope, test.wantCode)
			test.plan.requireFired(t)
		})
	}
}

func TestOutputV3SessionLockAuthorityCutsBlockWithoutHeaderMutation(t *testing.T) {
	tests := []struct {
		name      string
		plan      func(string) *outputV3ControlSessionFaultPlan
		wrongType bool
		wantCode  transfer.OutputFaultCode
	}{
		{
			name: "observation-io",
			plan: func(path string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSClassifyEntry, path, resumestate.SessionLockName)
			},
			wantCode: transfer.OutputFaultNamespaceUnsafe,
		},
		{name: "wrong-type", wrongType: true, wantCode: transfer.OutputFaultNamespaceUnsafe},
		{
			name: "acquire-io",
			plan: func(path string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSAcquireLock, path, resumestate.SessionLockName)
			},
			wantCode: transfer.OutputFaultStateIO,
		},
		{
			name: "acquire-recreated",
			plan: func(path string) *outputV3ControlSessionFaultPlan {
				return &outputV3ControlSessionFaultPlan{
					operation: outputV3CSAcquireLock, path: path,
					name: resumestate.SessionLockName, forceCreated: true,
				}
			},
			wantCode: transfer.OutputFaultNamespaceUnsafe,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			sessionID := opened.Session.SessionID()
			sessionPath := v3RecoverySessionPath(root, selection, sessionID)
			headerPath := filepath.Join(sessionPath, resumestate.HeaderRecordName)
			headerBefore, err := os.ReadFile(headerPath)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryCloseSession(t, opened.Session)

			lockPath := filepath.Join(sessionPath, resumestate.SessionLockName)
			if test.wrongType {
				if err := os.Remove(lockPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(lockPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			logicalPath := outputV3ControlSessionSessionPath(selection, sessionID)
			var plan *outputV3ControlSessionFaultPlan
			if test.plan != nil {
				plan = test.plan(logicalPath)
			}
			authority := v3RecoveryAuthority(t, root, sessionIDs)
			authority.platformFactory = outputV3ControlSessionFaultFactory(plan)
			session, openErr := authority.OpenSelection(context.Background(), selection)
			if session != nil {
				_ = session.(*filesystemOutputSession).closeHandles()
				t.Fatal("invalid session lock cut returned a session")
			}
			outputV3ControlSessionRequireFault(
				t, openErr, transfer.OutputFaultSession, test.wantCode,
			)
			if plan != nil {
				plan.requireFired(t)
			}
			headerAfter, err := os.ReadFile(headerPath)
			if err != nil || !bytes.Equal(headerAfter, headerBefore) {
				t.Fatalf("lock authority failure changed header = %x, %v; want %x", headerAfter, err, headerBefore)
			}

			if test.wrongType {
				return
			}
			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			v3RecoveryCloseSession(t, reopened.Session)
		})
	}
}

func outputV3ControlSessionReplaceDirectoryWithFile(name string) func(*testing.T, string) {
	return func(t *testing.T, sessionPath string) {
		t.Helper()
		path := filepath.Join(sessionPath, name)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("wrong-type"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func outputV3ControlSessionRequireFault(
	t *testing.T,
	err error,
	scope transfer.OutputFaultScope,
	code transfer.OutputFaultCode,
) {
	t.Helper()
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != scope || fault.Code() != code {
		t.Fatalf("output fault = %#v in %v, want scope=%v code=%v", fault, err, scope, code)
	}
}

func outputV3ControlSessionSessionPath(
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
) string {
	return strings.Join([]string{
		resumestate.ControlDirectoryName,
		resumestate.SessionsDirectoryName,
		resumestate.ResumeNamespaceName(selection.ResumeIntent()),
		resumestate.SessionDirectoryName(sessionID),
	}, "/")
}

const (
	outputV3CSRootBinding      = "root-binding"
	outputV3CSNames            = "names"
	outputV3CSNamesWithPrefix  = "names-with-prefix"
	outputV3CSObserveEntry     = "observe-entry"
	outputV3CSClassifyEntry    = "classify-entry"
	outputV3CSOpenDirectory    = "open-directory"
	outputV3CSCreateDirectory  = "create-directory"
	outputV3CSInstallDirectory = "install-directory"
	outputV3CSRemoveDirectory  = "remove-directory"
	outputV3CSOpenFile         = "open-file"
	outputV3CSCreateFile       = "create-file"
	outputV3CSRemoveFile       = "remove-file"
	outputV3CSAcquireLock      = "acquire-lock"
	outputV3CSSameDirectory    = "same-directory"
	outputV3CSSync             = "sync"
)

type outputV3ControlSessionFaultPlan struct {
	mu             sync.Mutex
	operation      string
	path           string
	name           string
	namePrefix     bool
	atCall         int
	seen           int
	fired          int
	failure        error
	forceDifferent bool
	forceCreated   bool
}

func outputV3ControlSessionFailure(operation, path, name string) *outputV3ControlSessionFaultPlan {
	return &outputV3ControlSessionFaultPlan{
		operation: operation, path: path, name: name, failure: errOutputV3ControlSessionInjected,
	}
}

func (plan *outputV3ControlSessionFaultPlan) trigger(operation, path, name string) (bool, error) {
	if plan == nil {
		return false, nil
	}
	plan.mu.Lock()
	defer plan.mu.Unlock()
	nameMatches := name == plan.name
	if plan.namePrefix {
		nameMatches = strings.HasPrefix(name, plan.name)
	}
	if operation != plan.operation || path != plan.path || !nameMatches {
		return false, nil
	}
	plan.seen++
	atCall := plan.atCall
	if atCall == 0 {
		atCall = 1
	}
	if plan.seen != atCall {
		return false, nil
	}
	plan.fired++
	return true, plan.failure
}

func (plan *outputV3ControlSessionFaultPlan) requireFired(t *testing.T) {
	t.Helper()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.fired != 1 {
		t.Fatalf("fault %s path=%q name=%q fired %d times, want once", plan.operation, plan.path, plan.name, plan.fired)
	}
}

type outputV3ControlSessionFaultPlatform struct {
	outputV3Platform
	root outputV3Directory
	plan *outputV3ControlSessionFaultPlan
}

func openOutputV3ControlSessionFaultPlatform(
	t *testing.T,
	root string,
	plan *outputV3ControlSessionFaultPlan,
) outputV3Platform {
	t.Helper()
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	return wrapOutputV3ControlSessionFaultPlatform(platform, plan)
}

func wrapOutputV3ControlSessionFaultPlatform(
	platform outputV3Platform,
	plan *outputV3ControlSessionFaultPlan,
) outputV3Platform {
	return &outputV3ControlSessionFaultPlatform{
		outputV3Platform: platform,
		root:             wrapOutputV3ControlSessionFaultDirectory(platform.Root(), plan, ""),
		plan:             plan,
	}
}

func outputV3ControlSessionFaultFactory(
	plan *outputV3ControlSessionFaultPlan,
) func(string, bool) (outputV3Platform, error) {
	return func(path string, create bool) (outputV3Platform, error) {
		platform, err := openOutputV3Platform(path, create)
		if err != nil {
			return nil, err
		}
		return wrapOutputV3ControlSessionFaultPlatform(platform, plan), nil
	}
}

func (platform *outputV3ControlSessionFaultPlatform) Root() outputV3Directory { return platform.root }

func (platform *outputV3ControlSessionFaultPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return wrapOutputV3ControlSessionFaultDirectory(root, platform.plan, "")
		},
	)
}

func (platform *outputV3ControlSessionFaultPlatform) RootBinding() (resumestate.OutputRootBinding, error) {
	if matched, err := platform.plan.trigger(outputV3CSRootBinding, "", ""); matched {
		return resumestate.OutputRootBinding{}, err
	}
	return platform.outputV3Platform.RootBinding()
}

type outputV3ControlSessionFaultDirectory struct {
	outputV3Directory
	plan *outputV3ControlSessionFaultPlan
	path string
}

func wrapOutputV3ControlSessionFaultDirectory(
	directory outputV3Directory,
	plan *outputV3ControlSessionFaultPlan,
	path string,
) outputV3Directory {
	if directory == nil {
		return nil
	}
	return &outputV3ControlSessionFaultDirectory{outputV3Directory: directory, plan: plan, path: path}
}

func unwrapOutputV3ControlSessionFaultDirectory(directory outputV3Directory) outputV3Directory {
	if wrapped, ok := directory.(*outputV3ControlSessionFaultDirectory); ok {
		return wrapped.outputV3Directory
	}
	return directory
}

func outputV3ControlSessionChildPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

func (directory *outputV3ControlSessionFaultDirectory) Duplicate() (outputV3Directory, error) {
	duplicate, err := directory.outputV3Directory.Duplicate()
	return wrapOutputV3ControlSessionFaultDirectory(duplicate, directory.plan, directory.path), err
}

func (directory *outputV3ControlSessionFaultDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	if matched, err := directory.plan.trigger(outputV3CSSameDirectory, directory.path, ""); matched {
		if err != nil {
			return false, err
		}
		return !directory.plan.forceDifferent, nil
	}
	return directory.outputV3Directory.SameDirectory(unwrapOutputV3ControlSessionFaultDirectory(other))
}

func (directory *outputV3ControlSessionFaultDirectory) Names(limit int) ([]string, error) {
	if matched, err := directory.plan.trigger(outputV3CSNames, directory.path, ""); matched {
		return nil, err
	}
	return directory.outputV3Directory.Names(limit)
}

func (directory *outputV3ControlSessionFaultDirectory) NamesWithPrefix(
	prefix string,
	limit int,
) ([]string, error) {
	if matched, err := directory.plan.trigger(outputV3CSNamesWithPrefix, directory.path, prefix); matched {
		return nil, err
	}
	return directory.outputV3Directory.NamesWithPrefix(prefix, limit)
}

func (directory *outputV3ControlSessionFaultDirectory) ObserveEntry(name string) (outputV3EntryKind, error) {
	if matched, err := directory.plan.trigger(outputV3CSObserveEntry, directory.path, name); matched {
		return outputV3EntryAbsent, err
	}
	return directory.outputV3Directory.ObserveEntry(name)
}

func (directory *outputV3ControlSessionFaultDirectory) ClassifyExactEntry(
	name string,
) (outputV3EntryKind, bool, error) {
	if matched, err := directory.plan.trigger(outputV3CSClassifyEntry, directory.path, name); matched {
		return outputV3EntryAbsent, false, err
	}
	return directory.outputV3Directory.ClassifyExactEntry(name)
}

func (directory *outputV3ControlSessionFaultDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	if matched, err := directory.plan.trigger(outputV3CSOpenDirectory, directory.path, name); matched {
		return nil, err
	}
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	return wrapOutputV3ControlSessionFaultDirectory(
		opened, directory.plan, outputV3ControlSessionChildPath(directory.path, name),
	), err
}

func (directory *outputV3ControlSessionFaultDirectory) OpenPinnedDirectory(
	expected outputV3EntryRef,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenPinnedDirectory(expected, private)
	return wrapOutputV3ControlSessionFaultDirectory(opened, directory.plan, directory.path), err
}

func (directory *outputV3ControlSessionFaultDirectory) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	if matched, err := directory.plan.trigger(outputV3CSCreateDirectory, directory.path, name); matched {
		return nil, err
	}
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	return wrapOutputV3ControlSessionFaultDirectory(
		created, directory.plan, outputV3ControlSessionChildPath(directory.path, name),
	), err
}

func (directory *outputV3ControlSessionFaultDirectory) InstallDirectoryNoReplace(
	candidate outputV3Directory,
	name string,
) (outputV3Directory, error) {
	if matched, err := directory.plan.trigger(outputV3CSInstallDirectory, directory.path, name); matched {
		return nil, err
	}
	installed, err := directory.outputV3Directory.InstallDirectoryNoReplace(
		unwrapOutputV3ControlSessionFaultDirectory(candidate), name,
	)
	return wrapOutputV3ControlSessionFaultDirectory(
		installed, directory.plan, outputV3ControlSessionChildPath(directory.path, name),
	), err
}

func (directory *outputV3ControlSessionFaultDirectory) RemoveDirectory(
	name string,
	expected outputV3Directory,
) error {
	if matched, err := directory.plan.trigger(outputV3CSRemoveDirectory, directory.path, name); matched {
		return err
	}
	return directory.outputV3Directory.RemoveDirectory(
		name, unwrapOutputV3ControlSessionFaultDirectory(expected),
	)
}

func (directory *outputV3ControlSessionFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputV3File, error) {
	if matched, err := directory.plan.trigger(outputV3CSOpenFile, directory.path, name); matched {
		return nil, err
	}
	return directory.outputV3Directory.OpenFile(name, private, writable)
}

func (directory *outputV3ControlSessionFaultDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputV3File, error) {
	if matched, err := directory.plan.trigger(outputV3CSCreateFile, directory.path, name); matched {
		return nil, err
	}
	return directory.outputV3Directory.CreateFile(name, private, size)
}

func (directory *outputV3ControlSessionFaultDirectory) RemoveFile(name string, expected outputV3File) error {
	if matched, err := directory.plan.trigger(outputV3CSRemoveFile, directory.path, name); matched {
		return err
	}
	return directory.outputV3Directory.RemoveFile(name, expected)
}

func (directory *outputV3ControlSessionFaultDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputV3Lock, bool, error) {
	if matched, err := directory.plan.trigger(outputV3CSAcquireLock, directory.path, name); matched {
		if err != nil || !directory.plan.forceCreated {
			return nil, false, err
		}
		lock, _, lockErr := directory.outputV3Directory.AcquireLock(name, existingOnly)
		return lock, true, lockErr
	}
	return directory.outputV3Directory.AcquireLock(name, existingOnly)
}

func (directory *outputV3ControlSessionFaultDirectory) Sync() error {
	if matched, err := directory.plan.trigger(outputV3CSSync, directory.path, ""); matched {
		return err
	}
	return directory.outputV3Directory.Sync()
}

func (directory *outputV3ControlSessionFaultDirectory) ValidateCreateAuthority() error {
	if validator, ok := directory.outputV3Directory.(outputV3CreateAuthorityValidator); ok {
		return validator.ValidateCreateAuthority()
	}
	return nil
}

func (directory *outputV3ControlSessionFaultDirectory) ValidateMetadataAuthority() error {
	if validator, ok := directory.outputV3Directory.(outputV3MetadataAuthorityValidator); ok {
		return validator.ValidateMetadataAuthority()
	}
	return nil
}

func (directory *outputV3ControlSessionFaultDirectory) ValidatePublicEntryNames(names []string) error {
	if validator, ok := directory.outputV3Directory.(outputV3PublicEntryNamesValidator); ok {
		return validator.ValidatePublicEntryNames(names)
	}
	for _, name := range names {
		if err := directory.outputV3Directory.ValidatePublicEntryName(name); err != nil {
			return err
		}
	}
	return nil
}

package outputnamespace

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3RecoversEveryBootstrapCandidateConstructionCut(t *testing.T) {
	for childCount := 1; childCount <= 3; childCount++ {
		t.Run(outputV3BootstrapChildLabel(childCount), func(t *testing.T) {
			rootPath := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, rootPath, nil)
			platform, err := openOutputV3Platform(rootPath, false)
			if err != nil {
				t.Fatal(err)
			}
			control, err := authority.newControl(platform)
			if err != nil {
				t.Fatal(err)
			}
			name := v3RecoveryBootstrapCandidateName(t, 0x31)
			candidate, err := platform.Root().CreateDirectory(name, true)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryBuildBootstrapPrefix(t, authority, candidate, control, childCount)
			if err := errors.Join(candidate.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}
			platform, err = openOutputV3Platform(rootPath, false)
			if err != nil {
				t.Fatal(err)
			}

			result, err := authority.OpenOrBootstrapControl(platform)
			namespace := result.Namespace
			created := result.Disposition == ControlInstalled
			if err != nil {
				t.Fatal(err)
			}
			if !created || namespace.control != control {
				t.Fatalf("recovered control = (%v, %v), want original candidate control", created, namespace.control)
			}
			if kind, err := platform.Root().ObserveEntry(name); err != nil || kind != outputcap.EntryAbsent {
				t.Fatalf("candidate after installation = (%v, %v), want absent", kind, err)
			}
			if err := errors.Join(namespace.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3CompletesEveryBootstrapCandidateCleanupCut(t *testing.T) {
	cleanupOrder := []string{
		resumestate.SessionsDirectoryName,
		resumestate.CoordinatorLockName,
		resumestate.ControlRecordName,
	}
	for removed := 0; removed <= len(cleanupOrder); removed++ {
		t.Run(outputV3BootstrapCleanupLabel(removed), func(t *testing.T) {
			rootPath := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, rootPath, nil)
			platform, err := openOutputV3Platform(rootPath, false)
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

			name := v3RecoveryBootstrapCandidateName(t, byte(0x41+removed))
			candidate, err := platform.Root().CreateDirectory(name, true)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryBuildBootstrapPrefix(t, authority, candidate, control, 3)
			for _, child := range cleanupOrder[:removed] {
				if err := removeBootstrapChild(candidate, child); err != nil {
					t.Fatal(err)
				}
				if err := candidate.Sync(); err != nil {
					t.Fatal(err)
				}
			}
			if err := errors.Join(candidate.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}
			platform, err = openOutputV3Platform(rootPath, false)
			if err != nil {
				t.Fatal(err)
			}

			reopenedResult, err := authority.OpenOrBootstrapControl(platform)
			reopened := reopenedResult.Namespace
			created := reopenedResult.Disposition == ControlInstalled
			if err != nil {
				t.Fatal(err)
			}
			if created || reopened.control != control {
				t.Fatalf("installed control changed while cleaning candidate: created=%v", created)
			}
			if kind, err := platform.Root().ObserveEntry(name); err != nil || kind != outputcap.EntryAbsent {
				t.Fatalf("cleanup candidate = (%v, %v), want absent", kind, err)
			}
			if err := errors.Join(reopened.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3RejectsDecodableIntentAliasBeforeMutation(t *testing.T) {
	rootPath := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, rootPath, nil)
	selection := v3RecoverySelection(t, false, 0)
	platform, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	controlResult, err := authority.OpenOrBootstrapControl(platform)
	control := controlResult.Namespace
	if err != nil {
		t.Fatal(err)
	}
	canonical := resumestate.ResumeNamespaceName(selection.ResumeIntent())
	alias := strings.ToUpper(canonical)
	if alias == canonical {
		t.Fatal("test intent unexpectedly has no alphabetic hex digits")
	}
	created, err := control.sessions.CreateDirectory(alias, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(created.Sync(), control.sessions.Sync(), created.Close()); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenCanonicalIntent(control.sessions, selection.ResumeIntent()); !errors.Is(err, outputfault.ErrIntentUnsafe) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("open alias namespace error = %v, want %v", err, outputfault.ErrIntentUnsafe)
	}
	names, err := control.sessions.Names(2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{alias}) {
		t.Fatalf("resume namespace names = %v, want only pre-existing alias %q", names, alias)
	}
	if err := errors.Join(control.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestOutputV3RecoversEverySessionCandidateChildCutAndInstallsIt(t *testing.T) {
	for childCount := 1; childCount <= len(sessionCandidateChildren); childCount++ {
		t.Run(outputV3SessionCandidateChildLabel(childCount), func(t *testing.T) {
			rootPath := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, rootPath, nil)
			selection := v3RecoverySelection(t, false, 0)
			platform, err := openOutputV3Platform(rootPath, false)
			if err != nil {
				t.Fatal(err)
			}
			controlResult, err := authority.OpenOrBootstrapControl(platform)
			control := controlResult.Namespace
			if err != nil {
				t.Fatal(err)
			}
			intent, err := OpenCanonicalIntent(control.sessions, selection.ResumeIntent())
			if err != nil {
				t.Fatal(err)
			}
			sessionID := v3RecoveryIdentity16[transfer.OutputSessionID](0x61)
			candidateName := resumestate.SessionCandidateName(sessionID)
			candidate, err := intent.CreateDirectory(candidateName, true)
			if err != nil {
				t.Fatal(err)
			}
			ancestry := v3RecoveryAncestryBinding(t, control.control.OutputRoot(), selection)
			header, err := authority.newHeader(control.control, selection, ancestry, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryBuildSessionCandidatePrefix(t, authority, candidate, header, childCount)
			controlState := control.control
			if err := errors.Join(candidate.Close(), intent.Close(), control.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}
			platform, err = openOutputV3Platform(rootPath, false)
			if err != nil {
				t.Fatal(err)
			}
			control, err = authority.OpenInstalledControl(platform.Root(), platform)
			if err != nil {
				t.Fatal(err)
			}
			intent, err = OpenCanonicalIntent(control.sessions, selection.ResumeIntent())
			if err != nil {
				t.Fatal(err)
			}

			installedResult, err := authority.OpenOrCreateSession(
				intent, controlState, selection, ancestry,
			)
			installedName, installed := installedResult.Name, installedResult.Directory
			created := installedResult.Disposition == SessionInstalled
			if err != nil {
				t.Fatal(err)
			}
			if !created || installedName != resumestate.SessionDirectoryName(sessionID) {
				t.Fatalf("installed session = (%q, %v), want recovered candidate", installedName, created)
			}
			state, err := inspectOutputSessionCandidate(installed, header)
			if err != nil || state != sessionCandidateComplete {
				t.Fatalf("validate recovered session = (%v, %v)", state, err)
			}
			if kind, err := intent.ObserveEntry(candidateName); err != nil || kind != outputcap.EntryAbsent {
				t.Fatalf("session candidate after install = (%v, %v), want absent", kind, err)
			}
			if err := errors.Join(installed.Close(), intent.Close(), control.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func v3RecoveryBootstrapCandidateName(t *testing.T, value byte) string {
	t.Helper()
	nonce, err := resumestate.BootstrapNonceFromBytes(bytes.Repeat([]byte{value}, resumestate.BootstrapNonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	return resumestate.BootstrapCandidateName(nonce)
}

func v3RecoveryBuildBootstrapPrefix(
	t *testing.T,
	authority Controller,
	candidate outputcap.Directory,
	control resumestate.Control,
	childCount int,
) {
	t.Helper()
	if childCount >= 1 {
		encoded, err := resumestate.EncodeControl(control)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (Store{random: authority.random}).CreateRecord(
			candidate, resumestate.ControlRecordName, encoded, resumestate.MaxControlStateBytes,
		); err != nil {
			t.Fatal(err)
		}
	}
	if childCount >= 2 {
		lock, err := candidate.CreateFile(resumestate.CoordinatorLockName, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(lock.Sync(), candidate.Sync(), lock.Close()); err != nil {
			t.Fatal(err)
		}
	}
	if childCount >= 3 {
		sessions, err := candidate.CreateDirectory(resumestate.SessionsDirectoryName, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(sessions.Sync(), candidate.Sync(), sessions.Close()); err != nil {
			t.Fatal(err)
		}
	}
}

func v3RecoveryBuildSessionCandidatePrefix(
	t *testing.T,
	authority Controller,
	candidate outputcap.Directory,
	header resumestate.Header,
	childCount int,
) {
	t.Helper()
	encoded, err := resumestate.EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{random: authority.random}).CreateRecord(
		candidate, resumestate.HeaderRecordName, encoded, resumestate.MaxSessionHeaderBytes,
	); err != nil {
		t.Fatal(err)
	}
	if childCount >= 2 {
		lock, err := candidate.CreateFile(resumestate.SessionLockName, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(lock.Sync(), candidate.Sync(), lock.Close()); err != nil {
			t.Fatal(err)
		}
	}
	for index, name := range []string{
		resumestate.FilesDirectoryName,
		resumestate.AnchorsDirectoryName,
		resumestate.StagesDirectoryName,
	} {
		if childCount < index+3 {
			break
		}
		child, err := candidate.CreateDirectory(name, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(child.Sync(), candidate.Sync(), child.Close()); err != nil {
			t.Fatal(err)
		}
	}
}

func outputV3BootstrapChildLabel(count int) string {
	return []string{"", "control", "lock", "sessions"}[count]
}

func outputV3BootstrapCleanupLabel(count int) string {
	return []string{"complete", "sessions-removed", "lock-removed", "envelope-removed"}[count]
}

func outputV3SessionCandidateChildLabel(count int) string {
	return sessionCandidateChildren[count-1]
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
			faultResult, openErr := authority.OpenOrBootstrapControl(faulted)
			namespace := faultResult.Namespace
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
			recoveryResult, err := authority.OpenOrBootstrapControl(recoveryPlatform)
			recovered := recoveryResult.Namespace
			if err != nil {
				_ = recoveryPlatform.Close()
				t.Fatalf("recover bootstrap authority cut: %v", err)
			}
			if test.prefixCount >= 1 && recovered.control != expectedControl {
				t.Fatalf("recovered control = %#v, want %#v", recovered.control, expectedControl)
			}
			candidates, listErr := recoveryPlatform.Root().NamesWithPrefix(
				resumestate.BootstrapCandidatePrefix, RootInspectionLimit,
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
	candidates := make(map[string]outputcap.Directory, len(names))
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
	installedResult, err := authority.OpenOrBootstrapControl(platform)
	installed := installedResult.Namespace
	created := installedResult.Disposition == ControlInstalled
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
		if err != nil || kind != outputcap.EntryAbsent {
			t.Fatalf("candidate %q after recovery = (%v, %v)", name, kind, err)
		}
	}
	if err := errors.Join(installed.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestOutputV3ExactTemporaryRemovalRefusesAmbiguousIdentity(t *testing.T) {
	for _, test := range []struct {
		name string
		kind outputcap.EntryKind
		err  error
		want error
	}{
		{name: "already-absent", kind: outputcap.EntryAbsent},
		{name: "changed-kind", kind: outputcap.EntryDirectory, want: outputcap.ErrUnsafeNamespace},
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

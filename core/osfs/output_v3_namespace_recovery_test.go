package osfs

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

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

			namespace, created, err := authority.openOrBootstrapControl(platform)
			if err != nil {
				t.Fatal(err)
			}
			if !created || namespace.control != control {
				t.Fatalf("recovered control = (%v, %v), want original candidate control", created, namespace.control)
			}
			if kind, err := platform.Root().ObserveEntry(name); err != nil || kind != outputV3EntryAbsent {
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
			installed, _, err := authority.openOrBootstrapControl(platform)
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

			reopened, created, err := authority.openOrBootstrapControl(platform)
			if err != nil {
				t.Fatal(err)
			}
			if created || reopened.control != control {
				t.Fatalf("installed control changed while cleaning candidate: created=%v", created)
			}
			if kind, err := platform.Root().ObserveEntry(name); err != nil || kind != outputV3EntryAbsent {
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
	control, _, err := authority.openOrBootstrapControl(platform)
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
	if opened, err := openCanonicalIntentDirectory(control.sessions, selection.ResumeIntent()); !errors.Is(err, errOutputIntentUnsafe) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("open alias namespace error = %v, want %v", err, errOutputIntentUnsafe)
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
	for childCount := 1; childCount <= len(outputSessionCandidateChildren); childCount++ {
		t.Run(outputV3SessionCandidateChildLabel(childCount), func(t *testing.T) {
			rootPath := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, rootPath, nil)
			selection := v3RecoverySelection(t, false, 0)
			platform, err := openOutputV3Platform(rootPath, false)
			if err != nil {
				t.Fatal(err)
			}
			control, _, err := authority.openOrBootstrapControl(platform)
			if err != nil {
				t.Fatal(err)
			}
			intent, err := openCanonicalIntentDirectory(control.sessions, selection.ResumeIntent())
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
			header, err := newOutputSessionHeader(control.control, selection, ancestry, sessionID)
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
			control, err = openInstalledControl(platform.Root(), platform)
			if err != nil {
				t.Fatal(err)
			}
			intent, err = openCanonicalIntentDirectory(control.sessions, selection.ResumeIntent())
			if err != nil {
				t.Fatal(err)
			}

			installedName, installed, created, err := authority.openOrCreateSessionDirectory(
				intent, controlState, selection, ancestry,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !created || installedName != resumestate.SessionDirectoryName(sessionID) {
				t.Fatalf("installed session = (%q, %v), want recovered candidate", installedName, created)
			}
			children, err := validateSessionChildren(installed)
			if err != nil {
				t.Fatalf("validate recovered session: %v", err)
			}
			if err := closeSessionChildren(children); err != nil {
				t.Fatal(err)
			}
			if kind, err := intent.ObserveEntry(candidateName); err != nil || kind != outputV3EntryAbsent {
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
	authority *FilesystemOutputAuthority,
	candidate outputV3Directory,
	control resumestate.Control,
	childCount int,
) {
	t.Helper()
	if childCount >= 1 {
		encoded, err := resumestate.EncodeControl(control)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (outputStateStore{random: authority.random}).createRecord(
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
	authority *FilesystemOutputAuthority,
	candidate outputV3Directory,
	header resumestate.Header,
	childCount int,
) {
	t.Helper()
	encoded, err := resumestate.EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (outputStateStore{random: authority.random}).createRecord(
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
	return outputSessionCandidateChildren[count-1]
}

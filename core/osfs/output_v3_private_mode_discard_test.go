package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3ListsAndExplicitlyDiscardsWrongPrivateEnvelope(t *testing.T) {
	for _, test := range []struct {
		name          string
		target        func(string) string
		attentionCode string
		referenceKind ResumeStateKind
	}{
		{
			name: "session-directory", target: func(sessionPath string) string { return sessionPath },
			attentionCode: "unsafe-session-directory", referenceKind: ResumeStateOpaqueUnsafe,
		},
		{
			name: "header-record", target: func(sessionPath string) string {
				return filepath.Join(sessionPath, resumestate.HeaderRecordName)
			},
			attentionCode: "session-header-unreadable", referenceKind: ResumeStateNeedsAttention,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			authority := v3RecoveryAuthority(t, root, nil)
			opened := v3RecoveryOpen(t, authority, root, selection)
			sessionPath := v3RecoverySessionPath(root, selection, opened.Session.SessionID())
			v3RecoveryCloseSession(t, opened.Session)
			if err := v3RecoveryMakePrivateEnvelopeUnsafe(test.target(sessionPath)); err != nil {
				t.Fatal(err)
			}

			inventory, err := authority.listResumeState(context.Background(), FilesystemResumeRoot{RootPath: root})
			if err != nil {
				t.Fatal(err)
			}
			defer v3RecoveryCloseInventory(t, inventory)
			summaries := inventory.Summaries()
			if len(summaries) != 1 {
				t.Fatalf("list wrong private envelope = %+v", summaries)
			}
			if summaries[0].Reference.Kind() != test.referenceKind ||
				!v3RecoveryHasAttention(summaries[0], test.attentionCode) {
				t.Fatalf("wrong private envelope summary = %+v", summaries[0])
			}
			settlement, err := authority.discardResumeState(context.Background(), summaries[0].Reference)
			if err != nil || settlement.Kind != Discarded {
				t.Fatalf("discard wrong private envelope = (%+v, %v)", settlement, err)
			}
			if _, err := os.Lstat(sessionPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("wrong-envelope session after discard lstat error = %v, want not exist", err)
			}
		})
	}
}

func TestOutputV3ExplicitDiscardBlocksOnUnsafePresentSessionLock(t *testing.T) {
	root, authority, sessionPath := v3RecoveryDiscardLockFixture(t)
	if err := v3RecoveryMakePrivateEnvelopeUnsafe(filepath.Join(sessionPath, resumestate.SessionLockName)); err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.listResumeState(context.Background(), FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 1 {
		t.Fatalf("list unsafe session lock = %+v", summaries)
	}
	summary := summaries[0]
	if !v3RecoveryHasAttention(summary, "session-lock-unsafe") {
		t.Fatalf("unsafe lock summary = %+v", summary)
	}
	if _, err := authority.discardResumeState(context.Background(), summary.Reference); err == nil ||
		v3RecoveryFaultScope(err) != transfer.OutputFaultSession {
		t.Fatalf("discard with unsafe present lock error = %v, want session-scoped block", err)
	}
	v3RecoveryAssertDiscardFixtureUnchanged(t, sessionPath)
}

func TestOutputV3ExplicitDiscardBlocksOnSessionLockAcquisitionFailure(t *testing.T) {
	for _, mode := range []v3RecoveryLockFailureMode{
		v3RecoveryLockInjectedIO,
		v3RecoveryLockCreatedRace,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			root, authority, sessionPath := v3RecoveryDiscardLockFixture(t)
			inventory, err := authority.listResumeState(context.Background(), FilesystemResumeRoot{RootPath: root})
			if err != nil {
				t.Fatal(err)
			}
			defer v3RecoveryCloseInventory(t, inventory)
			summaries := inventory.Summaries()
			if len(summaries) != 1 {
				t.Fatalf("list discard-lock fixture = %+v", summaries)
			}
			summary := summaries[0]
			authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
				platform, err := openOutputV3Platform(path, create)
				if err != nil {
					return nil, err
				}
				return v3RecoveryWrapLockFailurePlatform(platform, mode), nil
			}
			if _, err := authority.discardResumeState(context.Background(), summary.Reference); err == nil ||
				v3RecoveryFaultScope(err) != transfer.OutputFaultSession {
				t.Fatalf("discard with %s error = %v, want session-scoped block", mode, err)
			}
			v3RecoveryAssertDiscardFixtureUnchanged(t, sessionPath)
		})
	}
}

func v3RecoveryDiscardLockFixture(
	t *testing.T,
) (string, *FilesystemOutputAuthority, string) {
	t.Helper()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	authority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, authority, root, selection)
	v3RecoveryWritePrivateSentinel(t, opened.Session.sessionDir)
	sessionPath := v3RecoverySessionPath(root, selection, opened.Session.SessionID())
	v3RecoveryCloseSession(t, opened.Session)
	return root, authority, sessionPath
}

func v3RecoveryAssertDiscardFixtureUnchanged(t *testing.T, sessionPath string) {
	t.Helper()
	actual, err := os.ReadFile(filepath.Join(sessionPath, "sentinel"))
	if err != nil || string(actual) != "replacement" {
		t.Fatalf("blocked discard changed sentinel: bytes=%q err=%v", actual, err)
	}
	for _, name := range []string{
		resumestate.HeaderRecordName,
		resumestate.SessionLockName,
		resumestate.FilesDirectoryName,
		resumestate.AnchorsDirectoryName,
		resumestate.StagesDirectoryName,
	} {
		if _, err := os.Lstat(filepath.Join(sessionPath, name)); err != nil {
			t.Fatalf("blocked discard removed required child %q: %v", name, err)
		}
	}
}

type v3RecoveryLockFailureMode uint8

const (
	v3RecoveryLockInjectedIO v3RecoveryLockFailureMode = iota + 1
	v3RecoveryLockCreatedRace
)

func (mode v3RecoveryLockFailureMode) String() string {
	switch mode {
	case v3RecoveryLockInjectedIO:
		return "injected-io"
	case v3RecoveryLockCreatedRace:
		return "created-race"
	default:
		return "unknown"
	}
}

type v3RecoveryLockFailurePlatform struct {
	outputV3Platform
	root outputV3Directory
}

func v3RecoveryWrapLockFailurePlatform(
	platform outputV3Platform,
	mode v3RecoveryLockFailureMode,
) outputV3Platform {
	return &v3RecoveryLockFailurePlatform{
		outputV3Platform: platform,
		root:             v3RecoveryWrapLockFailureDirectory(platform.Root(), mode),
	}
}

func (platform *v3RecoveryLockFailurePlatform) Root() outputV3Directory { return platform.root }

func (platform *v3RecoveryLockFailurePlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*v3RecoveryLockFailureDirectory)
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return v3RecoveryWrapLockFailureDirectory(root, decorated.mode)
		},
	)
}

type v3RecoveryLockFailureDirectory struct {
	outputV3Directory
	mode v3RecoveryLockFailureMode
}

func v3RecoveryWrapLockFailureDirectory(
	directory outputV3Directory,
	mode v3RecoveryLockFailureMode,
) outputV3Directory {
	if directory == nil {
		return nil
	}
	return &v3RecoveryLockFailureDirectory{outputV3Directory: directory, mode: mode}
}

func v3RecoveryUnwrapLockFailureDirectory(directory outputV3Directory) outputV3Directory {
	if wrapped, ok := directory.(*v3RecoveryLockFailureDirectory); ok {
		return wrapped.outputV3Directory
	}
	return directory
}

func (directory *v3RecoveryLockFailureDirectory) Duplicate() (outputV3Directory, error) {
	duplicate, err := directory.outputV3Directory.Duplicate()
	return v3RecoveryWrapLockFailureDirectory(duplicate, directory.mode), err
}

func (directory *v3RecoveryLockFailureDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	return directory.outputV3Directory.SameDirectory(v3RecoveryUnwrapLockFailureDirectory(other))
}

func (directory *v3RecoveryLockFailureDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	return v3RecoveryWrapLockFailureDirectory(opened, directory.mode), err
}

func (directory *v3RecoveryLockFailureDirectory) OpenPinnedDirectory(
	expected outputV3EntryRef,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenPinnedDirectory(expected, private)
	return v3RecoveryWrapLockFailureDirectory(opened, directory.mode), err
}

func (directory *v3RecoveryLockFailureDirectory) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	return v3RecoveryWrapLockFailureDirectory(created, directory.mode), err
}

func (directory *v3RecoveryLockFailureDirectory) InstallDirectoryNoReplace(
	candidate outputV3Directory,
	name string,
) (outputV3Directory, error) {
	installed, err := directory.outputV3Directory.InstallDirectoryNoReplace(
		v3RecoveryUnwrapLockFailureDirectory(candidate), name,
	)
	return v3RecoveryWrapLockFailureDirectory(installed, directory.mode), err
}

func (directory *v3RecoveryLockFailureDirectory) RemoveDirectory(
	name string,
	expected outputV3Directory,
) error {
	return directory.outputV3Directory.RemoveDirectory(
		name, v3RecoveryUnwrapLockFailureDirectory(expected),
	)
}

func (directory *v3RecoveryLockFailureDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputV3Lock, bool, error) {
	if name != resumestate.SessionLockName {
		return directory.outputV3Directory.AcquireLock(name, existingOnly)
	}
	switch directory.mode {
	case v3RecoveryLockInjectedIO:
		return nil, false, errors.New("injected session-lock acquisition failure")
	case v3RecoveryLockCreatedRace:
		lock, _, err := directory.outputV3Directory.AcquireLock(name, existingOnly)
		return lock, true, err
	default:
		return directory.outputV3Directory.AcquireLock(name, existingOnly)
	}
}

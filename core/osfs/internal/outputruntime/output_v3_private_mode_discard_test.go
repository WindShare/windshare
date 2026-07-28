package outputruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3ListsAndExplicitlyDiscardsWrongPrivateEnvelope(t *testing.T) {
	t.Parallel()
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
			if err := runtimeMakePrivateEnvelopeUnsafe(test.target(sessionPath)); err != nil {
				t.Fatal(err)
			}

			inventory, err := authority.ListResumeState(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer v3RecoveryCloseInventory(t, inventory)
			summaries := inventory.Summaries()
			if len(summaries) != 1 {
				t.Fatalf("list wrong private envelope = %+v", summaries)
			}
			if summaries[0].Reference.Kind() != test.referenceKind ||
				!runtimePrivateHasAttention(summaries[0], test.attentionCode) {
				t.Fatalf("wrong private envelope summary = %+v", summaries[0])
			}
			settlement, err := authority.DiscardResumeState(context.Background(), summaries[0].Reference)
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
	t.Parallel()
	root, authority, sessionPath := v3RecoveryDiscardLockFixture(t)
	if err := runtimeMakePrivateEnvelopeUnsafe(filepath.Join(sessionPath, resumestate.SessionLockName)); err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 1 {
		t.Fatalf("list unsafe session lock = %+v", summaries)
	}
	summary := summaries[0]
	if !runtimePrivateHasAttention(summary, "session-lock-unsafe") {
		t.Fatalf("unsafe lock summary = %+v", summary)
	}
	if _, err := authority.DiscardResumeState(context.Background(), summary.Reference); err == nil ||
		v3RecoveryFaultScope(err) != transfer.OutputFaultSession {
		t.Fatalf("discard with unsafe present lock error = %v, want session-scoped block", err)
	}
	v3RecoveryAssertDiscardFixtureUnchanged(t, sessionPath)
}

func TestOutputV3ExplicitDiscardBlocksOnSessionLockAcquisitionFailure(t *testing.T) {
	t.Parallel()
	for _, mode := range []v3RecoveryLockFailureMode{
		v3RecoveryLockInjectedIO,
		v3RecoveryLockCreatedRace,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			root, authority, sessionPath := v3RecoveryDiscardLockFixture(t)
			inventory, err := authority.ListResumeState(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer v3RecoveryCloseInventory(t, inventory)
			summaries := inventory.Summaries()
			if len(summaries) != 1 {
				t.Fatalf("list discard-lock fixture = %+v", summaries)
			}
			summary := summaries[0]
			authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
				platform, err := openOutputRuntimeTestPlatform(path, create)
				if err != nil {
					return nil, err
				}
				return v3RecoveryWrapLockFailurePlatform(platform, mode), nil
			}
			if _, err := authority.DiscardResumeState(context.Background(), summary.Reference); err == nil ||
				v3RecoveryFaultScope(err) != transfer.OutputFaultSession {
				t.Fatalf("discard with %s error = %v, want session-scoped block", mode, err)
			}
			v3RecoveryAssertDiscardFixtureUnchanged(t, sessionPath)
		})
	}
}

func v3RecoveryDiscardLockFixture(
	t *testing.T,
) (string, *Authority, string) {
	t.Helper()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	authority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, authority, root, selection)
	runtimeWritePrivateSentinel(t, opened.Session.sessionDir)
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

func runtimePrivateHasAttention(summary ResumeStateSummary, code string) bool {
	for _, attention := range summary.Attention {
		if attention.Code == code {
			return true
		}
	}
	return false
}

func runtimeWritePrivateSentinel(t *testing.T, directory outputcap.Directory) {
	t.Helper()
	sentinel, err := directory.CreateFile("sentinel", true, int64(len("replacement")))
	if err != nil {
		t.Fatal(err)
	}
	written, err := sentinel.WriteAt([]byte("replacement"), 0)
	if err != nil || written != len("replacement") {
		t.Fatalf("write replacement sentinel = (%d, %v)", written, err)
	}
	if err := errors.Join(sentinel.Sync(), directory.Sync(), sentinel.Close()); err != nil {
		t.Fatal(err)
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
	outputcap.Platform
	root outputcap.Directory
}

func v3RecoveryWrapLockFailurePlatform(
	platform outputcap.Platform,
	mode v3RecoveryLockFailureMode,
) outputcap.Platform {
	return &v3RecoveryLockFailurePlatform{
		Platform: platform,
		root:     v3RecoveryWrapLockFailureDirectory(platform.Root(), mode),
	}
}

func (platform *v3RecoveryLockFailurePlatform) Root() outputcap.Directory { return platform.root }

func (platform *v3RecoveryLockFailurePlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*v3RecoveryLockFailureDirectory)
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return v3RecoveryWrapLockFailureDirectory(root, decorated.mode)
		},
	)
}

type v3RecoveryLockFailureDirectory struct {
	outputcap.Directory
	mode v3RecoveryLockFailureMode
}

func v3RecoveryWrapLockFailureDirectory(
	directory outputcap.Directory,
	mode v3RecoveryLockFailureMode,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &v3RecoveryLockFailureDirectory{Directory: directory, mode: mode}
}

func v3RecoveryUnwrapLockFailureDirectory(directory outputcap.Directory) outputcap.Directory {
	if wrapped, ok := directory.(*v3RecoveryLockFailureDirectory); ok {
		return wrapped.Directory
	}
	return directory
}

func (directory *v3RecoveryLockFailureDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	return v3RecoveryWrapLockFailureDirectory(duplicate, directory.mode), err
}

func (directory *v3RecoveryLockFailureDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return directory.Directory.SameDirectory(v3RecoveryUnwrapLockFailureDirectory(other))
}

func (directory *v3RecoveryLockFailureDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	return v3RecoveryWrapLockFailureDirectory(opened, directory.mode), err
}

func (directory *v3RecoveryLockFailureDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenPinnedDirectory(expected, private)
	return v3RecoveryWrapLockFailureDirectory(opened, directory.mode), err
}

func (directory *v3RecoveryLockFailureDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	return v3RecoveryWrapLockFailureDirectory(created, directory.mode), err
}

func (directory *v3RecoveryLockFailureDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	installed, err := directory.Directory.InstallDirectoryNoReplace(
		v3RecoveryUnwrapLockFailureDirectory(candidate), name,
	)
	return v3RecoveryWrapLockFailureDirectory(installed, directory.mode), err
}

func (directory *v3RecoveryLockFailureDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	return directory.Directory.RemoveDirectory(
		name, v3RecoveryUnwrapLockFailureDirectory(expected),
	)
}

func (directory *v3RecoveryLockFailureDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if name != resumestate.SessionLockName {
		return directory.Directory.AcquireLock(name, existingOnly)
	}
	switch directory.mode {
	case v3RecoveryLockInjectedIO:
		return nil, false, errors.New("injected session-lock acquisition failure")
	case v3RecoveryLockCreatedRace:
		lock, _, err := directory.Directory.AcquireLock(name, existingOnly)
		return lock, true, err
	default:
		return directory.Directory.AcquireLock(name, existingOnly)
	}
}

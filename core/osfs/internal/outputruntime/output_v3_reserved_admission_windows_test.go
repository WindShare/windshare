//go:build windows

package outputruntime

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/transfer"
)

func TestWindowsV3ReservedSelectionFamiliesRejectBeforeProbe(t *testing.T) {
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)
	stats := &windowsV3ReservedAdmissionStats{}
	nativeFactory := authority.platformFactory
	authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		return &windowsV3ReservedProbeCountingPlatform{Platform: platform, stats: stats}, nil
	}

	tests := []struct {
		name      string
		selection func(*testing.T) transfer.OutputSelection
	}{
		{
			name: "control exact root file",
			selection: func(t *testing.T) transfer.OutputSelection {
				return v3RecoverySelectionPaths(t, []string{".windshare-output"}, 1)
			},
		},
		{
			name: "control mixed-case descendant",
			selection: func(t *testing.T) transfer.OutputSelection {
				return windowsRuntimeReservedDirectorySelection(t, ".WiNdShArE-OuTpUt/child")
			},
		},
		{
			name: "bootstrap descendant",
			selection: func(t *testing.T) transfer.OutputSelection {
				return windowsRuntimeReservedDirectorySelection(t, ".windshare-output.bootstrap-dead/child")
			},
		},
		{
			name: "probe mixed-case descendant",
			selection: func(t *testing.T) transfer.OutputSelection {
				return windowsRuntimeReservedDirectorySelection(t, ".WiNdShArE-OuTpUt.PrObE-dead/child")
			},
		},
		{
			name: "metadata probe descendant",
			selection: func(t *testing.T) transfer.OutputSelection {
				return windowsRuntimeReservedDirectorySelection(t, ".windshare-output.metadata-probe-dead/child")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, err := authority.OpenSelection(context.Background(), test.selection(t))
			if session != nil {
				_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
				t.Fatal("reserved selection unexpectedly returned a session")
			}
			if !errors.Is(err, outputfault.ErrReservedPath) {
				t.Fatalf("reserved selection error = %v, want reserved-path rejection", err)
			}
			requireWindowsV3FreshSelectionFault(t, err)
		})
	}
	if stats.probeCalls != 0 {
		t.Fatalf("reserved selections reached the recoverability probe %d times", stats.probeCalls)
	}
	if stats.identityPreparations != 0 {
		t.Fatalf("reserved selections reached identity preparation %d times", stats.identityPreparations)
	}
	assertWindowsV3StaticAdmissionLeftRootUntouched(t, root)
}

func TestWindowsV3ReservedComponentKeyAllowsNormalSelection(t *testing.T) {
	root := v3RecoveryRoot(t)
	platform, err := openOutputRuntimeTestPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if err := validateReservedOutputSelection(
		platform, v3RecoverySelectionPaths(t, []string{"ordinary.bin"}, 1),
	); err != nil {
		t.Fatalf("normal selection was treated as internal output state: %v", err)
	}
	assertWindowsV3StaticAdmissionLeftRootUntouched(t, root)
}

func TestWindowsV3PlatformEquivalentSelectionRejectsBeforeProbe(t *testing.T) {
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)
	stats := &windowsV3ReservedAdmissionStats{}
	nativeFactory := authority.platformFactory
	authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := nativeFactory(path, create)
		if err != nil {
			return nil, err
		}
		return &windowsV3ReservedProbeCountingPlatform{Platform: platform, stats: stats}, nil
	}
	selection := v3RecoverySelectionPaths(t, []string{"Alias.bin", "alias.bin"}, 1)

	session, err := authority.OpenSelection(context.Background(), selection)
	if session != nil {
		_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
		t.Fatal("platform-equivalent selection unexpectedly returned a session")
	}
	if !strings.Contains(err.Error(), "platform-equivalent") {
		t.Fatalf("platform-equivalent selection error = %v", err)
	}
	requireWindowsV3FreshSelectionFault(t, err)
	if stats.probeCalls != 0 {
		t.Fatalf("platform-equivalent selection reached the recoverability probe %d times", stats.probeCalls)
	}
	if stats.identityPreparations != 0 {
		t.Fatalf("platform-equivalent selection reached identity preparation %d times", stats.identityPreparations)
	}
	assertWindowsV3StaticAdmissionLeftRootUntouched(t, root)
}

type windowsV3ReservedProbeCountingPlatform struct {
	outputcap.Platform
	stats *windowsV3ReservedAdmissionStats
}

type windowsV3ReservedAdmissionStats struct {
	probeCalls           int
	identityPreparations int
}

func (platform *windowsV3ReservedProbeCountingPlatform) Root() outputcap.Directory {
	if platform == nil || platform.Platform == nil {
		return nil
	}
	return wrapWindowsV3ReservedAdmissionDirectory(platform.Platform.Root(), platform.stats)
}

func (platform *windowsV3ReservedProbeCountingPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	if platform == nil || platform.Platform == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return wrapWindowsV3ReservedAdmissionDirectory(root, platform.stats)
		},
	)
}

func (platform *windowsV3ReservedProbeCountingPlatform) ProbeRecoverableFeatures() error {
	platform.stats.probeCalls++
	return platform.Platform.ProbeRecoverableFeatures()
}

// windowsV3ReservedAdmissionDirectory observes PrepareIdentityClaim because it
// is the capability operation that reaches NTFS's CreateOrGet Object-ID cut.
// This proves rejected static selections stop before native identity mutation
// without depending on outputwindows' private provider implementation.
type windowsV3ReservedAdmissionDirectory struct {
	outputcap.Directory
	stats *windowsV3ReservedAdmissionStats
}

func wrapWindowsV3ReservedAdmissionDirectory(
	directory outputcap.Directory,
	stats *windowsV3ReservedAdmissionStats,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &windowsV3ReservedAdmissionDirectory{Directory: directory, stats: stats}
}

func unwrapWindowsV3ReservedAdmissionDirectory(directory outputcap.Directory) outputcap.Directory {
	if wrapped, ok := directory.(*windowsV3ReservedAdmissionDirectory); ok {
		return wrapped.Directory
	}
	return directory
}

func (directory *windowsV3ReservedAdmissionDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return wrapWindowsV3ReservedAdmissionDirectory(duplicate, directory.stats), nil
}

func (directory *windowsV3ReservedAdmissionDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return directory.Directory.SameDirectory(unwrapWindowsV3ReservedAdmissionDirectory(other))
}

func (directory *windowsV3ReservedAdmissionDirectory) PrepareIdentityClaim() (
	outputcap.PersistentDirectoryIdentity,
	error,
) {
	directory.stats.identityPreparations++
	return directory.Directory.PrepareIdentityClaim()
}

func (directory *windowsV3ReservedAdmissionDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return wrapWindowsV3ReservedAdmissionDirectory(opened, directory.stats), nil
}

func (directory *windowsV3ReservedAdmissionDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenPinnedDirectory(expected, private)
	if err != nil {
		return nil, err
	}
	return wrapWindowsV3ReservedAdmissionDirectory(opened, directory.stats), nil
}

func (directory *windowsV3ReservedAdmissionDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return wrapWindowsV3ReservedAdmissionDirectory(created, directory.stats), nil
}

func (directory *windowsV3ReservedAdmissionDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	installed, err := directory.Directory.InstallDirectoryNoReplace(
		unwrapWindowsV3ReservedAdmissionDirectory(candidate), name,
	)
	if err != nil {
		return nil, err
	}
	return wrapWindowsV3ReservedAdmissionDirectory(installed, directory.stats), nil
}

func (directory *windowsV3ReservedAdmissionDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	return directory.Directory.RemoveDirectory(name, unwrapWindowsV3ReservedAdmissionDirectory(expected))
}

func windowsRuntimeReservedDirectorySelection(t *testing.T, path string) transfer.OutputSelection {
	t.Helper()
	share := v3RecoveryIdentity16[catalog.ShareInstance](0x71)
	root := v3RecoveryIdentity16[catalog.DirectoryID](0x72)
	rootGeneration := v3RecoveryIdentity16[catalog.DirectoryGeneration](0x73)
	components := strings.Split(path, "/")
	directories := make([]transfer.OutputSelectionDirectory, 0, len(components))
	for index := range components {
		directories = append(directories, transfer.OutputSelectionDirectory{
			Path:         strings.Join(components[:index+1], "/"),
			DirectoryID:  v3RecoveryIdentity16[catalog.DirectoryID](byte(0x74 + index)),
			Generation:   v3RecoveryIdentity16[catalog.DirectoryGeneration](byte(0x84 + index)),
			ModifiedTime: v3RecoveryModifiedTime(t),
		})
	}
	plan, err := transfer.NewOutputSelection(share, root, rootGeneration, directories, nil)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func assertWindowsV3StaticAdmissionLeftRootUntouched(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("static admission rejection changed output root: entries=%v error=%v", entries, err)
	}
}

func requireWindowsV3FreshSelectionFault(t *testing.T, err error) {
	t.Helper()
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultSession ||
		fault.Code() != transfer.OutputFaultNamespaceUnsafe {
		t.Fatalf("static selection error = %v, want session-scoped namespace rejection", err)
	}
	if _, found := errors.AsType[*transfer.OutputSessionError](err); found {
		t.Fatalf("fresh static selection rejection requested an explicit pause: %v", err)
	}
	if fault.RequiresJobPause() {
		t.Fatalf("fresh static selection rejection requested an effective pause: %v", err)
	}
}

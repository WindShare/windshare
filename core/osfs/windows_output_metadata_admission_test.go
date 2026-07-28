//go:build windows

package osfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func TestWindowsSelectionMetadataRejectsUnrepresentablePrecisionWithoutStateOrContent(t *testing.T) {
	rootPath := t.TempDir()
	selection := windowsSelectionMetadataSelection(t, []windowsSelectionMetadataFile{
		{size: 1, modified: windowsSelectionMetadataModified(t, 1_700_000_000, 0, catalog.TimePrecisionNanoseconds)},
		// This invalid middle value proves planning validates every claim instead
		// of checking only the min/max witnesses selected for native I/O.
		{size: 2, modified: windowsSelectionMetadataModified(t, 1_700_000_001, 1, catalog.TimePrecisionNanoseconds)},
		{size: 3, modified: windowsSelectionMetadataModified(t, 1_700_000_002, 999_999_900, catalog.TimePrecisionNanoseconds)},
	})
	probeCalls := 0
	authority := newOutputV3DecoratedPublicAuthority(t, rootPath, func(platform outputcap.Platform) outputcap.Platform {
		return &windowsMetadataAdmissionPlatform{Platform: platform, probeCalls: &probeCalls}
	})
	session, err := authority.OpenSelection(context.Background(), selection)
	if session != nil {
		_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
		t.Fatal("unrepresentable NTFS timestamp returned a session")
	}
	if !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("unrepresentable NTFS timestamp error = %v, want native unsupported cause", err)
	}
	windowsMetadataRequireFreshSelectionFault(t, err)
	if probeCalls != 0 {
		t.Fatalf("unrepresentable static timestamp reached recoverability probe %d times", probeCalls)
	}
	assertWindowsMetadataAdmissionLeftRootUntouched(t, rootPath)
}

func TestWindowsSelectionMetadataMaximumRejectionPrecedesStateAndContent(t *testing.T) {
	rootPath := t.TempDir()
	selection := windowsSelectionMetadataSelection(t, []windowsSelectionMetadataFile{
		{size: catalog.MaxFileSize, modified: windowsV3TestModifiedTime(t)},
	})
	probeCalls := 0
	authority := newOutputV3DecoratedPublicAuthority(t, rootPath, func(platform outputcap.Platform) outputcap.Platform {
		return &windowsMetadataAdmissionPlatform{
			Platform: platform, probeCalls: &probeCalls,
			reject: func(selection transfer.OutputSelection) error {
				for _, file := range selection.Files() {
					if file.ExpectedSize == catalog.MaxFileSize {
						return errors.Join(
							outputcap.ErrRecoverableOutputUnsupported,
							errors.New("injected native maximum-size rejection"),
						)
					}
				}
				return nil
			},
		}
	})
	session, err := authority.OpenSelection(context.Background(), selection)
	if session != nil {
		_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
		t.Fatal("native maximum-size rejection returned a session")
	}
	if !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("native maximum-size rejection error = %v, want native unsupported cause", err)
	}
	windowsMetadataRequireFreshSelectionFault(t, err)
	if probeCalls != 1 {
		t.Fatalf("native maximum-size rejection recoverability probe calls = %d, want 1", probeCalls)
	}
	assertWindowsMetadataAdmissionLeftNoStateOrContent(t, rootPath, selection.Files()[0].Path)
}

type windowsMetadataAdmissionPlatform struct {
	outputcap.Platform
	probeCalls *int
	reject     func(transfer.OutputSelection) error
}

func (platform *windowsMetadataAdmissionPlatform) ProbeRecoverableFeatures() error {
	(*platform.probeCalls)++
	return platform.Platform.ProbeRecoverableFeatures()
}

func (platform *windowsMetadataAdmissionPlatform) ValidateSelectionMetadata(
	selection transfer.OutputSelection,
) error {
	if platform.reject != nil {
		if err := platform.reject(selection); err != nil {
			return err
		}
	}
	return platform.Platform.ValidateSelectionMetadata(selection)
}

type windowsSelectionMetadataFile struct {
	size     uint64
	modified catalog.ModifiedTime
}

func windowsSelectionMetadataSelection(
	t *testing.T,
	files []windowsSelectionMetadataFile,
) transfer.OutputSelection {
	t.Helper()
	share := windowsV3TestIdentity16[catalog.ShareInstance](0xb1)
	root := windowsV3TestIdentity16[catalog.DirectoryID](0xb2)
	generation := windowsV3TestIdentity16[catalog.DirectoryGeneration](0xb3)
	selected := make([]transfer.OutputSelectionFile, len(files))
	for index, file := range files {
		selected[index] = transfer.OutputSelectionFile{
			Path:              fmt.Sprintf("metadata-%03d.bin", index),
			FileID:            windowsV3TestIdentity16[catalog.FileID](byte(0xc0 + index)),
			ParentDirectoryID: root,
			ParentGeneration:  generation,
			ExpectedSize:      file.size,
			ModifiedTime:      file.modified,
		}
	}
	plan, err := transfer.NewOutputSelection(share, root, generation, nil, selected)
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

func windowsSelectionMetadataModified(
	t *testing.T,
	seconds int64,
	nanoseconds uint32,
	precision catalog.TimePrecision,
) catalog.ModifiedTime {
	t.Helper()
	modified, err := catalog.NewModifiedTime(seconds, nanoseconds, precision)
	if err != nil {
		t.Fatal(err)
	}
	return modified
}

func assertWindowsMetadataAdmissionLeftNoStateOrContent(t *testing.T, rootPath, final string) {
	t.Helper()
	assertWindowsMetadataAdmissionLeftRootUntouched(t, rootPath)
	if _, err := os.Lstat(rootPath + string(os.PathSeparator) + final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata rejection created final content path: %v", err)
	}
}

func assertWindowsMetadataAdmissionLeftRootUntouched(t *testing.T, rootPath string) {
	t.Helper()
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("metadata rejection left output-root entries %v", entries)
	}
}

func windowsMetadataRequireFreshSelectionFault(t *testing.T, err error) {
	t.Helper()
	outputV3ControlSessionRequireFault(
		t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
	)
	if _, found := errors.AsType[*transfer.OutputSessionError](err); found {
		t.Fatalf("fresh metadata rejection requested an explicit pause: %v", err)
	}
	if fault, found := errors.AsType[*transfer.OutputFault](err); found && fault.RequiresJobPause() {
		t.Fatalf("fresh metadata rejection requested an effective pause: %v", err)
	}
}

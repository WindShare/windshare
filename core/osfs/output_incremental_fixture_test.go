//go:build linux || windows

package osfs

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

const outputFixtureBackendName = string(transfer.NativeFilesystemOutputBackendID)

// openOutputSelectionFixture translates the old test-only frozen plan into the
// product contract: OpenOutput receives the confirmed root intent, then every
// selected directory is admitted in ancestry order. It deliberately does not
// pre-admit files; BeginFile must supply a descriptor and the parent receipt.
func openOutputSelectionFixture(
	t *testing.T,
	authority *FilesystemOutputAuthority,
	rootPath string,
	selection transfer.OutputSelection,
) (transfer.OutputSession, map[string]transfer.DirectoryAdmission, error) {
	t.Helper()
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		return nil, nil, err
	}
	backend, err := transfer.NewOutputBackendID(outputFixtureBackendName)
	if err != nil {
		return nil, nil, err
	}
	intent, err := transfer.NewFilesystemTransferIntent(
		selection.ShareInstance(), selection.SyntheticRoot(), rules,
		rootPath, backend, transfer.OutputNativeTree,
	)
	if err != nil {
		return nil, nil, err
	}
	session, err := authority.OpenOutput(context.Background(), intent)
	if err != nil {
		return nil, nil, err
	}
	admissions := make(map[string]transfer.DirectoryAdmission, selection.DirectoryCount()+1)
	root := transfer.OutputDirectory{
		DirectoryID: selection.SyntheticRoot(), Generation: selection.RootGeneration(),
	}
	rootAdmission, err := session.AdmitDirectory(context.Background(), root)
	if err != nil {
		_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
		return nil, nil, err
	}
	admissions[""] = rootAdmission
	directories := selection.Directories()
	sort.Slice(directories, func(i, j int) bool {
		depth := func(path string) int {
			if path == "" {
				return 0
			}
			return strings.Count(path, "/") + 1
		}
		if depth(directories[i].Path) != depth(directories[j].Path) {
			return depth(directories[i].Path) < depth(directories[j].Path)
		}
		return directories[i].Path < directories[j].Path
	})
	for _, selected := range directories {
		parentPath := selected.Path
		if slash := strings.LastIndexByte(parentPath, '/'); slash >= 0 {
			parentPath = parentPath[:slash]
		} else {
			parentPath = ""
		}
		directory := transfer.OutputDirectory{
			Path: selected.Path, DirectoryID: selected.DirectoryID,
			Generation: selected.Generation, ModifiedTime: selected.ModifiedTime,
			ParentAdmission: admissions[parentPath],
		}
		admission, admitErr := session.AdmitDirectory(context.Background(), directory)
		if admitErr != nil {
			_, _ = session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
			return nil, nil, admitErr
		}
		admissions[selected.Path] = admission
	}
	return session, admissions, nil
}

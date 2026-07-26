package transfer

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestOutputSelectionRejectsLegacyResumeRootAliases(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xd1)
	root := transferID[catalog.DirectoryID](0xd2)
	generation := transferID[catalog.DirectoryGeneration](0xd3)
	for _, path := range []string{
		".wsresume-output-state",
		".WsReSuMe-OuTpUt-StAgE-dead/child.bin",
	} {
		t.Run(path, func(t *testing.T) {
			_, err := NewOutputSelection(share, root, generation, nil, []OutputSelectionFile{{
				Path: path, FileID: transferID[catalog.FileID](0xd4),
				ParentDirectoryID: root, ParentGeneration: generation,
				ExpectedSize: 1,
			}})
			if !errors.Is(err, ErrInvalidOutputSelection) {
				t.Fatalf("legacy resume root selection error = %v", err)
			}
		})
	}
}

//go:build !windows && !linux && !darwin

package osfs

import (
	"fmt"
	"io/fs"
	"os"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func catalogModifiedTime(information fs.FileInfo) (catalog.ModifiedTime, error) {
	return portableCatalogModifiedTime(information)
}

func platformCatalogBaseline(*os.File) (catalog.SourceIdentity, catalog.VersionCandidate, error) {
	return catalog.SourceIdentity{}, catalog.VersionCandidate{}, content.ErrUnsupportedStability
}

func newPlatformRootedRevisionSource([]string) (*RootedRevisionSource, error) {
	return nil, content.ErrUnsupportedStability
}

func openNativeOutputPlatform(_ string, _ bool) (outputcap.Platform, error) {
	return nil, fmt.Errorf("%w: certified only on Linux/ext4 and Windows/local-NTFS", outputcap.ErrRecoverableOutputUnsupported)
}

//go:build !windows && !linux && !darwin

package osfs

import (
	"io/fs"
	"os"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/diagnostics"
)

func catalogModifiedTime(information fs.FileInfo) (catalog.ModifiedTime, error) {
	return portableCatalogModifiedTime(information)
}

func platformCatalogBaseline(*os.File) (catalog.SourceIdentity, catalog.VersionCandidate, error) {
	return catalog.SourceIdentity{}, catalog.VersionCandidate{}, content.ErrUnsupportedStability
}

func newPlatformRootedRevisionSource([]*os.Root, diagnostics.Sink) (*RootedRevisionSource, error) {
	return nil, content.ErrUnsupportedStability
}

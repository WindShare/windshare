//go:build !windows && !linux && !darwin

package osfs

import (
	"os"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func platformCatalogBaseline(*os.File) (catalog.SourceIdentity, catalog.VersionCandidate, error) {
	return catalog.SourceIdentity{}, catalog.VersionCandidate{}, content.ErrUnsupportedStability
}

func newPlatformRootedRevisionSource([]string) (*RootedRevisionSource, error) {
	return nil, content.ErrUnsupportedStability
}

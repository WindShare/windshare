package filesystem

import (
	"context"
	"slices"

	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/osfs"
)

// ShareSource freezes the selection, but opens roots only when the engine starts
// preparation. Descendant discovery remains demand-driven and engine-owned.
func ShareSource(paths []string) liveshare.FileSourceFactory {
	selected := slices.Clone(paths)
	return liveshare.FileSourceFactoryFunc(func(ctx context.Context, source liveshare.FileSourceContext) (liveshare.FileSource, error) {
		return osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{
			Paths: selected, SyntheticRoot: source.SyntheticRoot, Identities: osfs.CatalogIdentitySourceFunc(source.NewIdentity),
			Diagnostics: source.Diagnostics, Omissions: source.Omissions,
		})
	})
}

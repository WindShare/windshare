package liveshare

import (
	"context"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/contentflow"
)

func testFileSource(paths []string) FileSourceFactory {
	selected := append([]string(nil), paths...)
	return FileSourceFactoryFunc(func(ctx context.Context, source FileSourceContext) (FileSource, error) {
		return osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{
			Paths: selected, SyntheticRoot: source.SyntheticRoot,
			Identities: osfs.CatalogIdentitySourceFunc(source.NewIdentity),
		})
	})
}

func testCatalogBudget() *catalog.BudgetAccount {
	budget, err := catalog.NewBudgetAccount("test-process", catalog.DefaultProcessBudgetLimits())
	if err != nil {
		panic(err)
	}
	return budget
}

func testCacheBudget() *contentflow.ProcessCacheBudget {
	budget, err := contentflow.NewProcessCacheBudget(defaultSharedBlockCacheBytes)
	if err != nil {
		panic(err)
	}
	return budget
}

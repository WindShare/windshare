package sendersession

import (
	"context"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/catalogflow"
)

type blockingCatalogPageSource struct {
	started chan context.Context
	stopped chan struct{}
}

var _ catalogflow.PageAddressedSource = (*blockingCatalogPageSource)(nil)

func (source *blockingCatalogPageSource) LoadPage(
	ctx context.Context,
	_ catalogflow.ListRequest,
	_ catalog.ScanProgressObserver,
) (catalogflow.PageResult, error) {
	select {
	case source.started <- ctx:
	case <-ctx.Done():
		return catalogflow.PageResult{}, ctx.Err()
	}
	<-ctx.Done()
	select {
	case source.stopped <- struct{}{}:
	default:
	}
	return catalogflow.PageResult{}, ctx.Err()
}

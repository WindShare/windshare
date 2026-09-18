package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/liveshare"
)

func TestApplicationShutdownReportsUnreleasedAggregateResources(t *testing.T) {
	for _, resource := range []string{"catalog", "cache"} {
		t.Run(resource, func(t *testing.T) {
			application, err := New(Config{CacheBytes: 10})
			if err != nil {
				t.Fatal(err)
			}
			release, err := reserveTaskResource(resource, liveshare.SenderConfig{
				CatalogBudget: application.catalog, CacheBudget: application.cache,
			}, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			err = application.Close(context.Background())
			var retained *UnreleasedResourcesError
			if !errors.As(err, &retained) || retained.Error() == "" {
				t.Fatalf("retained resources were not reported: %v", err)
			}
			if resource == "catalog" && retained.CatalogUsage == (catalog.ResourceUsage{}) {
				t.Fatal("catalog ownership was lost")
			}
			if resource == "cache" && retained.CacheBytes == 0 {
				t.Fatal("cache ownership was lost")
			}
		})
	}
}

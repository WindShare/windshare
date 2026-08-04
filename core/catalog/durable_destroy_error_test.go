package catalog

import "testing"

func TestFileCatalogDestroyReportsRootRemovalFailure(t *testing.T) {
	backend := &FileCatalogBackend{root: string([]byte{0})}
	if err := backend.Destroy(); err == nil {
		t.Fatal("invalid durable catalog root removal was reported as successful")
	}
}

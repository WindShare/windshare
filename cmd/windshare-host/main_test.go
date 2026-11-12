package main

import "testing"

func TestABIValuesAndHostOwnership(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	started := startHost()
	if started.Error != "" || started.Handle == 0 || started.Endpoint == nil {
		t.Fatalf("%+v", started)
	}
	if startHost().Error == "" {
		t.Fatal("created a second application engine")
	}
	if stopHost(started.Handle+1).Error == "" {
		t.Fatal("foreign handle accepted")
	}
	if result := stopHost(started.Handle); result.Error != "" {
		t.Fatal(result.Error)
	}
	if stopHost(started.Handle).Error == "" {
		t.Fatal("retired handle accepted")
	}
	value := WindShareHostStart()
	if value == nil {
		t.Fatal("missing C-owned result")
	}
	WindShareHostFree(value)
	value = WindShareHostStop(0)
	WindShareHostFree(value)
	if result := stopHost(registry.handle); result.Error != "" {
		t.Fatal(result.Error)
	}
}

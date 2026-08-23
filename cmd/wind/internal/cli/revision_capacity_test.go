package cli

import (
	"testing"

	"github.com/windshare/windshare/core/content/revisioncapacity"
)

func newTestRevisionCapacity(t testing.TB) *revisioncapacity.Coordinator {
	t.Helper()
	owner, err := revisioncapacity.NewProcessOwner(revisioncapacity.DefaultProcessConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Errorf("close test revision capacity owner: %v", err)
		}
	})
	return owner.Coordinator()
}

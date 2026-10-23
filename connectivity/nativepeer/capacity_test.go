package nativepeer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/connectivity/turncapacity"
)

func newTestConnectivity(t *testing.T, config Config) *NativePeerConnectivity {
	t.Helper()
	if config.Admission == nil {
		config.Admission = NewAdmission(AdmissionClock{})
	}
	if config.TURNCapacity == nil {
		config.TURNCapacity = turncapacity.NewPool()
	}
	native, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := native.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return native
}

func TestConnectivityRequiresExplicitCapacity(t *testing.T) {
	for _, config := range []Config{
		{},
		{Admission: NewAdmission(AdmissionClock{})},
		{TURNCapacity: turncapacity.NewPool()},
	} {
		if native, err := New(config); native != nil || !errors.Is(err, ErrCapacityRequired) {
			t.Fatalf("missing capacity accepted: native=%v err=%v", native, err)
		}
	}
}

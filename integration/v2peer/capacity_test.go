package v2peer_test

import (
	"context"
	"testing"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turncapacity"
)

func newCapacityNative(t *testing.T) *nativepeer.NativePeerConnectivity {
	t.Helper()
	native, err := nativepeer.New(nativepeer.Config{
		Admission:    nativepeer.NewAdmission(nativepeer.AdmissionClock{}),
		TURNCapacity: turncapacity.NewPool(),
	})
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

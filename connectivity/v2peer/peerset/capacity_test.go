package peerset

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turncapacity"
)

func newTestPeerSet(t *testing.T, config Config) (*PeerSet, error) {
	t.Helper()
	if config.Capacity == nil {
		var err error
		config.Capacity, err = NewCapacity(DefaultAttemptCapacity)
		if err != nil {
			t.Fatal(err)
		}
	}
	return New(config)
}

func newTestNative(t *testing.T, config nativepeer.Config) *nativepeer.NativePeerConnectivity {
	t.Helper()
	if config.Admission == nil {
		config.Admission = nativepeer.NewAdmission(nativepeer.AdmissionClock{})
	}
	config.TURNCapacity = turncapacity.NewPool()
	native, err := nativepeer.New(config)
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

func TestPeerSetRequiresExplicitAttemptCapacity(t *testing.T) {
	if owner, err := New(Config{}); owner != nil || !errors.Is(err, ErrConfig) {
		t.Fatalf("missing capacity accepted: owner=%v err=%v", owner, err)
	}
}

package v2peer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turncapacity"
)

func newTestNative(t *testing.T, config nativepeer.Config) *nativepeer.NativePeerConnectivity {
	t.Helper()
	if config.Admission == nil {
		config.Admission = nativepeer.NewAdmission(nativepeer.AdmissionClock{})
	}
	if config.TURNCapacity == nil {
		config.TURNCapacity = turncapacity.NewPool()
	}
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

func newTestSenderFactory(t *testing.T, config Config) (*Factory, error) {
	t.Helper()
	if config.Native == nil {
		config.Native = newTestNative(t, nativepeer.Config{})
	}
	return NewFactory(config)
}

func newTestReceiverFactory(t *testing.T, config ReceiverFactoryConfig) (*ReceiverFactory, error) {
	t.Helper()
	if config.Native == nil {
		config.Native = newTestNative(t, nativepeer.Config{})
	}
	return NewReceiverFactory(config)
}

func TestFactoriesRequireExplicitNativeOwnership(t *testing.T) {
	if factory, err := NewFactory(Config{}); factory != nil || !errors.Is(err, ErrConfig) {
		t.Fatalf("sender accepted missing native owner: factory=%v err=%v", factory, err)
	}
	if factory, err := NewReceiverFactory(ReceiverFactoryConfig{}); factory != nil || !errors.Is(err, ErrConfig) {
		t.Fatalf("receiver accepted missing native owner: factory=%v err=%v", factory, err)
	}
}

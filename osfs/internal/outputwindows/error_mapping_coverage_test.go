//go:build windows

package outputwindows

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestWindowsOutputErrorMapsNativeFailureClasses(t *testing.T) {
	base := errors.New("native failure")
	for _, test := range []struct {
		name string
		in   error
		want error
	}{
		{name: "nil", in: nil, want: nil},
		{name: "unsupported", in: errors.Join(errWindowsOutputUnsupported, base), want: outputcap.ErrRecoverableOutputUnsupported},
		{name: "unsafe", in: errors.Join(errWindowsOutputUnsafe, base), want: outputcap.ErrUnsafeNamespace},
		{name: "collision", in: errors.Join(errWindowsOutputCollision, base), want: outputcap.ErrNamespaceCollision},
		{name: "lock busy", in: errors.Join(errWindowsOutputLockBusy, base), want: outputcap.ErrNamespaceLockBusy},
		{name: "exists", in: fs.ErrExist, want: outputcap.ErrNamespaceCollision},
		{name: "ordinary", in: base, want: base},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := windowsOutputError(test.in)
			if test.want == nil {
				if got != nil {
					t.Fatalf("mapped nil error = %v", got)
				}
				return
			}
			if !errors.Is(got, test.want) {
				t.Fatalf("mapped error = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWindowsPersistentObjectIDStateCurrentHandlesNilZeroAndIdentity(t *testing.T) {
	var nilState *windowsPersistentObjectIDState
	if identity, ok := nilState.current(); ok || identity.valid() {
		t.Fatalf("nil state current = %v, %t", identity, ok)
	}
	state := newWindowsPersistentObjectIDState()
	if identity, ok := state.current(); ok || identity.valid() {
		t.Fatalf("zero state current = %v, %t", identity, ok)
	}
	state.identity[0] = 1
	identity, ok := state.current()
	if !ok || identity != state.identity {
		t.Fatalf("set state current = %v, %t", identity, ok)
	}
}

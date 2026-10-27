package outputfile

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type retainedState struct {
	closed bool
	err    error
}

func (state *retainedState) Close() error          { state.closed = true; return state.err }
func (state *retainedState) Size() (uint64, error) { return 7, state.err }
func (state *retainedState) SameFile(other outputcap.FileIdentity) (bool, error) {
	file, ok := other.(*Observed[*retainedState])
	return ok && file.State == state, state.err
}
func (state *retainedState) ReadAt(data []byte, offset int64) (int, error) {
	return len(data), state.err
}
func (state *retainedState) WriteAt(data []byte, offset int64) (int, error) {
	return len(data), state.err
}
func (state *retainedState) Sync() error                                { return state.err }
func (state *retainedState) SetModifiedTime(catalog.ModifiedTime) error { return state.err }
func (state *retainedState) MetadataMatches(uint64, catalog.ModifiedTime) (bool, error) {
	return true, state.err
}

func TestAccessViewsPreserveIdentityAndNativeFailuresWithoutEscalatingAuthority(t *testing.T) {
	nativeFailure := errors.New("native object was replaced")
	state := &retainedState{}
	observed := &Observed[*retainedState]{State: state}
	recovery := &Recovery[*retainedState]{State: state}
	mutable := &Mutable[*retainedState]{State: state}
	if _, writable := any(observed).(outputcap.MutableFile); writable {
		t.Fatal("observation acquired mutation authority")
	}
	if _, flushable := any(observed).(outputcap.RecoveryDurabilityFile); flushable {
		t.Fatal("observation acquired flush authority")
	}
	if _, readable := any(recovery).(outputcap.ObservedFile); readable {
		t.Fatal("recovery acquired content observation authority")
	}
	for _, failure := range []error{nil, nativeFailure} {
		state.err = failure
		for _, file := range []outputcap.FileIdentity{observed, recovery, mutable} {
			if size, err := file.Size(); size != 7 || !errors.Is(err, failure) {
				t.Fatalf("size=%d err=%v", size, err)
			}
			if same, err := file.SameFile(observed); !same || !errors.Is(err, failure) {
				t.Fatalf("identity=%t err=%v", same, err)
			}
		}
		for _, file := range []outputcap.ObservedFile{observed, mutable} {
			if count, err := file.ReadAt(make([]byte, 3), 4); count != 3 || !errors.Is(err, failure) {
				t.Fatalf("read=%d err=%v", count, err)
			}
			if match, err := file.MetadataMatches(7, catalog.ModifiedTime{}); !match || !errors.Is(err, failure) {
				t.Fatalf("metadata=%t err=%v", match, err)
			}
		}
		for _, file := range []outputcap.RecoveryDurabilityFile{recovery, mutable} {
			if err := file.Sync(); !errors.Is(err, failure) {
				t.Fatalf("flush error=%v", err)
			}
		}
		if count, err := mutable.WriteAt(make([]byte, 3), 4); count != 3 || !errors.Is(err, failure) {
			t.Fatalf("write=%d err=%v", count, err)
		}
		if err := mutable.SetModifiedTime(catalog.ModifiedTime{}); !errors.Is(err, failure) {
			t.Fatalf("metadata write error=%v", err)
		}
	}
	for _, file := range []outputcap.FileIdentity{observed, recovery, mutable} {
		state.closed = false
		if err := file.Close(); !errors.Is(err, nativeFailure) || !state.closed {
			t.Fatalf("native close failure lost: %v", err)
		}
	}
}

func TestAbsentViewsDenyOperationsAndCloseIdempotently(t *testing.T) {
	var observed *Observed[*retainedState]
	var recovery *Recovery[*retainedState]
	var mutable *Mutable[*retainedState]
	for _, file := range []outputcap.FileIdentity{observed, recovery, mutable} {
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := file.Size(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("nil size=%v", err)
		}
		if _, err := file.SameFile(observed); !errors.Is(err, ErrClosed) {
			t.Fatalf("nil identity=%v", err)
		}
	}
	for _, file := range []outputcap.ObservedFile{observed, mutable} {
		if _, err := file.ReadAt(nil, 0); !errors.Is(err, ErrClosed) {
			t.Fatalf("nil read=%v", err)
		}
		if _, err := file.MetadataMatches(0, catalog.ModifiedTime{}); !errors.Is(err, ErrClosed) {
			t.Fatalf("nil metadata=%v", err)
		}
	}
	for _, file := range []outputcap.RecoveryDurabilityFile{recovery, mutable} {
		if err := file.Sync(); !errors.Is(err, ErrClosed) {
			t.Fatalf("nil flush=%v", err)
		}
	}
	if _, err := mutable.WriteAt(nil, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil write=%v", err)
	}
	if err := mutable.SetModifiedTime(catalog.ModifiedTime{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil metadata write=%v", err)
	}
}

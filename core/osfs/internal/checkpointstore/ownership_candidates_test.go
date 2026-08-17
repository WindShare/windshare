package checkpointstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestOwnershipInspectionAuthenticatesCandidatesAndClassifiesContention(t *testing.T) {
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Materializer:        checkpointmodel.MaterializerNativeTree,
		Certification:       checkpointmodel.CertificationWindowsNTFSProcessRestart,
		AuthorityRef:        bytes.Repeat([]byte{0x92}, receivecontract.AuthorityRefBytes),
		RootOpenDisposition: checkpointmodel.CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := checkpointmodel.EncodeOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	candidate := TemporaryName(OwnershipFile, encoded, 0)

	t.Run("foreign-image", func(t *testing.T) {
		directory := newMemoryDirectory()
		writeMemoryFile(t, directory, candidate, []byte("foreign-image"))
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, checkpointmodel.ErrInvalidOwnership) {
			t.Fatalf("foreign ownership candidate = status:%d error:%v", status, err)
		}
	})

	t.Run("non-file", func(t *testing.T) {
		directory := newMemoryDirectory()
		if _, err := directory.CreateDirectory(candidate, true); err != nil {
			t.Fatal(err)
		}
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, checkpointmodel.ErrInvalidOwnership) {
			t.Fatalf("non-file ownership candidate = status:%d error:%v", status, err)
		}
	})

	t.Run("unexpected-sibling", func(t *testing.T) {
		directory := newMemoryDirectory()
		writeMemoryFile(t, directory, candidate, encoded)
		writeMemoryFile(t, directory, "foreign-entry", []byte("foreign"))
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, checkpointmodel.ErrInvalidOwnership) {
			t.Fatalf("ownership candidate with sibling = status:%d error:%v", status, err)
		}
	})

	t.Run("observation-failure", func(t *testing.T) {
		failure := errors.New("candidate observation failed")
		base := newMemoryDirectory()
		writeMemoryFile(t, base, candidate, encoded)
		directory := &faultDirectory{
			Directory: base,
			observeEntry: func(name string) (outputcap.EntryKind, error) {
				if name == OwnershipFile {
					return outputcap.EntryAbsent, nil
				}
				return outputcap.EntryAbsent, failure
			},
		}
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, failure) {
			t.Fatalf("ownership candidate observation = status:%d error:%v", status, err)
		}
	})

	t.Run("in-flight-candidate", func(t *testing.T) {
		base := newMemoryDirectory()
		writeMemoryFile(t, base, candidate, encoded)
		directory := &faultDirectory{
			Directory: base,
			openFile: func(name string, private, writable bool) (outputcap.MutableFile, error) {
				if name == candidate {
					return nil, outputcap.ErrFixedLinkSourceChanged
				}
				return base.OpenMutableFile(name, private)
			},
		}
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, outputcap.ErrNamespaceLockBusy) ||
			errors.Is(err, checkpointmodel.ErrInvalidOwnership) {
			t.Fatalf("in-flight ownership candidate = status:%d error:%v", status, err)
		}
	})

	t.Run("in-flight-candidate-read", func(t *testing.T) {
		base := newMemoryDirectory()
		writeMemoryFile(t, base, candidate, encoded)
		directory := &faultDirectory{
			Directory: base,
			openFile: func(name string, private, writable bool) (outputcap.MutableFile, error) {
				opened, err := base.OpenMutableFile(name, private)
				if err != nil || name != candidate {
					return opened, err
				}
				return &faultFile{
					MutableFile: opened,
					readAt: func([]byte, int64) (int, error) {
						return 0, outputcap.ErrFixedLinkSourceChanged
					},
				}, nil
			},
		}
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, outputcap.ErrNamespaceLockBusy) ||
			errors.Is(err, checkpointmodel.ErrInvalidOwnership) {
			t.Fatalf("in-flight ownership candidate read = status:%d error:%v", status, err)
		}
	})

	t.Run("preallocated-candidate-before-first-write", func(t *testing.T) {
		directory := newMemoryDirectory()
		writeMemoryFile(t, directory, candidate, make([]byte, len(encoded)))
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, outputcap.ErrNamespaceLockBusy) ||
			errors.Is(err, checkpointmodel.ErrInvalidOwnership) {
			t.Fatalf("preallocated ownership candidate = status:%d error:%v", status, err)
		}
	})

	t.Run("marker-observation-contention", func(t *testing.T) {
		directory := &faultDirectory{
			Directory: newMemoryDirectory(),
			observeEntry: func(string) (outputcap.EntryKind, error) {
				return outputcap.EntryAbsent, outputcap.ErrFixedLinkSourceChanged
			},
		}
		status, err := inspectOwnership(directory, ownership)
		if status != 0 || !errors.Is(err, outputcap.ErrNamespaceLockBusy) {
			t.Fatalf("ownership marker observation contention = status:%d error:%v", status, err)
		}
	})

	t.Run("marker-read-contention", func(t *testing.T) {
		base := newMemoryDirectory()
		writeMemoryFile(t, base, OwnershipFile, encoded)
		directory := &faultDirectory{
			Directory: base,
			openFile: func(name string, private, writable bool) (outputcap.MutableFile, error) {
				if name == OwnershipFile {
					return nil, outputcap.ErrFixedLinkSourceChanged
				}
				return base.OpenMutableFile(name, private)
			},
		}
		status, err := inspectOwnership(directory, ownership)
		if status != 0 || !errors.Is(err, outputcap.ErrNamespaceLockBusy) {
			t.Fatalf("ownership marker read contention = status:%d error:%v", status, err)
		}
	})

	t.Run("candidate-enumeration-contention", func(t *testing.T) {
		directory := &faultDirectory{
			Directory: newMemoryDirectory(),
			names: func(int) ([]string, error) {
				return nil, outputcap.ErrFixedLinkSourceChanged
			},
		}
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, outputcap.ErrNamespaceLockBusy) {
			t.Fatalf("ownership candidate enumeration contention = status:%d error:%v", status, err)
		}
	})

	t.Run("candidate-observation-contention", func(t *testing.T) {
		base := newMemoryDirectory()
		directory := &faultDirectory{
			Directory: base,
			names: func(int) ([]string, error) {
				return []string{candidate}, nil
			},
			observeEntry: func(name string) (outputcap.EntryKind, error) {
				if name == OwnershipFile {
					return outputcap.EntryAbsent, nil
				}
				return outputcap.EntryAbsent, outputcap.ErrFixedLinkSourceChanged
			},
		}
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, outputcap.ErrNamespaceLockBusy) {
			t.Fatalf("ownership candidate observation contention = status:%d error:%v", status, err)
		}
	})

	t.Run("candidate-disappears-after-enumeration", func(t *testing.T) {
		base := newMemoryDirectory()
		directory := &faultDirectory{
			Directory: base,
			names: func(int) ([]string, error) {
				return []string{candidate}, nil
			},
			observeEntry: func(name string) (outputcap.EntryKind, error) {
				if name == OwnershipFile {
					return outputcap.EntryAbsent, nil
				}
				return outputcap.EntryRegularFile, nil
			},
		}
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, outputcap.ErrNamespaceLockBusy) {
			t.Fatalf("disappeared ownership candidate = status:%d error:%v", status, err)
		}
	})
}

package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOwnershipInspectionAuthenticatesCandidatesAndClassifiesContention(t *testing.T) {
	backend, err := transfer.NewOutputBackendID("checkpointstore-test")
	if err != nil {
		t.Fatal(err)
	}
	rootIdentity := bytes.Repeat([]byte{0x92}, sha256.Size)
	ownership, err := resumestate.NewFileCheckpointOwnership(string(backend), rootIdentity)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := resumestate.EncodeFileCheckpointOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	candidate := TemporaryName(OwnershipFile, encoded, 0)

	t.Run("foreign-image", func(t *testing.T) {
		directory := newMemoryDirectory()
		writeMemoryFile(t, directory, candidate, []byte("foreign-image"))
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, resumestate.ErrFileCheckpointOwnership) {
			t.Fatalf("foreign ownership candidate = status:%d error:%v", status, err)
		}
	})

	t.Run("non-file", func(t *testing.T) {
		directory := newMemoryDirectory()
		if _, err := directory.CreateDirectory(candidate, true); err != nil {
			t.Fatal(err)
		}
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, resumestate.ErrFileCheckpointOwnership) {
			t.Fatalf("non-file ownership candidate = status:%d error:%v", status, err)
		}
	})

	t.Run("unexpected-sibling", func(t *testing.T) {
		directory := newMemoryDirectory()
		writeMemoryFile(t, directory, candidate, encoded)
		writeMemoryFile(t, directory, "foreign-entry", []byte("foreign"))
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, resumestate.ErrFileCheckpointOwnership) {
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
			openFile: func(name string, private, writable bool) (outputcap.File, error) {
				if name == candidate {
					return nil, outputcap.ErrFixedLinkSourceChanged
				}
				return base.OpenFile(name, private, writable)
			},
		}
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, outputcap.ErrNamespaceLockBusy) ||
			errors.Is(err, resumestate.ErrFileCheckpointOwnership) {
			t.Fatalf("in-flight ownership candidate = status:%d error:%v", status, err)
		}
	})

	t.Run("in-flight-candidate-read", func(t *testing.T) {
		base := newMemoryDirectory()
		writeMemoryFile(t, base, candidate, encoded)
		directory := &faultDirectory{
			Directory: base,
			openFile: func(name string, private, writable bool) (outputcap.File, error) {
				opened, err := base.OpenFile(name, private, writable)
				if err != nil || name != candidate {
					return opened, err
				}
				return &faultFile{
					File: opened,
					readAt: func([]byte, int64) (int, error) {
						return 0, outputcap.ErrFixedLinkSourceChanged
					},
				}, nil
			},
		}
		status, err := inspectOwnership(directory, ownership)
		if status != OwnershipMismatch || !errors.Is(err, outputcap.ErrNamespaceLockBusy) ||
			errors.Is(err, resumestate.ErrFileCheckpointOwnership) ||
			errors.Is(err, resumestate.ErrFileCheckpointBinding) {
			t.Fatalf("in-flight ownership candidate read = status:%d error:%v", status, err)
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
			openFile: func(name string, private, writable bool) (outputcap.File, error) {
				if name == OwnershipFile {
					return nil, outputcap.ErrFixedLinkSourceChanged
				}
				return base.OpenFile(name, private, writable)
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

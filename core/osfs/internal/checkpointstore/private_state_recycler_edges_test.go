package checkpointstore

import (
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

var errRecyclerFake = errors.New("private state recycler fake failure")

func TestPrivateStateRecyclerAcceptsMissingCurrentGenerationState(t *testing.T) {
	if empty, err := RecyclePrivateState(nil); err != nil || empty {
		t.Fatalf("nil control recycle = (%t, %v)", empty, err)
	}

	t.Run("both namespaces absent", func(t *testing.T) {
		control := newMemoryDirectory()
		if empty, err := RecyclePrivateState(control); err != nil || !empty {
			t.Fatalf("empty control recycle = (%t, %v)", empty, err)
		}
	})

	t.Run("ordinary namespace absent", func(t *testing.T) {
		control := newMemoryDirectory()
		journal, err := OpenLiveCleanupJournal(control)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		if empty, err := RecyclePrivateState(control); err != nil || !empty {
			t.Fatalf("cleanup-only recycle = (%t, %v)", empty, err)
		}
	})

	t.Run("cleanup namespace absent", func(t *testing.T) {
		control := newMemoryDirectory()
		registry, err := OpenOperationRegistry(control)
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
		if empty, err := RecyclePrivateState(control); err != nil || !empty {
			t.Fatalf("ordinary-only recycle = (%t, %v)", empty, err)
		}
	})
}

func TestPrivateStateRecyclerPreservesUnrecognizedOrNonemptyShapes(t *testing.T) {
	tests := map[string]func(*memoryDirectory, *OperationRegistry, *LiveCleanupJournal){
		"unknown registry entry": func(control *memoryDirectory, _ *OperationRegistry, _ *LiveCleanupJournal) {
			root := control.dirs[OrdinaryRegistryDirectoryV1]
			root.dirs["foreign"] = newMemoryDirectory()
		},
		"malformed active child": func(_ *memoryDirectory, registry *OperationRegistry, _ *LiveCleanupJournal) {
			registry.active.(*memoryDirectory).dirs["NOT-LOWER-HEX"] = newMemoryDirectory()
		},
		"candidate child is a file": func(_ *memoryDirectory, registry *OperationRegistry, _ *LiveCleanupJournal) {
			name := strings.Repeat("a", 64)
			registry.candidates.(*memoryDirectory).files[name] = &memoryFileData{}
		},
		"claim registry is nonempty": func(_ *memoryDirectory, registry *OperationRegistry, _ *LiveCleanupJournal) {
			registry.claims.(*memoryDirectory).files["claim"] = &memoryFileData{}
		},
		"unknown lease": func(_ *memoryDirectory, registry *OperationRegistry, _ *LiveCleanupJournal) {
			registry.leases.(*memoryDirectory).files["foreign.lock"] = &memoryFileData{}
		},
		"lease carrier has wrong kind": func(_ *memoryDirectory, registry *OperationRegistry, _ *LiveCleanupJournal) {
			name := strings.Repeat("b", 32) + ordinaryOperationLockSuffix
			registry.leases.(*memoryDirectory).dirs[name] = newMemoryDirectory()
		},
		"cleanup ticket remains": func(control *memoryDirectory, _ *OperationRegistry, _ *LiveCleanupJournal) {
			proof := control.dirs[checkpointmodel.LiveCleanupNamespaceV1]
			proof.dirs[liveCleanupTicketsDirectory].files["ticket"] = &memoryFileData{}
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			control, registry, journal := recyclerFixture(t)
			mutate(control, &registry, &journal)
			closeRecyclerFixture(t, &registry, &journal)
			if empty, err := RecyclePrivateState(control); err != nil || empty {
				t.Fatalf("recycle result = (%t, %v)", empty, err)
			}
		})
	}
}

func TestPrivateStateRecyclerRecognizesEveryStableLockCarrier(t *testing.T) {
	control, registry, journal := recyclerFixture(t)
	for name := range map[string]struct{}{
		strings.Repeat("1", 32) + ordinaryOperationLockSuffix: {},
		strings.Repeat("2", 64) + ordinaryActiveLockSuffix:    {},
		strings.Repeat("3", 64) + ordinaryClaimLockSuffix:     {},
	} {
		lock, _, err := registry.leases.AcquireLock(name, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
	}
	closeRecyclerFixture(t, &registry, &journal)
	if empty, err := RecyclePrivateState(control); err != nil || !empty {
		t.Fatalf("stable-lock recycle = (%t, %v)", empty, err)
	}
}

func TestPrivateStateRecyclerNameGrammar(t *testing.T) {
	for name, testCase := range map[string]struct {
		name  string
		bytes int
		want  bool
	}{
		"valid":       {name: strings.Repeat("a", 32), bytes: 16, want: true},
		"uppercase":   {name: strings.Repeat("A", 32), bytes: 16, want: false},
		"invalid hex": {name: strings.Repeat("g", 32), bytes: 16, want: false},
		"wrong size":  {name: strings.Repeat("a", 30), bytes: 16, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := validLowerHexName(testCase.name, testCase.bytes); got != testCase.want {
				t.Fatalf(
					"validLowerHexName(%q, %d) = %t, want %t",
					testCase.name,
					testCase.bytes,
					got,
					testCase.want,
				)
			}
		})
	}

	if validOrdinaryLockName("foreign.lock") {
		t.Fatal("unknown stable lock suffix was accepted")
	}
}

func TestPrivateStateRecyclerPropagatesInspectionFailures(t *testing.T) {
	t.Run("ordinary classification", func(t *testing.T) {
		control := &recyclerFaultDirectory{
			Directory: newMemoryDirectory(),
			classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryAbsent, false, errRecyclerFake
			},
		}
		if empty, err := RecyclePrivateState(control); empty || !errors.Is(err, errRecyclerFake) {
			t.Fatalf("ordinary inspection = (%t, %v)", empty, err)
		}
	})

	t.Run("cleanup classification", func(t *testing.T) {
		base := newMemoryDirectory()
		control := &recyclerFaultDirectory{
			Directory: base,
			classify: func(name string) (outputcap.EntryKind, bool, error) {
				if name == checkpointmodel.LiveCleanupNamespaceV1 {
					return outputcap.EntryAbsent, false, errRecyclerFake
				}
				return base.ClassifyExactEntry(name)
			},
		}
		if empty, err := RecyclePrivateState(control); empty || !errors.Is(err, errRecyclerFake) {
			t.Fatalf("cleanup inspection = (%t, %v)", empty, err)
		}
	})

	t.Run("classified directory cannot be opened", func(t *testing.T) {
		base := newMemoryDirectory()
		if _, err := base.CreateDirectory("state", true); err != nil {
			t.Fatal(err)
		}
		parent := &recyclerFaultDirectory{
			Directory: base,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return nil, errRecyclerFake
			},
		}
		if _, found, safe, err := openRecycleDirectory(parent, "state"); !found || safe ||
			!errors.Is(err, errRecyclerFake) {
			t.Fatalf("unopenable directory = (found %t, safe %t, %v)", found, safe, err)
		}
	})

	for name, namesErr := range map[string]error{
		"bounded enumeration": outputcap.ErrUnsafeNamespace,
		"filesystem error":    errRecyclerFake,
	} {
		t.Run("entry authentication "+name, func(t *testing.T) {
			directory := &recyclerFaultDirectory{
				Directory: newMemoryDirectory(),
				names: func(int) ([]string, error) {
					return nil, namesErr
				},
			}
			safe, err := authenticateRecycleEntries(directory, map[string]outputcap.EntryKind{})
			if safe {
				t.Fatal("failed enumeration was authenticated")
			}
			if errors.Is(namesErr, outputcap.ErrUnsafeNamespace) {
				if err != nil {
					t.Fatalf("bounded enumeration error = %v", err)
				}
			} else if !errors.Is(err, namesErr) {
				t.Fatalf("enumeration error = %v", err)
			}
		})
	}

	t.Run("entry classification", func(t *testing.T) {
		base := newMemoryDirectory()
		if _, err := base.CreateDirectory("known", true); err != nil {
			t.Fatal(err)
		}
		directory := &recyclerFaultDirectory{
			Directory: base,
			classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryAbsent, false, errRecyclerFake
			},
		}
		safe, err := authenticateRecycleEntries(directory, map[string]outputcap.EntryKind{
			"known": outputcap.EntryDirectory,
		})
		if safe || !errors.Is(err, errRecyclerFake) {
			t.Fatalf("classification failure = (%t, %v)", safe, err)
		}
	})

	t.Run("empty-directory enumeration", func(t *testing.T) {
		directory := &recyclerFaultDirectory{
			Directory: newMemoryDirectory(),
			names: func(int) ([]string, error) {
				return nil, outputcap.ErrUnsafeNamespace
			},
		}
		if empty, err := recycleDirectoryIsEmpty(directory); err != nil || empty {
			t.Fatalf("bounded empty check = (%t, %v)", empty, err)
		}
	})

	for name, namesErr := range map[string]error{
		"bounded proof": outputcap.ErrUnsafeNamespace,
		"proof failure": errRecyclerFake,
	} {
		t.Run(name, func(t *testing.T) {
			control := newMemoryDirectory()
			proof, err := control.CreateDirectory(checkpointmodel.LiveCleanupNamespaceV1, true)
			if err != nil {
				t.Fatal(err)
			}
			parent := &recyclerFaultDirectory{
				Directory: control,
				openDirectory: func(string, bool) (outputcap.Directory, error) {
					return &recyclerFaultDirectory{
						Directory: proof,
						names: func(int) ([]string, error) {
							return nil, namesErr
						},
					}, nil
				},
			}
			empty, err := recycleLiveCleanupJournal(parent)
			if empty {
				t.Fatal("uninspectable proof was recycled")
			}
			if errors.Is(namesErr, outputcap.ErrUnsafeNamespace) {
				if err != nil {
					t.Fatalf("bounded proof error = %v", err)
				}
			} else if !errors.Is(err, namesErr) {
				t.Fatalf("proof error = %v", err)
			}
		})
	}
}

func TestPrivateStateRecyclerPropagatesMutationFailures(t *testing.T) {
	t.Run("cleanup ticket namespace has wrong kind", func(t *testing.T) {
		control := newMemoryDirectory()
		proof, err := control.CreateDirectory(checkpointmodel.LiveCleanupNamespaceV1, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := proof.CreateFile(liveCleanupTicketsDirectory, true, 0); err != nil {
			t.Fatal(err)
		}
		if empty, err := recycleLiveCleanupJournal(control); err != nil || empty {
			t.Fatalf("wrong ticket kind = (%t, %v)", empty, err)
		}
	})

	t.Run("cleanup ticket removal", func(t *testing.T) {
		control := newMemoryDirectory()
		journal, err := OpenLiveCleanupJournal(control)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		proof := control.dirs[checkpointmodel.LiveCleanupNamespaceV1]
		parent := &recyclerFaultDirectory{
			Directory: control,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return &recyclerFaultDirectory{
					Directory: proof,
					removeDirectory: func(string, outputcap.Directory) error {
						return errRecyclerFake
					},
				}, nil
			},
		}
		if empty, err := recycleLiveCleanupJournal(parent); empty || !errors.Is(err, errRecyclerFake) {
			t.Fatalf("ticket removal = (%t, %v)", empty, err)
		}
	})

	t.Run("cleanup proof removal", func(t *testing.T) {
		control := newMemoryDirectory()
		journal, err := OpenLiveCleanupJournal(control)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		parent := &recyclerFaultDirectory{
			Directory: control,
			removeDirectory: func(string, outputcap.Directory) error {
				return errRecyclerFake
			},
		}
		if empty, err := recycleLiveCleanupJournal(parent); empty || !errors.Is(err, errRecyclerFake) {
			t.Fatalf("proof removal = (%t, %v)", empty, err)
		}
	})

	t.Run("ordinary root removal", func(t *testing.T) {
		control := newMemoryDirectory()
		registry, err := OpenOperationRegistry(control)
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
		parent := &recyclerFaultDirectory{
			Directory: control,
			removeDirectory: func(string, outputcap.Directory) error {
				return errRecyclerFake
			},
		}
		if empty, err := recycleOrdinaryRegistry(parent); empty || !errors.Is(err, errRecyclerFake) {
			t.Fatalf("ordinary removal = (%t, %v)", empty, err)
		}
	})

	for name, namesErr := range map[string]error{
		"bounded children": outputcap.ErrUnsafeNamespace,
		"child failure":    errRecyclerFake,
	} {
		t.Run(name, func(t *testing.T) {
			base := newMemoryDirectory()
			child, err := base.CreateDirectory("children", true)
			if err != nil {
				t.Fatal(err)
			}
			root := &recyclerFaultDirectory{
				Directory: base,
				openDirectory: func(string, bool) (outputcap.Directory, error) {
					return &recyclerFaultDirectory{
						Directory: child,
						names: func(int) ([]string, error) {
							return nil, namesErr
						},
					}, nil
				},
			}
			empty, err := recycleEmptyDirectoryChildren(root, recycleChildSpec{name: "children", childNameHexBytes: 16})
			if empty {
				t.Fatal("uninspectable children were recycled")
			}
			if errors.Is(namesErr, outputcap.ErrUnsafeNamespace) {
				if err != nil {
					t.Fatalf("bounded child error = %v", err)
				}
			} else if !errors.Is(err, namesErr) {
				t.Fatalf("child error = %v", err)
			}
		})
	}

	for name, namesErr := range map[string]error{
		"bounded leases": outputcap.ErrUnsafeNamespace,
		"lease failure":  errRecyclerFake,
	} {
		t.Run(name, func(t *testing.T) {
			base := newMemoryDirectory()
			leases, err := base.CreateDirectory(ordinaryLeasesDirectory, true)
			if err != nil {
				t.Fatal(err)
			}
			root := &recyclerFaultDirectory{
				Directory: base,
				openDirectory: func(string, bool) (outputcap.Directory, error) {
					return &recyclerFaultDirectory{
						Directory: leases,
						names: func(int) ([]string, error) {
							return nil, namesErr
						},
					}, nil
				},
			}
			empty, err := recycleLeaseDirectory(root)
			if empty {
				t.Fatal("uninspectable leases were recycled")
			}
			if errors.Is(namesErr, outputcap.ErrUnsafeNamespace) {
				if err != nil {
					t.Fatalf("bounded lease error = %v", err)
				}
			} else if !errors.Is(err, namesErr) {
				t.Fatalf("lease error = %v", err)
			}
		})
	}

	t.Run("unknown authenticated entry", func(t *testing.T) {
		directory := newMemoryDirectory()
		if _, err := directory.CreateDirectory("foreign", true); err != nil {
			t.Fatal(err)
		}
		safe, err := authenticateRecycleEntries(directory, map[string]outputcap.EntryKind{
			"known": outputcap.EntryDirectory,
		})
		if err != nil || safe {
			t.Fatalf("unknown entry authentication = (%t, %v)", safe, err)
		}
	})
}

type recyclerFaultDirectory struct {
	outputcap.Directory
	classify        func(string) (outputcap.EntryKind, bool, error)
	names           func(int) ([]string, error)
	openDirectory   func(string, bool) (outputcap.Directory, error)
	removeDirectory func(string, outputcap.Directory) error
}

func (directory *recyclerFaultDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if directory.classify != nil {
		return directory.classify(name)
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *recyclerFaultDirectory) Names(limit int) ([]string, error) {
	if directory.names != nil {
		return directory.names(limit)
	}
	return directory.Directory.Names(limit)
}

func (directory *recyclerFaultDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory.openDirectory != nil {
		return directory.openDirectory(name, private)
	}
	return directory.Directory.OpenDirectory(name, private)
}

func (directory *recyclerFaultDirectory) RemoveDirectory(name string, expected outputcap.Directory) error {
	if directory.removeDirectory != nil {
		return directory.removeDirectory(name, expected)
	}
	return directory.Directory.RemoveDirectory(name, expected)
}

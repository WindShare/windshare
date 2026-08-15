package checkpointstore

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type operationRegistryFaultDirectory struct {
	outputcap.Directory
	openDirectory   func(string, bool) (outputcap.Directory, error)
	createDirectory func(string, bool) (outputcap.Directory, error)
	acquireLock     func(string, bool) (outputcap.Lock, bool, error)
	classify        func(string) (outputcap.EntryKind, bool, error)
	names           func(int) ([]string, error)
	sync            func() error
	close           func() error
}

func (directory *operationRegistryFaultDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if directory.openDirectory != nil {
		return directory.openDirectory(name, private)
	}
	return directory.Directory.OpenDirectory(name, private)
}

func (directory *operationRegistryFaultDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if directory.createDirectory != nil {
		return directory.createDirectory(name, private)
	}
	return directory.Directory.CreateDirectory(name, private)
}

func (directory *operationRegistryFaultDirectory) AcquireLock(
	name string,
	private bool,
) (outputcap.Lock, bool, error) {
	if directory.acquireLock != nil {
		return directory.acquireLock(name, private)
	}
	return directory.Directory.AcquireLock(name, private)
}

func (directory *operationRegistryFaultDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	if directory.classify != nil {
		return directory.classify(name)
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *operationRegistryFaultDirectory) Names(limit int) ([]string, error) {
	if directory.names != nil {
		return directory.names(limit)
	}
	return directory.Directory.Names(limit)
}

func (directory *operationRegistryFaultDirectory) Sync() error {
	if directory.sync != nil {
		return directory.sync()
	}
	return directory.Directory.Sync()
}

func (directory *operationRegistryFaultDirectory) Close() error {
	if directory.close != nil {
		return directory.close()
	}
	return directory.Directory.Close()
}

func operationRegistryControl(
	t *testing.T,
	wrapRoot func(*memoryDirectory) outputcap.Directory,
) (*memoryDirectory, outputcap.Directory) {
	t.Helper()
	control := newMemoryDirectory()
	created, err := control.CreateDirectory(OrdinaryRegistryDirectoryV1, true)
	if err != nil {
		t.Fatal(err)
	}
	root := created.(*memoryDirectory)
	return root, &operationRegistryFaultDirectory{
		Directory: control,
		openDirectory: func(name string, private bool) (outputcap.Directory, error) {
			if name == OrdinaryRegistryDirectoryV1 {
				return wrapRoot(root), nil
			}
			return control.OpenDirectory(name, private)
		},
	}
}

func TestOpenOperationRegistryClosesEveryPartialConstructionCut(t *testing.T) {
	failure := errors.New("registry directory unavailable")
	for _, failedName := range []string{
		ordinaryOperationsDirectory,
		ordinaryActiveDirectory,
		ordinaryLeasesDirectory,
		ordinaryClaimsDirectory,
		ordinaryCandidatesDirectory,
	} {
		t.Run(failedName, func(t *testing.T) {
			root, control := operationRegistryControl(t, func(root *memoryDirectory) outputcap.Directory {
				return &operationRegistryFaultDirectory{
					Directory: root,
					createDirectory: func(name string, private bool) (outputcap.Directory, error) {
						if name == failedName {
							return nil, failure
						}
						return root.CreateDirectory(name, private)
					},
				}
			})
			if _, err := OpenOperationRegistry(control); !errors.Is(err, failure) {
				t.Fatalf("open registry error = %v", err)
			}
			if len(root.dirs) >= len(ordinaryRegistryEntries) {
				t.Fatal("faulted registry unexpectedly completed every namespace")
			}
		})
	}

	t.Run("root open", func(t *testing.T) {
		_, control := operationRegistryControl(t, func(*memoryDirectory) outputcap.Directory {
			return nil
		})
		openFailure := errors.New("registry root open failed")
		control = &operationRegistryFaultDirectory{
			Directory: control,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return nil, openFailure
			},
		}
		if _, err := OpenOperationRegistry(control); !errors.Is(err, openFailure) {
			t.Fatalf("root open error = %v", err)
		}
	})

	t.Run("authentication and close", func(t *testing.T) {
		closeFailure := errors.New("registry close failed")
		root, control := operationRegistryControl(t, func(root *memoryDirectory) outputcap.Directory {
			return &operationRegistryFaultDirectory{
				Directory: root,
				close:     func() error { return closeFailure },
			}
		})
		file, err := root.CreateFile("foreign", true, 0)
		if err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		if _, err := OpenOperationRegistry(control); !errors.Is(err, outputcap.ErrUnsafeNamespace) || !errors.Is(err, closeFailure) {
			t.Fatalf("registry authentication error = %v", err)
		}
	})
}

func TestBeginActiveFailsClosedAcrossLockAndLookupCuts(t *testing.T) {
	fixtureSeed := byte(0xe1)
	t.Run("lock acquisition", func(t *testing.T) {
		registry, err := OpenOperationRegistry(newMemoryDirectory())
		if err != nil {
			t.Fatal(err)
		}
		defer registry.Close()
		fixture := ordinaryRegistryFixture(t, fixtureSeed)
		failure := errors.New("active lock failed")
		registry.leases = &operationRegistryFaultDirectory{
			Directory: registry.leases,
			acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
				return nil, false, failure
			},
		}
		if _, _, err := registry.BeginActive(fixture.key); !errors.Is(err, failure) {
			t.Fatalf("lock failure = %v", err)
		}
	})

	t.Run("nil lock", func(t *testing.T) {
		registry, err := OpenOperationRegistry(newMemoryDirectory())
		if err != nil {
			t.Fatal(err)
		}
		defer registry.Close()
		fixture := ordinaryRegistryFixture(t, fixtureSeed+1)
		registry.leases = &operationRegistryFaultDirectory{
			Directory: registry.leases,
			acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
				return nil, false, nil
			},
		}
		if _, _, err := registry.BeginActive(fixture.key); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("nil lock failure = %v", err)
		}
	})

	t.Run("new lock sync", func(t *testing.T) {
		registry, err := OpenOperationRegistry(newMemoryDirectory())
		if err != nil {
			t.Fatal(err)
		}
		defer registry.Close()
		fixture := ordinaryRegistryFixture(t, fixtureSeed+2)
		failure := errors.New("active lock sync failed")
		registry.leases = &operationRegistryFaultDirectory{
			Directory: registry.leases,
			sync:      func() error { return failure },
		}
		if _, _, err := registry.BeginActive(fixture.key); !errors.Is(err, failure) {
			t.Fatalf("lock sync failure = %v", err)
		}
	})

	t.Run("active index open", func(t *testing.T) {
		registry, err := OpenOperationRegistry(newMemoryDirectory())
		if err != nil {
			t.Fatal(err)
		}
		defer registry.Close()
		fixture := ordinaryRegistryFixture(t, fixtureSeed+3)
		active := registry.active.(*memoryDirectory)
		if _, err := active.CreateDirectory(activeKeyName(fixture.key), true); err != nil {
			t.Fatal(err)
		}
		failure := errors.New("active index open failed")
		registry.active = &operationRegistryFaultDirectory{
			Directory: active,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return nil, failure
			},
		}
		if _, _, err := registry.BeginActive(fixture.key); !errors.Is(err, failure) {
			t.Fatalf("lookup failure = %v", err)
		}
	})

	t.Run("candidate survives admission lease release", func(t *testing.T) {
		registry, err := OpenOperationRegistry(newMemoryDirectory())
		if err != nil {
			t.Fatal(err)
		}
		defer registry.Close()
		fixture := ordinaryRegistryFixture(t, fixtureSeed+4)
		admission, lookup, err := registry.BeginActive(fixture.key)
		if err != nil || lookup.State() != ActiveLookupNone {
			t.Fatalf("initial admission = (%d, %v)", lookup.State(), err)
		}
		if err := admission.PrepareCandidate(fixture.intent.OperationID()); err != nil {
			t.Fatal(err)
		}
		if err := admission.Close(); err != nil {
			t.Fatal(err)
		}
		second, lookup, err := registry.BeginActive(fixture.key)
		if err != nil || lookup.State() != ActiveLookupNeedsAttention {
			t.Fatalf("candidate lookup = (%d, %v)", lookup.State(), err)
		}
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestActiveLookupTreatsUnboundedOrMalformedIndexesAsAmbiguous(t *testing.T) {
	for _, test := range []struct {
		name      string
		populate  func(t *testing.T, index *memoryDirectory)
		wrapIndex func(*memoryDirectory) outputcap.Directory
	}{
		{
			name: "enumeration failure",
			wrapIndex: func(index *memoryDirectory) outputcap.Directory {
				return &operationRegistryFaultDirectory{
					Directory: index,
					names: func(int) ([]string, error) {
						return nil, errors.New("active index listing failed")
					},
				}
			},
		},
		{
			name: "multiple entries",
			populate: func(t *testing.T, index *memoryDirectory) {
				t.Helper()
				for _, name := range []string{"first", "second"} {
					if err := InstallCreate(index, name, []byte{1}); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "malformed operation",
			populate: func(t *testing.T, index *memoryDirectory) {
				t.Helper()
				if err := InstallCreate(index, "malformed", []byte{1}); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, err := OpenOperationRegistry(newMemoryDirectory())
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			fixture := ordinaryRegistryFixture(t, fixtureSeedForName(test.name))
			active := registry.active.(*memoryDirectory)
			created, err := active.CreateDirectory(activeKeyName(fixture.key), true)
			if err != nil {
				t.Fatal(err)
			}
			index := created.(*memoryDirectory)
			if test.populate != nil {
				test.populate(t, index)
			}
			if test.wrapIndex != nil {
				registry.active = &operationRegistryFaultDirectory{
					Directory: active,
					openDirectory: func(string, bool) (outputcap.Directory, error) {
						return test.wrapIndex(index), nil
					},
				}
			}
			lookup, err := registry.LookupActive(fixture.key)
			if err != nil || lookup.State() != ActiveLookupAmbiguous {
				t.Fatalf("ambiguous lookup = (%d, %v)", lookup.State(), err)
			}
		})
	}
}

func fixtureSeedForName(name string) byte {
	var seed byte
	for index := range len(name) {
		seed += name[index]
	}
	if seed == 0 {
		return 1
	}
	return seed
}

type terminalFileStateFixture struct {
	lease       *OperationRegistryLease
	operation   *memoryDirectory
	files       *memoryDirectory
	checkpoints *memoryDirectory
}

func openTerminalFileStateFixture(t *testing.T, seed byte) terminalFileStateFixture {
	t.Helper()
	_, registry, lease, repository, _, _ := openRepositoryFixture(t, seed)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	previous := lease.Record()
	completed, err := checkpointmodel.NextOrdinaryOperationRecord(
		previous,
		checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle:    checkpointmodel.OrdinaryOperationCompleted,
			Lease:        checkpointmodel.OrdinaryLeaseHeld,
			ClosedReason: checkpointmodel.OrdinaryReasonNone,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Replace(previous, completed); err != nil {
		t.Fatal(err)
	}
	operations := lease.registry.operations.(*memoryDirectory)
	operation := operations.dirsForTest(t, operationNamespaceName(lease.operation))
	files := operation.dirsForTest(t, ordinaryFileStateDirectory)
	checkpoints := files.dirsForTest(t, CheckpointsDirectory)
	return terminalFileStateFixture{
		lease: lease, operation: operation, files: files, checkpoints: checkpoints,
	}
}

func (fixture terminalFileStateFixture) wrapOperation(
	open func(string, bool) (outputcap.Directory, error),
	classify func(string) (outputcap.EntryKind, bool, error),
) {
	operations := fixture.lease.registry.operations.(*memoryDirectory)
	fixture.lease.registry.operations = &operationRegistryFaultDirectory{
		Directory: operations,
		openDirectory: func(name string, private bool) (outputcap.Directory, error) {
			if name == operationNamespaceName(fixture.lease.operation) {
				if open != nil {
					return open(name, private)
				}
				return &operationRegistryFaultDirectory{
					Directory: fixture.operation,
					classify:  classify,
				}, nil
			}
			return operations.OpenDirectory(name, private)
		},
	}
}

func TestTerminalFileStateCleanupFailsClosedAtEveryNamespaceLayer(t *testing.T) {
	t.Run("operation open", func(t *testing.T) {
		fixture := openTerminalFileStateFixture(t, 0xa1)
		failure := errors.New("terminal operation open failed")
		fixture.wrapOperation(func(string, bool) (outputcap.Directory, error) {
			return nil, failure
		}, nil)
		if err := fixture.lease.CleanupEmptyFileState(); !errors.Is(err, failure) {
			t.Fatalf("operation open failure = %v", err)
		}
	})

	t.Run("file state classification", func(t *testing.T) {
		fixture := openTerminalFileStateFixture(t, 0xa2)
		fixture.wrapOperation(nil, func(name string) (outputcap.EntryKind, bool, error) {
			if name == ordinaryFileStateDirectory {
				return outputcap.EntryDirectory, false, nil
			}
			return fixture.operation.ClassifyExactEntry(name)
		})
		if err := fixture.lease.CleanupEmptyFileState(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("file state classification failure = %v", err)
		}
	})

	t.Run("file state wrong kind", func(t *testing.T) {
		fixture := openTerminalFileStateFixture(t, 0xa3)
		fixture.wrapOperation(nil, func(name string) (outputcap.EntryKind, bool, error) {
			if name == ordinaryFileStateDirectory {
				return outputcap.EntryRegularFile, true, nil
			}
			return fixture.operation.ClassifyExactEntry(name)
		})
		if err := fixture.lease.CleanupEmptyFileState(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("file state kind failure = %v", err)
		}
	})

	t.Run("file state open", func(t *testing.T) {
		fixture := openTerminalFileStateFixture(t, 0xa4)
		failure := errors.New("terminal files open failed")
		fixture.wrapOperation(func(string, bool) (outputcap.Directory, error) {
			return &operationRegistryFaultDirectory{
				Directory: fixture.operation,
				openDirectory: func(name string, private bool) (outputcap.Directory, error) {
					if name == ordinaryFileStateDirectory {
						return nil, failure
					}
					return fixture.operation.OpenDirectory(name, private)
				},
			}, nil
		}, nil)
		if err := fixture.lease.CleanupEmptyFileState(); !errors.Is(err, failure) {
			t.Fatalf("file state open failure = %v", err)
		}
	})

	t.Run("unknown file state entry", func(t *testing.T) {
		fixture := openTerminalFileStateFixture(t, 0xa5)
		file, err := fixture.files.CreateFile("foreign", true, 0)
		if err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		if err := fixture.lease.CleanupEmptyFileState(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("foreign file state failure = %v", err)
		}
	})

	t.Run("checkpoint classification", func(t *testing.T) {
		fixture := openTerminalFileStateFixture(t, 0xa6)
		fixture.wrapOperation(func(string, bool) (outputcap.Directory, error) {
			return &operationRegistryFaultDirectory{
				Directory: fixture.operation,
				openDirectory: func(name string, private bool) (outputcap.Directory, error) {
					if name == ordinaryFileStateDirectory {
						return &operationRegistryFaultDirectory{
							Directory: fixture.files,
							classify: func(name string) (outputcap.EntryKind, bool, error) {
								if name == CheckpointsDirectory {
									return outputcap.EntryDirectory, false, nil
								}
								return fixture.files.ClassifyExactEntry(name)
							},
						}, nil
					}
					return fixture.operation.OpenDirectory(name, private)
				},
			}, nil
		}, nil)
		if err := fixture.lease.CleanupEmptyFileState(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("checkpoint classification failure = %v", err)
		}
	})

	t.Run("checkpoint wrong kind", func(t *testing.T) {
		fixture := openTerminalFileStateFixture(t, 0xa7)
		fixture.wrapOperation(func(string, bool) (outputcap.Directory, error) {
			return &operationRegistryFaultDirectory{
				Directory: fixture.operation,
				openDirectory: func(name string, private bool) (outputcap.Directory, error) {
					if name == ordinaryFileStateDirectory {
						return &operationRegistryFaultDirectory{
							Directory: fixture.files,
							classify: func(name string) (outputcap.EntryKind, bool, error) {
								if name == CheckpointsDirectory {
									return outputcap.EntryRegularFile, true, nil
								}
								return fixture.files.ClassifyExactEntry(name)
							},
						}, nil
					}
					return fixture.operation.OpenDirectory(name, private)
				},
			}, nil
		}, nil)
		if err := fixture.lease.CleanupEmptyFileState(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("checkpoint kind failure = %v", err)
		}
	})

	t.Run("checkpoint namespace wrong kind", func(t *testing.T) {
		fixture := openTerminalFileStateFixture(t, 0xa8)
		delete(fixture.checkpoints.dirs, RecordsDirectory)
		fixture.wrapOperation(func(string, bool) (outputcap.Directory, error) {
			return &operationRegistryFaultDirectory{
				Directory: fixture.operation,
				openDirectory: func(name string, private bool) (outputcap.Directory, error) {
					if name == ordinaryFileStateDirectory {
						return &operationRegistryFaultDirectory{
							Directory: fixture.files,
							openDirectory: func(name string, private bool) (outputcap.Directory, error) {
								if name == CheckpointsDirectory {
									return &operationRegistryFaultDirectory{
										Directory: fixture.checkpoints,
										classify: func(name string) (outputcap.EntryKind, bool, error) {
											if name == RecordsDirectory {
												return outputcap.EntryRegularFile, true, nil
											}
											return fixture.checkpoints.ClassifyExactEntry(name)
										},
									}, nil
								}
								return fixture.files.OpenDirectory(name, private)
							},
						}, nil
					}
					return fixture.operation.OpenDirectory(name, private)
				},
			}, nil
		}, nil)
		if err := fixture.lease.CleanupEmptyFileState(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("checkpoint namespace failure = %v", err)
		}
	})

	t.Run("nonempty checkpoint shard root", func(t *testing.T) {
		fixture := openTerminalFileStateFixture(t, 0xa9)
		records := fixture.checkpoints.dirsForTest(t, RecordsDirectory)
		file, err := records.CreateFile("foreign", true, 0)
		if err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		if err := fixture.lease.CleanupEmptyFileState(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("checkpoint shard cleanup failure = %v", err)
		}
	})
}

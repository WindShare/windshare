package resumeauthority

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func TestResumeClosureDirectoryPinsRejectEveryIdentityRace(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*resumeClosureTree, *memoryDirectory, *memoryDirectory)
		want      error
	}{
		{
			name: "classification failure",
			configure: func(tree *resumeClosureTree, parent, _ *memoryDirectory) {
				tree.behavior(parent).classify = func(string) (outputcap.EntryKind, bool, error) {
					return outputcap.EntryAbsent, false, errResumeClosureClassify
				}
			},
			want: errResumeClosureClassify,
		},
		{
			name: "entry open failure",
			configure: func(tree *resumeClosureTree, parent, _ *memoryDirectory) {
				tree.behavior(parent).openEntry = func(string) (outputcap.CurrentEntryReference, error) {
					return nil, errResumeClosureOpen
				}
			},
			want: errResumeClosureOpen,
		},
		{
			name: "nil entry pin",
			configure: func(tree *resumeClosureTree, parent, _ *memoryDirectory) {
				tree.behavior(parent).openEntry = func(string) (outputcap.CurrentEntryReference, error) {
					return nil, nil
				}
			},
			want: outputcap.ErrUnsafeNamespace,
		},
		{
			name: "wrong entry kind",
			configure: func(tree *resumeClosureTree, parent, _ *memoryDirectory) {
				tree.behavior(parent).openEntry = func(string) (outputcap.CurrentEntryReference, error) {
					return &resumeClosureReference{kind: outputcap.EntryRegularFile}, nil
				}
			},
			want: outputcap.ErrUnsafeNamespace,
		},
		{
			name: "first identity check fails",
			configure: func(tree *resumeClosureTree, parent, _ *memoryDirectory) {
				tree.behavior(parent).entryMatch = func(
					string, outputcap.CurrentEntryReference,
				) (bool, error) {
					return false, errResumeClosureMatch
				}
			},
			want: errResumeClosureMatch,
		},
		{
			name: "replaced before open",
			configure: func(tree *resumeClosureTree, parent, _ *memoryDirectory) {
				tree.behavior(parent).entryMatch = func(
					string, outputcap.CurrentEntryReference,
				) (bool, error) {
					return false, nil
				}
			},
			want: errResumePinReplaced,
		},
		{
			name: "pinned open fails",
			configure: func(tree *resumeClosureTree, parent, _ *memoryDirectory) {
				tree.behavior(parent).openPinned = func(
					outputcap.CurrentEntryReference, bool,
				) (outputcap.Directory, error) {
					return nil, errResumeClosureOpen
				}
			},
			want: errResumeClosureOpen,
		},
		{
			name: "nil pinned directory",
			configure: func(tree *resumeClosureTree, parent, _ *memoryDirectory) {
				tree.behavior(parent).openPinned = func(
					outputcap.CurrentEntryReference, bool,
				) (outputcap.Directory, error) {
					return nil, nil
				}
			},
			want: outputcap.ErrUnsafeNamespace,
		},
		{
			name: "replaced after open",
			configure: func(tree *resumeClosureTree, parent, _ *memoryDirectory) {
				checks := 0
				tree.behavior(parent).entryMatch = func(
					name string, expected outputcap.CurrentEntryReference,
				) (bool, error) {
					checks++
					if checks == 2 {
						return false, nil
					}
					return parent.EntryMatches(name, expected)
				}
			},
			want: errResumePinReplaced,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := newMemoryDirectory()
			childPort, err := parent.CreateDirectory("child", true)
			if err != nil {
				t.Fatal(err)
			}
			child := childPort.(*memoryDirectory)
			tree := newResumeClosureTree()
			test.configure(tree, parent, child)
			pin, directory, err := pinExistingDirectory(tree.wrap(parent), "child")
			if !errors.Is(err, test.want) {
				t.Fatalf("pin error = %v, want %v", err, test.want)
			}
			_ = errors.Join(closeDirectory(directory), closeEntryReference(pin))
		})
	}

	t.Run("exact file pin rejects a read failure", func(t *testing.T) {
		parent := newMemoryDirectory()
		writeMemoryFile(t, parent, "marker", []byte("expected"))
		tree := newResumeClosureTree()
		tree.behavior(parent).openFile = func(string, bool, bool) (outputcap.File, error) {
			return nil, errResumeClosureOpen
		}
		pin, err := pinExactFile(tree.wrap(parent), "marker", []byte("expected"))
		if !errors.Is(err, errResumeClosureOpen) || pin != nil {
			t.Fatalf("exact file pin failure = %T, %v", pin, err)
		}
	})
}

func TestResumeClosureShardScanStopsAtEveryBoundedFailure(t *testing.T) {
	t.Run("owner listing failure", func(t *testing.T) {
		owner, _, tree := resumeClosureScanOwner(t)
		tree.behavior(owner.directory.(*resumeClosureTreeDirectory).base).names =
			func(int) ([]string, error) { return nil, errResumeClosureNames }
		if err := scanResumeShards(
			context.Background(), owner, new(resumeScanBudget), func(AttentionReason, []byte) {},
			func(*resumeShardPins, string) error { return nil },
		); !errors.Is(err, errResumeClosureNames) {
			t.Fatalf("owner listing error = %v", err)
		}
	})

	t.Run("cancellation before shard access", func(t *testing.T) {
		owner, _, _ := resumeClosureScanOwner(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := scanResumeShards(
			ctx, owner, new(resumeScanBudget), func(AttentionReason, []byte) {},
			func(*resumeShardPins, string) error { return nil },
		); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled shard scan = %v", err)
		}
	})

	t.Run("shard pin failure", func(t *testing.T) {
		owner, _, tree := resumeClosureScanOwner(t)
		base := owner.directory.(*resumeClosureTreeDirectory).base
		tree.behavior(base).openEntry = func(string) (outputcap.CurrentEntryReference, error) {
			return nil, errResumeClosureOpen
		}
		if err := scanResumeShards(
			context.Background(), owner, new(resumeScanBudget), func(AttentionReason, []byte) {},
			func(*resumeShardPins, string) error { return nil },
		); !errors.Is(err, errResumeClosureOpen) {
			t.Fatalf("shard pin error = %v", err)
		}
	})

	t.Run("entry listing overflows a small remaining budget", func(t *testing.T) {
		owner, shard, tree := resumeClosureScanOwner(t)
		tree.behavior(shard).names = func(int) ([]string, error) {
			return []string{"one", "two"}, nil
		}
		budget := resumeScanBudget{
			canonical: checkpointmodel.MaxCheckpointRecordsPerIntent - 1,
			auxiliary: checkpointmodel.MaxCheckpointAuxiliaryEntriesPerIntent,
		}
		if err := scanResumeShards(
			context.Background(), owner, &budget, func(AttentionReason, []byte) {},
			func(*resumeShardPins, string) error { return nil },
		); !errors.Is(err, errResumeUnknownChildren) {
			t.Fatalf("bounded entry overflow = %v", err)
		}
	})

	t.Run("visitor failure", func(t *testing.T) {
		owner, shard, _ := resumeClosureScanOwner(t)
		shard.mu.Lock()
		shard.files["opaque"] = &memoryFileData{bytes: []byte("opaque")}
		shard.mu.Unlock()
		if err := scanResumeShards(
			context.Background(), owner, new(resumeScanBudget), func(AttentionReason, []byte) {},
			func(*resumeShardPins, string) error { return errResumeClosureVisit },
		); !errors.Is(err, errResumeClosureVisit) {
			t.Fatalf("visitor error = %v", err)
		}
	})
}

func TestResumeClosureLeasedLifecycleRejectsCanceledClosedAndLostAuthority(t *testing.T) {
	fixture := newResumeAdapterFixture(t, 0xa1)
	inventory, err := mustResumeRepository(t, fixture.config).ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	leasedPort, err := inventory.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	repository := leasedPort.(*resumeLeasedRepository)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repository.Observe(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled observation = %v", err)
	}
	snapshot, err := repository.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cached, err := repository.Observe(context.Background()); err != nil ||
		len(cached.Checkpoints()) != len(snapshot.Checkpoints()) {
		t.Fatalf("cached observation = %+v, %v", cached, err)
	}
	actions := discardPlanForSnapshot(t, snapshot).Actions()
	if _, err := repository.Apply(canceled, actions[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled apply = %v", err)
	}
	var nilRepository *resumeLeasedRepository
	if _, err := nilRepository.Observe(context.Background()); err == nil {
		t.Fatal("nil leased repository was observable")
	}
	if _, err := nilRepository.Apply(context.Background(), actions[0]); err == nil {
		t.Fatal("nil leased repository accepted an action")
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Observe(context.Background()); err == nil {
		t.Fatal("closed leased repository was observable")
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}

	var nilInventory *resumeInventory
	if nilInventory.Entries() != nil || nilInventory.Close() != nil {
		t.Fatal("nil inventory did not settle as empty")
	}
	if _, err := nilInventory.Acquire(context.Background(), 0); err == nil {
		t.Fatal("nil inventory granted an acquisition")
	}
	if _, err := resumeIntentLayoutExact(nil); err == nil {
		t.Fatal("nil intent layout was treated as exact")
	}
}

func TestResumeClosureDiscardRejectsLostInternalAuthority(t *testing.T) {
	t.Run("snapshot attention", func(t *testing.T) {
		target := newResumeAdapterFixture(t, 0xa2)
		malformedID := mustRecordID(t, 0xd2)
		shardName, name := checkpointstore.RecordLocation(malformedID)
		_, _, records, _, _ := resumeClosureIntentLayout(t, target)
		malformedShard := mustMemoryShard(t, records, shardName)
		writeMemoryFile(t, malformedShard, name, []byte("malformed"))
		targetInventory, targetRepositoryPort, targetSnapshot := observeResumeFixture(t, target)
		if targetSnapshot.NamespaceEvidence() != EvidenceExact ||
			len(targetSnapshot.Attention()) == 0 {
			t.Fatalf("target attention snapshot = %+v", targetSnapshot)
		}

		_, sourceInventory, sourceRepository, sourceActions := resumeClosureObserved(t, 0xa2)
		result, err := targetRepositoryPort.Apply(context.Background(), sourceActions[0])
		if err != nil || result.Status() != ApplyNeedsAttention {
			t.Fatalf("attention-gated action = %v, %v", result.Status(), err)
		}
		assertResumeArtifactsPresent(t, target)
		resumeClosureClose(t, targetInventory, targetRepositoryPort)
		resumeClosureClose(t, sourceInventory, sourceRepository)
	})

	t.Run("checkpoint pin map entry", func(t *testing.T) {
		fixture, inventory, repository, actions := resumeClosureObserved(t, 0xa3)
		delete(repository.checkpointPins, fixture.record.RecordID())
		if _, err := repository.Apply(context.Background(), actions[0]); err == nil {
			t.Fatal("action survived loss of its retained checkpoint pin")
		}
		assertResumeArtifactsPresent(t, fixture)
		resumeClosureClose(t, inventory, repository)
	})

	t.Run("missing artifact and owner authority", func(t *testing.T) {
		fixture, inventory, repository, _ := resumeClosureObserved(t, 0xa4)
		checkpoint := repository.checkpointPins[fixture.record.RecordID()]
		result, err := repository.removeArtifact(checkpoint, nil)
		if err != nil || result.Status() != ApplyAlreadySatisfied {
			t.Fatalf("nil artifact settlement = %v, %v", result.Status(), err)
		}
		result, err = repository.syncOwnedShard(checkpoint, nil, checkpointstore.RecoveryStage, nil)
		if err != nil || result.Status() != ApplyNeedsAttention {
			t.Fatalf("nil owner settlement = %v, %v", result.Status(), err)
		}
		checkpoint.entry.shard = nil
		result, err = repository.syncEntryShard(checkpoint)
		if err != nil || result.Status() != ApplyNeedsAttention {
			t.Fatalf("nil record shard settlement = %v, %v", result.Status(), err)
		}
		if _, err := repository.applyExpected(
			context.Background(), checkpoint, ActionKind(255),
		); err == nil {
			t.Fatal("unknown reducer action reached mutation code")
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := repository.applyExpected(canceled, checkpoint, ActionRemoveStage); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled internal action = %v", err)
		}
		resumeClosureClose(t, inventory, repository)
	})
}

func TestResumeClosureSyncClassifiesAbsentCreatedAndUncertainShards(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutate     func(*testing.T, resumeAdapterFixture, *resumeLeasedRepository)
		wantStatus ApplyStatus
		wantErr    error
	}{
		{
			name:       "absent shard",
			wantStatus: ApplyAlreadySatisfied,
		},
		{
			name: "created after observation",
			mutate: func(t *testing.T, fixture resumeAdapterFixture, _ *resumeLeasedRepository) {
				_, _, _, stages, _ := resumeClosureIntentLayout(t, fixture)
				shardName, _ := recoveryArtifactLocation(t, fixture.record.OwnedOutputObject(), checkpointstore.RecoveryStage)
				stages.mu.Lock()
				stages.dirs[shardName] = newMemoryDirectory()
				stages.mu.Unlock()
			},
			wantStatus: ApplyNeedsAttention,
		},
		{
			name: "classification failure",
			mutate: func(t *testing.T, fixture resumeAdapterFixture, repository *resumeLeasedRepository) {
				_, _, _, stages, _ := resumeClosureIntentLayout(t, fixture)
				tree := newResumeClosureTree()
				tree.behavior(stages).classify = func(string) (outputcap.EntryKind, bool, error) {
					return outputcap.EntryAbsent, false, errResumeClosureClassify
				}
				repository.stages.directory = tree.wrap(stages)
			},
			wantErr: errResumeClosureClassify,
		},
		{
			name: "non-exact shard",
			mutate: func(t *testing.T, fixture resumeAdapterFixture, repository *resumeLeasedRepository) {
				_, _, _, stages, _ := resumeClosureIntentLayout(t, fixture)
				tree := newResumeClosureTree()
				tree.behavior(stages).classify = func(string) (outputcap.EntryKind, bool, error) {
					return outputcap.EntryDirectory, false, nil
				}
				repository.stages.directory = tree.wrap(stages)
			},
			wantStatus: ApplyNeedsAttention,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newResumeAdapterFixture(t, 0xa5)
			_, _, _, stages, _ := resumeClosureIntentLayout(t, fixture)
			shardName, _ := recoveryArtifactLocation(t, fixture.record.OwnedOutputObject(), checkpointstore.RecoveryStage)
			fixture.stageShard.mu.Lock()
			delete(fixture.stageShard.files, fixture.stageName)
			fixture.stageShard.mu.Unlock()
			stages.mu.Lock()
			delete(stages.dirs, shardName)
			stages.mu.Unlock()

			inventory, leasedPort, snapshot := observeResumeFixture(t, fixture)
			repository := leasedPort.(*resumeLeasedRepository)
			actions := discardPlanForSnapshot(t, snapshot).Actions()
			if len(actions) == 0 || actions[0].Kind() != ActionSyncStages {
				t.Fatalf("absent-stage actions = %v", actionKinds(actions))
			}
			if test.mutate != nil {
				test.mutate(t, fixture, repository)
			}
			result, err := repository.Apply(context.Background(), actions[0])
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("sync error = %v, want %v", err, test.wantErr)
				}
			} else if err != nil || result.Status() != test.wantStatus {
				t.Fatalf("sync settlement = %v, %v", result.Status(), err)
			}
			resumeClosureClose(t, inventory, repository)
		})
	}
}

func TestResumeClosureNativeRepositoryRejectsInvalidAndForeignOwnership(t *testing.T) {
	root := newMemoryDirectory()
	config, _ := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x91)
	valid := NativeResumeConfig{
		Root: root, BackendID: config.Ownership.Backend(),
		Certification: config.Ownership.Certification(),
		RootIdentity:  config.Ownership.RootIdentity().Bytes(),
	}
	for _, test := range []struct {
		name   string
		mutate func(*NativeResumeConfig)
	}{
		{name: "nil root", mutate: func(config *NativeResumeConfig) { config.Root = nil }},
		{name: "invalid backend", mutate: func(config *NativeResumeConfig) { config.BackendID = "" }},
		{name: "invalid certification", mutate: func(config *NativeResumeConfig) { config.Certification = "" }},
		{name: "short root identity", mutate: func(config *NativeResumeConfig) { config.RootIdentity = []byte{1} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if _, err := NewNativeResumeRepository(candidate); err == nil {
				t.Fatal("invalid native resume configuration was accepted")
			}
		})
	}

	fixture := newResumeAdapterFixture(t, 0x92)
	foreignBackend, err := transfer.NewOutputBackendID("foreign-native-backend")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewNativeResumeRepository(NativeResumeConfig{
		Root: fixture.root, BackendID: foreignBackend,
		Certification: fixture.config.Ownership.Certification(),
		RootIdentity:  fixture.config.Ownership.RootIdentity().Bytes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	inventoryPort, err := repository.ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	publicInventory, err := NewInventory(inventoryPort)
	if err != nil {
		t.Fatal(err)
	}
	summaries := publicInventory.Summaries()
	if len(summaries) != 1 || !summaries[0].NeedsAttention() ||
		summaries[0].Attention()[0].Reason() != AttentionCorruptBinding {
		t.Fatalf("foreign native ownership summaries = %+v", summaries)
	}
	if err := publicInventory.Close(); err != nil {
		t.Fatal(err)
	}

	duplicateErr := errors.New("native root duplication failed")
	tree := newResumeClosureTree()
	tree.behavior(fixture.root).duplicate = func() (outputcap.Directory, error) {
		return nil, duplicateErr
	}
	valid = NativeResumeConfig{
		Root: tree.wrap(fixture.root), BackendID: fixture.config.Ownership.Backend(),
		Certification: fixture.config.Ownership.Certification(),
		RootIdentity:  fixture.config.Ownership.RootIdentity().Bytes(),
	}
	repository, err = NewNativeResumeRepository(valid)
	if err != nil {
		t.Fatal(err)
	}
	if inventory, err := repository.ListResumeState(context.Background()); inventory != nil || !errors.Is(err, duplicateErr) {
		t.Fatalf("native duplicate failure = %T, %v", inventory, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	valid.Root = fixture.root
	repository, err = NewNativeResumeRepository(valid)
	if err != nil {
		t.Fatal(err)
	}
	if inventory, err := repository.ListResumeState(ctx); inventory != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled native list = %T, %v", inventory, err)
	}
}

func TestResumeClosureNativeOwnershipInspectionDistinguishesStructureAndFaults(t *testing.T) {
	assertInspection := func(
		t *testing.T,
		repository NativeResumeRepository,
		wantState nativeResumeOwnershipState,
		wantErr error,
	) {
		t.Helper()
		_, state, err := repository.inspectOwnership()
		if state != wantState || !errors.Is(err, wantErr) {
			t.Fatalf("ownership inspection = %v, %v; want %v, %v", state, err, wantState, wantErr)
		}
	}

	t.Run("checkpoint namespace absent", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xa1)
		control := fixture.root.dirsForTest(t, checkpointstore.ControlDirectory)
		control.mu.Lock()
		delete(control.dirs, checkpointstore.CheckpointDirectory)
		control.mu.Unlock()
		assertInspection(t, resumeClosureNativeRepository(t, fixture, fixture.root),
			nativeResumeNamespaceAbsent, nil)
	})

	t.Run("control wrong kind", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xa2)
		fixture.root.mu.Lock()
		delete(fixture.root.dirs, checkpointstore.ControlDirectory)
		fixture.root.files[checkpointstore.ControlDirectory] = &memoryFileData{bytes: []byte("opaque")}
		fixture.root.mu.Unlock()
		assertInspection(t, resumeClosureNativeRepository(t, fixture, fixture.root),
			nativeResumeOwnershipUnsafe, nil)
	})

	t.Run("checkpoint wrong kind", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xa3)
		control := fixture.root.dirsForTest(t, checkpointstore.ControlDirectory)
		control.mu.Lock()
		delete(control.dirs, checkpointstore.CheckpointDirectory)
		control.files[checkpointstore.CheckpointDirectory] = &memoryFileData{bytes: []byte("opaque")}
		control.mu.Unlock()
		assertInspection(t, resumeClosureNativeRepository(t, fixture, fixture.root),
			nativeResumeOwnershipUnsafe, nil)
	})

	t.Run("ownership wrong kind", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xa4)
		checkpointRoot := resumeClosureCheckpointRoot(t, fixture.root)
		checkpointRoot.mu.Lock()
		delete(checkpointRoot.files, checkpointstore.OwnershipFile)
		checkpointRoot.dirs[checkpointstore.OwnershipFile] = newMemoryDirectory()
		checkpointRoot.mu.Unlock()
		assertInspection(t, resumeClosureNativeRepository(t, fixture, fixture.root),
			nativeResumeOwnershipUnsafe, nil)
	})

	t.Run("ownership classification failure", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xa5)
		checkpointRoot := resumeClosureCheckpointRoot(t, fixture.root)
		tree := newResumeClosureTree()
		tree.behavior(checkpointRoot).classify = func(name string) (outputcap.EntryKind, bool, error) {
			if name == checkpointstore.OwnershipFile {
				return outputcap.EntryAbsent, false, errResumeClosureClassify
			}
			return checkpointRoot.ClassifyExactEntry(name)
		}
		assertInspection(t, resumeClosureNativeRepository(t, fixture, tree.wrap(fixture.root)),
			0, errResumeClosureClassify)
	})

	t.Run("ownership pin disappears", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xa6)
		checkpointRoot := resumeClosureCheckpointRoot(t, fixture.root)
		tree := newResumeClosureTree()
		tree.behavior(checkpointRoot).openEntry = func(name string) (outputcap.CurrentEntryReference, error) {
			if name == checkpointstore.OwnershipFile {
				return nil, nil
			}
			return checkpointRoot.OpenEntry(name)
		}
		assertInspection(t, resumeClosureNativeRepository(t, fixture, tree.wrap(fixture.root)),
			nativeResumeOwnershipUnsafe, nil)
	})

	t.Run("ownership pin comparison failure", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xa7)
		checkpointRoot := resumeClosureCheckpointRoot(t, fixture.root)
		tree := newResumeClosureTree()
		tree.behavior(checkpointRoot).entryMatch = func(
			name string, expected outputcap.CurrentEntryReference,
		) (bool, error) {
			if name == checkpointstore.OwnershipFile {
				return false, errResumeClosureMatch
			}
			return checkpointRoot.EntryMatches(name, expected)
		}
		assertInspection(t, resumeClosureNativeRepository(t, fixture, tree.wrap(fixture.root)),
			0, errResumeClosureMatch)
	})

	t.Run("ownership open failure", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xa8)
		checkpointRoot := resumeClosureCheckpointRoot(t, fixture.root)
		tree := newResumeClosureTree()
		tree.behavior(checkpointRoot).openFile = func(
			name string, private bool, writable bool,
		) (outputcap.File, error) {
			if name == checkpointstore.OwnershipFile {
				return nil, errResumeClosureOpen
			}
			return checkpointRoot.OpenFile(name, private, writable)
		}
		assertInspection(t, resumeClosureNativeRepository(t, fixture, tree.wrap(fixture.root)),
			0, errResumeClosureOpen)
	})

	t.Run("ownership close failure", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xa9)
		checkpointRoot := resumeClosureCheckpointRoot(t, fixture.root)
		tree := newResumeClosureTree()
		tree.behavior(checkpointRoot).openFile = func(
			name string, private bool, writable bool,
		) (outputcap.File, error) {
			file, err := checkpointRoot.OpenFile(name, private, writable)
			if err != nil {
				return nil, err
			}
			return &resumeClosureFile{File: file, closeErr: errResumeClosureOpen}, nil
		}
		assertInspection(t, resumeClosureNativeRepository(t, fixture, tree.wrap(fixture.root)),
			0, errResumeClosureOpen)
	})

	t.Run("ownership replaced after read", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xaa)
		checkpointRoot := resumeClosureCheckpointRoot(t, fixture.root)
		tree := newResumeClosureTree()
		tree.behavior(checkpointRoot).openFile = func(
			name string, private bool, writable bool,
		) (outputcap.File, error) {
			file, err := checkpointRoot.OpenFile(name, private, writable)
			if err != nil {
				return nil, err
			}
			checkpointRoot.mu.Lock()
			original := checkpointRoot.files[name]
			original.mu.Lock()
			replacement := &memoryFileData{bytes: bytes.Clone(original.bytes)}
			original.mu.Unlock()
			checkpointRoot.files[name] = replacement
			checkpointRoot.mu.Unlock()
			return file, nil
		}
		assertInspection(t, resumeClosureNativeRepository(t, fixture, tree.wrap(fixture.root)),
			nativeResumeOwnershipUnsafe, nil)
	})

	t.Run("context canceled by inspection", func(t *testing.T) {
		fixture := newResumeAdapterFixture(t, 0xab)
		ctx, cancel := context.WithCancel(context.Background())
		tree := newResumeClosureTree()
		tree.behavior(fixture.root).duplicate = func() (outputcap.Directory, error) {
			cancel()
			return fixture.root, nil
		}
		repository := resumeClosureNativeRepository(t, fixture, tree.wrap(fixture.root))
		if inventory, err := repository.ListResumeState(ctx); inventory != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("post-inspection cancellation = %T, %v", inventory, err)
		}
	})
}

func resumeClosureNativeRepository(
	t *testing.T,
	fixture resumeAdapterFixture,
	root outputcap.Directory,
) NativeResumeRepository {
	t.Helper()
	repository, err := NewNativeResumeRepository(NativeResumeConfig{
		Root: root, BackendID: fixture.config.Ownership.Backend(),
		Certification: fixture.config.Ownership.Certification(),
		RootIdentity:  fixture.config.Ownership.RootIdentity().Bytes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func resumeClosureAcquire(
	t *testing.T,
	config checkpointstore.CertifiedConfig,
) (PinnedInventory, *resumeLeasedRepository) {
	t.Helper()
	inventory, err := mustResumeRepository(t, config).ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	leased, err := inventory.Acquire(context.Background(), 0)
	if err != nil {
		_ = inventory.Close()
		t.Fatal(err)
	}
	repository, ok := leased.(*resumeLeasedRepository)
	if !ok {
		_ = errors.Join(leased.Close(), inventory.Close())
		t.Fatalf("leased repository type = %T", leased)
	}
	return inventory, repository
}

func resumeClosureObservationError(t *testing.T, config checkpointstore.CertifiedConfig) error {
	t.Helper()
	inventory, repository := resumeClosureAcquire(t, config)
	_, observeErr := repository.Observe(context.Background())
	if closeErr := errors.Join(repository.Close(), inventory.Close()); closeErr != nil {
		t.Fatal(closeErr)
	}
	return observeErr
}

func resumeClosureObserved(
	t *testing.T,
	fill byte,
) (resumeAdapterFixture, PinnedInventory, *resumeLeasedRepository, []Action) {
	t.Helper()
	fixture := newResumeAdapterFixture(t, fill)
	inventory, leased, snapshot := observeResumeFixture(t, fixture)
	repository := leased.(*resumeLeasedRepository)
	return fixture, inventory, repository, discardPlanForSnapshot(t, snapshot).Actions()
}

func resumeClosureObserveWithConfig(
	t *testing.T,
	config checkpointstore.CertifiedConfig,
) (PinnedInventory, LeasedRepository, RepositorySnapshot) {
	t.Helper()
	inventory, err := mustResumeRepository(t, config).ListResumeState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	leased, err := inventory.Acquire(context.Background(), 0)
	if err != nil {
		_ = inventory.Close()
		t.Fatal(err)
	}
	snapshot, err := leased.Observe(context.Background())
	if err != nil {
		_ = errors.Join(leased.Close(), inventory.Close())
		t.Fatal(err)
	}
	return inventory, leased, snapshot
}

func resumeClosureClose(
	t *testing.T,
	inventory PinnedInventory,
	leased LeasedRepository,
) {
	t.Helper()
	if err := errors.Join(leased.Close(), inventory.Close()); err != nil {
		t.Fatal(err)
	}
}

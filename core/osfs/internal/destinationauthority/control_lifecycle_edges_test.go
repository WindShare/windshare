package destinationauthority

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestControlLifecycleAbortsFailedBindingParticipant(t *testing.T) {
	platform := newDestinationPlatform()
	_, err := BindDestination(BindConfig{
		Platform:    platform,
		DisplayPath: filepath.Clean(t.TempDir()),
		OpenLiveCleanupJournal: func(outputcap.Directory) (LiveCleanupJournalHandle, error) {
			return LiveCleanupJournalHandle{}, errDestinationFake
		},
		RecyclePrivateState:   recycleFakePrivateState,
		ControlUseNonceSource: bytes.NewReader(bytes.Repeat([]byte{0x71}, controlParticipantNonceBytes)),
	})
	if !errors.Is(err, errDestinationFake) {
		t.Fatalf("failed bind error = %v", err)
	}
	participants := controlParticipantsNode(t, platform)
	if len(participants.entries) != 0 {
		t.Fatalf("failed bind retained participant markers = %v", participants.entries)
	}

	authority := bindRecyclingDestination(t, platform, 0x72, recycleFakePrivateState)
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if platform.root.entries[controlDirectoryName] != nil {
		t.Fatal("subsequent successful binding did not recycle failed-bind infrastructure")
	}
}

func TestControlLifecycleRetriesAnUnlockedNonceCollision(t *testing.T) {
	platform := newDestinationPlatform()
	crashed := bindRecyclingDestination(t, platform, 0x81, recycleFakePrivateState)
	if err := crashed.controlUse.participant.Close(); err != nil {
		t.Fatal(err)
	}
	crashed.controlUse = nil

	nonces := append(
		bytes.Repeat([]byte{0x81}, controlParticipantNonceBytes),
		bytes.Repeat([]byte{0x82}, controlParticipantNonceBytes)...,
	)
	journal := &destinationJournal{snapshot: LiveCleanupSnapshot{State: LiveCleanupScanComplete}}
	restarted, err := BindDestination(BindConfig{
		Platform:               platform,
		DisplayPath:            filepath.Clean(t.TempDir()),
		OpenLiveCleanupJournal: fakeJournalOpener(journal),
		RecyclePrivateState:    recycleFakePrivateState,
		ControlUseNonceSource:  bytes.NewReader(nonces),
	})
	if err != nil {
		t.Fatal(err)
	}
	if restarted.controlUse.participantName != strings.Repeat("82", controlParticipantNonceBytes)+controlParticipantLockSuffix {
		t.Fatalf("participant after collision = %q", restarted.controlUse.participantName)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	if err := crashed.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestControlLifecycleRejectsMalformedParticipantBeforeJoining(t *testing.T) {
	platform := newDestinationPlatform()
	owner := bindRecyclingDestination(t, platform, 0x91, recycleFakePrivateState)
	participants := controlParticipantsDirectoryForTest(t, owner.control)
	foreign, err := participants.CreateFile("foreign.lock", true, 0)
	if err != nil {
		t.Fatal(err)
	}

	journal := &destinationJournal{snapshot: LiveCleanupSnapshot{State: LiveCleanupScanComplete}}
	_, err = BindDestination(BindConfig{
		Platform:               platform,
		DisplayPath:            filepath.Clean(t.TempDir()),
		OpenLiveCleanupJournal: fakeJournalOpener(journal),
		RecyclePrivateState:    recycleFakePrivateState,
		ControlUseNonceSource:  bytes.NewReader(bytes.Repeat([]byte{0x92}, controlParticipantNonceBytes)),
	})
	if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("malformed participant bind error = %v", err)
	}
	if err := participants.RemoveFile("foreign.lock", foreign); err != nil {
		t.Fatal(err)
	}
	if err := participants.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestControlLifecycleDoesNotMutateAReplacementNamespace(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindRecyclingDestination(t, platform, 0xa1, recycleFakePrivateState)
	replacement := &destinationNode{
		id:       platform.nextID,
		private:  true,
		identity: []byte("replacement-control"),
		entries:  map[string]*destinationNode{},
	}
	platform.nextID++
	platform.root.entries[controlDirectoryName] = replacement

	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if platform.root.entries[controlDirectoryName] != replacement {
		t.Fatal("close mutated a replacement control namespace")
	}
}

func TestControlLifecycleSurfacesRecyclerFailureAfterDroppingLiveness(t *testing.T) {
	platform := newDestinationPlatform()
	recycleErr := errors.New("recycler unavailable")
	authority := bindRecyclingDestination(t, platform, 0xb1, func(outputcap.Directory) (bool, error) {
		return false, recycleErr
	})
	if err := authority.Close(); !errors.Is(err, recycleErr) {
		t.Fatalf("close error = %v", err)
	}
	if participants := controlParticipantsNode(t, platform); len(participants.entries) != 0 {
		t.Fatalf("failed recycler retained live participant = %v", participants.entries)
	}
}

func TestControlCoordinatorReportsBoundedContention(t *testing.T) {
	platform := newDestinationPlatform()
	lifecycle, err := platform.Root().CreateDirectory(controlLifecycleNamespace, true)
	if err != nil {
		t.Fatal(err)
	}
	held, _, err := lifecycle.AcquireLock(controlCoordinatorLock, false)
	if err != nil {
		t.Fatal(err)
	}
	if acquired, created, err := acquireControlCoordinator(lifecycle); acquired != nil || created ||
		!errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		t.Fatalf("contended coordinator = (%v, %t, %v)", acquired, created, err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestControlLifecycleAuthenticatesExactShapes(t *testing.T) {
	t.Run("unknown lifecycle entry", func(t *testing.T) {
		platform := newDestinationPlatform()
		lifecycle := newDestinationDirectoryNode(platform)
		participants := newDestinationDirectoryNode(platform)
		if _, err := lifecycle.CreateDirectory("foreign", true); err != nil {
			t.Fatal(err)
		}
		if err := authenticateControlLifecycle(lifecycle, participants); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("unknown lifecycle error = %v", err)
		}
	})

	t.Run("coordinator has wrong kind", func(t *testing.T) {
		platform := newDestinationPlatform()
		lifecycle := newDestinationDirectoryNode(platform)
		participants, err := lifecycle.CreateDirectory(controlParticipantsDirectory, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lifecycle.CreateDirectory(controlCoordinatorLock, true); err != nil {
			t.Fatal(err)
		}
		if err := authenticateControlLifecycle(lifecycle, participants); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("wrong coordinator kind error = %v", err)
		}
	})

	t.Run("participant has wrong kind", func(t *testing.T) {
		platform := newDestinationPlatform()
		lifecycle := newDestinationDirectoryNode(platform)
		participants, err := lifecycle.CreateDirectory(controlParticipantsDirectory, true)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.Repeat("c", controlParticipantNonceBytes*2) + controlParticipantLockSuffix
		if _, err := participants.CreateDirectory(name, true); err != nil {
			t.Fatal(err)
		}
		if err := authenticateControlLifecycle(lifecycle, participants); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("wrong participant kind error = %v", err)
		}
	})
}

func TestControlParticipantNameGrammar(t *testing.T) {
	valid := strings.Repeat("a", controlParticipantNonceBytes*2) + controlParticipantLockSuffix
	for name, want := range map[string]bool{
		valid:                  true,
		strings.ToUpper(valid): false,
		strings.Repeat("g", controlParticipantNonceBytes*2) + controlParticipantLockSuffix: false,
		strings.Repeat("a", controlParticipantNonceBytes*2):                                false,
	} {
		if got := validControlParticipantName(name); got != want {
			t.Errorf("validControlParticipantName(%q) = %t, want %t", name, got, want)
		}
	}
}

func TestControlLifecycleFailureBoundaries(t *testing.T) {
	t.Run("coordinator requires a lock capability", func(t *testing.T) {
		directory := &lifecycleFaultDirectory{
			Directory: newDestinationDirectoryNode(newDestinationPlatform()),
			acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
				return nil, false, nil
			},
		}
		if _, _, err := acquireControlCoordinator(directory); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("nil coordinator error = %v", err)
		}
	})

	t.Run("coordinator closes a lock returned with failure", func(t *testing.T) {
		lock := &lifecycleFaultLock{}
		directory := &lifecycleFaultDirectory{
			Directory: newDestinationDirectoryNode(newDestinationPlatform()),
			acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
				return lock, false, errDestinationFake
			},
		}
		if _, _, err := acquireControlCoordinator(directory); !errors.Is(err, errDestinationFake) {
			t.Fatalf("coordinator failure = %v", err)
		}
		if lock.closes != 1 {
			t.Fatalf("coordinator lock closes = %d", lock.closes)
		}
	})

	t.Run("participant entropy failure", func(t *testing.T) {
		participants := newDestinationDirectoryNode(newDestinationPlatform())
		if lease, err := createControlParticipant(participants, bytes.NewReader(nil)); lease != nil ||
			!errors.Is(err, io.EOF) {
			t.Fatalf("entropy failure = (%v, %v)", lease, err)
		}
	})

	t.Run("participant lock failure", func(t *testing.T) {
		participants := &lifecycleFaultDirectory{
			Directory: newDestinationDirectoryNode(newDestinationPlatform()),
			acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
				return nil, false, errDestinationFake
			},
		}
		entropy := bytes.NewReader(bytes.Repeat([]byte{0x11}, controlParticipantNonceBytes))
		if lease, err := createControlParticipant(participants, entropy); lease != nil ||
			!errors.Is(err, errDestinationFake) {
			t.Fatalf("lock failure = (%v, %v)", lease, err)
		}
	})

	t.Run("participant durability failure", func(t *testing.T) {
		participants := &lifecycleFaultDirectory{
			Directory: newDestinationDirectoryNode(newDestinationPlatform()),
			sync:      func() error { return errDestinationFake },
		}
		entropy := bytes.NewReader(bytes.Repeat([]byte{0x12}, controlParticipantNonceBytes))
		if lease, err := createControlParticipant(participants, entropy); lease != nil ||
			!errors.Is(err, errDestinationFake) {
			t.Fatalf("durability failure = (%v, %v)", lease, err)
		}
	})

	t.Run("participant nonce space is bounded", func(t *testing.T) {
		participants := newDestinationDirectoryNode(newDestinationPlatform())
		entropy := make([]byte, 0, maximumControlBindAttempts*controlParticipantNonceBytes)
		for nonce := range maximumControlBindAttempts {
			value := byte(nonce + 1)
			chunk := bytes.Repeat([]byte{value}, controlParticipantNonceBytes)
			entropy = append(entropy, chunk...)
			name := strings.Repeat(hexByte(value), controlParticipantNonceBytes) + controlParticipantLockSuffix
			lock, _, err := participants.AcquireLock(name, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if lease, err := createControlParticipant(participants, bytes.NewReader(entropy)); lease != nil ||
			!errors.Is(err, outputcap.ErrNamespaceCollision) {
			t.Fatalf("bounded collision = (%v, %v)", lease, err)
		}
	})
}

func TestControlLifecycleReleaseFailureBoundaries(t *testing.T) {
	t.Run("root entry is absent", func(t *testing.T) {
		root := newDestinationDirectoryNode(newDestinationPlatform())
		if current, err := controlMatchesRoot(root, root); err != nil || current {
			t.Fatalf("absent control match = (%t, %v)", current, err)
		}
	})

	t.Run("root entry is not a directory", func(t *testing.T) {
		root := newDestinationDirectoryNode(newDestinationPlatform())
		if _, err := root.CreateFile(controlDirectoryName, true, 0); err != nil {
			t.Fatal(err)
		}
		if current, err := controlMatchesRoot(root, root); err != nil || current {
			t.Fatalf("file control match = (%t, %v)", current, err)
		}
	})

	t.Run("root entry cannot be reopened", func(t *testing.T) {
		platform := newDestinationPlatform()
		root := newDestinationDirectoryNode(platform)
		control, err := root.CreateDirectory(controlDirectoryName, true)
		if err != nil {
			t.Fatal(err)
		}
		fault := &lifecycleFaultDirectory{
			Directory: root,
			openEntry: func(string) (outputcap.CurrentEntryReference, error) {
				return nil, errDestinationFake
			},
		}
		if current, err := controlMatchesRoot(fault, control); current || !errors.Is(err, ErrControlNamespaceChanged) {
			t.Fatalf("unopenable control match = (%t, %v)", current, err)
		}
	})

	t.Run("failed abort cannot reopen lifecycle", func(t *testing.T) {
		control := newDestinationDirectoryNode(newDestinationPlatform())
		lease := &controlUseLease{participantName: "unused", participant: &lifecycleFaultLock{}}
		if err := abortControlUse(control, lease); !errors.Is(err, ErrControlNamespaceChanged) {
			t.Fatalf("abort without lifecycle = %v", err)
		}
	})

	t.Run("failed abort cannot reopen participants", func(t *testing.T) {
		control := newDestinationDirectoryNode(newDestinationPlatform())
		if _, err := control.CreateDirectory(controlLifecycleNamespace, true); err != nil {
			t.Fatal(err)
		}
		lease := &controlUseLease{participantName: "unused", participant: &lifecycleFaultLock{}}
		if err := abortControlUse(control, lease); !errors.Is(err, ErrControlNamespaceChanged) {
			t.Fatalf("abort without participants = %v", err)
		}
	})

	t.Run("release requires lifecycle", func(t *testing.T) {
		root := newDestinationDirectoryNode(newDestinationPlatform())
		control := newDestinationDirectoryNode(newDestinationPlatform())
		lease := &controlUseLease{participantName: "unused", participant: &lifecycleFaultLock{}}
		if err := lease.releaseAndRecycle(root, control, recycleFakePrivateState); !errors.Is(err, ErrControlNamespaceChanged) {
			t.Fatalf("release without lifecycle = %v", err)
		}
	})

	t.Run("release requires participants", func(t *testing.T) {
		platform := newDestinationPlatform()
		root := newDestinationDirectoryNode(platform)
		control := newDestinationDirectoryNode(platform)
		if _, err := control.CreateDirectory(controlLifecycleNamespace, true); err != nil {
			t.Fatal(err)
		}
		lease := &controlUseLease{participantName: "unused", participant: &lifecycleFaultLock{}}
		if err := lease.releaseAndRecycle(root, control, recycleFakePrivateState); !errors.Is(err, ErrControlNamespaceChanged) {
			t.Fatalf("release without participants = %v", err)
		}
	})

	t.Run("nil release is inert", func(t *testing.T) {
		var lease *controlUseLease
		if err := lease.releaseAndRecycle(nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("changed guarded root aborts recycling", func(t *testing.T) {
		platform := newDestinationPlatform()
		authority := bindRecyclingDestination(t, platform, 0xc1, recycleFakePrivateState)
		platform.guardRoot = newDestinationDirectoryNode(platform).node
		if err := authority.Close(); !errors.Is(err, ErrRetainedRootChanged) {
			t.Fatalf("changed root close = %v", err)
		}
	})

	t.Run("unsafe participants drop only the current marker", func(t *testing.T) {
		platform := newDestinationPlatform()
		authority := bindRecyclingDestination(t, platform, 0xd1, recycleFakePrivateState)
		participants := controlParticipantsDirectoryForTest(t, authority.control)
		if _, err := participants.CreateFile("foreign.lock", true, 0); err != nil {
			t.Fatal(err)
		}
		if err := participants.Close(); err != nil {
			t.Fatal(err)
		}
		if err := authority.Close(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("unsafe participant close = %v", err)
		}
	})
}

func TestControlLifecycleRecycleHelpersRetainUnprovenState(t *testing.T) {
	t.Run("missing own marker is already released", func(t *testing.T) {
		lease := &controlUseLease{participantName: "gone", participant: &lifecycleFaultLock{}}
		participants := &lifecycleFaultDirectory{
			Directory: newDestinationDirectoryNode(newDestinationPlatform()),
			removeFile: func(string, outputcap.File) error {
				return fs.ErrNotExist
			},
		}
		if err := lease.removeOwnParticipant(participants); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("invalid stale participant is retained", func(t *testing.T) {
		participants := newDestinationDirectoryNode(newDestinationPlatform())
		if _, err := participants.CreateFile("foreign.lock", true, 0); err != nil {
			t.Fatal(err)
		}
		if last, err := removeStaleControlParticipants(participants); err != nil || last {
			t.Fatalf("invalid stale participant = (%t, %v)", last, err)
		}
	})

	t.Run("participant enumeration failure", func(t *testing.T) {
		participants := &lifecycleFaultDirectory{
			Directory: newDestinationDirectoryNode(newDestinationPlatform()),
			names: func(int) ([]string, error) {
				return nil, errDestinationFake
			},
		}
		if last, err := removeStaleControlParticipants(participants); last || !errors.Is(err, errDestinationFake) {
			t.Fatalf("enumeration failure = (%t, %v)", last, err)
		}
	})

	t.Run("participant acquisition failure", func(t *testing.T) {
		name := strings.Repeat("d", controlParticipantNonceBytes*2) + controlParticipantLockSuffix
		base := newDestinationDirectoryNode(newDestinationPlatform())
		if _, err := base.CreateFile(name, true, 0); err != nil {
			t.Fatal(err)
		}
		participants := &lifecycleFaultDirectory{
			Directory: base,
			acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
				return nil, false, errDestinationFake
			},
		}
		if last, err := removeStaleControlParticipants(participants); last || !errors.Is(err, errDestinationFake) {
			t.Fatalf("acquisition failure = (%t, %v)", last, err)
		}
	})

	t.Run("participant removal failure", func(t *testing.T) {
		name := strings.Repeat("e", controlParticipantNonceBytes*2) + controlParticipantLockSuffix
		base := newDestinationDirectoryNode(newDestinationPlatform())
		if _, err := base.CreateFile(name, true, 0); err != nil {
			t.Fatal(err)
		}
		participants := &lifecycleFaultDirectory{
			Directory: base,
			removeFile: func(string, outputcap.File) error {
				return errDestinationFake
			},
		}
		if last, err := removeStaleControlParticipants(participants); last || !errors.Is(err, errDestinationFake) {
			t.Fatalf("removal failure = (%t, %v)", last, err)
		}
	})

	t.Run("participant removal must be durable", func(t *testing.T) {
		name := strings.Repeat("f", controlParticipantNonceBytes*2) + controlParticipantLockSuffix
		base := newDestinationDirectoryNode(newDestinationPlatform())
		if _, err := base.CreateFile(name, true, 0); err != nil {
			t.Fatal(err)
		}
		participants := &lifecycleFaultDirectory{
			Directory: base,
			sync:      func() error { return errDestinationFake },
		}
		if last, err := removeStaleControlParticipants(participants); last || !errors.Is(err, errDestinationFake) {
			t.Fatalf("durability failure = (%t, %v)", last, err)
		}
	})

	t.Run("bounded control overflow is retained", func(t *testing.T) {
		control := &lifecycleFaultDirectory{
			Directory: newDestinationDirectoryNode(newDestinationPlatform()),
			names: func(int) ([]string, error) {
				return nil, outputcap.ErrUnsafeNamespace
			},
		}
		if clean, err := controlContainsOnlyLifecycle(control); err != nil || clean {
			t.Fatalf("bounded overflow = (%t, %v)", clean, err)
		}
	})

	t.Run("control enumeration failure is reported", func(t *testing.T) {
		control := &lifecycleFaultDirectory{
			Directory: newDestinationDirectoryNode(newDestinationPlatform()),
			names: func(int) ([]string, error) {
				return nil, errDestinationFake
			},
		}
		if clean, err := controlContainsOnlyLifecycle(control); clean || !errors.Is(err, errDestinationFake) {
			t.Fatalf("enumeration failure = (%t, %v)", clean, err)
		}
	})

	t.Run("exact removal rejects nil capabilities", func(t *testing.T) {
		if err := removeExactEmptyDirectory(nil, "missing", nil); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("nil removal error = %v", err)
		}
	})

	t.Run("exact removal rejects nonempty directory", func(t *testing.T) {
		parent := newDestinationDirectoryNode(newDestinationPlatform())
		child, err := parent.CreateDirectory("child", true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := child.CreateFile("state", true, 0); err != nil {
			t.Fatal(err)
		}
		if err := removeExactEmptyDirectory(parent, "child", child); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("nonempty removal error = %v", err)
		}
	})

	t.Run("guard closure is nil safe", func(t *testing.T) {
		if err := closeGuard(nil); err != nil {
			t.Fatal(err)
		}
		guardErr := errors.New("guard close")
		if err := closeGuard(&lifecycleFaultGuard{closeErr: guardErr}); !errors.Is(err, guardErr) {
			t.Fatalf("guard close error = %v", err)
		}
	})
}

type lifecycleFaultDirectory struct {
	outputcap.Directory
	acquireLock func(string, bool) (outputcap.Lock, bool, error)
	names       func(int) ([]string, error)
	openEntry   func(string) (outputcap.CurrentEntryReference, error)
	removeFile  func(string, outputcap.File) error
	sync        func() error
}

func (directory *lifecycleFaultDirectory) AcquireLock(name string, existingOnly bool) (outputcap.Lock, bool, error) {
	if directory.acquireLock != nil {
		return directory.acquireLock(name, existingOnly)
	}
	return directory.Directory.AcquireLock(name, existingOnly)
}

func (directory *lifecycleFaultDirectory) Names(limit int) ([]string, error) {
	if directory.names != nil {
		return directory.names(limit)
	}
	return directory.Directory.Names(limit)
}

func (directory *lifecycleFaultDirectory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	if directory.openEntry != nil {
		return directory.openEntry(name)
	}
	return directory.Directory.OpenEntry(name)
}

func (directory *lifecycleFaultDirectory) RemoveFile(name string, expected outputcap.File) error {
	if directory.removeFile != nil {
		return directory.removeFile(name, expected)
	}
	return directory.Directory.RemoveFile(name, expected)
}

func (directory *lifecycleFaultDirectory) Sync() error {
	if directory.sync != nil {
		return directory.sync()
	}
	return directory.Directory.Sync()
}

type lifecycleFaultLock struct {
	outputcap.Lock
	closes int
}

func (lock *lifecycleFaultLock) File() outputcap.File { return nil }
func (lock *lifecycleFaultLock) Close() error {
	lock.closes++
	return nil
}

type lifecycleFaultGuard struct{ closeErr error }

func (*lifecycleFaultGuard) Root() outputcap.Directory { return nil }
func (guard *lifecycleFaultGuard) Close() error        { return guard.closeErr }

func hexByte(value byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[value>>4], digits[value&0x0f]})
}

func controlParticipantsDirectoryForTest(t *testing.T, control outputcap.Directory) outputcap.Directory {
	t.Helper()
	lifecycle, err := openExactPrivateDirectory(control, controlLifecycleNamespace)
	if err != nil {
		t.Fatal(err)
	}
	participants, err := openExactPrivateDirectory(lifecycle, controlParticipantsDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Close(); err != nil {
		t.Fatal(err)
	}
	return participants
}

func controlParticipantsNode(t *testing.T, platform *destinationPlatform) *destinationNode {
	t.Helper()
	control := platform.root.entries[controlDirectoryName]
	if control == nil {
		t.Fatal("missing control namespace")
	}
	lifecycle := control.entries[controlLifecycleNamespace]
	if lifecycle == nil || lifecycle.entries[controlParticipantsDirectory] == nil {
		t.Fatal("missing participant namespace")
	}
	return lifecycle.entries[controlParticipantsDirectory]
}

func newDestinationDirectoryNode(platform *destinationPlatform) *destinationDirectory {
	node := &destinationNode{
		id:       platform.nextID,
		private:  true,
		identity: []byte("lifecycle-test"),
		entries:  map[string]*destinationNode{},
	}
	platform.nextID++
	return &destinationDirectory{platform: platform, node: node}
}

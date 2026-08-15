package destinationauthority

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestControlLifecycleRecyclesOnlyAfterTheLastBoundUser(t *testing.T) {
	platform := newDestinationPlatform()
	first := bindRecyclingDestination(t, platform, 0x11, recycleFakePrivateState)
	second := bindRecyclingDestination(t, platform, 0x22, recycleFakePrivateState)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if platform.root.entries[controlDirectoryName] == nil {
		t.Fatal("first close removed a namespace still held by a peer")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if platform.root.entries[controlDirectoryName] != nil {
		t.Fatal("last close retained an empty control namespace")
	}
}

func TestControlLifecyclePreservesStateAndUnknownEntries(t *testing.T) {
	t.Run("recovery state", func(t *testing.T) {
		platform := newDestinationPlatform()
		authority := bindRecyclingDestination(t, platform, 0x31, func(outputcap.Directory) (bool, error) {
			return false, nil
		})
		if err := authority.Close(); err != nil {
			t.Fatal(err)
		}
		control := platform.root.entries[controlDirectoryName]
		if control == nil || control.entries[controlLifecycleNamespace] == nil {
			t.Fatal("recoverable state lost its lifecycle namespace")
		}
		participants := control.entries[controlLifecycleNamespace].entries[controlParticipantsDirectory]
		if participants == nil || len(participants.entries) != 0 {
			t.Fatalf("closed participant markers = %+v", participants)
		}
	})

	t.Run("unknown control entry", func(t *testing.T) {
		platform := newDestinationPlatform()
		authority := bindRecyclingDestination(t, platform, 0x41, recycleFakePrivateState)
		if _, err := authority.control.CreateDirectory("foreign", true); err != nil {
			t.Fatal(err)
		}
		if err := authority.Close(); err != nil {
			t.Fatal(err)
		}
		control := platform.root.entries[controlDirectoryName]
		if control == nil || control.entries["foreign"] == nil {
			t.Fatal("unknown control entry was removed")
		}
	})
}

func TestControlLifecycleReclaimsCrashedParticipantMarkers(t *testing.T) {
	platform := newDestinationPlatform()
	crashed := bindRecyclingDestination(t, platform, 0x51, recycleFakePrivateState)
	if err := crashed.controlUse.participant.Close(); err != nil {
		t.Fatal(err)
	}
	crashed.controlUse = nil

	restarted := bindRecyclingDestination(t, platform, 0x61, recycleFakePrivateState)
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	if platform.root.entries[controlDirectoryName] != nil {
		t.Fatal("restart retained a stale participant marker")
	}
	if err := crashed.Close(); err != nil {
		t.Fatal(err)
	}
}

func bindRecyclingDestination(
	t *testing.T,
	platform *destinationPlatform,
	nonce byte,
	recycler PrivateStateRecycler,
) *BoundDestination {
	t.Helper()
	journal := &destinationJournal{snapshot: LiveCleanupSnapshot{State: LiveCleanupScanComplete}}
	authority, err := BindDestination(BindConfig{
		Platform:               platform,
		DisplayPath:            filepath.Clean(t.TempDir()),
		OpenLiveCleanupJournal: fakeJournalOpener(journal),
		RecyclePrivateState:    recycler,
		ControlUseNonceSource:  bytes.NewReader(bytes.Repeat([]byte{nonce}, controlParticipantNonceBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func recycleFakePrivateState(control outputcap.Directory) (bool, error) {
	kind, exact, err := control.ClassifyExactEntry(checkpointmodel.LiveCleanupNamespaceV1)
	if err != nil || !exact {
		return false, err
	}
	if kind == outputcap.EntryAbsent {
		return true, nil
	}
	if kind != outputcap.EntryDirectory {
		return false, nil
	}
	proof, err := openExactPrivateDirectory(control, checkpointmodel.LiveCleanupNamespaceV1)
	if err != nil {
		return false, err
	}
	names, namesErr := proof.Names(1)
	if namesErr != nil || len(names) != 0 {
		return false, errors.Join(namesErr, proof.Close())
	}
	removeErr := control.RemoveDirectory(checkpointmodel.LiveCleanupNamespaceV1, proof)
	if removeErr == nil {
		removeErr = control.Sync()
	}
	return removeErr == nil, errors.Join(removeErr, proof.Close())
}

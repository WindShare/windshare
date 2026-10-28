package outputruntime

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestLiveCleanupBacklogDrainsAcrossBindings(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	var events []FilesystemOutputTrace
	open := func() (*Authority, outputcap.Platform) {
		t.Helper()
		var platform outputcap.Platform
		authority, err := New(Config{
			RootPath: root,
			Tracer:   FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) { events = append(events, event) }),
			PlatformFactory: func(path string, create bool) (outputcap.Platform, error) {
				base, err := openOutputRuntimeTestPlatform(path, create)
				if err != nil {
					return nil, err
				}
				platform = &liveOnlyRuntimePlatform{Platform: base}
				return platform, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = authority.Close() })
		mode, err := authority.BindDestination(context.Background())
		if err != nil || !mode.LiveOnly() || !authority.binding.Capabilities().CrashCleanup().Supported() {
			t.Fatalf("cleanup backlog revoked the destination's capability: %+v %v", mode, err)
		}
		return authority, platform
	}

	first, platform := open()
	seedLiveCleanupBacklog(t, platform, first.destination.LiveCleanupProfile())
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		count uint64
		more  bool
	}{
		{uint64(ordinaryoutput.MaximumLiveCleanupTicketsPerBindV1), true},
		{1, false},
	} {
		authority, platform := open()
		event := events[len(events)-1]
		if event.RuntimeOperation != FilesystemOutputRuntimeAdmitDestination || event.DestinationBindingID.IsZero() || event.DestinationBindingID != authority.binding.ID() ||
			event.LiveCleanupTicketCount != want.count || event.LiveCleanupReclaimedTicketCount != want.count || event.LiveCleanupHasMore != want.more {
			t.Fatalf("bounded cleanup progress: %+v, want %+v", event, want)
		}
		if !want.more {
			control, err := platform.Root().OpenDirectory(checkpointstore.ControlDirectory, true)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := control.OpenDirectory(checkpointmodel.LiveCleanupNamespaceV1, true)
			if err != nil {
				t.Fatal(err)
			}
			names, err := proof.Names(1)
			if err != nil || len(names) != 1 || names[0] != "tickets" {
				t.Fatalf("cleanup retained a stage witness: %v %v", names, err)
			}
			if err := proof.Close(); err != nil {
				t.Fatal(err)
			}
			if err := control.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err := authority.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "published.bin")); err != nil || string(data) != "data" {
		t.Fatalf("cleanup changed the delivered file: %q %v", data, err)
	}
}

func seedLiveCleanupBacklog(t *testing.T, platform outputcap.Platform, profile checkpointmodel.LiveCleanupNativeProfile) {
	t.Helper()
	root := platform.Root()
	control, err := root.OpenDirectory(checkpointstore.ControlDirectory, true)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	journal, err := checkpointstore.OpenLiveCleanupJournal(control)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	proof, err := control.OpenDirectory(checkpointmodel.LiveCleanupNamespaceV1, true)
	if err != nil {
		t.Fatal(err)
	}
	defer proof.Close()
	creator := root.(interface {
		CreateLiveCleanupStage(outputcap.Directory, checkpointmodel.LiveCleanupTicket) error
	})
	for index := uint32(0); index <= ordinaryoutput.MaximumLiveCleanupTicketsPerBindV1; index++ {
		var nonce [checkpointmodel.LiveCleanupNonceBytesV1]byte
		binary.BigEndian.PutUint32(nonce[:], index+1)
		ticket, err := checkpointmodel.NewLiveCleanupTicket(checkpointmodel.LiveCleanupTicketSpec{
			Nonce: nonce[:], ExactSize: 4, Profile: profile, Generation: 1, State: checkpointmodel.LiveCleanupTicketCommitted,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Create(ticket); err != nil {
			t.Fatal(err)
		}
		// Cover crash cuts with a published hard link, an unfinished stage, and
		// tickets committed before stage creation without 257 physical data files.
		if index > 1 {
			continue
		}
		if err := creator.CreateLiveCleanupStage(proof, ticket); err != nil {
			t.Fatal(err)
		}
		stage, err := proof.OpenMutableFile(ticket.StageName(), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stage.WriteAt([]byte("data"), 0); err != nil {
			t.Fatal(err)
		}
		if err := stage.Sync(); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			final, err := root.LinkFileNoReplace(stage, "published.bin")
			if err != nil {
				t.Fatal(err)
			}
			if err := final.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err := stage.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

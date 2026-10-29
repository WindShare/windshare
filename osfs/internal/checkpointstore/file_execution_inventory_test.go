package checkpointstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestFileExecutionInventorySeparatesReadFailureFromCorruption(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0x81)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	record := checkpointRecordFixture(t, ownership, operation, 0x82)
	if _, err := repository.Create(record); err != nil {
		t.Fatal(err)
	}
	root := repository.records
	failure := errors.New("checkpoint record temporarily unreadable")
	var readErr error = failure
	repository.records = &faultDirectory{
		Directory: root,
		openDirectory: func(name string, private bool) (outputcap.Directory, error) {
			shard, err := root.OpenDirectory(name, private)
			if err != nil {
				return nil, err
			}
			return &faultDirectory{
				Directory: shard,
				openObserved: func(name string, private bool) (outputcap.ObservedFile, error) {
					if readErr != nil {
						return nil, readErr
					}
					return shard.OpenObservedFile(name, private)
				},
			}, nil
		},
	}
	store, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	slots, attention, err := store.Inventory()
	if !errors.Is(err, failure) || ErrorCodeFor(err) != ErrorStateIO || slots != nil || attention != nil {
		t.Fatalf("failed inventory = (%+v, %+v, %v), want I/O failure without corruption", slots, attention, err)
	}
	readErr = nil
	slots, attention, err = store.Inventory()
	if err != nil || len(slots) != 1 || len(attention) != 0 {
		t.Fatalf("retry inventory = (%+v, %+v, %v)", slots, attention, err)
	}
	selected, exact := slots[0].Record()
	if !exact || !bytes.Equal(selected.CanonicalBytes(), record.CanonicalBytes()) {
		t.Fatal("read failure changed the healthy checkpoint")
	}

	// Reopen authority after corrupting the image so the next inventory must
	// authenticate storage instead of relying on the successful cached scan.
	shardName, name := recordLocation(record.RecordID())
	shard, err := root.OpenDirectory(shardName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer shard.Close()
	file, err := shard.OpenMutableFile(name, true)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteAt([]byte("invalid"), 0)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	repository.authority = newFileExecutionAuthority()
	reopened, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	slots, attention, err = reopened.Inventory()
	if err != nil || len(slots) != 0 || len(attention) != 1 {
		t.Fatalf("corrupt inventory = (%+v, %+v, %v)", slots, attention, err)
	}
	if attention[0].Code() != AttentionCorruptRecord || attention[0].Reference() == "" || !RecoveryBlocked(attention) {
		t.Fatalf("corrupt checkpoint lost its diagnostic evidence: %+v", attention)
	}
}

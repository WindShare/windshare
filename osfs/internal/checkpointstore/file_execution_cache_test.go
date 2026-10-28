package checkpointstore

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
)

func TestFileRecordCacheBoundsMemoryAndReloadsEvictedProgress(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0xe1)
	defer registry.Close()
	defer lease.Close()
	defer repository.Close()
	store, err := NewFreshFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	management, err := NewFreshFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	base := checkpointRecordFixture(t, ownership, operation, 0xe2)
	var first checkpointmodel.Record
	for index := 1; index <= maximumCachedFileRecords+2; index++ {
		path := fmt.Sprintf("folder/file-%d.bin", index)
		objectBytes := make([]byte, transfer.OwnedObjectIdentityBytes)
		binary.BigEndian.PutUint64(objectBytes, uint64(index))
		object, err := checkpointmodel.ObjectIDFromBytes(objectBytes)
		if err != nil {
			t.Fatal(err)
		}
		record := checkpointRecordVariant(t, base, recordVariant{path: &path, object: &object})
		if index == 1 {
			first = record
		}
		if observed, err := store.installInitialRecord(context.Background(), record); err != nil || !observed.Installed() {
			t.Fatalf("install file %d: installed=%t error=%v", index, observed.Installed(), err)
		}
		if count := store.RecordCount(); count > maximumCachedFileRecords {
			t.Fatalf("cached records=%d exceeds %d", count, maximumCachedFileRecords)
		}
	}
	if _, cached := store.authority.physical[first.RecordID()]; cached {
		t.Fatal("fixture did not evict the first record")
	}
	// An active transaction can outlive cache eviction. Its exact disk record
	// remains authoritative when it next checkpoints or resumes.
	observed, err := store.installInitialRecord(context.Background(), first)
	selected, found := observed.Resolution().Record()
	if err != nil || observed.Installed() || !found || selected.RecordID() != first.RecordID() {
		t.Fatalf("reload evicted record: installed=%t found=%t error=%v", observed.Installed(), found, err)
	}
	slots, attention, err := management.Inventory()
	if err != nil || len(attention) != 0 || len(slots) != maximumCachedFileRecords+2 {
		t.Fatalf("management inventory lost evicted records: records=%d attention=%d error=%v", len(slots), len(attention), err)
	}
}

package protocolcontract

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
)

func TestBlockFragmentMutationMatrix(t *testing.T) {
	f := newFixture(t)
	record := canonicalBlockRecord(t, f)
	recordID := first(hash(record), 16)
	fragment := encodeSingleFragment(f.operationID, recordID, record)
	if !validSingleFragment(fragment, f.operationID, recordID) {
		t.Fatal("reference fragment is invalid")
	}

	mutations := map[string]func([]byte){
		"operation-id": func(value []byte) { value[4] ^= 1 },
		"record-id":    func(value []byte) { value[12] ^= 1 },
		"index":        func(value []byte) { binary.BigEndian.PutUint32(value[28:32], 1) },
		"count":        func(value []byte) { binary.BigEndian.PutUint32(value[32:36], 2) },
		"total-length": func(value []byte) { binary.BigEndian.PutUint32(value[36:40], uint32(len(record)+1)) },
		"payload-length": func(value []byte) {
			binary.BigEndian.PutUint32(value[40:44], uint32(len(record)+1))
		},
		"last-flag": func(value []byte) { value[2] = 0 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := slices.Clone(fragment)
			mutate(candidate)
			if validSingleFragment(candidate, f.operationID, recordID) {
				t.Fatal("accepted mutated fragment axis")
			}
		})
	}
	conflictingDuplicate := slices.Clone(fragment)
	conflictingDuplicate[len(conflictingDuplicate)-1] ^= 1
	if !fragmentDuplicateConflicts(fragment, conflictingDuplicate) {
		t.Fatal("conflicting duplicate was treated as idempotent")
	}
}

func encodeSingleFragment(operationID uint64, recordID, record []byte) []byte {
	header := make([]byte, 44)
	header[0], header[1], header[2] = 1, 8, 1
	binary.BigEndian.PutUint64(header[4:12], operationID)
	copy(header[12:28], recordID)
	binary.BigEndian.PutUint32(header[32:36], 1)
	binary.BigEndian.PutUint32(header[36:40], uint32(len(record)))
	binary.BigEndian.PutUint32(header[40:44], uint32(len(record)))
	return slices.Concat(header, record)
}

func validSingleFragment(fragment []byte, expectedOperationID uint64, expectedRecordID []byte) bool {
	if len(fragment) < 44 || fragment[0] != 1 || fragment[1] != 8 || fragment[2] != 1 || fragment[3] != 0 {
		return false
	}
	if binary.BigEndian.Uint64(fragment[4:12]) != expectedOperationID || !bytes.Equal(fragment[12:28], expectedRecordID) {
		return false
	}
	index := binary.BigEndian.Uint32(fragment[28:32])
	count := binary.BigEndian.Uint32(fragment[32:36])
	totalLength := binary.BigEndian.Uint32(fragment[36:40])
	payloadLength := binary.BigEndian.Uint32(fragment[40:44])
	payload := fragment[44:]
	if index != 0 || count != 1 || totalLength == 0 || totalLength > maxBlockRecordBytes ||
		payloadLength != uint32(len(payload)) || totalLength != payloadLength || len(payload) > maxFragmentPayload {
		return false
	}
	return bytes.Equal(first(hash(payload), 16), expectedRecordID)
}

func fragmentDuplicateConflicts(previous, candidate []byte) bool {
	if len(previous) < 44 || len(candidate) < 44 {
		return false
	}
	sameIdentityAndIndex := bytes.Equal(previous[4:28], candidate[4:28]) &&
		bytes.Equal(previous[28:32], candidate[28:32])
	return sameIdentityAndIndex && !bytes.Equal(previous, candidate)
}

package protocolcontract

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
)

func TestBlockFragmentMutationMatrix(t *testing.T) {
	f := newFixture(t)
	objects := buildSenderObjects(t, f)
	var record []byte
	for _, candidate := range objects {
		if candidate.domain == domainBlockRecord {
			record = candidate.encoded
			break
		}
	}
	if record == nil {
		t.Fatal("block record fixture is missing")
	}
	recordID := first(hash(record), 16)
	fragment := encodeSingleFragment(f.operationID, recordID, record)
	if !validSingleFragment(fragment, f.operationID, recordID) {
		t.Fatal("reference fragment is invalid")
	}

	mutations := map[string]func([]byte){
		"operation-id": func(value []byte) { value[4] ^= 1 },
		"record-id":    func(value []byte) { value[20] ^= 1 },
		"index":        func(value []byte) { binary.BigEndian.PutUint32(value[36:40], 1) },
		"count":        func(value []byte) { binary.BigEndian.PutUint32(value[40:44], 2) },
		"total-length": func(value []byte) { binary.BigEndian.PutUint32(value[44:48], uint32(len(record)+1)) },
		"payload-length": func(value []byte) {
			binary.BigEndian.PutUint32(value[48:52], uint32(len(record)+1))
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

func encodeSingleFragment(operationID, recordID, record []byte) []byte {
	header := make([]byte, 52)
	header[0], header[1], header[2] = 1, 8, 1
	copy(header[4:20], operationID)
	copy(header[20:36], recordID)
	binary.BigEndian.PutUint32(header[40:44], 1)
	binary.BigEndian.PutUint32(header[44:48], uint32(len(record)))
	binary.BigEndian.PutUint32(header[48:52], uint32(len(record)))
	return slices.Concat(header, record)
}

func validSingleFragment(fragment, expectedOperationID, expectedRecordID []byte) bool {
	if len(fragment) < 52 || fragment[0] != 1 || fragment[1] != 8 || fragment[2] != 1 || fragment[3] != 0 {
		return false
	}
	if !bytes.Equal(fragment[4:20], expectedOperationID) || !bytes.Equal(fragment[20:36], expectedRecordID) {
		return false
	}
	index := binary.BigEndian.Uint32(fragment[36:40])
	count := binary.BigEndian.Uint32(fragment[40:44])
	totalLength := binary.BigEndian.Uint32(fragment[44:48])
	payloadLength := binary.BigEndian.Uint32(fragment[48:52])
	payload := fragment[52:]
	if index != 0 || count != 1 || totalLength == 0 || totalLength > maxBlockRecordBytes ||
		payloadLength != uint32(len(payload)) || totalLength != payloadLength || len(payload) > maxFragmentPayload {
		return false
	}
	return bytes.Equal(first(hash(payload), 16), expectedRecordID)
}

func fragmentDuplicateConflicts(previous, candidate []byte) bool {
	if len(previous) < 52 || len(candidate) < 52 {
		return false
	}
	sameIdentityAndIndex := bytes.Equal(previous[4:36], candidate[4:36]) &&
		bytes.Equal(previous[36:40], candidate[36:40])
	return sameIdentityAndIndex && !bytes.Equal(previous, candidate)
}

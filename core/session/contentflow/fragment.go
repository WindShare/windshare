package contentflow

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	FragmentCodecVersion        = 1
	FragmentHeaderBytes         = 52
	MaxFragmentPayloadBytes     = 65_440
	MaxFragmentCount            = 128
	MaxOperationReassemblyBytes = uint64(records.MaxBlockRecordObjectBytes)
	FragmentTimeout             = 15 * time.Second
	FragmentTombstone           = 30 * time.Second
	MaxFragmentTombstones       = 4_096
)

var (
	ErrFragmentMalformed = errors.New("authenticated block fragment is malformed")
	ErrFragmentConflict  = errors.New("authenticated block fragment conflicts with prior data")
	ErrFragmentTimeout   = errors.New("block fragment reassembly timed out")
	ErrFragmentCancelled = errors.New("block fragment operation was cancelled")
	ErrRecordDigest      = errors.New("reassembled block record has the wrong record identity")
	ErrAssemblerClosed   = errors.New("block fragment assembler is closed")
)

type Fragment struct {
	OperationID protocolsession.OperationID
	RecordID    records.RecordID
	Index       uint32
	Count       uint32
	TotalLength uint32
	Last        bool
	Payload     []byte
}

func FragmentRecord(operationID protocolsession.OperationID, object []byte) ([]protocolsession.Message, error) {
	if operationID.IsZero() || len(object) == 0 || len(object) > records.MaxBlockRecordObjectBytes {
		return nil, ErrFragmentMalformed
	}
	count := (len(object) + MaxFragmentPayloadBytes - 1) / MaxFragmentPayloadBytes
	if count == 0 || count > MaxFragmentCount {
		return nil, ErrFragmentMalformed
	}
	recordID := records.RecordIDFromObject(object)
	messages := make([]protocolsession.Message, count)
	for index := 0; index < count; index++ {
		start := index * MaxFragmentPayloadBytes
		end := min(start+MaxFragmentPayloadBytes, len(object))
		plaintext := make([]byte, FragmentHeaderBytes, FragmentHeaderBytes+end-start)
		plaintext[0] = FragmentCodecVersion
		plaintext[1] = byte(protocolsession.MessageBlockFragment)
		if index == count-1 {
			plaintext[2] = 1
		}
		copy(plaintext[4:20], operationID.Bytes())
		copy(plaintext[20:36], recordID.Bytes())
		binary.BigEndian.PutUint32(plaintext[36:40], uint32(index))
		binary.BigEndian.PutUint32(plaintext[40:44], uint32(count))
		binary.BigEndian.PutUint32(plaintext[44:48], uint32(len(object)))
		binary.BigEndian.PutUint32(plaintext[48:52], uint32(end-start))
		plaintext = append(plaintext, object[start:end]...)
		message, err := protocolsession.DecodeMessage(plaintext)
		if err != nil {
			return nil, fmt.Errorf("build block fragment %d: %w", index, err)
		}
		messages[index] = message
	}
	return messages, nil
}

// DecodeAuthenticatedFragment must only receive plaintext returned by an
// OperationEnvelope opener. Its strict validation completes before any caller
// can use TotalLength to reserve or allocate reassembly storage.
func DecodeAuthenticatedFragment(plaintext []byte) (Fragment, error) {
	if len(plaintext) < FragmentHeaderBytes || len(plaintext) > protocolsession.MaxEnvelopePlaintextBytes {
		return Fragment{}, ErrFragmentMalformed
	}
	if plaintext[0] != FragmentCodecVersion || plaintext[1] != byte(protocolsession.MessageBlockFragment) || plaintext[2]&^byte(1) != 0 || plaintext[3] != 0 {
		return Fragment{}, ErrFragmentMalformed
	}
	operationID, err := protocolsession.OperationIDFromBytes(plaintext[4:20])
	if err != nil || operationID.IsZero() {
		return Fragment{}, ErrFragmentMalformed
	}
	recordID, err := records.RecordIDFromBytes(plaintext[20:36])
	if err != nil || recordID.IsZero() {
		return Fragment{}, ErrFragmentMalformed
	}
	index := binary.BigEndian.Uint32(plaintext[36:40])
	count := binary.BigEndian.Uint32(plaintext[40:44])
	total := binary.BigEndian.Uint32(plaintext[44:48])
	payloadLength := binary.BigEndian.Uint32(plaintext[48:52])
	if count == 0 || count > MaxFragmentCount || index >= count || total == 0 || total > records.MaxBlockRecordObjectBytes {
		return Fragment{}, ErrFragmentMalformed
	}
	expectedCount := (uint64(total) + MaxFragmentPayloadBytes - 1) / MaxFragmentPayloadBytes
	if uint64(count) != expectedCount || uint64(payloadLength) != uint64(len(plaintext)-FragmentHeaderBytes) {
		return Fragment{}, ErrFragmentMalformed
	}
	last := plaintext[2]&1 != 0
	if last != (index == count-1) {
		return Fragment{}, ErrFragmentMalformed
	}
	expectedPayload := uint32(MaxFragmentPayloadBytes)
	if last {
		expectedPayload = total - uint32(uint64(count-1)*MaxFragmentPayloadBytes)
	}
	if payloadLength != expectedPayload {
		return Fragment{}, ErrFragmentMalformed
	}
	return Fragment{
		OperationID: operationID, RecordID: recordID, Index: index, Count: count,
		TotalLength: total, Last: last, Payload: slices.Clone(plaintext[FragmentHeaderBytes:]),
	}, nil
}

func recordDigestMatches(id records.RecordID, object []byte) bool {
	digest := sha256.Sum256(object)
	return slices.Equal(id[:], digest[:len(id)])
}

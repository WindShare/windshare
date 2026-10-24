package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
)

const (
	LiveCleanupNamespaceV1     = "cleanup-v1"
	LiveCleanupTicketVersionV1 = uint8(1)
	LiveCleanupTicketDomainV1  = "windshare/live-cleanup-ticket/v1"
	LiveCleanupStageDomainV1   = "windshare/live-cleanup-stage/v1"

	LiveCleanupNonceBytesV1         = 16
	MaximumLiveCleanupTicketBytesV1 = 256
)

var ErrInvalidLiveCleanupTicket = errors.New("live cleanup ticket is invalid")

type LiveCleanupTicketState uint8

const (
	LiveCleanupTicketCommitted LiveCleanupTicketState = iota + 1
	LiveCleanupStageCreated
	LiveCleanupStageRemoved
)

func (state LiveCleanupTicketState) Valid() bool {
	return state >= LiveCleanupTicketCommitted && state <= LiveCleanupStageRemoved
}

// LiveCleanupNativeProfile is intentionally narrower than restart-recovery
// certification: a live-only ticket proves only cleanup semantics.
type LiveCleanupNativeProfile uint8

const (
	LiveCleanupLinuxExt4V1 LiveCleanupNativeProfile = iota + 1
	LiveCleanupWindowsNTFSV1
)

func (profile LiveCleanupNativeProfile) Valid() bool {
	return profile == LiveCleanupLinuxExt4V1 || profile == LiveCleanupWindowsNTFSV1
}

func (profile LiveCleanupNativeProfile) String() string {
	switch profile {
	case LiveCleanupLinuxExt4V1:
		return "linux/ext4/v1"
	case LiveCleanupWindowsNTFSV1:
		return "windows/ntfs/v1"
	default:
		return ""
	}
}

type LiveCleanupTicketSpec struct {
	Nonce      []byte
	ExactSize  uint64
	Profile    LiveCleanupNativeProfile
	Generation uint64
	State      LiveCleanupTicketState
}

// LiveCleanupTicket is a narrow ownership proof, not a resumable checkpoint.
// Its type cannot carry selection, intent, revision, ranges, or a public path.
type LiveCleanupTicket struct {
	nonce      [LiveCleanupNonceBytesV1]byte
	exactSize  uint64
	profile    LiveCleanupNativeProfile
	generation uint64
	state      LiveCleanupTicketState
}

func NewLiveCleanupTicket(spec LiveCleanupTicketSpec) (LiveCleanupTicket, error) {
	if len(spec.Nonce) != LiveCleanupNonceBytesV1 || allZero(spec.Nonce) ||
		!spec.Profile.Valid() ||
		spec.Generation == 0 || !spec.State.Valid() {
		return LiveCleanupTicket{}, ErrInvalidLiveCleanupTicket
	}
	var nonce [LiveCleanupNonceBytesV1]byte
	copy(nonce[:], spec.Nonce)
	return LiveCleanupTicket{
		nonce: nonce, exactSize: spec.ExactSize, profile: spec.Profile,
		generation: spec.Generation, state: spec.State,
	}, nil
}

func (ticket LiveCleanupTicket) Nonce() []byte                     { return slices.Clone(ticket.nonce[:]) }
func (ticket LiveCleanupTicket) ExactSize() uint64                 { return ticket.exactSize }
func (ticket LiveCleanupTicket) Profile() LiveCleanupNativeProfile { return ticket.profile }
func (ticket LiveCleanupTicket) Generation() uint64                { return ticket.generation }
func (ticket LiveCleanupTicket) State() LiveCleanupTicketState     { return ticket.state }
func (ticket LiveCleanupTicket) StageName() string {
	if !ticket.Valid() {
		return ""
	}
	// The name is derived entirely from the already-committed nonce, so an
	// ambiguous create can be reconciled without trusting caller path text.
	hash := sha256.New()
	_, _ = hash.Write([]byte(LiveCleanupStageDomainV1))
	_, _ = hash.Write([]byte{0, LiveCleanupTicketVersionV1})
	writeLiveCleanupFrame(hash, ticket.nonce[:])
	digest := hash.Sum(nil)
	return "stage-" + hex.EncodeToString(digest[:LiveCleanupNonceBytesV1]) + ".part"
}
func (ticket LiveCleanupTicket) Valid() bool {
	_, err := NewLiveCleanupTicket(LiveCleanupTicketSpec{
		Nonce: ticket.nonce[:], ExactSize: ticket.exactSize, Profile: ticket.profile,
		Generation: ticket.generation, State: ticket.state,
	})
	return err == nil
}

type LiveCleanupEvent uint8

const (
	LiveCleanupRecordStageCreated LiveCleanupEvent = iota + 1
	LiveCleanupRecordStageRemoved
)

func ReduceLiveCleanupTicket(ticket LiveCleanupTicket, event LiveCleanupEvent) (LiveCleanupTicket, error) {
	if !ticket.Valid() {
		return LiveCleanupTicket{}, ErrInvalidLiveCleanupTicket
	}
	next := ticket
	switch event {
	case LiveCleanupRecordStageCreated:
		if ticket.state != LiveCleanupTicketCommitted {
			return LiveCleanupTicket{}, ErrInvalidLiveCleanupTicket
		}
		next.state = LiveCleanupStageCreated
	case LiveCleanupRecordStageRemoved:
		if ticket.state != LiveCleanupStageCreated {
			return LiveCleanupTicket{}, ErrInvalidLiveCleanupTicket
		}
		next.state = LiveCleanupStageRemoved
	default:
		return LiveCleanupTicket{}, ErrInvalidLiveCleanupTicket
	}
	if ticket.generation == ^uint64(0) {
		return LiveCleanupTicket{}, ErrInvalidLiveCleanupTicket
	}
	next.generation++
	return next, nil
}

func EncodeLiveCleanupTicket(ticket LiveCleanupTicket) ([]byte, error) {
	if !ticket.Valid() {
		return nil, ErrInvalidLiveCleanupTicket
	}
	var encoded bytes.Buffer
	_, _ = encoded.WriteString(LiveCleanupTicketDomainV1)
	_ = encoded.WriteByte(0)
	_ = encoded.WriteByte(LiveCleanupTicketVersionV1)
	writeLiveCleanupFrame(&encoded, ticket.nonce[:])
	writeLiveCleanupUint64(&encoded, ticket.exactSize)
	_ = encoded.WriteByte(byte(ticket.profile))
	writeLiveCleanupUint64(&encoded, ticket.generation)
	_ = encoded.WriteByte(byte(ticket.state))
	if encoded.Len() > MaximumLiveCleanupTicketBytesV1 {
		return nil, ErrInvalidLiveCleanupTicket
	}
	return encoded.Bytes(), nil
}

func DecodeLiveCleanupTicket(encoded []byte) (LiveCleanupTicket, error) {
	if len(encoded) == 0 || len(encoded) > MaximumLiveCleanupTicketBytesV1 {
		return LiveCleanupTicket{}, ErrInvalidLiveCleanupTicket
	}
	prefix := append(append([]byte(nil), LiveCleanupTicketDomainV1...), 0, LiveCleanupTicketVersionV1)
	if !bytes.HasPrefix(encoded, prefix) {
		return LiveCleanupTicket{}, ErrInvalidLiveCleanupTicket
	}
	cursor := liveCleanupCursor{encoded: encoded, offset: len(prefix)}
	nonce, nonceErr := cursor.frame(LiveCleanupNonceBytesV1)
	size, sizeErr := cursor.uint64()
	profile, profileErr := cursor.byte()
	generation, generationErr := cursor.uint64()
	state, stateErr := cursor.byte()
	if errors.Join(nonceErr, sizeErr, profileErr, generationErr, stateErr) != nil || cursor.offset != len(encoded) {
		return LiveCleanupTicket{}, ErrInvalidLiveCleanupTicket
	}
	ticket, err := NewLiveCleanupTicket(LiveCleanupTicketSpec{
		Nonce: nonce, ExactSize: size, Profile: LiveCleanupNativeProfile(profile), Generation: generation,
		State: LiveCleanupTicketState(state),
	})
	canonical, canonicalErr := EncodeLiveCleanupTicket(ticket)
	if err != nil || canonicalErr != nil || !bytes.Equal(canonical, encoded) {
		return LiveCleanupTicket{}, ErrInvalidLiveCleanupTicket
	}
	return ticket, nil
}

type liveCleanupCursor struct {
	encoded []byte
	offset  int
}

func (cursor *liveCleanupCursor) take(count int) ([]byte, error) {
	if count < 0 || count > len(cursor.encoded)-cursor.offset {
		return nil, ErrInvalidLiveCleanupTicket
	}
	value := cursor.encoded[cursor.offset : cursor.offset+count]
	cursor.offset += count
	return value, nil
}

func (cursor *liveCleanupCursor) byte() (byte, error) {
	value, err := cursor.take(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (cursor *liveCleanupCursor) uint64() (uint64, error) {
	value, err := cursor.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value), nil
}

func (cursor *liveCleanupCursor) frame(maximum int) ([]byte, error) {
	length, err := cursor.uint64()
	if err != nil || length == 0 || length > uint64(maximum) || length > uint64(len(cursor.encoded)-cursor.offset) {
		return nil, fmt.Errorf("%w: framed field", ErrInvalidLiveCleanupTicket)
	}
	return cursor.take(int(length))
}

func writeLiveCleanupFrame(writer interface{ Write([]byte) (int, error) }, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

func writeLiveCleanupUint64(writer interface{ Write([]byte) (int, error) }, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func allZero(value []byte) bool {
	for _, current := range value {
		if current != 0 {
			return false
		}
	}
	return true
}

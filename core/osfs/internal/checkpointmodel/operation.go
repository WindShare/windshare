package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	receiveOperationVersion     = uint8(1)
	receiveOperationDomain      = "windshare/receive-operation/v1"
	compatibleOperationDomain   = "windshare/compatible-operation-key/v1"
	MaximumReceiveOperationSize = 16 * 1024 * 1024
)

var ErrInvalidReceiveOperation = errors.New("receive operation record is invalid")

type CompatibleOperationKey [sha256.Size]byte

func CompatibleOperationKeyFromBytes(raw []byte) (CompatibleOperationKey, error) {
	if len(raw) != sha256.Size {
		return CompatibleOperationKey{}, ErrInvalidReceiveOperation
	}
	var key CompatibleOperationKey
	copy(key[:], raw)
	if key.IsZero() {
		return CompatibleOperationKey{}, ErrInvalidReceiveOperation
	}
	return key, nil
}

func (key CompatibleOperationKey) Bytes() []byte { return slices.Clone(key[:]) }
func (key CompatibleOperationKey) IsZero() bool  { return key == CompatibleOperationKey{} }

func NewCLICompatibleOperationKey(
	selection transfer.SelectionSpec,
	artifact receivecontract.ArtifactSpec,
	authority receivecontract.AuthorityRef,
) (CompatibleOperationKey, error) {
	if selection.IsZero() || artifact.IsZero() || authority.IsZero() {
		return CompatibleOperationKey{}, ErrInvalidReceiveOperation
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(compatibleOperationDomain))
	_, _ = hash.Write([]byte{0, receiveOperationVersion})
	writeOperationFrame(hash, selection.CanonicalBytes())
	writeOperationFrame(hash, artifact.CanonicalBytes())
	writeOperationFrame(hash, []byte{byte(receivecontract.PlanDirectTree)})
	writeOperationFrame(hash, []byte{byte(receivecontract.AuthorityNativeContainer)})
	writeOperationFrame(hash, authority.Bytes())
	var key CompatibleOperationKey
	copy(key[:], hash.Sum(nil))
	return key, nil
}

type ReopenKeyKind uint8

const (
	ReopenNone ReopenKeyKind = iota + 1
	ReopenCLICompatible
)

type ReopenKey struct {
	kind ReopenKeyKind
	key  CompatibleOperationKey
}

func NoReopenKey() ReopenKey { return ReopenKey{kind: ReopenNone} }

func CLIReopenKey(key CompatibleOperationKey) (ReopenKey, error) {
	if key.IsZero() {
		return ReopenKey{}, ErrInvalidReceiveOperation
	}
	return ReopenKey{kind: ReopenCLICompatible, key: key}, nil
}

func (key ReopenKey) Kind() ReopenKeyKind                   { return key.kind }
func (key ReopenKey) CompatibleKey() CompatibleOperationKey { return key.key }

func (key ReopenKey) valid() bool {
	return key.kind == ReopenNone && key.key.IsZero() ||
		key.kind == ReopenCLICompatible && !key.key.IsZero()
}

// ReceiveOperation stores the immutable intent image. Decoding this envelope is
// deliberately not enough to grant authority: callers must VerifyIntent with the
// transfer package's canonical decoder before reopening a materializer.
type ReceiveOperation struct {
	operationID   receivecontract.OperationID
	intentBytes   []byte
	intentDigest  transfer.ReceiveIntentDigest
	bindingDigest receivecontract.BindingDigest
	reopenKey     ReopenKey
}

func NewReceiveOperation(intent transfer.ReceiveIntent, reopenKey ReopenKey) (ReceiveOperation, error) {
	if intent.IsZero() || !reopenKey.valid() {
		return ReceiveOperation{}, ErrInvalidReceiveOperation
	}
	record := ReceiveOperation{
		operationID: intent.OperationID(), intentBytes: intent.CanonicalBytes(),
		intentDigest: intent.Digest(), bindingDigest: intent.BindingDigest(), reopenKey: reopenKey,
	}
	if !record.Valid() {
		return ReceiveOperation{}, ErrInvalidReceiveOperation
	}
	return record, nil
}

func (record ReceiveOperation) OperationID() receivecontract.OperationID { return record.operationID }
func (record ReceiveOperation) IntentBytes() []byte                      { return slices.Clone(record.intentBytes) }
func (record ReceiveOperation) ReceiveIntentDigest() transfer.ReceiveIntentDigest {
	return record.intentDigest
}
func (record ReceiveOperation) BindingDigest() receivecontract.BindingDigest {
	return record.bindingDigest
}
func (record ReceiveOperation) ReopenKey() ReopenKey { return record.reopenKey }

func (record ReceiveOperation) Valid() bool {
	if record.operationID.IsZero() || record.intentDigest.IsZero() ||
		record.bindingDigest.IsZero() || !record.reopenKey.valid() ||
		len(record.intentBytes) == 0 || len(record.intentBytes) > MaximumReceiveOperationSize {
		return false
	}
	return transfer.ReceiveIntentDigest(sha256.Sum256(record.intentBytes)) == record.intentDigest
}

type ReceiveIntentDecoder func([]byte) (transfer.ReceiveIntent, error)

func (record ReceiveOperation) VerifyIntent(decode ReceiveIntentDecoder) (transfer.ReceiveIntent, error) {
	if !record.Valid() || decode == nil {
		return transfer.ReceiveIntent{}, ErrInvalidReceiveOperation
	}
	intent, err := decode(record.IntentBytes())
	if err != nil || intent.IsZero() || intent.OperationID() != record.operationID ||
		intent.Digest() != record.intentDigest || intent.BindingDigest() != record.bindingDigest ||
		!bytes.Equal(intent.CanonicalBytes(), record.intentBytes) {
		return transfer.ReceiveIntent{}, errors.Join(ErrInvalidReceiveOperation, err)
	}
	return intent, nil
}

func EncodeReceiveOperation(record ReceiveOperation) ([]byte, error) {
	if !record.Valid() {
		return nil, ErrInvalidReceiveOperation
	}
	var encoded bytes.Buffer
	_, _ = encoded.WriteString(receiveOperationDomain)
	_ = encoded.WriteByte(0)
	_ = encoded.WriteByte(receiveOperationVersion)
	writeOperationFrame(&encoded, record.operationID.Bytes())
	writeOperationFrame(&encoded, record.intentBytes)
	writeOperationFrame(&encoded, record.intentDigest.Bytes())
	writeOperationFrame(&encoded, record.bindingDigest.Bytes())
	_ = encoded.WriteByte(byte(record.reopenKey.kind))
	if record.reopenKey.kind == ReopenCLICompatible {
		writeOperationFrame(&encoded, record.reopenKey.key.Bytes())
	}
	return encoded.Bytes(), nil
}

func DecodeReceiveOperation(encoded []byte) (ReceiveOperation, error) {
	if len(encoded) == 0 || len(encoded) > MaximumReceiveOperationSize {
		return ReceiveOperation{}, ErrInvalidReceiveOperation
	}
	prefix := append(append([]byte(nil), receiveOperationDomain...), 0, receiveOperationVersion)
	if !bytes.HasPrefix(encoded, prefix) {
		return ReceiveOperation{}, ErrInvalidReceiveOperation
	}
	cursor := operationCursor{encoded: encoded, offset: len(prefix)}
	operationRaw, err := cursor.frame(receivecontract.StableIdentityBytes)
	if err != nil {
		return ReceiveOperation{}, err
	}
	intentBytes, err := cursor.frame(MaximumReceiveOperationSize)
	if err != nil {
		return ReceiveOperation{}, err
	}
	intentDigestRaw, err := cursor.frame(transfer.ReceiveIntentDigestBytes)
	if err != nil {
		return ReceiveOperation{}, err
	}
	bindingRaw, err := cursor.frame(sha256.Size)
	if err != nil {
		return ReceiveOperation{}, err
	}
	reopenDiscriminant, err := cursor.byte()
	if err != nil {
		return ReceiveOperation{}, err
	}
	reopen := ReopenKey{kind: ReopenKeyKind(reopenDiscriminant)}
	if reopen.kind == ReopenCLICompatible {
		raw, frameErr := cursor.frame(sha256.Size)
		if frameErr != nil {
			return ReceiveOperation{}, frameErr
		}
		reopen.key, err = CompatibleOperationKeyFromBytes(raw)
		if err != nil {
			return ReceiveOperation{}, err
		}
	}
	operation, operationErr := receivecontract.OperationIDFromBytes(operationRaw)
	intentDigest, intentErr := transfer.ReceiveIntentDigestFromBytes(intentDigestRaw)
	binding, bindingErr := receivecontract.BindingDigestFromBytes(bindingRaw)
	if cursor.offset != len(encoded) || operationErr != nil || intentErr != nil || bindingErr != nil {
		return ReceiveOperation{}, errors.Join(ErrInvalidReceiveOperation, operationErr, intentErr, bindingErr)
	}
	record := ReceiveOperation{
		operationID: operation, intentBytes: slices.Clone(intentBytes), intentDigest: intentDigest,
		bindingDigest: binding, reopenKey: reopen,
	}
	canonical, canonicalErr := EncodeReceiveOperation(record)
	if canonicalErr != nil || !bytes.Equal(canonical, encoded) {
		return ReceiveOperation{}, errors.Join(ErrInvalidReceiveOperation, canonicalErr)
	}
	return record, nil
}

type operationCursor struct {
	encoded []byte
	offset  int
}

func (cursor *operationCursor) byte() (byte, error) {
	if cursor.offset >= len(cursor.encoded) {
		return 0, ErrInvalidReceiveOperation
	}
	value := cursor.encoded[cursor.offset]
	cursor.offset++
	return value, nil
}

func (cursor *operationCursor) frame(maximum int) ([]byte, error) {
	if len(cursor.encoded)-cursor.offset < 8 {
		return nil, ErrInvalidReceiveOperation
	}
	length := binary.BigEndian.Uint64(cursor.encoded[cursor.offset : cursor.offset+8])
	cursor.offset += 8
	if length > uint64(maximum) || length > uint64(len(cursor.encoded)-cursor.offset) {
		return nil, fmt.Errorf("%w: framed field", ErrInvalidReceiveOperation)
	}
	value := cursor.encoded[cursor.offset : cursor.offset+int(length)]
	cursor.offset += int(length)
	return value, nil
}

func writeOperationFrame(writer interface{ Write([]byte) (int, error) }, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	admittedDirectoryVersion = uint8(1)
	admittedDirectoryDomain  = "windshare/admitted-directory/v1"
	maximumAdmittedDirectory = 64 * 1024
)

var ErrInvalidAdmittedDirectory = errors.New("admitted directory record is invalid")

type AdmittedDirectory struct {
	operationID     receivecontract.OperationID
	intentDigest    transfer.ReceiveIntentDigest
	layoutVersion   uint8
	layout          transfer.DirectoryAdmissionLayout
	admission       AggregateDigest
	parentAdmission AggregateDigest
	directoryID     catalog.DirectoryID
	generation      catalog.DirectoryGeneration
	canonicalPath   string
	modifiedTime    catalog.ModifiedTime
	ownedObjectID   transfer.OwnedObjectID
}

// NewAdmittedDirectory persists the immutable claim, not the HMAC capability.
// Hashing the runtime token preserves its parent-chain identity without making
// the restart repository a second authority capable of minting admissions.
func NewAdmittedDirectory(
	intent transfer.ReceiveIntent,
	admission transfer.DirectoryAdmission,
	ownedObjectID transfer.OwnedObjectID,
) (AdmittedDirectory, error) {
	if intent.IsZero() || admission.IsZero() || ownedObjectID.IsZero() ||
		admission.SchemaVersion() != transfer.DirectoryAdmissionV2 ||
		admission.LayoutVersion() != transfer.DirectoryAdmissionLayoutV1 ||
		admission.Layout() < transfer.DirectoryAdmissionTreeSingleFile ||
		admission.Layout() > transfer.DirectoryAdmissionTreeCatalogRoot ||
		admission.ReceiveIntentDigest() != intent.Digest() || admission.DirectoryID().IsZero() ||
		admission.Generation().IsZero() {
		return AdmittedDirectory{}, ErrInvalidAdmittedDirectory
	}
	path := admission.Path()
	parent := admission.ParentToken()
	if path == "" {
		if len(parent) != 0 {
			return AdmittedDirectory{}, ErrInvalidAdmittedDirectory
		}
	} else {
		canonical, err := catalog.CanonicalPath(path)
		if err != nil || canonical != path || len(parent) != sha256.Size {
			return AdmittedDirectory{}, ErrInvalidAdmittedDirectory
		}
	}
	record := AdmittedDirectory{
		operationID: intent.OperationID(), intentDigest: admission.ReceiveIntentDigest(),
		layoutVersion: admission.LayoutVersion(), layout: admission.Layout(),
		admission: aggregateDigest(admission.Bytes()), directoryID: admission.DirectoryID(),
		generation: admission.Generation(), canonicalPath: path,
		modifiedTime: admission.ModifiedTime(), ownedObjectID: ownedObjectID,
	}
	if len(parent) != 0 {
		record.parentAdmission = aggregateDigest(parent)
	}
	if !record.Valid() {
		return AdmittedDirectory{}, ErrInvalidAdmittedDirectory
	}
	return record, nil
}

func (record AdmittedDirectory) OperationID() receivecontract.OperationID { return record.operationID }
func (record AdmittedDirectory) ReceiveIntentDigest() transfer.ReceiveIntentDigest {
	return record.intentDigest
}
func (record AdmittedDirectory) LayoutVersion() uint8                      { return record.layoutVersion }
func (record AdmittedDirectory) Layout() transfer.DirectoryAdmissionLayout { return record.layout }
func (record AdmittedDirectory) AdmissionDigest() AggregateDigest          { return record.admission }
func (record AdmittedDirectory) ParentAdmissionDigest() AggregateDigest {
	return record.parentAdmission
}
func (record AdmittedDirectory) DirectoryID() catalog.DirectoryID        { return record.directoryID }
func (record AdmittedDirectory) Generation() catalog.DirectoryGeneration { return record.generation }
func (record AdmittedDirectory) CanonicalPath() string                   { return record.canonicalPath }
func (record AdmittedDirectory) ModifiedTime() catalog.ModifiedTime      { return record.modifiedTime }
func (record AdmittedDirectory) OwnedObjectID() transfer.OwnedObjectID   { return record.ownedObjectID }

func (record AdmittedDirectory) Valid() bool {
	if record.operationID.IsZero() || record.intentDigest.IsZero() || record.admission.IsZero() ||
		record.directoryID.IsZero() || record.generation.IsZero() || record.ownedObjectID.IsZero() ||
		record.layoutVersion != transfer.DirectoryAdmissionLayoutV1 ||
		record.layout < transfer.DirectoryAdmissionTreeSingleFile ||
		record.layout > transfer.DirectoryAdmissionTreeCatalogRoot || !validModifiedTime(record.modifiedTime) {
		return false
	}
	if record.canonicalPath == "" {
		return record.parentAdmission.IsZero()
	}
	canonical, err := catalog.CanonicalPath(record.canonicalPath)
	return err == nil && canonical == record.canonicalPath && !record.parentAdmission.IsZero()
}

func EncodeAdmittedDirectory(record AdmittedDirectory) ([]byte, error) {
	if !record.Valid() {
		return nil, ErrInvalidAdmittedDirectory
	}
	var encoded bytes.Buffer
	_, _ = encoded.WriteString(admittedDirectoryDomain)
	_ = encoded.WriteByte(0)
	_ = encoded.WriteByte(admittedDirectoryVersion)
	writeOperationFrame(&encoded, record.operationID.Bytes())
	writeOperationFrame(&encoded, record.intentDigest.Bytes())
	writeOperationFrame(&encoded, []byte{record.layoutVersion})
	writeOperationFrame(&encoded, []byte{byte(record.layout)})
	writeOperationFrame(&encoded, record.admission.Bytes())
	var parent []byte
	if !record.parentAdmission.IsZero() {
		parent = record.parentAdmission.Bytes()
	}
	writeOperationFrame(&encoded, parent)
	writeOperationFrame(&encoded, record.directoryID.Bytes())
	writeOperationFrame(&encoded, record.generation.Bytes())
	writeOperationFrame(&encoded, []byte(record.canonicalPath))
	writeOperationFrame(&encoded, encodeModifiedTime(record.modifiedTime))
	writeOperationFrame(&encoded, record.ownedObjectID.Bytes())
	return encoded.Bytes(), nil
}

func DecodeAdmittedDirectory(encoded []byte) (AdmittedDirectory, error) {
	if len(encoded) == 0 || len(encoded) > maximumAdmittedDirectory {
		return AdmittedDirectory{}, ErrInvalidAdmittedDirectory
	}
	prefix := append(append([]byte(nil), admittedDirectoryDomain...), 0, admittedDirectoryVersion)
	if !bytes.HasPrefix(encoded, prefix) {
		return AdmittedDirectory{}, ErrInvalidAdmittedDirectory
	}
	cursor := admittedDirectoryCursor{encoded: encoded, offset: len(prefix)}
	operationRaw, err := cursor.frame(receivecontract.StableIdentityBytes)
	if err != nil {
		return AdmittedDirectory{}, err
	}
	intentRaw, err := cursor.frame(transfer.ReceiveIntentDigestBytes)
	if err != nil {
		return AdmittedDirectory{}, err
	}
	layoutVersion, err := cursor.singleByte()
	if err != nil {
		return AdmittedDirectory{}, err
	}
	layout, err := cursor.singleByte()
	if err != nil {
		return AdmittedDirectory{}, err
	}
	admissionRaw, err := cursor.frame(sha256.Size)
	if err != nil {
		return AdmittedDirectory{}, err
	}
	parentRaw, err := cursor.frame(sha256.Size)
	if err != nil {
		return AdmittedDirectory{}, err
	}
	directoryRaw, err := cursor.frame(catalog.IdentityBytes)
	if err != nil {
		return AdmittedDirectory{}, err
	}
	generationRaw, err := cursor.frame(catalog.IdentityBytes)
	if err != nil {
		return AdmittedDirectory{}, err
	}
	pathRaw, err := cursor.frame(catalog.MaxPathBytes)
	if err != nil {
		return AdmittedDirectory{}, err
	}
	modifiedRaw, err := cursor.frame(14)
	if err != nil {
		return AdmittedDirectory{}, err
	}
	ownedRaw, err := cursor.frame(transfer.OwnedObjectIdentityBytes)
	if err != nil || cursor.offset != len(encoded) {
		return AdmittedDirectory{}, errors.Join(ErrInvalidAdmittedDirectory, err)
	}
	record, err := decodeAdmittedDirectoryValues(
		operationRaw, intentRaw, layoutVersion, layout, admissionRaw, parentRaw,
		directoryRaw, generationRaw, pathRaw, modifiedRaw, ownedRaw,
	)
	if err != nil {
		return AdmittedDirectory{}, err
	}
	canonical, err := EncodeAdmittedDirectory(record)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return AdmittedDirectory{}, errors.Join(ErrInvalidAdmittedDirectory, err)
	}
	return record, nil
}

func decodeAdmittedDirectoryValues(
	operationRaw, intentRaw []byte,
	layoutVersion, layout byte,
	admissionRaw, parentRaw, directoryRaw, generationRaw, pathRaw, modifiedRaw, ownedRaw []byte,
) (AdmittedDirectory, error) {
	operation, operationErr := receivecontract.OperationIDFromBytes(operationRaw)
	intent, intentErr := transfer.ReceiveIntentDigestFromBytes(intentRaw)
	admission, admissionErr := AggregateDigestFromBytes(admissionRaw)
	var parent AggregateDigest
	var parentErr error
	if len(parentRaw) != 0 {
		parent, parentErr = AggregateDigestFromBytes(parentRaw)
	}
	directory, directoryErr := catalog.DirectoryIDFromBytes(directoryRaw)
	generation, generationErr := catalog.DirectoryGenerationFromBytes(generationRaw)
	modified, modifiedErr := decodeModifiedTime(modifiedRaw)
	owned, ownedErr := transfer.OwnedObjectIDFromBytes(ownedRaw)
	if err := errors.Join(
		operationErr, intentErr, admissionErr, parentErr, directoryErr, generationErr, modifiedErr, ownedErr,
	); err != nil {
		return AdmittedDirectory{}, errors.Join(ErrInvalidAdmittedDirectory, err)
	}
	record := AdmittedDirectory{
		operationID: operation, intentDigest: intent, layoutVersion: layoutVersion,
		layout: transfer.DirectoryAdmissionLayout(layout), admission: admission,
		parentAdmission: parent, directoryID: directory, generation: generation,
		canonicalPath: string(pathRaw), modifiedTime: modified, ownedObjectID: owned,
	}
	if !record.Valid() {
		return AdmittedDirectory{}, ErrInvalidAdmittedDirectory
	}
	return record, nil
}

func aggregateDigest(value []byte) AggregateDigest {
	return AggregateDigest(sha256.Sum256(value))
}

func validModifiedTime(modified catalog.ModifiedTime) bool {
	if !modified.Present() {
		return modified == (catalog.ModifiedTime{})
	}
	restored, err := catalog.NewModifiedTime(modified.Seconds(), modified.Nanoseconds(), modified.Precision())
	return err == nil && restored == modified
}

func encodeModifiedTime(modified catalog.ModifiedTime) []byte {
	if !modified.Present() {
		return []byte{0}
	}
	encoded := make([]byte, 14)
	encoded[0] = 1
	binary.BigEndian.PutUint64(encoded[1:9], uint64(modified.Seconds()))
	binary.BigEndian.PutUint32(encoded[9:13], modified.Nanoseconds())
	encoded[13] = byte(modified.Precision())
	return encoded
}

func decodeModifiedTime(encoded []byte) (catalog.ModifiedTime, error) {
	if len(encoded) == 1 && encoded[0] == 0 {
		return catalog.ModifiedTime{}, nil
	}
	if len(encoded) != 14 || encoded[0] != 1 {
		return catalog.ModifiedTime{}, ErrInvalidAdmittedDirectory
	}
	modified, err := catalog.NewModifiedTime(
		int64(binary.BigEndian.Uint64(encoded[1:9])), binary.BigEndian.Uint32(encoded[9:13]),
		catalog.TimePrecision(encoded[13]),
	)
	if err != nil {
		return catalog.ModifiedTime{}, errors.Join(ErrInvalidAdmittedDirectory, err)
	}
	return modified, nil
}

type admittedDirectoryCursor struct {
	encoded []byte
	offset  int
}

func (cursor *admittedDirectoryCursor) frame(maximum int) ([]byte, error) {
	if len(cursor.encoded)-cursor.offset < 8 {
		return nil, ErrInvalidAdmittedDirectory
	}
	length := binary.BigEndian.Uint64(cursor.encoded[cursor.offset : cursor.offset+8])
	cursor.offset += 8
	if length > uint64(maximum) || length > uint64(len(cursor.encoded)-cursor.offset) {
		return nil, fmt.Errorf("%w: framed field", ErrInvalidAdmittedDirectory)
	}
	value := slices.Clone(cursor.encoded[cursor.offset : cursor.offset+int(length)])
	cursor.offset += int(length)
	return value, nil
}

func (cursor *admittedDirectoryCursor) singleByte() (byte, error) {
	value, err := cursor.frame(1)
	if err != nil || len(value) != 1 {
		return 0, errors.Join(ErrInvalidAdmittedDirectory, err)
	}
	return value[0], nil
}

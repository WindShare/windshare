package resumestate

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

const (
	MaxSessionHeaderBytes                 = 64 << 10
	MaxControlStateBytes                  = 64 << 10
	MaxFileStateBytes                     = 1 << 20
	MaxStateNestingDepth                  = 16
	recordLengthBytes                     = 4
	recordChecksumBytes                   = sha256.Size
	storedDurabilityProcessRestart  uint8 = 1
	storedDurabilityPowerLoss       uint8 = 2
	storedTimePrecisionSeconds      uint8 = 1
	storedTimePrecisionMilliseconds uint8 = 2
	storedTimePrecisionNanoseconds  uint8 = 3
)

var (
	headerMagic  = [8]byte{'W', 'S', 'O', 'H', 'D', 'R', '0', '3'}
	controlMagic = [8]byte{'W', 'S', 'O', 'C', 'T', 'L', '0', '3'}
	fileMagic    = [8]byte{'W', 'S', 'O', 'F', 'I', 'L', '0', '3'}

	stateEnc = func() cbor.EncMode {
		options := cbor.CoreDetEncOptions()
		options.NilContainers = cbor.NilContainerAsEmpty
		mode, err := options.EncMode()
		if err != nil {
			panic(err)
		}
		return mode
	}()
	stateDec = func() cbor.DecMode {
		mode, err := cbor.DecOptions{
			DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden,
			TagsMd: cbor.TagsForbidden, ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
			FieldNameMatching: cbor.FieldNameMatchingCaseSensitive, MaxNestedLevels: MaxStateNestingDepth,
			MaxArrayElements: MaxDurableRangesPerFile, MaxMapPairs: 32,
		}.DecMode()
		if err != nil {
			panic(err)
		}
		return mode
	}()
)

type storedHeader struct {
	Schema                  uint32 `cbor:"0,keyasint"`
	Backend                 string `cbor:"1,keyasint"`
	SessionID               []byte `cbor:"2,keyasint"`
	ShareInstance           []byte `cbor:"3,keyasint"`
	SyntheticRoot           []byte `cbor:"4,keyasint"`
	ResumeIntent            []byte `cbor:"5,keyasint"`
	SelectionIdentity       []byte `cbor:"6,keyasint"`
	SelectedDirectoryCount  uint32 `cbor:"7,keyasint"`
	SelectedFileCount       uint32 `cbor:"8,keyasint"`
	OutputRoot              []byte `cbor:"9,keyasint"`
	Lifecycle               uint8  `cbor:"10,keyasint"`
	StateGeneration         uint64 `cbor:"11,keyasint"`
	OutputRootCertification string `cbor:"12,keyasint"`
	OutputAncestry          []byte `cbor:"13,keyasint"`
}

type storedControl struct {
	Schema        uint32 `cbor:"0,keyasint"`
	Backend       string `cbor:"1,keyasint"`
	OutputRoot    []byte `cbor:"2,keyasint"`
	Certification string `cbor:"3,keyasint"`
	Durability    uint8  `cbor:"4,keyasint"`
	Generation    uint64 `cbor:"5,keyasint"`
}

type storedModifiedTime struct {
	Present     bool   `cbor:"0,keyasint"`
	Seconds     int64  `cbor:"1,keyasint"`
	Nanoseconds uint32 `cbor:"2,keyasint"`
	Precision   uint8  `cbor:"3,keyasint"`
}

type storedFileRecord struct {
	Schema                uint32             `cbor:"0,keyasint"`
	SessionID             []byte             `cbor:"1,keyasint"`
	ShareInstance         []byte             `cbor:"2,keyasint"`
	FileID                []byte             `cbor:"3,keyasint"`
	Revision              []byte             `cbor:"4,keyasint"`
	CanonicalLocator      string             `cbor:"5,keyasint"`
	LocatorDigest         []byte             `cbor:"6,keyasint"`
	OutputObject          []byte             `cbor:"7,keyasint"`
	ExactSize             uint64             `cbor:"8,keyasint"`
	StateGeneration       uint64             `cbor:"9,keyasint"`
	CheckpointGeneration  uint64             `cbor:"10,keyasint"`
	DurableRanges         [][2]uint64        `cbor:"11,keyasint"`
	Phase                 uint8              `cbor:"12,keyasint"`
	QuarantineReason      uint8              `cbor:"13,keyasint"`
	PhaseBeforeQuarantine uint8              `cbor:"14,keyasint"`
	ModifiedTime          storedModifiedTime `cbor:"15,keyasint"`
	RetirementReason      uint8              `cbor:"16,keyasint"`
	ChunkSize             uint32             `cbor:"17,keyasint"`
}

func EncodeControl(control Control) ([]byte, error) {
	if !control.valid() {
		return nil, fmt.Errorf("%w: global control record", ErrInvalidState)
	}
	durability, err := encodeStoredDurability(control.durability)
	if err != nil {
		return nil, err
	}
	return encodeEnvelope(controlMagic, storedControl{
		Schema: SchemaVersion, Backend: string(control.backend), OutputRoot: control.outputRoot.Bytes(),
		Certification: string(control.certification), Durability: durability, Generation: control.generation,
	}, MaxControlStateBytes)
}

func ReadControl(reader io.Reader) (Control, error) {
	encoded, err := readBoundedRecord(reader, MaxControlStateBytes)
	if err != nil {
		return Control{}, err
	}
	return DecodeControl(encoded)
}

func DecodeControl(encoded []byte) (Control, error) {
	var stored storedControl
	if err := decodeEnvelope(encoded, controlMagic, MaxControlStateBytes, &stored); err != nil {
		return Control{}, err
	}
	if stored.Schema != SchemaVersion {
		return Control{}, corrupt("unsupported global control schema", nil)
	}
	backend, backendErr := transfer.NewOutputBackendID(stored.Backend)
	certification, certificationErr := NewCertificationID(stored.Certification)
	root, rootErr := outputRootBindingFromBytes(certification, stored.OutputRoot)
	if backendErr != nil || rootErr != nil || certificationErr != nil {
		return Control{}, corrupt("invalid global control identity", nil)
	}
	durability, durabilityErr := decodeStoredDurability(stored.Durability)
	if durabilityErr != nil {
		return Control{}, durabilityErr
	}
	control, err := NewControl(ControlSpec{
		Backend: backend, OutputRoot: root, Certification: certification,
		Durability: durability, Generation: stored.Generation,
	})
	if err != nil {
		return Control{}, corrupt("invalid global control binding", err)
	}
	return control, nil
}

func EncodeHeader(header Header) ([]byte, error) {
	if !header.valid() {
		return nil, fmt.Errorf("%w: session header", ErrInvalidState)
	}
	return encodeEnvelope(headerMagic, storeHeader(header), MaxSessionHeaderBytes)
}

func ReadHeader(reader io.Reader) (Header, error) {
	encoded, err := readBoundedRecord(reader, MaxSessionHeaderBytes)
	if err != nil {
		return Header{}, err
	}
	return DecodeHeader(encoded)
}

func DecodeHeader(encoded []byte) (Header, error) {
	var stored storedHeader
	if err := decodeEnvelope(encoded, headerMagic, MaxSessionHeaderBytes, &stored); err != nil {
		return Header{}, err
	}
	return restoreHeader(stored)
}

func storeHeader(header Header) storedHeader {
	return storedHeader{
		Schema: SchemaVersion, Backend: string(header.backend), SessionID: header.sessionID.Bytes(),
		ShareInstance: header.shareInstance.Bytes(), SyntheticRoot: header.syntheticRoot.Bytes(),
		ResumeIntent: header.resumeIntent.Bytes(), SelectionIdentity: header.selectionIdentity.Bytes(),
		SelectedDirectoryCount: header.selectedDirectoryCount, SelectedFileCount: header.selectedFileCount,
		OutputRoot: header.outputRoot.Bytes(), Lifecycle: uint8(header.lifecycle), StateGeneration: header.stateGeneration,
		OutputRootCertification: string(header.outputRoot.Certification()),
		OutputAncestry:          header.outputAncestry.Bytes(),
	}
}

func restoreHeader(stored storedHeader) (Header, error) {
	if stored.Schema != SchemaVersion {
		return Header{}, corrupt("unsupported session header schema", nil)
	}
	backend, backendErr := transfer.NewOutputBackendID(stored.Backend)
	session, sessionErr := transfer.OutputSessionIDFromBytes(stored.SessionID)
	share, shareErr := catalog.ShareInstanceFromBytes(stored.ShareInstance)
	syntheticRoot, syntheticRootErr := catalog.DirectoryIDFromBytes(stored.SyntheticRoot)
	intent, intentErr := transfer.ResumeIntentFromBytes(stored.ResumeIntent)
	selection, selectionErr := transfer.SelectionIdentityFromBytes(stored.SelectionIdentity)
	certification, certificationErr := NewCertificationID(stored.OutputRootCertification)
	root, rootErr := outputRootBindingFromBytes(certification, stored.OutputRoot)
	ancestry, ancestryErr := outputAncestryBindingFromBytes(stored.OutputAncestry)
	if backendErr != nil || sessionErr != nil || shareErr != nil || syntheticRootErr != nil ||
		intentErr != nil || selectionErr != nil || certificationErr != nil || rootErr != nil ||
		ancestryErr != nil ||
		share.IsZero() || syntheticRoot.IsZero() {
		return Header{}, corrupt("invalid session header identity", nil)
	}
	header, err := newHeaderFromClaims(headerClaims{
		backend: backend, sessionID: session, shareInstance: share, syntheticRoot: syntheticRoot,
		resumeIntent: intent, selectionIdentity: selection,
		selectedDirectoryCount: stored.SelectedDirectoryCount, selectedFileCount: stored.SelectedFileCount,
		outputRoot: root, outputAncestry: ancestry,
		lifecycle: SessionLifecycle(stored.Lifecycle), stateGeneration: stored.StateGeneration,
	})
	if err != nil {
		return Header{}, corrupt("invalid session header binding", err)
	}
	return header, nil
}

func EncodeFileRecord(bound BoundFileRecord) ([]byte, error) {
	if !bound.valid() {
		return nil, fmt.Errorf("%w: file record", ErrInvalidState)
	}
	record := bound.record
	ranges := make([][2]uint64, 0, record.durableRanges.Len())
	for _, current := range record.durableRanges.Ranges() {
		ranges = append(ranges, [2]uint64{current.Offset, current.End})
	}
	stored := storedFileRecord{
		Schema: SchemaVersion, SessionID: record.sessionID.Bytes(), ShareInstance: record.shareInstance.Bytes(),
		FileID: record.fileID.Bytes(), Revision: record.revision.Bytes(), CanonicalLocator: record.canonicalLocator,
		LocatorDigest: append([]byte(nil), record.locatorDigest[:]...), OutputObject: record.outputObject.Bytes(),
		ExactSize: record.exactSize, StateGeneration: record.stateGeneration,
		ChunkSize:            record.chunkSize,
		CheckpointGeneration: record.checkpointGeneration, DurableRanges: ranges,
		Phase: uint8(record.phase), QuarantineReason: uint8(record.quarantineReason),
		PhaseBeforeQuarantine: uint8(record.phaseBeforeQuarantine),
		ModifiedTime:          storeModifiedTime(record.expectedMetadata.ModifiedTime),
		RetirementReason:      uint8(record.retirementReason),
	}
	return encodeEnvelope(fileMagic, stored, MaxFileStateBytes)
}

func ReadFileRecord(reader io.Reader) (FileRecord, error) {
	encoded, err := readBoundedRecord(reader, MaxFileStateBytes)
	if err != nil {
		return FileRecord{}, err
	}
	return DecodeFileRecord(encoded)
}

func DecodeFileRecord(encoded []byte) (FileRecord, error) {
	var stored storedFileRecord
	if err := decodeEnvelope(encoded, fileMagic, MaxFileStateBytes, &stored); err != nil {
		return FileRecord{}, err
	}
	if stored.Schema != SchemaVersion || len(stored.LocatorDigest) != sha256.Size || stored.DurableRanges == nil {
		return FileRecord{}, corrupt("invalid file record shape", nil)
	}
	session, sessionErr := transfer.OutputSessionIDFromBytes(stored.SessionID)
	share, shareErr := catalog.ShareInstanceFromBytes(stored.ShareInstance)
	file, fileErr := catalog.FileIDFromBytes(stored.FileID)
	revision, revisionErr := content.FileRevisionFromBytes(stored.Revision)
	object, objectErr := OutputObjectIDFromBytes(stored.OutputObject)
	if sessionErr != nil || shareErr != nil || fileErr != nil || revisionErr != nil || objectErr != nil ||
		share.IsZero() || file.IsZero() || revision.IsZero() {
		return FileRecord{}, corrupt("invalid file record identity", nil)
	}
	ranges := make([]content.Range, len(stored.DurableRanges))
	for index, pair := range stored.DurableRanges {
		ranges[index] = content.Range{Offset: pair[0], End: pair[1]}
	}
	durable, rangesErr := content.NewRangeSet(ranges)
	if rangesErr != nil {
		return FileRecord{}, corrupt("invalid file record ranges", rangesErr)
	}
	modified, modifiedErr := restoreModifiedTime(stored.ModifiedTime)
	if modifiedErr != nil {
		return FileRecord{}, modifiedErr
	}
	record, err := newFileRecordFromClaims(fileRecordClaims{
		sessionID: session, shareInstance: share, fileID: file, revision: revision,
		canonicalLocator: stored.CanonicalLocator, outputObject: object, exactSize: stored.ExactSize,
		chunkSize:       stored.ChunkSize,
		stateGeneration: stored.StateGeneration, checkpointGeneration: stored.CheckpointGeneration,
		durableRanges: durable, phase: FilePhase(stored.Phase),
		quarantineReason:      QuarantineReason(stored.QuarantineReason),
		phaseBeforeQuarantine: FilePhase(stored.PhaseBeforeQuarantine),
		expectedMetadata:      ExpectedMetadata{ModifiedTime: modified},
		retirementReason:      RetirementReason(stored.RetirementReason),
	})
	if err != nil || !bytes.Equal(stored.LocatorDigest, record.locatorDigest[:]) {
		return FileRecord{}, corrupt("invalid file record binding", err)
	}
	return record, nil
}

func encodeEnvelope(magic [8]byte, value any, limit int) ([]byte, error) {
	payload, err := stateEnc.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode resumestate record: %w", err)
	}
	if uint64(len(payload)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("%w: record payload length", ErrInvalidState)
	}
	encoded := make([]byte, 0, len(magic)+recordLengthBytes+len(payload)+recordChecksumBytes)
	encoded = append(encoded, magic[:]...)
	var payloadLength [recordLengthBytes]byte
	binary.BigEndian.PutUint32(payloadLength[:], uint32(len(payload)))
	encoded = append(encoded, payloadLength[:]...)
	encoded = append(encoded, payload...)
	checksum := sha256.Sum256(encoded)
	encoded = append(encoded, checksum[:]...)
	if len(encoded) > limit {
		return nil, fmt.Errorf("%w: record exceeds %d bytes", ErrInvalidState, limit)
	}
	return encoded, nil
}

func decodeEnvelope(encoded []byte, magic [8]byte, limit int, target any) error {
	minimum := len(magic) + recordLengthBytes + recordChecksumBytes + 1
	if len(encoded) < minimum || len(encoded) > limit || !bytes.Equal(encoded[:len(magic)], magic[:]) {
		return corrupt("invalid record envelope", nil)
	}
	payloadEnd := len(encoded) - recordChecksumBytes
	declaredLength := binary.BigEndian.Uint32(encoded[len(magic) : len(magic)+recordLengthBytes])
	actualLength := payloadEnd - len(magic) - recordLengthBytes
	if uint64(declaredLength) != uint64(actualLength) {
		return corrupt("record payload length mismatch", nil)
	}
	checksum := sha256.Sum256(encoded[:payloadEnd])
	if !bytes.Equal(checksum[:], encoded[payloadEnd:]) {
		return corrupt("record checksum mismatch", nil)
	}
	payload := encoded[len(magic)+recordLengthBytes : payloadEnd]
	if err := stateDec.Unmarshal(payload, target); err != nil {
		return corrupt("invalid record payload", err)
	}
	canonical, err := stateEnc.Marshal(target)
	if err != nil || !bytes.Equal(canonical, payload) {
		return corrupt("non-canonical record payload", err)
	}
	return nil
}

func readBoundedRecord(reader io.Reader, limit int) ([]byte, error) {
	if reader == nil || limit <= 0 {
		return nil, fmt.Errorf("%w: record reader", ErrInvalidState)
	}
	limited := &io.LimitedReader{R: reader, N: int64(limit) + 1}
	encoded, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read resumestate record: %w", err)
	}
	if len(encoded) > limit {
		return nil, corrupt("record exceeds bounded reader limit", nil)
	}
	return encoded, nil
}

func corrupt(message string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrCorruptState, message)
	}
	return fmt.Errorf("%w: %s: %w", ErrCorruptState, message, cause)
}

func storeModifiedTime(modified catalog.ModifiedTime) storedModifiedTime {
	precision := uint8(0)
	switch modified.Precision() {
	case catalog.TimePrecisionSeconds:
		precision = storedTimePrecisionSeconds
	case catalog.TimePrecisionMilliseconds:
		precision = storedTimePrecisionMilliseconds
	case catalog.TimePrecisionNanoseconds:
		precision = storedTimePrecisionNanoseconds
	}
	return storedModifiedTime{
		Present: modified.Present(), Seconds: modified.Seconds(), Nanoseconds: modified.Nanoseconds(),
		Precision: precision,
	}
}

func restoreModifiedTime(stored storedModifiedTime) (catalog.ModifiedTime, error) {
	if !stored.Present {
		if stored.Seconds != 0 || stored.Nanoseconds != 0 || stored.Precision != 0 {
			return catalog.ModifiedTime{}, corrupt("absent modified time carries values", nil)
		}
		return catalog.ModifiedTime{}, nil
	}
	precision, precisionErr := decodeStoredTimePrecision(stored.Precision)
	if precisionErr != nil {
		return catalog.ModifiedTime{}, precisionErr
	}
	modified, err := catalog.NewModifiedTime(stored.Seconds, stored.Nanoseconds, precision)
	if err != nil {
		return catalog.ModifiedTime{}, corrupt("invalid modified time", err)
	}
	return modified, nil
}

func encodeStoredDurability(durability transfer.DurabilityLevel) (uint8, error) {
	switch durability {
	case transfer.DurabilityProcessRestart:
		return storedDurabilityProcessRestart, nil
	case transfer.DurabilityPowerLoss:
		return storedDurabilityPowerLoss, nil
	default:
		return 0, fmt.Errorf("%w: durability has no schema code", ErrInvalidState)
	}
}

func decodeStoredDurability(stored uint8) (transfer.DurabilityLevel, error) {
	switch stored {
	case storedDurabilityProcessRestart:
		return transfer.DurabilityProcessRestart, nil
	case storedDurabilityPowerLoss:
		return transfer.DurabilityPowerLoss, nil
	default:
		return 0, corrupt("unknown durability code", nil)
	}
}

func decodeStoredTimePrecision(stored uint8) (catalog.TimePrecision, error) {
	switch stored {
	case storedTimePrecisionSeconds:
		return catalog.TimePrecisionSeconds, nil
	case storedTimePrecisionMilliseconds:
		return catalog.TimePrecisionMilliseconds, nil
	case storedTimePrecisionNanoseconds:
		return catalog.TimePrecisionNanoseconds, nil
	default:
		return 0, corrupt("unknown modified-time precision code", nil)
	}
}

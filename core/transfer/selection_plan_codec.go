package transfer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/windshare/windshare/core/catalog"
)

func encodeSelectionPlanRecord(record selectionPlanRecord) ([]byte, error) {
	if !validSelectionPath(record.path) || (record.kind != selectionPlanDirectoryKind && record.kind != selectionPlanFileKind) {
		return nil, ErrSelectionPlanState
	}
	path := []byte(record.path)
	payload := make(
		[]byte, 0,
		selectionPlanRecordHeaderBytes+len(path)+3*catalog.IdentityBytes+
			selectionExpectedSizeBytes+selectionModifiedTimeBytes,
	)
	payload = append(payload, record.kind)
	active := byte(0)
	if record.active {
		active = 1
	}
	payload = append(payload, active)
	payload = binary.BigEndian.AppendUint32(payload, uint32(len(path)))
	payload = append(payload, path...)
	switch record.kind {
	case selectionPlanDirectoryKind:
		if record.directory.directory.IsZero() || record.directory.generation.IsZero() {
			return nil, ErrSelectionPlanState
		}
		payload = append(payload, record.directory.directory.Bytes()...)
		payload = append(payload, record.directory.generation.Bytes()...)
		payload = appendSelectionModifiedTime(payload, record.directory.modified)
	case selectionPlanFileKind:
		if record.file.file.IsZero() || record.file.parentDirectory.IsZero() || record.file.parentGeneration.IsZero() ||
			record.file.expectedSize > catalog.MaxFileSize {
			return nil, ErrSelectionPlanState
		}
		payload = append(payload, record.file.file.Bytes()...)
		payload = append(payload, record.file.parentDirectory.Bytes()...)
		payload = append(payload, record.file.parentGeneration.Bytes()...)
		payload = binary.BigEndian.AppendUint64(payload, record.file.expectedSize)
		payload = appendSelectionModifiedTime(payload, record.file.modified)
	}
	return payload, nil
}

func appendSelectionModifiedTime(destination []byte, modified catalog.ModifiedTime) []byte {
	present := byte(0)
	if modified.Present() {
		present = 1
	}
	destination = append(destination, present)
	destination = binary.BigEndian.AppendUint64(destination, uint64(modified.Seconds()))
	destination = binary.BigEndian.AppendUint32(destination, modified.Nanoseconds())
	return append(destination, byte(modified.Precision()))
}

func decodeSelectionPlanRecord(payload []byte) (selectionPlanRecord, error) {
	record, remaining, err := decodeSelectionPlanRecordHeader(payload)
	if err != nil {
		return selectionPlanRecord{}, err
	}
	switch record.kind {
	case selectionPlanDirectoryKind:
		return decodeSelectionPlanDirectory(record, remaining)
	case selectionPlanFileKind:
		return decodeSelectionPlanFile(record, remaining)
	default:
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
}

func decodeSelectionPlanRecordHeader(
	payload []byte,
) (selectionPlanRecord, []byte, error) {
	if len(payload) < selectionPlanRecordHeaderBytes {
		return selectionPlanRecord{}, nil, ErrSelectionPlanState
	}
	active := payload[selectionPlanActiveFieldOffset]
	if active > 1 {
		return selectionPlanRecord{}, nil, ErrSelectionPlanState
	}
	pathBytes := int(binary.BigEndian.Uint32(
		payload[selectionPlanPathLengthOffset:selectionPlanRecordHeaderBytes],
	))
	if pathBytes <= 0 || pathBytes > len(payload)-selectionPlanRecordHeaderBytes {
		return selectionPlanRecord{}, nil, ErrSelectionPlanState
	}
	pathEnd := selectionPlanRecordHeaderBytes + pathBytes
	record := selectionPlanRecord{
		kind: payload[selectionPlanKindOffset], active: active == 1,
		path: string(payload[selectionPlanRecordHeaderBytes:pathEnd]),
	}
	if !validSelectionPath(record.path) {
		return selectionPlanRecord{}, nil, ErrSelectionPlanState
	}
	return record, payload[pathEnd:], nil
}

func decodeSelectionPlanDirectory(
	record selectionPlanRecord,
	encoded []byte,
) (selectionPlanRecord, error) {
	modifiedOffset := 2 * catalog.IdentityBytes
	if len(encoded) != modifiedOffset+selectionModifiedTimeBytes {
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	directory, err := catalog.DirectoryIDFromBytes(encoded[:catalog.IdentityBytes])
	if err != nil {
		return selectionPlanRecord{}, err
	}
	generation, err := catalog.DirectoryGenerationFromBytes(
		encoded[catalog.IdentityBytes:modifiedOffset],
	)
	if err != nil {
		return selectionPlanRecord{}, err
	}
	if directory.IsZero() || generation.IsZero() {
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	modified, err := decodeSelectionModifiedTime(encoded[modifiedOffset:])
	if err != nil {
		return selectionPlanRecord{}, err
	}
	record.directory = plannedDirectory{
		directory: directory, generation: generation, path: record.path, modified: modified,
	}
	return record, nil
}

func decodeSelectionPlanFile(
	record selectionPlanRecord,
	encoded []byte,
) (selectionPlanRecord, error) {
	expectedSizeOffset := 3 * catalog.IdentityBytes
	modifiedOffset := expectedSizeOffset + selectionExpectedSizeBytes
	if !record.active || len(encoded) != modifiedOffset+selectionModifiedTimeBytes {
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	file, err := catalog.FileIDFromBytes(encoded[:catalog.IdentityBytes])
	if err != nil {
		return selectionPlanRecord{}, err
	}
	parent, err := catalog.DirectoryIDFromBytes(
		encoded[catalog.IdentityBytes : 2*catalog.IdentityBytes],
	)
	if err != nil {
		return selectionPlanRecord{}, err
	}
	generation, err := catalog.DirectoryGenerationFromBytes(
		encoded[2*catalog.IdentityBytes : expectedSizeOffset],
	)
	if err != nil {
		return selectionPlanRecord{}, err
	}
	expectedSize := binary.BigEndian.Uint64(encoded[expectedSizeOffset:modifiedOffset])
	if file.IsZero() || parent.IsZero() || generation.IsZero() || expectedSize > catalog.MaxFileSize {
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	modified, err := decodeSelectionModifiedTime(encoded[modifiedOffset:])
	if err != nil {
		return selectionPlanRecord{}, err
	}
	record.file = plannedFile{
		file: file, path: record.path, expectedSize: expectedSize, modified: modified,
		parentDirectory: parent, parentGeneration: generation,
	}
	return record, nil
}

func decodeSelectionModifiedTime(encoded []byte) (catalog.ModifiedTime, error) {
	if len(encoded) != selectionModifiedTimeBytes || encoded[0] > 1 {
		return catalog.ModifiedTime{}, ErrSelectionPlanState
	}
	if encoded[0] == 0 {
		if !bytes.Equal(
			encoded[selectionModifiedSecondsOffset:],
			make([]byte, selectionModifiedTimeBytes-selectionModifiedSecondsOffset),
		) {
			return catalog.ModifiedTime{}, ErrSelectionPlanState
		}
		return catalog.ModifiedTime{}, nil
	}
	return catalog.NewModifiedTime(
		int64(binary.BigEndian.Uint64(
			encoded[selectionModifiedSecondsOffset:selectionModifiedNanosecondsOffset],
		)),
		binary.BigEndian.Uint32(
			encoded[selectionModifiedNanosecondsOffset:selectionModifiedPrecisionOffset],
		),
		catalog.TimePrecision(encoded[selectionModifiedPrecisionOffset]),
	)
}

func writeSelectionPlanFrame(writer io.Writer, payload []byte) error {
	if len(payload) == 0 || len(payload) > maximumSelectionRecordBytes {
		return ErrSelectionPlanState
	}
	var length [selectionPlanLengthBytes]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	if _, err := writer.Write(length[:]); err != nil {
		return fmt.Errorf("write selection plan frame length: %w", err)
	}
	if _, err := writer.Write(payload); err != nil {
		return fmt.Errorf("write selection plan frame: %w", err)
	}
	if _, err := writer.Write(length[:]); err != nil {
		return fmt.Errorf("write selection plan frame suffix: %w", err)
	}
	return nil
}

func readSelectionPlanFrame(reader io.Reader) (selectionPlanRecord, error) {
	var length [selectionPlanLengthBytes]byte
	if _, err := io.ReadFull(reader, length[:]); err != nil {
		return selectionPlanRecord{}, err
	}
	size := binary.BigEndian.Uint32(length[:])
	if size == 0 || size > maximumSelectionRecordBytes {
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return selectionPlanRecord{}, fmt.Errorf("read selection plan frame: %w", err)
	}
	var suffix [selectionPlanLengthBytes]byte
	if _, err := io.ReadFull(reader, suffix[:]); err != nil || suffix != length {
		return selectionPlanRecord{}, ErrSelectionPlanState
	}
	return decodeSelectionPlanRecord(payload)
}

func readSelectionPlanFrameAt(file *os.File, end int64) (selectionPlanRecord, int64, error) {
	if end < selectionPlanFrameBytes {
		return selectionPlanRecord{}, 0, ErrSelectionPlanState
	}
	var suffix [selectionPlanLengthBytes]byte
	if _, err := file.ReadAt(suffix[:], end-selectionPlanLengthBytes); err != nil {
		return selectionPlanRecord{}, 0, err
	}
	size := int64(binary.BigEndian.Uint32(suffix[:]))
	start := end - size - selectionPlanFrameBytes
	if size <= 0 || size > maximumSelectionRecordBytes || start < 0 {
		return selectionPlanRecord{}, 0, ErrSelectionPlanState
	}
	payload := make([]byte, size)
	var prefix [selectionPlanLengthBytes]byte
	if _, err := file.ReadAt(prefix[:], start); err != nil || prefix != suffix {
		return selectionPlanRecord{}, 0, ErrSelectionPlanState
	}
	if _, err := file.ReadAt(payload, start+selectionPlanLengthBytes); err != nil {
		return selectionPlanRecord{}, 0, err
	}
	record, err := decodeSelectionPlanRecord(payload)
	return record, start, err
}

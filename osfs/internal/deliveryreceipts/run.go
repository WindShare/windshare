package deliveryreceipts

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/fileexecution"
)

const (
	recordBytes       = sha256.Size*2 + transfer.OwnedObjectIdentityBytes
	runBufferBytes    = 32 * 1024
	maximumRunRecords = math.MaxInt64 / recordBytes
)

type run struct {
	file        scratchFile
	count       int64
	first, last transfer.MaterializationLocatorDigest
}

func (current *run) lookup(key transfer.MaterializationLocatorDigest) (fileexecution.DeliveryReceipt, bool, error) {
	if bytes.Compare(key[:], current.first[:]) < 0 || bytes.Compare(key[:], current.last[:]) > 0 {
		return fileexecution.DeliveryReceipt{}, false, nil
	}
	for low, high := int64(0), current.count; low < high; {
		middle := low + (high-low)/2
		var encoded [recordBytes]byte
		if _, err := current.file.ReadAt(encoded[:], middle*recordBytes); err != nil {
			return fileexecution.DeliveryReceipt{}, false, err
		}
		value := decodeEntry(encoded)
		switch compared := bytes.Compare(value.key[:], key[:]); {
		case compared < 0:
			low = middle + 1
		case compared > 0:
			high = middle
		default:
			return value.receipt, true, nil
		}
	}
	return fileexecution.DeliveryReceipt{}, false, nil
}

func encodeEntry(value entry) (encoded [recordBytes]byte) {
	copy(encoded[:sha256.Size], value.key[:])
	copy(encoded[sha256.Size:sha256.Size*2], value.receipt.DescriptorDigest[:])
	copy(encoded[sha256.Size*2:], value.receipt.Object[:])
	return
}

func decodeEntry(encoded [recordBytes]byte) (value entry) {
	copy(value.key[:], encoded[:sha256.Size])
	copy(value.receipt.DescriptorDigest[:], encoded[sha256.Size:sha256.Size*2])
	copy(value.receipt.Object[:], encoded[sha256.Size*2:])
	return
}

type runBuilder struct {
	run
	writer *bufio.Writer
}

func buildRun(create func() (scratchFile, error), write func(*runBuilder) error) (*run, error) {
	file, err := create()
	if err != nil {
		return nil, err
	}
	builder := &runBuilder{run: run{file: file}, writer: bufio.NewWriterSize(file, runBufferBytes)}
	if err := write(builder); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := builder.writer.Flush(); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &builder.run, nil
}

func (builder *runBuilder) append(value entry) error {
	if builder.count == maximumRunRecords {
		return errors.New("live delivery receipt scratch storage is full")
	}
	if builder.count == 0 {
		builder.first = value.key
	}
	builder.last = value.key
	encoded := encodeEntry(value)
	if _, err := builder.writer.Write(encoded[:]); err != nil {
		return err
	}
	builder.count++
	return nil
}

func writeRun(create func() (scratchFile, error), entries []entry) (*run, error) {
	return buildRun(create, func(builder *runBuilder) error {
		for _, value := range entries {
			if err := builder.append(value); err != nil {
				return err
			}
		}
		return nil
	})
}

type runCursor struct {
	reader    *bufio.Reader
	remaining int64
	value     entry
}

func cursor(current *run) *runCursor {
	return &runCursor{
		reader:    bufio.NewReaderSize(io.NewSectionReader(current.file, 0, current.count*recordBytes), runBufferBytes),
		remaining: current.count,
	}
}

func (cursor *runCursor) next() error {
	if cursor.remaining == 0 {
		return io.EOF
	}
	var encoded [recordBytes]byte
	if _, err := io.ReadFull(cursor.reader, encoded[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	cursor.value = decodeEntry(encoded)
	cursor.remaining--
	return nil
}

func mergeRuns(create func() (scratchFile, error), left, right *run) (*run, error) {
	return buildRun(create, func(builder *runBuilder) error {
		a, b := cursor(left), cursor(right)
		aErr, bErr := a.next(), b.next()
		for !errors.Is(aErr, io.EOF) || !errors.Is(bErr, io.EOF) {
			if aErr != nil && !errors.Is(aErr, io.EOF) {
				return aErr
			}
			if bErr != nil && !errors.Is(bErr, io.EOF) {
				return bErr
			}
			if errors.Is(bErr, io.EOF) || aErr == nil && bytes.Compare(a.value.key[:], b.value.key[:]) < 0 {
				if err := builder.append(a.value); err != nil {
					return err
				}
				aErr = a.next()
			} else {
				if aErr == nil && a.value.key == b.value.key {
					return errConflict
				}
				if err := builder.append(b.value); err != nil {
					return err
				}
				bErr = b.next()
			}
		}
		return nil
	})
}

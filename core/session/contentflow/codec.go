package contentflow

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

const controlSchemaVersion = 1

var bodyEncMode = func() cbor.EncMode {
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

var bodyDecMode = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxNestedLevels:  8,
		MaxArrayElements: 2_048,
		MaxMapPairs:      16,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

func EncodeOpenRequest(request OpenRequest) ([]byte, error) {
	validated, err := NewOpenRequest(request.Items())
	if err != nil {
		return nil, err
	}
	items := make([]any, len(validated.items))
	for index, item := range validated.items {
		ranges := item.InitialRanges.Ranges()
		encodedRanges := make([]any, len(ranges))
		for rangeIndex, requested := range ranges {
			encodedRanges[rangeIndex] = []any{requested.Offset, requested.End}
		}
		items[index] = []any{item.FileID.Bytes(), encodedRanges}
	}
	return bodyEncMode.Marshal(items)
}

func DecodeOpenRequest(encoded []byte) (OpenRequest, error) {
	var items []cbor.RawMessage
	if err := bodyDecMode.Unmarshal(encoded, &items); err != nil || len(items) == 0 || len(items) > content.MaxOpenRevisionBatch {
		return OpenRequest{}, fmt.Errorf("%w: malformed batch", ErrInvalidOpenRequest)
	}
	decoded := make([]OpenItem, len(items))
	for index, raw := range items {
		var fields []cbor.RawMessage
		if err := bodyDecMode.Unmarshal(raw, &fields); err != nil || len(fields) != 2 {
			return OpenRequest{}, fmt.Errorf("%w: item %d", ErrInvalidOpenRequest, index)
		}
		file, err := decodeFileID(fields[0])
		if err != nil {
			return OpenRequest{}, fmt.Errorf("%w: item %d file", ErrInvalidOpenRequest, index)
		}
		ranges, err := decodeRanges(fields[1])
		if err != nil {
			return OpenRequest{}, fmt.Errorf("%w: item %d ranges: %w", ErrInvalidOpenRequest, index, err)
		}
		decoded[index] = OpenItem{FileID: file, InitialRanges: ranges}
	}
	request, err := NewOpenRequest(decoded)
	if err != nil {
		return OpenRequest{}, err
	}
	canonical, err := EncodeOpenRequest(request)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return OpenRequest{}, ErrNonCanonicalBody
	}
	return request, nil
}

func EncodeBlockRequest(request BlockRequest) ([]byte, error) {
	validated, err := NewBlockRequest(request.leaseID, request.indices)
	if err != nil {
		return nil, err
	}
	return bodyEncMode.Marshal([]any{validated.leaseID.Bytes(), validated.indices})
}

func DecodeBlockRequest(encoded []byte) (BlockRequest, error) {
	var fields []cbor.RawMessage
	if err := bodyDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 2 {
		return BlockRequest{}, ErrInvalidBlockRequest
	}
	lease, err := decodeLeaseID(fields[0])
	if err != nil {
		return BlockRequest{}, ErrInvalidBlockRequest
	}
	var indices []uint64
	if err := bodyDecMode.Unmarshal(fields[1], &indices); err != nil {
		return BlockRequest{}, ErrInvalidBlockRequest
	}
	request, err := NewBlockRequest(lease, indices)
	if err != nil {
		return BlockRequest{}, err
	}
	canonical, err := EncodeBlockRequest(request)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return BlockRequest{}, ErrNonCanonicalBody
	}
	return request, nil
}

func EncodeLeaseRequest(lease content.LeaseID) ([]byte, error) {
	if lease.IsZero() {
		return nil, ErrInvalidLeaseRequest
	}
	return bodyEncMode.Marshal([]any{lease.Bytes()})
}

func DecodeLeaseRequest(encoded []byte) (content.LeaseID, error) {
	var fields []cbor.RawMessage
	if err := bodyDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 1 {
		return content.LeaseID{}, ErrInvalidLeaseRequest
	}
	lease, err := decodeLeaseID(fields[0])
	if err != nil {
		return content.LeaseID{}, ErrInvalidLeaseRequest
	}
	canonical, _ := EncodeLeaseRequest(lease)
	if !bytes.Equal(canonical, encoded) {
		return content.LeaseID{}, ErrNonCanonicalBody
	}
	return lease, nil
}

func EncodeCancelReason(reason CancelReason) ([]byte, error) {
	if reason < CancelReasonUser || reason > CancelReasonLaneRace {
		return nil, ErrInvalidCancelRequest
	}
	return bodyEncMode.Marshal([]any{uint64(reason)})
}

func DecodeCancelReason(encoded []byte) (CancelReason, error) {
	var fields []cbor.RawMessage
	if err := bodyDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 1 {
		return 0, ErrInvalidCancelRequest
	}
	raw, err := decodeUint(fields[0])
	if err != nil || raw < uint64(CancelReasonUser) || raw > uint64(CancelReasonLaneRace) {
		return 0, ErrInvalidCancelRequest
	}
	reason := CancelReason(raw)
	canonical, _ := EncodeCancelReason(reason)
	if !bytes.Equal(canonical, encoded) {
		return 0, ErrNonCanonicalBody
	}
	return reason, nil
}

func EncodeOpenResults(results OpenResults) ([]byte, error) {
	validated, err := NewOpenResults(results.Items())
	if err != nil {
		return nil, err
	}
	items := make([]any, len(validated.items))
	for index, result := range validated.items {
		if result.Failure != nil {
			var retryAfter any
			if result.Failure.Retryable {
				milliseconds, durationErr := durationMilliseconds(result.Failure.RetryAfter)
				if durationErr != nil {
					return nil, durationErr
				}
				retryAfter = milliseconds
			}
			items[index] = []any{result.FileID.Bytes(), uint64(1), uint64(result.Failure.Code), result.Failure.Retryable, retryAfter}
			continue
		}
		ttl, err := durationMilliseconds(result.Lease.TTL())
		if err != nil {
			return nil, err
		}
		renewAfter, err := durationMillisecondsAllowZero(result.Lease.RenewAfter())
		if err != nil {
			return nil, err
		}
		items[index] = []any{result.FileID.Bytes(), uint64(0), result.RevisionObject, result.Lease.ID().Bytes(), ttl, renewAfter}
	}
	return bodyEncMode.Marshal(map[uint64]any{0: uint64(controlSchemaVersion), 1: items})
}

type RemoteLease struct {
	ID         content.LeaseID
	TTL        time.Duration
	RenewAfter time.Duration
}

type ReceivedOpenResult struct {
	FileID         catalog.FileID
	RevisionObject []byte
	Lease          RemoteLease
	Failure        *RevisionFailure
}

func ValidateOpenResults(encoded []byte) error {
	_, err := decodeOpenResults(encoded)
	return err
}

func DecodeOpenResults(encoded []byte, expected []catalog.FileID) ([]ReceivedOpenResult, error) {
	if len(expected) == 0 || len(expected) > content.MaxOpenRevisionBatch {
		return nil, ErrInvalidOpenResults
	}
	results, err := decodeOpenResults(encoded)
	if err != nil || len(results) != len(expected) {
		return nil, errors.Join(ErrInvalidOpenResults, err)
	}
	for index, result := range results {
		if result.FileID != expected[index] {
			return nil, fmt.Errorf("%w: item %d", ErrInvalidOpenResults, index)
		}
	}
	return results, nil
}

func decodeOpenResults(encoded []byte) ([]ReceivedOpenResult, error) {
	if err := requireCanonicalBody(encoded); err != nil {
		return nil, err
	}
	var fields map[uint64]cbor.RawMessage
	if err := bodyDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 2 || fields[0] == nil || fields[1] == nil {
		return nil, ErrInvalidOpenResults
	}
	if schema, err := decodeUint(fields[0]); err != nil || schema != controlSchemaVersion {
		return nil, ErrInvalidOpenResults
	}
	var items []cbor.RawMessage
	if err := bodyDecMode.Unmarshal(fields[1], &items); err != nil ||
		len(items) == 0 || len(items) > content.MaxOpenRevisionBatch {
		return nil, ErrInvalidOpenResults
	}
	results := make([]ReceivedOpenResult, len(items))
	for index, raw := range items {
		result, err := decodeOpenResult(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: item %d", ErrInvalidOpenResults, index)
		}
		results[index] = result
	}
	return results, nil
}

func decodeOpenResult(encoded cbor.RawMessage) (ReceivedOpenResult, error) {
	var fields []cbor.RawMessage
	if err := bodyDecMode.Unmarshal(encoded, &fields); err != nil || (len(fields) != 5 && len(fields) != 6) {
		return ReceivedOpenResult{}, ErrInvalidOpenResults
	}
	file, err := decodeFileID(fields[0])
	if err != nil {
		return ReceivedOpenResult{}, err
	}
	status, err := decodeUint(fields[1])
	if err != nil {
		return ReceivedOpenResult{}, err
	}
	if status == 0 && len(fields) == 6 {
		object, err := decodeBytes(fields[2])
		if err != nil || len(object) == 0 {
			return ReceivedOpenResult{}, ErrInvalidOpenResults
		}
		lease, err := decodeLeaseID(fields[3])
		if err != nil {
			return ReceivedOpenResult{}, err
		}
		ttlMS, err := decodeUint(fields[4])
		if err != nil || ttlMS != uint64(RevisionLeaseTTL/time.Millisecond) {
			return ReceivedOpenResult{}, ErrInvalidOpenResults
		}
		renewMS, err := decodeUint(fields[5])
		if err != nil || renewMS != uint64(RevisionLeaseRenewAfter/time.Millisecond) {
			return ReceivedOpenResult{}, ErrInvalidOpenResults
		}
		return ReceivedOpenResult{
			FileID: file, RevisionObject: slices.Clone(object),
			Lease: RemoteLease{ID: lease, TTL: time.Duration(ttlMS) * time.Millisecond, RenewAfter: time.Duration(renewMS) * time.Millisecond},
		}, nil
	}
	if status != 1 || len(fields) != 5 {
		return ReceivedOpenResult{}, ErrInvalidOpenResults
	}
	code, err := decodeUint(fields[2])
	if err != nil || code > math.MaxUint16 {
		return ReceivedOpenResult{}, ErrInvalidOpenResults
	}
	var retryable bool
	if err := bodyDecMode.Unmarshal(fields[3], &retryable); err != nil {
		return ReceivedOpenResult{}, ErrInvalidOpenResults
	}
	var retryAfter time.Duration
	if retryable {
		milliseconds, err := decodeUint(fields[4])
		if err != nil ||
			milliseconds < uint64(MinRevisionFailureRetryAfter/time.Millisecond) ||
			milliseconds > uint64(MaxRevisionFailureRetryAfter/time.Millisecond) {
			return ReceivedOpenResult{}, ErrInvalidOpenResults
		}
		retryAfter = time.Duration(milliseconds) * time.Millisecond
	} else if !bytes.Equal(fields[4], []byte{0xf6}) {
		return ReceivedOpenResult{}, ErrInvalidOpenResults
	}
	failure, err := NewRevisionFailure(uint16(code), retryable, retryAfter)
	if err != nil {
		return ReceivedOpenResult{}, err
	}
	return ReceivedOpenResult{FileID: file, Failure: &failure}, nil
}

func EncodeLeaseResult(lease content.RevisionLease) ([]byte, error) {
	if lease.ID().IsZero() || lease.TTL() != RevisionLeaseTTL || lease.RenewAfter() != RevisionLeaseRenewAfter {
		return nil, ErrInvalidLeaseRequest
	}
	ttl, err := durationMilliseconds(lease.TTL())
	if err != nil {
		return nil, err
	}
	renew, err := durationMillisecondsAllowZero(lease.RenewAfter())
	if err != nil {
		return nil, err
	}
	return bodyEncMode.Marshal(map[uint64]any{0: uint64(controlSchemaVersion), 1: lease.ID().Bytes(), 2: ttl, 3: renew})
}

func ValidateLeaseResult(encoded []byte) error {
	_, err := decodeLeaseResult(encoded)
	return err
}

func DecodeLeaseResult(encoded []byte, expected content.LeaseID) (RemoteLease, error) {
	if expected.IsZero() {
		return RemoteLease{}, ErrInvalidLeaseRequest
	}
	lease, err := decodeLeaseResult(encoded)
	if err != nil || lease.ID != expected {
		return RemoteLease{}, errors.Join(ErrInvalidOperationResult, err)
	}
	return lease, nil
}

func decodeLeaseResult(encoded []byte) (RemoteLease, error) {
	if err := requireCanonicalBody(encoded); err != nil {
		return RemoteLease{}, err
	}
	var fields map[uint64]cbor.RawMessage
	if err := bodyDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 4 {
		return RemoteLease{}, ErrInvalidOperationResult
	}
	if schema, err := decodeUint(fields[0]); err != nil || schema != controlSchemaVersion {
		return RemoteLease{}, ErrInvalidOperationResult
	}
	lease, err := decodeLeaseID(fields[1])
	if err != nil {
		return RemoteLease{}, ErrInvalidOperationResult
	}
	ttlMS, err := decodeUint(fields[2])
	if err != nil || ttlMS != uint64(RevisionLeaseTTL/time.Millisecond) {
		return RemoteLease{}, ErrInvalidOperationResult
	}
	renewMS, err := decodeUint(fields[3])
	if err != nil || renewMS != uint64(RevisionLeaseRenewAfter/time.Millisecond) {
		return RemoteLease{}, ErrInvalidOperationResult
	}
	return RemoteLease{ID: lease, TTL: time.Duration(ttlMS) * time.Millisecond, RenewAfter: time.Duration(renewMS) * time.Millisecond}, nil
}

func EncodeOperationComplete(resultCount uint32) ([]byte, error) {
	return bodyEncMode.Marshal(map[uint64]any{0: uint64(controlSchemaVersion), 1: uint64(resultCount)})
}

func DecodeOperationComplete(encoded []byte) (uint32, error) {
	if err := requireCanonicalBody(encoded); err != nil {
		return 0, err
	}
	var fields map[uint64]cbor.RawMessage
	if err := bodyDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 2 {
		return 0, ErrInvalidOperationResult
	}
	if schema, err := decodeUint(fields[0]); err != nil || schema != controlSchemaVersion {
		return 0, ErrInvalidOperationResult
	}
	count, err := decodeUint(fields[1])
	if err != nil || count > math.MaxUint32 {
		return 0, ErrInvalidOperationResult
	}
	return uint32(count), nil
}

func requireCanonicalBody(encoded []byte) error {
	if len(encoded) == 0 {
		return ErrNonCanonicalBody
	}
	var decoded any
	if err := bodyDecMode.Unmarshal(encoded, &decoded); err != nil {
		return ErrNonCanonicalBody
	}
	canonical, err := bodyEncMode.Marshal(decoded)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return ErrNonCanonicalBody
	}
	return nil
}

func decodeRanges(encoded cbor.RawMessage) (content.RangeSet, error) {
	var items []cbor.RawMessage
	if err := bodyDecMode.Unmarshal(encoded, &items); err != nil || len(items) > MaxInitialRangesPerFile {
		return content.RangeSet{}, ErrInvalidOpenRequest
	}
	ranges := make([]content.Range, len(items))
	for index, item := range items {
		var bounds []uint64
		if err := bodyDecMode.Unmarshal(item, &bounds); err != nil || len(bounds) != 2 {
			return content.RangeSet{}, ErrInvalidOpenRequest
		}
		ranges[index] = content.Range{Offset: bounds[0], End: bounds[1]}
	}
	return content.NewRangeSet(ranges)
}

func decodeFileID(encoded cbor.RawMessage) (catalog.FileID, error) {
	raw, err := decodeBytes(encoded)
	if err != nil {
		return catalog.FileID{}, err
	}
	file, err := catalog.FileIDFromBytes(raw)
	if err != nil || file.IsZero() {
		return catalog.FileID{}, ErrInvalidOpenRequest
	}
	return file, nil
}

func decodeLeaseID(encoded cbor.RawMessage) (content.LeaseID, error) {
	raw, err := decodeBytes(encoded)
	if err != nil {
		return content.LeaseID{}, err
	}
	lease, err := content.LeaseIDFromBytes(raw)
	if err != nil || lease.IsZero() {
		return content.LeaseID{}, ErrInvalidLeaseRequest
	}
	return lease, nil
}

func decodeBytes(encoded cbor.RawMessage) ([]byte, error) {
	var value []byte
	if err := bodyDecMode.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func decodeUint(encoded cbor.RawMessage) (uint64, error) {
	var value uint64
	if err := bodyDecMode.Unmarshal(encoded, &value); err != nil {
		return 0, err
	}
	return value, nil
}

func durationMilliseconds(duration time.Duration) (uint64, error) {
	if duration <= 0 {
		return 0, ErrInvalidOpenResults
	}
	return durationMillisecondsAllowZero(duration)
}

func durationMillisecondsAllowZero(duration time.Duration) (uint64, error) {
	if duration < 0 || duration%time.Millisecond != 0 {
		return 0, ErrInvalidOpenResults
	}
	return uint64(duration / time.Millisecond), nil
}

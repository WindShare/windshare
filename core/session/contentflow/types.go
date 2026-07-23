package contentflow

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	MaxInitialRangesPerFile = 256
	MaxInitialRangesPerOpen = 1_024

	RevisionLeaseTTL        = content.LeaseTTL
	RevisionLeaseRenewAfter = content.LeaseTTL - content.LeaseRenewWindow

	MinRevisionFailureRetryAfter = protocolsession.MinOperationFailureRetryAfter
	MaxRevisionFailureRetryAfter = protocolsession.MaxOperationFailureRetryAfter

	RevisionCodeStale                uint16 = 0x3001
	RevisionCodeNotFound             uint16 = 0x3002
	RevisionCodeUnreadable           uint16 = 0x3003
	RevisionCodeUnsupportedStability uint16 = 0x3004
	RevisionCodeQuota                uint16 = 0x3005
	RevisionCodeLeaseExpired         uint16 = 0x3006
	RevisionCodeDrift                uint16 = 0x3007
	RevisionCodeInvalidLease         uint16 = 0x3008

	BlockCodeInvalidRef       uint16 = 0x4001
	BlockCodeOutOfRange       uint16 = 0x4002
	BlockCodeObjectAuth       uint16 = 0x4003
	BlockCodeFragmentConflict uint16 = 0x4004
	BlockCodeTimeout          uint16 = 0x4005
	BlockCodeCancelled        uint16 = 0x4006
)

type CancelReason uint8

const (
	CancelReasonUser CancelReason = iota + 1
	CancelReasonSuperseded
	CancelReasonOutputAbort
	CancelReasonTimeout
	CancelReasonLaneRace
)

var (
	ErrInvalidOpenRequest     = errors.New("content open request is invalid")
	ErrInvalidBlockRequest    = errors.New("content block request is invalid")
	ErrInvalidLeaseRequest    = errors.New("content lease request is invalid")
	ErrInvalidCancelRequest   = errors.New("content cancel request is invalid")
	ErrInvalidOpenResults     = errors.New("content open results are invalid")
	ErrInvalidOperationResult = errors.New("content operation result is invalid")
	ErrLeaseNotOwned          = errors.New("content lease is not owned by this protocol session")
	ErrServiceClosed          = errors.New("content sender service is closed")
	ErrServiceQueueFull       = errors.New("content sender service work queue is full")
	ErrOutboundUnavailable    = errors.New("content sender outbound path is unavailable")
	ErrNonCanonicalBody       = errors.New("content operation body is not canonical CBOR")
	ErrOperationIdentity      = errors.New("content operation identity is missing")
	ErrUnexpectedMessage      = errors.New("content sender received an unsupported message kind")
	ErrRevisionStoreContract  = errors.New("revision store violated the content sender contract")
)

type OpenItem struct {
	FileID        catalog.FileID
	InitialRanges content.RangeSet
}

type OpenRequest struct{ items []OpenItem }

func NewOpenRequest(items []OpenItem) (OpenRequest, error) {
	if len(items) == 0 || len(items) > content.MaxOpenRevisionBatch {
		return OpenRequest{}, ErrInvalidOpenRequest
	}
	owned := make([]OpenItem, len(items))
	totalRanges := 0
	for index, item := range items {
		if item.FileID.IsZero() {
			return OpenRequest{}, fmt.Errorf("%w: item %d has a zero file identity", ErrInvalidOpenRequest, index)
		}
		ranges := item.InitialRanges.Ranges()
		if len(ranges) > MaxInitialRangesPerFile {
			return OpenRequest{}, fmt.Errorf("%w: item %d has too many ranges", ErrInvalidOpenRequest, index)
		}
		totalRanges += len(ranges)
		if totalRanges > MaxInitialRangesPerOpen {
			return OpenRequest{}, ErrInvalidOpenRequest
		}
		cloned, err := content.NewRangeSet(ranges)
		if err != nil {
			return OpenRequest{}, fmt.Errorf("%w: item %d: %w", ErrInvalidOpenRequest, index, err)
		}
		owned[index] = OpenItem{FileID: item.FileID, InitialRanges: cloned}
	}
	return OpenRequest{items: owned}, nil
}

func (r OpenRequest) Items() []OpenItem {
	items := make([]OpenItem, len(r.items))
	for index, item := range r.items {
		ranges, _ := content.NewRangeSet(item.InitialRanges.Ranges())
		items[index] = OpenItem{FileID: item.FileID, InitialRanges: ranges}
	}
	return items
}

type BlockRequest struct {
	leaseID content.LeaseID
	indices []uint64
}

func NewBlockRequest(leaseID content.LeaseID, indices []uint64) (BlockRequest, error) {
	if leaseID.IsZero() || len(indices) == 0 || len(indices) > content.MaxRequestedBlockIndices {
		return BlockRequest{}, ErrInvalidBlockRequest
	}
	owned := slices.Clone(indices)
	for index := range owned {
		if index > 0 && owned[index] <= owned[index-1] {
			return BlockRequest{}, fmt.Errorf("%w: indices must be unique and strictly increasing", ErrInvalidBlockRequest)
		}
	}
	return BlockRequest{leaseID: leaseID, indices: owned}, nil
}

func (r BlockRequest) LeaseID() content.LeaseID { return r.leaseID }
func (r BlockRequest) Indices() []uint64        { return slices.Clone(r.indices) }

type RevisionFailure struct {
	Code       uint16
	Retryable  bool
	RetryAfter time.Duration
}

func NewRevisionFailure(code uint16, retryable bool, retryAfter time.Duration) (RevisionFailure, error) {
	if code < RevisionCodeStale || code > RevisionCodeInvalidLease {
		return RevisionFailure{}, errors.New("revision failure code is outside its scope")
	}
	if retryable && (retryAfter < MinRevisionFailureRetryAfter ||
		retryAfter > MaxRevisionFailureRetryAfter ||
		retryAfter%time.Millisecond != 0) {
		return RevisionFailure{}, errors.New("retryable revision failure delay must be an integral millisecond within its limit")
	}
	if !retryable && retryAfter != 0 {
		return RevisionFailure{}, errors.New("permanent revision failure cannot carry a retry delay")
	}
	return RevisionFailure{Code: code, Retryable: retryable, RetryAfter: retryAfter}, nil
}

type OpenResult struct {
	FileID         catalog.FileID
	Lease          content.RevisionLease
	RevisionObject []byte
	Failure        *RevisionFailure
}

func SuccessfulOpen(file catalog.FileID, lease content.RevisionLease, object []byte) (OpenResult, error) {
	if file.IsZero() || lease.ID().IsZero() || lease.Descriptor().FileID() != file || len(object) == 0 {
		return OpenResult{}, ErrInvalidOpenResults
	}
	return OpenResult{FileID: file, Lease: lease, RevisionObject: slices.Clone(object)}, nil
}

func FailedOpen(file catalog.FileID, failure RevisionFailure) (OpenResult, error) {
	validated, err := NewRevisionFailure(failure.Code, failure.Retryable, failure.RetryAfter)
	if file.IsZero() || err != nil {
		return OpenResult{}, ErrInvalidOpenResults
	}
	return OpenResult{FileID: file, Failure: &validated}, nil
}

func (r OpenResult) clone() OpenResult {
	copy := r
	copy.RevisionObject = slices.Clone(r.RevisionObject)
	if r.Failure != nil {
		failure := *r.Failure
		copy.Failure = &failure
	}
	return copy
}

type OpenResults struct{ items []OpenResult }

func NewOpenResults(items []OpenResult) (OpenResults, error) {
	if len(items) == 0 || len(items) > content.MaxOpenRevisionBatch {
		return OpenResults{}, ErrInvalidOpenResults
	}
	owned := make([]OpenResult, len(items))
	for index, item := range items {
		if item.FileID.IsZero() || (item.Failure == nil) == (len(item.RevisionObject) == 0) {
			return OpenResults{}, fmt.Errorf("%w: item %d must contain exactly one outcome", ErrInvalidOpenResults, index)
		}
		if item.Failure == nil {
			if item.Lease.ID().IsZero() || item.Lease.Descriptor().FileID() != item.FileID ||
				item.Lease.TTL() != RevisionLeaseTTL || item.Lease.RenewAfter() != RevisionLeaseRenewAfter {
				return OpenResults{}, ErrInvalidOpenResults
			}
		} else if _, err := NewRevisionFailure(item.Failure.Code, item.Failure.Retryable, item.Failure.RetryAfter); err != nil {
			return OpenResults{}, err
		}
		owned[index] = item.clone()
	}
	return OpenResults{items: owned}, nil
}

func (r OpenResults) Items() []OpenResult {
	items := make([]OpenResult, len(r.items))
	for index := range r.items {
		items[index] = r.items[index].clone()
	}
	return items
}

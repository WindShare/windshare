package catalogflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

const (
	DirectoryCodeStale       uint16 = 0x2001
	DirectoryCodePermission  uint16 = 0x2002
	DirectoryCodeCollision   uint16 = 0x2003
	DirectoryCodeTooWide     uint16 = 0x2004
	DirectoryCodeBudget      uint16 = 0x2005
	DirectoryCodePermanentIO uint16 = 0x2006
	DirectoryCodeTransientIO uint16 = 0x2007
	DirectoryCodeCancelled   uint16 = 0x2008

	MinRetryAfter = 250 * time.Millisecond
	MaxRetryAfter = 30 * time.Second
)

var (
	ErrInvalidRequest     = errors.New("catalog list request is invalid")
	ErrObjectIdentity     = errors.New("verified catalog object changes request identity")
	ErrPageGap            = errors.New("catalog page sequence contains a gap")
	ErrPageConflict       = errors.New("catalog page replay conflicts with committed content")
	ErrPostTerminal       = errors.New("catalog object arrived after the terminal page")
	ErrClientBudget       = errors.New("catalog client memory budget exceeded")
	ErrGenerationMismatch = errors.New("catalog request generation is stale")
	ErrPageOutOfRange     = errors.New("catalog request page is outside the generation")
	ErrUnverifiedObject   = errors.New("catalog verifier returned no page or failure")
	ErrResponseIdentity   = errors.New("verified catalog object does not answer its authenticated request")
)

type DirectoryFailure struct {
	ShareInstance catalog.ShareInstance
	DirectoryID   catalog.DirectoryID
	AttemptID     catalog.ScanAttemptID
	Code          uint16
	Retryable     bool
	RetryAfter    time.Duration
}

func NewDirectoryFailure(failure DirectoryFailure) (DirectoryFailure, error) {
	if failure.ShareInstance.IsZero() || failure.DirectoryID.IsZero() || failure.AttemptID.IsZero() {
		return DirectoryFailure{}, errors.New("catalog directory failure requires share, directory, and attempt identities")
	}
	if failure.Code < DirectoryCodeStale || failure.Code > DirectoryCodeCancelled {
		return DirectoryFailure{}, errors.New("catalog directory failure code is outside its scope")
	}
	if failure.Code == DirectoryCodeTransientIO && !failure.Retryable {
		return DirectoryFailure{}, errors.New("transient directory I/O must be retryable")
	}
	if failure.Retryable && (failure.RetryAfter < MinRetryAfter || failure.RetryAfter > MaxRetryAfter) {
		return DirectoryFailure{}, errors.New("retryable directory failure delay is outside the frozen range")
	}
	if !failure.Retryable && failure.RetryAfter != 0 {
		return DirectoryFailure{}, errors.New("permanent directory failure cannot carry a retry delay")
	}
	return failure, nil
}

func (f DirectoryFailure) Error() string {
	return fmt.Sprintf("catalog directory %x attempt %x failed with code %#x", f.DirectoryID, f.AttemptID, f.Code)
}

// DirectoryFailure marks an authenticated, directory-scoped source failure.
// Consumers may skip only this subtree; unmarked verifier, identity, transport,
// and budget errors remain fatal to the consuming operation.
func (f DirectoryFailure) DirectoryFailure() {}

type DirectoryResult struct {
	Snapshot catalog.DirectorySnapshot
	Failure  *DirectoryFailure
}

func SnapshotResult(snapshot catalog.DirectorySnapshot) DirectoryResult {
	return DirectoryResult{Snapshot: snapshot}
}

func FailureResult(failure DirectoryFailure) DirectoryResult {
	copy := failure
	return DirectoryResult{Failure: &copy}
}

type VerifiedObject struct {
	Page    catalog.CatalogPage
	Failure *DirectoryFailure
}

func VerifiedPage(page catalog.CatalogPage) VerifiedObject {
	return VerifiedObject{Page: page}
}

func VerifiedFailure(failure DirectoryFailure) VerifiedObject {
	copy := failure
	return VerifiedObject{Failure: &copy}
}

type DirectorySource interface {
	LoadDirectory(ctx context.Context, directory catalog.DirectoryID) (DirectoryResult, error)
}

// SealedObjectStore returns the exact bytes committed for an immutable page or
// scan attempt. Catalog chaining hashes complete sender-object bytes, so serving
// code must never reseal on demand with a new nonce.
type SealedObjectStore interface {
	LoadSealedPage(context.Context, catalog.CatalogPage) ([]byte, error)
	LoadSealedFailure(context.Context, DirectoryFailure) ([]byte, error)
}

type ObjectVerifier interface {
	Verify(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error)
}

type PageTransport interface {
	FetchPage(context.Context, ListRequest) ([]byte, error)
}

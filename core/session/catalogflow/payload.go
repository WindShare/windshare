package catalogflow

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
)

type CatalogFailureEncoder struct{}

func (CatalogFailureEncoder) EncodeFailure(record catalog.DirectoryFailureRecord) (catalog.FailurePayload, error) {
	failure, err := directoryFailureFromCatalogRecord(record)
	if err != nil {
		return catalog.FailurePayload{}, err
	}
	payload, err := EncodeDirectoryFailure(failure)
	if err != nil {
		return catalog.FailurePayload{}, err
	}
	return catalog.NewFailurePayload(payload)
}

func directoryFailureFromCatalogRecord(record catalog.DirectoryFailureRecord) (DirectoryFailure, error) {
	code, err := directoryCodeForFailureKind(record.Kind())
	if err != nil {
		return DirectoryFailure{}, err
	}
	return NewDirectoryFailure(DirectoryFailure{
		ShareInstance: record.ShareInstance(), DirectoryID: record.DirectoryID(), AttemptID: record.AttemptID(),
		Code: code, Retryable: record.Retryable(), RetryAfter: record.RetryAfter(),
	})
}

func directoryCodeForFailureKind(kind catalog.FailureKind) (uint16, error) {
	switch kind {
	case catalog.FailureKindStale:
		return DirectoryCodeStale, nil
	case catalog.FailureKindPermission:
		return DirectoryCodePermission, nil
	case catalog.FailureKindCollision:
		return DirectoryCodeCollision, nil
	case catalog.FailureKindTooWide:
		return DirectoryCodeTooWide, nil
	case catalog.FailureKindBudget:
		return DirectoryCodeBudget, nil
	case catalog.FailureKindPermanentIO:
		return DirectoryCodePermanentIO, nil
	case catalog.FailureKindTransientIO:
		return DirectoryCodeTransientIO, nil
	default:
		return 0, ErrObjectIdentity
	}
}

// CatalogPayloadDecoder checks the canonical record and its request binding
// after the session has authenticated delivery.
type CatalogPayloadDecoder struct{}

func (CatalogPayloadDecoder) Verify(ctx context.Context, share catalog.ShareInstance, request ListRequest, payload []byte) (VerifiedObject, error) {
	if err := ctx.Err(); err != nil {
		return VerifiedObject{}, err
	}
	if share.IsZero() || len(payload) == 0 || len(payload) > catalog.MaxCatalogPayloadBytes {
		return VerifiedObject{}, ErrObjectIdentity
	}
	var header struct {
		Schema uint64 `cbor:"0,keyasint"`
	}
	if err := catalogWireDec.Unmarshal(payload, &header); err != nil {
		return VerifiedObject{}, errors.Join(ErrWireObject, err)
	}
	switch header.Schema {
	case catalog.PagePayloadSchema:
		page, err := catalog.DecodeCatalogPage(payload)
		if errors.Is(err, catalog.ErrNonCanonicalPagePayload) {
			return VerifiedObject{}, ErrNonCanonicalWire
		}
		if err != nil {
			return VerifiedObject{}, errors.Join(ErrWireObject, err)
		}
		generation, pinned := request.Generation()
		if page.ShareInstance() != share || page.DirectoryID() != request.DirectoryID() ||
			page.PageIndex() != request.PageIndex() || pinned && page.Generation() != generation {
			return VerifiedObject{}, ErrObjectIdentity
		}
		return VerifiedPage(page), nil
	case directoryFailureSchema:
		if _, pinned := request.Generation(); request.PageIndex() != 0 || pinned {
			return VerifiedObject{}, ErrObjectIdentity
		}
		failure, err := DecodeDirectoryFailure(payload)
		if err != nil {
			return VerifiedObject{}, err
		}
		if failure.ShareInstance != share || failure.DirectoryID != request.DirectoryID() {
			return VerifiedObject{}, ErrObjectIdentity
		}
		return VerifiedFailure(failure), nil
	default:
		return VerifiedObject{}, ErrWireObject
	}
}

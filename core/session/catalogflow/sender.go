package catalogflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
)

type SenderService struct {
	shareInstance catalog.ShareInstance
	source        DirectorySource
	objects       SealedObjectStore
}

func NewSenderService(instance catalog.ShareInstance, source DirectorySource, objects SealedObjectStore) (*SenderService, error) {
	if instance.IsZero() || source == nil || objects == nil {
		return nil, errors.New("catalog sender service requires share identity, source, and sealed-object store")
	}
	return &SenderService{shareInstance: instance, source: source, objects: objects}, nil
}

func (s *SenderService) Serve(ctx context.Context, request ListRequest) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.directory.IsZero() || (request.pageIndex > 0 && request.generation == nil) {
		return nil, ErrInvalidRequest
	}
	result, err := s.source.LoadDirectory(ctx, request.directory)
	if err != nil {
		return nil, fmt.Errorf("load catalog directory: %w", err)
	}
	if result.Failure != nil {
		if result.Snapshot.PageCount() != 0 {
			return nil, errors.New("catalog source returned both a snapshot and a failure")
		}
		failure, validationErr := NewDirectoryFailure(*result.Failure)
		if validationErr != nil {
			return nil, fmt.Errorf("validate catalog source failure: %w", validationErr)
		}
		if failure.ShareInstance != s.shareInstance || failure.DirectoryID != request.directory {
			return nil, ErrObjectIdentity
		}
		encoded, loadErr := s.objects.LoadSealedFailure(ctx, failure)
		return validateSealedObject(encoded, loadErr)
	}

	snapshot := result.Snapshot
	if snapshot.PageCount() == 0 {
		return nil, errors.New("catalog source returned neither a snapshot nor a failure")
	}
	if snapshot.ShareInstance() != s.shareInstance || snapshot.DirectoryID() != request.directory {
		return nil, ErrObjectIdentity
	}
	if request.generation != nil && snapshot.Generation() != *request.generation {
		return nil, ErrGenerationMismatch
	}
	page, ok := snapshot.Page(request.pageIndex)
	if !ok {
		return nil, ErrPageOutOfRange
	}
	encoded, loadErr := s.objects.LoadSealedPage(ctx, page)
	return validateSealedObject(encoded, loadErr)
}

func validateSealedObject(encoded []byte, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	if len(encoded) == 0 || len(encoded) > catalog.MaxCatalogPageObjectBytes {
		return nil, fmt.Errorf("catalog sealed object has invalid length %d", len(encoded))
	}
	return append([]byte(nil), encoded...), nil
}

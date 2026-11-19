package catalogflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
)

type PageResult struct {
	page    catalog.PagePayload
	failure *addressedDirectoryFailure
}

// CatalogPageResult exposes only payloads whose canonical bytes and address are bound.
func CatalogPageResult(page catalog.PagePayload) PageResult {
	return PageResult{page: page}
}

type addressedDirectoryFailure struct {
	record  DirectoryFailure
	payload []byte
}

type PageAddressedSource interface {
	LoadPage(context.Context, ListRequest, catalog.ScanProgressObserver) (PageResult, error)
}

type CatalogStoreSourceConfig struct {
	ShareInstance catalog.ShareInstance
	Store         *catalog.CatalogStore
	SessionBudget *catalog.BudgetAccount
	Scanner       catalog.DirectoryScanner
}

// CatalogStoreSource preserves the page-addressed backend boundary. A later
// page never materializes its siblings, and only page zero without a generation
// may trigger the directory's share-scoped lazy scan.
type CatalogStoreSource struct {
	share   catalog.ShareInstance
	store   *catalog.CatalogStore
	budget  *catalog.BudgetAccount
	scanner catalog.DirectoryScanner
}

func NewCatalogStoreSource(config CatalogStoreSourceConfig) (*CatalogStoreSource, error) {
	if config.ShareInstance.IsZero() || config.Store == nil || config.SessionBudget == nil || config.Scanner == nil {
		return nil, errors.New("catalog store source requires share, store, session budget, and scanner")
	}
	return &CatalogStoreSource{
		share: config.ShareInstance, store: config.Store, budget: config.SessionBudget, scanner: config.Scanner,
	}, nil
}

func (source *CatalogStoreSource) LoadPage(
	ctx context.Context,
	request ListRequest,
	progress catalog.ScanProgressObserver,
) (PageResult, error) {
	if err := ctx.Err(); err != nil {
		return PageResult{}, err
	}
	directoryID := request.DirectoryID()
	generation, hasGeneration := request.Generation()
	committed, found, err := source.store.Directory(ctx, directoryID)
	if err != nil {
		return PageResult{}, err
	}
	if !found {
		return source.loadFirstPage(ctx, request, hasGeneration, progress)
	}
	return source.loadCommittedPage(ctx, request, committed, generation, hasGeneration)
}

func (source *CatalogStoreSource) loadFirstPage(
	ctx context.Context,
	request ListRequest,
	hasGeneration bool,
	progress catalog.ScanProgressObserver,
) (PageResult, error) {
	if request.PageIndex() != 0 || hasGeneration {
		return PageResult{}, ErrGenerationMismatch
	}
	committed, err := source.store.ListChildren(
		ctx, request.DirectoryID(), source.budget,
		catalog.ScanOptions{Retry: true, Progress: progress}, source.scanner,
	)
	if err == nil {
		return source.loadCommittedPage(ctx, request, committed, catalog.DirectoryGeneration{}, false)
	}
	var failure *catalog.DirectoryFailure
	if !errors.As(err, &failure) {
		return PageResult{}, err
	}
	wireFailure, err := source.translateFailure(failure)
	if err != nil {
		return PageResult{}, err
	}
	object, found, err := source.store.FailurePayload(ctx, failure.DirectoryID, failure.AttemptID)
	if err != nil {
		return PageResult{}, err
	}
	if !found {
		return PageResult{}, catalog.ErrCorruptCatalogStorage
	}
	return PageResult{failure: &addressedDirectoryFailure{record: wireFailure, payload: object.Bytes()}}, nil
}

func (source *CatalogStoreSource) loadCommittedPage(
	ctx context.Context,
	request ListRequest,
	committed catalog.CommittedDirectory,
	generation catalog.DirectoryGeneration,
	hasGeneration bool,
) (PageResult, error) {
	directoryID := request.DirectoryID()
	if committed.ShareInstance() != source.share || committed.DirectoryID() != directoryID {
		return PageResult{}, ErrObjectIdentity
	}
	if hasGeneration && committed.Generation() != generation {
		return PageResult{}, ErrGenerationMismatch
	}
	page, found, err := source.store.PagePayload(ctx, directoryID, committed.Generation(), request.PageIndex())
	if err != nil {
		return PageResult{}, err
	}
	if !found {
		return PageResult{}, ErrPageOutOfRange
	}
	return CatalogPageResult(page), nil
}

func (source *CatalogStoreSource) translateFailure(failure *catalog.DirectoryFailure) (DirectoryFailure, error) {
	code, err := directoryCodeForFailureKind(failure.Kind)
	if err != nil {
		return DirectoryFailure{}, err
	}
	return NewDirectoryFailure(DirectoryFailure{
		ShareInstance: source.share, DirectoryID: failure.DirectoryID, AttemptID: failure.AttemptID,
		Code: code, Retryable: failure.Transient, RetryAfter: failure.RetryAfter,
	})
}

type AddressedSenderService struct {
	share  catalog.ShareInstance
	source PageAddressedSource
}

func NewAddressedSenderService(
	share catalog.ShareInstance,
	source PageAddressedSource,
) (*AddressedSenderService, error) {
	if share.IsZero() || source == nil {
		return nil, errors.New("page-addressed catalog service requires share and source")
	}
	return &AddressedSenderService{share: share, source: source}, nil
}

func (service *AddressedSenderService) Serve(
	ctx context.Context,
	request ListRequest,
	progress catalog.ScanProgressObserver,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.directory.IsZero() || (request.pageIndex > 0 && request.generation == nil) {
		return nil, ErrInvalidRequest
	}
	result, err := service.source.LoadPage(ctx, request, progress)
	if err != nil {
		return nil, fmt.Errorf("load addressed catalog page: %w", err)
	}
	if failure := result.failure; failure != nil {
		if !result.page.IsZero() || failure.record.ShareInstance != service.share ||
			failure.record.DirectoryID != request.DirectoryID() {
			return nil, ErrObjectIdentity
		}
		if _, pinned := request.Generation(); request.PageIndex() != 0 || pinned {
			return nil, ErrGenerationMismatch
		}
		expected, err := EncodeDirectoryFailure(failure.record)
		if err != nil || !bytes.Equal(failure.payload, expected) {
			return nil, errors.Join(catalog.ErrCorruptCatalogStorage, err)
		}
		return expected, nil
	}
	address := result.page.Address()
	if result.page.IsZero() || address.ShareInstance != service.share || address.DirectoryID != request.DirectoryID() ||
		address.PageIndex != request.PageIndex() {
		return nil, ErrObjectIdentity
	}
	if generation, pinned := request.Generation(); pinned && address.Generation != generation {
		return nil, ErrGenerationMismatch
	}
	return result.page.Bytes(), nil
}

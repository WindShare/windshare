package catalogflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
)

type PageResult struct {
	Page         catalog.CatalogPage
	SealedObject []byte
	Failure      *DirectoryFailure
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
	object, found, err := source.store.FailureObject(ctx, failure.DirectoryID, failure.AttemptID)
	if err != nil {
		return PageResult{}, err
	}
	if !found {
		return PageResult{}, catalog.ErrCorruptCatalogStorage
	}
	return PageResult{Failure: &wireFailure, SealedObject: object.Bytes()}, nil
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
	page, found, err := source.store.Page(ctx, directoryID, committed.Generation(), request.PageIndex())
	if err != nil {
		return PageResult{}, err
	}
	if !found {
		return PageResult{}, ErrPageOutOfRange
	}
	object, found, err := source.store.PageObject(ctx, directoryID, committed.Generation(), request.PageIndex())
	if err != nil {
		return PageResult{}, err
	}
	if !found || object.Commitment() != page.Commitment() {
		return PageResult{}, catalog.ErrCorruptCatalogStorage
	}
	return PageResult{Page: page, SealedObject: object.Bytes()}, nil
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
	result, err := service.source.LoadPage(ctx, request, progress)
	if err != nil {
		return nil, fmt.Errorf("load addressed catalog page: %w", err)
	}
	if result.Failure != nil {
		if !result.Page.ShareInstance().IsZero() || len(result.SealedObject) == 0 {
			return nil, ErrObjectIdentity
		}
		return validateSealedObject(result.SealedObject, nil)
	}
	if result.Page.ShareInstance() != service.share {
		return nil, ErrObjectIdentity
	}
	object, err := catalog.NewSealedPageObject(result.SealedObject)
	if err != nil || object.Commitment() != result.Page.Commitment() {
		return nil, errors.Join(ErrObjectIdentity, err)
	}
	return object.Bytes(), nil
}

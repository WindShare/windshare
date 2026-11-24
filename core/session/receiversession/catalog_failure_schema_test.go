package receiversession

import (
	"bytes"
	"context"
	"errors"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sendersession"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"testing"
)

func TestMalformedDirectoryRequestUsesSharedFailureSchema(t *testing.T) {
	body, err := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	call, err := receiver.rpc.Begin(context.Background(), protocolsession.MessageListChildren, body)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.rpc.End(call)
	message, err := receiver.rpc.Await(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if message.Kind() != protocolsession.MessageOperationError {
		t.Fatalf("malformed catalog response kind = %d", message.Kind())
	}
	unsigned := message.Body()
	want, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
		Scope: protocolsession.OperationScopeDirectory,
		Code:  catalogflow.DirectoryCodePermanentIO, Message: "Catalog operation failed",
	})
	if err != nil || !bytes.Equal(unsigned, want) {
		t.Fatalf("directory operation failure = %x, want %x, err %v", unsigned, want, err)
	}
}

type failingCatalogPageSource struct{ err error }

func (source failingCatalogPageSource) LoadPage(
	context.Context,
	catalogflow.ListRequest,
	catalog.ScanProgressObserver,
) (catalogflow.PageResult, error) {
	return catalogflow.PageResult{}, source.err
}

func TestCatalogBudgetOperationErrorPreservesRetryableBudgetCode(t *testing.T) {
	fixture := newVerticalFixture(t)
	service, err := catalogflow.NewAddressedSenderService(fixture.share, failingCatalogPageSource{
		err: catalog.ErrBudgetExceeded,
	})
	if err != nil {
		t.Fatal(err)
	}
	senderFactory := newLocalFailureSenderFactory(
		t, fixture,
		sendersession.CatalogFactoryFunc(func() (*catalogflow.AddressedSenderService, error) {
			return service, nil
		}),
		nil,
	)
	sender, receiver := connectVerticalPair(t, senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	_, err = receiver.Catalog().LoadDirectory(context.Background(), fixture.directoryID)
	var remote RemoteOperationError
	value, normalized := boundaryFaultForTest(err)
	expected, _ := transferfault.NewCatalog(
		transferfault.ScopeDirectoryLocal, transferfault.CatalogUnavailable,
	)
	if !errors.As(err, &remote) || !normalized || value != expected {
		t.Fatalf("budget operation error = %v, want directory-local RemoteOperationError", err)
	}
	if remote.Failure().Scope() != protocolsession.OperationScopeDirectory ||
		remote.Failure().Code() != catalogflow.DirectoryCodeBudget ||
		!remote.Failure().Retryable() ||
		remote.Failure().RetryAfter() != catalog.DefaultScanRetryCooldown {
		t.Fatalf("budget operation failure = %+v, want retryable DirectoryCodeBudget (%#x, %v)",
			remote.Failure(), catalogflow.DirectoryCodeBudget, catalog.DefaultScanRetryCooldown)
	}
	if receiver.Err() != nil || sender.Err() != nil {
		t.Fatalf("budget operation error terminated session: receiver=%v sender=%v", receiver.Err(), sender.Err())
	}
}

func TestCatalogStoreExhaustedFailurePersistenceMapsToRetryableBudgetOperationError(t *testing.T) {
	fixture := newVerticalFixture(t)
	process, err := catalog.NewBudgetAccount("exhausted-process", catalog.DefaultProcessBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	shareBudget, err := catalog.NewBudgetAccount("exhausted-share", catalog.DefaultShareBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	store, err := catalog.NewCatalogStore(catalog.StoreConfig{
		ShareInstance:  fixture.share,
		ProcessBudget:  process,
		ShareBudget:    shareBudget,
		FailureEncoder: catalogflow.CatalogFailureEncoder{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	startup, err := catalog.NewBudgetAccount("exhausted-startup", catalog.DefaultSessionBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := catalog.NewSourceReference([]byte("dir"))
	identity, _ := catalog.NewSourceIdentity([]byte("id"))
	selectedDir, err := catalog.NewDirectoryNodeRecord(
		fixture.directoryID, fixture.syntheticRoot, "tight", ref, identity, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootCommit, err := catalog.NewSyntheticRootCommit(catalog.SyntheticRootCommitSpec{
		ShareInstance: fixture.share,
		SyntheticRoot: fixture.syntheticRoot,
		Generation:    id16[catalog.DirectoryGeneration](211),
		SelectedRoots: []catalog.NodeRecord{selectedDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitSyntheticRoot(context.Background(), rootCommit, startup); err != nil {
		t.Fatal(err)
	}

	// Session allows attempt admission (ScanAttemptLedgerBytes = 96 B) but cannot
	// fit even failure encoding (memoryFailureOverhead + MaxFailurePayloadBytes),
	// exercising the real CatalogStore -> sender_catalog.go fallback path.
	tinySession, err := catalog.NewBudgetAccount("exhausted-session", catalog.BudgetLimits{
		ActiveScans: 4, ScanWork: 100, Entries: 100, MemoryBytes: catalog.ScanAttemptLedgerBytes + 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := catalogflow.NewCatalogStoreSource(catalogflow.CatalogStoreSourceConfig{
		ShareInstance: fixture.share,
		Store:         store,
		SessionBudget: tinySession,
		Scanner: catalog.DirectoryScannerFunc(func(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
			return catalog.ScanResult{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := catalogflow.NewAddressedSenderService(fixture.share, source)
	if err != nil {
		t.Fatal(err)
	}
	senderFactory := newLocalFailureSenderFactory(
		t, fixture,
		sendersession.CatalogFactoryFunc(func() (*catalogflow.AddressedSenderService, error) {
			return service, nil
		}),
		nil,
	)
	sender, receiver := connectVerticalPair(t, senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	_, err = receiver.Catalog().LoadDirectory(context.Background(), fixture.directoryID)
	var remote RemoteOperationError
	if !errors.As(err, &remote) ||
		remote.Failure().Scope() != protocolsession.OperationScopeDirectory ||
		remote.Failure().Code() != catalogflow.DirectoryCodeBudget ||
		!remote.Failure().Retryable() ||
		remote.Failure().RetryAfter() != catalog.DefaultScanRetryCooldown {
		t.Fatalf("exhausted failure persistence error = %v, want retryable DirectoryCodeBudget", err)
	}
	if tinySession.Snapshot().Used != (catalog.ResourceUsage{}) {
		t.Fatalf("exhausted session leaked budget: %+v", tinySession.Snapshot().Used)
	}
}

func TestCatalogShareCapacityExhaustionMapsToNonRetryableBudgetOperationError(t *testing.T) {
	t.Run("direct share capacity error", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		service, err := catalogflow.NewAddressedSenderService(fixture.share, failingCatalogPageSource{
			err: catalog.ErrShareCapacityExceeded,
		})
		if err != nil {
			t.Fatal(err)
		}
		senderFactory := newLocalFailureSenderFactory(
			t, fixture,
			sendersession.CatalogFactoryFunc(func() (*catalogflow.AddressedSenderService, error) {
				return service, nil
			}),
			nil,
		)
		sender, receiver := connectVerticalPair(t, senderFactory, fixture.receiverFactory)
		defer sender.Close()
		defer receiver.Close()

		_, err = receiver.Catalog().LoadDirectory(context.Background(), fixture.directoryID)
		var remote RemoteOperationError
		if !errors.As(err, &remote) ||
			remote.Failure().Scope() != protocolsession.OperationScopeDirectory ||
			remote.Failure().Code() != catalogflow.DirectoryCodeBudget ||
			remote.Failure().Retryable() ||
			remote.Failure().RetryAfter() != 0 {
			t.Fatalf("share capacity operation error = %v, want non-retryable DirectoryCodeBudget", err)
		}
		if receiver.Err() != nil || sender.Err() != nil {
			t.Fatalf("share capacity error terminated session: receiver=%v sender=%v", receiver.Err(), sender.Err())
		}
	})

	t.Run("store share budget exhausted before failure persistence", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		process, err := catalog.NewBudgetAccount("perm-op-process", catalog.DefaultProcessBudgetLimits())
		if err != nil {
			t.Fatal(err)
		}
		// Leave room for the synthetic root plus ScanAttemptLedgerBytes, but not
		// for failure persistence (~1.15 KiB) so the fallback OperationError path
		// runs against an uncontended ShareBudget.
		shareBudget, err := catalog.NewBudgetAccount("perm-op-share", catalog.BudgetLimits{
			ActiveScans: 4, ScanWork: 100, Entries: 100, MemoryBytes: 1_500,
		})
		if err != nil {
			t.Fatal(err)
		}
		store, err := catalog.NewCatalogStore(catalog.StoreConfig{
			ShareInstance:  fixture.share,
			ProcessBudget:  process,
			ShareBudget:    shareBudget,
			FailureEncoder: catalogflow.CatalogFailureEncoder{},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		startup, err := catalog.NewBudgetAccount("perm-op-startup", catalog.DefaultSessionBudgetLimits())
		if err != nil {
			t.Fatal(err)
		}
		ref, _ := catalog.NewSourceReference([]byte("dir"))
		identity, _ := catalog.NewSourceIdentity([]byte("id"))
		selectedDir, err := catalog.NewDirectoryNodeRecord(
			fixture.directoryID, fixture.syntheticRoot, "tight", ref, identity, catalog.ModifiedTime{},
		)
		if err != nil {
			t.Fatal(err)
		}
		rootCommit, err := catalog.NewSyntheticRootCommit(catalog.SyntheticRootCommitSpec{
			ShareInstance: fixture.share,
			SyntheticRoot: fixture.syntheticRoot,
			Generation:    id16[catalog.DirectoryGeneration](212),
			SelectedRoots: []catalog.NodeRecord{selectedDir},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CommitSyntheticRoot(context.Background(), rootCommit, startup); err != nil {
			t.Fatal(err)
		}

		session, err := catalog.NewBudgetAccount("perm-op-session", catalog.DefaultSessionBudgetLimits())
		if err != nil {
			t.Fatal(err)
		}
		source, err := catalogflow.NewCatalogStoreSource(catalogflow.CatalogStoreSourceConfig{
			ShareInstance: fixture.share,
			Store:         store,
			SessionBudget: session,
			Scanner: catalog.DirectoryScannerFunc(func(ctx context.Context, req catalog.ScanRequest) (catalog.ScanResult, error) {
				fileRef, _ := catalog.NewSourceReference(bytes.Repeat([]byte("z"), 2<<10))
				fileID, _ := catalog.FileIDFromBytes(id16[catalog.FileID](220).Bytes())
				fileIdentity, _ := catalog.NewSourceIdentity([]byte("fid"))
				fileVersion, _ := catalog.NewVersionCandidate([]byte("ver"))
				return catalog.ScanResult{}, req.Children.Add(ctx, catalog.ScannedChild{
					FileID: fileID, Name: "big.bin", SourceReference: fileRef,
					SourceIdentity: fileIdentity, VersionCandidate: fileVersion, ExpectedSize: 1,
				})
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		service, err := catalogflow.NewAddressedSenderService(fixture.share, source)
		if err != nil {
			t.Fatal(err)
		}
		senderFactory := newLocalFailureSenderFactory(
			t, fixture,
			sendersession.CatalogFactoryFunc(func() (*catalogflow.AddressedSenderService, error) {
				return service, nil
			}),
			nil,
		)
		sender, receiver := connectVerticalPair(t, senderFactory, fixture.receiverFactory)
		defer sender.Close()
		defer receiver.Close()

		_, err = receiver.Catalog().LoadDirectory(context.Background(), fixture.directoryID)
		var remote RemoteOperationError
		if !errors.As(err, &remote) ||
			remote.Failure().Scope() != protocolsession.OperationScopeDirectory ||
			remote.Failure().Code() != catalogflow.DirectoryCodeBudget ||
			remote.Failure().Retryable() ||
			remote.Failure().RetryAfter() != 0 {
			t.Fatalf("exhausted share failure persistence error = %v, want non-retryable DirectoryCodeBudget", err)
		}
		if session.Snapshot().Used != (catalog.ResourceUsage{}) {
			t.Fatalf("exhausted share session leaked budget: %+v", session.Snapshot().Used)
		}
	})
}

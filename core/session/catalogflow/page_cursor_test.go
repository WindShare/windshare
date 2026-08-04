package catalogflow

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestPageCursorStreamsOneGenerationToItsTerminalPage(t *testing.T) {
	instance := shareInstance(t, 70)
	directory := directoryID(t, 71)
	want := twoPageSnapshot(t, instance, directory, 72, "first", "second")
	pages := want.Pages()
	codec := newMemoryObjectCodec()
	objects := make([][]byte, len(pages))
	for index, page := range pages {
		object, err := codec.LoadSealedPage(context.Background(), page)
		if err != nil {
			t.Fatal(err)
		}
		objects[index] = object
	}
	var requests []ListRequest
	client, err := NewClient(ClientConfig{
		ShareInstance:        instance,
		MaxPagesPerDirectory: 2,
		Transport: PageTransportFunc(func(_ context.Context, request ListRequest) ([]byte, error) {
			requests = append(requests, request)
			return append([]byte(nil), objects[request.PageIndex()]...), nil
		}),
		Verifier: codec,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	cursor, err := client.OpenDirectoryPages(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	for index, wantPage := range pages {
		got, present, nextErr := cursor.Next(context.Background())
		if nextErr != nil || !present || got.Commitment() != wantPage.Commitment() {
			t.Fatalf("page %d = present %v commitment %x, err %v", index, present, got.Commitment(), nextErr)
		}
	}
	if _, present, err := cursor.Next(context.Background()); err != nil || present {
		t.Fatalf("post-terminal next = present %v, err %v", present, err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	if _, present := requests[0].Generation(); present || requests[0].PageIndex() != 0 {
		t.Fatalf("first request = %#v", requests[0])
	}
	secondGeneration, present := requests[1].Generation()
	if !present || secondGeneration != want.Generation() || requests[1].PageIndex() != 1 {
		t.Fatalf("second request = %#v", requests[1])
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
}

func TestPageCursorRejectsInvalidAdmissionAndState(t *testing.T) {
	instance := shareInstance(t, 73)
	directory := directoryID(t, 74)
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			return []byte{1}, nil
		}),
		Verifier: ObjectVerifierFunc(func(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error) {
			return VerifiedObject{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.OpenDirectoryPages(cancelled, directory); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open = %v", err)
	}
	if _, err := client.OpenDirectoryPages(context.Background(), catalog.DirectoryID{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("zero-directory open = %v", err)
	}
	cursor, err := client.OpenDirectoryPages(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cursor.Next(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled next = %v", err)
	}
	client.Stop()
	if _, err := client.OpenDirectoryPages(context.Background(), directory); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("open after stop = %v", err)
	}
	if _, _, err := cursor.Next(context.Background()); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("next after stop = %v", err)
	}
	client.Stop()
	client.Close()
}

func TestPageCursorCloseCancelsItsFetchAndRejectsConcurrentNext(t *testing.T) {
	instance := shareInstance(t, 75)
	directory := directoryID(t, 76)
	started := make(chan struct{})
	var once sync.Once
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(ctx context.Context, _ ListRequest) ([]byte, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return nil, ctx.Err()
		}),
		Verifier: &countingVerifier{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	cursor, err := client.OpenDirectoryPages(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, nextErr := cursor.Next(context.Background())
		done <- nextErr
	}()
	<-started
	if _, _, err := cursor.Next(context.Background()); !errors.Is(err, ErrPageCursorState) {
		t.Fatalf("concurrent next = %v", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled fetch = %v", err)
	}
	if _, _, err := cursor.Next(context.Background()); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("next after close = %v", err)
	}
}

func TestPageCursorRejectsMutatedIdentityAndSuccessfulFetchAfterClose(t *testing.T) {
	instance := shareInstance(t, 92)
	directory := directoryID(t, 93)
	page := onePageSnapshot(t, instance, directory, 94, "file").Pages()[0]
	started := make(chan struct{})
	release := make(chan struct{})
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			close(started)
			<-release
			return []byte{1}, nil
		}),
		Verifier: ObjectVerifierFunc(func(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error) {
			return VerifiedPage(page), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	mutated := &pageCursor{client: client}
	if _, _, err := mutated.Next(context.Background()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("mutated cursor identity = %v", err)
	}
	opened, err := client.OpenDirectoryPages(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, nextErr := opened.Next(context.Background())
		done <- nextErr
	}()
	<-started
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrClientClosed) {
		t.Fatalf("successful fetch completed after cursor close: %v", err)
	}
}

func TestPageCursorSharesTheClientConcurrencyBudget(t *testing.T) {
	instance := shareInstance(t, 77)
	firstDirectory := directoryID(t, 78)
	secondDirectory := directoryID(t, 79)
	started := make(chan struct{})
	client, err := NewClient(ClientConfig{
		ShareInstance:      instance,
		MaxConcurrentLoads: 1,
		Transport: PageTransportFunc(func(ctx context.Context, _ ListRequest) ([]byte, error) {
			select {
			case <-started:
			default:
				close(started)
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}),
		Verifier: &countingVerifier{},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := client.OpenDirectoryPages(context.Background(), firstDirectory)
	second, _ := client.OpenDirectoryPages(context.Background(), secondDirectory)
	done := make(chan error, 1)
	go func() {
		_, _, nextErr := first.Next(context.Background())
		done <- nextErr
	}()
	<-started
	if _, _, err := second.Next(context.Background()); !errors.Is(err, ErrClientBudget) {
		t.Fatalf("second cursor admission = %v", err)
	}
	client.Stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped fetch = %v", err)
	}
	client.Close()
}

func TestPageCursorEnforcesPageAndObjectBoundaries(t *testing.T) {
	instance := shareInstance(t, 80)
	directory := directoryID(t, 81)
	twoPages := twoPageSnapshot(t, instance, directory, 82, "first", "second").Pages()
	codec := newMemoryObjectCodec()
	firstObject, err := codec.LoadSealedPage(context.Background(), twoPages[0])
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ShareInstance: instance, MaxPagesPerDirectory: 1,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			return append([]byte(nil), firstObject...), nil
		}),
		Verifier: codec,
	})
	if err != nil {
		t.Fatal(err)
	}
	cursor, _ := client.OpenDirectoryPages(context.Background(), directory)
	if _, present, err := cursor.Next(context.Background()); err != nil || !present {
		t.Fatalf("first page = present %v, err %v", present, err)
	}
	if _, _, err := cursor.Next(context.Background()); !errors.Is(err, ErrClientBudget) {
		t.Fatalf("page budget = %v", err)
	}
	client.Close()

	for name, object := range map[string][]byte{
		"empty":     nil,
		"oversized": make([]byte, 3),
	} {
		t.Run(name, func(t *testing.T) {
			limited, err := NewClient(ClientConfig{
				ShareInstance: instance, MaxObjectBytes: 2,
				Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
					return object, nil
				}),
				Verifier: &countingVerifier{},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(limited.Close)
			pageCursor, _ := limited.OpenDirectoryPages(context.Background(), directory)
			if _, _, err := pageCursor.Next(context.Background()); !errors.Is(err, ErrClientBudget) {
				t.Fatalf("object budget = %v", err)
			}
		})
	}
}

func TestPageCursorPropagatesTransportVerificationIdentityAndDirectoryFailures(t *testing.T) {
	instance := shareInstance(t, 83)
	directory := directoryID(t, 84)
	transportFailure := errors.New("transport unavailable")
	verificationFailure := errors.New("authentication failed")
	validPage := onePageSnapshot(t, instance, directory, 85, "file").Pages()[0]
	wrongPage := onePageSnapshot(t, instance, directoryID(t, 86), 87, "wrong").Pages()[0]
	directoryFailure := mustDirectoryFailure(t, instance, directory, 88, DirectoryCodePermanentIO, false)

	tests := []struct {
		name      string
		transport PageTransport
		verifier  ObjectVerifier
		want      error
	}{
		{
			name: "transport", want: transportFailure,
			transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
				return nil, transportFailure
			}),
			verifier: &countingVerifier{},
		},
		{
			name: "verification", want: verificationFailure,
			transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
				return []byte{1}, nil
			}),
			verifier: ObjectVerifierFunc(func(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error) {
				return VerifiedObject{}, verificationFailure
			}),
		},
		{
			name: "identity", want: ErrResponseIdentity,
			transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
				return []byte{1}, nil
			}),
			verifier: ObjectVerifierFunc(func(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error) {
				return VerifiedPage(wrongPage), nil
			}),
		},
		{
			name: "directory failure", want: directoryFailure,
			transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
				return []byte{1}, nil
			}),
			verifier: ObjectVerifierFunc(func(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error) {
				return VerifiedFailure(directoryFailure), nil
			}),
		},
		{
			name: "valid", want: nil,
			transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
				return []byte{1}, nil
			}),
			verifier: ObjectVerifierFunc(func(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error) {
				return VerifiedPage(validPage), nil
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewClient(ClientConfig{
				ShareInstance: instance, Transport: test.transport, Verifier: test.verifier,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.Close)
			cursor, err := client.OpenDirectoryPages(context.Background(), directory)
			if err != nil {
				t.Fatal(err)
			}
			_, present, err := cursor.Next(context.Background())
			if test.want == nil {
				if err != nil || !present {
					t.Fatalf("valid next = present %v, err %v", present, err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("next error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPageCursorRejectsBrokenPageChainsWithoutAdvancing(t *testing.T) {
	instance := shareInstance(t, 89)
	directory := directoryID(t, 90)
	valid := twoPageSnapshot(t, instance, directory, 91, "first", "second").Pages()
	wrongPreviousSnapshot := twoPageSnapshot(t, instance, directory, 92, "other", "tail")
	wrongPrevious := wrongPreviousSnapshot.Pages()[1]

	responses := []catalog.CatalogPage{valid[0], wrongPrevious}
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			return []byte{1}, nil
		}),
		Verifier: ObjectVerifierFunc(func(_ context.Context, _ catalog.ShareInstance, request ListRequest, _ []byte) (VerifiedObject, error) {
			page := responses[request.PageIndex()]
			if request.PageIndex() == 1 {
				// Keep the authenticated generation and request identity valid so the
				// cursor itself must reject the broken commitment chain.
				page = catalogPageWithChain(t, page, valid[0].Generation(), valid[0].PageIndex()+1, wrongPrevious.Previous())
			}
			return VerifiedPage(page), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	cursor, _ := client.OpenDirectoryPages(context.Background(), directory)
	if _, _, err := cursor.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cursor.Next(context.Background()); !errors.Is(err, ErrPageConflict) {
		t.Fatalf("broken chain = %v", err)
	}
}

func catalogPageWithChain(
	t *testing.T,
	page catalog.CatalogPage,
	generation catalog.DirectoryGeneration,
	index uint32,
	previous catalog.PageCommitment,
) catalog.CatalogPage {
	t.Helper()
	changed, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: page.ShareInstance(),
		DirectoryID:   page.DirectoryID(),
		Generation:    generation,
		PageIndex:     index,
		Previous:      previous,
		Entries:       page.Entries(),
		Terminal:      page.Terminal(),
	}, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

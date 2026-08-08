package catalogflow

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func TestListRequestCanonicalRoundTrip(t *testing.T) {
	directory := directoryID(t, 1)
	generation := generationID(t, 2)

	first, err := NewListRequest(directory, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeListRequest(first)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeListRequest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.DirectoryID() != directory || decoded.PageIndex() != 0 {
		t.Fatalf("decoded first request = %#v", decoded)
	}
	if _, present := decoded.Generation(); present {
		t.Fatal("first request unexpectedly gained a generation")
	}

	later, err := NewListRequest(directory, &generation, 7)
	if err != nil {
		t.Fatal(err)
	}
	laterBytes, err := EncodeListRequest(later)
	if err != nil {
		t.Fatal(err)
	}
	laterDecoded, err := DecodeListRequest(laterBytes)
	if err != nil {
		t.Fatal(err)
	}
	gotGeneration, present := laterDecoded.Generation()
	if !present || gotGeneration != generation || laterDecoded.PageIndex() != 7 {
		t.Fatalf("decoded later request = %#v", laterDecoded)
	}
}

func TestListRequestRejectsHostileCBOR(t *testing.T) {
	directory := directoryID(t, 3)
	request, err := NewListRequest(directory, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := EncodeListRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	for name, encoded := range map[string][]byte{
		"trailing byte": append(append([]byte(nil), canonical...), 0),
		"indefinite":    append(append([]byte{0x9f}, canonical[1:]...), 0xff),
		"wrong arity":   {0x82, 0x50, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0xf6},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeListRequest(encoded); err == nil {
				t.Fatal("hostile request was accepted")
			}
		})
	}
	if _, err := NewListRequest(directory, nil, 1); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("later page without generation error = %v", err)
	}
}

func TestSenderServiceAndClientCommitOnlyTerminalGeneration(t *testing.T) {
	instance := shareInstance(t, 4)
	directory := directoryID(t, 5)
	sibling := directoryID(t, 6)
	snapshot := twoPageSnapshot(t, instance, directory, 7, "a", "b")
	codec := newMemoryObjectCodec()
	source := &recordingSource{results: map[catalog.DirectoryID]DirectoryResult{
		directory: SnapshotResult(snapshot),
		sibling:   SnapshotResult(onePageSnapshot(t, instance, sibling, 8, "sibling")),
	}}
	service, err := NewSenderService(instance, source, codec)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	transport := &serviceTransport{service: service, beforeSecond: gate, secondReached: make(chan struct{})}
	client, err := NewClient(ClientConfig{ShareInstance: instance, Transport: transport, Verifier: codec})
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		loaded, loadErr := client.LoadDirectory(context.Background(), directory)
		if loadErr == nil && !loaded.Equal(snapshot) {
			loadErr = errors.New("client committed another snapshot")
		}
		result <- loadErr
	}()
	<-transport.secondReached
	if _, committed := client.Snapshot(directory); committed {
		t.Fatal("client exposed a generation before its terminal page")
	}
	if source.CallCount(sibling) != 0 {
		t.Fatal("loading one directory touched its sibling")
	}
	close(gate)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, committed := client.Snapshot(directory); !committed {
		t.Fatal("terminal generation was not committed")
	}
	if transport.CallCount() != 2 || source.CallCount(directory) != 2 {
		t.Fatalf("page/source calls = %d/%d", transport.CallCount(), source.CallCount(directory))
	}
	if !client.ReleaseDirectory(directory) || client.CachedBytes() != 0 {
		t.Fatal("directory cache release did not return its budget")
	}
	staleGeneration := generationID(t, 99)
	staleRequest, _ := NewListRequest(directory, &staleGeneration, 1)
	if _, err := service.Serve(context.Background(), staleRequest); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("stale generation error = %v", err)
	}
	currentGeneration := snapshot.Generation()
	outOfRange, _ := NewListRequest(directory, &currentGeneration, 9)
	if _, err := service.Serve(context.Background(), outOfRange); !errors.Is(err, ErrPageOutOfRange) {
		t.Fatalf("out-of-range page error = %v", err)
	}
}
func TestAssemblerRejectsGapConflictIdentityAndPostTerminal(t *testing.T) {
	instance := shareInstance(t, 14)
	directory := directoryID(t, 15)
	snapshot := twoPageSnapshot(t, instance, directory, 16, "a", "b")
	pages := snapshot.Pages()

	assembler, err := NewAssembler(instance, directory, 4)
	if err != nil {
		t.Fatal(err)
	}
	wrongGenerationPage := twoPageSnapshot(t, instance, directory, 99, "wrong-a", "wrong-b").Pages()[1]
	if _, err := assembler.Accept(VerifiedPage(wrongGenerationPage)); !errors.Is(err, ErrPageGap) {
		t.Fatalf("gap error = %v", err)
	}
	if status, err := assembler.Accept(VerifiedPage(pages[0])); err != nil || status != PageAccepted {
		t.Fatalf("first page = %v, %v", status, err)
	}
	if status, err := assembler.Accept(VerifiedPage(pages[0])); err != nil || status != PageReplay {
		t.Fatalf("idempotent replay = %v, %v", status, err)
	}
	conflicting := twoPageSnapshot(t, instance, directory, 16, "changed", "z").Pages()[0]
	if _, err := assembler.Accept(VerifiedPage(conflicting)); !errors.Is(err, ErrPageConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}
	if status, err := assembler.Accept(VerifiedPage(pages[1])); err != nil || status != GenerationCommitted {
		t.Fatalf("terminal page = %v, %v", status, err)
	}
	if status, err := assembler.Accept(VerifiedPage(pages[1])); err != nil || status != PageReplay {
		t.Fatalf("terminal replay = %v, %v", status, err)
	}
	extraEntry, err := catalog.NewFileEntry(fileID(t, 17), "extra", 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	extra, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: snapshot.Generation(),
		PageIndex: 2, Previous: pages[1].Commitment(), Entries: []catalog.Entry{extraEntry}, Terminal: true,
	}, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assembler.Accept(VerifiedPage(extra)); !errors.Is(err, ErrPostTerminal) {
		t.Fatalf("post-terminal append error = %v", err)
	}
	failure := mustDirectoryFailure(t, instance, directory, 18, DirectoryCodePermanentIO, false)
	if _, err := assembler.Accept(VerifiedFailure(failure)); !errors.Is(err, ErrPostTerminal) {
		t.Fatalf("post-terminal failure error = %v", err)
	}

	other := onePageSnapshot(t, instance, directoryID(t, 19), 20, "other").Pages()[0]
	otherAssembler, _ := NewAssembler(instance, directory, 2)
	if _, err := otherAssembler.Accept(VerifiedPage(other)); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("cross-directory error = %v", err)
	}
}
func TestDirectoryFailureValidation(t *testing.T) {
	directory := directoryID(t, 24)
	attempt := scanAttemptID(t, 25)
	valid := DirectoryFailure{
		ShareInstance: shareInstance(t, 26), DirectoryID: directory, AttemptID: attempt, Code: DirectoryCodeBudget,
		Retryable: true, RetryAfter: time.Second,
	}
	if _, err := NewDirectoryFailure(valid); err != nil {
		t.Fatalf("retryable budget failure = %v", err)
	}
	for name, mutate := range map[string]func(*DirectoryFailure){
		"zero attempt": func(f *DirectoryFailure) { f.AttemptID = catalog.ScanAttemptID{} },
		"wrong scope":  func(f *DirectoryFailure) { f.Code = 0x3001 },
		"short retry":  func(f *DirectoryFailure) { f.RetryAfter = time.Millisecond },
		"transient permanent": func(f *DirectoryFailure) {
			f.Code, f.Retryable, f.RetryAfter = DirectoryCodeTransientIO, false, 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if _, err := NewDirectoryFailure(candidate); err == nil {
				t.Fatal("invalid directory failure was accepted")
			}
		})
	}
}
func TestCatalogFlowConstructorsAndRequestEdgesFailClosed(t *testing.T) {
	instance := shareInstance(t, 30)
	directory := directoryID(t, 31)
	generation := generationID(t, 32)
	if _, err := NewAssembler(catalog.ShareInstance{}, directory, 1); err == nil {
		t.Fatal("assembler accepted a zero share")
	}
	if _, err := NewSenderService(instance, nil, newMemoryObjectCodec()); err == nil {
		t.Fatal("sender accepted a nil source")
	}
	if _, err := NewListRequest(catalog.DirectoryID{}, nil, 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("zero directory request = %v", err)
	}
	if _, err := NewListRequest(directory, &catalog.DirectoryGeneration{}, 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("zero generation request = %v", err)
	}
	if _, err := EncodeListRequest(ListRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("zero request encode = %v", err)
	}
	if _, err := DecodeListRequest(make([]byte, MaxListRequestBytes+1)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversize request = %v", err)
	}

	badDirectory, _ := requestEncMode.Marshal([]any{[]byte{1}, nil, uint64(0)})
	badGeneration, _ := requestEncMode.Marshal([]any{directory.Bytes(), []byte{1}, uint64(0)})
	bigPage, _ := requestEncMode.Marshal([]any{directory.Bytes(), generation.Bytes(), uint64(math.MaxUint32) + 1})
	for name, encoded := range map[string][]byte{
		"directory":  badDirectory,
		"generation": badGeneration,
		"page":       bigPage,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeListRequest(encoded); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestAssemblerRejectsUnverifiedAndCrossPageStateWithoutMutation(t *testing.T) {
	instance := shareInstance(t, 33)
	directory := directoryID(t, 34)
	snapshot := twoPageSnapshot(t, instance, directory, 35, "a", "b")
	pages := snapshot.Pages()
	assembler, _ := NewAssembler(instance, directory, 1)
	if _, err := assembler.Accept(VerifiedObject{}); !errors.Is(err, ErrUnverifiedObject) {
		t.Fatalf("empty verified object = %v", err)
	}
	failure := mustDirectoryFailure(t, instance, directory, 36, DirectoryCodePermanentIO, false)
	both := VerifiedFailure(failure)
	both.Page = pages[0]
	if _, err := assembler.Accept(both); !errors.Is(err, ErrUnverifiedObject) {
		t.Fatalf("page and failure = %v", err)
	}
	wrongShareFailure := failure
	wrongShareFailure.ShareInstance = shareInstance(t, 37)
	if _, err := assembler.Accept(VerifiedFailure(wrongShareFailure)); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("cross-share failure = %v", err)
	}
	if status, err := assembler.Accept(VerifiedPage(pages[0])); err != nil || status != PageAccepted {
		t.Fatalf("first page = %v, %v", status, err)
	}
	if assembler.PageCount() != 1 {
		t.Fatalf("page count = %d", assembler.PageCount())
	}
	if _, err := assembler.Accept(VerifiedPage(pages[1])); !errors.Is(err, ErrClientBudget) {
		t.Fatalf("page budget = %v", err)
	}
	if assembler.PageCount() != 1 {
		t.Fatal("rejected terminal mutated assembler")
	}

	full, _ := NewAssembler(instance, directory, 2)
	_, _ = full.Accept(VerifiedPage(pages[0]))
	if status, err := full.Accept(VerifiedPage(pages[1])); err != nil || status != GenerationCommitted {
		t.Fatalf("commit = %v, %v", status, err)
	}
	if _, err := full.NextRequest(); !errors.Is(err, ErrPostTerminal) {
		t.Fatalf("request after terminal = %v", err)
	}
}

func TestSenderServiceRejectsAmbiguousAndMisbindingSources(t *testing.T) {
	instance := shareInstance(t, 38)
	directory := directoryID(t, 39)
	request, _ := NewListRequest(directory, nil, 0)
	codec := newMemoryObjectCodec()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service, _ := NewSenderService(instance, &recordingSource{}, codec)
	if _, err := service.Serve(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled serve = %v", err)
	}
	if _, err := service.Serve(context.Background(), ListRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid serve request = %v", err)
	}

	loadError := DirectorySourceFunc(func(context.Context, catalog.DirectoryID) (DirectoryResult, error) {
		return DirectoryResult{}, errors.New("load failed")
	})
	service, _ = NewSenderService(instance, loadError, codec)
	if _, err := service.Serve(context.Background(), request); err == nil {
		t.Fatal("source error was hidden")
	}

	snapshot := onePageSnapshot(t, instance, directory, 40, "item")
	failure := mustDirectoryFailure(t, instance, directory, 41, DirectoryCodePermanentIO, false)
	for name, result := range map[string]DirectoryResult{
		"neither":              {},
		"both":                 {Snapshot: snapshot, Failure: &failure},
		"wrong share snapshot": SnapshotResult(onePageSnapshot(t, shareInstance(t, 42), directory, 43, "item")),
		"wrong share failure":  FailureResult(withFailure(failure, func(value *DirectoryFailure) { value.ShareInstance = shareInstance(t, 44) })),
	} {
		t.Run(name, func(t *testing.T) {
			source := DirectorySourceFunc(func(context.Context, catalog.DirectoryID) (DirectoryResult, error) { return result, nil })
			candidate, _ := NewSenderService(instance, source, codec)
			if _, err := candidate.Serve(context.Background(), request); err == nil {
				t.Fatal("invalid source result was accepted")
			}
		})
	}
}

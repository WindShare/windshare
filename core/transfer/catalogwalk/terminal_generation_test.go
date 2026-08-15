package catalogwalk

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestReadTerminalGenerationValidatesScopeCompletenessAndUsage(t *testing.T) {
	share := walkID[catalog.ShareInstance](1)
	directory := walkID[catalog.DirectoryID](2)
	file := walkID[catalog.FileID](3)
	page := walkPage(t, share, directory, []catalog.Entry{
		walkFileEntry(t, file, "file.bin"),
	}, true, 0)

	cursor := &walkCursor{pages: []catalog.CatalogPage{page}}
	limits, _ := NewLimits(1, 1, page.EstimatedMemoryBytes())
	meter, _ := NewMeter(limits)
	var visited []catalog.NodeID
	result, err := ReadTerminalGeneration(
		context.Background(), cursor, share, directory, meter,
		func(entry catalog.Entry) error {
			visited = append(visited, entry.NodeID())
			return nil
		},
	)
	if err != nil || !result.Complete || result.Exhausted != BudgetWithinLimits ||
		result.Directory.EntryCount() != 1 || len(visited) != 1 || visited[0] != file.NodeID() {
		t.Fatalf("result=%+v visited=%v err=%v", result, visited, err)
	}
	commitments := result.PageCommitments()
	if len(commitments) != 1 || commitments[0].IsZero() {
		t.Fatalf("terminal commitments = %+v", commitments)
	}
	commitments[0] = catalog.PageCommitment{}
	if result.PageCommitments()[0].IsZero() || (TerminalGeneration{}).PageCommitments() != nil {
		t.Fatal("terminal generation exposed mutable commitment state")
	}
	if !cursor.closed {
		t.Fatal("terminal generation cursor was not closed")
	}
	if got := meter.Usage(); got.AuthenticatedPages != 1 || got.Entries != 1 ||
		got.AuthenticatedMetadataBytes != page.EstimatedMemoryBytes() {
		t.Fatalf("usage=%+v", got)
	}
}

func TestReadTerminalGenerationDistinguishesEveryBudgetFromIntegrityFailure(t *testing.T) {
	share := walkID[catalog.ShareInstance](1)
	directory := walkID[catalog.DirectoryID](2)
	entry := walkFileEntry(t, walkID[catalog.FileID](3), "file.bin")
	second := walkFileEntry(t, walkID[catalog.FileID](4), "other.bin")
	page := walkPage(t, share, directory, []catalog.Entry{entry, second}, true, 0)

	for _, test := range []struct {
		name   string
		limits Limits
		want   BudgetLimit
	}{
		{
			name:   "entries",
			limits: mustWalkLimits(t, 1, 1, page.EstimatedMemoryBytes()),
			want:   BudgetEntries,
		},
		{
			name:   "metadata",
			limits: mustWalkLimits(t, 1, 2, page.EstimatedMemoryBytes()-1),
			want:   BudgetAuthenticatedMetadata,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			meter, _ := NewMeter(test.limits)
			cursor := &walkCursor{pages: []catalog.CatalogPage{page}}
			visited := 0
			result, err := ReadTerminalGeneration(
				context.Background(), cursor, share, directory, meter,
				func(catalog.Entry) error { visited++; return nil },
			)
			if err != nil || result.Exhausted != test.want || visited != 0 ||
				meter.Usage().AuthenticatedPages != 0 || !cursor.closed {
				t.Fatalf("result=%+v visited=%v usage=%+v closed=%v err=%v",
					result, visited, meter.Usage(), cursor.closed, err)
			}
		})
	}

	t.Run("omitted", func(t *testing.T) {
		omitted := walkPage(t, share, directory, []catalog.Entry{entry}, true, 1)
		meter, _ := NewMeter(mustWalkLimits(t, 1, 2, omitted.EstimatedMemoryBytes()))
		result, err := ReadTerminalGeneration(
			context.Background(), &walkCursor{pages: []catalog.CatalogPage{omitted}},
			share, directory, meter, func(catalog.Entry) error { return nil },
		)
		if err != nil || result.Complete || result.Directory.OmittedCount() != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("wrong scope", func(t *testing.T) {
		meter, _ := NewMeter(mustWalkLimits(t, 1, 1, page.EstimatedMemoryBytes()))
		wrong := walkID[catalog.DirectoryID](9)
		_, err := ReadTerminalGeneration(
			context.Background(), &walkCursor{pages: []catalog.CatalogPage{page}},
			share, wrong, meter, func(catalog.Entry) error { return nil },
		)
		if !errors.Is(err, ErrTerminalGenerationIntegrity) {
			t.Fatalf("wrong-scope err=%v", err)
		}
	})
}

func TestReadTerminalGenerationRejectsInvalidPageBeforeBudgetFallback(t *testing.T) {
	share := walkID[catalog.ShareInstance](1)
	directory := walkID[catalog.DirectoryID](2)
	page := walkPage(t, share, directory, []catalog.Entry{
		walkFileEntry(t, walkID[catalog.FileID](3), "file.bin"),
	}, true, 0)
	meter, _ := NewMeter(mustWalkLimits(t, 1, 2, page.EstimatedMemoryBytes()*2))
	cursor := &walkCursor{pages: []catalog.CatalogPage{page, page}}
	visited := 0
	result, err := ReadTerminalGeneration(
		context.Background(),
		cursor,
		share,
		directory,
		meter,
		func(catalog.Entry) error {
			visited++
			return nil
		},
	)
	if !errors.Is(err, ErrTerminalGenerationIntegrity) ||
		result != (TerminalGeneration{}) || visited != 1 || !cursor.closed {
		t.Fatalf("result=%+v visited=%v closed=%v err=%v", result, visited, cursor.closed, err)
	}
}

func TestReadTerminalGenerationPageBudgetFallsBackForAValidContinuation(t *testing.T) {
	share := walkID[catalog.ShareInstance](1)
	directory := walkID[catalog.DirectoryID](2)
	first := walkFileEntry(t, walkID[catalog.FileID](3), "first.bin")
	second := walkFileEntry(t, walkID[catalog.FileID](4), "second.bin")
	firstPage := walkSequencedPage(
		t,
		share,
		directory,
		[]catalog.Entry{first},
		0,
		catalog.PageCommitment{},
		false,
	)
	secondPage := walkSequencedPage(
		t,
		share,
		directory,
		[]catalog.Entry{second},
		1,
		firstPage.Commitment(),
		true,
	)
	meter, _ := NewMeter(mustWalkLimits(
		t,
		1,
		2,
		firstPage.EstimatedMemoryBytes()+secondPage.EstimatedMemoryBytes(),
	))
	cursor := &walkCursor{pages: []catalog.CatalogPage{firstPage, secondPage}}
	visited := 0
	result, err := ReadTerminalGeneration(
		context.Background(),
		cursor,
		share,
		directory,
		meter,
		func(catalog.Entry) error {
			visited++
			return nil
		},
	)
	if err != nil || result.Exhausted != BudgetAuthenticatedPages ||
		visited != 1 || !cursor.closed {
		t.Fatalf("result=%+v visited=%d closed=%v err=%v", result, visited, cursor.closed, err)
	}
}

func TestReadTerminalGenerationClosesOnVisitorAndTransportFailure(t *testing.T) {
	share := walkID[catalog.ShareInstance](1)
	directory := walkID[catalog.DirectoryID](2)
	page := walkPage(t, share, directory, []catalog.Entry{
		walkFileEntry(t, walkID[catalog.FileID](3), "file.bin"),
	}, true, 0)
	limits := mustWalkLimits(t, 2, 2, page.EstimatedMemoryBytes()*2)

	t.Run("visitor", func(t *testing.T) {
		meter, _ := NewMeter(limits)
		visitorErr := errors.New("visitor")
		cursor := &walkCursor{pages: []catalog.CatalogPage{page}}
		_, err := ReadTerminalGeneration(
			context.Background(), cursor, share, directory, meter,
			func(catalog.Entry) error { return visitorErr },
		)
		if !errors.Is(err, visitorErr) || !cursor.closed {
			t.Fatalf("closed=%v err=%v", cursor.closed, err)
		}
	})

	t.Run("next and close", func(t *testing.T) {
		meter, _ := NewMeter(limits)
		nextErr := errors.New("next")
		closeErr := errors.New("close")
		cursor := &walkCursor{nextErr: nextErr, closeErr: closeErr}
		_, err := ReadTerminalGeneration(
			context.Background(), cursor, share, directory, meter,
			func(catalog.Entry) error { return nil },
		)
		if !errors.Is(err, nextErr) || !errors.Is(err, closeErr) || !cursor.closed {
			t.Fatalf("closed=%v err=%v", cursor.closed, err)
		}
	})
}

func TestCatalogWalkClosedContracts(t *testing.T) {
	if !BudgetAuthenticatedPages.Valid() || BudgetWithinLimits.Valid() {
		t.Fatal("budget limit validity is not closed")
	}
	if _, ok := NewLimits(0, 1, 1); ok {
		t.Fatal("invalid limits accepted")
	}
	if meter, ok := NewMeter(Limits{}); ok || meter != nil {
		t.Fatal("invalid meter accepted")
	}
	var nilMeter *Meter
	if nilMeter.Usage() != (Usage{}) {
		t.Fatal("nil meter exposed usage")
	}
	valid := mustWalkLimits(t, 1, 1, 1)
	meter, _ := NewMeter(valid)
	_, err := ReadTerminalGeneration(
		context.Background(), nil,
		walkID[catalog.ShareInstance](1), walkID[catalog.DirectoryID](2),
		meter, func(catalog.Entry) error { return nil },
	)
	if !errors.Is(err, ErrInvalidTerminalGenerationWalk) {
		t.Fatalf("nil cursor err=%v", err)
	}
}

type walkCursor struct {
	pages    []catalog.CatalogPage
	index    int
	nextErr  error
	closeErr error
	closed   bool
}

func (cursor *walkCursor) Next(context.Context) (catalog.CatalogPage, bool, error) {
	if cursor.nextErr != nil {
		err := cursor.nextErr
		cursor.nextErr = nil
		return catalog.CatalogPage{}, false, err
	}
	if cursor.index >= len(cursor.pages) {
		return catalog.CatalogPage{}, false, nil
	}
	page := cursor.pages[cursor.index]
	cursor.index++
	return page, true, nil
}

func (cursor *walkCursor) Close() error {
	cursor.closed = true
	return cursor.closeErr
}

func walkFileEntry(t *testing.T, file catalog.FileID, name string) catalog.Entry {
	t.Helper()
	entry, err := catalog.NewFileEntry(file, name, 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func walkPage(
	t *testing.T,
	share catalog.ShareInstance,
	directory catalog.DirectoryID,
	entries []catalog.Entry,
	terminal bool,
	omitted uint64,
) catalog.CatalogPage {
	t.Helper()
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory,
		Generation: walkID[catalog.DirectoryGeneration](4),
		Entries:    entries, Terminal: terminal, OmittedCount: omitted,
	}, catalog.PageCommitterFunc(func(catalog.PageCommitInput) (catalog.PageCommitment, error) {
		return walkCommitment(5), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func walkSequencedPage(
	t *testing.T,
	share catalog.ShareInstance,
	directory catalog.DirectoryID,
	entries []catalog.Entry,
	index uint32,
	previous catalog.PageCommitment,
	terminal bool,
) catalog.CatalogPage {
	t.Helper()
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share,
		DirectoryID:   directory,
		Generation:    walkID[catalog.DirectoryGeneration](4),
		PageIndex:     index,
		Previous:      previous,
		Entries:       entries,
		Terminal:      terminal,
	}, catalog.PageCommitterFunc(func(catalog.PageCommitInput) (catalog.PageCommitment, error) {
		return walkCommitment(byte(10 + index)), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func mustWalkLimits(
	t *testing.T,
	pages, entries uint32,
	metadata uint64,
) Limits {
	t.Helper()
	limits, ok := NewLimits(pages, entries, metadata)
	if !ok {
		t.Fatal("invalid test limits")
	}
	return limits
}

func walkCommitment(seed byte) catalog.PageCommitment {
	raw := make([]byte, catalog.PageCommitmentBytes)
	for index := range raw {
		raw[index] = seed
	}
	value, _ := catalog.NewPageCommitment(raw)
	return value
}

func walkID[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var value T
	for index := range value {
		value[index] = seed
	}
	return value
}

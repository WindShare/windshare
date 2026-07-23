package catalogflow

import (
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
)

type AcceptStatus uint8

const (
	PageAccepted AcceptStatus = iota + 1
	PageReplay
	GenerationCommitted
)

type Assembler struct {
	shareInstance catalog.ShareInstance
	directoryID   catalog.DirectoryID
	maxPages      uint32
	generation    catalog.DirectoryGeneration
	pages         []catalog.CatalogPage
	snapshot      catalog.DirectorySnapshot
	terminal      bool
}

func NewAssembler(instance catalog.ShareInstance, directory catalog.DirectoryID, maxPages uint32) (*Assembler, error) {
	if instance.IsZero() || directory.IsZero() || maxPages == 0 {
		return nil, errors.New("catalog assembler requires identities and a positive page budget")
	}
	return &Assembler{shareInstance: instance, directoryID: directory, maxPages: maxPages}, nil
}

func (a *Assembler) Accept(object VerifiedObject) (AcceptStatus, error) {
	if object.Failure != nil {
		return a.acceptFailure(object)
	}
	return a.acceptPage(object.Page)
}

func (a *Assembler) acceptFailure(object VerifiedObject) (AcceptStatus, error) {
	if a.terminal {
		return 0, ErrPostTerminal
	}
	if len(a.pages) != 0 {
		return 0, ErrPageConflict
	}
	if !object.Page.ShareInstance().IsZero() {
		return 0, ErrUnverifiedObject
	}
	failure, err := NewDirectoryFailure(*object.Failure)
	if err != nil {
		return 0, err
	}
	if failure.ShareInstance != a.shareInstance || failure.DirectoryID != a.directoryID {
		return 0, ErrObjectIdentity
	}
	return 0, failure
}

func (a *Assembler) acceptPage(page catalog.CatalogPage) (AcceptStatus, error) {
	if page.ShareInstance().IsZero() {
		return 0, ErrUnverifiedObject
	}
	if page.ShareInstance() != a.shareInstance || page.DirectoryID() != a.directoryID {
		return 0, ErrObjectIdentity
	}
	if !a.generation.IsZero() && page.Generation() != a.generation {
		return 0, ErrObjectIdentity
	}

	index := page.PageIndex()
	if a.generation.IsZero() && index != 0 {
		return 0, ErrPageGap
	}
	if uint64(index) < uint64(len(a.pages)) {
		return a.acceptReplay(index, page)
	}
	return a.acceptNewPage(index, page)
}

func (a *Assembler) acceptReplay(index uint32, page catalog.CatalogPage) (AcceptStatus, error) {
	committed := a.pages[index]
	if committed.Commitment() != page.Commitment() || committed.Terminal() != page.Terminal() {
		return 0, ErrPageConflict
	}
	return PageReplay, nil
}

func (a *Assembler) acceptNewPage(index uint32, page catalog.CatalogPage) (AcceptStatus, error) {
	if a.terminal {
		return 0, ErrPostTerminal
	}
	if uint64(index) != uint64(len(a.pages)) {
		return 0, ErrPageGap
	}
	if index >= a.maxPages {
		return 0, ErrClientBudget
	}
	if index == 0 {
		if !page.Previous().IsZero() {
			return 0, fmt.Errorf("%w: first page has a predecessor", ErrPageConflict)
		}
	} else if page.Previous() != a.pages[index-1].Commitment() {
		return 0, ErrPageConflict
	}
	if !page.Terminal() {
		a.pages = append(a.pages, page)
		if a.generation.IsZero() {
			a.generation = page.Generation()
		}
		return PageAccepted, nil
	}
	candidate := append(append([]catalog.CatalogPage(nil), a.pages...), page)
	snapshot, err := catalog.NewDirectorySnapshot(candidate)
	if err != nil {
		return 0, fmt.Errorf("commit verified catalog generation: %w", err)
	}
	a.pages = candidate
	if a.generation.IsZero() {
		a.generation = page.Generation()
	}
	a.snapshot = snapshot
	a.terminal = true
	return GenerationCommitted, nil
}

func (a *Assembler) NextRequest() (ListRequest, error) {
	if a.terminal {
		return ListRequest{}, ErrPostTerminal
	}
	index := uint32(len(a.pages))
	if index == 0 {
		return NewListRequest(a.directoryID, nil, 0)
	}
	return NewListRequest(a.directoryID, &a.generation, index)
}

func (a *Assembler) Snapshot() (catalog.DirectorySnapshot, bool) {
	return a.snapshot, a.terminal
}

func (a *Assembler) PageCount() int { return len(a.pages) }

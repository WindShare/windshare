// Package catalogwalk owns streaming validation of authenticated terminal
// catalog generations. Callers reduce entries into bounded facts and discard
// those provisional facts unless the walk returns a complete generation.
package catalogwalk

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
)

var (
	ErrInvalidTerminalGenerationWalk = errors.New("terminal catalog generation walk is invalid")
	ErrTerminalGenerationIntegrity   = errors.New("terminal catalog generation failed integrity validation")
)

// BudgetLimit identifies the local resource dimension that stopped a walk.
// Exhaustion is proof inability, not catalog corruption.
type BudgetLimit uint8

const (
	BudgetWithinLimits BudgetLimit = iota
	BudgetAuthenticatedPages
	BudgetEntries
	BudgetAuthenticatedMetadata
)

func (limit BudgetLimit) Valid() bool {
	return limit >= BudgetAuthenticatedPages && limit <= BudgetAuthenticatedMetadata
}

// Limits bounds aggregate accepted work across any number of directory walks.
type Limits struct {
	authenticatedPages    uint32
	entries               uint32
	authenticatedMetadata uint64
}

func NewLimits(authenticatedPages, entries uint32, authenticatedMetadata uint64) (Limits, bool) {
	limits := Limits{
		authenticatedPages:    authenticatedPages,
		entries:               entries,
		authenticatedMetadata: authenticatedMetadata,
	}
	return limits, limits.valid()
}

func (limits Limits) valid() bool {
	return limits.authenticatedPages > 0 && limits.entries > 0 && limits.authenticatedMetadata > 0
}

// Usage reports only pages accepted into semantic validation. A page that
// crosses a local bound is released by the cursor without becoming proof input;
// metadata bytes charge its bounded decoded catalog representation.
type Usage struct {
	AuthenticatedPages         uint32
	Entries                    uint32
	AuthenticatedMetadataBytes uint64
}

// Meter applies one aggregate limit across a multi-directory proof.
type Meter struct {
	limits Limits
	usage  Usage
}

func NewMeter(limits Limits) (*Meter, bool) {
	if !limits.valid() {
		return nil, false
	}
	return &Meter{limits: limits}, true
}

func (meter *Meter) Usage() Usage {
	if meter == nil {
		return Usage{}
	}
	return meter.usage
}

func (meter *Meter) consume(page catalog.CatalogPage) BudgetLimit {
	if meter == nil {
		return BudgetAuthenticatedPages
	}
	pageEntries := uint32(page.EntryCount())
	pageMetadata := page.EstimatedMemoryBytes()
	switch {
	case meter.usage.AuthenticatedPages >= meter.limits.authenticatedPages:
		return BudgetAuthenticatedPages
	case pageEntries > meter.limits.entries-meter.usage.Entries:
		return BudgetEntries
	case pageMetadata > meter.limits.authenticatedMetadata-meter.usage.AuthenticatedMetadataBytes:
		return BudgetAuthenticatedMetadata
	default:
		meter.usage.AuthenticatedPages++
		meter.usage.Entries += pageEntries
		meter.usage.AuthenticatedMetadataBytes += pageMetadata
		return BudgetWithinLimits
	}
}

// TerminalGeneration is usable proof only when Complete is true and Exhausted
// is zero. Omitted children are valid catalog state but cannot prove a shape.
type TerminalGeneration struct {
	Directory   catalog.CommittedDirectory
	commitments *terminalGenerationCommitments
	Complete    bool
	Exhausted   BudgetLimit
}

type terminalGenerationCommitments struct {
	values []catalog.PageCommitment
}

// PageCommitments returns the authenticated chain needed to replay the same
// generation without exposing mutable walker state.
func (generation TerminalGeneration) PageCommitments() []catalog.PageCommitment {
	if generation.commitments == nil {
		return nil
	}
	return append([]catalog.PageCommitment(nil), generation.commitments.values...)
}

// ReadTerminalGeneration validates one forward-only generation against its
// expected authenticated scope. visit must only update disposable, bounded
// reducer state: entries remain provisional until this function returns a
// complete generation.
func ReadTerminalGeneration(
	ctx context.Context,
	cursor catalog.DirectoryPageCursor,
	expectedShare catalog.ShareInstance,
	expectedDirectory catalog.DirectoryID,
	meter *Meter,
	visit func(catalog.Entry) error,
) (result TerminalGeneration, resultErr error) {
	if ctx == nil || cursor == nil || expectedShare.IsZero() || expectedDirectory.IsZero() ||
		meter == nil || visit == nil {
		return TerminalGeneration{}, ErrInvalidTerminalGenerationWalk
	}
	defer func() {
		resultErr = terminalCursorCloseError(resultErr, cursor.Close())
	}()

	var validator catalog.DirectoryGenerationValidator
	commitments := make([]catalog.PageCommitment, 0, 1)
	for {
		page, ok, err := cursor.Next(ctx)
		if err != nil {
			return TerminalGeneration{}, err
		}
		if !ok {
			return finishTerminalGeneration(
				&validator, expectedShare, expectedDirectory, commitments,
			)
		}
		if err := acceptTerminalPage(&validator, page, expectedShare, expectedDirectory); err != nil {
			return TerminalGeneration{}, err
		}
		if limit := meter.consume(page); limit != BudgetWithinLimits {
			return TerminalGeneration{Exhausted: limit}, nil
		}
		commitments = append(commitments, page.Commitment())
		if err := visitTerminalPageEntries(ctx, page, visit); err != nil {
			return TerminalGeneration{}, err
		}
	}
}

func terminalCursorCloseError(resultErr, closeErr error) error {
	if resultErr == nil {
		return closeErr
	}
	if closeErr != nil {
		return errors.Join(resultErr, closeErr)
	}
	return resultErr
}

func finishTerminalGeneration(
	validator *catalog.DirectoryGenerationValidator,
	expectedShare catalog.ShareInstance,
	expectedDirectory catalog.DirectoryID,
	commitments []catalog.PageCommitment,
) (TerminalGeneration, error) {
	committed, err := validator.Finish()
	if err != nil {
		return TerminalGeneration{}, errors.Join(ErrTerminalGenerationIntegrity, err)
	}
	if committed.ShareInstance() != expectedShare || committed.DirectoryID() != expectedDirectory {
		return TerminalGeneration{}, ErrTerminalGenerationIntegrity
	}
	return TerminalGeneration{
		Directory: committed,
		commitments: &terminalGenerationCommitments{
			values: append([]catalog.PageCommitment(nil), commitments...),
		},
		Complete: committed.OmittedCount() == 0,
	}, nil
}

func acceptTerminalPage(
	validator *catalog.DirectoryGenerationValidator,
	page catalog.CatalogPage,
	expectedShare catalog.ShareInstance,
	expectedDirectory catalog.DirectoryID,
) error {
	if page.ShareInstance() != expectedShare || page.DirectoryID() != expectedDirectory {
		return fmt.Errorf(
			"%w: page scope differs from the requested directory",
			ErrTerminalGenerationIntegrity,
		)
	}
	if err := validator.AcceptPage(page); err != nil {
		return errors.Join(ErrTerminalGenerationIntegrity, err)
	}
	return nil
}

func visitTerminalPageEntries(
	ctx context.Context,
	page catalog.CatalogPage,
	visit func(catalog.Entry) error,
) error {
	for entryIndex := 0; entryIndex < page.EntryCount(); entryIndex++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry, exists := page.Entry(uint32(entryIndex))
		if !exists {
			return ErrTerminalGenerationIntegrity
		}
		if err := visit(entry); err != nil {
			return err
		}
	}
	return nil
}

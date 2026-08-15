// Package ordinaryoutput owns construction-time policy for ordinary native
// receive output. It deliberately contains no persisted receive contract.
package ordinaryoutput

import "github.com/windshare/windshare/core/catalog"

const (
	// OrdinaryOutputPolicyVersion separates active operations when any ordinary
	// layout, lookup, suffix, reservation, cleanup, or shape-budget semantic changes.
	OrdinaryOutputPolicyVersion = uint8(1)

	// MaximumResultNameReservationAttemptsV1 admits collision indexes 0 through 31.
	MaximumResultNameReservationAttemptsV1 = uint32(32)
	// MaximumLiveCleanupTicketsV1 bounds private cleanup inspection during binding.
	MaximumLiveCleanupTicketsV1 = uint32(256)
)

// ShapeProbeBudget bounds authenticated metadata work before shape resolution
// freezes the synthetic ordinary-output fallback.
type ShapeProbeBudget struct {
	directoryRequests     uint32
	authenticatedPages    uint32
	entries               uint32
	authenticatedMetadata uint64
	depth                 uint32
}

func NewShapeProbeBudget(
	directoryRequests uint32,
	authenticatedPages uint32,
	entries uint32,
	authenticatedMetadata uint64,
	depth uint32,
) (ShapeProbeBudget, bool) {
	budget := ShapeProbeBudget{
		directoryRequests: directoryRequests, authenticatedPages: authenticatedPages,
		entries: entries, authenticatedMetadata: authenticatedMetadata, depth: depth,
	}
	return budget, budget.Valid()
}

func (budget ShapeProbeBudget) DirectoryRequests() uint32  { return budget.directoryRequests }
func (budget ShapeProbeBudget) AuthenticatedPages() uint32 { return budget.authenticatedPages }
func (budget ShapeProbeBudget) Entries() uint32            { return budget.entries }
func (budget ShapeProbeBudget) AuthenticatedMetadataBytes() uint64 {
	return budget.authenticatedMetadata
}
func (budget ShapeProbeBudget) Depth() uint32 { return budget.depth }

func (budget ShapeProbeBudget) Valid() bool {
	return budget.directoryRequests > 0 && budget.authenticatedPages > 0 &&
		budget.entries > 0 && budget.authenticatedMetadata > 0 &&
		budget.depth > 0 && budget.depth <= uint32(catalog.MaxPathDepth)
}

var DefaultShapeProbeBudgetV1 = mustShapeProbeBudget(
	32,
	128,
	32_768,
	8*1024*1024,
	catalog.MaxPathDepth,
)

func mustShapeProbeBudget(
	directoryRequests uint32,
	authenticatedPages uint32,
	entries uint32,
	authenticatedMetadata uint64,
	depth int,
) ShapeProbeBudget {
	budget, ok := NewShapeProbeBudget(
		directoryRequests, authenticatedPages, entries, authenticatedMetadata, uint32(depth),
	)
	if !ok {
		panic("ordinary output default shape probe budget is invalid")
	}
	return budget
}

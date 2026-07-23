package transfer

import (
	"errors"

	"github.com/windshare/windshare/core/catalog"
)

const (
	// A claim charges the 16-byte identity plus conservative Go map bucket,
	// occupancy, and allocator overhead. Deriving the count from the existing
	// catalog-session memory budget keeps cross-branch duplicate detection hard
	// bounded without weakening authenticated identity checks.
	selectionIdentityClaimMemoryBytes = 64
	maxSelectionIdentityClaims        = int(catalog.DefaultSessionCatalogMemory / selectionIdentityClaimMemoryBytes)
)

var ErrSelectionIdentityBudget = errors.New("transfer selection identity budget exceeded")

type selectionIdentityClaims struct {
	seen map[catalog.NodeID]struct{}
	max  int
}

func newSelectionIdentityClaims(root catalog.DirectoryID) *selectionIdentityClaims {
	claims := &selectionIdentityClaims{
		seen: make(map[catalog.NodeID]struct{}), max: maxSelectionIdentityClaims,
	}
	claims.seen[root.NodeID()] = struct{}{}
	return claims
}

func (claims *selectionIdentityClaims) claim(node catalog.NodeID) error {
	if _, duplicate := claims.seen[node]; duplicate {
		return NewSessionFailure(ErrCatalogIdentity)
	}
	if len(claims.seen) >= claims.max {
		return NewJobResourceBudgetError(ErrSelectionIdentityBudget)
	}
	claims.seen[node] = struct{}{}
	return nil
}

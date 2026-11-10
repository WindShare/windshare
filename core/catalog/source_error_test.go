package catalog

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestSelectionNameCollisionPreservesBothLabelsWithoutClaimingCatalogCorruption(t *testing.T) {
	collision := &SelectionNameCollisionError{FirstLabel: "a/notes.txt", SecondLabel: "b/notes.txt"}
	wrapped := fmt.Errorf("open selected sources: %w", collision)
	var selected *SelectionNameCollisionError
	if !errors.As(wrapped, &selected) || selected != collision || errors.Is(wrapped, ErrSiblingCollision) {
		t.Fatalf("selection collision lost its distinct meaning: %v", wrapped)
	}
	for _, label := range []string{collision.FirstLabel, collision.SecondLabel} {
		if !strings.Contains(wrapped.Error(), strconv.Quote(label)) {
			t.Fatalf("diagnostic omitted selected label %q: %v", label, wrapped)
		}
	}
}

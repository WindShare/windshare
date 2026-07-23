package protocolcontract

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const vectorInventoryFile = "inventory.txt"

func TestVectorInventoryIsExact(t *testing.T) {
	inventoryPath := filepath.Join(vectorsDir, vectorInventoryFile)
	encoded, err := os.ReadFile(inventoryPath)
	if err != nil {
		t.Fatalf("read vector inventory: %v", err)
	}
	var expected []string
	for line := range strings.SplitSeq(string(encoded), "\n") {
		name := strings.TrimSpace(line)
		if name != "" && !strings.HasPrefix(name, "#") {
			expected = append(expected, name)
		}
	}
	if !slices.IsSorted(expected) {
		t.Fatalf("%s must stay sorted", inventoryPath)
	}
	if len(slices.Compact(slices.Clone(expected))) != len(expected) {
		t.Fatalf("%s contains duplicate filenames", inventoryPath)
	}

	entries, err := os.ReadDir(vectorsDir)
	if err != nil {
		t.Fatalf("read vector directory: %v", err)
	}
	var actual []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			actual = append(actual, entry.Name())
		}
	}
	slices.Sort(actual)
	if !slices.Equal(actual, expected) {
		t.Fatalf("JSON vector inventory mismatch\nactual:   %v\nexpected: %v", actual, expected)
	}
}

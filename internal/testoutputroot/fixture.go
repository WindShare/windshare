package testoutputroot

import (
	"path/filepath"
	"testing"
)

const outputDirectoryName = "output"

// Fixture keeps cleanup ownership with testing.TB while leaving the output
// directory absent so the production authority must create and certify it.
type Fixture struct {
	RootPath   string
	CreateRoot bool
}

func New(t testing.TB) Fixture {
	t.Helper()
	placement := newCertifiedPlacement(t)
	return Fixture{
		RootPath:   filepath.Join(placement, outputDirectoryName),
		CreateRoot: true,
	}
}

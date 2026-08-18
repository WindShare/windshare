package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const modulePath = "github.com/windshare/windshare"

func TestDerivePackageSetsAssignsEveryPackageDeterministically(t *testing.T) {
	t.Parallel()

	sets, err := derivePackageSets(
		[]string{
			modulePath + "/integration/v2peer",
			modulePath + "/core/session",
			modulePath + "/future",
			modulePath + "/core/catalog",
			modulePath + "/e2e",
		},
		[]string{
			modulePath + "/core/session",
			modulePath + "/core/catalog",
		},
	)
	if err != nil {
		t.Fatalf("derive package sets: %v", err)
	}

	assertStringsEqual(t, sets.all, []string{
		modulePath + "/core/catalog",
		modulePath + "/core/session",
		modulePath + "/e2e",
		modulePath + "/future",
		modulePath + "/integration/v2peer",
	})
	assertStringsEqual(t, sets.core, []string{
		modulePath + "/core/catalog",
		modulePath + "/core/session",
	})
	assertStringsEqual(t, sets.nonCore, []string{
		modulePath + "/e2e",
		modulePath + "/future",
		modulePath + "/integration/v2peer",
	})
}

func TestDerivePackageSetsRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		all        []string
		core       []string
		wantDetail string
	}{
		{
			name:       "duplicate all package",
			all:        []string{modulePath + "/cmd/wind", modulePath + "/cmd/wind"},
			core:       []string{modulePath + "/core/session"},
			wantDetail: "all package set contains duplicate",
		},
		{
			name:       "duplicate core package",
			all:        []string{modulePath + "/cmd/wind", modulePath + "/core/session"},
			core:       []string{modulePath + "/core/session", modulePath + "/core/session"},
			wantDetail: "core package set contains duplicate",
		},
		{
			name:       "core outside universe",
			all:        []string{modulePath + "/cmd/wind"},
			core:       []string{modulePath + "/core/session"},
			wantDetail: "absent from the production package universe",
		},
		{
			name:       "empty core",
			all:        []string{modulePath + "/cmd/wind"},
			core:       nil,
			wantDetail: "core package set is empty",
		},
		{
			name:       "empty non-core",
			all:        []string{modulePath + "/core/session"},
			core:       []string{modulePath + "/core/session"},
			wantDetail: "non-core package set is empty",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := derivePackageSets(test.all, test.core)
			if err == nil || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("derivePackageSets() error = %v, want detail %q", err, test.wantDetail)
			}
		})
	}
}

func TestValidateOwnershipRejectsOverlapAndIncompleteUnion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sets       packageSets
		wantDetail string
	}{
		{
			name: "intersection",
			sets: packageSets{
				all:     []string{"a", "b"},
				core:    []string{"a"},
				nonCore: []string{"a", "b"},
			},
			wantDetail: "belongs to both",
		},
		{
			name: "missing owner",
			sets: packageSets{
				all:     []string{"a", "b"},
				core:    []string{"a"},
				nonCore: nil,
			},
			wantDetail: "has no validation owner",
		},
		{
			name: "owner outside universe",
			sets: packageSets{
				all:     []string{"a"},
				core:    []string{"a"},
				nonCore: []string{"b"},
			},
			wantDetail: "outside the production package universe",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := test.sets.validateOwnership()
			if err == nil || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("validateOwnership() error = %v, want detail %q", err, test.wantDetail)
			}
		})
	}
}

func TestLoadPackageSetsUsesOnlyProductionUniversePatterns(t *testing.T) {
	t.Parallel()

	layout := &recordingModuleLayout{}
	lister := &recordingLister{responses: map[string][]string{
		allPattern:  {modulePath + "/cmd/wind", modulePath + "/core/session"},
		corePattern: {modulePath + "/core/session"},
	}}
	sets, err := loadPackageSets(context.Background(), layout, lister)
	if err != nil {
		t.Fatalf("load package sets: %v", err)
	}

	if layout.calls != 1 {
		t.Fatalf("module layout validation calls = %d, want 1", layout.calls)
	}
	if !reflect.DeepEqual(lister.patterns, []string{allPattern, corePattern}) {
		t.Fatalf("patterns = %v, want [%q %q]", lister.patterns, allPattern, corePattern)
	}
	assertStringsEqual(t, sets.nonCore, []string{modulePath + "/cmd/wind"})
}

func TestLoadPackageSetsFailsClosedBeforeListingAnUnapprovedLayout(t *testing.T) {
	t.Parallel()

	layout := &recordingModuleLayout{err: errors.New("unapproved nested module")}
	lister := &recordingLister{}
	_, err := loadPackageSets(context.Background(), layout, lister)
	if err == nil || !strings.Contains(err.Error(), "validate repository module layout") {
		t.Fatalf("loadPackageSets() error = %v, want module layout failure", err)
	}
	if len(lister.patterns) != 0 {
		t.Fatalf("go list patterns = %v, want none before layout validation", lister.patterns)
	}
}

func TestValidateModuleLayoutAllowsOnlyReviewedModuleBoundaries(t *testing.T) {
	t.Parallel()

	metadataPaths := []string{
		"go.mod",
		"go.sum",
		"internal/perfevidence/go.mod",
		"internal/perfevidence/go.sum",
		"spikes/webrtc/go.mod",
		"spikes/webrtc/go.sum",
		"README.md",
	}
	if err := validateModuleLayout(metadataPaths, validRootGoMod()); err != nil {
		t.Fatalf("validateModuleLayout() error = %v", err)
	}
}

func TestValidateModuleLayoutRejectsUnownedMetadataAndRetiredCoreRequirement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		metadataPaths []string
		rootGoMod     []byte
		wantDetail    string
	}{
		{
			name:          "unapproved nested module",
			metadataPaths: []string{"go.mod", "future/go.mod"},
			rootGoMod:     validRootGoMod(),
			wantDetail:    `unapproved module or workspace metadata "future/go.mod"`,
		},
		{
			name:          "workspace file",
			metadataPaths: []string{"go.mod", "go.work"},
			rootGoMod:     validRootGoMod(),
			wantDetail:    `unapproved module or workspace metadata "go.work"`,
		},
		{
			name:          "retired core module",
			metadataPaths: []string{"go.mod", "core\\go.mod"},
			rootGoMod:     validRootGoMod(),
			wantDetail:    `unapproved module or workspace metadata "core/go.mod"`,
		},
		{
			name:          "retired core checksum",
			metadataPaths: []string{"go.mod", "core/go.sum"},
			rootGoMod:     validRootGoMod(),
			wantDetail:    `unapproved module or workspace metadata "core/go.sum"`,
		},
		{
			name:          "retired root requirement",
			metadataPaths: []string{"go.mod"},
			rootGoMod: []byte(
				"module " + modulePath + "\n\n" +
					"go 1.26.5\n\n" +
					"require " + modulePath + "/core v0.0.0\n",
			),
			wantDetail: "retired core module requirement",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateModuleLayout(test.metadataPaths, test.rootGoMod)
			if err == nil || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("validateModuleLayout() error = %v, want detail %q", err, test.wantDetail)
			}
		})
	}
}

func TestEnvironmentWithGOWORKOffReplacesAmbientWorkspace(t *testing.T) {
	t.Parallel()

	got := environmentWithGOWORKOff([]string{
		"PATH=tools",
		"GOWORK=parent.work",
		"GoWoRk=second.work",
	})
	assertStringsEqual(t, got, []string{"PATH=tools", "GOWORK=off"})
}

type recordingLister struct {
	responses map[string][]string
	patterns  []string
}

func (lister *recordingLister) list(_ context.Context, pattern string) ([]string, error) {
	lister.patterns = append(lister.patterns, pattern)
	return append([]string(nil), lister.responses[pattern]...), nil
}

type recordingModuleLayout struct {
	calls int
	err   error
}

func (layout *recordingModuleLayout) validate(context.Context) error {
	layout.calls++
	return layout.err
}

func validRootGoMod() []byte {
	return []byte("module " + modulePath + "\n\ngo 1.26.5\n")
}

func assertStringsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

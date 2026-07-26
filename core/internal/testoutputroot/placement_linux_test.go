//go:build linux

package testoutputroot

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLinuxPlacementIsPrivateHomeChildWithOwnedCleanup(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home = filepath.Clean(home)
	placements := make([]string, 0, 2)
	t.Run("placements", func(t *testing.T) {
		first := New(t)
		second := New(t)
		for _, fixture := range []Fixture{first, second} {
			placement := filepath.Dir(fixture.RootPath)
			placements = append(placements, placement)
			if !filepath.IsAbs(placement) || filepath.Dir(placement) != home {
				t.Fatalf("Linux placement %q is not a direct absolute child of home %q", placement, home)
			}
			info, statErr := os.Stat(placement)
			if statErr != nil {
				t.Fatalf("inspect Linux placement: %v", statErr)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || !info.IsDir() || info.Mode().Perm() != 0o700 || stat.Uid != uint32(os.Geteuid()) {
				t.Fatalf("Linux placement is not a receiver-owned 0700 directory: info=%v stat=%v error=%v", info, stat, statErr)
			}
			if _, statErr := os.Lstat(fixture.RootPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("production output leaf exists before creation: %v", statErr)
			}
		}
		if placements[0] == placements[1] {
			t.Fatal("unique Linux placements reused a path")
		}
	})
	for _, placement := range placements {
		if _, err := os.Lstat(placement); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("test-owned Linux placement survived cleanup: %v", err)
		}
	}
}

func TestProvisionLinuxPlacementReportsExactBoundaryFailure(t *testing.T) {
	cause := errors.New("fixture failure")
	tests := []struct {
		name     string
		host     linuxPlacementHost
		wantPath string
	}{
		{
			name: "home",
			host: linuxPlacementHost{
				homeDirectory: func() (string, error) { return "", cause },
			},
		},
		{
			name: "create",
			host: linuxPlacementHost{
				homeDirectory:   func() (string, error) { return "/receiver", nil },
				createDirectory: func(string, string) (string, error) { return "", cause },
			},
		},
		{
			name: "protect",
			host: linuxPlacementHost{
				homeDirectory:   func() (string, error) { return "/receiver", nil },
				createDirectory: func(string, string) (string, error) { return "/receiver/placement", nil },
				protect:         func(string, os.FileMode) error { return cause },
			},
			wantPath: "/receiver/placement",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, err := provisionLinuxPlacement(test.host)
			if path != test.wantPath || !errors.Is(err, cause) {
				t.Fatalf("provision failure = (%q, %v), want (%q, wrapped cause)", path, err, test.wantPath)
			}
		})
	}
}

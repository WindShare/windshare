//go:build windows

package outputportable

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func createDirectoryJunction(t *testing.T, link, target string) {
	t.Helper()
	output, err := exec.Command("cmd", "/d", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J %q %q: %v (%s)", link, target, err, strings.TrimSpace(string(output)))
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

func TestCallerWindowsJunctionContainerOpensItsTargetWithoutFollowingInnerJunctions(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	elsewhere := filepath.Join(target, "elsewhere")
	for _, dir := range []string{target, elsewhere} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	container := filepath.Join(base, "downloads-junction")
	createDirectoryJunction(t, container, target)
	createDirectoryJunction(t, filepath.Join(target, "inner-junction"), elsewhere)

	for _, create := range []bool{false, true} {
		p, err := Open(container, create)
		if err != nil {
			t.Fatalf("Open(junction container, create=%t) = %v", create, err)
		}
		if p.RootOpenDisposition() != outputcap.CallerProvidedContainer {
			t.Fatalf("existing junction container disposition = %v", p.RootOpenDisposition())
		}
		d := p.Root().(*directory)
		info, err := os.Stat(target)
		if err != nil || !os.SameFile(d.info, info) {
			t.Fatalf("root is not pinned to the junction target: %v", err)
		}
		duplicate, err := d.Duplicate()
		if err != nil {
			t.Fatalf("duplicate pinned root: %v", err)
		}
		if err := duplicate.Close(); err != nil {
			t.Fatal(err)
		}
		if inner, err := d.OpenDirectory("inner-junction", false); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			if inner != nil {
				_ = inner.Close()
			}
			t.Fatalf("inner junction followed: %v", err)
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWindowsJunctionContainerRequiresAgreeingKernelResolution(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	other := filepath.Join(base, "other")
	gone := filepath.Join(base, "gone")
	for _, dir := range []string{target, other, gone} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	container := filepath.Join(base, "downloads-junction")
	createDirectoryJunction(t, container, target)
	dangling := filepath.Join(base, "dangling-junction")
	createDirectoryJunction(t, dangling, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	refusal := errors.New("kernel refused the junction")
	cases := []struct {
		name   string
		path   string
		follow func(string) (os.FileInfo, error)
	}{
		{"dangling", dangling, os.Stat},
		{"kernel refusal", container, func(string) (os.FileInfo, error) { return nil, refusal }},
		{"diverging target", container, func(string) (os.FileInfo, error) { return os.Stat(other) }},
	}
	t.Run("cases", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				for _, create := range []bool{false, true} {
					p, err := openContainer(tc.path, create, tc.follow)
					if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
						if p != nil {
							_ = p.Close()
						}
						t.Fatalf("openContainer(create=%t) = %v, want ErrUnsafeNamespace", create, err)
					}
				}
			})
		}
	})
	if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling junction recreated its target: %v", err)
	}
	if entries, err := os.ReadDir(other); err != nil || len(entries) != 0 {
		t.Fatalf("refused junction changed another directory: %v %v", entries, err)
	}
}

func TestNormalizeWindowsFinalPath(t *testing.T) {
	longPath := `\\?\C:\` + strings.Repeat("a", windowsShortFinalPathLimit)
	longUNC := `\\?\UNC\server\share\` + strings.Repeat("b", windowsShortFinalPathLimit)
	cases := []struct {
		input string
		want  string
	}{
		{`\\?\D:\Downloads`, `D:\Downloads`},
		{`\\?\D:\`, `D:\`},
		{`\\?\UNC\server\share\dir`, `\\server\share\dir`},
		{longPath, longPath},
		{longUNC, longUNC},
		{`\\?\Volume{01234567-89ab-cdef-0123-456789abcdef}\`, `\\?\Volume{01234567-89ab-cdef-0123-456789abcdef}\`},
	}
	for _, tc := range cases {
		if got := normalizeWindowsFinalPath(tc.input); got != tc.want {
			t.Fatalf("normalizeWindowsFinalPath(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

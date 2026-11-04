package platformsetup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRulePathCanonicalization(t *testing.T) {
	// Windows runners may supply TEMP with an 8.3 parent component. Establish
	// the long fixture path before comparing it with the short alias.
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "installed application.exe")
	if err := os.WriteFile(executable, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32768)
	if _, err := windows.GetShortPathName(path, &buffer[0], uint32(len(buffer))); err != nil {
		t.Fatal(err)
	}
	canonical, err := longInstallationPath(windows.UTF16ToString(buffer))
	if err != nil || !strings.EqualFold(canonical, executable) {
		t.Fatal(canonical, err)
	}
	linked := filepath.Join(filepath.Dir(executable), "linked.exe")
	if err := os.Link(executable, linked); err != nil {
		t.Fatal(err)
	}
	other, err := longInstallationPath(linked)
	if err != nil || strings.EqualFold(other, canonical) {
		t.Fatal("path owners merged", other, err)
	}
	for _, invalid := range []string{"missing.exe", "invalid\x00path"} {
		if _, err := longInstallationPath(invalid); err == nil {
			t.Fatal("invalid path accepted")
		}
	}
}

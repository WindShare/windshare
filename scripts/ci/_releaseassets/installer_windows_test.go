package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsInstallerShortTemporaryPath(t *testing.T) {
	root := t.TempDir()
	path, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	size, err := windows.GetShortPathName(path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, size)
	if _, err := windows.GetShortPathName(path, &buffer[0], size); err != nil {
		t.Fatal(err)
	}
	shortRoot := windows.UTF16ToString(buffer)
	if strings.EqualFold(shortRoot, root) {
		t.Skip("temporary volume does not provide an 8.3 alias")
	}
	shell, err := exec.LookPath("pwsh")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	// A binary fixture reproduces the runner's aliases without another Go build.
	platform := installerPlatform{"windows", shell, "scripts/install/windows/install.ps1", "wind.exe"}
	testInstallerFixture(t, repository, platform, "binary-bundle", shortRoot)
}

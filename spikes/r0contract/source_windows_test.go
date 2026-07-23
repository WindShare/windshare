//go:build windows

package r0contract

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

type fileIdentity struct {
	volume uint32
	index  uint64
}

func TestWindowsStableSourceHandleExcludesWritesAndSurvivesReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "source.bin")
	movedPath := filepath.Join(directory, "source-moved.bin")
	original := []byte("original revision")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	handle := openWindowsFile(t, path, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE)
	defer windows.CloseHandle(handle)
	originalIdentity := identityOf(t, handle)

	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	writeHandle, err := windows.CreateFile(
		name,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err == nil {
		windows.CloseHandle(writeHandle)
		t.Fatal("a stable-source handle unexpectedly allowed a concurrent writer")
	}
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("concurrent writer error = %v, want ERROR_SHARING_VIOLATION", err)
	}

	if err := os.Rename(path, movedPath); err != nil {
		t.Fatalf("rename while source is open: %v", err)
	}
	if err := os.WriteFile(path, []byte("replacement revision"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementHandle := openWindowsFile(
		t,
		path,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
	defer windows.CloseHandle(replacementHandle)
	if identityOf(t, replacementHandle) == originalIdentity {
		t.Fatal("path replacement retained the old volume/file identity")
	}

	buffer := make([]byte, len(original))
	var read uint32
	if err := windows.ReadFile(handle, buffer, &read, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buffer[:read], original) {
		t.Fatalf("stable handle read %q, want original revision %q", buffer[:read], original)
	}
}

func openWindowsFile(t *testing.T, path string, access, sharing uint32) windows.Handle {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		name,
		access,
		sharing,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func identityOf(t *testing.T, handle windows.Handle) fileIdentity {
	t.Helper()
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		t.Fatal(err)
	}
	return fileIdentity{
		volume: information.VolumeSerialNumber,
		index:  uint64(information.FileIndexHigh)<<32 | uint64(information.FileIndexLow),
	}
}

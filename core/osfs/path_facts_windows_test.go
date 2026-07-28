//go:build windows

package osfs

import (
	"io/fs"
	"syscall"
	"testing"
	"time"
)

func TestWindowsFilesystemPathFactsUseNativeSemantics(t *testing.T) {
	for _, test := range []struct {
		name        string
		information fs.FileInfo
		want        bool
	}{
		{name: "ordinary", information: windowsPathFactFileInfo{}, want: false},
		{name: "symlink-mode", information: windowsPathFactFileInfo{mode: fs.ModeSymlink}, want: true},
		{name: "native-reparse", information: windowsPathFactFileInfo{attributes: syscall.FILE_ATTRIBUTE_REPARSE_POINT}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isReparsePoint(test.information); got != test.want {
				t.Fatalf("reparse classification = %t, want %t", got, test.want)
			}
		})
	}
}

type windowsPathFactFileInfo struct {
	mode       fs.FileMode
	attributes uint32
}

func (information windowsPathFactFileInfo) Name() string       { return "entry" }
func (information windowsPathFactFileInfo) Size() int64        { return 0 }
func (information windowsPathFactFileInfo) Mode() fs.FileMode  { return information.mode }
func (information windowsPathFactFileInfo) ModTime() time.Time { return time.Time{} }
func (information windowsPathFactFileInfo) IsDir() bool        { return information.mode.IsDir() }
func (information windowsPathFactFileInfo) Sys() any {
	return &syscall.Win32FileAttributeData{FileAttributes: information.attributes}
}

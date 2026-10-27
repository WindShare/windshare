package checkpointstore

import (
	"bytes"
	"testing"
)

func TestRecordInstallationPreservesInterruptedImages(t *testing.T) {
	for _, replace := range []bool{false, true} {
		for _, image := range [][]byte{nil, make([]byte, 12), []byte("next\x00\x00\x00\x00\x00\x00\x00\x00")} {
			directory := newMemoryDirectory()
			previous, next := []byte("previous"), []byte("next-record!")
			name := "record.state"
			if replace {
				writeMemoryFile(t, directory, name, previous)
			}
			candidate := TemporaryName(name, next, 0)
			file, err := directory.CreateFile(candidate, true, int64(len(image)))
			if err != nil {
				t.Fatal(err)
			}
			if len(image) != 0 {
				if _, err := file.WriteAt(image, 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			install := func() error { return InstallCreate(directory, name, next) }
			if replace {
				install = func() error { return InstallReplace(directory, name, previous, next) }
			}
			if err := install(); err != nil {
				t.Fatalf("replace=%t image=%q: %v", replace, image, err)
			}
			if err := install(); err != nil {
				t.Fatalf("idempotent retry: %v", err)
			}
			actual, err := ReadFile(directory, name)
			if err != nil || !bytes.Equal(actual, next) {
				t.Fatalf("installed image = %q, %v", actual, err)
			}
			retained, err := directory.OpenObservedFile(candidate, true)
			if err != nil {
				t.Fatal(err)
			}
			size, err := retained.Size()
			if err != nil || size != uint64(len(image)) {
				t.Fatalf("unowned image changed: size=%d, %v", size, err)
			}
			got := make([]byte, len(image))
			if len(got) != 0 {
				if _, err := retained.ReadAt(got, 0); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(got, image) {
				t.Fatal("inexact candidate was overwritten")
			}
			if err := retained.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

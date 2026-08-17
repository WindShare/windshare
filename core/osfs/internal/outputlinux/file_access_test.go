//go:build linux

package outputlinux

import (
	"errors"
	"io"
	"reflect"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

func TestLinuxRecoveryDurabilityUsesMinimumSyncableMode(t *testing.T) {
	platform, _ := newLinuxAdapterTestPlatform(t)
	root := platform.Root()

	const stageName = "recovery-durability-stage.bin"
	created, err := root.CreateFile(stageName, true, 4)
	if err != nil {
		t.Fatal(err)
	}
	assertLinuxFileAccessMethods(t, created, []string{
		"Close", "MetadataMatches", "ReadAt", "SameFile", "SetModifiedTime", "Size", "Sync", "WriteAt",
	})
	if written, writeErr := created.WriteAt([]byte("data"), 0); writeErr != nil || written != 4 {
		t.Fatalf("write ordinary stage = %d, %v", written, writeErr)
	}
	if err := errors.Join(created.Sync(), created.Close()); err != nil {
		t.Fatal(err)
	}

	observed, err := root.OpenObservedFile(stageName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer observed.Close()
	recovery, err := root.OpenRecoveryDurabilityFile(stageName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close()

	concrete := recovery.(*linuxV3RecoveryDurabilityFile)
	flags, err := unix.FcntlInt(uintptr(concrete.state.native.fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.O_ACCMODE != unix.O_WRONLY {
		t.Fatalf("recovery native mode = %#x, want O_WRONLY", flags&unix.O_ACCMODE)
	}
	if size, sizeErr := recovery.Size(); sizeErr != nil || size != 4 {
		t.Fatalf("recovery size = %d, %v", size, sizeErr)
	}
	if same, sameErr := observed.SameFile(recovery); sameErr != nil || !same {
		t.Fatalf("observed/recovery identity = %t, %v", same, sameErr)
	}
	if err := recovery.Sync(); err != nil {
		t.Fatalf("real fsync through recovery authority: %v", err)
	}

	if _, ok := any(observed).(outputcap.RecoveryDurabilityFile); ok {
		t.Fatal("observed wrapper exposed durability mutation")
	}
	if _, ok := any(observed).(io.WriterAt); ok {
		t.Fatal("observed wrapper exposed data mutation")
	}
	if _, ok := any(recovery).(io.ReaderAt); ok {
		t.Fatal("recovery wrapper exposed content observation")
	}
	if _, ok := any(recovery).(io.WriterAt); ok {
		t.Fatal("recovery wrapper exposed data mutation")
	}
	if _, ok := any(recovery).(outputcap.MutableFile); ok {
		t.Fatal("recovery wrapper widened to mutable authority")
	}
	assertLinuxFileAccessMethods(t, observed, []string{"Close", "MetadataMatches", "ReadAt", "SameFile", "Size"})
	assertLinuxFileAccessMethods(t, recovery, []string{"Close", "SameFile", "Size", "Sync"})
}

func assertLinuxFileAccessMethods(t *testing.T, file any, want []string) {
	t.Helper()
	access := reflect.TypeOf(file)
	got := make([]string, 0, access.NumMethod())
	for method := range access.Methods() {
		got = append(got, method.Name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%T method set = %v, want %v", file, got, want)
	}
}

//go:build windows

package perfevidence

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsRejectsUntrustedWritableDACL(t *testing.T) {
	root := filepath.Join(t.TempDir(), "evidence")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;OICI;FA;;;%s)(A;OICI;FW;;;WD)", user.User.Sid.String(),
	))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		root,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := openOutputRootAuthority(root); err == nil || !strings.Contains(err.Error(), "untrusted principal") {
		t.Fatalf("Everyone-writable output authority was accepted: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected output authority was mutated: %v", entries)
	}
}

func TestPhysicalContainmentDetectsProspectiveSUBSTAlias(t *testing.T) {
	repository := newSnapshotFixtureRepository(t)
	drive := availableTestDrive(t)
	command := exec.Command("subst", drive, repository)
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("SUBST alias is unavailable: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("subst", drive, "/D").Run() })
	outputRoot := filepath.Join(drive+`\`, "prospective", "evidence")
	err := validateEvidenceOutputRoot(
		context.Background(), ProcessRunner{}, repository, outputRoot, "subst-alias",
	)
	if err == nil || !strings.Contains(err.Error(), "must be Git-ignored") {
		t.Fatalf("physical repository alias bypassed containment policy: %v", err)
	}
}

func TestRetainedWindowsRootAuthorityRejectsRename(t *testing.T) {
	root := filepath.Join(t.TempDir(), "evidence")
	stage, err := NewStage(root, "root-pin")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	if err := os.Rename(root, root+"-moved"); err == nil {
		t.Fatal("retained Windows root handle allowed its validated pathname to be replaced")
	}
}

func TestWindowsOutputRootDurabilityPrimitive(t *testing.T) {
	authority, err := openOutputRootAuthority(filepath.Join(t.TempDir(), "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.sync(); err != nil {
		_ = authority.close()
		t.Fatalf("directory metadata flush is unavailable: %v", err)
	}
	if err := authority.close(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsConsumptionAuthorityDeniesByteAndNameMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.bin")
	content := []byte("retained")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	authority, err := acquireConsumptionAuthority([]snapshotValidationTarget{{
		LogicalPath:  "input.bin",
		PhysicalPath: path,
		Bytes:        int64(len(content)),
		SHA256:       hashBytes(content),
	}}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = authority.Close()
		}
	}()
	if err := os.WriteFile(path, []byte("injected"), 0o600); err == nil {
		t.Fatal("retained byte authority allowed a concurrent writer")
	}
	if err := os.Rename(path, path+".replacement"); err == nil {
		t.Fatal("retained directory authority allowed a name swap")
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	if err := os.WriteFile(path, []byte("released"), 0o600); err != nil {
		t.Fatalf("authority did not release its byte lock: %v", err)
	}
}

func TestWindowsAuthorityDirectoryInventoryDeduplicatesLargeTargetSets(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "one", "two", "three")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	const targetCount = 20_000
	targets := make([]snapshotValidationTarget, 0, targetCount)
	for index := 0; index < targetCount; index++ {
		targets = append(targets, snapshotValidationTarget{
			LogicalPath:  fmt.Sprintf("target-%d", index),
			PhysicalPath: filepath.Join(nested, fmt.Sprintf("target-%d", index)),
		})
	}
	directories, err := windowsAuthorityDirectories(targets, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		root,
		filepath.Join(root, "one"),
		filepath.Join(root, "one", "two"),
		nested,
	}
	if len(directories) != len(want) {
		t.Fatalf("retained directories = %v, want %v", directories, want)
	}
	for _, expected := range want {
		found := false
		for _, actual := range directories {
			if platformPathKey(actual) == platformPathKey(expected) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("retained directories = %v, missing %s", directories, expected)
		}
	}
}

func TestWindowsSealedMutationOutputRemainsRollbackCapableAfterAdoption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.bin")
	sink, err := prepareMutationOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("rollback-capable")
	if written, err := sink.WriteContext(context.Background(), content); err != nil || written != len(content) {
		t.Fatalf("write sealed output: wrote %d: %v", written, err)
	}
	if err := sink.Seal(context.Background(), int64(len(content)), hashBytes(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.adopt(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rolled-back sealed output survives: %v", err)
	}
}

func availableTestDrive(t *testing.T) string {
	t.Helper()
	for letter := 'Z'; letter >= 'T'; letter-- {
		drive := string(letter) + ":"
		if _, err := os.Stat(drive + `\`); os.IsNotExist(err) {
			return drive
		}
	}
	t.Skip("no free drive letter is available for SUBST adversary")
	return ""
}

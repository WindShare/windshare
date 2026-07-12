package osfs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/windshare/windshare/core/manifest"
)

// NFC/NFD 一律用显式转义构造,避免源文件自身的编码形态混入被测数据。
const (
	nameNFC = "\u00e9.txt"  // é(单码点)
	nameNFD = "e\u0301.txt" // e + 组合尖音符
)

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// entryPaths 提取快照条目路径,便于与期望序列比对。
func entryPaths(snap *Snapshot) []string {
	out := make([]string, len(snap.Entries))
	for i, e := range snap.Entries {
		out[i] = e.Path
	}
	return out
}

func TestWalkNestedTree(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	mustWriteFile(t, filepath.Join(root, "a.txt"), "hello")
	mustWriteFile(t, filepath.Join(root, "sub", "b.bin"), "xyz")
	mustMkdirAll(t, filepath.Join(root, "sub", "empty"))

	snap, err := Walk([]string{root})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []string{"root", "root/a.txt", "root/sub", "root/sub/b.bin", "root/sub/empty"}
	got := entryPaths(snap)
	if len(got) != len(want) {
		t.Fatalf("entry count = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Entries[%d].Path = %q, want %q", i, got[i], want[i])
		}
	}
	for _, e := range snap.Entries {
		switch e.Path {
		case "root", "root/sub", "root/sub/empty":
			if !e.IsDir || e.Size != 0 {
				t.Errorf("%q: IsDir=%v Size=%d, want directory with size 0", e.Path, e.IsDir, e.Size)
			}
		case "root/a.txt":
			if e.IsDir || e.Size != 5 {
				t.Errorf("%q: IsDir=%v Size=%d, want file with size 5", e.Path, e.IsDir, e.Size)
			}
		case "root/sub/b.bin":
			if e.IsDir || e.Size != 3 {
				t.Errorf("%q: IsDir=%v Size=%d, want file with size 3", e.Path, e.IsDir, e.Size)
			}
		}
	}
	// mtime 与 stat 的毫秒值一致(快照即复核基线)。
	fi, err := os.Stat(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range snap.Entries {
		if e.Path == "root/a.txt" && e.MTime != fi.ModTime().UnixMilli() {
			t.Errorf("mtime = %d, want %d", e.MTime, fi.ModTime().UnixMilli())
		}
	}
	if len(snap.Skipped) != 0 {
		t.Errorf("Skipped = %v, want empty", snap.Skipped)
	}
}

func TestWalkSingleFileRoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "solo.dat")
	mustWriteFile(t, file, "1234")

	snap, err := Walk([]string{file})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Entries) != 1 || snap.Entries[0].Path != "solo.dat" || snap.Entries[0].Size != 4 {
		t.Fatalf("Entries = %+v, want one solo.dat entry of size 4", snap.Entries)
	}
}

func TestWalkMixedRoots(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "solo.dat")
	mustWriteFile(t, file, "1234")
	root := filepath.Join(dir, "pack")
	mustWriteFile(t, filepath.Join(root, "in.txt"), "x")

	snap, err := Walk([]string{file, root})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []string{"pack", "pack/in.txt", "solo.dat"}
	got := entryPaths(snap)
	if len(got) != len(want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Entries[%d] = %q, want %q in bytewise path order", i, got[i], want[i])
		}
	}
}

func TestWalkDuplicateRootBasename(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a", "same.txt")
	b := filepath.Join(dir, "b", "same.txt")
	mustWriteFile(t, a, "1")
	mustWriteFile(t, b, "2")

	_, err := Walk([]string{a, b})
	if !errors.Is(err, manifest.ErrDuplicatePath) {
		t.Fatalf("err = %v, want ErrDuplicatePath", err)
	}
}

func TestWalkRejectsCaseFoldedRootCollision(t *testing.T) {
	dir := t.TempDir()
	upper := filepath.Join(dir, "a", "Report.txt")
	lower := filepath.Join(dir, "b", "report.TXT")
	mustWriteFile(t, upper, "1")
	mustWriteFile(t, lower, "2")

	_, err := Walk([]string{upper, lower})
	if !errors.Is(err, manifest.ErrPathCollision) {
		t.Fatalf("err = %v, want ErrPathCollision", err)
	}
}

func TestWalkNormalizesNFDToNFC(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	mustWriteFile(t, filepath.Join(root, nameNFD), "nfc")

	snap, err := Walk([]string{root})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	wantPath := "root/" + nameNFC
	var found bool
	for _, e := range snap.Entries {
		if e.Path == wantPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("entries = %v, want %q after NFD-to-NFC normalization", entryPaths(snap), wantPath)
	}
	// 快照保留磁盘原始(NFD)名定位文件:canonical 路径必须仍可读到内容。
	got, err := NewSource(snap).ReadRange(wantPath, 0, 3)
	if err != nil {
		t.Fatalf("ReadRange with NFC path: %v", err)
	}
	if string(got) != "nfc" {
		t.Fatalf("content = %q, want %q", got, "nfc")
	}
}

func TestWalkNFCNFDCollision(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	mustMkdirAll(t, root)
	// NTFS 按 UTF-16 原样存名,NFC 与 NFD 双形态可并存;归一后重合必须报错,
	// 否则 Source 的 path→文件映射二义。
	mustWriteFile(t, filepath.Join(root, nameNFC), "nfc")
	if err := os.WriteFile(filepath.Join(root, nameNFD), []byte("nfd"), 0o644); err != nil {
		t.Skipf("filesystem does not support coexisting NFC/NFD names: %v", err)
	}
	_, err := Walk([]string{root})
	if !errors.Is(err, manifest.ErrDuplicatePath) {
		t.Fatalf("err = %v, want ErrDuplicatePath", err)
	}
}

func TestWalkMissingRoot(t *testing.T) {
	_, err := Walk([]string{filepath.Join(t.TempDir(), "nope")})
	if err == nil {
		t.Fatal("Walk should fail for a nonexistent root")
	}
}

func TestWalkSkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	mustWriteFile(t, filepath.Join(root, "real.txt"), "data")
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(filepath.Join(root, "real.txt"), link); err != nil {
		// Windows 默认不授予普通进程创建符号链接的特权(需开发者模式)。
		t.Skipf("cannot create symbolic link (privilege may be required): %v", err)
	}

	snap, err := Walk([]string{root})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := entryPaths(snap)
	if len(got) != 2 || got[0] != "root" || got[1] != "root/real.txt" {
		t.Fatalf("entries = %v, want [root root/real.txt]", got)
	}
	if len(snap.Skipped) != 1 || snap.Skipped[0].Path != link || snap.Skipped[0].Reason != SkipReasonReparsePoint {
		t.Fatalf("Skipped = %+v, want %q skipped as a reparse point", snap.Skipped, link)
	}
}

// makeJunction 建 NTFS junction(mklink /J 无需管理员权限);失败则 Skip。
func makeJunction(t *testing.T, junction, target string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("junction test requires Windows")
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create junction: %v: %s", err, out)
	}
}

func TestWalkSkipsJunction(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	mustWriteFile(t, filepath.Join(target, "secret.txt"), "s")
	root := filepath.Join(dir, "root")
	mustWriteFile(t, filepath.Join(root, "keep.txt"), "k")
	junction := filepath.Join(root, "jct")
	makeJunction(t, junction, target)

	snap, err := Walk([]string{root})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, e := range snap.Entries {
		if e.Path == "root/jct" || e.Path == "root/jct/secret.txt" {
			t.Errorf("junction content should not enter snapshot: %q", e.Path)
		}
	}
	if len(snap.Skipped) != 1 || snap.Skipped[0].Path != junction || snap.Skipped[0].Reason != SkipReasonReparsePoint {
		t.Fatalf("Skipped = %+v, want junction %q recorded", snap.Skipped, junction)
	}
}

func TestWalkRootIsJunction(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	mustWriteFile(t, filepath.Join(target, "f.txt"), "x")
	junction := filepath.Join(dir, "jroot")
	makeJunction(t, junction, target)

	snap, err := Walk([]string{junction})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Entries) != 0 {
		t.Fatalf("entries = %v, want empty because root is a reparse point", entryPaths(snap))
	}
	if len(snap.Skipped) != 1 || snap.Skipped[0].Reason != SkipReasonReparsePoint {
		t.Fatalf("Skipped = %+v, want root recorded", snap.Skipped)
	}
}

func TestWalkRejectsReservedName(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("reserved-name creation test requires Windows")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	mustMkdirAll(t, root)
	// Win32 层拒绝创建设备保留名,经 \\?\ 前缀绕过命名解析直达 NTFS。
	f, err := os.Create(`\\?\` + filepath.Join(root, "aux.txt"))
	if err != nil {
		t.Skipf("cannot create reserved-name file: %v", err)
	}
	f.Close()

	_, err = Walk([]string{root})
	if !errors.Is(err, manifest.ErrInvalidPath) {
		t.Fatalf("err = %v, want ErrInvalidPath for an unsafe representation", err)
	}
}

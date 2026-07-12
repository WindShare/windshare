package osfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// snapshotSingleFile 建一个内容已知的单文件快照,返回 canonical 路径与磁盘路径。
func snapshotSingleFile(t *testing.T, content string) (*Snapshot, string, string) {
	t.Helper()
	dir := t.TempDir()
	osPath := filepath.Join(dir, "data.bin")
	mustWriteFile(t, osPath, content)
	snap, err := Walk([]string{osPath})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return snap, "data.bin", osPath
}

func TestReadRange(t *testing.T) {
	snap, path, _ := snapshotSingleFile(t, "0123456789")
	src := NewSource(snap)

	cases := []struct {
		name string
		off  int64
		n    int64
		want string
	}{
		{"整读", 0, 10, "0123456789"},
		{"中段", 3, 4, "3456"},
		{"尾段", 8, 2, "89"},
		{"零长", 5, 0, ""},
		{"EOF 处零长", 10, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := src.ReadRange(path, tc.off, tc.n)
			if err != nil {
				t.Fatalf("ReadRange(%d,%d): %v", tc.off, tc.n, err)
			}
			if string(got) != tc.want {
				t.Fatalf("= %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadRangeRejectsOutsideSnapshot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	mustWriteFile(t, filepath.Join(root, "in.txt"), "x")
	// 根外放一个真实存在的文件:穿越若未被拦截,读它会"成功"。
	mustWriteFile(t, filepath.Join(dir, "outside.txt"), "secret")
	snap, err := Walk([]string{root})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	src := NewSource(snap)

	cases := []struct {
		name string
		path string
	}{
		{"父目录穿越", "../outside.txt"},
		{"根内穿越再逃逸", "root/../../outside.txt"},
		{"绝对路径", filepath.ToSlash(filepath.Join(dir, "outside.txt"))},
		{"未知路径", "root/nope.txt"},
		{"目录条目", "root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := src.ReadRange(tc.path, 0, 1)
			if !errors.Is(err, ErrNotInSnapshot) {
				t.Fatalf("err = %v, want ErrNotInSnapshot", err)
			}
		})
	}
}

func TestSourceBoundsHostilePathDiagnostics(t *testing.T) {
	hostile := strings.Repeat("é", 1<<19)
	tests := []struct {
		name string
		snap *Snapshot
		want error
	}{
		{name: "outside snapshot", snap: &Snapshot{files: map[string]fileRef{}}, want: ErrNotInSnapshot},
		{name: "out of range", snap: &Snapshot{files: map[string]fileRef{hostile: {size: 0}}}, want: ErrOutOfRange},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSource(tc.snap).ReadRange(hostile, 0, 1)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ReadRange error category = %v, want %v", errors.Unwrap(err), tc.want)
			}
			message := err.Error()
			if len(message) > 1024 {
				t.Fatalf("diagnostic length = %d, want <= 1024", len(message))
			}
			if !utf8.ValidString(message) {
				t.Fatal("diagnostic is not valid UTF-8")
			}
			if !strings.Contains(message, "…") {
				t.Fatal("diagnostic does not identify path truncation")
			}
		})
	}
}

func TestReadRangeBounds(t *testing.T) {
	snap, path, _ := snapshotSingleFile(t, "0123456789")
	src := NewSource(snap)

	cases := []struct {
		name string
		off  int64
		n    int64
	}{
		{"负 off", -1, 1},
		{"负 n", 0, -1},
		{"越尾", 8, 3},
		{"off 超 size", 11, 0},
		{"n 超 size", 0, 11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := src.ReadRange(path, tc.off, tc.n)
			if !errors.Is(err, ErrOutOfRange) {
				t.Fatalf("err = %v, want ErrOutOfRange", err)
			}
		})
	}
}

func TestReadRangeDriftSize(t *testing.T) {
	snap, path, osPath := snapshotSingleFile(t, "0123456789")
	// 快照后追加内容:size 变了,读后复核必须报漂移并中止。
	f, err := os.OpenFile(osPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("MORE"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	_, err = NewSource(snap).ReadRange(path, 0, 10)
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("err = %v, want ErrDrift", err)
	}
}

func TestReadRangeDriftMTime(t *testing.T) {
	snap, path, osPath := snapshotSingleFile(t, "0123456789")
	// 同 size、不同 mtime:模拟原地改写后时间戳变化。
	fi, err := os.Stat(osPath)
	if err != nil {
		t.Fatal(err)
	}
	shifted := fi.ModTime().Add(3 * time.Second)
	if err := os.Chtimes(osPath, shifted, shifted); err != nil {
		t.Fatal(err)
	}

	_, err = NewSource(snap).ReadRange(path, 0, 10)
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("err = %v, want ErrDrift", err)
	}
}

func TestReadRangeDeletedFile(t *testing.T) {
	snap, path, osPath := snapshotSingleFile(t, "0123456789")
	if err := os.Remove(osPath); err != nil {
		t.Fatal(err)
	}
	_, err := NewSource(snap).ReadRange(path, 0, 10)
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("err = %v, want ErrDrift", err)
	}
}

func TestReadRangeRejectsReplacementWithMatchingMetadata(t *testing.T) {
	snap, path, osPath := snapshotSingleFile(t, "original!!")
	ref := snap.files[path]
	replacement := filepath.Join(filepath.Dir(osPath), "replacement.bin")
	mustWriteFile(t, replacement, "unrelated!")
	matchingTime := time.UnixMilli(ref.mtime)
	if err := os.Chtimes(replacement, matchingTime, matchingTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(osPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(replacement, osPath); err != nil {
		t.Skipf("filesystem does not support hard-link replacement: %v", err)
	}
	fi, err := os.Stat(osPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != ref.size || fi.ModTime().UnixMilli() != ref.mtime {
		t.Skipf("filesystem cannot preserve the matching-metadata fixture: size=%d mtime=%d", fi.Size(), fi.ModTime().UnixMilli())
	}

	if _, err := NewSource(snap).ReadRange(path, 0, ref.size); !errors.Is(err, ErrDrift) {
		t.Fatalf("replacement with matching size and mtime: err = %v, want ErrDrift", err)
	}
}

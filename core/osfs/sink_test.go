package osfs

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/windshare/windshare/core/manifest"
)

func newSink(t *testing.T, opt SinkOptions) (*Sink, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "out")
	s, err := NewSink(root, opt)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s, root
}

type testOwnershipLedger struct {
	owned     map[string]bool
	recorded  []string
	recordErr error
}

type recordingRoot struct{ calls int }

type stubRandomAccessFile struct {
	writeN   int
	writeErr error
	closeErr error
	closed   bool
}

func (f *stubRandomAccessFile) WriteAt(data []byte, _ int64) (int, error) {
	if f.writeN < 0 {
		return len(data), f.writeErr
	}
	return f.writeN, f.writeErr
}

func (f *stubRandomAccessFile) Close() error {
	f.closed = true
	return f.closeErr
}

type stubRoot struct {
	mkdirErr   error
	openErr    error
	chtimesErr error
	file       randomAccessFile
}

func (r *stubRoot) MkdirAll(string, fs.FileMode) error { return r.mkdirErr }

func (r *stubRoot) OpenFile(string, int, fs.FileMode) (randomAccessFile, error) {
	return r.file, r.openErr
}

func (r *stubRoot) Chtimes(string, time.Time, time.Time) error { return r.chtimesErr }

func (r *stubRoot) Close() error { return nil }

func (r *recordingRoot) MkdirAll(string, fs.FileMode) error {
	r.calls++
	return nil
}

func (r *recordingRoot) OpenFile(string, int, fs.FileMode) (randomAccessFile, error) {
	r.calls++
	return nil, errors.New("unexpected open")
}

func (r *recordingRoot) Chtimes(string, time.Time, time.Time) error {
	r.calls++
	return nil
}

func (r *recordingRoot) Close() error { return nil }

func newTestOwnership(paths ...string) *testOwnershipLedger {
	ledger := &testOwnershipLedger{owned: make(map[string]bool)}
	for _, path := range paths {
		ledger.owned[path] = true
	}
	return ledger
}

func (l *testOwnershipLedger) Owns(path string) bool { return l.owned[path] }

func (l *testOwnershipLedger) RecordCreated(path string) error {
	if l.recordErr != nil {
		return l.recordErr
	}
	l.owned[path] = true
	l.recorded = append(l.recorded, path)
	return nil
}

// TestSinkRejectsUnsafePaths 是 §6.13 接收端穿越矩阵:三个入口
// (EnsureDir/WriteRange/SetMTime)对同一非法路径必须一致拒绝,且拒绝
// 发生在触盘之前。
func TestSinkRejectsUnsafePaths(t *testing.T) {
	s, root := newSink(t, SinkOptions{})

	cases := []struct {
		name    string
		path    string
		wantErr error
	}{
		{"空路径", "", manifest.ErrInvalidPath},
		{"父目录穿越", "../evil", manifest.ErrInvalidPath},
		{"内嵌穿越", "a/../evil", manifest.ErrInvalidPath},
		{"当前目录段", "a/./b", manifest.ErrInvalidPath},
		{"空段", "a//b", manifest.ErrInvalidPath},
		{"绝对路径", "/etc/passwd", manifest.ErrInvalidPath},
		{"盘符", "C:/windows/evil", manifest.ErrInvalidPath},
		{"反斜杠", `a\b`, manifest.ErrInvalidPath},
		{"UNC", "//srv/share/x", manifest.ErrInvalidPath},
		{"控制字符 NUL", "a\x00b", manifest.ErrInvalidPath},
		{"控制字符 TAB", "a\tb", manifest.ErrInvalidPath},
		{"非法字符 星号", "a*b", manifest.ErrInvalidPath},
		{"ADS 冒号", "a:stream", manifest.ErrInvalidPath},
		{"保留名", "aux", manifest.ErrInvalidPath},
		{"保留名带扩展", "sub/COM3.log", manifest.ErrInvalidPath},
		{"结尾点", "trail.", manifest.ErrInvalidPath},
		{"结尾空格", "trail ", manifest.ErrInvalidPath},
		{"journal 保留前缀", ".wsresume-abcd", manifest.ErrInvalidPath},
		{"journal 前缀变体", ".wsresumeX", manifest.ErrInvalidPath},
		{"journal 大小写变体", ".WSRESUME-abcd", manifest.ErrInvalidPath},
		{"Unicode bidi 控制", "photo\u202etxt.exe", manifest.ErrInvalidPath},
		{"Unicode ZWJ", "a\u200db", manifest.ErrInvalidPath},
		{"上标设备名", "COM¹.txt", manifest.ErrInvalidPath},
		{"控制台设备名", "CONOUT$.txt", manifest.ErrInvalidPath},
		{"非 NFC", "é.txt", manifest.ErrInvalidPath},
		// 单段即超两个平台的上限(Windows 260 UTF-16 码元 / POSIX 4096 字节),
		// 与输出根长度无关;段长也超 NTFS 单名 255 上限,故必须在触盘前拦下。
		{"超长路径", strings.Repeat("a", 4096), ErrPathTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.EnsureDir(tc.path); !errors.Is(err, tc.wantErr) {
				t.Errorf("EnsureDir: err = %v, want %v", err, tc.wantErr)
			}
			if err := s.WriteRange(tc.path, 0, []byte("x")); !errors.Is(err, tc.wantErr) {
				t.Errorf("WriteRange: err = %v, want %v", err, tc.wantErr)
			}
			if err := s.SetMTime(tc.path, 0); !errors.Is(err, tc.wantErr) {
				t.Errorf("SetMTime: err = %v, want %v", err, tc.wantErr)
			}
		})
	}
	// 矩阵跑完输出根应一尘不染:拒绝须发生在任何副作用之前。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("output root contains %v; rejection should happen before disk mutation", entries)
	}
}

func TestSinkBoundsHostilePathDiagnostic(t *testing.T) {
	root := &recordingRoot{}
	s := newSinkWithFilesystem(t.TempDir(), root, SinkOptions{})
	// A manifest can legitimately carry megabytes of canonical path bytes before
	// the platform-specific filesystem limit rejects them. Diagnostics must not
	// turn that authenticated metadata into a second equally large terminal write.
	const manifestEnvelopeHeadroom = 1 << 10
	hostile := strings.Repeat("é", (manifest.MaxManifestSize-manifestEnvelopeHeadroom)/len("é"))
	err := s.EnsureDir(hostile)
	if !errors.Is(err, ErrPathTooLong) {
		t.Fatalf("EnsureDir: err = %v, want ErrPathTooLong", err)
	}
	const maxDiagnosticBytes = 1024
	if got := len(err.Error()); got > maxDiagnosticBytes {
		t.Fatalf("diagnostic length = %d, want <= %d", got, maxDiagnosticBytes)
	}
	if !utf8.ValidString(err.Error()) {
		t.Fatal("diagnostic is not valid UTF-8")
	}
	if !strings.Contains(err.Error(), "…") {
		t.Fatalf("diagnostic does not identify truncation: %q", err)
	}
	if root.calls != 0 {
		t.Fatalf("overlong path reached the root capability %d times", root.calls)
	}
}

func TestNewSinkBoundsHostileRootDiagnostic(t *testing.T) {
	hostile := strings.Repeat("é", 1<<19)
	_, err := NewSink(hostile, SinkOptions{})
	if !errors.Is(err, ErrPathTooLong) {
		t.Fatalf("NewSink error category = %v, want ErrPathTooLong", errors.Unwrap(err))
	}
	message := err.Error()
	if len(message) > 1024 {
		t.Fatalf("diagnostic length = %d, want <= 1024", len(message))
	}
	if !utf8.ValidString(message) {
		t.Fatal("diagnostic is not valid UTF-8")
	}
	if !strings.Contains(message, "…") {
		t.Fatal("diagnostic does not identify root-path truncation")
	}
}

func TestSinkRedactsWrappedFilesystemDiagnostics(t *testing.T) {
	const secretMarker = "DO_NOT_PRINT_SECRET"
	sentinel := errors.New(secretMarker)
	leakyCause := &fs.PathError{
		Op:   "hostile operation",
		Path: strings.Repeat(secretMarker, 512) + "\xff",
		Err:  sentinel,
	}
	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "mkdir",
			run: func() error {
				return newSinkWithFilesystem(t.TempDir(), &stubRoot{mkdirErr: leakyCause}, SinkOptions{}).EnsureDir("safe")
			},
		},
		{
			name: "open",
			run: func() error {
				return newSinkWithFilesystem(t.TempDir(), &stubRoot{openErr: leakyCause}, SinkOptions{}).WriteRange("safe", 0, []byte("x"))
			},
		},
		{
			name: "mtime",
			run: func() error {
				return newSinkWithFilesystem(t.TempDir(), &stubRoot{chtimesErr: leakyCause}, SinkOptions{}).SetMTime("safe", 0)
			},
		},
		{
			name: "write",
			run: func() error {
				file := &stubRandomAccessFile{writeN: -1, writeErr: leakyCause}
				return newSinkWithFilesystem(t.TempDir(), &stubRoot{file: file}, SinkOptions{}).WriteRange("safe", 0, []byte("x"))
			},
		},
		{
			name: "close",
			run: func() error {
				file := &stubRandomAccessFile{writeN: -1, closeErr: leakyCause}
				return newSinkWithFilesystem(t.TempDir(), &stubRoot{file: file}, SinkOptions{}).WriteRange("safe", 0, []byte("x"))
			},
		},
		{
			name: "ownership ledger",
			run: func() error {
				ledger := newTestOwnership()
				ledger.recordErr = leakyCause
				file := &stubRandomAccessFile{writeN: -1}
				return newSinkWithFilesystem(t.TempDir(), &stubRoot{file: file}, SinkOptions{Ownership: ledger}).WriteRange("safe", 0, []byte("x"))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if !errors.Is(err, sentinel) {
				t.Fatal("wrapped cause is not reachable through errors.Is")
			}
			var pathErr *fs.PathError
			if !errors.As(err, &pathErr) || pathErr != leakyCause {
				t.Fatal("wrapped cause is not reachable through errors.As")
			}
			message := err.Error()
			if len(message) > 1024 {
				t.Fatalf("diagnostic length = %d, want <= 1024", len(message))
			}
			if !utf8.ValidString(message) {
				t.Fatal("diagnostic is not valid UTF-8")
			}
			if strings.Contains(message, secretMarker) {
				t.Fatal("diagnostic rendered sensitive wrapped-cause text")
			}
		})
	}
}

func TestSinkRejectsPathBeforeRootOperation(t *testing.T) {
	root := &recordingRoot{}
	s := newSinkWithFilesystem(t.TempDir(), root, SinkOptions{})
	if err := s.EnsureDir("../escape"); !errors.Is(err, manifest.ErrInvalidPath) {
		t.Fatalf("EnsureDir: %v", err)
	}
	if err := s.WriteRange("../escape", 0, []byte("x")); !errors.Is(err, manifest.ErrInvalidPath) {
		t.Fatalf("WriteRange: %v", err)
	}
	if err := s.SetMTime("../escape", 0); !errors.Is(err, manifest.ErrInvalidPath) {
		t.Fatalf("SetMTime: %v", err)
	}
	if root.calls != 0 {
		t.Fatalf("invalid paths reached the root capability %d times", root.calls)
	}
}

func TestSinkClassifiesFilesystemPathLimitErrors(t *testing.T) {
	boundaryErr := &fs.PathError{Op: "root operation", Path: "valid", Err: syscall.ENAMETOOLONG}
	tests := []struct {
		name string
		root *stubRoot
		run  func(*Sink) error
	}{
		{
			name: "mkdir",
			root: &stubRoot{mkdirErr: boundaryErr},
			run:  func(s *Sink) error { return s.EnsureDir("valid") },
		},
		{
			name: "open",
			root: &stubRoot{openErr: boundaryErr},
			run:  func(s *Sink) error { return s.WriteRange("valid", 0, []byte("x")) },
		},
		{
			name: "mtime",
			root: &stubRoot{chtimesErr: boundaryErr},
			run:  func(s *Sink) error { return s.SetMTime("valid", 0) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSinkWithFilesystem(t.TempDir(), tc.root, SinkOptions{})
			err := tc.run(s)
			if !errors.Is(err, ErrPathTooLong) {
				t.Fatalf("err = %v, want ErrPathTooLong", err)
			}
			if !errors.Is(err, syscall.ENAMETOOLONG) {
				t.Fatalf("classified error must preserve the filesystem cause: %v", err)
			}
		})
	}
}

func TestSinkClassifiesRealOverlongComponent(t *testing.T) {
	s, _ := newSink(t, SinkOptions{})
	err := s.WriteRange(strings.Repeat("n", 256), 0, []byte("x"))
	if !errors.Is(err, ErrPathTooLong) {
		t.Fatalf("err = %v, want ErrPathTooLong", err)
	}
}

func TestSinkWriteRangeReportsWriterAndCloseFailures(t *testing.T) {
	writeFailure := errors.New("write failed")
	closeFailure := errors.New("close failed")
	tests := []struct {
		name     string
		file     *stubRandomAccessFile
		wantErrs []error
	}{
		{
			name:     "short write",
			file:     &stubRandomAccessFile{writeN: 1},
			wantErrs: []error{io.ErrShortWrite},
		},
		{
			name:     "close failure",
			file:     &stubRandomAccessFile{writeN: -1, closeErr: closeFailure},
			wantErrs: []error{closeFailure},
		},
		{
			name:     "write and close failure",
			file:     &stubRandomAccessFile{writeErr: writeFailure, closeErr: closeFailure},
			wantErrs: []error{writeFailure, closeFailure},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := &stubRoot{file: tc.file}
			s := newSinkWithFilesystem(t.TempDir(), root, SinkOptions{})
			err := s.WriteRange("valid", 0, []byte("data"))
			for _, want := range tc.wantErrs {
				if !errors.Is(err, want) {
					t.Errorf("err = %v, want wrapped %v", err, want)
				}
			}
			if !tc.file.closed {
				t.Error("WriteRange must close the output after every attempted write")
			}
		})
	}
}

func TestSinkRoundTrip(t *testing.T) {
	s, root := newSink(t, SinkOptions{})
	if err := s.EnsureDir("d/sub"); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	// 乱序段写:先尾后头,验证 pwrite 语义的定位重组。
	if err := s.WriteRange("d/sub/f.bin", 4, []byte("5678")); err != nil {
		t.Fatalf("WriteRange tail segment: %v", err)
	}
	if err := s.WriteRange("d/sub/f.bin", 0, []byte("1234")); err != nil {
		t.Fatalf("WriteRange head segment: %v", err)
	}
	const fileMTime = int64(1234567890123)
	const dirMTime = int64(1111111111111)
	if err := s.SetMTime("d/sub/f.bin", fileMTime); err != nil {
		t.Fatalf("SetMTime file: %v", err)
	}
	if err := s.SetMTime("d/sub", dirMTime); err != nil {
		t.Fatalf("SetMTime directory: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "d", "sub", "f.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "12345678" {
		t.Fatalf("content = %q, want %q", got, "12345678")
	}
	fi, err := os.Stat(filepath.Join(root, "d", "sub", "f.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().UnixMilli() != fileMTime {
		t.Errorf("file mtime = %d, want %d with millisecond precision", fi.ModTime().UnixMilli(), fileMTime)
	}
	di, err := os.Stat(filepath.Join(root, "d", "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if di.ModTime().UnixMilli() != dirMTime {
		t.Errorf("directory mtime = %d, want %d", di.ModTime().UnixMilli(), dirMTime)
	}
}

func TestSinkSparseOutOfOrder(t *testing.T) {
	s, root := newSink(t, SinkOptions{})
	// 先写高偏移段:文件被拉长,空洞由文件系统补零;再回填首段,中段留零。
	if err := s.WriteRange("sparse.bin", 8, []byte("tail")); err != nil {
		t.Fatalf("high segment: %v", err)
	}
	if err := s.WriteRange("sparse.bin", 0, []byte("head")); err != nil {
		t.Fatalf("low segment: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "sparse.bin"))
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte("head"), 0, 0, 0, 0)
	want = append(want, []byte("tail")...)
	if !bytes.Equal(got, want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestSinkWriteRangeCreatesParents(t *testing.T) {
	s, root := newSink(t, SinkOptions{})
	if err := s.WriteRange("p1/p2/f.txt", 0, []byte("x")); err != nil {
		t.Fatalf("WriteRange: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "p1", "p2", "f.txt")); err != nil {
		t.Fatalf("parent directory was not created on demand: %v", err)
	}
}

func TestSinkRefusesExistingFile(t *testing.T) {
	s, root := newSink(t, SinkOptions{})
	mustWriteFile(t, filepath.Join(root, "keep.txt"), "user data")

	err := s.WriteRange("keep.txt", 0, []byte("clobber"))
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "user data" {
		t.Fatalf("user data was overwritten with %q", got)
	}
	// 目录不受"同名拒绝"约束:续传/重试都要求 EnsureDir 幂等。
	if err := s.EnsureDir("keep-dir"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureDir("keep-dir"); err != nil {
		t.Fatalf("EnsureDir should be idempotent: %v", err)
	}
}

func TestSinkSecondWriteSamePath(t *testing.T) {
	s, _ := newSink(t, SinkOptions{})
	if err := s.WriteRange("f.bin", 0, []byte("aa")); err != nil {
		t.Fatal(err)
	}
	// 同一会话的后续段写不应再撞 O_EXCL。
	if err := s.WriteRange("f.bin", 2, []byte("bb")); err != nil {
		t.Fatalf("second segment in the same session: %v", err)
	}
}

func TestSinkReopensOnlyOwnedPath(t *testing.T) {
	ledger := newTestOwnership("part.bin")
	s, root := newSink(t, SinkOptions{Ownership: ledger})
	mustWriteFile(t, filepath.Join(root, "part.bin"), "xxxxxx")
	mustWriteFile(t, filepath.Join(root, "unowned.bin"), "user")

	if err := s.WriteRange("part.bin", 2, []byte("AB")); err != nil {
		t.Fatalf("owned WriteRange: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "part.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "xxABxx" {
		t.Fatalf("content = %q, want %q", got, "xxABxx")
	}
	if err := s.WriteRange("unowned.bin", 0, []byte("bad")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("unowned existing path: err = %v, want ErrAlreadyExists", err)
	}
	got, err = os.ReadFile(filepath.Join(root, "unowned.bin"))
	if err != nil || string(got) != "user" {
		t.Fatalf("unowned file changed: %q, %v", got, err)
	}
}

func TestSinkRecordsFreshOwnershipBeforeSuccess(t *testing.T) {
	ledger := newTestOwnership()
	s, _ := newSink(t, SinkOptions{Ownership: ledger})
	if err := s.WriteRange("fresh.bin", 0, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if len(ledger.recorded) != 1 || ledger.recorded[0] != "fresh.bin" {
		t.Fatalf("recorded = %v, want [fresh.bin]", ledger.recorded)
	}
	if err := s.WriteRange("fresh.bin", 1, []byte("b")); err != nil {
		t.Fatalf("recorded path should reopen: %v", err)
	}
}

func TestSinkOwnershipRecordFailureIsSafe(t *testing.T) {
	ledger := newTestOwnership()
	ledger.recordErr = errors.New("journal unavailable")
	s, root := newSink(t, SinkOptions{Ownership: ledger})
	if err := s.WriteRange("fresh.bin", 0, []byte("data")); !errors.Is(err, ErrOwnershipRecord) {
		t.Fatalf("err = %v, want ErrOwnershipRecord", err)
	}
	if ledger.Owns("fresh.bin") {
		t.Fatal("failed persistence must not grant reopen authority")
	}
	if err := s.WriteRange("fresh.bin", 0, []byte("retry")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("safe orphan must remain unowned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "fresh.bin")); err != nil {
		t.Fatalf("exclusive create should leave a safe orphan: %v", err)
	}
}

func TestSinkOwnedPathMustExist(t *testing.T) {
	s, _ := newSink(t, SinkOptions{Ownership: newTestOwnership("missing.bin")})
	if err := s.WriteRange("missing.bin", 0, []byte("x")); !errors.Is(err, ErrOwnedFileMissing) {
		t.Fatalf("err = %v, want ErrOwnedFileMissing", err)
	}
}

func TestSinkNegativeOffset(t *testing.T) {
	s, _ := newSink(t, SinkOptions{})
	if err := s.WriteRange("f.bin", -1, []byte("x")); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("err = %v, want ErrOutOfRange", err)
	}
}

func TestSinkRejectsOffsetLengthOverflowBeforeCreation(t *testing.T) {
	s, root := newSink(t, SinkOptions{})
	if err := s.WriteRange("overflow.bin", math.MaxInt64, []byte("xx")); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("err = %v, want ErrOutOfRange", err)
	}
	if _, err := os.Stat(filepath.Join(root, "overflow.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overflowing write created output: %v", err)
	}
}

func TestSinkZeroLengthWriteMaterializesFile(t *testing.T) {
	s, root := newSink(t, SinkOptions{})
	// 空文件不占流、永无块到达,接收端靠零长写显式物化(§6.6 收尾物化)。
	if err := s.WriteRange("empty.txt", 0, nil); err != nil {
		t.Fatalf("zero-length write: %v", err)
	}
	fi, err := os.Stat(filepath.Join(root, "empty.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Fatalf("size = %d, want 0", fi.Size())
	}
}

func TestNewSinkRootIsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "occupied")
	mustWriteFile(t, file, "x")
	if _, err := NewSink(file, SinkOptions{}); err == nil {
		t.Fatal("NewSink should fail when a file occupies the output root")
	}
}

func TestNewSinkRejectsOverlongRootBeforeCreation(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("r", 4096))
	if _, err := NewSink(root, SinkOptions{}); !errors.Is(err, ErrPathTooLong) {
		t.Fatalf("err = %v, want ErrPathTooLong", err)
	}
}

func TestNewSinkCreatesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "deep", "out")
	s, err := NewSink(root, SinkOptions{})
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		t.Fatalf("output root was not created: %v", err)
	}
}

func TestSinkSetMTimeMissingTarget(t *testing.T) {
	s, _ := newSink(t, SinkOptions{})
	if err := s.SetMTime("nope.txt", time.Now().UnixMilli()); err == nil {
		t.Fatal("SetMTime should fail when the target does not exist")
	}
}

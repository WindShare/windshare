package manifest

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/windshare/windshare/core/layout"
)

// 手工推导的 CBOR 片段(RFC 8949 Core Deterministic,键按编码字节序):
// 顶层键序 v < entries < chunkSize,条目键序 path < size < isDir < mtime。
// 用独立于实现的字面量钉住编码形态,防编码选项漂移。
const (
	hexKeyV         = "6176"                 // "v"
	hexKeyEntries   = "67656e7472696573"     // "entries"
	hexKeyChunkSize = "696368756e6b53697a65" // "chunkSize"
	hexChunk1MiB    = "1a00100000"           // 1048576
	// {path:"a", size:1, isDir:false, mtime:0}
	hexEntryA = "a4" + "6470617468" + "6161" + "6473697a65" + "01" + "656973446972" + "f4" + "656d74696d65" + "00"
	// {v:1, entries:[entryA], chunkSize:1MiB}
	hexGolden = "a3" + hexKeyV + "01" + hexKeyEntries + "81" + hexEntryA + hexKeyChunkSize + hexChunk1MiB
	// {v:1, entries:[], chunkSize:1MiB}
	hexEmpty = "a3" + hexKeyV + "01" + hexKeyEntries + "80" + hexKeyChunkSize + hexChunk1MiB
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("invalid test hex: %v", err)
	}
	return b
}

func goldenManifest() *Manifest {
	return New(1<<20, []Entry{{Path: "a", Size: 1, MTime: 0, IsDir: false}})
}

func TestEncodeGoldenBytes(t *testing.T) {
	got, err := encMode.Marshal(goldenManifest())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := mustHex(t, hexGolden); !bytes.Equal(got, want) {
		t.Fatalf("encoding shape drifted:\n got=%x\nwant=%x", got, want)
	}
}

func TestEncodeNilEntriesAsEmptyArray(t *testing.T) {
	got, err := encMode.Marshal(New(1<<20, nil))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := mustHex(t, hexEmpty); !bytes.Equal(got, want) {
		t.Fatalf("nil Entries did not encode as an empty array:\n got=%x\nwant=%x", got, want)
	}
}

func TestEncodeDeterministic(t *testing.T) {
	m := New(1<<20, []Entry{
		{Path: "b", Size: 2, MTime: -1},
		{Path: "a", Size: 1, MTime: 3},
	})
	first, err := encMode.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	second, err := encMode.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("encoding differs for identical input")
	}

	swapped, err := encMode.Marshal(New(1<<20, []Entry{
		{Path: "a", Size: 1, MTime: 3},
		{Path: "b", Size: 2, MTime: -1},
	}))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Equal(first, swapped) {
		t.Fatalf("entries array order is not reflected in encoding")
	}
}

func TestDecodeManifest(t *testing.T) {
	tests := []struct {
		name    string
		hex     string
		wantOK  bool
		wantErr error // 非 nil 时要求 errors.Is 命中;nil 且 !wantOK 表示任意错误
	}{
		{name: "golden 单条目", hex: hexGolden, wantOK: true},
		{name: "空 entries", hex: hexEmpty, wantOK: true},
		{name: "非最短整数编码 v", hex: "a3" + hexKeyV + "1801" + hexKeyEntries + "80" + hexKeyChunkSize + hexChunk1MiB, wantErr: ErrNonCanonical},
		{name: "键未按确定性序排列", hex: "a3" + hexKeyV + "01" + hexKeyChunkSize + hexChunk1MiB + hexKeyEntries + "80", wantErr: ErrNonCanonical},
		{name: "不定长数组", hex: "a3" + hexKeyV + "01" + hexKeyEntries + "9fff" + hexKeyChunkSize + hexChunk1MiB, wantErr: ErrNonCanonical},
		{name: "重复键", hex: "a4" + hexKeyV + "01" + hexKeyV + "01" + hexKeyEntries + "80" + hexKeyChunkSize + hexChunk1MiB, wantErr: ErrNonCanonical},
		{name: "未知字段", hex: "a4" + hexKeyV + "01" + "6178" + "00" + hexKeyEntries + "80" + hexKeyChunkSize + hexChunk1MiB, wantErr: ErrNonCanonical},
		{name: "entries 为 null", hex: "a3" + hexKeyV + "01" + hexKeyEntries + "f6" + hexKeyChunkSize + hexChunk1MiB, wantErr: ErrNonCanonical},
		{name: "chunkSize 为浮点", hex: "a3" + hexKeyV + "01" + hexKeyEntries + "80" + hexKeyChunkSize + "fb4130000000000000", wantErr: ErrNonCanonical},
		{name: "isDir 为整数", hex: "a3" + hexKeyV + "01" + hexKeyEntries + "81" + "a4" + "6470617468" + "6161" + "6473697a65" + "01" + "656973446972" + "00" + "656d74696d65" + "00" + hexKeyChunkSize + hexChunk1MiB, wantErr: ErrNonCanonical},
		{name: "尾随多余字节", hex: hexEmpty + "00"},
		{name: "未知版本 v=2", hex: "a3" + hexKeyV + "02" + hexKeyEntries + "80" + hexKeyChunkSize + hexChunk1MiB, wantErr: ErrUnsupportedVersion},
		{name: "未知版本 v=0", hex: "a3" + hexKeyV + "00" + hexKeyEntries + "80" + hexKeyChunkSize + hexChunk1MiB, wantErr: ErrUnsupportedVersion},
		// B15 关键性质:未来 schema 带未知字段甚至不定长编码,探测仍须给出
		// "请升级"而非 CBOR 结构错误。{v:2, x:[_ 1,2,3]}
		{name: "未来 schema 宽容探测", hex: "a2" + hexKeyV + "02" + "6178" + "9f010203ff", wantErr: ErrUnsupportedVersion},
		{name: "缺少版本字段", hex: "a2" + hexKeyEntries + "80" + hexKeyChunkSize + hexChunk1MiB},
		{name: "v 为字符串", hex: "a1" + hexKeyV + "6161"},
		{name: "v 为负数", hex: "a1" + hexKeyV + "20"},
		{name: "顶层非 map", hex: "80"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := decodeManifest(mustHex(t, tt.hex))
			if tt.wantOK {
				if err != nil {
					t.Fatalf("expected success, got error: %v", err)
				}
				if m.Version != CurrentVersion || m.ChunkSize != 1<<20 {
					t.Fatalf("decoded fields mismatch: %+v", m)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error, got %+v", m)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error category mismatch: got %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// 接收侧流程:解封/解码只管结构合法,恶意但 canonical 的字段值(负 size)
// 由随后的 Validate 拒绝——两步各司其职。
func TestDecodeThenValidateRejectsNegativeSize(t *testing.T) {
	entryNeg := "a4" + "6470617468" + "6161" + "6473697a65" + "20" + "656973446972" + "f4" + "656d74696d65" + "00"
	m, err := decodeManifest(mustHex(t, "a3"+hexKeyV+"01"+hexKeyEntries+"81"+entryNeg+hexKeyChunkSize+hexChunk1MiB))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := m.Validate(); !errors.Is(err, ErrNegativeSize) {
		t.Fatalf("want ErrNegativeSize, got %v", err)
	}
}

func TestNew(t *testing.T) {
	entries := []Entry{{Path: "a", Size: 1}}
	m := New(4<<20, entries)
	if m.Version != CurrentVersion || m.ChunkSize != 4<<20 || !reflect.DeepEqual(m.Entries, entries) {
		t.Fatalf("New result mismatch: %+v", m)
	}
}

func TestValidate(t *testing.T) {
	const maxSize = layout.MaxStreamBytes
	valid := func(mutate func(*Manifest)) *Manifest {
		m := New(1<<20, []Entry{
			{Path: "docs", IsDir: true, MTime: 1},
			{Path: "docs/读我.md", Size: 42, MTime: -1},
			{Path: "empty", Size: 0},
		})
		if mutate != nil {
			mutate(m)
		}
		return m
	}
	tests := []struct {
		name    string
		m       *Manifest
		wantErr error // nil = 期望通过
	}{
		{name: "合法清单", m: valid(nil)},
		{name: "最小 chunkSize 合法", m: valid(func(m *Manifest) { m.ChunkSize = layout.MinChunkSize })},
		{name: "空 entries 合法", m: New(1<<20, nil)},
		{name: "mtime 最小互操作边界合法", m: New(1<<20, []Entry{{Path: "a", MTime: MinMTimeMilliseconds}})},
		{name: "mtime 最大互操作边界合法", m: New(1<<20, []Entry{{Path: "a", MTime: MaxMTimeMilliseconds}})},
		{name: "mtime 低于互操作边界", m: New(1<<20, []Entry{{Path: "a", MTime: MinMTimeMilliseconds - 1}}), wantErr: ErrMTimeOutOfRange},
		{name: "mtime 高于互操作边界", m: New(1<<20, []Entry{{Path: "a", MTime: MaxMTimeMilliseconds + 1}}), wantErr: ErrMTimeOutOfRange},
		{name: "chunkSize 低于最小值", m: valid(func(m *Manifest) { m.ChunkSize = layout.MinChunkSize / 2 }), wantErr: ErrInvalidChunkSize},
		{name: "chunkSize 超过最大值", m: valid(func(m *Manifest) { m.ChunkSize = layout.MaxChunkSize * 2 }), wantErr: ErrInvalidChunkSize},
		{name: "版本不符", m: valid(func(m *Manifest) { m.Version = 0 }), wantErr: ErrUnsupportedVersion},
		{name: "chunkSize 为零", m: valid(func(m *Manifest) { m.ChunkSize = 0 }), wantErr: ErrInvalidChunkSize},
		{name: "chunkSize 为负", m: valid(func(m *Manifest) { m.ChunkSize = -1024 }), wantErr: ErrInvalidChunkSize},
		{name: "chunkSize 非 2 的幂", m: valid(func(m *Manifest) { m.ChunkSize = 3 << 19 }), wantErr: ErrInvalidChunkSize},
		{name: "非法路径", m: New(1<<20, []Entry{{Path: "../escape"}}), wantErr: ErrInvalidPath},
		{name: "负 size", m: New(1<<20, []Entry{{Path: "a", Size: -1}}), wantErr: ErrNegativeSize},
		{name: "目录负 size 同拒", m: New(1<<20, []Entry{{Path: "d", Size: -1, IsDir: true}}), wantErr: ErrNegativeSize},
		{
			name: "前缀和恰达上限",
			m: New(layout.MaxChunkSize, []Entry{
				{Path: "a", Size: maxSize - 1},
				{Path: "b", Size: 1},
			}),
		},
		{
			name:    "块状态超限",
			m:       New(layout.MinChunkSize, []Entry{{Path: "a", Size: int64(layout.MaxChunkCount)*layout.MinChunkSize + 1}}),
			wantErr: ErrTooManyChunks,
		},
		{
			name: "前缀和超上限",
			m: New(1<<20, []Entry{
				{Path: "a", Size: maxSize},
				{Path: "b", Size: 1},
			}),
			wantErr: ErrStreamTooLarge,
		},
		{
			// 两个巨型 size 相加会回绕 int64,先比较后累加必须兜住。
			name: "前缀和回绕",
			m: New(1<<20, []Entry{
				{Path: "a", Size: 1 << 62},
				{Path: "b", Size: 1 << 62},
			}),
			wantErr: ErrStreamTooLarge,
		},
		{
			// 目录不占流(§6.4):目录 size 不计入前缀和。
			name: "目录 size 不计入流",
			m: New(layout.MaxChunkSize, []Entry{
				{Path: "d", Size: maxSize, IsDir: true},
				{Path: "f", Size: maxSize},
			}),
		},
		{name: "路径重复", m: New(1<<20, []Entry{{Path: "a"}, {Path: "a"}}), wantErr: ErrDuplicatePath},
		{name: "大小写折叠碰撞", m: New(1<<20, []Entry{{Path: "A.txt"}, {Path: "a.txt"}}), wantErr: ErrPathCollision},
		{name: "大小写折叠碰撞跨目录段", m: New(1<<20, []Entry{{Path: "Dir/x"}, {Path: "dir/x"}}), wantErr: ErrPathCollision},
		{name: "隐式目录前缀折叠碰撞", m: New(1<<20, []Entry{{Path: "Dir/a"}, {Path: "dir/b"}}), wantErr: ErrPathCollision},
		{name: "Unicode 隐式目录前缀折叠碰撞", m: New(1<<20, []Entry{{Path: "ẞ/a"}, {Path: "ss/b"}}), wantErr: ErrPathCollision},
		{name: "显式目录与子项拼写不一致", m: New(1<<20, []Entry{{Path: "Dir", IsDir: true}, {Path: "dir/a"}}), wantErr: ErrPathCollision},
		{name: "文件不能作为后续条目的祖先", m: New(1<<20, []Entry{{Path: "node"}, {Path: "node/child"}}), wantErr: ErrPathTypeConflict},
		{name: "文件不能覆盖已有隐式目录", m: New(1<<20, []Entry{{Path: "node/child"}, {Path: "node"}}), wantErr: ErrPathTypeConflict},
		{name: "排序靠前的兄弟不能遮蔽文件祖先冲突", m: New(1<<20, []Entry{{Path: "a"}, {Path: "a!"}, {Path: "a/b"}}), wantErr: ErrPathTypeConflict},
		{name: "排序靠前的兄弟不能遮蔽显式目录折叠冲突", m: New(1<<20, []Entry{{Path: "A", IsDir: true}, {Path: "a!"}, {Path: "a/b"}}), wantErr: ErrPathCollision},
		// 大写 ẞ 全折叠为 ss:非平凡 Unicode 折叠碰撞。
		{name: "Unicode 折叠碰撞", m: New(1<<20, []Entry{{Path: "ẞ.txt"}, {Path: "ss.txt"}}), wantErr: ErrPathCollision},
		{name: "NFD 路径被拒", m: New(1<<20, []Entry{{Path: "café"}}), wantErr: ErrInvalidPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.m.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("expected success, got: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error category mismatch: got %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidatePreservesLayoutGeometryCause(t *testing.T) {
	tests := []struct {
		name     string
		manifest *Manifest
		outer    error
		cause    error
	}{
		{
			name:     "chunk size",
			manifest: New(layout.MinChunkSize/2, []Entry{{Path: "a", Size: 1}}),
			outer:    ErrInvalidChunkSize,
			cause:    layout.ErrChunkSizeTooSmall,
		},
		{
			name: "chunk count",
			manifest: New(layout.MinChunkSize, []Entry{{
				Path: "a",
				Size: int64(layout.MaxChunkCount)*layout.MinChunkSize + 1,
			}}),
			outer: ErrTooManyChunks,
			cause: layout.ErrTooManyChunks,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.manifest.Validate()
			if !errors.Is(err, tc.outer) || !errors.Is(err, tc.cause) {
				t.Fatalf("err=%v, want outer %v and cause %v", err, tc.outer, tc.cause)
			}
		})
	}
}

func TestValidateStopsAtFirstError(t *testing.T) {
	// 路径校验先于 size 校验:同一条目两处皆错时报路径错,错误顺序稳定可依赖。
	m := New(1<<20, []Entry{{Path: "..", Size: -1}})
	if err := m.Validate(); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("want ErrInvalidPath, got %v", err)
	}
}

func TestManifestDiagnosticsBoundHostilePaths(t *testing.T) {
	longPath := strings.Repeat("a", 1<<18)
	tests := []struct {
		manifest *Manifest
		want     error
	}{
		{manifest: New(1<<20, []Entry{{Path: longPath}, {Path: longPath}}), want: ErrDuplicatePath},
		{manifest: New(1<<20, []Entry{{Path: longPath, Size: -1}}), want: ErrNegativeSize},
		{manifest: New(1<<20, []Entry{{Path: longPath, Size: layout.MaxStreamBytes + 1}}), want: ErrStreamTooLarge},
	}
	for _, tt := range tests {
		err := tt.manifest.Validate()
		if !errors.Is(err, tt.want) {
			t.Fatalf("Validate error = %v, want %v", err, tt.want)
		}
		if !utf8.ValidString(err.Error()) {
			t.Fatal("manifest diagnostic is not valid UTF-8")
		}
		if got, limit := len(err.Error()), 4*maxPathDiagnosticBytes; got > limit {
			t.Fatalf("manifest diagnostic is %d bytes, limit %d", got, limit)
		}
	}
}

func TestValidateHugeManifestFastEnough(t *testing.T) {
	// 校验对条目数应线性:64k 条目应瞬时完成(折叠碰撞用哈希表而非两两比对)。
	entries := make([]Entry, 0, 1<<16)
	for i := range 1 << 16 {
		entries = append(entries, Entry{Path: "d/" + strings.Repeat("x", 8) + "-" + strconv.Itoa(i), Size: 1})
	}
	if err := New(1<<20, entries).Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

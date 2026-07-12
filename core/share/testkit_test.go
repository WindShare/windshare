package share_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/layout"
	"github.com/windshare/windshare/core/share"
)

// ── 金标/键石共用的固定输入(§7:确定性命脉 = 注入 readSecret 与随机源)。

var (
	// goldSecret = 00..0f,与 keyderiv KAT 的 secret A 同值,方便逐层对拍。
	goldSecret = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	// goldShareID = base64url(00..08):9B 路由句柄的固定形态。
	goldShareID = "AAECAwQFBgcI"
)

// goldChunkSize 取 1 KiB:让 2300B 的小树恰好铺出「整块/跨文件块/末短块」
// 三种几何形态。
const goldChunkSize int64 = 1024

// goldNaivePath:NFC 非 ASCII 文件名(ï = U+00EF 单码点),钉住清单 CBOR 的
// UTF-8 字节形态。若源文件的 ï 被工具链改写(如转成 NFD),金标向量对拍
// 会立刻失败,字节形态由提交的向量文件守护。
const goldNaivePath = "tree/naïve.txt"

// counterRNG 输出递增字节流 0x00,0x01,…,0xff,0x00,…:固定 RNG 注入使
// manifest nonce 与逐块 nonce 完全确定(B11),且各 nonce 互不相同。
type counterRNG struct{ next byte }

func (c *counterRNG) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = c.next
		c.next++
	}
	return len(p), nil
}

// goldContent 生成文件内容:仿射字节模式按 seed 区分文件,确定可复现。
func goldContent(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)*7 + seed
	}
	return b
}

// goldTree 返回固定小文件树。流几何(chunkSize=1024):
//
//	a.txt [0,1500) ‖ b.bin [1500,2200) ‖ naïve.txt [2200,2300);streamLen=2300 → 3 块
//	块 0 = a[0:1024];块 1 = a[1024:1500]+b[0:548];块 2(短)= b[548:700]+naïve[0:100]
//
// 含空文件、空目录(不占流,走收尾物化)与 NFC 非 ASCII 名(钉 CBOR 的
// UTF-8 字节)。
func goldTree() ([]share.FileMeta, *memSource) {
	src := newMemSource(map[string][]byte{
		"tree/a.txt":     goldContent(1500, 1),
		"tree/b.bin":     goldContent(700, 2),
		"tree/empty.txt": {},
		goldNaivePath:    goldContent(100, 3),
	})
	files := []share.FileMeta{
		{Path: "tree", MTime: 1710000000000, IsDir: true},
		{Path: "tree/a.txt", Size: 1500, MTime: 1710000000001},
		{Path: "tree/b.bin", Size: 700, MTime: 1710000000002},
		{Path: "tree/empty.txt", Size: 0, MTime: 1710000000003},
		{Path: goldNaivePath, Size: 100, MTime: 1710000000004},
		{Path: "tree/sub", MTime: 1710000000005, IsDir: true},
	}
	return files, src
}

// newGoldSharer 以固定身份 + 计数 RNG 构建金标分享。
func newGoldSharer(t *testing.T, src *memSource, files []share.FileMeta) *share.Sharer {
	t.Helper()
	s, err := share.NewSharer(files, src, share.Options{
		ChunkSize:  goldChunkSize,
		ReadSecret: goldSecret,
		ShareID:    goldShareID,
		Rand:       &counterRNG{},
	})
	if err != nil {
		t.Fatalf("NewSharer: %v", err)
	}
	return s
}

// newGoldReceiver 从分享的链接与加密名片构建接收端。
func newGoldReceiver(t *testing.T, s *share.Sharer) (*share.Receiver, *memSink) {
	t.Helper()
	sealed, err := s.SealedManifest()
	if err != nil {
		t.Fatalf("SealedManifest: %v", err)
	}
	sink := newMemSink()
	r, err := share.NewReceiver(s.Link(), sealed, sink)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	return r, sink
}

func mustPlan(t *testing.T, receiver *share.Receiver, selectors []string) *share.TransferPlan {
	t.Helper()
	plan, err := receiver.Plan(selectors)
	if err != nil {
		t.Fatalf("Plan(%v): %v", selectors, err)
	}
	return plan
}

func chunkSlice(set layout.ChunkSet) []uint64 {
	return slices.Collect(set.Iter())
}

// ── 纯内存 FileSource/FileSink(键石测试:零真实 IO、零网络,§7)。

// memSource 是纯内存 FileSource;fail 非 nil 即一律返回它(模拟 osfs.Source
// 读后复核发现漂移的中止路径,§6.3);short 模拟实现缺陷的静默短读。
type memSource struct {
	files map[string][]byte
	fail  error
	short bool
}

func newMemSource(files map[string][]byte) *memSource {
	return &memSource{files: files}
}

func (s *memSource) ReadRange(path string, off, n int64) ([]byte, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	data, ok := s.files[path]
	if !ok {
		return nil, fmt.Errorf("memSource: unknown path %q", path)
	}
	if off < 0 || n < 0 || off+n > int64(len(data)) {
		return nil, fmt.Errorf("memSource: read outside %q at off=%d n=%d", path, off, n)
	}
	out := slices.Clone(data[off : off+n])
	if s.short && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out, nil
}

// memSink 是纯内存 FileSink,记录 SetMTime 调用顺序供物化次序断言;
// fail* 注入落盘故障(failMTimeOn 非空时只对该路径生效,用于打到目录分支)。
type memSink struct {
	files    map[string][]byte
	dirs     map[string]bool
	mtimes   map[string]int64
	mtimeLog []string

	failEnsure  error
	failWrite   error
	failMTime   error
	failMTimeOn string
}

func newMemSink() *memSink {
	return &memSink{files: map[string][]byte{}, dirs: map[string]bool{}, mtimes: map[string]int64{}}
}

func (s *memSink) EnsureDir(path string) error {
	if s.failEnsure != nil {
		return s.failEnsure
	}
	s.dirs[path] = true
	return nil
}

func (s *memSink) WriteRange(path string, off int64, data []byte) error {
	if s.failWrite != nil {
		return s.failWrite
	}
	f := s.files[path]
	if need := off + int64(len(data)); int64(len(f)) < need {
		grown := make([]byte, need)
		copy(grown, f)
		f = grown
	}
	copy(f[off:], data)
	s.files[path] = f
	return nil
}

func (s *memSink) SetMTime(path string, mtime int64) error {
	if s.failMTime != nil && (s.failMTimeOn == "" || s.failMTimeOn == path) {
		return s.failMTime
	}
	s.mtimes[path] = mtime
	s.mtimeLog = append(s.mtimeLog, path)
	return nil
}

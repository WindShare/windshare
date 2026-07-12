package share_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"

	"github.com/windshare/windshare/core/chunk"
	"github.com/windshare/windshare/core/internal/keyderiv"
	"github.com/windshare/windshare/core/layout"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/share"
)

// acceptAll 把 Sharer 的全部块按给定顺序喂给 Receiver(键石管线的最短通路)。
func acceptAll(t *testing.T, s *share.Sharer, plan *share.TransferPlan, order []uint64) {
	t.Helper()
	for _, i := range order {
		ct, err := s.Chunk(i)
		if err != nil {
			t.Fatalf("Chunk(%d): %v", i, err)
		}
		if err := plan.Accept(i, ct); err != nil {
			t.Fatalf("Accept(%d): %v", i, err)
		}
	}
}

// requireTreeRebuilt 断言 sink 与源树逐字节一致且元数据齐全。
func requireTreeRebuilt(t *testing.T, src *memSource, files []share.FileMeta, sink *memSink) {
	t.Helper()
	for path, want := range src.files {
		if got, ok := sink.files[path]; !ok || !bytes.Equal(got, want) {
			t.Errorf("%q reconstruction mismatch: got %d bytes, want %d", path, len(sink.files[path]), len(want))
		}
	}
	for _, f := range files {
		if f.IsDir && !sink.dirs[f.Path] {
			t.Errorf("directory %q was not materialized", f.Path)
		}
		if got := sink.mtimes[f.Path]; got != f.MTime {
			t.Errorf("%q mtime = %d, want %d", f.Path, got, f.MTime)
		}
	}
}

// 键石:内存级端到端(§7)——Sharer.Chunk 全量喂 TransferPlan.Accept,
// 重建字节 ⩵ 原始,空文件/空目录物化,mtime 保真且目录深度逆序。
func TestEndToEnd(t *testing.T) {
	files, src := goldTree()
	sharer := newGoldSharer(t, src, files)
	receiver, sink := newGoldReceiver(t, sharer)
	plan := mustPlan(t, receiver, nil)

	if sharer.NumChunks() != 3 || receiver.NumChunks() != 3 {
		t.Fatalf("NumChunks = %d/%d, want 3", sharer.NumChunks(), receiver.NumChunks())
	}
	if got := plan.Chunks().Ranges(); !slices.Equal(got, []layout.ChunkRange{{End: 3}}) {
		t.Fatalf("full plan is not compact: %v", got)
	}
	acceptAll(t, sharer, plan, []uint64{0, 1, 2})
	if have := plan.Sink().Have().Count(); have != 3 {
		t.Fatalf("have=%d, want 3", have)
	}
	if err := plan.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	requireTreeRebuilt(t, src, files, sink)

	// 物化次序:目录 mtime 必须晚于其全部子内容(文件 mtime 与子目录),
	// 且子目录先于父目录(深度逆序,§6.6)。
	pos := map[string]int{}
	for i, path := range sink.mtimeLog {
		pos[path] = i
	}
	for _, child := range []string{"tree/a.txt", "tree/empty.txt", "tree/sub"} {
		if pos["tree"] < pos[child] {
			t.Errorf("directory tree mtime was set before child %q (%d < %d)", child, pos["tree"], pos[child])
		}
	}
}

// 乱序 Accept:块是原子交付单位,顺序不是协议约束(§6.12)。
func TestOutOfOrderAccept(t *testing.T) {
	files, src := goldTree()
	sharer := newGoldSharer(t, src, files)
	receiver, sink := newGoldReceiver(t, sharer)
	plan := mustPlan(t, receiver, nil)

	acceptAll(t, sharer, plan, []uint64{2, 0, 1})
	if err := plan.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	requireTreeRebuilt(t, src, files, sink)
}

// Boundary chunks are authenticated in full, but the plan writes only selected ranges.
func TestSelectiveSubsetDoesNotMaterializeBoundarySiblings(t *testing.T) {
	files, src := goldTree()
	sharer := newGoldSharer(t, src, files)
	receiver, sink := newGoldReceiver(t, sharer)
	plan := mustPlan(t, receiver, []string{"tree/b.bin"})

	need := chunkSlice(plan.Chunks())
	if !slices.Equal(need, []uint64{1, 2}) {
		t.Fatalf("plan chunks = %v, want [1 2]", need)
	}
	if plan.SelectedBytes() != int64(len(src.files["tree/b.bin"])) {
		t.Fatalf("selected bytes=%d", plan.SelectedBytes())
	}
	var progressBytes int64
	for _, index := range need {
		bytesInChunk, err := plan.SelectedBytesInChunk(index)
		if err != nil {
			t.Fatalf("SelectedBytesInChunk(%d): %v", index, err)
		}
		progressBytes += bytesInChunk
	}
	if progressBytes != plan.SelectedBytes() {
		t.Fatalf("per-chunk selected bytes = %d, want %d", progressBytes, plan.SelectedBytes())
	}
	if selected := plan.SelectedEntries(); len(selected) != 1 || selected[0].Path != "tree/b.bin" {
		t.Fatalf("selected entries=%v", selected)
	}
	if err := plan.Finalize(); !errors.Is(err, share.ErrMissingBlocks) {
		t.Fatalf("early Finalize err = %v, want ErrMissingBlocks", err)
	}
	unselectedBlock, err := sharer.Chunk(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Accept(0, unselectedBlock); !errors.Is(err, share.ErrChunkNotSelected) {
		t.Fatalf("unselected block err=%v, want ErrChunkNotSelected", err)
	}

	acceptAll(t, sharer, plan, need)
	if err := plan.Finalize(); err != nil {
		t.Fatalf("Finalize(b.bin): %v", err)
	}
	if !bytes.Equal(sink.files["tree/b.bin"], src.files["tree/b.bin"]) {
		t.Error("b.bin reconstruction mismatch")
	}
	for _, unselected := range []string{"tree/a.txt", goldNaivePath, "tree/empty.txt"} {
		if _, exists := sink.files[unselected]; exists {
			t.Errorf("unselected boundary sibling %q was materialized", unselected)
		}
		if _, exists := sink.mtimes[unselected]; exists {
			t.Errorf("unselected path %q received mtime", unselected)
		}
	}

	// Have-state belongs to the plan. A different selection cannot inherit boundary
	// chunks whose newly selected ranges were never materialized.
	full := mustPlan(t, receiver, nil)
	if err := full.Finalize(); !errors.Is(err, share.ErrMissingBlocks) {
		t.Fatalf("independent full plan err=%v, want ErrMissingBlocks", err)
	}
}

// 目录子树选择:选整个 tree 等价全选。
func TestSelectiveSubtree(t *testing.T) {
	files, src := goldTree()
	sharer := newGoldSharer(t, src, files)
	receiver, sink := newGoldReceiver(t, sharer)
	plan := mustPlan(t, receiver, []string{"tree"})

	need := chunkSlice(plan.Chunks())
	if !slices.Equal(need, []uint64{0, 1, 2}) {
		t.Fatalf("plan chunks = %v", need)
	}
	acceptAll(t, sharer, plan, need)
	if err := plan.Finalize(); err != nil {
		t.Fatalf("Finalize(tree): %v", err)
	}
	requireTreeRebuilt(t, src, files, sink)
}

func TestPlanFinalizesSelectedEmptyEntriesWithoutChunks(t *testing.T) {
	files, src := goldTree()
	sharer := newGoldSharer(t, src, files)
	receiver, sink := newGoldReceiver(t, sharer)
	plan := mustPlan(t, receiver, []string{"tree/empty.txt", "tree/sub"})

	if !plan.Chunks().IsEmpty() || plan.SelectedBytes() != 0 {
		t.Fatalf("empty-only plan has chunks/bytes: %v/%d", plan.Chunks().Ranges(), plan.SelectedBytes())
	}
	if err := plan.Finalize(); err != nil {
		t.Fatalf("Finalize empty-only plan: %v", err)
	}
	if data, exists := sink.files["tree/empty.txt"]; !exists || len(data) != 0 {
		t.Fatal("selected empty file was not materialized")
	}
	if !sink.dirs["tree/sub"] {
		t.Fatal("selected empty directory was not materialized")
	}
	if sink.dirs["tree"] {
		t.Fatal("unselected parent directory entry was materialized")
	}
}

func TestPlanRejectsUnknownSelectorAtConstruction(t *testing.T) {
	files, src := goldTree()
	sharer := newGoldSharer(t, src, files)
	receiver, _ := newGoldReceiver(t, sharer)
	if _, err := receiver.Plan([]string{"no/such"}); !errors.Is(err, layout.ErrUnknownPath) {
		t.Fatalf("Plan unknown selector err=%v, want ErrUnknownPath", err)
	}
}

func TestPlanCanonicalizesSelectorsBeforeResolution(t *testing.T) {
	files, src := goldTree()
	sharer := newGoldSharer(t, src, files)
	receiver, _ := newGoldReceiver(t, sharer)

	nfc := mustPlan(t, receiver, []string{goldNaivePath})
	nfdSelector := "tree/nai\u0308ve.txt"
	nfd := mustPlan(t, receiver, []string{nfdSelector})
	if nfc.PlanID() != nfd.PlanID() || !slices.Equal(nfc.SelectedEntries(), nfd.SelectedEntries()) {
		t.Fatalf("canonically equivalent selectors diverged: NFC=%s NFD=%s", nfc.PlanID(), nfd.PlanID())
	}
	deduplicated := mustPlan(t, receiver, []string{goldNaivePath, nfdSelector})
	if deduplicated.PlanID() != nfc.PlanID() || len(deduplicated.SelectedEntries()) != 1 {
		t.Fatalf("canonical selector deduplication failed: id=%s entries=%d", deduplicated.PlanID(), len(deduplicated.SelectedEntries()))
	}
	if _, err := receiver.Plan([]string{"tree/../naïve.txt"}); !errors.Is(err, manifest.ErrInvalidPath) {
		t.Fatalf("unsafe selector error = %v, want ErrInvalidPath", err)
	}
}

func TestReceiverUsesSuiteOwnedMaximumSealedSize(t *testing.T) {
	files, src := goldTree()
	sharer := newGoldSharer(t, src, files)
	receiver, _ := newGoldReceiver(t, sharer)
	want, err := chunk.MaxSealedSize(link.SuiteAESGCM, goldChunkSize)
	if err != nil {
		t.Fatalf("MaxSealedSize: %v", err)
	}
	if got := receiver.MaxBlockBytes(); got != want {
		t.Fatalf("MaxBlockBytes = %d, want suite authority %d", got, want)
	}
}

func TestTransferSinkDeclaresRandomWriteDelivery(t *testing.T) {
	files, src := goldTree()
	sharer := newGoldSharer(t, src, files)
	receiver, _ := newGoldReceiver(t, sharer)
	if got := mustPlan(t, receiver, nil).Sink().DeliveryOrder(); got != session.DeliveryAnyOrder {
		t.Fatalf("TransferSink delivery order = %d, want DeliveryAnyOrder", got)
	}
}

func TestPlanIDUsesCanonicalSelectedEntries(t *testing.T) {
	files, src := goldTree()
	sharer := newGoldSharer(t, src, files)
	receiver, _ := newGoldReceiver(t, sharer)

	first := mustPlan(t, receiver, []string{"tree/b.bin", "tree/empty.txt"})
	second := mustPlan(t, receiver, []string{"tree/empty.txt", "tree/b.bin", "tree/b.bin"})
	if first.PlanID() != second.PlanID() {
		t.Fatalf("equivalent selections have different PlanID: %s != %s", first.PlanID(), second.PlanID())
	}
	const expectedPlanID = "41d202af62f8163dabd184b31c528e421d514fa66e2b053cb6e5ec8011c314df"
	if first.PlanID().String() != expectedPlanID {
		t.Fatalf("PlanID=%s, want %s", first.PlanID(), expectedPlanID)
	}
	if first.PlanID() == mustPlan(t, receiver, []string{"tree/b.bin"}).PlanID() {
		t.Fatal("different selections have identical PlanID")
	}
	if full, subtree := mustPlan(t, receiver, nil), mustPlan(t, receiver, []string{"tree"}); full.PlanID() != subtree.PlanID() {
		t.Fatalf("equivalent full selections differ: %s != %s", full.PlanID(), subtree.PlanID())
	}
	entries := first.SelectedEntries()
	entries[0].Path = "mutated"
	if first.SelectedEntries()[0].Path == "mutated" {
		t.Fatal("SelectedEntries exposed mutable plan storage")
	}
}

// 篡改块拒收:密文任意一字节翻转 → AEAD 失败,不触盘、不置位(§6.5)。
func TestTamperedBlockRejected(t *testing.T) {
	files, src := goldTree()
	s := newGoldSharer(t, src, files)
	r, sink := newGoldReceiver(t, s)
	plan := mustPlan(t, r, nil)

	ct, err := s.Chunk(0)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	ct[len(ct)/2] ^= 0x01
	if err := plan.Accept(0, ct); err == nil {
		t.Fatal("tampered block was accepted")
	}
	if len(sink.files) != 0 {
		t.Error("rejected block should not reach the sink")
	}
	if have := plan.Sink().Have().Count(); have != 0 {
		t.Errorf("rejected block advanced have-state to %d", have)
	}
}

// 错位块拒收:块 1 的密文冒充块 2 → AAD 位置绑定失败(§6.5)。
func TestMisplacedBlockRejected(t *testing.T) {
	files, src := goldTree()
	s := newGoldSharer(t, src, files)
	r, _ := newGoldReceiver(t, s)
	plan := mustPlan(t, r, nil)

	ct, err := s.Chunk(1)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if err := plan.Accept(2, ct); err == nil {
		t.Fatal("mispositioned block was accepted")
	}
	if err := plan.Accept(3, ct); !errors.Is(err, layout.ErrChunkOutOfRange) {
		t.Errorf("out-of-range block err = %v, want ErrChunkOutOfRange", err)
	}
	if _, err := s.Chunk(3); !errors.Is(err, layout.ErrChunkOutOfRange) {
		t.Errorf("out-of-range Chunk err = %v, want ErrChunkOutOfRange", err)
	}
	if err := plan.Sink().WriteBlock(3, nil); !errors.Is(err, layout.ErrChunkOutOfRange) {
		t.Errorf("out-of-range WriteBlock err = %v, want ErrChunkOutOfRange", err)
	}
}

// 跨分享块拒收:另一分享(不同 readSecret)的同号块 → 密钥树不同,Open 失败
// (§6.5 同源密钥绑定)。
func TestCrossShareRejected(t *testing.T) {
	files, src := goldTree()
	s := newGoldSharer(t, src, files)
	r, _ := newGoldReceiver(t, s)
	plan := mustPlan(t, r, nil)

	other, err := share.NewSharer(files, src, share.Options{
		ChunkSize:  goldChunkSize,
		ReadSecret: bytes.Repeat([]byte{0xff}, link.ReadSecretBytes),
		ShareID:    goldShareID,
		Rand:       &counterRNG{},
	})
	if err != nil {
		t.Fatalf("NewSharer(other): %v", err)
	}
	ct, err := other.Chunk(0)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if err := plan.Accept(0, ct); err == nil {
		t.Fatal("block from another share was accepted")
	}
}

// 错长块拒收:持链者能 Seal 出合法 tag 的短块,AEAD 管不了长度——几何
// 一致性由接收门面收口(ErrBlockLength,§6.5)。
func TestWrongLengthBlockRejected(t *testing.T) {
	files, src := goldTree()
	s := newGoldSharer(t, src, files)
	r, sink := newGoldReceiver(t, s)
	plan := mustPlan(t, r, nil)

	codec, err := chunk.NewCodec(keyderiv.StreamKey(goldSecret), goldChunkSize, &counterRNG{})
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	forged, err := codec.Seal(0, []byte("短明文"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := plan.Accept(0, forged); !errors.Is(err, share.ErrBlockLength) {
		t.Fatalf("err = %v, want ErrBlockLength", err)
	}
	if len(sink.files) != 0 {
		t.Error("wrong-length block should not reach the sink")
	}
}

// 漂移中止:Source 注入变更(读后复核失败)→ Chunk/ReadBlock 报错,
// 分享中止(§6.3)。
func TestDriftAborts(t *testing.T) {
	files, src := goldTree()
	s := newGoldSharer(t, src, files)

	drift := errors.New("osfs: source file changed; reshare required")
	src.fail = drift
	if _, err := s.Chunk(0); !errors.Is(err, drift) {
		t.Errorf("Chunk err = %v, want drift error", err)
	}
	if _, err := s.BlockStore().ReadBlock(0); !errors.Is(err, drift) {
		t.Errorf("ReadBlock err = %v, want drift error", err)
	}
}

// FileSource 短读拒发:静默短块会在接收端变成不可归因的解密失败。
func TestShortReadRejected(t *testing.T) {
	files, src := goldTree()
	s := newGoldSharer(t, src, files)
	src.short = true
	if _, err := s.Chunk(0); !errors.Is(err, share.ErrShortRead) {
		t.Errorf("err = %v, want ErrShortRead", err)
	}
}

// Seal-once 字节复用:SealedManifest 恒返同一字节串(GCM tag 即清单指纹,
// §6.3),且返回值为克隆——调用方改动不了缓存。
func TestSealedManifestReuse(t *testing.T) {
	files, src := goldTree()
	s := newGoldSharer(t, src, files)

	first, err := s.SealedManifest()
	if err != nil {
		t.Fatalf("SealedManifest: %v", err)
	}
	first[0] ^= 0xff
	second, err := s.SealedManifest()
	if err != nil {
		t.Fatalf("SealedManifest: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("return value should be a clone")
	}
	first[0] ^= 0xff
	if !bytes.Equal(first, second) {
		t.Fatal("SealedManifest bytes differ between calls; resealing would change the fingerprint")
	}
}

// Link 的密钥克隆语义,以及全空 Options 的生产默认路径(随机身份可用)。
func TestLinkAndDefaults(t *testing.T) {
	files, src := goldTree()
	s := newGoldSharer(t, src, files)
	l := s.Link()
	if l.Suite != link.SuiteAESGCM || l.ShareID != goldShareID || !bytes.Equal(l.ReadSecret, goldSecret) {
		t.Errorf("Link = %+v", l)
	}
	l.ReadSecret[0] ^= 0xff
	if !bytes.Equal(s.Link().ReadSecret, goldSecret) {
		t.Error("mutating the Link result changed the facade's internal key")
	}

	def, err := share.NewSharer(files, src, share.Options{})
	if err != nil {
		t.Fatalf("NewSharer with zero Options: %v", err)
	}
	if def.NumChunks() != 1 {
		// 默认 1 MiB 块:2300B 的树只有 1 块。
		t.Errorf("NumChunks = %d, want 1", def.NumChunks())
	}
	sealed, err := def.SealedManifest()
	if err != nil {
		t.Fatalf("SealedManifest: %v", err)
	}
	if _, err := share.NewReceiver(def.Link(), sealed, newMemSink()); err != nil {
		t.Errorf("random identity round trip failed: %v", err)
	}
}

// 空分享:只有目录与空文件 → 0 块,收尾物化独立完成整棵树(§6.6)。
func TestEmptyShare(t *testing.T) {
	files := []share.FileMeta{
		{Path: "hollow", MTime: 1710000000010, IsDir: true},
		{Path: "hollow/void.txt", Size: 0, MTime: 1710000000011},
	}
	src := newMemSource(map[string][]byte{"hollow/void.txt": {}})
	s := newGoldSharer(t, src, files)
	r, sink := newGoldReceiver(t, s)
	plan := mustPlan(t, r, nil)

	if s.NumChunks() != 0 || !plan.Chunks().IsEmpty() {
		t.Fatalf("empty-share geometry mismatch: NumChunks=%d chunks=%v", s.NumChunks(), plan.Chunks().Ranges())
	}
	if err := plan.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !sink.dirs["hollow"] {
		t.Error("empty directory was not materialized")
	}
	if got, ok := sink.files["hollow/void.txt"]; !ok || len(got) != 0 {
		t.Error("empty file was not materialized")
	}
	if sink.mtimes["hollow"] != 1710000000010 || sink.mtimes["hollow/void.txt"] != 1710000000011 {
		t.Errorf("mtime mismatch: %v", sink.mtimes)
	}
}

// 构造期参数校验(发送侧)。
func TestNewSharerRejects(t *testing.T) {
	files, src := goldTree()
	tests := []struct {
		name    string
		files   []share.FileMeta
		src     share.FileSource
		opt     share.Options
		wantErr error
	}{
		{"src 为空", files, nil, share.Options{}, share.ErrNilDependency},
		{"readSecret 短", files, src, share.Options{ReadSecret: []byte{1, 2}}, share.ErrBadOptions},
		{"shareId 非法", files, src, share.Options{ShareID: "not-base64url!"}, share.ErrBadOptions},
		{"chunkSize 非 2 幂", files, src, share.Options{ChunkSize: 1000}, layout.ErrChunkSizeNotPow2},
		{"chunkSize 低于下限", files, src, share.Options{ChunkSize: layout.MinChunkSize / 2}, layout.ErrChunkSizeTooSmall},
		{"块状态超限", []share.FileMeta{{
			Path: "huge",
			Size: int64(layout.MaxChunkCount)*layout.MinChunkSize + 1,
		}}, src, share.Options{ChunkSize: layout.MinChunkSize}, layout.ErrTooManyChunks},
		{"size 为负", []share.FileMeta{{Path: "x", Size: -1}}, src, share.Options{}, layout.ErrNegativeSize},
		{"路径重复", []share.FileMeta{{Path: "x", Size: 1}, {Path: "x", Size: 1}}, src, share.Options{}, layout.ErrDuplicatePath},
		{"路径非法", []share.FileMeta{{Path: "../escape", Size: 1}}, src, share.Options{}, manifest.ErrInvalidPath},
		// 折叠碰撞只有 manifest 层能查(layout 只看字节),验证 Seal 校验接线。
		{"折叠碰撞", []share.FileMeta{{Path: "A.txt", Size: 1}, {Path: "a.txt", Size: 1}}, src, share.Options{}, manifest.ErrPathCollision},
		{"隐式目录折叠碰撞", []share.FileMeta{{Path: "Dir/a", Size: 1}, {Path: "dir/b", Size: 1}}, src, share.Options{}, manifest.ErrPathCollision},
		{"文件祖先冲突", []share.FileMeta{{Path: "node", Size: 1}, {Path: "node/child", Size: 1}}, src, share.Options{}, manifest.ErrPathTypeConflict},
		// 随机源枯竭:readSecret 生成失败 / 16B 后 shareId 生成失败。
		{"随机源空", files, src, share.Options{Rand: bytes.NewReader(nil)}, io.EOF},
		{"随机源仅够 readSecret", files, src, share.Options{Rand: bytes.NewReader(make([]byte, link.ReadSecretBytes))}, io.EOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := share.NewSharer(tt.files, tt.src, tt.opt); !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// 构造期校验(接收侧):坏 sink、未知套件、坏密钥、篡改清单一律拒绝。
func TestNewReceiverRejects(t *testing.T) {
	files, src := goldTree()
	s := newGoldSharer(t, src, files)
	sealed, err := s.SealedManifest()
	if err != nil {
		t.Fatalf("SealedManifest: %v", err)
	}
	good := s.Link()

	if _, err := share.NewReceiver(good, sealed, nil); !errors.Is(err, share.ErrNilDependency) {
		t.Errorf("nil dst err = %v", err)
	}
	bad := good
	bad.Suite = 0x7f
	if _, err := share.NewReceiver(bad, sealed, newMemSink()); !errors.Is(err, link.ErrUnknownSuite) {
		t.Errorf("unknown suite err = %v", err)
	}
	bad = good
	bad.ReadSecret = []byte{1}
	if _, err := share.NewReceiver(bad, sealed, newMemSink()); !errors.Is(err, share.ErrBadLink) {
		t.Errorf("invalid key err = %v", err)
	}
	bad = good
	bad.ReadSecret = bytes.Repeat([]byte{0xee}, link.ReadSecretBytes)
	if _, err := share.NewReceiver(bad, sealed, newMemSink()); err == nil {
		t.Error("unseal with the wrong readSecret should fail")
	}
	tampered := slices.Clone(sealed)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := share.NewReceiver(good, tampered, newMemSink()); err == nil {
		t.Error("tampered manifest should be rejected")
	}
}

// 伪造清单结构被拒(§6.13 接收端不信任清单):绕过发送侧校验、以合法密钥
// 直接封装畸形清单,接收门面的校验链(Validate + NewCodec 界)必须拦下。
func TestReceiverRejectsForgedStructure(t *testing.T) {
	tests := []struct {
		name    string
		m       *manifest.Manifest
		wantErr error
	}{
		{"负 size", manifest.New(goldChunkSize, []manifest.Entry{{Path: "x", Size: -5}}), manifest.ErrNegativeSize},
		{"chunkSize 非 2 幂", manifest.New(1000, []manifest.Entry{{Path: "x", Size: 1}}), manifest.ErrInvalidChunkSize},
		{"chunkSize 低于下限", manifest.New(layout.MinChunkSize/2, []manifest.Entry{{Path: "x", Size: 1}}), manifest.ErrInvalidChunkSize},
		{"chunkSize 超上限", manifest.New(layout.MaxChunkSize*2, []manifest.Entry{{Path: "x", Size: 1}}), manifest.ErrInvalidChunkSize},
		{"块状态超限", manifest.New(layout.MinChunkSize, []manifest.Entry{{
			Path: "x",
			Size: int64(layout.MaxChunkCount)*layout.MinChunkSize + 1,
		}}), manifest.ErrTooManyChunks},
		{"路径穿越", manifest.New(goldChunkSize, []manifest.Entry{{Path: "../up", Size: 1}}), manifest.ErrInvalidPath},
		{"折叠碰撞", manifest.New(goldChunkSize, []manifest.Entry{{Path: "A.txt", Size: 1}, {Path: "a.txt", Size: 1}}), manifest.ErrPathCollision},
		{"隐式目录折叠碰撞", manifest.New(goldChunkSize, []manifest.Entry{{Path: "Dir/a", Size: 1}, {Path: "dir/b", Size: 1}}), manifest.ErrPathCollision},
		{"文件祖先冲突", manifest.New(goldChunkSize, []manifest.Entry{{Path: "node", Size: 1}, {Path: "node/child", Size: 1}}), manifest.ErrPathTypeConflict},
	}
	l := link.Link{Suite: link.SuiteAESGCM, ReadSecret: goldSecret, ShareID: goldShareID}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sealed := forgeSealed(t, tt.m)
			if _, err := share.NewReceiver(l, sealed, newMemSink()); !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// forgeSealed 以攻击者视角伪造 sealedManifest:与 manifest 包同参的确定性
// CBOR + stdlib GCM(aad=suiteByte),不经 manifest.Seal 的构建期校验。
func forgeSealed(t *testing.T, m *manifest.Manifest) []byte {
	t.Helper()
	opts := cbor.CoreDetEncOptions()
	opts.NilContainers = cbor.NilContainerAsEmpty
	em, err := opts.EncMode()
	if err != nil {
		t.Fatalf("cbor: %v", err)
	}
	plain, err := em.Marshal(m)
	if err != nil {
		t.Fatalf("encode CBOR: %v", err)
	}
	block, err := aes.NewCipher(keyderiv.ManifestKey(goldSecret))
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := bytes.Repeat([]byte{0x5a}, chunk.NonceBytes)
	return aead.Seal(nonce, nonce, plain, []byte{link.SuiteAESGCM})
}

// FileSink 故障沿门面如实上抛:Accept 写失败不置位;Finalize 的物化与
// mtime(含目录分支)各自传播。
func TestSinkFailuresPropagate(t *testing.T) {
	files, src := goldTree()
	boom := errors.New("sink failure")

	t.Run("Accept 写失败", func(t *testing.T) {
		s := newGoldSharer(t, src, files)
		r, sink := newGoldReceiver(t, s)
		plan := mustPlan(t, r, nil)
		sink.failWrite = boom
		ct, err := s.Chunk(0)
		if err != nil {
			t.Fatalf("Chunk: %v", err)
		}
		if err := plan.Accept(0, ct); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want injected failure", err)
		}
		if plan.Sink().Have().Count() != 0 {
			t.Error("failed write should not set the have bit")
		}
	})

	t.Run("Finalize 各阶段", func(t *testing.T) {
		s := newGoldSharer(t, src, files)
		r, sink := newGoldReceiver(t, s)
		plan := mustPlan(t, r, nil)
		acceptAll(t, s, plan, []uint64{0, 1, 2})
		sink.failEnsure = boom
		if err := plan.Finalize(); !errors.Is(err, boom) {
			t.Errorf("directory materialization failure was not returned: %v", err)
		}
		sink.failEnsure = nil
		sink.failWrite = boom
		if err := plan.Finalize(); !errors.Is(err, boom) {
			t.Errorf("empty-file materialization failure was not returned: %v", err)
		}
		sink.failWrite = nil
		sink.failMTime = boom
		if err := plan.Finalize(); !errors.Is(err, boom) {
			t.Errorf("file mtime failure was not returned: %v", err)
		}
		sink.failMTimeOn = "tree" // 只挂目录:文件 mtime 全过,打到目录分支
		if err := plan.Finalize(); !errors.Is(err, boom) {
			t.Errorf("directory mtime failure was not returned: %v", err)
		}
	})
}

func TestTransferErrorsBoundHostilePaths(t *testing.T) {
	const hostilePathBytes = 1 << 20
	hostile := strings.Repeat("é", hostilePathBytes/len("é"))
	const secretMarker = "DO_NOT_PRINT_SECRET"
	boom := errors.New(strings.Repeat(secretMarker, 512))
	assertSafe := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, boom) {
			t.Fatal("injected failure is not reachable through errors.Is")
		}
		const maxDiagnosticBytes = 1024
		message := err.Error()
		if got := len(message); got > maxDiagnosticBytes {
			t.Fatalf("transfer diagnostic length = %d, want <= %d", got, maxDiagnosticBytes)
		}
		if !utf8.ValidString(message) {
			t.Fatal("transfer diagnostic is not valid UTF-8")
		}
		if !strings.Contains(message, "…") {
			t.Fatal("transfer diagnostic does not identify truncation")
		}
		if strings.Contains(message, secretMarker) {
			t.Fatal("transfer diagnostic rendered sensitive wrapped-cause text")
		}
	}

	t.Run("receiver sink", func(t *testing.T) {
		files := []share.FileMeta{{Path: hostile, IsDir: true}}
		s := newGoldSharer(t, newMemSource(nil), files)
		r, sink := newGoldReceiver(t, s)
		plan := mustPlan(t, r, nil)
		sink.failEnsure = boom
		assertSafe(t, plan.Finalize())
	})

	t.Run("sender source", func(t *testing.T) {
		files := []share.FileMeta{{Path: hostile, Size: 1}}
		source := newMemSource(nil)
		source.fail = boom
		s := newGoldSharer(t, source, files)
		_, err := s.Chunk(0)
		assertSafe(t, err)
	})
}

// Entries 克隆语义:改返回值不得污染接收端的几何依据。
func TestEntriesClone(t *testing.T) {
	files, src := goldTree()
	s := newGoldSharer(t, src, files)
	r, _ := newGoldReceiver(t, s)

	entries := r.Entries()
	if len(entries) != len(files) {
		t.Fatalf("Entries count = %d, want %d", len(entries), len(files))
	}
	// 清单序 = 规范序 = goldTree 的声明序(已按字节序排列)。
	for i, f := range files {
		if entries[i].Path != f.Path {
			t.Errorf("entries[%d] = %q, want %q", i, entries[i].Path, f.Path)
		}
	}
	entries[0].Path = "hijack"
	if r.Entries()[0].Path != "tree" {
		t.Error("mutating the Entries result changed the internal manifest")
	}
}

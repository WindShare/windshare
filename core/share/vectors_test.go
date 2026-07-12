package share_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/windshare/windshare/core/chunk"
	"github.com/windshare/windshare/core/internal/keyderiv"
	"github.com/windshare/windshare/core/internal/testvec"
	"github.com/windshare/windshare/core/layout"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/share"
)

// update 重生成并落盘 testvectors/(T1.7 兑现 T0.3):
//
//	cd core && go test ./share -update
//
// 生成完全由固定输入决定,重跑必然无 diff(TestVectorFilesUpToDate 顺带
// 守住这一点)。
var update = flag.Bool("update", false, "重新生成 testvectors/ 黄金向量文件")

// 向量文件位于 core 模块之外,按仓库 checkout 相对路径读写(§7)。
const (
	vectorsDir = "../../testvectors"

	// These values deliberately duplicate the normative cross-language contract.
	// Deriving golden expectations from implementation constants would let an
	// accidental protocol change bless itself merely by running -update.
	vectorMinChunkSize       int64  = 1 << 10
	vectorMaxChunkSize       int64  = 4 << 20
	vectorMaxChunkCount      uint64 = 1 << 26
	vectorMaxChunkStateBytes uint64 = vectorMaxChunkCount / 8
	vectorMaxStreamBytes     int64  = 1 << 48
	vectorSegmentBytes       int64  = 16 << 30
	vectorPathPolicyVersion         = "windshare/path/v1-unicode-15.0.0"
	vectorUnicodeVersion            = "15.0.0"
	vectorPlanIDDomain              = "windshare/v1 transfer-plan\x00"
	vectorManifestKeyLabel          = "windshare/v1 manifest"
	vectorStreamKeyLabel            = "windshare/v1 stream"
	vectorSegKeyLabel               = "windshare/v1 seg"
	vectorSuiteAESGCM        byte   = 0x01
	vectorNonceBytes                = 12
	vectorMaxSafeJSONInteger int64  = 1<<53 - 1
)

// ── 向量 schema(字段序即 JSON 键序;二进制一律 base64 标准字母表含填充,
// u64 一律十进制字符串,见 testvectors/README.md)。

type keyderivCase struct {
	Name           string       `json:"name"`
	ReadSecretB64  string       `json:"readSecretB64"`
	ManifestKeyB64 string       `json:"manifestKeyB64"`
	StreamKeyB64   string       `json:"streamKeyB64"`
	SegKeys        []segKeyCase `json:"segKeys"`
}

type segKeyCase struct {
	Seg    uint32 `json:"seg"`
	KeyB64 string `json:"keyB64"`
}

type linkCase struct {
	Name          string   `json:"name"`
	Suite         byte     `json:"suite"`
	ReadSecretB64 string   `json:"readSecretB64"`
	ShareID       string   `json:"shareId"`
	Relays        []string `json:"relays"`
	Base          string   `json:"base"`
	URL           string   `json:"url"`
	BareURL       string   `json:"bareUrl"`
	KeyString     string   `json:"keyString"`
}

type manifestSealCase struct {
	Name             string      `json:"name"`
	ReadSecretB64    string      `json:"readSecretB64"`
	ManifestKeyB64   string      `json:"manifestKeyB64"`
	NonceB64         string      `json:"nonceB64"`
	Manifest         manifestDoc `json:"manifest"`
	CanonicalCBORB64 string      `json:"canonicalCborB64"`
	SealedB64        string      `json:"sealedManifestB64"`
}

type manifestDoc struct {
	V         uint64     `json:"v"`
	ChunkSize int64      `json:"chunkSize"`
	Entries   []entryDoc `json:"entries"`
}

type entryDoc struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	IsDir bool   `json:"isDir"`
}

type chunkSealCase struct {
	Name         string `json:"name"`
	StreamKeyB64 string `json:"streamKeyB64"`
	ChunkSize    int64  `json:"chunkSize"`
	Index        string `json:"index"`
	PlaintextB64 string `json:"plaintextB64"`
	BlockCTB64   string `json:"blockCTB64"`
}

type frameCase struct {
	Name     string      `json:"name"`
	Request  *requestDoc `json:"request,omitempty"`
	Block    *blockDoc   `json:"block,omitempty"`
	Error    *errorDoc   `json:"error,omitempty"`
	FrameB64 string      `json:"frameB64"`
}

type requestDoc struct {
	Indices []string `json:"indices"`
}

type blockDoc struct {
	Index      string `json:"index"`
	Seq        uint32 `json:"seq"`
	Last       bool   `json:"last"`
	PayloadB64 string `json:"payloadB64"`
}

type errorDoc struct {
	Code uint16 `json:"code"`
	Msg  string `json:"msg"`
}

type geometryCase struct {
	Name        string             `json:"name"`
	Constants   *geometryConstants `json:"constants,omitempty"`
	ChunkSize   string             `json:"chunkSize,omitempty"`
	StreamBytes string             `json:"streamBytes,omitempty"`
	Expected    string             `json:"expected,omitempty"`
	ChunkCount  string             `json:"chunkCount,omitempty"`
}

type geometryConstants struct {
	MinChunkSize       string `json:"minChunkSize"`
	MaxChunkSize       string `json:"maxChunkSize"`
	MaxChunkCount      string `json:"maxChunkCount"`
	MaxChunkStateBytes string `json:"maxChunkStateBytes"`
	MaxStreamBytes     string `json:"maxStreamBytes"`
	SegmentBytes       string `json:"segmentBytes"`
}

type pathPolicyCase struct {
	Name           string `json:"name"`
	PolicyVersion  string `json:"policyVersion,omitempty"`
	UnicodeVersion string `json:"unicodeVersion,omitempty"`
	Input          string `json:"input,omitempty"`
	Expected       string `json:"expected,omitempty"`
	Canonical      string `json:"canonical,omitempty"`
	CollisionKey   string `json:"collisionKey,omitempty"`
	CollisionGroup string `json:"collisionGroup,omitempty"`
}

type transferPlanCase struct {
	Name          string          `json:"name"`
	Selectors     []string        `json:"selectors"`
	SelectedPaths []string        `json:"selectedPaths"`
	SelectedBytes string          `json:"selectedBytes"`
	Chunks        []chunkRangeDoc `json:"chunks"`
	PlanID        string          `json:"planId"`
}

type chunkRangeDoc struct {
	First string `json:"first"`
	End   string `json:"end"`
}

// ── 生成:全部向量由固定输入(goldSecret/goldTree/计数 RNG)推出。

type vectorFile struct {
	kind  string
	desc  string
	cases []any
}

func buildVectorFiles(t *testing.T) []vectorFile {
	t.Helper()
	requireCryptoContract(t)
	manifestCases, chunkCases := shareCases(t)
	return []vectorFile{
		{"keyderiv",
			"HKDF-SHA256 密钥树 KAT(§6.3):manifestKey = HKDF(ikm=readSecret, salt=空, info='windshare/v1 manifest', L=32);streamKey 同法 info='windshare/v1 stream';segKey = HKDF(ikm=streamKey, salt=空, info='windshare/v1 seg'‖u32_be(seg), L=32)。label 为精确 ASCII 字面字节、无结尾 NUL。二进制字段一律 base64(标准字母表、含填充)。",
			keyderivCases(t)},
		{"link",
			"能力链接编解码(§6.3/§6.10):url = <base>/<shareId>[?r=<relay>…]#<keyString>;keyString = base64url 无填充(suiteByte‖readSecret(16B));bareUrl 为去 fragment 的裸链接(split-key 两段之一);shareId 取路径末段(9B 的 base64url 无填充);?r= 自始按多值列表解析。校验双向:结构 → url/bareUrl/keyString 逐字符一致;url(或 bareUrl+keyString)→ 结构还原一致。",
			linkCases(t)},
		{"manifest-seal",
			"清单确定性 CBOR + GCM 封装(§6.4,B4/B5/B6):canonicalCbor = RFC 8949 Core Deterministic 编码的 {v, chunkSize, entries[{path,size,mtime,isDir}]}(entries 数组顺序即流顺序;mtime 为 Unix 毫秒);sealedManifest = nonce(12B)‖AES-256-GCM(key=manifestKey, nonce, plaintext=canonicalCbor, aad=suiteByte 0x01)。解码步骤:取 sealedManifest 首 12B 为 nonce → manifestKey = HKDF(readSecret, 'windshare/v1 manifest') → GCM Open(aad=0x01)得 canonicalCbor → 严格 CBOR 解码。本用例出自固定分享:readSecret=000102…0f,注入计数 RNG(字节流 0x00,0x01,…),清单 nonce 消耗其前 12B。",
			manifestCases},
		{"chunk-seal",
			"块 AEAD(§6.3,B1–B3):blockCT = nonce(12B)‖AES-256-GCM(key=segKey(seg), nonce, plaintext=块明文, aad=suiteByte 0x01‖u64_be(index));seg = index ÷ (SegmentBytes(16GiB) ÷ chunkSize),segKey 派生见 keyderiv。解码步骤:取 blockCT 首 12B 为 nonce → streamKey = HKDF(readSecret, 'windshare/v1 stream') → segKey → GCM Open 验 tag(篡改/错位块号必失败)。share-block-* 与 manifest-seal 出自同一固定分享(块 nonce 继清单 nonce 之后依次消耗计数 RNG;块 1 跨文件、块 2 为跨文件末短块);cross-segment-seg1 为独立用例(index=2^24 → seg=1)。index 为十进制字符串(u64)。",
			chunkCases},
		{"frame-codec",
			"数据面帧定长小端布局(§6.7,与 core/session 金标一致):REQUEST = 0x01‖n:u32‖indices:u64×n;BLOCK = 0x02‖index:u64‖seq:u32‖flags:u8(bit0=last,余位必须为零)‖len:u32‖payload;ERROR = 0x03‖code:u16‖msglen:u16‖msg(UTF-8)。u64 值在本 JSON 中为十进制字符串(避 JS float64 精度损失)。不变量:整帧 ≤ MaxFrameSize(65536B),故 BLOCK payload ≤ 65518B——block-max-frame 用例恰为整帧上限。",
			frameCases(t)},
		{"geometry",
			"打包流资源几何的跨实现契约:所有大小与计数均为十进制字符串。chunkSize 必须是 [1 KiB,4 MiB] 内的 2 次幂;chunkCount = ceil(streamBytes/chunkSize),最多 2^26;一位/块的稠密状态最多 8 MiB;streamBytes 最多 2^48(256 TiB)。segmentBytes=16 GiB 是独立的加密子密钥轮换跨度,绝不放宽 4 MiB 块/缓冲上限。实现必须先验证资源边界,再分配块状态。expected 是 valid 或稳定的错误类别。",
			geometryCases(t)},
		{"path-policy",
			"版本化跨平台路径契约。Canonicalize 只做 NFC;CollisionKey = NFC(Unicode 15.0.0 locale-independent full case fold(path))。valid 用例记录规范路径及碰撞键,同一 collisionGroup 必须碰撞;invalid-path 用例必须在清单或文件系统操作前拒绝。根级 .wsresume 前缀按完整折叠保留,Cc/Cf、Win32 非法/设备名(含上标 COM/LPT 与 CONIN$/CONOUT$)均拒绝。",
			pathPolicyCases(t)},
		{"transfer-plan",
			"选择只编译一次的传输契约。selectors=null 表示完整选择,selectors=[] 表示空选择;目录选择子选择子树,精确文件只选择该文件。chunks 是规范化半开区间 [first,end),数值为十进制字符串。planId = hex(SHA-256('windshare/v1 transfer-plan\\x00' ‖ 对按 UTF-8 字节排序的每条已选规范路径追加 u64_be(路径字节数) ‖ 路径字节))。utf8-byte-order-not-utf16 用例的 BMP 私用字符与 supplementary 字符顺序专门防止浏览器误用 JavaScript 默认 UTF-16 sort。",
			transferPlanCases(t)},
	}
}

func geometryCases(t *testing.T) []any {
	t.Helper()
	requireGeometryContract(t)
	decimalInt := func(value int64) string { return strconv.FormatInt(value, 10) }
	decimalUint := func(value uint64) string { return strconv.FormatUint(value, 10) }
	check := func(name string, chunkSize, streamBytes int64, expected, chunkCount string) any {
		return geometryCase{
			Name: name, ChunkSize: decimalInt(chunkSize), StreamBytes: decimalInt(streamBytes),
			Expected: expected, ChunkCount: chunkCount,
		}
	}
	return []any{
		geometryCase{Name: "protocol-constants", Constants: &geometryConstants{
			MinChunkSize:       decimalInt(vectorMinChunkSize),
			MaxChunkSize:       decimalInt(vectorMaxChunkSize),
			MaxChunkCount:      decimalUint(vectorMaxChunkCount),
			MaxChunkStateBytes: decimalUint(vectorMaxChunkStateBytes),
			MaxStreamBytes:     decimalInt(vectorMaxStreamBytes),
			SegmentBytes:       decimalInt(vectorSegmentBytes),
		}},
		check("empty-stream", vectorMinChunkSize, 0, "valid", "0"),
		check("one-byte-stream", vectorMinChunkSize, 1, "valid", "1"),
		check("exact-minimum-chunk", vectorMinChunkSize, vectorMinChunkSize, "valid", "1"),
		check("minimum-chunk-plus-one", vectorMinChunkSize, vectorMinChunkSize+1, "valid", "2"),
		check("exact-maximum-chunk-count", vectorMinChunkSize, int64(vectorMaxChunkCount)*vectorMinChunkSize, "valid", decimalUint(vectorMaxChunkCount)),
		check("maximum-stream", vectorMaxChunkSize, vectorMaxStreamBytes, "valid", decimalUint(vectorMaxChunkCount)),
		check("chunk-size-below-minimum", vectorMinChunkSize/2, 0, "chunk-size-too-small", ""),
		check("chunk-size-above-maximum", vectorMaxChunkSize*2, 0, "chunk-size-too-large", ""),
		check("chunk-size-not-power-of-two", vectorMinChunkSize+vectorMinChunkSize/2, 0, "chunk-size-not-power-of-two", ""),
		check("negative-stream", vectorMinChunkSize, -1, "negative-stream", ""),
		check("one-over-maximum-chunk-count", vectorMinChunkSize, int64(vectorMaxChunkCount)*vectorMinChunkSize+1, "too-many-chunks", ""),
		check("one-over-maximum-stream", vectorMaxChunkSize, vectorMaxStreamBytes+1, "stream-too-large", ""),
	}
}

func pathPolicyCases(t *testing.T) []any {
	t.Helper()
	policy := manifest.CurrentPathPolicy()
	requirePathPolicyContract(t, policy)
	cases := []any{pathPolicyCase{
		Name: "policy-version", PolicyVersion: vectorPathPolicyVersion, UnicodeVersion: vectorUnicodeVersion,
	}}
	valid := func(name, input, group string) {
		canonical, err := policy.Canonicalize(input)
		if err != nil {
			t.Fatalf("Canonicalize(%q): %v", input, err)
		}
		key, err := policy.CollisionKey(canonical)
		if err != nil {
			t.Fatalf("CollisionKey(%q): %v", canonical, err)
		}
		cases = append(cases, pathPolicyCase{
			Name: name, Input: input, Expected: "valid", Canonical: canonical,
			CollisionKey: key, CollisionGroup: group,
		})
	}
	invalid := func(name, input string) {
		if _, err := policy.Canonicalize(input); !errors.Is(err, manifest.ErrInvalidPath) {
			t.Fatalf("Canonicalize(%q) error = %v, want ErrInvalidPath", input, err)
		}
		cases = append(cases, pathPolicyCase{Name: name, Input: input, Expected: "invalid-path"})
	}

	valid("ascii", "tree/readme.txt", "")
	valid("sharp-s-uppercase", "ẞ.txt", "sharp-s")
	valid("sharp-s-expanded", "ss.txt", "sharp-s")
	valid("turkish-ascii-uppercase-i", "I.txt", "turkish-ascii-i")
	valid("turkish-ascii-lowercase-i", "i.txt", "turkish-ascii-i")
	valid("turkish-dotted-uppercase-i", "İ.txt", "turkish-dotted-i")
	valid("turkish-dotted-expanded", "i\u0307.txt", "turkish-dotted-i")
	valid("turkish-dotless-i", "ı.txt", "")
	valid("nfc", "café.txt", "nfc-nfd")
	valid("nfd-canonicalized", "cafe\u0301.txt", "nfc-nfd")
	// These characters gained canonical compositions after Unicode 15. Ambient
	// browser ICU must not silently advance this versioned path identity.
	valid("post-policy-composition-sequence", "\U000105D2\u0307.txt", "")
	valid("post-policy-composite", "\U000105C9.txt", "")
	valid("nested-journal-name", "tree/.wsresume-part", "")
	invalid("folded-journal-prefix", ".WSRESUME-part")
	invalid("full-fold-journal-prefix", ".wſresume-part")
	invalid("bidi-format-character", "safe/evil\u202Etxt.exe")
	invalid("zero-width-joiner", "safe/a\u200Db")
	invalid("control-character", "safe/line\nbreak")
	invalid("superscript-com-device", "COM¹.txt")
	invalid("superscript-lpt-device", "lpt²")
	invalid("console-input-device", "CONIN$")
	invalid("console-output-device", "conout$.log")
	return cases
}

func transferPlanCases(t *testing.T) []any {
	t.Helper()
	files, src := goldTree()
	receiver, _ := newGoldReceiver(t, newGoldSharer(t, src, files))
	inputs := []struct {
		name      string
		selectors []string
		receiver  *share.Receiver
	}{
		{name: "full-selection", selectors: nil, receiver: receiver},
		{name: "explicit-directory-subtree", selectors: []string{"tree"}, receiver: receiver},
		{name: "exact-file", selectors: []string{"tree/b.bin"}, receiver: receiver},
		{name: "selector-order-and-duplicates", selectors: []string{"tree/b.bin", "tree", "tree/b.bin"}, receiver: receiver},
		{name: "empty-selection", selectors: []string{}, receiver: receiver},
		{name: "utf8-byte-order-not-utf16", selectors: nil, receiver: newUTF8OrderingReceiver(t)},
	}
	cases := make([]any, 0, len(inputs))
	for _, input := range inputs {
		plan := mustPlan(t, input.receiver, input.selectors)
		selected := plan.SelectedEntries()
		paths := make([]string, 0, len(selected))
		for _, entry := range selected {
			paths = append(paths, entry.Path)
		}
		ranges := plan.Chunks().Ranges()
		chunks := make([]chunkRangeDoc, 0, len(ranges))
		for _, chunkRange := range ranges {
			chunks = append(chunks, chunkRangeDoc{
				First: strconv.FormatUint(chunkRange.First, 10),
				End:   strconv.FormatUint(chunkRange.End, 10),
			})
		}
		planID := normativePlanID(paths)
		if got := plan.PlanID().String(); got != planID {
			t.Fatalf("PlanID implementation = %s, normative preimage = %s", got, planID)
		}
		cases = append(cases, transferPlanCase{
			Name: input.name, Selectors: input.selectors, SelectedPaths: paths,
			SelectedBytes: strconv.FormatInt(plan.SelectedBytes(), 10),
			Chunks:        chunks, PlanID: planID,
		})
	}
	return cases
}

func newUTF8OrderingReceiver(t *testing.T) *share.Receiver {
	t.Helper()
	// UTF-8 preserves code-point order, while JavaScript's default UTF-16 sort
	// places the supplementary character's high surrogate before U+E000. Keeping
	// the manifest in that opposite order makes an omitted byte-sort observable.
	entries := []manifest.Entry{
		{Path: "𐀀-supplementary.txt", MTime: 1},
		{Path: "\uE000-bmp-private.txt", MTime: 2},
	}
	doc := manifest.New(vectorMinChunkSize, entries)
	key := vectorHKDF(t, goldSecret, vectorManifestKeyLabel)
	sealed, err := manifest.Seal(key, doc, bytes.NewReader(bytes.Repeat([]byte{0x42}, vectorNonceBytes)))
	if err != nil {
		t.Fatalf("seal UTF-8 ordering manifest: %v", err)
	}
	receiver, err := share.NewReceiver(link.Link{
		Suite: vectorSuiteAESGCM, ReadSecret: slices.Clone(goldSecret), ShareID: goldShareID,
	}, sealed, newMemSink())
	if err != nil {
		t.Fatalf("open UTF-8 ordering manifest: %v", err)
	}
	return receiver
}

func keyderivCases(t *testing.T) []any {
	t.Helper()
	secretB := bytes.Repeat([]byte{0xff}, link.ReadSecretBytes)
	build := func(name string, secret []byte, segs []uint32) keyderivCase {
		stream := vectorHKDF(t, secret, vectorStreamKeyLabel)
		c := keyderivCase{
			Name:           name,
			ReadSecretB64:  b64(secret),
			ManifestKeyB64: b64(vectorHKDF(t, secret, vectorManifestKeyLabel)),
			StreamKeyB64:   b64(stream),
		}
		for _, seg := range segs {
			var suffix [4]byte
			binary.BigEndian.PutUint32(suffix[:], seg)
			c.SegKeys = append(c.SegKeys, segKeyCase{
				Seg: seg, KeyB64: b64(vectorHKDF(t, stream, vectorSegKeyLabel+string(suffix[:]))),
			})
		}
		return c
	}
	return []any{
		// seg 256 与 seg 1 联手钉死 u32_be 字节序;max 钉边界。
		build("secret-00-0f", goldSecret, []uint32{0, 1, 256, 0xffffffff}),
		build("secret-ff", secretB, []uint32{0}),
	}
}

func linkCases(t *testing.T) []any {
	t.Helper()
	build := func(name string, secret []byte, shareID, base string, relays []string) linkCase {
		l := link.Link{Suite: link.SuiteAESGCM, ReadSecret: secret, ShareID: shareID, Relays: relays}
		url, err := l.URL(base)
		if err != nil {
			t.Fatalf("link.URL: %v", err)
		}
		bare, key, err := l.SplitURL(base)
		if err != nil {
			t.Fatalf("link.SplitURL: %v", err)
		}
		return linkCase{
			Name: name, Suite: l.Suite, ReadSecretB64: b64(secret), ShareID: shareID,
			Relays: relays, Base: base, URL: url, BareURL: bare, KeyString: key,
		}
	}
	altShareID := base64.RawURLEncoding.EncodeToString([]byte{0xf0, 0xe1, 0xd2, 0xc3, 0xb4, 0xa5, 0x96, 0x87, 0x78})
	return []any{
		build("relay-pair", goldSecret, goldShareID, "https://windshare.example",
			[]string{"relay-a.example", "relay-b.example"}),
		// 前端可部署在子路径下(shareId 恒为末段);无 ?r= 即同源/官方中转。
		build("sub-path-no-relay", bytes.Repeat([]byte{0xff}, link.ReadSecretBytes),
			altShareID, "https://ws.example/app", nil),
	}
}

// shareCases 从金标分享(goldTree + 固定身份 + 计数 RNG)导出清单与块向量:
// 生成本身走 share 门面,向量因此顺带钉死门面的接线与 RNG 消耗次序(B11)。
func shareCases(t *testing.T) (manifestCases, chunkCases []any) {
	t.Helper()
	files, src := goldTree()
	s := newGoldSharer(t, src, files)
	sealed, err := s.SealedManifest()
	if err != nil {
		t.Fatalf("SealedManifest: %v", err)
	}
	manifestKey := keyderiv.ManifestKey(goldSecret)
	nonce := sealed[:vectorNonceBytes]
	cbor := gcmOpen(t, manifestKey, nonce, sealed[vectorNonceBytes:], []byte{vectorSuiteAESGCM})

	doc := manifestDoc{V: manifest.CurrentVersion, ChunkSize: goldChunkSize}
	for _, f := range files {
		doc.Entries = append(doc.Entries, entryDoc{Path: f.Path, Size: f.Size, MTime: f.MTime, IsDir: f.IsDir})
	}
	manifestCases = []any{manifestSealCase{
		Name:             "share-tree",
		ReadSecretB64:    b64(goldSecret),
		ManifestKeyB64:   b64(manifestKey),
		NonceB64:         b64(nonce),
		Manifest:         doc,
		CanonicalCBORB64: b64(cbor),
		SealedB64:        b64(sealed),
	}}

	// 打包流 = 占流文件按清单序拼接;块 i 明文 = stream[i·cs : min((i+1)·cs, len)]。
	stream := slices.Concat(src.files["tree/a.txt"], src.files["tree/b.bin"], src.files[goldNaivePath])
	streamKey := keyderiv.StreamKey(goldSecret)
	names := []string{"share-block-0-full", "share-block-1-cross-file", "share-block-2-short-tail"}
	for i, name := range names {
		lo := int64(i) * goldChunkSize
		hi := min(lo+goldChunkSize, int64(len(stream)))
		ct, err := s.Chunk(uint64(i))
		if err != nil {
			t.Fatalf("Chunk(%d): %v", i, err)
		}
		chunkCases = append(chunkCases, chunkSealCase{
			Name:         name,
			StreamKeyB64: b64(streamKey),
			ChunkSize:    goldChunkSize,
			Index:        strconv.Itoa(i),
			PlaintextB64: b64(stream[lo:hi]),
			BlockCTB64:   b64(ct),
		})
	}

	// 跨段块:index=2^24 在 chunkSize=1024 下恰是段 1 的首块,钉死 segKey 轮换。
	// 独立于金标分享(16 GiB 流无法入向量),nonce 固定注入。
	const crossSegIndex = uint64(1) << 24
	segNonce := []byte{0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xab}
	segPlain := goldContent(64, 9)
	codec, err := chunk.NewCodec(streamKey, goldChunkSize, bytes.NewReader(segNonce))
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	segCT, err := codec.Seal(crossSegIndex, segPlain)
	if err != nil {
		t.Fatalf("Seal across segment boundary: %v", err)
	}
	chunkCases = append(chunkCases, chunkSealCase{
		Name:         "cross-segment-seg1",
		StreamKeyB64: b64(streamKey),
		ChunkSize:    goldChunkSize,
		Index:        strconv.FormatUint(crossSegIndex, 10),
		PlaintextB64: b64(segPlain),
		BlockCTB64:   b64(segCT),
	})
	return manifestCases, chunkCases
}

func frameCases(t *testing.T) []any {
	t.Helper()
	request := func(name string, indices []uint64) frameCase {
		f, err := session.EncodeRequest(indices)
		if err != nil {
			t.Fatalf("EncodeRequest: %v", err)
		}
		doc := &requestDoc{}
		for _, i := range indices {
			doc.Indices = append(doc.Indices, strconv.FormatUint(i, 10))
		}
		return frameCase{Name: name, Request: doc, FrameB64: b64(f)}
	}
	block := func(name string, b session.Block) frameCase {
		f, err := session.EncodeBlock(b)
		if err != nil {
			t.Fatalf("EncodeBlock: %v", err)
		}
		return frameCase{Name: name, Block: &blockDoc{
			Index: strconv.FormatUint(b.Index, 10), Seq: b.Seq, Last: b.Last, PayloadB64: b64(b.Payload),
		}, FrameB64: b64(f)}
	}
	errf := func(name string, code uint16, msg string) frameCase {
		f, err := session.EncodeError(code, msg)
		if err != nil {
			t.Fatalf("EncodeError: %v", err)
		}
		return frameCase{Name: name, Error: &errorDoc{Code: code, Msg: msg}, FrameB64: b64(f)}
	}
	// 前五例与 core/session 的硬编码金标(frame_test.go)同输入,字节必然一致。
	maxPayload := make([]byte, session.MaxBlockPayload)
	for i := range maxPayload {
		maxPayload[i] = byte(i)
	}
	return []any{
		request("request-two-indices", []uint64{0x0807060504030201, 2}),
		block("block-last", session.Block{Index: 3, Seq: 1, Last: true, Payload: []byte{0xaa, 0xbb, 0xcc}}),
		block("block-not-last", session.Block{Index: 0x1122334455667788, Seq: 0, Payload: []byte{0x00}}),
		errf("error-block-read", session.ErrCodeBlockRead, "drift"),
		errf("error-empty-msg", session.ErrCodeBadRequest, ""),
		block("block-max-frame", session.Block{Index: 7, Seq: 0, Last: true, Payload: maxPayload}),
	}
}

// ── 落盘与新鲜度:文件字节 ⩵ 当前代码的生成结果(顺带保证 -update 幂等)。

func renderVectorFile(t *testing.T, vf vectorFile) []byte {
	t.Helper()
	env := struct {
		Version     int    `json:"version"`
		Kind        string `json:"kind"`
		Description string `json:"description"`
		Cases       []any  `json:"cases"`
	}{testvec.EnvelopeVersion, vf.kind, vf.desc, vf.cases}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		t.Fatalf("encode vector %s: %v", vf.kind, err)
	}
	return buf.Bytes()
}

func TestVectorFilesUpToDate(t *testing.T) {
	for _, vf := range buildVectorFiles(t) {
		path := filepath.Join(vectorsDir, vf.kind+".json")
		want := renderVectorFile(t, vf)
		if *update {
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (run go test ./share -update first): %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from current generation; if the implementation changed intentionally, run go test ./share -update and review the diff", path)
		}
	}
}

func TestVectorJSONNumbersAreJavaScriptSafe(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(vectorsDir, "*.json"))
	if err != nil {
		t.Fatalf("glob vector files: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no vector files found")
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var document any
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&document); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		assertSafeJSONNumbers(t, filepath.Base(path), document)
	}
}

func assertSafeJSONNumbers(t *testing.T, path string, value any) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			assertSafeJSONNumbers(t, path+"."+key, child)
		}
	case []any:
		for index, child := range value {
			assertSafeJSONNumbers(t, path+"["+strconv.Itoa(index)+"]", child)
		}
	case json.Number:
		integer, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			t.Fatalf("%s contains a non-integer or out-of-range JSON number %q; encode protocol integers as decimal strings", path, value)
		}
		if integer < -vectorMaxSafeJSONInteger || integer > vectorMaxSafeJSONInteger {
			t.Fatalf("%s contains JavaScript-unsafe JSON integer %s; encode it as a decimal string", path, value)
		}
	}
}

// ── 校验:经 testvec 信封读回,按各 kind 的解码步骤逐字节复算(TS 侧 T5.1
// 对着同一份文件做镜像校验)。

func loadCases[T any](t *testing.T, kind string) []T {
	t.Helper()
	f, err := testvec.Load(filepath.Join(vectorsDir, kind+".json"))
	if err != nil {
		t.Fatalf("load vector %s: %v", kind, err)
	}
	if f.Kind != kind {
		t.Fatalf("kind = %q, want %q", f.Kind, kind)
	}
	out := make([]T, len(f.Cases))
	for i := range f.Cases {
		if err := f.Cases[i].Decode(&out[i]); err != nil {
			t.Fatalf("decode case %s: %v", f.Cases[i].Name, err)
		}
	}
	return out
}

func TestVectorKeyderiv(t *testing.T) {
	for _, c := range loadCases[keyderivCase](t, "keyderiv") {
		t.Run(c.Name, func(t *testing.T) {
			secret := unb64(t, c.ReadSecretB64)
			requireB64Equal(t, "manifestKey", c.ManifestKeyB64, keyderiv.ManifestKey(secret))
			stream := keyderiv.StreamKey(secret)
			requireB64Equal(t, "streamKey", c.StreamKeyB64, stream)
			for _, sc := range c.SegKeys {
				requireB64Equal(t, "segKey", sc.KeyB64, keyderiv.SegKey(stream, sc.Seg))
			}
		})
	}
}

func TestVectorLink(t *testing.T) {
	for _, c := range loadCases[linkCase](t, "link") {
		t.Run(c.Name, func(t *testing.T) {
			l := link.Link{Suite: c.Suite, ReadSecret: unb64(t, c.ReadSecretB64), ShareID: c.ShareID, Relays: c.Relays}
			url, err := l.URL(c.Base)
			if err != nil || url != c.URL {
				t.Errorf("URL = %q, %v; want %q", url, err, c.URL)
			}
			bare, key, err := l.SplitURL(c.Base)
			if err != nil || bare != c.BareURL || key != c.KeyString {
				t.Errorf("SplitURL = %q %q, %v; want %q %q", bare, key, err, c.BareURL, c.KeyString)
			}
			parsed, err := link.Parse(c.URL)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			requireLinkEqual(t, parsed, l)
			merged, err := link.Merge(c.BareURL, c.KeyString)
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			requireLinkEqual(t, merged, l)
		})
	}
}

func TestVectorManifestSeal(t *testing.T) {
	requireCryptoContract(t)
	for _, c := range loadCases[manifestSealCase](t, "manifest-seal") {
		t.Run(c.Name, func(t *testing.T) {
			key := unb64(t, c.ManifestKeyB64)
			requireB64Equal(t, "manifestKey", c.ManifestKeyB64, keyderiv.ManifestKey(unb64(t, c.ReadSecretB64)))
			m := manifest.New(c.Manifest.ChunkSize, nil)
			m.Version = c.Manifest.V
			for _, e := range c.Manifest.Entries {
				m.Entries = append(m.Entries, manifest.Entry{Path: e.Path, Size: e.Size, MTime: e.MTime, IsDir: e.IsDir})
			}
			sealed, err := manifest.Seal(key, m, bytes.NewReader(unb64(t, c.NonceB64)))
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			requireB64Equal(t, "sealedManifest", c.SealedB64, sealed)
			// canonical CBOR 明文另拍:TS 侧可先单验 CBOR 编码,再验 GCM。
			cbor := gcmOpen(t, key, sealed[:vectorNonceBytes], sealed[vectorNonceBytes:], []byte{vectorSuiteAESGCM})
			requireB64Equal(t, "canonicalCbor", c.CanonicalCBORB64, cbor)
			opened, err := manifest.Open(key, unb64(t, c.SealedB64))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if opened.Version != c.Manifest.V || opened.ChunkSize != c.Manifest.ChunkSize || len(opened.Entries) != len(c.Manifest.Entries) {
				t.Fatalf("Open result does not match vector structure: %+v", opened)
			}
			for i, e := range c.Manifest.Entries {
				if opened.Entries[i] != (manifest.Entry{Path: e.Path, Size: e.Size, MTime: e.MTime, IsDir: e.IsDir}) {
					t.Errorf("entries[%d] = %+v, want %+v", i, opened.Entries[i], e)
				}
			}
		})
	}
}

func TestVectorChunkSeal(t *testing.T) {
	requireCryptoContract(t)
	for _, c := range loadCases[chunkSealCase](t, "chunk-seal") {
		t.Run(c.Name, func(t *testing.T) {
			index, err := strconv.ParseUint(c.Index, 10, 64)
			if err != nil {
				t.Fatalf("index: %v", err)
			}
			streamKey := unb64(t, c.StreamKeyB64)
			blockCT := unb64(t, c.BlockCTB64)
			plaintext := unb64(t, c.PlaintextB64)
			if len(blockCT) < vectorNonceBytes {
				t.Fatalf("blockCT length = %d, want at least %d", len(blockCT), vectorNonceBytes)
			}
			chunksPerSegment := uint64(vectorSegmentBytes / c.ChunkSize)
			segment := uint32(index / chunksPerSegment)
			var segmentSuffix [4]byte
			binary.BigEndian.PutUint32(segmentSuffix[:], segment)
			segmentKey := vectorHKDF(t, streamKey, vectorSegKeyLabel+string(segmentSuffix[:]))
			var aad [9]byte
			aad[0] = vectorSuiteAESGCM
			binary.BigEndian.PutUint64(aad[1:], index)
			independent := gcmOpen(t, segmentKey, blockCT[:vectorNonceBytes], blockCT[vectorNonceBytes:], aad[:])
			requireB64Equal(t, "independent plaintext", c.PlaintextB64, independent)
			// 重 Seal:以向量内嵌 nonce(blockCT 前 12B)作 rng,输出须逐字节复现。
			codec, err := chunk.NewCodec(streamKey, c.ChunkSize, bytes.NewReader(blockCT[:vectorNonceBytes]))
			if err != nil {
				t.Fatalf("NewCodec: %v", err)
			}
			sealed, err := codec.Seal(index, plaintext)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			requireB64Equal(t, "blockCT", c.BlockCTB64, sealed)
			opened, err := codec.Open(index, blockCT)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			requireB64Equal(t, "plaintext", c.PlaintextB64, opened)
		})
	}
}

func TestVectorFrameCodec(t *testing.T) {
	for _, c := range loadCases[frameCase](t, "frame-codec") {
		t.Run(c.Name, func(t *testing.T) {
			frame := session.Frame(unb64(t, c.FrameB64))
			if len(frame) > session.MaxFrameSize {
				t.Fatalf("frame length %d exceeds MaxFrameSize", len(frame))
			}
			msg, err := session.Decode(frame)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			switch {
			case c.Request != nil:
				var indices []uint64
				for _, s := range c.Request.Indices {
					v, err := strconv.ParseUint(s, 10, 64)
					if err != nil {
						t.Fatalf("index %q: %v", s, err)
					}
					indices = append(indices, v)
				}
				encoded, err := session.EncodeRequest(indices)
				if err != nil {
					t.Fatalf("EncodeRequest: %v", err)
				}
				requireB64Equal(t, "frame", c.FrameB64, encoded)
				req, ok := msg.(*session.Request)
				if !ok || !slices.Equal(req.Indices, indices) {
					t.Errorf("Decode = %#v", msg)
				}
			case c.Block != nil:
				index, err := strconv.ParseUint(c.Block.Index, 10, 64)
				if err != nil {
					t.Fatalf("index: %v", err)
				}
				want := session.Block{Index: index, Seq: c.Block.Seq, Last: c.Block.Last, Payload: unb64(t, c.Block.PayloadB64)}
				encoded, err := session.EncodeBlock(want)
				if err != nil {
					t.Fatalf("EncodeBlock: %v", err)
				}
				requireB64Equal(t, "frame", c.FrameB64, encoded)
				blk, ok := msg.(*session.Block)
				if !ok || blk.Index != want.Index || blk.Seq != want.Seq || blk.Last != want.Last || !bytes.Equal(blk.Payload, want.Payload) {
					t.Errorf("Decode = %#v", msg)
				}
			case c.Error != nil:
				encoded, err := session.EncodeError(c.Error.Code, c.Error.Msg)
				if err != nil {
					t.Fatalf("EncodeError: %v", err)
				}
				requireB64Equal(t, "frame", c.FrameB64, encoded)
				e, ok := msg.(*session.Error)
				if !ok || e.Code != c.Error.Code || e.Msg != c.Error.Msg {
					t.Errorf("Decode = %#v", msg)
				}
			default:
				t.Fatal("case is missing request, block, or error data")
			}
		})
	}
}

func TestVectorGeometry(t *testing.T) {
	requireGeometryContract(t)
	for _, c := range loadCases[geometryCase](t, "geometry") {
		t.Run(c.Name, func(t *testing.T) {
			if c.Constants != nil {
				want := geometryConstants{
					MinChunkSize:       strconv.FormatInt(vectorMinChunkSize, 10),
					MaxChunkSize:       strconv.FormatInt(vectorMaxChunkSize, 10),
					MaxChunkCount:      strconv.FormatUint(vectorMaxChunkCount, 10),
					MaxChunkStateBytes: strconv.FormatUint(vectorMaxChunkStateBytes, 10),
					MaxStreamBytes:     strconv.FormatInt(vectorMaxStreamBytes, 10),
					SegmentBytes:       strconv.FormatInt(vectorSegmentBytes, 10),
				}
				if *c.Constants != want {
					t.Fatalf("constants = %+v, want %+v", *c.Constants, want)
				}
				return
			}

			chunkSize, err := strconv.ParseInt(c.ChunkSize, 10, 64)
			if err != nil {
				t.Fatalf("parse chunkSize: %v", err)
			}
			streamBytes, err := strconv.ParseInt(c.StreamBytes, 10, 64)
			if err != nil {
				t.Fatalf("parse streamBytes: %v", err)
			}
			geometry, err := layout.ValidateGeometry(chunkSize, streamBytes)
			if c.Expected == "valid" {
				if err != nil {
					t.Fatalf("ValidateGeometry: %v", err)
				}
				if got := strconv.FormatUint(geometry.ChunkCount(), 10); got != c.ChunkCount {
					t.Fatalf("chunkCount = %s, want %s", got, c.ChunkCount)
				}
				return
			}
			wantErr := map[string]error{
				"chunk-size-not-power-of-two": layout.ErrChunkSizeNotPow2,
				"chunk-size-too-small":        layout.ErrChunkSizeTooSmall,
				"chunk-size-too-large":        layout.ErrChunkSizeTooLarge,
				"negative-stream":             layout.ErrNegativeStreamLen,
				"stream-too-large":            layout.ErrStreamTooLarge,
				"too-many-chunks":             layout.ErrTooManyChunks,
			}[c.Expected]
			if wantErr == nil {
				t.Fatalf("unknown expected result %q", c.Expected)
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("ValidateGeometry error = %v, want %v", err, wantErr)
			}
		})
	}
}

func TestVectorPathPolicy(t *testing.T) {
	policy := manifest.CurrentPathPolicy()
	requirePathPolicyContract(t, policy)
	groupKeys := make(map[string]string)
	groupCounts := make(map[string]int)
	for _, c := range loadCases[pathPolicyCase](t, "path-policy") {
		t.Run(c.Name, func(t *testing.T) {
			if c.PolicyVersion != "" {
				if c.PolicyVersion != policy.Version() || c.UnicodeVersion != policy.UnicodeVersion() {
					t.Fatalf("policy metadata = %q/%q, want %q/%q", c.PolicyVersion, c.UnicodeVersion, policy.Version(), policy.UnicodeVersion())
				}
				return
			}
			canonical, err := policy.Canonicalize(c.Input)
			switch c.Expected {
			case "valid":
				if err != nil {
					t.Fatalf("Canonicalize: %v", err)
				}
				if canonical != c.Canonical {
					t.Fatalf("canonical = %q, want %q", canonical, c.Canonical)
				}
				key, err := policy.CollisionKey(canonical)
				if err != nil {
					t.Fatalf("CollisionKey: %v", err)
				}
				if key != c.CollisionKey {
					t.Fatalf("collisionKey = %q, want %q", key, c.CollisionKey)
				}
				if c.CollisionGroup != "" {
					if first, exists := groupKeys[c.CollisionGroup]; exists && first != key {
						t.Fatalf("collision group %q has keys %q and %q", c.CollisionGroup, first, key)
					}
					groupKeys[c.CollisionGroup] = key
					groupCounts[c.CollisionGroup]++
				}
			case "invalid-path":
				if !errors.Is(err, manifest.ErrInvalidPath) {
					t.Fatalf("Canonicalize error = %v, want ErrInvalidPath", err)
				}
			default:
				t.Fatalf("unknown expected result %q", c.Expected)
			}
		})
	}
	for group, count := range groupCounts {
		if count < 2 {
			t.Errorf("collision group %q has only %d case", group, count)
		}
	}
}

func TestVectorTransferPlan(t *testing.T) {
	files, src := goldTree()
	goldReceiver, _ := newGoldReceiver(t, newGoldSharer(t, src, files))
	utf8Receiver := newUTF8OrderingReceiver(t)
	for _, c := range loadCases[transferPlanCase](t, "transfer-plan") {
		t.Run(c.Name, func(t *testing.T) {
			receiver := goldReceiver
			if c.Name == "utf8-byte-order-not-utf16" {
				receiver = utf8Receiver
			}
			plan := mustPlan(t, receiver, c.Selectors)
			selected := plan.SelectedEntries()
			paths := make([]string, 0, len(selected))
			for _, entry := range selected {
				paths = append(paths, entry.Path)
			}
			if !slices.Equal(paths, c.SelectedPaths) {
				t.Fatalf("selected paths = %v, want %v", paths, c.SelectedPaths)
			}
			if got := strconv.FormatInt(plan.SelectedBytes(), 10); got != c.SelectedBytes {
				t.Fatalf("selected bytes = %s, want %s", got, c.SelectedBytes)
			}
			ranges := plan.Chunks().Ranges()
			if len(ranges) != len(c.Chunks) {
				t.Fatalf("chunk ranges = %v, want %v", ranges, c.Chunks)
			}
			for i, got := range ranges {
				if strconv.FormatUint(got.First, 10) != c.Chunks[i].First || strconv.FormatUint(got.End, 10) != c.Chunks[i].End {
					t.Errorf("chunk range %d = [%d,%d), want [%s,%s)", i, got.First, got.End, c.Chunks[i].First, c.Chunks[i].End)
				}
			}
			if want := normativePlanID(c.SelectedPaths); want != c.PlanID {
				t.Fatalf("vector planId = %s, normative preimage = %s", c.PlanID, want)
			}
			if got := plan.PlanID().String(); got != c.PlanID {
				t.Fatalf("planId = %s, want %s", got, c.PlanID)
			}
		})
	}
}

// ── 工具。

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func vectorHKDF(t *testing.T, secret []byte, info string) []byte {
	t.Helper()
	key, err := hkdf.Key(sha256.New, secret, nil, info, sha256.Size)
	if err != nil {
		t.Fatalf("derive normative HKDF vector: %v", err)
	}
	return key
}

func requireGeometryContract(t *testing.T) {
	t.Helper()
	if layout.MinChunkSize != vectorMinChunkSize || layout.MaxChunkSize != vectorMaxChunkSize ||
		layout.MaxChunkCount != vectorMaxChunkCount || layout.MaxChunkStateBytes != vectorMaxChunkStateBytes ||
		layout.MaxStreamBytes != vectorMaxStreamBytes || chunk.SegmentBytes != vectorSegmentBytes {
		t.Fatalf("geometry implementation drifted from the normative vector contract")
	}
}

func requireCryptoContract(t *testing.T) {
	t.Helper()
	if link.SuiteAESGCM != vectorSuiteAESGCM || chunk.NonceBytes != vectorNonceBytes ||
		chunk.SegmentBytes != vectorSegmentBytes {
		t.Fatalf("crypto implementation drifted from the normative vector contract")
	}
}

func requirePathPolicyContract(t *testing.T, policy manifest.PathPolicy) {
	t.Helper()
	if policy.Version() != vectorPathPolicyVersion || policy.UnicodeVersion() != vectorUnicodeVersion {
		t.Fatalf("path policy implementation = %q/%q, normative contract = %q/%q",
			policy.Version(), policy.UnicodeVersion(), vectorPathPolicyVersion, vectorUnicodeVersion)
	}
}

func normativePlanID(paths []string) string {
	ordered := slices.Clone(paths)
	slices.Sort(ordered)
	hash := sha256.New()
	_, _ = hash.Write([]byte(vectorPlanIDDomain))
	var size [8]byte
	for _, path := range ordered {
		binary.BigEndian.PutUint64(size[:], uint64(len(path)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(path))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode base64 %q: %v", s, err)
	}
	return b
}

func requireB64Equal(t *testing.T, what, wantB64 string, got []byte) {
	t.Helper()
	if b64(got) != wantB64 {
		t.Errorf("%s mismatch:\n got  %s\n want %s", what, b64(got), wantB64)
	}
}

func requireLinkEqual(t *testing.T, got, want link.Link) {
	t.Helper()
	if got.Suite != want.Suite || !bytes.Equal(got.ReadSecret, want.ReadSecret) ||
		got.ShareID != want.ShareID || !slices.Equal(got.Relays, want.Relays) {
		t.Errorf("Link = %+v, want %+v", got, want)
	}
}

// gcmOpen 用 stdlib 独立解封(不经 manifest 包),交叉验证向量里的
// canonical CBOR 明文字节。
func gcmOpen(t *testing.T, key, nonce, ct, aad []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	plain, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		t.Fatalf("gcm open: %v", err)
	}
	return plain
}

package manifest

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/fxamacker/cbor/v2"

	"github.com/windshare/windshare/core/layout"
	"github.com/windshare/windshare/core/link"
)

// CurrentVersion 是本实现唯一认识的清单 schema 版本(CBOR 字段 v)。
// 版本演进不加兼容分支:解码先宽容探测 v,未知版本明确报"请升级"(§6.4/B15)。
const CurrentVersion = 1

// MaxManifestSize 限制 sealedManifest 全长(§8):发送端 Seal 前预检,在出链接
// 前给出明确报错而非等中转拒收;中转限额经 root import 复用同一常量。
const MaxManifestSize = 16 << 20 // 16 MiB

const (
	// JavaScript clients must reproduce authenticated metadata exactly. Keeping
	// mtime inside Number's integer range prevents one valid Go manifest from
	// acquiring a rounded timestamp in the browser implementation.
	MaxMTimeMilliseconds int64 = 1<<53 - 1
	MinMTimeMilliseconds int64 = -MaxMTimeMilliseconds
)

const (
	// FingerprintBytes is the authenticated GCM tag that identifies the complete
	// sealed manifest for resume binding. It is a manifest concern, not a chunk
	// envelope constant, even though both current suites use 128-bit GCM tags.
	FingerprintBytes = 16
	sealedNonceBytes = 12
)

// manifestKeyBytes:manifestKey 必须是 AES-256 整键长。长度不符几乎必然意味着
// 调用方绕过了 keyderiv 派生,报错优于让 aes.NewCipher 静默降档到 AES-128/192。
const manifestKeyBytes = 32

// maxDecodeArrayElements 界住恶意 CBOR 的数组规模,避免解码器在结构校验前就被
// 撑爆内存:合法 entries 数受 MaxManifestSize/最小条目编码长(≈26 B)≈ 6.5×10⁵
// 上界,取 2²⁰ 留裕量。
const maxDecodeArrayElements = 1 << 20

// 错误按类别提供 errors.Is 锚点;具体成因经 fmt.Errorf("%w: …") 附于消息。
var (
	// ErrUnsupportedVersion:清单 v 不被本端认识——通常是发送端更新、本端落后(B15)。
	ErrUnsupportedVersion = errors.New("manifest: unsupported manifest version; upgrade required")
	ErrManifestTooLarge   = errors.New("manifest: manifest exceeds MaxManifestSize")
	ErrNonCanonical       = errors.New("manifest: manifest encoding is not canonical")
	ErrUnsupportedSuite   = errors.New("manifest: unsupported cipher suite")
	ErrInvalidPath        = errors.New("manifest: invalid path")
	ErrDuplicatePath      = errors.New("manifest: duplicate path")
	ErrPathCollision      = errors.New("manifest: folded path collision")
	ErrPathTypeConflict   = errors.New("manifest: a file entry cannot contain descendants")
	ErrNegativeSize       = errors.New("manifest: entry size is negative")
	ErrMTimeOutOfRange    = errors.New("manifest: entry mtime is outside the interoperable integer range")
	ErrStreamTooLarge     = errors.New("manifest: stream length exceeds MaxStreamBytes")
	ErrTooManyChunks      = errors.New("manifest: chunk count exceeds MaxChunkCount")
	ErrInvalidChunkSize   = errors.New("manifest: invalid chunkSize")
	ErrSealedTooShort     = errors.New("manifest: sealed manifest is too short")
)

// Entry 即 §6.4 钉死的条目 schema:MTime 为 Unix epoch 毫秒;无 offset/streamLen
// ——几何由双端按数组顺序对 size 前缀和各自推导,派生量无从伪造(B14)。
type Entry struct {
	Path  string `cbor:"path"`
	Size  int64  `cbor:"size"`
	MTime int64  `cbor:"mtime"`
	IsDir bool   `cbor:"isDir"`
}

// Manifest 是目录模型。Entries 数组顺序即打包流顺序,由清单 GCM tag 认证;
// 目录与 size=0 的文件不占流。
type Manifest struct {
	Version   uint64  `cbor:"v"`
	ChunkSize int64   `cbor:"chunkSize"`
	Entries   []Entry `cbor:"entries"`
}

// Fingerprint is the complete sealed-manifest identity persisted by resume
// state. An array value prevents callers from retaining mutable sealed bytes.
type Fingerprint [FingerprintBytes]byte

// SealedFingerprint extracts the authenticated identity only from a structurally
// valid current-suite envelope. Cryptographic validation remains Open's job; this
// boundary exists so callers cannot invent tag offsets or minimum lengths.
func SealedFingerprint(sealed []byte) (Fingerprint, error) {
	var fingerprint Fingerprint
	if len(sealed) > MaxManifestSize {
		return fingerprint, fmt.Errorf("%w: %d bytes", ErrManifestTooLarge, len(sealed))
	}
	if len(sealed) < sealedNonceBytes+FingerprintBytes {
		return fingerprint, fmt.Errorf("%w: got %d bytes, need at least %d", ErrSealedTooShort, len(sealed), sealedNonceBytes+FingerprintBytes)
	}
	copy(fingerprint[:], sealed[len(sealed)-FingerprintBytes:])
	return fingerprint, nil
}

// New 以当前 schema 版本构造清单,免去调用方手填 Version 后被 Validate 拒绝。
func New(chunkSize int64, entries []Entry) *Manifest {
	return &Manifest{Version: CurrentVersion, ChunkSize: chunkSize, Entries: entries}
}

// encMode 取 RFC 8949 Core Deterministic:同一清单在 Go/TS 两端逐字节一致,
// sealedManifest 才能纳入黄金向量逐字节对拍(B4/B6)。NilContainerAsEmpty 把
// nil Entries 钉成空数组,使 canonical 形态唯一(null 形态会被解码侧的重编码
// 比对拒绝)。
var encMode = func() cbor.EncMode {
	opts := cbor.CoreDetEncOptions()
	opts.NilContainers = cbor.NilContainerAsEmpty
	em, err := opts.EncMode()
	if err != nil {
		panic(err)
	}
	return em
}()

// probeDecMode 故意宽容:未来版本的清单可能带未知字段乃至本端不识别的编码形态,
// 探测 v 时不能先被它们绊倒,否则旧接收端遇新清单只会抛不可读的 CBOR 结构错误,
// 而非"请升级"(B15)。探测结果只用于读 v,不进入业务逻辑。
var probeDecMode = func() cbor.DecMode {
	dm, err := cbor.DecOptions{
		FieldNameMatching: cbor.FieldNameMatchingCaseSensitive,
		MaxArrayElements:  maxDecodeArrayElements,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return dm
}()

// strictDecMode 拒绝重复键、不定长编码、CBOR tag 与未知字段。fxamacker 没有
// "拒非最短整数编码/拒未排序键"的解码选项,这两类偏差由解码后的确定性重编码
// 比对兜底(见 decodeManifest)。
var strictDecMode = func() cbor.DecMode {
	dm, err := cbor.DecOptions{
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		IndefLength:       cbor.IndefLengthForbidden,
		TagsMd:            cbor.TagsForbidden,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
		FieldNameMatching: cbor.FieldNameMatchingCaseSensitive,
		MaxArrayElements:  maxDecodeArrayElements,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return dm
}()

// Validate 是 B7 全清单结构校验,构建期(Seal 内)与解封后(Receiver 下载前)
// 共用同一双眼睛(§6.13 纵深):本端构造缺陷与恶意清单走同一条拒绝路径。
func (m *Manifest) Validate() error {
	if m.Version != CurrentVersion {
		return fmt.Errorf("%w: got v=%d, supported v=%d", ErrUnsupportedVersion, m.Version, CurrentVersion)
	}

	// Path policy remains manifest-owned, while every byte/chunk geometry decision is
	// delegated to layout so Go and TypeScript have one reproducible resource contract.
	seen := make(map[string]string, len(m.Entries))
	geometryEntries := make([]layout.Entry, len(m.Entries))
	pathRecords := make([]manifestPathRecord, 0, len(m.Entries))
	for i, entry := range m.Entries {
		if err := ValidatePath(entry.Path); err != nil {
			return err
		}
		if entry.MTime < MinMTimeMilliseconds || entry.MTime > MaxMTimeMilliseconds {
			return fmt.Errorf("%w: entry %d has mtime=%d, range is [%d,%d]", ErrMTimeOutOfRange, i, entry.MTime, MinMTimeMilliseconds, MaxMTimeMilliseconds)
		}
		key := foldPath(entry.Path)
		if first, ok := seen[key]; ok {
			if first == entry.Path {
				return fmt.Errorf("%w: %s", ErrDuplicatePath, QuotePathForDiagnostic(entry.Path))
			}
			return fmt.Errorf("%w: %s conflicts with %s", ErrPathCollision, QuotePathForDiagnostic(entry.Path), QuotePathForDiagnostic(first))
		}
		seen[key] = entry.Path
		pathRecords = append(pathRecords, manifestPathRecord{entry: entry, collisionKey: key})
		geometryEntries[i] = layout.Entry{Path: entry.Path, Size: entry.Size, IsDir: entry.IsDir}
	}
	if err := validatePathHierarchy(pathRecords); err != nil {
		return err
	}
	_, err := layout.DeriveGeometry(geometryEntries, m.ChunkSize)
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, layout.ErrChunkSizeNotPow2),
		errors.Is(err, layout.ErrChunkSizeTooSmall),
		errors.Is(err, layout.ErrChunkSizeTooLarge):
		return fmt.Errorf("%w: %w", ErrInvalidChunkSize, err)
	case errors.Is(err, layout.ErrNegativeSize):
		return fmt.Errorf("%w: %w", ErrNegativeSize, layout.ErrNegativeSize)
	case errors.Is(err, layout.ErrStreamTooLarge):
		return fmt.Errorf("%w: %w", ErrStreamTooLarge, layout.ErrStreamTooLarge)
	case errors.Is(err, layout.ErrTooManyChunks):
		return fmt.Errorf("%w: %w", ErrTooManyChunks, err)
	case errors.Is(err, layout.ErrDuplicatePath):
		return fmt.Errorf("%w: %w", ErrDuplicatePath, layout.ErrDuplicatePath)
	default:
		return err
	}
}

type manifestPathRecord struct {
	entry        Entry
	collisionKey string
}

// validatePathHierarchy rejects trees that cannot have one cross-platform materialized
// meaning. Full-path collision checks do not catch differently cased implicit parents,
// and a file used as an ancestor can never coexist with its descendants.
func validatePathHierarchy(records []manifestPathRecord) error {
	slices.SortFunc(records, func(a, b manifestPathRecord) int {
		return strings.Compare(a.collisionKey, b.collisionKey)
	})
	for i, current := range records {
		if i > 0 {
			previous := records[i-1]
			if previousPrefix, currentPrefix, mismatch := foldedPrefixSpellingMismatch(previous.entry.Path, current.entry.Path); mismatch {
				return fmt.Errorf("%w: path prefixes %s and %s have the same cross-platform identity", ErrPathCollision, QuotePathForDiagnostic(previousPrefix), QuotePathForDiagnostic(currentPrefix))
			}
		}
		descendantPrefix := current.collisionKey + "/"
		firstDescendant, _ := slices.BinarySearchFunc(records, descendantPrefix, func(record manifestPathRecord, prefix string) int {
			return strings.Compare(record.collisionKey, prefix)
		})
		if firstDescendant == len(records) || !strings.HasPrefix(records[firstDescendant].collisionKey, descendantPrefix) {
			continue
		}
		descendant := records[firstDescendant]
		if currentSpelling, descendantSpelling, mismatch := foldedPrefixSpellingMismatch(current.entry.Path, descendant.entry.Path); mismatch {
			return fmt.Errorf("%w: path prefixes %s and %s have the same cross-platform identity", ErrPathCollision, QuotePathForDiagnostic(currentSpelling), QuotePathForDiagnostic(descendantSpelling))
		}
		if !current.entry.IsDir {
			return fmt.Errorf("%w: file %s is an ancestor of %s", ErrPathTypeConflict, QuotePathForDiagnostic(current.entry.Path), QuotePathForDiagnostic(descendant.entry.Path))
		}
	}
	return nil
}

func foldedPrefixSpellingMismatch(a, b string) (aPrefix, bPrefix string, mismatch bool) {
	for aStart, bStart := 0, 0; ; {
		aEnd, aMore := nextPathSegmentEnd(a, aStart)
		bEnd, bMore := nextPathSegmentEnd(b, bStart)
		aSegment, bSegment := a[aStart:aEnd], b[bStart:bEnd]
		if pathCollisionKey(aSegment) != pathCollisionKey(bSegment) {
			return "", "", false
		}
		if aSegment != bSegment {
			return a[:aEnd], b[:bEnd], true
		}
		if !aMore || !bMore {
			return "", "", false
		}
		aStart, bStart = aEnd+1, bEnd+1
	}
}

func nextPathSegmentEnd(path string, start int) (end int, more bool) {
	if slash := strings.IndexByte(path[start:], '/'); slash >= 0 {
		return start + slash, true
	}
	return len(path), false
}

// Seal 构建期封装清单:确定性 CBOR → nonce(12,取自 rng)‖ GCM(aad=suiteByte)。
// rng 为 nil 时退回 crypto/rand;测试注入固定 rng 使 sealed 字节可对拍(B5/B11)。
// 每分享只应 Seal 一次并复用返回字节——GCM tag 即清单指纹,续传 journal 锚定它,
// 重 Seal 会令指纹漂移(§6.3)。
func Seal(manifestKey []byte, m *Manifest, rng io.Reader) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	aead, err := newAEAD(manifestKey)
	if err != nil {
		return nil, err
	}
	plain, err := encMode.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("manifest: encode CBOR: %w", err)
	}
	// 预检对象 = 交给中转的 sealed 全长(中转按 MaxManifestSize 限额),故计入
	// nonce 与 tag 的开销。
	sealedLen := len(plain) + aead.NonceSize() + aead.Overhead()
	if sealedLen > MaxManifestSize {
		return nil, fmt.Errorf("%w: encoded size is %d bytes", ErrManifestTooLarge, sealedLen)
	}
	if rng == nil {
		rng = rand.Reader
	}
	nonce := make([]byte, aead.NonceSize(), sealedLen)
	if _, err := io.ReadFull(rng, nonce); err != nil {
		return nil, fmt.Errorf("manifest: read random nonce: %w", err)
	}
	// out = nonce ‖ GCM 输出:以 nonce 切片为 dst 原地追加,容量已预留,零拷贝。
	return aead.Seal(nonce, nonce, plain, suiteAAD(link.SuiteAESGCM)), nil
}

// Open 解封并严格解码清单,不做结构校验——那是接收流程里显式的一步
// (Open → Validate → 下载,§6.13),错误分类因此对调用方可见。
// M1 只有 suite 0x01,固定分派;分派结构本身为 0x02 留位(B13/§6.14)。
func Open(manifestKey, sealed []byte) (*Manifest, error) {
	return openSuite(link.SuiteAESGCM, manifestKey, sealed)
}

// openSuite 按 suiteByte 分派 sealed 的解析:尾部结构(tag、未来的 ‖sig(64))
// 是套件属性,长度取自各分支的 AEAD 而非跨包字面量(B13)。
func openSuite(suite byte, manifestKey, sealed []byte) (*Manifest, error) {
	// 与发送端预检同限:超限 blob 直接拒,不为它付解密成本(诚实端不会触发)。
	if len(sealed) > MaxManifestSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrManifestTooLarge, len(sealed))
	}
	switch suite {
	case link.SuiteAESGCM:
		aead, err := newAEAD(manifestKey)
		if err != nil {
			return nil, err
		}
		if len(sealed) < aead.NonceSize()+aead.Overhead() {
			return nil, fmt.Errorf("%w: got %d bytes, need at least %d", ErrSealedTooShort, len(sealed), aead.NonceSize()+aead.Overhead())
		}
		nonce, ct := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
		plain, err := aead.Open(nil, nonce, ct, suiteAAD(suite))
		if err != nil {
			return nil, fmt.Errorf("manifest: unseal failed (wrong key or tampered content): %w", err)
		}
		return decodeManifest(plain)
	default:
		return nil, fmt.Errorf("%w:0x%02x", ErrUnsupportedSuite, suite)
	}
}

// decodeManifest 先宽容探测 v(B15),已知版本才严格解码,最后以确定性重编码
// 比对强制 canonical 形态唯一——形态不唯一,黄金向量对拍与"GCM tag 即清单指纹"
// 都会失去意义。
func decodeManifest(plain []byte) (*Manifest, error) {
	var probe struct {
		// 指针区分"字段缺失"与"值为 0":前者是结构损坏,后者是版本不识别。
		Version *uint64 `cbor:"v"`
	}
	if err := probeDecMode.Unmarshal(plain, &probe); err != nil {
		return nil, fmt.Errorf("manifest: decode manifest CBOR: %w", err)
	}
	if probe.Version == nil {
		return nil, errors.New("manifest: missing version field v")
	}
	if *probe.Version != CurrentVersion {
		return nil, fmt.Errorf("%w: manifest v=%d, supported v=%d", ErrUnsupportedVersion, *probe.Version, CurrentVersion)
	}
	var m Manifest
	if err := strictDecMode.Unmarshal(plain, &m); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNonCanonical, err)
	}
	reencoded, err := encMode.Marshal(&m)
	if err != nil {
		return nil, fmt.Errorf("manifest: re-encode CBOR: %w", err)
	}
	if !bytes.Equal(reencoded, plain) {
		return nil, fmt.Errorf("%w: differs from deterministic re-encoding", ErrNonCanonical)
	}
	return &m, nil
}

// suiteAAD:清单 AAD 只编码套件字节做域分隔;位置绑定是块 AAD 的职责(§6.3)。
func suiteAAD(suite byte) []byte { return []byte{suite} }

func newAEAD(manifestKey []byte) (cipher.AEAD, error) {
	if len(manifestKey) != manifestKeyBytes {
		return nil, fmt.Errorf("manifest: manifestKey must be %d bytes for AES-256, got %d", manifestKeyBytes, len(manifestKey))
	}
	block, err := aes.NewCipher(manifestKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

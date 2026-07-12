package chunk

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/windshare/windshare/core/internal/keyderiv"
	"github.com/windshare/windshare/core/layout"
	"github.com/windshare/windshare/core/link"
)

// §8 协议常量:值变更即破坏既有分享的可解密性,一律具名并由测试钉死。
const (
	// DefaultChunkSize = 1 MiB:块是加密/校验/续传单位;大文件为主可上调至 4 MiB。
	DefaultChunkSize int64 = 1 << 20

	// SegmentBytes is the cryptographic subkey-rotation span. It is intentionally
	// independent from layout.MaxChunkSize: rotation geometry must not authorize a
	// multi-gigabyte block allocation.
	SegmentBytes int64 = 16 << 30

	// MaxSealsPerSegKey = 2³²(NIST SP 800-38D 随机 IV 上限):单 segKey 的
	// Seal 调用次数熔断。计的是 Seal 次数而非块位置数——多接收端扇出、重试、
	// 续传重发都在累加;达界后 nonce 碰撞概率仍 ≤ 2⁻³²。
	MaxSealsPerSegKey uint64 = 1 << 32

	// NonceBytes = 12:每次 Seal 取全新随机 nonce,前置于密文上线(§6.3)。
	NonceBytes = 12

	// TagBytes = 16:GCM 认证 tag。
	TagBytes = 16
)

var (
	ErrInvalidStreamKey     = errors.New("chunk: invalid streamKey length")
	ErrInvalidChunkSize     = errors.New("chunk: chunkSize violates layout geometry")
	ErrNilRand              = errors.New("chunk: rng must not be nil; use crypto/rand.Reader in production")
	ErrPlaintextTooLong     = errors.New("chunk: plaintext exceeds chunkSize")
	ErrInvalidPlaintextSize = errors.New("chunk: plaintext size must not be negative")
	ErrSealedSizeOverflow   = errors.New("chunk: sealed block size overflows int64")
	ErrBlockTooShort        = errors.New("chunk: block ciphertext is too short")
	ErrBlockTooLong         = errors.New("chunk: block ciphertext exceeds chunkSize")
	ErrBlockIndexOutOfRange = errors.New("chunk: block index exceeds layout.MaxChunkCount")
	ErrUnknownSuite         = errors.New("chunk: unsupported block cipher suite; upgrade required")

	// ErrSealLimit:达到 MaxSealsPerSegKey 意味着随机 nonce 的生日界预算耗尽,
	// 继续 Seal 会让碰撞概率越过 2⁻³²,必须中止分享(B12)。
	ErrSealLimit = errors.New("chunk: per-segment seal limit reached; share aborted")
)

// SealedSize returns the exact wire size for one plaintext under a suite. Keeping
// nonce, authentication tag, and future suite trailer arithmetic here prevents
// receivers from predicting a suite's private envelope shape.
func SealedSize(suite byte, plaintextBytes int64) (int64, error) {
	if plaintextBytes < 0 {
		return 0, ErrInvalidPlaintextSize
	}
	trailer, err := suiteTrailerLen(suite)
	if err != nil {
		return 0, err
	}
	baseOverhead := int64(NonceBytes + TagBytes)
	if trailer < 0 || uint64(trailer) > uint64(math.MaxInt64-baseOverhead) {
		return 0, ErrSealedSizeOverflow
	}
	overhead := baseOverhead + int64(trailer)
	if plaintextBytes > math.MaxInt64-overhead {
		return 0, ErrSealedSizeOverflow
	}
	return plaintextBytes + overhead, nil
}

// MaxSealedSize validates a chunk-size ceiling before converting it to the
// maximum wire allocation for the suite. It is intentionally distinct from
// SealedSize: callers must not accidentally treat an arbitrary plaintext length
// as an allocation policy.
func MaxSealedSize(suite byte, chunkSize int64) (int64, error) {
	if _, err := layout.ValidateGeometry(chunkSize, 0); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrInvalidChunkSize, err)
	}
	return SealedSize(suite, chunkSize)
}

// Codec 对一条打包流做块级 AEAD(suite 0x01:AES-256-GCM)。
//
// 并发安全:发送端扇出时多个接收会话并发 Seal 同一分享的块;nonce 取自共享
// rng 且 Seal 计数是安全不变量,两者都必须在锁内串行推进。
type Codec struct {
	suite         byte
	chunkSize     int64
	maxSealedSize int64
	chunksPerSeg  uint64
	rng           io.Reader

	mu        sync.Mutex
	streamKey []byte
	segs      map[uint32]*segState
}

// segState 按 segKey 缓存 AEAD 与 Seal 计数;segKey 懒派生——多数分享
// ≤ 16 GiB 只会触碰段 0。
type segState struct {
	aead  cipher.AEAD
	seals uint64
}

// NewCodec 构造块编解码器。streamKey 是 keyderiv.StreamKey 的输出(本包不做
// readSecret 级派生);rng 注入而非内取 crypto/rand,金标向量的确定性以此为命脉(§7)。
func NewCodec(streamKey []byte, chunkSize int64, rng io.Reader) (*Codec, error) {
	if len(streamKey) != keyderiv.KeyBytes {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", ErrInvalidStreamKey, len(streamKey), keyderiv.KeyBytes)
	}
	// Chunk and manifest construction share layout's exact size contract. Wrapping
	// both sentinels keeps the package-level error stable while exposing the root cause.
	if _, err := layout.ValidateGeometry(chunkSize, 0); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidChunkSize, err)
	}
	if rng == nil {
		return nil, ErrNilRand
	}
	maxSealedSize, err := SealedSize(link.SuiteAESGCM, chunkSize)
	if err != nil {
		return nil, err
	}
	return &Codec{
		suite:         link.SuiteAESGCM,
		chunkSize:     chunkSize,
		maxSealedSize: maxSealedSize,
		chunksPerSeg:  uint64(SegmentBytes / chunkSize),
		rng:           rng,
		// 防调用方复用底层数组:streamKey 决定整条流的全部 segKey。
		streamKey: append([]byte(nil), streamKey...),
		segs:      map[uint32]*segState{},
	}, nil
}

// Seal 加密全局块号 i 的明文,输出 nonce‖ct‖tag。每次调用都从注入 rng 取全新
// 12B nonce——重复 Seal 同一块号也绝不复用 nonce,这是随机 nonce 方案的全部意义(§6.3)。
func (c *Codec) Seal(i uint64, plaintext []byte) ([]byte, error) {
	seg, err := c.segOf(i)
	if err != nil {
		return nil, err
	}
	if int64(len(plaintext)) > c.chunkSize {
		// 块几何(§6.4)保证明文 ≤ chunkSize;超长意味着调用方几何推导有误,
		// 且会突破段字节预算,拒绝而非静默放行。
		return nil, fmt.Errorf("%w: block %d plaintext is %d bytes, limit %d", ErrPlaintextTooLong, i, len(plaintext), c.chunkSize)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st, err := c.segStateLocked(seg)
	if err != nil {
		return nil, err
	}
	if st.seals >= MaxSealsPerSegKey {
		return nil, fmt.Errorf("%w (segment %d)", ErrSealLimit, seg)
	}
	sealedSize, err := SealedSize(c.suite, int64(len(plaintext)))
	if err != nil {
		return nil, err
	}
	out := make([]byte, NonceBytes, int(sealedSize))
	if _, err := io.ReadFull(c.rng, out); err != nil {
		return nil, fmt.Errorf("chunk: read random nonce: %w", err)
	}
	st.seals++
	// dst 与 nonce 共享底层数组:容量恰好,GCM 输出追加在 nonce 之后,零拷贝
	// 得到 nonce‖ct‖tag 线格式。
	return st.aead.Seal(out, out[:NonceBytes], plaintext, c.aad(i)), nil
}

// Open 解密全局块号 i 的块密文:按 suiteByte 分派解析出 nonce 与 GCM 输入,
// 再以 AAD=suiteByte‖u64_be(i) 验证——错位/换块/跨密钥在此全部失败(§6.5)。
func (c *Codec) Open(i uint64, blockCT []byte) ([]byte, error) {
	seg, err := c.segOf(i)
	if err != nil {
		return nil, err
	}
	if int64(len(blockCT)) > c.maxSealedSize {
		return nil, fmt.Errorf("%w: block %d ciphertext is %d bytes, limit %d", ErrBlockTooLong, i, len(blockCT), c.maxSealedSize)
	}
	nonce, gcmCT, err := parseBlockCT(c.suite, blockCT)
	if err != nil {
		return nil, err
	}
	st, cached, err := c.segStateForOpen(seg)
	if err != nil {
		return nil, err
	}
	plaintext, err := st.aead.Open(nil, nonce, gcmCT, c.aad(i))
	if err != nil {
		return nil, fmt.Errorf("chunk: authenticate and decrypt block %d: %w", i, err)
	}
	if !cached {
		c.cacheAuthenticatedSegment(seg, st)
	}
	return plaintext, nil
}

// aad 编码「域 + 位置」:suiteByte 做域分隔,u64_be(全局块号) 做位置绑定——
// 随机 nonce 不再编码位置,位置绑定全由 AAD 承担(§6.5)。
func (c *Codec) aad(i uint64) []byte {
	buf := make([]byte, 1+8)
	buf[0] = c.suite
	binary.BigEndian.PutUint64(buf[1:], i)
	return buf
}

// segOf 按 seg = i/(SegmentBytes/chunkSize) 选段(§6.3);寻址始终用全局块号,
// 分段纯属内部加密细节。
func (c *Codec) segOf(i uint64) (uint32, error) {
	// Codec does not own a particular manifest's final chunk count, but every
	// valid layout is bounded by MaxChunkCount. Enforcing that global domain here
	// prevents direct callers from turning block indices into an unbounded key
	// cache namespace before a higher layer can apply its exact share geometry.
	if i >= layout.MaxChunkCount {
		return 0, fmt.Errorf("%w: block %d, valid global range [0,%d)", ErrBlockIndexOutOfRange, i, layout.MaxChunkCount)
	}
	return uint32(i / c.chunksPerSeg), nil
}

// segStateLocked 懒派生并缓存 segKey 的 AEAD;派生本身只在 keyderiv 发生。
// 调用方须持 c.mu。
func (c *Codec) segStateLocked(seg uint32) (*segState, error) {
	if st, ok := c.segs[seg]; ok {
		return st, nil
	}
	st, err := c.newSegState(seg)
	if err != nil {
		return nil, err
	}
	c.segs[seg] = st
	return st, nil
}

// segStateForOpen leaves a missing segment provisional. An unauthenticated
// network envelope must not gain ownership of long-lived key-cache state merely
// by selecting a new, otherwise valid segment number.
func (c *Codec) segStateForOpen(seg uint32) (*segState, bool, error) {
	c.mu.Lock()
	st, ok := c.segs[seg]
	c.mu.Unlock()
	if ok {
		return st, true, nil
	}
	st, err := c.newSegState(seg)
	return st, false, err
}

func (c *Codec) cacheAuthenticatedSegment(seg uint32, st *segState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.segs[seg]; !exists {
		c.segs[seg] = st
	}
}

func (c *Codec) newSegState(seg uint32) (*segState, error) {
	block, err := aes.NewCipher(keyderiv.SegKey(c.streamKey, seg))
	if err != nil {
		return nil, err // 不可达:SegKey 恒 32 字节(AES-256)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err // 不可达:标准 AES 分组恒支持 GCM
	}
	return &segState{aead: aead}, nil
}

// parseBlockCT 按 suiteByte 分派块密文解析:先切 12B nonce,再按 suite 特定的
// 尾部长度界定 GCM 输入——尾部不硬编码,为 suite 0x02 的 ‖sig(64) 留演进位(B13)。
func parseBlockCT(suite byte, blockCT []byte) (nonce, gcmCT []byte, err error) {
	trailer, err := suiteTrailerLen(suite)
	if err != nil {
		return nil, nil, err
	}
	if len(blockCT) < NonceBytes+TagBytes+trailer {
		return nil, nil, fmt.Errorf("%w: got %d bytes, suite 0x%02x requires at least %d", ErrBlockTooShort, len(blockCT), suite, NonceBytes+TagBytes+trailer)
	}
	body := blockCT[:len(blockCT)-trailer]
	return body[:NonceBytes], body[NonceBytes:], nil
}

// suiteTrailerLen 给出 GCM tag 之后的 suite 特定尾部长度:0x01 无尾部;
// 0x02(M2)将为 64B 逐块签名(§6.14)。M1 不实现签名,仅留解析位。
func suiteTrailerLen(suite byte) (int, error) {
	switch suite {
	case link.SuiteAESGCM:
		return 0, nil
	default:
		return 0, fmt.Errorf("%w(suite 0x%02x)", ErrUnknownSuite, suite)
	}
}

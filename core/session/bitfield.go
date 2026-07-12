package session

import (
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"math/bits"
	"slices"

	"github.com/windshare/windshare/core/layout"
)

// ErrBitfieldEncoding:Bitfield 序列化字节非法(头部残缺/长度不符/padding 非零)。
var ErrBitfieldEncoding = errors.New("session: invalid bitfield encoding")

// Bitfield 记录连续块号区间 [0, Len()) 的持有状态,是断点续传的最小状态:
// .wsresume journal 与网页端 IndexedDB 都持久化本序列化(§6.12)。
//
// 线格式 = u64 小端位长 ‖ ⌈n/8⌉ 字节紧凑位组;位 i 存于字节 i/8 的第 i%8 位
// (LSB 优先),与数据面帧同走小端习惯。尾部 padding 位必须为零——同一状态
// 只有唯一编码,journal 校验与跨语言对拍不必容忍等价类。
//
// 值语义同切片:副本共享底层存储,Set 对所有副本可见。
type Bitfield struct {
	n    uint64
	bits []byte
}

// NewBitfield 建一张 n 位全零位图。调用方只能使用经过布局校验的几何；
// 超界代表程序不变量已被绕过，因此在分配前失败而不是制造无界稠密状态。
func NewBitfield(n uint64) Bitfield {
	if n > layout.MaxChunkCount {
		panic(layout.ErrTooManyChunks)
	}
	return Bitfield{n: n, bits: make([]byte, byteLen(n))}
}

// byteLen = ⌈n/8⌉,以移位实现,n 任意大都不上溢。
func byteLen(n uint64) uint64 {
	l := n >> 3
	if n&7 != 0 {
		l++
	}
	return l
}

// Len 返回位数(即块总数)。
func (b Bitfield) Len() uint64 { return b.n }

// Get 报告位 i 是否置位。越界一律视为"未持有"——需求集运算里,选中块号
// 超出位图长度只该意味着还没拿到,不该是错误。
func (b Bitfield) Get(i uint64) bool {
	if i >= b.n {
		return false
	}
	return b.bits[i>>3]&(1<<(i&7)) != 0
}

// Set 置位 i。越界说明调用方的块几何算错了,与切片越界同性质,直接 panic。
func (b Bitfield) Set(i uint64) {
	if i >= b.n {
		panic(fmt.Sprintf("session: Bitfield.Set(%d) out of range (length %d)", i, b.n))
	}
	b.bits[i>>3] |= 1 << (i & 7)
}

// Count 返回已置位数。
func (b Bitfield) Count() uint64 {
	var c uint64
	for _, x := range b.bits {
		c += uint64(bits.OnesCount8(x))
	}
	return c
}

// SetBits yields set indices in ascending order without allocating an index
// slice. Resume validation can therefore inspect dense state while keeping
// scheduler state interval-compact.
func (b Bitfield) SetBits() iter.Seq[uint64] {
	return func(yield func(uint64) bool) {
		for byteIndex, value := range b.bits {
			for value != 0 {
				bit := bits.TrailingZeros8(value)
				index := uint64(byteIndex*8 + bit)
				if index >= b.n || !yield(index) {
					return
				}
				value &^= 1 << bit
			}
		}
	}
}

// Restore copies a same-geometry snapshot into the existing backing storage.
// This preserves the shared value semantics used by a plan sink and its journal.
func (b Bitfield) Restore(snapshot Bitfield) error {
	if b.n != snapshot.n {
		return fmt.Errorf("%w: restore length %d into %d", ErrBitfieldEncoding, snapshot.n, b.n)
	}
	copy(b.bits, snapshot.bits)
	return nil
}

// countRange counts set bits in [first, end) without expanding chunk indices.
// ReceiveSession uses it to derive compact resume demand from ChunkSet ranges.
func (b Bitfield) countRange(first, end uint64) uint64 {
	var count uint64
	for first < end && first&7 != 0 {
		if b.Get(first) {
			count++
		}
		first++
	}
	for first+8 <= end {
		count += uint64(bits.OnesCount8(b.bits[first>>3]))
		first += 8
	}
	for first < end {
		if b.Get(first) {
			count++
		}
		first++
	}
	return count
}

// nextClear returns the first missing bit in [first, end). Whole bytes are
// skipped so an almost-complete large resume does not rescan every chunk.
func (b Bitfield) nextClear(first, end uint64) (uint64, bool) {
	for first < end && first&7 != 0 {
		if !b.Get(first) {
			return first, true
		}
		first++
	}
	for first+8 <= end {
		missing := ^b.bits[first>>3]
		if missing != 0 {
			return first + uint64(bits.TrailingZeros8(missing)), true
		}
		first += 8
	}
	for first < end {
		if !b.Get(first) {
			return first, true
		}
		first++
	}
	return 0, false
}

// MarshalBinary 输出序列化(格式见类型注释)。构造保证 padding 位恒为零,
// 无需清洗。
func (b Bitfield) MarshalBinary() ([]byte, error) {
	out := make([]byte, 8+len(b.bits))
	binary.LittleEndian.PutUint64(out, b.n)
	copy(out[8:], b.bits)
	return out, nil
}

// UnmarshalBinary 解析并整体替换 b。journal 可能被截断或篡改,任何长度不符、
// padding 位非零都拒绝——续传状态宁可作废重下,不可带伤续用。
func (b *Bitfield) UnmarshalBinary(data []byte) error {
	if len(data) < 8 {
		return fmt.Errorf("%w: bit-length header is shorter than 8 bytes", ErrBitfieldEncoding)
	}
	n := binary.LittleEndian.Uint64(data)
	if n > layout.MaxChunkCount {
		return errors.Join(ErrBitfieldEncoding, layout.ErrTooManyChunks)
	}
	if uint64(len(data)-8) != byteLen(n) {
		return fmt.Errorf("%w: bit length %d requires %d bitmap bytes, got %d", ErrBitfieldEncoding, n, byteLen(n), len(data)-8)
	}
	raw := data[8:]
	if n&7 != 0 {
		if pad := raw[len(raw)-1] &^ (1<<(n&7) - 1); pad != 0 {
			return fmt.Errorf("%w: final-byte padding bits are nonzero (0x%02x)", ErrBitfieldEncoding, pad)
		}
	}
	b.n = n
	b.bits = slices.Clone(raw) // 快照:不与调用方的缓冲共享
	return nil
}

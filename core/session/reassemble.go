package session

import "fmt"

// reassembly 收集一个块在其当前分配通道上的 BLOCK 帧。按 seq 归位,乱序容忍;
// 重复 seq、越过末帧、双 last、超内存预算都是协议违规,由调用方按通道级错误
// 处置。帧只从当前持有分配的通道计入——同一块两次发送的随机 nonce 不同,
// 跨次混拼必然过不了 AEAD,在重组层就拒绝(§6.12:部分到达的帧丢弃重取)。
type reassembly struct {
	frames  map[uint32][]byte
	bytes   int64
	lastSeq int64 // -1 = 尚未见 last 帧
}

func newReassembly() *reassembly {
	return &reassembly{frames: map[uint32][]byte{}, lastSeq: -1}
}

// add 计入一帧;凑齐即返回按 seq 拼接的完整 blockCT。maxBytes 封顶累计
// payload:不带 last 位的无尽帧流不能无限撑大内存(§6.13 同族攻击面)。
func (a *reassembly) add(b *Block, maxBytes int64) (blockCT []byte, complete bool, err error) {
	if _, dup := a.frames[b.Seq]; dup {
		return nil, false, fmt.Errorf("%w: block %d has duplicate frame seq=%d", ErrPeerViolation, b.Index, b.Seq)
	}
	if a.lastSeq >= 0 && int64(b.Seq) > a.lastSeq {
		return nil, false, fmt.Errorf("%w: block %d frame seq=%d exceeds final seq=%d", ErrPeerViolation, b.Index, b.Seq, a.lastSeq)
	}
	if b.Last {
		if a.lastSeq >= 0 {
			return nil, false, fmt.Errorf("%w: block %d has a second final frame", ErrPeerViolation, b.Index)
		}
		for seq := range a.frames {
			if int64(seq) > int64(b.Seq) {
				return nil, false, fmt.Errorf("%w: block %d existing frame seq=%d exceeds final seq=%d", ErrPeerViolation, b.Index, seq, b.Seq)
			}
		}
		a.lastSeq = int64(b.Seq)
	}
	if a.bytes += int64(len(b.Payload)); a.bytes > maxBytes {
		return nil, false, fmt.Errorf("%w: block %d reassembly exceeds the %d-byte limit", ErrPeerViolation, b.Index, maxBytes)
	}
	a.frames[b.Seq] = b.Payload

	if a.lastSeq < 0 || int64(len(a.frames)) != a.lastSeq+1 {
		return nil, false, nil
	}
	out := make([]byte, 0, a.bytes)
	for seq := int64(0); seq <= a.lastSeq; seq++ {
		out = append(out, a.frames[uint32(seq)]...)
	}
	return out, true, nil
}

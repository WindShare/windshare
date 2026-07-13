package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// SendSession 服务单个接收端:收 REQUEST,按块 ReadBlock→Seal→切帧→回 BLOCK。
// 发送端扇出时每接收会话一个独立实例(§6.6)。串行处理请求即天然背压:
// Send 被传输层顶住时不再读盘加密,单会话内存有界;并发供块的粒度由外层
// "每接收端一套会话"承担,而非会话内并行。
type SendSession struct {
	ch     FrameChannel
	store  BlockStore
	sealer Sealer

	started   atomic.Bool
	closeOnce sync.Once
	closed    chan struct{}
}

// NewSendSession 构造发送会话。会话接管 ch 的生命周期:Run 退出即关闭
// (FrameChannel 实现须幂等 Close,与 net.Conn 同规)。
func NewSendSession(ch FrameChannel, store BlockStore, sealer Sealer) (*SendSession, error) {
	if ch == nil || store == nil || sealer == nil {
		return nil, fmt.Errorf("%w: ch, store, and sealer are required", ErrNilDependency)
	}
	return &SendSession{ch: ch, store: store, sealer: sealer, closed: make(chan struct{})}, nil
}

// Run 阻塞服务,直到通道入站流关闭(对端收工,返回 nil)、上下文取消、
// Close 或致命错误。
func (s *SendSession) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return ErrSessionReused
	}
	defer s.ch.Close()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.closed:
			return ErrSessionClosed
		case f, ok := <-s.ch.Recv():
			if !ok {
				// Close() 先关 closed 信号再关通道,两个分支可能同时就绪而由
				// select 随机取——这里补判,保证 Close 后的返回值确定。
				select {
				case <-s.closed:
					return ErrSessionClosed
				default:
					return nil
				}
			}
			if err := s.handle(ctx, f); err != nil {
				return err
			}
		}
	}
}

// Close 幂等地终止会话;正在 Run 的 goroutine 以 ErrSessionClosed 退出。
func (s *SendSession) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return s.ch.Close()
}

func (s *SendSession) handle(ctx context.Context, f Frame) error {
	msg, err := Decode(f)
	if err != nil {
		// 可靠有序通道上的畸形帧不是丢包,是对端实现坏了或恶意——通知后终止。
		cause := fmt.Errorf("%w: %w", ErrPeerViolation, err)
		return s.terminate(ctx, ErrCodeBadRequest, err.Error(), cause)
	}
	switch m := msg.(type) {
	case *Request:
		return s.serve(ctx, m)
	case *Error:
		return m // 对端要求中止,原样上抛
	default:
		cause := fmt.Errorf("%w: sender received frame type 0x%02x", ErrPeerViolation, f[0])
		return s.terminate(ctx, ErrCodeBadRequest, "sender does not accept this frame type", cause)
	}
}

func (s *SendSession) serve(ctx context.Context, req *Request) error {
	// 先整体校验再供块:越界块号说明对端协议实现有误,与其供出半个请求
	// 再断,不如原样拒绝整个请求。
	count := s.store.BlockCount()
	for _, idx := range req.Indices {
		if idx >= count {
			msg := fmt.Sprintf("block index %d is out of range (block count %d)", idx, count)
			return s.terminate(ctx, ErrCodeBadRequest, msg, fmt.Errorf("%w: %s", ErrPeerViolation, msg))
		}
	}
	for _, idx := range req.Indices {
		plaintext, err := s.store.ReadBlock(idx)
		if err != nil {
			// 含快照漂移中止(§6.6):分享级致命,对端收到该 code 应整体失败。
			cause := fmt.Errorf("session: read block %d: %w", idx, err)
			return s.terminate(ctx, ErrCodeBlockRead, err.Error(), cause)
		}
		blockCT, err := s.sealer.Seal(idx, plaintext)
		if err != nil {
			// 含 Seal 计数熔断(B12):同样分享级。
			cause := fmt.Errorf("session: seal block %d: %w", idx, err)
			return s.terminate(ctx, ErrCodeSeal, err.Error(), cause)
		}
		frames, err := SplitBlockCT(idx, blockCT, MaxBlockPayload)
		if err != nil {
			cause := fmt.Errorf("session: split block %d: %w", idx, err)
			return s.terminate(ctx, ErrCodeSeal, cause.Error(), cause)
		}
		for _, bf := range frames {
			if err := s.ch.Send(ctx, bf); err != nil {
				return fmt.Errorf("session: send block %d: %w", idx, err)
			}
		}
	}
	return nil
}

// terminate preserves both failures: callers need the original domain cause,
// while orchestration must also know when the peer could not observe it.
func (s *SendSession) terminate(ctx context.Context, code uint16, msg string, cause error) error {
	msg = truncateErrorMessage(strings.ToValidUTF8(msg, "\uFFFD"))
	f, err := EncodeError(code, msg)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("%w while encoding: %w", ErrTerminalDelivery, err))
	}
	if err := s.ch.SendTerminal(ctx, f); err != nil {
		return errors.Join(cause, fmt.Errorf("%w: %w", ErrTerminalDelivery, err))
	}
	return cause
}

// 白盒测试:hub 状态机里依赖竞态时序的分支(定时器输掉 Stop 竞赛、查找与
// 挂起交错等)无法从 WS 外壳确定性触达,这里直接驱动内部状态验证其幂等性。
package signaling

import (
	"testing"

	"github.com/windshare/windshare/relay/admission"
	"github.com/windshare/windshare/relay/protocol"
)

func TestReapShareIgnoresStaleTimer(t *testing.T) {
	controller, err := admission.NewController(admission.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	lease, decision := controller.AdmitRegister("source", 1, nil)
	if !decision.Allowed() {
		t.Fatal(decision)
	}
	h := NewHub(Config{Admission: controller})
	sh := &share{id: shareX, manifestFrame: protocol.EncodeManifestFrame([]byte("m")), sessions: map[protocol.SessionID]*receiverSession{}, admissionLease: lease}
	h.shares[shareX] = sh

	// 场景 1:宽限计时器触发时 share 已被重注册(sender 非 nil)→ 不回收。
	sh.sender = &conn{}
	h.reapShare(sh)
	if h.shares[shareX] != sh || controller.Snapshot().ManifestBytes != 1 {
		t.Fatal("resumed share should not be reclaimed by an expired timer")
	}

	// 场景 2:计时器触发时注册表里已是另一个同名 share → 不动新 share。
	fresh := &share{id: shareX, sessions: map[protocol.SessionID]*receiverSession{}, admissionLease: lease}
	sh.admissionLease = nil
	h.shares[shareX] = fresh
	stale := &share{id: shareX, manifestFrame: protocol.EncodeManifestFrame([]byte("old"))}
	h.reapShare(stale)
	if h.shares[shareX] != fresh {
		t.Fatal("expired timer should not reclaim a successor share")
	}

	// 场景 3:真正挂起的 share 被回收并释放预算。
	fresh.manifestFrame = protocol.EncodeManifestFrame([]byte("m"))
	h.reapShare(fresh)
	if _, ok := h.shares[shareX]; ok || controller.Snapshot().ManifestBytes != 0 {
		t.Fatalf("suspended share should be reclaimed, snapshot=%+v", controller.Snapshot())
	}
	if fresh.admissionLease != lease {
		t.Fatal("reaping mutated the published lease pointer; concurrent join snapshots must remain race-free")
	}
}

const shareX = "whitebox0001"

func TestOpenSessionRefusesInactiveShare(t *testing.T) {
	h := NewHub(Config{})
	sh := &share{id: shareX, sessions: map[protocol.SessionID]*receiverSession{}}
	h.shares[shareX] = sh

	// 挂起(无发送端)→ 拒开会话;join 侧会以 not_found 兜住。
	if sess := h.openSession(sh, &conn{}); sess != nil {
		t.Fatal("suspended share should not open a session")
	}

	// hub 已停摆 → 拒。
	h2 := NewHub(Config{})
	h2.Close()
	if sess := h2.openSession(sh, &conn{}); sess != nil {
		t.Fatal("closed hub should not open a session")
	}
}

func TestBeginRegisterAfterHubClosed(t *testing.T) {
	h := NewHub(Config{})
	h.Close()
	reg := protocol.NewRegister(shareX, protocol.HashResumeToken(make([]byte, protocol.ResumeTokenBytes)))
	if _, outcome := h.beginRegister(&conn{}, reg); outcome != registerCollision {
		t.Fatalf("register on a closed hub should be rejected, got %v", outcome)
	}
}

func TestSessionTeardownIdempotent(t *testing.T) {
	h := NewHub(Config{})
	sh := &share{id: shareX, sessions: map[protocol.SessionID]*receiverSession{}}
	h.shares[shareX] = sh
	sess := &receiverSession{id: protocol.SessionID{1}}

	// 会话早已不在注册表:kill/end 都应无害返回,不触碰连接。
	// (sess.recv 为 nil,若走到通知/关闭路径将直接 panic 暴露问题。)
	h.killSession(sh, sess, protocol.ErrCodeSessionOverflow, "x")
	h.endSession(sh, sess, true)

	// share 也已被顶替时同理。
	h.shares[shareX] = &share{id: shareX, sessions: map[protocol.SessionID]*receiverSession{}}
	h.killSession(sh, sess, protocol.ErrCodeSessionOverflow, "x")
	h.endSession(sh, sess, true)
}

// TestResumeRegisterOutcomePerDisconnectOrdering 钉死"发送端断开处理"与
// "重注册处理"两种交错各自唯一的结果:注册结果只是 hub 已发布状态的纯函数。
// 断开尚未处理(发送端在服务端视角仍在线)→ 冲突;挂起后清单不一致的 resume
// → resume_rejected,且不得伤及挂起中的 share(仍可被正确字节恢复)。
// e2e 侧由 suspendSender 屏障保证只落在第二种交错上。
func TestResumeRegisterOutcomePerDisconnectOrdering(t *testing.T) {
	token := make([]byte, protocol.ResumeTokenBytes)
	reg := protocol.NewResumeRegister(shareX, protocol.HashResumeToken(token), protocol.EncodeResumeToken(token))
	sealed := protocol.EncodeManifestFrame([]byte("sealed"))

	h := NewHub(Config{})
	oldSender := &conn{}
	sh := &share{
		id:              shareX,
		resumeTokenHash: reg.ResumeTokenHash,
		manifestFrame:   sealed,
		sender:          oldSender,
		sessions:        map[protocol.SessionID]*receiverSession{},
	}
	h.shares[shareX] = sh

	// 交错 1:重注册先于断开处理到达 → 该 share 仍活跃,唯一合法答案是冲突。
	if _, outcome := h.beginRegister(&conn{}, reg); outcome != registerCollision {
		t.Fatalf("resume against a live sender = %v, want registerCollision", outcome)
	}

	// 交错 2:断开先被处理(挂起)→ 走宽限语义,清单不一致必须 resume_rejected。
	h.senderGone(sh, oldSender)
	attempt, outcome := h.beginRegister(&conn{}, reg)
	if outcome != registerOK {
		t.Fatalf("resume during grace failed at begin: %v", outcome)
	}
	forged := protocol.EncodeManifestFrame([]byte("forged"))
	if _, outcome := h.finishRegister(&conn{}, reg, forged, attempt); outcome != registerResumeRejected {
		t.Fatalf("mismatched manifest resume = %v, want registerResumeRejected", outcome)
	}

	// 被拒的抢注不得破坏挂起态:正主凭字节一致清单仍可恢复。
	attempt, outcome = h.beginRegister(&conn{}, reg)
	if outcome != registerOK {
		t.Fatalf("resume after rejected hijack failed at begin: %v", outcome)
	}
	resumed := &conn{}
	sh2, outcome := h.finishRegister(resumed, reg, sealed, attempt)
	if outcome != registerOK || sh2 != sh || sh.sender != resumed {
		t.Fatalf("legitimate resume after rejected hijack = %v (share %p, sender %p)", outcome, sh2, sh.sender)
	}
}

func TestSenderGoneIgnoresSupersededConn(t *testing.T) {
	h := NewHub(Config{})
	current := &conn{}
	sh := &share{id: shareX, sender: current, sessions: map[protocol.SessionID]*receiverSession{}}
	h.shares[shareX] = sh

	// 旧连接的断开回调在新连接已接管后到达 → 不得把 share 打入挂起。
	stale := &conn{}
	h.senderGone(sh, stale)
	if sh.sender != current || sh.graceTimer != nil {
		t.Fatal("stale disconnect callback should not affect a resumed share")
	}
}

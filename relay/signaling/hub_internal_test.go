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

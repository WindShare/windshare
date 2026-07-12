package e2e

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testnetwork"
)

// resumeHeldRE extracts selected-chunk resume progress from get output.
var resumeHeldRE = regexp.MustCompile(`resume state found: (\d+)/(\d+) selected blocks already present`)

// TestResumeAfterKill:杀 get 进程 → 重跑 → journal 续传完成且字节正确(§6.12)。
// 经节流 TCP 代理让回环下载慢到可确定性打断:第一次 get 落下若干块与 journal
// 后被杀,第二次 get 从 journal 只请求缺失块、收尾并逐字节一致。
func TestResumeAfterKill(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	relayURL, _ := startRelay(t)
	proxy := startThrottleProxy(t, relayURL)
	proxy.setDelay(20 * time.Millisecond) // 下行限速,保证 get#1 来得及被打断

	// Eight MiB leaves scheduling headroom after the first durable checkpoint
	// even when native loopback P2P is substantially faster than relay.
	payload := make([]byte, 8<<20)
	for i := range payload {
		payload[i] = byte(i*131 + 7)
	}
	srcRoot := writeTree(t, treeSpec{files: map[string][]byte{"big.bin": payload}})

	// share 经代理注册 → 链接 ?r= 指向代理,get 亦走代理下载。
	sp := startShare(t, proxy.url(), []string{srcRoot}, "--block-size", e2eBlockSize)
	full := sp.waitLine(t, "Link: ", procIOTimeout)

	out := t.TempDir()
	outTree := filepath.Join(out, "tree")

	// Kill from the CLI's post-checkpoint progress event. Relay throttling is not
	// a valid oracle once native P2P can bypass the proxy, and opening the journal
	// from the observer would interfere with atomic replacement on Windows.
	get1 := startCmd(t, "get#1", windshareBin, "get", full, "-o", out)
	waitUntil(t, func() bool {
		if get1.exited() {
			t.Fatalf("get#1 exited before a durable partial checkpoint; stderr=%s", get1.stderr.String())
		}
		return treeBytes(outTree) > 0 && strings.Contains(get1.stderr.String(), "Downloading:")
	}, 20*time.Second, "durable partial resume checkpoint")
	if get1.exited() {
		t.Fatalf("get#1 completed after a partial checkpoint; stderr=%s", get1.stderr.String())
	}
	get1.kill()

	// get#2:解除限速,从 journal 续传至完成。
	proxy.setDelay(0)
	code, _, stderr := runGet(t, getTimeout, full, "-o", out)
	if code != 0 {
		t.Fatalf("resumed get exit code %d; stderr=%s", code, stderr)
	}
	sp.kill()

	m := resumeHeldRE.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("get#2 did not report resume state; stderr=%s", stderr)
	}
	resumedHeld, _ := strconv.Atoi(m[1])
	if resumedHeld < 1 {
		t.Errorf("resume should skip at least one block, got %s/%s blocks", m[1], m[2])
	}
	assertTreeEqual(t, srcRoot, outTree)
}

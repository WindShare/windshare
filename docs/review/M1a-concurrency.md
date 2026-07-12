# M1a 对抗评审:调度器正确性 + 并发/传输集成

- 评审员:独立对抗评审(未参与实现),只读审查,不改代码、不 commit。
- 范围:`core/session`(frame/bitfield/reassemble/receive/send/wire)、`transport/relay`(SenderConn/ReceiverConn/Channel/link/重连)、`relay/signaling`+`relay/forward`(转发排序/排空)、`cmd/windshare/internal/cli`(会话编排/journal/rejoin)。
- 依据:执行计划 §3 / §6.6 / §6.7 / §6.8 / §6.11 / §6.12 / §8,M1-实施说明 §7 / §10。
- 复核手段:通读 + `go test -race`(全绿)+ 一处独立编写的 hostile-relay 复现程序(见 S-1)。
- 结论:**调度器块协议本身(需求集、在途窗口、超时重试、帧重组、有序重排缓冲界、热切换、评分)逻辑正确且内存有界,未发现死锁/goroutine 泄漏/需求集算错。** 但在**传输集成层发现一处严重缺陷**:不可信中转可用 `bye`+转发帧的平凡序列使**发送端进程 panic 崩溃**(send on closed channel)。此外独立复核确认已知缺陷#1(漂移 ERROR 被 bye 超车)真实,并给出根因与最小修复。

## 严重度计数

| 严重度 | 数量 |
|---|---|
| 严重(Critical) | 1 |
| 中等(Medium) | 3 |
| 轻微(Low) | 2 |
| 确认合规项 | 12 |

**缺陷#1 推荐修法(一句话)**:把"分享级 ERROR"从数据面降级问题当作分层错误来治——它是控制面终止信号,应与 `bye`/`sender_gone` 同走高优先且不被 endSession 丢弃的路径端到端交付,而非现在的数据道(被 bye 超车、被 `endSession` 连队一起丢)。

---

## 严重(Critical)

### S-1 不可信中转 `bye{sid}` 后续发同 sid 转发帧 → 发送端 `panic: send on closed channel`(进程崩溃)

**位置**:`transport/relay/channel.go:125-131`(`deliver`)、`transport/relay/sender.go:307-344`(`serveLink` 的 `Bye`/会话级 `Error` 分支与转发帧分支)、`transport/relay/sender.go:351-367`(`ensureSession` 墓碑复用)。

**问题**:`serveLink` 处理 `Bye`(或会话级 `Error`)时对该会话通道执行 `ch.shut(...)` + `ch.finalizeRecv()`,后者 `close(recvCh)`;但该会话**作为墓碑保留在 `s.live` 中**(注释明言"已闭合的会话保留在表中直至链路收尾……拦住会话终结后的在途帧被误认为新会话而复活")。随后到达的**同 sid 转发帧**在同一读循环里经 `ensureSession` 命中这条**已关闭 recvCh 的墓碑通道**(`ok=true`,不发新会话事件),调用 `deliver`:

```go
func (c *Channel) deliver(f session.Frame) {
    select {
    case c.recvCh <- f:      // recvCh 已被 finalizeRecv 关闭 → 此分支"就绪"且执行即 panic
    case <-c.closedCh:       // 亦就绪
    case <-c.l.ctx.Done():
    }
}
```

`recvCh` 已关闭时,`c.recvCh <- f` 在 select 中是"可执行"分支,Go 在多个就绪分支间**随机选取**;一旦选中即 `panic: send on closed channel`。同一 goroutine 顺序执行(bye 与后续转发帧都在 `serveLink` 读循环里),故 `closedCh` 必已就绪,但这**不阻止** select 选中 send 分支。灌入若干帧后几乎必然命中。

**为何 `-race` 未捕获**:这是**单 goroutine 逻辑错误**而非数据竞争(deliver 与 finalizeRecv 在同一读循环 goroutine 串行),`-race` 无从报警;现有测试也无一在 bye/会话级 error 之后继续投递转发帧,故一直漏网。

**可达性与威胁定级**:诚实中转在 `endSession` 时 `CloseSession` 丢弃该会话去往发送端的转发队列且 bye 在会话静默后才合成,故正常路径不触发;但**中转在威胁模型中不可信**(§2.2,`hostile_test.go` 开篇亦明言"客户端对中转同样不可全信")。恶意/有缺陷中转发送 `bye{sid}` 紧跟一帧 `0x02‖sid‖任意内层帧`即可**远程使发送端进程崩溃**——而发送端进程即分享进程,崩溃等于对**所有**接收端拒绝服务。定为严重。

**复现**(独立编写、已实测触发,非仓库测试):脚本中转在 `registered` 后写 `EncodeForwardFrame(sid,REQUEST)` 物化会话 → 写 `NewBye(sid)` → 再写 200 帧同 sid 转发帧。`go run -race` 稳定得到:

```
panic: send on closed channel
  transport/relay.(*Channel).deliver(...)  channel.go:126
  transport/relay.(*SenderConn).serveLink(...) sender.go:344
  transport/relay.(*SenderConn).run(...)       sender.go:240
```

**最小侵入修复(推荐)**:让 `deliver` 对已闭合墓碑**确定性丢弃**,而非撞进 send-on-closed。先做非阻塞闭合预检:

```go
func (c *Channel) deliver(f session.Frame) {
    select {
    case <-c.closedCh:
        return          // 墓碑:静默丢弃(会话已终结,§6.12 在途帧本就作废)
    default:
    }
    select {
    case c.recvCh <- f:
    case <-c.closedCh:
    case <-c.l.ctx.Done():
    }
}
```

正确性论证:`recvCh` 仅由读循环 goroutine 在 `shut()`(已 `close(closedCh)`)之后经 `finalizeRecv` 关闭,故"`recvCh` 已关闭 ⇒ `closedCh` 已关闭";预检因此在墓碑上必然命中并提前返回,杜绝 send-on-closed。**建议叠加**:`ensureSession` 命中既有通道时若其 `State()==Closed` 直接返回丢弃(语义更显式,亦挡住第二 select 的窗口),或更根本地在 `serveLink` 维护"已终结 sid 集合",终结态 sid 的转发帧不经通道直接丢。权衡:`deliver` 预检为一处纯本地改动、零协议影响,是最小且完备的修复;`ensureSession`/终结集为纵深加固。**同时建议补一条 hostile-relay 回归测试**(bye/会话级 error 后灌转发帧),它本可拦下本缺陷(见 L-2)。

---

## 中等(Medium)

### M-1(即已知缺陷#1,独立复核确认)分享级 ERROR 走数据道被 bye 超车,漂移中止信号在中转处丢失

**位置**:`core/session/send.go:100-110,126-133`(`notify` 经 `s.ch.Send` 发 ERROR)、`transport/relay/channel.go:53-76`(`Send` 一律入**数据道** `c.l.data`)、`transport/relay/channel.go:106-121`(`shut` 的 `bye` 入**高优先道** `c.l.high`)、`transport/relay/client.go:208-231`(`sendLoop` 高道排空前不碰数据道)、`relay/signaling/hub.go:288-305`(`endSession`→`CloseSession` 丢弃会话转发积压)。

**根因(独立推导)**:发送端漂移时 `SendSession.serve` 走 `store.ReadBlock`→`ErrDrift`→`notify(ErrCodeBlockRead)`,ERROR 帧经 `Channel.Send` 入**数据道**;随后 `Run` 返回触发 `defer s.ch.Close()`→`shut(...,sendBye=true)`,`bye` 入**高优先道**。同一 `sendLoop` 高道优先,故**bye 在发送端→中转这一跳就超车了 ERROR**。中转读到 bye → `byeFromSender`→`endSession`→`sess.recv.pump.CloseSession(sid)` **丢弃**去往接收端的转发队列(其中可能正排着那帧 ERROR),并向接收端发 bye(高道)。接收端因此只看到一次"干净 bye/会话终结",看不到分享级 ERROR,无法把失败归因为漂移。这与 `TestDriftAbort`(`cmd/.../e2e_test.go:507-512`)自己的对冲——"接受 ExitDrift **或** ExitNetwork"——完全吻合,是设计缺口而非偶发。

**为何"仅修发送端同道"不够**:即便发送端把 ERROR 与 bye 排到同一道(ERROR 先、bye 后),中转在**中转→接收端**这一跳仍会重新分层——`sender_gone`/`bye` 走接收端高道、内层 ERROR 走接收端转发(数据)道,高道再次超车;且 `endSession` 无条件丢弃该会话转发积压。故这是**两跳分层 + 中转零知识 + endSession 丢积压**共同决定的结构性问题,不是单点 off-by-one。

**推荐修法(见首页一句话)**:承认分享级终止 ERROR 是**控制面**而非数据面,给它一条与 `bye`/`sender_gone` 同级、且**不被会话终结丢弃**的端到端路径。实现上两种取向:(A) 由发送端把终止原因经一条控制消息告知中转,中转以带**语义化 code**(如 `sender_aborted`)的会话级 error 通知接收端——真正治本,但触及 `relay/protocol` 消息集,超出"一行改动";(B) 折中:发送端将终止 ERROR 走高道 + 中转 `endSession` 前对该会话转发队列做一次 flush 再关——仍是跨 `channel.go`/`pump.go`/`hub.go` 的改动。权衡:M1a 若暂不做结构修复,应把 `TestDriftAbort` 的"双可接受退出码"**显式记为已知限制**(现已有注释,建议升格为 §6.6/§6.7 的 backlog 条目),避免被误读为已解决。定为中等:不崩溃、不损坏数据,但让"漂移 → 请重新分享"这一产品级诊断在中转回退路径上不可靠。

### M-2 恶意清单以极小 chunkSize 撑爆接收端 bitfield / 需求集分配 → OOM(§6.13 承诺的防护未覆盖块数维度)

**位置**:`core/manifest/manifest.go:129-131`(`chunkSize` 仅校验"正的 2 的幂",无下限)、`core/share/receiver.go:59-68`(`layout.New` + `session.NewBitfield(lay.NumChunks())`)、`cmd/windshare/internal/cli/get.go:204-209`(`selectChunks` 的 `all := make([]uint64, rcv.NumChunks())`)、`core/session/bitfield.go:28-30`(`make([]byte, byteLen(n))`)。

**问题**:接收端信任清单里的 `chunkSize`。`Validate`/`layout.New` 只要求它是正的 2 的幂,**没有下限**;`MaxStreamBytes=2⁵³−1` 只约束**字节数**,不约束**块数**。恶意持链者/发送端(§2.2:发送端不可信于接收端)构造 `chunkSize=1`、单条 `size≈2⁵³` 的清单(16 MiB 上限内轻易做到)→ `NumChunks≈2⁵³`。`NewReceiver` 即 `NewBitfield(2⁵³)`→`make([]byte, 2⁵⁰)`≈1 PiB;全量下载时 `selectChunks` 更是 `make([]uint64, 2⁵³)`≈2⁵⁶ 字节——两者都在**下载开始前**触发 OOM。§6.13 明确把"恶意 streamLen 撑爆 bitfield/需求集分配"列为应封的攻击面,但现有实现只封了字节维度。

**说明与定级**:触发者需持有效链接(密钥派生认证了清单,恶意中转无密钥不能伪造),故不是最恶劣的中转攻击面,而是"不可信发送端 → 接收端"的资源耗尽;分配点跨 `manifest`/`share`/`cli`,`session` 只是被动接收超大 `selected`/`NumChunks`。修法:给 `chunkSize` 设下限(如 ≥4 KiB 或 ≥64 KiB)并/或对 `NumChunks` 设硬上限,在 `manifest.Validate` 与 `layout.New` 一处拒绝。定为中等。

### M-3 调度器事件循环在 `ce.ch.Send` 上同步阻塞 → 传输背压期暂停超时扫描与热切换感知

**位置**:`core/session/receive.go:348-385`(`schedule` 内 `ce.ch.Send(ctx, f)` 同步调用)、`transport/relay/channel.go:66-75`(`Send` 队满即阻塞)、`transport/relay/client.go:38-39`(`dataLaneFrames=16`)、`transport/relay/client.go:33`(`writeTimeout=30s`)。

**问题**:接收端块协议的单一事件循环在 `schedule` 里对通道 `Send` **同步阻塞**发 REQUEST。relay `Channel.Send` 在数据道(16 帧)满时阻塞。若中转读得慢(TCP 背压把本地数据道灌满),事件循环被顶在 `Send` 上——期间**不处理入站 BLOCK、不跑 `expire` 超时扫描、不感知新 Open 通道(热切换)**。逃逸依赖底层 `writeTimeout`(30s)令链路失败→`ctx.Done`→`Send` 返回→`retireChannel`。REQUEST 小、窗口仅 8,正常打不满 16;但慢/恶意中转可借此把接收端事件循环停摆至多约 30s,拖慢其向 P2P 的热切换。定为中等偏轻:有界、可自愈,但违背"信令/调度不被数据面背压拖累"的设计意图(§6.8 同族)。修法:REQUEST 发送与事件循环解耦(异步入队或对 `Send` 加短 deadline)。

---

## 轻微(Low)

### L-1 `Channel.deliver` 在缓冲满 + 链路并发失败时可丢帧,弱化"关闭前送达帧先交付"保证

**位置**:`transport/relay/channel.go:125-131`。

**问题**:`deliver` 的三路 select 在 `recvCh` 有空位时正常投递;但当 `recvCh`(32 帧)**已满**而 `c.l.ctx.Done()` 同时就绪(链路此刻失败),select 可能选中 `ctx.Done` 分支**丢弃该帧**。对分享级 ERROR 的正常时序无影响(ERROR 读入时链路尚活、缓冲通常有位),故不构成 M-1 的独立成因;但在数据面背压满载且链路同时断裂的窄窗里,"调度器保证把关闭前送达的帧交付完"这一集成假设(`receive.go:299-316` retireChannel 注释所依赖)会有极小概率不成立。定为轻微。建议:缓冲有空位时优先保证投递(把 `recvCh<-f` 单独一层 select 优先,ctx/closed 仅在真阻塞时兜底)。

### L-2 缺少针对"会话终结后仍收转发帧/信令"的 hostile-relay 回归测试

**位置**:`transport/relay/hostile_test.go`(现有脚本中转覆盖握手否决、会话级 error、协议违规重连,但无 bye/会话级 error 之后继续灌同 sid 转发帧的用例)。

**问题**:S-1 之所以长期潜伏,直接原因是测试面缺此一格。`hostile_test.go` 的立意正是"客户端对中转不可全信、拒绝路径必须可测",却漏了"终结后灌帧"这条。建议补一条:register→registered→物化会话→bye(或会话级 error)→再发若干同 sid 转发帧,断言 `SenderConn` 不 panic、会话正常闭合、连接存活。此测试对 S-1 修复亦是回归锚点。

---

## 确认合规项(逐条复核通过)

1. **帧编解码**:定长小端、类型字节 `REQUEST=0x01/BLOCK=0x02/ERROR=0x03`、`FlagLast=bit0` 且未定义位为零即拒、整帧 ≤ `MaxFrameSize`、各类型头长与声明长严格相等否则 `ErrFrameMalformed`;`Decode` 对 payload 做 `append` 拷贝不与传输缓冲共享(`frame.go:158-221`,金标 `TestGoldenFrames`/`TestDecodeRejects` 覆盖)。
2. **块切帧/重组**:`SplitBlockCT` seq 自 0 递增、末帧置 last、`nonce` 自然落首帧;`reassembly.add` 容忍乱序 seq,拒重复 seq/双 last/越过末帧/超 `maxBytes`(`reassemble.go`)。重组内存有界:`partial ⊆ inflight ≤ InFlightWindow`,每块 ≤ `MaxBlockBytes`。
3. **只认当前分配来路的帧**:`onBlock` 对 `assigned[idx].ce != ce` 的帧丢弃,杜绝两次发送(不同 nonce)的残帧跨次混拼(`receive.go:463-469`),与 §6.12"部分到达帧丢弃重取"一致。
4. **有序交付 + 队头保护**:`eligible` 在 Ordered 下 `horizon=min(pending,InFlightWindow)`,重排缓冲 `buffered ≤ InFlightWindow−1` 有界;`pending[0]`(最小未交付块)恒为首个候选、落当轮最优通道,`deliverReady` 升序放行;`TestReceiveOrderedHeadRetryPriority` 覆盖队头丢失不堵死缓冲。
5. **超时/重试/耗尽**:`expire` 撤销超时在途、把半死通道评分推高防其持续赢得重派,`MaxBlockAttempts` 耗尽即 `ErrBlockExhausted`,无限等待被证否;Open 失败退回重派而非立即处死通道(中转偶发损坏与恶意难分,交重试上限收敛)。
6. **热切换**:新 Open 通道以 0 分排序最优、立即拿块自证;`reapClosed`/`retireChannel` 把断连通道在途块退回需求池、丢弃半块,条目暂留至 pump `down` 事件以排空关闭前合法帧(尤其分享级 ERROR)——**调度器侧**排空语义正确(`receive.go:290-325`)。
7. **需求集**:`NeedSet` = 选中 − Have(),去重升序;续传经预置 Have 位在构造期扣减(`TestReceiveResumeWithPresetHave`)。
8. **无死锁/泄漏**:`Run` 单事件循环免锁,pump 经 `loopDone`/`Wait` 收束;`checkNoLeak` 及 `-race` 全绿。Close/teardown 幂等,孤儿通道统一关闭。
9. **transport/relay 包裹与墓碑**:`0x02‖sessionId‖内层帧` 收发对称(`protocol/frame.go`);`ensureSession` 隐式物化 + 墓碑防复活(除 S-1 的 deliver 缺陷外,防复活意图本身正确);signal 亦物化会话(§6.11 offer 可先到)。
10. **两级队列 + 排空**:客户端 `sendLoop` 与中转 `forward.Pump` 均高道排空前不碰数据道;中转 `closeAfterFlush`/`WaitIdle` 在关连接前 flush 终局 error/bye(`conn.go`/`pump.go`),`ServeConn` 等 `closedCh` 再返回以防终局消息死在半路。
11. **宽限重连**:`resumeToken` 原像 + 同字节清单校验在 hub 落实;客户端 `reconnect` 对 `share_id_collision` 退避重试(半开 TCP 下 `senderGone` 未落地的良性竞态)、对 `resume_rejected` 等身份否决立即终局(`sender.go:398-440`,`TestSenderReconnectResumeRejectedIsTerminal`)。
12. **CLI 编排**:`runWithRejoin` 复用**同一** `ReceiveSession`,rejoin 仅 `AddChannel` 新通道,需求集/位图/在途状态全数保留(§6.6 热切换原语);rejoin 后比对清单指纹,漂移即拒绝拼接(`get.go:304-311`);journal 经临时文件 + rename 原子落位、节流刷新(`journal.go:56-74`);Ctrl-C 走 `signal.NotifyContext`;退出码分层 OK/Failure/Usage/Network/Drift 语义清晰;`redial` 让会话终局裁决优先于重连结果(分享级 ERROR 与连接拆除竞态下的正确取舍)。

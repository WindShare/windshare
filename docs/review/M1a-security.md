# M1a 对抗评审:路径安全 + 中转/信令安全

- 评审员:独立对抗评审(未参与实现),只读审查。
- 范围:`core/manifest`(path.go/manifest.go)、`core/osfs`(sink/walk/source/pathlimit/reparse)、`relay/signaling`、`relay/forward`、`relay/httpapi`、`relay/protocol`、`transport/relay`。
- 依据:执行计划 §2.2 / §6.7 / §6.8 / §6.13 / §8,M1-实施说明 §10。
- 结论:未发现路径穿越/越根落盘的可利用链;发送端构建期与接收端落盘期两侧路径校验落实到位。**主要问题集中在中转的可用性(DoS)边界**:信令高优先通道与 register 面缺少每会话/每源的公平性隔离,单个持链接收端可拖垮整分享,单个源可耗尽节点 register 预算。另有一处 `.wsresume` 前缀在大小写不敏感文件系统上可被绕过。

## 严重度计数

| 严重度 | 数量 |
|---|---|
| 严重(Critical) | 0 |
| 中等(Medium) | 3 |
| 轻微(Low) | 3 |
| 确认合规项 | 14 |

---

## 中等(Medium)

### M-1 接收端 `signal` 洪泛溢出发送端**共享**高优先通道 → 整分享被拆(违反 §6.8 每会话隔离)

**位置**:`relay/signaling/conn.go:59-70`(`sendMsg` → `EnqueueHigh` 失败即 `closeNow`)、`relay/signaling/hub.go:445-457`(`relayControlToSender` → `sender.sendMsg`)、`relay/forward/pump.go:26`(`DefaultHighLaneMessages = 256`)、`relay/forward/pump.go:117-128`(`EnqueueHigh` 满即返回 false)。

**问题**:§6.8 的核心不变量是"单个慢/恶意接收端的溢出**只断该会话、不波及他人**,且绝不停读发送端 WS"。这个不变量在**数据面(转发队列)**上正确落实(见 M/合规项),但**信令高优先通道是发送端连接级共享资源**,其溢出被当作**连接级致命**处理:

- 接收端每发一条 `signal{sessionId=自己, kind, payload}`,`serveReceiver` 走 `relayControlToSender` → `sender.sendMsg(m)` → 入队到**发送端**的高优先通道(容量 256)。
- 该通道由发送端 pump 写者异步排空。若接收端**突发** 257+ 条合法 signal 快于写者排空(发送端此刻在读盘/Seal 而 WS ack 变慢,或攻击者用接近 64 KiB 的大 payload 拖慢写),`EnqueueHigh` 返回 false → `sendMsg` 内 `sender.closeNow()`。
- 发送端连接被关 → `defer h.senderGone` 触发 → **该 shareId 下全部接收会话**被 `sender_gone` 终结(`hub.go:195-215`)。

**攻击链(最小复现)**:
1. 攻击者持链接,`join` 成功拿到 `sessionId`。
2. 在自己会话内连续发送 ≥257 条 `signal`(合法字段:`{"type":"signal","sessionId":"<自己>","kind":"candidate","payload":"1"}`,单条约 70 B),尽量快、不等回应。
3. 发送端高优先通道被填满 → 中转关闭发送端连接 → 同一分享的其他所有接收端收到 `sender_gone`、下载中断。

一个"被授权的"持链者(§2.2 把半公开渠道流转列为一等假设,任何拿到链接者都能 join)即可对**其他诚实接收端**制造跨会话拒绝服务。现有测试 `TestBackpressureKillsSlowSessionOnly` 只覆盖了**转发数据面**的每会话溢出→会话级 kill;**高优先信令通道的溢出路径无测试、无每会话计量**。

**修复建议(根因,非打补丁)**:信令通道也应有"每会话"隔离/背压,而非发送端连接级"全或无"。可选:(a) 对**转发中的 signal** 按接收会话做令牌桶限速(signal 本是低频协商流,正常速率极低);(b) 为每会话维护独立的"在途已转发信令"计数上限,超限只断该会话(与数据面 `Overflow → killSession` 同构),而不是让发送端连接因某个接收端的洪泛而 `closeNow`;(c) 高优先通道对"来自单个会话的转发控制帧"设子配额。核心是把"信令溢出"从连接级致命降级为会话级止损,恢复 §6.8 的隔离承诺。

---

### M-2 register 面缺少每源限速与空闲超时,单源可耗尽节点 `MaxTotalManifestBytes`(节点级拒绝新分享)

**位置**:`relay/httpapi/handler.go:63-82`(WS 入口仅做 Origin + `ValidateShareID`,不限速)、`relay/cmd/wsrelay/main.go:66-73`(限速器**仅**注入 `JoinGate`,register 无对应闸)、`relay/signaling/hub.go:178`(预算检查是**全局**计数 `manifestBytes`)、`relay/signaling/conn.go:149-151`(`handshakeTimeout` 只覆盖首条消息;`serveSender`/`serveReceiver` 主循环的 `c.ws.Read(c.ctx)` 无读截止,`conn.go:234`、`conn.go:354`)。

**问题**:join 有每 shareId + 每 IP 双闸(`ratelimit.go`),但 **register 没有任何每 IP/每源限速或每源清单字节配额**。`MaxTotalManifestBytes`(默认 512 MiB)只封住**全节点**驻留内存,不做来源公平性:

- 单个 IP 可开多条 WS,各注册一个不同 shareId + 16 MiB 清单,约 **32 次**注册即耗尽 512 MiB 预算 → 此后**所有**诚实发送端的新 register 都拿 `manifest_budget_exceeded`(节点级功能拒绝)。
- 叠加**无空闲超时**:register 成功后连接可长期静默保持(中转不强制 keepalive、主循环无读截止),攻击者无需持续流量即可**长期占住**预算与句柄/协程,直至 TCP 断开。

**影响**:节点级可用性 —— 单个未鉴权源即可让"注册分享"这一核心功能对全网停摆。`MaxTotalManifestBytes` 防了 OOM,但没防"公平性/占用"。§6.1"不做"清单里列的是"并发规模压测与每接收端限速",并未豁免 register 面的滥用防护;§6.8 明确把"自注册大清单再海量 join"列为要防的放大面,却只对 join 侧落了闸。

**修复建议**:(a) 对 register 加**每 IP 速率限制 + 每 IP 并发分享数/清单字节配额**(与 join 侧限速器同一套桶,复用 `httpapi` 的 `bucketClass`);(b) 给已建立连接加**空闲读截止**(基于 `KeepaliveInterval` 的宽限倍数),静默连接超时即断,回收预算与句柄。二者都属"中转在无鉴权面前的必要自保",与 M1 的威胁档位("防恶意中转/防滥用")一致。

---

### M-3 `.wsresume` journal 前缀拒绝是**大小写敏感**的,大小写不敏感文件系统上可被绕过并覆盖续传 journal

**位置**:`core/manifest/path.go:20`(`resumeJournalPrefix = ".wsresume"`)、`core/manifest/path.go:66`(`strings.HasPrefix(p, resumeJournalPrefix)`,大小写敏感);对照 journal 落盘 `cmd/windshare/internal/cli/journal.go:20,48-51`,以及非续传 `O_EXCL` 保护 `core/osfs/sink.go:112-131`。

**问题**:§6.13 明确要求"以 `.wsresume` 开头的条目 → 拒绝,**防分享内容覆盖/伪造 journal**"。但 `ValidatePath` 的前缀匹配是字节级大小写敏感的,而 journal 落在输出根、Windows/macOS 文件系统大小写不敏感。恶意发送端(路径安全的威胁模型正是"发送端不可信于接收端落盘",§2.2)自 Seal 清单、知道自己的清单指纹前缀,可构造根级条目 `.WSRESUME-<指纹前缀>`(或 `.WsResume-…`):

- 它以 `.WSRESUME` 开头,**不匹配**大小写敏感的 `.wsresume` 前缀 → `ValidatePath` 放行;
- `Manifest.Validate` 的折叠碰撞只在**清单条目之间**比对(`manifest.go:151-158`),journal 文件名不在清单里,故也不拦;
- 落盘时在 Windows 上 `.WSRESUME-<fp>` 与真实 journal `.wsresume-<fp>` 是同一文件。

**后果**:
- **续传模式**(`Resume=true`,`sink.go:118` 不加 `O_EXCL`):写入该条目会**覆盖/污染正在使用的 journal**(bitfield + 指纹),破坏续传状态 → 可用性损伤,且发生在受害者本次下载中途。
- 非续传模式:首次触碰经 `O_EXCL`,因 journal 已存在而 `ErrAlreadyExists` 中止下载 —— 仍是被恶意清单诱导的下载失败,但不静默损坏。

严格说这是"分享内容能与工具保留文件撞名"这一被文档承诺挡住的攻击面,在大小写不敏感 FS 上未被完全兑现。

**修复建议(根因)**:前缀判定应与文件系统的"同一名字"语义一致 —— 用**大小写折叠 + NFC** 归一后再比前缀(与 `foldPath` 同一口径,`path.go:119-121`),即 `strings.HasPrefix(foldPath(p), foldPath(resumeJournalPrefix))`。这样任何折叠后落到 `.wsresume` 的根级条目都被拒,与 §6.13 的意图对齐。

---

## 轻微(Low)

### L-1 接收端落盘不检查中间路径分量是否为符号链接/reparse point;`safeJoin` 只校验逻辑字符串,预置符号链接可越根

**位置**:`core/osfs/sink.go:55-70`(`resolve`:`filepath.Join` + `filepath.Rel` 逃逸检查 + 长度上限,全部作用于**逻辑路径串**)、`sink.go:95`(`os.MkdirAll(filepath.Dir(joined))`)、`sink.go:122`(`os.OpenFile`)。对照:reparse point 的"不跟随"只在**发送端 Walk** 落实(`walk.go:72,102-110`)。

**问题**:§6.13 声称"符号链接/reparse point 不跟随",但该保证只在**发送端打包**时兑现;**接收端 Sink 从不检查**输出树中的中间目录分量是否是已存在的符号链接/junction。`filepath.Rel` 逃逸检查基于路径**字符串**而非解析后的真实路径:若输出根下 `sub/` 已是指向根外(如 `C:\Windows` 或 `/etc`)的符号链接,则 `EnsureDir("sub")`(`MkdirAll` 视其已存在即返回)后写 `sub/x` 会**穿过链接落到根外**;`O_EXCL` 只保证叶子文件创建的原子性,拦不住父目录分量的符号链接跟随(经典 TOCTOU)。

**威胁模型限定**:清单本身**无法**引入符号链接(接收端只创建目录与常规文件),故此路径需要**本地攻击者预先在输出目录放置符号链接**——这超出 §2.2 文档化的"恶意发送端 vs 接收端落盘"模型(能写输出目录的本地攻击者本已越过防线)。因此定级 Low。但它是"接收端也应不跟随 reparse point"这一 §6.13 表述与实现之间的**不对称缺口**,值得记录。

**修复建议**:落盘时对 `root` 到目标之间逐段用 `Lstat` 拒绝 reparse point(复用 `osfs.isReparsePoint`),或在受支持平台用 `O_NOFOLLOW`/`openat` 家族逐级打开;至少在文档里把"接收端不防预置符号链接"列为已知边界。

### L-2 上标数字设备名变体(`COM¹`/`COM²`/`COM³`,U+00B9/B2/B3)不在保留名检测内

**位置**:`core/manifest/path.go:107-114`(`isWindowsReservedName`,`strings.ToLower` 后比对 ASCII `com1..com9`)。

**问题**:Win32 路径规范化历史上会把上标 `¹²³` 映射为 `COM1/2/3`,故 `COM¹.txt` 可能仍解析到 COM3 设备,但当前检测只覆盖 ASCII 数字。属极冷门边角,Low/信息级。修复:保留名比对前把 U+00B9/B2/B3 折叠为 `1/2/3`,或对含这些码点的段直接拒绝。

### L-3 Origin 白名单按 host(含端口)精确匹配,默认端口写法差异会误拦

**位置**:`relay/httpapi/handler.go:105-111,116-135`(`normalizeOrigin` 归一为 `scheme://host`,host 含端口)。

**问题**:白名单若配 `https://windshare.top`,而某来源发 `https://windshare.top:443` 会因 host 串不等而被拒。浏览器 Origin 通常省略默认端口,实际影响很小;"无 Origin 放行"的策略本身合理(见合规项 C-13)。Low/信息级。建议归一时剥离与 scheme 匹配的默认端口。

---

## 任务项 7:已知缺陷#1(漂移 ERROR vs bye 通道竞态)的安全影响评估

**缺陷描述**(实现自述于 `cmd/windshare/internal/cli/e2e_test.go:507-509`、`get.go:321-322`):发送端读块时 `osfs.Source` 复核到 size/mtime 漂移(`source.go:44-47` → `ErrDrift`),发送会话将其转成**数据面** share 级 `ERROR` 帧(`ErrCodeBlockRead`,走每会话转发队列)并中止;而会话拆除/`bye` 走**信令高优先**通道(§6.8)。高优先的 `bye`/连接拆除可能**先于**低优先的数据面 `ERROR` 到达,接收端于是看到的是"会话被终结后重连失败"(`ExitNetwork`)而非具体的漂移码(`ExitDrift`)。

**安全影响评估:无(纯可用性/诊断问题)**。理由:

1. **不影响完整性**:漂移中止会**留下缺块**;接收端 `Finalize` 强制"所选块全部到位"(`receiver.go:216-239`,否则 `ErrMissingBlocks`),不会把半新半旧/不完整的输出物化为"完成"。竞态只改变**接收端显示哪个错误码/退出码**,不改变"是否接受了损坏内容"。
2. **不改变认证边界**:接收端只接受通过 AEAD(`Accept` → `chunk.Open`,`receiver.go:89-100`)且几何一致的块;漂移意味着发送端**停止**供给缺块,接收端无从伪造。
3. **无可被利用面**:竞态两端(`ExitDrift` / `ExitNetwork`)都是"下载失败"的诚实终局,攻击者无法借此让接收端误判"成功"。

**建议(诊断确定性,非安全)**:让漂移 share 级 `ERROR` 也走高优先信令面(或在 flush 完数据面 `ERROR` 后再发 `bye`),或接收端把"会话被拆且有缺块"统一按"下载未完成"呈现。属 UX 打磨,不改变安全结论。

---

## 任务项 8:威胁模型一致性

代码行为与 §2.2 文档化的残余风险**吻合,未发现超出文档承诺的额外泄漏**:

- **对称完整性"能验即能造"**:接收端完整性锚点全部来自 `readSecret` 派生密钥(`manifest.Open`/`chunk.Open`),任何持链者可 Seal 合法伪块 —— 与 §2.2 残余风险一致,由 M2 suite `0x02` 结构性修复。`chunk`/`manifest.openSuite` 已按 suiteByte 分派、不硬编码尾长(`manifest.go:203-228`),满足 §6.14 前向要求。
- **REQUEST 块号访问模式泄漏**:中转对转发帧只解外层 `sessionId` 路由头(`protocol.DecodeForwardFrame`,`frame.go:80-87`),内层零解析;但内层 `REQUEST` 的块号是明文,恶意中转**可**读到访问模式 —— 正是 §2.2/§6.7 列入的已知泄漏,无额外泄漏。
- **中转零知识**:`sealedManifest` 原样存储(`adoptShare`,`hub.go:181-190`)、join 时原样回放(`serveReceiver`,`conn.go:348`),中转从不解析密文内层。日志仅记 `shareId` + error code(`conn.go:254,381`)——`shareId` 对中转本就可见,无内容泄漏。
- **resumeTokenHash 不外泄**:join 回传只含 `manifest{sessionId}` + 清单,**不含** `resumeTokenHash`;接收端无从获知该哈希,恶意中转即便获知也受 SHA-256 原像抗性保护(`VerifyResumeToken` 常数时间,`message.go:295-306`)。与 §6.8 一致。

---

## 确认合规项

- **C-1 路径穿越纵深(发送+接收两侧)**:`../`/`.`/空段/前导尾随 `/`/`//` 由 `validateSegment` 空段+相对段规则统覆(`path.go:79-103`);盘符与 UNC 由 `:` 与 `\` 落入 `windowsIllegalPathChars` 一并封死(`path.go:16`);控制字符(含 NUL、C0/C1)由 `unicode.IsControl`(`path.go:88`);非法 UTF-8 与非 NFC 拒绝(`path.go:41,58-63`)。发送端构建期经 `CanonicalPath`(`walk.go:127-131` → `add`),接收端落盘期经 `manifest.ValidatePath`(`sink.go:56`)+ `Manifest.Validate`(`receiver.go:52`),**两侧同规**。
- **C-2 Windows 保留名 + 结尾空格/点 + ADS**:`CON/PRN/AUX/NUL/COM1-9/LPT1-9`(含带扩展名 `CON.txt`、含结尾空格 `CON .txt`)由 `isWindowsReservedName` 取首点前主干 + 剥尾空格比对(`path.go:107-114`);结尾空格/点由 `validateSegment`(`path.go:96-98`);`:`(ADS/盘符)由非法字符集(`path.go:91`)。
- **C-3 safeJoin 逃逸兜底**:`resolve` 在 `ValidatePath` 之外**独立**用 `filepath.Rel` 复核不逃逸输出根(`sink.go:62-65`),两层不互相依赖,构成纵深。
- **C-4 MAX_PATH 上限**:组合后绝对路径按 UTF-16 码元 + NUL 计长,超 260 拒绝(`pathlimit_windows.go`),在每个落盘入口 `resolve` 强制;POSIX 侧 4096(`pathlimit_other.go`)。
- **C-5 大小写 + NFC 折叠碰撞**:`Manifest.Validate` 以 `foldPath`(case-fold + NFC)为键检测跨条目碰撞并区分"精确重复"与"折叠碰撞"(`manifest.go:151-158`);构建期(`Seal` → `Validate`)与接收期(`NewReceiver` → `Validate`)同规。
- **C-6 `.wsresume` 前缀拒绝**:根级前缀匹配落实(`path.go:66`)——**但大小写敏感性缺口见 M-3**。
- **C-7 已存在同名非续传拒绝(O_EXCL 原子)**:首次触碰某路径且非续传时置 `O_CREATE|O_EXCL`,内核原子判定存在性,消除 stat-then-create 空窗(`sink.go:112-131`);续传模式显式跳过该保护。
- **C-8 符号链接/reparse 不跟随(发送端)**:Walk 对根与遍历项均查 `FILE_ATTRIBUTE_REPARSE_POINT`(`reparse_windows.go`)/`ModeSymlink`(`reparse_other.go`),跳过并记 `Skipped`,junction 显式 `SkipDir` 防下潜(`walk.go:72,102-110`)。**接收端缺口见 L-1**。
- **C-9 清单结构校验**:`size≥0`、前缀和 ≤ `MaxStreamBytes`(先比后加防 int64 回绕)、path 唯一、chunkSize 为正 2 的幂,全部在 `Manifest.Validate`(`manifest.go:124-161`);严格 CBOR + 确定性重编码比对拒非 canonical(`manifest.go:233-259`)。
- **C-10 中转零知识 / shareId 一致性 / 活跃碰撞**:清单原样存储回放(见任务项 8);`register`/`join` 消息内 `shareId` 与路径不一致即拒(`conn.go:185-188,301-304`);活跃 shareId 碰撞回 `share_id_collision`(`hub.go:156-160`)。测试:`TestShareIDMismatchRejected`、`TestActiveShareIDCollisionRejected`。
- **C-11 断线宽限:常数时间比对 + 字节一致 + 竞态安全**:`VerifyResumeToken` 用 `subtle.ConstantTimeCompare`(`message.go:295-306`);重注册要求 token 原像 + `bytes.Equal(sealed, existing.sealedManifest)`(`hub.go:165-168`);`reapShare` 回调复核 `sh.sender != nil` 免疫"Stop 与触发赛跑"(`hub.go:171-174,218-226`)。测试:`TestResumeRejectedOnBadToken`、`TestResumeRejectedOnManifestMismatch`、`TestGraceExpiryReclaimsShare`、`TestReapShareIgnoresStaleTimer`。宽限窗内抢注被 token 封堵;期满回收后的复活是 §2.2 文档化残余(M2 修复),一致。
- **C-12 背压 DoS(数据面):每会话有界队列溢出只断该会话、不停读发送端**:`EnqueueForward` 非阻塞、满即 `Overflow`(`pump.go:165-181`),`killSession` 只终结该会话(`hub.go:267-284`、`conn.go:281-291,412-418`);发送端读循环永不阻塞入队。测试:`TestBackpressureKillsSlowSessionOnly`。**(注:信令高优先通道不具此隔离——见 M-1。)**
- **C-13 join 每 shareId + 每 IP 限速;接收端不可冒用会话**:双桶"全或无"消费 + 桶表上限 + prune(`ratelimit.go`);接收端转发/信令帧 `sessionId != sess.id` 即拒(`conn.go:401-405,446-449`)。测试:`TestJoinRateLimited`、`TestReceiverCannotSpoofOtherSession`、`TestReceiverSignalSpoofRejected`。
- **C-14 帧/清单/信令边界执行 + 信令优先于数据**:`MaxSignalingMessageBytes` 读限 + Decode 双检(`conn.go:147`、`message.go:203-205`);`MaxManifestSize` 读限 + slack + 解码后复核(`conn.go:191,208-211`);`MaxFrameSize` 转发帧上限(`conn.go:266,392`);`MaxTotalManifestBytes` 全局预算(`hub.go:178`);pump `next()` 高通道排空前不碰转发帧(`pump.go:242-261`)。health/config 仅回状态/版本/限额,无 shareId 列表/计数/IP 等敏感信息(`handler.go:51-61`);Origin 白名单大小写不敏感、"无 Origin=非浏览器放行"策略合理(`handler.go:116-135`)。测试:`TestManifestTooLargeRejected`、`TestManifestBudgetRejectsNewRegister`、`TestOversizeForwardFrameRejected`。**(register 面的每源公平性缺口见 M-2。)**

# P2P 直连成功率执行计划

状态：待批准
范围：无人工端口映射时，尽可能提高浏览器接收端与 Go 发送端的 WebRTC 直连率。提高 STUN 可用性，特别是中国大陆。

## 当前基线

- 内容 activation 已经让应用 relay 立即传输，同时在后台执行 P2P；纯目录浏览不启动 ICE。
- 当前使用单一 `PeerConnection + 固定 STUN`，已经支持 Trickle ICE、一次一个 attempt、有限重试与 relay 并行承载。
- 本计划只扩展现有恢复框架，不新建第二套 retry、detach、identity 或 lane admission 所有者。

## 目标与边界

- `direct` 只表示未经过 TURN 或应用 relay 的已认证 candidate pair。
- 出链接、纯浏览和 relay 首字节不得等待探测、端口映射或 P2P。
- 普通用户不选择 STUN、不理解 NAT，也不手工开放端口；`relay-only` 保持零 ICE、零 STUN 和零端口映射。
- 双硬 NAT、CGNAT 无公网映射、UDP 封锁等不可穿透拓扑明确降级，不以无界探测伪造成功率。
- NAT、provider 和路径成本留在 `connectivity`；`core` 只接收认证后的 `FrameChannel`。
- 主界面只依据已接纳的活动 lane，以及 WebRTC lane 的 selected pair，显示“已直连”“中转传输”或“两者并行”；candidate 或端口映射成功只进入诊断。
- 连接流程不得临时提权或阻塞用户操作；防火墙权限不可得时记录原因并继续 relay。
- 中国大陆运营商、家宽、移动网络和 IPv4/双栈样本用于后续增强证据，不阻塞实现或发布，也不预先承诺成功率数字。

## 目标架构

现有 recovery supervisor 与 PeerConnection factories 重构为一个连接所有者：

```text
PeerConnectivity
├── ICEEndpointPool        受信部署配置中的全部 STUN 备选
├── SocketAuthority        Go 进程内按网络代际拥有 UDP socket
├── ReachabilityAuthority  公网 IPv6 与自动端口映射租约
├── AttemptPlanner         自动选优并生成有界 AttemptICEProfile
├── PeerSet                唯一拥有 attempt、替换、接纳与释放
└── PathObserver           selected pair、失败阶段与恢复事实
```

- `PeerSet` 吸收现有顺序恢复逻辑；不得与另一套 supervisor 并存。
- 每个 Attempt 使用新的 `PeerConnection` 和 `AttemptID`，但同一网络代际复用稳定 socket；首版不在失败的 PeerConnection 上叠加原地 ICE restart。
- `SocketAuthority` 的生命周期属于进程级网络代际，通过引用计数覆盖 attempt、已接纳 lane 和映射租约，不属于单个协议会话。
- `transport/webrtc` 只适配单个 PeerConnection/DataChannel，不拥有重试、映射或路径策略。

## 实施顺序

### 0. 修正失败作用域

- 将 peer failure 明确为 `attempt-transient`、`path-terminal`、`session-terminal`。ICE/STUN/超时等可恢复网络失败只结束当前 attempt；协议或策略拒绝终止 peer path；认证与会话不变量失败才终止 session。
- `OPERATION_ERROR` 对当前 negotiation identity 始终终态；typed reason 决定是否扩大到 path 或 session。`attempt-transient` 以新的 `AttemptID` 恢复，不得用通用 `Retryable` 重跑同一 operation。
- Go 先失败和浏览器先失败必须得到相同分类，并继续受现有 wave/session budget 约束。

### 1. 补齐直连事实

- 扩展现有 attempt observation 与 trace，以 `ProtocolSessionID + PeerPathID + AttemptID + NetworkGenerationID + ICEProfileID + side` 重建一次尝试；不新增平行诊断管线。
- 记录 STUN、candidate、offer/answer、connectivity check、selected pair、DTLS、DataChannel Open、lane admission、detach 和终止的阶段与耗时。
- candidate 事实包含 `host/srflx/prflx/relay`、UDP/TCP、IPv4/IPv6、接口类别、去重结果和可得的 STUN endpoint；浏览器无法归因时写 `unknown`，且不记录 ICE/TURN credential。
- WebRTC selected pair 记录 candidate 类型、协议、地址族、RTT、存活时间和切换原因；结合 lane transport 将路径分类为 `direct`、`turn` 或 `application-relay`，selected-pair RTT 不得冒充 STUN RTT。
- 失败使用封闭阶段/原因码；provider 原始错误只作附属诊断。观察使用有界非阻塞队列，丢失汇总为 `observer_loss`。
- 先形成当前固定 STUN、默认 per-attempt socket 生命周期的真实基线，不预先承诺成功率数字。

### 2. 建立受控 STUN 池与 attempt profile

- `ICEEndpointPool` 可容纳包括数百个端点的自有或经审核目录，但来源仅限构建、部署和本地配置；relay 信令、分享链接和远程响应不得注入 STUN。
- “270”直接参考 [FastSend 固定提交的 `publicStunList.ts`](https://github.com/ShouChenICU/FastSend/blob/8d0991af54b8e1108a7b803bc723ee248ebe002b/app/utils/publicStunList.ts)：它包含 270 个 endpoint 配置条目、269 个主机名，不代表 270 台独立物理服务器。该文件未注明上游来源、可用性验证时间或使用授权，因此只借鉴大池机制，不直接复制为 WindShare 的受信目录。
- endpoint 使用稳定 ID，并标记地域、故障域、网络提供商、端口、地址族、信任层级和部署优先级。
- `EndpointSelector` 是 `AttemptPlanner` 内的纯策略：过滤不兼容或处于失败冷却期的端点，优先选择地域和故障域不同的两个端点；无运行事实时按信任层级、部署优先级和网络代际种子加权轮换。
- 可归因的 `icecandidateerror` 按 endpoint 记录；srflx 产出和 candidate 到达耗时按 profile 记录，浏览器无法归因时不得猜测 endpoint。事实仅在内存中按 `NetworkGenerationID` 复用，网络变化即清空，不持久化或跨网络学习。
- 首次 `AttemptICEProfile` 最多激活两个 endpoint；备用 profile 从未使用的故障域选择另外两个。每个 recovery wave 最多触达四个 STUN，浏览器和 Go 可使用不同 profile。
- 同一 profile 的 endpoint 由 ICE 并行 gathering 和 Trickle，不串行探测，不等待完整 gathering，也不创建探测专用 PeerConnection。
- 默认部署覆盖国内受控端点，优先 UDP 3478；UDP 443 仅在独立 listener 不冲突且确实部署时进入池。应用 relay 与 STUN 使用独立 listener 和健康状态。
- STUN 故障不影响应用 relay；`relay-only` 不解析或访问 STUN endpoint。

### 3. 验证 Pion 的稳定 socket 与外部端口能力

- 针对固定 Pion 版本证明同一 `net.PacketConn` 能覆盖 host/srflx gathering、ICE checks、DataChannel 数据和 fresh replacement attempt。
- 证明 PCP、NAT-PMP、UPnP 返回的实际外部 IP **和端口**可发布为本地 candidate，并由同一 socket 收包；不得只改写 IP、假定内外端口相同或发布未验证 candidate。
- 明确映射晚到时能否注入当前 attempt；未证明前只允许其进入下一 fresh attempt。
- 若公开 API 不能表达这些语义，在 `transport/webrtc` 边界升级或补齐适配。本验证只阻塞稳定 socket 与端口映射，不阻塞第 0～2 阶段。

### 4. 稳定 Go 侧 ICE socket

- 用 Pion `API + SettingEngine + ICE UDP mux` 取代 factory 直接调用默认 `NewPeerConnection`。
- 同一网络代际复用 UDP4/UDP6 socket，使 discovery、ICE checks、attempt 和已接纳 lane 共享 NAT 映射。
- 保留有效 LAN、mDNS、global-unicast IPv6 与接口路径；按地址族、接口类别和 candidate 类型限制噪声。
- 网络变化创建新代际；旧代际在引用归零后释放，candidate、映射和观察事实不得跨代复用。
- 防火墙规则属于安装/平台层；连接模块不提权、不改全局策略，权限不可得时继续 relay。

### 5. 建立发送端公网可达路径

- 发布当前路由可用的 global-unicast IPv6；tentative、deprecated、过期或已换代地址不得继续使用。
- 只有内容 activation、尚未直连且普通 ICE 已获得短暂先行机会时，才后台有界尝试 PCP、NAT-PMP 和受控 UPnP；直连成功立即取消未完成工作。
- 多种映射协议由 `ReachabilityAuthority` 统一竞争，只保留一个租约并撤销其余结果。映射绑定 socket 与网络代际，只发布设备实际返回且公网可路由的 IP/端口；追加 candidate，不替换 LAN、IPv6 或 STUN candidate。
- 映射晚到默认供下一 fresh attempt 使用；只有第 3 阶段证明安全时才加入当前 attempt。最终可达性仍由认证 ICE check 裁决。
- 租约到期前刷新；网关重启、地址变化和网络切换时重建；正常退出撤销，崩溃依靠短 TTL 收敛。

### 6. 有界恢复

- 首次 attempt 使用主 profile、单一 PeerConnection、Trickle ICE 和现有 phase budget，不等待 gathering 完成。
- `attempt-transient` 失败后以同一稳定 socket 创建 fresh PeerConnection；备用 profile 可用时至多旋转一次，任何时刻只有一个 PeerConnection 在尝试。
- direct 失效时健康 relay 持续承载内容。仅网络变化、lane detach 或一次有预算的延迟恢复可重新触发；不做永久周期打洞。
- 网络变化使用新 socket 代际；lane admission 转移所有权后及时释放失败 attempt 与无用 candidate。
- 后台尝试不得让传输进度归零或状态反复闪烁；只有已接纳 lane 的 selected pair 证明后才显示“已直连”。

### 7. 后续独立子阶段

- 端口预测、birthday attack、ICE-TCP 和 WebTransport 不在本轮直连实现范围。
- TURN/TCP/TLS 443、Go proxy dialer 及更多中国大陆运营商和地域部署作为 fallback 子阶段独立推进，不阻塞直连主线，也不得标记为 direct。

## 验证与收敛

- 自动化测试验证失败作用域、profile 选择、socket/attempt 所有权、候选预算、代际隔离、实际外部端口、映射刷新与撤销、crash TTL、relay 降级和资源释放；网关行为使用确定性 fake。
- Pion spike 验证实际外部端口、收包路径和映射晚到语义；真实路由器与中国大陆运营商网络只做条件允许时的 smoke，不是交付前置。
- 每阶段运行聚焦 gate；全部代码完成后运行 `make ci-parallel`。

## 明确不做

- 不让用户手工选择 STUN、调整 timeout 或理解 NAT 类型。
- 不在出链接、纯浏览或应用启动时提前进行 ICE、STUN、端口映射或防火墙提权。
- 不接受 relay 或能力链接动态下发 STUN 地址；本阶段不做远程配置签名系统。
- 不创建并行或探测专用 PeerConnection，不做无限重试、无界 socket、候选洪泛或端口扫描。
- 不把 TURN、WebSocket relay、端口映射成功或同机转发标记为 direct。

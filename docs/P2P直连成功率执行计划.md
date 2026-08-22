# P2P 直连成功率执行计划

状态：待批准
范围：无人工端口映射时，尽可能提高浏览器接收端与 Go 发送端、Go 接收端与 Go 发送端的 WebRTC 直连率。提高 STUN 可用性，特别是中国大陆。

## 当前基线

- 内容 activation 已经让应用 relay 立即传输，同时在后台执行 P2P；纯目录浏览不启动 ICE。
- 当前使用单一 `PeerConnection + 固定 STUN`，已经支持 Trickle ICE、一次一个 attempt、有限重试与 relay 并行承载。
- 接收端固定创建 Offer/DataChannel，发送端固定 Answer；双方均为 Full ICE agent，都会收集 candidate、发送 connectivity check。固定协商角色不等于发送端被动打洞。
- 本计划只扩展现有恢复框架，不新建第二套 retry、detach、identity 或 lane admission 所有者。

## 目标与边界

- `direct` 只表示未经过 TURN 或应用 relay 的已认证 candidate pair。
- 出链接、纯浏览和 relay 首字节不得等待探测、端口映射或 P2P。
- 普通用户不选择 STUN、不理解 NAT，也不手工开放端口；`relay-only` 保持零 ICE、零 STUN 和零端口映射。
- 双硬 NAT、CGNAT 无公网映射、UDP 封锁等不可穿透拓扑明确降级，不以无界探测伪造成功率。
- NAT、provider 和路径成本留在 `connectivity`；`core` 只接收认证后的 `FrameChannel`。
- 不根据预判 NAT 类型切换 Offerer；角色反转不增加可用 candidate pair，只有真实网络证据证明存在稳定增益时才另行评估。
- 主界面只依据已接纳的活动 lane，以及 WebRTC lane 的 selected pair，显示“已直连”“中转传输”或“两者并行”；candidate 或端口映射成功只进入诊断。
- 连接流程不得临时提权或阻塞用户操作；防火墙权限不可得时记录原因并继续 relay。
- 代表性真实网络样本不阻塞实现或发布；缺少样本时只声明机制完成，不宣称直连率提升或数字。

## 目标架构

现有 recovery supervisor 与 PeerConnection factories 重构为一个连接所有者：

```text
PeerConnectivity
├── ICEEndpointPool        STUN 候选目录、信任准入与有界 profile
├── SocketAuthority        Go 进程内按网络代际拥有 UDP socket
├── ReachabilityAuthority  公网 IPv6 与自动端口映射租约
├── AttemptPlanner         自动选优并生成有界 AttemptICEProfile
├── PeerSet                唯一拥有 attempt、替换、接纳与释放
└── PathObserver           selected pair、失败阶段与恢复事实
```

- `PeerSet` 吸收现有顺序恢复逻辑；不得与另一套 supervisor 并存。
- 每个 Attempt 使用新的 `PeerConnection` 和 `AttemptID`，但同一网络代际复用稳定 socket；首版不在失败的 PeerConnection 上叠加原地 ICE restart。
- `SocketAuthority` 的生命周期属于进程级网络代际，通过引用计数覆盖 attempt、已接纳 lane 和映射租约；映射租约不得自行维持无需求的代际。
- `transport/webrtc` 只适配单个 PeerConnection/DataChannel，不拥有重试、映射或路径策略。

## 实施顺序

### 0. 修正失败作用域

- 将 peer failure 明确为 `attempt-transient`、`path-terminal`、`session-terminal`。ICE/STUN/超时等可恢复网络失败只结束当前 attempt；协议或策略拒绝终止 peer path；认证与会话不变量失败才终止 session。
- `OPERATION_ERROR` 对当前 negotiation identity 始终终态；typed reason 决定是否扩大到 path 或 session。`attempt-transient` 以新的 `AttemptID` 恢复，不得用通用 `Retryable` 重跑同一 operation。
- Go 与浏览器使用同一份封闭 reason-to-scope 向量；未知 reason 只停止 peer path，错误文本或通用 `Retryable` 不得产生 retry 或 session authority。

### 1. 补齐直连事实

- 扩展现有 attempt observation 与 trace，以 `ProtocolSessionID + PeerPathID + AttemptID + NetworkGenerationID + ICEProfileID + side` 重建一次尝试；不新增平行诊断管线。
- 记录 STUN、candidate、offer/answer、connectivity check、selected pair、DTLS、DataChannel Open、lane admission、detach 和终止的阶段与耗时。
- candidate 事实包含 `host/srflx/prflx/relay`、UDP/TCP、IPv4/IPv6、接口类别、去重结果和可得的 STUN endpoint；浏览器无法归因时写 `unknown`，且不记录 ICE/TURN credential。
- WebRTC selected pair 记录 candidate 类型、协议、地址族、RTT、存活时间和切换原因；结合 lane transport 将路径分类为 `direct`、`turn` 或 `application-relay`，selected-pair RTT 不得冒充 STUN RTT。
- 失败使用封闭阶段/原因码；provider 原始错误只作附属诊断。观察使用有界非阻塞队列，丢失汇总为 `observer_loss`。
- 先形成当前固定 STUN、默认 per-attempt socket 生命周期的真实基线，不预先承诺成功率数字。

### 2. 验证 Pion 的稳定 socket 与外部端口能力

- 针对固定 Pion 版本证明同一 `net.PacketConn` 能覆盖 host/srflx gathering、ICE checks、DataChannel 数据和 fresh replacement attempt。
- 证明 PCP、NAT-PMP、UPnP 返回的实际外部 IP **和端口**可发布为本地 candidate，并由同一 socket 收包；不得只改写 IP、假定内外端口相同或发布未验证 candidate。
- 明确映射晚到时能否注入当前 attempt；未证明前只允许其进入下一 fresh attempt。
- 若公开 API 不能表达这些语义，在 `transport/webrtc` 边界升级或补齐适配。本验证只阻塞第 4～5 阶段。

### 3. 建立受控 STUN 池与 attempt profile

- `ICEEndpointPool` 分离候选目录与受信可用池；来源仅限构建、部署和本地配置，relay 信令、分享链接和远程响应不得注入 STUN。
- 候选目录可收录数百个 endpoint，但收录不等于启用；未审核端点不进入官方默认 profile，只能由本地配置显式启用。
- endpoint 使用稳定 ID，并标记地域、故障域、网络提供商、端口、地址族、信任层级和部署优先级；未知事实不得猜测。
- `EndpointSelector` 是 `AttemptPlanner` 内的纯策略：过滤不兼容、不受信或处于失败冷却期的端点，优先选择地域和故障域不同的两个端点；无运行事实时按信任层级、部署优先级和网络代际种子加权轮换。
- 可归因的 `icecandidateerror` 按 endpoint 记录；srflx 产出和 candidate 到达耗时按 profile 记录，浏览器无法归因时不得猜测 endpoint。事实仅在内存中按 `NetworkGenerationID` 复用，网络变化即清空。
- 首次 `AttemptICEProfile` 最多激活两个 endpoint；备用 profile 从未使用的故障域选择另外两个。每侧每个 recovery wave 最多触达四个 STUN，浏览器和 Go 可使用不同 profile。
- 不在应用启动时解析或探测整个目录；同一 profile 由 ICE 并行 gathering 和 Trickle，不等待完整 gathering，也不创建探测专用 PeerConnection。
- 默认部署至少提供面向中国大陆网络的受控 STUN-only endpoint，优先 UDP 3478；UDP 443 仅在独立 listener 不冲突且确实部署时进入池。STUN 使用独立 listener、健康状态、速率限制和指标，不启用 TURN allocation。
- STUN 故障不影响应用 relay；`relay-only` 不加载、解析或访问 STUN endpoint。

### 4. 稳定所有 Go 端点的 ICE socket

- 用进程级 `NativePeerConnectivity` 统一向 Go 发送端和 Go 接收端提供 Pion `API + SettingEngine + ICE UDP mux`，取代两类 factory 直接调用默认 `NewPeerConnection`。
- 同一网络代际复用 UDP4/UDP6 socket，使 discovery、ICE checks、attempt 和已接纳 lane 共享 NAT 映射。
- 保留有效 LAN、mDNS、global-unicast IPv6 与接口路径；按地址族、接口类别和 candidate 类型限制噪声。
- 默认路由、可用接口或地址、VPN 与休眠恢复变化经去抖后创建新代际；旧代际在引用归零后释放，candidate、映射和观察事实不得跨代复用。
- 防火墙规则属于安装/平台层；连接模块不提权、不改全局策略，权限不可得时继续 relay。

### 5. 建立 Go 端点公网可达路径

- 发布当前路由可用的 global-unicast IPv6；tentative、deprecated、过期或已换代地址不得继续使用。
- 任意 Go 端点只有在内容 activation、尚未直连且普通 ICE 已获得短暂先行机会时，才后台有界尝试 PCP、NAT-PMP 和受控 UPnP；直连成功立即取消未完成工作。浏览器端不模拟原生端口映射能力。
- 多种映射协议由 `ReachabilityAuthority` 统一竞争，只保留一个租约并撤销其余结果。映射绑定 socket 与网络代际，只发布设备实际返回且公网可路由的 IP/端口；追加 candidate，不替换 LAN、IPv6 或 STUN candidate。
- 映射晚到默认供下一 fresh attempt 使用；只有第 2 阶段证明安全时才加入当前 attempt。最终可达性仍由认证 ICE check 裁决。
- 映射租约只由 active attempt 或已接纳 lane 的可达需求持有；最后一个需求结束后在有界 grace 内停止刷新并撤销。网络变化时重建，崩溃依靠短 TTL 收敛。

### 6. 有界恢复

- 首次 attempt 使用主 profile、单一 PeerConnection、Trickle ICE 和现有 phase budget，不等待 gathering 完成。
- `attempt-transient` 失败后以同一稳定 socket 创建 fresh PeerConnection；备用 profile 可用时至多旋转一次，任何时刻只有一个 PeerConnection 在尝试。
- Go 接收端 `PeerSet` 统一创建 fresh Offer；Go 发送端只回答当前 attempt。lane detach 或任一 Go 端网络代际变化由该所有者触发恢复，不引入双向 Offer 或第二套 supervisor。
- direct 失效时健康 relay 持续承载内容。仅网络变化、lane detach 或一次有预算的延迟恢复可重新触发；不做永久周期打洞。
- 网络变化使用新 socket 代际；lane admission 转移所有权后及时释放失败 attempt 与无用 candidate。
- 后台尝试不得让传输进度归零或状态反复闪烁；只有已接纳 lane 的 selected pair 证明后才显示“已直连”。

### 7. 证据驱动的后续研究

- 端口预测、birthday attack、ICE-TCP 和 WebTransport 不在本轮直连实现范围。
- 本计划不引入 TURN。只有使用证据表明现有应用 relay 存在明确的可用性或性能缺口时，才单独评估 TURN/TCP/TLS 与 Go proxy dialer；任何 TURN 路径都不得标记为 direct。

## 验证与收敛

- 自动化测试验证失败作用域、profile 选择、socket/attempt 所有权、候选预算、代际隔离、实际外部端口、映射刷新与撤销、crash TTL、relay 降级和资源释放；Go↔Go 覆盖发送端映射、接收端映射、双方映射和双方不可映射四种拓扑，网关行为使用确定性 fake。
- Pion spike 验证实际外部端口、收包路径和映射晚到语义；单机确定性拓扑证明机制正确，真实网络仅在使用中积累证据，不是交付前置。
- 每阶段运行聚焦 gate；全部代码完成后运行 `make ci-parallel`。

## 明确不做

- 不让用户手工选择 STUN、调整 timeout 或理解 NAT 类型。
- 不在出链接、纯浏览或应用启动时提前进行 ICE、STUN、端口映射或防火墙提权。
- 不接受 relay 或能力链接动态下发 STUN 地址；本阶段不做远程配置签名系统。
- 不创建并行或探测专用 PeerConnection，不做无限重试、无界 socket、候选洪泛或端口扫描。
- 不为 NAT 类型增加 Offer/Answer 角色反转或 glare 协商。
- 本轮不引入 TURN；WebSocket relay、端口映射成功或同机转发不得标记为 direct。

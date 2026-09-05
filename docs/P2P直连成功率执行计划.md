# P2P 直连成功率执行计划

状态：待批准
范围：无人工端口映射时，尽可能提高浏览器接收端与 Go 发送端、Go 接收端与 Go 发送端的 WebRTC 直连率。提高 STUN 可用性，特别是中国大陆。

## 当前基线

- 浏览器内容 activation 已让 relay 立即传输并在后台有限重试 P2P；纯目录浏览不启动 ICE。
- 两端使用单一 `PeerConnection + 固定 STUN`，支持 Trickle ICE；Go CLI 目前只启动一次 attempt，大文件或大小未知时 relay 最多等待 8 秒。
- 接收端固定创建 Offer/DataChannel，发送端固定 Answer；双方均为 Full ICE agent，都会收集 candidate、发送 connectivity check。固定协商角色不等于发送端被动打洞。
- 本计划沿用浏览器恢复框架并补齐 Go 接收端恢复，不新建第二套 retry、detach、identity 或 lane admission 所有者。

## 目标与边界

- `direct` 只表示未经过 TURN 或应用 relay 的已认证 candidate pair。
- 出链接、纯浏览和 relay 首字节不得等待探测、端口映射或 P2P。
- `auto` 模式采用直连 + 多个已配置且可用的中转并行，由现有内容调度器分配不同内容块；直连成功不退出中转，公共中转限速限流后续单独实现。
- 普通用户不选择 STUN、不理解 NAT，也不手工开放端口；`relay-only` 保持零 ICE、零 STUN 和零端口映射。
- 不凭 CGNAT、自动映射失败或 UDP 不通预判直连失败；由有界 ICE 检查裁决，已验证支持的 TCP 路径也参与尝试。预算耗尽后继续 relay。
- NAT、provider 和路径成本留在 `connectivity`；`core` 接收认证后的 `FrameChannel` 与传输无关的路径类别，不依赖具体 provider。
- 不根据预判 NAT 类型切换 Offerer；角色反转不增加可用 candidate pair，只有真实网络证据证明存在稳定增益时才另行评估。
- “已直连”依据已接纳 WebRTC lane 的 selected pair；“中转传输”和“两者并行”依据近期实际内容流量，不能仅凭连接已建立显示。candidate 或端口映射成功只进入诊断。
- 连接流程不得临时提权或阻塞用户操作；防火墙权限不可得时记录原因并继续 relay。
- 功能实现与发布不以公网服务器、受控 STUN 部署或真实网络样本为前置；缺少样本时只声明机制完成，直连率改善仍待验证，不宣称提升或数字。

## 目标架构

现有 P2P recovery supervisor 与 PeerConnection factories 重构为一个连接所有者；会话代际恢复仍由接收端 supervisor 负责：

```text
PeerConnectivity
├── ICEEndpointPool        STUN 候选目录、信任准入与有界 profile
├── SocketAuthority        Go 进程内按网络代际拥有 socket 与保活交接
├── ReachabilityAuthority  公网 IPv6、端口映射与 IPv6 pinhole 租约
├── AttemptPlanner         自动选优并生成有界 AttemptICEProfile
├── PeerSet                唯一拥有 attempt、替换、接纳与释放
└── PathObserver           selected pair、失败阶段与恢复事实
```

- `PeerSet` 吸收现有顺序恢复逻辑，按 `ProtocolSessionID + PeerPathID` 独立管理恢复；不同接收者可并行，共享 socket 不共享重试预算或取消状态，不得与另一套 P2P 恢复循环并存。进程级对 attempt、socket、STUN 与映射另设容量和速率上限，跨接收者公平分配，网络换代不重置总预算。
- 每个 Attempt 使用新的 `PeerConnection` 和 `AttemptID`，但同一网络代际内同一 peer path 复用稳定 socket；首版不在失败的 PeerConnection 上叠加原地 ICE restart。
- 本轮可只建立一条直连，但模型与调度须允许同一接收者未来多个 `PeerPath`/lane 并存；尝试串行限制仅作用于同一 peer path。
- `SocketAuthority` 的生命周期属于进程级网络代际，通过引用计数覆盖 attempt、已接纳 lane 和映射租约；映射租约不得自行维持无需求的代际。
- `transport/webrtc` 只适配单个 PeerConnection/DataChannel，不拥有重试、映射或路径策略。

## 实施顺序

### 0. 统一连接策略、失败作用域与时间预算

- Go 接收端以需求驱动的 `PeerSet` 替换一次性 attempt，与浏览器统一恢复策略；`auto` 在内容需求激活时立即接纳 relay，移除大小判定和 8 秒等待。
- `auto` 在接收者通过认证进入文件浏览后，每个 session 至多做一次有界 ICE 预热，复用 `PeerSet` 和总预算，不阻塞浏览、不做端口映射。内容激活接管当前 attempt 或已接纳 lane；无内容需求时，浏览结束即取消预热并释放 lane，成功 lane 仅有界保留，纯浏览不重试。
- 补齐发送端多 relay 配置与并行注册、接收端逐 lane 认证接纳及独立重连/释放，各 lane 加入同一 `ProtocolSession` 的内容调度；额外 relay 不阻塞首条可用路径。由 `connectivity` 提供可信路径类别，替换“追加 lane 即 direct”的假设；内容准入统一覆盖现有与后续 lane，重连不得绕过 `p2p-only`。
- 将 peer failure 明确为 `attempt-transient`、`path-terminal`、`session-terminal`。ICE/STUN/超时等可恢复网络失败只结束当前 attempt；协议或策略拒绝终止 peer path；认证与会话不变量失败终止 session 并停止恢复。
- 沿用浏览器会话代际恢复，在 Go 接收端补齐：全部 lane 因网络丢失时退役旧 session，在恢复预算内重新握手、获取 lease，并由下载任务保留仍有效的进度；不复活旧 session 权限，不因换代重置总预算。`PeerSet` 只负责代际内的 P2P 恢复，`p2p-only` 仍受当前 wave 上限约束。
- `OPERATION_ERROR` 对当前 negotiation identity 始终终态；typed reason 决定是否扩大到 path 或 session。`attempt-transient` 以新的 `AttemptID` 恢复，不得用通用 `Retryable` 重跑同一 operation。
- Go 与浏览器使用同一份封闭 reason-to-scope 向量；未知 reason 只停止 peer path，错误文本或通用 `Retryable` 不得产生 retry 或 session authority。
- 当前两端以 15 秒覆盖协商到 DataChannel Open。由现有 attempt 所有者分别管理信令准备、ICE 检查、DTLS/DataChannel 建立与 lane admission 预算；保留 attempt、wave 总上限，将 session 累计总上限改为可补充的单位时间尝试次数与耗时预算。两端统一策略，阶段推进不重置 attempt/wave 预算。
- 同步重构现有 attempt 身份记录与重放防护，采用可推进的退役边界和有界保留；回收记录后旧 attempt 仍不可接纳，正常重试不得因累计身份数耗尽而终止 session。
- ICE 检查开始后按 [RFC 8863](https://www.rfc-editor.org/rfc/rfc8863.html)保留等待晚到候选和 `prflx` 的机会；39.5 秒是默认重传参数下的参考值，不统一套用所有阶段。验证并协调 provider 内部失败时机、ICE 等待窗口与总预算，避免任一层提前结束；首次连接等待与已连接后的断线检测分开配置。平台缺少里程碑时明确记录预算归属，不伪造阶段事实。

### 1. 补齐直连事实

- 扩展现有 attempt observation 与 trace，以 `ProtocolSessionID + PeerPathID + AttemptID + NetworkGenerationID + ICEProfileID + side` 重建一次尝试；不新增平行诊断管线。
- 记录 STUN、candidate、offer/answer、connectivity check、selected pair、DTLS、DataChannel Open、lane admission、detach 和终止的阶段与耗时。
- candidate 事实包含 `host/srflx/prflx/relay`、UDP/TCP、IPv4/IPv6、接口类别、去重/裁剪原因和可得的 STUN endpoint；浏览器无法归因时写 `unknown`，且不记录 ICE/TURN credential。
- WebRTC selected pair 记录 candidate 类型、协议、地址族、RTT、存活时间和切换原因；结合 lane transport 将路径分类为 `direct`、`turn` 或 `application-relay`，selected-pair RTT 不得冒充 STUN RTT。
- 失败使用封闭阶段/原因码；provider 原始错误只作附属诊断。观察使用有界非阻塞队列，丢失汇总为 `observer_loss`。
- 先形成当前固定 STUN、默认 per-attempt socket 生命周期的本地可重复基线，公网基线随使用补充；按下载记录首次可用直连耗时、直连有效内容字节占比和回退停顿，不预先承诺成功率数字。

### 2. 验证 Pion 的 socket、外部端口与 ICE-TCP 能力

- 针对固定 Pion 版本证明同一 `net.PacketConn` 能覆盖 host/srflx gathering、ICE checks、DataChannel 数据和 fresh replacement attempt；补测 Go↔Go 双端端口复用时，同一远端 IP/端口的多 session/PeerPath 并存、替换及关闭隔离。
- 证明 PCP、NAT-PMP、UPnP 返回的实际外部 IP **和端口**可发布为本地 candidate，并由同一 socket 收包；不得只改写 IP、假定内外端口相同或发布未验证 candidate。
- 明确映射晚到时能否注入当前 attempt；未证明前只允许其进入下一 fresh attempt。
- 验证 fresh attempt 间隙的 srflx 保活交接，以及重复接管不会产生并行心跳；socket 存活不等于 NAT 映射存活。
- 执行 ICE-TCP spike：Go passive listener 与浏览器 active、Go↔Go，覆盖 IPv4/IPv6、UDP 不通和实际外部 TCP 端口。按目标浏览器记录互通能力，TCP 结果不阻塞 UDP 主线。
- 若公开 API 不能表达这些语义，在 `transport/webrtc` 边界升级或补齐适配；验证仅阻塞依赖该能力的后续步骤。

### 3. 建立受控 STUN 池与候选策略

- `ICEEndpointPool` 分离候选目录与受信可用池；来源仅限构建、部署和本地配置，relay 信令、分享链接和远程响应不得注入 STUN。
- 候选目录可收录数百个 endpoint，但收录不等于启用；未审核端点不进入官方默认 profile，只能由本地配置显式启用。
- endpoint 使用稳定 ID，并标记地域、故障域、网络提供商、端口、地址族、信任层级和部署优先级；未知事实不得猜测。
- `EndpointSelector` 是 `AttemptPlanner` 内的纯策略：过滤不兼容、不受信或处于失败冷却期的端点，优先选择地域和故障域不同的两个端点；无运行事实时按信任层级、部署优先级和网络代际种子加权轮换。
- 可归因的 `icecandidateerror` 按 endpoint 记录；srflx 产出和 candidate 到达耗时按 profile 记录，浏览器无法归因时不得猜测 endpoint。事实仅在内存中按 `NetworkGenerationID` 复用，网络变化即清空。
- 首次 `AttemptICEProfile` 最多激活两个 endpoint；备用 profile 从未使用的故障域选择另外两个。每侧每个 recovery wave 最多触达四个 STUN，浏览器和 Go 可使用不同 profile。
- 不在应用启动时解析或探测整个目录；同一 profile 由 ICE 并行 gathering 和 Trickle，不等待完整 gathering，也不创建探测专用 PeerConnection。
- 将受控 STUN 服务集成到 `wsrelay`，与应用中转共用一个二进制、同一进程，移除独立 `wsstun` 命令并统一部署产物；面向中国大陆网络的公网节点在资源具备后部署并加入默认池，优先 UDP 3478；UDP 443 仅在独立 listener 不冲突且确实部署时进入池。STUN 使用独立 listener、健康状态、速率限制和指标，不启用 TURN allocation。
- STUN 故障不影响应用 relay；`relay-only` 不加载、解析或访问 STUN endpoint。STUN 的 UDP 443 listener 只改善 STUN 可达性，不代表对端 candidate 使用 443。
- 两端分离本地候选选择预算与远端信令硬上限；本地超额按路径去重、裁剪，不终止整个 attempt。Trickle 发送前为 LAN、IPv4/IPv6、srflx 与晚到映射/TCP 候选预留名额，不等待完整 gathering；已发送候选仍计入总额，远端超限仍按协议处理。
- 按 [RFC 8421](https://www.rfc-editor.org/info/rfc8421/)通过 ICE candidate priority 交错安排地址族与接口；不让坏 IPv6 或单一接口耗尽检查机会。Go 在 ICE 层配置，浏览器保留原生调度；无可靠接口事实时不猜测或一刀切过滤。

### 4. 稳定所有 Go 端点的 ICE socket

- 用进程级 `NativePeerConnectivity` 统一向 Go 发送端和 Go 接收端提供 Pion `API + SettingEngine + ICE UDP mux`，取代两类 factory 直接调用默认 `NewPeerConnection`。
- 同一网络代际内按 peer path 稳定复用 UDP4/UDP6 socket，让 discovery、ICE checks、attempt 和已接纳 lane 使用同一本地端点；跨路径共享须经第 2 阶段证明收包隔离，否则从有界 socket 池分配独立端点。不假定面向不同目的地址时 NAT 映射相同。
- `SocketAuthority` 在有预算的待执行 attempt 间隙接管必要的 srflx 保活；active attempt/已接纳 lane 使用 ICE 自身机制，交接不重复刷新，也不合并不同 peer 的 consent 检查。没有需求就停止，不维持永久 STUN 心跳。
- 保留有效 LAN、mDNS、global-unicast IPv6 与接口路径；按地址族、接口类别和 candidate 类型限制噪声。
- 默认路由、可用接口或地址、VPN 与休眠恢复变化经去抖后创建新代际；旧代际在引用归零后释放，candidate、映射和观察事实不得跨代复用。
- 安装/平台层实现官方桌面端一次、可跳过的防火墙授权引导（安装或首次设置），配置所需应用规则；日常连接不提权、不改全局策略，拒绝后记录原因并继续 relay。

### 5. 建立 Go 端点公网可达路径

- 先在 Go/浏览器信令契约中补齐跨 attempt 的路径需求、撤销、映射就绪与网络换代通知，由同一 `PeerSet` 处理。控制绑定 session/path、发送侧网络代际及单调序号，需求有界到期并表达已排定 attempt 的持有期限；去重、拒绝过期通知，不复用已终态的 negotiation operation。
- 发布当前路由可用的 global-unicast IPv6；tentative、deprecated、过期或已换代地址不得继续使用。公网 IPv6 不等于入站可达；网关支持时以 PCP 或 UPnP IGD v2 `WANIPv6FirewallControl` 创建按需 IPv6 pinhole，与映射统一管理续租和撤销。
- Go 端点仅为已有内容 activation 且尚未直连的 peer path，在普通 ICE 获得短暂先行机会后后台有界尝试 PCP、NAT-PMP 和受控 UPnP；直连成功仅取消该路径未完成的映射需求，其他路径需求及已接纳 lane 所需租约继续保留。浏览器端不模拟原生端口映射能力。
- `ReachabilityAuthority` 按网络代际、出口、本地端点、传输协议及可达范围管理租约；仅合并提供同等可达性的重复结果。IPv4 映射、IPv6 pinhole、不同出口及 TCP/UDP 租约可并存，撤销重复结果不得破坏保留路径。
- 只发布设备实际返回且公网可路由的 IP/端口；追加 candidate，不替换 LAN、IPv6 或 STUN candidate。
- 映射晚到默认由 `PeerSet` 使用每个 wave 预留的一次延迟恢复机会创建 fresh attempt，计入本轮及单位时间预算；无内容需求、已直连时不触发，预算不足则等待后续恢复机会。只有第 2 阶段证明安全时才加入当前 attempt，最终可达性仍由认证 ICE check 裁决。
- 映射租约只由预算内已排定的待执行 attempt、active attempt 或已接纳 lane 的可达需求持有；最后一个需求结束后在有界 grace 内停止刷新并撤销。网络变化时重建，崩溃依靠短 TTL 收敛。

### 6. 有界恢复

- 首次 attempt 使用主 profile、单一 PeerConnection、Trickle ICE 和第 0 阶段的分阶段预算，不等待 gathering 完成。
- 有内容需求时，`attempt-transient` 失败后以同一稳定 socket 创建 fresh PeerConnection；备用 profile 可用时每个 wave 内至多旋转一次，同一 peer path 内任何时刻只有一个 PeerConnection 在尝试。
- 接收端（浏览器或 Go）的 `PeerSet` 统一创建 fresh Offer；Go 发送端只回答当前 attempt，并经认证会话信令通知对应接收端映射就绪或网络代际变化。lane detach 同样由接收端触发恢复，不引入双向 Offer 或第二套 P2P 恢复循环。
- `auto` 下 direct 失效时健康 relay 持续承载内容。可恢复失败后，只要仍有内容需求且尚未直连，就允许低频退避开启新 wave；网络变化、lane detach 和映射就绪也由同一 `PeerSet` 在单位时间预算内触发。
- `p2p-only` 禁止 relay 内容传输；首次连接或直连失效均在有界 wave 内尝试，恢复时保留有效进度，wave 耗尽或不可恢复的 path/session 失败后报错结束下载。网络失败退出时按输出模块能力保存进度：支持持久续传的提供续传入口，否则提示重新下载；不为统一续传新增整份临时副本。
- 已直连即停止后台尝试，无内容需求时仅允许单次浏览预热；暂停后恢复或新下载可在预算内重新激活，`path-terminal`/`session-terminal` 不自动重试。
- 网络变化使用新 socket 代际；lane admission 转移所有权后及时释放失败 attempt 与无用 candidate。
- 后台尝试不得让传输进度归零或状态反复闪烁；只有已接纳 lane 的 selected pair 证明后才显示“已直连”。

### 7. 扩展受限网络路径

- 第 2 阶段证明互通后，将 ICE-TCP 纳入现有 profile、`SocketAuthority` 与 `PeerSet`；在同一 PeerConnection 中保留 UDP 路径，并为 Go 提供公网或自动映射可达的 TCP passive listener。仅对已验证组合启用，不新建恢复所有者；TCP 443 不等于 HTTPS，也不保证通过严格代理。
- 基础映射完成后，补充标准 PCP server 发现和支持 PCP proxy 的多层 NAT 路径；仅访问本地配置或标准网络发现的服务器，采用代理返回的最外层公网端点与有效租期，不以第一层映射冒充公网可达。设备不支持时继续普通 ICE/relay。[RFC 7648](https://www.rfc-editor.org/info/rfc7648/)
- 端口预测、birthday attack 和 WebTransport 不在本轮范围。
- 本计划不引入 TURN。只有使用证据表明现有应用 relay 存在明确的可用性或性能缺口时，才单独评估 TURN/TCP/TLS 与 Go proxy dialer；任何 TURN 路径都不得标记为 direct。

## 验证与收敛

- 自动化测试聚焦失败/预算作用域、退避与预算补充、长期重试与身份回收后的重放拒绝、预热复用/释放、网络退出续传、需求结束取消、晚到 `prflx`、候选裁剪与地址族公平性、保活交接、代际隔离、租约续期/撤销与 crash TTL、relay 降级和资源释放；Go↔Go 覆盖单侧、双侧映射及双方不可映射，补充 IPv6 pinhole 与 PCP proxy。定时和网关行为使用 fake，避免真实等待扩大本地测试耗时。
- Pion spike 验证实际外部端口、收包路径、映射晚到、attempt 间隙保活与 ICE-TCP 互通；确定性拓扑证明机制正确。资源允许时安排国内跨网、Windows 默认环境和局域网的少量真实网络对照，缺少样本不阻塞交付。
- 每阶段运行聚焦 gate；全部代码完成后运行 `make ci-parallel`。

## 明确不做

- 不让用户手工选择 STUN、调整 timeout 或理解 NAT 类型。
- 不在出链接或应用启动时提前进行 ICE、STUN 或端口映射；纯浏览仅允许上述预热，日常连接不触发防火墙提权。
- 不接受 relay 或能力链接动态下发 STUN 地址；本阶段不做远程配置签名系统。
- 不为同一 peer path 创建并行尝试或探测专用 PeerConnection，不做无需求或无速率限制的重试、无界 socket、候选洪泛或端口扫描。
- 不为 NAT 类型增加 Offer/Answer 角色反转或 glare 协商。
- 本轮不引入 TURN；WebSocket relay、端口映射成功或同机转发不得标记为 direct。

# P2P 直连成功率执行计划

状态：待批准  
范围：无人工端口映射时，尽可能提高浏览器接收端与 Go 发送端的 WebRTC 直连率。提高 TURN 可用性，特别是中国大陆。

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

## 目标架构

现有 recovery supervisor 与 PeerConnection factories 重构为一个连接所有者：

```text
PeerConnectivity
├── EndpointDiscovery      多 STUN、IPv4/IPv6 与映射事实
├── SocketAuthority        Go 侧 UDP socket 与网络代际
├── ReachabilityAuthority  公网 IPv6 与自动端口映射租约
├── AttemptPlanner         由证据选择 attempt profile
├── PeerSet                唯一拥有 attempt、获胜、替换与释放
└── PathObserver           selected pair、失败阶段与恢复事实
```

- `PeerSet` 吸收现有顺序恢复逻辑；不得与另一套 supervisor 并存。
- 每个 Attempt 使用新的 `PeerConnection` 和 `AttemptID`，但同一网络代际复用稳定 socket；首版不在失败的 PeerConnection 上叠加原地 ICE restart。
- `transport/webrtc` 只适配单个 PeerConnection/DataChannel，不拥有重试、映射或路径策略。

## 实施顺序

### 0. 证明稳定 socket 与真实外部端口

- 用一个 `net.PacketConn` 和 universal UDP mux 证明 host/srflx gathering、ICE checks、DataChannel 数据及 replacement attempt 均使用同一 socket。
- 证明 PCP、NAT-PMP、UPnP 返回的实际外部 IP **和端口**可作为本地 candidate 发布并由同一 socket 收包；不得只改写 IP、假定内外端口相同或发布未验证 candidate。
- 若固定 Pion 版本的公开 API 不能表达上述语义，先在 `transport/webrtc` 边界升级或补齐适配，再进入生产实现。

### 1. 补齐直连事实

- 扩展现有 attempt observation 与 trace，以 `ProtocolSessionID + PeerPathID + AttemptID + side` 重建一次尝试；不新增平行诊断管线。
- 记录 STUN、candidate、offer/answer、connectivity check、selected pair、DTLS、DataChannel Open、lane admission、detach 和终止的阶段与耗时。
- candidate 事实包含 `host/srflx/prflx/relay`、UDP/TCP、IPv4/IPv6、接口类别、STUN endpoint 和去重结果，不记录 ICE/TURN credential。
- WebRTC selected pair 记录 candidate 类型、协议、地址族、RTT、存活时间和切换原因；结合 lane transport 将路径分类为 `direct`、`turn` 或 `application-relay`。
- 失败使用封闭阶段/原因码；provider 原始错误只作附属诊断。观察使用有界非阻塞队列，丢失汇总为 `observer_loss`。
- 先形成当前固定 STUN、默认 per-attempt socket 生命周期的真实基线，不预先承诺成功率数字。

### 2. 稳定 Go 侧 ICE socket

- 用 Pion `API + SettingEngine + universal UDP mux` 取代 factory 直接调用默认 `NewPeerConnection`。
- 同一网络代际内复用 UDP4/UDP6 socket，使 discovery、ICE checks 和 replacement attempts 共享 NAT 映射。
- 保留有效 LAN、mDNS、global-unicast IPv6 与接口路径；按地址族、接口类别和 candidate 类型预留配额，避免噪声先耗尽 64 个 candidate。
- 网络变化创建新代际；旧代际在所属 attempt 结束后释放，candidate、映射和观察不得跨代复用。
- 防火墙规则属于安装/平台适配层。连接模块不提权、不修改全局策略；缺少许可只降低 direct，不影响 relay。

### 3. 建立发送端公网可达路径

- 发布当前路由可用的 global-unicast IPv6；tentative、deprecated、过期或已换代地址不得继续使用。
- 只有内容 activation 存在且尚无已证明的公网可达路径时，后台尝试 PCP、NAT-PMP 和受控 UPnP IGD；普通 ICE 已成功时取消未完成的映射工作。
- 映射绑定稳定 socket 与网络代际，使用设备实际返回的外部 IP/端口追加 candidate，不替换 LAN、IPv6 或 STUN candidate。
- 只发布公网可路由结果；最终可达性仍由认证 ICE connectivity check 裁决。
- 租约到期前刷新，网关重启、地址变化和网络切换时重建；正常退出撤销，崩溃依靠短 TTL 收敛。

### 4. 部署可用双 STUN

- 原生 relay 二进制同时提供应用 relay 与标准 STUN，但两者使用独立 listener 和健康状态。
- 默认客户端在构建配置中固定两个 endpoint：韩国域名服务器提供应用 relay 与 STUN，国内 IP 服务器只提供 UDP STUN；不要求国内服务器具备域名、TLS 或 WSS。
- endpoint 使用实际可用的地址族；本轮不要求多运营商、多地域或 IPv6 STUN 部署。优先使用 UDP 3478，UDP 443 仅在不冲突时启用。
- 自部署版本可在本地部署配置中替换 endpoint；relay 信令、分享链接和远程响应不得动态注入或覆盖 STUN 地址。
- 普通 profile 先使用主 STUN；主端点未及时产出有效 srflx 时启用备用。只有困难网络诊断需要判断映射是否随目标变化时才并行查询不同端点。
- STUN 故障不影响应用 relay；`relay-only` 不解析或访问 STUN endpoint。

### 5. 自适应恢复与困难网络

- 第一轮保留现有 Trickle ICE、单一 PeerConnection 和现有 phase budget，不等待 gathering 完成。
- 初次失败后创建新的 punch generation：使用新的 Attempt/PeerConnection、现有认证信令和同一稳定 socket，而不是原地修补失败对象。
- 默认一次只有一个 attempt。只有端点映射证据表明标准 ICE 可能受益时，`PeerSet` 才启动 2～3 个有界并行 socket/STUN profile。
- 第一条通过 lane admission 的 DataChannel 获胜；立即关闭失败、等价或落后的 attempt，并释放 socket、candidate 和观察资源。
- direct 失效时健康 relay 继续承载内容，同时在既有 wave/session budget 内后台恢复；网络变化使用新 socket 代际。

### 6. 后续实验

- 端口预测、birthday attack、ICE-TCP、WebTransport、TURN 和更多 STUN 地域不在本轮实现或验证范围。
- 后续只有具备相应网络与服务器条件时才单独立项；不得把这些实验变成本计划的交付前置。

## 验证与收敛

- 自动化测试验证 socket/attempt 所有权、候选预算、代际隔离、映射租约、失败降级和资源释放；网关行为使用确定性 fake，不搭建复杂 NAT 实验室。
- 真实路由器端口映射只做条件允许时的人工 smoke，不是 Agent 交付前置；自动化测试仍须验证实际外部端口、刷新、撤销和 crash TTL 语义。
- 每阶段运行聚焦 gate；全部代码完成后运行 `make ci-parallel`。

## 明确不做

- 不让用户手工选择 STUN、调整 timeout 或理解 NAT 类型。
- 不在出链接、纯浏览或应用启动时提前进行 ICE、STUN、端口映射或防火墙提权。
- 不接受 relay 或能力链接动态下发 STUN 地址；本阶段不做配置签名系统。
- 不用无限重试、无界 socket、候选洪泛或端口扫描换取偶然成功。
- 不把 TURN、WebSocket relay、端口映射成功或同机转发标记为 direct。

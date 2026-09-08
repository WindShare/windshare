# Pion fork 后续处理

更新：2026-09-08。基线：WindShare `bd1df255`，ICE `v4.4.2`，WebRTC `v4.2.20`。

目标是先消除本地正确性和重连延迟问题，再把通用修复与能力接口逐项反哺，减少 fork 的长期维护成本。第 1–3 项本地修复已完成，其上游提案及其余项目仍待处理；PR 标题是建议，没有创建上游 issue 或 PR。

## 当前范围

自维护差异集中在 [ICE 补丁](../third_party/pion/patches/ice.patch)和 [WebRTC 补丁](../third_party/pion/patches/webrtc.patch)。不要把升级带入的上游源码变化一起提交。

本次依赖同步已经让上游接管 STUN 响应事务校验，并接入 `GetXORMappedAddrContext` 的 gathering 取消能力；这两项不再单独反哺。指定本地端点的接口和空闲期间强制刷新仍属于本地扩展。

## 处理顺序与你需要做的事

第 1–3 项本地修复已完成，接下来按 4–5 提交通用 bugfix，最后逐项推进 6–8 的能力接口。上游 review 等待期间可以继续下一项本地工作，无须暂停整个项目。

| 顺序 | 工作 | 你需要做什么 | 去向 |
| --- | --- | --- | --- |
| 1 | 空闲 STUN 刷新取消与 socket 交接（本地已完成） | 后续确认刷新 API 的提案内容 | WindShare；成熟后向 `pion/ice` 提 API PR |
| 2 | 修复网卡排序覆盖 TCP 方向偏好（本地已完成） | 后续参与优先级扩展接口讨论 | WindShare；按需向 `pion/ice` 提 issue |
| 3 | 映射候选遵守候选类型限制（本地已完成） | 随端点映射提案一起解释配置语义 | WindShare；并入第 8 项 |
| 4 | UDP mux 初始化竞态 | 用你的 GitHub 账号提交独立修复，处理 review | `pion/ice` PR |
| 5 | TCP srflx 类型和地址保真 | 先提交 ICE PR；其可用版本发布后，再提交 WebRTC PR | `pion/ice` → `pion/webrtc` |
| 6 | 暴露 srflx mux，补齐多本地端点选择 | 先讨论 API，再拆分提交 | `pion/webrtc`、`pion/ice` |
| 7 | 独立配置初始 ICE 检查窗口 | 确认超时语义提案，先 ICE、后 WebRTC | `pion/ice` → `pion/webrtc` |
| 8 | 通用外部 IP＋端口映射 | 发 issue 讨论数据模型；获得方向反馈后提交实现 | `pion/ice` → `pion/webrtc` |

本地修复、最小复现、分支、测试和英文 PR 文案都可以由 Codex 准备。你主要负责 GitHub 账号操作、确认对外提交内容，以及涉及公共 API 取舍的维护者讨论；也可以另行明确授权 Codex 提交。普通 bugfix 可直接提 PR，接口扩展宜先发 issue。

## 1. 空闲 STUN 刷新取消与 socket 交接

**原问题。** 强制刷新没有接收 context，Claim 取消空闲任务后仍持有 authority 锁等待退出。STUN 无响应时，新连接可能等待剩余刷新超时（默认最长约 500ms），并阻塞同一 authority 上需要该锁的操作。

**本地已完成。** [强制刷新](../third_party/pion/ice/udp_mux_universal.go)将调用方 context 传入上游事务，包括阻塞写入的取消。[Claim](../connectivity/socketauthority/ownership.go)先预留独占权，再在锁外取消并等待空闲任务，交接前重新检查租约、关闭和退役状态。重复 StartIdle 在锁外等待并检查最新任务；关闭保留容量记账，直到 socket 与空闲任务结束。原 socket、保活期限和重试策略保持不变。

受控 socket 回归测试覆盖无响应 STUN、阻塞写取消后复用、重复领取与 StartIdle、等待期间关闭或退役，以及容量回收；通过通道协调时序，无长时间 sleep。刷新结果和交接开始/结束接入现有 native connectivity trace，沿用 session、path 和网络代际标识。

**上游处理。** 本地稳定后，向 `pion/ice` 提出支持 context 的显式刷新接口，讲清缓存、并发刷新和正在进行的 gathering 如何交互。authority 的锁与租约状态机留在 WindShare。建议标题：`Add cancellable explicit STUN mapped-address refresh`。

## 2. 候选优先级保留协议语义

**原问题。** 按 IP 分配完整 LocalPreference 会覆盖 TCP 方向偏好；收到远端 passive 候选后动态创建的 active TCP 又绕过了该策略，导致普通候选和动态候选评分不一致。

**本地已完成。** [产品策略](../transport/webrtc/provider/priority.go)只提供冻结的基址顺序；[ICE 评分入口](../third_party/pion/ice/provider_priority.go)在启动候选、构造检查表和发布之前组合一次最终分数。保留类型、UDP/TCP、TCP 方向及 TURN 自身偏好；每个协议偏好组先为各基址保留首次机会，重复端口、映射和未列出的基址使用余下独立分数。空间耗尽显式报告，禁止回绕到另一个方向。动态 active TCP 走同一入口。

回归覆盖同 IP active/passive、重复 active、多基址双栈 UDP/TCP 映射、迟到基址、非默认 TCP 类型偏移、配置快照、发布/统计一致性和分数边界，无长时间等待。最终优先级和 TCP 方向进入现有 native connectivity trace，沿用 session、path、attempt 标识。此修复不调整中转等待或重试期限；实际直连成功率收益仍需网络证据。[RFC 6544 §4.2](https://www.rfc-editor.org/rfc/rfc6544.html#section-4.2)中的方向排序是可调整的推荐值，并非不可配置的硬要求。

**上游处理。** 本地策略已修复，后续讨论上游真正需要暴露的扩展点。网卡顺序、IPv4/IPv6 选择策略留在产品层，不把整个 `ProviderConfig` 或 WindShare 的具体排序移入上游。建议 issue 标题：`Support application-defined interface preference while preserving ICE-TCP priorities`。

## 3. 映射候选遵守候选类型限制

**原问题。** [映射候选收集](../third_party/pion/ice/gather.go)在候选类型筛选循环之外无条件执行，导致 host-only、relay-only 等显式禁用 srflx 的配置仍会领取映射 socket 并发布 srflx 候选。这是本地扩展与 Pion 配置契约不一致。

**本地已完成。** 映射候选归入 srflx gathering 流程，只有启用该类型时才收集；继续按 UDP/TCP、IPv4/IPv6 网络配置筛选，并绑定已验证映射的真实 socket。没有 STUN URL 时也保留有效映射。

**用户体验边界。** WindShare 默认配置允许 srflx，正常分享继续尝试映射直连。这里的 ICE relay-only 指仅允许 TURN 候选，不代表“5 秒后中转先传”；中转传输期间仍应继续探索 P2P，不改变中转等待、重试或切换策略。这项属于底层配置正确性修复，不宣称提高默认直连成功率。

同一组双栈 UDP/TCP 映射覆盖默认配置、srflx-only、host-only、relay-only、host＋relay 及各网络限制；检查 socket 领取、发布、内部候选、统计和真实 base，禁用时不靠信令过滤。上游原版没有这条本地路径，不单独声称这是上游已有 bug，后续并入第 8 项。

## 4. 反哺 UDP mux 初始化竞态

**问题与现状。** 本地已经修复：先构造并发布完整 UniversalUDPMux，再启动读取 socket 的协程。否则预先排队的 STUN 包可能让读协程访问尚未初始化的嵌入对象。

**PR 范围。** 仅移植 [udp_mux.go](../third_party/pion/ice/udp_mux.go)、[udp_mux_universal.go](../third_party/pion/ice/udp_mux_universal.go)中与初始化顺序相关的修改，以及一个独立回归测试。以 [已有排队 STUN 测试](../connectivity/socketauthority/mux_initialization_test.go)为基础，使其直接在 `pion/ice` 中运行，不依赖 WindShare。先在未修复上游确认复现，再用 race 检测验证修复。

**你需要做。** 提交到 `pion/ice`；正文解释“socket 在构造前已有数据”这个触发条件，以及初始化完成后启动 reader 的原因。无需搭载任何 provider API。建议标题：`Fix UniversalUDPMux initialization race with queued STUN packets`。

## 5. 反哺 TCP srflx 类型与地址保真

**问题与现状。** 本地已补齐 TCP srflx 的 TCPType、候选解析和实际 socket 地址类型。上游提交应覆盖这条数据链，避免只修构造函数后仍在转换中丢失信息。

**第一份 PR：`pion/ice`。** 修改 [CandidateServerReflexive](../third_party/pion/ice/candidate_server_reflexive.go)和候选解析，保留 TCPType，并为 TCP 使用 TCP 地址。补充序列化往返测试、内部地址类型检查，覆盖 IPv4/IPv6 与既有 UDP 行为。建议标题：`Preserve TCP type and resolved address for server-reflexive candidates`。

**第二份 PR：`pion/webrtc`。** 修改 [ICECandidate.ToICE](../third_party/pion/webrtc/icecandidate.go)传递 srflx 的 TCPType，并补充转换测试。建议标题：`Preserve server-reflexive TCP type in ICECandidate conversion`。

**你需要做。** 先推进 ICE PR。WebRTC PR 依赖该修改进入可引用的 ICE 版本；若提前开 draft，明确标注依赖 PR，最终使用上游认可的版本，不提交指向 WindShare 本地目录的 replace。

## 6. 暴露 srflx mux 与本地端点选择

**需求。** STUN 探测和候选使用同一真实 socket；多网卡、双栈场景必须能明确选择本地端点。上游 ICE 已有 `WithUDPMuxSrflx`，本地 WebRTC 目前借助 `SetICEProviderConfig` 传递它。

**拆分。** 先向 `pion/webrtc` 提最小的 srflx mux 设置接口，直接连接 ICE 的既有能力。再向 `pion/ice` 讨论带 context 和本地端点的查询契约，连同地址族匹配处理；避免让查询某一接口的映射时实际使用另一个接口。支持这些基础接口后，WindShare 组合 socket 能力的代码继续留在自己的适配层。

**你需要做。** 先发接口提案，附单 socket 和多个本地端点两种使用例子。建议分别使用 `Expose server-reflexive UDP mux configuration` 和 `Support local-endpoint-scoped STUN gathering`。上游只需要协议与 socket 能力，不需要 WindShare 的 session、path、租约、重试模型。

## 7. 独立配置初始 ICE 检查窗口

**需求。** 初始连通性探索需要等待迟到候选或入站检查；连接建立后又需要及时发现失联。当前 [provider](../transport/webrtc/provider/connection.go)使用独立初始窗口，避免把两个阶段的超时一起拉长。

**上游处理。** 在现有 `initialCheckingTimeout()` 基础上讨论独立 AgentOption，后续再接入 WebRTC SettingEngine。明确默认值、显式禁用、调用方取消，以及与 disconnected/failed timeout 的关系。还应区分 PAC 的最短等待语义与应用的总体连接期限，不能仅凭设置一个超时就宣称完整实现 PAC。[RFC 8863 §4](https://www.rfc-editor.org/rfc/rfc8863.html#section-4)推荐的默认 39.5 秒应作为语义依据。

保留“短失联预算之后仍能通过迟到入站检查建连”的小规模测试，不把它改成实际等待 39.5 秒。你需要确认提案并处理 API 讨论。建议标题：`Configure initial ICE checking timeout independently of connected failure detection`。

## 8. 通用外部 IP＋端口映射

**需求。** 路由器分配的外部端口可能不同于内部端口。当前地址重写规则只表达 IP；[MappedEndpoint](../third_party/pion/ice/provider_config.go)表达本地端点到外部端点的对应关系，并绑定实际 socket。

**先讨论模型。** 提案应解释 UDP/TCP、本地与外部端点、candidate type、真实 base、网络限制和生命周期。结合现有地址重写机制决定是扩展端点模型还是增加独立映射能力，避免简单叠加产品专用参数。第 3 项的筛选修复是前置工作，第 5 项的 TCP 修复是完整 TCP 映射支持的依赖。

映射发现、路由器协议、租约续期、网络代际和“迟到映射需要新建尝试”的产品策略留在 WindShare。先拿本地端口转发及真实 payload 的证据说明机制；不要把本地测试包装成公网直连成功率证明。

**你需要做。** 向 `pion/ice` 发 issue，拿到模型方向反馈后再实现 PR，之后接 WebRTC 设置入口。建议标题：`Support externally allocated IP and port mappings bound to local ICE sockets`。上游若暂不接受，继续保留可复现的小补丁即可，无需阻塞产品开发。

## 每项如何收尾

- 动手提交前重新核对上游 main、相关 issue 和 PR；若已有修复，优先升级或协助现有 PR。将实际链接补到对应条目，并标记本地完成、已提交、已合并或已移除补丁。
- 本地修改 third_party 后，同步源码、patch 和 manifest，运行 `go run ./scripts/ci/_piondeps -reproduce`。修复时用对应的小测试定位，最终代码交接按仓库要求运行 `make ci-parallel`。上游 PR 运行上游自己的相关检查。
- PR 使用独立分支，每份只解决一个问题。正文写触发场景、失败原因、修复理由和实际执行的测试；不带其他依赖升级或 WindShare 配置。提交信息不添加任何 attribution trailers。
- 上游合并后，等修改进入项目决定采用的上游版本，再升级并删除对应补丁，更新 manifest 和复现结果。PR 合并本身不代表本地已经摆脱该补丁。

第 1 项已通过补丁重建、受影响 Go 包的短测试与 race 检测、`make check` 和最终 `make ci-parallel`（含 E2E trace 契约与全仓 gopls）。第 2 项已通过受影响包的短测试、race 检测、补丁重建和最终 `make ci-parallel`（含 E2E trace 契约与全仓 gopls）。第 3 项已通过受影响包的短测试、race 检测、补丁重建和最终 `make ci-parallel`（含映射 payload、浏览器契约与全仓 gopls）。三项上游提案尚未提交，其余条目仍为待办。

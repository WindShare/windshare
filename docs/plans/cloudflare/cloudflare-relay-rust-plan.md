# Cloudflare 原生中转与 Rust 统一计划

## 目的

WindShare 应允许每位用户将私人中转部署到自己的 Cloudflare 免费账户，并把它作为个人中转配置导入。

在当前协议中，中转并非仅在 P2P 失败后才建立：它先承载发送端注册、接收端加入、ProtocolSession 初始通道和 P2P 信令，再作为加密内容帧的后备传输 Lane。它不是标准 TURN，也不参与 ICE 候选中继，因此不会提高 WebRTC 本身的 NAT 穿透成功率；但它能在直连不可用时独立完成 WindShare 传输，所以当前设计不依赖 TURN。

长期设计还应消除分别维护 Go 与 Cloudflare 中转语义的成本。方案采用一个 Rust 中转语义内核和两个 Rust 平台适配器：

- Rust Cloudflare Worker/Durable Object 宿主，用于个人低流量部署。
- 原生 Rust 二进制程序，用于项目运营的中转、自托管和 Cloudflare Tunnel 部署。

这里的“纯 Rust”指中转业务源码、控制语义和两个适配器均由 Rust 实现；Cloudflare 仍按 Workers 的 Wasm/JavaScript 宿主运行。平台边界上的有限数据复制是可接受的，不把零复制作为交付前提。必要的宿主 shim 仅处理缺失绑定或输入边界，不引入第二套 TypeScript 控制语义。

## 设计原则

- **Cloudflare 纯属可选能力。** 官方构建默认配置正式中转与前端地址，localhost 仅用于开发；未配置个人中转时，创建分享、打开链接、P2P 协商和官方 relay 后备的体验必须保持不变。接收者不需要 Cloudflare 账户、额外安装或手动选择中转。
- **原生服务器是功能与性能基准。** Cloudflare 的单对象吞吐、休眠、Wasm、配额和队列限制不得进入原生服务器的默认值、热路径或依赖图。原生二进制程序不得依赖 Wrangler、JavaScript、Wasm 或 Cloudflare API。
- **统一决策，不统一 I/O 机制。** Rust 引擎统一认证、所有权、会话、恢复、停止和截止时间，并用小型显式状态机约束持久化结果、资源清理与响应确认的处理顺序；原生与 Cloudflare 适配器分别实现最适合平台的转发、排队、背压和并发。不能为了复用代码把原生数据面降到最低共同能力。
- **Cloudflare 载荷由适配器持有。** 已获许可的不透明数据帧可以在 Rust Worker 适配器中复制为有界 `Vec<u8>`；复制不构成协议语义，也不改变 E2EE 边界。普通数据帧仍不得进入 SQLite、结构化日志、无界应用队列或控制状态机；适配器解析固定头，通过共享内核检查许可与额度，并执行有界转发。零复制只作为后续优化方向。
- **默认流程零新增操作。** Rust 重写本身不构成用户功能。个人中转首次部署和导入，后续按需同步更新；日常分享不增加操作，也不要求用户选择技术方案。
- **主动退出即停止。** 发送端先停止本地服务，再使用 `StopAuthority` 有界清理各中转，不无限等待不可达候选。异常断开只保留有界恢复宽限，宽限结束后停止。STOP 在永久事实持久化、旧许可撤销、未提交队列丢弃和发布写入静止后确认；已提交数据仍可能到达，其占用独立结算。提交结果不确定时禁止恢复或重新发布。
- **保留现有并行能力。** 保留当前多中转候选和多 Lane 能力；私人 Cloudflare relay 是可选候选。Cloudflare 可按自身容量限制并发，不降低原生服务器能力，也不增加每次分享的选择。
- **持久化不进入载荷热路径。** SQLite 只保存有界生命周期状态、密封描述符和永久停止标记；不保存、排队或检查不透明内容帧，原生与 Cloudflare 数据帧转发都不执行数据库操作。
- **以行为和基本负载决定切换。** 当前 Go 中转尚未作为已部署服务运行，不建立迁移、双栈或旧数据兼容义务。Rust 版本先通过真实端到端流程、协议向量、关键状态迁移、正常休眠、下载中部署/重启续传和基本慢连接测试；通过后在同一变更中替换并删除 Go 服务端。性能测量用于发现明显退化，不把未运行过的 Go 实现当作线上基准。
- **Go 服务端是参考，不是规范。** 契约是线协议、共享向量和客户端可见语义，不是 Go 的结构、常量或流程。移植时遇到 Go 不合理或有更好方案之处，应大胆改进而不是逐行复刻，例如 challenge 改由连接按值持有、取消进程级账本。改动影响客户端可见行为时，在同一变更中同步更新 Go／浏览器客户端、共享向量与协议文档。
- **恢复复用现有续传。** 正常休眠保留连接和会话：运行时重建只隔离旧回调，不构成连接换代或会话退役，存活会话连同未结算额度经内核校验后恢复；部署、运行时重启或故障断连复用重连续传，允许短暂停顿和未确认数据重传。配额对账和远端清理可 best-effort，不持久化载荷或追求完全无感恢复。

### 用户可见结果

| 用户场景 | 预期变化 |
| --- | --- |
| 未配置个人中转 | 无变化；继续使用当前默认中转策略。 |
| 配置个人 Cloudflare 中转 | 首次部署和导入，后续按需同步更新；日常仍创建普通分享链接。 |
| 接收分享 | 无变化；打开链接后自动尝试 P2P 和已发布的中转候选。 |
| 普通服务器自托管 | 继续运行独立原生二进制程序；功能不减少，性能不受 Cloudflare 限制。 |
| 中转故障 | 自动使用允许集合内其余已发布候选；仅私人模式不连接官方中转，无可用私人候选时明确失败。普通分享可以展示当前采用的候选和后备原因。 |

因此，这不是面向所有用户的速度升级：对未启用者应是零变化；对启用者是信任边界和部署控制权升级，不承诺比项目中转更快。选择“仅私人中转”是中转配置阶段的一次性高级设置，会主动以可用性换取更窄的信任面；普通分享可以显示当前配置和自动决策结果，但不增加每次分享的技术选择。

## 当前建议

Implementation progress is tracked in [Rust relay implementation](overview.md).
The [2026-10-17 host experiments](../../../spikes/relay-hosts/README.md) support
keeping native synchronous submission: healthy receivers progressed while a real
writer stalled. Follow-up Preview controls located the reset failure in the Rust
asynchronous callback path: aborting after the Rust future returns lets the object
recover. This does not establish buffer release. With an 8 KiB client receive
buffer, the old connection still delivered all 8 MiB after the new object answered;
reset also disconnected a healthy peer on the same share. `close()`, `CLOSED`, an
Alarm or an empty socket list cannot supply `Input::Closed` evidence.

The kernel now retains delivery ledgers independently of communicating sockets.
Recovery requires a complete responsibility checkpoint or explicit host evidence of
full release; unknown responsibility refuses restoration. An independent manifest
detects missing generation records, and detached generations retain capacity until
settlement or proven release. Restart and empty socket lists cannot refund occupancy.

STOP acknowledgement now follows durable revocation, unsubmitted-queue discard
and publication quiescence, independently of delivery settlement. Stopped recovery
retains submitted occupancy and original deadlines; duplicate STOP and late callbacks
cannot change that responsibility. Protocol and Go/TypeScript contracts preserve
the existing wire format and explicitly allow already-submitted bytes to arrive.

结论：**采用共享 Rust 语义内核与两个平台适配器。** 先明确启动额度、失联判断与缓冲回收，跑通 Cloudflare 真实分享、下载和恢复，再完成原生替换。同步调整 Go／浏览器客户端、测试夹具和构建发布入口。

引擎签发绑定 `relay_identity`、分享、会话、来源/目标连接及其代次、方向和协议版本的 `ForwardPermit`，统一创建、替换与撤销。许可只表达路由授权；STOP 或连接替换后，排队帧和延迟回调不得沿用已撤销的许可。

线协议将双向 `WS2B` 设为必选规则，Go／浏览器客户端与两种中转统一发送并遵守，不按平台协商开关。初始容量纳入现有握手，并从连接总预算预留新会话启动额度，避免首帧与窗口声明相互等待。采用流水发送和消费后的批量反馈，反馈阈值适配窗口，不逐帧等待确认；中转控制消息不受内容额度阻塞。Cloudflare 按目标端尚未预留的交付额度发放 `WS2W`，发放时预留对应容量，仅在目标客户端消费并反馈后补充。原生保留 socket 背压，独立设置窗口与并发，通过正常浏览、下载和多接收者请求测量排除明显退化。

Cloudflare 的已授未用额度、事件内临时载荷和宿主未确认发送数据共用目标连接代次的有界预算；`send()` 返回或会话退役不释放已提交宿主的未确认占用。

退役先撤销许可并丢弃未提交队列，再按序通知目标端。目标端消费或丢弃此前帧后反馈最终帧数、字节数；按连接代次、会话和方向幂等结算，仅回收连接预算，不复活会话。取消立即生效，结算在后台完成；结算无法推进时有界终止异常连接，确认终止后回收剩余占用。

原生适配器按分享串行化控制状态，数据转发由独立连接任务并行执行，可使用 Tokio、有界公平队列和原生 WebSocket 背压。原生须明确提交点：许可复核与提交在同一同步临界区完成并与撤销线性化，撤销后不再新增提交；socket 等待留在控制锁外，慢接收者只阻塞自身会话。Cloudflare Rust 适配器使用休眠和平台发送语义；普通数据帧可以经过 Rust/Wasm 内存，复制必须有界并纳入 CPU、内存和吞吐测量。平台差异只能改变内部容量和故障触发点，不能改变帧顺序、会话隔离、所有权或客户端可见的安全语义。

两个适配器均承接注册、加入、密封描述符交付、初始 Relay Lane、P2P 信令、Relay Lane 恢复和不透明帧转发。原生版本还须保留当前 UDP STUN、独立健康检查和限流；STUN 不进入 Cloudflare 适配器。

注册证明继续绑定 descriptor 原始字节的摘要；中转验证摘要但不解析端到端对象外壳，保留发送者认证、防重放和大小限制。Go 已按此边界实现；协议规范和共享向量分别明确上传授权与接收端对象有效性。接收端继续完整验证对象，使对象格式可独立于中转协议演进。

浏览器保留 TypeScript 协议代码，Go 客户端保留 Go 编解码器，以共享向量保持三端协议一致。Rust 必须通过共享 `ed25519-acceptance.json`，按协议补齐严格校验，不能仅依赖库的 `verify_strict`；注册、恢复和 STOP 证明复用并补齐跨语言向量，逐字节核对签名 preimage。发送端对象向量继续约束 Go／TypeScript 接收端，不要求 Rust 中转复刻对象解析。除非实测收益明确，否则不迁移浏览器协议代码到 Wasm。

Cloudflare 优先使用高层 `workers-rs` API，先验证回调前复制的峰值内存；仅在超出预算时补复制前的大小检查，不预先引入零复制框架。SQLite 保存生命周期事实；WebSocket 附件保存有界的角色、握手/准入状态、连接/会话/许可代次、credit、待结算占用、最近入站时间及待发控制状态，并在事件结束前更新。唤醒后恢复状态并检查绝对截止时间，不重置额度或延长宽限；状态不一致时仅结束受影响会话并重连。每次事件结束时不得遗留应用数据帧待发队列；获准帧在事件内提交宿主，发送失败时明确结束受影响通道并重连。

Go 与浏览器对两种中转统一使用 `WS2H/WS2A` 并保留 nonce；双方以同连接的合法入站消息刷新存活期限，空闲方向仍发送保活，不因单次回复排队超时断线。连接存活与业务进度分别判断，心跳和无关流量不得延长会话进度或资源结算期限。保留有界队列和原生控制优先调度，不靠缩小正常窗口迁就心跳。

引擎输出下一绝对截止时间；原生使用定时器，Cloudflare 合并 Alarm 并在事件入口复核。有截止时间时，仅在无待触发 Alarm 或最早截止时间早于已设 Alarm 时设置；延后则保留旧 Alarm，触发后按最新状态重排，避免每帧或每次心跳重写且不放宽超时。为握手、会话准入、连接存活和断线恢复安排必要检查；正常浏览、暂停操作和 P2P 传输不因中转无数据而超时。单条中转失联只触发该连接恢复，不终止其他健康路径。

## 目标架构

共享内核采用 `no_std + alloc`，由每分享 `share::ShareRelay` 统一持有生命周期、握手、连接、会话与交付账本。`handle(input, now)` 返回有序控制发送、存储、清理命令与下一截止时间；内核负责 wire ID 分配、撤销扇出、持久化屏障和恢复校验。同步 `submit` 在同一独占借用中完成许可复核、FIFO 记账与宿主提交，socket 等待放在调用之前。适配器只执行 I/O、提供容量与调度；休眠宿主在事件末刷新发生变化的连接附件，唤醒时将存活附件交给内核组合校验。原生不启用附件变更记录，热路径无需逐帧快照。交付元数据采用按需分配、有硬上限的稳定索引槽池，握手连接不预留 flow 槽位，快照仅保存仍需保留的 flow 及其索引；连接与会话上限不等于每连接的预分配长度。统一 `observation::Observer` 输出决策，平台仅补充运行环境字段。

生命周期自身区分等待所有权持久化确认的 `Activating` 与已可服务的 `Published`。注册与 RESUME 复用同一提交屏障；确认事件必须匹配操作身份并满足原握手截止时间，才能允许 REGISTERED、JOIN 和转发。`ShareRelay` 负责执行、重试和清理，不另设可服务状态来重新解释生命周期。

```text
Go CLI / 浏览器 / 未来客户端
              │
              └── 中转线协议
                    ├── Cloudflare Rust Worker/DO 宿主
                    │     ├── 边缘 Worker：路由、升级与无状态注册凭据校验
                    │     ├── ShareRelay DO：每分享准入、持久化执行与平台数据面
                    │     └── Rust：严格控制协议与唯一语义内核
                    │
                    └── 原生 HTTP/WebSocket 路由器
                          ├── 进程级准入控制：部署总量与新建速率
                          ├── 每分享 Actor：生命周期与控制状态
                          ├── 连接任务：有界并行转发
                          └── 原生 Rust：直接调用同一协议与语义内核

两条路径共享 Rust 严格协议实现和纯控制面引擎，但不共享平台 I/O 实现；Cloudflare 的 Rust 代码编译为 Worker 所需的 Wasm 产物
```

客户端在升级请求的查询参数中携带非秘密分享路由提示；Worker 据此选定 ShareRelay DO，DO 校验协议中的分享身份与提示一致。提示不授予权限、不进入 `relay_identity`；Go 与浏览器拨号同步调整。

Cloudflare 保留每连接、每分享的资源硬上限，并按共享 isolate 的内存峰值（含 Wasm/JS、复制和宿主缓冲）留余量。窗口和并发由多会话慢网测量确定，不照搬原生上限；必要时增加适配器内的 isolate 级在途计数，不引入逐帧跨 DO 协调。首版不设 `DeploymentControl`：边缘 Worker 无状态校验发送端注册凭据，ShareRelay 限制每分享资源，用量统计使用 Cloudflare 自带分析。此方案不提供部署级统一新建限额或总容量硬保证，统计不参与实时准入。新建准入额度不阻止已有分享恢复或 STOP；账户配额耗尽不在保证内。确需部署级硬上限时，再引入具备幂等申请、释放与过期规则的容量预留。

### Attachment budget

Each generation record contains its complete ledger, handshake and receiver-owned
session snapshots. A replicated header retains runtime identity and the wire-session
high-water mark. New admissions refresh all retained records; ordinary delivery and
feedback update only affected generations. A separately retained `CheckpointManifest`
names every required record, including disconnected generations. Missing records or
unknown responsibility cannot authorize recovery. The host must preserve the latest
coherent manifest and records independently of socket attachments; the Cloudflare
storage mechanism remains unresolved and must be established before integration.

Cloudflare's serialized attachment limit is **16,384 bytes**. The initial host profile
sets session admission and per-connection retained-flow limits to **32**, including
unsettled retired flows in each connection's bound, and
reserves at least **512 bytes** for host serialization/metadata, leaving at most **15,872
bytes** for the kernel record. `CheckpointBudget::check` verifies the worst encoded
connection state against this allowance before tracking begins. The sender's fanout
therefore fits without spreading its accounting across unrelated sockets or writing
delivery counters to SQLite. Native capacities remain independent.

The Worker must store the binary record as an ArrayBuffer/typed byte array and verify
the actual wrapper's serialized size; a JavaScript number array or base64 expansion
cannot assume the same budget. Increase the reserved overhead or reduce the Cloudflare
profile if needed. `flush_checkpoints` replaces complete changed attachments synchronously
after outputs finish. On partial failure, retry the retained batch while active or close
affected sockets before returning; never hibernate with stale delivery accounting.
Deployment validation must exercise this path through the real Worker binding.

## 部署体验

首版支持用户 **Fork 当前 WindShare 仓库，通过 GitHub Actions 自动部署到 Cloudflare**。用户首次配置部署权限并启用工作流，部署时生成发送端注册凭据，导入后照常分享；以后通过 `Sync fork → Update branch` 同步上游稳定部署分支，由 Actions 更新原部署，首版不另建 CLI 部署器。

部署参数与凭据通过 Variables／Secrets 配置，避免修改源码造成同步冲突；更新保留中转地址、注册凭据与必要持久状态。个人配置分开保存公开地址、私有注册凭据与允许的候选集合，贯穿首次注册、恢复、重新发布和 STOP；凭据只授权发送端注册，不进入分享链接。候选保持现有并行策略，仅私人模式只发布并连接私人候选。接收者无需账户或额外配置。

中转在本仓库内使用可独立构建和部署的 Rust 工作区，共享内核仅维护一份源码；未来是否拆仓库另行决定。

## 可观测性

两个适配器应使用相同的结构化决策词汇。适合记录的字段包括部署 ID、构建版本、平台、操作 ID、非秘密诊断形式的分享标识、连接 ID、会话 ID、代次、许可代次、事件、决策、结果和拒绝原因。客户端监督器还应记录中转配置 ID、允许的候选集合、当前 Lane、后备原因和每候选停止结果。

日志绝不能包含能力片段、注册授权凭据、证明密钥、密封描述符字节或不透明传输载荷。在现有握手中公布构建版本、中转协议版本和能力；客户端按协议与所需能力判断兼容性，真正不兼容时明确提示升级，不因构建版本不同拒绝连接。

普通分享可以展示当前正在使用哪个中转或 Lane、为何发生后备以及哪些候选停止失败；这些是运行结果和诊断信息，不是要求用户作出的技术选择。界面不能用底层 Worker、Durable Object 或 Actor 术语要求用户作决定。

## 免费套餐假设

本设计依赖 Cloudflare 免费 Workers 套餐提供 Durable Objects、SQLite 支持、WebSocket 休眠以及适合个人轻量使用的计费方式。这些限制和价格属于外部政策，不是协议保证，必须在实施前和发布前重新确认。

首版不依赖宿主 Ping/Pong 或 `setWebSocketAutoResponse`；自动回复需调整现有探测格式，仅在实测收益明确时评估。以现有心跳频率和超时为基线，合并 Alarm 截止时间检查，核算消息、Alarm 调用与写入及唤醒执行时长，并实测慢网吞吐；可休眠的空闲时间免 duration 不代表心跳处理免费。数据帧打包只在存在吞吐瓶颈时引入，并须协商和限界。

Cloudflare 的消息大小、对象吞吐、CPU、休眠和配额只能出现在 Cloudflare 适配器配置与部署诊断中。共享引擎只表达协议安全上限和过载结果，原生默认值不得引用免费套餐数值。Cloudflare 调整政策时，普通服务器部署不需要修改配置或重新发布。

产品应把免费部署描述为适合个人及轻量使用，不应承诺无限传输量、公共服务容量或 SLA。

## 外部资料

- [Cloudflare Workers 的 Rust 语言支持](https://developers.cloudflare.com/workers/languages/rust/)
- [Durable Object State API](https://developers.cloudflare.com/durable-objects/api/state/)
- [Durable Objects WebSocket 休眠](https://developers.cloudflare.com/durable-objects/best-practices/websockets/)
- [Durable Objects SQLite 存储](https://developers.cloudflare.com/durable-objects/api/sqlite-storage-api/)
- [Durable Objects 价格](https://developers.cloudflare.com/durable-objects/platform/pricing/)
- [Durable Objects 限制](https://developers.cloudflare.com/durable-objects/platform/limits/)
- [Cloudflare Workers 的 GitHub Actions 部署](https://developers.cloudflare.com/workers/ci-cd/external-cicd/github-actions/)
- [`workers-rs` Durable Object 宏](https://github.com/cloudflare/workers-rs/blob/main/worker-macros/src/durable_object.rs)
- [`workers-rs` WebSocket 实现](https://github.com/cloudflare/workers-rs/blob/main/worker/src/websocket.rs)

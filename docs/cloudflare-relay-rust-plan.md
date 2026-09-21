# Cloudflare 原生中转与 Rust 统一计划

状态：纯 Rust 方向实施计划草案
最后更新：2026-08-10

## 目的

WindShare 应允许每位用户将私人中转部署到自己的 Cloudflare 免费账户，并把它作为个人中转配置导入。

在当前协议中，中转并非仅在 P2P 失败后才建立：它先承载发送端注册、接收端加入、ProtocolSession 初始通道和 P2P 信令，再作为加密内容帧的后备传输 Lane。它不是标准 TURN，也不参与 ICE 候选中继，因此不会提高 WebRTC 本身的 NAT 穿透成功率；但它能在直连不可用时独立完成 WindShare 传输，所以当前设计不依赖 TURN。

长期设计还应消除分别维护 Go 与 Cloudflare 中转语义的成本。方案采用一个 Rust 中转语义内核和两个 Rust 平台适配器：

- Rust Cloudflare Worker/Durable Object 宿主，用于个人低流量部署。
- 原生 Rust 二进制程序，用于项目运营的中转、自托管和 Cloudflare Tunnel 部署。

这里的“纯 Rust”指中转业务源码、控制语义和两个适配器均由 Rust 实现；Cloudflare 仍按 Workers 的 Wasm/JavaScript 宿主运行。平台边界上的有限数据复制是可接受的，不把零复制作为交付前提。若某个必需的 Cloudflare API 尚未被 Rust 绑定覆盖，只允许增加针对该 API 的最小宿主 shim，不引入第二套 TypeScript 控制语义。

## 设计原则

- **Cloudflare 纯属可选能力。** 官方构建默认包含官方 relay；未配置个人中转时，创建分享、打开链接、P2P 协商和官方 relay 后备的体验必须保持不变。接收者不需要 Cloudflare 账户、额外安装或手动选择中转。
- **原生服务器是功能与性能基准。** Cloudflare 的单对象吞吐、休眠、Wasm、配额和队列限制不得进入原生服务器的默认值、热路径或依赖图。原生二进制程序不得依赖 Wrangler、JavaScript、Wasm 或 Cloudflare API。
- **统一决策，不统一 I/O 机制。** Rust 引擎统一认证、所有权、会话、恢复、停止和截止时间；原生与 Cloudflare 适配器分别实现最适合平台的转发、排队、背压和并发。不能为了复用代码把原生数据面降到最低共同能力。
- **Cloudflare 载荷由适配器持有。** 已获许可的不透明数据帧可以在 Rust Worker 适配器中复制为有界 `Vec<u8>`；复制不构成协议语义，也不改变 E2EE 边界。普通数据帧仍不得进入 SQLite、结构化日志、无界应用队列或控制状态机；适配器只做固定头验证、许可查找和转发，并受明确的帧大小与排队预算约束。零复制只作为后续优化方向。
- **默认流程零新增操作。** Rust 重写本身不构成用户功能。个人中转只允许增加一次性的部署和导入步骤；普通分享可以展示当前自动采用的中转、Lane 和后备原因，但不要求用户选择技术方案。
- **主动退出即停止。** 发送端正常退出时先使用 `StopAuthority` 停止分享并清理已创建候选；异常断开只保留有界恢复宽限，宽限结束后停止。停止结果本身可以 best-effort。
- **自动候选与可扩展并行。** P2P 和官方 relay 自动工作；私人 Cloudflare relay 只是发送端增加或调整的候选，不是使用产品的前置条件。首版可以只采用一个 Relay Lane，但会话、许可和候选模型不得阻止未来同时采用多个 Peer/Relay Lane；内容范围调度器未来可以把不同块分配给不同连接。
- **不增加分享时的技术选择。** 接收者不配置也不选择中转。
- **持久化不进入载荷热路径。** SQLite 只保存有界生命周期状态、密封描述符和永久停止标记；不保存、排队或检查不透明内容帧，原生与 Cloudflare 数据帧转发都不执行数据库操作。
- **以行为和基本负载决定切换。** 当前 Go 中转尚未作为已部署服务运行，不建立迁移、双栈或旧数据兼容义务。Rust 版本先通过真实端到端流程、协议向量、重启/恢复和基本慢接收者测试；通过后在同一变更中替换并删除 Go。性能测量用于发现明显退化，不把未运行过的 Go 实现当作线上基准。
- **非核心恢复允许 best-effort。** 续传、配额对账和部分停止可以 best-effort；核心授权和安全失败必须明确。不要为了理论上的完美恢复重新增加大量状态机复杂度。

### 用户可见结果

| 用户场景 | 预期变化 |
| --- | --- |
| 未配置个人中转 | 无变化；继续使用当前默认中转策略。 |
| 配置个人 Cloudflare 中转 | 仅首次增加部署和导入；之后仍创建一个普通分享链接。 |
| 接收分享 | 无变化；打开链接后自动尝试 P2P 和已发布的中转候选。 |
| 普通服务器自托管 | 继续运行独立原生二进制程序；功能不减少，性能不受 Cloudflare 限制。 |
| 中转故障 | 按发送端已保存的中转配置自动尝试下一个候选；若配置为仅个人中转则明确失败。普通分享可以展示当前采用的候选和后备原因。 |

因此，这不是面向所有用户的速度升级：对未启用者应是零变化；对启用者是信任边界和部署控制权升级，不承诺比项目中转更快。选择“仅私人中转”是中转配置阶段的一次性高级设置，会主动以可用性换取更窄的信任面；普通分享可以显示当前配置和自动决策结果，但不增加每次分享的技术选择。

## 当前建议

结论：**方案有条件可行，最终采用纯 Rust relay 实现。** Rust 承载中转服务的唯一控制面领域逻辑，并分别提供原生 Rust 和 Rust Cloudflare Worker/Durable Object 适配器；Cloudflare 允许平台边界上的有限载荷复制，不再引入 TypeScript 平台壳或跨语言控制 ABI。首个实现门槛是验证 Rust Worker 必需 API 和一次完整部署流程；当前 Go 中转作为未部署的参考实现，Rust 通过端到端验证后直接替换。

由于当前没有线上用户或需要迁移的活动状态，Rust 版本通过路由、所有权、生命周期、准入、持久化、可观测性和基本负载验证后即可退役 Go 服务端；不为不存在的旧部署增加兼容层。Cloudflare 的平台限制仍只属于 Cloudflare 适配器，不能改变原生适配器的默认值或依赖图。

引擎在会话建立后签发绑定 `relay_identity`、分享、会话、来源与目标连接、连接代次、方向和协议版本的 `ForwardPermit`。适配器对每个不透明帧做严格固定头检查和许可查找后直接转发，不把载荷送入状态机，也不为每帧产生持久化效果。许可的创建、替换和撤销仍完全由共享引擎决定。

原生适配器是性能参考实现，可以使用 Tokio、多核 Actor、有界公平队列和原生 WebSocket 背压。Cloudflare Rust 适配器使用休眠和平台发送语义；普通数据帧可以经过 Rust/Wasm 内存，复制必须有界并纳入 CPU、内存和吞吐测量。平台差异只能改变内部容量和故障触发点，不能改变帧顺序、会话隔离、所有权或客户端可见的安全语义。

Cloudflare 与原生适配器必须承接现有中转的完整职责，包括注册、加入、密封描述符交付、初始 Relay Lane、P2P 信令承载、Relay Lane 恢复和不透明帧转发。不能把它建模成 P2P 失败后才临时创建的可选数据连接。

浏览器应继续使用 TypeScript 协议代码，Go 发送端和接收端应保留 Go 客户端编解码器。通过跨语言规范向量保持这些编解码器一致。除非后续测量证明存在明确收益，否则把浏览器协议代码迁移到 Wasm 不在本计划范围内。

Cloudflare 适配器默认使用 Rust Worker/Durable Object 和高层 `workers-rs` API。若某个必需 API 缺少绑定，只增加局部的 JS/`JsValue` shim，并保持 Rust 负责全部控制语义；不因此重写一套 TypeScript 服务端。Rust Worker 能否可靠完成升级、休眠、套接字恢复、SQLite、Alarm 和平台故障处理，应在小型能力探针中确认。原生功能和产品可用性优先于零复制优化。

## 目标架构

```text
Go CLI / 浏览器 / 未来客户端
              │
              └── 中转线协议
                    ├── Cloudflare Rust Worker/DO 宿主
                    │     ├── 边缘 Worker：路由、升级与部署准入
                    │     ├── DeploymentControl DO：部署准入与总量预留
                    │     ├── ShareRelay DO：持久化执行与平台数据面
                    │     └── Rust：严格控制协议与唯一语义内核
                    │
                    └── 原生 HTTP/WebSocket 路由器
                          ├── 进程级准入控制：部署总量与新建速率
                          ├── 每分享 Actor：生命周期与原生数据面
                          └── 原生 Rust：直接调用同一协议与语义内核

两条路径共享 Rust 严格协议实现和纯控制面引擎，但不共享平台 I/O 实现；Cloudflare 的 Rust 代码编译为 Worker 所需的 Wasm 产物
```



## 部署体验

首版首选流程是提供 **Deploy to Cloudflare** 按钮。



## 可观测性

两个适配器应使用相同的结构化决策词汇。适合记录的字段包括部署 ID、构建版本、平台、操作 ID、非秘密诊断形式的分享标识、连接 ID、会话 ID、代次、许可代次、事件、决策、结果和拒绝原因。客户端监督器还应记录 RelayPolicy、候选配置 ID、候选顺序、当前 Lane、后备原因和每候选停止结果。

日志绝不能包含能力片段、注册授权凭据、证明密钥、密封描述符字节或不透明传输载荷。系统应暴露足以诊断部署的构建和协议能力元数据，但不能泄露私人配置。

普通分享可以展示当前正在使用哪个中转或 Lane、为何发生后备以及哪些候选停止失败；这些是运行结果和诊断信息，不是要求用户作出的技术选择。界面不能用底层 Worker、Durable Object 或 Actor 术语要求用户作决定。

## 免费套餐假设

本设计依赖 Cloudflare 免费 Workers 套餐提供 Durable Objects、SQLite 支持、WebSocket 休眠以及适合个人轻量使用的计费方式。这些限制和价格属于外部政策，不是协议保证，必须在实施前和发布前重新确认。

休眠十分关键，因为中转连接可能长时间空闲。当前帧大小明显低于 Cloudflare 文档中的 WebSocket 消息上限，但单个 Durable Object 的请求速率上限可能成为高吞吐传输的实际瓶颈。可行性原型应确定线协议是否需要经过协商且有明确上限的批处理信封；增加无界队列不能替代流量控制。

Cloudflare 的消息大小、对象吞吐、CPU、休眠和配额只能出现在 Cloudflare 适配器配置与部署诊断中。共享引擎只表达协议安全上限和过载结果，原生默认值不得引用免费套餐数值。Cloudflare 调整政策时，普通服务器部署不需要修改配置或重新发布。

产品应把免费部署描述为适合个人及轻量使用，不应承诺无限传输量、公共服务容量或 SLA。



## 外部资料

- [Cloudflare Workers 的 Rust 语言支持](https://developers.cloudflare.com/workers/languages/rust/)
- [Durable Object State API](https://developers.cloudflare.com/durable-objects/api/state/)
- [Durable Objects WebSocket 休眠](https://developers.cloudflare.com/durable-objects/best-practices/websockets/)
- [Durable Objects SQLite 存储](https://developers.cloudflare.com/durable-objects/api/sqlite-storage-api/)
- [Durable Objects 价格](https://developers.cloudflare.com/durable-objects/platform/pricing/)
- [Durable Objects 限制](https://developers.cloudflare.com/durable-objects/platform/limits/)
- [Cloudflare Deploy Buttons](https://developers.cloudflare.com/workers/platform/deploy-buttons/)
- [`workers-rs` Durable Object 宏](https://github.com/cloudflare/workers-rs/blob/main/worker-macros/src/durable_object.rs)
- [`workers-rs` WebSocket 实现](https://github.com/cloudflare/workers-rs/blob/main/worker/src/websocket.rs)

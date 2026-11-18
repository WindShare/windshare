# Cloudflare 发布与中转管理台执行计划

保留两个部署入口。默认的 **Deploy private relay** 为自建者交付一台可用、可管理的中转；独立的 **Deploy network authority** 为网络运营者交付账号、额度和节点管理。普通用户直接分享或打开链接。自建者只需在客户端输入中转地址并批准自己的设备。运营者部署第二台节点时，沿用已有的账号库和签名体系。

## 部署后，用户拿到什么

| 入口 | 交付内容 | 用户的下一步 |
| --- | --- | --- |
| Cloudflare `full`（默认） | 接收网页、私有中转、本节点管理台，并尝试配置托管 TURN | 在 CLI 或桌面端输入中转地址，打开管理台，核对设备名称和配对码后批准。接收者直接打开分享链接。 |
| Cloudflare `relay-only` | 私有中转和同一个节点管理台，接收网页由已有站点提供 | 使用与 `full` 相同的设备授权流程。 |
| Cloudflare `frontend` | 接收网页、目录和安装入口 | 发布站点。官方前端继续用这一模式向各区域分发同一份构建。 |
| 独立的网络管理后台 | Authority Worker、独立的 SQLite Durable Object 和现有网络管理页 | 首次设置网络管理员，之后从同一个后台登记多台节点、配置额度、处理举报和封禁。 |
| 原生 `wsrelay` | 单个二进制内嵌本节点管理页，接收网页独立发布 | 启动节点，打开一次性链接设置管理员，再用中转地址为自己的设备授权。 |

`full` 今天已经带节点管理台。自定义域名部署把中转和管理域名路由到同一个 Worker；使用 `workers.dev` 时，部署器创建 `<name>-console`，连接主 Worker 的同一个 `ACCESS` 对象。两种拓扑都通过 `/.well-known/windshare` 公布真实的 `console_url`。如果每次 `full` 再创建一套网络后台，增加第二台中转就会产生第二套管理员、账号和签名身份，反而无法统一运营。优先改善已有后台的入口，比增加部署选项更有价值。[部署配置](../../../scripts/deploy/cloudflare/configuration.mjs) · [服务发现](../../../relay-rust/crates/relay-cloudflare/src/platform/edge.rs)

## 实施顺序

### 1. 统一 CLI 与桌面的配对和中转选择

CLI 的 `wind relay onboard <relay-url>` 和桌面端共用引擎的配对与选路设置。首次配对直达带设备码的审批页。以后在桌面设置点“管理此中转”，按服务发现返回的地址打开控制台首页。

- **自动（默认）**。保留已启用的节点，有空位时启用新配对的节点。每条链接最多 8 个候选。8 台都由用户选定时，设置页提示内置网络已无补位名额。第 9 台可保存为“未启用”，之后可在设置中调整选择。
- **仅使用所选中转**。分享只尝试已勾选的节点。新配对的节点先保存，原有勾选保持不变。全部不可用时保留已选文件，提示重试或修改中转选择。

CLI 和桌面按引擎结果显示节点是否启用、当前策略和分享域名。等待配置的分享保留已选文件，配对后按当前设置重试。本次分享已明确指定节点时，继续使用该选择。[配置写入](../../../engine/relaysetup/installation.go) · [分享意图](../../../engine/relayconfig/selection.go) · [桌面恢复分享](../../../host/desktop/setup.go)

自动模式沿用并行注册，先为已启用的节点保留名额，再用内置网络补位。账号票据仅用于从内置目录自动选出的官方节点，手工指定的地址沿用节点凭据或匿名准入。两种模式都在首台中转注册成功且本地可接待时出链接，其余节点可随后接入，传输优先 P2P。[候选名额](../../../connectivity/relayplan/plan.go) · [注册调度](../../../connectivity/relayset/admission.go)

是否延迟内置节点注册，留待对比出链耗时、接收方首次接入成功率与耗时、注册请求数和公共中转实际转发字节后决定。测量应包含发送方能连自建节点、接收方却连不上的情况。已发链接沿用现有候选和恢复调度，额外补位后置。

分享链接默认使用官方接收网页，也可在设置中指定其他域名。配对保留已有网页选择。客户端添加地址和手工服务 key 时，仅合并对应节点的凭据，保留其他节点、接收网页和 TURN 设置。`wind config import` 继续用于整份配置替换。`full` 配对成功后，自建者可点“使用我的接收网页”保存 CLI 和桌面共用的域名。`relay-only` 配对后即可使用默认网页分享。[前端选择](../../../engine/internal/networkruntime/frontend.go)

调整私有中转的 Actions 名称和部署摘要，让自建者能区分“中转已发布”和“TURN 尚待配置”。本地部署继续输出可点击地址；GitHub 摘要说明从 Cloudflare 的 **Domains & Routes** 取得中转地址，并给出配对命令。复用现有地址屏蔽和输出逻辑。[设备配对](../../../engine/relaysetup/enrollment.go) · [部署摘要](../../../scripts/deploy/cloudflare/reporting.mjs)

### 2. 用现有管理页独立发布 Authority，接通首批节点

新增 **Deploy network authority** 工作流。首次由运营者手动开通，启用后由默认分支的 Authority 相关改动自动更新。沿用 `network-authority.py` 发布内嵌现有管理页的 Rust/Wasm Worker 和 `AUTHORITY` 绑定。使用独立的 GitHub Environment 保存部署参数，保持 Worker 名、API 域名和存储身份稳定。官方部署沿用现有分区 DNS 和源站路由，自建运营者填写自己的域名。收窄私有中转工作流的 `relay-rust/**` 触发范围，两条工作流分别监听各自的界面、服务端及共享依赖。[现有 Authority 部署器](../../../scripts/deploy/network-authority.py) · [官方前端工作流](../../../.github/workflows/official-network.yml)

运营者在本机生成并保管离线根密钥，将公开信任材料放入部署配置，在线密钥通过 Environment Secret 与对应证书同批发布。首次初始化令牌在本机生成，CI 部署器从 Environment Secret 读取该值，完整的一次性链接只在本机显示。后续更新和失败重跑复用原有状态，管理员照常登录。[密钥工具](../../../relay-rust/crates/network-authority/src/bin/network-authority-keys.rs) · [管理员接口](../../../relay-rust/crates/network-authority/src/host/routes.rs)

首批官方节点上线时，运营者登记签名描述中的节点公钥，在节点配置 Authority 上报地址和受信签发者，再签发含这些 `relay` 节点的目录快照。候选客户端内置该快照、目录信任根及 Authority 地址和根公钥。用新安装的候选客户端完成一次账号中转传输，确认用量上报后，公开发布同一份快照和客户端。[节点接入](../../../relay-rust/crates/network-authority/README.md#enroll-and-publish) · [网络发布配置](../../../deploy/network/README.md)

后台分别显示登记情况和最近上报时间，“已收录”以已发布签名目录中的节点公钥及版本为准。后台一键扩容留到 P3。仅对已知使用同一受信目录的接收页生成节点别名，其他接收页使用明确的中转地址。[P3 扩容流程](global-network.md#p3--console-onboarding-and-fleet-automation) · [分享链接生成](../../../engine/internal/networkruntime/resolve.go)

### 3. 原生节点复用网页授权，管理页随二进制交付

原生端的主要工作在授权。现在 `wsrelay` 首次启动生成节点身份和本地 Owner key；Go 引擎读取签名描述后，把原生节点归为匿名或手工密钥两种路径。把**允许匿名分享**与**支持设备授权**拆成独立能力，在签名描述中发布管理入口，同步更新 Rust 描述生成端和 Go 消费端。公开节点的访客可以直接匿名使用，主人走设备配对获得自用优先级。再次添加同一地址时保留已领取的凭据。[节点身份](../../../relay-rust/crates/relay-native/src/node/identity.rs) · [签名描述](../../../relay-rust/crates/relay-native/src/node/descriptor.rs) · [引擎接入分支](../../../engine/relaysetup/workflow.go)

给原生节点增加一次性管理员初始化、同源会话和管理路由。复用 `relay-access` 的设备授权状态机，用本地 SQLite 保存管理员、配对申请、设备凭据和撤销记录。手工配置的 key 也受同一撤销记录约束，配置重载或重启后撤销仍生效。引擎沿用现有申请、领取、安装确认及中断恢复流程，无人值守设备仍可手工配置密钥。社区发现和贡献额度沿用全局计划的 P4 安排。[授权状态机](../../../relay-rust/crates/relay-access/src/enrollment.rs) · [原生 HTTP 入口](../../../relay-rust/crates/relay-native/src/http/route.rs) · [社区节点计划](global-network.md#p4--community-nodes)

原生和 Cloudflare 节点复用同一套设备审批与凭据管理界面，各自签发和保存凭据。同一节点的主人设备共用 Owner 身份，每台设备的 key 可单独撤销。原生端在撤销前提示将停止该设备在本节点的现有中转，Cloudflare 保留“同时停止现有分享”选项。发给他人的服务 key 各有独立身份，导出配置只提供中转地址和 key。Cloudflare 沿用 Owner 密码、管理域名、`SHARES` 和 `ACCESS` 绑定，并提供 TURN 管理。原生部署模板允许所选接收网页的 Origin，使接收者能直接打开链接下载。[凭据生成](../../../relay-rust/crates/relay-cloudflare/src/platform/access/management.rs) · [原生浏览器来源策略](../../../relay-rust/crates/relay-native/src/http/origin.rs)

原生网页授权可独立于 Authority 发布。二进制和 Docker 镜像内嵌节点管理页，页面随本地 API 同版升级。公网 HTTPS 沿用现有反向代理。[原生部署](../../../relay-rust/crates/relay-native/README.md) · [原生镜像构建](../../../deploy/wsrelay/Dockerfile) · [二进制发布](../../../.github/workflows/release.yml)

### 4. 按实际复用需求迁移 React

当两个宿主共用的节点管理页需要组件化时，再复用 `web` 的 React、TypeScript、Vite 和依赖锁。接收页和管理页各有构建入口。`full` 交付两者，`relay-only` 只交付节点管理页，`frontend` 只交付接收页。前端构建放在页面开发与打包环节，日常 Rust 开发沿用 Cargo 入口。[现有构建入口](../../../web/vite.config.ts) · [私有中转工作流](../../../.github/workflows/cloudflare-relay.yml)

网络管理页继续使用现有实现。以后若有组件值得共用，再独立迁移，保留 `/console#initialize=…`、现有管理操作、同源会话、CSRF token 和独立的公共 `/report` 入口。[现有页面行为](../../../consoles/network/App.tsx) · [Worker 路由](../../../relay-rust/crates/network-authority/src/host/edge.rs)

# 节点管理台与网络管理台 UI 重写计划

用户认为现有两套管理台的视觉效果太差，明确要求**重新设计节点管理台和网络管理台的全部页面，并用 React 完整重写两套 UI**。重做页面布局、字号层级、间距和操作状态。

先重写节点管理台，让自建者从 CLI 或桌面端打开链接，核对设备，批准后回到客户端。再重写网络管理台，让运营者登录后就能比较各节点的最近上报和目录收录情况，并直接查看举报。普通用户继续从客户端分享、从接收页下载。[中转服务需求](../../clarifications/中转服务.md) · [原始需求](../../clarifications/原始需求.md)

两次重写分批交付，取代[原计划第 4 节的按需迁移安排](relay-and-network-consoles.md#4-按实际复用需求迁移-react)。审批反馈和节点策略写入可先在现有页面与 API 交付。两个管理台共用根目录的 `consoles/` 工程，各自拥有入口、API 客户端和管理员会话。原生 `wsrelay` 与 Cloudflare 中转共用节点页面。网络管理台和公共举报页随 Network Authority Worker 独立发布。

## 第一阶段 · 重写节点管理台

1. **从配对链接直达审批。** 客户端发现 `console_url`，申请配对后打开服务端返回的 `verification_uri_complete`（`/?code=…`）。需要登录时保留配对码，登录后回到这条申请。审批页采用单栏，以设备名为标题，配对码用等宽字体只展示一次，“批准设备”作为唯一主按钮，手机首屏即可完成核对。审批接口返回成功就提示回到客户端，由客户端领取并保存凭据。查不到待审批申请时提示核对设备码或回客户端查看进度。只有已知的申请到期时间已过，或宿主明确返回过期，才提示重新配对。从管理首页进入时，保留手工输入设备码的路径。[配对协议](../../../relay-rust/console/src/lib.rs) · [客户端领取流程](../../../engine/relaysetup/enrollment.go)
2. **按宿主提供登录入口。** 原生节点主人通过 `/#initialize=…` 设置密码并自动登录。Cloudflare 节点主人使用部署时配置的密码。节点页面从同源 `/api/session` 取得会话和能力。带码页只加载审批需要的数据。已登录后，只有宿主明确返回会话失效才切回登录页。凭据或 TURN 查询失败时，在对应区域提示重试，主人仍能批准设备。[原生初始化](../../../relay-rust/crates/relay-native/src/console/sessions.rs) · [Cloudflare 登录](../../../relay-rust/crates/relay-cloudflare/src/platform/access/sessions.rs)
3. **从首页管理节点访问。** 首页列出凭据名称、最近使用时间和撤销状态。手工创建服务密钥放在单独的操作入口，成功后一次性展示并支持复制中转地址和密钥。撤销使用原生确认框，初始焦点放在“取消”。确认框按宿主能力说明影响。原生节点会停止该凭据在本节点上的现有中转，直连传输仍可继续。Cloudflare 撤销凭据会一并撤回该设备的托管 TURN 租约，使用 TURN 的传输可能需要切换路径。可勾选同时停止该凭据在本节点的分享，也可稍后停止剩余分享。支持托管 TURN 的节点单独展示设置与租约状态，统计可用时再提供用量查询。[节点管理操作](../../../consoles/relay/credentials.tsx)
4. **交付第一套页面。** 在 `consoles/` 建立 React、TypeScript、Vite 工程，共用一份 `package.json`、pnpm 锁文件和基础 TypeScript 配置。`build:relay` 把 `consoles/relay/` 的入口、静态资源和路径清单写入 `relay-rust/console/` 的专用产物目录。两个宿主通过现有 crate 的资源接口取用页面。切换时移除被替代的手写资源，配对协议仍由 Rust crate 和客户端负责。[共享资源入口](../../../relay-rust/console/src/lib.rs)

## 第二阶段 · 重写网络管理台

在 `consoles/network/` 增加独立构建入口。Authority 的 `/` 和 `/console` 提供管理页。首次部署通过 `/console#initialize=…` 创建网络管理员的用户名和密码。管理台用自己的会话读取 `/v1/admin/console`，已登录的写操作携带该会话的 CSRF token。[Authority 管理接口](../../../relay-rust/crates/network-authority/src/host/routes.rs)

1. **默认打开节点列表。** 桌面按列比较区域、公共中转策略、最近上报和目录收录情况，窄屏按同样顺序排列，完整公钥放在详情中。列表及空状态都可登记节点，填写公钥、分区和地区。服务端分别处理登记和策略修改。首次登记时启用公共中转，策略版本从 1 开始，仅 `global` 分区默认允许匿名分享。重复登记直接返回已有节点供页面打开。新节点显示“等待首次上报”，收到上报后展示时间和负载快照。目录镜像摘要核对成功后，按公钥显示“已收录”或“未收录”及目录版本。核对失败则显示“目录未核实”，登记和策略编辑仍可用。[登记与目录发布](../../../relay-rust/crates/network-authority/README.md#enroll-and-publish)

   “暂停此节点的公共中转”和“允许匿名分享”分别提交，服务端在同一事务内读取当前记录、更新所选字段并递增策略版本号。暂停前说明影响范围。节点应用策略后，匿名分享和使用本网络账号的分享都无法在此节点建立新的中转连接，已有的此类连接也会断开。使用本地凭据的分享仍可通过该节点中转。保存后显示“策略已保存，随节点下次成功上报下发”。列表标明展示的是已保存策略，不以保存成功或最近上报推断节点已执行。[节点策略下发](../../../relay-rust/crates/network-authority/src/usage.rs) · [策略作用范围](../../../relay-rust/crates/relay-native/src/node/access/mod.rs)
2. **策略设置单独编辑。** 集中展示账号总数、注册里程碑提示、注册方式、注册限流及每账号每日中转流量额度（GiB）。按账号数量切换或固定使用工作量证明时可调整难度。已配置邮件服务时可固定启用邮箱验证。表单独立保存编辑草稿，查看举报、操作节点或刷新后台快照时保留未提交的值。按账号排查用量、实时监控属于后续运营需求。[策略模型](../../../relay-rust/crates/network-authority/src/model.rs) · [后台快照](../../../relay-rust/crates/network-authority/src/governance.rs)
3. **举报直达访问限制操作。** 主导航提供举报入口。运营者查看原因后，可对举报中的分享 ID 设置或移除官方中转限制。移除成功时显示“已移除此分享的限制”，账号限制仍可能生效。按已知账号 ID 管理限制使用独立入口。页面从后台快照展示所选对象的直接限制，举报记录保留供查阅。公共 `/report` 使用独立 HTML 入口，仅加载举报表单及其依赖。`/report#share=…` 只预填公开分享 ID，举报者无需登录即可向 `/v1/reports` 提交原因。[接收页举报链接](../../../web/src/application/network/authority.ts) · [举报与访问限制](../../../relay-rust/crates/network-authority/src/governance.rs) · [账号限制的作用范围](../../../relay-rust/crates/network-authority/src/public_blocks.rs)
4. **交付网络管理台。** `build:network` 将管理页、举报页及其资源清单写入 `relay-rust/crates/network-authority/` 的专用产物目录。Worker 按清单提供资源，沿用现有内容安全策略和目录镜像来源配置，替换原手写页面。两个应用都用到的样式、表单控件和操作反馈此时再提取到 `consoles/ui/`。[Authority 页面路由](../../../relay-rust/crates/network-authority/src/host/edge.rs)

## 本地开发与发布

两个开发入口都由 Vite 提供页面。`dev:relay` 将 `/api/` 转发给本地原生节点或本地 Cloudflare 中转。原生节点以 Vite 地址作为公布的中转地址时，代理还需转发 `/.well-known/windshare` 的发现请求，并支持 `/v2/ws` 的 WebSocket 升级。`dev:network` 将 `/v1/` 转发给本地 Authority Worker。宿主公布的管理地址使用浏览器实际访问的本地 Origin，代理保留 Host、Origin 和会话 Cookie。本地联调时，用 CLI 完成配对，再仅用刚保存的节点分享文件并从接收端下载。网络管理台登录后保存一次策略。写操作经过宿主原有的校验。[节点来源检查](../../../relay-rust/crates/relay-native/src/console/http.rs) · [Worker 来源检查](../../../relay-rust/crates/network-authority/src/host/edge.rs)

**构建产物随源码入库。** 两个构建命令各自完整替换所属产物目录，前端源码与产物同批提交，干净检出后 Cargo 和原生镜像可直接嵌入管理台。控制台相关 CI 使用锁定依赖重建受影响的应用，逐文件比对清单和资源，包括新增与删除的文件。发布节点管理台前，从原生节点和 Cloudflare 中转分别打开构建后的带码审批页。发布网络管理台前，从 Authority 打开 `/console` 和 `/report`。用浏览器确认各页脚本和样式均正常加载。按交付归属调整工作流的路径过滤。节点页面变动由私有中转流程发布，网络页面变动由 Authority 流程发布。共享组件、工具链或依赖变动时重建受影响的应用，各宿主按自己的节奏发布。[原生镜像](../../../deploy/wsrelay/Dockerfile) · [私有中转发布](../../../.github/workflows/cloudflare-relay.yml) · [Authority 发布](../../../.github/workflows/network-authority.yml)

| 交付目标 | 随宿主发布的页面 |
| --- | --- |
| 原生二进制、Docker 镜像、Cloudflare `relay-only` | 节点管理台 |
| Cloudflare `full` | 节点管理台，以及由 `web/` 独立构建的接收页 |
| Cloudflare `frontend` | 接收页 |
| Network Authority Worker | 网络管理台和公共举报页 |

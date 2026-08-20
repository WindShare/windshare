# 接收失败可观测性执行计划

## 目标

浏览器在不改变接收 authority、协议行为和正常传输性能的前提下，为开发者保留可关联、可导出的失败证据：

- 默认只为用户可见失败相关的事实生成小型 `IncidentRecordV1`，同一 `IncidentScope` 的 Console 输出有界。
- 开发者显式开启后记录有界详细时间线；关闭时不构造 trace payload。
- 浏览器与 Go sender 通过共同 protocol correlation 关联，不新增线协议交互。

## 产品边界

- 浏览器诊断面向开发者，不增加诊断 UI、诊断码或普通用户操作。
- 浏览器失败历史和详细 trace 仅保存在当前页面内存；刷新或关闭页面即清除。
- incident 准入使用 closed `PresentationBoundary`、`PresentationOutcome` 和 `PresentationDecision`，分别表达呈现位置、用户可见结果与 `incident/excluded` 决定；覆盖 join、browse、preview、projection/authority、receive、lifecycle action、retained inventory/action，不得从 UI 文案、最终 state 或原始 Error 猜测。
- 只有非预期且用户可见的结果生成 incident，包括 browse/preview/inventory/action failure、`partial-directory(reason=failures)`、故障驱动的 `resumable-*`/`restart-required`、`needs-attention` 和 controller failed。取消、stale replacement、主动暂停/停止/discard、正常 expiry 和 picker 拒绝不生成；相同浏览器异常在 write、checkpoint 或 settlement 阶段仍可判定为故障。
- 每个独立呈现 attempt 创建具名 `IncidentScope`，两个 attempt 不得共享 scope 或 Console 配额；同一 scope 最多输出 `MAX_CONSOLE_REPORTS_PER_SCOPE` 条 Console 主错误，初始值为 25，超出后只累计具名的 Console suppression。
- 当前 Go CLI 的 `--trace` 继续显式开启。未来桌面应用默认写有界轻量日志，详细 trace 仍显式开启；桌面日志实现不属于本计划。
- 暂不对 protocol correlation ID 做额外 hash projection，统一使用 base64url 无填充编码。
- capability、文件名、路径、handle、内容、原始 Error、未经审查的 message 和 stack 仍禁止进入诊断产物。
- 诊断不得参与重试、lane 选择、锁、checkpoint、输出事务、清理或 lifecycle ownership。

## 语义模型

### 业务语义与诊断事实

- `Fault`、lifecycle outcome 和 authority 继续是产品控制语义的唯一来源。
- 在拥有 `Fault` 含义的边界只分类一次；同一纯分类器同时产出产品使用的 `Fault` 与诊断使用的不可变 `FailureFact`，禁止另建诊断映射。
- `FailureFact` 必须是 common envelope + closed kind payload 的 discriminated union，不得成为 flat optional-field record。Fault variant 复用已有 `Fault`；只有非 Fault 域才定义本域 closed code。
- 已认证远端 operation failure 使用内部无版本的 `ProtocolFailure`，在 protocol fact payload 中保留 `request_kind`、wire `scope/code`、`retryable`、可选 `retry_after_ms` 和 operation correlation；record projector 才生成 `ProtocolFailureV1`。它不是第二个 `Fault`，也不得被本地恢复决定改写。
- 每个 fact variant 只允许本域的 stage、closed native error class、`recovery_disposition` 和 correlation 组合；禁止通用 message、stack 和开放对象。
- typed wrapper 只保留已审查的 fact 和主因/后果关系。未知或不兼容对象在呈现边界成为 `unclassified`，不得递归检查任意 cause graph。
- 业务代码不得读取 `FailureFact` 作控制决策；诊断 consumer 失败必须被隔离。
- `IncidentScope` identity 为 closed `scope_kind + scope_sequence`；sequence 在本地单调递增，scope 状态在首个 fact 到达时才分配。
- scope owner 固定为：join、projection/authority 由 `V2ReceiverController` 持有，browse 由 `BrowserNavigationCoordinator` 持有，preview 由 `V2PreviewController` 持有，receive/lifecycle action 由 `ActiveReceiveCoordinator` 持有，retained inventory/action 由 `RetainedInventoryCoordinator` 持有。
- owner 显式传递只读 `IncidentScopeHandle` 和 failure-only `FailureFactSink`；子组件只能记录，只有 owner 能幂等关闭。禁止全局 current scope，也不得从 UI state 推断 scope；trace 关闭时 sink 仍接收已分类 fact，但不接收原始 Error。

### Failure incident

`FailureIncident` 表示一次用户可见失败，而不是一个 Error 对象：

- `trigger`：触发 failed/needs-attention 或用户可见 action failure 的主事实。
- `contributors`：同一结果中的其他独立事实，按稳定 fingerprint 聚合并保留精确计数和有界代表。
- `consequences`：settlement、detach、lane retirement 或 cleanup 等后续故障，不得替换 trigger。
- incident identity 由 `runtime_run_id + incident_sequence` 组成；去重以 `IncidentScope` 为准，不依赖 Error 对象 identity。
- fingerprint 只包含 closed semantic fields，不包含 correlation、时间或开放值；未知 code 归入 `unclassified`。聚合保留精确总数、有界 buckets/代表和精确 `overflow_fact_count`，不承诺未保留 fingerprint 的种类数。
- scope owner 在该结果已有的 settlement/cleanup 完成后关闭 scope；reporter 在 scope 关闭或具名诊断期限到达时封存 incident，用户状态发布不等待诊断。封存后的 consequence 使用只读 `IncidentLink` 生成带 `root_incident_sequence` 的关联记录，不修改旧记录。晚到关联表按数量和年龄双重有界，淘汰计入 diagnostics health。

每个呈现边界必须提交 closed `PresentationDecision`：incident 携带 `PresentationBoundary + PresentationOutcome`，excluded 携带具名排除原因；顶层 `IncidentReporter` 不重新检查 Error 或 UI state。每个 record 仅调用一次 `console.error`，同一 scope 受 Console 上限约束，并加入有界内存历史。

### Incident record

`IncidentRecordV1` 是 diagnostics record projector 生成、供 Console、inspect 和 export 共同使用的不可变 DTO；session、connectivity、transfer 和 output 域不得构造或导入它：

- 固定 allowlist 和 closed unions；所有字段可直接编码为标准 JSON。
- 不能证明小于 JavaScript safe integer 的 bigint/u64 字段使用规范十进制字符串，包括 lifecycle generation 和大计数；`uint32` 等有明确安全上限的字段保留 JSON number。
- 字符串、列表、contributors、consequences、单记录和历史均有具名上限。
- context 只读取无 IO、无锁、无副作用的内存 snapshot，并携带各自 generation；不宣称跨模块原子快照。
- 包含 schema、build、runtime、run/incident identity、sequence/time、controller/lifecycle、聚合 progress/output state、correlation 和 `diagnostics_health_at_seal`。
- `diagnostics_health_at_seal` 是该 record 的 retention/Console admission 决定完成后、调用可失败 sink 前的累计快照，分别记录 `fact_overflow`、incident history eviction、Console suppression、late-link eviction，以及 trace dropped/overwritten/sampled/coalesced；不得合并为含义不明的 `loss`。
- `DiagnosticBundleV1` 另带导出切点的 `diagnostics_health_at_export`，不得把 record 快照表述为实时 health。
- Console、内存历史和 export 分别隔离失败，产品控制流不等待它们。

### 模块边界

- `web/src/diagnostics/incident/` 封装 fact、scope、聚合、限流和有界历史；`trace/` 封装 switch 与 recorder；`export/` 封装 versioned DTO、NDJSON 和开发者 API。
- 各业务模块只定义或接收本域小型 port；`main.tsx` composition root 负责适配和装配，业务域不得依赖全局诊断 API 或 export schema。

## 浏览器详细 trace

### 动态开关

- composition root 持有一个可撤销 `TraceSwitch`；其 `current` 才是真正可选的 typed observer。
- `enable()` 立即作用于当前 receive；expiry 或 `disable()` 立即撤销 observer 并封存 capture。
- 生产者先读取 active observer，存在后才构造事件。禁止 NOOP observer 和关闭时仍构造 payload 的 wrapper。
- progress/measure 等产品 observer 与诊断 trace port 分离，关闭 trace 不得影响 UI 或业务状态。
- 各模块只接收本域小型 port，不引入全局万能事件总线。

### Recorder

- 覆盖 controller/projection、protocol operation、connectivity/peer、lane、transfer、FSA/OPFS、checkpoint、settlement 和 cleanup 的关键转换。
- 不记录逐 block 成功事件；复用已有聚合 progress，高频 observation 在 recorder 内采样或合并。
- recorder 同时受事件数量、单事件大小和总字节上限约束；不执行逐事件 Console 或持久化 IO，并记录 dropped、overwritten、sampled 和 coalesced 计数。
- 开启期间的第一个 incident 固定 pre-failure window，并继续一个有界 post-failure tail，直到 terminal、静默期限或容量上限；随后封存 capture。其他 incident 作为同一 capture 内的有界 marker，不并行维护多个 tail。

### 开发者 API

暴露冻结的 `windshareDiagnostics`：

- `enable()`：开始新的有界 trace capture、替换旧 trace，并设置自动过期；incident 历史不受影响。
- `disable()`：停止并封存当前 capture，保留到下一次 enable 或 clear。
- `status()`：返回 enabled/sealed、expiry、容量和具名 health 计数。
- `inspectLastFailure()`：返回最新 `IncidentRecordV1`。
- `export()`：导出当前状态的确定性 `DiagnosticBundleV1` NDJSON snapshot，包含 header、内存 incident 历史、可选 trace 和 `diagnostics_health_at_export`；不停止或清除 capture。
- `clear()`：清除内存 incident 历史和 trace 数据；若 trace 已开启，则从空 buffer 继续并保留原 expiry。

## 跨运行时关联

- Go 和浏览器只共享导出的 `CorrelationV1` JSON shape、公共 envelope 字段、编码规则及真正相同的 protocol 枚举；浏览器本地 output/FSA code 不进入 Go schema。
- `runtime_run_id` 只标识本地运行，不作为跨端 join key。
- 内部继续使用无版本、互不混用的 typed byte identity；只有 record projector 将其编码为 `CorrelationV1`，业务模块不得传递已字符串化的 correlation DTO。
- `protocol_session_id`、`protocol_operation_id`、`peer_path_id` 和 `peer_attempt_id` 等固定宽度 byte identity 采用共同 base64url 无填充编码；`lane_id + lane_epoch` 保持 `uint32` JSON number，并使用 Go↔TypeScript test vectors。
- sequence 和 elapsed time 各运行时独立。wall time 仅供参考，不宣称可靠的跨端全局顺序。
- 建立 session 之前或本地 continuation 阶段可能没有跨端 correlation，必须显式允许字段缺失。
- 可观测性不新增密钥派生、线协议字段或网络交互。

## Go 工作

1. 保留 `runtrace` 的非阻塞队列、progress 合并、独立 writer、loss 计数和 health summary。
2. 将序列化升级为 v3 envelope + typed payload，移除继续扩张的 flat optional-field record；复用现有 event/visitor 和 recorder 调度，不创建平行事件体系或双写兼容格式。
3. 接入共享 `CorrelationV1` 编码，禁止把本地 `run_id` 当作跨端 join key。
4. 为 sender terminal 增加 closed trigger/provenance，区分正常停止、调用方停止、远端关闭、lane retirement 和本地故障。
5. protocol operation 在产生错误的位置保留 session、operation、request、适用时的 response/lane、scope/code、authenticated retryability/retry-after 和 settlement outcome。
6. terminal send、detach 或 cleanup 失败只记录为 consequence；除非它确实触发命令终止，否则不得表述为更早接收失败的原因。
7. 不记录逐 block 成功事件，不重写 recorder queue/writer。

## 实施顺序

1. 定义 `PresentationBoundary`/`PresentationOutcome`/`PresentationDecision`、discriminated `FailureFact`/内部 `ProtocolFailure`、scope owner/handle/lifetime、fingerprint/overflow、`FailureIncident`、record projector、versioned DTO、容量策略和共同 test vectors。
2. 移除无条件 Console trace，拆分产品 observer 与诊断 port，引入动态 `TraceSwitch`，保证关闭路径不构造 payload。
3. 在已确认的分类边界一次性产出 Fault/fact，通过 scope-owned sink 保留 fact，并在每个具名呈现边界提交 `PresentationDecision`，完成聚合、封存、关联、限流和有界保留。
4. 实现 `windshareDiagnostics`、有界 recorder、pre/post failure capture 和确定性导出。
5. 升级 Go trace envelope、correlation 和 terminal provenance，保留现有 recorder 运行机制。
6. 使用两端产物重建 FSA continuation 故障，并根据证据删除无用事件。

## 验证

- 用 closed 呈现矩阵测试 join、browse、preview、projection/authority、receive、lifecycle action、retained inventory/action 的 boundary/outcome 组合、incident 准入与具名排除。
- 测试 incident 聚合、trigger/contributor/consequence、scope 去重、owner-only close、attempt 隔离、晚到 consequence 关联/淘汰、每 record 单次报告和每 scope 25 条 Console 上限。
- 测试诊断 fact 不参与业务决策，observer/reporter/export 故障不改变接收、settlement 或 cleanup。
- 验证浏览器历史仅存在于内存，reload/clear 后消失，disable 保留已封存 capture。
- 验证 trace 动态作用于当前 receive；关闭时不构造 trace payload，也不执行逐事件 trace Console/storage 工作；有界 failure incident 报告不受影响。
- 测试 FailureFact variant 约束、allowlist、closed native class、`ProtocolFailureV1` projection、fingerprint 排除 correlation、精确总数/overflow、JSON/NDJSON、不可变记录、容量上限、两个 health 快照切点和确定性导出。
- 用共同 fixtures 验证 Go 与 TypeScript 的 byte identity base64url、lane `uint32`、大整数十进制字符串、公共枚举和 envelope 编码一致。
- 验证 pre-failure window、单一有界 tail 和 cleanup consequence 不会覆盖 trigger。
- 使用大目录比较 trace 关闭/开启时的吞吐、event loop 响应性、内存和 recorder loss；交付前运行 `make ci`。

## 不包含

- 浏览器诊断 UI、持久化失败历史、跨页面恢复或浏览器硬崩溃前 trace。
- 持续全保真记录、逐 block 时间线或多个并行 failure tail。
- protocol identity 的额外 hash projection、诊断密钥派生或新线协议交互。
- 原始异常、路径、文件名、capability、handle、内容或未审查文本。
- 本计划内实现未来桌面应用的默认轻量日志。

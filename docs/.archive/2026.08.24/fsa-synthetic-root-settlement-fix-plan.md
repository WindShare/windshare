# FSA result-root 路径与结算证据修复计划

## 问题摘要

浏览器已经完成 582 个文件、6,762,858 字节的写入，最终结算却抛出 `FSA result-root settlement has invalid root evidence`，随后被归一化为 `session/output_pause/dependency_contract`。

当前代码混用了两种路径坐标：

- 逻辑产物路径包含命名结果根，例如 `["windshare", "dir"]`；
- FSA 已经预留并创建命名结果根，其物化路径应相对该根，例如同一目录为 `["dir"]`，结果根自身为 `[]`。

`artifactDirectoryPath()` 产生逻辑产物路径，FSA materializer 和 settlement 却直接把它当作根内相对路径。空路径还同时表示“仅用于认证的祖先”和“已物化的结果根”，结算器再以 `artifactPath.length === 0` 猜测根语义，导致路径坐标、目录身份和结算语义互相冲突。直接把 synthetic root 的结算预期改成 `["windshare"]` 会保留这项混用，并可能固化 `windshare/windshare` 双层目录。

`pyvenv.cfg` 的 compatible-name 恢复和发送端 `Trace is incomplete` 是独立问题，不纳入本次修复。

## 修复设计

1. 在产物布局模块中以不同类型区分逻辑产物路径与 FSA 物化相对路径。目录投影返回 `reference` 或 `materialize(relativePath)`，禁止再用空数组同时表达认证祖先和结果根。路径只在 DirectTree/FSA 边界重基一次，FSA 各层不得自行删除或补加根名称。
2. 从 `ReceiveIntent` 产生唯一的 FSA 根证据预期：
   - single-file 不产生目录根证据；
   - directory anchor 的根身份是 anchor `directoryId`；
   - synthetic-root 的根身份是 `intent.syntheticRoot`；
   - 两种 result-root 在 FSA 根内的物化路径均为 `[]`。
3. DirectTree 按根预期决定 admission：single-file 不 admission 目录；directory anchor 的祖先仅作认证引用，anchor 自身以 `[]` admission；synthetic-root 自身以 `[]` admission。
4. 让 directory admission、文件/目录物化、manifest、checkpoint 与 settlement 使用同一个 FSA 相对路径投影。逻辑产物路径只用于产物布局和用户可见诊断，不作为 FSA handle 定位路径。
5. 提升受路径坐标影响的持久化 layout/binding 版本并明确拒绝旧记录，避免旧 checkpoint、manifest 或 admission 被按新坐标静默解释；本阶段不迁移旧状态。
6. settlement 按根预期、规范化相对路径、唯一性和 finalized receipt 校验证据，不再通过路径形状重新推断根语义。
7. 根证据不匹配时记录结构化诊断，包含 operation、layout/anchor kind、预期身份与路径、实际候选和 `requireComplete`。

## 测试调整

- 使用完整 `TransferJob -> FSA admission -> materialization -> settlement` 链路覆盖 single-file、directory anchor 和 synthetic-root，不再由 fixture 手写生产路径。
- 验证物理布局只有 `report.bin`、`photos/...` 或 `windshare/...`，不得出现额外结果根层级。
- 覆盖错误目录身份、错误相对路径、重复根证据、缺失根证据和未 finalized 的完整结算。
- 覆盖 `pause -> reopen/resume -> settlement`，确认 checkpoint 和 manifest 始终使用同一相对坐标。
- 保留碰撞改名和 compatible-name 场景，确认物理根名变化或名称恢复后仍能完成最终结算。

## 实施顺序

1. 补充完整链路回归测试，分别暴露双层结果根、single-file 根 admission 和最终结算失败。
2. 提取带 `reference/materialize` 语义的 FSA 相对路径投影，统一 admission、materialization、manifest 与 checkpoint。
3. 更新持久化版本并补充暂停、重开和恢复链路。
4. 用显式根证据预期替换 settlement 的空路径推断。
5. 清理绕过生产投影的 fixture，补齐负向用例和结构化诊断断言。

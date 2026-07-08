module github.com/windshare/windshare

go 1.26.4

// 双模块发版机制(执行计划 §6.2):根模块以版本 require core,禁用 replace
// (含 replace 会令 `go install .../cmd/windshare@latest` 失败)。core 尚未发布,
// 此处暂填零值占位 pseudo-version,本地构建一律经 go.work 解析。
// 发版两步:① 打 core/vX.Y.Z tag;② 将下行升为该版本(或首次 push 后的真实
// pseudo-version),再打根模块 tag。
require github.com/windshare/windshare/core v0.0.0-00010101000000-000000000000

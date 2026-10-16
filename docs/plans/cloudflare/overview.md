
## Rust Relay

`relay-rust/` 是独立 Cargo workspace，首个 crate 为 `no_std`、无 I/O 和异步运行时依赖的 `windshare-relay-core`。`sender_auth::SenderPublicKey` 已实现严格公钥检查与验签，直接复用 Go／浏览器的 `ed25519-acceptance.json`；公钥合法不代表持有私钥或获得中转操作权限。`registration`、`stop` 与 `descriptor_upload` 解码中转收到的控制帧并编码其回复；认证消费一次性 challenge，把发送者证明转为不可复制的授权，上传授权只核对已签名摘要。字节布局、签名原文与拒绝规则由 `v2-session.json` 的共享向量约束。

`handshake::ControlHandshake` 是不做 I/O 的每连接状态机，按首帧分派注册、恢复、STOP 与 JOIN，依次处理恢复凭据、准入、challenge、证明和描述符上传。证明帧不携带 challenge ID，challenge 由所属连接按值持有、随握手截止时间过期，因此不设进程级账本；未决 challenge 数受已准入连接数约束。宿主经分享生命周期准入后才提供随机种子签发 challenge，拒绝不消耗 challenge。引擎统一使用毫秒级 `UnixMillis` 绝对时间，握手输出当前截止时间。

`make rust` 检查格式、Clippy、原生测试和内核 Wasm 构建，接入 `make check`、`make ci`、`make ci-parallel` 及 Linux/Windows GitHub CI。本地前置工具与门禁职责见 [validation.md](validation.md#rust-relay-checks)。
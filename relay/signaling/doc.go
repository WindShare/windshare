// Package signaling 实现按 shareId 的 WS hub(执行计划 §6.8):register/join
// (join 是清单的唯一获取路径)、SDP/ICE 转发、sessionId 分配、断线宽限
// 重注册(resumeToken 原像 + sealedManifest 字节一致)与信令高优先发送队列。
// WS 端点 /v1/ws/<shareId>,消息内 shareId 须与路径一致(§6.7)。
package signaling

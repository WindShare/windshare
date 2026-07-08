// Package protocol 存放信令 JSON 消息与转发帧 sessionId 包裹的共享线协议
// 类型(执行计划 §6.2/§6.7)。纯类型零依赖:transport/relay 客户端与 relay
// 服务端共同 import;不得引入第三方依赖,也不得反向依赖任何实现包。
// 线协议版本位(/v1/ws/<shareId> 的 v1)覆盖信令 JSON 与数据面帧布局。
package protocol

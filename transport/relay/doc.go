// Package relay 以 WebSocket 实现 core/session.FrameChannel(执行计划
// §6.2/§6.7):连接中转 /v1/ws/<shareId> 端点,承载信令 JSON、清单二进制帧
// 与数据面转发帧。接口在消费侧(core/session)定义,本包返回具体实现;
// 与服务端共享的线协议类型取自 relay/protocol(M1a T3.1)。
package relay

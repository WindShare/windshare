// Package forward 实现 P2P 失败时的应用层加密字节转发数据面(执行计划 §6.8):
// 按 sessionId 维护有界转发队列(容量 InFlightWindow)做每会话背压,队列满
// 即断该会话并回 ERROR;绝不因单个慢接收端停读发送端 WS。只包裹路由、
// 不解析内层帧,对密文零知识。
package forward

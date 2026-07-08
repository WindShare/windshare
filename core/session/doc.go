// Package session 定义传输抽象与块协议调度(执行计划 §6.6–§6.7)。
//
// FrameChannel/BlockStore/Sink 接口在此消费侧定义,transport/*(根模块)与
// osfs 在外层实现;调度器独占块协议(请求队列/在途/超时/重排/源评分/
// 有序交付/热切换),传输层只搬帧。数据面帧(REQUEST/BLOCK/ERROR,定长
// 小端二进制)的编解码与 MaxFrameSize 等线常量同归此包,Go↔TS 以金标向量
// 逐字节对拍。
package session

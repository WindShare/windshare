// 调度器(执行计划 §6.2/§6.6,M1b T5.2):与 Go 端 core/session 同构的块协议
// (需求集/在途/重试/热切换/有序交付模式——流式 ZIP/StreamSaver 需按序出字节);
// 数据面帧 REQUEST/BLOCK/ERROR 的小端二进制编解码与金标向量逐字节对拍。
export {};

package session

// 线常量(执行计划 §8;归属本包,root 侧 transport/relay 与 relay 服务端
// import 使用,M1-实施说明 §7)。仅线上互通常量:8 MiB/10 s 连接策略归
// 接收端编排层(web/src/connectivity)所有,不在本包(锁定契约 11)。
const (
	// MaxFrameSize 是数据面单帧上限——取 DataChannel 跨浏览器安全值;
	// BLOCK 的 payload 不得超过它,块密文按它切帧(§6.7)。
	MaxFrameSize = 64 * 1024

	// InFlightWindow 是每通道在途请求窗口(块数);中转的每会话转发队列
	// 容量亦以它为基准(§6.8)。
	InFlightWindow = 8
)

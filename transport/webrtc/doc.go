// Package webrtc 以 pion DataChannel 实现 core/session.FrameChannel
// (执行计划 §6.2,M1c T3.2):P2P 直连与热切换的传输载体。pion 等网络
// 重依赖隔离在根模块,core 保持可审计、可重实现(§6.2)。
package webrtc

// Package httpapi 提供中转的 HTTP 面(执行计划 §6.8):仅 health/config
// (config 通告支持的线协议版本与限额,供客户端预检,§6.7)与 WS Origin
// 白名单。不设 HTTP 清单端点——清单只经 WS join 获取;静态前端由站点/CDN
// 托管,与中转分离。
package httpapi

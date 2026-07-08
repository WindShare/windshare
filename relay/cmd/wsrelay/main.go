// wsrelay 是中转/信令服务器入口(执行计划 §6.8):WS hub 承载 register/join/
// 信令转发/回退数据面,HTTP 仅 health/config。二进制名取 wsrelay,避免与
// transport/relay 及过泛的 `relay` 混淆(§6.2)。M0 仅为可构建占位,
// 服务在 M1a(T2.1–T2.3)实现。
package main

func main() {}

// Package layout 实现打包流(packed stream)布局的纯算术(执行计划 §6.4)。
//
// 文件按规范化相对路径的 UTF-8 字节序排序拼成一条逻辑流;offset/streamLen
// 不入清单,双端各自按 entries 数组顺序对 size 做前缀和推导(附录 B14)。
// 提供全局块号↔文件 range 的双向映射,支撑选择性下载与按需读盘。
package layout

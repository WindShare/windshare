// Package osfs 提供真实文件系统的 FileSource/FileSink 与遍历(执行计划 §6.13)。
//
// 遍历不跟随符号链接与一切 reparse point;落盘前路径校验(safeJoin、Windows
// 保留名/非法字符、路径长度上限、.wsresume 保留前缀、折叠碰撞);按需读后
// 复核 size/mtime 快照,变更即中止(§6.3 漂移处理)。NFC 校验用
// golang.org/x/text,仅限本 IO 边缘。
package osfs

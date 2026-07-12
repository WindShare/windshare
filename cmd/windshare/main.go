// windshare 是 CLI 入口(执行计划 §6.9):`share <paths>` 仅 stat 出链接并
// 在线供块,`get <link>` 解析链接、拉清单、下载落盘。在线分享生命周期 =
// 当前进程。逻辑全部在 internal/cli(可测),main 只做 os 接线。
package main

import (
	"os"

	"github.com/windshare/windshare/cmd/windshare/internal/cli"
)

func main() { os.Exit(cli.Main()) }

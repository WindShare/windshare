package cli

import (
	"fmt"
	"io"
	"time"
)

// progressInterval 是进度行的最小刷新间隔:节流只为不刷屏,精度无语义。
const progressInterval = 500 * time.Millisecond

// progress 是简洁行式进度输出(§6.9):选中字节/速率。调用方保证
// step 串行(接收会话事件循环单线程),done 在会话结束后调用,无并发。
type progress struct {
	w              io.Writer
	totalBytes     int64
	completedBytes int64

	start         time.Time
	receivedBytes int64
	lastPrint     time.Time
	printed       bool
}

func newProgress(w io.Writer, totalBytes, completedBytes int64) *progress {
	return &progress{w: w, totalBytes: totalBytes, completedBytes: completedBytes, start: time.Now()}
}

// step records selected bytes durably materialized by one authenticated block.
func (p *progress) step(n int64) {
	p.receivedBytes += n
	current := min(p.completedBytes+p.receivedBytes, p.totalBytes)
	if time.Since(p.lastPrint) < progressInterval && current < p.totalBytes {
		return
	}
	p.lastPrint = time.Now()
	p.printed = true
	elapsed := time.Since(p.start).Seconds()
	rate := 0.0
	if elapsed > 0 {
		rate = float64(p.receivedBytes) / elapsed
	}
	// \r 原地重写:进度是瞬态信息,不值得滚动整屏日志。
	fmt.Fprintf(p.w, "\rDownloading: %s/%s, %s/s", formatBytes(current), formatBytes(p.totalBytes), formatBytes(int64(rate)))
}

// done 收尾进度行(补换行),返回累计字节与耗时供汇总。
func (p *progress) done() (bytes int64, elapsed time.Duration) {
	if p.printed {
		fmt.Fprintln(p.w)
	}
	return p.receivedBytes, time.Since(p.start)
}

// formatBytes 以二进制单位表达字节量(块大小/清单限额皆二进制习惯)。
func formatBytes(n int64) string {
	const unit = 1024
	switch {
	case n >= unit*unit*unit:
		return fmt.Sprintf("%.2f GiB", float64(n)/(unit*unit*unit))
	case n >= unit*unit:
		return fmt.Sprintf("%.2f MiB", float64(n)/(unit*unit))
	case n >= unit:
		return fmt.Sprintf("%.1f KiB", float64(n)/unit)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

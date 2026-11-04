package main

// 崩溃防护：整个程序里所有长驻 goroutine 都必须挂一个 guard。
//
// 背景：Go 里任意一个后台 goroutine panic 都会带走整个进程，
// 表现就是「测速跑到一半面板突然打不开，重新进又是空的」。
// 这里统一兜住：记一条 crash.log，然后让那条 goroutine 安静死掉，
// 其余流程照常 —— 宁可少一条流，也不能整机失联。

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync/atomic"
	"time"
)

var (
	crashMu    int32
	crashPath  string
	crashCount atomic.Int64
)

// setCrashLog 由 main 在拿到 -data-dir 后调用一次。
func setCrashLog(dir string) {
	if dir == "" {
		return
	}
	crashPath = filepath.Join(dir, "crash.log")
}

func crashRecord(where string, rec any) {
	crashCount.Add(1)
	msg := fmt.Sprintf("[%s] panic #%d @ %s: %v\n%s\n",
		time.Now().Format("2006-01-02 15:04:05"), crashCount.Load(), where, rec, debug.Stack())

	if crashPath != "" {
		// 超过 2MB 就轮转一次，避免无限长
		if fi, err := os.Stat(crashPath); err == nil && fi.Size() > 2<<20 {
			_ = os.Rename(crashPath, crashPath+".1")
		}
		if f, err := os.OpenFile(crashPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			_, _ = f.WriteString(msg)
			_ = f.Close()
		}
	}
	_, _ = os.Stderr.WriteString(msg)
}

// guard 形如 `defer guard("downloadLoop")`，放在 goroutine 第一行。
func guard(where string) {
	if r := recover(); r != nil {
		crashRecord(where, r)
	}
}

// safeGo 用于无法直接 defer 的一次性 goroutine。
func safeGo(where string, fn func()) {
	go func() {
		defer guard(where)
		fn()
	}()
}

// CrashCount 供 /api/status 暴露，前端可以提示「本次运行有过 N 次内部异常」。
func CrashCount() int64 { return crashCount.Load() }

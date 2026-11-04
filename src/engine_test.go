package main

// 传输引擎的回归测试。
//
// 这两个用例都盯着同一件事：上传流在「对端不接受 body」时的重连节奏。
// 413 / 405 / 鉴权拒绝 / Connection: close 后不等 body 就 FIN，全都长这个样子 ——
// 服务器乐意握手，但一个字节都不收。

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// newClosingListener 起一个只握手不收数据的 TCP 服务：来一条连接就立刻关掉。
func newClosingListener(t *testing.T) (net.Listener, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	var accepted atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln, &accepted
}

func newUploadEngine(t *testing.T, addr string, conc int) (*Engine, *Stats) {
	t.Helper()
	st := NewStats()
	st.Reset()
	p := &Profile{
		ID:              "upload-backoff",
		Mode:            "up",
		Concurrency:     conc,
		UploadEndpoints: []Endpoint{{URL: "http://" + addr + "/up"}},
	}
	e := NewEngine(p, st, "")
	e.Prepare()
	return e, st
}

// TestUploadLoopBacksOffWhenPeerClosesImmediately 守的是「空转烧 CPU」这条线。
//
// uploadOnce 在 EPIPE / ECONNRESET 时返回 nil（与「正常传完」同一个返回值），
// 所以循环若不在成功路径上等待，就会变成纯握手热循环：建 TCP → 写头 → 被 FIN → 立刻重来。
//
// 判据用「500ms 内被接受的连接数」：有退避时是个位数，没退避时是成千上万，
// 中间空两个数量级，不存在临界抖动。
func TestUploadLoopBacksOffWhenPeerClosesImmediately(t *testing.T) {
	ln, accepted := newClosingListener(t)
	e, _ := newUploadEngine(t, ln.Addr().String(), 0)

	if s := e.AddStream("upload"); s == nil {
		t.Fatal("没能起上传流，测试前提不成立")
	}
	time.Sleep(500 * time.Millisecond)

	e.Stop("测试结束")
	e.wg.Wait()

	if n := accepted.Load(); n > 10 {
		t.Fatalf("500ms 内重连了 %d 次：对端秒断时上传流没有退避，会一直空转烧 CPU", n)
	}
}

// TestUploadQuickResetCountsOnEndpointNotGlobally 守的是「失败该记在谁头上」。
//
// 秒断必须让用户在端点上看得见（每行的红字 + 端点失败数），但**不能**记进全局
// Failures —— 那个计数器喂的是调度器的失败风暴保护（一轮 ≥ 6 次就暂停加流），
// 而它是全局的、不分上下行。双向模式下上传端点坏掉、下载源正常时，把它算进全局
// 会让下载侧的爬坡被永久按在起始并发上，测出来的下载速度严重偏低且不报错。
func TestUploadQuickResetCountsOnEndpointNotGlobally(t *testing.T) {
	ln, _ := newClosingListener(t)
	e, st := newUploadEngine(t, ln.Addr().String(), 0)

	if s := e.AddStream("upload"); s == nil {
		t.Fatal("没能起上传流，测试前提不成立")
	}
	time.Sleep(400 * time.Millisecond)

	e.Stop("测试结束")
	e.wg.Wait()

	if len(e.ul) != 1 {
		t.Fatalf("端点表异常：期望 1 条，得到 %d", len(e.ul))
	}
	if n := e.ul[0].Failures.Load(); n == 0 {
		t.Fatal("对端一直秒断，端点的 Failures 却是 0：界面上看不出这条源在失败")
	}
	if n := st.Failures.Load(); n != 0 {
		t.Fatalf("秒断被记进了全局 Failures（%d 次）：会误伤另一个方向的爬坡", n)
	}
}

// TestAddStreamHonorsConfiguredCap 守的是「并发填多少就是多少」。
//
// 早先 profile.Concurrency 只被写进配置和回显，引擎侧一个引用都没有：
// AddStream 与调度器都只认编译期常量 maxConcurrency=64，
// 于是用户填 4、起始 4 条，然后一路爬到带宽拐点 11 条 —— 配置形同虚设。
func TestAddStreamHonorsConfiguredCap(t *testing.T) {
	ln, _ := newClosingListener(t)
	e, _ := newUploadEngine(t, ln.Addr().String(), 4)
	defer func() { e.Stop("测试结束"); e.wg.Wait() }()

	if got := e.MaxStreams(); got != 4 {
		t.Fatalf("MaxStreams() = %d，期望 4", got)
	}
	added := 0
	for i := 0; i < 10; i++ {
		if e.AddStream("upload") != nil {
			added++
		}
	}
	if added != 4 {
		t.Fatalf("上限 4，却成功加了 %d 条流", added)
	}
	if c := e.StreamCount(); c != 4 {
		t.Fatalf("StreamCount() = %d，期望 4", c)
	}
}

// TestConcurrencyZeroFallsBackToBuiltinCap 守的是「不填 = 不限制」。
// 0 必须回落到编译期上限，否则默认值一变就会静默压掉吞吐。
func TestConcurrencyZeroFallsBackToBuiltinCap(t *testing.T) {
	ln, _ := newClosingListener(t)
	e, _ := newUploadEngine(t, ln.Addr().String(), 0)
	defer func() { e.Stop("测试结束"); e.wg.Wait() }()

	if got := e.MaxStreams(); got != maxConcurrency {
		t.Fatalf("并发为 0 时应回落到 %d，实际 %d", maxConcurrency, got)
	}
	added := 0
	for i := 0; i < 10; i++ {
		if e.AddStream("upload") != nil {
			added++
		}
	}
	if added != 10 {
		t.Fatalf("未设上限时前 10 条应全部成功，实际 %d 条", added)
	}
}

// TestStartDoesNotExceedCap 守的是「开局就不能越界」。
// 起始流数写死 4，上限设成 2 时必须只起 2 条，否则用户一开始就看到超标的数字。
func TestStartDoesNotExceedCap(t *testing.T) {
	ln, _ := newClosingListener(t)
	e, _ := newUploadEngine(t, ln.Addr().String(), 2)

	n := e.Start()
	e.Stop("测试结束")
	e.wg.Wait()
	if e.sched != nil {
		e.sched.stop()
	}
	if e.res != nil {
		e.res.Stop()
	}

	if n != 2 {
		t.Fatalf("上限 2 时开局起了 %d 条流", n)
	}
	if c := e.StreamCount(); c > 2 {
		t.Fatalf("开局流数 %d 超过上限 2", c)
	}
}

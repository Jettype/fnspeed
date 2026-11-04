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

func newUploadEngine(t *testing.T, addr string) (*Engine, *Stats) {
	t.Helper()
	st := NewStats()
	st.Reset()
	p := &Profile{
		ID:              "upload-backoff",
		Mode:            "up",
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
	e, _ := newUploadEngine(t, ln.Addr().String())

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

// TestUploadLoopCountsZeroByteResetAsFailure 守的是「调度器的失败保护能不能看见它」。
//
// 早先 EPIPE 只记 Reconnects、不记 Failures，于是 scheduler 的失败风暴保护
// （burst >= 6 暂停加流）永远不触发：界面显示一切正常，CPU 却已被握手套满。
// 一个字节都没发出去就被踢，必须算失败。
func TestUploadLoopCountsZeroByteResetAsFailure(t *testing.T) {
	ln, _ := newClosingListener(t)
	e, st := newUploadEngine(t, ln.Addr().String())

	if s := e.AddStream("upload"); s == nil {
		t.Fatal("没能起上传流，测试前提不成立")
	}
	time.Sleep(400 * time.Millisecond)

	e.Stop("测试结束")
	e.wg.Wait()

	if n := st.Failures.Load(); n == 0 {
		t.Fatal("对端一直秒断，Failures 却是 0：失败保护看不见这种失败")
	}
}

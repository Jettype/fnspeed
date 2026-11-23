package main

// 结果判定：把吞吐、延迟、丢包、本机资源放在一起，给出瓶颈归属。
//
// 判定顺序刻意从「最能排除误判」的开始：
// 先看本机是否已经打满（CPU/内存），再看链路丢包与 RTT 结构，最后才怀疑目标端。

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Verdict struct {
	Level string `json:"level"` // ok | local | link | target | idle
	Text  string `json:"text"`
}

// Latency 是把网络延迟折进吞吐解释后的派生指标。
//
// 为什么要有这一层：同样是「跑不满」，1 Gbps 家宽跑到 900 Mbps 是正常，
// 跨境 200ms RTT 跑到 300 Mbps 也是正常 —— 后者卡在 TCP 的带宽时延积（BDP）
// 上，跟目标端限速长得一模一样。只看吞吐数字分不出这两种情况，
// 只能靠 RTT 反推出「每条流需要多少在途数据」，再和内核允许的单连接窗口比。
type Latency struct {
	// RTTMs 优先取目标端，其次取网关。两者都没有时为 0（未探测）。
	RTTMs    float64 `json:"rttMs"`
	JitterMs float64 `json:"jitterMs"`
	Loss     float64 `json:"loss"`

	// BDPKB 是「维持当前总吞吐所需的在途字节」= 速率 × RTT。
	BDPKB int64 `json:"bdpKb"`
	// PerStreamKB = BDP / 有效流数，即每条连接需要撑开的窗口。
	PerStreamKB int64 `json:"perStreamKb"`
	// WindowKB 是内核此刻允许的单连接接收窗口上限（tcp_rmem 第三项）。
	WindowKB int64 `json:"windowKb"`

	// Bound 是延迟侧的瓶颈判断："" 无 / "loss" 丢包 / "rtt" 窗口受限。
	Bound   string `json:"bound,omitempty"`
	Streams int    `json:"streams"`
}

const (
	// rttBoundMinRTT：低于这个 RTT，单流的窗口压力可以忽略，
	// 跑不满就只能是目标端限速或本机受限。
	rttBoundMinRTT = 60.0
	// rttBoundPerStreamKB：单流需要撑开 512KB 以上在途数据时，
	// 窗口/拥塞控制才开始明显压吞吐（默认 tcp_rmem 上限 6MB 量级）。
	rttBoundPerStreamKB = 512
	// lossBound：超过 2% 的丢包会直接把 CUBIC 的吞吐砍半，
	// 此时谈"目标端限速"没有意义。
	lossBound = 0.02
)

// socketWindowKB 读内核允许的单连接接收窗口上限。
// 读不到（非 Linux / 权限）时按 4MB 估算 —— 宁可低估，不要把窗口说话说得太满。
func socketWindowKB() int64 {
	b, err := os.ReadFile("/proc/sys/net/ipv4/tcp_rmem")
	if err != nil {
		return 4096
	}
	f := strings.Fields(string(b))
	if len(f) != 3 {
		return 4096
	}
	n, err := strconv.Atoi(f[2])
	if err != nil || n <= 0 {
		return 4096
	}
	return int64(n) / 1024
}

// deriveLatency 把 ping 结果与当前吞吐折成 Latency。
// 只做派生，不改任何计数 —— 它可以随时被重算，不影响统计口径。
func deriveLatency(s *Snapshot) Latency {
	t, g := s.Ping.Target, s.Ping.Gateway
	l := Latency{JitterMs: t.Jitter, Loss: t.Loss, WindowKB: socketWindowKB()}
	if t.AvgRTT > 0 {
		l.RTTMs = t.AvgRTT
	} else if g.AvgRTT > 0 {
		l.RTTMs = g.AvgRTT
		l.JitterMs, l.Loss = g.Jitter, g.Loss
	}
	l.Streams = s.Concurrency
	if l.Streams <= 0 {
		l.Streams = 1
	}
	rate := s.RecvRate + s.SentRate
	if l.RTTMs <= 0 || rate <= 0 {
		return l
	}
	bdp := float64(rate) * l.RTTMs / 1000 // 字节
	l.BDPKB = int64(bdp / 1024)
	l.PerStreamKB = l.BDPKB / int64(l.Streams)
	switch {
	case l.Loss >= lossBound:
		l.Bound = "loss"
	case l.RTTMs >= rttBoundMinRTT && l.PerStreamKB >= rttBoundPerStreamKB:
		l.Bound = "rtt"
	}
	return l
}

// Judge 根据当前快照给出结论。
func Judge(snap Snapshot) Verdict {
	net := snap.RecvRate + snap.SentRate

	if !snap.Running && snap.RecvTotal == 0 && snap.SentTotal == 0 {
		// 区分「从没点过开始」和「跑了但一个字节都没拿到」——
		// 后者才是用户最需要解释的情况，不能笼统说成「尚未运行」。
		if snap.ElapsedSec < 0.5 && snap.Failures == 0 {
			return Verdict{Level: "idle", Text: "尚未运行"}
		}
		return Verdict{Level: "target", Text: fmt.Sprintf("跑了 %.0f 秒、%d 次失败，一个字节都没拿到：目标端拒绝或连不上，看下方端点状态里的错误，或点「网络自检」定位到具体哪一层", snap.ElapsedSec, snap.Failures)}
	}

	// 1. 本机瓶颈
	if snap.Res.CPU >= 85 {
		return Verdict{Level: "local", Text: fmt.Sprintf("本机 CPU 已到 %.0f%%，先限速或减少并发，否则测的是 NAS 不是链路", snap.Res.CPU)}
	}
	if snap.Res.MemUsed >= 92 {
		return Verdict{Level: "local", Text: fmt.Sprintf("内存占用 %.0f%%，可能触发换页，结果不可信", snap.Res.MemUsed)}
	}

	// 2. 全部失败：必须「一个字节都没收到」才算。
	//    注意不能用瞬时速率判断——刚跑完那一刻速率必然归零，
	//    只要累计流量 > 0 就说明是跑通了的。
	if snap.Failures > 0 && snap.RecvTotal == 0 && snap.SentTotal == 0 && snap.ElapsedSec > 3 {
		return Verdict{Level: "target", Text: "所有流都失败，一个字节都没收到，目标端拒绝或不可达，先看下方 URL 状态里的错误"}
	}

	// 3. 链路：丢包优先于 RTT。丢包会把拥塞窗口反复砍半，
	//    这时无论 RTT 高低，吞吐都上不去，加并发只会加剧重传。
	lt := snap.Latency
	if lt.Loss >= lossBound && lt.RTTMs > 0 {
		return Verdict{Level: "link", Text: fmt.Sprintf("到目标丢包 %.1f%%（RTT %.0fms），吞吐会反复回退，先解决链路再加并发", lt.Loss*100, lt.RTTMs)}
	}
	if t := snap.Ping.Target; t.Sent >= 3 && t.Jitter > 60 && t.Loss > 0.01 && t.AvgRTT > 0 {
		return Verdict{Level: "link", Text: fmt.Sprintf("RTT 抖动 %.0fms（均值 %.0fms）且伴随丢包，链路不稳", t.Jitter, t.AvgRTT)}
	}

	// 4. 延迟造成的窗口受限：只在「并发已经封顶（steady）」时报，
	//    爬坡阶段的 RTT 结论没有意义（流数还在变，BDP 每轮都在重算）。
	if lt.Bound == "rtt" && snap.Phase == "steady" {
		return Verdict{Level: "link", Text: fmt.Sprintf(
			"RTT %.0fms：当前 %.0f Mbps 下每条流需要在途 %.1f MB（BDP），单流已被窗口限制。继续加并发有效，加时长无效；源离得近、RTT 降到 60ms 以内则不需要这么多流",
			lt.RTTMs, float64(net*8)/1e6, float64(lt.PerStreamKB)/1024)}
	}

	// 5. 掉速补偿频繁触发，说明目标端在限速
	if snap.Compensations >= 3 && snap.Phase == "steady" {
		return Verdict{Level: "target", Text: fmt.Sprintf("已触发 %d 次掉速补偿，目标端疑似对单 IP 限速，再加流收益有限", snap.Compensations)}
	}

	// 6. 跑完后的衰减判断：稳定值远低于峰值，说明中途被限速/链路回退
	if snap.Phase == "done" {
		steady := snap.SteadyRecv + snap.SteadySent
		peak := snap.PeakRecv + snap.PeakSent
		if peak > 0 && steady > 0 && float64(steady)/float64(peak) < 0.6 {
			return Verdict{Level: "target", Text: fmt.Sprintf("稳定值只有峰值的 %.0f%%，目标端在限速或链路中途回退", float64(steady)/float64(peak)*100)}
		}
		text := "跑完，数值稳定；上限由目标端与运营商共同决定"
		if lt.RTTMs > 0 {
			text = fmt.Sprintf("跑完，数值稳定；RTT %.0fms、丢包 %.1f%%，上限由目标端与运营商共同决定", lt.RTTMs, lt.Loss*100)
		}
		return Verdict{Level: "ok", Text: text}
	}

	// 运行中的零星失败是正常的（服务端分块结束会主动断），不据此下结论
	if snap.Failures > 0 && snap.RecvTotal+snap.SentTotal > 0 && snap.Running && net == 0 {
		return Verdict{Level: "target", Text: fmt.Sprintf("%d 次失败且当前无吞吐，目标端部分端点不稳，已自动 failover", snap.Failures)}
	}

	if snap.Phase == "ramp" {
		return Verdict{Level: "ok", Text: "爬坡中，前 3 秒不计入稳定值"}
	}
	if lt.RTTMs > 0 {
		return Verdict{Level: "ok", Text: fmt.Sprintf("链路上限由目标端与运营商共同决定，数值稳定（RTT %.0fms · 抖动 %.0fms）", lt.RTTMs, lt.JitterMs)}
	}
	return Verdict{Level: "ok", Text: "链路上限由目标端与运营商共同决定，数值稳定"}
}

package main

// 结果判定：把吞吐、丢包、抖动、本机资源放在一起，给出瓶颈归属。
//
// 判定顺序刻意从「最能排除误判」的开始：
// 先看本机是否已经打满（CPU/内存），再看链路丢包，最后才怀疑目标端。

import "fmt"

type Verdict struct {
	Level string `json:"level"` // ok | local | link | target | idle
	Text  string `json:"text"`
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

	// 3. 链路
	t := snap.Ping.Target
	// 阈值放宽：满速跑的时候握手 RTT 本身就会抖，
	// 只有「又丢又抖」才敢说是链路问题，否则容易把目标端限速误判成链路。
	if t.Sent >= 3 {
		if t.Loss > 0.05 {
			return Verdict{Level: "link", Text: fmt.Sprintf("到目标丢包 %.1f%%，吞吐会反复回退，先解决链路", t.Loss*100)}
		}
		if t.Jitter > 60 && t.Loss > 0.01 && t.AvgRTT > 0 {
			return Verdict{Level: "link", Text: fmt.Sprintf("RTT 抖动 %.0fms（均值 %.0fms）且伴随丢包，链路不稳", t.Jitter, t.AvgRTT)}
		}
	}

	// 4. 掉速补偿频繁触发，说明目标端在限速
	if snap.Compensations >= 3 && snap.Phase == "steady" {
		return Verdict{Level: "target", Text: fmt.Sprintf("已触发 %d 次掉速补偿，目标端疑似对单 IP 限速，再加流收益有限", snap.Compensations)}
	}

	// 5. 跑完后的衰减判断：稳定值远低于峰值，说明中途被限速/链路回退
	if snap.Phase == "done" {
		steady := snap.SteadyRecv + snap.SteadySent
		peak := snap.PeakRecv + snap.PeakSent
		if peak > 0 && steady > 0 && float64(steady)/float64(peak) < 0.6 {
			return Verdict{Level: "target", Text: fmt.Sprintf("稳定值只有峰值的 %.0f%%，目标端在限速或链路中途回退", float64(steady)/float64(peak)*100)}
		}
		return Verdict{Level: "ok", Text: "跑完，数值稳定；上限由目标端与运营商共同决定"}
	}

	// 运行中的零星失败是正常的（服务端分块结束会主动断），不据此下结论
	if snap.Failures > 0 && snap.RecvTotal+snap.SentTotal > 0 && snap.Running && net == 0 {
		return Verdict{Level: "target", Text: fmt.Sprintf("%d 次失败且当前无吞吐，目标端部分端点不稳，已自动 failover", snap.Failures)}
	}

	if snap.Phase == "ramp" {
		return Verdict{Level: "ok", Text: "爬坡中，前 3 秒不计入稳定值"}
	}
	return Verdict{Level: "ok", Text: "链路上限由目标端与运营商共同决定，数值稳定"}
}

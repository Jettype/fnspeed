package main

// 调度器的回归测试：爬坡状态机、掉速补偿、以及窗口统计。
//
// 这是整个测速策略的核心，之前一条测试都没有 —— 改爬坡逻辑等于蒙眼改。
// 这里守住的几条不变量：
//   * 头 4 秒不决策（TCP 慢启动期的样本没有参考价值）
//   * 加流后要等满 4 秒才比较增益，否则新连接还在慢启动就被判定「无增益」
//   * 增益 ≥5% 视为有效，连续两轮无增益才锁定拐点
//   * 巡航期跌破拐点 75% 才补偿，且 4 秒内不连环触发
//   * 补偿后拐点最多砸到 85% 水位，不能一次见底（否则下一轮又触发）
//
// 所有用例都不联网：没有可用下载源时 pickKind 返回空、AddStream 返回 nil，
// 正是「加到上限」的真实分支。

import (
	"context"
	"math"
	"testing"
	"time"
)

// newSchedEngine 造一个不联网的 Engine：没有任何可用端点，
// 因此 AddStream 永远返回 nil，不会起 goroutine 发请求。
func newSchedEngine(t *testing.T) (*Engine, *scheduler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st := NewStats()
	st.SetStartAt(time.Now().Add(-10 * time.Second)) // 越过 4 秒慢启动门槛
	st.Running.Store(true)

	e := &Engine{
		profile: &Profile{Mode: "down"},
		stats:   st,
		ctx:     ctx,
		cancel:  cancel,
		streams: make(map[int]*stream),
	}
	sch := newScheduler(e)
	e.sched = sch

	t.Cleanup(func() {
		cancel()
		sch.stop()
	})
	return e, sch
}

// seedStreams 往引擎里塞若干条假流，rate 单位 B/s。
// cancel 给成空函数，避免 RestartStream 调 nil 崩掉。
func seedStreams(e *Engine, rates ...int64) []*stream {
	out := make([]*stream, 0, len(rates))
	e.mu.Lock()
	for i, r := range rates {
		s := &stream{id: i + 1, kind: "download", ctx: e.ctx, cancel: func() {}, born: time.Now()}
		s.rate.Store(r)
		e.streams[s.id] = s
		out = append(out, s)
	}
	e.mu.Unlock()
	e.conc.Store(int32(len(rates)))
	return out
}

// setPoints 用一个落在 3 秒窗口内的采样点把当前吞吐设成 v。
func setPoints(s *scheduler, now time.Time, v int64) {
	s.points = []ratePoint{{t: now.Add(-time.Second), recv: v}}
}

// ---------------- 窗口统计 ----------------

func TestAvgWindowSelectsRange(t *testing.T) {
	now := time.Now()
	s := &scheduler{lastBytes: map[int]int64{}, done: make(chan struct{})}
	s.points = []ratePoint{
		{t: now.Add(-10 * time.Second), recv: 999999}, // 远在窗口外
		{t: now.Add(-3 * time.Second), recv: 1000, sent: 1000},
		{t: now.Add(-2 * time.Second), recv: 2000},
		{t: now.Add(-1 * time.Second), recv: 3000}, // 最后一点即「现在」
	}

	// 窗口取「最后一点往前 3 秒」，即 (-4s, -1s]，命中后三个点
	want := float64(2000+2000+3000) / 3
	if got := s.avgWindow(0, 3*time.Second); math.Abs(got-want) > 0.01 {
		t.Fatalf("窗口均值 %.2f，期望 %.2f", got, want)
	}
}

func TestAvgWindowEmpty(t *testing.T) {
	s := &scheduler{lastBytes: map[int]int64{}, done: make(chan struct{})}
	if got := s.avgWindow(0, 3*time.Second); got != 0 {
		t.Fatalf("无采样点应返回 0，实际 %.2f", got)
	}
}

func TestMaxInPicksHighestWithinWindow(t *testing.T) {
	now := time.Now()
	s := &scheduler{lastBytes: map[int]int64{}, done: make(chan struct{})}
	s.points = []ratePoint{
		{t: now.Add(-20 * time.Second), recv: 999999}, // 窗口外的大值，不能算进来
		{t: now.Add(-5 * time.Second), recv: 500, sent: 100},
		{t: now.Add(-1 * time.Second), recv: 700, sent: 100},
	}
	if got := s.maxIn(10 * time.Second); got != 800 {
		t.Fatalf("窗口最大值 %d，期望 800", got)
	}
}

func TestMedian(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		want float64
	}{
		{"空", nil, 0},
		{"单个", []float64{5}, 5},
		{"奇数个", []float64{3, 1, 2}, 2},
		{"偶数个取中间均值", []float64{4, 1, 3, 2}, 2.5},
		{"含重复", []float64{7, 7, 1}, 7},
	}
	for _, c := range cases {
		if got := median(c.in); got != c.want {
			t.Errorf("%s：median(%v) = %v，期望 %v", c.name, c.in, got, c.want)
		}
	}

	in := []float64{3, 1, 2}
	_ = median(in)
	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Errorf("median 不该修改入参，实际 %v", in)
	}
}

// ---------------- 采样 ----------------

func TestSampleComputesWireRate(t *testing.T) {
	e, s := newSchedEngine(t)
	now := time.Now()

	e.stats.RecvBytes.Store(1000)
	if p := s.sample(now); p.recv != 0 {
		t.Fatalf("首个采样点没有基线，速率应为 0，实际 %d", p.recv)
	}

	e.stats.RecvBytes.Store(1500)
	e.stats.SentBytes.Store(300)
	p := s.sample(now.Add(time.Second))
	if p.recv != 500 {
		t.Fatalf("下行速率 %d，期望 500 B/s", p.recv)
	}
	if p.sent != 300 {
		t.Fatalf("上行速率 %d，期望 300 B/s", p.sent)
	}
}

func TestSampleKeepsLast60Points(t *testing.T) {
	e, s := newSchedEngine(t)
	now := time.Now()
	for i := 0; i < 70; i++ {
		e.stats.RecvBytes.Add(500)
		s.sample(now.Add(time.Duration(i) * time.Second))
	}
	if len(s.points) != 60 {
		t.Fatalf("保留点数 %d，期望 60（30 秒窗口）", len(s.points))
	}
}

func TestSampleComputesPerStreamRate(t *testing.T) {
	e, s := newSchedEngine(t)
	streams := seedStreams(e, 0)
	now := time.Now()

	s.sample(now)
	streams[0].bytes.Store(1024)
	s.sample(now.Add(time.Second))

	if got := streams[0].rate.Load(); got != 1024 {
		t.Fatalf("单流速率 %d，期望 1024 B/s", got)
	}
}

func TestSampleClampsNegativeStreamRate(t *testing.T) {
	e, s := newSchedEngine(t)
	streams := seedStreams(e, 0)
	streams[0].bytes.Store(5000)
	now := time.Now()

	s.sample(now)
	streams[0].bytes.Store(100) // 计数器回退（重连后重置），不能算出负速率
	s.sample(now.Add(time.Second))

	if got := streams[0].rate.Load(); got != 0 {
		t.Fatalf("计数器回退时速率应归 0，实际 %d", got)
	}
}

// ---------------- 爬坡 ----------------

func TestEvaluateIgnoredDuringSlowStart(t *testing.T) {
	e, s := newSchedEngine(t)
	now := time.Now()
	e.stats.SetStartAt(now.Add(-2 * time.Second)) // 还在头 4 秒里
	setPoints(s, now, 5e6)

	s.evaluate(ratePoint{}, now)

	if s.locked || s.probing {
		t.Fatal("慢启动期内不该做任何决策")
	}
}

func TestEvaluateLocksWhenNoStreamCanBeAdded(t *testing.T) {
	e, s := newSchedEngine(t)
	now := time.Now()
	setPoints(s, now, 5e6)

	s.evaluate(ratePoint{}, now)

	if !s.locked {
		t.Fatal("加不出新流时应锁定拐点，否则每 2 秒空转")
	}
	if s.capRate != 5e6 {
		t.Fatalf("拐点 %.0f，期望 5e6", s.capRate)
	}
	if got := e.Phase(); got != "steady" {
		t.Fatalf("阶段 %q，期望 steady", got)
	}
}

func TestEvaluateLocksAtMaxConcurrency(t *testing.T) {
	e, s := newSchedEngine(t)
	rates := make([]int64, maxConcurrency)
	for i := range rates {
		rates[i] = 1000
	}
	seedStreams(e, rates...)

	now := time.Now()
	setPoints(s, now, 1e8)
	s.evaluate(ratePoint{}, now)

	if !s.locked {
		t.Fatalf("%d 条流已达上限，应直接锁定", maxConcurrency)
	}
}

func TestEvaluateFailureBurstPausesRamp(t *testing.T) {
	e, s := newSchedEngine(t)
	now := time.Now()
	setPoints(s, now, 5e6)
	e.stats.Failures.Store(10) // 目标端成片拒绝

	s.evaluate(ratePoint{}, now)

	if s.locked {
		t.Fatal("失败风暴只该暂停这一轮，不该永久锁定")
	}
	if n := e.StreamCount(); n != 0 {
		t.Fatalf("失败风暴中不该加流，实际加了 %d 条", n)
	}
}

func TestEvaluateProbingWaitsForSlowStartToEnd(t *testing.T) {
	_, s := newSchedEngine(t)
	now := time.Now()
	setPoints(s, now, 10e6)
	s.probing = true
	s.probeAt = now.Add(-1 * time.Second) // 新连接才起来 1 秒
	s.baseline = 10e6

	s.evaluate(ratePoint{}, now)

	if !s.probing {
		t.Fatal("不足 4 秒就结束探测，会把慢启动误判成无增益")
	}
	if s.noGain != 0 {
		t.Fatalf("无增益计数 %d，期望 0（这一轮不该计入）", s.noGain)
	}
}

func TestEvaluateProbingGainResetsNoGain(t *testing.T) {
	_, s := newSchedEngine(t)
	now := time.Now()
	setPoints(s, now, 11e6) // 比基线涨 10%
	s.probing = true
	s.probeAt = now.Add(-5 * time.Second)
	s.baseline = 10e6
	s.noGain = 1

	s.evaluate(ratePoint{}, now)

	if s.noGain != 0 {
		t.Fatalf("增益 10%% 应把无增益计数清零，实际 %d", s.noGain)
	}
	if s.locked {
		t.Fatal("还有增益就不该锁定")
	}
}

func TestEvaluateProbingNoGainCounts(t *testing.T) {
	_, s := newSchedEngine(t)
	now := time.Now()
	setPoints(s, now, 10e6)
	s.probing = true
	s.probeAt = now.Add(-5 * time.Second)
	s.baseline = 10e6

	s.evaluate(ratePoint{}, now)

	if s.noGain != 1 {
		t.Fatalf("无增益计数 %d，期望 1", s.noGain)
	}
	if s.locked {
		t.Fatal("只丢一轮增益还不能锁定，要给下一批流机会")
	}
}

func TestEvaluateProbingSecondLossLocks(t *testing.T) {
	e, s := newSchedEngine(t)
	now := time.Now()
	setPoints(s, now, 10e6)
	s.probing = true
	s.probeAt = now.Add(-5 * time.Second)
	s.baseline = 10e6
	s.noGain = 1 // 上一轮已经没涨

	s.evaluate(ratePoint{}, now)

	if !s.locked {
		t.Fatal("连续两轮无增益应锁定拐点")
	}
	if s.capRate != 10e6 {
		t.Fatalf("拐点 %.0f，期望 10e6", s.capRate)
	}
	if got := e.Phase(); got != "steady" {
		t.Fatalf("阶段 %q，期望 steady", got)
	}
}

// ---------------- 补偿 ----------------

func TestCompensateDropsSlowStreams(t *testing.T) {
	e, s := newSchedEngine(t)
	seedStreams(e, 100, 10000, 10000, 10000) // 一条远低于中位数一半

	s.compensate()

	if got := e.stats.SlowDropped.Load(); got != 1 {
		t.Fatalf("剔除慢流 %d 条，期望 1", got)
	}
	if got := e.stats.Compensations.Load(); got != 1 {
		t.Fatalf("补偿次数 %d，期望 1", got)
	}
}

func TestCompensateRestartsHalfWhenAllStalled(t *testing.T) {
	e, s := newSchedEngine(t)
	seedStreams(e, 0, 0, 0, 0) // 全部没跑起来

	s.compensate()

	if got := e.stats.Compensations.Load(); got != 1 {
		t.Fatalf("补偿次数 %d，期望 1", got)
	}
	if got := e.stats.SlowDropped.Load(); got != 0 {
		t.Fatalf("全停滞时不该记「慢流」，实际 %d", got)
	}
}

func TestCompensateAddsStreamWhenAllHealthy(t *testing.T) {
	e, s := newSchedEngine(t)
	seedStreams(e, 10000, 10000, 10000, 10000)

	s.compensate()

	// 没有可用源，AddStream 返回 nil，因此既不加流也不计一次补偿
	if got := e.stats.Compensations.Load(); got != 0 {
		t.Fatalf("补偿次数 %d，期望 0", got)
	}
}

func TestCompensateWithoutStreamsIsNoop(t *testing.T) {
	e, s := newSchedEngine(t)
	s.compensate()
	if got := e.stats.Compensations.Load(); got != 0 {
		t.Fatalf("没有流时不该有动作，实际补偿 %d 次", got)
	}
}

func TestEvaluateCompensatesOnDropAndKeepsFloor(t *testing.T) {
	e, s := newSchedEngine(t)
	seedStreams(e, 10000, 10000, 10000, 10000)
	s.locked = true
	s.capRate = 10e6

	now := time.Now()
	setPoints(s, now, 5e6) // 掉到拐点的一半
	s.evaluate(ratePoint{}, now)

	// 承认新水位，但地板是 10e6 * 0.85，不能一次砸到 5e6
	if want := 8.5e6; s.capRate != want {
		t.Fatalf("补偿后拐点 %.0f，期望 %.0f（地板 85%%）", s.capRate, want)
	}
	if s.lastComp.IsZero() {
		t.Fatal("补偿后必须记录时刻，否则会连环触发")
	}
}

func TestEvaluateDoesNotCompensateWithinFourSeconds(t *testing.T) {
	e, s := newSchedEngine(t)
	seedStreams(e, 10000, 10000, 10000, 10000)
	s.locked = true
	s.capRate = 10e6

	now := time.Now()
	s.lastComp = now.Add(-1 * time.Second)
	setPoints(s, now, 5e6)
	s.evaluate(ratePoint{}, now)

	if got := e.stats.Compensations.Load(); got != 0 {
		t.Fatalf("4 秒内不该重复补偿，实际补了 %d 次", got)
	}
	if s.capRate != 10e6 {
		t.Fatalf("被节流时不改拐点，实际 %.0f", s.capRate)
	}
}

func TestEvaluateToleratesSmallDip(t *testing.T) {
	e, s := newSchedEngine(t)
	seedStreams(e, 10000, 10000, 10000, 10000)
	s.locked = true
	s.capRate = 10e6

	now := time.Now()
	setPoints(s, now, 8e6) // 只掉了 20%，没跌破 75%
	s.evaluate(ratePoint{}, now)

	if got := e.stats.Compensations.Load(); got != 0 {
		t.Fatalf("小幅波动不该补偿，实际 %d 次", got)
	}
}

func TestEvaluateRaisesCapWhenThroughputGrows(t *testing.T) {
	e, s := newSchedEngine(t)
	seedStreams(e, 10000, 10000, 10000, 10000)
	s.locked = true
	s.capRate = 5e6

	now := time.Now()
	setPoints(s, now, 8e6) // 别的设备让出带宽
	s.evaluate(ratePoint{}, now)

	if s.capRate != 8e6 {
		t.Fatalf("吞吐涨过拐点时水位上移，实际 %.0f", s.capRate)
	}
}

func TestEvaluateCoverGapIsThrottled(t *testing.T) {
	e, s := newSchedEngine(t)
	// 有源可用但一条流都没分到 —— coverGap 应为真
	e.dl = []*URLState{{Status: StatusOK}}
	seedStreams(e, 100, 2000)

	s.locked = true
	s.capRate = 10e6
	now := time.Now()
	s.lastCover = now.Add(-1 * time.Second) // 距上次补漏不足 12 秒
	setPoints(s, now, 10e6)

	s.evaluate(ratePoint{}, now)

	if got := e.stats.Compensations.Load(); got != 0 {
		t.Fatalf("12 秒内不该重复挪流，实际 %d 次", got)
	}
}

// 覆盖补漏的前提条件本身：只有「可用、非小文件、且没有流」的源才算漏。
func TestEngineCoverGap(t *testing.T) {
	e, _ := newSchedEngine(t)
	if e.coverGap() {
		t.Fatal("没有源时不该报漏")
	}

	ok := &URLState{Status: StatusOK}
	e.dl = []*URLState{ok}
	if !e.coverGap() {
		t.Fatal("有可用且空闲的源，应报漏")
	}

	ok.active.Store(1)
	if e.coverGap() {
		t.Fatal("源上已有流，不算漏")
	}

	ok.active.Store(0)
	ok.Short.Store(true)
	if e.coverGap() {
		t.Fatal("小文件不参与覆盖补漏")
	}
}

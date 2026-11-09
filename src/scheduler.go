package main

// 调度器：负责「爬坡」与「掉速补偿」，是整个测速策略的核心。
//
// 爬坡（贪心加流，不设时间上限）：
//   起始 4 流。每 2s 采样一次 3s 均速，加一批流后等 4s（新连接慢启动）再比较；
//   只要吞吐较加流前增长 ≥5%，就继续加，加到 64 流或连续两轮无增益为止。
//   旧实现「10 秒后强制锁定」会让千兆链路永远停在 6 条流上，已废弃。
//
// 补偿：锁定后若吞吐跌到拐点的 75% 以下，先剔除速率低于中位数 50% 的慢流
//       并重开连接；若所有流健康仍然跌，则 +1 流重置拥塞窗口。
//       反过来，若吞吐涨过拐点 25%（别的设备让出了带宽），解锁重爬。

import (
	"sort"
	"sync"
	"time"
)

type ratePoint struct {
	t       time.Time
	recv    int64 // B/s
	sent    int64 // B/s
	streams int
}

type scheduler struct {
	e *Engine

	mu        sync.Mutex
	points    []ratePoint
	lastBytes map[int]int64
	lastTick  time.Time
	lastRec   int64
	lastSen   int64

	// 爬坡状态机
	baseline  float64   // 本批加流前的 3s 均速
	probing   bool      // 刚加过流，正在等慢启动结束
	probeAt   time.Time // 本批加流的时刻
	noGain    int       // 连续无增益轮数
	locked    bool      // 已到拐点，进入巡航
	capRate   float64   // 拐点处的吞吐（B/s）
	lastComp  time.Time // 上次补偿时刻，防止抖动式连环触发
	lastCover time.Time // 上次「覆盖补漏」时刻
	lastFail  int64     // 上次看到的累计失败数，用于失败率保护

	done chan struct{}
	once sync.Once
}

func newScheduler(e *Engine) *scheduler {
	return &scheduler{e: e, lastBytes: make(map[int]int64), done: make(chan struct{})}
}

func (s *scheduler) stop() {
	s.once.Do(func() { close(s.done) })
}

func (s *scheduler) run() {
	defer guard("scheduler")
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	lastEval := time.Now()

	for {
		select {
		case <-s.done:
			return
		case <-s.e.ctx.Done():
			return
		case now := <-t.C:
			p := s.sample(now)
			if now.Sub(lastEval) >= 2*time.Second {
				s.evaluate(p, now)
				lastEval = now
			}
		}
	}
}

func (s *scheduler) sample(now time.Time) ratePoint {
	rec := s.e.stats.RecvBytes.Load()
	sen := s.e.stats.SentBytes.Load()

	s.mu.Lock()
	defer s.mu.Unlock()

	var rr, rs int64
	if !s.lastTick.IsZero() {
		if dt := now.Sub(s.lastTick).Seconds(); dt > 0 {
			rr = int64(float64(rec-s.lastRec) / dt)
			rs = int64(float64(sen-s.lastSen) / dt)
		}
	}
	dt := 0.0
	if !s.lastTick.IsZero() {
		dt = now.Sub(s.lastTick).Seconds()
	}
	s.lastTick, s.lastRec, s.lastSen = now, rec, sen

	// 每流速率：调度器独占这些写入，worker 只写 bytes
	streams := s.e.Streams()
	live := make(map[int]struct{}, len(streams))
	for _, st := range streams {
		live[st.id] = struct{}{}
		b := st.bytes.Load()
		if prev, ok := s.lastBytes[st.id]; ok && dt > 0 {
			v := int64(float64(b-prev) / dt)
			if v < 0 {
				v = 0
			}
			st.rate.Store(v)
		}
		s.lastBytes[st.id] = b
	}
	// 退出的流必须从 map 里摘掉。RestartStream 每次都给新 id，不清理的话
	// 一天的补偿下来这里能攒出几十万条（每条约 40 字节 + map 开销），
	// 而且每条都要参与遍历 —— 表面看是「跑久了内存缓慢上涨、调度变慢」。
	for id := range s.lastBytes {
		if _, ok := live[id]; !ok {
			delete(s.lastBytes, id)
		}
	}

	p := ratePoint{t: now, recv: rr, sent: rs, streams: s.e.StreamCount()}
	s.points = append(s.points, p)
	if len(s.points) > 60 { // 30 秒
		s.points = s.points[len(s.points)-60:]
	}
	return p
}

func (s *scheduler) evaluate(p ratePoint, now time.Time) {
	elapsed := now.Sub(s.e.stats.StartAt()).Seconds()
	// 头 4 秒是 TCP 慢启动期，样本没有参考价值；同时等至少两条采样点落盘。
	if elapsed < 4 {
		return
	}

	cur := s.avgWindow(0, 3*time.Second)
	if cur <= 0 {
		// 完全没吞吐：由引擎的 stall 检测负责重连/终止，这里不参与决策。
		return
	}
	n := s.e.StreamCount()

	if s.probing {
		if now.Sub(s.probeAt) < 4*time.Second {
			return // 新连接还在慢启动，此刻比较会误判为「无增益」
		}
		s.probing = false
		gain := (cur - s.baseline) / s.baseline
		if gain >= 0.05 {
			s.noGain = 0
		} else {
			s.noGain++
		}
		// 还没点亮全部源：哪怕这一批没有增益，也再补一条。
		// 带宽早就饱和时总吞吐不会涨，但用户要看到的是「每条源都参与了」，
		// 否则列表末尾几条永远 0 字节，看着像挂了一样。
		if s.e.coverGap() && s.e.StreamCount() < s.e.MaxStreams() {
			if s.e.AddStream(s.e.pickKind()) != nil {
				s.baseline = cur
				s.probing = true
				s.probeAt = now
				s.noGain = 0
				return
			}
		}
		if s.noGain >= 2 || n >= s.e.MaxStreams() {
			s.locked = true
			s.capRate = cur
			s.e.SetPhase("steady")
		}
		return
	}

	if !s.locked {
		// 失败率保护：目标端成片拒绝/断连时，加流只会把失败放大。
		// 注意只「暂停这一轮」，不永久锁定 —— 开局几个 TLS 抖动
		// 也会攒出几次失败，锁死就再也爬不上去了。
		fail := s.e.stats.Failures.Load()
		burst := fail - s.lastFail
		s.lastFail = fail
		if burst >= 6 {
			return
		}

		if n >= s.e.MaxStreams() {
			s.locked = true
			s.capRate = cur
			s.e.SetPhase("steady")
			return
		}
		s.addBatch(cur, now)
		return
	}

	// ---- 巡航期 ----
	// 覆盖补漏：并发被带宽拐点卡住时，流数可能少于源数，于是列表末尾几条源
	// 从头到尾一条流都分不到，看起来就是「只有前几条在发力」。
	// 每 12 秒把最慢的一条流挪给没覆盖的源，慢跑一遍也能把坐标点亮。
	if s.e.coverGap() && now.Sub(s.lastCover) > 12*time.Second {
		if slow := s.e.slowestDL(); slow != nil {
			s.e.RestartStream(slow)
			s.lastCover = now
			s.e.stats.Compensations.Add(1)
		}
	}
	if cur > s.capRate {
		s.capRate = cur // 水位上移，后续掉速按新峰值算
	}
	if cur < s.capRate*0.75 && now.Sub(s.lastComp) > 4*time.Second {
		s.compensate()
		s.lastComp = now
		// 承认新的水位，但别把基准一次性砸到底，否则会连环补偿
		if floor := s.capRate * 0.85; cur > floor {
			s.capRate = cur
		} else {
			s.capRate = floor
		}
	}
}

// addBatch 加一批流，并记录基线等待观察。步长随并发数递增，
// 小并发时细粒度找拐点，大并发时快速拉满。
func (s *scheduler) addBatch(cur float64, now time.Time) {
	n := s.e.StreamCount()
	step := 2
	switch {
	case n >= 24:
		step = 4
	case n >= 8:
		step = 3
	}

	added := 0
	for i := 0; i < step; i++ {
		if s.e.AddStream(s.e.pickKind()) == nil {
			break
		}
		added++
	}
	if added == 0 {
		s.locked = true
		s.capRate = cur
		s.e.SetPhase("steady")
		return
	}

	s.baseline = cur
	s.probing = true
	s.probeAt = now
}

func (s *scheduler) compensate() {
	streams := s.e.Streams()
	if len(streams) == 0 {
		return
	}

	rates := make([]float64, 0, len(streams))
	for _, st := range streams {
		rates = append(rates, float64(st.rate.Load()))
	}
	med := median(rates)

	if med <= 0 {
		// 全都没跑起来：重启一半，换一批新连接
		for i, st := range streams {
			if i%2 == 0 {
				s.e.RestartStream(st)
			}
		}
		s.e.stats.Compensations.Add(1)
		return
	}

	var slow []*stream
	for _, st := range streams {
		if float64(st.rate.Load()) < med*0.5 {
			slow = append(slow, st)
		}
	}
	if len(slow) > 0 {
		for _, st := range slow {
			s.e.RestartStream(st)
		}
		s.e.stats.Compensations.Add(1)
		s.e.stats.SlowDropped.Add(int64(len(slow)))
		return
	}

	// 所有流健康但仍然掉速 → 加一条流，新连接的拥塞窗口从初始值重来
	if s.e.AddStream(s.e.pickKind()) != nil {
		s.e.stats.Compensations.Add(1)
	}
}

// avgWindow 返回 [now-to, now-from] 区间内 (recv+sent) 的平均速率。
func (s *scheduler) avgWindow(from, to time.Duration) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.points) == 0 {
		return 0
	}
	now := s.points[len(s.points)-1].t
	lo, hi := now.Add(-to), now.Add(-from)
	var sum float64
	var n int
	for _, p := range s.points {
		if p.t.Before(lo) || p.t.After(hi) {
			continue
		}
		sum += float64(p.recv + p.sent)
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// maxIn 返回最近 d 时间窗内 (recv+sent) 的最大值。
func (s *scheduler) maxIn(d time.Duration) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.points) == 0 {
		return 0
	}
	cut := s.points[len(s.points)-1].t.Add(-d)
	var mx int64
	for _, p := range s.points {
		if p.t.Before(cut) {
			continue
		}
		if v := p.recv + p.sent; v > mx {
			mx = v
		}
	}
	return mx
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := make([]float64, len(v))
	copy(c, v)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

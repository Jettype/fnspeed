package main

// Stats 与 Ring 的回归测试。
//
// 锁三件事：
//  1. 环形缓冲满后覆盖最旧的，Snapshot 吐出来的顺序仍是「旧 → 新」；
//  2. 稳定值丢前 3 秒慢启动、丢后 10% 尾部 —— 1.5.1 定下的口径，
//     改这里等于改「稳定值」的定义，必须显式改测试；
//  3. 快照里的瞬时速率取自 Tick 算好的值，而不是现场差分。
//     1.5.x 出现过「累计流量在涨、大号速率恒 0」，根因就是 SSE 紧跟在
//     Tick 后面触发，现场差分 dt≈0、商恒等于 0。

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------- Ring ----------------

func TestRingKeepsNewestAfterWrap(t *testing.T) {
	r := NewRing(3)
	for i := int64(1); i <= 5; i++ {
		r.Push(Sample{T: i, Recv: i * 10})
	}
	got := r.Snapshot(0)
	want := []int64{3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("样本数 %d，期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i].T != want[i] {
			t.Errorf("第 %d 个样本 T=%d，期望 %d（顺序应为旧→新）", i, got[i].T, want[i])
		}
	}
}

func TestRingNotFullYet(t *testing.T) {
	r := NewRing(5)
	r.Push(Sample{T: 1})
	r.Push(Sample{T: 2})
	got := r.Snapshot(0)
	if len(got) != 2 || got[0].T != 1 || got[1].T != 2 {
		t.Fatalf("未满时应原样返回，实际 %+v", got)
	}
}

func TestRingSnapshotLimitTakesNewest(t *testing.T) {
	r := NewRing(5)
	for i := int64(1); i <= 5; i++ {
		r.Push(Sample{T: i})
	}
	got := r.Snapshot(2)
	if len(got) != 2 || got[0].T != 4 || got[1].T != 5 {
		t.Fatalf("limit 应取最新的两个，实际 %+v", got)
	}
}

func TestRingEmpty(t *testing.T) {
	if got := NewRing(4).Snapshot(0); len(got) != 0 {
		t.Fatalf("空缓冲应返回空切片，实际 %+v", got)
	}
}

func TestRingZeroCapacityFallsBack(t *testing.T) {
	r := NewRing(0) // 非法容量走默认值，不能 panic
	r.Push(Sample{T: 1})
	if got := r.Snapshot(0); len(got) != 1 {
		t.Fatalf("容量回退后仍应可用，实际 %+v", got)
	}
}

// ---------------- 稳定值口径 ----------------

// statsWithSeries 造一个已塞入样本的 Stats，StartAt 固定为 base，
// 便于精确控制「前 N 秒」这个判定。
func statsWithSeries(base time.Time, series []Sample) *Stats {
	st := NewStats()
	st.StartAt = base
	st.Running.Store(true)
	for _, s := range series {
		st.ring.Push(s)
	}
	return st
}

// buildSeries 按「每 step 一个点、每个点增加 delta 字节」造累计序列。
func buildSeries(base int64, step int64, deltas []int64) []Sample {
	out := make([]Sample, 0, len(deltas)+1)
	var recv int64
	out = append(out, Sample{T: base})
	for i, d := range deltas {
		recv += d
		out = append(out, Sample{T: base + int64(i+1)*step, Recv: recv})
	}
	return out
}

// 前 3 秒的样本必须被丢弃。若把慢启动期算进稳定值，家里千兆链路
// 会被前几秒的低速拖低一截，用户看到的就是「稳定值明显低于曲线中段」。
func TestFinalizeDropsSlowStartWindow(t *testing.T) {
	// 每 500ms 一个点，共 8 个点（0 ~ 3.5s）。
	// 0~2.5s 的 5 个区间是 1e9 B/s，3.0s 起的 2 个区间是 1e6 B/s。
	deltas := []int64{
		5e8, 5e8, 5e8, 5e8, 5e8, // 0.5s ~ 2.5s：1e9 B/s，全部落在慢启动窗
		5e5, 5e5, // 3.0s、3.5s：1e6 B/s，应被保留
	}
	st := statsWithSeries(time.UnixMilli(0), buildSeries(0, 500, deltas))
	st.Finalize(2)

	if got := st.steadyRecv.Load(); got != 1e6 {
		t.Fatalf("稳定值 %d，期望 1e6（慢启动期必须剔除，否则会被 1e9 拉高）", got)
	}
}

// 后 10% 的样本必须被丢弃。用 10 个有效区间，最后一个塞入 1e9：
// 保留 9 个时中位数稳稳是 1e6；一旦不再截尾，中位数会跳到 5.005e8。
func TestFinalizeDropsTailTenPercent(t *testing.T) {
	deltas := make([]int64, 0, 15)
	for i := 0; i < 14; i++ {
		deltas = append(deltas, 5e5) // 1e6 B/s
	}
	deltas = append(deltas, 5e8) // 最后一个区间 1e9 B/s

	st := statsWithSeries(time.UnixMilli(0), buildSeries(0, 500, deltas))
	st.Finalize(4)

	if got := st.steadyRecv.Load(); got != 1e6 {
		t.Fatalf("稳定值 %d，期望 1e6（尾部 10%% 必须剔除，否则被末端尖峰拉偏）", got)
	}
}

func TestFinalizeNeedsEnoughSamples(t *testing.T) {
	st := statsWithSeries(time.UnixMilli(0), buildSeries(0, 500, []int64{5e5, 5e5}))
	st.Finalize(1)
	if got := st.steadyRecv.Load(); got != 0 {
		t.Fatalf("样本不足时应保持 0，实际 %d", got)
	}
	if !st.finalized.Load() {
		t.Fatal("即便样本不足，finalized 也应置位，避免重复结算")
	}
}

func TestFinalizeIsIdempotent(t *testing.T) {
	deltas := []int64{5e5, 5e5, 5e5, 5e5, 5e5, 5e5, 5e5, 5e5}
	st := statsWithSeries(time.UnixMilli(0), buildSeries(0, 500, deltas))
	st.Finalize(3)
	first := st.steadyRecv.Load()
	if first <= 0 {
		t.Fatalf("首次结算应得出稳定值，实际 %d", first)
	}

	// 结算后再灌一批高得多的样本，重新结算不应改变结果
	for i := 0; i < 10; i++ {
		st.ring.Push(Sample{T: 1000 * int64(i+1), Recv: 1e12})
	}
	st.Finalize(3)

	if got := st.steadyRecv.Load(); got != first {
		t.Fatalf("二次结算改了稳定值：%d -> %d", first, got)
	}
}

// ---------------- 瞬时速率（D4 恒 0 bps 的回归） ----------------

func TestSnapshotRateComesFromTick(t *testing.T) {
	st := NewStats()
	st.Reset()
	st.Running.Store(true)

	st.Tick() // 建立基线，此时还没有速率
	st.RecvBytes.Add(2_000_000)
	st.SentBytes.Add(1_000_000)
	time.Sleep(60 * time.Millisecond)
	st.Tick()

	a := st.Snapshot(0, "", nil)
	if a.RecvRate <= 0 {
		t.Fatalf("快照速率 %d，期望 >0（累计字节在涨就必须算出速率）", a.RecvRate)
	}
	if a.SentRate <= 0 {
		t.Fatalf("上行速率 %d，期望 >0", a.SentRate)
	}
	if a.PeakRecv < a.RecvRate {
		t.Fatalf("峰值 %d 不应小于当前速率 %d", a.PeakRecv, a.RecvRate)
	}

	// 关键：连拉两次快照，速率必须一致。
	// 如果哪天又改回「现场用 lastRec 重算」，第二次调用 dt≈0 就会算出 0，
	// 这正是 1.5.x 那个「累计流量在涨、大号速率一直 0」的现象。
	b := st.Snapshot(0, "", nil)
	if a.RecvRate != b.RecvRate {
		t.Fatalf("两次快照速率不一致：%d vs %d（现场重算会撞上 dt≈0）", a.RecvRate, b.RecvRate)
	}
}

func TestSnapshotRateIsZeroWhenIdle(t *testing.T) {
	st := NewStats()
	st.Reset()
	st.Running.Store(false)
	st.lastRR.Store(12345) // 上个周期的残留值，停表后不应再显示

	if got := st.Snapshot(0, "", nil).RecvRate; got != 0 {
		t.Fatalf("停止后速率 %d，期望 0", got)
	}
}

// ---------------- 计时 ----------------

func TestSnapshotElapsedFreezesAfterStop(t *testing.T) {
	st := NewStats()
	st.Reset()
	st.StartAt = time.Now().Add(-10 * time.Second)
	st.Running.Store(true)
	st.endAt.Store(time.Now().Add(-5 * time.Second).UnixMilli())
	st.Running.Store(false)

	a := st.Snapshot(0, "", nil)
	time.Sleep(40 * time.Millisecond)
	b := st.Snapshot(0, "", nil)

	if math.Abs(a.ElapsedSec-5) > 0.5 {
		t.Fatalf("停止后耗时 %.2fs，期望约 5s", a.ElapsedSec)
	}
	if a.ElapsedSec != b.ElapsedSec {
		t.Fatalf("停止后耗时应冻结：%.4f vs %.4f", a.ElapsedSec, b.ElapsedSec)
	}
}

func TestSnapshotAvgSkipsSlowStart(t *testing.T) {
	st := NewStats()
	st.Reset()
	st.Running.Store(true)
	st.RecvBytes.Store(1_000_000)

	if got := st.Snapshot(0, "", nil).AvgRecv; got != 0 {
		t.Fatalf("开头 3 秒内不该给平均值，实际 %d", got)
	}

	st.StartAt = time.Now().Add(-10 * time.Second)
	got := st.Snapshot(0, "", nil).AvgRecv
	if got < 90_000 || got > 110_000 {
		t.Fatalf("平均值 %d，期望约 100000（1MB / 10s）", got)
	}
}

// ---------------- 历史记录 ----------------

func TestAppendHistoryWritesOneJSONLine(t *testing.T) {
	dir := t.TempDir()
	rec := HistoryRec{At: 123, DurationSec: 30, Concurrency: 8, RecvTotal: 999}

	if err := AppendHistory(dir, rec); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := AppendHistory(dir, rec); err != nil {
		t.Fatalf("追加失败: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	lines := 0
	for _, ln := range splitLines(b) {
		var got HistoryRec
		if err := json.Unmarshal([]byte(ln), &got); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v", lines+1, err)
		}
		if got.RecvTotal != 999 || got.DurationSec != 30 {
			t.Errorf("字段丢了吗：%+v", got)
		}
		lines++
	}
	if lines != 2 {
		t.Fatalf("应有 2 行，实际 %d（必须是追加而不是覆盖）", lines)
	}
}

func TestAppendHistoryEmptyDirIsNoop(t *testing.T) {
	if err := AppendHistory("", HistoryRec{}); err != nil {
		t.Fatalf("空 dataDir 应直接返回 nil，实际 %v", err)
	}
}

func splitLines(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

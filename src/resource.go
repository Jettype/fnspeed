package main

// 本机资源采样：CPU 使用率、内存占用、goroutine 数。
// 用来判断吞吐上不去到底是链路问题，还是 NAS 自己先到顶。

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ResStat struct {
	CPU        float64 `json:"cpu"`     // 0-100
	MemUsed    float64 `json:"memUsed"` // 0-100
	MemTotalMB int64   `json:"memTotalMB"`
	Goroutines int     `json:"goroutines"`
}

type ResSampler struct {
	mu     sync.RWMutex
	cur    ResStat
	prev   cpuSample
	primed bool

	stop chan struct{}
	wg   sync.WaitGroup
}

type cpuSample struct {
	idle  uint64
	total uint64
}

func NewResSampler() *ResSampler {
	return &ResSampler{stop: make(chan struct{})}
}

func (r *ResSampler) Start() {
	r.prev, r.primed = readCPU()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer guard("resSampler")
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.sample()
			}
		}
	}()
}

func (r *ResSampler) Stop() {
	select {
	case <-r.stop:
		return
	default:
	}
	close(r.stop)
	r.wg.Wait()
}

func (r *ResSampler) sample() {
	cur, ok := readCPU()
	if !ok || !r.primed {
		r.prev, r.primed = cur, true
		return
	}
	dt := cur.total - r.prev.total
	di := cur.idle - r.prev.idle
	r.prev = cur

	var cpu float64
	if dt > 0 {
		cpu = float64(dt-di) / float64(dt) * 100
		if cpu < 0 {
			cpu = 0
		}
		if cpu > 100 {
			cpu = 100
		}
	}

	used, totalMB := readMem()
	var memPct float64
	if totalMB > 0 {
		memPct = float64(used) / float64(totalMB) * 100
	}

	r.mu.Lock()
	r.cur = ResStat{
		CPU:        cpu,
		MemUsed:    memPct,
		MemTotalMB: totalMB,
		Goroutines: runtime.NumGoroutine(),
	}
	r.mu.Unlock()
}

func (r *ResSampler) Snapshot() ResStat {
	r.mu.RLock()
	s := r.cur
	r.mu.RUnlock()
	s.Goroutines = runtime.NumGoroutine()
	return s
}

func readCPU() (cpuSample, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(ln, "cpu ") {
			continue
		}
		f := strings.Fields(ln)[1:]
		var total, idle uint64
		for i, v := range f {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				continue
			}
			total += n
			// idle + iowait
			if i == 3 || i == 4 {
				idle += n
			}
		}
		if total == 0 {
			return cpuSample{}, false
		}
		return cpuSample{idle: idle, total: total}, true
	}
	return cpuSample{}, false
}

func readMem() (usedMB, totalMB int64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var total, avail int64
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 {
			continue
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = n / 1024
		case "MemAvailable:":
			avail = n / 1024
		}
	}
	if total <= 0 {
		return 0, 0
	}
	return total - avail, total
}

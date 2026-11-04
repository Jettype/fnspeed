package main

// fnspeed-sink：压测接收端。
// GET  /dl?bytes=N&token=T   动态生成随机字节流，不落盘，支持 Range
// POST/UP /up                接收上传，直接丢弃
// GET  /health               健康检查

import (
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	block     = make([]byte, 1<<20) // 1MB 随机块，进程内复用
	token     string
	maxBytes  int64
	listenAdr string
	rateMB    float64
)

func main() {
	var (
		addr  = flag.String("addr", ":8977", "监听地址")
		tok   = flag.String("token", "", "访问 token；为空则从环境变量 FNS_SPEED_TOKEN 读取，仍为空表示不校验")
		maxGB = flag.Float64("max-gb", 50, "单请求最大字节数（GB）")
		rateFlag = flag.Float64("rate", 0, "每条连接的发送限速（MB/s），0 表示不限；用于模拟受限链路")
	)
	flag.Parse()

	listenAdr = *addr
	token = *tok
	if token == "" {
		token = os.Getenv("FNS_SPEED_TOKEN")
	}
	rateMB = *rateFlag
	maxBytes = int64(*maxGB * 1024 * 1024 * 1024)
	if maxBytes <= 0 {
		maxBytes = 50 << 30
	}

	if _, err := rand.Read(block); err != nil {
		log.Fatalf("初始化随机块失败: %v", err)
	}
	if token == "" {
		log.Println("警告：未设置 token，任何能访问该端口的人都能拉取流量")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/dl", handleDL)
	mux.HandleFunc("/up", handleUp)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"maxBytes":%d}`, maxBytes)
	})

	srv := &http.Server{
		Addr:              listenAdr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // 大流量不限写超时
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("fnspeed-sink 监听 %s，单请求上限 %.1f GB", listenAdr, float64(maxBytes)/(1<<30))
	log.Fatal(srv.ListenAndServe())
}

func authOK(r *http.Request) bool {
	if token == "" {
		return true
	}
	t := r.URL.Query().Get("token")
	if t == "" {
		t = r.Header.Get("X-Token")
	}
	if t == "" {
		h := r.Header.Get("Authorization")
		t = strings.TrimPrefix(h, "Bearer ")
	}
	return t == token
}

func handleDL(w http.ResponseWriter, r *http.Request) {
	if !authOK(r) {
		http.Error(w, "token 无效", http.StatusForbidden)
		return
	}

	n := int64(0)
	if s := r.URL.Query().Get("bytes"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v < 0 {
			http.Error(w, "bytes 参数非法", http.StatusBadRequest)
			return
		}
		n = v
	}
	if n == 0 {
		n = 1 << 30 // 默认 1GB
	}
	if n > maxBytes {
		http.Error(w, fmt.Sprintf("超过单请求上限 %d 字节", maxBytes), http.StatusRequestEntityTooLarge)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Encoding", "identity")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "no-store")

	start, end := int64(0), n-1
	if rg := r.Header.Get("Range"); rg != "" {
		s, e, ok := parseRange(rg, n)
		if !ok {
			http.Error(w, "Range 不合法", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, end = s, e
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, n))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	remain := end - start + 1
	// 起始偏移对齐到块边界，让每次写入都从块的随机位置开始，内容不重复可预测性也够
	off := int(start % int64(len(block)))

	var written int64
	for remain > 0 {
		chunk := int64(len(block) - off)
		if chunk > remain {
			chunk = remain
		}
		if _, err := w.Write(block[off : off+int(chunk)]); err != nil {
			return // 客户端断开，正常现象
		}
		remain -= chunk
		off = 0

		// 模拟受限链路：每写满 1MB 睡够这段时间
		if rateMB > 0 {
			written += chunk
			if written >= 1<<20 {
				need := time.Duration(float64(written) / (rateMB * (1 << 20)) * float64(time.Second))
				time.Sleep(need)
				written = 0
			}
		}
	}
}

// parseRange 只支持单区间 "bytes=a-b" / "bytes=a-" / "bytes=-n"。
func parseRange(header string, size int64) (int64, int64, bool) {
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i] // 多区间只取第一段
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false
	}
	var start, end int64
	if a == "" {
		n, err := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	v, err := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	if err != nil || v < 0 {
		return 0, 0, false
	}
	start = v
	end = size - 1
	if b != "" {
		e, err := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
		if err != nil || e < start {
			return 0, 0, false
		}
		end = e
	}
	if start >= size {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true
}

func handleUp(w http.ResponseWriter, r *http.Request) {
	if !authOK(r) {
		http.Error(w, "token 无效", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "只接受 POST/PUT", http.StatusMethodNotAllowed)
		return
	}
	// 读多少丢多少，不落盘、不缓冲
	n, err := io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if err != nil {
		var ne net.Error
		if ok := asNetErr(err, &ne); ok {
			// 客户端主动断开，属于压测正常结束
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"received":%d}`, n)
}

func asNetErr(err error, target *net.Error) bool {
	if ne, ok := err.(net.Error); ok {
		*target = ne
		return true
	}
	return false
}

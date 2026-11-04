package main

import (
	"embed"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed web/*
var webFS embed.FS

func main() {
	var (
		port     = flag.Int("port", 8976, "HTTP 监听端口（UI + API，仅绑定回环地址）")
		sock     = flag.String("sock", "", "Unix Socket 路径（fnOS 统一网关入口，留空则只开 TCP）")
		dataDir  = flag.String("data-dir", "", "可写数据目录 var/（历史记录）")
		etcDir   = flag.String("etc-dir", "", "配置目录 etc/（builtin.json / user.json 落盘位置）")
		basePath = flag.String("base-path", "/app/fnspeed", "fnOS 网关挂载前缀")
	)
	flag.Parse()

	if *dataDir == "" {
		*dataDir = "var"
	}
	if *etcDir == "" {
		*etcDir = filepath.Join(*dataDir, "etc")
	}
	if err := os.MkdirAll(*dataDir, 0755); err != nil {
		log.Printf("创建数据目录失败（继续运行，历史不落盘）: %v", err)
	}
	if err := os.MkdirAll(*etcDir, 0755); err != nil {
		log.Printf("创建配置目录失败（继续运行，保存会失败）: %v", err)
	}

	setCrashLog(*dataDir)

	ApplyKernelTuning()

	store := NewStore(*etcDir)
	settings := NewSettingsStore(*etcDir)
	app := NewApp(*dataDir, store, settings)
	log.Printf("全局参数：并发 %d / 时长 %ds（etc/settings.json）", settings.Get().Concurrency, settings.Get().DurationSec)
	cfg, seeded, note, err := store.EnsureSeeded()
	if err != nil {
		log.Printf("写入内置预设失败（继续用内存配置）: %v", err)
	}
	app.cfg, app.note = cfg, note
	if note != "" {
		log.Printf("配置恢复：%s", note)
	}
	if seeded {
		log.Printf("默认配置已同步（%d 个配置集），自建配置目录 %s", len(cfg.Profiles), store.Path())
	}
	log.Printf("配置集 %d 个，用户区 %s", len(cfg.Profiles), store.Path())

	mux := http.NewServeMux()
	app.Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		b, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "前端资源缺失", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		// 刻意不设置 X-Frame-Options / CSP frame-ancestors：
		// 应用要在飞牛 App 的 webview 与 FN Connect 的 iframe 中打开。
		_, _ = w.Write(b)
	})

	handler := withBasePath(mux, *basePath)

	// 只绑回环地址：对外入口一律走 fnOS 统一网关（Unix Socket）。
	// 应用中心要求「不监听公网端口」，绑 0.0.0.0 会被审核判为不合规；
	// 回环 + 网关也顺带挡住了局域网内未鉴权访问 API 的路径。
	srv := &http.Server{
		Addr:              "127.0.0.1:" + itoa(*port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("fnspeed 启动，TCP 127.0.0.1:%d，网关前缀 %s", *port, *basePath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("TCP 监听失败（可能端口被占用）: %v", err)
		}
	}()

	// fnOS 统一网关：网关拿到登录态后，把 /app/fnspeed/* 从 Unix Socket 转发进来。
	// 这条通道是「手机 App 内点开」和「FN Connect 远程打开」唯一可靠的方式——
	// 它复用 fnOS 的域名与 HTTPS，不受端口映射和 mixed-content 限制。
	if *sock != "" {
		_ = os.Remove(*sock)
		ln, err := net.Listen("unix", *sock)
		if err != nil {
			log.Printf("网关 socket 监听失败（应用仍可用 TCP 直连）: %v", err)
		} else {
			_ = os.Chmod(*sock, 0770)
			log.Printf("网关 socket 就绪 %s", *sock)
			go func() {
				if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
					log.Printf("网关 socket 退出: %v", err)
				}
			}()
		}
		defer func() { _ = os.Remove(*sock) }()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("收到退出信号，停止任务")
	app.mu.Lock()
	if app.engine != nil {
		app.engine.Stop("服务退出")
	}
	app.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
}

// withBasePath 让服务同时适配两种访问方式：
//  1. Nginx / App 反代到 /app/fnspeed/ → 剥掉前缀后交给 mux
//  2. 直连 http://ip:8976/ → 原样交给 mux
//
// 前端一律使用相对路径（api/status），因此两种入口都能正确命中。
func withBasePath(h http.Handler, base string) http.Handler {
	base = strings.TrimSuffix(base, "/")
	if base == "" || base == "/" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == base {
			// 不带尾斜杠时相对路径会解析错，统一跳一次
			http.Redirect(w, r, base+"/", http.StatusMovedPermanently)
			return
		}
		if strings.HasPrefix(p, base+"/") {
			r.URL.Path = strings.TrimPrefix(p, base)
		}
		h.ServeHTTP(w, r)
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

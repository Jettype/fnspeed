package main

// 内核参数调优。仅在有 root 权限时生效，失败不影响压测运行（只影响上限）。

import (
	"log"
	"os"
	"os/exec"
	"strings"
)

const sysctlPath = "/etc/sysctl.d/99-fnspeed.conf"

// sysctlBody 必须与 cmd/install_callback 里的 heredoc 逐字一致。
// 两处写的是同一个文件：内容相同才能靠「读出来比对」跳过重写，
// 一旦首行注释有差异，每次以 root 启动都会白写一次盘。
const sysctlBody = `# fnspeed 带宽压测：加大 socket 缓冲 + BBR
# 由 install_callback 在安装时写入；以 root 启动时本程序会校正一次。卸载不清除。
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.ipv4.tcp_rmem = 4096 262144 16777216
net.ipv4.tcp_wmem = 4096 262144 16777216
net.ipv4.tcp_congestion_control = bbr
`

// ApplyKernelTuning 写入 sysctl 配置并立即生效。
// 非 root 时静默跳过 —— 应用中心安装时已由 install_callback 以 root 写过一次。
func ApplyKernelTuning() {
	if os.Geteuid() != 0 {
		log.Println("非 root 运行，跳过内核参数调优（socket 缓冲区将受系统默认上限约束）")
		return
	}
	cur, _ := os.ReadFile(sysctlPath)
	if string(cur) == sysctlBody {
		_ = exec.Command("sysctl", "-q", "-p", sysctlPath).Run()
		return
	}
	if err := os.WriteFile(sysctlPath, []byte(sysctlBody), 0644); err != nil {
		log.Printf("写入 %s 失败（跳过调优）: %v", sysctlPath, err)
		return
	}
	if out, err := exec.Command("sysctl", "-q", "-p", sysctlPath).CombinedOutput(); err != nil {
		log.Printf("sysctl 应用失败: %v %s", err, strings.TrimSpace(string(out)))
		return
	}
	cc, _ := exec.Command("sysctl", "-n", "net.ipv4.tcp_congestion_control").Output()
	log.Printf("内核调优已应用，拥塞控制: %s", strings.TrimSpace(string(cc)))
}

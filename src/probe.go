package main

// 预检：用一条 Range 请求探测端点的传输特征，据此决定传输模式。
// 结果三态：可用 / 时效失效 / 非直传端点，前端直接按这个着色。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type TransferMode string

const (
	ModeChunkLoop TransferMode = "chunk"  // 有 Content-Length 且支持 Range
	ModeFileLoop  TransferMode = "file"   // 有 Content-Length 不支持 Range
	ModeStream    TransferMode = "stream" // 无 Content-Length（chunked）
	ModeRaw       TransferMode = "raw"    // 上传用裸 TCP
)

type URLStatus string

const (
	StatusUnknown URLStatus = "unknown"
	StatusOK      URLStatus = "ok"
	StatusExpired URLStatus = "expired" // 签名失效
	StatusInvalid URLStatus = "invalid" // 端点不可用 / 非直传
)

type ProbeResult struct {
	Mode   TransferMode
	Status URLStatus
	Size   int64
	Err    string
}

const probeTimeout = 6 * time.Second

// ProbeURL 探测一个下载端点。
func ProbeURL(ctx context.Context, ep Endpoint) ProbeResult {
	if err := checkEndpointURL(ep.URL); err != nil {
		return ProbeResult{Status: StatusInvalid, Err: err.Error()}
	}

	// 上传端点如果指向站点首页，不可能接受数据，直接判非直传。
	if isHomepageURL(ep.URL) {
		return ProbeResult{Status: StatusInvalid, Mode: ModeRaw, Err: "非直传端点（指向站点首页）"}
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	res := doProbe(ctx, ep, false, true)
	// 有的镜像站对 Range 请求直接 403（清华 TUNA 就是这样），
	// 换个不带 Range 的普通 GET 再试一次，能拿到 200 就按长流跑。
	if res.Status != StatusOK {
		if r2 := doProbe(ctx, ep, isTimeLimitedURL(ep.URL), false); r2.Status == StatusOK {
			return r2
		}
	}
	return res
}

func doProbe(ctx context.Context, ep Endpoint, withReferer, withRange bool) ProbeResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.URL, nil)
	if err != nil {
		return ProbeResult{Status: StatusInvalid, Err: "构造请求失败"}
	}
	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}
	if withRange {
		req.Header.Set("Range", "bytes=0-0")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", browserUA)
	}
	// 不少镜像站按 Accept 头做防盗链，缺省时直接 403。
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "identity")
	}
	if withReferer && req.Header.Get("Referer") == "" {
		if u, e := url.Parse(ep.URL); e == nil {
			req.Header.Set("Referer", u.Scheme+"://"+u.Host+"/")
		}
	}

	client := newProbeClient()
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Timeout() {
			return ProbeResult{Status: StatusUnknown, Err: fmt.Sprintf("探测超时（%g 秒无响应，可能是 DNS 慢、被墙，或目标端限流）", probeTimeout.Seconds())}
		}
		return ProbeResult{Status: StatusUnknown, Err: shortErr(err)}
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusPartialContent:
		size := parseContentRangeTotal(resp.Header.Get("Content-Range"))
		return ProbeResult{Mode: ModeChunkLoop, Status: StatusOK, Size: size}

	case resp.StatusCode == http.StatusOK:
		if resp.ContentLength > 0 {
			return ProbeResult{Mode: ModeFileLoop, Status: StatusOK, Size: resp.ContentLength}
		}
		if resp.ContentLength == 0 && resp.Header.Get("Content-Length") == "0" {
			return ProbeResult{Mode: ModeStream, Status: StatusOK, Err: "响应体为空"}
		}
		return ProbeResult{Mode: ModeStream, Status: StatusOK}

	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		if isTimeLimitedURL(ep.URL) {
			return ProbeResult{Status: StatusExpired, Err: fmt.Sprintf("HTTP %d，签名可能已失效", resp.StatusCode)}
		}
		return ProbeResult{Status: StatusInvalid, Err: fmt.Sprintf("HTTP %d，拒绝访问", resp.StatusCode)}

	// 418 / 429 是镜像站的风控与限流，不是链路的错，也不代表 URL 永久失效。
	case resp.StatusCode == 418:
		return ProbeResult{Status: StatusInvalid, Err: "HTTP 418，目标端风控拦截（家宽 IP 一般不会触发，换源或稍后重试）"}
	case resp.StatusCode == 429:
		return ProbeResult{Status: StatusUnknown, Err: "HTTP 429，目标端限流"}

	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return ProbeResult{Status: StatusInvalid, Err: fmt.Sprintf("HTTP %d，端点不可用", resp.StatusCode)}

	case resp.StatusCode >= 500:
		return ProbeResult{Status: StatusUnknown, Err: fmt.Sprintf("HTTP %d，服务端异常", resp.StatusCode)}

	default:
		return ProbeResult{Status: StatusInvalid, Err: fmt.Sprintf("HTTP %d，未预期状态", resp.StatusCode)}
	}
}

// parseContentRangeTotal 从 "bytes 0-0/12345" 里取出总长度。取不到返回 0。
func parseContentRangeTotal(cr string) int64 {
	i := strings.LastIndexByte(cr, '/')
	if i < 0 || i == len(cr)-1 {
		return 0
	}
	s := strings.TrimSpace(cr[i+1:])
	if s == "*" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

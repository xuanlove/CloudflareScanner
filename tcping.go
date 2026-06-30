package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/VividCortex/ewma"
	"github.com/cheggaaa/pb/v3"
)

// tcping 单次 TCP 连接测速,返回是否成功与延迟(ms)
func tcping(addr netip.Addr, port int, timeout time.Duration) (bool, float32) {
	startTime := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(addr.String(), fmt.Sprintf("%d", port)), timeout)
	if err != nil {
		return false, 0
	}
	defer func() { _ = conn.Close() }()
	duration := float32(time.Since(startTime).Microseconds()) / 1000.0
	return true, duration
}

// checkConnection 探测 failTime 次,带退避,返回成功延迟列表
func checkConnection(addr netip.Addr, cfg Config) []float32 {
	times := make([]float32, 0, cfg.FailTime)
	for i := 0; i < cfg.FailTime; i++ {
		ok, t := tcping(addr, cfg.TcpPort, cfg.TcpTimeout)
		if ok {
			times = append(times, t)
		}
		if i < cfg.FailTime-1 && cfg.BackoffDuration > 0 {
			time.Sleep(cfg.BackoffDuration)
		}
	}
	return times
}

// tcpingHandler 单 IP 完整测试
// 修复原 bug:移除无效的 IP 末位递增分支,失败即放弃(采样已随机化)
// 返回: success, pingReceived(成功次数), pingTimes(成功延迟列表)
func tcpingHandler(addr netip.Addr, pingCount int, cfg Config, progressHandler func(e progressEvent)) (bool, int, []float32) {
	probeTimes := checkConnection(addr, cfg)
	if len(probeTimes) == 0 {
		// 探测失败:补齐进度条到该 IP 应有次数,使总数对齐
		progressHandler(NoAvailableIPFound)
		return false, 0, nil
	}
	progressHandler(AvailableIPFound)
	times := make([]float32, 0, pingCount)
	times = append(times, probeTimes...)
	// 补足剩余次数(pingCount - failTime)
	for i := cfg.FailTime; i < pingCount; i++ {
		ok, t := tcping(addr, cfg.TcpPort, cfg.TcpTimeout)
		progressHandler(NormalPing)
		if ok {
			times = append(times, t)
		}
	}
	return true, len(times), times
}

// handleProgressGenerator 创建进度回调
func handleProgressGenerator(bar *pb.ProgressBar, pingTime, failTime int) func(e progressEvent) {
	return func(e progressEvent) {
		switch e {
		case NoAvailableIPFound:
			bar.Add(pingTime)
		case AvailableIPFound:
			bar.Add(failTime)
		case NormalPing:
			bar.Increment()
		}
	}
}

// runTcping 并发执行 TCPing
func (s *Scanner) runTcping(ips []netip.Addr) []CloudflareIPData {
	total := len(ips) * s.cfg.PingTime
	bar := pb.StartNew(total)
	defer bar.Finish()

	var wg sync.WaitGroup
	var mu sync.Mutex
	data := make([]CloudflareIPData, 0)
	control := make(chan struct{}, s.cfg.PingRoutine)

	for _, ip := range ips {
		wg.Add(1)
		control <- struct{}{}
		go func(ip netip.Addr) {
			defer wg.Done()
			defer func() { <-control }()
			ph := handleProgressGenerator(bar, s.cfg.PingTime, s.cfg.FailTime)
			ok, recv, times := tcpingHandler(ip, s.cfg.PingTime, s.cfg, ph)
			if !ok {
				return
			}
			d := CloudflareIPData{
				IP:           ip,
				PingCount:    s.cfg.PingTime,
				PingReceived:  recv,
				PingTimes:    times,
			}
			d.Finalize()
			mu.Lock()
			data = append(data, d)
			mu.Unlock()
		}(ip)
	}
	wg.Wait()
	return data
}

// GetDialContextByAddr 返回绑定到指定 IP 的 DialContext,使 HTTP 请求经由该 Cloudflare 节点
func GetDialContextByAddr(addr netip.Addr, port int) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		d := &net.Dialer{Timeout: 5 * time.Second}
		return d.DialContext(ctx, "tcp", net.JoinHostPort(addr.String(), fmt.Sprintf("%d", port)))
	}
}

// DownloadSpeedHandler 下载测速,返回是否成功与下载速度(B/s)
// 修复:增加 HTTP 总超时、TLS ServerName,避免卡死与证书校验问题
func DownloadSpeedHandler(addr netip.Addr, cfg Config) (bool, float32) {
	transport := &http.Transport{
		DialContext: GetDialContextByAddr(addr, 443),
		TLSClientConfig: &tls.Config{
			ServerName: "speed.cloudflare.com",
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.DownloadTestTime + 5*time.Second, // 总超时保护
	}
	url := fmt.Sprintf("%s?bytes=%d", cfg.DownloadURL, cfg.DownloadSize)
	response, err := client.Get(url)
	if err != nil {
		return false, 0
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return false, 0
	}

	timeStart := time.Now()
	timeEnd := timeStart.Add(cfg.DownloadTestTime)
	contentLength := response.ContentLength
	buffer := make([]byte, 1024)
	var contentRead int64
	timeSlice := cfg.DownloadTestTime / 100
	var lastContentRead int64
	timeCounter := 1
	nextTime := timeStart.Add(timeSlice)
	e := ewma.NewMovingAverage()

	for contentLength != contentRead {
		currentTime := time.Now()
		if currentTime.After(nextTime) {
			timeCounter++
			nextTime = timeStart.Add(timeSlice * time.Duration(timeCounter))
			e.Add(float64(contentRead - lastContentRead))
			lastContentRead = contentRead
		}
		if currentTime.After(timeEnd) {
			break
		}
		n, err := response.Body.Read(buffer)
		contentRead += int64(n)
		if err != nil {
			if err == io.EOF {
				e.Add(float64(contentRead-lastContentRead) / (float64(nextTime.Sub(currentTime)) / float64(timeSlice)))
			}
			break
		}
	}
	speed := float32(e.Value()) / (float32(cfg.DownloadTestTime.Seconds()) / 100)
	return true, speed
}

// runDownloads 并行下载测速,失败时自动顺延下一个候选,直到收集够 count 个成功结果或耗尽列表
func (s *Scanner) runDownloads(data []CloudflareIPData) []CloudflareIPData {
	bar := pb.StartNew(s.cfg.DownloadTestCount)
	defer bar.Finish()

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, s.cfg.DownloadRoutine)
	out := make([]CloudflareIPData, 0, s.cfg.DownloadTestCount)

	for i := 0; i < len(data); i++ {
		mu.Lock()
		done := len(out) >= s.cfg.DownloadTestCount
		mu.Unlock()
		if done {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			ip := data[idx].IP
			ok, speed := DownloadSpeedHandler(ip, s.cfg)
			bar.Add(1)
			if !ok || speed <= 0 {
				return
			}
			mu.Lock()
			if len(out) < s.cfg.DownloadTestCount {
				d := data[idx]
				d.DownloadSpeed = speed
				out = append(out, d)
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	return out
}

package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"time"
)

// Config 程序配置(替代原全局变量)
type Config struct {
	PingRoutine       int           // TCPing 并发协程数
	PingTime          int           // 每个 IP 的 TCPing 次数
	DownloadTestCount int           // 参与下载测速的节点数
	DownloadTestTime  time.Duration // 单节点下载测速时长
	DownloadRoutine   int           // 下载测速并发数
	DownloadURL       string        // 测速 URL(不含 query)
	DownloadSize      int64         // 测速下载字节数
	IPv4File          string        // IPv4 CIDR 文件
	IPv6File          string        // IPv6 CIDR 文件(不存在则跳过)
	MaxIPsPerRange    int           // 每个 CIDR 段最大采样 IP 数(0=不限制)
	OutputPrefix      string        // 输出文件前缀(无扩展名)
	OutputFormats     []string      // 输出格式: csv / json / table
	TcpPort           int           // TCPing 端口
	TcpTimeout        time.Duration // TCP 连接超时
	FailTime          int           // 初始探测重试次数
	BackoffDuration   time.Duration // 重试退避时长
	SortKey           string        // 最终排序: speed | score | ping
}

// DefaultConfig 返回默认配置
func DefaultConfig() Config {
	return Config{
		PingRoutine:       400,
		PingTime:          10,
		DownloadTestCount: 10,
		DownloadTestTime:  10 * time.Second,
		DownloadRoutine:   4,
		DownloadURL:       "https://speed.cloudflare.com/__down",
		DownloadSize:      100_000_000,
		IPv4File:          "ip.txt",
		IPv6File:          "ip6.txt",
		MaxIPsPerRange:    256,
		OutputPrefix:      "./result",
		OutputFormats:     []string{"csv", "table"},
		TcpPort:           443,
		TcpTimeout:        1 * time.Second,
		FailTime:          4,
		BackoffDuration:   200 * time.Millisecond,
		SortKey:           "score",
	}
}

// CloudflareIPData 单个 IP 的测速结果
type CloudflareIPData struct {
	IP            netip.Addr
	PingCount     int     // 总 ping 次数
	PingReceived  int     // 成功次数
	PingTimes     []float32 // 所有成功 ping 的延迟记录(ms)
	AvgPingTime   float32 // 平均延迟
	MedPingTime   float32 // 中位延迟(更稳健)
	DownloadSpeed float32 // 下载速度(B/s)
}

// RecvRate 接收率
func (cf *CloudflareIPData) RecvRate() float32 {
	if cf.PingCount == 0 {
		return 0
	}
	return float32(cf.PingReceived) / float32(cf.PingCount)
}

// Finalize 在收集完 ping 数据后计算统计量(平均、中位数)
func (cf *CloudflareIPData) Finalize() {
	if len(cf.PingTimes) == 0 {
		return
	}
	var sum float32
	for _, t := range cf.PingTimes {
		sum += t
	}
	cf.AvgPingTime = sum / float32(len(cf.PingTimes))
	sorted := make([]float32, len(cf.PingTimes))
	copy(sorted, cf.PingTimes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		cf.MedPingTime = (sorted[mid-1] + sorted[mid]) / 2
	} else {
		cf.MedPingTime = sorted[mid]
	}
}

// DownloadSpeedMBps 下载速度(MB/s)
func (cf *CloudflareIPData) DownloadSpeedMBps() float32 {
	return cf.DownloadSpeed / 1024 / 1024
}

// CloudflareIPDataSet 结果集
type CloudflareIPDataSet []CloudflareIPData

// SortByPing 按接收率↓、中位延迟↑排序(用于选 TopN 进入下载测速)
func (cfs CloudflareIPDataSet) SortByPing() {
	sort.Slice(cfs, func(i, j int) bool {
		ri, rj := cfs[i].RecvRate(), cfs[j].RecvRate()
		if ri != rj {
			return ri > rj
		}
		return cfs[i].MedPingTime < cfs[j].MedPingTime
	})
}

// SortBySpeed 按下载速度↓排序
func (cfs CloudflareIPDataSet) SortBySpeed() {
	sort.Slice(cfs, func(i, j int) bool {
		return cfs[i].DownloadSpeed > cfs[j].DownloadSpeed
	})
}

// SortByScore 按综合评分排序(评分 = 速度 × 接收率 / 中位延迟)
func (cfs CloudflareIPDataSet) SortByScore() {
	score := func(c CloudflareIPData) float64 {
		s := float64(c.RecvRate()) * float64(c.DownloadSpeed)
		if c.MedPingTime > 0 {
			s /= float64(c.MedPingTime)
		}
		return s
	}
	sort.Slice(cfs, func(i, j int) bool {
		return score(cfs[i]) > score(cfs[j])
	})
}

// progressEvent 进度事件
type progressEvent int

const (
	NoAvailableIPFound progressEvent = iota
	AvailableIPFound
	NormalPing
)

// ExportCsv 导出 CSV
func ExportCsv(filePath string, data []CloudflareIPData) error {
	fp, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("create %s: %w", filePath, err)
	}
	defer fp.Close()
	w := csv.NewWriter(fp)
	defer w.Flush()
	_ = w.Write([]string{"IP Address", "Ping Count", "Ping Received", "Recv Rate", "Avg Ping(ms)", "Med Ping(ms)", "Download Speed(MB/s)"})
	for _, d := range data {
		_ = w.Write([]string{
			d.IP.String(),
			strconv.Itoa(d.PingCount),
			strconv.Itoa(d.PingReceived),
			strconv.FormatFloat(float64(d.RecvRate()), 'f', 4, 32),
			strconv.FormatFloat(float64(d.AvgPingTime), 'f', 4, 32),
			strconv.FormatFloat(float64(d.MedPingTime), 'f', 4, 32),
			strconv.FormatFloat(float64(d.DownloadSpeedMBps()), 'f', 4, 32),
		})
	}
	return w.Error()
}

// ExportJSON 导出 JSON
func ExportJSON(filePath string, data []CloudflareIPData) error {
	type item struct {
		IP           string  `json:"ip"`
		PingCount    int     `json:"ping_count"`
		PingReceived int     `json:"ping_received"`
		RecvRate     float32 `json:"recv_rate"`
		AvgPingMs    float32 `json:"avg_ping_ms"`
		MedPingMs    float32 `json:"med_ping_ms"`
		DownloadMBps float32 `json:"download_mbps"`
	}
	items := make([]item, 0, len(data))
	for _, d := range data {
		items = append(items, item{
			IP:           d.IP.String(),
			PingCount:    d.PingCount,
			PingReceived: d.PingReceived,
			RecvRate:     d.RecvRate(),
			AvgPingMs:    d.AvgPingTime,
			MedPingMs:    d.MedPingTime,
			DownloadMBps: d.DownloadSpeedMBps(),
		})
	}
	buf, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, buf, 0644)
}

// PrintTable 控制台表格输出
func PrintTable(data []CloudflareIPData, limit int) {
	if limit > len(data) {
		limit = len(data)
	}
	fmt.Printf("%-4s %-40s %-8s %-8s %-12s %-12s %-15s\n",
		"#", "IP", "Recv", "Rate", "AvgPing(ms)", "MedPing(ms)", "Speed(MB/s)")
	for i := 0; i < limit; i++ {
		d := data[i]
		fmt.Printf("%-4d %-40s %-8d %-8.4f %-12.4f %-12.4f %-15.4f\n",
			i+1, d.IP.String(), d.PingReceived, d.RecvRate(),
			d.AvgPingTime, d.MedPingTime, d.DownloadSpeedMBps())
	}
}

// Scanner 主扫描器,持有配置与随机源(替代全局变量)
type Scanner struct {
	cfg Config
	rng *rand.Rand
}

// NewScanner 创建扫描器
func NewScanner(cfg Config) *Scanner {
	return &Scanner{
		cfg: cfg,
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

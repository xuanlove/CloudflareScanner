package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// parseFlags 解析命令行参数,返回配置
func parseFlags() Config {
	cfg := DefaultConfig()
	flag.IntVar(&cfg.PingRoutine, "routines", cfg.PingRoutine, "TCPing 并发协程数(建议 ≤1000)")
	flag.IntVar(&cfg.PingTime, "ping", cfg.PingTime, "每个 IP 的 TCPing 次数")
	flag.IntVar(&cfg.DownloadTestCount, "download", cfg.DownloadTestCount, "参与下载测速的节点数")
	flag.DurationVar(&cfg.DownloadTestTime, "time", cfg.DownloadTestTime, "单节点下载测速时长")
	flag.IntVar(&cfg.DownloadRoutine, "download-routines", cfg.DownloadRoutine, "下载测速并发数")
	flag.StringVar(&cfg.IPv4File, "ipv4", cfg.IPv4File, "IPv4 CIDR 文件")
	flag.StringVar(&cfg.IPv6File, "ipv6", cfg.IPv6File, "IPv6 CIDR 文件(不存在则跳过)")
	flag.IntVar(&cfg.MaxIPsPerRange, "max-per-range", cfg.MaxIPsPerRange, "每个 CIDR 段最大采样 IP 数(0=不限制)")
	flag.StringVar(&cfg.OutputPrefix, "out", cfg.OutputPrefix, "输出文件前缀(无扩展名)")
	flag.StringVar(&cfg.DownloadURL, "url", cfg.DownloadURL, "测速 URL(不含 query)")
	flag.Int64Var(&cfg.DownloadSize, "size", cfg.DownloadSize, "测速下载字节数")
	flag.StringVar(&cfg.SortKey, "sort", cfg.SortKey, "最终排序: speed | score | ping")
	var formats string
	flag.StringVar(&formats, "formats", "csv,table", "输出格式,逗号分隔: csv,json,table")
	flag.Parse()
	if strings.TrimSpace(formats) != "" {
		cfg.OutputFormats = splitAndTrim(formats)
	}
	return cfg
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	cfg := parseFlags()
	scanner := NewScanner(cfg)

	ips, err := scanner.loadAllIPs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载 IP 失败: %v\n", err)
		os.Exit(1)
	}
	if len(ips) == 0 {
		fmt.Fprintln(os.Stderr, "没有可测试的 IP")
		os.Exit(1)
	}
	fmt.Printf("已加载 %d 个 IP,开始 TCPing(协程数=%d, ping 次数=%d)\n",
		len(ips), cfg.PingRoutine, cfg.PingTime)

	data := scanner.runTcping(ips)
	fmt.Printf("TCPing 完成,可用 IP %d 个\n", len(data))
	if len(data) == 0 {
		fmt.Fprintln(os.Stderr, "没有可用 IP,退出")
		os.Exit(1)
	}

	// 按接收率/中位延迟排序,选 TopN 进入下载测速
	CloudflareIPDataSet(data).SortByPing()
	if len(data) > cfg.DownloadTestCount {
		data = data[:cfg.DownloadTestCount]
	}

	fmt.Printf("开始下载测速(节点数=%d, 单节点时长=%s, 并发=%d)\n",
		len(data), cfg.DownloadTestTime, cfg.DownloadRoutine)
	data = scanner.runDownloads(data)
	fmt.Printf("下载测速完成,成功 %d 个\n", len(data))

	// 测速后按指定键重排(默认综合评分)
	switch cfg.SortKey {
	case "speed":
		CloudflareIPDataSet(data).SortBySpeed()
	case "ping":
		// 保持 ping 排序
	default:
		CloudflareIPDataSet(data).SortByScore()
	}

	// 多格式输出
	for _, f := range cfg.OutputFormats {
		switch f {
		case "csv":
			path := cfg.OutputPrefix + ".csv"
			if err := ExportCsv(path, data); err != nil {
				fmt.Fprintf(os.Stderr, "导出 CSV 失败: %v\n", err)
			} else {
				fmt.Printf("已导出 %s\n", path)
			}
		case "json":
			path := cfg.OutputPrefix + ".json"
			if err := ExportJSON(path, data); err != nil {
				fmt.Fprintf(os.Stderr, "导出 JSON 失败: %v\n", err)
			} else {
				fmt.Printf("已导出 %s\n", path)
			}
		case "table":
			PrintTable(data, len(data))
		}
	}
}

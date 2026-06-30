package main

import (
	"bufio"
	"fmt"
	"math/big"
	"math/rand"
	"net/netip"
	"os"
	"strings"
)

// loadIPsFromFile 从 CIDR 文件加载 IP 列表,每个 CIDR 段最多采样 maxPerRange 个
func loadIPsFromFile(path string, maxPerRange int, rng *rand.Rand) ([]netip.Addr, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var result []netip.Addr
	scanner := bufio.NewScanner(file)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		prefix, err := netip.ParsePrefix(line)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", line, err)
		}
		result = append(result, expandPrefix(prefix, maxPerRange, rng)...)
	}
	return result, scanner.Err()
}

// loadAllIPs 加载 IPv4 + IPv6(IPv6 文件不存在则跳过)
func (s *Scanner) loadAllIPs() ([]netip.Addr, error) {
	ips, err := loadIPsFromFile(s.cfg.IPv4File, s.cfg.MaxIPsPerRange, s.rng)
	if err != nil {
		return nil, fmt.Errorf("load ipv4: %w", err)
	}
	if _, statErr := os.Stat(s.cfg.IPv6File); statErr == nil {
		v6, err := loadIPsFromFile(s.cfg.IPv6File, s.cfg.MaxIPsPerRange, s.rng)
		if err != nil {
			return nil, fmt.Errorf("load ipv6: %w", err)
		}
		ips = append(ips, v6...)
	}
	return ips, nil
}

// expandPrefix 展开 CIDR 为 IP 列表;总 IP 数超过 max 时随机采样
func expandPrefix(prefix netip.Prefix, max int, rng *rand.Rand) []netip.Addr {
	if max <= 0 {
		max = 256
	}
	is4 := prefix.Addr().Is4()
	hostBits := 32 - prefix.Bits()
	if !is4 {
		hostBits = 128 - prefix.Bits()
	}
	total := new(big.Int).Lsh(big.NewInt(1), uint(hostBits))

	// 总数 ≤ max,全展开
	if total.Cmp(big.NewInt(int64(max))) <= 0 {
		count := int(total.Int64())
		out := make([]netip.Addr, 0, count)
		addr := prefix.Masked().Addr()
		for i := 0; i < count; i++ {
			out = append(out, addr)
			addr = addr.Next()
		}
		return out
	}
	// 否则随机采样
	return samplePrefix(prefix, max, rng, is4)
}

// samplePrefix 在 CIDR 范围内随机采样 max 个不重复 IP
func samplePrefix(prefix netip.Prefix, max int, rng *rand.Rand, is4 bool) []netip.Addr {
	base := prefix.Masked().Addr()
	baseBig := addrToBigInt(base)
	hostBits := 32 - prefix.Bits()
	if !is4 {
		hostBits = 128 - prefix.Bits()
	}
	rangeSize := new(big.Int).Lsh(big.NewInt(1), uint(hostBits))

	out := make([]netip.Addr, 0, max)
	seen := make(map[string]struct{}, max)
	for len(out) < max {
		off := new(big.Int).Rand(rng, rangeSize)
		addrBig := new(big.Int).Add(baseBig, off)
		addr := bigIntToAddr(addrBig, is4)
		if !prefix.Contains(addr) {
			continue
		}
		key := addr.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, addr)
	}
	return out
}

func addrToBigInt(addr netip.Addr) *big.Int {
	b := addr.As16()
	return new(big.Int).SetBytes(b[:])
}

func bigIntToAddr(n *big.Int, is4 bool) netip.Addr {
	b := n.Bytes()
	var arr [16]byte
	copy(arr[16-len(b):], b)
	if is4 {
		addr, _ := netip.AddrFromSlice(arr[12:])
		return addr
	}
	addr, _ := netip.AddrFromSlice(arr[:])
	return addr
}

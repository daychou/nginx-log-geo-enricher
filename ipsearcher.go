package main

import (
	"fmt"
	"log"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
)

// maxCacheEntries IP 缓存条目上限，超过后整体清空，防止异常流量（大量随机源 IP）导致内存膨胀
const maxCacheEntries = 1_000_000

// IPSearcher 预加载 ip2region 数据库，提供带缓存的 IP 地理位置查询
type IPSearcher struct {
	v4         *xdb.Searcher
	v6         *xdb.Searcher
	cache      sync.Map
	cacheCount int64 // 当前缓存条目数（原子访问）
}

func NewIPSearcher(v4DBPath, v6DBPath string) (*IPSearcher, error) {
	s := &IPSearcher{}

	if v4DBPath != "" {
		searcher, err := loadSearcher(v4DBPath)
		if err != nil {
			return nil, fmt.Errorf("加载 IPv4 数据库失败: %w", err)
		}
		s.v4 = searcher
		log.Printf("[INFO] IPv4 数据库加载成功: %s", v4DBPath)
	}

	if v6DBPath != "" {
		searcher, err := loadSearcher(v6DBPath)
		if err != nil {
			return nil, fmt.Errorf("加载 IPv6 数据库失败: %w", err)
		}
		s.v6 = searcher
		log.Printf("[INFO] IPv6 数据库加载成功: %s", v6DBPath)
	}

	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			s.clearCache()
		}
	}()

	return s, nil
}

func loadSearcher(dbPath string) (*xdb.Searcher, error) {
	cBuff, err := xdb.LoadContentFromFile(dbPath)
	if err != nil {
		return nil, fmt.Errorf("无法加载 xdb 文件 %s: %w", dbPath, err)
	}
	header, err := xdb.LoadHeaderFromBuff(cBuff)
	if err != nil {
		return nil, fmt.Errorf("无法解析 xdb header: %w", err)
	}
	version, err := xdb.VersionFromHeader(header)
	if err != nil {
		return nil, fmt.Errorf("无法识别 IP 版本: %w", err)
	}
	searcher, err := xdb.NewWithBuffer(version, cBuff)
	if err != nil {
		return nil, fmt.Errorf("创建 searcher 失败: %w", err)
	}
	return searcher, nil
}

func (s *IPSearcher) Lookup(ip string) string {
	if v, ok := s.cache.Load(ip); ok {
		return v.(string)
	}

	addr, err := netip.ParseAddr(ip)
	if err != nil {
		result := "非法IP"
		s.storeCache(ip, result)
		return result
	}

	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		result := "内网IP"
		s.storeCache(ip, result)
		return result
	}

	var searcher *xdb.Searcher
	if addr.Is4() {
		searcher = s.v4
	} else if addr.Is6() {
		searcher = s.v6
	}

	if searcher == nil {
		result := "无对应数据库"
		s.storeCache(ip, result)
		return result
	}

	result, err := searcher.Search(ip)
	if err != nil {
		result = fmt.Sprintf("查询失败:%s", err.Error())
		s.storeCache(ip, result)
		return result
	}

	result = formatGeoResult(result)
	s.storeCache(ip, result)
	return result
}

// storeCache 写入缓存并维护计数，超过上限时整体清空
func (s *IPSearcher) storeCache(ip, result string) {
	if _, loaded := s.cache.LoadOrStore(ip, result); !loaded {
		if atomic.AddInt64(&s.cacheCount, 1) > maxCacheEntries {
			s.clearCache()
		}
	}
}

func (s *IPSearcher) clearCache() {
	var n int64
	s.cache.Range(func(key, value interface{}) bool {
		s.cache.Delete(key)
		n++
		return true
	})
	atomic.StoreInt64(&s.cacheCount, 0)
	log.Printf("[INFO] IP 缓存已清理，共清理 %d 条", n)
}

// formatGeoResult 清洗 ip2region 返回结果
// IPv4: "亚洲|中国|江西|赣州||电信|..." → 省/市固定索引2,3，再取后续
// IPv6: "中国|省|市|...|运营商|CN"   → 省/市固定索引1,2，最后一段CN排除
// 通过首段是否为大洲名自动区分，直辖市(上海|上海)去重，空段忽略
func formatGeoResult(raw string) string {
	if raw == "" || strings.TrimSpace(raw) == "" {
		return "未知"
	}
	parts := strings.Split(raw, "|")

	pick := func(i int) string {
		if i >= 0 && i < len(parts) {
			return strings.TrimSpace(parts[i])
		}
		return ""
	}

	isV4 := isContinent(pick(0))

	var province, city string
	var extra []string

	if isV4 {
		province = pick(2)
		city = pick(3)
		// IPv4 格式: 大洲|国家|省|市|[区]|[运营商]|[...元数据...]
		// 位置字段只有前6段(索引0-5)，索引6开始是坐标/时区等元数据
		// 限制只取索引4、5(区县+运营商)，最多2个额外字段
		end := len(parts)
		if end > 6 {
			end = 6
		}
		for i := 4; i < end; i++ {
			extra = append(extra, pick(i))
		}
	} else {
		province = pick(1)
		city = pick(2)
		// IPv6 格式: 国家|省|市|[区]|[运营商]|[...元数据...]|国家代码
		// 位置字段只有前5段(索引0-4)，索引5开始是坐标/时区等元数据
		// 限制只取索引3、4(区县+运营商)，最多2个额外字段
		end := len(parts) - 1 // 先排除末尾国家代码
		if end > 5 {
			// 超过5段说明运营商后面还有元数据，截断到索引5(即最多收集索引3和4)
			end = 5
		}
		for i := 3; i < end; i++ {
			extra = append(extra, pick(i))
		}
	}

	var result []string
	if province != "" && province != "0" {
		result = append(result, province)
	}
	if city != "" && city != "0" && city != province {
		result = append(result, city)
	}
	for _, s := range extra {
		if s != "" && s != "0" {
			result = append(result, s)
		}
	}

	if len(result) == 0 {
		return "未知"
	}
	return strings.Join(result, "")
}

func isContinent(s string) bool {
	switch s {
	case "亚洲", "欧洲", "非洲", "北美洲", "南美洲", "大洋洲", "南极洲":
		return true
	}
	return false
}

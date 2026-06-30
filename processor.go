package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

// processRotatedFile 读取轮转后的旧文件中未处理的数据，返回 (成功数, 失败数, 最终已消费偏移)
func processRotatedFile(oldFilePath string, offset int64, searcher *IPSearcher, ipField, geoField string, writer *bufio.Writer) (int64, int64, int64) {
	log.Printf("[INFO] 处理轮转旧文件: %s (offset=%d)", oldFilePath, offset)

	f, err := os.Open(oldFilePath)
	if err != nil {
		log.Printf("[WARN] 无法打开旧文件 %s: %v", oldFilePath, err)
		return 0, 0, offset
	}
	defer f.Close()

	pos := offset
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		log.Printf("[WARN] 无法 seek 旧文件到 offset=%d: %v，从头开始", offset, err)
		pos = 0
	}

	reader := bufio.NewReaderSize(f, 128*1024)
	var processed, errs int64

	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			pos += int64(len(line))
			trimmed := strings.TrimRight(line, "\n\r")
			if trimmed != "" {
				if writeEnriched(trimmed, searcher, ipField, geoField, writer) {
					processed++
				} else {
					errs++
				}
			}
			break
		}
		if err != nil {
			log.Printf("[ERROR] 读取旧文件出错: %v", err)
			break
		}

		pos += int64(len(line))
		trimmed := strings.TrimRight(line, "\n\r")
		if trimmed != "" {
			if writeEnriched(trimmed, searcher, ipField, geoField, writer) {
				processed++
			} else {
				errs++
			}
		}
	}

	writer.Flush()
	log.Printf("[INFO] 旧文件处理完成: %s, %d 条成功, %d 条失败, 最终偏移=%d", oldFilePath, processed, errs, pos)
	return processed, errs, pos
}

func enrichLine(line string, searcher *IPSearcher, ipField, geoField string) (string, error) {
	var logEntry map[string]interface{}
	// 使用 UseNumber 保留数字字段的原始精度（避免大整数被转成 float64 丢精度或变科学计数法）
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&logEntry); err != nil {
		return "", fmt.Errorf("JSON 解析失败: %w", err)
	}

	ipValue, ok := logEntry[ipField]
	if !ok {
		logEntry[geoField] = "无IP字段"
	} else {
		ip, ok := ipValue.(string)
		if !ok {
			logEntry[geoField] = "IP字段格式错误"
		} else if ip == "" || ip == "-" {
			logEntry[geoField] = "无IP"
		} else {
			logEntry[geoField] = searcher.Lookup(ip)
		}
	}

	result, err := json.Marshal(logEntry)
	if err != nil {
		return "", fmt.Errorf("JSON 序列化失败: %w", err)
	}

	return string(result), nil
}

// writeEnriched 增强一行日志并写入，返回是否成功
func writeEnriched(line string, searcher *IPSearcher, ipField, geoField string, writer *bufio.Writer) bool {
	enriched, err := enrichLine(line, searcher, ipField, geoField)
	if err != nil {
		log.Printf("[WARN] 处理行失败: %v，原始内容: %.200s", err, line)
		// 用 json.Marshal 构造，保证 fallback 仍是合法 JSON（原始行可能非法，须作为字符串转义）
		fallback, mErr := json.Marshal(map[string]string{
			"_error": err.Error(),
			"_raw":   line,
		})
		if mErr != nil {
			fallback = []byte(`{"_error":"fallback marshal failed"}`)
		}
		writer.Write(fallback)
		writer.WriteByte('\n')
		return false
	}
	writer.WriteString(enriched)
	writer.WriteByte('\n')
	return true
}

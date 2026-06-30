package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

// FieldCleaner 字段值清洗配置。
// Fields 为需要清洗的字段名集合，replacer 预编译去除字符。
// Fields 为空时不启用清洗（零开销）。
type FieldCleaner struct {
	Fields   map[string]bool
	replacer *strings.Replacer // 预编译的字符去除器
}

// NewFieldCleaner 创建字段清洗器，预编译 replacer 避免每条日志重复构建。
// chars 为要去除的字符集合，如 "[]"。
func NewFieldCleaner(fields map[string]bool, chars string) *FieldCleaner {
	if len(fields) == 0 {
		return nil
	}
	oldnew := make([]string, 0, len(chars)*2)
	for _, c := range chars {
		oldnew = append(oldnew, string(c), "")
	}
	return &FieldCleaner{
		Fields:   fields,
		replacer: strings.NewReplacer(oldnew...),
	}
}

// processRotatedFile 读取轮转后的旧文件中未处理的数据，返回 (成功数, 失败数, 最终已消费偏移)
func processRotatedFile(oldFilePath string, offset int64, searcher *IPSearcher, ipField, geoField string, writer *bufio.Writer, errWriter *bufio.Writer, cleaner *FieldCleaner) (int64, int64, int64) {
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
				if writeEnriched(trimmed, searcher, ipField, geoField, writer, errWriter, cleaner) {
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
			if writeEnriched(trimmed, searcher, ipField, geoField, writer, errWriter, cleaner) {
				processed++
			} else {
				errs++
			}
		}
	}

	writer.Flush()
	errWriter.Flush()
	log.Printf("[INFO] 旧文件处理完成: %s, %d 条成功, %d 条失败, 最终偏移=%d", oldFilePath, processed, errs, pos)
	return processed, errs, pos
}

func enrichLine(line string, searcher *IPSearcher, ipField, geoField string, cleaner *FieldCleaner) (string, error) {
	var logEntry map[string]interface{}
	// 使用 UseNumber 保留数字字段的原始精度（避免大整数被转成 float64 丢精度或变科学计数法）
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&logEntry); err != nil {
		return "", fmt.Errorf("JSON 解析失败: %w", err)
	}

	// 清洗指定字段值中的干扰字符（如方括号），仅 cleaner 非 nil 时启用
	if cleaner != nil {
		for field := range cleaner.Fields {
			if v, ok := logEntry[field]; ok {
				if s, ok := v.(string); ok {
					logEntry[field] = cleaner.replacer.Replace(s)
				}
			}
		}
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

	// 使用 Encoder 并关闭 HTML 转义，避免 & < > 被转成 & 等 Unicode 转义序列
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(logEntry); err != nil {
		return "", fmt.Errorf("JSON 序列化失败: %w", err)
	}

	// Encode 会在末尾追加换行符，去掉它保持与原行一致
	return string(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// writeEnriched 增强一行日志并写入。成功时写入 writer，解析/处理失败时写入 errWriter。
// 返回是否成功。
func writeEnriched(line string, searcher *IPSearcher, ipField, geoField string, writer *bufio.Writer, errWriter *bufio.Writer, cleaner *FieldCleaner) bool {
	enriched, err := enrichLine(line, searcher, ipField, geoField, cleaner)
	if err != nil {
		log.Printf("[WARN] 处理行失败: %v，原始内容: %.200s", err, line)
		// 构造 fallback JSON，写入错误日志文件，关闭 HTML 转义保持原内容不变
		var fbBuf bytes.Buffer
		fbEnc := json.NewEncoder(&fbBuf)
		fbEnc.SetEscapeHTML(false)
		mErr := fbEnc.Encode(map[string]string{
			"_error": err.Error(),
			"_raw":   line,
		})
		fallback := bytes.TrimRight(fbBuf.Bytes(), "\n")
		if mErr != nil {
			fallback = []byte(`{"_error":"fallback marshal failed"}`)
		}
		errWriter.Write(fallback)
		errWriter.WriteByte('\n')
		return false
	}
	writer.WriteString(enriched)
	writer.WriteByte('\n')
	return true
}

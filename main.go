package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	inputFile := flag.String("input", "/alidata/logs/nginx/accesslog.log", "nginx 访问日志文件路径")
	outputFile := flag.String("output", "/alidata/logs/nginx/accesslog_geo.log", "输出日志文件路径（带地理位置）")
	v4DBPath := flag.String("v4db", "/Users/daizl/daychou/pub-nali-api/data/ip2region_v4.xdb", "ip2region IPv4 数据库路径")
	v6DBPath := flag.String("v6db", "/Users/daizl/daychou/pub-nali-api/data/ip2region_v6.xdb", "ip2region IPv6 数据库路径")
	ipField := flag.String("field", "remote", "JSON 中包含 IP 地址的字段名")
	geoField := flag.String("geofield", "geo", "输出 JSON 中存放地理位置的字段名")
	checkpointPath := flag.String("checkpoint", "", "断点文件路径（默认为输出文件同目录下的 .checkpoint）")
	cleanFields := flag.String("clean-fields", "", "需要清洗字符的字段名，逗号分隔（如 real_ip,x_forwarded_for）")
	cleanChars := flag.String("clean-chars", "[]", "要去除的字符集合")
	flag.Parse()

	if *inputFile == *outputFile {
		log.Fatalf("[FATAL] 输入文件和输出文件不能相同: %s", *inputFile)
	}

	if *checkpointPath == "" {
		*checkpointPath = filepath.Join(filepath.Dir(*outputFile), ".nginx-geo-enricher.checkpoint")
	}

	log.Printf("[INFO] ======== nginx 日志地理位置增强工具 ========")
	log.Printf("[INFO] 输入文件:    %s", *inputFile)
	log.Printf("[INFO] 输出文件:    %s", *outputFile)
	log.Printf("[INFO] 断点文件:    %s", *checkpointPath)
	log.Printf("[INFO] IP 字段:     %s", *ipField)
	log.Printf("[INFO] Geo 字段:    %s", *geoField)

	// 构建字段清洗器（clean-fields 为空则不启用）
	var cleaner *FieldCleaner
	if *cleanFields != "" {
		fields := make(map[string]bool)
		for _, f := range strings.Split(*cleanFields, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				fields[f] = true
			}
		}
		if len(fields) > 0 {
			cleaner = NewFieldCleaner(fields, *cleanChars)
			log.Printf("[INFO] 字段清洗已启用: fields=%s chars=%q", *cleanFields, *cleanChars)
		}
	}

	// 启动时预检断点文件目录写入权限，避免跑起来才发现写不了
	if err := touchCheckpoint(*checkpointPath); err != nil {
		log.Fatalf("[FATAL] 断点文件权限检查失败: %v", err)
	}
	log.Printf("[INFO] 断点文件写入权限检查通过: %s", *checkpointPath)

	// 初始化 IP 查询器
	searcher, err := NewIPSearcher(*v4DBPath, *v6DBPath)
	if err != nil {
		log.Fatalf("[FATAL] 初始化 IP 查询器失败: %v", err)
	}

	// 打开输出文件（追加模式）
	outFile, err := os.OpenFile(*outputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("[FATAL] 无法打开输出文件 %s: %v", *outputFile, err)
	}
	defer outFile.Close()

	writer := bufio.NewWriterSize(outFile, 512*1024)

	// ======== 断点恢复 ========
	cp, cpErr := loadCheckpoint(*checkpointPath)
	tailer := NewLogTailer(*inputFile)

	if cpErr == nil && cp != nil {
		log.Printf("[INFO] 发现断点文件: inode=%d offset=%d (%s)", cp.Inode, cp.Offset, cp.Updated)

		currentInode, statErr := getFileInode(*inputFile)
		switch {
		case statErr != nil:
			// 当前文件暂不可读（可能正处于轮转窗口），按断点交给 tailer，待文件出现后由 inode 判定
			log.Printf("[WARN] 暂时无法获取当前文件状态: %v，按断点交给 tailer 处理", statErr)
			tailer.SetResume(cp.Inode, cp.Offset)
		case currentInode == cp.Inode:
			// 同一个文件，直接续传
			log.Printf("[INFO] 文件 inode 匹配，从 offset=%d 续传", cp.Offset)
			tailer.SetResume(cp.Inode, cp.Offset)
		default:
			// inode 不同 = 停机期间 logrotate 轮转了
			// 1) 尝试找到被重命名的旧文件，补齐 cp.Offset 之后的剩余数据
			dir := filepath.Dir(*inputFile)
			oldFile, findErr := findFileByInode(dir, cp.Inode)
			if findErr == nil {
				log.Printf("[INFO] 找到轮转后的旧文件: %s", oldFile)
				p, e, finalOff := processRotatedFile(oldFile, cp.Offset, searcher, *ipField, *geoField, writer, cleaner)
				// 先 flush 输出再落 checkpoint，标记旧文件已消费完，保证补齐过程崩溃可恢复
				if err := writer.Flush(); err != nil {
					log.Printf("[WARN] flush 输出失败: %v", err)
				} else {
					saveCheckpoint(*checkpointPath, &Checkpoint{Path: *inputFile, Inode: cp.Inode, Offset: finalOff})
				}
				log.Printf("[INFO] 旧文件补齐完成: %d 条成功, %d 条失败, 最终偏移=%d", p, e, finalOff)
			} else {
				log.Printf("[WARN] 未找到 inode=%d 的旧文件（可能已被 logrotate 删除），可能丢失少量数据", cp.Inode)
			}
			// 2) 让 tailer 从当前新文件头开始读取：
			//    SetResume 传入的 inode 与当前文件不匹配 → openFile 走"停机期间已轮转"分支，从 offset 0 读
			tailer.SetResume(cp.Inode, cp.Offset)
			log.Printf("[INFO] 将从当前新文件 %s (inode=%d) 头部开始读取", *inputFile, currentInode)
		}
	} else {
		log.Printf("[INFO] 无有效断点，从文件末尾开始追踪新数据")
	}

	// 启动日志追踪器
	go tailer.Start()

	// 监听退出信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// 主处理循环
	var processed, errors int64
	startTime := time.Now()
	var lastOffset int64
	var lastInode uint64
	var linesSinceSave int64

	// saveProgress 关键：先把输出落盘（flush），再持久化断点。
	// 这样断点永远不会"领先"于已写出的数据 —— 崩溃/kill -9 重启后最多重读极少量已写数据，绝不丢日志。
	saveProgress := func() {
		if lastOffset <= 0 {
			return
		}
		if err := writer.Flush(); err != nil {
			// flush 失败则不推进断点，避免把尚未落盘的数据标记为已处理
			log.Printf("[WARN] flush 输出失败，暂不推进断点: %v", err)
			return
		}
		// 如需抗"断电"（而非仅进程崩溃），可在此追加 outFile.Sync()，但有明显性能开销
		saveCheckpoint(*checkpointPath, &Checkpoint{Path: *inputFile, Inode: lastInode, Offset: lastOffset})
		linesSinceSave = 0
	}

	// rotateOutput 在输入日志轮转时同步轮转输出文件。
	// 将当前输出文件重命名为带日期后缀（与原日志轮转格式一致），然后打开新文件继续写入。
	rotateOutput := func(rotateTime time.Time) {
		// 1. 先 flush 确保旧 writer 缓冲区数据全部落盘
		if err := writer.Flush(); err != nil {
			log.Printf("[WARN] 轮转前 flush 输出失败: %v", err)
		}
		// 2. 关闭当前输出文件
		if err := outFile.Close(); err != nil {
			log.Printf("[WARN] 关闭旧输出文件失败: %v", err)
		}

		// 3. 重命名输出文件，日期后缀格式与输入日志轮转一致 (例如 accesslog.log-20260630)
		dateSuffix := rotateTime.Format("20060102")
		rotatedPath := *outputFile + "-" + dateSuffix
		// 若目标已存在（极少情况，如同一天多次轮转），追加序号
		if _, err := os.Stat(rotatedPath); err == nil {
			for i := 1; i < 100; i++ {
				altPath := fmt.Sprintf("%s-%s.%d", *outputFile, dateSuffix, i)
				if _, err := os.Stat(altPath); os.IsNotExist(err) {
					rotatedPath = altPath
					break
				}
			}
		}
		if err := os.Rename(*outputFile, rotatedPath); err != nil {
			log.Printf("[WARN] 轮转输出文件失败: %v", err)
		} else {
			log.Printf("[INFO] 输出文件已轮转: %s -> %s", *outputFile, rotatedPath)
		}

		// 4. 打开新的输出文件（追加模式）
		newFile, err := os.OpenFile(*outputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatalf("[FATAL] 无法重新打开输出文件 %s: %v", *outputFile, err)
		}
		outFile = newFile
		writer = bufio.NewWriterSize(newFile, 512*1024)
		log.Printf("[INFO] 新输出文件已创建: %s", *outputFile)
	}

	log.Printf("[INFO] 开始处理日志...")

	// processOne 处理单行日志，内部按阈值触发 flush。
	// 500 行阈值配合 512KB 缓冲区：500*800 字节 ≈ 400KB < 512KB，
	// 缓冲区永远装得下两次 checkpoint 之间的全部数据，bufio 不会自动落盘，
	// 从而杜绝"bufio 已落盘但 checkpoint 未更新 → kill -9 重启后数据重复"。
	processOne := func(ll LogLine) {
		if writeEnriched(ll.Data, searcher, *ipField, *geoField, writer, cleaner) {
			processed++
		} else {
			errors++
		}

		if ll.Offset > 0 {
			lastOffset = ll.Offset
			lastInode = ll.Inode
			linesSinceSave++
			if linesSinceSave >= 500 {
				saveProgress()
			}
		}

		if processed > 0 && processed%10000 == 0 {
			elapsed := time.Since(startTime).Seconds()
			rate := float64(processed) / elapsed
			log.Printf("[STATS] 已处理 %d 条 (%.0f 条/秒), 错误 %d 条, 断点 offset=%d",
				processed, rate, errors, lastOffset)
		}
	}

loop:
	for {
		// 第一阶段：阻塞等待至少一行或退出信号
		select {
		case ll, ok := <-tailer.Lines():
			if !ok {
				break loop
			}
			processOne(ll)
		case rotateTime := <-tailer.RotateEvents():
			log.Printf("[INFO] 输入日志已轮转，同步轮转输出文件")
			rotateOutput(rotateTime)
		case sig := <-sigChan:
			log.Printf("[INFO] 收到信号 %v，正在优雅退出...", sig)
			tailer.Stop()
			for ll := range tailer.Lines() {
				processOne(ll)
			}
			break loop
		}

		// 第二阶段：尽可能多地非阻塞消费 channel 中的行，同时响应信号。
		// drain 循环内每次迭代都检查信号通道——解决消费高峰期
		// Ctrl+C / kill 无法退出的问题。
	drainLoop:
		for {
			select {
			case ll, ok := <-tailer.Lines():
				if !ok {
					break loop
				}
				processOne(ll)
			case rotateTime := <-tailer.RotateEvents():
				log.Printf("[INFO] 输入日志已轮转，同步轮转输出文件")
				rotateOutput(rotateTime)
			case sig := <-sigChan:
				log.Printf("[INFO] 收到信号 %v，正在优雅退出...", sig)
				tailer.Stop()
				for ll := range tailer.Lines() {
					processOne(ll)
				}
				break loop
			default:
				// channel 已空 → 立即 flush，解决文件读完（tailer 进入
				// EOF 等待）后最后一批数据滞留在 bufio.Writer 中的问题。
				if linesSinceSave > 0 {
					saveProgress()
				}
				break drainLoop
			}
		}
	}

	// 最终：先 flush 再存断点（顺序很重要，保证断点不领先于已落盘数据）
	saveProgress()
	log.Printf("[INFO] 最终断点已保存: offset=%d inode=%d", lastOffset, lastInode)

	elapsed := time.Since(startTime).Seconds()
	log.Printf("[INFO] ======== 程序退出 ========")
	log.Printf("[INFO] 总处理: %d 条, 错误: %d 条, 耗时: %.1f 秒", processed, errors, elapsed)
}

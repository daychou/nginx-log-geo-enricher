package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
	"time"
)

// LogLine 一行日志及其在文件中的精确偏移（用于断点续传）
type LogLine struct {
	Data   string // 日志内容（不含换行符）
	Offset int64  // 此行之后的下一个字节偏移（即断点续传的起始位置）
	Inode  uint64 // 读取时的文件 inode
}

type LogTailer struct {
	path         string
	file         *os.File
	inode        uint64
	reader       *bufio.Reader
	offset       int64 // 当前已消费的精确字节偏移（=下一个待读字节，也是任何未完成尾行的起点）
	started      bool  // 是否已完成首次打开（用于区分冷启动 vs 轮转后重开）
	linesChan    chan LogLine
	stopChan     chan struct{}
	resumeInode  uint64 // 断点续传：期望的 inode（0=不续传）
	resumeOffset int64  // 断点续传：seek 到这个偏移开始读
	rotateChan   chan time.Time // 通知主循环：输入日志已轮转，输出文件需要同步轮转
}

func NewLogTailer(path string) *LogTailer {
	return &LogTailer{
		path:       path,
		linesChan:  make(chan LogLine, 1000),
		stopChan:   make(chan struct{}),
		rotateChan: make(chan time.Time, 1), // 带缓冲，避免发送阻塞
	}
}

// SetResume 设置断点续传参数
func (t *LogTailer) SetResume(inode uint64, offset int64) {
	t.resumeInode = inode
	t.resumeOffset = offset
}

func (t *LogTailer) Lines() <-chan LogLine {
	return t.linesChan
}

// RotateEvents 返回输入日志轮转事件通道，主循环监听此通道以同步轮转输出文件
func (t *LogTailer) RotateEvents() <-chan time.Time {
	return t.rotateChan
}

func (t *LogTailer) Start() {
	// 文件由本协程独占，退出时在此关闭，避免与 Stop() 产生数据竞争
	defer close(t.linesChan)
	defer t.closeFile()

	for {
		select {
		case <-t.stopChan:
			return
		default:
		}

		if t.file == nil {
			if err := t.openFile(); err != nil {
				log.Printf("[WARN] 无法打开文件 %s: %v，1秒后重试...", t.path, err)
				t.waitOrStop(1 * time.Second)
				continue
			}
		}

		line, err := t.reader.ReadString('\n')

		switch {
		case err == nil:
			// 完整的一行（含换行符），offset 推进到此行之后
			t.offset += int64(len(line))
			data := strings.TrimRight(line, "\r\n")
			if data != "" {
				t.sendLine(LogLine{Data: data, Offset: t.offset, Inode: t.inode})
			}
			continue

		case err == io.EOF:
			// 优先检测文件截断（如 > accesslog.log 或 truncate），必须放在处理
			// 不完整行之前。截断保留 inode 但文件归零/变小，若不清零 offset，
			// 新数据会从 offset=0 开始写入，tailer 永远等在旧 offset 处读不到。
			if t.isTruncated() {
				newSize := int64(-1)
				if s, err := os.Stat(t.path); err == nil {
					newSize = s.Size()
				}
				log.Printf("[INFO] 检测到文件被截断, inode=%d 原偏移=%d 新大小=%d，重置偏移到 0",
					t.inode, t.offset, newSize)
				t.offset = 0
				if _, serr := t.file.Seek(0, io.SeekStart); serr == nil {
					t.reader.Reset(t.file)
				}
				continue
			}

			// EOF 时返回的 line 是尚无换行符的"不完整行"。
			// 对于实时跟踪的文件，绝不能把不完整行当成完整记录发出，
			// 否则会写出残缺 JSON，且后续追加的部分会被当成第二条记录。
			// 这里回退到行首，下次重新读取，直到换行符出现。
			if len(line) > 0 {
				if _, serr := t.file.Seek(t.offset, io.SeekStart); serr == nil {
					t.reader.Reset(t.file)
				}
			}

			// 在 EOF（旧文件已读尽）时检测 logrotate 轮转
			if t.isRotated() {
				log.Printf("[INFO] 检测到日志轮转, inode=%d 已读到偏移=%d，切换到新文件", t.inode, t.offset)
				// 通知主循环同步轮转输出文件
				select {
				case t.rotateChan <- time.Now():
				default:
				}
				t.closeFile() // 下一轮 openFile 会以"轮转重开"模式从新文件头(offset 0)读取
				continue
			}

			t.waitOrStop(100 * time.Millisecond)
			continue

		default:
			log.Printf("[ERROR] 读取文件出错: %v", err)
			t.closeFile()
			t.waitOrStop(1 * time.Second)
			continue
		}
	}
}

func (t *LogTailer) sendLine(ll LogLine) {
	select {
	case t.linesChan <- ll:
	case <-t.stopChan:
	}
}

func (t *LogTailer) openFile() error {
	f, err := os.Open(t.path)
	if err != nil {
		return err
	}

	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}

	sysStat, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		f.Close()
		return errors.New("无法获取文件 inode")
	}

	t.file = f
	t.inode = sysStat.Ino

	// 决定起始读取位置：
	//   1) 断点续传且 inode 匹配  → 从上次的 offset 继续（不丢不重）
	//   2) 断点续传但 inode 不匹配 → 停机期间已轮转，新文件从头(0)读
	//   3) 无断点 + 首次冷启动      → 从文件末尾开始（只处理新增数据，跳过历史）
	//   4) 运行期间轮转后重开       → 新文件从头(0)读（否则会丢掉检测到轮转前已写入新文件的数据）
	var startPos int64
	var reason string
	switch {
	case t.resumeInode != 0 && t.resumeInode == t.inode:
		startPos = t.resumeOffset
		if startPos > stat.Size() {
			// 异常情况（文件被截断/变短），回退到文件末尾，避免 seek 越界后空等
			startPos = stat.Size()
		}
		reason = fmt.Sprintf("断点续传 offset=%d", startPos)
	case t.resumeInode != 0:
		startPos = 0
		reason = fmt.Sprintf("停机期间已轮转(inode %d→%d)，新文件从头读", t.resumeInode, t.inode)
	case !t.started:
		startPos = stat.Size()
		reason = "首次启动，从文件末尾读取新增数据"
	default:
		startPos = 0
		reason = "轮转后重开，新文件从头读"
	}

	if _, err := f.Seek(startPos, io.SeekStart); err != nil {
		f.Close()
		t.file = nil
		t.inode = 0
		return fmt.Errorf("seek 到 offset=%d 失败: %w", startPos, err)
	}

	t.offset = startPos
	t.resumeInode = 0
	t.resumeOffset = 0
	t.started = true
	t.reader = bufio.NewReaderSize(f, 128*1024)
	log.Printf("[INFO] 开始追踪文件: %s (inode=%d, %s)", t.path, t.inode, reason)
	return nil
}

func (t *LogTailer) isRotated() bool {
	stat, err := os.Stat(t.path)
	if err != nil {
		return false
	}
	sysStat, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return sysStat.Ino != t.inode
}

// isTruncated 检测文件是否被截断（如 echo > accesslog.log 或 : > accesslog.log）。
// 和 logrotate 不同，截断操作保留原 inode，仅将文件大小归零，此时必须重置 offset=0，
// 否则新写入的数据在 offset 以下，tailer 永远读不到。
func (t *LogTailer) isTruncated() bool {
	stat, err := os.Stat(t.path)
	if err != nil {
		return false
	}
	sysStat, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	// 必须 inode 相同（排除 logrotate 轮转），且文件大小显著小于已读偏移
	return sysStat.Ino == t.inode && stat.Size() < t.offset
}

func (t *LogTailer) closeFile() {
	if t.file != nil {
		t.file.Close()
		t.file = nil
		t.reader = nil
		t.inode = 0
	}
}

func (t *LogTailer) waitOrStop(d time.Duration) {
	select {
	case <-t.stopChan:
	case <-time.After(d):
	}
}

func (t *LogTailer) Stop() {
	// 仅发出停止信号；文件由 Start() 协程在退出时自行关闭，避免数据竞争
	close(t.stopChan)
}

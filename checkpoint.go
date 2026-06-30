package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Checkpoint 持久化读取进度，用于重启后断点续接
type Checkpoint struct {
	Path    string `json:"path"`    // 日志文件路径
	Inode   uint64 `json:"inode"`   // 文件 inode（检测轮转）
	Offset  int64  `json:"offset"`  // 已消费的字节偏移量（下次从这里开始读）
	Updated string `json:"updated"` // 最后更新时间
}

func loadCheckpoint(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("解析断点文件失败: %w", err)
	}
	return &cp, nil
}

func saveCheckpoint(path string, cp *Checkpoint) {
	cp.Updated = time.Now().Format(time.RFC3339)
	data, err := json.Marshal(cp)
	if err != nil {
		log.Fatalf("[FATAL] 序列化断点数据失败: %v", err)
	}
	// 原子写入：先写临时文件，再 rename
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		log.Fatalf("[FATAL] 写入断点临时文件失败 (路径=%s): %v", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		log.Fatalf("[FATAL] 重命名断点文件失败 (%s -> %s): %v", tmpPath, path, err)
	}
}

// touchCheckpoint 检查断点文件所在目录是否可写（启动时权限预检）。
// 文件存在则打开不修改内容，不存在则创建空文件验证目录可写。
func touchCheckpoint(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("无法打开/创建断点文件 %s: %w", path, err)
	}
	defer f.Close()
	return nil
}

func getFileInode(filePath string) (uint64, error) {
	stat, err := os.Stat(filePath)
	if err != nil {
		return 0, err
	}
	sysStat, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("无法获取文件 inode")
	}
	return sysStat.Ino, nil
}

// findFileByInode 在目录中搜索指定 inode 的文件（用于找 logrotate 重命名后的旧文件）
func findFileByInode(dir string, targetInode uint64) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fullPath := filepath.Join(dir, entry.Name())
		inode, err := getFileInode(fullPath)
		if err != nil {
			continue
		}
		if inode == targetInode {
			return fullPath, nil
		}
	}
	return "", fmt.Errorf("在目录 %s 中未找到 inode=%d 的文件", dir, targetInode)
}

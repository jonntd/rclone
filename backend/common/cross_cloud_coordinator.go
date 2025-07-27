// Package common provides unified cross-cloud transfer coordination
package common

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
)

// CrossCloudCoordinator 跨云传输协调器
// 🚀 统一管理跨云传输的状态协调和断点续传
type CrossCloudCoordinator struct {
	mu              sync.RWMutex
	activeTransfers map[string]*CrossCloudTransfer // 活跃的跨云传输
	resumeManager   UnifiedResumeManager           // 断点续传管理器
	errorHandler    *UnifiedErrorHandler           // 统一错误处理器
	tempFileManager *TempFileManager               // 临时文件管理器
	transferStats   *CrossCloudStats               // 跨云传输统计
}

// CrossCloudTransfer 跨云传输状态
type CrossCloudTransfer struct {
	TransferID      string                 // 传输唯一标识
	SourceFSName    string                 // 源文件系统名称
	DestFS          fs.Fs                  // 目标文件系统
	SourceObj       fs.ObjectInfo          // 源对象信息
	DestPath        string                 // 目标路径
	FileSize        int64                  // 文件大小
	Status          CrossCloudStatus       // 传输状态
	DownloadedBytes int64                  // 已下载字节数
	UploadedBytes   int64                  // 已上传字节数
	TempFilePath    string                 // 临时文件路径
	MD5Hash         string                 // 文件MD5哈希
	StartTime       time.Time              // 开始时间
	LastUpdateTime  time.Time              // 最后更新时间
	RetryCount      int                    // 重试次数
	ErrorHistory    []error                // 错误历史
	Metadata        map[string]interface{} // 扩展元数据
}

// CrossCloudStatus 跨云传输状态枚举
type CrossCloudStatus int

const (
	StatusPending CrossCloudStatus = iota
	StatusDownloading
	StatusDownloadComplete
	StatusUploading
	StatusUploadComplete
	StatusCompleted
	StatusFailed
	StatusCancelled
)

func (s CrossCloudStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusDownloading:
		return "downloading"
	case StatusDownloadComplete:
		return "download_complete"
	case StatusUploading:
		return "uploading"
	case StatusUploadComplete:
		return "upload_complete"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// CrossCloudStats 跨云传输统计
type CrossCloudStats struct {
	mu                    sync.RWMutex
	TotalTransfers        int64            `json:"total_transfers"`
	CompletedTransfers    int64            `json:"completed_transfers"`
	FailedTransfers       int64            `json:"failed_transfers"`
	TotalBytesTransferred int64            `json:"total_bytes_transferred"`
	AverageSpeed          float64          `json:"average_speed_mbps"`
	DuplicateDownloads    int64            `json:"duplicate_downloads_avoided"`
	ResumedTransfers      int64            `json:"resumed_transfers"`
	ErrorsByType          map[string]int64 `json:"errors_by_type"`
}

// TempFileManager 临时文件管理器
type TempFileManager struct {
	mu        sync.RWMutex
	tempFiles map[string]*TempFileInfo // 临时文件信息
	tempDir   string                   // 临时文件目录
}

// TempFileInfo 临时文件信息
type TempFileInfo struct {
	FilePath     string    // 文件路径
	FileSize     int64     // 文件大小
	MD5Hash      string    // MD5哈希
	CreatedTime  time.Time // 创建时间
	LastAccessed time.Time // 最后访问时间
	RefCount     int       // 引用计数
}

// NewCrossCloudCoordinator 创建跨云传输协调器
func NewCrossCloudCoordinator(resumeManager UnifiedResumeManager, errorHandler *UnifiedErrorHandler) *CrossCloudCoordinator {
	tempDir := filepath.Join(os.TempDir(), "rclone_cross_cloud")
	os.MkdirAll(tempDir, 0755)

	return &CrossCloudCoordinator{
		activeTransfers: make(map[string]*CrossCloudTransfer),
		resumeManager:   resumeManager,
		errorHandler:    errorHandler,
		tempFileManager: &TempFileManager{
			tempFiles: make(map[string]*TempFileInfo),
			tempDir:   tempDir,
		},
		transferStats: &CrossCloudStats{
			ErrorsByType: make(map[string]int64),
		},
	}
}

// StartTransfer 开始跨云传输
// 🔧 优化：统一的传输启动逻辑，避免重复下载
func (c *CrossCloudCoordinator) StartTransfer(ctx context.Context, sourceObj fs.ObjectInfo, destFS fs.Fs, destPath string) (*CrossCloudTransfer, error) {
	transferID := c.generateTransferID(sourceObj, destFS, destPath)

	c.mu.Lock()
	defer c.mu.Unlock()

	// 检查是否已有活跃的传输
	if existingTransfer, exists := c.activeTransfers[transferID]; exists {
		if existingTransfer.Status != StatusFailed && existingTransfer.Status != StatusCancelled {
			fs.Debugf(nil, "🔄 跨云传输已存在，返回现有传输: %s", transferID)
			return existingTransfer, nil
		}
		// 清理失败的传输
		delete(c.activeTransfers, transferID)
	}

	// 创建新的传输
	transfer := &CrossCloudTransfer{
		TransferID:     transferID,
		SourceFSName:   sourceObj.Fs().Name(),
		DestFS:         destFS,
		SourceObj:      sourceObj,
		DestPath:       destPath,
		FileSize:       sourceObj.Size(),
		Status:         StatusPending,
		StartTime:      time.Now(),
		LastUpdateTime: time.Now(),
		Metadata:       make(map[string]interface{}),
	}

	c.activeTransfers[transferID] = transfer
	c.transferStats.TotalTransfers++

	fs.Infof(nil, "🌐 开始跨云传输: %s → %s (大小: %s, ID: %s)",
		transfer.SourceFSName, destFS.Name(), fs.SizeSuffix(sourceObj.Size()), transferID)

	return transfer, nil
}

// CheckExistingDownload 检查是否存在已下载的文件
// 🔧 关键优化：避免重复下载，提高跨云传输效率
func (c *CrossCloudCoordinator) CheckExistingDownload(ctx context.Context, sourceObj fs.ObjectInfo) (io.ReadCloser, int64, func(), error) {
	// 生成文件标识
	fileID := c.generateFileID(sourceObj)

	c.tempFileManager.mu.RLock()
	tempInfo, exists := c.tempFileManager.tempFiles[fileID]
	c.tempFileManager.mu.RUnlock()

	if !exists {
		return nil, 0, nil, nil
	}

	// 检查临时文件是否存在且完整
	if _, err := os.Stat(tempInfo.FilePath); os.IsNotExist(err) {
		// 文件不存在，清理记录
		c.tempFileManager.mu.Lock()
		delete(c.tempFileManager.tempFiles, fileID)
		c.tempFileManager.mu.Unlock()
		return nil, 0, nil, nil
	}

	// 验证文件大小
	fileInfo, err := os.Stat(tempInfo.FilePath)
	if err != nil || fileInfo.Size() != sourceObj.Size() {
		// 文件不完整，清理
		c.cleanupTempFile(fileID)
		return nil, 0, nil, nil
	}

	// 打开文件
	file, err := os.Open(tempInfo.FilePath)
	if err != nil {
		return nil, 0, nil, err
	}

	// 增加引用计数
	c.tempFileManager.mu.Lock()
	tempInfo.RefCount++
	tempInfo.LastAccessed = time.Now()
	c.tempFileManager.mu.Unlock()

	fs.Infof(nil, "🎯 发现已下载文件，避免重复下载: %s (大小: %s)",
		tempInfo.FilePath, fs.SizeSuffix(tempInfo.FileSize))

	c.transferStats.mu.Lock()
	c.transferStats.DuplicateDownloads++
	c.transferStats.mu.Unlock()

	cleanup := func() {
		file.Close()
		c.tempFileManager.mu.Lock()
		if tempInfo.RefCount > 0 {
			tempInfo.RefCount--
		}
		c.tempFileManager.mu.Unlock()
	}

	return file, tempInfo.FileSize, cleanup, nil
}

// SaveDownloadedFile 保存下载的文件
// 🔧 优化：统一的临时文件管理，支持断点续传
func (c *CrossCloudCoordinator) SaveDownloadedFile(ctx context.Context, sourceObj fs.ObjectInfo, reader io.Reader) (string, string, error) {
	fileID := c.generateFileID(sourceObj)
	tempFilePath := filepath.Join(c.tempFileManager.tempDir, fmt.Sprintf("%s.tmp", fileID))

	// 创建临时文件
	tempFile, err := os.Create(tempFilePath)
	if err != nil {
		return "", "", fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer tempFile.Close()

	// 计算MD5并写入文件
	hash := md5.New()
	multiWriter := io.MultiWriter(tempFile, hash)

	written, err := io.Copy(multiWriter, reader)
	if err != nil {
		os.Remove(tempFilePath)
		return "", "", fmt.Errorf("写入临时文件失败: %w", err)
	}

	md5Hash := fmt.Sprintf("%x", hash.Sum(nil))

	// 验证文件大小
	if written != sourceObj.Size() {
		os.Remove(tempFilePath)
		return "", "", fmt.Errorf("文件大小不匹配: 期望%d，实际%d", sourceObj.Size(), written)
	}

	// 保存临时文件信息
	c.tempFileManager.mu.Lock()
	c.tempFileManager.tempFiles[fileID] = &TempFileInfo{
		FilePath:     tempFilePath,
		FileSize:     written,
		MD5Hash:      md5Hash,
		CreatedTime:  time.Now(),
		LastAccessed: time.Now(),
		RefCount:     1,
	}
	c.tempFileManager.mu.Unlock()

	fs.Infof(nil, "✅ 跨云传输文件下载完成: %s (大小: %s, MD5: %s)",
		tempFilePath, fs.SizeSuffix(written), md5Hash[:8])

	return tempFilePath, md5Hash, nil
}

// UpdateTransferStatus 更新传输状态
func (c *CrossCloudCoordinator) UpdateTransferStatus(transferID string, status CrossCloudStatus, downloadedBytes, uploadedBytes int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	transfer, exists := c.activeTransfers[transferID]
	if !exists {
		return fmt.Errorf("传输不存在: %s", transferID)
	}

	transfer.Status = status
	transfer.DownloadedBytes = downloadedBytes
	transfer.UploadedBytes = uploadedBytes
	transfer.LastUpdateTime = time.Now()

	fs.Debugf(nil, "🔄 跨云传输状态更新: %s -> %s (下载: %s, 上传: %s)",
		transferID, status.String(),
		fs.SizeSuffix(downloadedBytes), fs.SizeSuffix(uploadedBytes))

	return nil
}

// CompleteTransfer 完成传输
func (c *CrossCloudCoordinator) CompleteTransfer(transferID string, success bool, err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	transfer, exists := c.activeTransfers[transferID]
	if !exists {
		return fmt.Errorf("传输不存在: %s", transferID)
	}

	if success {
		transfer.Status = StatusCompleted
		c.transferStats.CompletedTransfers++
		c.transferStats.TotalBytesTransferred += transfer.FileSize

		fs.Infof(nil, "✅ 跨云传输完成: %s (耗时: %v)",
			transferID, time.Since(transfer.StartTime))
	} else {
		transfer.Status = StatusFailed
		if err != nil {
			transfer.ErrorHistory = append(transfer.ErrorHistory, err)
		}
		c.transferStats.FailedTransfers++

		fs.Errorf(nil, "❌ 跨云传输失败: %s (错误: %v)", transferID, err)
	}

	// 清理活跃传输记录
	delete(c.activeTransfers, transferID)

	return nil
}

// 辅助函数
func (c *CrossCloudCoordinator) generateTransferID(sourceObj fs.ObjectInfo, destFS fs.Fs, destPath string) string {
	return fmt.Sprintf("%s_%s_%s_%d",
		sourceObj.Fs().Name(),
		destFS.Name(),
		strings.ReplaceAll(destPath, "/", "_"),
		sourceObj.Size())
}

func (c *CrossCloudCoordinator) generateFileID(sourceObj fs.ObjectInfo) string {
	return fmt.Sprintf("%s_%s_%d_%d",
		sourceObj.Fs().Name(),
		strings.ReplaceAll(sourceObj.Remote(), "/", "_"),
		sourceObj.Size(),
		sourceObj.ModTime(context.Background()).Unix())
}

func (c *CrossCloudCoordinator) cleanupTempFile(fileID string) {
	c.tempFileManager.mu.Lock()
	defer c.tempFileManager.mu.Unlock()

	if tempInfo, exists := c.tempFileManager.tempFiles[fileID]; exists {
		os.Remove(tempInfo.FilePath)
		delete(c.tempFileManager.tempFiles, fileID)
	}
}

// GetStats 获取跨云传输统计信息
func (c *CrossCloudCoordinator) GetStats() *CrossCloudStats {
	c.transferStats.mu.RLock()
	defer c.transferStats.mu.RUnlock()

	// 创建副本，避免锁拷贝
	stats := &CrossCloudStats{
		TotalTransfers:        c.transferStats.TotalTransfers,
		CompletedTransfers:    c.transferStats.CompletedTransfers,
		FailedTransfers:       c.transferStats.FailedTransfers,
		TotalBytesTransferred: c.transferStats.TotalBytesTransferred,
		AverageSpeed:          c.transferStats.AverageSpeed,
		DuplicateDownloads:    c.transferStats.DuplicateDownloads,
		ResumedTransfers:      c.transferStats.ResumedTransfers,
		ErrorsByType:          make(map[string]int64),
	}

	for k, v := range c.transferStats.ErrorsByType {
		stats.ErrorsByType[k] = v
	}

	return stats
}

// CleanupExpiredTempFiles 清理过期的临时文件
func (c *CrossCloudCoordinator) CleanupExpiredTempFiles(maxAge time.Duration) int {
	c.tempFileManager.mu.Lock()
	defer c.tempFileManager.mu.Unlock()

	cleaned := 0
	now := time.Now()

	for fileID, tempInfo := range c.tempFileManager.tempFiles {
		if tempInfo.RefCount == 0 && now.Sub(tempInfo.LastAccessed) > maxAge {
			os.Remove(tempInfo.FilePath)
			delete(c.tempFileManager.tempFiles, fileID)
			cleaned++
		}
	}

	if cleaned > 0 {
		fs.Infof(nil, "🧹 清理过期临时文件: %d 个", cleaned)
	}

	return cleaned
}

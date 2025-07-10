// Package resume provides unified resume functionality for cloud storage backends
package resume

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
)

// ResumeInfo 统一的断点续传信息结构
type ResumeInfo struct {
	// 基本信息
	TaskID   string `json:"task_id"`   // 任务唯一标识
	FileName string `json:"file_name"` // 文件名
	FileSize int64  `json:"file_size"` // 文件大小
	FilePath string `json:"file_path"` // 文件路径
	FileHash string `json:"file_hash"` // 文件哈希（MD5等）

	// 分片信息
	ChunkSize       int64            `json:"chunk_size"`       // 分片大小
	TotalChunks     int64            `json:"total_chunks"`     // 总分片数
	CompletedChunks map[int64]bool   `json:"completed_chunks"` // 已完成的分片
	ChunkHashes     map[int64]string `json:"chunk_hashes"`     // 分片哈希值（可选）

	// 时间信息
	CreatedAt   time.Time `json:"created_at"`   // 创建时间
	LastUpdated time.Time `json:"last_updated"` // 最后更新时间

	// 扩展信息（不同网盘可能需要的特殊字段）
	Extra map[string]interface{} `json:"extra,omitempty"`
}

// ResumeManager 统一的断点续传管理器接口
type ResumeManager interface {
	// SaveResumeInfo 保存断点续传信息
	SaveResumeInfo(info *ResumeInfo) error

	// LoadResumeInfo 加载断点续传信息
	LoadResumeInfo(taskID string) (*ResumeInfo, error)

	// DeleteResumeInfo 删除断点续传信息
	DeleteResumeInfo(taskID string) error

	// CleanExpiredResumes 清理过期的断点续传信息
	CleanExpiredResumes(maxAge time.Duration) error

	// ListResumes 列出所有断点续传信息
	ListResumes() ([]*ResumeInfo, error)

	// Close 关闭管理器
	Close() error
}

// FileSystemResumeManager 基于文件系统的断点续传管理器
type FileSystemResumeManager struct {
	mu       sync.RWMutex
	cacheDir string
}

// NewFileSystemResumeManager 创建基于文件系统的断点续传管理器
func NewFileSystemResumeManager(cacheDir string) (*FileSystemResumeManager, error) {
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "rclone-resume")
	}

	err := os.MkdirAll(cacheDir, 0755)
	if err != nil {
		return nil, fmt.Errorf("创建断点续传缓存目录失败: %w", err)
	}

	return &FileSystemResumeManager{
		cacheDir: cacheDir,
	}, nil
}

// SaveResumeInfo 保存断点续传信息到文件
func (fsrm *FileSystemResumeManager) SaveResumeInfo(info *ResumeInfo) error {
	fsrm.mu.Lock()
	defer fsrm.mu.Unlock()

	info.LastUpdated = time.Now()

	// 使用taskID的MD5作为文件名，避免特殊字符问题
	hasher := md5.New()
	hasher.Write([]byte(info.TaskID))
	fileName := fmt.Sprintf("%x.json", hasher.Sum(nil))
	filePath := filepath.Join(fsrm.cacheDir, fileName)

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化断点续传信息失败: %w", err)
	}

	err = os.WriteFile(filePath, data, 0644)
	if err != nil {
		return fmt.Errorf("保存断点续传信息失败: %w", err)
	}

	fs.Debugf(nil, "保存断点续传信息: %s, 已完成分片: %d/%d",
		info.FileName, len(info.CompletedChunks), info.TotalChunks)

	return nil
}

// LoadResumeInfo 从文件加载断点续传信息
func (fsrm *FileSystemResumeManager) LoadResumeInfo(taskID string) (*ResumeInfo, error) {
	fsrm.mu.RLock()
	defer fsrm.mu.RUnlock()

	// 使用taskID的MD5作为文件名
	hasher := md5.New()
	hasher.Write([]byte(taskID))
	fileName := fmt.Sprintf("%x.json", hasher.Sum(nil))
	filePath := filepath.Join(fsrm.cacheDir, fileName)

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 文件不存在，返回nil而不是错误
		}
		return nil, fmt.Errorf("读取断点续传信息失败: %w", err)
	}

	var info ResumeInfo
	err = json.Unmarshal(data, &info)
	if err != nil {
		return nil, fmt.Errorf("解析断点续传信息失败: %w", err)
	}

	fs.Debugf(nil, "加载断点续传信息: %s, 已完成分片: %d/%d",
		info.FileName, len(info.CompletedChunks), info.TotalChunks)

	return &info, nil
}

// DeleteResumeInfo 删除断点续传信息文件
func (fsrm *FileSystemResumeManager) DeleteResumeInfo(taskID string) error {
	fsrm.mu.Lock()
	defer fsrm.mu.Unlock()

	// 使用taskID的MD5作为文件名
	hasher := md5.New()
	hasher.Write([]byte(taskID))
	fileName := fmt.Sprintf("%x.json", hasher.Sum(nil))
	filePath := filepath.Join(fsrm.cacheDir, fileName)

	err := os.Remove(filePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除断点续传信息失败: %w", err)
	}

	fs.Debugf(nil, "删除断点续传信息: %s", taskID)
	return nil
}

// CleanExpiredResumes 清理过期的断点续传信息
func (fsrm *FileSystemResumeManager) CleanExpiredResumes(maxAge time.Duration) error {
	fsrm.mu.Lock()
	defer fsrm.mu.Unlock()

	entries, err := os.ReadDir(fsrm.cacheDir)
	if err != nil {
		return fmt.Errorf("读取断点续传缓存目录失败: %w", err)
	}

	now := time.Now()
	cleanedCount := 0

	for _, entry := range entries {
		if entry.IsDir() || !filepath.Ext(entry.Name()) == ".json" {
			continue
		}

		filePath := filepath.Join(fsrm.cacheDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		// 检查文件修改时间
		if now.Sub(info.ModTime()) > maxAge {
			err = os.Remove(filePath)
			if err == nil {
				cleanedCount++
			}
		}
	}

	if cleanedCount > 0 {
		fs.Debugf(nil, "清理了 %d 个过期的断点续传信息", cleanedCount)
	}

	return nil
}

// ListResumes 列出所有断点续传信息
func (fsrm *FileSystemResumeManager) ListResumes() ([]*ResumeInfo, error) {
	fsrm.mu.RLock()
	defer fsrm.mu.RUnlock()

	entries, err := os.ReadDir(fsrm.cacheDir)
	if err != nil {
		return nil, fmt.Errorf("读取断点续传缓存目录失败: %w", err)
	}

	var resumes []*ResumeInfo

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		filePath := filepath.Join(fsrm.cacheDir, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var info ResumeInfo
		err = json.Unmarshal(data, &info)
		if err != nil {
			continue
		}

		resumes = append(resumes, &info)
	}

	return resumes, nil
}

// Close 关闭管理器（文件系统版本无需特殊处理）
func (fsrm *FileSystemResumeManager) Close() error {
	return nil
}

// GenerateTaskID 生成任务ID
func GenerateTaskID(filePath string, fileSize int64, fileHash string) string {
	hasher := md5.New()
	hasher.Write([]byte(fmt.Sprintf("%s_%d_%s", filePath, fileSize, fileHash)))
	return fmt.Sprintf("%x", hasher.Sum(nil))
}

// IsChunkCompleted 检查分片是否已完成
func (info *ResumeInfo) IsChunkCompleted(chunkIndex int64) bool {
	if info.CompletedChunks == nil {
		return false
	}
	return info.CompletedChunks[chunkIndex]
}

// MarkChunkCompleted 标记分片为已完成
func (info *ResumeInfo) MarkChunkCompleted(chunkIndex int64) {
	if info.CompletedChunks == nil {
		info.CompletedChunks = make(map[int64]bool)
	}
	info.CompletedChunks[chunkIndex] = true
}

// GetCompletedChunkCount 获取已完成的分片数量
func (info *ResumeInfo) GetCompletedChunkCount() int64 {
	return int64(len(info.CompletedChunks))
}

// GetCompletionPercentage 获取完成百分比
func (info *ResumeInfo) GetCompletionPercentage() float64 {
	if info.TotalChunks == 0 {
		return 0
	}
	return float64(len(info.CompletedChunks)) / float64(info.TotalChunks) * 100
}

// CrossCloudTransferInfo 跨云传输断点续传信息
type CrossCloudTransferInfo struct {
	TransferID  string `json:"transfer_id"`  // 传输任务唯一标识
	SourcePath  string `json:"source_path"`  // 源文件路径
	DestPath    string `json:"dest_path"`    // 目标文件路径
	FileSize    int64  `json:"file_size"`    // 文件大小
	ChunkSize   int64  `json:"chunk_size"`   // 分片大小
	TotalChunks int64  `json:"total_chunks"` // 总分片数

	// 下载进度
	DownloadedChunks map[int64]bool `json:"downloaded_chunks"`  // 已下载的分片
	DownloadTempPath string         `json:"download_temp_path"` // 下载临时文件路径

	// 上传进度
	UploadedChunks map[int64]string `json:"uploaded_chunks"` // 已上传的分片 (chunk_index -> etag)
	UploadID       string           `json:"upload_id"`       // 上传任务ID

	// 时间信息
	CreatedAt   time.Time `json:"created_at"`
	LastUpdated time.Time `json:"last_updated"`

	// 状态信息
	Phase string `json:"phase"` // "downloading", "uploading", "completed"
}

// CrossCloudResumeManager 跨云传输断点续传管理器
type CrossCloudResumeManager struct {
	fsManager *FileSystemResumeManager
}

// NewCrossCloudResumeManager 创建跨云传输断点续传管理器
func NewCrossCloudResumeManager(cacheDir string) (*CrossCloudResumeManager, error) {
	// 优先使用BadgerDB，如果失败则回退到文件系统
	fsManager, err := NewFileSystemResumeManager(filepath.Join(cacheDir, "cross-cloud"))
	if err != nil {
		return nil, fmt.Errorf("创建跨云传输断点续传管理器失败: %w", err)
	}

	return &CrossCloudResumeManager{
		fsManager: fsManager,
	}, nil
}

// SaveCrossCloudTransferInfo 保存跨云传输断点续传信息
func (ccrm *CrossCloudResumeManager) SaveCrossCloudTransferInfo(info *CrossCloudTransferInfo) error {
	// 转换为通用的ResumeInfo格式
	resumeInfo := &ResumeInfo{
		TaskID:          info.TransferID,
		FileName:        filepath.Base(info.SourcePath),
		FileSize:        info.FileSize,
		FilePath:        info.SourcePath,
		ChunkSize:       info.ChunkSize,
		TotalChunks:     info.TotalChunks,
		CompletedChunks: info.DownloadedChunks,
		CreatedAt:       info.CreatedAt,
		LastUpdated:     info.LastUpdated,
		Extra: map[string]interface{}{
			"dest_path":       info.DestPath,
			"download_temp":   info.DownloadTempPath,
			"uploaded_chunks": info.UploadedChunks,
			"upload_id":       info.UploadID,
			"phase":           info.Phase,
		},
	}

	return ccrm.fsManager.SaveResumeInfo(resumeInfo)
}

// LoadCrossCloudTransferInfo 加载跨云传输断点续传信息
func (ccrm *CrossCloudResumeManager) LoadCrossCloudTransferInfo(transferID string) (*CrossCloudTransferInfo, error) {
	resumeInfo, err := ccrm.fsManager.LoadResumeInfo(transferID)
	if err != nil || resumeInfo == nil {
		return nil, err
	}

	// 从通用格式转换回跨云传输格式
	info := &CrossCloudTransferInfo{
		TransferID:       resumeInfo.TaskID,
		SourcePath:       resumeInfo.FilePath,
		FileSize:         resumeInfo.FileSize,
		ChunkSize:        resumeInfo.ChunkSize,
		TotalChunks:      resumeInfo.TotalChunks,
		DownloadedChunks: resumeInfo.CompletedChunks,
		CreatedAt:        resumeInfo.CreatedAt,
		LastUpdated:      resumeInfo.LastUpdated,
	}

	// 从Extra字段恢复跨云传输特有的信息
	if resumeInfo.Extra != nil {
		if destPath, ok := resumeInfo.Extra["dest_path"].(string); ok {
			info.DestPath = destPath
		}
		if downloadTemp, ok := resumeInfo.Extra["download_temp"].(string); ok {
			info.DownloadTempPath = downloadTemp
		}
		if uploadID, ok := resumeInfo.Extra["upload_id"].(string); ok {
			info.UploadID = uploadID
		}
		if phase, ok := resumeInfo.Extra["phase"].(string); ok {
			info.Phase = phase
		}
		if uploadedChunks, ok := resumeInfo.Extra["uploaded_chunks"].(map[int64]string); ok {
			info.UploadedChunks = uploadedChunks
		}
	}

	// 初始化空的map
	if info.DownloadedChunks == nil {
		info.DownloadedChunks = make(map[int64]bool)
	}
	if info.UploadedChunks == nil {
		info.UploadedChunks = make(map[int64]string)
	}

	return info, nil
}

// DeleteCrossCloudTransferInfo 删除跨云传输断点续传信息
func (ccrm *CrossCloudResumeManager) DeleteCrossCloudTransferInfo(transferID string) error {
	return ccrm.fsManager.DeleteResumeInfo(transferID)
}

// GenerateCrossCloudTransferID 生成跨云传输任务ID
func GenerateCrossCloudTransferID(sourcePath, destPath string, fileSize int64) string {
	hasher := md5.New()
	hasher.Write([]byte(fmt.Sprintf("cross_%s_%s_%d_%d",
		sourcePath, destPath, fileSize, time.Now().Unix())))
	return fmt.Sprintf("cross_%x", hasher.Sum(nil))
}

// IsDownloadCompleted 检查下载是否完成
func (info *CrossCloudTransferInfo) IsDownloadCompleted() bool {
	return int64(len(info.DownloadedChunks)) >= info.TotalChunks
}

// IsUploadCompleted 检查上传是否完成
func (info *CrossCloudTransferInfo) IsUploadCompleted() bool {
	return int64(len(info.UploadedChunks)) >= info.TotalChunks
}

// GetDownloadProgress 获取下载进度
func (info *CrossCloudTransferInfo) GetDownloadProgress() (completed, total int64, percentage float64) {
	completed = int64(len(info.DownloadedChunks))
	total = info.TotalChunks
	if total > 0 {
		percentage = float64(completed) / float64(total) * 100
	}
	return
}

// GetUploadProgress 获取上传进度
func (info *CrossCloudTransferInfo) GetUploadProgress() (completed, total int64, percentage float64) {
	completed = int64(len(info.UploadedChunks))
	total = info.TotalChunks
	if total > 0 {
		percentage = float64(completed) / float64(total) * 100
	}
	return
}

// BadgerResumeManager BadgerDB断点续传管理器（推荐使用）
type BadgerResumeManager struct {
	mu          sync.RWMutex
	resumeCache interface{} // 使用interface{}避免循环依赖，实际类型为*cache.BadgerCache
	cacheDir    string
}

// NewBadgerResumeManager 创建BadgerDB断点续传管理器
// 注意：需要在具体的backend中实现，这里提供接口定义
func NewBadgerResumeManager(name, cacheDir string) (ResumeManager, error) {
	// 这个函数需要在具体的backend中实现
	// 因为需要导入cache包，而这里为了避免循环依赖不能直接导入
	return nil, fmt.Errorf("BadgerDB断点续传管理器需要在具体的backend中实现")
}

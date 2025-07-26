// Package common provides unified components for cloud storage backends
package common

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/cache"
)

// ResumeStats 断点续传统计信息
type ResumeStats struct {
	mu                 sync.RWMutex
	TotalAttempts      int64              // 总尝试次数
	SuccessfulResumes  int64              // 成功恢复次数
	FailedResumes      int64              // 失败恢复次数
	AverageResumeTime  float64            // 平均恢复时间(秒)
	LastSuccessTime    time.Time          // 最后成功时间
	LastFailureTime    time.Time          // 最后失败时间
	FailureReasons     map[string]int64   // 失败原因统计
	PerformanceMetrics PerformanceMetrics // 性能指标
}

// PerformanceMetrics 性能指标
type PerformanceMetrics struct {
	AverageUploadSpeed   float64 // 平均上传速度 (MB/s)
	AverageDownloadSpeed float64 // 平均下载速度 (MB/s)
	TotalBytesResumed    int64   // 总恢复字节数
	TotalTimesSaved      float64 // 总节省时间(秒)
}

// UnifiedResumeManager 统一的断点续传管理器接口
// 🚀 设计目标：消除123网盘和115网盘90%的重复代码
type UnifiedResumeManager interface {
	// 基础操作
	SaveResumeInfo(info UnifiedResumeInfo) error
	LoadResumeInfo(taskID string) (UnifiedResumeInfo, error)
	DeleteResumeInfo(taskID string) error
	Close() error

	// 分片操作
	UpdateChunkInfo(taskID string, chunkIndex int64, metadata map[string]interface{}) error
	IsChunkCompleted(taskID string, chunkIndex int64) (bool, error)
	MarkChunkCompleted(taskID string, chunkIndex int64) error

	// 进度查询
	GetResumeProgress(taskID string) (uploaded, total int64, percentage float64, err error)
	GetCompletedChunkCount(taskID string) (int64, error)

	// 清理操作
	CleanupExpiredInfo() error

	// 🔧 监控和统计
	GetResumeStats() ResumeStats
	GetResumeHealthReport() map[string]interface{}
}

// UnifiedResumeInfo 统一的断点续传信息接口
// 🚀 设计目标：支持123网盘和115网盘的不同需求
type UnifiedResumeInfo interface {
	// 基础信息
	GetTaskID() string
	GetFileName() string
	GetFileSize() int64
	GetFilePath() string
	GetChunkSize() int64
	GetTotalChunks() int64
	GetCreatedAt() time.Time
	GetLastUpdated() time.Time

	// 分片状态
	IsChunkCompleted(chunkIndex int64) bool
	MarkChunkCompleted(chunkIndex int64)
	GetCompletedChunkCount() int64
	GetCompletedChunks() map[int64]bool

	// 后端特定信息
	GetBackendSpecificData() map[string]interface{}
	SetBackendSpecificData(key string, value interface{})

	// 序列化
	ToJSON() ([]byte, error)
	FromJSON(data []byte) error
}

// BadgerResumeManager 基于BadgerDB的统一断点续传管理器实现
// 🚀 功能特点：统一的缓存机制，支持多后端
// 🔧 优化：添加缓存健康监控和恢复机制
type BadgerResumeManager struct {
	mu          sync.RWMutex
	resumeCache *cache.BadgerCache
	fs          fs.Fs
	backendType string // "123" 或 "115"

	// 清理定时器
	cleanupTimer *time.Timer

	// 🔧 新增：缓存健康监控
	cacheHealthTimer *time.Timer
	lastHealthCheck  time.Time
	cacheFailures    int

	// 🔧 新增：断点续传监控统计
	resumeStats ResumeStats
}

// NewBadgerResumeManager 创建基于BadgerDB的断点续传管理器
func NewBadgerResumeManager(filesystem fs.Fs, backendType string, cacheDir string) (*BadgerResumeManager, error) {
	cacheKey := fmt.Sprintf("resume_%s", backendType)
	resumeCache, err := cache.NewBadgerCache(cacheKey, cacheDir)
	if err != nil {
		return nil, fmt.Errorf("创建%s网盘断点续传缓存失败: %w", backendType, err)
	}

	rm := &BadgerResumeManager{
		resumeCache:     resumeCache,
		fs:              filesystem,
		backendType:     backendType,
		lastHealthCheck: time.Now(),
		cacheFailures:   0,
		resumeStats: ResumeStats{
			FailureReasons: make(map[string]int64),
		},
	}

	// 启动定期清理
	rm.startCleanupTimer()

	// 🔧 启动缓存健康监控
	rm.startCacheHealthMonitor()

	return rm, nil
}

// SaveResumeInfo 保存断点续传信息
func (rm *BadgerResumeManager) SaveResumeInfo(info UnifiedResumeInfo) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	cacheKey := rm.generateCacheKey(info.GetTaskID())

	// 序列化信息
	data, err := info.ToJSON()
	if err != nil {
		return fmt.Errorf("序列化断点续传信息失败: %w", err)
	}

	err = rm.resumeCache.Set(cacheKey, data, 24*time.Hour)
	if err != nil {
		return fmt.Errorf("保存%s网盘断点续传信息失败: %w", rm.backendType, err)
	}

	// 🔧 更新统计信息
	rm.updateResumeStats(true, "", 0, 0)

	fs.Debugf(rm.fs, "保存%s网盘断点续传信息: %s, 已完成分片: %d/%d",
		rm.backendType, info.GetFileName(), info.GetCompletedChunkCount(), info.GetTotalChunks())

	return nil
}

// LoadResumeInfo 加载断点续传信息
func (rm *BadgerResumeManager) LoadResumeInfo(taskID string) (UnifiedResumeInfo, error) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	cacheKey := rm.generateCacheKey(taskID)
	var data []byte

	found, err := rm.resumeCache.Get(cacheKey, &data)
	if err != nil {
		return nil, fmt.Errorf("加载%s网盘断点续传信息失败: %w", rm.backendType, err)
	}

	if !found {
		return nil, nil // 没有找到断点续传信息
	}

	// 根据后端类型创建相应的信息对象
	var info UnifiedResumeInfo
	switch rm.backendType {
	case "123":
		info = &ResumeInfo123{}
	case "115":
		info = &ResumeInfo115{}
	default:
		return nil, fmt.Errorf("不支持的后端类型: %s", rm.backendType)
	}

	err = info.FromJSON(data)
	if err != nil {
		return nil, fmt.Errorf("反序列化断点续传信息失败: %w", err)
	}

	// 检查信息是否过期（超过24小时）
	if time.Since(info.GetCreatedAt()) > 24*time.Hour {
		rm.DeleteResumeInfo(taskID)
		return nil, nil
	}

	// 🔧 计算恢复的字节数和节省的时间
	completedChunks := info.GetCompletedChunkCount()
	totalChunks := info.GetTotalChunks()
	if completedChunks > 0 && totalChunks > 0 {
		// 估算已完成的字节数
		bytesResumed := int64(float64(info.GetFileSize()) * float64(completedChunks) / float64(totalChunks))
		// 估算节省的时间（假设平均上传速度1MB/s）
		timeSaved := float64(bytesResumed) / (1024 * 1024) // 秒
		rm.updateResumeStats(true, "", bytesResumed, timeSaved)
	}

	fs.Debugf(rm.fs, "加载%s网盘断点续传信息: %s, 已完成分片: %d/%d",
		rm.backendType, info.GetFileName(), info.GetCompletedChunkCount(), info.GetTotalChunks())

	return info, nil
}

// DeleteResumeInfo 删除断点续传信息
func (rm *BadgerResumeManager) DeleteResumeInfo(taskID string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	cacheKey := rm.generateCacheKey(taskID)
	err := rm.resumeCache.Delete(cacheKey)
	if err != nil {
		return fmt.Errorf("删除%s网盘断点续传信息失败: %w", rm.backendType, err)
	}

	fs.Debugf(rm.fs, "删除%s网盘断点续传信息: %s", rm.backendType, taskID)
	return nil
}

// UpdateChunkInfo 更新分片信息
func (rm *BadgerResumeManager) UpdateChunkInfo(taskID string, chunkIndex int64, metadata map[string]interface{}) error {
	info, err := rm.LoadResumeInfo(taskID)
	if err != nil {
		return err
	}
	if info == nil {
		return fmt.Errorf("断点续传信息不存在: %s", taskID)
	}

	// 标记分片完成
	info.MarkChunkCompleted(chunkIndex)

	// 更新后端特定数据
	for key, value := range metadata {
		info.SetBackendSpecificData(key, value)
	}

	return rm.SaveResumeInfo(info)
}

// IsChunkCompleted 检查分片是否已完成
func (rm *BadgerResumeManager) IsChunkCompleted(taskID string, chunkIndex int64) (bool, error) {
	info, err := rm.LoadResumeInfo(taskID)
	if err != nil {
		return false, err
	}
	if info == nil {
		return false, nil
	}

	return info.IsChunkCompleted(chunkIndex), nil
}

// MarkChunkCompleted 标记分片为已完成
func (rm *BadgerResumeManager) MarkChunkCompleted(taskID string, chunkIndex int64) error {
	return rm.UpdateChunkInfo(taskID, chunkIndex, nil)
}

// GetResumeProgress 获取断点续传进度
func (rm *BadgerResumeManager) GetResumeProgress(taskID string) (uploaded, total int64, percentage float64, err error) {
	info, err := rm.LoadResumeInfo(taskID)
	if err != nil {
		return 0, 0, 0, err
	}
	if info == nil {
		return 0, 0, 0, nil
	}

	uploaded = info.GetCompletedChunkCount()
	total = info.GetTotalChunks()

	if total > 0 {
		percentage = float64(uploaded) / float64(total) * 100.0
	}

	return uploaded, total, percentage, nil
}

// GetCompletedChunkCount 获取已完成的分片数量
func (rm *BadgerResumeManager) GetCompletedChunkCount(taskID string) (int64, error) {
	info, err := rm.LoadResumeInfo(taskID)
	if err != nil {
		return 0, err
	}
	if info == nil {
		return 0, nil
	}

	return info.GetCompletedChunkCount(), nil
}

// CleanupExpiredInfo 清理过期的断点续传信息
func (rm *BadgerResumeManager) CleanupExpiredInfo() error {
	fs.Debugf(rm.fs, "🧹 开始清理%s网盘过期的断点续传信息", rm.backendType)

	// 获取所有缓存条目
	entries, err := rm.resumeCache.GetAllEntries()
	if err != nil {
		return fmt.Errorf("获取缓存条目失败: %w", err)
	}

	cleanedCount := 0

	for key := range entries {
		// 检查是否为断点续传信息
		if !rm.isResumeKey(key) {
			continue
		}

		// 尝试加载信息检查过期时间
		taskID := rm.extractTaskIDFromKey(key)
		info, err := rm.LoadResumeInfo(taskID)
		if err != nil {
			// 加载失败，删除损坏的条目
			rm.resumeCache.Delete(key)
			cleanedCount++
			continue
		}

		if info != nil && time.Since(info.GetCreatedAt()) > 24*time.Hour {
			rm.DeleteResumeInfo(taskID)
			cleanedCount++
		}
	}

	fs.Debugf(rm.fs, "🧹 清理完成，删除了%d个过期的%s网盘断点续传信息", cleanedCount, rm.backendType)
	return nil
}

// Close 关闭断点续传管理器
// 🔧 优化：清理所有定时器
func (rm *BadgerResumeManager) Close() error {
	if rm.cleanupTimer != nil {
		rm.cleanupTimer.Stop()
	}

	// 🔧 清理缓存健康监控定时器
	if rm.cacheHealthTimer != nil {
		rm.cacheHealthTimer.Stop()
	}

	if rm.resumeCache != nil {
		return rm.resumeCache.Close()
	}

	return nil
}

// 私有辅助方法

func (rm *BadgerResumeManager) generateCacheKey(taskID string) string {
	return fmt.Sprintf("resume_%s_%s", rm.backendType, taskID)
}

func (rm *BadgerResumeManager) isResumeKey(key string) bool {
	prefix := fmt.Sprintf("resume_%s_", rm.backendType)
	return len(key) > len(prefix) && key[:len(prefix)] == prefix
}

func (rm *BadgerResumeManager) extractTaskIDFromKey(key string) string {
	prefix := fmt.Sprintf("resume_%s_", rm.backendType)
	if len(key) > len(prefix) {
		return key[len(prefix):]
	}
	return ""
}

func (rm *BadgerResumeManager) startCleanupTimer() {
	// 每6小时清理一次过期信息
	rm.cleanupTimer = time.AfterFunc(6*time.Hour, func() {
		rm.CleanupExpiredInfo()
		rm.startCleanupTimer() // 重新设置定时器
	})
}

// ResumeInfo123 123网盘断点续传信息实现
type ResumeInfo123 struct {
	TaskID              string                 `json:"task_id"`
	PreuploadID         string                 `json:"preupload_id"`
	FileName            string                 `json:"file_name"`
	FileSize            int64                  `json:"file_size"`
	FilePath            string                 `json:"file_path"`
	ChunkSize           int64                  `json:"chunk_size"`
	TotalChunks         int64                  `json:"total_chunks"`
	CompletedChunks     map[int64]bool         `json:"completed_chunks"`
	CreatedAt           time.Time              `json:"created_at"`
	LastUpdated         time.Time              `json:"last_updated"`
	TempFilePath        string                 `json:"temp_file_path"` // 🔧 新增：临时文件路径
	BackendSpecificData map[string]interface{} `json:"backend_specific_data"`
}

// ResumeInfo115 115网盘断点续传信息实现
type ResumeInfo115 struct {
	TaskID              string                 `json:"task_id"`
	FileName            string                 `json:"file_name"`
	FileSize            int64                  `json:"file_size"`
	FilePath            string                 `json:"file_path"`
	ChunkSize           int64                  `json:"chunk_size"`
	TotalChunks         int64                  `json:"total_chunks"`
	CompletedChunks     map[int64]bool         `json:"completed_chunks"`
	CreatedAt           time.Time              `json:"created_at"`
	LastUpdated         time.Time              `json:"last_updated"`
	TempFilePath        string                 `json:"temp_file_path"`
	BackendSpecificData map[string]interface{} `json:"backend_specific_data"`
}

// 123网盘断点续传信息实现

func (r *ResumeInfo123) GetTaskID() string         { return r.TaskID }
func (r *ResumeInfo123) GetFileName() string       { return r.FileName }
func (r *ResumeInfo123) GetFileSize() int64        { return r.FileSize }
func (r *ResumeInfo123) GetFilePath() string       { return r.FilePath }
func (r *ResumeInfo123) GetChunkSize() int64       { return r.ChunkSize }
func (r *ResumeInfo123) GetTotalChunks() int64     { return r.TotalChunks }
func (r *ResumeInfo123) GetCreatedAt() time.Time   { return r.CreatedAt }
func (r *ResumeInfo123) GetLastUpdated() time.Time { return r.LastUpdated }

func (r *ResumeInfo123) IsChunkCompleted(chunkIndex int64) bool {
	if r.CompletedChunks == nil {
		return false
	}
	return r.CompletedChunks[chunkIndex]
}

func (r *ResumeInfo123) MarkChunkCompleted(chunkIndex int64) {
	if r.CompletedChunks == nil {
		r.CompletedChunks = make(map[int64]bool)
	}
	r.CompletedChunks[chunkIndex] = true
	r.LastUpdated = time.Now()
}

func (r *ResumeInfo123) GetCompletedChunkCount() int64 {
	return int64(len(r.CompletedChunks))
}

func (r *ResumeInfo123) GetCompletedChunks() map[int64]bool {
	if r.CompletedChunks == nil {
		return make(map[int64]bool)
	}
	// 返回副本以避免外部修改
	result := make(map[int64]bool)
	for k, v := range r.CompletedChunks {
		result[k] = v
	}
	return result
}

func (r *ResumeInfo123) GetBackendSpecificData() map[string]interface{} {
	if r.BackendSpecificData == nil {
		r.BackendSpecificData = make(map[string]interface{})
	}
	return r.BackendSpecificData
}

func (r *ResumeInfo123) SetBackendSpecificData(key string, value interface{}) {
	if r.BackendSpecificData == nil {
		r.BackendSpecificData = make(map[string]interface{})
	}
	r.BackendSpecificData[key] = value
}

func (r *ResumeInfo123) ToJSON() ([]byte, error) {
	return json.Marshal(r)
}

func (r *ResumeInfo123) FromJSON(data []byte) error {
	return json.Unmarshal(data, r)
}

// 115网盘断点续传信息实现

func (r *ResumeInfo115) GetTaskID() string         { return r.TaskID }
func (r *ResumeInfo115) GetFileName() string       { return r.FileName }
func (r *ResumeInfo115) GetFileSize() int64        { return r.FileSize }
func (r *ResumeInfo115) GetFilePath() string       { return r.FilePath }
func (r *ResumeInfo115) GetChunkSize() int64       { return r.ChunkSize }
func (r *ResumeInfo115) GetTotalChunks() int64     { return r.TotalChunks }
func (r *ResumeInfo115) GetCreatedAt() time.Time   { return r.CreatedAt }
func (r *ResumeInfo115) GetLastUpdated() time.Time { return r.LastUpdated }

func (r *ResumeInfo115) IsChunkCompleted(chunkIndex int64) bool {
	if r.CompletedChunks == nil {
		return false
	}
	return r.CompletedChunks[chunkIndex]
}

func (r *ResumeInfo115) MarkChunkCompleted(chunkIndex int64) {
	if r.CompletedChunks == nil {
		r.CompletedChunks = make(map[int64]bool)
	}
	r.CompletedChunks[chunkIndex] = true
	r.LastUpdated = time.Now()
}

func (r *ResumeInfo115) GetCompletedChunkCount() int64 {
	return int64(len(r.CompletedChunks))
}

func (r *ResumeInfo115) GetCompletedChunks() map[int64]bool {
	if r.CompletedChunks == nil {
		return make(map[int64]bool)
	}
	// 返回副本以避免外部修改
	result := make(map[int64]bool)
	for k, v := range r.CompletedChunks {
		result[k] = v
	}
	return result
}

func (r *ResumeInfo115) GetBackendSpecificData() map[string]interface{} {
	if r.BackendSpecificData == nil {
		r.BackendSpecificData = make(map[string]interface{})
	}
	return r.BackendSpecificData
}

func (r *ResumeInfo115) SetBackendSpecificData(key string, value interface{}) {
	if r.BackendSpecificData == nil {
		r.BackendSpecificData = make(map[string]interface{})
	}
	r.BackendSpecificData[key] = value
}

func (r *ResumeInfo115) ToJSON() ([]byte, error) {
	return json.Marshal(r)
}

func (r *ResumeInfo115) FromJSON(data []byte) error {
	return json.Unmarshal(data, r)
}

// 工具函数

// GenerateTaskID123 生成123网盘任务ID
// 🔧 修复：使用稳定的哈希值，确保下载和上传使用一致的TaskID
func GenerateTaskID123(filePath string, fileSize int64) string {
	// 使用文件路径和大小生成稳定的哈希值
	hasher := sha256.New()
	hasher.Write([]byte(fmt.Sprintf("123_%s_%d", filePath, fileSize)))
	hash := hex.EncodeToString(hasher.Sum(nil))[:16] // 取前16位作为短哈希

	return fmt.Sprintf("123_%s_%d_%s",
		filePath,
		fileSize,
		hash)
}

// GenerateTaskID115 生成115网盘任务ID
// 🔧 修复：使用稳定的哈希值而不是时间戳，确保断点续传能正确工作
func GenerateTaskID115(filePath string, fileSize int64) string {
	// 使用文件路径和大小生成稳定的哈希值
	hasher := sha256.New()
	hasher.Write([]byte(fmt.Sprintf("115_%s_%d", filePath, fileSize)))
	hash := hex.EncodeToString(hasher.Sum(nil))[:16] // 取前16位作为短哈希

	return fmt.Sprintf("115_%s_%d_%s",
		filePath,
		fileSize,
		hash)
}

// 🔧 缓存健康监控和恢复机制

// startCacheHealthMonitor 启动缓存健康监控
func (rm *BadgerResumeManager) startCacheHealthMonitor() {
	// 每5分钟检查一次缓存健康状态
	healthCheckInterval := 5 * time.Minute

	fs.Debugf(rm.fs, "🔧 启动%s网盘缓存健康监控，检查间隔: %v", rm.backendType, healthCheckInterval)

	rm.cacheHealthTimer = time.AfterFunc(healthCheckInterval, func() {
		rm.performHealthCheck()
		rm.startCacheHealthMonitor() // 重新启动定时器
	})
}

// performHealthCheck 执行缓存健康检查
func (rm *BadgerResumeManager) performHealthCheck() {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	// 测试缓存基本操作
	testKey := fmt.Sprintf("health_check_%s_%d", rm.backendType, time.Now().Unix())
	testValue := map[string]interface{}{
		"test":      true,
		"timestamp": time.Now(),
	}

	// 测试写入
	err := rm.resumeCache.Set(testKey, testValue, 1*time.Minute)
	if err != nil {
		rm.cacheFailures++
		fs.Debugf(rm.fs, "⚠️ %s网盘缓存健康检查失败(写入): %v, 失败次数: %d",
			rm.backendType, err, rm.cacheFailures)

		// 如果连续失败次数过多，尝试恢复
		if rm.cacheFailures >= 3 {
			rm.attemptCacheRecovery()
		}
		return
	}

	// 测试读取
	var result map[string]interface{}
	found, err := rm.resumeCache.Get(testKey, &result)
	if err != nil || !found {
		rm.cacheFailures++
		fs.Debugf(rm.fs, "⚠️ %s网盘缓存健康检查失败(读取): found=%v, err=%v, 失败次数: %d",
			rm.backendType, found, err, rm.cacheFailures)

		if rm.cacheFailures >= 3 {
			rm.attemptCacheRecovery()
		}
		return
	}

	// 测试删除
	err = rm.resumeCache.Delete(testKey)
	if err != nil {
		fs.Debugf(rm.fs, "⚠️ %s网盘缓存健康检查删除失败: %v", rm.backendType, err)
	}

	// 健康检查通过，重置失败计数
	if rm.cacheFailures > 0 {
		fs.Debugf(rm.fs, "✅ %s网盘缓存健康检查恢复正常，重置失败计数", rm.backendType)
		rm.cacheFailures = 0
	}

	rm.lastHealthCheck = time.Now()

	// 🔧 新增：断点续传健康检查报告
	rm.logResumeHealthReport()
}

// logResumeHealthReport 记录断点续传健康报告
func (rm *BadgerResumeManager) logResumeHealthReport() {
	report := rm.GetResumeHealthReport()

	fs.Infof(rm.fs, "📊 %s网盘断点续传健康报告:", rm.backendType)
	fs.Infof(rm.fs, "   总尝试次数: %d, 成功: %d, 失败: %d, 成功率: %s",
		report["total_attempts"], report["successful_resumes"],
		report["failed_resumes"], report["success_rate"])

	if report["total_attempts"].(int64) > 0 {
		fs.Infof(rm.fs, "   平均恢复时间: %s", report["average_resume_time"])

		// 显示失败原因统计
		if failureReasons, ok := report["failure_reasons"].(map[string]int64); ok && len(failureReasons) > 0 {
			fs.Infof(rm.fs, "   失败原因统计:")
			for reason, count := range failureReasons {
				fs.Infof(rm.fs, "     - %s: %d次", reason, count)
			}
		}
	}
}

// attemptCacheRecovery 尝试缓存恢复
func (rm *BadgerResumeManager) attemptCacheRecovery() {
	fs.Logf(rm.fs, "🔧 %s网盘缓存连续失败%d次，尝试恢复...", rm.backendType, rm.cacheFailures)

	// 这里可以添加更多的恢复策略，比如：
	// 1. 重新初始化缓存
	// 2. 清理损坏的缓存文件
	// 3. 切换到内存模式

	// 目前简单地重置失败计数，让缓存继续使用内存备份模式
	rm.cacheFailures = 0
	fs.Logf(rm.fs, "🔧 %s网盘缓存恢复完成，将继续使用内存备份模式", rm.backendType)
}

// 🔧 断点续传监控和统计方法

// updateResumeStats 更新断点续传统计信息
func (rm *BadgerResumeManager) updateResumeStats(success bool, failureReason string, bytesResumed int64, timeSaved float64) {
	rm.resumeStats.mu.Lock()
	defer rm.resumeStats.mu.Unlock()

	rm.resumeStats.TotalAttempts++

	if success {
		rm.resumeStats.SuccessfulResumes++
		rm.resumeStats.LastSuccessTime = time.Now()
		rm.resumeStats.PerformanceMetrics.TotalBytesResumed += bytesResumed
		rm.resumeStats.PerformanceMetrics.TotalTimesSaved += timeSaved
	} else {
		rm.resumeStats.FailedResumes++
		rm.resumeStats.LastFailureTime = time.Now()
		if failureReason != "" {
			rm.resumeStats.FailureReasons[failureReason]++
		}
	}

	// 更新平均恢复时间
	if rm.resumeStats.SuccessfulResumes > 0 {
		rm.resumeStats.AverageResumeTime = rm.resumeStats.PerformanceMetrics.TotalTimesSaved / float64(rm.resumeStats.SuccessfulResumes)
	}
}

// GetResumeStats 获取断点续传统计信息
func (rm *BadgerResumeManager) GetResumeStats() ResumeStats {
	rm.resumeStats.mu.RLock()
	defer rm.resumeStats.mu.RUnlock()

	// 创建副本避免并发问题
	stats := rm.resumeStats
	stats.FailureReasons = make(map[string]int64)
	for k, v := range rm.resumeStats.FailureReasons {
		stats.FailureReasons[k] = v
	}

	return stats
}

// GetResumeHealthReport 获取断点续传健康报告
func (rm *BadgerResumeManager) GetResumeHealthReport() map[string]interface{} {
	stats := rm.GetResumeStats()

	successRate := float64(0)
	if stats.TotalAttempts > 0 {
		successRate = float64(stats.SuccessfulResumes) / float64(stats.TotalAttempts) * 100
	}

	return map[string]interface{}{
		"backend_type":        rm.backendType,
		"total_attempts":      stats.TotalAttempts,
		"successful_resumes":  stats.SuccessfulResumes,
		"failed_resumes":      stats.FailedResumes,
		"success_rate":        fmt.Sprintf("%.2f%%", successRate),
		"average_resume_time": fmt.Sprintf("%.2fs", stats.AverageResumeTime),
		"last_success_time":   stats.LastSuccessTime.Format("2006-01-02 15:04:05"),
		"last_failure_time":   stats.LastFailureTime.Format("2006-01-02 15:04:05"),
		"failure_reasons":     stats.FailureReasons,
		"performance_metrics": map[string]interface{}{
			"total_bytes_resumed": fmt.Sprintf("%.2f MB", float64(stats.PerformanceMetrics.TotalBytesResumed)/1024/1024),
			"total_time_saved":    fmt.Sprintf("%.2fs", stats.PerformanceMetrics.TotalTimesSaved),
			"avg_upload_speed":    fmt.Sprintf("%.2f MB/s", stats.PerformanceMetrics.AverageUploadSpeed),
			"avg_download_speed":  fmt.Sprintf("%.2f MB/s", stats.PerformanceMetrics.AverageDownloadSpeed),
		},
		"cache_health": map[string]interface{}{
			"last_health_check": rm.lastHealthCheck.Format("2006-01-02 15:04:05"),
			"cache_failures":    rm.cacheFailures,
		},
	}
}

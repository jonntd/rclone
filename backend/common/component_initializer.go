package common

import (
	"fmt"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/cache"
)

// ComponentInitializationConfig 组件初始化配置
type ComponentInitializationConfig struct {
	BackendType           string             // 后端类型："123" 或 "115"
	LogContext            fs.Fs              // 日志上下文
	CacheDir              string             // 缓存目录
	MemoryBufferLimit     int64              // 内存缓冲限制（字节）
	ContinueOnError       bool               // 组件初始化失败时是否继续执行
	CreateDownloadAdapter func() interface{} // 下载适配器创建函数
}

// UnifiedComponentSet 统一组件集合
type UnifiedComponentSet struct {
	ResumeManager         UnifiedResumeManager         // 断点续传管理器
	ErrorHandler          *UnifiedErrorHandler         // 错误处理器
	MemoryOptimizer       *MemoryOptimizer             // 内存优化器
	ConcurrentDownloader  *UnifiedConcurrentDownloader // 并发下载器
	CrossCloudCoordinator *CrossCloudCoordinator       // 跨云传输协调器
}

// InitializeUnifiedComponents 统一的组件初始化函数
// 替代原来的重复初始化逻辑，支持123网盘和115网盘
func InitializeUnifiedComponents(config *ComponentInitializationConfig) (*UnifiedComponentSet, error) {
	if config == nil {
		return nil, fmt.Errorf("组件初始化配置不能为空")
	}

	if config.BackendType == "" {
		return nil, fmt.Errorf("后端类型不能为空")
	}

	if config.LogContext == nil {
		return nil, fmt.Errorf("日志上下文不能为空")
	}

	// 设置默认值
	if config.CacheDir == "" {
		config.CacheDir = cache.GetCacheDir(config.BackendType + "drive")
	}

	if config.MemoryBufferLimit <= 0 {
		config.MemoryBufferLimit = 50 * 1024 * 1024 // 默认50MB
	}

	components := &UnifiedComponentSet{}

	// 1. 初始化断点续传管理器（最高优先级，其他组件依赖它）
	if resumeManager, err := NewBadgerResumeManager(config.LogContext, config.BackendType, config.CacheDir); err != nil {
		if config.LogContext != nil {
			fs.Errorf(config.LogContext, "初始化%s网盘断点续传管理器失败: %v", config.BackendType, err)
		}
		if !config.ContinueOnError {
			return nil, fmt.Errorf("初始化断点续传管理器失败: %w", err)
		}
	} else {
		components.ResumeManager = resumeManager
		if config.LogContext != nil {
			fs.Debugf(config.LogContext, "%s网盘统一断点续传管理器初始化成功", config.BackendType)
		}
	}

	// 2. 初始化错误处理器（第二优先级，跨云传输协调器依赖它）
	components.ErrorHandler = NewUnifiedErrorHandler(config.BackendType)
	if config.LogContext != nil {
		fs.Debugf(config.LogContext, "%s网盘统一错误处理器初始化成功", config.BackendType)
	}

	// 3. 初始化跨云传输协调器（依赖断点续传管理器和错误处理器）
	if components.ResumeManager != nil && components.ErrorHandler != nil {
		components.CrossCloudCoordinator = NewCrossCloudCoordinator(components.ResumeManager, components.ErrorHandler)
		if config.LogContext != nil {
			fs.Debugf(config.LogContext, "%s网盘跨云传输协调器初始化成功", config.BackendType)
		}
	}

	// 4. 初始化内存优化器（独立组件）
	components.MemoryOptimizer = NewMemoryOptimizer(config.MemoryBufferLimit)
	if config.LogContext != nil {
		fs.Debugf(config.LogContext, "%s网盘内存优化器初始化成功", config.BackendType)
	}

	// 5. 初始化并发下载器（依赖断点续传管理器和下载适配器）
	if components.ResumeManager != nil && config.CreateDownloadAdapter != nil {
		downloadAdapterInterface := config.CreateDownloadAdapter()
		if downloadAdapter, ok := downloadAdapterInterface.(DownloadAdapter); ok {
			components.ConcurrentDownloader = NewUnifiedConcurrentDownloader(config.LogContext, config.BackendType, downloadAdapter, components.ResumeManager)
			if config.LogContext != nil {
				fs.Debugf(config.LogContext, "%s网盘统一并发下载器初始化成功", config.BackendType)
			}
		} else {
			if config.LogContext != nil {
				fs.Errorf(config.LogContext, "%s网盘下载适配器类型转换失败", config.BackendType)
			}
		}
	}

	return components, nil
}

// Initialize123Components 初始化123网盘统一组件
// 专门为123网盘设计的组件初始化函数
func Initialize123Components(logContext fs.Fs, createDownloadAdapter func() interface{}) (*UnifiedComponentSet, error) {
	config := &ComponentInitializationConfig{
		BackendType:           "123",
		LogContext:            logContext,
		MemoryBufferLimit:     50 * 1024 * 1024, // 50MB
		ContinueOnError:       true,             // 组件失败不阻止文件系统工作
		CreateDownloadAdapter: createDownloadAdapter,
	}

	return InitializeUnifiedComponents(config)
}

// Initialize115Components 初始化115网盘统一组件
// 专门为115网盘设计的组件初始化函数
func Initialize115Components(logContext fs.Fs, createDownloadAdapter func() interface{}) (*UnifiedComponentSet, error) {
	config := &ComponentInitializationConfig{
		BackendType:           "115",
		LogContext:            logContext,
		MemoryBufferLimit:     50 * 1024 * 1024, // 50MB
		ContinueOnError:       true,             // 组件失败不阻止文件系统工作
		CreateDownloadAdapter: createDownloadAdapter,
	}

	return InitializeUnifiedComponents(config)
}

// ValidateComponentInitializationConfig 验证组件初始化配置
func ValidateComponentInitializationConfig(config *ComponentInitializationConfig) error {
	if config == nil {
		return fmt.Errorf("组件初始化配置不能为空")
	}

	if config.BackendType != "123" && config.BackendType != "115" {
		return fmt.Errorf("不支持的后端类型: %s", config.BackendType)
	}

	if config.LogContext == nil {
		return fmt.Errorf("日志上下文不能为空")
	}

	if config.MemoryBufferLimit <= 0 {
		return fmt.Errorf("内存缓冲限制必须大于0")
	}

	return nil
}

// GetComponentInitializationOrder 获取组件初始化顺序
// 返回组件初始化的推荐顺序，确保依赖关系正确
func GetComponentInitializationOrder() []string {
	return []string{
		"ResumeManager",         // 1. 断点续传管理器（最高优先级）
		"ErrorHandler",          // 2. 错误处理器
		"CrossCloudCoordinator", // 3. 跨云传输协调器（依赖前两者）
		"MemoryOptimizer",       // 4. 内存优化器（独立）
		"ConcurrentDownloader",  // 5. 并发下载器（依赖断点续传管理器）
	}
}

// LogComponentInitializationResult 记录组件初始化结果
func LogComponentInitializationResult(backendType string, logContext fs.Fs, components *UnifiedComponentSet, err error) {
	if logContext == nil {
		return
	}

	if err != nil {
		fs.Errorf(logContext, "%s统一组件初始化失败: %v", backendType, err)
		return
	}

	// 统计成功初始化的组件数量
	successCount := 0
	totalCount := 5 // 总共5个组件

	if components.ResumeManager != nil {
		successCount++
	}
	if components.ErrorHandler != nil {
		successCount++
	}
	if components.CrossCloudCoordinator != nil {
		successCount++
	}
	if components.MemoryOptimizer != nil {
		successCount++
	}
	if components.ConcurrentDownloader != nil {
		successCount++
	}

	if successCount == totalCount {
		fs.Debugf(logContext, "%s统一组件初始化完成: 成功初始化%d/%d个组件", backendType, successCount, totalCount)
	} else {
		fs.Infof(logContext, "%s统一组件部分初始化完成: 成功初始化%d/%d个组件", backendType, successCount, totalCount)
	}
}

// GetDefaultComponentConfig 获取默认的组件配置
func GetDefaultComponentConfig(backendType string, logContext fs.Fs) *ComponentInitializationConfig {
	return &ComponentInitializationConfig{
		BackendType:       backendType,
		LogContext:        logContext,
		MemoryBufferLimit: 50 * 1024 * 1024, // 50MB
		ContinueOnError:   true,             // 默认策略：组件失败不阻止文件系统工作
	}
}

// IsComponentInitializationRequired 检查是否需要初始化统一组件
func IsComponentInitializationRequired(backendType string) bool {
	// 目前123和115网盘都需要统一组件初始化
	return backendType == "123" || backendType == "115"
}

// GetRequiredComponents 获取指定后端类型需要的组件列表
func GetRequiredComponents(backendType string) []string {
	// 所有后端都需要这些基础组件
	baseComponents := []string{
		"ResumeManager",
		"ErrorHandler",
		"CrossCloudCoordinator",
		"MemoryOptimizer",
		"ConcurrentDownloader",
	}

	// 根据后端类型可以扩展特定组件
	switch backendType {
	case "123", "115":
		return baseComponents
	default:
		return []string{}
	}
}

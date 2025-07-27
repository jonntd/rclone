package common

import (
	"fmt"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/cache"
)

// CacheInstanceConfig 缓存实例配置
type CacheInstanceConfig struct {
	Name      string              // 缓存名称，如 "parent_ids", "dir_list"
	CachePtr  **cache.BadgerCache // 缓存实例指针
	Required  bool                // 是否为必需的缓存
	OnFailure string              // 失败时的处理策略："continue", "error", "warn"
}

// UnifiedCacheInitializer 统一的缓存初始化器
type UnifiedCacheInitializer struct {
	BackendType     string                // 后端类型："123" 或 "115"
	CacheDir        string                // 缓存目录
	LogContext      fs.Fs                 // 日志上下文
	ContinueOnError bool                  // 缓存失败时是否继续执行
	Instances       []CacheInstanceConfig // 缓存实例配置列表
}

// InitializeCloudDriveCache 统一的云盘缓存初始化函数
// 替代原来的重复初始化逻辑，支持123网盘和115网盘
// 🔧 新增：支持缓存优化配置参数
func InitializeCloudDriveCache(backendType string, logContext fs.Fs, instances map[string]**cache.BadgerCache, config *UnifiedCacheConfig) error {
	if backendType == "" {
		return fmt.Errorf("后端类型不能为空")
	}

	if instances == nil || len(instances) == 0 {
		return fmt.Errorf("缓存实例配置不能为空")
	}

	// 获取缓存目录
	cacheDir := cache.GetCacheDir(backendType + "drive")

	// 记录初始化开始
	if logContext != nil {
		fs.Debugf(logContext, "开始初始化%s缓存系统: %s", backendType, cacheDir)
	}

	// 使用现有的公共缓存初始化函数 - 支持用户配置
	cacheConfig := &cache.CloudDriveCacheConfig{
		CacheType:       backendType + "drive",
		CacheDir:        cacheDir,
		CacheInstances:  instances,
		ContinueOnError: true, // 缓存失败不阻止文件系统工作
		LogContext:      logContext,
		// 应用用户配置参数
		MaxCacheSize:       int64(config.MaxCacheSize),
		TargetCleanSize:    int64(config.TargetCleanSize),
		MemTableSize:       int64(config.MemTableSize),
		EnableSmartCleanup: config.EnableSmartCleanup,
		CleanupStrategy:    config.CleanupStrategy,
	}

	err := cache.InitCloudDriveCache(cacheConfig)
	if err != nil {
		if logContext != nil {
			fs.Errorf(logContext, "初始化%s缓存失败: %v", backendType, err)
		}
		// 缓存初始化失败不应该阻止文件系统工作，继续执行
	} else {
		if logContext != nil {
			fs.Infof(logContext, "简化缓存管理器初始化成功 (%s)", backendType)
		}
	}

	return nil
}

// Initialize123Cache 初始化123网盘缓存
// 专门为123网盘设计的缓存初始化函数
// 🔧 新增：支持缓存优化配置参数
func Initialize123Cache(logContext fs.Fs, parentIDCache, dirListCache, pathToIDCache **cache.BadgerCache, config *UnifiedCacheConfig) error {
	instances := map[string]**cache.BadgerCache{
		"parent_ids": parentIDCache,
		"dir_list":   dirListCache,
		"path_to_id": pathToIDCache,
	}

	return InitializeCloudDriveCache("123", logContext, instances, config)
}

// Initialize115Cache 初始化115网盘缓存
// 专门为115网盘设计的缓存初始化函数
// 🔧 新增：支持缓存优化配置参数
func Initialize115Cache(logContext fs.Fs, pathResolveCache, dirListCache, metadataCache, fileIDCache **cache.BadgerCache, config *UnifiedCacheConfig) error {
	instances := map[string]**cache.BadgerCache{
		"path_resolve": pathResolveCache,
		"dir_list":     dirListCache,
		"metadata":     metadataCache,
		"file_id":      fileIDCache,
	}

	return InitializeCloudDriveCache("115", logContext, instances, config)
}

// ValidateCacheInstances 验证缓存实例配置的有效性
func ValidateCacheInstances(instances map[string]**cache.BadgerCache) error {
	if instances == nil || len(instances) == 0 {
		return fmt.Errorf("缓存实例配置不能为空")
	}

	for name, cachePtr := range instances {
		if cachePtr == nil {
			return fmt.Errorf("缓存实例指针为空: %s", name)
		}
	}

	return nil
}

// GetCacheInstanceNames 获取指定后端类型的标准缓存实例名称
func GetCacheInstanceNames(backendType string) []string {
	switch backendType {
	case "123":
		return []string{"parent_ids", "dir_list", "path_to_id"}
	case "115":
		return []string{"path_resolve", "dir_list", "metadata", "file_id"}
	default:
		return []string{}
	}
}

// CreateStandardCacheConfig 创建标准的缓存配置
// 为指定后端类型创建标准的缓存配置结构
func CreateStandardCacheConfig(backendType string, logContext fs.Fs) *cache.CloudDriveCacheConfig {
	return &cache.CloudDriveCacheConfig{
		CacheType:       backendType + "drive",
		CacheDir:        cache.GetCacheDir(backendType + "drive"),
		ContinueOnError: true, // 默认在缓存初始化失败时继续执行
		LogContext:      logContext,
	}
}

// LogCacheInitializationResult 记录缓存初始化结果
func LogCacheInitializationResult(backendType string, logContext fs.Fs, err error, instanceCount int) {
	if logContext == nil {
		return
	}

	if err != nil {
		fs.Errorf(logContext, "%s缓存系统初始化失败: %v", backendType, err)
	} else {
		fs.Infof(logContext, "%s缓存系统初始化成功，共初始化%d个缓存实例", backendType, instanceCount)
	}
}

// GetCacheDirForBackend 获取指定后端的缓存目录
func GetCacheDirForBackend(backendType string) string {
	return cache.GetCacheDir(backendType + "drive")
}

// IsCacheInitializationRequired 检查是否需要初始化缓存
// 根据后端类型和配置决定是否需要初始化缓存系统
func IsCacheInitializationRequired(backendType string) bool {
	// 目前123和115网盘都需要缓存初始化
	return backendType == "123" || backendType == "115"
}

// GetDefaultCacheErrorHandling 获取默认的缓存错误处理策略
func GetDefaultCacheErrorHandling() bool {
	// 默认策略：缓存初始化失败时继续执行，不阻止文件系统工作
	return true
}

// CleanupCacheInstances 清理缓存实例
// 用于程序退出时的资源清理
func CleanupCacheInstances(backendType string, logContext fs.Fs, instances []*cache.BadgerCache) {
	if logContext != nil {
		fs.Debugf(logContext, "开始清理%s缓存资源...", backendType)
	}

	for i, c := range instances {
		if c != nil {
			if err := c.Close(); err != nil {
				if logContext != nil {
					fs.Debugf(logContext, "关闭%s缓存%d失败: %v", backendType, i, err)
				}
			}
		}
	}

	if logContext != nil {
		fs.Debugf(logContext, "%s缓存资源清理完成", backendType)
	}
}

// Package cache provides cloud drive cache management utilities
package cache

import (
	"fmt"

	"github.com/rclone/rclone/fs"
)

// CloudDriveCacheConfig 云盘缓存配置
type CloudDriveCacheConfig struct {
	// 基础配置
	CacheType string // 缓存类型标识，如 "115drive", "123drive"
	CacheDir  string // 缓存目录路径

	// 缓存实例配置
	CacheInstances map[string]**BadgerCache // 缓存实例映射

	// 错误处理配置
	ContinueOnError bool // 缓存初始化失败时是否继续执行

	// 日志配置
	LogContext fs.Fs // 日志上下文，用于输出日志
}

// InitCloudDriveCache 初始化云盘缓存系统
// 这是一个通用的缓存初始化函数，支持多种云盘后端
func InitCloudDriveCache(config *CloudDriveCacheConfig) error {
	if config == nil {
		return fmt.Errorf("缓存配置不能为空")
	}

	if config.CacheType == "" {
		return fmt.Errorf("缓存类型不能为空")
	}

	if config.CacheDir == "" {
		// 使用默认缓存目录
		config.CacheDir = GetCacheDir(config.CacheType)
	}

	if config.CacheInstances == nil {
		return fmt.Errorf("缓存实例配置不能为空")
	}

	// 记录初始化开始
	if config.LogContext != nil {
		fs.Debugf(config.LogContext, "开始初始化%s缓存系统: %s", config.CacheType, config.CacheDir)
	}

	// 初始化各个缓存实例
	successCount := 0
	totalCount := len(config.CacheInstances)

	for name, cachePtr := range config.CacheInstances {
		if cachePtr == nil {
			if config.LogContext != nil {
				fs.Errorf(config.LogContext, "缓存实例指针为空: %s", name)
			}
			continue
		}

		// 创建BadgerCache实例
		cache, err := NewBadgerCache(name, config.CacheDir)
		if err != nil {
			if config.LogContext != nil {
				fs.Errorf(config.LogContext, "初始化%s缓存失败: %v", name, err)
			}

			// 根据配置决定是否继续执行
			if !config.ContinueOnError {
				return fmt.Errorf("初始化%s缓存失败: %w", name, err)
			}
		} else {
			// 成功初始化
			*cachePtr = cache
			successCount++

			if config.LogContext != nil {
				fs.Debugf(config.LogContext, "%s缓存初始化成功", name)
			}
		}
	}

	// 记录初始化结果
	if config.LogContext != nil {
		if successCount == totalCount {
			fs.Debugf(config.LogContext, "%s缓存系统初始化完成: %s (成功: %d/%d)",
				config.CacheType, config.CacheDir, successCount, totalCount)
		} else {
			fs.Infof(config.LogContext, "%s缓存系统部分初始化完成: %s (成功: %d/%d)",
				config.CacheType, config.CacheDir, successCount, totalCount)
		}
	}

	return nil
}

// CreateCacheConfig 创建标准的缓存配置
// 这是一个便利函数，用于创建常见的缓存配置
func CreateCacheConfig(cacheType string, logContext fs.Fs, cacheInstances map[string]**BadgerCache) *CloudDriveCacheConfig {
	return &CloudDriveCacheConfig{
		CacheType:       cacheType,
		CacheDir:        GetCacheDir(cacheType),
		CacheInstances:  cacheInstances,
		ContinueOnError: true, // 默认在缓存初始化失败时继续执行
		LogContext:      logContext,
	}
}

// ValidateCacheConfig 验证缓存配置的有效性
func ValidateCacheConfig(config *CloudDriveCacheConfig) error {
	if config == nil {
		return fmt.Errorf("缓存配置不能为空")
	}

	if config.CacheType == "" {
		return fmt.Errorf("缓存类型不能为空")
	}

	if len(config.CacheInstances) == 0 {
		return fmt.Errorf("缓存实例配置不能为空")
	}

	// 检查缓存实例指针的有效性
	for name, cachePtr := range config.CacheInstances {
		if cachePtr == nil {
			return fmt.Errorf("缓存实例指针为空: %s", name)
		}
	}

	return nil
}

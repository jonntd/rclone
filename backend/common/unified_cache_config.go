package common

import (
	"fmt"
	"time"
)

// UnifiedCacheConfig 统一的缓存配置结构
// 用于标准化123网盘和115网盘的缓存配置，提高一致性和可维护性
type UnifiedCacheConfig struct {
	// 基础缓存TTL配置 - 统一为5分钟策略
	ParentIDCacheTTL    time.Duration // 父目录ID验证缓存TTL
	DirListCacheTTL     time.Duration // 目录列表缓存TTL
	PathToIDCacheTTL    time.Duration // 路径到ID映射缓存TTL
	MetadataCacheTTL    time.Duration // 文件元数据缓存TTL
	FileIDCacheTTL      time.Duration // 文件ID验证缓存TTL
	DownloadURLCacheTTL time.Duration // 下载URL缓存TTL（123网盘特有）

	// 扩展缓存配置
	NetworkSpeedCacheTTL time.Duration // 网络速度缓存TTL
	MD5CacheTTL          time.Duration // MD5缓存TTL（跨云传输用）
	PickCodeCacheTTL     time.Duration // PickCode缓存TTL（115网盘特有）

	// 缓存清理配置
	CleanupInterval time.Duration // 缓存清理间隔
	CleanupTimeout  time.Duration // 缓存清理超时

	// 后端类型标识
	BackendType string // "123" 或 "115"
}

// DefaultUnifiedCacheConfig 返回指定后端类型的默认统一缓存配置
// backendType: "123" 或 "115"
func DefaultUnifiedCacheConfig(backendType string) UnifiedCacheConfig {
	// 统一TTL策略 - 5分钟
	unifiedTTL := 5 * time.Minute

	config := UnifiedCacheConfig{
		// 基础缓存配置 - 所有后端通用
		ParentIDCacheTTL: unifiedTTL,
		DirListCacheTTL:  unifiedTTL,
		PathToIDCacheTTL: unifiedTTL,
		MetadataCacheTTL: unifiedTTL,
		FileIDCacheTTL:   unifiedTTL,

		// 扩展缓存配置
		NetworkSpeedCacheTTL: unifiedTTL,
		MD5CacheTTL:          24 * time.Hour, // MD5缓存保持24小时
		PickCodeCacheTTL:     unifiedTTL,

		// 缓存清理配置
		CleanupInterval: 5 * time.Minute,
		CleanupTimeout:  2 * time.Minute,

		// 后端类型
		BackendType: backendType,
	}

	// 后端特定的配置调整
	switch backendType {
	case "123":
		// 123网盘特有配置
		config.DownloadURLCacheTTL = 0 // 动态TTL，根据API返回的过期时间
	case "115":
		// 115网盘特有配置
		config.DownloadURLCacheTTL = unifiedTTL // 115网盘使用固定TTL
	default:
		// 默认配置
		config.DownloadURLCacheTTL = unifiedTTL
	}

	return config
}

// GetTTLForCacheType 根据缓存类型获取对应的TTL
// 提供统一的TTL查询接口
func (c *UnifiedCacheConfig) GetTTLForCacheType(cacheType string) time.Duration {
	switch cacheType {
	case "parent_id":
		return c.ParentIDCacheTTL
	case "dir_list":
		return c.DirListCacheTTL
	case "path_to_id":
		return c.PathToIDCacheTTL
	case "metadata":
		return c.MetadataCacheTTL
	case "file_id":
		return c.FileIDCacheTTL
	case "download_url":
		return c.DownloadURLCacheTTL
	case "network_speed":
		return c.NetworkSpeedCacheTTL
	case "md5":
		return c.MD5CacheTTL
	case "pick_code":
		return c.PickCodeCacheTTL
	default:
		// 默认返回统一TTL
		return 5 * time.Minute
	}
}

// SetTTLForCacheType 设置指定缓存类型的TTL
// 提供统一的TTL设置接口
func (c *UnifiedCacheConfig) SetTTLForCacheType(cacheType string, ttl time.Duration) {
	switch cacheType {
	case "parent_id":
		c.ParentIDCacheTTL = ttl
	case "dir_list":
		c.DirListCacheTTL = ttl
	case "path_to_id":
		c.PathToIDCacheTTL = ttl
	case "metadata":
		c.MetadataCacheTTL = ttl
	case "file_id":
		c.FileIDCacheTTL = ttl
	case "download_url":
		c.DownloadURLCacheTTL = ttl
	case "network_speed":
		c.NetworkSpeedCacheTTL = ttl
	case "md5":
		c.MD5CacheTTL = ttl
	case "pick_code":
		c.PickCodeCacheTTL = ttl
	}
}

// IsUnifiedTTL 检查是否使用统一TTL策略
// 返回true表示所有基础缓存都使用相同的TTL
func (c *UnifiedCacheConfig) IsUnifiedTTL() bool {
	baseTTL := c.ParentIDCacheTTL
	return c.DirListCacheTTL == baseTTL &&
		c.PathToIDCacheTTL == baseTTL &&
		c.MetadataCacheTTL == baseTTL &&
		c.FileIDCacheTTL == baseTTL
}

// GetCacheTypes 获取当前后端支持的缓存类型列表
func (c *UnifiedCacheConfig) GetCacheTypes() []string {
	baseTypes := []string{
		"parent_id",
		"dir_list",
		"path_to_id",
		"metadata",
		"file_id",
		"network_speed",
		"md5",
	}

	switch c.BackendType {
	case "123":
		baseTypes = append(baseTypes, "download_url")
	case "115":
		baseTypes = append(baseTypes, "download_url", "pick_code")
	}

	return baseTypes
}

// Validate 验证缓存配置的有效性
func (c *UnifiedCacheConfig) Validate() error {
	// 检查TTL是否为正值
	if c.ParentIDCacheTTL <= 0 ||
		c.DirListCacheTTL <= 0 ||
		c.PathToIDCacheTTL <= 0 ||
		c.MetadataCacheTTL <= 0 ||
		c.FileIDCacheTTL <= 0 {
		return fmt.Errorf("缓存TTL必须为正值")
	}

	// 检查后端类型
	if c.BackendType != "123" && c.BackendType != "115" {
		return fmt.Errorf("不支持的后端类型: %s", c.BackendType)
	}

	return nil
}

// Clone 创建配置的深拷贝
func (c *UnifiedCacheConfig) Clone() UnifiedCacheConfig {
	return UnifiedCacheConfig{
		ParentIDCacheTTL:     c.ParentIDCacheTTL,
		DirListCacheTTL:      c.DirListCacheTTL,
		PathToIDCacheTTL:     c.PathToIDCacheTTL,
		MetadataCacheTTL:     c.MetadataCacheTTL,
		FileIDCacheTTL:       c.FileIDCacheTTL,
		DownloadURLCacheTTL:  c.DownloadURLCacheTTL,
		NetworkSpeedCacheTTL: c.NetworkSpeedCacheTTL,
		MD5CacheTTL:          c.MD5CacheTTL,
		PickCodeCacheTTL:     c.PickCodeCacheTTL,
		CleanupInterval:      c.CleanupInterval,
		CleanupTimeout:       c.CleanupTimeout,
		BackendType:          c.BackendType,
	}
}

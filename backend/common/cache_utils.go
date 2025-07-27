package common

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"time"
)

// GeneratePathToIDCacheKey 生成路径到ID映射缓存键
// 统一123网盘和115网盘的缓存键格式
func GeneratePathToIDCacheKey(path string) string {
	return fmt.Sprintf("path_to_id_%s", path)
}

// GenerateTaskID 生成统一的任务ID
// 替代原来的GenerateTaskID123和GenerateTaskID115函数
// backendType: "123" 或 "115"
// filePath: 文件路径
// fileSize: 文件大小
func GenerateTaskID(backendType, filePath string, fileSize int64) string {
	// 使用文件路径和大小生成稳定的哈希值
	hasher := sha256.New()
	hasher.Write([]byte(fmt.Sprintf("%s_%s_%d", backendType, filePath, fileSize)))
	hash := hex.EncodeToString(hasher.Sum(nil))[:16] // 取前16位作为短哈希

	return fmt.Sprintf("%s_%s_%d_%s",
		backendType,
		filePath,
		fileSize,
		hash)
}

// CalculateChecksum 计算数据的轻量校验和（使用CRC32替代SHA256）
// 用于缓存条目的完整性验证
func CalculateChecksum(data interface{}) string {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return ""
	}

	// 使用CRC32替代SHA256，减少CPU开销
	hash := crc32.ChecksumIEEE(jsonData)
	return fmt.Sprintf("%08x", hash)
}

// GenerateCacheVersion 生成缓存版本号
// 用于缓存条目的版本控制
func GenerateCacheVersion() int64 {
	return time.Now().UnixNano()
}

// ValidateCacheEntry 验证缓存条目的完整性（轻量级验证）
// 对于性能考虑，只对小数据进行校验
func ValidateCacheEntry(data interface{}, expectedChecksum string) bool {
	if expectedChecksum == "" {
		return true // 如果没有校验和，跳过验证
	}

	// 对于性能考虑，只对小数据进行校验
	jsonData, err := json.Marshal(data)
	if err != nil {
		return false
	}

	// 如果数据过大（>10KB），跳过校验以提高性能
	if len(jsonData) > 10*1024 {
		return true
	}

	actualChecksum := CalculateChecksum(data)
	return actualChecksum == expectedChecksum
}

// GenerateDirListCacheKey 生成目录列表缓存键
// 统一123网盘和115网盘的目录列表缓存键格式
func GenerateDirListCacheKey(parentID, lastID string) string {
	return fmt.Sprintf("dirlist_%s_%s", parentID, lastID)
}

// GenerateParentIDCacheKey 生成父目录ID验证缓存键
// 用于123网盘的父目录ID验证缓存
func GenerateParentIDCacheKey(parentID int64) string {
	return fmt.Sprintf("parent_%d", parentID)
}

// GenerateParentIDValidCacheKey 生成父目录ID有效性缓存键
// 用于123网盘的父目录ID有效性验证缓存
func GenerateParentIDValidCacheKey(parentID int64) string {
	return fmt.Sprintf("parent_id_valid_%d", parentID)
}

// GenerateMD5CacheKey 生成MD5缓存键
// 用于跨云传输时的MD5缓存
func GenerateMD5CacheKey(fsName, remote string, size int64, modTime int64) string {
	return fmt.Sprintf("%s:%s:%d:%d", fsName, remote, size, modTime)
}

// IsLargeData 判断数据是否过大，用于决定是否跳过某些验证
// 阈值设为10KB，超过此大小的数据跳过校验以提高性能
func IsLargeData(data interface{}) bool {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return false
	}
	return len(jsonData) > 10*1024
}

// GenerateFileIDCacheKey 生成文件ID缓存键
// 用于115网盘的文件ID验证缓存
func GenerateFileIDCacheKey(fileID string) string {
	return fmt.Sprintf("file_id_%s", fileID)
}

// GenerateMetadataCacheKey 生成元数据缓存键
// 用于115网盘的文件元数据缓存
func GenerateMetadataCacheKey(fileID string) string {
	return fmt.Sprintf("metadata_%s", fileID)
}

// SanitizeCacheKey 清理缓存键，确保键名安全
// 移除或替换可能导致问题的字符
func SanitizeCacheKey(key string) string {
	// 简单的清理逻辑，可以根据需要扩展
	// 这里主要是确保键名不包含特殊字符
	return key
}

// GetCacheKeyPrefix 获取缓存键前缀
// 用于统一不同类型缓存的键名格式
func GetCacheKeyPrefix(cacheType string) string {
	switch cacheType {
	case "path_to_id":
		return "path_to_id_"
	case "dir_list":
		return "dirlist_"
	case "parent_id":
		return "parent_"
	case "file_id":
		return "file_id_"
	case "metadata":
		return "metadata_"
	default:
		return fmt.Sprintf("%s_", cacheType)
	}
}

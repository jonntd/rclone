// Package cache provides a high-performance persistent cache implementation using BadgerDB
package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/rclone/rclone/fs"
)

// PersistentCache 持久化缓存接口，支持多种实现
type PersistentCache interface {
	Set(key string, value interface{}, ttl time.Duration) error
	Get(key string, result interface{}) (bool, error)
	Delete(key string) error
	DeletePrefix(prefix string) error
	Clear() error
	Stats() map[string]interface{}
	ListAllKeys() ([]string, error)
	GetAllEntries() (map[string]interface{}, error)
	GetKeysByPrefix(prefix string) ([]string, error)
	Close() error
}

// BadgerCache 基于BadgerDB的高性能持久化缓存
type BadgerCache struct {
	db             *badger.DB
	name           string
	basePath       string
	operationCount int64 // 🔧 轻量级优化：操作计数器，用于定期检查缓存大小
}

// CacheEntry 缓存条目的通用结构
type CacheEntry struct {
	Key       string      `json:"key"`
	Value     interface{} `json:"value"`
	CreatedAt time.Time   `json:"created_at"`
	ExpiresAt time.Time   `json:"expires_at"`
}

// NewBadgerCache 创建新的BadgerDB缓存实例 - 支持多实例冲突处理
func NewBadgerCache(name, basePath string) (*BadgerCache, error) {
	// 创建缓存目录
	cacheDir := filepath.Join(basePath, name)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("创建缓存目录失败: %w", err)
	}

	// 配置BadgerDB选项 - 轻量级优化：提高性能和稳定性
	opts := badger.DefaultOptions(cacheDir)
	opts.Logger = nil                // 禁用BadgerDB日志，避免干扰rclone日志
	opts.SyncWrites = false          // 异步写入，提高性能
	opts.CompactL0OnClose = true     // 关闭时压缩，减少磁盘占用
	opts.ValueLogFileSize = 64 << 20 // 64MB value log文件，适合大文件传输
	opts.MemTableSize = 64 << 20     // 🔧 从32MB增加到64MB，提高写入性能
	opts.BaseTableSize = 8 << 20     // 8MB基础表大小
	opts.BaseLevelSize = 64 << 20    // 64MB基础级别大小
	opts.NumMemtables = 3            // 🔧 从2增加到3，提高并发性能
	opts.NumLevelZeroTables = 2      // 限制L0表数量
	opts.NumLevelZeroTablesStall = 4 // L0表停顿阈值
	opts.ValueThreshold = 512        // 🔧 从1024减少到512，更多数据存储在LSM中，提高读取性能

	// 多实例冲突处理：尝试打开数据库，如果失败则使用只读模式
	db, err := badger.Open(opts)
	if err != nil {
		// 检查是否是数据库锁定错误（多实例冲突）
		if isLockError(err) {
			fs.Infof(nil, "检测到多实例缓存冲突，尝试只读模式: %s", cacheDir)

			// 尝试只读模式
			opts.ReadOnly = true
			db, err = badger.Open(opts)
			if err != nil {
				fs.Infof(nil, "只读模式也失败，创建模拟缓存: %s, 错误: %v", cacheDir, err)
				// 创建一个模拟的BadgerCache，所有操作都是空操作
				return &BadgerCache{
					db:       nil, // 空数据库指针，表示禁用状态
					name:     name + "_disabled",
					basePath: basePath,
				}, nil
			}
			fs.Infof(nil, "使用只读缓存模式: %s", cacheDir)
		} else {
			return nil, fmt.Errorf("打开BadgerDB失败: %w", err)
		}
	}

	cache := &BadgerCache{
		db:       db,
		name:     name,
		basePath: basePath,
	}

	// 只有在非只读模式下才启动后台垃圾回收
	if !opts.ReadOnly {
		go cache.runGC()
	}

	fs.Debugf(nil, "BadgerDB缓存初始化成功: %s (只读模式: %v)", cacheDir, opts.ReadOnly)
	return cache, nil
}

// Set 设置缓存值，支持TTL
func (c *BadgerCache) Set(key string, value interface{}, ttl time.Duration) error {
	// 如果数据库被禁用，静默忽略
	if c.db == nil {
		return nil
	}

	// 🔧 轻量级优化：定期检查缓存大小
	c.operationCount++
	if c.operationCount%1000 == 0 {
		c.checkCacheSize()
	}

	entry := CacheEntry{
		Key:       key,
		Value:     value,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("序列化缓存条目失败: %w", err)
	}

	return c.db.Update(func(txn *badger.Txn) error {
		e := badger.NewEntry([]byte(key), data).WithTTL(ttl)
		return txn.SetEntry(e)
	})
}

// Get 获取缓存值
func (c *BadgerCache) Get(key string, result interface{}) (bool, error) {
	// 如果数据库被禁用，总是返回未找到
	if c.db == nil {
		return false, nil
	}

	var found bool
	var entry CacheEntry

	err := c.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(key))
		if err != nil {
			if err == badger.ErrKeyNotFound {
				return nil // 键不存在，不是错误
			}
			return err
		}

		return item.Value(func(val []byte) error {
			if err := json.Unmarshal(val, &entry); err != nil {
				return fmt.Errorf("反序列化缓存条目失败: %w", err)
			}
			found = true
			return nil
		})
	})

	if err != nil {
		return false, err
	}

	if !found {
		return false, nil
	}

	// 检查是否过期（双重保险，BadgerDB TTL + 应用层检查）
	if time.Now().After(entry.ExpiresAt) {
		// 异步删除过期条目
		go c.Delete(key)
		return false, nil
	}

	// 将值反序列化到result中
	valueData, err := json.Marshal(entry.Value)
	if err != nil {
		return false, fmt.Errorf("序列化缓存值失败: %w", err)
	}

	if err := json.Unmarshal(valueData, result); err != nil {
		return false, fmt.Errorf("反序列化缓存值失败: %w", err)
	}

	return true, nil
}

// GetBool 获取布尔值的便捷方法
func (c *BadgerCache) GetBool(key string) (bool, bool) {
	var result bool
	found, err := c.Get(key, &result)
	if err != nil {
		fs.Debugf(nil, "获取布尔缓存失败 %s: %v", key, err)
		return false, false
	}
	return result, found
}

// SetBool 设置布尔值的便捷方法
func (c *BadgerCache) SetBool(key string, value bool, ttl time.Duration) error {
	return c.Set(key, value, ttl)
}

// Delete 删除缓存条目
func (c *BadgerCache) Delete(key string) error {
	// 如果数据库被禁用，静默忽略
	if c.db == nil {
		return nil
	}
	return c.db.Update(func(txn *badger.Txn) error {
		return txn.Delete([]byte(key))
	})
}

// DeletePrefix 删除指定前缀的所有缓存条目
func (c *BadgerCache) DeletePrefix(prefix string) error {
	// 如果数据库被禁用，静默忽略
	if c.db == nil {
		return nil
	}
	return c.db.Update(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		prefixBytes := []byte(prefix)
		for it.Seek(prefixBytes); it.ValidForPrefix(prefixBytes); it.Next() {
			if err := txn.Delete(it.Item().Key()); err != nil {
				return err
			}
		}
		return nil
	})
}

// Clear 清空所有缓存
func (c *BadgerCache) Clear() error {
	// 如果数据库被禁用，静默忽略
	if c.db == nil {
		return nil
	}
	return c.db.DropAll()
}

// Stats 获取缓存统计信息
func (c *BadgerCache) Stats() map[string]interface{} {
	// 如果数据库被禁用，返回禁用状态信息
	if c.db == nil {
		return map[string]interface{}{
			"name":       c.name,
			"lsm_size":   0,
			"vlog_size":  0,
			"total_size": 0,
			"status":     "disabled",
		}
	}
	lsm, vlog := c.db.Size()
	return map[string]interface{}{
		"name":       c.name,
		"lsm_size":   lsm,
		"vlog_size":  vlog,
		"total_size": lsm + vlog,
		"status":     "active",
	}
}

// ListAllKeys 列出所有缓存键
func (c *BadgerCache) ListAllKeys() ([]string, error) {
	// 如果数据库被禁用，返回空列表
	if c.db == nil {
		return []string{}, nil
	}

	var keys []string

	err := c.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false // 只需要键，不需要值
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			key := string(item.Key())
			keys = append(keys, key)
		}
		return nil
	})

	return keys, err
}

// GetAllEntries 获取所有缓存条目
func (c *BadgerCache) GetAllEntries() (map[string]interface{}, error) {
	// 如果数据库被禁用，返回空映射
	if c.db == nil {
		return map[string]interface{}{}, nil
	}

	entries := make(map[string]interface{})

	err := c.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			key := string(item.Key())

			// 检查是否过期
			if item.ExpiresAt() > 0 && item.ExpiresAt() < uint64(time.Now().Unix()) {
				continue // 跳过过期的条目
			}

			err := item.Value(func(val []byte) error {
				// 尝试解析为不同类型
				var value interface{}

				// 首先尝试解析为JSON
				if err := json.Unmarshal(val, &value); err == nil {
					entries[key] = value
				} else {
					// 如果不是JSON，存储为字符串
					entries[key] = string(val)
				}
				return nil
			})

			if err != nil {
				entries[key] = fmt.Sprintf("读取错误: %v", err)
			}
		}
		return nil
	})

	return entries, err
}

// GetKeysByPrefix 根据前缀获取键
func (c *BadgerCache) GetKeysByPrefix(prefix string) ([]string, error) {
	// 如果数据库被禁用，返回空列表
	if c.db == nil {
		return []string{}, nil
	}

	var keys []string

	err := c.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		prefixBytes := []byte(prefix)
		for it.Seek(prefixBytes); it.ValidForPrefix(prefixBytes); it.Next() {
			item := it.Item()
			key := string(item.Key())
			keys = append(keys, key)
		}
		return nil
	})

	return keys, err
}

// Close 关闭缓存数据库
func (c *BadgerCache) Close() error {
	if c.db != nil {
		fs.Debugf(nil, "关闭BadgerDB缓存: %s", c.name)
		return c.db.Close()
	}
	return nil
}

// runGC 运行后台垃圾回收 - 优化频率和阈值
func (c *BadgerCache) runGC() {
	ticker := time.NewTicker(15 * time.Minute) // 减少GC频率，从5分钟改为15分钟
	defer ticker.Stop()

	for range ticker.C {
		// 使用更保守的GC阈值，减少内存压力
		err := c.db.RunValueLogGC(0.5) // 从0.7改为0.5，更积极地回收
		if err != nil && err != badger.ErrNoRewrite {
			fs.Debugf(nil, "BadgerDB垃圾回收失败 %s: %v", c.name, err)
		} else if err == nil {
			fs.Debugf(nil, "BadgerDB垃圾回收完成 %s", c.name)
		}
	}
}

// checkCacheSize 检查缓存大小，超过限制时清理
// 🔧 轻量级优化：简单的缓存大小控制机制
func (c *BadgerCache) checkCacheSize() {
	if c.db == nil {
		return
	}

	// 获取数据库大小信息
	lsm, vlog := c.db.Size()
	totalSize := lsm + vlog

	// 设置最大缓存大小为100MB
	const maxCacheSize = 100 << 20 // 100MB

	if totalSize > maxCacheSize {
		fs.Debugf(nil, "缓存 %s 大小超过限制 (%d MB)，执行清理", c.name, totalSize>>20)
		// 简单策略：超过限制就清空缓存
		if err := c.Clear(); err != nil {
			fs.Debugf(nil, "清理缓存 %s 失败: %v", c.name, err)
		} else {
			fs.Debugf(nil, "已清理缓存 %s", c.name)
		}
	}
}

// GetCacheDir 获取缓存目录路径 - 支持多实例共享
func GetCacheDir(name string) string {
	// 优先使用用户缓存目录 - 所有实例共享同一路径
	if userCacheDir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(userCacheDir, "rclone", name)
	}

	// 回退到临时目录
	return filepath.Join(os.TempDir(), "rclone_cache", name)
}

// CleanupExpiredCaches 清理过期的缓存目录（可选的维护功能）
func CleanupExpiredCaches(basePath string, maxAge time.Duration) error {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return err
	}

	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			cachePath := filepath.Join(basePath, entry.Name())
			fs.Debugf(nil, "清理过期缓存目录: %s", cachePath)
			os.RemoveAll(cachePath)
		}
	}

	return nil
}

// isLockError 检查错误是否为数据库锁定错误（多实例冲突）
func isLockError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	// 检查常见的数据库锁定错误信息
	lockKeywords := []string{
		"resource temporarily unavailable",
		"database is locked",
		"cannot acquire directory lock",
		"lock",
		"resource busy",
	}

	for _, keyword := range lockKeywords {
		if strings.Contains(errStr, keyword) {
			return true
		}
	}
	return false
}

// NullCache 空缓存实现，用于多实例冲突时的降级方案
type NullCache struct {
	name     string
	basePath string
}

// NewNullCache 创建空缓存实例
func NewNullCache(name, basePath string) *NullCache {
	return &NullCache{
		name:     name,
		basePath: basePath,
	}
}

// Set 空实现，不执行任何操作
func (c *NullCache) Set(key string, value interface{}, ttl time.Duration) error {
	return nil // 静默忽略
}

// Get 空实现，总是返回未找到
func (c *NullCache) Get(key string, result interface{}) (bool, error) {
	return false, nil // 总是未找到
}

// Delete 空实现，不执行任何操作
func (c *NullCache) Delete(key string) error {
	return nil // 静默忽略
}

// DeletePrefix 空实现，不执行任何操作
func (c *NullCache) DeletePrefix(prefix string) error {
	return nil // 静默忽略
}

// Clear 空实现，不执行任何操作
func (c *NullCache) Clear() error {
	return nil // 静默忽略
}

// Stats 返回空统计信息
func (c *NullCache) Stats() map[string]interface{} {
	return map[string]interface{}{
		"name":       c.name + "_null",
		"lsm_size":   0,
		"vlog_size":  0,
		"total_size": 0,
		"type":       "null_cache",
	}
}

// ListAllKeys 返回空键列表
func (c *NullCache) ListAllKeys() ([]string, error) {
	return []string{}, nil
}

// GetAllEntries 返回空条目列表
func (c *NullCache) GetAllEntries() (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

// GetKeysByPrefix 返回空键列表
func (c *NullCache) GetKeysByPrefix(prefix string) ([]string, error) {
	return []string{}, nil
}

// Close 空实现，不执行任何操作
func (c *NullCache) Close() error {
	return nil // 静默忽略
}

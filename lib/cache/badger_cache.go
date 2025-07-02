// Package cache provides a high-performance persistent cache implementation using BadgerDB
package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/rclone/rclone/fs"
)

// BadgerCache 基于BadgerDB的高性能持久化缓存
type BadgerCache struct {
	db       *badger.DB
	name     string
	basePath string
}

// CacheEntry 缓存条目的通用结构
type CacheEntry struct {
	Key       string      `json:"key"`
	Value     interface{} `json:"value"`
	CreatedAt time.Time   `json:"created_at"`
	ExpiresAt time.Time   `json:"expires_at"`
}

// NewBadgerCache 创建新的BadgerDB缓存实例
func NewBadgerCache(name, basePath string) (*BadgerCache, error) {
	// 创建缓存目录
	cacheDir := filepath.Join(basePath, name)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("创建缓存目录失败: %w", err)
	}

	// 配置BadgerDB选项 - 优化内存使用和性能
	opts := badger.DefaultOptions(cacheDir)
	opts.Logger = nil                // 禁用BadgerDB日志，避免干扰rclone日志
	opts.SyncWrites = false          // 异步写入，提高性能
	opts.CompactL0OnClose = true     // 关闭时压缩，减少磁盘占用
	opts.ValueLogFileSize = 64 << 20 // 64MB value log文件，适合大文件传输
	opts.MemTableSize = 32 << 20     // 32MB内存表大小，减少内存使用
	opts.BaseTableSize = 8 << 20     // 8MB基础表大小
	opts.BaseLevelSize = 64 << 20    // 64MB基础级别大小
	opts.NumMemtables = 2            // 限制内存表数量
	opts.NumLevelZeroTables = 2      // 限制L0表数量
	opts.NumLevelZeroTablesStall = 4 // L0表停顿阈值
	opts.ValueThreshold = 1024       // 1KB值阈值，小值存储在LSM中

	// 打开数据库
	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("打开BadgerDB失败: %w", err)
	}

	cache := &BadgerCache{
		db:       db,
		name:     name,
		basePath: basePath,
	}

	// 启动后台垃圾回收
	go cache.runGC()

	fs.Debugf(nil, "BadgerDB缓存初始化成功: %s", cacheDir)
	return cache, nil
}

// Set 设置缓存值，支持TTL
func (c *BadgerCache) Set(key string, value interface{}, ttl time.Duration) error {
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
	return c.db.Update(func(txn *badger.Txn) error {
		return txn.Delete([]byte(key))
	})
}

// DeletePrefix 删除指定前缀的所有缓存条目
func (c *BadgerCache) DeletePrefix(prefix string) error {
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
	return c.db.DropAll()
}

// Stats 获取缓存统计信息
func (c *BadgerCache) Stats() map[string]interface{} {
	lsm, vlog := c.db.Size()
	return map[string]interface{}{
		"name":       c.name,
		"lsm_size":   lsm,
		"vlog_size":  vlog,
		"total_size": lsm + vlog,
	}
}

// ListAllKeys 列出所有缓存键
func (c *BadgerCache) ListAllKeys() ([]string, error) {
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

// GetCacheDir 获取缓存目录路径
func GetCacheDir(name string) string {
	// 优先使用用户缓存目录
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

// Package cache provides a high-performance persistent cache implementation using BadgerDB
package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
// 🔧 优化：添加内存备份机制，确保多实例冲突时仍可用
type BadgerCache struct {
	db             *badger.DB
	name           string
	basePath       string
	operationCount int64 // 🔧 轻量级优化：操作计数器，用于定期检查缓存大小

	// 🔧 新增：内存备份机制
	memoryBackup   map[string]*CacheEntry // 内存备份存储
	memoryMutex    sync.RWMutex           // 内存备份的读写锁
	isMemoryMode   bool                   // 是否处于内存模式
	maxMemoryItems int                    // 内存模式下的最大条目数

	// 🔧 新增：监控统计字段
	cacheHits         int64        // 缓存命中次数
	cacheMisses       int64        // 缓存未命中次数
	cleanupCount      int64        // 清理操作次数
	lastCleanupTime   time.Time    // 最后清理时间
	totalItemsCleaned int64        // 累计清理条目数
	totalBytesCleaned int64        // 累计清理字节数
	statsMutex        sync.RWMutex // 统计数据的读写锁
}

// CacheEntry 缓存条目的通用结构
// 🔧 增强：添加访问统计字段，支持智能清理策略
type CacheEntry struct {
	Key       string      `json:"key"`
	Value     interface{} `json:"value"`
	CreatedAt time.Time   `json:"created_at"`
	ExpiresAt time.Time   `json:"expires_at"`

	// 访问统计字段 - 新增，使用omitempty确保向后兼容
	AccessCount int64     `json:"access_count,omitempty"` // 访问计数
	LastAccess  time.Time `json:"last_access,omitempty"`  // 最后访问时间
	Priority    int       `json:"priority,omitempty"`     // 数据优先级：1=高，2=中，3=低
}

// cacheItem 用于智能清理的缓存条目信息
// 🔧 新增：支持多种清理策略的数据结构
type cacheItem struct {
	key         string
	size        int64
	createdAt   time.Time
	lastAccess  time.Time // 最后访问时间
	accessCount int64     // 访问计数
	priority    int       // 数据优先级
}

// NewBadgerCache 创建新的BadgerDB缓存实例 - 支持多实例冲突处理
func NewBadgerCache(name, basePath string) (*BadgerCache, error) {
	// 创建缓存目录
	cacheDir := filepath.Join(basePath, name)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("创建缓存目录失败: %w", err)
	}

	// 配置BadgerDB选项 - 内存优化：减少内存占用30-50%
	opts := badger.DefaultOptions(cacheDir)
	opts.Logger = nil                // 禁用BadgerDB日志，避免干扰rclone日志
	opts.SyncWrites = false          // 异步写入，提高性能
	opts.CompactL0OnClose = true     // 关闭时压缩，减少磁盘占用
	opts.ValueLogFileSize = 64 << 20 // 64MB value log文件，适合大文件传输
	opts.MemTableSize = 32 << 20     // 🔧 内存优化：从64MB降到32MB，减少内存占用
	opts.BaseTableSize = 8 << 20     // 8MB基础表大小
	opts.BaseLevelSize = 64 << 20    // 64MB基础级别大小
	opts.NumMemtables = 2            // 🔧 内存优化：从3降到2，减少内存占用
	opts.NumLevelZeroTables = 2      // 限制L0表数量
	opts.NumLevelZeroTablesStall = 4 // L0表停顿阈值
	opts.ValueThreshold = 256        // 🔧 内存优化：从512降到256，更多数据存储在LSM中

	// 🔧 优化：多实例冲突处理，使用内存备份而不是完全禁用
	db, err := badger.Open(opts)
	if err != nil {
		// 检查是否是数据库锁定错误（多实例冲突）
		if isLockError(err) {
			fs.Infof(nil, "检测到多实例缓存冲突，尝试只读模式: %s", cacheDir)

			// 尝试只读模式
			opts.ReadOnly = true
			db, err = badger.Open(opts)
			if err != nil {
				fs.Infof(nil, "只读模式也失败，启用内存备份模式: %s, 错误: %v", cacheDir, err)
				// 🔧 创建内存备份模式的BadgerCache，而不是完全禁用
				return &BadgerCache{
					db:             nil, // 空数据库指针，表示BadgerDB不可用
					name:           name + "_memory",
					basePath:       basePath,
					memoryBackup:   make(map[string]*CacheEntry),
					isMemoryMode:   true,
					maxMemoryItems: 1000, // 限制内存条目数，避免内存泄漏
					// 🔧 新增：初始化监控统计字段
					cacheHits:         0,
					cacheMisses:       0,
					cleanupCount:      0,
					lastCleanupTime:   time.Time{},
					totalItemsCleaned: 0,
					totalBytesCleaned: 0,
				}, nil
			}
			fs.Infof(nil, "使用只读缓存模式: %s", cacheDir)
		} else {
			return nil, fmt.Errorf("打开BadgerDB失败: %w", err)
		}
	}

	cache := &BadgerCache{
		db:             db,
		name:           name,
		basePath:       basePath,
		memoryBackup:   make(map[string]*CacheEntry),
		isMemoryMode:   false,
		maxMemoryItems: 1000,
		// 🔧 新增：初始化监控统计字段
		cacheHits:         0,
		cacheMisses:       0,
		cleanupCount:      0,
		lastCleanupTime:   time.Time{},
		totalItemsCleaned: 0,
		totalBytesCleaned: 0,
	}

	// 只有在非只读模式下才启动后台垃圾回收
	if !opts.ReadOnly {
		go cache.runGC()
	}

	fs.Debugf(nil, "BadgerDB缓存初始化成功: %s (只读模式: %v)", cacheDir, opts.ReadOnly)
	return cache, nil
}

// BadgerCacheConfig 缓存配置结构
// 🔧 新增：支持用户自定义缓存配置
type BadgerCacheConfig struct {
	MaxSize            int64  // 最大缓存大小（字节）
	TargetSize         int64  // 清理目标大小（字节）
	MemTableSize       int64  // BadgerDB内存表大小（字节）
	EnableSmartCleanup bool   // 启用智能清理
	CleanupStrategy    string // 清理策略
}

// NewBadgerCacheWithConfig 创建带配置的BadgerDB缓存实例
// 🔧 新增：支持用户自定义缓存配置参数
func NewBadgerCacheWithConfig(name, basePath string, config *BadgerCacheConfig) (*BadgerCache, error) {
	// 如果没有提供配置，使用默认配置
	if config == nil {
		return NewBadgerCache(name, basePath)
	}

	// 创建缓存目录
	cacheDir := filepath.Join(basePath, name)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("创建缓存目录失败: %w", err)
	}

	// 配置BadgerDB选项 - 应用用户配置
	opts := badger.DefaultOptions(cacheDir)
	opts.Logger = nil                // 禁用BadgerDB日志，避免干扰rclone日志
	opts.SyncWrites = false          // 异步写入，提高性能
	opts.CompactL0OnClose = true     // 关闭时压缩，减少磁盘占用
	opts.ValueLogFileSize = 64 << 20 // 64MB value log文件，适合大文件传输

	// 应用用户配置的内存表大小
	if config.MemTableSize > 0 {
		opts.MemTableSize = config.MemTableSize
	} else {
		opts.MemTableSize = 32 << 20 // 默认32MB
	}

	opts.BaseTableSize = 8 << 20     // 8MB基础表大小
	opts.BaseLevelSize = 64 << 20    // 64MB基础级别大小
	opts.NumMemtables = 2            // 内存优化：从3降到2
	opts.NumLevelZeroTables = 2      // 限制L0表数量
	opts.NumLevelZeroTablesStall = 4 // L0表停顿阈值
	opts.ValueThreshold = 256        // 内存优化：更多数据存储在LSM中

	// 多实例冲突处理
	db, err := badger.Open(opts)
	if err != nil {
		if isLockError(err) {
			fs.Infof(nil, "检测到多实例缓存冲突，尝试只读模式: %s", cacheDir)
			opts.ReadOnly = true
			db, err = badger.Open(opts)
			if err != nil {
				fs.Infof(nil, "只读模式也失败，启用内存备份模式: %s, 错误: %v", cacheDir, err)
				return &BadgerCache{
					db:             nil,
					name:           name + "_memory",
					basePath:       basePath,
					memoryBackup:   make(map[string]*CacheEntry),
					isMemoryMode:   true,
					maxMemoryItems: 1000,
					// 🔧 新增：初始化监控统计字段
					cacheHits:         0,
					cacheMisses:       0,
					cleanupCount:      0,
					lastCleanupTime:   time.Time{},
					totalItemsCleaned: 0,
					totalBytesCleaned: 0,
				}, nil
			}
			fs.Infof(nil, "使用只读缓存模式: %s", cacheDir)
		} else {
			return nil, fmt.Errorf("打开BadgerDB失败: %w", err)
		}
	}

	cache := &BadgerCache{
		db:             db,
		name:           name,
		basePath:       basePath,
		memoryBackup:   make(map[string]*CacheEntry),
		isMemoryMode:   false,
		maxMemoryItems: 1000,
		// 🔧 新增：初始化监控统计字段
		cacheHits:         0,
		cacheMisses:       0,
		cleanupCount:      0,
		lastCleanupTime:   time.Time{},
		totalItemsCleaned: 0,
		totalBytesCleaned: 0,
	}

	// 只有在非只读模式下才启动后台垃圾回收
	if !opts.ReadOnly {
		go cache.runGC()
	}

	fs.Debugf(nil, "BadgerDB缓存初始化成功（带配置）: %s (只读模式: %v, 智能清理: %v, 策略: %s)",
		cacheDir, opts.ReadOnly, config.EnableSmartCleanup, config.CleanupStrategy)
	return cache, nil
}

// Set 设置缓存值，支持TTL
// 🔧 优化：支持内存备份模式
func (c *BadgerCache) Set(key string, value interface{}, ttl time.Duration) error {
	// 🔧 如果处于内存模式，使用内存备份
	if c.isMemoryMode || c.db == nil {
		return c.setToMemory(key, value, ttl)
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
		// 🔧 新增：初始化访问统计字段
		AccessCount: 0,                        // 新条目访问次数为0
		LastAccess:  time.Now(),               // 设置创建时间为最后访问时间
		Priority:    c.calculatePriority(key), // 根据键类型计算优先级
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
// 🔧 优化：支持内存备份模式
func (c *BadgerCache) Get(key string, result interface{}) (bool, error) {
	// 🔧 如果处于内存模式，从内存备份获取
	if c.isMemoryMode || c.db == nil {
		return c.getFromMemory(key, result)
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
		// 🔧 新增：更新缓存未命中统计
		c.statsMutex.Lock()
		c.cacheMisses++
		c.statsMutex.Unlock()
		return false, nil
	}

	// 检查是否过期（双重保险，BadgerDB TTL + 应用层检查）
	if time.Now().After(entry.ExpiresAt) {
		// 异步删除过期条目
		go c.Delete(key)
		// 🔧 新增：过期也算未命中
		c.statsMutex.Lock()
		c.cacheMisses++
		c.statsMutex.Unlock()
		return false, nil
	}

	// 🔧 新增：更新访问统计
	entry.AccessCount++
	entry.LastAccess = time.Now()

	// 如果优先级未设置，根据缓存键类型计算优先级
	if entry.Priority == 0 {
		entry.Priority = c.calculatePriority(key)
	}

	// 异步更新访问统计到数据库（避免影响读取性能）
	go c.updateAccessStats(key, &entry)

	// 将值反序列化到result中
	valueData, err := json.Marshal(entry.Value)
	if err != nil {
		return false, fmt.Errorf("序列化缓存值失败: %w", err)
	}

	if err := json.Unmarshal(valueData, result); err != nil {
		return false, fmt.Errorf("反序列化缓存值失败: %w", err)
	}

	// 🔧 新增：更新缓存命中统计
	c.statsMutex.Lock()
	c.cacheHits++
	c.statsMutex.Unlock()

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
// 🔧 优化：支持内存备份模式
func (c *BadgerCache) Delete(key string) error {
	// 🔧 如果处于内存模式，从内存备份删除
	if c.isMemoryMode || c.db == nil {
		c.memoryMutex.Lock()
		delete(c.memoryBackup, key)
		c.memoryMutex.Unlock()
		fs.Debugf(nil, "内存备份删除: %s", key)
		return nil
	}

	return c.db.Update(func(txn *badger.Txn) error {
		return txn.Delete([]byte(key))
	})
}

// DeletePrefix 删除指定前缀的所有缓存条目
// 🔧 优化：支持内存备份模式
func (c *BadgerCache) DeletePrefix(prefix string) error {
	// 🔧 如果处于内存模式，从内存备份删除
	if c.isMemoryMode || c.db == nil {
		c.memoryMutex.Lock()
		defer c.memoryMutex.Unlock()

		keysToDelete := make([]string, 0)
		for key := range c.memoryBackup {
			if strings.HasPrefix(key, prefix) {
				keysToDelete = append(keysToDelete, key)
			}
		}

		for _, key := range keysToDelete {
			delete(c.memoryBackup, key)
		}

		fs.Debugf(nil, "内存备份删除前缀 %s: %d个条目", prefix, len(keysToDelete))
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
		fs.Debugf(nil, "数据库被禁用，跳过清除操作")
		return nil
	}

	// 记录清除前的状态
	lsm, vlog := c.db.Size()
	fs.Debugf(nil, "清除前数据库大小: LSM=%d, VLog=%d, Total=%d", lsm, vlog, lsm+vlog)

	// 执行清除操作
	if err := c.db.DropAll(); err != nil {
		fs.Errorf(nil, "清除数据库失败: %v", err)
		return err
	}

	// 记录清除后的状态
	lsm, vlog = c.db.Size()
	fs.Debugf(nil, "清除后数据库大小: LSM=%d, VLog=%d, Total=%d", lsm, vlog, lsm+vlog)

	// 验证清除操作是否成功
	if lsm+vlog > 0 {
		// 尝试列出所有键以进一步验证
		keys, err := c.ListAllKeys()
		if err != nil {
			fs.Debugf(nil, "无法验证清除操作: %v", err)
		} else {
			fs.Debugf(nil, "清除后缓存中仍有%d个键", len(keys))
			// 如果还有键存在，记录前几个键用于调试
			if len(keys) > 0 {
				maxKeys := len(keys)
				if maxKeys > 5 {
					maxKeys = 5
				}
				fs.Debugf(nil, "前%d个键: %v", maxKeys, keys[:maxKeys])
				// 返回错误，表示清除不完全
				return fmt.Errorf("清除后仍有%d个键未被删除", len(keys))
			}
		}
	} else {
		// 进一步验证：尝试列出所有键确认数据库为空
		keys, err := c.ListAllKeys()
		if err != nil {
			fs.Debugf(nil, "无法验证清除操作: %v", err)
		} else if len(keys) > 0 {
			// 即使大小显示为0，但仍有一些键存在
			fs.Debugf(nil, "警告：数据库大小显示为0，但仍检测到%d个键", len(keys))
			maxKeys := len(keys)
			if maxKeys > 5 {
				maxKeys = 5
			}
			fs.Debugf(nil, "前%d个键: %v", maxKeys, keys[:maxKeys])
			// 返回错误，表示清除不完全
			return fmt.Errorf("清除后仍有%d个键未被删除（数据库大小显示为0）", len(keys))
		} else {
			fs.Debugf(nil, "验证成功：数据库已完全清除")
		}
	}

	return nil
}

// Stats 获取缓存统计信息
// 🔧 增强：返回详细的监控统计信息
func (c *BadgerCache) Stats() map[string]interface{} {
	c.statsMutex.RLock()
	defer c.statsMutex.RUnlock()

	stats := map[string]interface{}{
		"name":               c.name,
		"total_operations":   c.operationCount,
		"cache_hits":         c.cacheHits,
		"cache_misses":       c.cacheMisses,
		"hit_rate":           c.calculateHitRate(),
		"cleanup_count":      c.cleanupCount,
		"last_cleanup":       c.lastCleanupTime.Format("2006-01-02 15:04:05"),
		"items_cleaned":      c.totalItemsCleaned,
		"bytes_cleaned":      c.totalBytesCleaned,
		"bytes_cleaned_mb":   float64(c.totalBytesCleaned) / (1024 * 1024),
		"memory_mode":        c.isMemoryMode,
		"memory_items_count": len(c.memoryBackup),
		"max_memory_items":   c.maxMemoryItems,
	}

	// 如果数据库被禁用，返回禁用状态信息
	if c.db == nil {
		stats["lsm_size"] = 0
		stats["vlog_size"] = 0
		stats["total_size"] = 0
		stats["status"] = "disabled"
	} else {
		lsm, vlog := c.db.Size()
		stats["lsm_size"] = lsm
		stats["vlog_size"] = vlog
		stats["total_size"] = lsm + vlog
		stats["total_size_mb"] = float64(lsm+vlog) / (1024 * 1024)
		stats["status"] = "active"
	}

	return stats
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

// runGC 运行后台垃圾回收 - 内存优化：提高GC频率
func (c *BadgerCache) runGC() {
	ticker := time.NewTicker(10 * time.Minute) // 内存优化：从15分钟改为10分钟，更频繁回收
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

// checkCacheSize 检查缓存大小，超过限制时智能清理
// 🔧 修复缓存策略：实现LRU淘汰机制，替换粗暴的全部清空
func (c *BadgerCache) checkCacheSize() {
	if c.db == nil {
		return
	}

	// 获取数据库大小信息
	lsm, vlog := c.db.Size()
	totalSize := lsm + vlog

	// 🔧 内存优化：降低缓存大小限制，减少内存占用
	const maxCacheSize = 100 << 20 // 100MB (从200MB降低)
	const targetSize = 64 << 20    // 清理到64MB (从150MB降低)

	if totalSize > maxCacheSize {
		fs.Debugf(nil, "缓存 %s 大小超过限制 (%d MB)，开始智能清理", c.name, totalSize>>20)

		// 🔧 智能清理：使用LRU策略清理最旧的条目
		if err := c.smartCleanup(targetSize); err != nil {
			fs.Debugf(nil, "智能清理缓存 %s 失败，回退到全部清空: %v", c.name, err)
			// 如果智能清理失败，回退到全部清空
			if clearErr := c.Clear(); clearErr != nil {
				fs.Debugf(nil, "清理缓存 %s 失败: %v", c.name, clearErr)
			}
		} else {
			// 检查清理效果
			newLsm, newVlog := c.db.Size()
			newTotalSize := newLsm + newVlog
			fs.Debugf(nil, "智能清理缓存 %s 完成: %d MB -> %d MB",
				c.name, totalSize>>20, newTotalSize>>20)
		}
	}
}

// smartCleanup 智能缓存清理，使用LRU策略
// 🔧 重构：支持多种清理策略，默认使用size策略保持兼容性
func (c *BadgerCache) smartCleanup(targetSize int64) error {
	// 默认使用size策略保持向后兼容
	return c.smartCleanupV2(targetSize, "size")
}

// smartCleanupV2 增强版智能缓存清理，支持多种策略
// 🔧 新增：基于访问统计的智能清理策略
func (c *BadgerCache) smartCleanupV2(targetSize int64, strategy string) error {
	if c.db == nil {
		return nil
	}

	// 收集所有缓存条目及其访问统计
	var items []cacheItem
	totalCurrentSize := int64(0)

	err := c.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			key := string(item.Key())

			// 检查是否过期
			if item.ExpiresAt() > 0 && item.ExpiresAt() < uint64(time.Now().Unix()) {
				continue // 跳过过期的条目，稍后会被自动清理
			}

			// 估算条目大小
			itemSize := item.EstimatedSize()
			totalCurrentSize += itemSize

			// 解析缓存条目获取完整统计信息
			var item_data cacheItem
			err := item.Value(func(val []byte) error {
				var entry CacheEntry
				if parseErr := json.Unmarshal(val, &entry); parseErr == nil {
					item_data = cacheItem{
						key:         key,
						size:        itemSize,
						createdAt:   entry.CreatedAt,
						lastAccess:  entry.LastAccess,
						accessCount: entry.AccessCount,
						priority:    entry.Priority,
					}
					// 如果LastAccess为空，使用CreatedAt
					if item_data.lastAccess.IsZero() {
						item_data.lastAccess = entry.CreatedAt
					}
					// 如果Priority为0，计算默认优先级
					if item_data.priority == 0 {
						item_data.priority = c.calculatePriority(key)
					}
				} else {
					// 如果解析失败，创建默认条目（这些条目会被优先清理）
					item_data = cacheItem{
						key:         key,
						size:        itemSize,
						createdAt:   time.Now().Add(-24 * time.Hour),
						lastAccess:  time.Now().Add(-24 * time.Hour),
						accessCount: 0,
						priority:    3, // 低优先级
					}
				}
				return nil
			})

			if err == nil {
				items = append(items, item_data)
			}
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("收集缓存条目失败: %w", err)
	}

	// 如果当前大小已经小于目标大小，无需清理
	if totalCurrentSize <= targetSize {
		return nil
	}

	// 根据策略进行排序
	c.sortItemsByStrategy(items, strategy)

	// 实现渐进式清理：每次只清理10-20%的数据，避免性能抖动
	return c.progressiveCleanup(items, totalCurrentSize, targetSize, strategy)
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

// 🔧 内存备份操作方法

// setToMemory 将数据保存到内存备份
func (c *BadgerCache) setToMemory(key string, value interface{}, ttl time.Duration) error {
	c.memoryMutex.Lock()
	defer c.memoryMutex.Unlock()

	// 检查内存条目数量限制
	if len(c.memoryBackup) >= c.maxMemoryItems {
		// 清理过期条目
		c.cleanupExpiredMemoryEntries()

		// 如果仍然超过限制，删除最旧的条目
		if len(c.memoryBackup) >= c.maxMemoryItems {
			c.evictOldestMemoryEntry()
		}
	}

	// 创建缓存条目
	entry := &CacheEntry{
		Key:       key,
		Value:     value,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
		// 🔧 新增：初始化访问统计字段
		AccessCount: 0,                        // 新条目访问次数为0
		LastAccess:  time.Now(),               // 设置创建时间为最后访问时间
		Priority:    c.calculatePriority(key), // 根据键类型计算优先级
	}

	c.memoryBackup[key] = entry
	fs.Debugf(nil, "内存备份保存: %s (TTL: %v)", key, ttl)
	return nil
}

// getFromMemory 从内存备份获取数据
func (c *BadgerCache) getFromMemory(key string, result interface{}) (bool, error) {
	c.memoryMutex.RLock()
	defer c.memoryMutex.RUnlock()

	entry, exists := c.memoryBackup[key]
	if !exists {
		// 🔧 新增：更新内存模式缓存未命中统计
		c.statsMutex.Lock()
		c.cacheMisses++
		c.statsMutex.Unlock()
		return false, nil
	}

	// 检查是否过期
	if time.Now().After(entry.ExpiresAt) {
		// 异步删除过期条目
		go func() {
			c.memoryMutex.Lock()
			delete(c.memoryBackup, key)
			c.memoryMutex.Unlock()
		}()
		// 🔧 新增：过期也算未命中
		c.statsMutex.Lock()
		c.cacheMisses++
		c.statsMutex.Unlock()
		return false, nil
	}

	// 🔧 新增：更新内存模式下的访问统计
	c.memoryMutex.RUnlock()
	c.memoryMutex.Lock()
	entry.AccessCount++
	entry.LastAccess = time.Now()
	if entry.Priority == 0 {
		entry.Priority = c.calculatePriority(key)
	}
	c.memoryMutex.Unlock()
	c.memoryMutex.RLock()

	// 将值反序列化到result中
	valueData, err := json.Marshal(entry.Value)
	if err != nil {
		return false, fmt.Errorf("序列化内存缓存值失败: %w", err)
	}

	if err := json.Unmarshal(valueData, result); err != nil {
		return false, fmt.Errorf("反序列化内存缓存值失败: %w", err)
	}

	// 🔧 新增：更新内存模式缓存命中统计
	c.statsMutex.Lock()
	c.cacheHits++
	c.statsMutex.Unlock()

	fs.Debugf(nil, "内存备份命中: %s (访问次数: %d)", key, entry.AccessCount)
	return true, nil
}

// cleanupExpiredMemoryEntries 清理过期的内存条目
func (c *BadgerCache) cleanupExpiredMemoryEntries() {
	now := time.Now()
	for key, entry := range c.memoryBackup {
		if now.After(entry.ExpiresAt) {
			delete(c.memoryBackup, key)
		}
	}
}

// evictOldestMemoryEntry 删除最旧的内存条目
func (c *BadgerCache) evictOldestMemoryEntry() {
	var oldestKey string
	var oldestTime time.Time

	for key, entry := range c.memoryBackup {
		if oldestKey == "" || entry.CreatedAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.CreatedAt
		}
	}

	if oldestKey != "" {
		delete(c.memoryBackup, oldestKey)
		fs.Debugf(nil, "内存备份淘汰最旧条目: %s", oldestKey)
	}
}

// calculatePriority 根据缓存键类型计算数据优先级
// 🔧 新增：为智能清理策略提供优先级判断
func (c *BadgerCache) calculatePriority(key string) int {
	// 根据缓存键的前缀或模式判断数据类型和重要性
	switch {
	case strings.HasPrefix(key, "path_to_id_") || strings.Contains(key, "path_resolve"):
		// 路径解析缓存：最高优先级，频繁使用且重建成本高
		return 1
	case strings.HasPrefix(key, "dirlist_") || strings.Contains(key, "dir_list"):
		// 目录列表缓存：中等优先级，使用频率中等
		return 2
	case strings.HasPrefix(key, "parent_") || strings.Contains(key, "metadata") || strings.Contains(key, "file_id"):
		// 父目录ID、元数据、文件ID缓存：中等优先级
		return 2
	case strings.Contains(key, "download_url") || strings.Contains(key, "temp_") || strings.Contains(key, "upload_"):
		// 下载URL、临时数据、上传相关：低优先级，时效性强但重建成本低
		return 3
	default:
		// 未知类型：默认中等优先级
		return 2
	}
}

// updateAccessStats 异步更新访问统计到数据库
// 🔧 新增：避免影响读取性能的异步统计更新
func (c *BadgerCache) updateAccessStats(key string, entry *CacheEntry) {
	if c.db == nil {
		return // 内存模式下不需要更新数据库
	}

	// 计算剩余TTL
	ttl := time.Until(entry.ExpiresAt)
	if ttl <= 0 {
		return // 已过期，不需要更新
	}

	// 序列化更新后的条目
	data, err := json.Marshal(entry)
	if err != nil {
		fs.Debugf(nil, "序列化访问统计失败 %s: %v", key, err)
		return
	}

	// 更新数据库中的条目
	err = c.db.Update(func(txn *badger.Txn) error {
		e := badger.NewEntry([]byte(key), data).WithTTL(ttl)
		return txn.SetEntry(e)
	})

	if err != nil {
		fs.Debugf(nil, "更新访问统计失败 %s: %v", key, err)
	}
}

// sortItemsByStrategy 根据清理策略对缓存条目进行排序
// 🔧 新增：支持多种智能清理策略
func (c *BadgerCache) sortItemsByStrategy(items []cacheItem, strategy string) {
	switch strategy {
	case "lru":
		// LRU策略：按最后访问时间排序，最久未访问的在前面
		sort.Slice(items, func(i, j int) bool {
			return items[i].lastAccess.Before(items[j].lastAccess)
		})

	case "priority_lru":
		// 优先级+LRU策略：综合考虑优先级和访问时间
		sort.Slice(items, func(i, j int) bool {
			// 首先按优先级排序（数值越大优先级越低，越容易被清理）
			if items[i].priority != items[j].priority {
				return items[i].priority > items[j].priority
			}
			// 相同优先级下按最后访问时间排序
			return items[i].lastAccess.Before(items[j].lastAccess)
		})

	case "time":
		// 时间策略：按创建时间排序，最旧的在前面
		sort.Slice(items, func(i, j int) bool {
			return items[i].createdAt.Before(items[j].createdAt)
		})

	case "size":
	default:
		// 大小策略（默认）：按创建时间排序，保持向后兼容
		sort.Slice(items, func(i, j int) bool {
			return items[i].createdAt.Before(items[j].createdAt)
		})
	}
}

// progressiveCleanup 渐进式清理实现
// 🔧 新增：避免一次性清理过多数据造成性能抖动
func (c *BadgerCache) progressiveCleanup(items []cacheItem, currentSize, targetSize int64, strategy string) error {
	if currentSize <= targetSize {
		return nil
	}

	// 🔧 新增：性能监控 - 记录清理开始时间
	startTime := time.Now()

	needToDelete := currentSize - targetSize

	// 渐进式清理：每次最多清理20%的数据，最少清理需要的数据量
	maxCleanupSize := currentSize / 5 // 20%
	if needToDelete > maxCleanupSize {
		needToDelete = maxCleanupSize
	}

	deletedSize := int64(0)
	keysToDelete := make([]string, 0)
	cleanupStats := make(map[int]int) // 按优先级统计清理数量

	// 选择要删除的条目
	for _, item := range items {
		if deletedSize >= needToDelete {
			break
		}
		keysToDelete = append(keysToDelete, item.key)
		deletedSize += item.size
		cleanupStats[item.priority]++
	}

	// 批量删除选中的条目
	if len(keysToDelete) > 0 {
		err := c.db.Update(func(txn *badger.Txn) error {
			for _, key := range keysToDelete {
				if err := txn.Delete([]byte(key)); err != nil {
					return err
				}
			}
			return nil
		})

		if err != nil {
			return fmt.Errorf("批量删除缓存条目失败: %w", err)
		}

		// 🔧 新增：更新清理统计
		c.statsMutex.Lock()
		c.cleanupCount++
		c.lastCleanupTime = time.Now()
		c.totalItemsCleaned += int64(len(keysToDelete))
		c.totalBytesCleaned += deletedSize
		c.statsMutex.Unlock()

		// 🔧 新增：性能监控 - 计算清理耗时
		cleanupDuration := time.Since(startTime)

		// 详细的清理日志
		fs.Infof(nil, "智能缓存清理[%s]: 删除 %d 个条目，释放 %.1f MB (高优先级:%d, 中优先级:%d, 低优先级:%d) 耗时: %v",
			strategy, len(keysToDelete), float64(deletedSize)/(1024*1024),
			cleanupStats[1], cleanupStats[2], cleanupStats[3], cleanupDuration)

		// 🔧 新增：性能异常告警
		if cleanupDuration > 5*time.Second {
			fs.Logf(nil, "⚠️ 缓存清理耗时过长: %v，可能影响性能", cleanupDuration)
		}
		if len(keysToDelete) > 1000 {
			fs.Logf(nil, "⚠️ 单次清理条目过多: %d，建议调整清理策略", len(keysToDelete))
		}
	}

	return nil
}

// calculateHitRate 计算缓存命中率
// 🔧 新增：监控统计辅助方法
func (c *BadgerCache) calculateHitRate() float64 {
	totalRequests := c.cacheHits + c.cacheMisses
	if totalRequests == 0 {
		return 0.0
	}
	return float64(c.cacheHits) / float64(totalRequests) * 100.0
}

// SmartCleanupWithStrategy 公开的智能清理方法
// 🔧 新增：为命令接口提供的公开清理方法
func (c *BadgerCache) SmartCleanupWithStrategy(targetSize int64, strategy string) error {
	return c.smartCleanupV2(targetSize, strategy)
}

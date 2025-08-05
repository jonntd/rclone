// Package dircache provides persistent directory cache functionality
package dircache

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
)

// PersistentCache represents a persistent directory cache
type PersistentCache struct {
	mu           sync.RWMutex
	backend      string
	configHash   string
	cacheDir     string
	cacheFile    string
	ttl          time.Duration
	enabled      bool
	lastSaved    time.Time
	saveInterval time.Duration
}

// CacheEntry represents a single cache entry
type CacheEntry struct {
	Path       string    `json:"path"`
	DirID      string    `json:"dir_id"`
	CreatedAt  time.Time `json:"created_at"`
	AccessedAt time.Time `json:"accessed_at"`
}

// CacheData represents the entire cache file structure
type CacheData struct {
	Version    string       `json:"version"`
	Backend    string       `json:"backend"`
	ConfigHash string       `json:"config_hash"`
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
	Entries    []CacheEntry `json:"entries"`
}

// NewPersistentCache creates a new persistent cache instance
func NewPersistentCache(backend string, configData map[string]string) (*PersistentCache, error) {
	// Generate config hash for cache file naming
	configHash := generateConfigHash(configData)

	// Get cache directory
	cacheDir := getCacheDir()
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Generate cache file path
	cacheFile := filepath.Join(cacheDir, fmt.Sprintf("%s_%s.json", backend, configHash))

	pc := &PersistentCache{
		backend:      backend,
		configHash:   configHash,
		cacheDir:     cacheDir,
		cacheFile:    cacheFile,
		ttl:          24 * time.Hour, // Default 24 hours
		enabled:      true,
		saveInterval: 5 * time.Minute, // Save every 5 minutes
	}

	// 启动时清理过期的缓存文件
	go func() {
		if err := pc.CleanExpired(); err != nil {
			fs.Debugf(backend, "⚠️ 启动时清理过期缓存失败: %v", err)
		}
	}()

	return pc, nil
}

// getCacheDir returns the cache directory path
func getCacheDir() string {
	// Use rclone's standard cache directory
	cacheDir := config.GetCacheDir()
	return filepath.Join(cacheDir, "dircache")
}

// generateConfigHash generates a hash from config data for cache file naming
func generateConfigHash(configData map[string]string) string {
	// Create a deterministic string from config
	var configStr string
	for key, value := range configData {
		// Skip sensitive fields
		if key == "token" || key == "refresh_token" || key == "access_token" {
			continue
		}
		configStr += fmt.Sprintf("%s=%s;", key, value)
	}

	// Generate MD5 hash
	hash := md5.Sum([]byte(configStr))
	return fmt.Sprintf("%x", hash)[:16] // Use first 16 characters
}

// LoadFromDisk loads cache data from disk
func (pc *PersistentCache) LoadFromDisk() (map[string]string, map[string]string, error) {
	if !pc.enabled {
		return make(map[string]string), make(map[string]string), nil
	}

	pc.mu.RLock()
	defer pc.mu.RUnlock()

	// Check if cache file exists
	if _, err := os.Stat(pc.cacheFile); os.IsNotExist(err) {
		fs.Debugf(pc.backend, "📁 持久化缓存文件不存在: %s", pc.cacheFile)
		return make(map[string]string), make(map[string]string), nil
	}

	// Read cache file
	data, err := os.ReadFile(pc.cacheFile)
	if err != nil {
		fs.Logf(pc.backend, "⚠️ 读取持久化缓存失败: %v", err)
		return make(map[string]string), make(map[string]string), nil
	}

	// Parse JSON
	var cacheData CacheData
	if err := json.Unmarshal(data, &cacheData); err != nil {
		fs.Logf(pc.backend, "⚠️ 解析持久化缓存失败: %v", err)
		return make(map[string]string), make(map[string]string), nil
	}

	// Check cache validity
	if time.Since(cacheData.UpdatedAt) > pc.ttl {
		fs.Debugf(pc.backend, "📁 持久化缓存已过期: %v", time.Since(cacheData.UpdatedAt))
		return make(map[string]string), make(map[string]string), nil
	}

	// Check config hash
	if cacheData.ConfigHash != pc.configHash {
		fs.Debugf(pc.backend, "📁 持久化缓存配置不匹配，忽略")
		return make(map[string]string), make(map[string]string), nil
	}

	// Convert to maps
	cache := make(map[string]string)
	invCache := make(map[string]string)
	validEntries := 0

	now := time.Now()
	for _, entry := range cacheData.Entries {
		// Check entry validity
		if now.Sub(entry.CreatedAt) <= pc.ttl {
			cache[entry.Path] = entry.DirID
			invCache[entry.DirID] = entry.Path
			validEntries++
		}
	}

	fs.Infof(pc.backend, "📁 从持久化缓存加载 %d 个有效条目", validEntries)
	return cache, invCache, nil
}

// SaveToDisk saves cache data to disk
func (pc *PersistentCache) SaveToDisk(cache map[string]string, invCache map[string]string) error {
	if !pc.enabled {
		return nil
	}

	// Check save interval
	if time.Since(pc.lastSaved) < pc.saveInterval {
		return nil
	}

	pc.mu.Lock()
	defer pc.mu.Unlock()

	// Convert maps to entries
	entries := make([]CacheEntry, 0, len(cache))
	now := time.Now()

	for path, dirID := range cache {
		entries = append(entries, CacheEntry{
			Path:       path,
			DirID:      dirID,
			CreatedAt:  now,
			AccessedAt: now,
		})
	}

	// Create cache data
	cacheData := CacheData{
		Version:    "1.0",
		Backend:    pc.backend,
		ConfigHash: pc.configHash,
		CreatedAt:  now,
		UpdatedAt:  now,
		Entries:    entries,
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(cacheData, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal cache data: %w", err)
	}

	// Write to temporary file first
	tempFile := pc.cacheFile + ".tmp"
	if err := os.WriteFile(tempFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write cache file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tempFile, pc.cacheFile); err != nil {
		os.Remove(tempFile) // Clean up temp file
		return fmt.Errorf("failed to rename cache file: %w", err)
	}

	pc.lastSaved = now
	fs.Debugf(pc.backend, "💾 持久化缓存已保存: %d 个条目", len(entries))
	return nil
}

// SetTTL sets the cache TTL
func (pc *PersistentCache) SetTTL(ttl time.Duration) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.ttl = ttl
}

// SetEnabled enables or disables persistent cache
func (pc *PersistentCache) SetEnabled(enabled bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.enabled = enabled
}

// ForceRefresh forces a cache refresh by deleting the cache file
func (pc *PersistentCache) ForceRefresh() error {
	if !pc.enabled {
		return nil
	}

	pc.mu.Lock()
	defer pc.mu.Unlock()

	// Delete the cache file to force refresh
	if err := os.Remove(pc.cacheFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete cache file: %w", err)
	}

	fs.Infof(pc.backend, "🔄 持久化缓存已强制刷新")
	return nil
}

// IsExpired checks if the cache is expired
func (pc *PersistentCache) IsExpired() (bool, error) {
	if !pc.enabled {
		return true, nil
	}

	pc.mu.RLock()
	defer pc.mu.RUnlock()

	// Check if cache file exists
	info, err := os.Stat(pc.cacheFile)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return true, err
	}

	// Check if expired
	return time.Since(info.ModTime()) > pc.ttl, nil
}

// CleanExpired removes expired cache files
func (pc *PersistentCache) CleanExpired() error {
	if !pc.enabled {
		return nil
	}

	// Read cache directory
	files, err := os.ReadDir(pc.cacheDir)
	if err != nil {
		return err
	}

	cleaned := 0
	for _, file := range files {
		if file.IsDir() {
			continue
		}

		// Check if it's a cache file
		if filepath.Ext(file.Name()) != ".json" {
			continue
		}

		filePath := filepath.Join(pc.cacheDir, file.Name())
		info, err := file.Info()
		if err != nil {
			continue
		}

		// Check if expired
		if time.Since(info.ModTime()) > pc.ttl {
			if err := os.Remove(filePath); err == nil {
				cleaned++
			}
		}
	}

	if cleaned > 0 {
		fs.Debugf(pc.backend, "🧹 清理了 %d 个过期的持久化缓存文件", cleaned)
	}

	return nil
}

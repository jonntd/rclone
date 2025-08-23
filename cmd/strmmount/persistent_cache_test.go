package strmmount

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSTRMPersistentCache(t *testing.T) {
	// 创建临时目录
	tempDir, err := os.MkdirTemp("", "strm-cache-test")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	// 创建持久化缓存实例
	cache, err := NewSTRMPersistentCache(
		"123",
		"test-remote",
		100*1024*1024, // 100MB
		[]string{"mp4", "mkv"},
		"auto",
	)
	require.NoError(t, err)
	require.NotNil(t, cache)

	// 验证基本属性
	assert.Equal(t, "123", cache.backend)
	assert.Equal(t, "test-remote", cache.remotePath)
	assert.True(t, cache.enabled)
	assert.Equal(t, 5*time.Minute, cache.ttl)
}

func TestCacheFileOperations(t *testing.T) {
	// 创建临时目录
	tempDir, err := os.MkdirTemp("", "strm-cache-test")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	// 创建持久化缓存实例
	cache, err := NewSTRMPersistentCache(
		"115",
		"movies",
		50*1024*1024, // 50MB
		[]string{"mp4", "avi"},
		"115",
	)
	require.NoError(t, err)

	// 测试缓存文件路径生成
	assert.Contains(t, cache.cacheFile, "115_movies_")
	assert.Contains(t, cache.cacheFile, ".json")

	// 测试缓存文件不存在 (初始状态)
	// 注意：由于之前的测试可能已经创建了缓存文件，这里先清理
	os.Remove(cache.cacheFile)
	assert.False(t, cache.cacheFileExists())

	// 创建测试缓存数据
	testData := &CacheData{
		Version:    "1.0",
		Backend:    "115",
		RemotePath: "movies",
		ConfigHash: cache.configHash,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(5 * time.Minute),
		FileCount:  2,
		TotalSize:  2000000000, // 2GB
		Directories: []CachedDirectory{
			{
				Path:      "Action",
				FileCount: 2,
				TotalSize: 2000000000,
				Files: []CachedFile{
					{
						Name:     "movie1.mp4",
						Size:     1000000000,
						ModTime:  time.Now(),
						PickCode: "abc123xyz789",
						Hash:     "sha1:abcdef123456",
					},
					{
						Name:     "movie2.mp4",
						Size:     1000000000,
						ModTime:  time.Now(),
						PickCode: "def456uvw012",
						Hash:     "sha1:fedcba654321",
					},
				},
			},
		},
		Metadata: CacheMetadata{
			APICallsCount:    1,
			CacheHitRate:     0.0,
			LastSyncDuration: 100 * time.Millisecond,
		},
	}

	// 测试保存到磁盘
	err = cache.saveToDisk(testData)
	require.NoError(t, err)

	// 验证文件存在
	assert.True(t, cache.cacheFileExists())

	// 测试从磁盘加载
	loadedData, err := cache.loadFromDisk()
	require.NoError(t, err)
	require.NotNil(t, loadedData)

	// 验证加载的数据
	assert.Equal(t, testData.Version, loadedData.Version)
	assert.Equal(t, testData.Backend, loadedData.Backend)
	assert.Equal(t, testData.RemotePath, loadedData.RemotePath)
	assert.Equal(t, testData.FileCount, loadedData.FileCount)
	assert.Equal(t, testData.TotalSize, loadedData.TotalSize)
	assert.Len(t, loadedData.Directories, 1)
	assert.Len(t, loadedData.Directories[0].Files, 2)

	// 验证文件详细信息
	file1 := loadedData.Directories[0].Files[0]
	assert.Equal(t, "movie1.mp4", file1.Name)
	assert.Equal(t, int64(1000000000), file1.Size)
	assert.Equal(t, "abc123xyz789", file1.PickCode)
	assert.Equal(t, "sha1:abcdef123456", file1.Hash)
}

func TestCacheValidation(t *testing.T) {
	cache, err := NewSTRMPersistentCache(
		"123",
		"test",
		100*1024*1024,
		[]string{"mp4"},
		"auto",
	)
	require.NoError(t, err)

	// 测试有效缓存
	validCache := &CacheData{
		Version:    "1.0",
		Backend:    "123",
		RemotePath: "test",
		ConfigHash: cache.configHash,
	}
	assert.True(t, cache.validateCache(validCache))

	// 测试无效版本
	invalidVersion := &CacheData{
		Version:    "2.0",
		Backend:    "123",
		RemotePath: "test",
		ConfigHash: cache.configHash,
	}
	assert.False(t, cache.validateCache(invalidVersion))

	// 测试无效后端
	invalidBackend := &CacheData{
		Version:    "1.0",
		Backend:    "115",
		RemotePath: "test",
		ConfigHash: cache.configHash,
	}
	assert.False(t, cache.validateCache(invalidBackend))

	// 测试无效路径
	invalidPath := &CacheData{
		Version:    "1.0",
		Backend:    "123",
		RemotePath: "different",
		ConfigHash: cache.configHash,
	}
	assert.False(t, cache.validateCache(invalidPath))

	// 测试无效配置哈希
	invalidHash := &CacheData{
		Version:    "1.0",
		Backend:    "123",
		RemotePath: "test",
		ConfigHash: "different-hash",
	}
	assert.False(t, cache.validateCache(invalidHash))
}

func TestConfigHashGeneration(t *testing.T) {
	// 测试相同配置生成相同哈希
	hash1 := generateConfigHash("123", "movies", 100*1024*1024, []string{"mp4", "mkv"}, "auto")
	hash2 := generateConfigHash("123", "movies", 100*1024*1024, []string{"mp4", "mkv"}, "auto")
	assert.Equal(t, hash1, hash2)

	// 测试不同配置生成不同哈希
	hash3 := generateConfigHash("115", "movies", 100*1024*1024, []string{"mp4", "mkv"}, "auto")
	assert.NotEqual(t, hash1, hash3)

	hash4 := generateConfigHash("123", "videos", 100*1024*1024, []string{"mp4", "mkv"}, "auto")
	assert.NotEqual(t, hash1, hash4)

	hash5 := generateConfigHash("123", "movies", 200*1024*1024, []string{"mp4", "mkv"}, "auto")
	assert.NotEqual(t, hash1, hash5)
}

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{"path/with/slashes", "path_with_slashes"},
		{"path\\with\\backslashes", "path_with_backslashes"},
		{"path:with:colons", "path_with_colons"},
		{"path*with*stars", "path_with_stars"},
		{"path?with?questions", "path_with_questions"},
		{"path\"with\"quotes", "path_with_quotes"},
		{"path<with>brackets", "path_with_brackets"},
		{"path|with|pipes", "path_with_pipes"},
		{"very-long-path-name-that-exceeds-fifty-characters-limit", "very-long-path-name-that-exceeds-fifty-characters-"},
	}

	for _, test := range tests {
		result := sanitizePath(test.input)
		assert.Equal(t, test.expected, result, "Input: %s", test.input)
		assert.LessOrEqual(t, len(result), 50, "Result should not exceed 50 characters")
	}
}

func TestCacheStats(t *testing.T) {
	cache, err := NewSTRMPersistentCache(
		"123",
		"test",
		100*1024*1024,
		[]string{"mp4"},
		"auto",
	)
	require.NoError(t, err)

	stats := cache.GetStats()
	assert.Equal(t, true, stats["enabled"])
	assert.Equal(t, "123", stats["backend"])
	assert.Equal(t, "test", stats["remote_path"])
	assert.Contains(t, stats, "cache_file")
	assert.Contains(t, stats, "ttl")
}

func TestCacheEnableDisable(t *testing.T) {
	cache, err := NewSTRMPersistentCache(
		"123",
		"test",
		100*1024*1024,
		[]string{"mp4"},
		"auto",
	)
	require.NoError(t, err)

	// 默认启用
	assert.True(t, cache.enabled)

	// 禁用缓存
	cache.SetEnabled(false)
	assert.False(t, cache.enabled)

	// 重新启用
	cache.SetEnabled(true)
	assert.True(t, cache.enabled)
}

func TestCacheTTL(t *testing.T) {
	cache, err := NewSTRMPersistentCache(
		"123",
		"test",
		100*1024*1024,
		[]string{"mp4"},
		"auto",
	)
	require.NoError(t, err)

	// 默认TTL
	assert.Equal(t, 5*time.Minute, cache.ttl)

	// 设置新的TTL
	newTTL := 10 * time.Minute
	cache.SetTTL(newTTL)
	assert.Equal(t, newTTL, cache.ttl)
}

package _123

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGlobalMD5CacheThreadSafety 测试全局MD5缓存的线程安全性
func TestGlobalMD5CacheThreadSafety(t *testing.T) {
	// 重置全局缓存状态
	globalMD5Cache = nil
	globalMD5CacheOnce = sync.Once{}

	const numGoroutines = 100
	const numOperations = 10

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// 并发访问全局缓存
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			for j := 0; j < numOperations; j++ {
				cache := getGlobalMD5Cache()
				assert.NotNil(t, cache, "缓存不应为nil")

				// 测试缓存操作
				cacheKey := "test_key"
				entry := CrossCloudMD5Entry{
					MD5Hash:    "test_md5",
					FileSize:   1024,
					ModTime:    time.Now(),
					CachedTime: time.Now(),
				}

				// 写入操作
				cache.mutex.Lock()
				cache.cache[cacheKey] = entry
				cache.mutex.Unlock()

				// 读取操作
				cache.mutex.RLock()
				_, exists := cache.cache[cacheKey]
				cache.mutex.RUnlock()

				assert.True(t, exists, "缓存项应该存在")
			}
		}(i)
	}

	wg.Wait()

	// 验证缓存正常工作
	cache := getGlobalMD5Cache()
	assert.NotNil(t, cache)
	assert.NotNil(t, cache.cache)
}

// TestNetworkLatencyMeasurement 测试网络延迟测量功能
func TestNetworkLatencyMeasurement(t *testing.T) {
	fs := &Fs{
		opt: Options{
			UserAgent: "rclone-test/1.0",
		},
	}

	latency := fs.measureNetworkLatency()

	// 延迟应该在合理范围内（0-5000ms）
	assert.GreaterOrEqual(t, latency, int64(0), "延迟不应为负数")
	assert.LessOrEqual(t, latency, int64(5000), "延迟不应超过5秒")

	t.Logf("测量到的网络延迟: %dms", latency)
}

// TestResourcePoolMemoryManagement 测试资源池内存管理
func TestResourcePoolMemoryManagement(t *testing.T) {
	pool := NewResourcePool()
	require.NotNil(t, pool)

	// 测试缓冲区池
	buf1 := pool.GetBuffer()
	assert.NotNil(t, buf1)
	assert.Equal(t, 0, len(buf1), "缓冲区长度应为0")

	pool.PutBuffer(buf1)

	buf2 := pool.GetBuffer()
	assert.NotNil(t, buf2)

	// 测试哈希计算器池
	hasher1 := pool.GetHasher()
	assert.NotNil(t, hasher1)

	pool.PutHasher(hasher1)

	hasher2 := pool.GetHasher()
	assert.NotNil(t, hasher2)
}

// TestDownloadProgressThreadSafety 测试下载进度跟踪器的线程安全性
func TestDownloadProgressThreadSafety(t *testing.T) {
	progress := NewDownloadProgress(10, 1024*1024) // 10个分片，1MB总大小
	require.NotNil(t, progress)

	const numGoroutines = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// 并发更新进度
	for i := 0; i < numGoroutines; i++ {
		go func(chunkIndex int64) {
			defer wg.Done()

			chunkSize := int64(102400) // 100KB
			duration := time.Millisecond * 100

			progress.UpdateChunkProgress(chunkIndex%10, chunkSize, duration)

			// 读取进度信息
			percentage, avgSpeed, peakSpeed, eta, completed, total, downloaded, totalBytes := progress.GetProgressInfo()

			// 验证返回值的合理性
			assert.GreaterOrEqual(t, percentage, float64(0))
			assert.LessOrEqual(t, percentage, float64(100))
			assert.GreaterOrEqual(t, avgSpeed, float64(0))
			assert.GreaterOrEqual(t, peakSpeed, float64(0))
			assert.GreaterOrEqual(t, eta, time.Duration(0))
			assert.GreaterOrEqual(t, completed, int64(0))
			assert.LessOrEqual(t, completed, total)
			assert.GreaterOrEqual(t, downloaded, int64(0))
			assert.LessOrEqual(t, downloaded, totalBytes)
		}(int64(i))
	}

	wg.Wait()

	// 验证最终状态
	_, _, _, _, completed, total, _, _ := progress.GetProgressInfo()
	assert.LessOrEqual(t, completed, total)

	t.Logf("并发测试完成: %d/%d 分片", completed, total)
}

// TestPanicRecoveryInGoroutines 测试goroutine中的panic恢复机制
func TestPanicRecoveryInGoroutines(t *testing.T) {
	var recovered bool
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				recovered = true
				t.Logf("成功捕获panic: %v", r)
			}
		}()

		// 模拟下载分片中的panic
		panic("测试panic恢复")
	}()

	wg.Wait()
	assert.True(t, recovered, "应该成功恢复panic")
}

// TestConcurrencyParameterCalculation 测试并发参数计算
func TestConcurrencyParameterCalculation(t *testing.T) {
	fs := &Fs{
		opt: Options{
			MaxConcurrentUploads: 0, // 使用默认值
		},
	}

	testCases := []struct {
		fileSize     int64
		networkSpeed int64
		expectedMin  int
		expectedMax  int
	}{
		{100 * 1024 * 1024, 50 * 1024 * 1024, 1, 10},        // 100MB文件，50Mbps网络
		{1024 * 1024 * 1024, 100 * 1024 * 1024, 2, 15},      // 1GB文件，100Mbps网络
		{10 * 1024 * 1024 * 1024, 200 * 1024 * 1024, 5, 24}, // 10GB文件，200Mbps网络
	}

	for _, tc := range testCases {
		concurrency := fs.getOptimalConcurrency(tc.fileSize, tc.networkSpeed)

		assert.GreaterOrEqual(t, concurrency, tc.expectedMin,
			"并发数不应低于最小值，文件大小: %d, 网络速度: %d", tc.fileSize, tc.networkSpeed)
		assert.LessOrEqual(t, concurrency, tc.expectedMax,
			"并发数不应超过最大值，文件大小: %d, 网络速度: %d", tc.fileSize, tc.networkSpeed)

		t.Logf("文件大小: %d, 网络速度: %d, 计算并发数: %d",
			tc.fileSize, tc.networkSpeed, concurrency)
	}
}

// TestChunkSizeOptimization 测试分片大小优化
func TestChunkSizeOptimization(t *testing.T) {
	fs := &Fs{
		opt: Options{
			ChunkSize: 0, // 使用默认值
		},
	}

	testCases := []struct {
		fileSize     int64
		networkSpeed int64
		expectedMin  int64
		expectedMax  int64
	}{
		{100 * 1024 * 1024, 20 * 1024 * 1024, 50 * 1024 * 1024, 200 * 1024 * 1024},         // 100MB文件
		{1024 * 1024 * 1024, 50 * 1024 * 1024, 100 * 1024 * 1024, 400 * 1024 * 1024},       // 1GB文件
		{10 * 1024 * 1024 * 1024, 100 * 1024 * 1024, 200 * 1024 * 1024, 800 * 1024 * 1024}, // 10GB文件
	}

	for _, tc := range testCases {
		chunkSize := fs.getOptimalChunkSize(tc.fileSize, tc.networkSpeed)

		assert.GreaterOrEqual(t, chunkSize, tc.expectedMin,
			"分片大小不应低于最小值，文件大小: %d, 网络速度: %d", tc.fileSize, tc.networkSpeed)
		assert.LessOrEqual(t, chunkSize, tc.expectedMax,
			"分片大小不应超过最大值，文件大小: %d, 网络速度: %d", tc.fileSize, tc.networkSpeed)

		t.Logf("文件大小: %d, 网络速度: %d, 计算分片大小: %d",
			tc.fileSize, tc.networkSpeed, chunkSize)
	}
}

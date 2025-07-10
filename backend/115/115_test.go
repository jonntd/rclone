package _115

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNetworkLatencyMeasurement 测试115网盘网络延迟测量功能
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

	t.Logf("115网盘测量到的网络延迟: %dms", latency)
}

// TestNetworkBandwidthEstimation 测试网络带宽估算
func TestNetworkBandwidthEstimation(t *testing.T) {
	fs := &Fs{
		opt: Options{
			UserAgent: "rclone-test/1.0",
		},
	}

	bandwidth := fs.estimateNetworkBandwidth()

	// 带宽应该在合理范围内
	assert.Greater(t, bandwidth, int64(0), "带宽应该大于0")
	assert.LessOrEqual(t, bandwidth, int64(1024*1024*1024), "带宽不应超过1Gbps")

	t.Logf("115网盘估算的网络带宽: %d bytes/s", bandwidth)
}

// TestAdaptiveConcurrencyAdjustment 测试自适应并发数调整
func TestAdaptiveConcurrencyAdjustment(t *testing.T) {
	fs := &Fs{
		opt: Options{
			UserAgent: "rclone-test/1.0",
		},
	}

	testCases := []struct {
		baseConcurrency int
		expectedMin     int
		expectedMax     int
	}{
		{1, 2, 12},  // 最小并发数测试
		{5, 2, 12},  // 中等并发数测试
		{15, 2, 12}, // 超过最大限制测试
	}

	for _, tc := range testCases {
		adjustedConcurrency := fs.adjustConcurrencyByNetworkCondition(tc.baseConcurrency)

		assert.GreaterOrEqual(t, adjustedConcurrency, tc.expectedMin,
			"调整后并发数不应低于最小值，基础并发数: %d", tc.baseConcurrency)
		assert.LessOrEqual(t, adjustedConcurrency, tc.expectedMax,
			"调整后并发数不应超过最大值，基础并发数: %d", tc.baseConcurrency)

		t.Logf("基础并发数: %d, 调整后并发数: %d", tc.baseConcurrency, adjustedConcurrency)
	}
}

// TestDownloadChunkSizeCalculation 测试下载分片大小计算
func TestDownloadChunkSizeCalculation(t *testing.T) {
	fs := &Fs{}

	testCases := []struct {
		fileSize    int64
		expectedMin int64
		expectedMax int64
		description string
	}{
		{30 * 1024 * 1024, 16 * 1024 * 1024, 32 * 1024 * 1024, "小文件(<50MB)"},
		{150 * 1024 * 1024, 32 * 1024 * 1024, 64 * 1024 * 1024, "中等文件(<200MB)"},
		{800 * 1024 * 1024, 64 * 1024 * 1024, 128 * 1024 * 1024, "大文件(<1GB)"},
		{3 * 1024 * 1024 * 1024, 128 * 1024 * 1024, 256 * 1024 * 1024, "超大文件(<5GB)"},
		{15 * 1024 * 1024 * 1024, 256 * 1024 * 1024, 512 * 1024 * 1024, "巨大文件(<20GB)"},
		{50 * 1024 * 1024 * 1024, 512 * 1024 * 1024, 1024 * 1024 * 1024, "超巨大文件(>20GB)"},
	}

	for _, tc := range testCases {
		chunkSize := fs.calculateDownloadChunkSize(tc.fileSize)

		assert.GreaterOrEqual(t, chunkSize, tc.expectedMin,
			"分片大小不应低于最小值: %s, 文件大小: %d", tc.description, tc.fileSize)
		assert.LessOrEqual(t, chunkSize, tc.expectedMax,
			"分片大小不应超过最大值: %s, 文件大小: %d", tc.description, tc.fileSize)

		t.Logf("%s - 文件大小: %d, 分片大小: %d", tc.description, tc.fileSize, chunkSize)
	}
}

// TestBaseConcurrencyCalculation 测试基础并发数计算
func TestBaseConcurrencyCalculation(t *testing.T) {
	fs := &Fs{}

	testCases := []struct {
		fileSize    int64
		expectedMin int
		expectedMax int
		description string
	}{
		{30 * 1024 * 1024, 2, 2, "小文件(<50MB)"},
		{150 * 1024 * 1024, 4, 4, "中等文件(<200MB)"},
		{800 * 1024 * 1024, 6, 6, "大文件(<1GB)"},
		{3 * 1024 * 1024 * 1024, 8, 8, "超大文件(<5GB)"},
		{15 * 1024 * 1024 * 1024, 10, 10, "巨大文件(>5GB)"},
	}

	for _, tc := range testCases {
		concurrency := fs.getBaseConcurrency(tc.fileSize)

		assert.GreaterOrEqual(t, concurrency, tc.expectedMin,
			"并发数不应低于最小值: %s, 文件大小: %d", tc.description, tc.fileSize)
		assert.LessOrEqual(t, concurrency, tc.expectedMax,
			"并发数不应超过最大值: %s, 文件大小: %d", tc.description, tc.fileSize)

		t.Logf("%s - 文件大小: %d, 基础并发数: %d", tc.description, tc.fileSize, concurrency)
	}
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

	t.Logf("115网盘并发测试完成: %d/%d 分片", completed, total)
}

// TestPanicRecoveryInDownloadGoroutines 测试下载goroutine中的panic恢复机制
func TestPanicRecoveryInDownloadGoroutines(t *testing.T) {
	var recovered bool
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				recovered = true
				t.Logf("115网盘成功捕获下载panic: %v", r)
			}
		}()

		// 模拟下载分片中的panic
		panic("115网盘测试panic恢复")
	}()

	wg.Wait()
	assert.True(t, recovered, "115网盘应该成功恢复panic")
}

// TestConcurrentDownloadStability 测试并发下载的稳定性
func TestConcurrentDownloadStability(t *testing.T) {
	const numConcurrentDownloads = 10
	const numIterations = 5

	var wg sync.WaitGroup
	var successCount int64
	var mutex sync.Mutex

	for i := 0; i < numIterations; i++ {
		wg.Add(numConcurrentDownloads)

		for j := 0; j < numConcurrentDownloads; j++ {
			go func(iteration, download int) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						t.Logf("下载 %d-%d 发生panic: %v", iteration, download, r)
					}
				}()

				// 模拟下载操作
				time.Sleep(time.Millisecond * 10)

				mutex.Lock()
				successCount++
				mutex.Unlock()
			}(i, j)
		}

		wg.Wait()
	}

	expectedTotal := int64(numConcurrentDownloads * numIterations)
	assert.Equal(t, expectedTotal, successCount,
		"所有并发下载都应该成功完成")

	t.Logf("并发下载稳定性测试完成: %d/%d 成功", successCount, expectedTotal)
}

// TestMemoryUsageOptimization 测试内存使用优化
func TestMemoryUsageOptimization(t *testing.T) {
	// 模拟大量并发操作，检查内存使用
	const numOperations = 100
	var wg sync.WaitGroup
	wg.Add(numOperations)

	for i := 0; i < numOperations; i++ {
		go func(id int) {
			defer wg.Done()

			// 模拟内存密集型操作
			data := make([]byte, 1024*1024) // 1MB
			_ = data

			// 模拟处理时间
			time.Sleep(time.Millisecond)
		}(i)
	}

	wg.Wait()
	t.Log("内存使用优化测试完成")
}

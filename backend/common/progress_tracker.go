// Package common 云盘通用组件
// 🔧 重构：提取可复用的进度跟踪组件，支持115和123网盘
package common

import (
	"fmt"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
)

// ProgressTracker 通用进度跟踪器
// 🔧 重构：基于115网盘的DownloadProgress，提取为可复用组件
type ProgressTracker struct {
	totalChunks      int64
	completedChunks  int64
	totalBytes       int64
	transferredBytes int64
	startTime        time.Time
	lastUpdateTime   time.Time
	chunkSizes       map[int64]int64         // 记录每个分片的大小
	chunkTimes       map[int64]time.Duration // 记录每个分片的传输时间
	peakSpeed        float64                 // 峰值速度 MB/s
	mu               sync.RWMutex
}

// NewProgressTracker 创建新的进度跟踪器
func NewProgressTracker(totalChunks, totalBytes int64) *ProgressTracker {
	return &ProgressTracker{
		totalChunks:    totalChunks,
		totalBytes:     totalBytes,
		startTime:      time.Now(),
		lastUpdateTime: time.Now(),
		chunkSizes:     make(map[int64]int64),
		chunkTimes:     make(map[int64]time.Duration),
		peakSpeed:      0,
	}
}

// UpdateChunkProgress 更新分片传输进度
func (pt *ProgressTracker) UpdateChunkProgress(chunkIndex, chunkSize int64, chunkDuration time.Duration) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	// 如果这个分片还没有记录，增加完成计数
	if _, exists := pt.chunkSizes[chunkIndex]; !exists {
		pt.completedChunks++
		pt.transferredBytes += chunkSize
		pt.chunkSizes[chunkIndex] = chunkSize
		pt.chunkTimes[chunkIndex] = chunkDuration
		pt.lastUpdateTime = time.Now()

		// 计算并更新峰值速度
		if chunkDuration.Seconds() > 0 {
			chunkSpeed := float64(chunkSize) / chunkDuration.Seconds() / 1024 / 1024 // MB/s
			if chunkSpeed > pt.peakSpeed {
				pt.peakSpeed = chunkSpeed
			}
		}
	}
}

// GetProgressInfo 获取当前进度信息
func (pt *ProgressTracker) GetProgressInfo() (percentage float64, avgSpeed float64, peakSpeed float64, eta time.Duration, completed, total int64, transferredBytes, totalBytes int64) {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	elapsed := time.Since(pt.startTime)
	percentage = float64(pt.completedChunks) / float64(pt.totalChunks) * 100

	if elapsed.Seconds() > 0 {
		avgSpeed = float64(pt.transferredBytes) / elapsed.Seconds() / 1024 / 1024 // MB/s
	}

	peakSpeed = pt.peakSpeed

	if avgSpeed > 0 && pt.transferredBytes > 0 {
		remainingBytes := pt.totalBytes - pt.transferredBytes
		etaSeconds := float64(remainingBytes) / (avgSpeed * 1024 * 1024)
		eta = time.Duration(etaSeconds) * time.Second
	}

	return percentage, avgSpeed, peakSpeed, eta, pt.completedChunks, pt.totalChunks, pt.transferredBytes, pt.totalBytes
}

// ProgressDisplayer 进度显示器接口
type ProgressDisplayer interface {
	DisplayProgress(progress *ProgressTracker, networkName, fileName string)
}

// StandardProgressDisplayer 标准进度显示器
// 🔧 重构：基于115网盘的displayDownloadProgress，提取为可复用组件
type StandardProgressDisplayer struct {
	displayInterval int64 // 每N个分片显示一次进度
}

// NewStandardProgressDisplayer 创建标准进度显示器
func NewStandardProgressDisplayer(displayInterval int64) *StandardProgressDisplayer {
	if displayInterval <= 0 {
		displayInterval = 5 // 默认每5个分片显示一次
	}
	return &StandardProgressDisplayer{
		displayInterval: displayInterval,
	}
}

// DisplayProgress 显示传输进度
func (spd *StandardProgressDisplayer) DisplayProgress(progress *ProgressTracker, networkName, fileName string) {
	percentage, avgSpeed, _, eta, completed, total, transferredBytes, _ := progress.GetProgressInfo()

	// 🚀 优化：减少独立进度显示频率，只在关键节点显示
	if completed%spd.displayInterval == 0 || completed == total {
		// 计算ETA显示
		etaStr := "ETA: -"
		if eta > 0 {
			etaStr = fmt.Sprintf("ETA: %v", eta.Round(time.Second))
		} else if avgSpeed > 0 {
			etaStr = "ETA: 计算中..."
		}

		// 简化的进度显示格式，集成到rclone日志系统
		fs.Debugf(nil, "📥 %s: %d/%d分片 (%.1f%%) | %s | %.2f MB/s | %s",
			networkName, completed, total, percentage,
			fs.SizeSuffix(transferredBytes), avgSpeed, etaStr)
	}
}

// ConcurrencyCalculator 并发数计算器
// 🔧 重构：提取通用的并发数计算逻辑
type ConcurrencyCalculator struct {
	MaxConcurrency int
	MinConcurrency int
}

// DefaultConcurrencyCalculator 返回默认的并发数计算器
func DefaultConcurrencyCalculator() *ConcurrencyCalculator {
	return &ConcurrencyCalculator{
		MaxConcurrency: 4, // 默认最大4并发
		MinConcurrency: 1, // 默认最小1并发
	}
}

// CalculateBaseConcurrency 计算基础并发数
func (cc *ConcurrencyCalculator) CalculateBaseConcurrency(fileSize int64) int {
	switch {
	case fileSize < 50*1024*1024: // <50MB
		return 1
	case fileSize < 200*1024*1024: // <200MB
		return 2
	case fileSize < 1*1024*1024*1024: // <1GB
		return 3
	default: // >=1GB
		return 4
	}
}

// AdjustConcurrency 根据网络条件调整并发数
func (cc *ConcurrencyCalculator) AdjustConcurrency(baseConcurrency int, networkLatency time.Duration, networkBandwidth int64) int {
	// 延迟调整因子
	latencyFactor := 1.0
	if networkLatency > 200*time.Millisecond {
		latencyFactor = 0.8 // 高延迟降低并发
	} else if networkLatency < 50*time.Millisecond {
		latencyFactor = 1.2 // 低延迟提升并发
	}

	// 带宽调整因子
	bandwidthFactor := 1.0
	if networkBandwidth > 50*1024*1024 { // >50Mbps
		bandwidthFactor = 1.1
	} else if networkBandwidth > 20*1024*1024 { // >20Mbps
		bandwidthFactor = 1.0
	} else if networkBandwidth > 10*1024*1024 { // >10Mbps
		bandwidthFactor = 0.95
	} else { // <10Mbps
		bandwidthFactor = 0.8
	}

	// 计算调整后的并发数
	adjustedConcurrency := int(float64(baseConcurrency) * latencyFactor * bandwidthFactor)

	// 确保在合理范围内
	if adjustedConcurrency > cc.MaxConcurrency {
		adjustedConcurrency = cc.MaxConcurrency
	}
	if adjustedConcurrency < cc.MinConcurrency {
		adjustedConcurrency = cc.MinConcurrency
	}

	return adjustedConcurrency
}

// NetworkMeasurer 网络测量器
// 🔧 重构：提取网络测量功能
type NetworkMeasurer struct{}

// NewNetworkMeasurer 创建网络测量器
func NewNetworkMeasurer() *NetworkMeasurer {
	return &NetworkMeasurer{}
}

// MeasureLatency 测量网络延迟
func (nm *NetworkMeasurer) MeasureLatency() time.Duration {
	// 简化的延迟测量，实际实现可以更复杂
	return 100 * time.Millisecond // 默认100ms
}

// EstimateBandwidth 估算网络带宽
func (nm *NetworkMeasurer) EstimateBandwidth() int64 {
	// 简化的带宽估算，实际实现可以更复杂
	return 50 * 1024 * 1024 // 默认50Mbps
}

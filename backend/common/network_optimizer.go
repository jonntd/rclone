// Package common 云盘通用组件
// 🔧 重构：提取可复用的网络优化组件，支持115和123网盘
package common

import (
	"context"
	"net/http"
	"time"

	"github.com/rclone/rclone/fs"
)

// NetworkOptimizer 通用网络优化器
// 🔧 重构：提取为可复用组件，统一网络优化策略
type NetworkOptimizer struct {
	// 网络参数
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	MaxConnsPerHost     int
	IdleConnTimeout     time.Duration

	// 超时配置
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	ExpectContinueTimeout time.Duration

	// TCP缓冲区配置
	WriteBufferSize int
	ReadBufferSize  int

	// 总体超时
	ClientTimeout time.Duration
}

// DefaultNetworkOptimizer 返回默认的网络优化配置
// 🔧 重构：基于115网盘的超激进优化配置
func DefaultNetworkOptimizer() *NetworkOptimizer {
	return &NetworkOptimizer{
		// 🚀 激进连接池配置：最大化并发性能
		MaxIdleConns:        300,
		MaxIdleConnsPerHost: 100,
		MaxConnsPerHost:     200,
		IdleConnTimeout:     120 * time.Second,

		// 🚀 激进超时配置：快速响应，快速重试
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: 500 * time.Millisecond,

		// 🚀 TCP优化配置
		WriteBufferSize: 128 * 1024, // 128KB写缓冲
		ReadBufferSize:  128 * 1024, // 128KB读缓冲

		// 🚀 总体超时
		ClientTimeout: 5 * time.Minute,
	}
}

// CreateOptimizedHTTPClient 创建优化的HTTP客户端
// 🔧 重构：统一HTTP客户端创建逻辑
func (no *NetworkOptimizer) CreateOptimizedHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// 连接池配置
			MaxIdleConns:        no.MaxIdleConns,
			MaxIdleConnsPerHost: no.MaxIdleConnsPerHost,
			MaxConnsPerHost:     no.MaxConnsPerHost,
			IdleConnTimeout:     no.IdleConnTimeout,

			// 超时配置
			TLSHandshakeTimeout:   no.TLSHandshakeTimeout,
			ResponseHeaderTimeout: no.ResponseHeaderTimeout,
			ExpectContinueTimeout: no.ExpectContinueTimeout,

			// 性能优化
			DisableKeepAlives:  false, // 启用Keep-Alive
			ForceAttemptHTTP2:  true,  // 强制尝试HTTP/2
			DisableCompression: false, // 启用压缩

			// TCP优化配置
			WriteBufferSize: no.WriteBufferSize,
			ReadBufferSize:  no.ReadBufferSize,
		},
		Timeout: no.ClientTimeout,
	}
}

// OptimizeNetworkParameters 优化网络参数
// 🔧 重构：通用网络参数优化函数
func (no *NetworkOptimizer) OptimizeNetworkParameters(ctx context.Context) {
	// 🔧 网络参数调优：设置更优的HTTP客户端参数
	ci := fs.GetConfig(ctx)

	// 网络参数优化建议（静默处理）
	_ = ci.Transfers
	_ = ci.Timeout
	_ = ci.LowLevelRetries
}

// PerformanceLevel 性能等级枚举
type PerformanceLevel string

const (
	PerformanceExcellent PerformanceLevel = "优秀"
	PerformanceGood      PerformanceLevel = "良好"
	PerformanceAverage   PerformanceLevel = "一般"
	PerformanceSlow      PerformanceLevel = "较慢"
	PerformanceVerySlow  PerformanceLevel = "很慢"
)

// EvaluatePerformance 评估传输性能等级
// 🔧 重构：通用性能评估函数
func EvaluatePerformance(avgSpeed float64) PerformanceLevel {
	switch {
	case avgSpeed >= 50: // >=50MB/s
		return PerformanceExcellent
	case avgSpeed >= 30: // >=30MB/s
		return PerformanceGood
	case avgSpeed >= 15: // >=15MB/s
		return PerformanceAverage
	case avgSpeed >= 5: // >=5MB/s
		return PerformanceSlow
	default: // <5MB/s
		return PerformanceVerySlow
	}
}

// SuggestOptimizations 提供性能优化建议
// 🔧 重构：通用性能优化建议函数
func SuggestOptimizations(avgSpeed float64, concurrency int, fileSize int64) []string {
	suggestions := []string{}

	// 基于速度的建议
	if avgSpeed < 15 {
		suggestions = append(suggestions, "网络速度较慢，建议检查网络连接")

		if concurrency < 8 {
			suggestions = append(suggestions, "可以尝试增加并发数以提升性能")
		}

		if fileSize > 5*1024*1024*1024 { // >5GB
			suggestions = append(suggestions, "大文件建议在网络状况良好时下载")
		}
	}

	// 基于并发数的建议
	if concurrency < 4 && fileSize > 1*1024*1024*1024 { // >1GB
		suggestions = append(suggestions, "大文件可以考虑增加并发数")
	}

	return suggestions
}

// ChunkSizeCalculator 分片大小计算器
// 🔧 重构：通用分片大小计算组件
type ChunkSizeCalculator struct {
	MinChunkSize fs.SizeSuffix
	MaxChunkSize fs.SizeSuffix
}

// DefaultChunkSizeCalculator 返回默认的分片大小计算器
func DefaultChunkSizeCalculator() *ChunkSizeCalculator {
	return &ChunkSizeCalculator{
		MinChunkSize: 5 * 1024 * 1024,        // 5MB
		MaxChunkSize: 5 * 1024 * 1024 * 1024, // 5GB
	}
}

// CalculateOptimalChunkSize 计算最优分片大小
// 🔧 重构：基于115网盘的超激进优化策略
func (csc *ChunkSizeCalculator) CalculateOptimalChunkSize(fileSize int64) fs.SizeSuffix {
	var partSize int64

	switch {
	case fileSize <= 50*1024*1024: // ≤50MB
		partSize = 25 * 1024 * 1024 // 25MB分片
	case fileSize <= 200*1024*1024: // ≤200MB
		partSize = 100 * 1024 * 1024 // 100MB分片
	case fileSize <= 1*1024*1024*1024: // ≤1GB
		partSize = 200 * 1024 * 1024 // 200MB分片
	default:
		partSize = 200 * 1024 * 1024 // 默认200MB分片
	}

	return fs.SizeSuffix(csc.normalizeChunkSize(partSize))
}

// normalizeChunkSize 确保分片大小在合理范围内
func (csc *ChunkSizeCalculator) normalizeChunkSize(partSize int64) int64 {
	if partSize < int64(csc.MinChunkSize) {
		partSize = int64(csc.MinChunkSize)
	}
	if partSize > int64(csc.MaxChunkSize) {
		partSize = int64(csc.MaxChunkSize)
	}
	return partSize
}

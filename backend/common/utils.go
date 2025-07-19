// Package common provides shared utilities for cloud storage backends
package common

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
)

// GetHTTPClient creates an optimized HTTP client for cloud storage operations
// 🔧 统一HTTP客户端配置，支持大文件传输和优化的连接池
func GetHTTPClient(ctx context.Context) *http.Client {
	t := fshttp.NewTransportCustom(ctx, func(t *http.Transport) {
		// 🔧 大幅增加响应头超时时间，支持大文件上传
		t.ResponseHeaderTimeout = 10 * time.Minute // 从2分钟增加到10分钟

		// 优化连接池配置
		t.MaxIdleConns = 100                 // 最大空闲连接数
		t.MaxIdleConnsPerHost = 20           // 每个主机的最大空闲连接数
		t.MaxConnsPerHost = 50               // 每个主机的最大连接数
		t.IdleConnTimeout = 90 * time.Second // 空闲连接超时
		t.DisableKeepAlives = false          // 启用Keep-Alive
		t.ForceAttemptHTTP2 = true           // 强制尝试HTTP/2

		// 优化超时设置
		t.DialContext = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		t.ExpectContinueTimeout = 1 * time.Second
	})

	return &http.Client{
		Transport: t,
		// 🔧 大幅增加总超时时间，支持大文件上传
		Timeout: 15 * time.Minute, // 从3分钟增加到15分钟
	}
}

// GetStandardHTTPClient creates a standard HTTP client using rclone defaults
// 🔧 简化版本：直接使用rclone的标准HTTP客户端配置
func GetStandardHTTPClient(ctx context.Context) *http.Client {
	// rclone的fshttp.NewClient已经提供了合适的默认配置
	// 包括超时、连接池、TLS设置等，无需额外自定义
	return fshttp.NewClient(ctx)
}

// ShouldRetry determines if an operation should be retried based on the error
// 🔧 统一重试逻辑，使用rclone标准的错误判断
func ShouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	// 使用rclone标准的上下文错误检查
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}

	// 使用rclone标准的重试判断
	return fserrors.ShouldRetry(err), err
}

// ShouldRetryHTTP determines if an HTTP operation should be retried
// 🔧 HTTP特定的重试逻辑，处理HTTP状态码
func ShouldRetryHTTP(ctx context.Context, resp *http.Response, err error) (bool, error) {
	// 使用rclone标准的上下文错误检查
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}

	// 检查HTTP响应状态码
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			// 429 Too Many Requests - 应该重试
			fs.Debugf(nil, "收到429限流错误，将重试")
			return true, fserrors.NewErrorRetryAfter(30 * time.Second)
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			// 5xx 服务器错误 - 应该重试
			fs.Debugf(nil, "收到%d服务器错误，将重试", resp.StatusCode)
			return true, err
		case http.StatusUnauthorized:
			// 401 未授权 - 不应该重试（需要刷新token）
			return false, err
		}
	}

	// 使用rclone标准的重试判断
	return fserrors.ShouldRetry(err), err
}

// CalculateOptimalChunkSize calculates optimal chunk size based on file size
// 🔧 统一分片大小计算逻辑
func CalculateOptimalChunkSize(fileSize int64) int64 {
	const (
		minChunkSize  = 10 * 1024 * 1024  // 10MB 最小分片
		maxChunkSize  = 200 * 1024 * 1024 // 200MB 最大分片
		baseChunkSize = 100 * 1024 * 1024 // 100MB 基础分片
	)

	// 根据文件大小调整分片大小
	if fileSize < 100*1024*1024 { // <100MB
		return minChunkSize
	} else if fileSize > 5*1024*1024*1024 { // >5GB
		return maxChunkSize
	}

	return baseChunkSize
}

// CalculateOptimalConcurrency calculates optimal concurrency based on file size
// 🔧 统一并发数计算逻辑
func CalculateOptimalConcurrency(fileSize int64, maxConcurrency int) int {
	// 基础并发数
	concurrency := 4

	// 根据文件大小调整
	if fileSize < 100*1024*1024 { // <100MB
		concurrency = 2
	} else if fileSize > 1*1024*1024*1024 { // >1GB
		concurrency = 4
	}

	// 应用最大并发数限制
	if maxConcurrency > 0 && concurrency > maxConcurrency {
		concurrency = maxConcurrency
	}

	if concurrency < 1 {
		concurrency = 1
	}

	return concurrency
}

// CalculateAdaptiveTimeout calculates timeout based on file size and operation type
// 🔧 统一超时时间计算逻辑
func CalculateAdaptiveTimeout(fileSize int64, operationType string) time.Duration {
	// 基础超时时间
	baseTimeout := 5 * time.Minute

	// 根据文件大小调整
	if fileSize > 1*1024*1024*1024 { // >1GB
		baseTimeout = 15 * time.Minute
	} else if fileSize > 100*1024*1024 { // >100MB
		baseTimeout = 10 * time.Minute
	}

	// 根据操作类型调整
	switch operationType {
	case "upload":
		return time.Duration(float64(baseTimeout) * 1.5)
	case "download":
		return time.Duration(float64(baseTimeout) * 1.2)
	case "concurrent":
		return time.Duration(float64(baseTimeout) * 2.0)
	default:
		return baseTimeout
	}
}

// Package common 云盘通用组件
// 🔧 重构：提取可复用的错误处理组件，支持115和123网盘
package common

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/rclone/rclone/fs/fserrors"
)

// CloudDriveErrorClassifier 云盘通用错误分类器
// 🔧 重构：基于115网盘的错误分类器，提取为可复用组件
type CloudDriveErrorClassifier struct {
	// 错误分类统计
	ServerOverloadCount  int64
	URLExpiredCount      int64
	NetworkTimeoutCount  int64
	RateLimitCount       int64
	AuthErrorCount       int64
	PermissionErrorCount int64
	NotFoundErrorCount   int64
	UnknownErrorCount    int64
}

// ErrorType 错误类型枚举
type ErrorType string

const (
	ErrorServerOverload ErrorType = "server_overload"
	ErrorURLExpired     ErrorType = "url_expired"
	ErrorNetworkTimeout ErrorType = "network_timeout"
	ErrorRateLimit      ErrorType = "rate_limit"
	ErrorAuth           ErrorType = "auth_error"
	ErrorPermission     ErrorType = "permission_error"
	ErrorNotFound       ErrorType = "not_found"
	ErrorFatal          ErrorType = "fatal_error"
	ErrorUnknown        ErrorType = "unknown"
)

// ClassifyError 统一错误分类方法
func (c *CloudDriveErrorClassifier) ClassifyError(err error) ErrorType {
	if err == nil {
		return ErrorUnknown
	}

	errStr := strings.ToLower(err.Error())

	// 服务器过载错误
	if strings.Contains(errStr, "502") || strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "504") || strings.Contains(errStr, "server overload") ||
		strings.Contains(errStr, "服务器过载") {
		c.ServerOverloadCount++
		return ErrorServerOverload
	}

	// URL过期错误
	if strings.Contains(errStr, "download url is invalid") ||
		strings.Contains(errStr, "expired") || strings.Contains(errStr, "url过期") ||
		strings.Contains(errStr, "invalid url") {
		c.URLExpiredCount++
		return ErrorURLExpired
	}

	// 网络超时错误
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "超时") ||
		strings.Contains(errStr, "connection reset") || strings.Contains(errStr, "connection refused") {
		c.NetworkTimeoutCount++
		return ErrorNetworkTimeout
	}

	// 限流错误
	if strings.Contains(errStr, "429") || strings.Contains(errStr, "too many requests") ||
		strings.Contains(errStr, "rate limit") || strings.Contains(errStr, "限流") {
		c.RateLimitCount++
		return ErrorRateLimit
	}

	// 认证错误
	if strings.Contains(errStr, "401") || strings.Contains(errStr, "unauthorized") ||
		strings.Contains(errStr, "token") || strings.Contains(errStr, "认证失败") {
		c.AuthErrorCount++
		return ErrorAuth
	}

	// 权限错误
	if strings.Contains(errStr, "403") || strings.Contains(errStr, "forbidden") ||
		strings.Contains(errStr, "permission denied") {
		c.PermissionErrorCount++
		return ErrorPermission
	}

	// 资源不存在错误
	if strings.Contains(errStr, "404") || strings.Contains(errStr, "not found") ||
		strings.Contains(errStr, "file not found") {
		c.NotFoundErrorCount++
		return ErrorNotFound
	}

	c.UnknownErrorCount++
	return ErrorUnknown
}

// RetryPolicy 重试策略
type RetryPolicy struct {
	MaxRetries map[ErrorType]int
	BaseDelay  time.Duration
}

// DefaultRetryPolicy 返回默认的重试策略
func DefaultRetryPolicy() *RetryPolicy {
	return &RetryPolicy{
		MaxRetries: map[ErrorType]int{
			ErrorServerOverload: 3,
			ErrorURLExpired:     4,
			ErrorNetworkTimeout: 2,
			ErrorRateLimit:      3,
			ErrorAuth:           0, // 认证错误不重试
			ErrorPermission:     0, // 权限错误不重试
			ErrorNotFound:       0, // 资源不存在不重试
			ErrorFatal:          0, // 致命错误不重试
			ErrorUnknown:        2, // 未知错误重试2次
		},
		BaseDelay: 1 * time.Second,
	}
}

// ShouldRetry 判断是否应该重试
func (rp *RetryPolicy) ShouldRetry(errorType ErrorType, retryCount int) bool {
	maxRetries, exists := rp.MaxRetries[errorType]
	if !exists {
		maxRetries = rp.MaxRetries[ErrorUnknown]
	}
	return retryCount < maxRetries
}

// CalculateRetryDelay 计算重试延迟
func (rp *RetryPolicy) CalculateRetryDelay(errorType ErrorType, retryCount int) time.Duration {
	switch errorType {
	case ErrorURLExpired:
		// URL过期错误不需要延迟
		return 0
	case ErrorRateLimit:
		// 限流错误使用更长的延迟
		return 30 * time.Second
	case ErrorServerOverload:
		// 服务器过载使用指数退避
		delay := time.Duration(1<<uint(retryCount)) * rp.BaseDelay
		if delay > 15*time.Second {
			delay = 15 * time.Second
		}
		return delay
	case ErrorNetworkTimeout:
		// 网络超时使用固定延迟
		return 2 * time.Second
	default:
		// 其他错误使用标准指数退避
		delay := time.Duration(1<<uint(retryCount)) * rp.BaseDelay
		if delay > 15*time.Second {
			delay = 15 * time.Second
		}
		return delay
	}
}

// CloudDriveRetryHandler 云盘通用重试处理器
// 🔧 重构：基于115网盘的shouldRetry函数，提取为可复用组件
type CloudDriveRetryHandler struct {
	classifier  *CloudDriveErrorClassifier
	retryPolicy *RetryPolicy
}

// NewCloudDriveRetryHandler 创建云盘重试处理器
func NewCloudDriveRetryHandler() *CloudDriveRetryHandler {
	return &CloudDriveRetryHandler{
		classifier:  &CloudDriveErrorClassifier{},
		retryPolicy: DefaultRetryPolicy(),
	}
}

// ShouldRetry 检查是否应该重试请求
// 🔧 重构：优先使用rclone标准错误处理，辅以云盘特定逻辑
func (crh *CloudDriveRetryHandler) ShouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	// 使用rclone标准的上下文错误检查
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}

	// 优先使用rclone标准的HTTP状态码处理
	if resp != nil {
		switch resp.StatusCode {
		case 429: // Too Many Requests
			return true, fserrors.NewErrorRetryAfter(30 * time.Second)
		case 401: // Unauthorized - 让auth handler处理
			return false, err
		case 403, 404: // Forbidden, Not Found - 不重试
			return false, err
		case 500, 502, 503, 504: // Server errors - 重试
			return true, err
		}
	}

	// 对于网络错误，优先使用rclone标准判断
	if err != nil {
		// 检查是否为rclone标准的可重试错误
		if fserrors.ShouldRetry(err) {
			return true, err
		}

		// 云盘特定的错误处理
		errorType := crh.classifier.ClassifyError(err)

		switch errorType {
		case ErrorURLExpired:
			// URL过期错误应该重试（会触发URL刷新）
			return true, err
		case ErrorAuth:
			// 认证错误让上层处理
			return false, err
		default:
			// 其他错误不重试
			return false, err
		}
	}

	return false, nil
}

// ExecuteWithRetry 执行带重试的操作
func (crh *CloudDriveRetryHandler) ExecuteWithRetry(ctx context.Context, operation func() error) error {
	var lastErr error

	for retry := 0; retry < 5; retry++ { // 最多重试5次
		err := operation()
		if err == nil {
			return nil // 成功
		}

		lastErr = err
		errorType := crh.classifier.ClassifyError(err)

		if !crh.retryPolicy.ShouldRetry(errorType, retry) {
			break // 不应该重试
		}

		delay := crh.retryPolicy.CalculateRetryDelay(errorType, retry)
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				// 继续重试
			}
		}
	}

	return lastErr
}

// GetErrorStatistics 获取错误统计信息
func (crh *CloudDriveRetryHandler) GetErrorStatistics() map[string]int64 {
	return map[string]int64{
		"server_overload":  crh.classifier.ServerOverloadCount,
		"url_expired":      crh.classifier.URLExpiredCount,
		"network_timeout":  crh.classifier.NetworkTimeoutCount,
		"rate_limit":       crh.classifier.RateLimitCount,
		"auth_error":       crh.classifier.AuthErrorCount,
		"permission_error": crh.classifier.PermissionErrorCount,
		"not_found":        crh.classifier.NotFoundErrorCount,
		"unknown":          crh.classifier.UnknownErrorCount,
	}
}

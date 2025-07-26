// Package common provides unified components for cloud storage backends
package common

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
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

// 🚀 统一错误处理器 - 新增组件
// UnifiedErrorHandler 统一错误处理器，支持123网盘和115网盘
type UnifiedErrorHandler struct {
	backendType string
	classifier  *CloudDriveErrorClassifier
	// 🔧 新增：错误统计功能
	stats ErrorStats
	mu    sync.RWMutex // 保护统计数据的并发访问
}

// ErrorStats 错误统计信息
type ErrorStats struct {
	TotalErrors       int64            `json:"total_errors"`
	ErrorsByType      map[string]int64 `json:"errors_by_type"`
	RetryAttempts     int64            `json:"retry_attempts"`
	SuccessfulRetries int64            `json:"successful_retries"`
	LastErrorTime     time.Time        `json:"last_error_time"`
	LastErrorType     string           `json:"last_error_type"`
}

// NewUnifiedErrorHandler 创建统一错误处理器
func NewUnifiedErrorHandler(backendType string) *UnifiedErrorHandler {
	return &UnifiedErrorHandler{
		backendType: backendType,
		classifier:  &CloudDriveErrorClassifier{},
		stats: ErrorStats{
			ErrorsByType: make(map[string]int64),
		},
	}
}

// HandleErrorWithRetry 统一错误处理和重试逻辑
func (h *UnifiedErrorHandler) HandleErrorWithRetry(ctx context.Context, err error, operation string, attempt int, maxRetries int) (shouldRetry bool, retryDelay time.Duration) {
	if err == nil {
		return false, 0
	}

	// 检查上下文是否已取消
	if fserrors.ContextError(ctx, &err) {
		return false, 0
	}

	// 🔧 新增：记录错误统计
	h.recordError(err)

	// 分类错误
	errorType := h.classifier.ClassifyError(err)

	// 根据错误类型决定重试策略
	switch errorType {
	case ErrorAuth, ErrorPermission, ErrorNotFound, ErrorFatal:
		// 这些错误不应该重试
		return false, 0
	case ErrorURLExpired:
		// URL过期立即重试，最多2次
		if attempt < 2 {
			h.recordRetryAttempt(false) // 记录重试尝试
			return true, 0              // 立即重试
		}
		return false, 0
	case ErrorRateLimit:
		// 速率限制使用较长延迟
		if attempt < maxRetries {
			h.recordRetryAttempt(false) // 记录重试尝试
			return true, time.Duration(15+attempt*10) * time.Second
		}
		return false, 0
	case ErrorServerOverload:
		// 服务器过载使用指数退避
		if attempt < maxRetries {
			delay := time.Duration(1<<uint(attempt)) * time.Second
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			h.recordRetryAttempt(false) // 记录重试尝试
			return true, delay
		}
		return false, 0
	case ErrorNetworkTimeout:
		// 网络超时使用标准重试
		if attempt < maxRetries {
			h.recordRetryAttempt(false) // 记录重试尝试
			return true, time.Duration(2+attempt*2) * time.Second
		}
		return false, 0
	default:
		// 未知错误使用保守重试
		if attempt < 2 {
			h.recordRetryAttempt(false) // 记录重试尝试
			return true, time.Duration(3+attempt*2) * time.Second
		}
		return false, 0
	}
}

// WrapError 包装错误信息，提供用户友好的错误消息
func (h *UnifiedErrorHandler) WrapError(err error, operation string, attempt int) error {
	if err == nil {
		return nil
	}

	errorType := h.classifier.ClassifyError(err)

	switch errorType {
	case ErrorAuth:
		return fmt.Errorf("🔐 %s网盘认证失败 - %s (尝试 %d)\n"+
			"💡 建议解决方案：\n"+
			"   1. 重新配置%s网盘认证信息\n"+
			"   2. 检查token是否已过期\n"+
			"   3. 运行 'rclone config' 重新授权\n"+
			"原始错误: %w", h.backendType, operation, attempt, h.backendType, err)
	case ErrorPermission:
		return fmt.Errorf("🚫 %s网盘权限不足 - %s (尝试 %d)\n"+
			"💡 建议解决方案：\n"+
			"   1. 检查是否有足够的权限访问该文件/目录\n"+
			"   2. 确认%s网盘账户状态正常\n"+
			"   3. 联系管理员获取必要权限\n"+
			"原始错误: %w", h.backendType, operation, attempt, h.backendType, err)
	case ErrorNotFound:
		return fmt.Errorf("📁 %s网盘文件不存在 - %s (尝试 %d)\n"+
			"💡 建议解决方案：\n"+
			"   1. 检查文件路径是否正确\n"+
			"   2. 确认文件是否已被移动或删除\n"+
			"   3. 使用 'rclone ls' 查看可用文件\n"+
			"原始错误: %w", h.backendType, operation, attempt, err)
	case ErrorRateLimit:
		return fmt.Errorf("⏱️ %s网盘请求频率过高 - %s (尝试 %d)\n"+
			"💡 建议解决方案：\n"+
			"   1. 稍等片刻后重试\n"+
			"   2. 减少并发传输数量\n"+
			"   3. 使用 --transfers=1 限制并发\n"+
			"原始错误: %w", h.backendType, operation, attempt, err)
	case ErrorServerOverload:
		return fmt.Errorf("🔧 %s网盘服务器暂时不可用 - %s (尝试 %d)\n"+
			"💡 建议解决方案：\n"+
			"   1. 稍后重试操作\n"+
			"   2. 检查%s网盘服务状态\n"+
			"   3. 如果问题持续，请联系技术支持\n"+
			"原始错误: %w", h.backendType, operation, attempt, h.backendType, err)
	case ErrorURLExpired:
		return fmt.Errorf("🔗 %s网盘下载链接过期 - %s (尝试 %d)\n"+
			"💡 建议解决方案：\n"+
			"   1. 系统将自动刷新链接并重试\n"+
			"   2. 如果问题持续，请稍后再试\n"+
			"原始错误: %w", h.backendType, operation, attempt, err)
	case ErrorNetworkTimeout:
		return fmt.Errorf("🌐 %s网盘网络连接问题 - %s (尝试 %d)\n"+
			"💡 建议解决方案：\n"+
			"   1. 检查网络连接是否稳定\n"+
			"   2. 尝试重新执行操作\n"+
			"   3. 如果问题持续，请稍后再试\n"+
			"原始错误: %w", h.backendType, operation, attempt, err)
	case ErrorFatal:
		return fserrors.FatalError(fmt.Errorf("💀 %s网盘致命错误 - %s (尝试 %d): %w", h.backendType, operation, attempt, err))
	default:
		return fmt.Errorf("❌ %s网盘操作失败 - %s (尝试 %d)\n"+
			"💡 建议解决方案：\n"+
			"   1. 检查网络连接和认证状态\n"+
			"   2. 重试操作或稍后再试\n"+
			"   3. 如需帮助，请提供以下错误信息\n"+
			"原始错误: %w", h.backendType, operation, attempt, err)
	}
}

// ExecuteWithRetry 执行操作并自动重试
func (h *UnifiedErrorHandler) ExecuteWithRetry(ctx context.Context, operation func() error, operationName string, maxRetries int) error {
	var lastErr error

	for attempt := 0; attempt < maxRetries+1; attempt++ {
		err := operation()
		if err == nil {
			return nil // 成功
		}

		lastErr = err

		// 检查是否应该重试
		shouldRetry, retryDelay := h.HandleErrorWithRetry(ctx, err, operationName, attempt, maxRetries)
		if !shouldRetry {
			break
		}

		// 等待重试延迟
		if retryDelay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryDelay):
			}
		}
	}

	// 包装最终错误
	return h.WrapError(lastErr, operationName, maxRetries)
}

// HandleSpecificError 处理特定类型的错误，提供更详细的解决方案
func (h *UnifiedErrorHandler) HandleSpecificError(err error, operation string) error {
	if err == nil {
		return nil
	}

	errorMsg := strings.ToLower(err.Error())

	// 文件名相关错误
	if strings.Contains(errorMsg, "filename") || strings.Contains(errorMsg, "name") {
		return fmt.Errorf("📝 %s网盘文件名问题 - %s\n"+
			"💡 建议解决方案：\n"+
			"   1. 检查文件名是否包含特殊字符\n"+
			"   2. 确保文件名长度不超过255字符\n"+
			"   3. 避免使用以下字符: \" \\ / : * ? | > <\n"+
			"原始错误: %w", h.backendType, operation, err)
	}

	// 存储空间不足错误
	if strings.Contains(errorMsg, "space") || strings.Contains(errorMsg, "quota") || strings.Contains(errorMsg, "storage") {
		return fmt.Errorf("💾 %s网盘存储空间不足 - %s\n"+
			"💡 建议解决方案：\n"+
			"   1. 清理%s网盘中的不需要文件\n"+
			"   2. 升级存储空间套餐\n"+
			"   3. 选择其他目标位置\n"+
			"原始错误: %w", h.backendType, operation, h.backendType, err)
	}

	// 父目录ID错误（123网盘特有）
	if strings.Contains(errorMsg, "parentfileid") || strings.Contains(errorMsg, "parent") {
		return fmt.Errorf("📂 %s网盘目录结构问题 - %s\n"+
			"💡 建议解决方案：\n"+
			"   1. 系统将自动修复目录结构\n"+
			"   2. 如果问题持续，请清理缓存\n"+
			"   3. 使用 'rclone config' 重新配置\n"+
			"原始错误: %w", h.backendType, operation, err)
	}

	// 使用标准错误包装
	return h.WrapError(err, operation, 1)
}

// HandleHTTPError 处理HTTP错误
func (h *UnifiedErrorHandler) HandleHTTPError(ctx context.Context, resp *http.Response, operation string, attempt int) (shouldRetry bool, retryAfter time.Duration, err error) {
	if resp == nil {
		return false, 0, fmt.Errorf("nil HTTP response")
	}

	var baseErr error

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		baseErr = fmt.Errorf("authentication failed: HTTP %d", resp.StatusCode)
	case http.StatusForbidden:
		baseErr = fmt.Errorf("permission denied: HTTP %d", resp.StatusCode)
	case http.StatusNotFound:
		baseErr = fmt.Errorf("object not found: HTTP %d", resp.StatusCode)
	case http.StatusTooManyRequests:
		baseErr = fmt.Errorf("rate limited: HTTP %d", resp.StatusCode)
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		baseErr = fmt.Errorf("server error: HTTP %d", resp.StatusCode)
	default:
		baseErr = fmt.Errorf("HTTP error: %d", resp.StatusCode)
	}

	shouldRetryResult, retryAfterResult := h.HandleErrorWithRetry(ctx, baseErr, operation, attempt, 3)
	return shouldRetryResult, retryAfterResult, baseErr
}

// recordError 记录错误统计信息
// 🔧 新增：统计错误类型和频率，用于监控和分析
func (h *UnifiedErrorHandler) recordError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.stats.TotalErrors++
	h.stats.LastErrorTime = time.Now()

	// 分类错误并统计
	errorType := h.classifier.ClassifyError(err)
	errorTypeStr := string(errorType)
	h.stats.ErrorsByType[errorTypeStr]++
	h.stats.LastErrorType = errorTypeStr
}

// recordRetryAttempt 记录重试尝试
// 🔧 新增：统计重试次数，用于分析重试效果
func (h *UnifiedErrorHandler) recordRetryAttempt(successful bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.stats.RetryAttempts++
	if successful {
		h.stats.SuccessfulRetries++
	}
}

// GetStats 获取错误统计信息
// 🔧 新增：提供统计数据查询接口
func (h *UnifiedErrorHandler) GetStats() ErrorStats {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// 创建副本以避免并发访问问题
	statsCopy := h.stats
	statsCopy.ErrorsByType = make(map[string]int64)
	for k, v := range h.stats.ErrorsByType {
		statsCopy.ErrorsByType[k] = v
	}

	return statsCopy
}

// ResetStats 重置错误统计信息
// 🔧 新增：提供统计数据重置功能
func (h *UnifiedErrorHandler) ResetStats() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.stats = ErrorStats{
		ErrorsByType: make(map[string]int64),
	}
}

package strmmount

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"golang.org/x/time/rate"
)

// APIRateLimiter API 速率限制器
type APIRateLimiter struct {
	mu           sync.RWMutex
	limiter      *rate.Limiter
	backend      string
	qpsLimit     rate.Limit
	burstLimit   int
	callCount    int64
	lastReset    time.Time
	enabled      bool
	
	// 统计信息
	totalCalls   int64
	blockedCalls int64
	avgWaitTime  time.Duration
}

// RateLimiterConfig 速率限制器配置
type RateLimiterConfig struct {
	Backend     string        // 后端类型
	QPS         rate.Limit    // 每秒请求数限制
	Burst       int           // 突发请求数
	Enabled     bool          // 是否启用
	LogLevel    string        // 日志级别
}

// NewAPIRateLimiter 创建新的 API 速率限制器
func NewAPIRateLimiter(backend string) *APIRateLimiter {
	config := getBackendRateLimitConfig(backend)
	
	limiter := &APIRateLimiter{
		backend:     backend,
		qpsLimit:    config.QPS,
		burstLimit:  config.Burst,
		enabled:     config.Enabled,
		lastReset:   time.Now(),
	}
	
	if limiter.enabled {
		limiter.limiter = rate.NewLimiter(config.QPS, config.Burst)
		fs.Infof(nil, "🛡️ [RATE-LIMITER] 已启用 %s 网盘速率限制: %.1f QPS, 突发: %d", 
			backend, float64(config.QPS), config.Burst)
	} else {
		fs.Infof(nil, "⚠️ [RATE-LIMITER] %s 网盘速率限制已禁用", backend)
	}
	
	return limiter
}

// getBackendRateLimitConfig 获取后端速率限制配置
func getBackendRateLimitConfig(backend string) RateLimiterConfig {
	switch backend {
	case "123":
		return RateLimiterConfig{
			Backend: "123",
			QPS:     rate.Limit(8.0),  // 保守设置：每秒8次请求
			Burst:   12,               // 允许短时间内12次突发请求
			Enabled: true,
		}
	case "115":
		return RateLimiterConfig{
			Backend: "115", 
			QPS:     rate.Limit(15.0), // 每秒15次请求
			Burst:   20,               // 允许短时间内20次突发请求
			Enabled: true,
		}
	default:
		return RateLimiterConfig{
			Backend: backend,
			QPS:     rate.Limit(5.0),  // 未知后端使用最保守设置
			Burst:   8,
			Enabled: true,
		}
	}
}

// Wait 等待获取 API 调用许可
func (rl *APIRateLimiter) Wait(ctx context.Context) error {
	if !rl.enabled {
		return nil
	}
	
	startTime := time.Now()
	
	// 等待速率限制器许可
	err := rl.limiter.Wait(ctx)
	if err != nil {
		rl.mu.Lock()
		rl.blockedCalls++
		rl.mu.Unlock()
		return fmt.Errorf("速率限制等待失败: %w", err)
	}
	
	waitTime := time.Since(startTime)
	
	// 更新统计信息
	rl.mu.Lock()
	rl.totalCalls++
	rl.callCount++
	
	// 更新平均等待时间
	if rl.avgWaitTime == 0 {
		rl.avgWaitTime = waitTime
	} else {
		rl.avgWaitTime = (rl.avgWaitTime + waitTime) / 2
	}
	rl.mu.Unlock()
	
	// 记录较长的等待时间
	if waitTime > 100*time.Millisecond {
		fs.Debugf(nil, "🐌 [RATE-LIMITER] API 调用等待: %v (后端: %s)", waitTime, rl.backend)
	}
	
	return nil
}

// TryWait 尝试获取 API 调用许可（非阻塞）
func (rl *APIRateLimiter) TryWait() bool {
	if !rl.enabled {
		return true
	}
	
	allowed := rl.limiter.Allow()
	
	rl.mu.Lock()
	if allowed {
		rl.totalCalls++
		rl.callCount++
	} else {
		rl.blockedCalls++
	}
	rl.mu.Unlock()
	
	if !allowed {
		fs.Debugf(nil, "🚫 [RATE-LIMITER] API 调用被限制 (后端: %s)", rl.backend)
	}
	
	return allowed
}

// GetStats 获取速率限制器统计信息
func (rl *APIRateLimiter) GetStats() RateLimiterStats {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	
	now := time.Now()
	duration := now.Sub(rl.lastReset)
	
	var currentQPS float64
	if duration.Seconds() > 0 {
		currentQPS = float64(rl.callCount) / duration.Seconds()
	}
	
	return RateLimiterStats{
		Backend:      rl.backend,
		Enabled:      rl.enabled,
		QPSLimit:     float64(rl.qpsLimit),
		CurrentQPS:   currentQPS,
		TotalCalls:   rl.totalCalls,
		BlockedCalls: rl.blockedCalls,
		AvgWaitTime:  rl.avgWaitTime,
		Duration:     duration,
		BurstLimit:   rl.burstLimit,
	}
}

// RateLimiterStats 速率限制器统计信息
type RateLimiterStats struct {
	Backend      string
	Enabled      bool
	QPSLimit     float64
	CurrentQPS   float64
	TotalCalls   int64
	BlockedCalls int64
	AvgWaitTime  time.Duration
	Duration     time.Duration
	BurstLimit   int
}

// ResetStats 重置统计信息
func (rl *APIRateLimiter) ResetStats() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	
	rl.callCount = 0
	rl.lastReset = time.Now()
	
	fs.Debugf(nil, "🔄 [RATE-LIMITER] 统计信息已重置 (后端: %s)", rl.backend)
}

// SetEnabled 启用或禁用速率限制器
func (rl *APIRateLimiter) SetEnabled(enabled bool) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	
	rl.enabled = enabled
	
	if enabled && rl.limiter == nil {
		rl.limiter = rate.NewLimiter(rl.qpsLimit, rl.burstLimit)
		fs.Infof(nil, "✅ [RATE-LIMITER] 已启用速率限制 (后端: %s)", rl.backend)
	} else if !enabled {
		fs.Infof(nil, "⚠️ [RATE-LIMITER] 已禁用速率限制 (后端: %s)", rl.backend)
	}
}

// UpdateLimits 更新速率限制参数
func (rl *APIRateLimiter) UpdateLimits(qps rate.Limit, burst int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	
	rl.qpsLimit = qps
	rl.burstLimit = burst
	
	if rl.enabled {
		rl.limiter = rate.NewLimiter(qps, burst)
		fs.Infof(nil, "🔧 [RATE-LIMITER] 已更新限制: %.1f QPS, 突发: %d (后端: %s)", 
			float64(qps), burst, rl.backend)
	}
}

// LogStats 记录统计信息
func (rl *APIRateLimiter) LogStats() {
	stats := rl.GetStats()
	
	if !stats.Enabled {
		return
	}
	
	blockRate := float64(0)
	if stats.TotalCalls > 0 {
		blockRate = float64(stats.BlockedCalls) / float64(stats.TotalCalls) * 100
	}
	
	fs.Infof(nil, "📊 [RATE-LIMITER] 统计 (%s): 当前QPS=%.2f/%.1f, 总调用=%d, 阻塞=%d(%.1f%%), 平均等待=%v", 
		stats.Backend, stats.CurrentQPS, stats.QPSLimit, stats.TotalCalls, 
		stats.BlockedCalls, blockRate, stats.AvgWaitTime)
}

// ConcurrencyLimiter 并发限制器
type ConcurrencyLimiter struct {
	semaphore chan struct{}
	maxConcurrent int
	currentCount  int64
	mu            sync.RWMutex
}

// NewConcurrencyLimiter 创建新的并发限制器
func NewConcurrencyLimiter(maxConcurrent int) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		semaphore:     make(chan struct{}, maxConcurrent),
		maxConcurrent: maxConcurrent,
	}
}

// Acquire 获取并发许可
func (cl *ConcurrencyLimiter) Acquire(ctx context.Context) error {
	select {
	case cl.semaphore <- struct{}{}:
		cl.mu.Lock()
		cl.currentCount++
		cl.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release 释放并发许可
func (cl *ConcurrencyLimiter) Release() {
	select {
	case <-cl.semaphore:
		cl.mu.Lock()
		cl.currentCount--
		cl.mu.Unlock()
	default:
		// 信号量已空，不需要释放
	}
}

// GetCurrentCount 获取当前并发数
func (cl *ConcurrencyLimiter) GetCurrentCount() int64 {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	return cl.currentCount
}

// APICallWrapper API 调用包装器，集成速率限制和并发控制
func (fsys *STRMFS) APICallWrapper(ctx context.Context, operation string, fn func() error) error {
	// 1. 获取并发许可
	if fsys.concurrencyLimiter != nil {
		if err := fsys.concurrencyLimiter.Acquire(ctx); err != nil {
			return fmt.Errorf("获取并发许可失败: %w", err)
		}
		defer fsys.concurrencyLimiter.Release()
	}
	
	// 2. 等待速率限制器许可
	if fsys.rateLimiter != nil {
		if err := fsys.rateLimiter.Wait(ctx); err != nil {
			return fmt.Errorf("速率限制等待失败: %w", err)
		}
	}
	
	// 3. 执行 API 调用
	startTime := time.Now()
	err := fn()
	duration := time.Since(startTime)
	
	// 4. 记录调用信息
	if err != nil {
		fs.Debugf(nil, "❌ [API] %s 失败: %v (耗时: %v)", operation, err, duration)
	} else {
		fs.Debugf(nil, "✅ [API] %s 成功 (耗时: %v)", operation, duration)
	}
	
	return err
}

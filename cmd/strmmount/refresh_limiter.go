package strmmount

import (
	"context"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
)

// RefreshLimiter 智能刷新限制器
type RefreshLimiter struct {
	// 基础配置
	minInterval       time.Duration            // 最小刷新间隔
	maxInterval       time.Duration            // 最大刷新间隔
	qpsThreshold      float64                  // QPS 阈值
	changeRateThreshold float64                // 变更率阈值
	
	// 状态跟踪
	lastRefresh       map[string]time.Time     // 每个目录的最后刷新时间
	accessCount       map[string]int           // 访问计数
	changeHistory     map[string]*ChangeStats  // 变更历史
	accessPatterns    map[string]*AccessPattern // 访问模式
	mu                sync.RWMutex
	
	// 统计信息
	allowedCount      int64                    // 允许的刷新次数
	blockedCount      int64                    // 阻止的刷新次数
	
	// QPS 跟踪
	qpsTracker        *QPSTracker
}

// ChangeStats 变更统计
type ChangeStats struct {
	TotalRefreshes  int       // 总刷新次数
	ActualChanges   int       // 实际变更次数
	ChangeRate      float64   // 变更率
	LastChangeTime  time.Time // 最后变更时间
	RecentChanges   []time.Time // 最近变更时间列表
}

// AccessPattern 访问模式
type AccessPattern struct {
	LastAccess    time.Time   // 最后访问时间
	AccessCount   int         // 访问次数
	AccessTimes   []time.Time // 访问时间列表
	PredictedNext time.Time   // 预测的下次访问时间
	Frequency     time.Duration // 访问频率
}

// QPSTracker QPS 跟踪器
type QPSTracker struct {
	apiCalls    []time.Time   // API 调用时间列表
	mu          sync.RWMutex
	windowSize  time.Duration // 统计窗口大小
}

// RefreshLimiterStats 刷新限制器统计信息
type RefreshLimiterStats struct {
	TotalDirectories    int     // 总目录数
	AllowedRefreshes    int64   // 允许的刷新次数
	BlockedRefreshes    int64   // 阻止的刷新次数
	CurrentQPS          float64 // 当前 QPS
	AverageChangeRate   float64 // 平均变更率
	ActivePatterns      int     // 活跃访问模式数
}

// NewRefreshLimiter 创建新的刷新限制器
func NewRefreshLimiter(minInterval, maxInterval time.Duration, qpsThreshold float64) *RefreshLimiter {
	return &RefreshLimiter{
		minInterval:       minInterval,
		maxInterval:       maxInterval,
		qpsThreshold:      qpsThreshold,
		changeRateThreshold: 0.1, // 默认变更率阈值
		lastRefresh:       make(map[string]time.Time),
		accessCount:       make(map[string]int),
		changeHistory:     make(map[string]*ChangeStats),
		accessPatterns:    make(map[string]*AccessPattern),
		qpsTracker:        NewQPSTracker(),
	}
}

// NewQPSTracker 创建新的 QPS 跟踪器
func NewQPSTracker() *QPSTracker {
	return &QPSTracker{
		apiCalls:   make([]time.Time, 0),
		windowSize: time.Minute, // 1分钟窗口
	}
}

// ShouldRefresh 检查是否应该刷新缓存
func (rl *RefreshLimiter) ShouldRefresh(dirPath string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	
	now := time.Now()
	
	// 记录访问模式
	rl.recordAccess(dirPath, now)
	
	// 1. 检查最小间隔限制
	if lastTime, exists := rl.lastRefresh[dirPath]; exists {
		timeSinceLastRefresh := now.Sub(lastTime)
		dynamicInterval := rl.getDynamicInterval(dirPath)
		
		if timeSinceLastRefresh < dynamicInterval {
			rl.blockedCount++
			fs.Debugf(nil, "🚫 [REFRESH-LIMIT] 间隔限制: %s (上次: %v前, 需要: %v)", 
				dirPath, timeSinceLastRefresh, dynamicInterval)
			return false
		}
	}
	
	// 2. 检查 QPS 限制
	currentQPS := rl.qpsTracker.GetCurrentQPS()
	if currentQPS > rl.qpsThreshold {
		rl.blockedCount++
		fs.Warnf(nil, "⚠️ [REFRESH-LIMIT] QPS 限制: %.2f > %.2f, 延迟刷新: %s", 
			currentQPS, rl.qpsThreshold, dirPath)
		return false
	}
	
	// 3. 基于变更历史的智能判断
	if !rl.shouldRefreshBasedOnHistory(dirPath) {
		rl.blockedCount++
		fs.Debugf(nil, "🧠 [REFRESH-LIMIT] 智能跳过: %s (低变更率)", dirPath)
		return false
	}
	
	// 4. 基于访问模式的预测
	if !rl.shouldRefreshBasedOnPattern(dirPath, now) {
		rl.blockedCount++
		fs.Debugf(nil, "📊 [REFRESH-LIMIT] 模式跳过: %s (非预期访问)", dirPath)
		return false
	}
	
	// 允许刷新
	rl.lastRefresh[dirPath] = now
	rl.allowedCount++
	
	fs.Infof(nil, "✅ [REFRESH-LIMIT] 允许刷新: %s (QPS: %.2f)", dirPath, currentQPS)
	return true
}

// getDynamicInterval 获取动态刷新间隔
func (rl *RefreshLimiter) getDynamicInterval(dirPath string) time.Duration {
	// 基于 QPS 动态调整间隔
	currentQPS := rl.qpsTracker.GetCurrentQPS()
	
	if currentQPS > rl.qpsThreshold {
		// QPS 过高，延长间隔
		multiplier := currentQPS / rl.qpsThreshold
		newInterval := time.Duration(float64(rl.minInterval) * multiplier)
		
		if newInterval > rl.maxInterval {
			return rl.maxInterval
		}
		return newInterval
	}
	
	// 基于变更历史调整间隔
	if stats, exists := rl.changeHistory[dirPath]; exists {
		if stats.ChangeRate < rl.changeRateThreshold {
			// 变更率很低，延长间隔
			return rl.minInterval * 2
		}
		
		// 如果最近有变更，缩短间隔
		if len(stats.RecentChanges) > 0 && 
		   time.Since(stats.RecentChanges[len(stats.RecentChanges)-1]) < time.Hour {
			return rl.minInterval / 2
		}
	}
	
	return rl.minInterval
}

// shouldRefreshBasedOnHistory 基于历史变更判断是否应该刷新
func (rl *RefreshLimiter) shouldRefreshBasedOnHistory(dirPath string) bool {
	stats, exists := rl.changeHistory[dirPath]
	if !exists {
		return true // 首次访问，允许刷新
	}
	
	// 如果变更率很低且最近没有变更，跳过刷新
	if stats.ChangeRate < rl.changeRateThreshold && 
	   time.Since(stats.LastChangeTime) > time.Hour*24 {
		return false
	}
	
	// 如果连续多次刷新都没有变更，降低刷新频率
	if len(stats.RecentChanges) == 0 && stats.TotalRefreshes > 5 {
		return false
	}
	
	return true
}

// shouldRefreshBasedOnPattern 基于访问模式判断是否应该刷新
func (rl *RefreshLimiter) shouldRefreshBasedOnPattern(dirPath string, now time.Time) bool {
	pattern, exists := rl.accessPatterns[dirPath]
	if !exists || len(pattern.AccessTimes) < 3 {
		return true // 数据不足，允许刷新
	}
	
	// 如果访问频率很低，降低刷新频率
	if pattern.Frequency > time.Hour*6 {
		return false
	}
	
	return true
}

// recordAccess 记录访问模式
func (rl *RefreshLimiter) recordAccess(dirPath string, accessTime time.Time) {
	pattern, exists := rl.accessPatterns[dirPath]
	if !exists {
		pattern = &AccessPattern{
			AccessTimes: make([]time.Time, 0),
		}
		rl.accessPatterns[dirPath] = pattern
	}
	
	pattern.LastAccess = accessTime
	pattern.AccessCount++
	pattern.AccessTimes = append(pattern.AccessTimes, accessTime)
	
	// 保持最近100次访问记录
	if len(pattern.AccessTimes) > 100 {
		pattern.AccessTimes = pattern.AccessTimes[len(pattern.AccessTimes)-100:]
	}
	
	// 计算访问频率
	if len(pattern.AccessTimes) >= 2 {
		totalDuration := pattern.AccessTimes[len(pattern.AccessTimes)-1].Sub(pattern.AccessTimes[0])
		pattern.Frequency = totalDuration / time.Duration(len(pattern.AccessTimes)-1)
	}
}

// RecordRefreshResult 记录刷新结果
func (rl *RefreshLimiter) RecordRefreshResult(dirPath string, hasChanges bool) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	
	stats, exists := rl.changeHistory[dirPath]
	if !exists {
		stats = &ChangeStats{
			RecentChanges: make([]time.Time, 0),
		}
		rl.changeHistory[dirPath] = stats
	}
	
	stats.TotalRefreshes++
	if hasChanges {
		stats.ActualChanges++
		stats.LastChangeTime = time.Now()
		stats.RecentChanges = append(stats.RecentChanges, time.Now())
		
		// 保持最近10次变更记录
		if len(stats.RecentChanges) > 10 {
			stats.RecentChanges = stats.RecentChanges[len(stats.RecentChanges)-10:]
		}
	}
	
	// 更新变更率
	stats.ChangeRate = float64(stats.ActualChanges) / float64(stats.TotalRefreshes)
	
	fs.Debugf(nil, "📊 [REFRESH-STATS] %s: 变更率=%.2f%% (%d/%d)", 
		dirPath, stats.ChangeRate*100, stats.ActualChanges, stats.TotalRefreshes)
}

// RecordAPICall 记录 API 调用
func (qt *QPSTracker) RecordAPICall() {
	qt.mu.Lock()
	defer qt.mu.Unlock()
	
	now := time.Now()
	qt.apiCalls = append(qt.apiCalls, now)
	
	// 清理过期的记录
	cutoff := now.Add(-qt.windowSize)
	for i, callTime := range qt.apiCalls {
		if callTime.After(cutoff) {
			qt.apiCalls = qt.apiCalls[i:]
			break
		}
	}
}

// GetCurrentQPS 获取当前 QPS
func (qt *QPSTracker) GetCurrentQPS() float64 {
	qt.mu.RLock()
	defer qt.mu.RUnlock()
	
	if len(qt.apiCalls) == 0 {
		return 0
	}
	
	return float64(len(qt.apiCalls)) / qt.windowSize.Seconds()
}

// SetChangeRateThreshold 设置变更率阈值
func (rl *RefreshLimiter) SetChangeRateThreshold(threshold float64) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.changeRateThreshold = threshold
}

// GetStats 获取统计信息
func (rl *RefreshLimiter) GetStats() RefreshLimiterStats {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	
	return RefreshLimiterStats{
		TotalDirectories:  len(rl.lastRefresh),
		AllowedRefreshes:  rl.allowedCount,
		BlockedRefreshes:  rl.blockedCount,
		CurrentQPS:        rl.qpsTracker.GetCurrentQPS(),
		AverageChangeRate: rl.calculateAverageChangeRate(),
		ActivePatterns:    len(rl.accessPatterns),
	}
}

// calculateAverageChangeRate 计算平均变更率
func (rl *RefreshLimiter) calculateAverageChangeRate() float64 {
	if len(rl.changeHistory) == 0 {
		return 0
	}
	
	totalRate := 0.0
	for _, stats := range rl.changeHistory {
		totalRate += stats.ChangeRate
	}
	
	return totalRate / float64(len(rl.changeHistory))
}

// LogStats 记录统计信息
func (rl *RefreshLimiter) LogStats() {
	stats := rl.GetStats()
	fs.Infof(nil, "📊 [REFRESH-LIMIT] 统计: 目录=%d, 允许=%d, 阻止=%d, QPS=%.3f, 平均变更率=%.2f%%, 访问模式=%d", 
		stats.TotalDirectories, stats.AllowedRefreshes, stats.BlockedRefreshes, 
		stats.CurrentQPS, stats.AverageChangeRate*100, stats.ActivePatterns)
}

package strmmount

import (
	"fmt"
	"time"

	"github.com/rclone/rclone/fs"
)

// RefreshConfig 刷新限制配置
type RefreshConfig struct {
	// 基础限制配置
	EnableRefreshLimit  bool        // 启用智能刷新限制
	MinRefreshInterval  fs.Duration // 最小刷新间隔
	MaxRefreshInterval  fs.Duration // 最大刷新间隔
	QPSThreshold        float64     // QPS 阈值
	ChangeRateThreshold float64     // 变更率阈值

	// 智能优化配置
	EnableRequestMerging  bool        // 启用请求合并
	EnablePredictiveCache bool        // 启用预测性缓存
	AccessPatternWindow   fs.Duration // 访问模式分析窗口

	// 高级配置
	MaxAccessHistory    int         // 最大访问历史记录数
	MaxChangeHistory    int         // 最大变更历史记录数
	StatsReportInterval fs.Duration // 统计报告间隔
}

// DefaultRefreshConfig 默认刷新限制配置
var DefaultRefreshConfig = RefreshConfig{
	EnableRefreshLimit:    true,
	MinRefreshInterval:    fs.Duration(time.Hour),     // 1小时
	MaxRefreshInterval:    fs.Duration(time.Hour * 6), // 6小时
	QPSThreshold:          0.5,                        // 0.5 QPS
	ChangeRateThreshold:   0.1,                        // 10% 变更率
	EnableRequestMerging:  true,
	EnablePredictiveCache: false,                         // 默认禁用预测性缓存
	AccessPatternWindow:   fs.Duration(time.Hour * 24),   // 24小时
	MaxAccessHistory:      100,                           // 最多100次访问记录
	MaxChangeHistory:      50,                            // 最多50次变更记录
	StatsReportInterval:   fs.Duration(time.Minute * 10), // 10分钟报告一次
}

// GetBackendOptimizedConfig 获取针对特定后端优化的配置
func GetBackendOptimizedConfig(backend string) RefreshConfig {
	config := DefaultRefreshConfig

	switch backend {
	case "123":
		// 123网盘 - 极保守配置
		config.MinRefreshInterval = fs.Duration(time.Hour * 2)  // 2小时
		config.MaxRefreshInterval = fs.Duration(time.Hour * 12) // 12小时
		config.QPSThreshold = 0.2                               // 0.2 QPS
		config.ChangeRateThreshold = 0.05                       // 5% 变更率

	case "115":
		// 115网盘 - 保守配置
		config.MinRefreshInterval = fs.Duration(time.Hour)     // 1小时
		config.MaxRefreshInterval = fs.Duration(time.Hour * 8) // 8小时
		config.QPSThreshold = 0.4                              // 0.4 QPS
		config.ChangeRateThreshold = 0.08                      // 8% 变更率

	default:
		// 未知后端 - 最保守配置
		config.MinRefreshInterval = fs.Duration(time.Hour * 3)  // 3小时
		config.MaxRefreshInterval = fs.Duration(time.Hour * 24) // 24小时
		config.QPSThreshold = 0.1                               // 0.1 QPS
		config.ChangeRateThreshold = 0.03                       // 3% 变更率
	}

	return config
}

// CreateRefreshLimiterFromConfig 根据配置创建刷新限制器
func CreateRefreshLimiterFromConfig(backend string, config RefreshConfig) *RefreshLimiter {
	if !config.EnableRefreshLimit {
		return nil
	}

	// 获取后端优化配置
	optimizedConfig := GetBackendOptimizedConfig(backend)

	// 用户配置覆盖默认配置
	if config.MinRefreshInterval > 0 {
		optimizedConfig.MinRefreshInterval = config.MinRefreshInterval
	}
	if config.MaxRefreshInterval > 0 {
		optimizedConfig.MaxRefreshInterval = config.MaxRefreshInterval
	}
	if config.QPSThreshold > 0 {
		optimizedConfig.QPSThreshold = config.QPSThreshold
	}
	if config.ChangeRateThreshold > 0 {
		optimizedConfig.ChangeRateThreshold = config.ChangeRateThreshold
	}

	// 创建刷新限制器
	limiter := NewRefreshLimiter(
		time.Duration(optimizedConfig.MinRefreshInterval),
		time.Duration(optimizedConfig.MaxRefreshInterval),
		optimizedConfig.QPSThreshold,
	)
	limiter.SetChangeRateThreshold(optimizedConfig.ChangeRateThreshold)

	fs.Infof(nil, "🛡️ [REFRESH-CONFIG] 刷新限制已启用: %s 网盘", backend)
	fs.Infof(nil, "📊 [REFRESH-CONFIG] 配置: 间隔=%v-%v, QPS阈值=%.2f, 变更率阈值=%.2f%%",
		optimizedConfig.MinRefreshInterval, optimizedConfig.MaxRefreshInterval,
		optimizedConfig.QPSThreshold, optimizedConfig.ChangeRateThreshold*100)

	return limiter
}

// ValidateRefreshConfig 验证刷新配置
func ValidateRefreshConfig(config *RefreshConfig) error {
	if config.MinRefreshInterval <= 0 {
		config.MinRefreshInterval = DefaultRefreshConfig.MinRefreshInterval
	}

	if config.MaxRefreshInterval <= config.MinRefreshInterval {
		config.MaxRefreshInterval = config.MinRefreshInterval * 6
	}

	if config.QPSThreshold <= 0 {
		config.QPSThreshold = DefaultRefreshConfig.QPSThreshold
	}

	if config.ChangeRateThreshold <= 0 || config.ChangeRateThreshold > 1 {
		config.ChangeRateThreshold = DefaultRefreshConfig.ChangeRateThreshold
	}

	if config.AccessPatternWindow <= 0 {
		config.AccessPatternWindow = DefaultRefreshConfig.AccessPatternWindow
	}

	if config.MaxAccessHistory <= 0 {
		config.MaxAccessHistory = DefaultRefreshConfig.MaxAccessHistory
	}

	if config.MaxChangeHistory <= 0 {
		config.MaxChangeHistory = DefaultRefreshConfig.MaxChangeHistory
	}

	if config.StatsReportInterval <= 0 {
		config.StatsReportInterval = DefaultRefreshConfig.StatsReportInterval
	}

	return nil
}

// GetRefreshConfigSummary 获取刷新配置摘要
func GetRefreshConfigSummary(config RefreshConfig) string {
	if !config.EnableRefreshLimit {
		return "刷新限制: 禁用"
	}

	return fmt.Sprintf("刷新限制: 启用 (间隔=%v-%v, QPS=%.2f, 变更率=%.1f%%)",
		config.MinRefreshInterval, config.MaxRefreshInterval,
		config.QPSThreshold, config.ChangeRateThreshold*100)
}

// RefreshLimiterMode 刷新限制器模式
type RefreshLimiterMode int

const (
	RefreshLimiterModeDisabled     RefreshLimiterMode = iota // 禁用
	RefreshLimiterModeConservative                           // 保守模式
	RefreshLimiterModeBalanced                               // 平衡模式
	RefreshLimiterModeAggressive                             // 激进模式
	RefreshLimiterModeCustom                                 // 自定义模式
)

// GetConfigByMode 根据模式获取配置
func GetConfigByMode(mode RefreshLimiterMode, backend string) RefreshConfig {
	baseConfig := GetBackendOptimizedConfig(backend)

	switch mode {
	case RefreshLimiterModeDisabled:
		baseConfig.EnableRefreshLimit = false

	case RefreshLimiterModeConservative:
		// 最保守设置
		baseConfig.MinRefreshInterval = baseConfig.MinRefreshInterval * 2
		baseConfig.MaxRefreshInterval = baseConfig.MaxRefreshInterval * 2
		baseConfig.QPSThreshold = baseConfig.QPSThreshold * 0.5
		baseConfig.ChangeRateThreshold = baseConfig.ChangeRateThreshold * 0.5

	case RefreshLimiterModeBalanced:
		// 使用默认设置

	case RefreshLimiterModeAggressive:
		// 更激进的设置
		baseConfig.MinRefreshInterval = baseConfig.MinRefreshInterval / 2
		baseConfig.MaxRefreshInterval = baseConfig.MaxRefreshInterval / 2
		baseConfig.QPSThreshold = baseConfig.QPSThreshold * 2
		baseConfig.ChangeRateThreshold = baseConfig.ChangeRateThreshold * 2

	case RefreshLimiterModeCustom:
		// 保持用户自定义设置
	}

	return baseConfig
}

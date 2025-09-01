//go:build cmount && ((linux && cgo) || (darwin && cgo) || (freebsd && cgo) || windows)

package strmmount

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/shirou/gopsutil/v3/process"
	"github.com/winfsp/cgofuse/fuse"
)

// AccessRecord 记录访问信息
type AccessRecord struct {
	Timestamp   time.Time `json:"timestamp"`
	ProcessName string    `json:"process_name"`
	ProcessID   int32     `json:"process_id"`
	Operation   string    `json:"operation"`
	Path        string    `json:"path"`
	Allowed     bool      `json:"allowed"`
	Reason      string    `json:"reason"`
}

// AccessControlConfig 访问控制配置
type AccessControlConfig struct {
	// 启用访问监控
	EnableMonitoring bool `json:"enable_monitoring"`

	// 启用访问控制
	EnableControl bool `json:"enable_control"`

	// 白名单模式（true）还是黑名单模式（false）
	WhitelistMode bool `json:"whitelist_mode"`

	// 允许的进程列表（白名单模式）
	AllowedProcesses []string `json:"allowed_processes"`

	// 禁止的进程列表（黑名单模式）
	BlockedProcesses []string `json:"blocked_processes"`

	// 系统进程处理策略
	SystemProcessPolicy string `json:"system_process_policy"` // "allow", "block", "log_only"

	// 日志文件路径
	LogFile string `json:"log_file"`

	// 最大日志记录数
	MaxLogRecords int `json:"max_log_records"`

	// 详细日志模式
	VerboseLogging bool `json:"verbose_logging"`
}

// AccessController 访问控制器
type AccessController struct {
	config  AccessControlConfig
	records []AccessRecord
	mu      sync.RWMutex
	logFile *os.File

	// 系统进程识别
	systemProcesses map[string]bool

	// 日志计数器，减少重复日志
	fuseContextLogCount int
}

// loadAccessControlConfigFromRclone 从rclone配置中加载访问控制配置
func loadAccessControlConfigFromRclone(remoteName string) AccessControlConfig {
	// 默认配置
	defaultConfig := AccessControlConfig{
		EnableMonitoring:    false,
		EnableControl:       false,
		WhitelistMode:       false,
		AllowedProcesses:    []string{},
		BlockedProcesses:    []string{},
		SystemProcessPolicy: "allow",
		LogFile:             "",
		MaxLogRecords:       1000,
		VerboseLogging:      false,
	}

	// 1. 从全局strm-mount配置读取
	loadConfigFromStorage(&defaultConfig, "strm-mount")

	// 2. 从remote特定配置读取（覆盖全局配置）
	if remoteName != "" {
		loadConfigFromStorage(&defaultConfig, remoteName)
	}

	// 3. 环境变量优先级最高（覆盖配置文件）
	if os.Getenv("STRM_ACCESS_MONITORING") != "" {
		defaultConfig.EnableMonitoring = getEnvBool("STRM_ACCESS_MONITORING", defaultConfig.EnableMonitoring)
	}
	if os.Getenv("STRM_ACCESS_CONTROL") != "" {
		defaultConfig.EnableControl = getEnvBool("STRM_ACCESS_CONTROL", defaultConfig.EnableControl)
	}
	if os.Getenv("STRM_WHITELIST_MODE") != "" {
		defaultConfig.WhitelistMode = getEnvBool("STRM_WHITELIST_MODE", defaultConfig.WhitelistMode)
	}
	if os.Getenv("STRM_ALLOWED_PROCESSES") != "" {
		defaultConfig.AllowedProcesses = getEnvStringSlice("STRM_ALLOWED_PROCESSES", defaultConfig.AllowedProcesses)
	}
	if os.Getenv("STRM_BLOCKED_PROCESSES") != "" {
		defaultConfig.BlockedProcesses = getEnvStringSlice("STRM_BLOCKED_PROCESSES", defaultConfig.BlockedProcesses)
	}
	if os.Getenv("STRM_SYSTEM_POLICY") != "" {
		defaultConfig.SystemProcessPolicy = getEnvString("STRM_SYSTEM_POLICY", defaultConfig.SystemProcessPolicy)
	}
	if os.Getenv("STRM_ACCESS_LOG") != "" {
		defaultConfig.LogFile = getEnvString("STRM_ACCESS_LOG", defaultConfig.LogFile)
	}
	if os.Getenv("STRM_MAX_LOG_RECORDS") != "" {
		defaultConfig.MaxLogRecords = getEnvInt("STRM_MAX_LOG_RECORDS", defaultConfig.MaxLogRecords)
	}
	if os.Getenv("STRM_VERBOSE_ACCESS") != "" {
		defaultConfig.VerboseLogging = getEnvBool("STRM_VERBOSE_ACCESS", defaultConfig.VerboseLogging)
	}

	return defaultConfig
}

// loadConfigFromStorage 从配置存储中加载配置
func loadConfigFromStorage(cfg *AccessControlConfig, sectionName string) {
	storage := config.LoadedData()

	if val, ok := storage.GetValue(sectionName, "access_control_enable"); ok {
		if parsed, err := strconv.ParseBool(val); err == nil {
			cfg.EnableControl = parsed
		}
	}
	if val, ok := storage.GetValue(sectionName, "access_monitoring_enable"); ok {
		if parsed, err := strconv.ParseBool(val); err == nil {
			cfg.EnableMonitoring = parsed
		}
	}
	if val, ok := storage.GetValue(sectionName, "access_whitelist_mode"); ok {
		if parsed, err := strconv.ParseBool(val); err == nil {
			cfg.WhitelistMode = parsed
		}
	}
	if val, ok := storage.GetValue(sectionName, "access_allowed_processes"); ok {
		cfg.AllowedProcesses = strings.Split(val, ",")
		// 清理空白字符
		for i, proc := range cfg.AllowedProcesses {
			cfg.AllowedProcesses[i] = strings.TrimSpace(proc)
		}
	}
	if val, ok := storage.GetValue(sectionName, "access_blocked_processes"); ok {
		cfg.BlockedProcesses = strings.Split(val, ",")
		// 清理空白字符
		for i, proc := range cfg.BlockedProcesses {
			cfg.BlockedProcesses[i] = strings.TrimSpace(proc)
		}
	}
	if val, ok := storage.GetValue(sectionName, "access_system_policy"); ok {
		cfg.SystemProcessPolicy = val
	}
	if val, ok := storage.GetValue(sectionName, "access_log_file"); ok {
		cfg.LogFile = val
	}
	if val, ok := storage.GetValue(sectionName, "access_max_log_records"); ok {
		if parsed, err := strconv.Atoi(val); err == nil {
			cfg.MaxLogRecords = parsed
		}
	}
	if val, ok := storage.GetValue(sectionName, "access_verbose_logging"); ok {
		if parsed, err := strconv.ParseBool(val); err == nil {
			cfg.VerboseLogging = parsed
		}
	}
}

// 辅助函数：从环境变量获取布尔值
func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

// 辅助函数：从环境变量获取字符串
func getEnvString(key string, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// 辅助函数：从环境变量获取整数
func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

// 辅助函数：从环境变量获取字符串切片
func getEnvStringSlice(key string, defaultValue []string) []string {
	if value := os.Getenv(key); value != "" {
		parts := strings.Split(value, ",")
		result := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				result = append(result, trimmed)
			}
		}
		return result
	}
	return defaultValue
}

// NewAccessControllerWithRemote 创建新的访问控制器（指定remote名称）
func NewAccessControllerWithRemote(remoteName string) (*AccessController, error) {
	// 从rclone配置中加载配置
	config := loadAccessControlConfigFromRclone(remoteName)

	controller, err := NewAccessController(config)
	if err != nil {
		return nil, err
	}

	// 如果启用了监控或控制，显示配置信息
	if config.EnableMonitoring || config.EnableControl {
		fs.Infof(nil, "✅ [ACCESS] 访问控制器已启动 (remote: %s)", remoteName)
		fs.Infof(nil, "📊 [ACCESS] 监控: %v, 控制: %v, 白名单模式: %v",
			config.EnableMonitoring, config.EnableControl, config.WhitelistMode)

		if len(config.AllowedProcesses) > 0 {
			fs.Infof(nil, "✅ [ACCESS] 允许的进程: %v", config.AllowedProcesses)
		}
		if len(config.BlockedProcesses) > 0 {
			fs.Infof(nil, "🚫 [ACCESS] 阻止的进程: %v", config.BlockedProcesses)
		}
		fs.Infof(nil, "🔧 [ACCESS] 系统进程策略: %s", config.SystemProcessPolicy)
	}

	return controller, nil
}

// NewAccessController 创建访问控制器
func NewAccessController(config AccessControlConfig) (*AccessController, error) {
	ac := &AccessController{
		config:  config,
		records: make([]AccessRecord, 0, config.MaxLogRecords),
		systemProcesses: map[string]bool{
			"mds":          true, // macOS Metadata Server
			"mds_stores":   true, // macOS Metadata Stores
			"mdbulkimport": true, // macOS Metadata Bulk Import
			// "Finder":         true, // macOS Finder - 移除，允许Finder显示文件夹
			"Spotlight":      true, // macOS Spotlight
			"fsevents":       true, // File System Events
			"kernel_task":    true, // Kernel Task
			"updatedb":       true, // Linux updatedb
			"tracker-miner":  true, // Linux Tracker
			"baloo_file":     true, // KDE Baloo
			"Windows Search": true, // Windows Search
			"SearchIndexer":  true, // Windows Search Indexer
		},
	}

	// 设置默认值
	if config.MaxLogRecords == 0 {
		ac.config.MaxLogRecords = 1000
	}

	if config.SystemProcessPolicy == "" {
		ac.config.SystemProcessPolicy = "log_only"
	}

	// 打开日志文件
	if config.LogFile != "" {
		var err error
		ac.logFile, err = os.OpenFile(config.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open log file %s: %w", config.LogFile, err)
		}
	}

	return ac, nil
}

// Close 关闭访问控制器
func (ac *AccessController) Close() error {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	if ac.logFile != nil {
		return ac.logFile.Close()
	}
	return nil
}

// getCallerProcessInfo 获取调用进程信息
func (ac *AccessController) getCallerProcessInfo() (processName string, processID int32, err error) {
	// 使用cgofuse的Getcontext()获取真正的调用进程信息
	uid, gid, pid := fuse.Getcontext()

	if pid <= 0 {
		return "", 0, fmt.Errorf("invalid process ID from FUSE context: %d", pid)
	}

	// 使用gopsutil获取进程名称
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return "", 0, fmt.Errorf("failed to get process info for PID %d: %w", pid, err)
	}

	name, err := proc.Name()
	if err != nil {
		return "", 0, fmt.Errorf("failed to get process name for PID %d: %w", pid, err)
	}

	// 减少重复的FUSE context日志
	if ac.config.VerboseLogging && ac.fuseContextLogCount < 5 {
		fs.Debugf(nil, "🔍 [ACCESS] FUSE context: uid=%d, gid=%d, pid=%d, name=%s", uid, gid, pid, name)
		ac.fuseContextLogCount++
	}

	return name, int32(pid), nil
}

// isSystemProcess 判断是否为系统进程
func (ac *AccessController) isSystemProcess(processName string) bool {
	// 直接匹配
	if ac.systemProcesses[processName] {
		return true
	}

	// 模糊匹配
	lowerName := strings.ToLower(processName)
	for sysProc := range ac.systemProcesses {
		if strings.Contains(lowerName, strings.ToLower(sysProc)) {
			return true
		}
	}

	return false
}

// shouldAllowAccess 判断是否允许访问
func (ac *AccessController) shouldAllowAccess(processName string, path string, operation string) (bool, string) {
	if !ac.config.EnableControl {
		return true, "access control disabled"
	}

	// 系统进程特殊处理
	if ac.isSystemProcess(processName) {
		switch ac.config.SystemProcessPolicy {
		case "allow":
			return true, "system process allowed by policy"
		case "block":
			return false, "system process blocked by policy"
		case "log_only":
			return true, "system process logged only"
		default:
			return true, "default system process policy"
		}
	}

	if ac.config.WhitelistMode {
		// 白名单模式：只允许列表中的进程
		for _, allowed := range ac.config.AllowedProcesses {
			if strings.Contains(strings.ToLower(processName), strings.ToLower(allowed)) {
				return true, "process in whitelist"
			}
		}
		return false, "process not in whitelist"
	} else {
		// 黑名单模式：禁止列表中的进程
		for _, blocked := range ac.config.BlockedProcesses {
			if strings.Contains(strings.ToLower(processName), strings.ToLower(blocked)) {
				return false, "process in blacklist"
			}
		}
		return true, "process not in blacklist"
	}
}

// logAccess 记录访问日志
func (ac *AccessController) logAccess(record AccessRecord) {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	// 添加到内存记录
	if len(ac.records) >= ac.config.MaxLogRecords {
		// 删除最旧的记录
		ac.records = ac.records[1:]
	}
	ac.records = append(ac.records, record)

	// 写入日志文件
	if ac.logFile != nil {
		logLine := fmt.Sprintf("[%s] %s(%d) %s %s -> %v (%s)\n",
			record.Timestamp.Format("2006-01-02 15:04:05"),
			record.ProcessName,
			record.ProcessID,
			record.Operation,
			record.Path,
			record.Allowed,
			record.Reason,
		)
		ac.logFile.WriteString(logLine)
		ac.logFile.Sync()
	}

	// 控制台日志
	if ac.config.VerboseLogging || !record.Allowed {
		logLevel := "DEBUG"
		if !record.Allowed {
			logLevel = "WARN"
		}

		fs.Logf(nil, "[%s] 🔍 [ACCESS] %s(%d) %s %s -> %v (%s)",
			logLevel,
			record.ProcessName,
			record.ProcessID,
			record.Operation,
			record.Path,
			record.Allowed,
			record.Reason,
		)
	}
}

// CheckAccess 检查访问权限
func (ac *AccessController) CheckAccess(path string, operation string) bool {
	if !ac.config.EnableMonitoring && !ac.config.EnableControl {
		return true
	}

	// 获取调用进程信息
	processName, processID, err := ac.getCallerProcessInfo()
	if err != nil {
		if ac.config.VerboseLogging {
			fs.Debugf(nil, "⚠️ [ACCESS] Failed to get caller process info: %v", err)
		}
		// 如果无法获取进程信息，默认允许访问
		return true
	}

	// 判断是否允许访问
	allowed, reason := ac.shouldAllowAccess(processName, path, operation)

	// 记录访问日志
	if ac.config.EnableMonitoring {
		record := AccessRecord{
			Timestamp:   time.Now(),
			ProcessName: processName,
			ProcessID:   processID,
			Operation:   operation,
			Path:        path,
			Allowed:     allowed,
			Reason:      reason,
		}
		ac.logAccess(record)
	}

	return allowed
}

// GetAccessRecords 获取访问记录
func (ac *AccessController) GetAccessRecords() []AccessRecord {
	ac.mu.RLock()
	defer ac.mu.RUnlock()

	// 返回副本
	records := make([]AccessRecord, len(ac.records))
	copy(records, ac.records)
	return records
}

// GetAccessStats 获取访问统计
func (ac *AccessController) GetAccessStats() map[string]interface{} {
	ac.mu.RLock()
	defer ac.mu.RUnlock()

	stats := map[string]interface{}{
		"total_records": len(ac.records),
		"processes":     make(map[string]int),
		"operations":    make(map[string]int),
		"allowed":       0,
		"blocked":       0,
	}

	for _, record := range ac.records {
		// 统计进程
		if processCount, ok := stats["processes"].(map[string]int); ok {
			processCount[record.ProcessName]++
		}

		// 统计操作
		if operationCount, ok := stats["operations"].(map[string]int); ok {
			operationCount[record.Operation]++
		}

		// 统计允许/阻止
		if record.Allowed {
			stats["allowed"] = stats["allowed"].(int) + 1
		} else {
			stats["blocked"] = stats["blocked"].(int) + 1
		}
	}

	return stats
}

// DefaultAccessControlConfig 返回默认的访问控制配置
func DefaultAccessControlConfig() AccessControlConfig {
	return AccessControlConfig{
		EnableMonitoring:    true,
		EnableControl:       false, // 默认只监控，不控制
		WhitelistMode:       false,
		AllowedProcesses:    []string{"Jellyfin", "Emby", "Plex", "Kodi"},
		BlockedProcesses:    []string{"mds", "Spotlight"}, // 移除Finder，允许Finder显示文件夹
		SystemProcessPolicy: "log_only",
		LogFile:             "",
		MaxLogRecords:       1000,
		VerboseLogging:      false,
	}
}

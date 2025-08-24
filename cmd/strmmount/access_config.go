//go:build cmount && ((linux && cgo) || (darwin && cgo) || (freebsd && cgo) || windows)

package strmmount

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rclone/rclone/fs"
)

// AccessConfigManager 访问控制配置管理器
type AccessConfigManager struct {
	configPath string
}

// NewAccessConfigManager 创建配置管理器
func NewAccessConfigManager(configDir string) *AccessConfigManager {
	if configDir == "" {
		// 使用默认配置目录
		homeDir, _ := os.UserHomeDir()
		configDir = filepath.Join(homeDir, ".config", "rclone", "strm-mount")
	}

	// 确保配置目录存在
	os.MkdirAll(configDir, 0755)

	return &AccessConfigManager{
		configPath: filepath.Join(configDir, "access_control.json"),
	}
}

// LoadConfig 加载配置
func (acm *AccessConfigManager) LoadConfig() (AccessControlConfig, error) {
	// 默认配置
	config := DefaultAccessControlConfig()

	// 检查配置文件是否存在
	if _, err := os.Stat(acm.configPath); os.IsNotExist(err) {
		fs.Infof(nil, "📄 [CONFIG] 访问控制配置文件不存在，使用默认配置: %s", acm.configPath)
		return config, nil
	}

	// 读取配置文件
	data, err := os.ReadFile(acm.configPath)
	if err != nil {
		return config, fmt.Errorf("读取配置文件失败: %w", err)
	}

	// 解析JSON
	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("解析配置文件失败: %w", err)
	}

	fs.Infof(nil, "✅ [CONFIG] 访问控制配置已加载: %s", acm.configPath)
	return config, nil
}

// SaveConfig 保存配置
func (acm *AccessConfigManager) SaveConfig(config AccessControlConfig) error {
	// 序列化为JSON
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	// 写入文件
	if err := os.WriteFile(acm.configPath, data, 0644); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}

	fs.Infof(nil, "💾 [CONFIG] 访问控制配置已保存: %s", acm.configPath)
	return nil
}

// CreateDefaultConfig 创建默认配置文件
func (acm *AccessConfigManager) CreateDefaultConfig() error {
	config := DefaultAccessControlConfig()

	// 添加一些示例配置
	config.AllowedProcesses = []string{
		"Jellyfin",  // Jellyfin媒体服务器
		"Emby",      // Emby媒体服务器
		"Plex",      // Plex媒体服务器
		"Kodi",      // Kodi媒体中心
		"VLC",       // VLC播放器
		"mpv",       // mpv播放器
		"ffmpeg",    // FFmpeg
		"MediaInfo", // MediaInfo
		"HandBrake", // HandBrake
	}

	config.BlockedProcesses = []string{
		"mds",        // macOS元数据服务
		"mds_stores", // macOS元数据存储
		"Spotlight",  // macOS Spotlight
		// "Finder",          // macOS Finder (允许Finder显示文件夹，但限制深度访问)
		"updatedb",       // Linux updatedb
		"tracker-miner",  // Linux Tracker
		"baloo_file",     // KDE Baloo
		"Windows Search", // Windows搜索
		"SearchIndexer",  // Windows搜索索引
	}

	return acm.SaveConfig(config)
}

// GetConfigPath 获取配置文件路径
func (acm *AccessConfigManager) GetConfigPath() string {
	return acm.configPath
}

// ValidateConfig 验证配置
func (acm *AccessConfigManager) ValidateConfig(config AccessControlConfig) error {
	// 检查基本设置
	if config.MaxLogRecords <= 0 {
		return fmt.Errorf("MaxLogRecords必须大于0")
	}

	if config.SystemProcessPolicy != "" {
		validPolicies := []string{"allow", "block", "log_only"}
		valid := false
		for _, policy := range validPolicies {
			if config.SystemProcessPolicy == policy {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("SystemProcessPolicy必须是: allow, block, log_only 之一")
		}
	}

	// 检查日志文件路径
	if config.LogFile != "" {
		dir := filepath.Dir(config.LogFile)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return fmt.Errorf("日志文件目录不存在: %s", dir)
		}
	}

	return nil
}

// PrintConfig 打印配置信息
func (acm *AccessConfigManager) PrintConfig(config AccessControlConfig) {
	fmt.Printf("\n🔒 访问控制配置:\n")
	fmt.Printf("  监控启用: %v\n", config.EnableMonitoring)
	fmt.Printf("  控制启用: %v\n", config.EnableControl)

	if config.EnableControl {
		mode := "黑名单"
		if config.WhitelistMode {
			mode = "白名单"
		}
		fmt.Printf("  控制模式: %s\n", mode)

		if config.WhitelistMode && len(config.AllowedProcesses) > 0 {
			fmt.Printf("  允许的进程:\n")
			for _, proc := range config.AllowedProcesses {
				fmt.Printf("    - %s\n", proc)
			}
		}

		if !config.WhitelistMode && len(config.BlockedProcesses) > 0 {
			fmt.Printf("  禁止的进程:\n")
			for _, proc := range config.BlockedProcesses {
				fmt.Printf("    - %s\n", proc)
			}
		}
	}

	fmt.Printf("  系统进程策略: %s\n", config.SystemProcessPolicy)

	if config.LogFile != "" {
		fmt.Printf("  日志文件: %s\n", config.LogFile)
	}

	fmt.Printf("  最大日志记录: %d\n", config.MaxLogRecords)
	fmt.Printf("  详细日志: %v\n", config.VerboseLogging)
	fmt.Printf("  配置文件: %s\n", acm.configPath)
	fmt.Println()
}

// GetExampleConfig 获取示例配置
func GetExampleConfig() string {
	config := DefaultAccessControlConfig()
	config.EnableControl = true
	config.WhitelistMode = true
	config.AllowedProcesses = []string{"Jellyfin", "Emby", "Plex", "Kodi"}
	config.LogFile = "/tmp/strm_access.log"
	config.VerboseLogging = true

	data, _ := json.MarshalIndent(config, "", "  ")
	return string(data)
}

// 环境变量配置覆盖
func OverrideConfigFromEnv(config *AccessControlConfig) {
	// 从环境变量覆盖配置
	if os.Getenv("STRM_ACCESS_CONTROL") == "true" {
		config.EnableControl = true
	} else if os.Getenv("STRM_ACCESS_CONTROL") == "false" {
		config.EnableControl = false
	}

	if os.Getenv("STRM_ACCESS_MONITORING") == "true" {
		config.EnableMonitoring = true
	} else if os.Getenv("STRM_ACCESS_MONITORING") == "false" {
		config.EnableMonitoring = false
	}

	if os.Getenv("STRM_WHITELIST_MODE") == "true" {
		config.WhitelistMode = true
	} else if os.Getenv("STRM_WHITELIST_MODE") == "false" {
		config.WhitelistMode = false
	}

	if logFile := os.Getenv("STRM_ACCESS_LOG"); logFile != "" {
		config.LogFile = logFile
	}

	if os.Getenv("STRM_VERBOSE_ACCESS") == "true" {
		config.VerboseLogging = true
	} else if os.Getenv("STRM_VERBOSE_ACCESS") == "false" {
		config.VerboseLogging = false
	}

	if policy := os.Getenv("STRM_SYSTEM_POLICY"); policy != "" {
		config.SystemProcessPolicy = policy
	}
}

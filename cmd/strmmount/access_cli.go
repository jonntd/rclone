//go:build cmount && ((linux && cgo) || (darwin && cgo) || (freebsd && cgo) || windows)

package strmmount

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// AddAccessControlCommands 添加访问控制相关命令
func AddAccessControlCommands(rootCmd *cobra.Command) {
	accessCmd := &cobra.Command{
		Use:   "access",
		Short: "访问控制管理",
		Long:  "管理strm-mount的访问控制和监控功能",
	}

	// 配置管理命令
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "配置管理",
		Long:  "管理访问控制配置",
	}

	// 显示配置
	configCmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "显示当前配置",
		RunE: func(cmd *cobra.Command, args []string) error {
			configManager := NewAccessConfigManager("")
			config, err := configManager.LoadConfig()
			if err != nil {
				return fmt.Errorf("加载配置失败: %w", err)
			}

			configManager.PrintConfig(config)
			return nil
		},
	})

	// 创建默认配置
	configCmd.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "创建默认配置文件",
		RunE: func(cmd *cobra.Command, args []string) error {
			configManager := NewAccessConfigManager("")

			// 检查配置文件是否已存在
			if _, err := os.Stat(configManager.GetConfigPath()); err == nil {
				fmt.Printf("配置文件已存在: %s\n", configManager.GetConfigPath())
				fmt.Print("是否覆盖? (y/N): ")
				var response string
				fmt.Scanln(&response)
				if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
					fmt.Println("操作已取消")
					return nil
				}
			}

			if err := configManager.CreateDefaultConfig(); err != nil {
				return fmt.Errorf("创建配置文件失败: %w", err)
			}

			fmt.Printf("✅ 默认配置文件已创建: %s\n", configManager.GetConfigPath())
			fmt.Println("\n📝 请编辑配置文件以自定义访问控制设置")
			return nil
		},
	})

	// 验证配置
	configCmd.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "验证配置文件",
		RunE: func(cmd *cobra.Command, args []string) error {
			configManager := NewAccessConfigManager("")
			config, err := configManager.LoadConfig()
			if err != nil {
				return fmt.Errorf("加载配置失败: %w", err)
			}

			if err := configManager.ValidateConfig(config); err != nil {
				return fmt.Errorf("配置验证失败: %w", err)
			}

			fmt.Println("✅ 配置文件验证通过")
			return nil
		},
	})

	// 显示示例配置
	configCmd.AddCommand(&cobra.Command{
		Use:   "example",
		Short: "显示示例配置",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("📄 示例配置文件内容:")
			fmt.Println(GetExampleConfig())
			return nil
		},
	})

	accessCmd.AddCommand(configCmd)

	// 监控命令
	monitorCmd := &cobra.Command{
		Use:   "monitor",
		Short: "访问监控",
		Long:  "监控和分析文件系统访问",
	}

	// 实时监控
	monitorCmd.AddCommand(&cobra.Command{
		Use:   "live",
		Short: "实时监控访问",
		Long:  "实时显示文件系统访问情况",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("🔍 实时访问监控 (按Ctrl+C退出)")
			fmt.Println("注意: 需要strm-mount正在运行且启用了访问监控")
			fmt.Println()

			// 这里可以实现实时监控逻辑
			// 例如读取日志文件或连接到运行中的strm-mount实例

			fmt.Println("💡 提示: 请设置环境变量 STRM_ACCESS_LOG 来指定日志文件路径")
			fmt.Println("💡 提示: 请设置环境变量 STRM_VERBOSE_ACCESS=true 来启用详细日志")

			return nil
		},
	})

	// 分析日志
	var logFile string
	analyzeCmd := &cobra.Command{
		Use:   "analyze",
		Short: "分析访问日志",
		Long:  "分析访问日志文件，生成统计报告",
		RunE: func(cmd *cobra.Command, args []string) error {
			if logFile == "" {
				return fmt.Errorf("请指定日志文件路径 (使用 --log-file 参数)")
			}

			return analyzeAccessLog(logFile)
		},
	}
	analyzeCmd.Flags().StringVarP(&logFile, "log-file", "f", "", "访问日志文件路径")
	monitorCmd.AddCommand(analyzeCmd)

	accessCmd.AddCommand(monitorCmd)

	// 控制命令
	controlCmd := &cobra.Command{
		Use:   "control",
		Short: "访问控制",
		Long:  "管理访问控制规则",
	}

	// 添加允许的进程
	var processName string
	allowCmd := &cobra.Command{
		Use:   "allow",
		Short: "添加允许的进程",
		RunE: func(cmd *cobra.Command, args []string) error {
			if processName == "" {
				return fmt.Errorf("请指定进程名称 (使用 --process 参数)")
			}

			return addAllowedProcess(processName)
		},
	}
	allowCmd.Flags().StringVarP(&processName, "process", "p", "", "进程名称")
	controlCmd.AddCommand(allowCmd)

	// 添加禁止的进程
	blockCmd := &cobra.Command{
		Use:   "block",
		Short: "添加禁止的进程",
		RunE: func(cmd *cobra.Command, args []string) error {
			if processName == "" {
				return fmt.Errorf("请指定进程名称 (使用 --process 参数)")
			}

			return addBlockedProcess(processName)
		},
	}
	blockCmd.Flags().StringVarP(&processName, "process", "p", "", "进程名称")
	controlCmd.AddCommand(blockCmd)

	accessCmd.AddCommand(controlCmd)

	rootCmd.AddCommand(accessCmd)
}

// analyzeAccessLog 分析访问日志
func analyzeAccessLog(logFile string) error {
	// 检查文件是否存在
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		return fmt.Errorf("日志文件不存在: %s", logFile)
	}

	fmt.Printf("📊 分析访问日志: %s\n\n", logFile)

	// 这里可以实现日志分析逻辑
	// 例如统计访问次数、进程分布、时间分布等

	fmt.Println("💡 提示: 日志分析功能正在开发中")
	fmt.Println("💡 提示: 您可以使用 grep、awk 等工具手动分析日志文件")

	return nil
}

// addAllowedProcess 添加允许的进程
func addAllowedProcess(processName string) error {
	configManager := NewAccessConfigManager("")
	config, err := configManager.LoadConfig()
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}

	// 检查是否已存在
	for _, existing := range config.AllowedProcesses {
		if existing == processName {
			fmt.Printf("⚠️ 进程 '%s' 已在允许列表中\n", processName)
			return nil
		}
	}

	// 添加到允许列表
	config.AllowedProcesses = append(config.AllowedProcesses, processName)

	// 保存配置
	if err := configManager.SaveConfig(config); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}

	fmt.Printf("✅ 已添加允许的进程: %s\n", processName)
	return nil
}

// addBlockedProcess 添加禁止的进程
func addBlockedProcess(processName string) error {
	configManager := NewAccessConfigManager("")
	config, err := configManager.LoadConfig()
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}

	// 检查是否已存在
	for _, existing := range config.BlockedProcesses {
		if existing == processName {
			fmt.Printf("⚠️ 进程 '%s' 已在禁止列表中\n", processName)
			return nil
		}
	}

	// 添加到禁止列表
	config.BlockedProcesses = append(config.BlockedProcesses, processName)

	// 保存配置
	if err := configManager.SaveConfig(config); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}

	fmt.Printf("✅ 已添加禁止的进程: %s\n", processName)
	return nil
}

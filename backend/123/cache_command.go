package _123

import (
	"context"
	"fmt"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
	"github.com/spf13/cobra"
)

func init() {
	cmd.Root.AddCommand(tree123Command)
}

var tree123Command = &cobra.Command{
	Use:   "tree123 remote:",
	Short: "显示123网盘目录树",
	Long:  "以树形结构显示123网盘的目录和文件",
	Args:  cobra.ExactArgs(1),
	RunE:  runTree123Command,
}

// runTree123Command 执行123网盘目录树命令
func runTree123Command(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// 解析远程路径
	fsrc, err := fs.NewFs(ctx, args[0])
	if err != nil {
		return fmt.Errorf("无法连接到远程: %v", err)
	}

	// 检查是否为123网盘
	f123, ok := fsrc.(*Fs)
	if !ok {
		return fmt.Errorf("指定的远程不是123网盘")
	}

	// 创建缓存查看器
	viewer := NewCacheViewer(f123)

	// 生成目录树
	fmt.Println("🔄 正在生成目录树...")

	treeText, err := viewer.GenerateDirectoryTreeText()
	if err != nil {
		return fmt.Errorf("生成目录树失败: %v", err)
	}

	fmt.Print(treeText)
	return nil
}

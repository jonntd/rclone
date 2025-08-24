// Package strmmount implements a FUSE mounting system for rclone remotes
// that virtualizes video files as .strm files for media servers.

//go:build cmount

// Package strmmount implements a FUSE mounting system for rclone remotes
// that virtualizes video files as .strm files for media servers.
package strmmount

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/flags"
	"github.com/rclone/rclone/lib/buildinfo"
	"github.com/rclone/rclone/vfs"
	"github.com/spf13/pflag"
	"github.com/winfsp/cgofuse/fuse"
)

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	return &Config{
		VideoExtensions: []string{"iso", "mp4", "mkv", "avi", "mov", "wmv", "flv", "webm", "m4v", "3gp", "ts", "m2ts"},
		MinFileSize:     200 * 1024 * 1024, // 100MB
		URLFormat:       "auto",
		CacheTimeout:    fs.Duration(30 * time.Minute),
		MaxCacheSize:    10000,

		// 持久化缓存默认配置
		PersistentCache:   true,                         // 默认启用
		CacheDir:          "",                           // 使用默认缓存目录
		CacheTTL:          fs.Duration(5 * time.Minute), // 5分钟过期
		MaxPersistentSize: 100 * 1024 * 1024,            // 100MB
		SyncInterval:      fs.Duration(1 * time.Minute), // 1分钟同步间隔
		EnableCompression: true,                         // 启用压缩
		BackgroundSync:    true,                         // 启用后台同步
	}
}

// Global configuration
var strmConfig = DefaultConfig()

func init() {
	// Register the command
	cmd := mountlib.NewMountCommand("strm-mount", false, mount)
	cmd.Short = "Mount cloud storage with video files as .strm files"
	cmd.Long = `
Mount cloud storage with video files virtualized as .strm files.

This command mounts a cloud storage remote and presents video files as
.strm files containing cloud protocol URLs (123://fileId or 115://pickCode).
This is useful for media servers like Jellyfin, Emby, or Plex.

Video files larger than --min-size will be virtualized as .strm files,
while other files remain accessible normally.

Supported cloud storage backends:
- 123 网盘: Creates 123://fileId URLs
- 115 网盘: Creates 115://pickCode URLs  
- Other backends: Uses file paths

Example:
    rclone strm-mount 123:Movies /mnt/strm-movies
    rclone strm-mount 115:Series /mnt/strm-series --min-size 50M
`

	// Add strm-mount specific flags
	cmdFlags := cmd.Flags()
	addSTRMFlags(cmdFlags)

	// Register for remote control
	mountlib.AddRc("strm-mount", mount)
	buildinfo.Tags = append(buildinfo.Tags, "strm-mount")
}

// addSTRMFlags adds strm-mount specific flags
func addSTRMFlags(flagSet *pflag.FlagSet) {
	flags.StringArrayVarP(flagSet, &strmConfig.VideoExtensions, "video-ext", "", strmConfig.VideoExtensions,
		"Video file extensions to virtualize", "")
	flags.FVarP(flagSet, &strmConfig.MinFileSize, "min-size", "",
		"Minimum file size to virtualize", "")
	flags.StringVarP(flagSet, &strmConfig.URLFormat, "url-format", "", strmConfig.URLFormat,
		"URL format: auto, 123, 115, path", "")
	flags.FVarP(flagSet, &strmConfig.CacheTimeout, "cache-timeout", "",
		"Cache timeout for file metadata", "")
	flags.IntVarP(flagSet, &strmConfig.MaxCacheSize, "max-cache-size", "", strmConfig.MaxCacheSize,
		"Maximum cache entries", "")

	// 持久化缓存参数
	flags.BoolVarP(flagSet, &strmConfig.PersistentCache, "persistent-cache", "", strmConfig.PersistentCache,
		"Enable persistent cache for faster startup", "")
	flags.StringVarP(flagSet, &strmConfig.CacheDir, "cache-dir", "", strmConfig.CacheDir,
		"Custom cache directory path", "")
	flags.FVarP(flagSet, &strmConfig.CacheTTL, "cache-ttl", "",
		"Cache time-to-live (e.g. 5m, 1h)", "")
	flags.FVarP(flagSet, &strmConfig.MaxPersistentSize, "max-persistent-size", "",
		"Maximum persistent cache size", "")
	flags.FVarP(flagSet, &strmConfig.SyncInterval, "sync-interval", "",
		"Background sync interval", "")
	flags.BoolVarP(flagSet, &strmConfig.EnableCompression, "enable-compression", "", strmConfig.EnableCompression,
		"Enable cache compression", "")
	flags.BoolVarP(flagSet, &strmConfig.BackgroundSync, "background-sync", "", strmConfig.BackgroundSync,
		"Enable background sync", "")
}

// mount implements the strm-mount functionality
func mount(VFS *vfs.VFS, mountpoint string, opt *mountlib.Options) (<-chan error, func() error, error) {
	startTime := time.Now()
	f := VFS.Fs()

	// Check if the backend supports strm virtualization
	backendName := f.Name()
	fs.Infof(nil, "🎬 [PERF] Starting STRM mount for %s backend at %s", backendName, mountpoint)
	fs.Infof(nil, "📊 [PERF] Mount start time: %s", startTime.Format("15:04:05.000"))

	// Log configuration details
	fs.Infof(nil, "⚙️ [CONFIG] Video extensions: %v", strmConfig.VideoExtensions)
	fs.Infof(nil, "⚙️ [CONFIG] Min file size: %s", strmConfig.MinFileSize)
	fs.Infof(nil, "⚙️ [CONFIG] URL format: %s", strmConfig.URLFormat)
	fs.Infof(nil, "⚙️ [CONFIG] Cache timeout: %s", strmConfig.CacheTimeout)
	fs.Infof(nil, "⚙️ [CONFIG] Max cache size: %d", strmConfig.MaxCacheSize)

	// Optimize config based on backend
	configStartTime := time.Now()
	optimizeConfigForBackend(backendName, strmConfig)
	fs.Infof(nil, "📊 [PERF] Config optimization took: %v", time.Since(configStartTime))

	// Create STRM filesystem
	fsCreateStartTime := time.Now()
	strmFS := NewSTRMFS(VFS, opt, strmConfig)
	fs.Infof(nil, "📊 [PERF] STRM filesystem creation took: %v", time.Since(fsCreateStartTime))

	// Create FUSE host
	fuseHostStartTime := time.Now()
	host := fuse.NewFileSystemHost(strmFS)
	host.SetCapReaddirPlus(true)

	// Set case sensitivity based on backend
	if opt.CaseInsensitive.Valid {
		host.SetCapCaseInsensitive(opt.CaseInsensitive.Value)
		fs.Infof(nil, "🔧 [CONFIG] Case insensitive: %v (explicit)", opt.CaseInsensitive.Value)
	} else {
		caseInsensitive := f.Features().CaseInsensitive
		host.SetCapCaseInsensitive(caseInsensitive)
		fs.Infof(nil, "🔧 [CONFIG] Case insensitive: %v (auto-detected)", caseInsensitive)
	}

	// Create mount options
	optionsStartTime := time.Now()
	options := createMountOptions(VFS, opt.DeviceName, mountpoint, opt)
	fs.Infof(nil, "📊 [PERF] FUSE host creation took: %v", time.Since(fuseHostStartTime))
	fs.Infof(nil, "📊 [PERF] Mount options creation took: %v", time.Since(optionsStartTime))
	fs.Infof(nil, "🔧 [CONFIG] FUSE mount options: %q", options)

	// Start mounting in background
	errChan := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fs.Errorf(nil, "💥 [ERROR] STRM mount panic: %v", r)
				errChan <- fmt.Errorf("strm-mount failed: %v", r)
			}
		}()

		mountStartTime := time.Now()
		fs.Infof(nil, "🚀 [PERF] Starting FUSE mount operation...")

		var err error
		ok := host.Mount(mountpoint, options)
		mountDuration := time.Since(mountStartTime)

		if !ok {
			err = fmt.Errorf("strm-mount failed")
			fs.Errorf(f, "❌ [ERROR] STRM mount failed after %v", mountDuration)
		} else {
			fs.Infof(nil, "✅ [PERF] STRM mount successful! Total mount time: %v", mountDuration)
			fs.Infof(nil, "📊 [PERF] Total initialization time: %v", time.Since(startTime))
		}
		errChan <- err
	}()

	// Unmount function
	unmount := func() error {
		strmFS.VFS.Shutdown()
		fs.Debugf(nil, "Calling STRM host.Unmount")
		if host.Unmount() {
			fs.Debugf(nil, "STRM unmounted successfully")
			return nil
		}
		return fmt.Errorf("strm host unmount failed")
	}

	return errChan, unmount, nil
}

// optimizeConfigForBackend optimizes configuration based on backend type
func optimizeConfigForBackend(backendName string, config *Config) {
	switch backendName {
	case "123":
		config.URLFormat = "123"
		fs.Debugf(nil, "🔧 Optimized for 123 backend: URL format = 123")
	case "115":
		config.URLFormat = "115"
		fs.Debugf(nil, "🔧 Optimized for 115 backend: URL format = 115")
	default:
		config.URLFormat = "path"
		fs.Debugf(nil, "🔧 Using path format for %s backend", backendName)
	}
}

// createMountOptions creates mount options for cgofuse
func createMountOptions(VFS *vfs.VFS, deviceName, mountpoint string, opt *mountlib.Options) []string {
	options := []string{
		"-o", fmt.Sprintf("fsname=%s", deviceName),
		"-o", "subtype=rclone-strm",
		"-o", fmt.Sprintf("attr_timeout=%g", time.Duration(opt.AttrTimeout).Seconds()),
	}

	// 设置自定义卷名称
	volumeName := opt.VolumeName
	if volumeName == "" {
		// 如果没有指定卷名称，使用remote名称
		remoteName := VFS.Fs().Name()
		if remoteName != "" {
			volumeName = fmt.Sprintf("%s (STRM)", remoteName)
		} else {
			volumeName = "STRM Mount"
		}
	}

	// 在macOS上设置volname
	if runtime.GOOS == "darwin" {
		options = append(options, "-o", fmt.Sprintf("volname=%s", volumeName))
	}

	if opt.DebugFUSE {
		options = append(options, "-o", "debug")
	}

	if opt.AllowOther {
		options = append(options, "-o", "allow_other")
	}

	if opt.AllowRoot {
		options = append(options, "-o", "allow_root")
	}

	if opt.DefaultPermissions {
		options = append(options, "-o", "default_permissions")
	}

	// Read-only mount for safety
	options = append(options, "-o", "ro")

	// 🛡️ QPS保护：禁用系统自动扫描，避免大量API调用
	switch runtime.GOOS {
	case "darwin":
		// macOS: 禁用Finder和Spotlight自动扫描
		options = append(options, "-o", "noappledouble") // 禁用Apple双叉文件
		options = append(options, "-o", "noapplexattr")  // 禁用Apple扩展属性
		// 注释掉nobrowse，让挂载点在Finder中可见
		// options = append(options, "-o", "nobrowse")      // 不在Finder侧边栏显示
		options = append(options, "-o", "noatime")     // 不更新访问时间
		options = append(options, "-o", "allow_other") // 允许其他用户访问
		// 设置用户权限，让当前用户可以访问
		options = append(options, "-o", fmt.Sprintf("uid=%d", os.Getuid())) // 设置用户ID
		options = append(options, "-o", fmt.Sprintf("gid=%d", os.Getgid())) // 设置组ID
	case "linux":
		// Linux: 禁用updatedb、Tracker、Baloo等自动索引
		options = append(options, "-o", "noatime")    // 不更新访问时间
		options = append(options, "-o", "nodiratime") // 不更新目录访问时间
		options = append(options, "-o", "nodev")      // 不解释设备文件
		options = append(options, "-o", "nosuid")     // 忽略suid位
		options = append(options, "-o", "noexec")     // 不允许执行文件
	case "windows":
		// Windows: 禁用Windows Search和Defender自动扫描
		options = append(options, "-o", "noatime") // 不更新访问时间
		// Windows特有的选项会由WinFSP处理
	default:
		// 其他系统: 基本的QPS保护
		options = append(options, "-o", "noatime") // 不更新访问时间
	}

	return options
}

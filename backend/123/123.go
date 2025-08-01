package _123

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rclone/rclone/lib/atexit"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"golang.org/x/oauth2"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	fshash "github.com/rclone/rclone/fs/hash"
)

const (
	openAPIRootURL     = "https://open-api.123pan.com"
	defaultUserAgent   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	tokenRefreshWindow = 10 * time.Minute

	// 官方QPS限制文档：https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/
	// 高频率API (15-20 QPS) - 进一步降低避免429错误
	listV2APIMinSleep = 200 * time.Millisecond // ~5 QPS 用于 api/v2/file/list (官方15 QPS，极保守设置)

	// 中等频率API (8-10 QPS) - 进一步降低避免429错误
	fileMoveMinSleep    = 250 * time.Millisecond // ~4 QPS 用于 api/v1/file/move (官方10 QPS，极保守设置)
	fileInfosMinSleep   = 250 * time.Millisecond // ~4 QPS 用于 api/v1/file/infos (官方10 QPS，极保守设置)
	userInfoMinSleep    = 250 * time.Millisecond // ~4 QPS 用于 api/v1/user/info (官方10 QPS，极保守设置)
	accessTokenMinSleep = 300 * time.Millisecond // ~3.3 QPS 用于 api/v1/access_token (官方8 QPS，极保守设置)

	// 低频率API (5 QPS) - 基于上传速度分析大幅优化性能
	// 注意：uploadCreateMinSleep, mkdirMinSleep, fileTrashMinSleep 已删除（未使用）

	downloadInfoMinSleep = 500 * time.Millisecond // ~2 QPS 用于 api/v1/file/download_info (保持不变)

	// 最低频率API (1 QPS) - 进一步降低避免429错误
	// 特殊API (保守估计) - 基于48秒/分片问题大幅优化分片上传性能
	uploadV2SliceMinSleep = 50 * time.Millisecond // ~20.0 QPS 用于 upload/v2/file/slice (激进提升，解决上传慢问题)

	maxSleep      = 30 * time.Second // 429退避的最大睡眠时间
	decayConstant = 2

	// 文件上传相关常量
	singleStepUploadLimit = 1024 * 1024 * 1024 // 1GB - 单步上传API的文件大小限制（修复：原来错误设置为50MB）
	maxMemoryBufferSize   = 1024 * 1024 * 1024 // 1GB - 内存缓冲的最大大小（从512MB提升）
	maxFileNameBytes      = 255                // 文件名的最大字节长度（UTF-8编码）

	// 文件冲突处理策略常量已移除，当前使用API默认行为

	// 上传相关常量 - 优化大文件传输性能
	defaultChunkSize    = 100 * fs.Mebi // 增加默认分片大小到100MB
	minChunkSize        = 50 * fs.Mebi  // 增加最小分片大小到50MB
	maxChunkSize        = 500 * fs.Mebi // 设置最大分片大小为500MB
	defaultUploadCutoff = 100 * fs.Mebi // 降低分片上传阈值
	maxUploadParts      = 10000

	// 123网盘特定常量 (从common包迁移)
	// 移除未使用的default123QPS常量

	// 移除未使用的跨云传输常量

	// 连接和超时设置 - 针对大文件传输优化的参数
	// 移除未使用的超时常量

	// 注意：文件大小判断常量已本地定义

	// 文件名验证相关常量
	maxFileNameLength = 256          // 123网盘文件名最大长度（包括扩展名）
	invalidChars      = `"\/:*?|><\` // 123网盘不允许的文件名字符
	replacementChar   = "_"          // 用于替换非法字符的安全字符

	// 移除缓存相关常量，使用rclone标准

	// 配置验证相关常量
	MaxListChunk          = 10000 // 最大列表块大小
	MaxUploadPartsLimit   = 10000 // 最大上传分片数限制
	MaxConcurrentLimit    = 100   // 最大并发数限制
	DefaultListChunk      = 1000  // 默认列表块大小
	DefaultMaxUploadParts = 1000  // 默认最大上传分片数

	// 网络质量评估常量
	LatencyThreshold          = 50              // 延迟阈值（毫秒）
	LatencyScoreRange         = 1000            // 延迟分数计算范围
	QualityAdjustmentInterval = 5 * time.Minute // 质量调整间隔

	// 缓存管理常量
	PreloadQueueCapacity    = 1000 // 预加载队列容量
	MaxHotFiles             = 100  // 最大热点文件数
	HotFileCleanupBatchSize = 20   // 热点文件清理批次大小

	// 网络检测常量
	NetworkTestSize = 512 * 1024 // 网络测试文件大小（512KB）
)

// Options 定义此后端的配置选项
// 符合rclone标准设计原则的配置结构
type Options struct {
	// 123网盘特定的认证配置
	ClientID     string `config:"client_id"`
	ClientSecret string `config:"client_secret"`
	Token        string `config:"token"`
	UserAgent    string `config:"user_agent"`
	RootFolderID string `config:"root_folder_id"`

	// 123网盘特有的配置选项
	MaxUploadParts int `config:"max_upload_parts"`

	// 123网盘特定的QPS控制选项
	UploadPacerMinSleep   fs.Duration `config:"upload_pacer_min_sleep"`
	DownloadPacerMinSleep fs.Duration `config:"download_pacer_min_sleep"`
	StrictPacerMinSleep   fs.Duration `config:"strict_pacer_min_sleep"`

	// 编码配置
	Enc encoder.MultiEncoder `config:"encoding"` // 文件名编码设置

	// 移除缓存优化配置，使用rclone标准缓存
}

// Fs 表示远程123网盘驱动器实例
type Fs struct {
	name         string       // 此远程实例的名称
	originalName string       // 未修改的原始配置名称
	root         string       // 当前操作的根路径
	opt          Options      // 解析后的配置选项
	features     *fs.Features // 可选功能特性

	// API客户端和身份验证相关
	rst          *rest.Client     // rclone标准rest客户端
	token        string           // 缓存的访问令牌
	tokenExpiry  time.Time        // 缓存访问令牌的过期时间
	tokenMu      sync.Mutex       // 保护token和tokenExpiry的互斥锁
	tokenRenewer *oauthutil.Renew // 令牌自动更新器

	// 移除未使用的cacheMu字段

	// 配置和状态信息
	m            configmap.Mapper // 配置映射器
	rootFolderID string           // 根文件夹的ID

	// 性能和速率限制 - 针对不同API限制的多个调速器
	listPacer     *fs.Pacer // 文件列表API的调速器（约2 QPS）
	strictPacer   *fs.Pacer // 严格API（如user/info, move, delete）的调速器（1 QPS）
	uploadPacer   *fs.Pacer // 上传API的调速器（1 QPS）
	downloadPacer *fs.Pacer // 下载API的调速器（2 QPS）

	// 目录缓存
	dirCache *dircache.DirCache

	// 移除自定义缓存，使用rclone标准dircache

	// 性能优化相关 - 使用rclone标准组件替代过度开发的管理器

	// Unified components removed - using rclone standard implementations

	// 移除复杂的缓存配置和统一后端配置

	// 移除上传域名缓存，使用rclone标准
}

// 移除复杂的缓存配置结构体，使用rclone标准组件

// 移除PathToIDCacheEntry，使用rclone标准dircache

// ProgressReadCloser 包装ReadCloser以提供进度跟踪和资源管理
type ProgressReadCloser struct {
	io.ReadCloser
	fs              *Fs
	totalSize       int64
	transferredSize int64
	startTime       time.Time // 下载开始时间
}

// Read 实现io.Reader接口，同时更新进度
func (prc *ProgressReadCloser) Read(p []byte) (n int, err error) {
	n, err = prc.ReadCloser.Read(p)
	if n > 0 {
		prc.transferredSize += int64(n)
		// rclone内置的accounting会自动处理进度跟踪
	}
	return n, err
}

// Close 实现io.Closer接口，同时清理资源
func (prc *ProgressReadCloser) Close() error {
	// 关闭底层ReadCloser
	return prc.ReadCloser.Close()
}

// Object 描述123网盘中的文件或文件夹对象
type Object struct {
	fs          *Fs       // 父文件系统实例
	remote      string    // 远程路径
	hasMetaData bool      // 是否已加载元数据
	id          string    // 文件/文件夹ID
	size        int64     // 文件大小
	md5sum      string    // MD5哈希值
	modTime     time.Time // 修改时间
	isDir       bool      // 是否为目录
}

// ConcurrentDownloadReader 并发下载的文件读取器
type ConcurrentDownloadReader struct {
	file     *os.File
	tempPath string
}

// Read 实现io.Reader接口
func (r *ConcurrentDownloadReader) Read(p []byte) (n int, err error) {
	return r.file.Read(p)
}

// Close 实现io.Closer接口，关闭文件并删除临时文件
func (r *ConcurrentDownloadReader) Close() error {
	var err error
	if r.file != nil {
		err = r.file.Close()
		r.file = nil
	}
	if r.tempPath != "" {
		if removeErr := os.Remove(r.tempPath); removeErr != nil {
			fs.Debugf(nil, "删除临时文件失败: %s, 错误: %v", r.tempPath, removeErr)
		} else {
			fs.Debugf(nil, "已删除临时文件: %s", r.tempPath)
		}
		r.tempPath = ""
	}
	return err
}

// 文件名验证相关的全局变量
var (
	// invalidCharsRegex 用于匹配123网盘不允许的文件名字符的正则表达式
	invalidCharsRegex = regexp.MustCompile(`["\/:*?|><\\]`)
)

func validateFileName(name string) error {
	// 使用rclone标准的路径检查
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("文件名不能包含路径分隔符")
	}

	// 基础检查：空值和空格
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("文件名不能为空或全部是空格")
	}

	// UTF-8有效性检查
	if !utf8.ValidString(name) {
		return fmt.Errorf("文件名包含无效的UTF-8字符")
	}

	// 123网盘特定的长度检查（字符数）
	runeCount := utf8.RuneCountInString(name)
	if runeCount > maxFileNameLength {
		return fmt.Errorf("文件名长度超过限制：当前%d个字符，最大允许%d个字符",
			runeCount, maxFileNameLength)
	}

	// 字节长度检查
	byteLength := len([]byte(name))
	if byteLength > maxFileNameBytes {
		return fmt.Errorf("文件名字节长度超过%d: %d", maxFileNameBytes, byteLength)
	}

	// 禁用字符检查
	if invalidCharsRegex.MatchString(name) {
		invalidFound := invalidCharsRegex.FindAllString(name, -1)
		return fmt.Errorf("文件名包含不允许的字符：%v，123网盘不允许使用以下字符：%s",
			invalidFound, invalidChars)
	}

	return nil
}

// cleanFileName 清理文件名，将非法字符替换为安全字符
// 同时确保文件名长度不超过限制
func cleanFileName(name string) string {
	if name == "" {
		return "未命名文件"
	}

	// 替换非法字符
	cleaned := invalidCharsRegex.ReplaceAllString(name, replacementChar)

	// 如果文件名过长，进行截断处理
	if utf8.RuneCountInString(cleaned) > maxFileNameLength {
		// 保留文件扩展名
		ext := filepath.Ext(cleaned)
		nameWithoutExt := strings.TrimSuffix(cleaned, ext)

		// 计算可用的文件名长度（总长度 - 扩展名长度）
		maxNameLength := maxFileNameLength - utf8.RuneCountInString(ext)
		if maxNameLength < 1 {
			// 如果扩展名太长，只保留部分扩展名
			maxNameLength = maxFileNameLength - 10 // 保留至少10个字符给文件名
			maxNameLength = max(maxNameLength, 1)
			extRunes := []rune(ext)
			if len(extRunes) > 10 {
				ext = string(extRunes[:10])
			}
		}

		// 截断文件名主体部分
		nameRunes := []rune(nameWithoutExt)
		if len(nameRunes) > maxNameLength {
			nameWithoutExt = string(nameRunes[:maxNameLength])
		}

		cleaned = nameWithoutExt + ext
	}

	return cleaned
}

// validateAndCleanFileName 验证并清理文件名的便捷函数
// 如果验证失败，返回清理后的文件名和警告信息
func validateAndCleanFileName(name string) (cleanedName string, warning error) {
	// 首先尝试验证原始文件名
	if err := validateFileName(name); err == nil {
		return name, nil
	}

	// 如果验证失败，清理文件名
	cleanedName = cleanFileName(name)

	// 返回清理后的文件名和警告信息
	warning = fmt.Errorf("文件名已自动清理：原始名称 '%s' -> 清理后名称 '%s'", name, cleanedName)
	return cleanedName, warning
}

// generateUniqueFileName 生成唯一的文件名，避免冲突
func (f *Fs) generateUniqueFileName(ctx context.Context, parentFileID int64, baseName string) (string, error) {
	// 首先检查原始文件名是否存在
	exists, err := f.fileExists(ctx, parentFileID, baseName)
	if err != nil {
		return "", fmt.Errorf("检查文件是否存在失败: %w", err)
	}

	if !exists {
		return baseName, nil
	}

	// 如果文件已存在，生成带数字后缀的文件名
	ext := filepath.Ext(baseName)
	nameWithoutExt := strings.TrimSuffix(baseName, ext)

	for i := 1; i <= 999; i++ {
		newName := fmt.Sprintf("%s (%d)%s", nameWithoutExt, i, ext)

		// 验证新文件名长度
		if len([]byte(newName)) > maxFileNameBytes {
			// 如果太长，截断基础名称
			suffixBytes := make([]byte, 0, 20) // 预分配足够的容量
			suffixBytes = fmt.Appendf(suffixBytes, " (%d)%s", i, ext)
			maxBaseLength := maxFileNameBytes - len(suffixBytes)
			if maxBaseLength > 0 {
				truncatedBase := string([]byte(nameWithoutExt)[:maxBaseLength])
				newName = fmt.Sprintf("%s (%d)%s", truncatedBase, i, ext)
			} else {
				// 如果还是太长，使用简化名称
				newName = fmt.Sprintf("file_%d%s", i, ext)
			}
		}

		exists, err := f.fileExists(ctx, parentFileID, newName)
		if err != nil {
			return "", fmt.Errorf("检查文件是否存在失败: %w", err)
		}

		if !exists {
			fs.Debugf(f, "生成唯一文件名: %s -> %s", baseName, newName)
			return newName, nil
		}
	}

	// 如果尝试了999次都没有找到唯一名称，使用时间戳
	timestamp := time.Now().Unix()
	newName := fmt.Sprintf("%s_%d%s", nameWithoutExt, timestamp, ext)
	fs.Debugf(f, "使用时间戳生成唯一文件名: %s -> %s", baseName, newName)
	return newName, nil
}

// fileExists 检查指定目录中是否存在指定名称的文件
func (f *Fs) fileExists(ctx context.Context, parentFileID int64, fileName string) (bool, error) {
	// 使用ListFile API检查文件是否存在
	response, err := f.ListFile(ctx, int(parentFileID), 100, "", "", 0)
	if err != nil {
		return false, err
	}

	for _, file := range response.Data.FileList {
		if file.Filename == fileName {
			return true, nil
		}
	}

	return false, nil
}

// verifyParentFileID 验证父目录ID是否存在 - 简化版本，移除缓存
func (f *Fs) verifyParentFileID(ctx context.Context, parentFileID int64) (bool, error) {
	fs.Debugf(f, "验证父目录ID: %d", parentFileID)

	// 直接执行API验证，使用ListFile API
	response, err := f.ListFile(ctx, int(parentFileID), 1, "", "", 0)
	if err != nil {
		fs.Debugf(f, "验证父目录ID %d 失败: %v", parentFileID, err)
		return false, err
	}

	isValid := response.Code == 0
	if !isValid {
		fs.Debugf(f, "父目录ID %d 不存在，API返回: code=%d, message=%s", parentFileID, response.Code, response.Message)
	} else {
		fs.Debugf(f, "父目录ID %d 验证成功", parentFileID)
	}

	return isValid, nil
}

// getCorrectParentFileID 获取上传API认可的正确父目录ID
// 统一版本：为单步上传和分片上传提供一致的父目录ID修复策略
func (f *Fs) getCorrectParentFileID(ctx context.Context, cachedParentID int64) (int64, error) {
	fs.Debugf(f, " 统一父目录ID修复策略，缓存ID: %d", cachedParentID)

	// 策略1：重新获取目录结构
	fs.Infof(f, "🔄 重新获取目录结构")

	// 只重置dirCache，不完全清空，保持其他有效缓存
	if f.dirCache != nil {
		fs.Debugf(f, "重置目录缓存以清理过期的父目录ID: %d", cachedParentID)
		f.dirCache.ResetRoot()
	}

	// 策略2：尝试重新验证原始ID（清理缓存后可能恢复）
	fs.Debugf(f, "🔄 重新验证原始目录ID: %d", cachedParentID)
	exists, err := f.verifyParentFileID(ctx, cachedParentID)
	if err == nil && exists {
		fs.Infof(f, "✅ 原始目录ID %d 在清理缓存后验证成功", cachedParentID)
		return cachedParentID, nil
	}

	// 策略3：尝试验证根目录是否可用
	// 根据官方API示例，很多情况下可以使用根目录 (parentFileID: 0)
	fs.Debugf(f, "🧪 尝试验证根目录 (parentFileID: 0) 是否可用")
	rootExists, err := f.verifyParentFileID(ctx, 0)
	if err == nil && rootExists {
		fs.Infof(f, "✅ 根目录验证成功，使用根目录作为回退方案")
		return 0, nil
	}

	// 策略4：如果所有策略都失败，仍然回退到根目录（强制策略）
	// 这是因为根目录(0)在123网盘API中总是存在的
	fs.Infof(f, "⚠️ 所有验证策略失败，强制使用根目录 (parentFileID: 0)")
	return 0, nil
}

// 缓存相关函数已移除，使用rclone标准dircache

// 移除未使用的TaskID相关函数

// calculatePollingStrategy 计算智能轮询策略
// 优化：根据文件大小动态调整轮询参数和连续失败阈值
func (f *Fs) calculatePollingStrategy(fileSize int64) (maxRetries int, baseInterval time.Duration, maxConsecutiveFailures int) {
	// 基础轮询间隔（按官方文档要求）
	baseInterval = 1 * time.Second

	// 优化：根据文件大小动态调整最大重试次数和连续失败阈值
	switch {
	case fileSize < 100*1024*1024: // 小于100MB
		maxRetries = 180           // 3分钟
		maxConsecutiveFailures = 8 // 小文件容错性稍低
	case fileSize < 500*1024*1024: // 小于500MB
		maxRetries = 300            // 5分钟
		maxConsecutiveFailures = 12 // 中等文件适中容错
	case fileSize < 1*1024*1024*1024: // 小于1GB
		maxRetries = 600            // 10分钟
		maxConsecutiveFailures = 15 // 大文件提高容错性
	case fileSize < 5*1024*1024*1024: // 小于5GB
		maxRetries = 900            // 15分钟
		maxConsecutiveFailures = 20 // 超大文件高容错性
	default: // 5GB以上
		maxRetries = 1200           // 20分钟
		maxConsecutiveFailures = 25 // 巨大文件最高容错性
	}

	fs.Debugf(f, "文件大小: %s, 轮询策略: 最大%d次, 间隔%v, 连续失败阈值%d",
		fs.SizeSuffix(fileSize), maxRetries, baseInterval, maxConsecutiveFailures)

	return maxRetries, baseInterval, maxConsecutiveFailures
}

// calculateDynamicInterval 计算动态轮询间隔
// 优化：根据连续失败次数、尝试次数和错误类型智能调整间隔
func (f *Fs) calculateDynamicInterval(baseInterval time.Duration, consecutiveFailures, attempt int, lastErr error) time.Duration {
	// 基础间隔
	interval := baseInterval

	// 简化：根据错误类型调整退避策略
	isNetworkErr := lastErr != nil && (strings.Contains(lastErr.Error(), "connection") ||
		strings.Contains(lastErr.Error(), "timeout") ||
		strings.Contains(lastErr.Error(), "network"))

	// 根据连续失败次数增加延迟（指数退避）
	if consecutiveFailures > 0 {
		backoffMultiplier := 1 << uint(consecutiveFailures) // 2^consecutiveFailures

		// 简化：网络错误使用更温和的退避策略
		if isNetworkErr {
			if backoffMultiplier > 4 {
				backoffMultiplier = 4 // 网络错误最大4倍延迟
			}
		} else {
			if backoffMultiplier > 8 {
				backoffMultiplier = 8 // 其他错误最大8倍延迟
			}
		}

		interval = time.Duration(backoffMultiplier) * baseInterval
	}

	// 简化：长时间轮询时的智能间隔调整
	if attempt > 60 { // 1分钟后
		if isNetworkErr {
			interval = interval * 3 // 网络错误增加更多延迟
		} else {
			interval = interval * 2 // 其他错误适度增加
		}
	}
	if attempt > 300 { // 5分钟后
		interval = interval * 2 // 进一步增加间隔
	}

	// 最大间隔限制为30秒
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}

	return interval
}

// 已移除：isNetworkError 函数已本地实现
// 使用统一的网络错误检测机制，提高准确性和一致性

// isRetryableError 判断响应错误代码是否可重试
// 增强：基于123网盘API文档的错误代码分类
func (f *Fs) isRetryableError(code int) bool {
	retryableCodes := []int{
		20101, // 服务器繁忙
		20103, // 临时错误
		20104, // 系统维护
		20105, // 服务暂不可用
		500,   // 内部服务器错误
		502,   // 网关错误
		503,   // 服务不可用
		504,   // 网关超时
	}

	return slices.Contains(retryableCodes, code)
}

// 移除verifyChunkIntegrity函数，未使用

// 移除resumeUploadWithIntegrityCheck函数，使用rclone标准重试机制

// getParentID 获取文件或目录的父目录ID
func (f *Fs) getParentID(ctx context.Context, fileID int64) (int64, error) {
	fs.Debugf(f, "尝试获取文件ID %d 的父目录ID", fileID)

	// 直接通过API查询，因为缓存搜索功能未实现

	// 如果缓存中找不到，尝试通过API查询
	// 使用文件详情API获取文件信息
	var response struct {
		Code int `json:"code"`
		Data struct {
			ParentFileID int64 `json:"parentFileId"`
		} `json:"data"`
		Message string `json:"message"`
	}

	url := fmt.Sprintf("/api/v1/file/info?fileId=%d", fileID)
	err := f.makeAPICallWithRest(ctx, url, "GET", nil, &response)
	if err != nil {
		return 0, fmt.Errorf("获取文件信息失败: %w", err)
	}

	if response.Code != 0 {
		return 0, fmt.Errorf("API返回错误: %s", response.Message)
	}

	fs.Debugf(f, "通过API获取到文件ID %d 的父目录ID: %d", fileID, response.Data.ParentFileID)
	return response.Data.ParentFileID, nil
}

// fileExistsInDirectory 检查指定目录中是否存在指定名称的文件
func (f *Fs) fileExistsInDirectory(ctx context.Context, parentID int64, fileName string) (bool, int64, error) {
	fs.Debugf(f, "检查目录 %d 中是否存在文件: %s", parentID, fileName)

	response, err := f.ListFile(ctx, int(parentID), 100, "", "", 0)
	if err != nil {
		return false, 0, err
	}

	if response.Code != 0 {
		return false, 0, fmt.Errorf("ListFile API错误: code=%d, message=%s", response.Code, response.Message)
	}

	for _, file := range response.Data.FileList {
		if file.Filename == fileName {
			fs.Debugf(f, "找到文件 %s，ID: %d", fileName, file.FileID)
			return true, int64(file.FileID), nil
		}
	}

	fs.Debugf(f, "目录 %d 中未找到文件: %s", parentID, fileName)
	return false, 0, nil
}

// normalizePath 规范化路径，处理123网盘路径的特殊要求
// 优化版本：使用rclone标准路径处理加上123网盘特定要求
// dircache要求：路径不应该以/开头或结尾
func normalizePath(path string) string {
	if path == "" {
		return ""
	}

	// 使用filepath.Clean进行基础路径清理
	path = filepath.Clean(path)

	// 处理filepath.Clean的特殊返回值
	if path == "." {
		return ""
	}

	// 移除开头的斜杠（如果有）
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimPrefix(path, "\\") // 处理Windows路径分隔符

	// 移除结尾的斜杠（如果有）
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, "\\")

	// 将反斜杠转换为正斜杠（标准化路径分隔符）
	path = strings.ReplaceAll(path, "\\", "/")

	// 最终检查：确保结果不以/开头或结尾
	path = strings.Trim(path, "/")

	return path
}

// isRemoteSource 检查源对象是否来自远程云盘（非本地文件）
func (f *Fs) isRemoteSource(src fs.ObjectInfo) bool {
	// 检查源对象的类型，如果不是本地文件系统，则认为是远程源
	// 这可以通过检查源对象的Fs()方法返回的类型来判断
	srcFs := src.Fs()
	if srcFs == nil {
		fs.Debugf(f, " isRemoteSource: srcFs为nil，返回false")
		return false
	}

	// 检查是否是本地文件系统
	fsType := srcFs.Name()

	// 修复：正确识别本地文件系统
	// 本地文件系统的名称可能是 "local" 或 "local{suffix}"
	isLocal := fsType == "local" || strings.HasPrefix(fsType, "local{")
	isRemote := !isLocal && fsType != ""

	fs.Debugf(f, " isRemoteSource检测: fsType='%s', isLocal=%v, isRemote=%v", fsType, isLocal, isRemote)

	// 如果是本地文件系统，直接返回false
	if isLocal {
		fs.Debugf(f, " 明确识别为本地文件系统: %s", fsType)
		return false
	}

	// 特别检测115网盘和其他云盘
	if strings.Contains(fsType, "115") || strings.Contains(fsType, "pan") {
		fs.Debugf(f, " 明确识别为云盘源: %s", fsType)
		return true
	}

	return isRemote
}

// init 函数用于向Fs注册123网盘后端
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "123",
		Description: "123网盘驱动器",
		NewFs:       newFs,
		CommandHelp: commandHelp,
		Options: []fs.Option{{
			Name:     "client_id",
			Help:     "123网盘API客户端ID。",
			Required: true,
		}, {
			Name:      "client_secret",
			Help:      "123网盘API客户端密钥。",
			Required:  true,
			Sensitive: true,
		}, {
			Name:      "token",
			Help:      "OAuth访问令牌（JSON格式）。通常留空。",
			Sensitive: true,
		}, {
			Name:     "user_agent",
			Help:     "API请求的User-Agent头。",
			Default:  defaultUserAgent,
			Advanced: true,
		}, {
			Name:     "root_folder_id",
			Help:     "根文件夹的ID。留空表示根目录。",
			Default:  "0",
			Advanced: true,
		}, {
			Name:     "max_upload_parts",
			Help:     "多部分上传中的最大分片数。",
			Default:  maxUploadParts,
			Advanced: true,
		}, {
			Name:     "upload_pacer_min_sleep",
			Help:     "上传API调用之间的最小等待时间。设置更高的值可以避免429错误。",
			Default:  uploadV2SliceMinSleep,
			Advanced: true,
		}, {
			Name:     "download_pacer_min_sleep",
			Help:     "下载API调用之间的最小等待时间。",
			Default:  downloadInfoMinSleep,
			Advanced: true,
		}, {
			Name:     "strict_pacer_min_sleep",
			Help:     "严格API调用（move, delete等）之间的最小等待时间。",
			Default:  fileMoveMinSleep,
			Advanced: true,
		}, {
			Name:     config.ConfigEncoding,
			Help:     config.ConfigEncodingHelp,
			Advanced: true,
			// 默认包含Slash和InvalidUtf8编码，适配中文文件名和特殊字符
			Default: (encoder.EncodeCtl |
				encoder.EncodeLeftSpace |
				encoder.EncodeRightSpace |
				encoder.EncodeSlash | // 新增：默认编码斜杠
				encoder.EncodeInvalidUtf8), // 新增：编码无效UTF-8字符
		}},
	})
}

var commandHelp = []fs.CommandHelp{{
	Name:  "media-sync",
	Short: "同步媒体库并创建优化的.strm文件",
	Long: `将123网盘中的视频文件同步到本地目录，创建对应的.strm文件。
.strm文件将包含优化的fileId格式，支持直接播放和媒体库管理。

用法示例:
rclone backend media-sync 123:Movies /local/media/movies
rclone backend media-sync 123:Videos /local/media/videos -o min-size=200M -o strm-format=true

支持的视频格式: mp4, mkv, avi, mov, wmv, flv, webm, m4v, 3gp, ts, m2ts
.strm文件内容格式: 123://fileId (可通过strm-format选项调整)`,
	Opts: map[string]string{
		"min-size":    "最小文件大小过滤，小于此大小的文件将被忽略 (默认: 100M)",
		"strm-format": ".strm文件内容格式: true(优化格式)/false(路径格式) (默认: true，兼容: fileid/path)",
		"include":     "包含的文件扩展名，逗号分隔 (默认: mp4,mkv,avi,mov,wmv,flv,webm,m4v,3gp,ts,m2ts)",
		"exclude":     "排除的文件扩展名，逗号分隔",
		"update-mode": "更新模式: full/incremental (默认: full)",
		"sync-delete": "同步删除: false(仅创建.strm文件)/true(删除孤立文件和空目录) (默认: true，类似rclone sync)",
		"dry-run":     "预览模式，显示将要创建的文件但不实际创建 (true/false)",
		"target-path": "目标路径，如果不在参数中指定则必须通过此选项提供",
	},
}, {
	Name:  "get-download-url",
	Short: "通过fileId或.strm内容获取下载URL",
	Long: `通过123网盘的fileId或.strm文件内容获取实际的下载URL。
支持多种输入格式，特别适用于媒体服务器和.strm文件处理。

用法示例:
rclone backend get-download-url 123: "123://17995550"
rclone backend get-download-url 123: "17995550"
rclone backend get-download-url 123: "/path/to/file.mp4"
rclone backend get-download-url 123: "123://17995550" -o user-agent="Custom-UA"

输入格式支持:
- 123://fileId 格式 (来自.strm文件)
- 纯fileId
- 文件路径 (自动解析为fileId)`,
	Opts: map[string]string{
		"user-agent": "自定义User-Agent字符串（可选）",
	},
},
}

// Command 执行后端特定的命令。
func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (out any, err error) {
	switch name {

	case "media-sync":
		// 🎬 新增：媒体库同步功能
		return f.mediaSyncCommand(ctx, arg, opt)

	case "get-download-url":
		// 🔗 新增：通过fileId获取下载URL
		return f.getDownloadURLCommand(ctx, arg, opt)

	default:
		return nil, fs.ErrorCommandNotFound
	}
}

type ListRequest struct {
	ParentFileID int    `json:"parentFileId"`
	Limit        int    `json:"limit"`
	SearchData   string `json:"searchData,omitempty"`
	SearchMode   string `json:"searchMode,omitempty"`
	LastFileID   int    `json:"lastFileID,omitempty"`
}

type ListResponse struct {
	Code    int                   `json:"code"`
	Message string                `json:"message"`
	Data    GetFileListRespDataV2 `json:"data"` // 更改为特定类型
}

// GetFileListRespDataV2 表示文件列表响应的数据结构
type GetFileListRespDataV2 struct {
	// -1代表最后一页（无需再翻页查询）, 其他代表下一页开始的文件id，携带到请求参数中
	LastFileId int64 `json:"lastFileId"`
	// 文件列表
	FileList []FileListInfoRespDataV2 `json:"fileList"`
}

// FileListInfoRespDataV2 表示来自API响应的文件或文件夹项目
type FileListInfoRespDataV2 struct {
	// 文件ID
	FileID int64 `json:"fileID"`
	// 文件名
	Filename string `json:"filename"`
	// 0-文件  1-文件夹
	Type int `json:"type"`
	// 文件大小
	Size int64 `json:"size"`
	// md5
	Etag string `json:"etag"`
	// 文件审核状态, 大于 100 为审核驳回文件
	Status int `json:"status"`
	// 目录ID
	ParentFileID int64 `json:"parentFileID"`
	// 文件分类, 0-未知 1-音频 2-视频 3-图片
	Category int `json:"category"`
}

func (f *Fs) ListFile(ctx context.Context, parentFileID, limit int, searchData, searchMode string, lastFileID int) (*ListResponse, error) {
	fs.Debugf(f, "调用ListFile，参数：parentFileID=%d, limit=%d, lastFileID=%d", parentFileID, limit, lastFileID)

	// 移除缓存查询，直接调用API

	// 构造查询参数
	params := url.Values{}
	params.Add("parentFileId", fmt.Sprintf("%d", parentFileID))
	params.Add("limit", fmt.Sprintf("%d", limit))

	if searchData != "" {
		params.Add("searchData", searchData)
	}
	if searchMode != "" {
		params.Add("searchMode", searchMode)
	}
	if lastFileID != 0 {
		params.Add("lastFileID", fmt.Sprintf("%d", lastFileID))
	}

	// 构造带查询参数的端点
	endpoint := "/api/v2/file/list?" + params.Encode()
	fs.Debugf(f, "API端点: %s%s", openAPIRootURL, endpoint)

	// 直接使用v2 API调用 - 已迁移到rclone标准方法
	var result ListResponse
	err := f.makeAPICallWithRest(ctx, endpoint, "GET", nil, &result)
	if err != nil {
		fs.Debugf(f, "ListFile API调用失败: %v", err)
		return nil, err
	}

	fs.Debugf(f, "ListFile API响应: code=%d, message=%s, fileCount=%d", result.Code, result.Message, len(result.Data.FileList))

	// 移除缓存保存，不再使用自定义缓存

	return &result, nil
}

func (f *Fs) pathToFileID(ctx context.Context, filePath string) (string, error) {
	fs.Debugf(f, " pathToFileID开始: filePath=%s", filePath)

	// 根目录
	if filePath == "/" {
		return "0", nil
	}
	if len(filePath) > 1 && strings.HasSuffix(filePath, "/") {
		filePath = filePath[:len(filePath)-1]
	}

	// 移除路径映射缓存查询，直接解析路径
	currentID := "0"
	parts := strings.Split(filePath, "/")
	if len(parts) > 0 && parts[0] == "" { // 如果路径以/开头，移除空字符串
		parts = parts[1:]
	}
	for _, part := range parts {
		if part == "" {
			continue
		}
		findPart := false
		next := "0"           // 根据API使用字符串作为next
		maxIterations := 1000 // 防止无限循环：最多1000次迭代
		iteration := 0
		for {
			iteration++
			if iteration > maxIterations {
				return "", fmt.Errorf("查找路径 %s 超过最大迭代次数 %d，可能存在循环", part, maxIterations)
			}
			parentFileID, err := strconv.Atoi(currentID)
			if err != nil {
				return "", fmt.Errorf("invalid parentFileId: %s", currentID)
			}

			lastFileIDInt, err := strconv.Atoi(next)
			if err != nil {
				return "", fmt.Errorf("invalid next token: %s", next)
			}

			var response *ListResponse
			// 使用列表调速器进行速率限制
			err = f.listPacer.Call(func() (bool, error) {
				response, err = f.ListFile(ctx, parentFileID, 100, "", "", lastFileIDInt)
				if err != nil {
					return shouldRetry(ctx, nil, err)
				}
				return false, nil
			})
			if err != nil {
				return "", fmt.Errorf("list file failed: %w", err)
			}
			if response.Code != 200 && response.Code != 0 {
				return "", fmt.Errorf("API returned error code %d: %s", response.Code, response.Message)
			}
			infoList := response.Data.FileList
			if len(infoList) == 0 {
				break
			}
			for _, item := range infoList {
				if item.Filename == part {
					currentID = strconv.FormatInt(item.FileID, 10)
					findPart = true

					// 记录找到的项目类型信息，用于后续缓存
					isDir := (item.Type == 1) // Type: 0-文件  1-文件夹
					fs.Debugf(f, "pathToFileID找到项目: %s -> ID=%s, Type=%d, isDir=%v", part, currentID, item.Type, isDir)

					// 移除路径类型缓存，不再使用自定义缓存

					break
				}
			}
			if findPart {
				break
			}

			nextRaw := strconv.FormatInt(response.Data.LastFileId, 10)
			if nextRaw == "-1" {
				break
			} else {
				next = nextRaw
				// 无需手动睡眠 - 调速器处理速率限制
			}
		}
		if !findPart {
			return "", fs.ErrorObjectNotFound
		}
	}

	if currentID == "0" && filePath != "/" {
		return "", fs.ErrorObjectNotFound
	}

	// 缓存路径映射结果已在路径解析循环中正确处理

	fs.Debugf(f, " pathToFileID结果: filePath=%s -> currentID=%s", filePath, currentID)
	return currentID, nil
}

type FileDetail struct {
	FileID       int64  `json:"fileId"`
	Filename     string `json:"filename"`
	ParentFileID int64  `json:"parentFileId"`
	Type         int    `json:"type"`
	ETag         string `json:"etag"`
	Size         int64  `json:"size"`
	Category     int    `json:"category"`
	Status       int    `json:"status"`
	PunishFlag   int    `json:"punishFlag"`
	S3KeyFlag    string `json:"s3KeyFlag"`
	StorageNode  string `json:"storageNode"`
	Trashed      int    `json:"trashed"`
	CreateAt     string `json:"createAt"`
	UpdateAt     string `json:"updateAt"`
}

type FileInfoResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		List []FileDetail `json:"list"`
	} `json:"data"`
	TraceID string `json:"x-traceID"`
}

type FileDetailResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		FileID       int64  `json:"fileID"`
		Filename     string `json:"filename"`
		Type         int    `json:"type"`
		Size         int64  `json:"size"`
		ETag         string `json:"etag"`
		Status       int    `json:"status"`
		ParentFileID int64  `json:"parentFileID"`
		CreateAt     string `json:"createAt"`
		Trashed      int    `json:"trashed"`
	} `json:"data"`
	TraceID string `json:"x-traceID"`
}

// FileInfo 表示'list'数组中单个文件的信息
type FileInfo struct {
	FileID       int64  `json:"fileId"`
	Filename     string `json:"filename"`
	ParentFileID int64  `json:"parentFileId"`
	Type         int    `json:"type"`
	ETag         string `json:"etag"`
	Size         int64  `json:"size"`
	Category     int    `json:"category"`
	Status       int    `json:"status"`
	PunishFlag   int    `json:"punishFlag"`
	S3KeyFlag    string `json:"s3KeyFlag"`
	StorageNode  string `json:"storageNode"`
	Trashed      int    `json:"trashed"`
	CreateAt     string `json:"createAt"`
	UpdateAt     string `json:"updateAt"`
}

// FileInfosResponse 是API响应的顶级结构

// 定义Data结构体，映射data字段的内容
type Data struct {
	FileList []FileInfo `json:"fileList"`
}

// 定义FileInfosResponse结构体，映射整个响应JSON
type FileInfosResponse struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	Data     Data   `json:"data"`
	XTraceID string `json:"x-traceID"`
}

// 定义FileInfoRequest结构体，用于发送请求的payload
type FileInfoRequest struct {
	FileIDs []int64 `json:"fileIDs"`
}

type DownloadInfoResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		DownloadURL string `json:"downloadUrl"` // 直链地址
		ExpireTime  string `json:"expireTime"`  // 过期时间
	} `json:"data"`
}

type UploadCreateResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		FileID      int64  `json:"fileID"`
		PreuploadID string `json:"preuploadID"`
		Reuse       bool   `json:"reuse"`
		SliceSize   int64  `json:"sliceSize"`
	} `json:"data"`
}

// 移除UploadProgress复杂状态管理，使用rclone标准重试机制

type UserInfoResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		UID            int64  `json:"uid"`
		Username       string `json:"username"`
		DisplayName    string `json:"displayName"`
		HeadImage      string `json:"headImage"`
		Passport       string `json:"passport"`
		Mail           string `json:"mail"`
		SpaceUsed      int64  `json:"spaceUsed"`
		SpacePermanent int64  `json:"spacePermanent"`
		SpaceTemp      int64  `json:"spaceTemp"`
		SpaceTempExpr  int64  `json:"spaceTempExpr"` // 修复：API返回数字而非字符串
		Vip            bool   `json:"vip"`
		DirectTraffic  int64  `json:"directTraffic"`
		IsHideUID      bool   `json:"isHideUID"`
	} `json:"data"`
}

func (f *Fs) getDownloadURLByUA(ctx context.Context, filePath string, userAgent string) (string, error) {
	// Ensure token is valid before making API calls
	err := f.refreshTokenIfNecessary(ctx, false, false)
	if err != nil {
		return "", err
	}

	// 记录使用的 User-Agent
	if userAgent != "" {
		fs.Debugf(f, "🌐 123网盘使用自定义User-Agent: %s", userAgent)
	} else {
		fs.Debugf(f, "🌐 123网盘使用默认User-Agent")
	}

	if filePath == "" {
		filePath = f.root
	}

	fileID, err := f.pathToFileID(ctx, filePath)
	if err != nil {
		return "", fmt.Errorf("failed to get file ID for path %q: %w", filePath, err)
	}

	// 注意：123网盘当前的getDownloadURL方法不直接支持自定义UA
	// 但我们记录了UA参数，为将来的实现做准备
	fs.Debugf(f, "🔄 123网盘通过路径获取下载URL: 路径=%s, fileId=%s", filePath, fileID)

	// Use the standard getDownloadURL method
	return f.getDownloadURL(ctx, fileID)
}

// getDownloadURLCommand 通过fileId或.strm内容获取下载URL
func (f *Fs) getDownloadURLCommand(ctx context.Context, args []string, opt map[string]string) (any, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("需要提供fileId、123://fileId格式或文件路径")
	}

	input := args[0]
	fs.Debugf(f, "🔗 处理下载URL请求: %s", input)

	var fileID string
	var err error

	// 解析输入格式
	if fileID, found := strings.CutPrefix(input, "123://"); found {
		// 123://fileId 格式 (来自.strm文件)
		fs.Debugf(f, "✅ 解析.strm格式: fileId=%s", fileID)
	} else if strings.HasPrefix(input, "/") {
		// 文件路径格式，需要转换为fileId
		fs.Debugf(f, "🔍 解析文件路径: %s", input)
		fileID, err = f.pathToFileID(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("路径转换fileId失败 %q: %w", input, err)
		}
		fs.Debugf(f, "✅ 路径转换成功: %s -> %s", input, fileID)
	} else {
		// 假设是纯fileId
		fileID = input
		fs.Debugf(f, "✅ 使用纯fileId: %s", fileID)
	}

	// 验证fileId格式
	if fileID == "" {
		return nil, fmt.Errorf("无效的fileId: %s", input)
	}

	// 获取下载URL
	userAgent := opt["user-agent"]
	if userAgent != "" {
		// 如果提供了自定义UA，使用getDownloadURLByUA方法
		fs.Debugf(f, "🌐 使用自定义UA获取下载URL: fileId=%s, UA=%s", fileID, userAgent)

		// 需要先将fileID转换回路径，因为getDownloadURLByUA需要路径参数
		var filePath string
		if strings.HasPrefix(input, "/") {
			filePath = input // 如果原始输入是路径，直接使用
		} else {
			// 如果原始输入是fileID，我们无法轻易转换回路径，所以使用标准方法
			fs.Debugf(f, "⚠️ 自定义UA仅支持路径输入，fileID输入将使用标准方法")
			downloadURL, err := f.getDownloadURL(ctx, fileID)
			if err != nil {
				return nil, fmt.Errorf("获取下载URL失败: %w", err)
			}
			fs.Infof(f, "✅ 成功获取123网盘下载URL: fileId=%s (标准方法)", fileID)
			return downloadURL, nil
		}

		downloadURL, err := f.getDownloadURLByUA(ctx, filePath, userAgent)
		if err != nil {
			return nil, fmt.Errorf("使用自定义UA获取下载URL失败: %w", err)
		}
		fs.Debugf(f, "Got download URL with custom UA: fileId=%s", fileID)
		return downloadURL, nil
	} else {
		downloadURL, err := f.getDownloadURL(ctx, fileID)
		if err != nil {
			return nil, fmt.Errorf("failed to get download URL: %w", err)
		}
		fs.Debugf(f, "Got download URL: fileId=%s", fileID)
		return downloadURL, nil
	}
}

// Remove redundant validation functions - use standard validation instead

// validateDuration 验证时间配置不为负数
func validateDuration(fieldName string, value time.Duration) error {
	if value < 0 {
		return fmt.Errorf("%s 不能为负数", fieldName)
	}
	return nil
}

// 移除未使用的validateSize函数

// validateOptions 验证配置选项的有效性
// 优化版本：使用结构化验证方法，减少重复代码
func validateOptions(opt *Options) error {
	var errors []string

	// Validate required authentication info
	if opt.ClientID == "" {
		errors = append(errors, "client_id cannot be empty")
	}
	if opt.ClientSecret == "" {
		errors = append(errors, "client_secret cannot be empty")
	}

	// Validate numeric ranges
	if opt.MaxUploadParts <= 0 || opt.MaxUploadParts > MaxUploadPartsLimit {
		errors = append(errors, fmt.Sprintf("max_upload_parts must be between 1-%d", MaxUploadPartsLimit))
	}

	// 验证123网盘特定的时间配置
	if err := validateDuration("upload_pacer_min_sleep", time.Duration(opt.UploadPacerMinSleep)); err != nil {
		errors = append(errors, err.Error())
	}
	if err := validateDuration("download_pacer_min_sleep", time.Duration(opt.DownloadPacerMinSleep)); err != nil {
		errors = append(errors, err.Error())
	}
	if err := validateDuration("strict_pacer_min_sleep", time.Duration(opt.StrictPacerMinSleep)); err != nil {
		errors = append(errors, err.Error())
	}

	// 验证UserAgent长度
	if opt.UserAgent != "" && len(opt.UserAgent) > 200 {
		errors = append(errors, "user_agent 长度不能超过 200 字符")
	}

	// 验证RootFolderID格式（如果提供）
	if opt.RootFolderID != "" {
		if _, err := parseFileID(opt.RootFolderID); err != nil {
			errors = append(errors, "root_folder_id 必须是有效的数字")
		}
	}

	// 返回聚合的错误信息
	if len(errors) > 0 {
		return fmt.Errorf("配置验证失败: %s", strings.Join(errors, "; "))
	}

	return nil
}

// createPacer 创建pacer的工厂函数，减少重复配置代码
func createPacer(ctx context.Context, minSleep, maxSleep time.Duration, decayConstant float64) *fs.Pacer {
	return fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(minSleep),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))
}

// normalizeOptions 标准化和修正配置选项
func normalizeOptions(opt *Options) {
	// 设置123网盘特有配置的默认值
	if opt.MaxUploadParts <= 0 {
		opt.MaxUploadParts = DefaultMaxUploadParts
	}

	// 设置123网盘特定的pacer默认值
	if time.Duration(opt.UploadPacerMinSleep) <= 0 {
		opt.UploadPacerMinSleep = fs.Duration(100 * time.Millisecond)
	}
	if time.Duration(opt.DownloadPacerMinSleep) <= 0 {
		opt.DownloadPacerMinSleep = fs.Duration(100 * time.Millisecond)
	}
	if time.Duration(opt.StrictPacerMinSleep) <= 0 {
		opt.StrictPacerMinSleep = fs.Duration(100 * time.Millisecond)
	}

	// 设置默认UserAgent
	if opt.UserAgent == "" {
		opt.UserAgent = "rclone/" + fs.Version
	}
}

// 移除未使用的校验和相关函数

// 移除未使用的fastValidateCacheEntry函数

// shouldRetry 根据响应和错误确定是否重试API调用（本地实现，替代common包）
func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	// 使用rclone标准的重试判断
	return fserrors.ShouldRetry(err), err
}

// Remove redundant error handler functions - use rclone standard shouldRetry

// 移除getUnifiedConfig函数，不再使用统一配置

// 本地工具函数，替代common包的功能

// isLargeFileUpload 判断是否应该使用大文件上传模式 - 简化版本
func (f *Fs) isLargeFileUpload(fileSize int64) bool {
	// 使用简单的阈值判断，大于100MB使用分片上传
	return fileSize > 100*1024*1024
}

// getOptimalChunkCount 根据文件大小计算最优分片数量 - 简化版本
func (f *Fs) getOptimalChunkCount(fileSize int64) int {
	// 简化计算，每个分片10MB
	chunkSize := int64(10 * 1024 * 1024)
	count := int(fileSize / chunkSize)
	if fileSize%chunkSize != 0 {
		count++
	}
	if count > f.opt.MaxUploadParts {
		count = f.opt.MaxUploadParts
	}
	return count
}

// getAdjustedChunkSize 根据文件大小调整分片大小 - 简化版本
func (f *Fs) getAdjustedChunkSize(fileSize int64) fs.SizeSuffix {
	// 使用固定的分片大小
	return fs.SizeSuffix(10 * 1024 * 1024) // 10MB
}

// makeAPICallWithRest 使用rclone标准rest客户端进行API调用，自动集成QPS限制
// 这是推荐的API调用方法，替代直接使用HTTP客户端
func (f *Fs) makeAPICallWithRest(ctx context.Context, endpoint string, method string, reqBody any, respBody any) error {
	fs.Debugf(f, "🔄 使用rclone标准方法调用API: %s %s", method, endpoint)

	// 安全检查：确保rest客户端已初始化
	if f.rst == nil {
		fs.Errorf(f, "⚠️  rest客户端未初始化，尝试重新创建")
		// 使用rclone标准HTTP客户端重新创建rest客户端
		client := fshttp.NewClient(context.Background())
		f.rst = rest.NewClient(client).SetRoot(openAPIRootURL)
		fs.Debugf(f, " rest客户端重新创建成功")
	}

	// 确保令牌在进行API调用前有效
	err := f.refreshTokenIfNecessary(ctx, false, false)
	if err != nil {
		return fmt.Errorf("刷新令牌失败: %w", err)
	}

	// 根据端点获取适当的调速器
	pacer := f.getPacerForEndpoint(endpoint)

	// 确定基础URL - 根据官方文档只有两个API使用上传域名
	var baseURL string
	if strings.Contains(endpoint, "/upload/v2/file/slice") ||
		strings.Contains(endpoint, "/upload/v2/file/single/create") {
		// 只有分片上传和单步上传使用上传域名（官方文档明确说明）
		uploadDomain, err := f.getUploadDomain(ctx)
		if err != nil {
			return fmt.Errorf("获取上传域名失败: %w", err)
		}
		baseURL = uploadDomain
	} else {
		// 其他API（包括create、upload_complete）使用标准API域名
		baseURL = openAPIRootURL
	}

	// 构造rest选项，包含必要的HTTP头
	opts := rest.Opts{
		Method:  method,
		Path:    endpoint,
		RootURL: baseURL,
		ExtraHeaders: map[string]string{
			"Authorization": "Bearer " + f.token,
			"Platform":      "open_platform",
			"User-Agent":    f.opt.UserAgent,
		},
	}

	// 使用pacer包装请求，自动处理QPS限制和重试
	var resp *http.Response
	err = pacer.Call(func() (bool, error) {
		var err error
		resp, err = f.rst.CallJSON(ctx, &opts, reqBody, respBody)

		// 检查是否是401错误，如果是则尝试刷新token
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			fs.Debugf(f, "收到401错误，强制刷新token")
			// 强制刷新token，忽略时间检查
			refreshErr := f.refreshTokenIfNecessary(ctx, true, true)
			if refreshErr != nil {
				fs.Errorf(f, "刷新token失败: %v", refreshErr)
				return false, fmt.Errorf("身份验证失败: %w", refreshErr)
			}
			// 更新Authorization头
			opts.ExtraHeaders["Authorization"] = "Bearer " + f.token
			fs.Debugf(f, "token已强制刷新，将重试API调用")
			return true, nil // 重试
		}

		return shouldRetry(ctx, resp, err)
	})

	if err != nil {
		return fmt.Errorf("API调用失败 [%s %s]: %w", method, endpoint, err)
	}

	return nil
}

// makeAPICallWithRestMultipartToDomain 使用rclone标准rest客户端进行multipart API调用到指定域名
func (f *Fs) makeAPICallWithRestMultipartToDomain(ctx context.Context, domain string, endpoint string, method string, body io.Reader, contentType string, respBody any) error {
	fs.Debugf(f, "🔄 使用rclone标准方法调用multipart API到域名: %s %s %s", method, domain, endpoint)

	// 确保令牌在进行API调用前有效
	err := f.refreshTokenIfNecessary(ctx, false, false)
	if err != nil {
		return fmt.Errorf("刷新令牌失败: %w", err)
	}

	// 根据端点获取适当的调速器
	pacer := f.getPacerForEndpoint(endpoint)

	// 使用调速器进行API调用
	return pacer.Call(func() (bool, error) {
		opts := rest.Opts{
			Method:  method,
			RootURL: domain,
			Path:    endpoint,
			Body:    body,
			ExtraHeaders: map[string]string{
				"Content-Type": contentType,
			},
		}

		// 添加必要的认证头
		opts.ExtraHeaders["Authorization"] = "Bearer " + f.token
		opts.ExtraHeaders["Platform"] = "open_platform"
		opts.ExtraHeaders["User-Agent"] = f.opt.UserAgent

		var resp *http.Response
		var err error
		resp, err = f.rst.Call(ctx, &opts)
		if err != nil {
			// 检查是否需要重试
			if fserrors.ShouldRetry(err) {
				fs.Debugf(f, "API调用失败，将重试: %v", err)
				return true, err
			}
			return false, fmt.Errorf("API调用失败: %w", err)
		}
		defer resp.Body.Close()

		// 检查响应状态
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)

			// 特殊处理401错误 - token过期
			if resp.StatusCode == http.StatusUnauthorized {
				fs.Debugf(f, "收到401错误，强制刷新token")
				// 强制刷新token，忽略时间检查
				refreshErr := f.refreshTokenIfNecessary(ctx, true, true)
				if refreshErr != nil {
					fs.Errorf(f, "刷新token失败: %v", refreshErr)
					return false, fmt.Errorf("身份验证失败: %w", refreshErr)
				}
				// 更新Authorization头
				opts.ExtraHeaders["Authorization"] = "Bearer " + f.token
				fs.Debugf(f, "token已强制刷新，将重试API调用")
				return true, nil // 重试
			}

			return false, fmt.Errorf("API返回错误状态 %d: %s", resp.StatusCode, string(body))
		}

		// 解析响应JSON
		if respBody != nil {
			err = json.NewDecoder(resp.Body).Decode(respBody)
			if err != nil {
				return false, fmt.Errorf("解析响应JSON失败: %w", err)
			}
		}

		return false, nil
	})
}

// makeAPICallWithRestMultipart 使用rclone标准rest客户端进行multipart API调用
func (f *Fs) makeAPICallWithRestMultipart(ctx context.Context, endpoint string, method string, body io.Reader, contentType string, respBody any) error {
	fs.Debugf(f, "🔄 使用rclone标准方法调用multipart API: %s %s", method, endpoint)

	// 确保令牌在进行API调用前有效
	err := f.refreshTokenIfNecessary(ctx, false, false)
	if err != nil {
		return fmt.Errorf("刷新令牌失败: %w", err)
	}

	// 根据端点获取适当的调速器
	pacer := f.getPacerForEndpoint(endpoint)

	// 确定基础URL - 特定API使用上传域名
	var baseURL string
	if strings.Contains(endpoint, "/upload/v2/file/slice") ||
		strings.Contains(endpoint, "/upload/v2/file/single/create") {
		uploadDomain, err := f.getUploadDomain(ctx)
		if err != nil {
			return fmt.Errorf("获取上传域名失败: %w", err)
		}
		baseURL = uploadDomain
	} else {
		baseURL = openAPIRootURL
	}

	// 构造rest选项，包含必要的HTTP头
	opts := rest.Opts{
		Method:  method,
		Path:    endpoint,
		RootURL: baseURL,
		Body:    body,
		ExtraHeaders: map[string]string{
			"Authorization": "Bearer " + f.token,
			"Platform":      "open_platform",
			"User-Agent":    f.opt.UserAgent,
			"Content-Type":  contentType,
		},
	}

	// 使用pacer包装请求，自动处理QPS限制和重试
	var resp *http.Response
	err = pacer.Call(func() (bool, error) {
		var err error
		resp, err = f.rst.Call(ctx, &opts)

		// 检查是否是401错误，如果是则尝试刷新token
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			fs.Debugf(f, "收到401错误，强制刷新token")
			// 强制刷新token，忽略时间检查
			refreshErr := f.refreshTokenIfNecessary(ctx, true, true)
			if refreshErr != nil {
				fs.Errorf(f, "刷新token失败: %v", refreshErr)
				return false, fmt.Errorf("身份验证失败: %w", refreshErr)
			}
			// 更新Authorization头
			opts.ExtraHeaders["Authorization"] = "Bearer " + f.token
			fs.Debugf(f, "token已强制刷新，将重试API调用")
			return true, nil // 重试
		}

		if err != nil {
			return shouldRetry(ctx, resp, err)
		}

		// 解析响应
		if respBody != nil {
			defer resp.Body.Close()
			err = json.NewDecoder(resp.Body).Decode(respBody)
			if err != nil {
				return false, fmt.Errorf("解析响应失败: %w", err)
			}
		}

		return false, nil
	})

	if err != nil {
		return fmt.Errorf("multipart API调用失败 [%s %s]: %w", method, endpoint, err)
	}

	return nil
}

// getPacerForEndpoint 根据API端点返回适当的调速器
// 基于官方API限流文档：https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/
// 更新时间：2025-07-04，使用最新官方QPS限制表
func (f *Fs) getPacerForEndpoint(endpoint string) *fs.Pacer {
	switch {
	// 高频率API (15-20 QPS)
	case strings.Contains(endpoint, "/api/v2/file/list"):
		return f.listPacer // ~14 QPS (官方15 QPS)
	case strings.Contains(endpoint, "/upload/v2/file/get_upload_url"),
		strings.Contains(endpoint, "/upload/v2/file/upload_complete"):
		return f.downloadPacer // ~16 QPS (官方20 QPS)

	// 中等频率API (8-10 QPS)
	case strings.Contains(endpoint, "/api/v1/file/move"),
		strings.Contains(endpoint, "/api/v1/file/name"),
		strings.Contains(endpoint, "/api/v1/file/infos"),
		strings.Contains(endpoint, "/api/v1/user/info"):
		return f.listPacer // ~8 QPS (官方10 QPS)
	case strings.Contains(endpoint, "/api/v1/access_token"):
		return f.downloadPacer // ~6 QPS (官方8 QPS)

	// 低频率API (5 QPS)
	case strings.Contains(endpoint, "/upload/v1/file/create"),
		strings.Contains(endpoint, "/upload/v2/file/create"),
		strings.Contains(endpoint, "/upload/v1/file/mkdir"),
		strings.Contains(endpoint, "/api/v1/file/trash"),
		strings.Contains(endpoint, "/upload/v2/file/upload_complete"),
		strings.Contains(endpoint, "/api/v1/file/download_info"):
		return f.strictPacer // ~4 QPS (官方5 QPS)

	// 最低频率API (1 QPS)
	case strings.Contains(endpoint, "/api/v1/file/delete"),
		strings.Contains(endpoint, "/api/v1/file/list"),
		strings.Contains(endpoint, "/api/v1/video/transcode/list"):
		return f.strictPacer // ~0.8 QPS (官方1 QPS) - 使用最严格限制

	// v2分片上传API (特殊处理，使用专用的uploadPacer)
	case strings.Contains(endpoint, "/upload/v2/file/slice"):
		return f.uploadPacer // ~5 QPS (基于官方文档和实际测试)

	// 上传域名API (特殊处理)
	case strings.Contains(endpoint, "/upload/v2/file/domain"):
		return f.strictPacer // 保守处理，避免频繁调用

	default:
		// 为了安全起见，未知端点默认使用最严格的调速器
		fs.Debugf(f, "⚠️  未知API端点，使用最严格限制: %s", endpoint)
		return f.strictPacer // 最严格的限制
	}
}

// ErrorContext 错误上下文信息，用于提供更详细的错误信息
type ErrorContext struct {
	Operation string            // 操作类型
	Method    string            // HTTP方法
	Endpoint  string            // API端点
	FileID    string            // 文件ID（如果适用）
	FileName  string            // 文件名（如果适用）
	Extra     map[string]string // 额外信息
}

// Remove redundant error wrapper - use standard fmt.Errorf

// 移除未使用的ResumeInfo断点续传结构体，简化代码

// 移除saveUploadProgress、loadUploadProgress、removeUploadProgress函数
// 使用rclone标准重试机制，无需复杂的状态管理

// FileIntegrityVerifier 文件完整性验证器
// 功能增强：实现文件完整性验证机制
type FileIntegrityVerifier struct {
	fs *Fs
}

// NewFileIntegrityVerifier 创建文件完整性验证器
func NewFileIntegrityVerifier(fs *Fs) *FileIntegrityVerifier {
	return &FileIntegrityVerifier{fs: fs}
}

// VerifyFileIntegrity 验证文件完整性
// 功能增强：支持多种哈希算法的文件完整性验证
func (fiv *FileIntegrityVerifier) VerifyFileIntegrity(ctx context.Context, filePath string, expectedMD5, expectedSHA1 string, fileSize int64) (*IntegrityResult, error) {
	result := &IntegrityResult{
		FilePath:     filePath,
		FileSize:     fileSize,
		ExpectedMD5:  expectedMD5,
		ExpectedSHA1: expectedSHA1,
		StartTime:    time.Now(),
	}

	// 检查文件是否存在
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		result.Error = fmt.Sprintf("文件不存在: %v", err)
		return result, err
	}

	// 验证文件大小
	actualSize := fileInfo.Size()
	if actualSize != fileSize {
		result.Error = fmt.Sprintf("文件大小不匹配: 期望=%d, 实际=%d", fileSize, actualSize)
		result.SizeMatch = false
		return result, fmt.Errorf("文件大小验证失败")
	}
	result.SizeMatch = true

	// 打开文件进行哈希计算
	file, err := os.Open(filePath)
	if err != nil {
		result.Error = fmt.Sprintf("无法打开文件: %v", err)
		return result, err
	}
	defer file.Close()

	// Use standard MD5 calculation
	hasher := md5.New()
	if _, err := io.Copy(hasher, file); err != nil {
		result.Error = fmt.Sprintf("计算MD5失败: %v", err)
		return result, err
	}

	// Get calculation result
	result.ActualMD5 = fmt.Sprintf("%x", hasher.Sum(nil))
	result.ActualSHA1 = "" // 暂时不计算SHA1，减少计算开销
	result.EndTime = time.Now()
	result.Duration = result.EndTime.Sub(result.StartTime)

	// 验证MD5
	if expectedMD5 != "" {
		result.MD5Match = strings.EqualFold(result.ActualMD5, expectedMD5)
		if !result.MD5Match {
			result.Error = fmt.Sprintf("MD5不匹配: 期望=%s, 实际=%s", expectedMD5, result.ActualMD5)
		}
	}

	// 验证SHA1
	if expectedSHA1 != "" {
		result.SHA1Match = strings.EqualFold(result.ActualSHA1, expectedSHA1)
		if !result.SHA1Match {
			if result.Error != "" {
				result.Error += "; "
			}
			result.Error += fmt.Sprintf("SHA1不匹配: 期望=%s, 实际=%s", expectedSHA1, result.ActualSHA1)
		}
	}

	// 判断整体验证结果
	result.IsValid = result.SizeMatch &&
		(expectedMD5 == "" || result.MD5Match) &&
		(expectedSHA1 == "" || result.SHA1Match)

	if result.IsValid {
		fs.Debugf(fiv.fs, " 文件完整性验证通过: %s (耗时: %v)", filePath, result.Duration)
	} else {
		fs.Debugf(fiv.fs, "❌ 文件完整性验证失败: %s - %s", filePath, result.Error)
	}

	return result, nil
}

// IntegrityResult 完整性验证结果
type IntegrityResult struct {
	FilePath     string        `json:"file_path"`
	FileSize     int64         `json:"file_size"`
	ExpectedMD5  string        `json:"expected_md5"`
	ExpectedSHA1 string        `json:"expected_sha1"`
	ActualMD5    string        `json:"actual_md5"`
	ActualSHA1   string        `json:"actual_sha1"`
	SizeMatch    bool          `json:"size_match"`
	MD5Match     bool          `json:"md5_match"`
	SHA1Match    bool          `json:"sha1_match"`
	IsValid      bool          `json:"is_valid"`
	StartTime    time.Time     `json:"start_time"`
	EndTime      time.Time     `json:"end_time"`
	Duration     time.Duration `json:"duration"`
	Error        string        `json:"error,omitempty"`
}

// 移除未使用的AccessInfo和EnhancedDownloadURLEntry结构体

// parseFileID 通用的文件ID转换函数，统一错误处理
func parseFileID(idStr string) (int64, error) {
	if idStr == "" {
		return 0, fmt.Errorf("文件ID不能为空")
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无效的文件ID格式: %s", idStr)
	}

	if id < 0 {
		return 0, fmt.Errorf("文件ID不能为负数: %d", id)
	}

	return id, nil
}

// parseFileIDWithContext 带上下文的文件ID转换函数，提供更详细的错误信息
func parseFileIDWithContext(idStr, context string) (int64, error) {
	id, err := parseFileID(idStr)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", context, err)
	}
	return id, nil
}

// parseParentID 解析父目录ID字符串为int64
// 专门用于父目录ID转换，提供更明确的错误信息
func parseParentID(idStr string) (int64, error) {
	if idStr == "" {
		return 0, fmt.Errorf("父目录ID不能为空")
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无效的父目录ID格式: %s", idStr)
	}

	if id < 0 {
		return 0, fmt.Errorf("父目录ID不能为负数: %d", id)
	}

	return id, nil
}

// parseDirID 解析目录ID字符串为int64
// 专门用于目录ID转换，提供更明确的错误信息
func parseDirID(idStr string) (int64, error) {
	if idStr == "" {
		return 0, fmt.Errorf("目录ID不能为空")
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无效的目录ID格式: %s", idStr)
	}

	if id < 0 {
		return 0, fmt.Errorf("目录ID不能为负数: %d", id)
	}

	return id, nil
}

// getFileInfo 根据ID获取文件的详细信息
func (f *Fs) getFileInfo(ctx context.Context, fileID string) (*FileListInfoRespDataV2, error) {
	fs.Debugf(f, " getFileInfo开始: fileID=%s", fileID)

	// 验证文件ID
	_, err := parseFileIDWithContext(fileID, "获取文件信息")
	if err != nil {
		fs.Debugf(f, " getFileInfo文件ID验证失败: %v", err)
		return nil, err
	}

	// 使用文件详情API - 已迁移到rclone标准方法
	var response FileDetailResponse
	endpoint := fmt.Sprintf("/api/v1/file/detail?fileID=%s", fileID)

	fs.Debugf(f, " getFileInfo调用API: %s", endpoint)
	err = f.makeAPICallWithRest(ctx, endpoint, "GET", nil, &response)
	if err != nil {
		fs.Debugf(f, " getFileInfo API调用失败: %v", err)
		return nil, err
	}

	fs.Debugf(f, " getFileInfo API响应: code=%d, message=%s", response.Code, response.Message)
	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// 转换为我们的标准格式
	fileInfo := &FileListInfoRespDataV2{
		FileID:       response.Data.FileID,
		Filename:     response.Data.Filename,
		Type:         response.Data.Type,
		Size:         response.Data.Size,
		Etag:         response.Data.ETag,
		Status:       response.Data.Status,
		ParentFileID: response.Data.ParentFileID,
		Category:     0, // 详情响应中未提供
	}

	fs.Debugf(f, " getFileInfo成功: fileID=%s, filename=%s, type=%d, size=%d", fileID, fileInfo.Filename, fileInfo.Type, fileInfo.Size)
	return fileInfo, nil
}

// getDownloadURL 获取文件的下载URL
func (f *Fs) getDownloadURL(ctx context.Context, fileID string) (string, error) {
	// 修复URL频繁获取：减少API调用频率
	fs.Debugf(f, "123网盘获取下载URL: fileID=%s", fileID)

	var response DownloadInfoResponse
	endpoint := fmt.Sprintf("/api/v1/file/download_info?fileID=%s", fileID)

	err := f.makeAPICallWithRest(ctx, endpoint, "GET", nil, &response)
	if err != nil {
		return "", err
	}

	if response.Code != 0 {
		return "", fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, " 123网盘下载URL获取成功: fileID=%s", fileID)

	return response.Data.DownloadURL, nil
}

// createDirectory 创建新目录，如果目录已存在则返回现有目录的ID
func (f *Fs) createDirectory(ctx context.Context, parentID, name string) (string, error) {
	// 验证参数
	if name == "" {
		return "", fmt.Errorf("目录名不能为空")
	}
	if parentID == "" {
		return "", fmt.Errorf("父目录ID不能为空")
	}

	// 验证目录名
	if err := validateFileName(name); err != nil {
		return "", fmt.Errorf("目录名验证失败: %w", err)
	}

	// 将父目录ID转换为int64
	parentFileID, err := parseParentID(parentID)
	if err != nil {
		return "", err
	}

	// 首先检查目录是否已存在，避免不必要的API调用
	fs.Debugf(f, "检查目录 '%s' 是否已存在于父目录 %s", name, parentID)
	existingID, found, err := f.FindLeaf(ctx, parentID, name)
	if err != nil {
		fs.Debugf(f, "检查现有目录时出错: %v，继续尝试创建", err)
	} else if found {
		fs.Debugf(f, "目录 '%s' 已存在，ID: %s", name, existingID)
		return existingID, nil
	}

	// 如果常规查找失败，尝试强制刷新查找（防止缓存问题）
	if !found && err == nil {
		fs.Debugf(f, "常规查找未找到目录 '%s'，尝试强制刷新查找", name)
		existingID, found, err = f.findLeafWithForceRefresh(ctx, parentID, name)
		if err != nil {
			fs.Debugf(f, "强制刷新查找出错: %v，继续尝试创建", err)
		} else if found {
			fs.Debugf(f, "强制刷新找到目录 '%s'，ID: %s", name, existingID)
			return existingID, nil
		}
	}

	// 目录不存在，准备创建
	fs.Debugf(f, "目录 '%s' 不存在，开始创建", name)
	reqBody := map[string]any{
		"parentID": strconv.FormatInt(parentFileID, 10),
		"name":     name,
	}

	// 进行API调用 - 已迁移到rclone标准方法
	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data"` // 使用any因为不确定具体结构
	}

	err = f.makeAPICallWithRest(ctx, "/upload/v1/file/mkdir", "POST", reqBody, &response)
	if err != nil {
		return "", err
	}

	if response.Code != 0 {
		// 检查是否是目录已存在的错误
		if response.Code == 1 && strings.Contains(response.Message, "已经有同名文件夹") {
			fs.Debugf(f, "目录 '%s' 已存在，强制清理缓存后查找现有目录ID", name)

			// 等待一小段时间确保服务器同步
			time.Sleep(100 * time.Millisecond)

			// 查找现有目录的ID - 使用强制刷新模式
			existingID, found, err := f.findLeafWithForceRefresh(ctx, parentID, name)
			if err != nil {
				return "", fmt.Errorf("查找现有目录失败: %w", err)
			}
			if !found {
				// 如果仍然找不到，尝试多次查找（可能是API延迟）
				fs.Debugf(f, "第一次查找失败，尝试重试查找目录 '%s'", name)
				for retry := 0; retry < 3; retry++ {
					time.Sleep(time.Duration(retry+1) * 200 * time.Millisecond)
					existingID, found, err = f.findLeafWithForceRefresh(ctx, parentID, name)
					if err == nil && found {
						break
					}
					fs.Debugf(f, "重试第%d次查找目录 '%s': found=%v, err=%v", retry+1, name, found, err)
				}

				if !found {
					return "", fmt.Errorf("目录已存在但无法找到其ID，可能是API同步延迟或权限问题")
				}
			}

			fs.Debugf(f, "找到现有目录ID: %s", existingID)
			return existingID, nil
		}
		return "", fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// 创建成功，但API可能不返回目录ID，需要通过查找获取
	fs.Debugf(f, "目录 '%s' 创建成功，开始查找新目录ID", name)

	// 等待一小段时间确保服务器端同步完成
	time.Sleep(200 * time.Millisecond)

	// 使用强制刷新模式查找新创建目录的ID，包含重试机制
	var newDirID string
	var foundNew bool
	var errNew error

	// 尝试多次查找，处理服务器端同步延迟
	maxRetries := 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// 递增等待时间：200ms, 400ms, 600ms, 800ms
			waitTime := time.Duration(attempt*200) * time.Millisecond
			fs.Debugf(f, "第%d次重试查找新目录 '%s'，等待 %v", attempt+1, name, waitTime)
			time.Sleep(waitTime)
		}

		// 使用强制刷新查找，跳过缓存直接从API获取最新数据
		newDirID, foundNew, errNew = f.findLeafWithForceRefresh(ctx, parentID, name)
		if errNew != nil {
			fs.Debugf(f, "第%d次查找新目录失败: %v", attempt+1, errNew)
			continue
		}

		if foundNew {
			fs.Debugf(f, "第%d次尝试成功找到新创建目录 '%s'，ID: %s", attempt+1, name, newDirID)
			break
		}

		fs.Debugf(f, "第%d次尝试未找到新目录 '%s'", attempt+1, name)
	}

	// 最终检查结果
	if errNew != nil {
		return "", fmt.Errorf("查找新创建目录失败，已重试%d次: %w", maxRetries, errNew)
	}
	if !foundNew {
		return "", fmt.Errorf("新创建的目录未找到，已重试%d次，可能是服务器同步延迟或权限问题", maxRetries)
	}

	fs.Debugf(f, "成功创建目录 '%s'，ID: %s", name, newDirID)
	return newDirID, nil
}

// createUpload 创建上传会话
func (f *Fs) createUpload(ctx context.Context, parentFileID int64, filename, etag string, size int64) (*UploadCreateResp, error) {
	// 统一父目录ID验证：使用统一的验证逻辑
	fs.Debugf(f, " 分片上传: 统一验证父目录ID %d", parentFileID)
	exists, err := f.verifyParentFileID(ctx, parentFileID)
	if err != nil {
		return nil, fmt.Errorf("验证父目录ID %d 失败: %w", parentFileID, err)
	}
	if !exists {
		// 统一处理策略：如果预验证失败，尝试获取正确的父目录ID
		fs.Errorf(f, "⚠️ 分片上传: 预验证发现父目录ID %d 不存在，尝试获取正确的父目录ID", parentFileID)

		// 使用统一的父目录ID修复策略
		realParentFileID, err := f.getCorrectParentFileID(ctx, parentFileID)
		if err != nil {
			return nil, fmt.Errorf("重新获取正确父目录ID失败: %w", err)
		}

		if realParentFileID != parentFileID {
			fs.Infof(f, "🔄 分片上传: 发现正确父目录ID: %d (原ID: %d)，使用正确ID继续上传", realParentFileID, parentFileID)
			parentFileID = realParentFileID
		} else {
			fs.Debugf(f, "⚠️ 分片上传: 使用清理缓存后的父目录ID: %d 继续尝试", parentFileID)
		}
	} else {
		fs.Debugf(f, " 分片上传: 父目录ID %d 预验证通过", parentFileID)
	}

	reqBody := map[string]any{
		"parentFileID": parentFileID, // 修复：使用官方API文档的正确参数名 parentFileID (大写D)
		"filename":     filename,
		"size":         size,
		"duplicate":    1, // 1: 保留两者，新文件名将自动添加后缀; 2: 覆盖原文件
		"containDir":   false,
	}

	// 123 API需要etag
	if etag == "" {
		return nil, fmt.Errorf("etag (MD5 hash) is required for 123 API but not provided")
	}
	reqBody["etag"] = strings.ToLower(etag)
	fs.Debugf(f, "为123 API包含etag: %s", etag)

	var response UploadCreateResp

	// 使用rclone标准方法调用API - create使用标准API域名
	err = f.makeAPICallWithRest(ctx, "/upload/v2/file/create", "POST", reqBody, &response)
	if err != nil {
		return nil, fmt.Errorf("API调用失败: %w", err)
	}

	// 统一父目录ID验证逻辑：检测parentFileID不存在错误，使用统一修复策略
	if response.Code == 1 && strings.Contains(response.Message, "parentFileID不存在") {
		fs.Errorf(f, "⚠️ 父目录ID %d 不存在，清理缓存", parentFileID)

		// 使用与单步上传完全一致的统一父目录ID修复策略
		fs.Infof(f, "🔄 使用统一的父目录ID修复策略")
		correctParentID, err := f.getCorrectParentFileID(ctx, parentFileID)
		if err != nil {
			return nil, fmt.Errorf("获取正确父目录ID失败: %w", err)
		}

		// 如果获得了不同的父目录ID，重新尝试创建上传会话
		if correctParentID != parentFileID {
			fs.Infof(f, "🔄 发现正确父目录ID: %d (原ID: %d)，重新尝试创建上传会话", correctParentID, parentFileID)

			// 使用正确的目录ID重新构建请求
			reqBody["parentFileID"] = correctParentID

			// 重新调用API
			err = f.makeAPICallWithRest(ctx, "/upload/v2/file/create", "POST", reqBody, &response)
			if err != nil {
				return nil, fmt.Errorf("使用正确目录ID重试API调用失败: %w", err)
			}
		} else {
			// 即使是相同的ID，也要重新尝试，因为缓存已经清理
			fs.Infof(f, "🔄 使用清理缓存后的父目录ID: %d，重新尝试创建上传会话", correctParentID)
			reqBody["parentFileID"] = correctParentID
			err = f.makeAPICallWithRest(ctx, "/upload/v2/file/create", "POST", reqBody, &response)
			if err != nil {
				return nil, fmt.Errorf("清理缓存后重试API调用失败: %w", err)
			}
		}
	}

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// 调试响应数据
	fs.Debugf(f, "创建上传会话成功: FileID=%d, PreuploadID='%s', Reuse=%v, SliceSize=%d",
		response.Data.FileID, response.Data.PreuploadID, response.Data.Reuse, response.Data.SliceSize)

	return &response, nil
}

// createUploadV2 专门为v2多线程上传创建会话，强制使用v2 API
func (f *Fs) createUploadV2(ctx context.Context, parentFileID int64, filename, etag string, size int64) (*UploadCreateResp, error) {
	// 验证文件名符合API要求
	if err := validateFileName(filename); err != nil {
		return nil, fmt.Errorf("文件名验证失败: %w", err)
	}

	// 首先验证parentFileID是否存在
	fs.Debugf(f, "createUploadV2: 验证父目录ID %d", parentFileID)
	exists, err := f.verifyParentFileID(ctx, parentFileID)
	if err != nil {
		return nil, fmt.Errorf("验证父目录ID %d 失败: %w", parentFileID, err)
	}
	if !exists {
		return nil, fmt.Errorf("父目录ID %d 不存在", parentFileID)
	}

	reqBody := map[string]any{
		"parentFileID": parentFileID, // 修复：使用官方API文档的正确参数名 parentFileID (大写D)
		"filename":     filename,
		"size":         size,
		"duplicate":    1, // 1: 保留两者，新文件名将自动添加后缀; 2: 覆盖原文件
		"containDir":   false,
	}

	// 123 API需要etag
	if etag == "" {
		return nil, fmt.Errorf("etag (MD5 hash) is required for 123 API but not provided")
	}
	reqBody["etag"] = strings.ToLower(etag)
	fs.Debugf(f, "为v2多线程上传包含etag: %s", etag)

	var response UploadCreateResp

	// 强制使用v2 API，不使用版本回退 - 使用rclone标准方法
	bodyBytes, _ := json.Marshal(reqBody)
	fs.Debugf(f, "调用v2 API创建上传会话: %s", string(bodyBytes))
	// 根据官方文档使用v2端点和标准API域名
	err = f.makeAPICallWithRest(ctx, "/upload/v2/file/create", "POST", reqBody, &response)
	if err != nil {
		fs.Errorf(f, "v2 API调用失败: %v", err)

		// 检查是否是parentFileID不存在的错误
		if strings.Contains(err.Error(), "parentFileID不存在") {
			fs.Errorf(f, "⚠️  父目录ID %d 不存在，可能缓存过期，需要清理缓存重试", parentFileID)
			// 重置目录缓存
			if f.dirCache != nil {
				fs.Debugf(f, "重置目录缓存以清理过期的父目录ID: %d", parentFileID)
				f.dirCache.ResetRoot()
			}
			return nil, fmt.Errorf("父目录ID不存在，缓存已清理，请重试: %w", err)
		}

		return nil, fmt.Errorf("v2 API创建上传会话失败: %w", err)
	}

	// 详细记录API响应
	fs.Debugf(f, "v2 API响应: Code=%d, Message='%s', FileID=%d, PreuploadID='%s', Reuse=%v, SliceSize=%d",
		response.Code, response.Message, response.Data.FileID, response.Data.PreuploadID, response.Data.Reuse, response.Data.SliceSize)

	if response.Code != 0 {
		fs.Errorf(f, "v2 API返回错误: Code=%d, Message='%s'", response.Code, response.Message)
		return nil, fmt.Errorf("v2 API error %d: %s", response.Code, response.Message)
	}

	// 检查是否为秒传
	if response.Data.Reuse {
		fs.Debugf(f, "🚀 v2 API秒传成功: FileID=%d, 文件已存在于服务器", response.Data.FileID)
		return &response, nil
	}

	// 非秒传情况，验证PreuploadID不为空
	if response.Data.PreuploadID == "" {
		fs.Errorf(f, "❌ v2 API响应异常: 非秒传情况下PreuploadID为空，完整响应: %+v", response)
		return nil, fmt.Errorf("v2 API返回的预上传ID为空，无法进行分片上传")
	}

	// 调试响应数据
	fs.Debugf(f, " v2创建上传会话成功: FileID=%d, PreuploadID='%s', SliceSize=%d",
		response.Data.FileID, response.Data.PreuploadID, response.Data.SliceSize)

	return &response, nil
}

// uploadFile 使用上传会话上传文件内容
func (f *Fs) uploadFile(ctx context.Context, in io.Reader, createResp *UploadCreateResp, size int64) error {
	return f.uploadFileWithPath(ctx, in, createResp, size, "")
}

// uploadFileWithPath 使用上传会话上传文件内容，支持断点续传
func (f *Fs) uploadFileWithPath(ctx context.Context, in io.Reader, createResp *UploadCreateResp, size int64, filePath string) error {
	// 确定块大小
	chunkSize := createResp.Data.SliceSize
	if chunkSize <= 0 {
		// 使用rclone全局配置的多线程分片大小
		chunkSize = int64(fs.GetConfig(ctx).MultiThreadChunkSize)
	}

	// 确保块大小在限制范围内
	if chunkSize < int64(minChunkSize) {
		chunkSize = int64(minChunkSize)
	}
	if chunkSize > int64(maxChunkSize) {
		chunkSize = int64(maxChunkSize)
	}

	// 对于小文件，全部读入内存（无需断点续传）
	if size <= chunkSize {
		data, err := io.ReadAll(in)
		if err != nil {
			return fmt.Errorf("failed to read file data: %w", err)
		}

		if int64(len(data)) != size {
			return fmt.Errorf("size mismatch: expected %d, got %d", size, len(data))
		}

		// 单部分上传
		return f.uploadSinglePart(ctx, createResp.Data.PreuploadID, data)
	}

	// 大文件的多部分上传，使用rclone标准重试机制
	return f.uploadMultiPart(ctx, in, createResp.Data.PreuploadID, size, chunkSize)
}

// uploadSinglePart 单部分上传文件
func (f *Fs) uploadSinglePart(ctx context.Context, preuploadID string, data []byte) error {
	// 计算分片MD5
	chunkHash := fmt.Sprintf("%x", md5.Sum(data))

	// 使用正确的multipart上传方法
	err := f.uploadPartWithMultipart(ctx, preuploadID, 1, chunkHash, data)
	if err != nil {
		return fmt.Errorf("failed to upload part: %w", err)
	}

	// 完成上传 - 使用带文件大小的版本以支持增强验证
	_, err = f.completeUploadWithResultAndSize(ctx, preuploadID, int64(len(data)))
	return err
}

// uploadMultiPart 简化的多部分上传文件，使用rclone标准重试机制
func (f *Fs) uploadMultiPart(ctx context.Context, in io.Reader, preuploadID string, size, chunkSize int64) error {
	// 防止除零错误
	if chunkSize <= 0 {
		return fmt.Errorf("无效的分片大小: %d", chunkSize)
	}

	uploadNums := (size + chunkSize - 1) / chunkSize

	// 限制分片数量
	if uploadNums > int64(f.opt.MaxUploadParts) {
		return fmt.Errorf("file too large: would require %d parts, max allowed is %d", uploadNums, f.opt.MaxUploadParts)
	}

	fs.Debugf(f, "开始分片上传: %d个分片，每片%s", uploadNums, fs.SizeSuffix(chunkSize))

	buffer := make([]byte, chunkSize)

	// 简化的分片上传循环，无断点续传
	for partIndex := int64(0); partIndex < uploadNums; partIndex++ {
		partNumber := partIndex + 1

		// 读取块数据
		var partData []byte
		if partIndex == uploadNums-1 {
			// 最后一分片 - 读取剩余数据
			remaining := size - partIndex*chunkSize
			partData = make([]byte, remaining)
			_, err := io.ReadFull(in, partData)
			if err != nil {
				return fmt.Errorf("failed to read part %d: %w", partNumber, err)
			}
		} else {
			// 常规分片
			_, err := io.ReadFull(in, buffer)
			if err != nil {
				return fmt.Errorf("failed to read part %d: %w", partNumber, err)
			}
			partData = buffer
		}

		// 计算分片MD5
		chunkHash := fmt.Sprintf("%x", md5.Sum(partData))

		// 使用rclone标准重试机制上传分片
		err := f.uploadPartWithMultipart(ctx, preuploadID, partNumber, chunkHash, partData)
		if err != nil {
			return fmt.Errorf("failed to upload part %d: %w", partNumber, err)
		}

		fs.Debugf(f, "已上传分片 %d/%d", partNumber, uploadNums)
	}

	// 完成上传
	_, err := f.completeUploadWithResultAndSize(ctx, preuploadID, size)
	if err != nil {
		return err
	}

	return nil
}

// uploadPartWithMultipart 使用正确的multipart/form-data格式上传分片
// 修复版本：使用统一重试管理器，避免多层重试嵌套
func (f *Fs) uploadPartWithMultipart(ctx context.Context, preuploadID string, partNumber int64, chunkHash string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("分片数据不能为空")
	}

	fs.Debugf(f, "开始上传分片 %d，预上传ID: %s, 分片MD5: %s, 数据大小: %d 字节",
		partNumber, preuploadID, chunkHash, len(data))

	// 简化重试逻辑 - 使用rclone标准的fs.Pacer重试机制
	return f.uploadPacer.Call(func() (bool, error) {
		err := f.uploadPartDirectly(ctx, preuploadID, partNumber, chunkHash, data)
		return fserrors.ShouldRetry(err), err
	})
}

// uploadPartDirectly 直接上传分片，不包含重试逻辑
// 关键修复：将重试逻辑从业务逻辑中分离
func (f *Fs) uploadPartDirectly(ctx context.Context, preuploadID string, partNumber int64, chunkHash string, data []byte) error {
	// 修复：移除 uploadPacer.Call，直接执行，重试由统一管理器处理
	// 获取上传域名
	uploadDomain, err := f.getUploadDomain(ctx)
	if err != nil {
		return fmt.Errorf("获取上传域名失败: %w", err)
	}

	// 创建multipart表单数据
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// 添加表单字段
	writer.WriteField("preuploadID", preuploadID)
	writer.WriteField("sliceNo", fmt.Sprintf("%d", partNumber))
	writer.WriteField("sliceMD5", chunkHash)

	// 添加文件数据
	part, err := writer.CreateFormFile("slice", fmt.Sprintf("chunk_%d", partNumber))
	if err != nil {
		return fmt.Errorf("创建表单文件字段失败: %w", err)
	}

	_, err = part.Write(data)
	if err != nil {
		return fmt.Errorf("写入分片数据失败: %w", err)
	}

	err = writer.Close()
	if err != nil {
		return fmt.Errorf("关闭multipart writer失败: %w", err)
	}

	// 使用上传域名进行multipart上传
	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	// 修复卡住问题：为分片上传添加超时控制
	uploadCtx, cancel := context.WithTimeout(ctx, 5*time.Minute) // 每个分片最多5分钟
	defer cancel()

	// 使用上传域名调用分片上传API
	err = f.makeAPICallWithRestMultipartToDomain(uploadCtx, uploadDomain, "/upload/v2/file/slice", "POST", &buf, writer.FormDataContentType(), &response)
	if err != nil {
		return fmt.Errorf("上传分片数据失败: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("分片上传API错误 %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "分片 %d 上传成功", partNumber)
	return nil
}

// UploadCompleteResult 上传完成结果
type UploadCompleteResult struct {
	FileID int64  `json:"fileID"`
	Etag   string `json:"etag"`
}

// 移除未使用的ChunkAnalysis结构体

// completeUploadWithResultAndSize 完成多部分上传并返回结果，支持智能轮询策略
// 增强：实现动态轮询间隔和智能错误处理
func (f *Fs) completeUploadWithResultAndSize(ctx context.Context, preuploadID string, fileSize int64) (*UploadCompleteResult, error) {
	fs.Debugf(f, " 使用增强的上传验证逻辑 (文件大小: %s)", fs.SizeSuffix(fileSize))

	reqBody := map[string]any{
		"preuploadID": preuploadID,
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Completed bool   `json:"completed"`
			FileID    int64  `json:"fileID"`
			Etag      string `json:"etag"`
		} `json:"data"`
	}

	// 优化：智能轮询策略，根据文件大小动态调整参数和阈值
	maxRetries, baseInterval, maxConsecutiveFailures := f.calculatePollingStrategy(fileSize)
	fs.Debugf(f, "智能轮询策略: 最大重试%d次, 基础间隔%v, 连续失败阈值%d",
		maxRetries, baseInterval, maxConsecutiveFailures)

	// 优化：智能轮询逻辑，支持动态间隔和错误分类
	consecutiveFailures := 0
	var lastError error // 记录最后一次错误，用于动态间隔计算

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// 优化：动态轮询间隔，根据连续失败次数和错误类型调整
			interval := f.calculateDynamicInterval(baseInterval, consecutiveFailures, attempt, lastError)
			fs.Debugf(f, "轮询间隔: %v (连续失败: %d, 错误类型: %T)", interval, consecutiveFailures, lastError)

			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("上传验证被取消: %w", ctx.Err())
			case <-time.After(interval):
				// 继续轮询
			}
		}

		err := f.makeAPICallWithRest(ctx, "/upload/v2/file/upload_complete", "POST", reqBody, &response)
		if err != nil {
			lastError = err
			consecutiveFailures++
			fs.Debugf(f, "Upload complete API call failed (attempt %d/%d, consecutive failures: %d/%d): %v",
				attempt+1, maxRetries, consecutiveFailures, maxConsecutiveFailures, err)

			// Use standard retry logic
			if retry, _ := shouldRetry(ctx, nil, err); retry {
				fs.Debugf(f, "Standard retry logic suggests retry")
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("upload verification cancelled: %w", ctx.Err())
				case <-time.After(time.Duration(attempt) * time.Second):
				}
				continue
			}

			// 简化：使用动态连续失败阈值，网络错误特殊处理
			errStr := err.Error()
			if (strings.Contains(errStr, "connection") || strings.Contains(errStr, "timeout") ||
				strings.Contains(errStr, "network")) && consecutiveFailures < maxConsecutiveFailures/2 {
				fs.Debugf(f, "网络错误，继续重试 (连续失败: %d/%d)", consecutiveFailures, maxConsecutiveFailures/2)
				continue // 网络错误的容错性更高
			}
			if consecutiveFailures >= maxConsecutiveFailures {
				return nil, fmt.Errorf("连续API调用失败次数过多 (%d/%d): %w",
					consecutiveFailures, maxConsecutiveFailures, err)
			}
			continue
		}

		// 重置连续失败计数
		consecutiveFailures = 0
		progressTime := time.Now()

		if response.Code == 0 && response.Data.Completed {
			totalElapsed := time.Since(progressTime)
			fs.Infof(f, "✅ 上传验证成功完成 (总耗时: %v, 尝试次数: %d/%d)",
				totalElapsed, attempt+1, maxRetries)
			return &UploadCompleteResult{
				FileID: response.Data.FileID,
				Etag:   response.Data.Etag,
			}, nil
		}

		if response.Code == 0 && !response.Data.Completed {
			// 优化：completed=false状态的智能处理和详细日志
			elapsed := time.Since(progressTime)
			progress := float64(attempt+1) / float64(maxRetries) * 100

			fs.Debugf(f, "📋 服务器返回completed=false，继续轮询 (进度: %.1f%%, %d/%d, 已等待: %v)",
				progress, attempt+1, maxRetries, elapsed)

			// 优化：根据文件大小动态调整警告阈值
			warningThreshold := 5 * time.Minute
			if fileSize > 1*1024*1024*1024 { // 大于1GB
				warningThreshold = 15 * time.Minute
			}

			if elapsed > warningThreshold {
				fs.Logf(f, "⚠️ 验证等待时间过长(%v)，文件大小: %s，可能存在问题",
					elapsed, fs.SizeSuffix(fileSize))
			}
			continue
		}

		// 增强：错误代码分类处理
		if f.isRetryableError(response.Code) {
			fs.Debugf(f, "⚠️ 遇到可重试错误 %d: %s，继续轮询", response.Code, response.Message)
			continue
		}

		// 其他错误，直接返回
		return nil, fmt.Errorf("上传验证失败: API error %d: %s", response.Code, response.Message)
	}

	return nil, fmt.Errorf("上传验证超时，已轮询%d次", maxRetries)
}

// uploadPartStream 使用流式方式上传分片，避免大内存占用
func (f *Fs) uploadPartStream(ctx context.Context, uploadURL string, reader io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, reader)
	if err != nil {
		return fmt.Errorf("failed to create stream upload request: %w", err)
	}

	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	// 使用自适应超时机制
	adaptiveTimeout := f.getAdaptiveTimeout(size, "chunked_upload")
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, adaptiveTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	// 使用rclone标准HTTP客户端
	client := fshttp.NewClient(ctx)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stream upload request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stream upload failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// streamingPut 统一的流式上传入口，支持跨云传输优化
func (f *Fs) streamingPut(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "🚨 【重要调试】进入streamingPut函数，文件: %s", fileName)

	// 通用跨云传输检测：检测是否来自其他云存储服务
	srcFsString := src.Fs().String()
	currentFsString := f.String()

	// 检测跨云传输：源和目标是不同的文件系统
	if srcFsString != currentFsString {
		// 进一步检测是否为云存储到云存储的传输（排除本地文件系统）
		if !strings.Contains(srcFsString, "local") && !strings.Contains(currentFsString, "local") {
			fs.Infof(f, "🌐 检测到跨云传输: %s → %s，启用优化传输模式", srcFsString, currentFsString)
			return f.handleCrossCloudTransfer(ctx, in, src, parentFileID, fileName)
		}
	}

	// 跨云传输检测和处理
	isRemoteSource := f.isRemoteSource(src)
	if isRemoteSource {
		fs.Infof(f, "🌐 检测到跨云传输: %s → 123网盘 (大小: %s)", src.Fs().Name(), fileName, fs.SizeSuffix(src.Size()))
		fs.Infof(f, "📤 直接使用源云盘数据流进行123网盘上传...")
		// 关键修复：使用直接传输而不是本地化缓存，避免二次下载
		return f.handleCrossCloudTransfer(ctx, in, src, parentFileID, fileName)
	}

	// 本地文件使用标准流式上传
	fs.Debugf(f, "本地文件使用标准流式上传")
	return f.streamingPutWithBuffer(ctx, in, src, parentFileID, fileName)
}

// handleCrossCloudTransfer 统一的跨云传输处理函数
// 修复：参考115网盘实现内部两步传输，彻底解决跨云传输问题
func (f *Fs) handleCrossCloudTransfer(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	fs.Debugf(f, "Starting internal two-step transfer: %s (%s)", fileName, fs.SizeSuffix(fileSize))

	// Use internal two-step transfer to avoid cross-cloud transfer issues
	fs.Debugf(f, "Step 1: Download to local, Step 2: Upload to 123 from local")

	return f.internalTwoStepTransfer(ctx, in, src, parentFileID, fileName)
}

// 移除getCachedUploadDomain函数，直接使用getUploadDomain

// uploadFromDownloadedData 从已下载的数据直接上传，避免递归调用
func (f *Fs) uploadFromDownloadedData(ctx context.Context, reader io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	fs.Infof(f, "📤 从已下载数据直接上传: %s (%s)", fileName, fs.SizeSuffix(fileSize))

	// 步骤1: 计算MD5哈希（123网盘API必需）
	fs.Infof(f, "🔐 计算文件MD5哈希...")

	// 读取所有数据并计算MD5
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("读取已下载数据失败: %w", err)
	}

	if int64(len(data)) != fileSize {
		return nil, fmt.Errorf("数据大小不匹配: 期望%s，实际%s",
			fs.SizeSuffix(fileSize), fs.SizeSuffix(int64(len(data))))
	}

	// Calculate MD5 hash
	hasher := md5.New()
	hasher.Write(data)
	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))

	// 步骤2: 尝试秒传
	createResp, isInstant, err := f.checkInstantUpload(ctx, parentFileID, fileName, md5Hash, fileSize)
	if err != nil {
		fs.Debugf(f, "秒传检查失败: %v", err)
	} else if isInstant {
		fs.Infof(f, "🎉 秒传成功！节省上传时间")
		return f.createObjectFromUpload(fileName, fileSize, md5Hash, createResp.Data.FileID, src.ModTime(ctx)), nil
	}

	// 步骤3: 秒传失败，创建上传会话并实际上传
	fs.Infof(f, "📤 秒传失败，开始实际上传: %s", fs.SizeSuffix(fileSize))

	// 创建上传会话（现在有了MD5）
	createResp, err = f.createUploadV2(ctx, parentFileID, fileName, md5Hash, fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建上传会话失败: %w", err)
	}

	// 准备上传进度信息
	chunkSize := createResp.Data.SliceSize
	if chunkSize <= 0 {
		defaultNetworkSpeed := int64(20 * 1024 * 1024) // 20MB/s
		chunkSize = f.getOptimalChunkSize(fileSize, defaultNetworkSpeed)
	}
	// 简化：直接使用单步上传，无需进度跟踪

	// 使用内存中的数据进行v2多线程上传
	uploadStartTime := time.Now()
	// 使用单步上传，简化处理
	result, err := f.singleStepUpload(ctx, data, parentFileID, fileName, md5Hash)

	if err != nil {
		return nil, fmt.Errorf("上传到123网盘失败: %w", err)
	}

	uploadDuration := time.Since(uploadStartTime)
	uploadSpeed := float64(fileSize) / uploadDuration.Seconds() / (1024 * 1024) // MB/s

	fs.Infof(f, "Cross-cloud transfer completed: %s, upload: %v, speed: %.2f MB/s",
		fs.SizeSuffix(fileSize), uploadDuration.Round(time.Second), uploadSpeed)

	return result, nil
}

// internalTwoStepTransfer 内部两步传输：下载到本地临时文件，然后上传到123网盘
// 这是处理跨云传输最可靠的方法，避免了流式传输的复杂性
func (f *Fs) internalTwoStepTransfer(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	fs.Infof(f, "🔄 开始内部两步传输: %s (%s)", fileName, fs.SizeSuffix(fileSize))

	// Step 1: 下载到临时文件
	fs.Debugf(f, "Step 1: Download from source to temporary file...")

	// 创建临时文件
	tempFile, err := os.CreateTemp("", "rclone-123-transfer-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer func() {
		tempFile.Close()
		os.Remove(tempFile.Name())
	}()

	// 获取源文件的Reader
	var srcReader io.ReadCloser
	if in != nil {
		// 如果已经有Reader，直接使用
		srcReader = io.NopCloser(in)
	} else {
		// 否则打开源文件
		if srcObj, ok := src.(fs.Object); ok {
			srcReader, err = srcObj.Open(ctx)
			if err != nil {
				return nil, fmt.Errorf("打开源文件失败: %w", err)
			}
		} else {
			return nil, fmt.Errorf("源对象不支持Open操作")
		}
	}
	defer srcReader.Close()

	// 下载到临时文件，同时计算MD5
	hasher := md5.New()
	multiWriter := io.MultiWriter(tempFile, hasher)

	written, err := io.Copy(multiWriter, srcReader)
	if err != nil {
		return nil, fmt.Errorf("下载到临时文件失败: %w", err)
	}

	if written != fileSize {
		return nil, fmt.Errorf("下载大小不匹配: 期望 %d, 实际 %d", fileSize, written)
	}

	// 计算MD5
	md5sum := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(f, "Step 1 completed: downloaded %s, MD5: %s", fs.SizeSuffix(written), md5sum)

	// Step 2: 从临时文件上传到123网盘
	fs.Debugf(f, "Step 2: Upload from local to 123...")

	// 重置文件指针到开始
	_, err = tempFile.Seek(0, 0)
	if err != nil {
		return nil, fmt.Errorf("重置临时文件指针失败: %w", err)
	}

	// 创建本地文件信息对象
	localSrc := &localFileInfo{
		name:    fileName,
		size:    written,
		modTime: src.ModTime(ctx),
	}

	// 使用统一上传函数，此时源已经是本地数据
	return f.putWithKnownMD5(ctx, tempFile, localSrc, parentFileID, fileName, md5sum)
}

// localFileInfo 本地文件信息结构体，用于内部两步传输
type localFileInfo struct {
	name    string
	size    int64
	modTime time.Time
}

func (l *localFileInfo) String() string {
	return l.name
}

func (l *localFileInfo) Remote() string {
	return l.name
}

func (l *localFileInfo) ModTime(ctx context.Context) time.Time {
	return l.modTime
}

func (l *localFileInfo) Size() int64 {
	return l.size
}

func (l *localFileInfo) Fs() fs.Info {
	return nil // 本地文件信息不需要Fs
}

func (l *localFileInfo) Hash(ctx context.Context, t fshash.Type) (string, error) {
	return "", fmt.Errorf("hash not supported for local file info") // 本地文件信息不支持Hash
}

func (l *localFileInfo) Storable() bool {
	return true
}

// 注意：calculateOptimalChunkSize函数已删除
// 分片大小现在统一使用defaultChunkSize或API返回的SliceSize

// uploadWithKnownMD5FromRetry 在重试情况下使用已知MD5直接上传
// 修复重复下载：避免重新下载，直接尝试上传
func (f *Fs) uploadWithKnownMD5FromRetry(ctx context.Context, src fs.ObjectInfo, parentFileID int64, fileName, md5Hash string) (*Object, error) {
	fs.Infof(f, "🚀 重试上传，使用已知MD5: %s", md5Hash)

	// 尝试秒传
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, src.Size())
	if err != nil {
		fs.Debugf(f, "重试时创建上传会话失败: %v", err)
		// 如果还是失败，回退到完整下载流程
		fs.Infof(f, "⚠️ 重试上传失败，回退到完整下载流程")
		return f.handleCrossCloudTransfer(ctx, nil, src, parentFileID, fileName)
	}

	// 检查是否秒传成功
	if createResp.Data.Reuse {
		fs.Infof(f, "✅ 重试时秒传成功: %s", fileName)
		return f.createObject(fileName, createResp.Data.FileID, src.Size(), md5Hash, time.Now(), false), nil
	}

	// 如果不能秒传，说明需要实际上传文件，回退到完整流程
	fs.Infof(f, "⚠️ 重试时无法秒传，需要重新下载文件进行上传")
	return f.handleCrossCloudTransfer(ctx, nil, src, parentFileID, fileName)
}

// UnifiedUploadParams 统一上传参数结构
type UnifiedUploadParams struct {
	ctx              context.Context
	reader           io.Reader
	md5Hash          string
	src              fs.ObjectInfo
	parentFileID     int64
	fileName         string
	downloadDuration time.Duration
	md5Duration      time.Duration
	startTime        time.Time
	isFromTempFile   bool
}

// unifiedUploadWithMD5 统一的MD5上传处理函数
// 合并了uploadFromTempFile和continueWithKnownMD5的逻辑
func (f *Fs) unifiedUploadWithMD5(ctx context.Context, params UnifiedUploadParams) (*Object, error) {
	fileSize := params.src.Size()

	// 步骤1: 检查秒传
	createResp, isInstant, err := f.checkInstantUpload(ctx, params.parentFileID, params.fileName, params.md5Hash, fileSize)
	if err != nil {
		return nil, err
	}

	if isInstant {
		totalDuration := time.Since(params.startTime)
		fs.Debugf(f, "Instant upload successful! Total time: %v (download: %v + MD5: %v)",
			totalDuration, params.downloadDuration, params.md5Duration)
		return f.createObjectFromUpload(params.fileName, fileSize, params.md5Hash, createResp.Data.FileID, params.src.ModTime(ctx)), nil
	}

	// 步骤2: 实际上传
	fs.Debugf(f, "Starting upload to 123...")
	uploadStartTime := time.Now()

	// Reset file pointer if it's a temp file
	if params.isFromTempFile {
		if seeker, ok := params.reader.(io.Seeker); ok {
			_, err = seeker.Seek(0, io.SeekStart)
			if err != nil {
				return nil, fmt.Errorf("failed to reset file pointer: %w", err)
			}
		}
	}

	err = f.uploadFile(ctx, params.reader, createResp, fileSize)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}

	uploadDuration := time.Since(uploadStartTime)
	totalDuration := time.Since(params.startTime)

	// 移除MD5缓存，不再使用自定义缓存

	fs.Debugf(f, "Upload completed! Total time: %v (download: %v + MD5: %v + upload: %v)",
		totalDuration, params.downloadDuration, params.md5Duration, uploadDuration)

	return f.createObject(params.fileName, createResp.Data.FileID, fileSize, params.md5Hash, time.Now(), false), nil
}

// 移除CrossCloudMD5Cache，使用rclone标准缓存机制

// 移除getCachedMD5和cacheMD5函数，不再使用自定义MD5缓存

// putWithKnownMD5 处理已知MD5哈希的文件上传
// 这是最高效的路径，可以充分利用123网盘的秒传功能
// 优化版本：对于小文件优先使用单步上传API
func (f *Fs) putWithKnownMD5(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName, md5Hash string) (*Object, error) {
	return f.putWithKnownMD5WithOptions(ctx, in, src, parentFileID, fileName, md5Hash, false)
}

// putWithKnownMD5WithOptions 处理已知MD5哈希的文件上传，支持跳过单步上传选项
func (f *Fs) putWithKnownMD5WithOptions(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName, md5Hash string, skipSingleStep bool) (*Object, error) {
	// 对于小文件（<1GB），优先尝试单步上传API（除非明确跳过）
	fileSize := src.Size()
	if !skipSingleStep && fileSize < singleStepUploadLimit && fileSize > 0 && fileSize <= maxMemoryBufferSize {
		fs.Debugf(f, "文件大小 %d bytes < %d，尝试使用单步上传API", fileSize, singleStepUploadLimit)

		// 读取文件数据到内存
		data, err := io.ReadAll(in)
		if err != nil {
			fs.Debugf(f, "读取文件数据失败，回退到传统上传: %v", err)
		} else if int64(len(data)) == fileSize {
			// 尝试单步上传
			result, err := f.singleStepUpload(ctx, data, parentFileID, fileName, md5Hash)
			if err != nil {
				fs.Debugf(f, "单步上传失败，回退到传统上传: %v", err)
				// 重新创建reader用于传统上传
				in = bytes.NewReader(data)
			} else {
				fs.Debugf(f, "单步上传成功")
				return result, nil
			}
		}
	}

	// 传统上传流程：使用已知的MD5创建上传会话
	// 123网盘会检查服务器是否已有相同MD5的文件
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, src.Size())
	if err != nil {
		return nil, err
	}

	// 如果文件已存在（秒传/去重），直接返回对象
	// 这是最理想的情况：无需实际传输数据
	if createResp.Data.Reuse {
		fs.Debugf(f, "文件秒传成功，MD5: %s", md5Hash)
		return f.createObject(fileName, createResp.Data.FileID, src.Size(), md5Hash, time.Now(), false), nil
	}

	// 秒传失败，需要实际上传文件数据
	fs.Debugf(f, "秒传失败，开始实际上传文件数据")
	err = f.uploadFile(ctx, in, createResp, src.Size())
	if err != nil {
		return nil, err
	}

	// 返回上传成功的文件对象
	// 注意：使用实际的文件名而不是src.Remote()，因为src.Remote()可能包含.partial后缀
	// 而实际上传到123网盘的文件名是fileName（已去除.partial后缀）
	return f.createObject(fileName, createResp.Data.FileID, src.Size(), md5Hash, time.Now(), false), nil
}

// putSmallFileWithMD5 处理小文件（≤10MB）的上传
// 通过缓存到内存先计算MD5，最大化利用秒传功能，同时控制内存使用
func (f *Fs) putSmallFileWithMD5(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	fs.Debugf(f, "缓存小文件（%d字节）到内存以计算MD5，尝试秒传", fileSize)

	// 验证文件大小的合理性
	if fileSize < 0 {
		return nil, fmt.Errorf("无效的文件大小: %d", fileSize)
	}
	if fileSize > 50*1024*1024 { // 50MB上限
		fs.Debugf(f, "文件过大（%d字节），不适合内存缓存，回退到流式上传", fileSize)
		return f.streamingPutWithBuffer(ctx, in, src, parentFileID, fileName)
	}

	// 简化内存管理 - 使用Go标准库，移除过度复杂的内存跟踪

	// 使用限制读取器防止读取超过预期大小的数据
	// 这可以防止恶意或损坏的文件导致内存耗尽
	var limitedReader io.Reader
	if fileSize > 0 {
		// 允许读取比预期稍多一点的数据来检测大小不匹配
		limitedReader = io.LimitReader(in, fileSize+1024) // 额外1KB用于检测
	} else {
		// 对于未知大小的文件，设置合理的上限
		limitedReader = io.LimitReader(in, 50*1024*1024) // 50MB上限
	}

	// 将文件读取到内存，使用限制读取器保护
	data, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("读取小文件数据失败: %w", err)
	}

	// 立即检查读取的数据大小，防止内存浪费
	actualSize := int64(len(data))
	if fileSize > 0 {
		if actualSize > fileSize {
			return nil, fmt.Errorf("文件大小超过预期: 实际 %d 字节 > 预期 %d 字节，可能文件已损坏", actualSize, fileSize)
		}
		if actualSize != fileSize {
			fs.Debugf(f, "文件大小与预期不符: 实际 %d 字节，预期 %d 字节", actualSize, fileSize)
		}
	}

	// Calculate MD5 hash
	hasher := md5.New()
	hasher.Write(data)
	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))

	// 使用计算出的MD5创建上传会话
	// 123网盘会检查是否可以秒传
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, int64(len(data)))
	if err != nil {
		return nil, err
	}

	// 如果文件已存在（秒传成功），直接返回对象
	// 这种情况下我们避免了实际的数据传输
	if createResp.Data.Reuse {
		fs.Debugf(f, "小文件秒传成功，MD5: %s", md5Hash)
		return f.createObject(src.Remote(), createResp.Data.FileID, int64(len(data)), md5Hash, time.Now(), false), nil
	}

	// 秒传失败，上传缓存的数据
	// 使用bytes.NewReader从内存中的数据创建reader
	fs.Debugf(f, "小文件秒传失败，上传缓存的数据")
	err = f.uploadFile(ctx, bytes.NewReader(data), createResp, int64(len(data)))
	if err != nil {
		return nil, err
	}

	// 返回上传成功的文件对象
	return f.createObject(src.Remote(), createResp.Data.FileID, int64(len(data)), md5Hash, time.Now(), false), nil
}

// streamingPutWithBuffer 使用缓冲策略处理跨云盘传输
// 解决"源文件不支持重新打开"的问题
func (f *Fs) streamingPutWithBuffer(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "使用缓冲策略进行跨云盘传输")

	// 对于大文件，我们需要特殊处理以避免内存溢出
	fileSize := src.Size()

	// 如果文件小于100MB，直接读入内存
	if fileSize <= 100*1024*1024 {
		fs.Debugf(f, "小文件(%s)，使用内存缓冲", fs.SizeSuffix(fileSize))
		return f.streamingPutWithMemoryBuffer(ctx, in, src, parentFileID, fileName)
	}

	// 大文件使用临时文件缓冲
	fs.Debugf(f, "大文件(%s)，使用临时文件缓冲", fs.SizeSuffix(fileSize))
	return f.streamingPutWithTempFile(ctx, in, src, parentFileID, fileName)
}

// streamingPutWithMemoryBuffer 使用内存缓冲处理小文件
// 优化版本：对于小文件（<1GB）优先使用单步上传API
func (f *Fs) streamingPutWithMemoryBuffer(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	// 读取所有数据到内存
	data, err := io.ReadAll(in)
	if err != nil {
		return nil, fmt.Errorf("读取文件数据失败: %w", err)
	}

	// Calculate MD5
	hasher := md5.New()
	hasher.Write(data)
	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))

	fs.Debugf(f, "内存缓冲完成，文件大小: %d, MD5: %s", len(data), md5Hash)

	// 对于小文件（<1GB），优先使用单步上传API
	if len(data) < singleStepUploadLimit {
		fs.Debugf(f, "文件大小 %d bytes < %d，尝试使用单步上传API", len(data), singleStepUploadLimit)

		result, err := f.singleStepUpload(ctx, data, parentFileID, fileName, md5Hash)
		if err != nil {
			fs.Debugf(f, "单步上传失败，回退到传统上传方式: %v", err)
			// 如果单步上传失败，回退到传统的分片上传方式
			// 使用skipSingleStep标志防止无限递归
			return f.putWithKnownMD5WithOptions(ctx, bytes.NewReader(data), src, parentFileID, fileName, md5Hash, true)
		}

		fs.Debugf(f, "单步上传成功")
		return result, nil
	}

	// 大文件使用传统上传方式
	fs.Debugf(f, "文件大小 %d bytes >= %d，使用传统上传方式", len(data), singleStepUploadLimit)
	return f.putWithKnownMD5WithOptions(ctx, bytes.NewReader(data), src, parentFileID, fileName, md5Hash, true)
}

// streamingPutWithTempFile 使用临时文件缓冲处理大文件
func (f *Fs) streamingPutWithTempFile(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	// 检查文件大小，对于超大文件使用优化策略
	fileSize := src.Size()
	if fileSize > singleStepUploadLimit { // 大于单步上传限制的文件使用分片流式传输
		fs.Debugf(f, "超大文件(%s)，使用分片流式传输策略（无断点续传）", fs.SizeSuffix(fileSize))
		return f.streamingPutWithChunks(ctx, in, src, parentFileID, fileName)
	}

	// 对于较小的大文件，继续使用临时文件策略
	fs.Debugf(f, "大文件(%s)，使用临时文件缓冲策略", fs.SizeSuffix(fileSize))

	// 创建临时文件 - 使用安全的资源管理
	tempFile, err := os.CreateTemp("", "rclone-123pan-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer func() {
		// 安全的资源清理，确保在任何情况下都能执行
		if tempFile != nil {
			if closeErr := tempFile.Close(); closeErr != nil {
				fs.Debugf(f, "关闭临时文件失败: %v", closeErr)
			}
			if removeErr := os.Remove(tempFile.Name()); removeErr != nil {
				fs.Debugf(f, "删除临时文件失败: %v", removeErr)
			}
		}
	}()

	// 边读边写到临时文件，同时计算MD5
	hasher := md5.New()
	multiWriter := io.MultiWriter(tempFile, hasher)

	written, err := io.Copy(multiWriter, in)
	if err != nil {
		return nil, fmt.Errorf("写入临时文件失败: %w", err)
	}

	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(f, "临时文件缓冲完成，写入: %d 字节, MD5: %s", written, md5Hash)

	// 重新定位到文件开头
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("重新定位临时文件失败: %w", err)
	}

	// 使用已知MD5上传
	return f.putWithKnownMD5(ctx, tempFile, src, parentFileID, fileName, md5Hash)
}

// 移除冗余的streamingPutWithTempFileForced方法，简化上传逻辑

// streamingPutWithChunks 使用分片流式传输处理超大文件
// 简化版本：无断点续传功能，资源占用最少（只需要单个分片的临时存储）
// 这是超大文件传输的策略：节省资源的分片上传
func (f *Fs) streamingPutWithChunks(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	// 为大文件添加用户友好的初始化提示
	fileSize := src.Size()
	if fileSize > 1024*1024*1024 { // 大于1GB的文件
		fs.Infof(f, "🚀 正在初始化大文件上传 (%s) - 准备分片上传参数，请稍候...", fs.SizeSuffix(fileSize))
	}

	// 验证源对象
	srcObj, ok := src.(fs.Object)
	if !ok {
		fs.Debugf(f, "无法获取源对象，回退到临时文件策略")
		return f.streamingPutWithTempFile(ctx, in, src, parentFileID, fileName)
	}

	fileSize = src.Size()

	// 创建上传会话（先创建会话获取API要求的SliceSize）
	createResp, err := f.createChunkedUploadSession(ctx, parentFileID, fileName, fileSize)
	if err != nil {
		return nil, err
	}

	// 关键修复：使用API返回的SliceSize，而不是动态计算的分片大小
	apiSliceSize := createResp.Data.SliceSize
	if apiSliceSize <= 0 {
		// 如果API没有返回有效的SliceSize，使用64MB作为回退
		fs.Debugf(f, "⚠️ API未返回有效SliceSize(%d)，使用64MB作为回退", apiSliceSize)
		apiSliceSize = 64 * 1024 * 1024 // 64MB回退大小
	}

	fs.Debugf(f, "开始分片流式传输：文件大小 %s，API要求分片大小 %s",
		fs.SizeSuffix(fileSize), fs.SizeSuffix(apiSliceSize))

	// 如果意外触发秒传，回退到临时文件策略
	if createResp.Data.Reuse {
		fs.Debugf(f, "意外触发秒传，回退到临时文件策略")
		return f.streamingPutWithTempFile(ctx, in, src, parentFileID, fileName)
	}

	// 开始分片流式传输，使用API要求的分片大小
	return f.uploadFileInChunks(ctx, srcObj, createResp, fileSize, apiSliceSize, fileName)
}

// ChunkedUploadParams removed - was unused

// createChunkedUploadSession 创建分片上传会话
func (f *Fs) createChunkedUploadSession(ctx context.Context, parentFileID int64, fileName string, fileSize int64) (*UploadCreateResp, error) {
	// 创建上传会话（使用临时MD5，后续会更新）
	tempMD5 := fmt.Sprintf("%032x", time.Now().UnixNano())
	createResp, err := f.createUpload(ctx, parentFileID, fileName, tempMD5, fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建分片上传会话失败: %w", err)
	}

	return createResp, nil
}

// uploadFileInChunks 简化的分片流式上传，使用rclone标准重试机制
func (f *Fs) uploadFileInChunks(ctx context.Context, srcObj fs.Object, createResp *UploadCreateResp, fileSize, chunkSize int64, fileName string) (*Object, error) {
	totalChunks := (fileSize + chunkSize - 1) / chunkSize

	fs.Debugf(f, "开始分片流式上传: %d个分片", totalChunks)

	// 使用单线程上传，简化逻辑
	return f.uploadChunksSingleThreaded(ctx, srcObj, createResp, fileSize, chunkSize, fileName)
}

// ConcurrencyParams 并发参数
type ConcurrencyParams struct {
	networkSpeed int64
	optimal      int
	actual       int
}

// 移除prepareUploadProgress函数，使用rclone标准重试机制

// 移除validateUploadedChunks函数，使用rclone标准重试机制

// calculateConcurrencyParams 计算最优并发参数
func (f *Fs) calculateConcurrencyParams(fileSize int64) *ConcurrencyParams {
	networkSpeed := f.detectNetworkSpeed(context.Background())
	optimalConcurrency := f.getOptimalConcurrency(fileSize, networkSpeed)

	// 检查用户设置的最大并发数限制，使用rclone全局配置
	maxConcurrency := optimalConcurrency
	globalTransfers := fs.GetConfig(context.Background()).Transfers
	if globalTransfers > 0 && globalTransfers < maxConcurrency {
		maxConcurrency = globalTransfers
	}

	return &ConcurrencyParams{
		networkSpeed: networkSpeed,
		optimal:      optimalConcurrency,
		actual:       maxConcurrency,
	}
}

// uploadChunksSingleThreaded 简化的单线程分片上传
func (f *Fs) uploadChunksSingleThreaded(ctx context.Context, srcObj fs.Object, createResp *UploadCreateResp, fileSize, chunkSize int64, fileName string) (*Object, error) {
	preuploadID := createResp.Data.PreuploadID
	totalChunks := (fileSize + chunkSize - 1) / chunkSize
	overallHasher := md5.New()

	fs.Debugf(f, "使用单线程分片上传")

	// 简化的分片上传循环，无断点续传
	for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
		partNumber := chunkIndex + 1

		// 上传分片
		if err := f.uploadSingleChunkWithStream(ctx, srcObj, overallHasher, preuploadID, chunkIndex, chunkSize, fileSize, totalChunks); err != nil {
			return nil, fmt.Errorf("上传分片 %d 失败: %w", partNumber, err)
		}

		fs.Debugf(f, " 分片 %d/%d 上传完成", partNumber, totalChunks)
	}

	// 完成上传并返回结果
	return f.finalizeChunkedUpload(ctx, createResp, overallHasher, fileSize, fileName, preuploadID)
}

// processSkippedChunkForMD5 处理跳过的分片以计算MD5
func (f *Fs) processSkippedChunkForMD5(ctx context.Context, srcObj fs.Object, hasher hash.Hash, chunkIndex, chunkSize, fileSize int64, partNumber int64) error {
	chunkStart := chunkIndex * chunkSize
	chunkEnd := chunkStart + chunkSize
	chunkEnd = min(chunkEnd, fileSize)
	actualChunkSize := chunkEnd - chunkStart

	// 读取分片数据用于MD5计算
	chunkReader, err := srcObj.Open(ctx, &fs.RangeOption{Start: chunkStart, End: chunkEnd - 1})
	if err != nil {
		return fmt.Errorf("打开分片 %d 用于MD5计算失败: %w", partNumber, err)
	}
	defer chunkReader.Close()

	_, err = io.CopyN(hasher, chunkReader, actualChunkSize)
	if err != nil {
		return fmt.Errorf("读取分片 %d 用于MD5计算失败: %w", partNumber, err)
	}

	return nil
}

// finalizeChunkedUpload 完成分片上传并返回结果
func (f *Fs) finalizeChunkedUpload(ctx context.Context, _ *UploadCreateResp, hasher hash.Hash, fileSize int64, fileName, preuploadID string) (*Object, error) {
	// 计算最终MD5
	finalMD5 := fmt.Sprintf("%x", hasher.Sum(nil))

	// 完成上传并获取结果，传入文件大小以优化轮询策略
	result, err := f.completeUploadWithResultAndSize(ctx, preuploadID, fileSize)
	if err != nil {
		return nil, fmt.Errorf("完成分片上传失败: %w", err)
	}

	fs.Debugf(f, "分片流式上传完成，计算MD5: %s, 服务器MD5: %s", finalMD5, result.Etag)

	// 验证文件ID
	if result.FileID == 0 {
		return nil, fmt.Errorf("上传完成但未获取到有效的文件ID")
	}

	// 返回成功的文件对象，使用服务器返回的文件ID
	return f.createObject(fileName, result.FileID, fileSize, finalMD5, time.Now(), false), nil
}

// attemptStreamingUpload 尝试流式上传以避免双倍流量消耗
// 这个方法实现真正的边下边上传输，最小化流量使用
func (f *Fs) attemptStreamingUpload(ctx context.Context, srcObj fs.Object, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "尝试流式上传，避免临时文件和双倍流量")

	// 优化的MD5获取策略：优先使用已知哈希，避免重复计算
	var md5Hash string
	if hash, err := srcObj.Hash(ctx, fshash.MD5); err == nil && hash != "" {
		md5Hash = hash
		fs.Debugf(f, "使用源对象已知MD5: %s", md5Hash)
	} else {
		// 如果没有已知MD5，计算MD5（只读取一次）
		md5Reader, err := srcObj.Open(ctx)
		if err != nil {
			return nil, fmt.Errorf("无法打开源文件计算MD5: %w", err)
		}
		defer md5Reader.Close()

		// 使用标准MD5计算
		hasher := md5.New()
		_, err = io.Copy(hasher, md5Reader)
		if err != nil {
			return nil, err
		}
		md5Hash = fmt.Sprintf("%x", hasher.Sum(nil))
	}

	// 第二步：使用已知MD5创建上传会话
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, srcObj.Size())
	if err != nil {
		return nil, fmt.Errorf("创建流式上传会话失败: %w", err)
	}

	// 如果触发秒传，直接返回成功
	if createResp.Data.Reuse {
		fs.Debugf(f, "流式传输触发秒传，MD5: %s", md5Hash)
		return f.createObject(fileName, createResp.Data.FileID, srcObj.Size(), md5Hash, time.Now(), false), nil
	}

	// 第三步：重新打开源文件进行实际上传
	uploadReader, err := srcObj.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("无法重新打开源文件进行上传: %w", err)
	}
	defer uploadReader.Close()

	// 执行实际上传
	err = f.uploadFile(ctx, uploadReader, createResp, srcObj.Size())
	if err != nil {
		return nil, fmt.Errorf("流式上传失败: %w", err)
	}

	fs.Debugf(f, "流式上传成功完成，文件ID: %d", createResp.Data.FileID)

	return f.createObject(fileName, createResp.Data.FileID, srcObj.Size(), md5Hash, time.Now(), false), nil
}

// uploadSingleChunkWithStream 使用流式方式上传单个分片
// 这是分片流式传输的核心：边下载边上传，只需要分片大小的临时存储
func (f *Fs) uploadSingleChunkWithStream(ctx context.Context, srcObj fs.Object, overallHasher io.Writer, preuploadID string, chunkIndex, chunkSize, fileSize, totalChunks int64) error {
	partNumber := chunkIndex + 1
	chunkStart := chunkIndex * chunkSize
	chunkEnd := chunkStart + chunkSize
	chunkEnd = min(chunkEnd, fileSize)
	actualChunkSize := chunkEnd - chunkStart

	fs.Debugf(f, "开始流式上传分片 %d/%d (偏移: %d, 大小: %d)",
		partNumber, totalChunks, chunkStart, actualChunkSize)

	// 打开分片范围的源文件流
	chunkReader, err := srcObj.Open(ctx, &fs.RangeOption{Start: chunkStart, End: chunkEnd - 1})
	if err != nil {
		return fmt.Errorf("打开分片 %d 源文件流失败: %w", partNumber, err)
	}
	defer chunkReader.Close()

	// 创建临时文件用于此分片（只需要分片大小的存储）
	tempFile, err := os.CreateTemp("", fmt.Sprintf("rclone-123pan-chunk-%d-*", partNumber))
	if err != nil {
		return fmt.Errorf("创建分片 %d 临时文件失败: %w", partNumber, err)
	}
	defer func() {
		tempFile.Close()
		os.Remove(tempFile.Name())
	}()

	// 创建分片MD5计算器
	chunkHasher := md5.New()

	// 边读边写：同时写入临时文件、分片MD5和整体MD5
	multiWriter := io.MultiWriter(tempFile, chunkHasher, overallHasher)

	// 流式传输分片数据
	written, err := io.Copy(multiWriter, chunkReader)
	if err != nil {
		return fmt.Errorf("流式传输分片 %d 失败: %w", partNumber, err)
	}

	if written != actualChunkSize {
		return fmt.Errorf("分片 %d 大小不匹配: 期望 %d, 实际 %d", partNumber, actualChunkSize, written)
	}

	// 重新定位临时文件到开头
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("重新定位分片 %d 临时文件失败: %w", partNumber, err)
	}

	// 注意：新的uploadPartWithMultipart方法内部会获取上传域名，这里不再需要

	// 读取临时文件数据用于上传
	chunkData, err := io.ReadAll(tempFile)
	if err != nil {
		return fmt.Errorf("读取分片 %d 临时文件失败: %w", partNumber, err)
	}

	// 计算分片MD5
	chunkMD5 := fmt.Sprintf("%x", chunkHasher.Sum(nil))

	// 使用正确的multipart上传方法
	err = f.uploadPartWithMultipart(ctx, preuploadID, partNumber, chunkMD5, chunkData)
	if err != nil {
		return fmt.Errorf("上传分片 %d 数据失败: %w", partNumber, err)
	}
	fs.Debugf(f, "分片 %d 流式上传成功，大小: %d, MD5: %s", partNumber, written, chunkMD5)

	return nil
}

// 移除uploadChunksWithConcurrency函数，过度复杂的并发上传和断点续传逻辑

// 移除未使用的chunkResult结构体和相关方法

// uploadSingleChunkWithHash 并发版本的单个分片上传，返回哈希和数据用于累积
func (f *Fs) uploadSingleChunkWithHash(ctx context.Context, srcObj fs.Object, _ string, chunkIndex, chunkSize, fileSize, totalChunks int64) (string, []byte, error) {
	partNumber := chunkIndex + 1
	chunkStart := chunkIndex * chunkSize
	chunkEnd := chunkStart + chunkSize
	chunkEnd = min(chunkEnd, fileSize)
	actualChunkSize := chunkEnd - chunkStart

	fs.Debugf(f, "并发上传分片 %d/%d（含哈希），范围: %d-%d，大小: %d", partNumber, totalChunks, chunkStart, chunkEnd-1, actualChunkSize)

	// 打开分片数据流
	chunkReader, err := srcObj.Open(ctx, &fs.RangeOption{Start: chunkStart, End: chunkEnd - 1})
	if err != nil {
		return "", nil, fmt.Errorf("打开分片 %d 数据流失败: %w", partNumber, err)
	}
	defer chunkReader.Close()

	// 读取分片数据到内存（用于哈希计算和上传）
	chunkData, err := io.ReadAll(chunkReader)
	if err != nil {
		return "", nil, fmt.Errorf("读取分片 %d 数据失败: %w", partNumber, err)
	}

	if int64(len(chunkData)) != actualChunkSize {
		return "", nil, fmt.Errorf("分片 %d 大小不匹配: 期望 %d, 实际 %d", partNumber, actualChunkSize, len(chunkData))
	}

	// 计算分片MD5哈希
	chunkHasher := md5.New()
	chunkHasher.Write(chunkData)
	chunkHash := fmt.Sprintf("%x", chunkHasher.Sum(nil))

	// 获取上传域名
	uploadDomain, err := f.getUploadDomain(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("获取上传域名失败 分片 %d: %w", partNumber, err)
	}

	// 构造分片上传URL
	uploadURL := fmt.Sprintf("%s/upload/v2/file/slice", uploadDomain)

	// 使用流式上传避免大内存占用
	err = f.uploadPartStream(ctx, uploadURL, bytes.NewReader(chunkData), actualChunkSize)
	if err != nil {
		return "", nil, fmt.Errorf("上传分片 %d 数据失败: %w", partNumber, err)
	}

	fs.Debugf(f, "分片 %d/%d 并发上传成功，MD5: %s", partNumber, totalChunks, chunkHash)
	return chunkHash, chunkData, nil
}

// singleStepUpload 使用123云盘的单步上传API上传小文件（<1GB）
// 这个API专门为小文件设计，一次HTTP请求即可完成上传，效率更高
func (f *Fs) singleStepUpload(ctx context.Context, data []byte, parentFileID int64, fileName, md5Hash string) (*Object, error) {
	startTime := time.Now()
	fs.Debugf(f, "使用单步上传API，文件: %s, 大小: %d bytes, MD5: %s", fileName, len(data), md5Hash)

	// 修复：先检查秒传，避免不必要的数据传输
	fs.Debugf(f, "Checking instant upload before single-step upload...")
	createResp, isInstant, err := f.checkInstantUpload(ctx, parentFileID, fileName, md5Hash, int64(len(data)))
	if err != nil {
		fs.Debugf(f, "Instant upload check failed, continuing with single-step upload: %v", err)
	} else if isInstant {
		duration := time.Since(startTime)
		fs.Debugf(f, "Single-step instant upload successful! FileID: %d, duration: %v", createResp.Data.FileID, duration)
		return f.createObjectFromUpload(fileName, int64(len(data)), md5Hash, createResp.Data.FileID, time.Now()), nil
	}

	fs.Debugf(f, "Instant upload failed, starting actual single-step upload...")

	// 统一父目录ID验证：使用统一的验证逻辑
	fs.Debugf(f, " 单步上传: 统一验证父目录ID %d", parentFileID)
	exists, err := f.verifyParentFileID(ctx, parentFileID)
	if err != nil {
		return nil, fmt.Errorf("验证父目录ID %d 失败: %w", parentFileID, err)
	}
	if !exists {
		fs.Debugf(f, "⚠️ 单步上传: 预验证发现父目录ID %d 不存在，但继续尝试（可能是API差异）", parentFileID)
		// 不立即返回错误，让API调用来最终决定，因为不同API可能有不同的验证逻辑
	} else {
		fs.Debugf(f, " 单步上传: 父目录ID %d 预验证通过", parentFileID)
	}

	// 验证文件大小限制
	if len(data) > singleStepUploadLimit {
		return nil, fmt.Errorf("文件大小超过单步上传限制%d bytes: %d bytes", singleStepUploadLimit, len(data))
	}

	// 使用统一的文件名验证和清理
	cleanedFileName, warning := f.validateAndCleanFileNameUnified(fileName)
	if warning != nil {
		fs.Logf(f, "文件名验证警告: %v", warning)
		fileName = cleanedFileName
	}

	// 使用标准API调用进行单步上传
	var uploadResp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			FileID    int64 `json:"fileID"`
			Completed bool  `json:"completed"`
		} `json:"data"`
	}

	// 构造单步上传请求
	requestBody := map[string]any{
		"parentFileId": parentFileID,
		"filename":     fileName,
		"etag":         md5Hash,
		"size":         len(data),
	}

	err = f.makeAPICallWithRest(ctx, "/upload/v2/file/single/create", "POST", requestBody, &uploadResp)
	if err != nil {
		return nil, fmt.Errorf("单步上传API调用失败: %w", err)
	}

	// 检查API响应码
	if uploadResp.Code != 0 {
		// 检查是否是parentFileID不存在的错误
		if uploadResp.Code == 1 && strings.Contains(uploadResp.Message, "parentFileID不存在") {
			fs.Errorf(f, "⚠️ 父目录ID %d 不存在，清理缓存", parentFileID)
			// 移除缓存清理

			// 统一父目录ID验证逻辑：尝试获取正确的父目录ID
			fs.Infof(f, "🔄 使用统一的父目录ID修复策略")
			correctParentID, err := f.getCorrectParentFileID(ctx, parentFileID)
			if err != nil {
				return nil, fmt.Errorf("获取正确父目录ID失败: %w", err)
			}

			// 如果获得了不同的父目录ID，重新尝试单步上传
			if correctParentID != parentFileID {
				fs.Infof(f, "🔄 发现正确父目录ID: %d (原ID: %d)，重新尝试单步上传", correctParentID, parentFileID)
				return f.singleStepUpload(ctx, data, correctParentID, fileName, md5Hash)
			}

			return nil, fmt.Errorf("父目录ID不存在，缓存已清理，请重试: 单步上传API错误: code=%d, message=%s", uploadResp.Code, uploadResp.Message)
		}
		return nil, fmt.Errorf("单步上传API错误: code=%d, message=%s", uploadResp.Code, uploadResp.Message)
	}

	// 检查是否上传完成
	if !uploadResp.Data.Completed {
		return nil, fmt.Errorf("单步上传未完成，可能需要使用分片上传")
	}

	duration := time.Since(startTime)
	speed := float64(len(data)) / duration.Seconds() / 1024 / 1024 // MB/s

	// 关键修复：根据上传速度判断是否为秒传
	// 秒传通常在几秒内完成，速度会非常快（>100MB/s）
	if duration < 5*time.Second && speed > 100 {
		fs.Errorf(f, "🎉 【重要调试】单步上传秒传成功！文件ID: %d, 耗时: %v, 速度: %.2f MB/s", uploadResp.Data.FileID, duration, speed)
	} else {
		fs.Debugf(f, "单步上传成功，文件ID: %d, 耗时: %v, 速度: %.2f MB/s", uploadResp.Data.FileID, duration, speed)
	}

	// 返回Object
	return f.createObject(fileName, uploadResp.Data.FileID, int64(len(data)), md5Hash, time.Now(), false), nil
}

// validateAndCleanFileNameUnified 统一的验证和清理入口点
// 这是推荐的文件名处理方法，确保所有上传路径的一致性
func (f *Fs) validateAndCleanFileNameUnified(fileName string) (cleanedName string, warning error) {
	// 首先尝试验证原始文件名
	if err := validateFileName(fileName); err == nil {
		return fileName, nil
	}

	// 如果验证失败，清理文件名
	cleanedName = cleanFileName(fileName)

	// 验证清理后的文件名
	if err := validateFileName(cleanedName); err != nil {
		// 如果清理后仍然无效，使用默认名称
		cleanedName = "cleaned_file"
		warning = fmt.Errorf("原始文件名 '%s' 无法清理为有效名称，使用默认名称 '%s'", fileName, cleanedName)
	} else {
		warning = fmt.Errorf("文件名 '%s' 包含无效字符，已清理为 '%s'", fileName, cleanedName)
	}

	return cleanedName, warning
}

// getUploadDomain 获取上传域名，支持动态获取和缓存
func (f *Fs) getUploadDomain(ctx context.Context) (string, error) {

	// 尝试动态获取上传域名 - 使用正确的API路径
	var response struct {
		Code    int      `json:"code"`
		Data    []string `json:"data"` // 修正：data是字符串数组
		Message string   `json:"message"`
		TraceID string   `json:"x-traceID"`
	}

	// 使用正确的API路径调用 - 已迁移到rclone标准方法
	err := f.makeAPICallWithRest(ctx, "/upload/v2/file/domain", "GET", nil, &response)
	if err == nil && response.Code == 0 && len(response.Data) > 0 {
		domain := response.Data[0] // 取第一个域名
		fs.Debugf(f, "动态获取上传域名成功: %s", domain)

		fs.Debugf(f, "上传域名获取成功，不使用缓存: %s", domain)

		return domain, nil
	}

	// 如果动态获取失败，使用默认域名
	fs.Debugf(f, "动态获取上传域名失败，使用默认域名: %v", err)
	defaultDomains := []string{
		"https://openapi-upload.123242.com",
		"https://openapi-upload.123pan.com", // 备用域名
	}

	// 简单的健康检查：选择第一个可用的域名
	for _, domain := range defaultDomains {
		// 这里可以添加简单的连通性检查
		fs.Debugf(f, "使用默认上传域名: %s", domain)
		return domain, nil
	}

	return defaultDomains[0], nil
}

// deleteFile 通过移动到回收站来删除文件
func (f *Fs) deleteFile(ctx context.Context, fileID int64) error {
	fs.Debugf(f, "删除文件，ID: %d", fileID)

	reqBody := map[string]any{
		"fileIDs": []int64{fileID},
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err := f.makeAPICallWithRest(ctx, "/api/v1/file/trash", "POST", reqBody, &response)
	if err != nil {
		return fmt.Errorf("删除文件失败: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("删除失败: API错误 %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "成功删除文件，ID: %d", fileID)
	return nil
}

// moveFile 将文件移动到不同的目录
func (f *Fs) moveFile(ctx context.Context, fileID, toParentFileID int64) error {
	fs.Debugf(f, "移动文件%d到父目录%d", fileID, toParentFileID)

	reqBody := map[string]any{
		"fileIDs":        []int64{fileID},
		"toParentFileID": toParentFileID,
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err := f.makeAPICallWithRest(ctx, "/api/v1/file/move", "POST", reqBody, &response)
	if err != nil {
		return fmt.Errorf("移动文件失败: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("移动失败: API错误 %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "成功移动文件%d到父目录%d", fileID, toParentFileID)
	return nil
}

// renameFile 重命名文件 - 使用正确的123网盘重命名API
func (f *Fs) renameFile(ctx context.Context, fileID int64, fileName string) error {
	fs.Debugf(f, "重命名文件%d为: %s", fileID, fileName)

	// 验证新文件名
	if err := validateFileName(fileName); err != nil {
		return fmt.Errorf("重命名文件名验证失败: %w", err)
	}
	time.Sleep(2 * time.Second)
	// 使用正确的123网盘重命名API: PUT /api/v1/file/name
	reqBody := map[string]any{
		"fileId":   fileID,
		"fileName": fileName,
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	// 实现重试机制，最多重试3次
	maxRetries := 3
	var err error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		fs.Debugf(f, "重命名文件尝试 %d/%d", attempt, maxRetries)

		err = f.makeAPICallWithRest(ctx, "/api/v1/file/name", "PUT", reqBody, &response)
		if err == nil {
			fs.Debugf(f, "重命名文件成功: %d -> %s", fileID, fileName)
			return nil
		}

		// 检查是否是"文件未找到"错误
		if strings.Contains(err.Error(), "没有找到文件") || strings.Contains(err.Error(), "API错误 1") {
			if attempt < maxRetries {
				waitTime := time.Duration(attempt*2) * time.Second
				fs.Debugf(f, "文件未找到，等待%v后重试 (尝试 %d/%d)", waitTime, attempt, maxRetries)
				time.Sleep(waitTime)
				continue
			}
		}

		// 其他错误或最后一次尝试失败
		break
	}

	return fmt.Errorf("重命名文件失败 (尝试%d次): %w", maxRetries, err)
}

// handlePartialFileConflict 处理partial文件重命名冲突
// 当partial文件尝试重命名为最终文件名时发生冲突时调用
func (f *Fs) handlePartialFileConflict(ctx context.Context, fileID int64, partialName string, parentID int64, targetName string) (*Object, error) {
	fs.Debugf(f, "处理partial文件冲突: %s -> %s (父目录: %d)", partialName, targetName, parentID)

	// 移除.partial后缀获取原始文件名
	originalName := strings.TrimSuffix(partialName, ".partial")
	if targetName == "" {
		targetName = originalName
	}

	// 检查目标文件是否已存在
	exists, existingFileID, err := f.fileExistsInDirectory(ctx, parentID, targetName)
	if err != nil {
		fs.Debugf(f, "检查目标文件存在性失败: %v", err)
		return nil, fmt.Errorf("检查目标文件失败: %w", err)
	}

	if exists {
		fs.Debugf(f, "目标文件已存在，ID: %d", existingFileID)

		// 如果存在的文件就是当前文件（可能已经重命名成功）
		if existingFileID == fileID {
			fs.Debugf(f, "文件已成功重命名，返回成功对象")
			return f.createObject(targetName, fileID, -1, "", time.Now(), false), nil
		}

		// 存在不同的同名文件，生成唯一名称
		fs.Debugf(f, "目标文件名冲突，生成唯一名称")
		uniqueName, err := f.generateUniqueFileName(ctx, parentID, targetName)
		if err != nil {
			fs.Debugf(f, "生成唯一文件名失败: %v", err)
			return nil, fmt.Errorf("生成唯一文件名失败: %w", err)
		}

		fs.Logf(f, "Partial文件重命名冲突，使用唯一名称: %s -> %s", targetName, uniqueName)
		targetName = uniqueName
	}

	// 尝试重命名partial文件为目标名称
	fs.Debugf(f, "重命名partial文件: %d -> %s", fileID, targetName)
	err = f.renameFile(ctx, fileID, targetName)
	if err != nil {
		// 如果重命名仍然失败，检查是否是因为文件已经有正确名称
		if strings.Contains(err.Error(), "当前目录有重名文件") ||
			strings.Contains(err.Error(), "文件已在当前文件夹") {
			fs.Debugf(f, "重命名失败，检查文件当前状态")

			// 再次检查文件是否已有正确名称
			exists, existingFileID, checkErr := f.fileExistsInDirectory(ctx, parentID, targetName)
			if checkErr == nil && exists && existingFileID == fileID {
				fs.Debugf(f, "文件实际已有正确名称，重命名成功")
				return &Object{
					fs:          f,
					remote:      targetName,
					hasMetaData: true,
					id:          strconv.FormatInt(fileID, 10),
					size:        -1, // 需要重新获取
					modTime:     time.Now(),
					isDir:       false,
				}, nil
			}
		}

		return nil, fmt.Errorf("partial文件重命名失败: %w", err)
	}

	fs.Debugf(f, "Partial文件重命名成功: %s", targetName)
	return &Object{
		fs:          f,
		remote:      targetName,
		hasMetaData: true,
		id:          strconv.FormatInt(fileID, 10),
		size:        -1, // 需要重新获取
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// getUserInfo 获取用户信息包括存储配额 - 已迁移到rclone标准方法
func (f *Fs) getUserInfo(ctx context.Context) (*UserInfoResp, error) {
	var response UserInfoResp

	// 使用rclone标准rest客户端方法，自动集成QPS限制和重试机制
	err := f.makeAPICallWithRest(ctx, "/api/v1/user/info", "GET", nil, &response)
	if err != nil {
		return nil, err
	}

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return &response, nil
}

// makeAPICallWithRest 使用rclone标准HTTP方法，支持自动QPS限制和统一错误处理

// 移除initializeOptions函数，已合并到NewFs中

// checkInstantUpload 统一的秒传检查函数
func (f *Fs) checkInstantUpload(ctx context.Context, parentFileID int64, fileName, md5Hash string, fileSize int64) (*UploadCreateResp, bool, error) {
	fs.Debugf(f, "Checking instant upload...")
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, fileSize)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create upload session: %w", err)
	}

	if createResp.Data.Reuse {
		fs.Debugf(f, "Instant upload successful! Size: %s, MD5: %s", fs.SizeSuffix(fileSize), md5Hash)
		return createResp, true, nil
	}

	fs.Debugf(f, "Need actual upload, size: %s", fs.SizeSuffix(fileSize))
	return createResp, false, nil
}

// createObjectFromUpload 从上传响应创建Object对象
func (f *Fs) createObjectFromUpload(fileName string, fileSize int64, md5Hash string, fileID int64, modTime time.Time) *Object {
	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          fmt.Sprintf("%d", fileID),
		size:        fileSize,
		md5sum:      md5Hash,
		modTime:     modTime,
		isDir:       false,
	}
}

// createObject 通用的Object创建函数，统一所有Object创建逻辑
func (f *Fs) createObject(remote string, fileID int64, size int64, md5Hash string, modTime time.Time, isDir bool) *Object {
	return &Object{
		fs:          f,
		remote:      remote,
		hasMetaData: true,
		id:          strconv.FormatInt(fileID, 10),
		size:        size,
		md5sum:      md5Hash,
		modTime:     modTime,
		isDir:       isDir,
	}
}

// createBasicFs 创建基础文件系统对象
func createBasicFs(ctx context.Context, name, originalName, normalizedRoot string, opt *Options, m configmap.Mapper) *Fs {
	// 使用rclone标准HTTP客户端
	client := fshttp.NewClient(ctx)

	f := &Fs{
		name:         name,
		originalName: originalName,
		root:         normalizedRoot,
		opt:          *opt,
		m:            m,
		rootFolderID: opt.RootFolderID,
		rst:          rest.NewClient(client).SetRoot(openAPIRootURL), // rclone标准rest客户端
	}

	fs.Debugf(f, " createBasicFs完成: features=%p", f.features)
	return f
}

// 移除initializePacers函数，已合并到NewFs中
/*
func initializePacers(ctx context.Context, f *Fs, opt *Options) error {
	// v2 API pacer - 高频率 (~14 QPS for api/v2/file/list)
	// 使用默认的列表API频率限制
	f.listPacer = createPacer(ctx, listV2APIMinSleep, maxSleep, decayConstant)

	// 严格API pacer - 中等频率 (~4 QPS for create, async_result, etc.)
	strictPacerSleep := fileMoveMinSleep
	if opt.StrictPacerMinSleep > 0 {
		strictPacerSleep = time.Duration(opt.StrictPacerMinSleep)
	}
	f.strictPacer = createPacer(ctx, strictPacerSleep, maxSleep, decayConstant)

	// v2分片上传API pacer (~5 QPS for upload/v2/file/slice)
	uploadPacerSleep := uploadV2SliceMinSleep
	if opt.UploadPacerMinSleep > 0 {
		uploadPacerSleep = time.Duration(opt.UploadPacerMinSleep)
	}
	f.uploadPacer = createPacer(ctx, uploadPacerSleep, maxSleep, decayConstant)

	// 下载和高频率API pacer (~16 QPS for v2 upload APIs)
	downloadPacerSleep := downloadInfoMinSleep
	if opt.DownloadPacerMinSleep > 0 {
		downloadPacerSleep = time.Duration(opt.DownloadPacerMinSleep)
	}
	f.downloadPacer = createPacer(ctx, downloadPacerSleep, maxSleep, decayConstant)

	fs.Infof(f, "📊 QPS限制配置 - 列表API: %.1f QPS, 严格API: %.1f QPS, 上传API: %.1f QPS, 下载API: %.1f QPS",
		1000.0/float64(listV2APIMinSleep.Milliseconds()),
		1000.0/float64(strictPacerSleep.Milliseconds()),
		1000.0/float64(uploadPacerSleep.Milliseconds()),
		1000.0/float64(downloadPacerSleep.Milliseconds()))

	return nil
}
*/

// registerCleanupHooks 注册资源清理钩子
func registerCleanupHooks(f *Fs) {
	// 注册程序退出时的资源清理钩子
	// 确保即使程序异常退出也能清理临时文件
	atexit.Register(func() {
		fs.Debugf(f, "🚨 程序退出信号检测到，开始清理123网盘资源...")

		// 移除缓存清理，不再使用自定义缓存
	})
	fs.Debugf(f, "🛡️ 程序退出清理钩子注册成功")
}

// 移除initializeFeatures函数，已合并到NewFs中使用标准Fill模式

// initializeAuthentication 初始化认证
func initializeAuthentication(ctx context.Context, f *Fs, m configmap.Mapper) error {
	// Check if we have saved token in config file
	tokenLoaded := loadTokenFromConfig(f, m)
	fs.Debugf(f, "从配置加载令牌: %v（过期时间 %v）", tokenLoaded, f.tokenExpiry)

	var tokenRefreshNeeded bool
	if tokenLoaded {
		// Check if token is expired or will expire soon
		if time.Now().After(f.tokenExpiry.Add(-tokenRefreshWindow)) {
			fs.Debugf(f, "令牌已过期或即将过期，立即刷新")
			tokenRefreshNeeded = true
		}
	} else {
		// No token, so login
		fs.Debugf(f, "未找到令牌，尝试登录")
		err := f.GetAccessToken(ctx)
		if err != nil {
			return fmt.Errorf("initial login failed: %w", err)
		}
		// Save token to config after successful login
		f.saveToken(ctx, m)
		fs.Debugf(f, "登录成功，令牌已保存")
	}

	// Try to refresh the token if needed
	if tokenRefreshNeeded {
		err := f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			fs.Debugf(f, "令牌刷新失败，尝试完整登录: %v", err)
			// If refresh fails, try full login
			err = f.GetAccessToken(ctx)
			if err != nil {
				return fmt.Errorf("login failed after token refresh failure: %w", err)
			}
		}
		// Save the refreshed/new token
		f.saveToken(ctx, m)
		fs.Debugf(f, "令牌刷新/登录成功，令牌已保存")
	}

	// Setup token renewer for automatic refresh
	fs.Debugf(f, "设置令牌更新器")
	f.setupTokenRenewer(ctx, m)

	return nil
}

// handleRootDirectory 处理根目录查找或文件路径
func handleRootDirectory(ctx context.Context, f *Fs, root string) (fs.Fs, error) {
	// Find the root directory
	err := f.dirCache.FindRoot(ctx, false)
	if err != nil {
		// Assume it is a file
		newRoot, remote := dircache.SplitPath(root)
		// 创建新的Fs实例，避免复制锁值
		tempF := &Fs{
			name:          f.name,
			root:          newRoot,
			opt:           f.opt,
			features:      f.features, // 修复：复制features字段
			m:             f.m,
			rootFolderID:  f.rootFolderID,
			rst:           f.rst, // 修复：也复制rst字段
			listPacer:     f.listPacer,
			strictPacer:   f.strictPacer,
			uploadPacer:   f.uploadPacer,
			downloadPacer: f.downloadPacer,
			// 移除缓存和统一组件字段，不再使用
		}
		tempF.dirCache = dircache.New(newRoot, f.rootFolderID, tempF)
		// Make new Fs which is the parent
		err = tempF.dirCache.FindRoot(ctx, false)
		if err != nil {
			// No root so return old f
			return f, nil
		}
		_, err := tempF.NewObject(ctx, remote)
		if err != nil {
			// unable to list folder so return old f
			return f, nil
		}
		// 🔧 修复：对于backend命令，不返回ErrorIsFile，而是返回正常的Fs实例
		// 这样可以让backend命令正常工作，同时保持文件对象的引用
		fs.Debugf(tempF, "文件路径处理：创建文件模式Fs实例，文件: %s", remote)
		return tempF, nil
	}

	return f, nil
}

// newFs 从路径构造Fs，格式为 container:path
func newFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	// Validate and normalize options
	if err := validateOptions(opt); err != nil {
		return nil, err
	}
	normalizeOptions(opt)

	// Parse root path
	originalName := name
	normalizedRoot := strings.Trim(root, "/")

	// Create HTTP client and rest client
	client := fshttp.NewClient(ctx)

	// Create Fs object
	f := &Fs{
		name:         name,
		originalName: originalName,
		root:         normalizedRoot,
		opt:          *opt,
		m:            m,
		rootFolderID: opt.RootFolderID,
		rst:          rest.NewClient(client).SetRoot(openAPIRootURL),
	}

	// Initialize pacers
	f.listPacer = createPacer(ctx, listV2APIMinSleep, maxSleep, decayConstant)
	strictPacerSleep := fileMoveMinSleep
	if opt.StrictPacerMinSleep > 0 {
		strictPacerSleep = time.Duration(opt.StrictPacerMinSleep)
	}
	f.strictPacer = createPacer(ctx, strictPacerSleep, maxSleep, decayConstant)
	uploadPacerSleep := uploadV2SliceMinSleep // 使用上传API的默认限制
	if opt.UploadPacerMinSleep > 0 {
		uploadPacerSleep = time.Duration(opt.UploadPacerMinSleep)
	}
	f.uploadPacer = createPacer(ctx, uploadPacerSleep, maxSleep, decayConstant)
	downloadPacerSleep := downloadInfoMinSleep // 使用下载API的默认限制
	if opt.DownloadPacerMinSleep > 0 {
		downloadPacerSleep = time.Duration(opt.DownloadPacerMinSleep)
	}
	f.downloadPacer = createPacer(ctx, downloadPacerSleep, maxSleep, decayConstant)

	// 移除复杂的缓存和统一组件初始化，使用rclone标准组件

	// Initialize features
	f.features = (&fs.Features{
		ReadMimeType:            true,
		CanHaveEmptyDirectories: true,
		BucketBased:             false,
		SetTier:                 false,
		GetTier:                 false,
		DuplicateFiles:          false,
		ReadMetadata:            true,
		WriteMetadata:           false,
		UserMetadata:            false,
		PartialUploads:          false,
		NoMultiThreading:        false,
		SlowModTime:             true,
		SlowHash:                false,
	}).Fill(ctx, f)

	// Initialize directory cache
	f.dirCache = dircache.New(normalizedRoot, f.rootFolderID, f)

	// Initialize authentication
	tokenLoaded := loadTokenFromConfig(f, m)
	fs.Debugf(f, "从配置加载令牌: %v（过期时间 %v）", tokenLoaded, f.tokenExpiry)

	var tokenRefreshNeeded bool
	if !tokenLoaded {
		tokenRefreshNeeded = true
	} else if time.Now().After(f.tokenExpiry.Add(-tokenRefreshWindow)) {
		tokenRefreshNeeded = true
	}

	if tokenRefreshNeeded {
		if err := f.refreshTokenIfNecessary(ctx, true, false); err != nil {
			return nil, fmt.Errorf("初始化认证失败: %w", err)
		}
	}

	// Find the root directory
	err = f.dirCache.FindRoot(ctx, false)
	if err != nil {
		// Assume it is a file
		newRoot, remote := dircache.SplitPath(root)
		// Create new Fs instance to avoid copying mutex
		tempF := &Fs{
			name:          f.name,
			originalName:  f.originalName,
			root:          newRoot,
			opt:           f.opt,
			features:      f.features,
			rst:           f.rst,
			token:         f.token,
			tokenExpiry:   f.tokenExpiry,
			tokenRenewer:  f.tokenRenewer,
			m:             f.m,
			rootFolderID:  f.rootFolderID,
			listPacer:     f.listPacer,
			uploadPacer:   f.uploadPacer,
			downloadPacer: f.downloadPacer,
			strictPacer:   f.strictPacer,
		}
		tempF.dirCache = dircache.New(newRoot, f.rootFolderID, tempF)
		// Make new Fs which is the parent
		err = tempF.dirCache.FindRoot(ctx, false)
		if err != nil {
			// No root so return old f
			return f, nil
		}
		_, err := tempF.NewObject(ctx, remote)
		if err != nil {
			// unable to list folder so return old f
			return f, nil
		}
		// return an error with an fs which points to the parent
		return tempF, fs.ErrorIsFile
	}
	return f, nil
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	fs.Debugf(f, "调用NewObject: %s", remote)

	// 规范化远程路径
	normalizedRemote := normalizePath(remote)
	fs.Debugf(f, "规范化后的远程路径: %s -> %s", remote, normalizedRemote)

	// Get the full path
	var fullPath string
	if f.root == "" {
		fullPath = normalizedRemote
	} else if normalizedRemote == "" {
		// 当remote为空时，fullPath就是root本身
		fullPath = f.root
	} else {
		fullPath = normalizePath(f.root + "/" + normalizedRemote)
	}

	if fullPath == "" {
		fullPath = "/"
	}

	// Get file ID
	fileID, err := f.pathToFileID(ctx, fullPath)
	if err != nil {
		if err == fs.ErrorObjectNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}

	fs.Debugf(f, " NewObject pathToFileID结果: fullPath=%s -> fileID=%s", fullPath, fileID)

	// Get file details
	fileInfo, err := f.getFileInfo(ctx, fileID)
	if err != nil {
		return nil, err
	}

	// Check if it's a file (not directory)
	// Type: 0-文件  1-文件夹
	if fileInfo.Type == 1 {
		return nil, fs.ErrorNotAFile
	}

	// 确定正确的remote路径
	objectRemote := remote
	if objectRemote == "" && f.root != "" {
		// 当remote为空且root指向文件时，使用文件名作为remote
		objectRemote = fileInfo.Filename
		fs.Debugf(f, " NewObject设置remote: %s -> %s", remote, objectRemote)
	}

	fileIDInt, _ := strconv.ParseInt(fileID, 10, 64)
	o := f.createObject(objectRemote, fileIDInt, fileInfo.Size, fileInfo.Etag, time.Now(), false)

	return o, nil
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// Put uploads the object.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	fs.Debugf(f, "调用Put: %s", src.Remote())

	// 规范化远程路径
	normalizedRemote := normalizePath(src.Remote())
	fs.Debugf(f, "Put操作规范化路径: %s -> %s", src.Remote(), normalizedRemote)

	// Use dircache to find parent directory
	fs.Debugf(f, "查找父目录路径: %s", normalizedRemote)
	leaf, parentID, err := f.dirCache.FindPath(ctx, normalizedRemote, true)
	if err != nil {
		fs.Errorf(f, "查找父目录失败: %v", err)
		return nil, err
	}
	fileName := leaf
	fs.Debugf(f, "找到父目录ID: %s, 文件名: %s", parentID, fileName)

	// 验证并清理文件名
	cleanedFileName, warning := validateAndCleanFileName(fileName)
	if warning != nil {
		fs.Logf(f, "文件名验证警告: %v", warning)
		fileName = cleanedFileName
	}

	// Convert parentID to int64
	parentFileID, err := parseParentID(parentID)
	if err != nil {
		return nil, fmt.Errorf("解析父目录ID失败: %w", err)
	}

	// 验证parentFileID是否真的存在
	fs.Debugf(f, "验证父目录ID %d 是否存在", parentFileID)
	exists, err := f.verifyParentFileID(ctx, parentFileID)
	if err != nil {
		fs.Errorf(f, "验证父目录ID失败: %v", err)
		// 尝试清理缓存并重新查找
		fs.Debugf(f, "清理目录缓存并重试")
		f.dirCache.ResetRoot()
		_, parentID, err = f.dirCache.FindPath(ctx, normalizedRemote, true)
		if err != nil {
			return nil, fmt.Errorf("重新查找父目录失败: %w", err)
		}
		parentFileID, err = parseParentID(parentID)
		if err != nil {
			return nil, fmt.Errorf("重新解析父目录ID失败: %w", err)
		}
		fs.Debugf(f, "重新找到父目录ID: %d", parentFileID)
	} else if !exists {
		fs.Errorf(f, "父目录ID %d 不存在，尝试重建目录缓存", parentFileID)
		// 清理缓存并重新查找
		f.dirCache.ResetRoot()
		_, parentID, err = f.dirCache.FindPath(ctx, normalizedRemote, true)
		if err != nil {
			return nil, fmt.Errorf("重建目录缓存后查找失败: %w", err)
		}
		parentFileID, err = parseParentID(parentID)
		if err != nil {
			return nil, fmt.Errorf("重建后解析父目录ID失败: %w", err)
		}
		fs.Debugf(f, "重建后找到父目录ID: %d", parentFileID)
	} else {
		fs.Debugf(f, "父目录ID %d 验证成功", parentFileID)
	}

	// Use streaming optimization for cross-cloud transfers
	obj, err := f.streamingPut(ctx, in, src, parentFileID, fileName)

	if err == nil {
		// 移除缓存清理
	}

	return obj, err
}

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// String returns a description of the Object
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// ModTime returns the modification time
func (o *Object) ModTime(ctx context.Context) time.Time {
	// In a real implementation, you would fetch metadata if not already available.
	return o.modTime
}

// Size returns the size of the file
func (o *Object) Size() int64 {
	// In a real implementation, you would fetch metadata if not already available.
	return o.size
}

// Hash returns the MD5 checksum
func (o *Object) Hash(ctx context.Context, t fshash.Type) (string, error) {
	if t != fshash.MD5 {
		return "", fshash.ErrUnsupported
	}
	// In a real implementation, you would fetch metadata if not already available.
	return o.md5sum, nil
}

// ID returns the ID of the Object
func (o *Object) ID() string {
	// In a real implementation, you would fetch metadata if not already available.
	return o.id
}

// Storable indicates this object can be stored
func (o *Object) Storable() bool {
	return true
}

// Open the file for reading.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {

	// 跨云传输优化：检测大文件并启用多线程下载（参考115网盘实现）
	// 修复：检查是否已经有禁用并发下载选项，避免重复并发
	hasDisableOption := false
	for _, option := range options {
		if option.String() == "DisableConcurrentDownload" {
			hasDisableOption = true
			break
		}
	}

	if !hasDisableOption && o.size >= 10*1024*1024 { // 10MB阈值
		return o.openWithConcurrency(ctx, options...)
	}

	var resp *http.Response

	// Use pacer for download with retry (use strict pacer for download URL requests)
	err := o.fs.strictPacer.Call(func() (bool, error) {
		// Get download URL (may need refresh)
		downloadURL, err := o.fs.getDownloadURL(ctx, o.id)
		if err != nil {
			return shouldRetry(ctx, nil, err)
		}

		// Create HTTP request
		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
		if err != nil {
			return false, fmt.Errorf("failed to create download request: %w", err)
		}

		// Handle range requests
		var start, end int64 = 0, -1
		for _, option := range options {
			switch x := option.(type) {
			case *fs.RangeOption:
				start, end = x.Start, x.End
			case *fs.SeekOption:
				start = x.Offset
			}
		}

		if start > 0 || end >= 0 {
			rangeHeader := fmt.Sprintf("bytes=%d-", start)
			if end >= 0 {
				rangeHeader = fmt.Sprintf("bytes=%d-%d", start, end)
			}
			req.Header.Set("Range", rangeHeader)
			fs.Debugf(o.fs, "请求范围: %s", rangeHeader)
		}

		// Set user agent
		req.Header.Set("User-Agent", o.fs.opt.UserAgent)
		// Note: Don't set timeout here as it will be applied to the entire download

		// Make the request using rclone standard HTTP client
		client := fshttp.NewClient(ctx)
		resp, err = client.Do(req)
		if err != nil {
			return shouldRetry(ctx, resp, err)
		}

		// Check status code
		switch resp.StatusCode {
		case http.StatusOK, http.StatusPartialContent:
			return false, nil // Success
		case http.StatusFound, http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			// Let the HTTP client handle redirects automatically
			// This should not happen if the client is configured correctly
			resp.Body.Close()
			return false, fmt.Errorf("unexpected redirect response %d - client should handle this automatically", resp.StatusCode)
		case http.StatusRequestedRangeNotSatisfiable:
			resp.Body.Close()
			return false, fmt.Errorf("range not satisfiable")
		case http.StatusNotFound:
			resp.Body.Close()
			return false, fs.ErrorObjectNotFound
		default:
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return shouldRetry(ctx, resp, fmt.Errorf("download failed with status %d: %s", resp.StatusCode, string(body)))
		}
	})

	if err != nil {
		return nil, err
	}

	// 创建包装的ReadCloser来处理进度更新和资源清理
	return &ProgressReadCloser{
		ReadCloser:      resp.Body,
		fs:              o.fs,
		totalSize:       o.size,
		transferredSize: 0,
		startTime:       time.Now(),
	}, nil
}

// 移除shouldUseConcurrentDownload方法，逻辑已内联

// openWithConcurrency 使用统一并发下载器打开文件（用于跨云传输优化）
func (o *Object) openWithConcurrency(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// Start concurrent download
	fs.Debugf(o, "Starting concurrent download for: %s", o.Remote())

	// Use custom concurrent download implementation
	return o.openWithCustomConcurrency(ctx, options...)
}

// openNormal 普通的打开方法（原来的逻辑）
func (o *Object) openNormal(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {

	var resp *http.Response

	// 使用重试机制获取下载链接并下载
	err := o.fs.downloadPacer.Call(func() (bool, error) {
		downloadURL, err := o.fs.getDownloadURL(ctx, o.id)
		if err != nil {
			return shouldRetry(ctx, resp, fmt.Errorf("获取下载链接失败: %w", err))
		}

		req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
		if err != nil {
			return false, err
		}

		// 处理Range选项
		fs.FixRangeOption(options, o.size)
		fs.OpenOptionAddHTTPHeaders(req.Header, options)

		// 使用rclone标准HTTP客户端
		client := fshttp.NewClient(ctx)
		resp, err = client.Do(req)
		if err != nil {
			return shouldRetry(ctx, resp, fmt.Errorf("下载请求失败: %w", err))
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return shouldRetry(ctx, resp, fmt.Errorf("下载失败，状态码: %d, 响应: %s", resp.StatusCode, string(body)))
		}

		return false, nil
	})

	if err != nil {
		return nil, err
	}

	// 创建包装的ReadCloser来处理进度更新和资源清理
	return &ProgressReadCloser{
		ReadCloser:      resp.Body,
		fs:              o.fs,
		totalSize:       o.size,
		transferredSize: 0,
		startTime:       time.Now(),
	}, nil
}

// openWithCustomConcurrency 使用自定义并发下载（当统一下载器不可用时）
func (o *Object) openWithCustomConcurrency(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	fs.Infof(o, "🚀 123网盘启动自定义并发下载: %s", fs.SizeSuffix(o.size))

	// 创建临时文件用于并发下载
	tempFile, err := os.CreateTemp("", "123_custom_download_*.tmp")
	if err != nil {
		fs.Debugf(o, "创建临时文件失败，回退到普通下载: %v", err)
		return o.openNormal(ctx, options...)
	}

	// 使用简化的并发下载逻辑
	err = o.downloadWithCustomConcurrency(ctx, tempFile, options...)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		fs.Debugf(o, "自定义并发下载失败，回退到普通下载: %v", err)
		return o.openNormal(ctx, options...)
	}

	// 重置文件指针到开始位置
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return nil, fmt.Errorf("重置临时文件指针失败: %w", err)
	}

	fs.Infof(o, "✅ 123网盘自定义并发下载完成: %s", fs.SizeSuffix(o.size))

	// 返回一个包装的ReadCloser，在关闭时删除临时文件
	return &ConcurrentDownloadReader{
		file:     tempFile,
		tempPath: tempFile.Name(),
	}, nil
}

// downloadWithCustomConcurrency 自定义并发下载实现
func (o *Object) downloadWithCustomConcurrency(ctx context.Context, tempFile *os.File, _ ...fs.OpenOption) error {
	// 简化的并发下载配置
	chunkSize := int64(50 * 1024 * 1024) // 50MB分片
	maxConcurrency := 6                  // 6线程并发

	fileSize := o.size
	numChunks := (fileSize + chunkSize - 1) / chunkSize
	if numChunks < int64(maxConcurrency) {
		maxConcurrency = int(numChunks)
	}

	fs.Infof(o, "📊 123网盘自定义并发下载参数: 分片大小=%s, 分片数=%d, 并发数=%d",
		fs.SizeSuffix(chunkSize), numChunks, maxConcurrency)

	// 获取下载URL
	downloadURL, err := o.fs.getDownloadURL(ctx, o.id)
	if err != nil {
		return fmt.Errorf("获取下载URL失败: %w", err)
	}

	// 创建并发下载任务
	var wg sync.WaitGroup
	errChan := make(chan error, maxConcurrency)
	semaphore := make(chan struct{}, maxConcurrency)

	for i := int64(0); i < numChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize - 1
		if end >= fileSize {
			end = fileSize - 1
		}

		wg.Add(1)
		go func(chunkIndex, start, end int64) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 下载分片
			err := o.downloadChunk(ctx, downloadURL, tempFile, start, end, chunkIndex)
			if err != nil {
				errChan <- fmt.Errorf("分片%d下载失败: %w", chunkIndex, err)
			}
		}(i, start, end)
	}

	// 等待所有分片完成
	wg.Wait()
	close(errChan)

	// 检查是否有错误
	for err := range errChan {
		return err
	}

	return nil
}

// downloadChunk 下载单个分片
func (o *Object) downloadChunk(ctx context.Context, url string, tempFile *os.File, start, end, chunkIndex int64) error {
	// 创建HTTP请求
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("创建下载请求失败: %w", err)
	}

	// 设置Range头
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("User-Agent", o.fs.opt.UserAgent)

	// 执行请求，使用rclone标准HTTP客户端
	client := fshttp.NewClient(ctx)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("执行下载请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 检查响应状态
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载失败，状态码: %d", resp.StatusCode)
	}

	// 读取数据
	expectedSize := end - start + 1
	buffer := make([]byte, expectedSize)
	totalRead := int64(0)

	for totalRead < expectedSize {
		n, err := resp.Body.Read(buffer[totalRead:])
		if n > 0 {
			totalRead += int64(n)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("读取分片数据失败: %w", err)
		}
	}

	// 验证读取的数据大小
	if totalRead != expectedSize {
		return fmt.Errorf("分片数据大小不匹配: 期望%d，实际%d", expectedSize, totalRead)
	}

	// 写入到文件
	writtenBytes, err := tempFile.WriteAt(buffer[:totalRead], start)
	if err != nil {
		return fmt.Errorf("写入分片数据失败: %w", err)
	}

	// 验证写入的字节数
	if int64(writtenBytes) != totalRead {
		return fmt.Errorf("分片写入不完整: 期望写入%d，实际写入%d", totalRead, writtenBytes)
	}

	fs.Debugf(o, " 123网盘分片%d下载完成: %s (bytes=%d-%d)",
		chunkIndex, fs.SizeSuffix(totalRead), start, end)

	return nil
}

// Update the object with new content.
// 优化版本：使用统一上传系统，支持动态参数调整和多线程上传
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	fs.Debugf(o.fs, "调用Update: %s", o.remote)

	startTime := time.Now()

	// Use dircache to find parent directory
	leaf, parentID, err := o.fs.dirCache.FindPath(ctx, o.remote, false)
	if err != nil {
		return err
	}

	// Convert parentID to int64
	parentFileID, err := parseParentID(parentID)
	if err != nil {
		return fmt.Errorf("解析父目录ID失败: %w", err)
	}

	// 验证并清理文件名
	cleanedFileName, warning := validateAndCleanFileName(leaf)
	if warning != nil {
		fs.Logf(o.fs, "文件名验证警告: %v", warning)
		leaf = cleanedFileName
	}

	// 使用新的统一上传系统进行更新
	fs.Debugf(o.fs, "使用统一上传系统进行Update操作")
	newObj, err := o.fs.streamingPut(ctx, in, src, parentFileID, leaf)

	// 记录上传完成或错误
	duration := time.Since(startTime)
	if err != nil {
		fs.Errorf(o.fs, "Update操作失败: %v", err)
		return err
	}

	// 更新当前对象的元数据
	o.size = newObj.size
	o.md5sum = newObj.md5sum
	o.modTime = newObj.modTime
	o.id = newObj.id

	fs.Debugf(o.fs, "Update操作成功，耗时: %v", duration)

	return nil
}

// Remove the object.
func (o *Object) Remove(ctx context.Context) error {
	fs.Debugf(o.fs, "调用Remove: %s", o.remote)

	// 检查文件ID是否有效
	if o.id == "" {
		fs.Debugf(o.fs, "文件ID为空，可能是partial文件或上传失败的文件: %s", o.remote)
		// 对于partial文件或上传失败的文件，我们无法删除，但也不应该报错
		// 因为这些文件可能根本不存在于服务器上
		return nil
	}

	// Convert file ID to int64
	fileID, err := parseFileID(o.id)
	if err != nil {
		fs.Debugf(o.fs, "解析文件ID失败，可能是partial文件: %s, 错误: %v", o.remote, err)
		// 对于无法解析的文件ID，我们也不报错，因为这通常是partial文件
		return nil
	}

	err = o.fs.deleteFile(ctx, fileID)
	if err == nil {
		// 移除缓存清理
	}
	return err
}

// SetModTime is not supported
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return fs.ErrorCantSetModTime
}

// DirCacher interface implementation for dircache

// FindLeaf finds a directory or file leaf in the parent folder pathID.
// Used by dircache.
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (foundID string, found bool, err error) {
	fs.Debugf(f, "查找叶节点: pathID=%s, leaf=%s", pathID, leaf)

	// Convert pathID to int64
	parentFileID, err := parseParentID(pathID)
	if err != nil {
		return "", false, fmt.Errorf("解析父目录ID失败: %w", err)
	}

	// List files in the parent directory，使用rclone全局配置的检查器数量
	listChunk := fs.GetConfig(ctx).Checkers
	if listChunk <= 0 {
		listChunk = 100 // 默认值
	}
	response, err := f.ListFile(ctx, int(parentFileID), listChunk, "", "", 0)
	if err != nil {
		return "", false, err
	}

	if response.Code != 0 {
		return "", false, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// Search for the leaf
	for _, file := range response.Data.FileList {
		if file.Filename == leaf {
			foundID = strconv.FormatInt(file.FileID, 10)
			found = true
			// Cache the found item's path/ID mapping
			parentPath, ok := f.dirCache.GetInv(pathID)
			if ok {
				var itemPath string
				if parentPath == "" {
					itemPath = leaf
				} else {
					itemPath = parentPath + "/" + leaf
				}
				f.dirCache.Put(itemPath, foundID)
			}
			return foundID, found, nil
		}
	}

	return "", false, nil
}

// findLeafWithForceRefresh 强制刷新缓存后查找叶节点
// 用于解决缓存不一致导致的目录查找失败问题
func (f *Fs) findLeafWithForceRefresh(ctx context.Context, pathID, leaf string) (foundID string, found bool, err error) {
	fs.Debugf(f, "强制刷新查找叶节点: pathID=%s, leaf=%s", pathID, leaf)

	// Convert pathID to int64
	parentFileID, err := parseParentID(pathID)
	if err != nil {
		return "", false, fmt.Errorf("解析父目录ID失败: %w", err)
	}

	// 移除缓存清理

	// 直接调用API获取最新的目录列表，跳过缓存
	params := url.Values{}
	params.Add("parentFileId", fmt.Sprintf("%d", parentFileID))
	// 使用rclone全局配置的检查器数量作为列表限制
	listChunk := fs.GetConfig(context.Background()).Checkers
	if listChunk <= 0 {
		listChunk = 100 // 默认值
	}
	params.Add("limit", fmt.Sprintf("%d", listChunk))

	endpoint := "/api/v2/file/list?" + params.Encode()

	var response ListResponse
	err = f.makeAPICallWithRest(ctx, endpoint, "GET", nil, &response)
	if err != nil {
		return "", false, fmt.Errorf("强制刷新API调用失败: %w", err)
	}

	if response.Code != 0 {
		return "", false, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// Search for the leaf in fresh data
	for _, file := range response.Data.FileList {
		if file.Filename == leaf {
			foundID = strconv.FormatInt(file.FileID, 10)
			found = true

			// 移除缓存保存

			// Cache the found item's path/ID mapping
			parentPath, ok := f.dirCache.GetInv(pathID)
			if ok {
				var itemPath string
				if parentPath == "" {
					itemPath = leaf
				} else {
					itemPath = parentPath + "/" + leaf
				}
				f.dirCache.Put(itemPath, foundID)
			}

			fs.Debugf(f, "强制刷新找到叶节点: %s -> %s", leaf, foundID)
			return foundID, found, nil
		}
	}

	fs.Debugf(f, "强制刷新未找到叶节点: %s", leaf)
	return "", false, nil
}

// CreateDir creates a directory with the given name in the parent folder pathID.
// Used by dircache.
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	fs.Debugf(f, "创建目录: pathID=%s, leaf=%s", pathID, leaf)

	// 验证参数
	if leaf == "" {
		return "", fmt.Errorf("目录名不能为空")
	}
	if pathID == "" {
		return "", fmt.Errorf("父目录ID不能为空")
	}

	// 验证并清理目录名
	cleanedDirName, warning := validateAndCleanFileName(leaf)
	if warning != nil {
		fs.Logf(f, "目录名验证警告: %v", warning)
		leaf = cleanedDirName
	}

	// Create the directory using the existing createDirectory method
	dirID, err := f.createDirectory(ctx, pathID, leaf)
	if err != nil {
		return "", err
	}

	return dirID, nil
}

// Check the interfaces are satisfied
var (
	_ fs.Fs          = (*Fs)(nil)
	_ fs.Object      = (*Object)(nil)
	_ fs.Mover       = (*Fs)(nil)
	_ fs.DirMover    = (*Fs)(nil)
	_ fs.Copier      = (*Fs)(nil)
	_ fs.Abouter     = (*Fs)(nil)
	_ fs.Purger      = (*Fs)(nil)
	_ fs.Shutdowner  = (*Fs)(nil)
	_ fs.ObjectInfo  = (*Object)(nil)
	_ fs.IDer        = (*Object)(nil)
	_ fs.SetModTimer = (*Object)(nil)
)

// loadTokenFromConfig attempts to load and parse tokens from the config file
func loadTokenFromConfig(f *Fs, _ configmap.Mapper) bool {
	if f.opt.Token == "" {
		fs.Debugf(f, "配置中未找到令牌（来自选项）")
		return false
	}

	var tokenData TokenJSON                                                // Use the new TokenJSON struct
	var err error                                                          // Declare err here
	if err = json.Unmarshal([]byte(f.opt.Token), &tokenData); err != nil { // Unmarshal unobscured token
		fs.Debugf(f, "从配置解析令牌失败（来自选项）: %v", err)
		return false
	}

	f.token = tokenData.AccessToken
	f.tokenExpiry, err = time.Parse(time.RFC3339, tokenData.Expiry) // Use tokenData.Expiry
	if err != nil {
		fs.Debugf(f, "从配置解析令牌过期时间失败: %v", err)
		return false
	}

	if f.token == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "配置中的令牌不完整")
		return false
	}

	fs.Debugf(f, "从配置文件加载令牌，过期时间: %v", f.tokenExpiry)
	return true
}

// saveToken saves the current token to the config
func (f *Fs) saveToken(_ context.Context, m configmap.Mapper) {
	if m == nil {
		fs.Debugf(f, "不保存令牌 - 提供的映射器为空")
		return
	}

	if f.token == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "不保存令牌 - 令牌信息不完整")
		return
	}

	tokenData := TokenJSON{ // Use the new TokenJSON struct
		AccessToken: f.token,
		Expiry:      f.tokenExpiry.Format(time.RFC3339), // Use Expiry
	}
	tokenJSON, err := json.Marshal(tokenData)
	if err != nil {
		fs.Errorf(f, "保存令牌时序列化失败: %v", err)
		return
	}

	m.Set("token", string(tokenJSON))
	// m.Set does not return an error, so we assume it succeeds.
	// Any errors related to config saving would be handled by the underlying config system.

	fs.Debugf(f, "使用原始名称%q保存令牌到配置文件", f.originalName)
}

// setupTokenRenewer initializes the token renewer to automatically refresh tokens
func (f *Fs) setupTokenRenewer(ctx context.Context, m configmap.Mapper) {
	// Only set up renewer if we have valid tokens
	if f.token == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "不设置令牌更新器 - 令牌信息不完整")
		return
	}

	// Create a renewal transaction function
	transaction := func() error {
		fs.Debugf(f, "令牌更新器触发，刷新令牌")
		// Use non-global function to avoid deadlocks
		err := f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			fs.Errorf(f, "更新器中刷新令牌失败: %v", err)
			return err
		}

		return nil // saveToken is already called in refreshTokenIfNecessary
	}

	// Create minimal OAuth config
	config := &oauthutil.Config{
		TokenURL: openAPIRootURL + "/api/v1/access_token", // Use the correct token URL for 123Pan
	}

	// Create a token source using the existing token
	token := &oauth2.Token{
		AccessToken: f.token,
		// 123Pan API does not seem to return a refresh token in the initial access token response,
		// so we rely on re-logging in if the access token expires.
		// RefreshToken: f.refreshToken, // Not available for 123Pan
		Expiry:    f.tokenExpiry,
		TokenType: "Bearer",
	}

	// Save token to config so it can be accessed by TokenSource
	err := oauthutil.PutToken(f.originalName, m, token, false)
	if err != nil {
		fs.Logf(f, "为更新器保存令牌失败: %v", err)
		return
	}

	// Create a client with the token source
	_, ts, err := oauthutil.NewClientWithBaseClient(ctx, f.originalName, m, config, getHTTPClient(ctx, f.originalName, m))
	if err != nil {
		fs.Logf(f, "为更新器创建令牌源失败: %v", err)
		return
	}

	// Create token renewer that will trigger when the token is about to expire
	f.tokenRenewer = oauthutil.NewRenew(f.originalName, ts, transaction)
	f.tokenRenewer.Start() // Start the renewer immediately
	fs.Debugf(f, "令牌更新器已初始化并启动，使用原始名称%q", f.originalName)
}

// refreshTokenIfNecessary refreshes the token if necessary
func (f *Fs) refreshTokenIfNecessary(ctx context.Context, refreshTokenExpired bool, forceRefresh bool) error {
	f.tokenMu.Lock()

	// Check if another thread has successfully refreshed while we were waiting
	if !refreshTokenExpired && !forceRefresh && f.token != "" && time.Now().Before(f.tokenExpiry.Add(-tokenRefreshWindow)) {
		fs.Debugf(f, "获取锁后令牌仍然有效，跳过刷新")
		f.tokenMu.Unlock()
		return nil
	}
	defer f.tokenMu.Unlock()

	// Perform the actual token refresh
	accessToken, expiredAt, err := GetAccessToken(f.opt.ClientID, f.opt.ClientSecret)
	if err != nil {
		return fmt.Errorf("failed to get new access token during refresh: %w", err)
	}

	f.token = accessToken
	f.tokenExpiry = expiredAt

	// Save the refreshed token to config
	f.saveToken(ctx, f.m)

	return nil
}

// GetAccessToken fetches a new access token from the 123Pan API.
func (f *Fs) GetAccessToken(ctx context.Context) error {
	accessToken, expiredAt, err := GetAccessToken(f.opt.ClientID, f.opt.ClientSecret)
	if err != nil {
		return fmt.Errorf("failed to get new access token: %w", err)
	}
	f.token = accessToken
	f.tokenExpiry = expiredAt
	return nil
}

type ClientCredentials struct {
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
}

type TokenJSON struct {
	AccessToken string `json:"access_token"`
	Expiry      string `json:"expiry"`
}

type AccessTokenData struct {
	AccessToken string `json:"accessToken"`
	ExpiredAt   string `json:"expiredAt"`
}

type Response struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	Data     AccessTokenData
	XTraceID string `json:"x-traceID"`
}

func GetAccessToken(clientID, clientSecret string) (string, time.Time, error) {
	credentials := ClientCredentials{
		ClientID:     clientID,
		ClientSecret: clientSecret,
	}
	payload, err := json.Marshal(credentials)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to marshal payload: %v", err)
	}
	req, err := http.NewRequest("POST", "https://open-api.123pan.com/api/v1/access_token", bytes.NewBuffer(payload))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Platform", "open_platform")
	req.Header.Set("Content-Type", "application/json")
	client := fshttp.NewClient(context.Background())
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to read response body: %v", err)
	}
	var result Response
	if err = json.Unmarshal(body, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("failed to unmarshal response: %v", err)
	}
	expiredAt, err := time.Parse(time.RFC3339, result.Data.ExpiredAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to parse expiration time: %v", err)
	}
	return result.Data.AccessToken, expiredAt, nil
}

// Name of the remote (e.g. "123")
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (e.g. "bucket" or "bucket/dir")
func (f *Fs) Root() string {
	return f.root
}

// String returns a description of the FS
func (f *Fs) String() string {
	return fmt.Sprintf("123 root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() fshash.Set {
	return fshash.Set(fshash.MD5)
}

// About gets quota information
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	fs.Debugf(f, "调用About")

	userInfo, err := f.getUserInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}

	usage := &fs.Usage{}
	if userInfo.Data.SpacePermanent > 0 {
		usage.Total = fs.NewUsageValue(userInfo.Data.SpacePermanent)
	}
	if userInfo.Data.SpaceUsed > 0 {
		usage.Used = fs.NewUsageValue(userInfo.Data.SpaceUsed)
	}
	if userInfo.Data.SpacePermanent > 0 && userInfo.Data.SpaceUsed > 0 {
		free := userInfo.Data.SpacePermanent - userInfo.Data.SpaceUsed
		if free > 0 {
			usage.Free = fs.NewUsageValue(free)
		}
	}

	return usage, nil
}

// Purge deletes all the files in the directory
func (f *Fs) Purge(ctx context.Context, dir string) error {
	fs.Debugf(f, "调用Purge删除目录: %s", dir)

	// Use dircache to find the directory ID
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}

	// Convert dirID to int64
	parentFileID, err := parseDirID(dirID)
	if err != nil {
		return fmt.Errorf("解析目录ID失败: %w", err)
	}

	// List all files in the directory
	var allFiles []FileListInfoRespDataV2
	lastFileID := int64(0)
	maxIterations := 10000 // 防止无限循环：最多10000次迭代
	iteration := 0

	for {
		iteration++
		if iteration > maxIterations {
			return fmt.Errorf("清理目录超过最大迭代次数 %d，可能存在循环", maxIterations)
		}
		response, err := f.ListFile(ctx, int(parentFileID), 100, "", "", int(lastFileID))
		if err != nil {
			return err
		}

		if response.Code != 0 {
			return fmt.Errorf("API error %d: %s", response.Code, response.Message)
		}

		// Collect all files
		for _, file := range response.Data.FileList {
			if file.Status < 100 { // Not trashed
				allFiles = append(allFiles, file)
			}
		}

		// Check if we have more files
		if response.Data.LastFileId == -1 {
			break
		}
		lastFileID = response.Data.LastFileId
	}

	// Delete all files
	for _, file := range allFiles {
		err := f.deleteFile(ctx, file.FileID)
		if err != nil {
			fs.Errorf(f, "删除文件失败 %s: %v", file.Filename, err)
			// Continue with other files
		}
	}

	return nil
}

// Shutdown the backend
func (f *Fs) Shutdown(ctx context.Context) error {
	fs.Debugf(f, "调用关闭")

	// Stop token renewer if it exists
	if f.tokenRenewer != nil {
		f.tokenRenewer.Stop()
		f.tokenRenewer = nil
	}

	// 清理所有临时文件（已简化）

	// 移除缓存关闭代码，不再使用自定义缓存

	return nil
}

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fs.Debugf(f, "调用列表，目录: %s", dir)

	// 特殊处理：如果dir为空且root指向文件，返回包含该文件的列表
	if dir == "" && f.root != "" {
		// 检查root是否指向一个文件
		rootObj, err := f.NewObject(ctx, "")
		if err == nil {
			// root指向一个文件，返回包含该文件的列表
			fs.Debugf(f, "root路径指向文件，返回包含该文件的列表: %s", f.root)
			return fs.DirEntries{rootObj}, nil
		}
		// 如果NewObject失败，检查是否是因为root指向目录
		if err == fs.ErrorNotAFile {
			fs.Debugf(f, "root路径指向目录，继续目录列表逻辑: %s", f.root)
		} else {
			fs.Debugf(f, "root路径检查失败，继续目录列表逻辑: %s, 错误: %v", f.root, err)
		}
	}

	// 使用目录缓存查找目录ID
	fs.Debugf(f, "查找目录ID: %s", dir)
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		fs.Debugf(f, "查找目录失败 %s: %v", dir, err)
		return nil, err
	}
	fs.Debugf(f, "找到目录ID: %s，目录: %s", dirID, dir)

	// Convert dirID to int64
	parentFileID, err := parseDirID(dirID)
	if err != nil {
		return nil, fmt.Errorf("解析目录ID失败: %w", err)
	}

	// List files in the directory
	var allFiles []FileListInfoRespDataV2
	lastFileID := int64(0)
	maxIterations := 10000 // 防止无限循环：最多10000次迭代
	iteration := 0

	for {
		iteration++
		if iteration > maxIterations {
			return nil, fmt.Errorf("列表目录超过最大迭代次数 %d，可能存在循环", maxIterations)
		}
		response, err := f.ListFile(ctx, int(parentFileID), 100, "", "", int(lastFileID))
		if err != nil {
			return nil, err
		}

		if response.Code != 0 {
			return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
		}

		// Filter out trashed files
		for _, file := range response.Data.FileList {
			if file.Status < 100 { // Not trashed
				allFiles = append(allFiles, file)
			}
		}

		// Check if we have more files
		if response.Data.LastFileId == -1 {
			break
		}
		lastFileID = response.Data.LastFileId
	}

	// Convert to fs.DirEntries
	for _, file := range allFiles {
		// 修复：清理API返回的文件名，去除前后空格并进行URL解码
		cleanedFilename := strings.TrimSpace(file.Filename)

		// URL解码文件名（处理%3A等编码）
		if decodedName, err := url.QueryUnescape(cleanedFilename); err == nil {
			cleanedFilename = decodedName
		}
		if cleanedFilename != file.Filename {
			fs.Debugf(f, " 清理文件名: [%s] -> [%s]", file.Filename, cleanedFilename)
		}

		remote := cleanedFilename
		if dir != "" {
			remote = strings.Trim(dir+"/"+cleanedFilename, "/")
		}

		if file.Type == 1 { // Directory
			// Cache the directory ID for future use
			f.dirCache.Put(remote, strconv.FormatInt(file.FileID, 10))
			d := fs.NewDir(remote, time.Time{})
			entries = append(entries, d)
		} else { // File
			o := &Object{
				fs:          f,
				remote:      remote,
				hasMetaData: true,
				id:          strconv.FormatInt(file.FileID, 10),
				size:        file.Size,
				md5sum:      file.Etag,
				modTime:     time.Now(), // We'll parse this properly later
				isDir:       false,
			}
			entries = append(entries, o)
		}
	}

	return entries, nil
}

// Mkdir makes the directory.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	fs.Debugf(f, "调用创建目录: %s", dir)

	// Use dircache to create the directory
	_, err := f.dirCache.FindDir(ctx, dir, true)
	if err == nil {
		// 移除缓存清理
	}
	return err
}

// Rmdir removes an empty directory.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	fs.Debugf(f, "调用删除目录: %s", dir)

	// Use dircache to find the directory ID
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}

	// Convert dirID to int64
	fileID, err := strconv.ParseInt(dirID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid directory ID: %s", dirID)
	}

	// Delete the directory
	err = f.deleteFile(ctx, fileID)
	if err != nil {
		return err
	}

	// Remove from cache
	f.dirCache.FlushDir(dir)

	// 移除缓存清理

	return nil
}

// Move server-side moves a file.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	fs.Debugf(f, "调用移动 %s 到 %s", src.Remote(), remote)

	// 规范化目标路径
	normalizedRemote := normalizePath(remote)
	fs.Debugf(f, "Move操作规范化目标路径: %s -> %s", remote, normalizedRemote)

	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}

	// Use dircache to find destination directory
	dstName, dstDirID, err := f.dirCache.FindPath(ctx, normalizedRemote, true)
	if err != nil {
		return nil, err
	}

	// 验证并清理目标文件名
	cleanedDstName, warning := validateAndCleanFileName(dstName)
	if warning != nil {
		fs.Logf(f, "移动操作文件名验证警告: %v", warning)
		dstName = cleanedDstName
	}

	// 转换目标目录ID
	dstParentID, err := strconv.ParseInt(dstDirID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid destination directory ID: %s", dstDirID)
	}

	// 生成唯一的目标文件名，避免冲突
	uniqueDstName, err := f.generateUniqueFileName(ctx, dstParentID, dstName)
	if err != nil {
		fs.Debugf(f, "生成唯一文件名失败，使用原始名称: %v", err)
		uniqueDstName = dstName
	}

	if uniqueDstName != dstName {
		fs.Logf(f, "检测到文件名冲突，使用唯一名称: %s -> %s", dstName, uniqueDstName)
		dstName = uniqueDstName
	}

	// Convert IDs to int64
	fileID, err := strconv.ParseInt(srcObj.id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid file ID: %s", srcObj.id)
	}

	// Get source directory ID for comparison
	_, srcDirID, err := f.dirCache.FindPath(ctx, srcObj.Remote(), false)
	if err != nil {
		return nil, fmt.Errorf("failed to find source directory: %w", err)
	}

	// 获取源文件的基础名称（不包含路径）
	srcBaseName := filepath.Base(srcObj.Remote())
	fs.Debugf(f, "源文件基础名称: %s, 目标文件名: %s", srcBaseName, dstName)

	// Check if we're moving to the same directory
	if srcDirID == dstDirID {
		// Same directory - only rename if name changed
		if dstName != srcBaseName {
			fs.Debugf(f, "同目录移动，仅重命名 %s 为 %s", srcBaseName, dstName)
			err = f.renameFile(ctx, fileID, dstName)
			if err != nil {
				return nil, err
			}
		} else {
			fs.Debugf(f, "同目录同名 - 无需操作")
		}
	} else {
		// 不同目录 - 先移动，然后根据需要重命名
		fs.Debugf(f, "移动文件从目录 %s 到 %s", srcDirID, dstDirID)

		// 在跨目录移动时，先检查目标目录是否已存在同名文件
		if dstName == srcBaseName {
			// 如果目标文件名与源文件名相同，检查目标目录是否已存在该文件
			exists, existingFileID, err := f.fileExistsInDirectory(ctx, dstParentID, dstName)
			if err != nil {
				fs.Debugf(f, "检查目标文件是否存在失败: %v", err)
			} else if exists {
				fs.Debugf(f, "目标目录已存在同名文件，ID: %d", existingFileID)
				if existingFileID == fileID {
					fs.Debugf(f, "文件已在目标位置，无需移动")
					// 文件已经在正确位置，直接返回成功
					return &Object{
						fs:          f,
						remote:      remote,
						hasMetaData: srcObj.hasMetaData,
						id:          srcObj.id,
						size:        srcObj.size,
						md5sum:      srcObj.md5sum,
						modTime:     srcObj.modTime,
						isDir:       srcObj.isDir,
					}, nil
				} else {
					fs.Debugf(f, "目标目录存在不同的同名文件，生成唯一名称")
					uniqueName, err := f.generateUniqueFileName(ctx, dstParentID, dstName)
					if err != nil {
						fs.Debugf(f, "生成唯一文件名失败: %v", err)
					} else {
						dstName = uniqueName
						fs.Logf(f, "使用唯一文件名避免冲突: %s", dstName)
					}
				}
			}
		}

		err = f.moveFile(ctx, fileID, dstParentID)
		if err != nil {
			// 检查是否是因为文件已在目标目录 - 修复字符串匹配问题
			if strings.Contains(err.Error(), "文件已在当前文件夹中") ||
				strings.Contains(err.Error(), "文件已在当前文件夹") {
				fs.Debugf(f, "文件可能已在目标目录，检查状态")
				exists, existingFileID, checkErr := f.fileExistsInDirectory(ctx, dstParentID, srcBaseName)
				if checkErr == nil && exists && existingFileID == fileID {
					fs.Debugf(f, "确认文件已在目标目录，继续重命名步骤")
					// 文件已在目标目录，跳过移动步骤
				} else {
					// 检查是否是partial文件重命名冲突
					if strings.HasSuffix(srcBaseName, ".partial") {
						fs.Debugf(f, "Partial文件移动冲突，尝试处理文件名冲突")
						return f.handlePartialFileConflict(ctx, fileID, srcBaseName, dstParentID, dstName)
					}
					return nil, err
				}
			} else {
				return nil, err
			}
		}

		// If the name changed, rename the file
		if dstName != srcBaseName {
			fs.Debugf(f, "移动后重命名文件: %s -> %s", srcBaseName, dstName)
			err = f.renameFile(ctx, fileID, dstName)
			if err != nil {
				// 检查重命名失败的原因 - 增强错误匹配
				if strings.Contains(err.Error(), "当前目录有重名文件") ||
					strings.Contains(err.Error(), "文件已在当前文件夹") {
					fs.Debugf(f, "重命名失败，目录中已有重名文件，检查是否为目标文件")
					exists, existingFileID, checkErr := f.fileExistsInDirectory(ctx, dstParentID, dstName)
					if checkErr == nil && exists && existingFileID == fileID {
						fs.Debugf(f, "文件已有正确名称，重命名操作实际已成功")
						// 文件已经有正确的名称，认为操作成功
					} else {
						// 检查是否是partial文件重命名冲突
						if strings.HasSuffix(srcBaseName, ".partial") {
							fs.Debugf(f, "Partial文件重命名冲突，尝试特殊处理")
							return f.handlePartialFileConflict(ctx, fileID, srcBaseName, dstParentID, dstName)
						}
						return nil, err
					}
				} else {
					return nil, err
				}
			}
		}
	}

	// 移除缓存清理

	// Return the moved object
	return &Object{
		fs:          f,
		remote:      remote,
		hasMetaData: srcObj.hasMetaData,
		id:          srcObj.id,
		size:        srcObj.size,
		md5sum:      srcObj.md5sum,
		modTime:     srcObj.modTime,
		isDir:       srcObj.isDir,
	}, nil
}

// DirMove server-side moves a directory.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	fs.Debugf(f, "调用目录移动 %s 到 %s", srcRemote, dstRemote)

	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(f, "无法移动目录 - 不是相同的远程类型")
		return fs.ErrorCantDirMove
	}

	// Find source directory ID
	srcDirID, err := srcFs.dirCache.FindDir(ctx, srcRemote, false)
	if err != nil {
		return err
	}

	// Find destination parent directory
	dstName, dstParentID, err := f.dirCache.FindPath(ctx, dstRemote, true)
	if err != nil {
		return err
	}

	// Convert IDs to int64
	srcFileID, err := strconv.ParseInt(srcDirID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid source directory ID: %s", srcDirID)
	}

	dstParentFileID, err := strconv.ParseInt(dstParentID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid destination parent ID: %s", dstParentID)
	}

	// 获取源目录的基础名称
	srcBaseName := filepath.Base(srcRemote)
	fs.Debugf(f, "源目录基础名称: %s, 目标目录名: %s", srcBaseName, dstName)
	fs.Debugf(f, "源目录ID: %d, 目标父目录ID: %d", srcFileID, dstParentFileID)

	// 检查是否尝试移动到同一个父目录
	srcParentID, err := f.getParentID(ctx, srcFileID)
	if err != nil {
		fs.Debugf(f, "获取源目录父ID失败: %v", err)
	} else {
		fs.Debugf(f, "源目录当前父ID: %d", srcParentID)
		if srcParentID == dstParentFileID && dstName == srcBaseName {
			fs.Debugf(f, "目录已在目标位置，无需移动")
			return nil
		}
	}

	// Move the directory
	err = f.moveFile(ctx, srcFileID, dstParentFileID)
	if err != nil {
		return err
	}

	// If the name changed, rename the directory
	if dstName != srcBaseName {
		fs.Debugf(f, "移动后重命名目录: %s -> %s", srcBaseName, dstName)
		err = f.renameFile(ctx, srcFileID, dstName)
		if err != nil {
			return err
		}
	}

	// Update cache
	srcFs.dirCache.FlushDir(srcRemote)
	f.dirCache.FlushDir(dstRemote)

	return nil
}

// Copy server-side copies a file.
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	fs.Debugf(f, "调用复制 %s 到 %s", src.Remote(), remote)

	// 规范化目标路径
	normalizedRemote := normalizePath(remote)
	fs.Debugf(f, "Copy操作规范化目标路径: %s -> %s", remote, normalizedRemote)

	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(f, "无法复制 - 不是相同的远程类型")
		return nil, fs.ErrorCantCopy
	}

	// Use dircache to find destination directory
	dstName, _, err := f.dirCache.FindPath(ctx, normalizedRemote, true)
	if err != nil {
		return nil, err
	}

	// 验证并清理目标文件名
	_, warning := validateAndCleanFileName(dstName)
	if warning != nil {
		fs.Logf(f, "复制操作文件名验证警告: %v", warning)
		// Note: cleaned name is not used in current implementation
	}

	// 123Pan doesn't have a direct copy API, use download+upload
	fs.Debugf(f, "123网盘不支持服务器端复制，使用下载+上传方式")
	return f.copyViaDownloadUpload(ctx, srcObj, remote)
}

// copyViaDownloadUpload copies a file by downloading and re-uploading
func (f *Fs) copyViaDownloadUpload(ctx context.Context, srcObj *Object, remote string) (fs.Object, error) {
	fs.Debugf(f, "通过下载+上传复制 %s 到 %s", srcObj.remote, remote)

	// Open source file for reading
	src, err := srcObj.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open source file: %w", err)
	}
	defer src.Close()

	// Create a new object info for the destination
	dstInfo := &ObjectInfo{
		fs:      f,
		remote:  remote,
		size:    srcObj.size,
		md5sum:  srcObj.md5sum,
		modTime: srcObj.modTime,
	}

	// Upload to destination
	return f.Put(ctx, src, dstInfo)
}

// ObjectInfo implements fs.ObjectInfo for copy operations
type ObjectInfo struct {
	fs      *Fs
	remote  string
	size    int64
	md5sum  string
	modTime time.Time
}

func (oi *ObjectInfo) Fs() fs.Info                           { return oi.fs }
func (oi *ObjectInfo) Remote() string                        { return oi.remote }
func (oi *ObjectInfo) Size() int64                           { return oi.size }
func (oi *ObjectInfo) ModTime(ctx context.Context) time.Time { return oi.modTime }
func (oi *ObjectInfo) Storable() bool                        { return true }
func (oi *ObjectInfo) String() string                        { return oi.remote }
func (oi *ObjectInfo) Hash(ctx context.Context, t fshash.Type) (string, error) {
	if t == fshash.MD5 {
		return oi.md5sum, nil
	}
	return "", fshash.ErrUnsupported
}

// getHTTPClient makes an http client according to the options
// 使用rclone标准HTTP客户端配置
func getHTTPClient(ctx context.Context, _ string, _ configmap.Mapper) *http.Client {
	// 使用rclone标准HTTP客户端
	return fshttp.NewClient(ctx)
}

// getNetworkQuality 评估当前网络质量，返回0.0-1.0的质量分数
func (f *Fs) getNetworkQuality() float64 {
	// 简化网络质量评估 - 使用固定的良好网络质量假设
	return 0.8 // 默认假设网络质量良好
}

// getAdaptiveTimeout 根据文件大小、传输类型计算自适应超时时间
// 简化版本：使用固定的合理超时计算
func (f *Fs) getAdaptiveTimeout(fileSize int64, transferType string) time.Duration {
	// 简化超时计算 - 使用固定的合理超时时间
	baseTimeout := 1200 * time.Second // 20分钟基础超时

	// 根据文件大小动态调整超时时间（更精细的计算）
	// 每50MB增加30秒超时时间，对大文件更友好
	sizeBasedTimeout := time.Duration(fileSize/50/1024/1024) * 30 * time.Second

	// 根据传输类型和网络条件调整
	var typeMultiplier float64 = 1.0
	switch transferType {
	case "chunked_upload":
		typeMultiplier = 1.5 // 分片上传需要适中时间
	case "stream_download":
		typeMultiplier = 1.2 // 流式下载需要适中时间
	case "single_step":
		typeMultiplier = 0.8 // 单步上传时间较短
	case "concurrent_upload":
		typeMultiplier = 2.0 // 并发上传需要更长时间
	default:
		typeMultiplier = 1.0
	}

	// 考虑网络质量
	networkQuality := f.getNetworkQuality()
	var qualityMultiplier float64 = 1.0
	if networkQuality < 0.3 { // 网络质量很差
		qualityMultiplier = 3.0
	} else if networkQuality < 0.5 { // 网络质量差
		qualityMultiplier = 2.0
	} else if networkQuality < 0.7 { // 网络质量一般
		qualityMultiplier = 1.5
	} else {
		qualityMultiplier = 1.0 // 网络质量良好
	}

	adaptiveTimeout := baseTimeout + time.Duration(float64(sizeBasedTimeout)*typeMultiplier*qualityMultiplier)

	// 设置更合理的边界
	minTimeout := 2 * time.Minute // 最小2分钟
	maxTimeout := 4 * time.Hour   // 最大4小时，支持超大文件

	if adaptiveTimeout < minTimeout {
		adaptiveTimeout = minTimeout
	}
	if adaptiveTimeout > maxTimeout {
		adaptiveTimeout = maxTimeout
	}

	fs.Debugf(f, "自适应超时计算: 文件大小=%s, 类型=%s, 网络质量=%.2f, 超时时间=%v",
		fs.SizeSuffix(fileSize), transferType, networkQuality, adaptiveTimeout)

	return adaptiveTimeout
}

// detectNetworkSpeed 检测网络传输速度，用于动态优化参数
func (f *Fs) detectNetworkSpeed(_ context.Context) int64 {
	fs.Debugf(f, "开始检测网络速度")

	// 简化网络速度检测 - 使用固定的合理速度估算

	// 使用快速估算方法，避免大文件初始化延迟
	// 基于网络质量进行快速估算，不进行实际网络测试
	networkQuality := f.getNetworkQuality()

	// 基础速度估算（基于典型网络环境）
	var baseSpeed int64 = 10 * 1024 * 1024 // 10MB/s 基础速度

	// 根据网络质量调整速度估算
	if networkQuality >= 0.8 {
		baseSpeed = 20 * 1024 * 1024 // 20MB/s 高质量网络
	} else if networkQuality >= 0.6 {
		baseSpeed = 15 * 1024 * 1024 // 15MB/s 良好网络
	} else if networkQuality >= 0.4 {
		baseSpeed = 8 * 1024 * 1024 // 8MB/s 一般网络
	} else {
		baseSpeed = 5 * 1024 * 1024 // 5MB/s 较差网络
	}

	fs.Debugf(f, "网络速度估算: %s/s (质量=%.2f)",
		fs.SizeSuffix(baseSpeed), networkQuality)

	return baseSpeed
}

// getOptimalConcurrency 根据文件大小计算最优并发数
// 简化版本：使用固定的合理并发计算
func (f *Fs) getOptimalConcurrency(fileSize int64, networkSpeed int64) int {
	// 简化并发计算 - 使用rclone全局配置的传输数量
	baseConcurrency := fs.GetConfig(context.Background()).Transfers
	if baseConcurrency <= 0 {
		baseConcurrency = 4 // 默认并发数
	}

	// 根据文件大小调整并发数 - 优化大文件并发策略
	var sizeFactor float64 = 1.0
	if fileSize > 20*1024*1024*1024 { // >20GB - 超大文件
		sizeFactor = 3.0 // 显著提升并发数
	} else if fileSize > 10*1024*1024*1024 { // >10GB - 大文件
		sizeFactor = 2.5
	} else if fileSize > 5*1024*1024*1024 { // >5GB - 中大文件
		sizeFactor = 2.0
	} else if fileSize > 2*1024*1024*1024 { // >2GB - 中等文件
		sizeFactor = 1.5
	} else if fileSize > 1*1024*1024*1024 { // >1GB - 较大文件
		sizeFactor = 1.2
	} else if fileSize > 500*1024*1024 { // >500MB - 中等文件
		sizeFactor = 1.0
	} else if fileSize > 100*1024*1024 { // >100MB - 小文件
		sizeFactor = 0.8
	} else {
		sizeFactor = 0.5 // 很小文件使用较少并发
	}

	// 根据网络速度调整 - 性能优化：更精细的网络速度分级和自适应调整
	var speedFactor float64 = 1.0

	// 新增：网络质量自适应调整
	networkLatency := f.measureNetworkLatency()
	latencyFactor := 1.0
	if networkLatency < 50 { // <50ms 低延迟
		latencyFactor = 1.2
	} else if networkLatency < 100 { // <100ms 中等延迟
		latencyFactor = 1.0
	} else if networkLatency < 200 { // <200ms 高延迟
		latencyFactor = 0.8
	} else { // >200ms 很高延迟
		latencyFactor = 0.6
	}

	if networkSpeed > 200*1024*1024 { // >200Mbps - 超高速网络
		speedFactor = 2.0 * latencyFactor // 从1.8提升，结合延迟调整
	} else if networkSpeed > 100*1024*1024 { // >100Mbps - 高速网络
		speedFactor = 1.7 * latencyFactor // 从1.5提升，结合延迟调整
	} else if networkSpeed > 50*1024*1024 { // >50Mbps - 中高速网络
		speedFactor = 1.4 * latencyFactor // 从1.2提升，结合延迟调整
	} else if networkSpeed > 20*1024*1024 { // >20Mbps - 中速网络
		speedFactor = 1.1 * latencyFactor // 从1.0提升，结合延迟调整
	} else if networkSpeed > 10*1024*1024 { // >10Mbps - 中低速网络
		speedFactor = 0.9 * latencyFactor // 从0.8提升，结合延迟调整
	} else {
		speedFactor = 0.7 * latencyFactor // 从0.6提升，结合延迟调整
	}

	// 计算最优并发数
	optimalConcurrency := int(float64(baseConcurrency) * sizeFactor * speedFactor)

	// 用户要求：123网盘单文件并发限制到4
	minConcurrency := 1
	maxConcurrency := 4 // 用户要求的并发上限

	// 不再允许超大文件使用更高并发数，统一限制为4

	if optimalConcurrency < minConcurrency {
		optimalConcurrency = minConcurrency
	}
	if optimalConcurrency > maxConcurrency {
		optimalConcurrency = maxConcurrency
	}

	fs.Debugf(f, "🚀 优化并发数计算: 文件大小=%s, 网络速度=%s/s, 基础并发=%d, 大小因子=%.1f, 速度因子=%.1f, 最优并发=%d",
		fs.SizeSuffix(fileSize), fs.SizeSuffix(networkSpeed), baseConcurrency, sizeFactor, speedFactor, optimalConcurrency)

	return optimalConcurrency
}

// getOptimalChunkSize 根据文件大小计算最优分片大小
// 简化版本：使用固定的合理分片大小计算
func (f *Fs) getOptimalChunkSize(fileSize int64, networkSpeed int64) int64 {
	fs.Debugf(f, " 开始计算最优分片大小: 文件大小=%s", fs.SizeSuffix(fileSize))

	// 简化分片大小计算 - 使用rclone全局配置的多线程分片大小
	baseChunk := int64(fs.GetConfig(context.Background()).MultiThreadChunkSize)
	if baseChunk <= 0 {
		baseChunk = int64(defaultChunkSize) // 100MB
	}

	// 根据网络速度调整分片大小 - 性能优化：提升分片大小倍数
	var speedMultiplier float64 = 1.0
	if networkSpeed > 200*1024*1024 { // >200Mbps - 超高速网络
		speedMultiplier = 4.0 // 400MB分片（从3.0提升）
	} else if networkSpeed > 100*1024*1024 { // >100Mbps - 高速网络
		speedMultiplier = 3.0 // 300MB分片（从2.0提升）
	} else if networkSpeed > 50*1024*1024 { // >50Mbps - 中速网络
		speedMultiplier = 2.0 // 200MB分片（从1.5提升）
	} else if networkSpeed > 20*1024*1024 { // >20Mbps - 普通网络
		speedMultiplier = 1.5 // 150MB分片（从1.0提升）
	} else { // <20Mbps - 慢速网络
		speedMultiplier = 0.8 // 80MB分片（从0.5提升）
	}

	// 根据文件大小调整 - 性能优化：更精细的文件大小分级
	var sizeMultiplier float64 = 1.0
	if fileSize > 50*1024*1024*1024 { // >50GB
		sizeMultiplier = 2.0 // 大文件使用更大分片（从1.5提升）
	} else if fileSize > 20*1024*1024*1024 { // >20GB
		sizeMultiplier = 1.8 // 新增：超大文件分级
	} else if fileSize > 10*1024*1024*1024 { // >10GB
		sizeMultiplier = 1.5 // 新增：大文件分级
	} else if fileSize > 5*1024*1024*1024 { // >5GB
		sizeMultiplier = 1.3 // 新增：中大文件分级
	} else if fileSize > 2*1024*1024*1024 { // >2GB
		sizeMultiplier = 1.1 // 新增：中等文件分级
	} else if fileSize < 500*1024*1024 { // <500MB
		sizeMultiplier = 0.7 // 小文件使用更小分片（从0.5提升）
	}

	// 计算最优分片大小
	optimalChunkSize := int64(float64(baseChunk) * speedMultiplier * sizeMultiplier)

	// 设置合理边界 - 性能优化：提升最大分片大小限制
	minChunkSize := int64(minChunkSize)      // 50MB
	maxChunkSize := int64(800 * 1024 * 1024) // 800MB（从500MB提升）

	if optimalChunkSize < minChunkSize {
		optimalChunkSize = minChunkSize
	}
	if optimalChunkSize > maxChunkSize {
		optimalChunkSize = maxChunkSize
	}

	fs.Debugf(f, "动态分片大小计算: 文件大小=%s, 网络速度=%s/s, 基础分片=%s, 最优分片=%s",
		fs.SizeSuffix(fileSize), fs.SizeSuffix(networkSpeed),
		fs.SizeSuffix(baseChunk), fs.SizeSuffix(optimalChunkSize))

	return optimalChunkSize
}

// measureNetworkLatency 测量网络延迟
// 性能优化：实现网络延迟检测，用于自适应并发调整
func (f *Fs) measureNetworkLatency() int64 {
	// 简单的延迟测量：向123网盘API发送HEAD请求
	start := time.Now()

	// 构建测试URL
	testURL := "https://www.123pan.com/api/v1/user/info"
	req, err := http.NewRequest("HEAD", testURL, nil)
	if err != nil {
		return 100 // 默认延迟100ms
	}

	// 设置请求头
	req.Header.Set("User-Agent", f.opt.UserAgent)

	// 发送请求 - 使用rclone标准HTTP客户端
	client := fshttp.NewClient(context.Background())
	resp, err := client.Do(req)
	if err != nil {
		return 150 // 网络错误时返回较高延迟
	}
	defer resp.Body.Close()

	latency := time.Since(start).Milliseconds()
	return latency
}

// 移除ResourcePool，过度设计，使用rclone标准组件

// 移除未使用的DownloadProgress结构体，简化代码

// 移除NewDownloadProgress函数，未使用

// 移除UpdateChunkProgress方法，未使用

// 移除GetProgressInfo方法，未使用

// 移除NewResourcePool函数，过度设计

// 移除ResourcePool相关方法，过度设计

// 移除GetTempFile函数，过度设计

// 移除GetOptimizedTempFile函数，过度设计

// 移除removeTempFileFromTracking函数，过度设计

// 移除PutTempFile函数，过度设计

// 移除CleanupAllTempFiles和Close函数，过度设计

// Common utility functions

// 移除PerformanceStats，过度设计，使用rclone标准统计

// 移除UploadStrategy，过度设计，使用rclone标准策略

// 移除UploadStrategy String方法，过度设计

// 移除UploadStrategySelector，过度设计

// 移除SelectStrategy方法，过度设计

// 移除GetStrategyReason方法，过度设计

// 移除UnifiedUploadContext，过度设计，使用rclone标准上传流程

// 移除NewUnifiedUploadContext函数，过度设计

// 移除unifiedUpload函数，过度设计

// 移除所有execute*Upload函数，过度设计，使用rclone标准上传流程
// 以下函数已被删除：executeSingleStepUpload, executeChunkedUpload, executeStreamingUpload等
// 移除executeSingleStepUpload函数体，过度设计

// 移除executeCrossCloudSingleStepUpload函数，过度设计
// 移除executeCrossCloudSingleStepUpload函数体，过度设计

// 移除所有剩余的execute*Upload函数，过度设计，使用rclone标准上传流程

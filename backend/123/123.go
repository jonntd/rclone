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
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rclone/rclone/lib/cache"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
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
	// Note: uploadCreateMinSleep, mkdirMinSleep, fileTrashMinSleep removed (unused)

	downloadInfoMinSleep = 500 * time.Millisecond // ~2 QPS 用于 api/v1/file/download_info (保持不变)

	// 根据123网盘API限流表优化的上传分片QPS
	uploadV2SliceMinSleep = 20 * time.Millisecond // 50 QPS 用于 upload/v2/file/slice (API限流表最高值)

	// 文件上传相关常量 - 根据123网盘OpenAPI文档
	singleStepUploadLimit = 1024 * 1024 * 1024 // 1GB - V2版本单步上传API的文件大小限制
	maxMemoryBufferSize   = 1024 * 1024 * 1024 // 1GB - 内存缓冲的最大大小（从512MB提升）
	maxFileNameBytes      = 255                // 文件名的最大字节长度（UTF-8编码）

	// 上传相关常量 - 优化大文件传输性能
	defaultChunkSize    = 100 * fs.Mebi // 增加默认分片大小到100MB
	minChunkSize        = 50 * fs.Mebi  // 增加最小分片大小到50MB
	maxChunkSize        = 500 * fs.Mebi // 设置最大分片大小为500MB
	defaultUploadCutoff = 100 * fs.Mebi // 降低分片上传阈值
	maxUploadParts      = 10000

	// 文件名验证相关常量
	maxFileNameLength = 256          // 123网盘文件名最大长度（包括扩展名）
	invalidChars      = `"\/:*?|><\` // 123网盘不允许的文件名字符
	replacementChar   = "_"          // 用于替换非法字符的安全字符
	// 配置验证相关常量
	MaxListChunk          = 10000 // 最大列表块大小
	MaxUploadPartsLimit   = 10000 // 最大上传分片数限制
	MaxConcurrentLimit    = 100   // 最大并发数限制
	DefaultListChunk      = 1000  // 默认列表块大小
	DefaultMaxUploadParts = 1000  // 默认最大上传分片数

	// Simplified network constants
	DefaultConcurrency = 4   // Default concurrency for uploads
	DefaultChunkSize   = 100 // Default chunk size in MB

	// Cache management constants
	PreloadQueueCapacity    = 1000 // 预加载队列容量
	MaxHotFiles             = 100  // 最大热点文件数
	HotFileCleanupBatchSize = 20   // 热点文件清理批次大小

	// CDN故障转移默认配置（无需用户配置）
	defaultMaxCDNRetries = 3 // 默认CDN重试最大次数

	// 下载相关常量
	defaultDownloadChunkSize   = 50 * 1024 * 1024 // 50MB 默认下载分片大小
	defaultDownloadConcurrency = 6                // 默认下载并发数
	minFileSizeForConcurrency  = 10 * 1024 * 1024 // 10MB 启用并发下载的最小文件大小
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

	// 流式哈希传输模式选项
	StreamHashMode bool `config:"stream_hash_mode"`

	// CDN故障转移配置选项（默认启用，无需用户配置）
	// EnableCDNFailover、MaxCDNRetries等已设为合理默认值

	// 编码配置
	Enc encoder.MultiEncoder `config:"encoding"` // 文件名编码设置
}

// CachedURL 缓存的下载URL信息
type CachedURL struct {
	URL       string    // 下载URL
	ExpiresAt time.Time // 过期时间
}

// 💾 持久化缓存数据结构
type PersistentParentDirCache struct {
	Data    map[int64]time.Time `json:"data"`
	SavedAt time.Time           `json:"saved_at"`
	Version string              `json:"version"`
}

type PersistentListFileCache struct {
	Data    map[string]*CachedListResponse `json:"data"`
	SavedAt time.Time                      `json:"saved_at"`
	Version string                         `json:"version"`
}

type CachedListResponse struct {
	Response  *ListResponse `json:"response"`
	CachedAt  time.Time     `json:"cached_at"`
	ExpiresAt time.Time     `json:"expires_at"`
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

	// 配置和状态信息
	m            configmap.Mapper // 配置映射器
	rootFolderID string           // 根文件夹的ID

	// 缓存系统
	parentDirCache   map[int64]time.Time // 缓存已验证的父目录ID和验证时间
	downloadURLCache sync.Map            // 下载URL缓存 (map[string]CachedURL) - 使用sync.Map提升并发性能
	listFileCache    *cache.Cache        // ListFile结果缓存
	cacheMu          sync.RWMutex        // 保护parentDirCache的读写锁

	// 💾 持久化缓存系统
	persistentCacheDir string // 持久化缓存目录
	parentDirCacheFile string // 父目录缓存文件路径
	listFileCacheFile  string // ListFile缓存文件路径

	// 上传域名缓存
	uploadDomain    string       // 缓存的上传域名
	uploadDomainExp time.Time    // 上传域名过期时间
	uploadDomainMu  sync.RWMutex // 保护上传域名缓存的读写锁

	// 性能和速率限制 - 根据123网盘API限流表精确优化的调速器
	listPacer     *fs.Pacer // 文件列表API的调速器（10 QPS）
	strictPacer   *fs.Pacer // 严格API（如user/info, move, delete）的调速器（10 QPS）
	uploadPacer   *fs.Pacer // 上传分片API的调速器（50 QPS - 最高）
	downloadPacer *fs.Pacer // 下载API的调速器（10 QPS）
	batchPacer    *fs.Pacer // 批量操作API的调速器（5 QPS - 最低）
	tokenPacer    *fs.Pacer // 令牌相关API的调速器（1 QPS - 最严格）

	// 目录缓存
	dirCache *dircache.DirCache
}

// ProgressReadCloser 混合进度显示包装器
// 🔧 既提供详细的进度日志，又兼容rclone的accounting系统
type ProgressReadCloser struct {
	io.ReadCloser
	fs              *Fs
	object          *Object
	totalSize       int64
	transferredSize int64
	startTime       time.Time
	lastLogTime     time.Time
	lastLogPercent  int
}

// Read 实现io.Reader接口，提供详细进度日志但不干扰rclone的accounting
func (prc *ProgressReadCloser) Read(p []byte) (n int, err error) {
	n, err = prc.ReadCloser.Read(p)
	if n > 0 {
		prc.transferredSize += int64(n)

		// 计算进度百分比
		var percentage int
		if prc.totalSize > 0 {
			percentage = int(float64(prc.transferredSize) / float64(prc.totalSize) * 100)
		}

		now := time.Now()
		// 减少日志频率：只在进度变化超过2%或时间间隔超过5秒时输出日志
		shouldLog := (percentage >= prc.lastLogPercent+2) ||
			(now.Sub(prc.lastLogTime) > 5*time.Second) ||
			(prc.transferredSize == prc.totalSize) ||
			(percentage == 100)

		if shouldLog {
			elapsed := now.Sub(prc.startTime)
			var speed float64
			if elapsed.Seconds() > 0 {
				speed = float64(prc.transferredSize) / elapsed.Seconds() / 1024 / 1024 // MB/s
			}

			fs.Debugf(prc.object, "📥 123下载进度: %s/%s (%d%%) 速度: %.2f MB/s",
				fs.SizeSuffix(prc.transferredSize),
				fs.SizeSuffix(prc.totalSize),
				percentage,
				speed)
			prc.lastLogTime = now
			prc.lastLogPercent = percentage
		}
	}
	return n, err
}

// Close 实现io.Closer接口，同时清理资源
func (prc *ProgressReadCloser) Close() error {
	// 输出最终下载统计
	elapsed := time.Since(prc.startTime)
	var avgSpeed float64
	if elapsed.Seconds() > 0 {
		avgSpeed = float64(prc.transferredSize) / elapsed.Seconds() / 1024 / 1024 // MB/s
	}

	fs.Debugf(prc.object, "✅ 123下载完成: %s, 耗时: %v, 平均速度: %.2f MB/s",
		fs.SizeSuffix(prc.transferredSize),
		elapsed.Truncate(time.Second),
		avgSpeed)

	// 关闭底层ReadCloser
	return prc.ReadCloser.Close()
}

// TwoStepProgressReader 跨云传输统一进度显示
// 🔧 重新实现：解决跨云传输进度跳跃和不连续的问题
type TwoStepProgressReader struct {
	io.ReadCloser
	fs             *Fs
	remote         string
	totalSize      int64
	downloadBytes  int64
	uploadBytes    int64
	phase          string // "download" or "upload"
	startTime      time.Time
	lastLogTime    time.Time
	lastLogPercent int
	account        *accounting.Account
}

// NewTwoStepProgressReader 创建跨云传输进度跟踪器
func NewTwoStepProgressReader(rc io.ReadCloser, fs *Fs, remote string, size int64, account *accounting.Account) *TwoStepProgressReader {
	return &TwoStepProgressReader{
		ReadCloser: rc,
		fs:         fs,
		remote:     remote,
		totalSize:  size,
		phase:      "download",
		startTime:  time.Now(),
		account:    account,
	}
}

// Read 实现io.Reader接口，统一处理跨云传输进度
func (tpr *TwoStepProgressReader) Read(p []byte) (n int, err error) {
	n, err = tpr.ReadCloser.Read(p)
	if n > 0 {
		if tpr.phase == "download" {
			tpr.downloadBytes += int64(n)
			tpr.updateProgress()
		}
	}
	return n, err
}

// SwitchToUpload 切换到上传阶段
func (tpr *TwoStepProgressReader) SwitchToUpload() {
	tpr.phase = "upload"
	tpr.uploadBytes = 0
	fs.Debugf(tpr.fs, "🔄 跨云传输切换到上传阶段: %s", tpr.remote)
}

// UpdateUploadProgress 更新上传进度
func (tpr *TwoStepProgressReader) UpdateUploadProgress(uploaded int64) {
	if tpr.phase == "upload" {
		tpr.uploadBytes = uploaded
		tpr.updateProgress()
	}
}

// updateProgress 统一更新进度显示
func (tpr *TwoStepProgressReader) updateProgress() {
	var currentBytes, totalBytes int64
	var phasePercent, totalPercent int
	var emoji string

	if tpr.phase == "download" {
		currentBytes = tpr.downloadBytes
		totalBytes = tpr.totalSize
		phasePercent = int(float64(tpr.downloadBytes) / float64(tpr.totalSize) * 100)
		totalPercent = phasePercent / 2 // 下载占总进度的50%
		emoji = "📥"
	} else {
		currentBytes = tpr.uploadBytes
		totalBytes = tpr.totalSize
		phasePercent = int(float64(tpr.uploadBytes) / float64(tpr.totalSize) * 100)
		totalPercent = 50 + phasePercent/2 // 上传占总进度的50%
		emoji = "📤"
	}

	now := time.Now()
	// 减少日志频率：只在进度变化超过5%或时间间隔超过3秒时输出日志
	shouldLog := (totalPercent >= tpr.lastLogPercent+5) ||
		(now.Sub(tpr.lastLogTime) > 3*time.Second) ||
		(phasePercent == 100) ||
		(totalPercent == 100)

	if shouldLog {
		elapsed := now.Sub(tpr.startTime)
		var avgSpeed float64
		if elapsed.Seconds() > 0 {
			if tpr.phase == "download" {
				avgSpeed = float64(tpr.downloadBytes) / elapsed.Seconds() / 1024 / 1024
			} else {
				totalTransferred := tpr.totalSize + tpr.uploadBytes
				avgSpeed = float64(totalTransferred) / elapsed.Seconds() / 1024 / 1024
			}
		}

		fs.Infof(tpr.fs, "%s 跨云传输 %s: %d%% | 总进度: %d%% | %s/%s | 速度: %.2f MB/s",
			emoji, tpr.phase, phasePercent, totalPercent,
			fs.SizeSuffix(currentBytes), fs.SizeSuffix(totalBytes), avgSpeed)

		tpr.lastLogTime = now
		tpr.lastLogPercent = totalPercent
	}

	// 更新rclone的accounting系统（虚拟进度）
	if tpr.account != nil {
		_ = tpr.downloadBytes + tpr.uploadBytes
		// 不直接调用account.SetBytes，让rclone自然处理
		// 这里只是记录，实际进度由上面的日志显示
	}
}

// Close 关闭并输出最终统计
func (tpr *TwoStepProgressReader) Close() error {
	elapsed := time.Since(tpr.startTime)
	totalTransferred := tpr.downloadBytes + tpr.uploadBytes
	var avgSpeed float64
	if elapsed.Seconds() > 0 {
		avgSpeed = float64(totalTransferred) / elapsed.Seconds() / 1024 / 1024
	}

	fs.Infof(tpr.fs, "✅ 跨云传输完成: %s | 总耗时: %v | 平均速度: %.2f MB/s",
		tpr.remote, elapsed.Truncate(time.Second), avgSpeed)

	return tpr.ReadCloser.Close()
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
			fs.Debugf(nil, "❌ 删除临时文件失败: %s, 错误: %v", r.tempPath, removeErr)
		} else {
			fs.Debugf(nil, "✅ 删除临时文件成功: %s", r.tempPath)
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
			fs.Debugf(f, "📝 生成唯一文件名: %s -> %s", baseName, newName)
			return newName, nil
		}
	}

	// 如果尝试了999次都没有找到唯一名称，使用时间戳
	timestamp := time.Now().Unix()
	newName := fmt.Sprintf("%s_%d%s", nameWithoutExt, timestamp, ext)
	fs.Debugf(f, "⏰ 使用时间戳生成唯一文件名: %s -> %s", baseName, newName)
	return newName, nil
}

// fileExists 检查指定目录中是否存在指定名称的文件
func (f *Fs) fileExists(ctx context.Context, parentFileID int64, fileName string) (bool, error) {
	// 使用ListFile API检查文件是否存在（limit=100是API最大限制）
	response, err := f.ListFile(ctx, int(parentFileID), 100, "", "", 0)
	if err != nil {
		return false, err
	}

	for _, file := range response.Data.FileList {
		// 🔧 检查文件名匹配且文件有效（不在回收站且未被审核驳回）
		if file.Filename == fileName && isValidFile(file) {
			return true, nil
		}
	}

	return false, nil
}

// verifyParentFileID 验证父目录ID是否存在（带缓存优化）
func (f *Fs) verifyParentFileID(ctx context.Context, parentFileID int64) (bool, error) {
	// 检查缓存，如果最近验证过（5分钟内），直接返回成功
	f.cacheMu.RLock()
	if lastVerified, exists := f.parentDirCache[parentFileID]; exists {
		if time.Since(lastVerified) < 5*time.Minute {
			f.cacheMu.RUnlock()
			fs.Debugf(f, "📁 父目录ID %d 缓存验证通过（上次验证: %v）", parentFileID, lastVerified.Format("15:04:05"))
			return true, nil
		}
	}
	f.cacheMu.RUnlock()

	fs.Debugf(f, "🔍 验证父目录ID: %d", parentFileID)

	// 直接执行API验证，使用ListFile API（limit=100是API最大限制）
	response, err := f.ListFile(ctx, int(parentFileID), 100, "", "", 0)
	if err != nil {
		fs.Debugf(f, "❌ 验证父目录ID %d 失败: %v", parentFileID, err)
		return false, err
	}

	isValid := response.Code == 0
	if !isValid {
		fs.Debugf(f, "⚠️ 父目录ID %d 不存在，API返回: code=%d, message=%s", parentFileID, response.Code, response.Message)
	} else {
		// 验证成功，更新缓存
		f.cacheMu.Lock()
		f.parentDirCache[parentFileID] = time.Now()
		f.cacheMu.Unlock()
		// 💾 保存到持久化缓存
		go f.saveParentDirCache() // 异步保存，避免阻塞
		fs.Debugf(f, "✅ 父目录ID %d 验证成功", parentFileID)
	}

	return isValid, nil
}

// getCorrectParentFileID 获取上传API认可的正确父目录ID
// 统一版本：为单步上传和分片上传提供一致的父目录ID修复策略
func (f *Fs) getCorrectParentFileID(ctx context.Context, cachedParentID int64) int64 {
	fs.Debugf(f, "🔧 统一父目录ID修复策略，缓存ID: %d", cachedParentID)

	// Strategy 1: Re-acquire directory structure
	fs.Debugf(f, "🔄 重新获取目录结构")

	// Reset dirCache only, keep other valid caches
	if f.dirCache != nil {
		fs.Debugf(f, "🔄 重置目录缓存以清除过期的父目录ID: %d", cachedParentID)
		f.dirCache.ResetRoot()
	}

	// Strategy 2: Re-verify original ID (may recover after cache cleanup)
	fs.Debugf(f, "🔍 重新验证原始目录ID: %d", cachedParentID)
	exists, err := f.verifyParentFileID(ctx, cachedParentID)
	if err == nil && exists {
		fs.Debugf(f, "✅ 缓存清理后原始目录ID %d 验证成功", cachedParentID)
		return cachedParentID
	}

	// Strategy 3: Try to verify if root directory is available
	fs.Debugf(f, "🏠 尝试验证根目录 (parentFileID: 0)")
	rootExists, err := f.verifyParentFileID(ctx, 0)
	if err == nil && rootExists {
		fs.Debugf(f, "✅ 根目录验证成功，用作回退方案")
		return 0
	}

	// Strategy 4: Force fallback to root directory
	fs.Debugf(f, "❌ 所有验证策略失败，强制使用根目录 (parentFileID: 0)")
	return 0
}

// getParentID 获取文件或目录的父目录ID
func (f *Fs) getParentID(ctx context.Context, fileID int64) (int64, error) {
	fs.Debugf(f, "🔍 尝试获取文件ID %d 的父目录ID", fileID)

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

	fs.Debugf(f, "✅ 通过API获取到文件ID %d 的父目录ID: %d", fileID, response.Data.ParentFileID)
	return response.Data.ParentFileID, nil
}

// isValidFile 检查文件是否有效（不在回收站且未被审核驳回）
// 根据123云盘API文档，必须过滤 trashed=1 和 status>=100 的文件
func isValidFile(file FileListInfoRespDataV2) bool {
	return file.Trashed == 0 && file.Status < 100
}

// fileExistsInDirectory 检查指定目录中是否存在指定名称的文件
func (f *Fs) fileExistsInDirectory(ctx context.Context, parentID int64, fileName string) (bool, int64, error) {
	fs.Debugf(f, "🔍 检查目录 %d 中是否存在文件: %s", parentID, fileName)

	// 使用ListFile API检查文件是否存在（limit=100是API最大限制）
	response, err := f.ListFile(ctx, int(parentID), 100, "", "", 0)
	if err != nil {
		return false, 0, err
	}

	if response.Code != 0 {
		return false, 0, fmt.Errorf("ListFile API错误: code=%d, message=%s", response.Code, response.Message)
	}

	for _, file := range response.Data.FileList {
		// 🔧 检查文件名匹配且不在回收站且未被审核驳回
		if file.Filename == fileName && file.Trashed == 0 && file.Status < 100 {
			fs.Debugf(f, "✅ 找到文件 %s，ID: %d (trashed=%d, status=%d)", fileName, file.FileID, file.Trashed, file.Status)
			return true, file.FileID, nil
		}
	}

	fs.Debugf(f, "❌ 目录 %d 中未找到文件: %s", parentID, fileName)
	return false, 0, nil
}

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

// getCachedDownloadURL 获取缓存的下载URL
func (f *Fs) getCachedDownloadURL(fileID string) (string, bool) {
	// 使用sync.Map的Load方法，无需外部锁保护
	value, exists := f.downloadURLCache.Load(fileID)
	if !exists {
		return "", false
	}

	cached := value.(CachedURL)

	// 检查是否过期（根据123网盘API文档，下载URL有效期为2小时）
	if time.Now().After(cached.ExpiresAt) {
		// 过期了，异步清理（sync.Map的Delete是并发安全的）
		go f.downloadURLCache.Delete(fileID)
		return "", false
	}

	fs.Debugf(f, "⬇️ 使用缓存的下载URL: fileID=%s", fileID)
	return cached.URL, true
}

// setCachedDownloadURL 设置下载URL缓存
func (f *Fs) setCachedDownloadURL(fileID, url string) {
	// 下载URL缓存100分钟（优化TTL，更好利用2小时API有效期，留20分钟安全边际）
	cached := CachedURL{
		URL:       url,
		ExpiresAt: time.Now().Add(100 * time.Minute),
	}

	// 使用sync.Map的Store方法，无需外部锁保护
	f.downloadURLCache.Store(fileID, cached)

	fs.Debugf(f, "💾 缓存下载URL: fileID=%s, 有效期100分钟", fileID)
}

// invalidateDownloadURL 使指定文件的下载URL缓存失效
func (f *Fs) invalidateDownloadURL(fileID string) {
	f.downloadURLCache.Delete(fileID)
	fs.Debugf(f, "🗑️ 已清除下载URL缓存: fileID=%s", fileID)
}

// 🔧 移除非标准的启发式文件判断函数
// 遵循Google Drive、OneDrive等主流网盘的做法：完全信任服务器返回的类型标识

// isRemoteSource 检查源对象是否来自远程云盘（非本地文件）
// 使用rclone标准的Features.IsLocal方法进行检测
func (f *Fs) isRemoteSource(src fs.ObjectInfo) bool {
	srcFs := src.Fs()
	if srcFs == nil {
		fs.Debugf(f, "🔍 isRemoteSource: srcFs为nil，返回false")
		return false
	}

	// 使用rclone标准方法：检查Features.IsLocal
	features := srcFs.Features()
	if features != nil && features.IsLocal {
		fs.Debugf(f, "🏠 rclone标准检测: 本地文件系统 (%s)", srcFs.Name())
		return false
	}

	// 非本地文件系统即为远程源
	fs.Debugf(f, "☁️ rclone标准检测: 远程文件系统 (%s) - 启用跨云传输模式", srcFs.Name())
	return true
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
			Name: "stream_hash_mode",
			Help: `启用流式哈希计算模式，用于跨云传输时节省本地存储空间。
- false: 使用传统模式（默认）
- true: 先流式计算文件哈希尝试秒传，失败后再流式上传
适用于本地存储空间有限的环境。`,
			Default:  false,
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
		"sync-delete": "同步删除: false(仅创建.strm文件)/true(删除孤立文件和空目录) (默认: false，安全模式)",
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

	case "refresh-cache":
		// 🔄 新增：刷新目录缓存
		return f.refreshCacheCommand(ctx, arg)

	case "clear-cache":
		// 🧹 新增：清理持久化缓存
		return f.clearCacheCommand(ctx, arg, opt)

	case "cache-stats":
		// 📊 新增：查看缓存统计
		return f.cacheStatsCommand(ctx, arg, opt)

	default:
		return nil, fs.ErrorCommandNotFound
	}
}

// APIResponse 定义API响应的通用接口，用于检查响应码
type APIResponse interface {
	GetCode() int
	GetMessage() string
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

// GetCode 实现APIResponse接口
func (r *ListResponse) GetCode() int {
	return r.Code
}

// GetMessage 实现APIResponse接口
func (r *ListResponse) GetMessage() string {
	return r.Message
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
	// 回收站标识, 0-正常 1-在回收站 (重要：必须过滤trashed=1的文件)
	Trashed int `json:"trashed"`
}

func (f *Fs) ListFile(ctx context.Context, parentFileID, limit int, searchData, searchMode string, lastFileID int) (*ListResponse, error) {
	fs.Debugf(f, "📋 调用ListFile，参数：parentFileID=%d, limit=%d, lastFileID=%d", parentFileID, limit, lastFileID)

	// 🔧 性能优化：智能ListFile缓存（支持分页缓存）
	// 缓存条件：无搜索查询，limit=100（标准分页大小）
	if searchData == "" && searchMode == "" && limit == 100 {
		// 🚀 改进：支持分页缓存，为每个分页单独缓存
		cacheKey := fmt.Sprintf("listfile_%d_%d", parentFileID, lastFileID)
		if cached, found := f.listFileCache.GetMaybe(cacheKey); found {
			if result, ok := cached.(*ListResponse); ok {
				fs.Debugf(f, "🎯 ListFile缓存命中: parentFileID=%d, lastFileID=%d", parentFileID, lastFileID)
				return result, nil
			}
		}

		// 🚀 智能缓存：如果是第一页(lastFileID=0)，也检查完整目录缓存
		if lastFileID == 0 {
			fullCacheKey := fmt.Sprintf("listfile_full_%d", parentFileID)
			if cached, found := f.listFileCache.GetMaybe(fullCacheKey); found {
				if result, ok := cached.(*ListResponse); ok {
					fs.Debugf(f, "🎯 ListFile完整目录缓存命中: parentFileID=%d", parentFileID)
					return result, nil
				}
			}
		}
	}

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
	fs.Debugf(f, "🌐 API端点: %s%s", openAPIRootURL, endpoint)

	// 直接使用v2 API调用 - 已迁移到rclone标准方法
	var result ListResponse
	err := f.makeAPICallWithRest(ctx, endpoint, "GET", nil, &result)
	if err != nil {
		fs.Debugf(f, "❌ ListFile API调用失败: %v", err)
		return nil, err
	}

	fs.Debugf(f, "✅ ListFile API响应: code=%d, message=%s, fileCount=%d", result.Code, result.Message, len(result.Data.FileList))

	// 🔧 性能优化：智能缓存存储（支持分页缓存）
	if searchData == "" && searchMode == "" && limit == 100 {
		// 🚀 改进：为每个分页单独缓存
		cacheKey := fmt.Sprintf("listfile_%d_%d", parentFileID, lastFileID)
		f.listFileCache.Put(cacheKey, &result)
		// 💾 同时保存到持久化缓存
		f.saveListFileCacheEntry(cacheKey, &result)
		fs.Debugf(f, "💾 ListFile结果已缓存: parentFileID=%d, lastFileID=%d", parentFileID, lastFileID)

		// 🚀 智能缓存：如果是第一页且返回的文件数少于limit，说明是完整目录，额外缓存
		if lastFileID == 0 && len(result.Data.FileList) < limit {
			fullCacheKey := fmt.Sprintf("listfile_full_%d", parentFileID)
			f.listFileCache.Put(fullCacheKey, &result)
			f.saveListFileCacheEntry(fullCacheKey, &result)
			fs.Debugf(f, "💾 ListFile完整目录已缓存: parentFileID=%d (%d个文件)", parentFileID, len(result.Data.FileList))
		}
	}

	return &result, nil
}

// listFileDirectAPI 直接调用API获取文件列表，绕过缓存（用于强制刷新）
func (f *Fs) listFileDirectAPI(ctx context.Context, parentFileID, limit int, searchData, searchMode string, lastFileID int) (*ListResponse, error) {
	fs.Debugf(f, "🌐 直接API调用ListFile，参数：parentFileID=%d, limit=%d, lastFileID=%d", parentFileID, limit, lastFileID)

	// 构建API参数
	params := url.Values{}
	params.Set("limit", strconv.Itoa(limit))
	params.Set("parentFileId", strconv.Itoa(parentFileID))
	if lastFileID > 0 {
		params.Set("lastFileId", strconv.Itoa(lastFileID))
	}
	if searchData != "" {
		params.Set("searchData", searchData)
	}
	if searchMode != "" {
		params.Set("searchMode", searchMode)
	}

	// 直接调用API，不使用缓存
	var result ListResponse
	err := f.makeAPICallWithRest(ctx, "/api/v2/file/list?"+params.Encode(), "GET", nil, &result)
	if err != nil {
		return nil, fmt.Errorf("ListFile API调用失败: %w", err)
	}

	fs.Debugf(f, "🌐 直接API调用成功: code=%d, message=%s, fileCount=%d", result.Code, result.Message, len(result.Data.FileList))
	return &result, nil
}

// clearListFileCache 清除指定父目录的ListFile缓存
func (f *Fs) clearListFileCache(parentFileID int64, reason string) {
	// 🚀 改进：清除所有相关的缓存键（分页缓存和完整目录缓存）

	// 清除完整目录缓存
	fullCacheKey := fmt.Sprintf("listfile_full_%d", parentFileID)
	if f.listFileCache.Delete(fullCacheKey) {
		fs.Debugf(f, "🗑️ 清除ListFile完整目录缓存: parentFileID=%d (%s)", parentFileID, reason)
	}

	// 清除分页缓存（尝试清除常见的分页）
	// 注意：这里只能清除已知的分页，实际使用中可能需要更智能的缓存管理
	for lastFileID := 0; lastFileID < 10; lastFileID++ {
		cacheKey := fmt.Sprintf("listfile_%d_%d", parentFileID, lastFileID)
		if f.listFileCache.Delete(cacheKey) {
			fs.Debugf(f, "🗑️ 清除ListFile分页缓存: parentFileID=%d, lastFileID=%d (%s)", parentFileID, lastFileID, reason)
		}
	}
}

// clearListFileCacheByPath 根据路径清除相关的ListFile缓存
func (f *Fs) clearListFileCacheByPath(ctx context.Context, dirPath string, reason string) {
	if dirPath == "" {
		dirPath = "/"
	}

	// 清除父目录的缓存
	parentDir := path.Dir(dirPath)
	if parentDir == "." {
		parentDir = ""
	}

	parentID, err := f.dirCache.FindDir(ctx, parentDir, false)
	if err == nil {
		if parentFileID, err2 := strconv.ParseInt(parentID, 10, 64); err2 == nil {
			f.clearListFileCache(parentFileID, reason)
		}
	}

	// 如果是根目录操作，也清除根目录缓存
	if parentDir == "" || parentDir == "/" {
		f.clearListFileCache(0, reason)
	}
}

func (f *Fs) pathToFileID(ctx context.Context, filePath string) (string, error) {
	fs.Debugf(f, "🔍 pathToFileID开始: filePath=%s", filePath)

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
			// 使用列表调速器进行速率限制（limit=100是API最大限制）
			err = f.listPacer.Call(func() (bool, error) {
				response, err = f.ListFile(ctx, parentFileID, 100, "", "", lastFileIDInt)
				if err != nil {
					return shouldRetry(err)
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
				// 只处理有效文件（不在回收站且未被审核驳回）
				if item.Trashed == 0 && item.Status < 100 && item.Filename == part {
					currentID = strconv.FormatInt(item.FileID, 10)
					findPart = true

					// 记录找到的项目类型信息，用于后续缓存
					isDir := (item.Type == 1) // Type: 0-文件  1-文件夹

					// ✅ 直接使用123网盘API的Type字段，无需额外判断
					// API的Type字段已经很准确：0=文件，1=文件夹
					fs.Debugf(f, "✅ 123网盘API类型: '%s' Type=%d, isDir=%v", part, item.Type, isDir)

					fs.Debugf(f, "pathToFileID找到项目: %s -> ID=%s, Type=%d, isDir=%v", part, currentID, item.Type, isDir)

					// 移除路径类型缓存，不再使用自定义缓存

					break
				} else if item.Filename == part {
					fs.Debugf(f, "🗑️ pathToFileID跳过无效文件: %s (trashed=%d, status=%d)", item.Filename, item.Trashed, item.Status)
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

// GetCode 实现APIResponse接口
func (r *FileInfoResponse) GetCode() int {
	return r.Code
}

// GetMessage 实现APIResponse接口
func (r *FileInfoResponse) GetMessage() string {
	return r.Message
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

// GetCode 实现APIResponse接口
func (r *FileDetailResponse) GetCode() int {
	return r.Code
}

// GetMessage 实现APIResponse接口
func (r *FileDetailResponse) GetMessage() string {
	return r.Message
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

// GetCode 实现APIResponse接口
func (r *FileInfosResponse) GetCode() int {
	return r.Code
}

// GetMessage 实现APIResponse接口
func (r *FileInfosResponse) GetMessage() string {
	return r.Message
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

// GetCode 实现APIResponse接口
func (r *DownloadInfoResponse) GetCode() int {
	return r.Code
}

// GetMessage 实现APIResponse接口
func (r *DownloadInfoResponse) GetMessage() string {
	return r.Message
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

// GetCode 实现APIResponse接口
func (r *UploadCreateResp) GetCode() int {
	return r.Code
}

// GetMessage 实现APIResponse接口
func (r *UploadCreateResp) GetMessage() string {
	return r.Message
}

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
		SpaceTempExpr  int64  `json:"spaceTempExpr"`
		Vip            bool   `json:"vip"`
		DirectTraffic  int64  `json:"directTraffic"`
		IsHideUID      bool   `json:"isHideUID"`
	} `json:"data"`
}

// GetCode 实现APIResponse接口
func (r *UserInfoResp) GetCode() int {
	return r.Code
}

// GetMessage 实现APIResponse接口
func (r *UserInfoResp) GetMessage() string {
	return r.Message
}

func (f *Fs) getDownloadURLByUA(ctx context.Context, filePath string, userAgent string) (string, error) {
	// Ensure token is valid before making API calls
	err := f.refreshTokenIfNecessary(false, false)
	if err != nil {
		return "", err
	}

	// Record User-Agent usage
	if userAgent != "" {
		fs.Debugf(f, "🌐 使用自定义User-Agent: %s", userAgent)
	} else {
		fs.Debugf(f, "🌐 使用默认User-Agent")
	}

	if filePath == "" {
		filePath = f.root
	}

	fileID, err := f.pathToFileID(ctx, filePath)
	if err != nil {
		return "", fmt.Errorf("failed to get file ID for path %q: %w", filePath, err)
	}

	fs.Debugf(f, "📥 通过路径获取下载URL: path=%s, fileId=%s", filePath, fileID)

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

	// Parse input format
	if extractedID, found := strings.CutPrefix(input, "123://"); found {
		// 123://fileId format (from .strm files)
		fileID = extractedID
		fs.Debugf(f, "📱 解析.strm格式: fileId=%s", fileID)
	} else if strings.HasPrefix(input, "/") {
		// File path format, needs conversion to fileId
		fs.Debugf(f, "📂 解析文件路径: %s", input)
		fileID, err = f.pathToFileID(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("path to fileId conversion failed %q: %w", input, err)
		}
		fs.Debugf(f, "✅ 路径转换成功: %s -> %s", input, fileID)
	} else {
		// Assume pure fileId
		fileID = input
		fs.Debugf(f, "🆔 使用纯文件ID: %s", fileID)
	}

	// 验证fileId格式
	if fileID == "" {
		return nil, fmt.Errorf("无效的fileId: %s", input)
	}

	// Get download URL
	userAgent := opt["user-agent"]
	if userAgent != "" {
		// If custom UA provided, use getDownloadURLByUA method
		fs.Debugf(f, "🌐 使用自定义UA获取下载URL: fileId=%s, UA=%s", fileID, userAgent)

		// Need to convert fileID back to path since getDownloadURLByUA needs path parameter
		var filePath string
		if strings.HasPrefix(input, "/") {
			filePath = input // If original input is path, use directly
		} else {
			// If original input is fileID, we can't easily convert back to path, so use standard method
			fs.Debugf(f, "⚠️ 自定义UA仅支持路径输入，文件ID输入将使用标准方法")
			downloadURL, err := f.getDownloadURL(ctx, fileID)
			if err != nil {
				return nil, fmt.Errorf("failed to get download URL: %w", err)
			}
			fs.Debugf(f, "✅ 成功获取下载URL: fileId=%s (标准方法)", fileID)
			return downloadURL, nil
		}

		downloadURL, err := f.getDownloadURLByUA(ctx, filePath, userAgent)
		if err != nil {
			return nil, fmt.Errorf("使用自定义UA获取下载URL失败: %w", err)
		}
		fs.Debugf(f, "✅ 使用自定义UA获取下载URL成功: fileId=%s", fileID)
		return downloadURL, nil
	}
	downloadURL, err := f.getDownloadURL(ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("failed to get download URL: %w", err)
	}
	fs.Debugf(f, "✅ 获取下载URL成功: fileId=%s", fileID)
	return downloadURL, nil
}

// refreshCacheCommand 刷新目录缓存
func (f *Fs) refreshCacheCommand(ctx context.Context, args []string) (any, error) {
	fs.Infof(f, "🔄 开始刷新缓存...")

	// 清除内存中的listFileCache
	f.listFileCache.Clear()
	fs.Debugf(f, "✅ 已清除ListFile缓存")

	// 清除持久化dirCache
	if err := f.dirCache.ForceRefreshPersistent(); err != nil {
		fs.Logf(f, "⚠️ 清除持久化缓存失败: %v", err)
	} else {
		fs.Debugf(f, "✅ 已清除持久化dirCache")
	}

	// 重置dirCache
	f.dirCache.Flush()
	fs.Debugf(f, "✅ 已重置内存dirCache")

	// 如果指定了路径，尝试重新构建该路径的缓存
	if len(args) > 0 && args[0] != "" {
		targetPath := args[0]
		fs.Debugf(f, "🔄 重新构建路径缓存: %s", targetPath)

		// 尝试查找目录以重新构建缓存
		if _, err := f.dirCache.FindDir(ctx, targetPath, false); err != nil {
			fs.Logf(f, "⚠️ 重新构建路径缓存失败: %v", err)
		} else {
			fs.Debugf(f, "✅ 路径缓存重新构建成功: %s", targetPath)
		}
	}

	return map[string]any{
		"status":  "success",
		"message": "缓存刷新完成",
	}, nil
}

// validateDuration 验证时间配置不为负数
func validateDuration(fieldName string, value time.Duration) error {
	if value < 0 {
		return fmt.Errorf("%s 不能为负数", fieldName)
	}
	return nil
}

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
func createPacer(ctx context.Context, minSleep time.Duration) *fs.Pacer {
	return fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(minSleep),
		pacer.MaxSleep(30*time.Second),
		pacer.DecayConstant(2)))
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

	// CDN故障转移功能已设置为合理的默认值，无需用户配置
}

// shouldRetry 根据响应和错误确定是否重试API调用（本地实现，替代common包）
func shouldRetry(err error) (bool, error) {
	// 使用rclone标准的重试判断
	return fserrors.ShouldRetry(err), err
}

// isConnectionTimeout 检查错误是否为连接超时
func isConnectionTimeout(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()

	// 检查常见的连接超时错误模式
	timeoutPatterns := []string{
		"dial tcp",
		"connect: connection timed out",
		"i/o timeout",
		"context deadline exceeded",
		"connection timeout",
		"timeout",
	}

	for _, pattern := range timeoutPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// openWithCDNFailover 带CDN故障转移的文件打开方法
func (o *Object) openWithCDNFailover(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// CDN故障转移默认启用

	var lastErr error
	maxRetries := defaultMaxCDNRetries

	for attempt := 0; attempt < maxRetries; attempt++ {
		fs.Debugf(o, "🔄 CDN故障转移尝试 %d/%d", attempt+1, maxRetries)

		rc, err := o.openNormal(ctx, options...)
		if err == nil {
			if attempt > 0 {
				fs.Infof(o, "✅ CDN故障转移成功 (尝试 %d/%d)", attempt+1, maxRetries)
			}
			return rc, nil
		}

		// 检查是否为CDN连接超时
		if isConnectionTimeout(err) {
			fs.Debugf(o, "🔄 检测到CDN连接超时，刷新下载URL (尝试 %d/%d): %v",
				attempt+1, maxRetries, err)

			// 强制刷新下载URL以获取新的CDN节点
			o.fs.invalidateDownloadURL(o.id)
			lastErr = err

			// 如果不是最后一次尝试，继续重试
			if attempt < maxRetries-1 {
				// 添加短暂延迟，避免过于频繁的重试
				time.Sleep(time.Duration(attempt+1) * time.Second)
				continue
			}
		} else {
			// 非连接超时错误，直接返回
			fs.Debugf(o, "❌ 非CDN超时错误，停止故障转移: %v", err)
			return nil, err
		}

		lastErr = err
	}

	fs.Errorf(o, "❌ CDN故障转移失败，已尝试 %d 次", maxRetries)
	return nil, fmt.Errorf("CDN failover exhausted after %d attempts: %w", maxRetries, lastErr)
}

// openWithSimpleRetry 简化的重试机制：失败时直接重新获取URL
func (o *Object) openWithSimpleRetry(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// 第一次尝试：使用缓存的下载URL
	rc, err := o.openNormal(ctx, options...)
	if err == nil {
		return rc, nil
	}

	// 检查是否为连接超时错误
	if isConnectionTimeout(err) {
		fs.Debugf(o, "🔄 下载失败，重新获取URL重试: %v", err)

		// 清除缓存的下载URL，强制重新获取（可能获得新的CDN节点）
		o.fs.invalidateDownloadURL(o.id)

		// 第二次尝试：使用新的下载URL
		rc, retryErr := o.openNormal(ctx, options...)
		if retryErr == nil {
			fs.Debugf(o, "✅ 重新获取URL后下载成功")
			return rc, nil
		}

		// 两次都失败，返回最后的错误
		fs.Debugf(o, "❌ 重新获取URL后仍然失败: %v", retryErr)
		return nil, retryErr
	}

	// 非连接超时错误，直接返回
	fs.Debugf(o, "❌ 非连接超时错误，不重试: %v", err)
	return nil, err
}

// makeAPICallWithRest 使用rclone标准rest客户端进行API调用，自动集成QPS限制
// 这是推荐的API调用方法，替代直接使用HTTP客户端
func (f *Fs) makeAPICallWithRest(ctx context.Context, endpoint string, method string, reqBody any, respBody any) error {
	fs.Debugf(f, "🌐 使用rclone标准方法进行API调用: %s %s", method, endpoint)

	// Safety check: ensure rest client is initialized
	if f.rst == nil {
		fs.Debugf(f, "🔧 Rest客户端未初始化，尝试重新创建")
		// 使用rclone标准方法重新创建rest客户端
		client := fshttp.NewClient(ctx) // 使用传入的context而不是background
		f.rst = rest.NewClient(client).SetRoot(openAPIRootURL)
		fs.Debugf(f, "✅ Rest客户端重新创建成功")
	}

	// Check token validity with reduced frequency
	if f.token == "" || time.Now().After(f.tokenExpiry.Add(-tokenRefreshWindow)) {
		err := f.refreshTokenIfNecessary(false, false)
		if err != nil {
			return fmt.Errorf("刷新令牌失败: %w", err)
		}
	}

	// 根据端点获取适当的调速器
	pacer := f.getPacerForEndpoint(endpoint)

	// 确定基础URL - 根据官方文档只有两个API使用上传域名
	var baseURL string
	if strings.Contains(endpoint, "/upload/v2/file/slice") ||
		strings.Contains(endpoint, "/upload/v2/file/single/create") {
		// 只有分片上传和单步上传使用上传域名（官方文档明确说明）
		uploadDomain := f.getUploadDomain(ctx)
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
	err := pacer.Call(func() (bool, error) {
		var err error
		resp, err = f.rst.CallJSON(ctx, &opts, reqBody, respBody)

		// 检查是否是HTTP 401错误，如果是则尝试刷新token
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			fs.Debugf(f, "🔐 收到HTTP 401错误，强制刷新token")
			// 强制刷新token，忽略时间检查
			refreshErr := f.refreshTokenIfNecessary(true, true)
			if refreshErr != nil {
				fs.Errorf(f, "刷新token失败: %v", refreshErr)
				return false, fmt.Errorf("身份验证失败: %w", refreshErr)
			}
			// 更新Authorization头
			opts.ExtraHeaders["Authorization"] = "Bearer " + f.token
			fs.Debugf(f, "✅ token已强制刷新，将重试API调用")
			return true, nil // 重试
		}

		// 检查响应体中的code字段是否为401（token过期）
		if err == nil && resp != nil && resp.StatusCode == 200 && respBody != nil {
			if apiResp, ok := respBody.(APIResponse); ok {
				if apiResp.GetCode() == 401 {
					fs.Debugf(f, "🔐 响应体中检测到401错误(token过期)，API消息: %s", apiResp.GetMessage())
					fs.Debugf(f, "🔄 当前token过期时间: %v", f.tokenExpiry)
					// 强制刷新token，忽略时间检查
					refreshErr := f.refreshTokenIfNecessary(true, true)
					if refreshErr != nil {
						fs.Errorf(f, "❌ token刷新失败: %v", refreshErr)
						return false, fmt.Errorf("身份验证失败: %w", refreshErr)
					}
					// 更新Authorization头
					opts.ExtraHeaders["Authorization"] = "Bearer " + f.token
					fs.Debugf(f, "✅ token已强制刷新，新过期时间: %v，将重试API调用", f.tokenExpiry)
					return true, nil // 重试
				}
			}
		}

		return shouldRetry(err)
	})

	if err != nil {
		return fmt.Errorf("API调用失败 [%s %s]: %w", method, endpoint, err)
	}

	return nil
}

// makeAPICallWithRestMultipartToDomain 使用rclone标准rest客户端进行multipart API调用到指定域名
func (f *Fs) makeAPICallWithRestMultipartToDomain(ctx context.Context, domain string, endpoint string, method string, body io.Reader, contentType string, respBody any) error {
	fs.Debugf(f, "📤 向域名发起multipart API调用: %s %s %s", method, domain, endpoint)

	// 确保令牌在进行API调用前有效
	err := f.refreshTokenIfNecessary(false, false)
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
				fs.Debugf(f, "🔄 API调用失败，将重试: %v", err)
				return true, err
			}
			return false, fmt.Errorf("API调用失败: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		// 检查响应状态
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)

			// 特殊处理401错误 - token过期
			if resp.StatusCode == http.StatusUnauthorized {
				fs.Debugf(f, "🔐 收到401错误，强制刷新token")
				// 强制刷新token，忽略时间检查
				refreshErr := f.refreshTokenIfNecessary(true, true)
				if refreshErr != nil {
					fs.Errorf(f, "❌ 刷新token失败: %v", refreshErr)
					return false, fmt.Errorf("身份验证失败: %w", refreshErr)
				}
				// 更新Authorization头
				opts.ExtraHeaders["Authorization"] = "Bearer " + f.token
				fs.Debugf(f, "✅ token已强制刷新，将重试API调用")
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
		// For safety, unknown endpoints default to strictest pacer
		fs.Debugf(f, "⚠️ 未知API端点，使用最严格限制: %s", endpoint)
		return f.strictPacer // Strictest limits
	}
}

// Use rclone standard hash verification instead

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

// getFileInfo 根据ID获取文件的详细信息
func (f *Fs) getFileInfo(ctx context.Context, fileID string) (*FileListInfoRespDataV2, error) {
	fs.Debugf(f, "📋 getFileInfo开始: fileID=%s", fileID)

	// 验证文件ID
	_, err := parseFileID(fileID)
	if err != nil {
		fs.Debugf(f, "❌ getFileInfo文件ID验证失败: %v", err)
		return nil, fmt.Errorf("获取文件信息: %w", err)
	}

	// 使用文件详情API - 已迁移到rclone标准方法
	var response FileDetailResponse
	endpoint := fmt.Sprintf("/api/v1/file/detail?fileID=%s", fileID)

	fs.Debugf(f, "🌐 getFileInfo调用API: %s", endpoint)
	err = f.makeAPICallWithRest(ctx, endpoint, "GET", nil, &response)
	if err != nil {
		fs.Debugf(f, "❌ getFileInfo API调用失败: %v", err)
		return nil, err
	}

	fs.Debugf(f, "📋 getFileInfo API响应: code=%d, message=%s", response.Code, response.Message)
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

	fs.Debugf(f, "✅ getFileInfo成功: fileID=%s, filename=%s, type=%d, size=%d", fileID, fileInfo.Filename, fileInfo.Type, fileInfo.Size)
	return fileInfo, nil
}

// getDownloadURL 获取文件的下载URL
func (f *Fs) getDownloadURL(ctx context.Context, fileID string) (string, error) {
	// 首先检查缓存
	if cachedURL, found := f.getCachedDownloadURL(fileID); found {
		return cachedURL, nil
	}

	// 缓存未命中，调用API获取
	fs.Debugf(f, "🔍 123网盘获取下载URL: fileID=%s", fileID)

	var response DownloadInfoResponse
	endpoint := fmt.Sprintf("/api/v1/file/download_info?fileID=%s", fileID)

	err := f.makeAPICallWithRest(ctx, endpoint, "GET", nil, &response)
	if err != nil {
		return "", err
	}

	if response.Code != 0 {
		return "", fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	downloadURL := response.Data.DownloadURL
	fs.Debugf(f, "⬇️ 123网盘下载URL获取成功: fileID=%s", fileID)

	// 缓存下载URL
	f.setCachedDownloadURL(fileID, downloadURL)

	return downloadURL, nil
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
	parentFileID, err := parseFileID(parentID)
	if err != nil {
		return "", fmt.Errorf("父目录ID无效: %w", err)
	}

	// 首先检查目录是否已存在，避免不必要的API调用
	fs.Debugf(f, "🔍 检查目录 '%s' 是否已存在于父目录 %s", name, parentID)
	existingID, found, err := f.FindLeaf(ctx, parentID, name)
	if err != nil {
		fs.Debugf(f, "⚠️ 检查现有目录时出错: %v，继续尝试创建", err)
	} else if found {
		fs.Debugf(f, "✅ 目录 '%s' 已存在，ID: %s", name, existingID)
		return existingID, nil
	}

	// 如果常规查找失败，尝试强制刷新查找（防止缓存问题）
	if !found && err == nil {
		fs.Debugf(f, "🔄 常规查找未找到目录 '%s'，尝试强制刷新查找", name)
		existingID, found, err = f.findLeafWithForceRefresh(ctx, parentID, name)
		if err != nil {
			fs.Debugf(f, "⚠️ 强制刷新查找出错: %v，继续尝试创建", err)
		} else if found {
			fs.Debugf(f, "✅ 强制刷新找到目录 '%s'，ID: %s", name, existingID)
			return existingID, nil
		}
	}

	// 目录不存在，准备创建
	fs.Debugf(f, "📁 目录 '%s' 不存在，开始创建", name)
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
	fs.Debugf(f, "✅ 目录 '%s' 创建成功，开始查找新目录ID", name)

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

	fs.Debugf(f, "✅ 成功创建目录 '%s'，ID: %s", name, newDirID)
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
		// Unified handling strategy: if pre-verification fails, try to get correct parent ID
		fs.Debugf(f, "Chunked upload: pre-verification found parent ID %d does not exist, trying to get correct parent ID", parentFileID)

		// Use unified parent ID repair strategy
		realParentFileID := f.getCorrectParentFileID(ctx, parentFileID)

		if realParentFileID != parentFileID {
			fs.Debugf(f, "Chunked upload: found correct parent ID: %d (original ID: %d), using correct ID to continue upload", realParentFileID, parentFileID)
			parentFileID = realParentFileID
		} else {
			fs.Debugf(f, "Chunked upload: using cached parent ID %d after cleanup to continue attempt", parentFileID)
		}
	} else {
		fs.Debugf(f, "Chunked upload: parent ID %d pre-verification passed", parentFileID)
	}

	reqBody := map[string]any{
		"parentFileID": parentFileID, // 使用官方API文档的正确参数名
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

	// Unified parent ID verification logic: detect parentFileID not exist error, use unified repair strategy
	if response.Code == 1 && strings.Contains(response.Message, "parentFileID不存在") {
		fs.Debugf(f, "Parent ID %d does not exist, cleaning cache", parentFileID)

		// Use unified parent ID repair strategy consistent with single-step upload
		fs.Debugf(f, "Using unified parent ID repair strategy")
		correctParentID := f.getCorrectParentFileID(ctx, parentFileID)

		// If got different parent ID, retry creating upload session
		if correctParentID != parentFileID {
			fs.Debugf(f, "Found correct parent ID: %d (original ID: %d), retrying upload session creation", correctParentID, parentFileID)

			// Rebuild request with correct directory ID
			reqBody["parentFileID"] = correctParentID

			// Retry API call
			err = f.makeAPICallWithRest(ctx, "/upload/v2/file/create", "POST", reqBody, &response)
			if err != nil {
				return nil, fmt.Errorf("retry API call with correct directory ID failed: %w", err)
			}
		} else {
			// Even if same ID, retry because cache has been cleaned
			fs.Debugf(f, "Using cached parent ID %d after cleanup, retrying upload session creation", correctParentID)
			reqBody["parentFileID"] = correctParentID
			err = f.makeAPICallWithRest(ctx, "/upload/v2/file/create", "POST", reqBody, &response)
			if err != nil {
				return nil, fmt.Errorf("retry API call after cache cleanup failed: %w", err)
			}
		}
	}

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// Debug response data
	fs.Debugf(f, "Upload session created successfully: FileID=%d, PreuploadID='%s', Reuse=%v, SliceSize=%d",
		response.Data.FileID, response.Data.PreuploadID, response.Data.Reuse, response.Data.SliceSize)

	return &response, nil
}

// uploadFile 使用上传会话上传文件内容
func (f *Fs) uploadFile(ctx context.Context, in io.Reader, createResp *UploadCreateResp, size int64) error {
	return f.uploadFileWithPath(ctx, in, createResp, size)
}

// uploadFileWithPath 使用上传会话上传文件内容，支持断点续传
func (f *Fs) uploadFileWithPath(ctx context.Context, in io.Reader, createResp *UploadCreateResp, size int64) error {
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

	fs.Debugf(f, "📤 开始分片上传: %d个分片，每片%s", uploadNums, fs.SizeSuffix(chunkSize))

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

		// 减少进度日志频率：只在每10个分片或最后一个分片时输出
		if partNumber%10 == 0 || partNumber == uploadNums {
			fs.Infof(f, "📤 上传进度: %d/%d (%.1f%%)", partNumber, uploadNums, float64(partNumber)/float64(uploadNums)*100)
		} else {
			fs.Debugf(f, "已上传分片 %d/%d", partNumber, uploadNums)
		}
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

	fs.Debugf(f, "🔧 开始上传分片 %d，预上传ID: %s, 分片MD5: %s, 数据大小: %d 字节",
		partNumber, preuploadID, chunkHash, len(data))

	// 简化重试逻辑 - 使用rclone标准的fs.Pacer重试机制
	return f.uploadPacer.Call(func() (bool, error) {
		err := f.uploadPartDirectly(ctx, preuploadID, partNumber, chunkHash, data)
		return fserrors.ShouldRetry(err), err
	})
}

// uploadPartDirectly 直接上传分片，不包含重试逻辑
// 关键修复：将重试逻辑从业务逻辑中分离，修复multipart数据在重试时丢失的问题
func (f *Fs) uploadPartDirectly(ctx context.Context, preuploadID string, partNumber int64, chunkHash string, data []byte) error {
	// 获取上传域名
	uploadDomain := f.getUploadDomain(ctx)

	// 使用上传域名进行multipart上传
	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	// 修复卡住问题：为分片上传添加超时控制
	uploadCtx, cancel := context.WithTimeout(ctx, 5*time.Minute) // 每个分片最多5分钟
	defer cancel()

	// 根据端点获取适当的调速器
	pacer := f.getPacerForEndpoint("/upload/v2/file/slice")

	// 修复关键问题：在pacer重试循环内重新创建multipart数据
	return pacer.Call(func() (bool, error) {
		// 每次重试都重新创建multipart表单数据，避免body被消耗的问题
		var buf bytes.Buffer
		writer := multipart.NewWriter(&buf)

		// 添加表单字段
		_ = writer.WriteField("preuploadID", preuploadID)
		_ = writer.WriteField("sliceNo", fmt.Sprintf("%d", partNumber))
		_ = writer.WriteField("sliceMD5", chunkHash)

		// 添加文件数据
		part, err := writer.CreateFormFile("slice", fmt.Sprintf("chunk_%d", partNumber))
		if err != nil {
			return false, fmt.Errorf("创建表单文件字段失败: %w", err)
		}

		_, err = part.Write(data)
		if err != nil {
			return false, fmt.Errorf("写入分片数据失败: %w", err)
		}

		err = writer.Close()
		if err != nil {
			return false, fmt.Errorf("关闭multipart writer失败: %w", err)
		}

		// 直接使用rest客户端，避免嵌套的pacer调用
		opts := rest.Opts{
			Method:  "POST",
			RootURL: uploadDomain,
			Path:    "/upload/v2/file/slice",
			Body:    &buf,
			ExtraHeaders: map[string]string{
				"Content-Type":  writer.FormDataContentType(),
				"Authorization": "Bearer " + f.token,
				"Platform":      "open_platform",
				"User-Agent":    f.opt.UserAgent,
			},
		}

		resp, err := f.rst.Call(uploadCtx, &opts)
		if err != nil {
			// 检查是否需要重试
			if fserrors.ShouldRetry(err) {
				fs.Debugf(f, "🔄 分片 %d 上传失败，将重试: %v", partNumber, err)
				return true, err
			}
			return false, fmt.Errorf("上传分片数据失败: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		// 检查响应状态
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			return false, fmt.Errorf("分片上传HTTP错误 %d: %s", resp.StatusCode, string(body))
		}

		// 解析响应
		err = json.NewDecoder(resp.Body).Decode(&response)
		if err != nil {
			return false, fmt.Errorf("解析分片上传响应失败: %w", err)
		}

		if response.Code != 0 {
			return false, fmt.Errorf("分片上传API错误 %d: %s", response.Code, response.Message)
		}

		fs.Debugf(f, "✅ 分片 %d 上传成功", partNumber)
		return false, nil
	})
}

// uploadPartWithRetry 带增强重试机制的分片上传
func (f *Fs) uploadPartWithRetry(ctx context.Context, preuploadID string, partNumber int64, chunkHash string, data []byte) error {
	maxRetries := 3
	baseDelay := 1 * time.Second

	fs.Debugf(f, "🚀 开始上传分片 %d，大小: %d bytes，MD5: %s", partNumber, len(data), chunkHash)

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// 指数退避延迟
			delay := time.Duration(attempt) * baseDelay
			fs.Debugf(f, "🔄 分片 %d 重试 %d/%d，等待 %v", partNumber, attempt, maxRetries-1, delay)
			time.Sleep(delay)
		}

		// 记录每次尝试的开始时间
		attemptStart := time.Now()
		fs.Debugf(f, "📤 分片 %d 尝试 %d/%d 开始", partNumber, attempt+1, maxRetries)

		err := f.uploadPartWithMultipart(ctx, preuploadID, partNumber, chunkHash, data)
		if err == nil {
			duration := time.Since(attemptStart)
			// 只在关键分片记录详细信息
			if partNumber%10 == 0 || partNumber == 1 {
				fs.Debugf(f, "✅ 分片 %d 上传成功，耗时: %v，速度: %s/s",
					partNumber, duration, fs.SizeSuffix(int64(float64(len(data))/duration.Seconds())))
			}
			return nil
		}

		// 详细记录错误信息
		duration := time.Since(attemptStart)
		fs.Debugf(f, "❌ 分片 %d 尝试 %d/%d 失败，耗时: %v，错误: %v",
			partNumber, attempt+1, maxRetries, duration, err)

		// 检查是否应该重试
		if !fserrors.ShouldRetry(err) {
			fs.Errorf(f, "❌ 分片 %d 上传失败，不可重试的错误: %v", partNumber, err)
			return fmt.Errorf("分片 %d 上传失败，不可重试: %w", partNumber, err)
		}

		// 检查上下文是否被取消
		if ctx.Err() != nil {
			fs.Errorf(f, "❌ 分片 %d 上传被取消: %v", partNumber, ctx.Err())
			return fmt.Errorf("分片 %d 上传被取消: %w", partNumber, ctx.Err())
		}

		fs.Debugf(f, "⚠️ 分片 %d 上传失败，将重试: %v", partNumber, err)
	}

	fs.Errorf(f, "❌ 分片 %d 上传最终失败，已重试 %d 次", partNumber, maxRetries)
	return fmt.Errorf("分片 %d 上传失败，已重试 %d 次", partNumber, maxRetries)
}

// UploadCompleteResult 上传完成结果
type UploadCompleteResult struct {
	FileID int64  `json:"fileID"`
	Etag   string `json:"etag"`
}

// completeUploadWithResultAndSize simplified upload completion with standard retry logic
func (f *Fs) completeUploadWithResultAndSize(ctx context.Context, preuploadID string, fileSize int64) (*UploadCompleteResult, error) {
	fs.Debugf(f, "🔍 完成上传验证 (文件大小: %s)", fs.SizeSuffix(fileSize))

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

	// 优化的轮询策略：根据文件大小调整等待时间
	var maxRetries int
	var interval time.Duration

	if fileSize <= 100*1024*1024 { // 100MB以下
		maxRetries = 120 // 4分钟
		interval = 2 * time.Second
	} else if fileSize <= 1024*1024*1024 { // 1GB以下
		maxRetries = 300 // 10分钟
		interval = 2 * time.Second
	} else { // 1GB以上
		maxRetries = 600 // 20分钟
		interval = 2 * time.Second
	}

	fs.Debugf(f, "🔄 开始轮询上传完成状态，最大等待时间: %v", time.Duration(maxRetries)*interval)

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("upload verification cancelled: %w", ctx.Err())
			case <-time.After(interval):
			}
		}

		err := f.makeAPICallWithRest(ctx, "/upload/v2/file/upload_complete", "POST", reqBody, &response)
		if err != nil {
			fs.Debugf(f, "❌ 上传完成API调用失败 (尝试 %d/%d): %v", attempt+1, maxRetries, err)

			// Use standard retry logic
			if retry, _ := shouldRetry(err); retry {
				continue
			}
			return nil, fmt.Errorf("upload verification failed: %w", err)
		}

		if response.Code == 0 && response.Data.Completed {
			fs.Infof(f, "✅ 上传验证成功完成 (耗时: %v)", time.Duration(attempt)*interval)
			return &UploadCompleteResult{
				FileID: response.Data.FileID,
				Etag:   response.Data.Etag,
			}, nil
		}

		if response.Code == 0 && !response.Data.Completed {
			// 每30秒显示一次进度信息
			if attempt%15 == 0 && attempt > 0 {
				elapsed := time.Duration(attempt) * interval
				fs.Infof(f, "⏳ 123网盘服务器正在处理文件，已等待: %v", elapsed)
			} else {
				fs.Debugf(f, "🔄 服务器返回completed=false，继续轮询 (尝试 %d/%d)", attempt+1, maxRetries)
			}
			continue
		}

		// Handle API errors
		if response.Code != 0 {
			// 特殊处理"文件正在校验中"错误 - 这是正常的等待状态
			if response.Code == 20103 || strings.Contains(response.Message, "文件正在校验中") {
				// 每30秒显示一次进度信息
				if attempt%15 == 0 && attempt > 0 {
					elapsed := time.Duration(attempt) * interval
					fs.Infof(f, "⏳ 123网盘服务器正在校验文件，已等待: %v", elapsed)
				} else {
					fs.Debugf(f, "⏳ 文件正在校验中，继续等待 (尝试 %d/%d): %s", attempt+1, maxRetries, response.Message)
				}
				continue
			}

			// 其他错误直接返回
			fs.Debugf(f, "❌ 上传验证失败: API错误 %d: %s", response.Code, response.Message)
			return nil, fmt.Errorf("upload verification failed: API error %d: %s", response.Code, response.Message)
		}
	}

	return nil, fmt.Errorf("upload verification timeout after %d attempts", maxRetries)
}

// findPathSafe 安全的FindPath调用，自动处理文件路径vs目录路径
func (f *Fs) findPathSafe(ctx context.Context, remote string, create bool) (leaf, directoryID string, err error) {
	fs.Debugf(f, "🔧 findPathSafe: 处理路径 '%s', create=%v", remote, create)

	// 🔧 移除启发式文件检测，使用标准rclone流程

	// 使用标准的FindPath处理
	fs.Debugf(f, "🔧 findPathSafe: 使用标准FindPath处理路径 '%s'", remote)
	return f.dirCache.FindPath(ctx, remote, create)
}

// streamingPut 简化的上传入口，支持两种主要路径：小文件直接上传和大文件分片上传
func (f *Fs) streamingPut(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "📤 开始上传: %s", fileName)

	// 简化的跨云传输检测：只检测是否为远程源
	isRemoteSource := f.isRemoteSource(src)
	if isRemoteSource {
		fs.Debugf(f, "☁️ 检测到跨云传输: %s → 123Pan (大小: %s)", src.Fs().Name(), fs.SizeSuffix(src.Size()))
		return f.handleCrossCloudTransfer(ctx, in, src, parentFileID, fileName)
	}

	// 根据文件大小选择上传策略：根据123网盘API规范
	// ≤ 1GB: 使用单步上传API
	// > 1GB: 使用分片上传API
	fileSize := src.Size()
	if fileSize <= singleStepUploadLimit { // 1GB threshold per API spec
		fs.Debugf(f, "📋 统一上传策略选择: 单步上传 - File ≤ 1GB (%s), using single-step upload API", fs.SizeSuffix(fileSize))
		return f.directSmallFileUpload(ctx, in, src, parentFileID, fileName)
	}
	fs.Debugf(f, "📋 统一上传策略选择: 分片上传 - File > 1GB (%s), using chunked upload API", fs.SizeSuffix(fileSize))
	return f.directLargeFileUpload(ctx, in, src, parentFileID, fileName)
}

// directSmallFileUpload 直接上传小文件（≤1GB），使用单步上传API
func (f *Fs) directSmallFileUpload(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()

	// 对于较小的文件（≤100MB），直接读取到内存
	if fileSize <= 100*1024*1024 {
		// 读取所有数据到内存
		data, err := io.ReadAll(in)
		if err != nil {
			return nil, fmt.Errorf("failed to read file data: %w", err)
		}

		// Calculate MD5
		hasher := md5.New()
		hasher.Write(data)
		md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))

		fs.Debugf(f, "📝 小文件加载到内存, 大小: %d, MD5: %s", len(data), md5Hash)
		return f.singleStepUpload(ctx, data, parentFileID, fileName, md5Hash)
	}

	// 对于较大的文件（100MB-1GB），使用临时文件避免内存压力
	fs.Debugf(f, "📁 大型单步文件 (%s)，使用临时文件进行单步上传", fs.SizeSuffix(fileSize))
	return f.uploadLargeFileWithTempFileForSingleStep(ctx, in, src, parentFileID, fileName)
}

// directLargeFileUpload 直接分片上传大文件（>1GB），使用分片上传API
func (f *Fs) directLargeFileUpload(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()

	// For very large files, use chunked streaming
	if fileSize > singleStepUploadLimit {
		fs.Debugf(f, "📦 超大文件 (%s)，使用分片流式传输", fs.SizeSuffix(fileSize))

		// Validate source object for chunked upload
		srcObj, ok := src.(fs.Object)
		if !ok {
			fs.Debugf(f, "⚠️ 无法获取源对象，回退到临时文件策略")
			return f.uploadLargeFileWithTempFile(ctx, in, src, parentFileID, fileName)
		}

		return f.uploadFileInChunksSimplified(ctx, srcObj, parentFileID, fileName)
	}

	// For moderately large files, use temp file strategy
	fs.Debugf(f, "📁 大文件 (%s)，使用临时文件策略", fs.SizeSuffix(fileSize))
	return f.uploadLargeFileWithTempFile(ctx, in, src, parentFileID, fileName)
}

// uploadLargeFileWithTempFile 使用临时文件处理大文件上传
func (f *Fs) uploadLargeFileWithTempFile(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "📁 大文件 (%s)，使用临时文件策略", fs.SizeSuffix(src.Size()))

	// Create temp file with safe resource management
	tempFile, err := os.CreateTemp("", "rclone-123-upload-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	fs.Debugf(f, "💾 创建临时文件: %s | 原因: 大文件临时缓存 | 预计大小: %s", tempFile.Name(), fs.SizeSuffix(src.Size()))
	defer func() {
		// Safe resource cleanup
		if tempFile != nil {
			if closeErr := tempFile.Close(); closeErr != nil {
				fs.Debugf(f, "⚠️ 关闭临时文件失败: %v", closeErr)
			} else {
				fs.Debugf(f, "📁 关闭临时文件成功: %s", tempFile.Name())
			}
			if removeErr := os.Remove(tempFile.Name()); removeErr != nil {
				fs.Debugf(f, "⚠️ 删除临时文件失败: %v", removeErr)
			} else {
				fs.Debugf(f, "🧹 删除临时文件成功: %s", tempFile.Name())
			}
		}
	}()

	// Write to temp file while calculating MD5
	fs.Debugf(f, "✍️ 开始写入临时文件: %s", tempFile.Name())
	hasher := md5.New()
	multiWriter := io.MultiWriter(tempFile, hasher)

	written, err := io.Copy(multiWriter, in)
	if err != nil {
		return nil, fmt.Errorf("failed to write to temp file: %w", err)
	}

	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(f, "✅ 临时文件缓冲完成, 写入: %d bytes, MD5: %s", written, md5Hash)

	// Seek back to beginning
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("failed to seek temp file: %w", err)
	}

	// Upload with known MD5
	return f.putWithKnownMD5(ctx, tempFile, src, parentFileID, fileName, md5Hash)
}

// uploadLargeFileWithTempFileForSingleStep 使用临时文件处理大文件的单步上传（100MB-1GB）
func (f *Fs) uploadLargeFileWithTempFileForSingleStep(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "Large single-step file (%s), using temp file for single-step upload", fs.SizeSuffix(src.Size()))

	// Create temp file with safe resource management
	tempFile, err := os.CreateTemp("", "rclone-123-singlestep-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	defer func() {
		// Safe resource cleanup
		if tempFile != nil {
			if closeErr := tempFile.Close(); closeErr != nil {
				fs.Debugf(f, "Failed to close temp file: %v", closeErr)
			}
			if removeErr := os.Remove(tempFile.Name()); removeErr != nil {
				fs.Debugf(f, "Failed to remove temp file: %v", removeErr)
			}
		}
	}()

	// Write to temp file while calculating MD5
	hasher := md5.New()
	multiWriter := io.MultiWriter(tempFile, hasher)

	written, err := io.Copy(multiWriter, in)
	if err != nil {
		return nil, fmt.Errorf("failed to write to temp file: %w", err)
	}

	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(f, "Temp file buffering complete for single-step upload, written: %d bytes, MD5: %s", written, md5Hash)

	// Seek back to beginning
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("failed to seek temp file: %w", err)
	}

	// Read all data for single-step upload
	data, err := io.ReadAll(tempFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read temp file data: %w", err)
	}

	// Use single-step upload API
	return f.singleStepUpload(ctx, data, parentFileID, fileName, md5Hash)
}

// uploadFileInChunksSimplified 简化的分片上传处理超大文件
func (f *Fs) uploadFileInChunksSimplified(ctx context.Context, srcObj fs.Object, parentFileID int64, fileName string) (*Object, error) {
	fileSize := srcObj.Size()
	fs.Infof(f, "📦 初始化大文件上传 (%s) - 准备分片上传参数...", fs.SizeSuffix(fileSize))

	// 计算文件MD5哈希 - 123网盘API要求提供etag
	fs.Debugf(f, "🔐 为大文件上传计算MD5哈希...")
	md5Hash, err := srcObj.Hash(ctx, fshash.MD5)
	if err != nil {
		return nil, fmt.Errorf("failed to compute MD5 hash: %w", err)
	}
	if md5Hash == "" {
		return nil, fmt.Errorf("MD5 hash is required for 123 API but could not be computed")
	}
	fs.Debugf(f, "✅ MD5哈希计算完成: %s", md5Hash)

	// Create upload session with MD5 hash
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, fileSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create upload session: %w", err)
	}

	// 严格按照123网盘API返回的分片大小进行分片
	// 根据OpenAPI文档，sliceSize是服务器决定的，客户端不能自定义
	apiSliceSize := createResp.Data.SliceSize
	if apiSliceSize <= 0 {
		// 如果API没有返回分片大小，使用默认值
		apiSliceSize = int64(defaultChunkSize)
		fs.Debugf(f, "⚠️ API未返回分片大小，使用默认值: %s", fs.SizeSuffix(apiSliceSize))
	} else {
		fs.Debugf(f, "📋 使用123网盘API指定的分片大小: %s for file size: %s",
			fs.SizeSuffix(apiSliceSize), fs.SizeSuffix(fileSize))
	}

	// If instant upload triggered, return success
	if createResp.Data.Reuse {
		fs.Debugf(f, "🚀 分片上传过程中触发秒传")
		return f.createObject(fileName, createResp.Data.FileID, fileSize, "", time.Now()), nil
	}

	// Start chunked streaming upload using optimal chunk size
	return f.uploadFileInChunks(ctx, srcObj, createResp, fileSize, apiSliceSize, fileName)
}

// handleCrossCloudTransfer 统一的跨云传输处理函数
// 支持传统模式和流式哈希模式
func (f *Fs) handleCrossCloudTransfer(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()

	// 检查是否启用流式哈希模式
	if f.opt.StreamHashMode {
		fs.Infof(f, "🌊 启用流式哈希模式: %s (%s)", fileName, fs.SizeSuffix(fileSize))

		// 🔧 类型安全检查：streamHashTransferWithReader 需要 fs.Object 类型
		srcObj, ok := src.(fs.Object)
		if !ok {
			fs.Infof(f, "⚠️ 源对象不支持流式传输，使用两步传输: %s", fileName)
			return f.internalTwoStepTransfer(ctx, in, src, parentFileID, fileName)
		}

		// 🔧 关键修复：使用传递的Reader（已被rclone Transfer包装），确保进度正确显示
		return f.streamHashTransferWithReader(ctx, in, srcObj, parentFileID, fileName)
	}
	fs.Infof(f, "🔄 开始两步传输: %s (%s)", fileName, fs.SizeSuffix(fileSize))
	fs.Infof(f, "📥 步骤1: 下载到本地 → 📤 步骤2: 上传到123网盘")
	return f.internalTwoStepTransfer(ctx, in, src, parentFileID, fileName)
}

// internalTwoStepTransfer 内部两步传输：下载到本地临时文件，然后上传到123网盘
// 这是处理跨云传输最可靠的方法，避免了流式传输的复杂性
func (f *Fs) internalTwoStepTransfer(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	transferStartTime := time.Now()
	fileSize := src.Size()
	fs.Infof(f, "🚀 开始两步传输: %s (%s)", fileName, fs.SizeSuffix(fileSize))

	// Step 1: 下载到临时文件
	fs.Infof(f, "📥 步骤1: 正在下载...")

	// 创建临时文件
	tempFile, err := os.CreateTemp("", "rclone-123-twostep-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	fs.Debugf(f, "💾 创建临时文件: %s | 原因: 两步传输Step1下载缓存 | 目标大小: %s", tempFile.Name(), fs.SizeSuffix(fileSize))
	defer func() {
		if tempFile != nil {
			if closeErr := tempFile.Close(); closeErr != nil {
				fs.Debugf(f, "⚠️ 关闭临时文件失败: %v", closeErr)
			} else {
				fs.Debugf(f, "📁 关闭临时文件成功: %s", tempFile.Name())
			}
			if removeErr := os.Remove(tempFile.Name()); removeErr != nil {
				fs.Debugf(f, "⚠️ 删除临时文件失败: %v", removeErr)
			} else {
				fs.Debugf(f, "🧹 删除临时文件成功: %s", tempFile.Name())
			}
		}
	}()
	fs.Debugf(f, "✍️ 开始写入临时文件: %s", tempFile.Name())

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
	defer func() { _ = srcReader.Close() }()

	// 下载到临时文件，同时计算MD5，并显示下载进度
	hasher := md5.New()
	multiWriter := io.MultiWriter(tempFile, hasher)

	// 🔧 修复进度显示问题：直接使用原始Reader，避免重复进度跟踪
	// 不使用TwoStepProgressReader，让rclone的accounting系统处理进度
	// 内部两步传输的进度由主传输进度显示，不需要额外的进度跟踪
	fs.Debugf(f, "📥 Step 1: 开始下载到临时文件")

	written, err := io.Copy(multiWriter, srcReader)
	if err != nil {
		return nil, fmt.Errorf("下载到临时文件失败: %w", err)
	}

	if written != fileSize {
		return nil, fmt.Errorf("下载大小不匹配: 期望 %d, 实际 %d", fileSize, written)
	}

	// 计算MD5
	md5sum := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Infof(f, "✅ 步骤1完成: 已下载 %s", fs.SizeSuffix(written))

	// Step 2: 从临时文件上传到123网盘
	fs.Infof(f, "📤 步骤2: 正在上传...")

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
	result, err := f.putWithKnownMD5(ctx, tempFile, localSrc, parentFileID, fileName, md5sum)
	if err != nil {
		return nil, err
	}

	// 显示两步传输总体统计
	totalDuration := time.Since(transferStartTime)
	avgSpeed := float64(fileSize) / totalDuration.Seconds()
	fs.Infof(f, "🎉 两步传输完成: %s | ⏱️ 总用时 %v | 🚀 平均 %s/s",
		fs.SizeSuffix(fileSize), totalDuration.Truncate(time.Second), fs.SizeSuffix(int64(avgSpeed)))

	return result, nil
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

// Removed unused function: unifiedUploadWithMD5

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
		fs.Debugf(f, "📏 文件大小 %d bytes < %d，尝试使用单步上传API", fileSize, singleStepUploadLimit)

		// 读取文件数据到内存
		data, err := io.ReadAll(in)
		if err != nil {
			fs.Debugf(f, "❌ 读取文件数据失败，回退到传统上传: %v", err)
		} else if int64(len(data)) == fileSize {
			// 尝试单步上传
			result, err := f.singleStepUpload(ctx, data, parentFileID, fileName, md5Hash)
			if err != nil {
				fs.Debugf(f, "❌ 单步上传失败，回退到传统上传: %v", err)
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
		fs.Debugf(f, "🚀 文件秒传成功，MD5: %s", md5Hash)
		return f.createObject(fileName, createResp.Data.FileID, src.Size(), md5Hash, time.Now()), nil
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
	return f.createObject(fileName, createResp.Data.FileID, src.Size(), md5Hash, time.Now()), nil
}

// uploadFileInChunks 简化的分片流式上传，使用rclone标准重试机制
func (f *Fs) uploadFileInChunks(ctx context.Context, srcObj fs.Object, createResp *UploadCreateResp, fileSize, chunkSize int64, fileName string) (*Object, error) {
	totalChunks := (fileSize + chunkSize - 1) / chunkSize

	fs.Debugf(f, "🌊 开始分片流式上传: %d个分片", totalChunks)

	// 使用单线程上传，简化逻辑
	return f.uploadChunksSingleThreaded(ctx, srcObj, createResp, fileSize, chunkSize, fileName)
}

// uploadChunksSingleThreaded 简化的单线程分片上传
func (f *Fs) uploadChunksSingleThreaded(ctx context.Context, srcObj fs.Object, createResp *UploadCreateResp, fileSize, chunkSize int64, fileName string) (*Object, error) {
	preuploadID := createResp.Data.PreuploadID
	totalChunks := (fileSize + chunkSize - 1) / chunkSize
	overallHasher := md5.New()

	fs.Infof(f, "🌊 开始单线程分片上传: 文件=%s, 总大小=%s, 分片数=%d, 分片大小=%s",
		fileName, fs.SizeSuffix(fileSize), totalChunks, fs.SizeSuffix(chunkSize))

	uploadStart := time.Now()
	var lastProgressTime time.Time
	var lastProgressPercent int

	// 增强的分片上传循环，带详细错误处理和进度跟踪
	for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
		partNumber := chunkIndex + 1

		// 检查上下文是否被取消
		if ctx.Err() != nil {
			fs.Errorf(f, "❌ 分片上传被取消: %v", ctx.Err())
			return nil, fmt.Errorf("分片上传被取消: %w", ctx.Err())
		}

		chunkStart := time.Now()
		fs.Debugf(f, "📤 开始上传分片 %d/%d", partNumber, totalChunks)

		// 上传分片
		if err := f.uploadSingleChunkWithStream(ctx, srcObj, overallHasher, preuploadID, chunkIndex, chunkSize, fileSize, totalChunks); err != nil {
			fs.Errorf(f, "❌ 分片 %d/%d 上传失败: %v", partNumber, totalChunks, err)
			return nil, fmt.Errorf("上传分片 %d/%d 失败: %w", partNumber, totalChunks, err)
		}

		chunkDuration := time.Since(chunkStart)
		uploadedBytes := (chunkIndex + 1) * chunkSize
		if uploadedBytes > fileSize {
			uploadedBytes = fileSize
		}

		progress := float64(uploadedBytes) / float64(fileSize) * 100
		progressPercent := int(progress)
		now := time.Now()

		// 智能进度显示：每完成一个分片或进度变化超过10%时显示
		shouldShowProgress := (progressPercent >= lastProgressPercent+10) ||
			(now.Sub(lastProgressTime) > 10*time.Second) ||
			(chunkIndex == totalChunks-1) // 最后一个分片

		if shouldShowProgress {
			elapsed := now.Sub(uploadStart)
			var avgSpeed float64
			if elapsed.Seconds() > 0 {
				avgSpeed = float64(uploadedBytes) / elapsed.Seconds() / 1024 / 1024 // MB/s
			}

			fs.Infof(f, "📊 123分片上传进度: %d/%d分片 (%d%%) %s/%s 速度: %.2f MB/s [%s]",
				partNumber, totalChunks, progressPercent,
				fs.SizeSuffix(uploadedBytes), fs.SizeSuffix(fileSize),
				avgSpeed, fileName)
			lastProgressTime = now
			lastProgressPercent = progressPercent
		}

		fs.Debugf(f, "✅ 分片 %d/%d 上传完成，耗时: %v",
			partNumber, totalChunks, chunkDuration)
	}

	totalUploadTime := time.Since(uploadStart)
	avgSpeed := float64(fileSize) / totalUploadTime.Seconds()
	fs.Debugf(f, "🎯 所有分片上传完成，总耗时: %v，平均速度: %s/s",
		totalUploadTime, fs.SizeSuffix(int64(avgSpeed)))

	// 完成上传并返回结果
	return f.finalizeChunkedUpload(ctx, overallHasher, fileSize, fileName, preuploadID)
}

// finalizeChunkedUpload 完成分片上传并返回结果
func (f *Fs) finalizeChunkedUpload(ctx context.Context, hasher hash.Hash, fileSize int64, fileName, preuploadID string) (*Object, error) {
	// 计算最终MD5
	finalMD5 := fmt.Sprintf("%x", hasher.Sum(nil))

	// 完成上传并获取结果，传入文件大小以优化轮询策略
	result, err := f.completeUploadWithResultAndSize(ctx, preuploadID, fileSize)
	if err != nil {
		return nil, fmt.Errorf("完成分片上传失败: %w", err)
	}

	fs.Debugf(f, "✅ 分片流式上传完成，计算MD5: %s, 服务器MD5: %s", finalMD5, result.Etag)

	// 验证文件ID
	if result.FileID == 0 {
		return nil, fmt.Errorf("上传完成但未获取到有效的文件ID")
	}

	// 返回成功的文件对象，使用服务器返回的文件ID
	return f.createObject(fileName, result.FileID, fileSize, finalMD5, time.Now()), nil
}

// uploadSingleChunkWithStream 使用流式方式上传单个分片
// 这是分片流式传输的核心：边下载边上传，只需要分片大小的临时存储
func (f *Fs) uploadSingleChunkWithStream(ctx context.Context, srcObj fs.Object, overallHasher io.Writer, preuploadID string, chunkIndex, chunkSize, fileSize, totalChunks int64) error {
	partNumber := chunkIndex + 1
	chunkStart := chunkIndex * chunkSize
	chunkEnd := chunkStart + chunkSize
	chunkEnd = min(chunkEnd, fileSize)
	actualChunkSize := chunkEnd - chunkStart

	fs.Debugf(f, "🌊 开始流式上传分片 %d/%d (偏移: %d, 大小: %d)",
		partNumber, totalChunks, chunkStart, actualChunkSize)

	// 打开分片范围的源文件流
	chunkReader, err := srcObj.Open(ctx, &fs.RangeOption{Start: chunkStart, End: chunkEnd - 1})
	if err != nil {
		return fmt.Errorf("打开分片 %d 源文件流失败: %w", partNumber, err)
	}
	defer func() { _ = chunkReader.Close() }()

	// 创建临时文件用于此分片（只需要分片大小的存储）
	tempFile, err := os.CreateTemp("", fmt.Sprintf("rclone-123-chunk-%d-*.tmp", partNumber))
	if err != nil {
		return fmt.Errorf("创建分片 %d 临时文件失败: %w", partNumber, err)
	}
	fs.Debugf(f, "💾 创建分片临时文件: %s | 分片: %d/%d | 大小: %s", tempFile.Name(), partNumber, totalChunks, fs.SizeSuffix(actualChunkSize))
	defer func() {
		if closeErr := tempFile.Close(); closeErr != nil {
			fs.Debugf(f, "⚠️ 关闭分片临时文件失败: %v", closeErr)
		} else {
			fs.Debugf(f, "📁 关闭分片临时文件成功: %s", tempFile.Name())
		}
		if removeErr := os.Remove(tempFile.Name()); removeErr != nil {
			fs.Debugf(f, "⚠️ 删除分片临时文件失败: %v", removeErr)
		} else {
			fs.Debugf(f, "🧹 删除分片临时文件成功: %s", tempFile.Name())
		}
	}()

	// 创建分片MD5计算器
	chunkHasher := md5.New()

	// 边读边写：同时写入临时文件、分片MD5和整体MD5
	multiWriter := io.MultiWriter(tempFile, chunkHasher, overallHasher)

	// 流式传输分片数据
	fs.Debugf(f, "📤 开始流式传输分片 %d 到临时文件: %s", partNumber, tempFile.Name())
	written, err := io.Copy(multiWriter, chunkReader)
	if err != nil {
		return fmt.Errorf("流式传输分片 %d 失败: %w", partNumber, err)
	}
	fs.Debugf(f, "✅ 分片 %d 写入临时文件完成: %s | 写入字节: %s", partNumber, tempFile.Name(), fs.SizeSuffix(written))

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

	// 使用增强重试的multipart上传方法
	err = f.uploadPartWithRetry(ctx, preuploadID, partNumber, chunkMD5, chunkData)
	if err != nil {
		return fmt.Errorf("上传分片 %d 数据失败: %w", partNumber, err)
	}
	fs.Debugf(f, "✅ 分片 %d 流式上传成功，大小: %d, MD5: %s", partNumber, written, chunkMD5)

	return nil
}

// singleStepUpload 使用123云盘的单步上传API上传小文件（<1GB）
// 这个API专门为小文件设计，一次HTTP请求即可完成上传，效率更高
func (f *Fs) singleStepUpload(ctx context.Context, data []byte, parentFileID int64, fileName, md5Hash string) (*Object, error) {
	startTime := time.Now()
	fs.Debugf(f, "📤 使用单步上传API，文件: %s, 大小: %d bytes, MD5: %s", fileName, len(data), md5Hash)

	// 优化：先检查秒传，避免不必要的数据传输
	fs.Debugf(f, "🔍 单步上传: 优先检查秒传...")
	createResp, isInstant, err := f.checkInstantUpload(ctx, parentFileID, fileName, md5Hash, int64(len(data)))
	if err != nil {
		fs.Debugf(f, "❌ 秒传检查失败，继续单步上传: %v", err)
	} else if isInstant {
		duration := time.Since(startTime)
		fs.Debugf(f, "🚀 单步上传: 秒传成功! FileID: %d, 耗时: %v, 速度: 瞬时", createResp.Data.FileID, duration)
		return f.createObjectFromUpload(fileName, int64(len(data)), md5Hash, createResp.Data.FileID, time.Now()), nil
	}

	fs.Debugf(f, "⚠️ 秒传失败，开始实际单步上传...")

	// 统一父目录ID验证：使用统一的验证逻辑
	fs.Debugf(f, " 单步上传: 统一验证父目录ID %d", parentFileID)
	exists, err := f.verifyParentFileID(ctx, parentFileID)
	if err != nil {
		return nil, fmt.Errorf("验证父目录ID %d 失败: %w", parentFileID, err)
	}
	if !exists {
		fs.Debugf(f, "⚠️ 单步上传: 预验证发现父目录ID %d 不存在，但继续尝试 (可能的API差异)", parentFileID)
		// Don't return error immediately, let API call make final decision as different APIs may have different validation logic
	} else {
		fs.Debugf(f, "✅ 单步上传: 父目录ID %d 预验证通过", parentFileID)
	}

	// 验证文件大小限制
	if len(data) > singleStepUploadLimit {
		return nil, fmt.Errorf("文件大小超过单步上传限制%d bytes: %d bytes", singleStepUploadLimit, len(data))
	}

	// 使用统一的文件名验证和清理
	cleanedFileName, warning := f.validateAndCleanFileNameUnified(fileName)
	if warning != nil {
		fs.Logf(f, "⚠️ 文件名验证警告: %v", warning)
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

	// 获取上传域名
	uploadDomain := f.getUploadDomain(ctx)

	// 创建multipart表单数据
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// 添加表单字段
	_ = writer.WriteField("parentFileID", fmt.Sprintf("%d", parentFileID))
	_ = writer.WriteField("filename", fileName)
	_ = writer.WriteField("etag", md5Hash)
	_ = writer.WriteField("size", fmt.Sprintf("%d", len(data)))

	// 添加文件数据
	fileWriter, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return nil, fmt.Errorf("创建文件表单字段失败: %w", err)
	}

	_, err = fileWriter.Write(data)
	if err != nil {
		return nil, fmt.Errorf("写入文件数据失败: %w", err)
	}

	err = writer.Close()
	if err != nil {
		return nil, fmt.Errorf("关闭multipart writer失败: %w", err)
	}

	// 使用上传域名进行multipart上传
	err = f.makeAPICallWithRestMultipartToDomain(ctx, uploadDomain, "/upload/v2/file/single/create", "POST", &buf, writer.FormDataContentType(), &uploadResp)
	if err != nil {
		return nil, fmt.Errorf("单步上传API调用失败: %w", err)
	}

	// 检查API响应码
	if uploadResp.Code != 0 {
		// 检查是否是parentFileID不存在的错误
		if uploadResp.Code == 1 && strings.Contains(uploadResp.Message, "parentFileID不存在") {
			fs.Debugf(f, "⚠️ 父目录ID %d 不存在，清理缓存", parentFileID)
			// Remove cache cleanup

			// Unified parent ID verification logic: try to get correct parent ID
			fs.Debugf(f, "🔧 使用统一父目录ID修复策略")
			correctParentID := f.getCorrectParentFileID(ctx, parentFileID)

			// If got different parent ID, retry single-step upload
			if correctParentID != parentFileID {
				fs.Debugf(f, "✅ 找到正确的父目录ID: %d (原始ID: %d)，重试单步上传", correctParentID, parentFileID)
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

	// 优化：根据上传速度和时间判断上传类型并提供详细日志
	if duration < 5*time.Second && speed > 100 {
		fs.Debugf(f, "🚀 单步上传: 疑似秒传成功! FileID: %d, 耗时: %v, 速度: %.2f MB/s", uploadResp.Data.FileID, duration, speed)
	} else {
		fs.Debugf(f, "✅ 单步上传: 实际上传成功, FileID: %d, 耗时: %v, 速度: %.2f MB/s", uploadResp.Data.FileID, duration, speed)
	}

	// 🔧 缓存一致性修复：清除父目录的ListFile缓存
	f.clearListFileCache(parentFileID, "文件上传成功")

	// 返回Object
	return f.createObject(fileName, uploadResp.Data.FileID, int64(len(data)), md5Hash, time.Now()), nil
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
func (f *Fs) getUploadDomain(ctx context.Context) string {
	// 检查缓存是否有效
	f.uploadDomainMu.RLock()
	if f.uploadDomain != "" && time.Now().Before(f.uploadDomainExp) {
		domain := f.uploadDomain
		f.uploadDomainMu.RUnlock()
		fs.Debugf(f, "🎯 上传域名缓存命中: %s", domain)
		return domain
	}
	f.uploadDomainMu.RUnlock()

	// 缓存过期或不存在，重新获取
	fs.Debugf(f, "🔄 上传域名缓存过期，重新获取")

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
		fs.Debugf(f, "✅ 动态获取上传域名成功: %s", domain)

		// 缓存域名，有效期10分钟
		f.uploadDomainMu.Lock()
		f.uploadDomain = domain
		f.uploadDomainExp = time.Now().Add(10 * time.Minute)
		f.uploadDomainMu.Unlock()

		fs.Debugf(f, "💾 上传域名已缓存10分钟: %s", domain)
		return domain
	}

	// 如果动态获取失败，使用默认域名
	fs.Debugf(f, "⚠️ 动态获取上传域名失败，使用默认域名: %v", err)
	defaultDomains := []string{
		"https://openapi-upload.123242.com",
		"https://openapi-upload.123pan.com", // 备用域名
	}

	// 选择第一个默认域名，并缓存较短时间（2分钟）
	domain := defaultDomains[0]
	f.uploadDomainMu.Lock()
	f.uploadDomain = domain
	f.uploadDomainExp = time.Now().Add(2 * time.Minute) // 默认域名缓存时间较短
	f.uploadDomainMu.Unlock()

	fs.Debugf(f, "🌐 使用默认上传域名并缓存2分钟: %s", domain)
	return domain
}

// deleteFile 通过移动到回收站来删除文件
func (f *Fs) deleteFile(ctx context.Context, fileID int64) error {
	fs.Debugf(f, "🗑️ 删除文件，ID: %d", fileID)

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

	fs.Debugf(f, "✅ 成功删除文件，ID: %d", fileID)
	return nil
}

// moveFile 将文件移动到不同的目录
func (f *Fs) moveFile(ctx context.Context, fileID, toParentFileID int64) error {
	fs.Debugf(f, "📦 移动文件%d到父目录%d", fileID, toParentFileID)

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

	fs.Debugf(f, "✅ 成功移动文件%d到父目录%d", fileID, toParentFileID)
	return nil
}

// renameFile 重命名文件 - 使用正确的123网盘重命名API
func (f *Fs) renameFile(ctx context.Context, fileID int64, fileName string) error {
	fs.Debugf(f, "📝 重命名文件%d为: %s", fileID, fileName)

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

	// Use standard API call with built-in retry
	err := f.makeAPICallWithRest(ctx, "/api/v1/file/name", "PUT", reqBody, &response)
	if err != nil {
		return fmt.Errorf("rename file failed: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("rename file API error %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "✅ 文件重命名成功: %d -> %s", fileID, fileName)
	return nil
}

// handlePartialFileConflict 处理partial文件重命名冲突
// 当partial文件尝试重命名为最终文件名时发生冲突时调用
func (f *Fs) handlePartialFileConflict(ctx context.Context, fileID int64, partialName string, parentID int64, targetName string) (*Object, error) {
	fs.Debugf(f, "🔧 处理partial文件冲突: %s -> %s (父目录: %d)", partialName, targetName, parentID)

	// 移除.partial后缀获取原始文件名
	originalName := strings.TrimSuffix(partialName, ".partial")
	if targetName == "" {
		targetName = originalName
	}

	// 检查目标文件是否已存在
	exists, existingFileID, err := f.fileExistsInDirectory(ctx, parentID, targetName)
	if err != nil {
		fs.Debugf(f, "⚠️ 检查目标文件存在性失败: %v", err)
		return nil, fmt.Errorf("检查目标文件失败: %w", err)
	}

	if exists {
		fs.Debugf(f, "⚠️ 目标文件已存在，ID: %d", existingFileID)

		// 如果存在的文件就是当前文件（可能已经重命名成功）
		if existingFileID == fileID {
			fs.Debugf(f, "文件已成功重命名，返回成功对象")
			return f.createObject(targetName, fileID, -1, "", time.Now()), nil
		}

		// 存在不同的同名文件，生成唯一名称
		fs.Debugf(f, "目标文件名冲突，生成唯一名称")
		uniqueName, err := f.generateUniqueFileName(ctx, parentID, targetName)
		if err != nil {
			fs.Debugf(f, "❌ 生成唯一文件名失败: %v", err)
			return nil, fmt.Errorf("生成唯一文件名失败: %w", err)
		}

		fs.Logf(f, "⚠️ Partial文件重命名冲突，使用唯一名称: %s -> %s", targetName, uniqueName)
		targetName = uniqueName
	}

	// 尝试重命名partial文件为目标名称
	fs.Debugf(f, "📝 重命名partial文件: %d -> %s", fileID, targetName)
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

	fs.Debugf(f, "✅ Partial文件重命名成功: %s", targetName)
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

// checkInstantUpload 统一的秒传检查函数
func (f *Fs) checkInstantUpload(ctx context.Context, parentFileID int64, fileName, md5Hash string, fileSize int64) (*UploadCreateResp, bool, error) {
	fs.Debugf(f, "🔍 正常秒传检查流程: 检查服务器是否已有相同文件...")
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, fileSize)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create upload session: %w", err)
	}

	if createResp.Data.Reuse {
		fs.Debugf(f, "🚀 秒传成功: 服务器已有相同文件，无需上传数据 - Size: %s, MD5: %s", fs.SizeSuffix(fileSize), md5Hash)
		return createResp, true, nil
	}

	fs.Debugf(f, "⬆️ 秒传检查完成: 需要实际上传文件数据 - Size: %s", fs.SizeSuffix(fileSize))
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
func (f *Fs) createObject(remote string, fileID int64, size int64, md5Hash string, modTime time.Time) *Object {
	return &Object{
		fs:          f,
		remote:      remote,
		hasMetaData: true,
		id:          strconv.FormatInt(fileID, 10),
		size:        size,
		md5sum:      md5Hash,
		modTime:     modTime,
		isDir:       false,
	}
}

// newFs 从路径构造Fs，格式为 container:path
func newFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	fs.Debugf(nil, "🚀 123网盘newFs调用: name=%s, root=%s", name, root)

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
		name:             name,
		originalName:     originalName,
		root:             normalizedRoot,
		opt:              *opt,
		m:                m,
		parentDirCache:   make(map[int64]time.Time), // 初始化父目录缓存
		downloadURLCache: sync.Map{},                // 初始化下载URL缓存 - 使用sync.Map提升并发性能
		listFileCache:    cache.New(),               // 初始化ListFile结果缓存
		rootFolderID:     opt.RootFolderID,
		rst:              rest.NewClient(client).SetRoot(openAPIRootURL),
	}

	// Initialize pacers
	f.listPacer = createPacer(ctx, listV2APIMinSleep)
	strictPacerSleep := fileMoveMinSleep
	if opt.StrictPacerMinSleep > 0 {
		strictPacerSleep = time.Duration(opt.StrictPacerMinSleep)
	}
	f.strictPacer = createPacer(ctx, strictPacerSleep)
	uploadPacerSleep := uploadV2SliceMinSleep // 使用上传API的默认限制
	if opt.UploadPacerMinSleep > 0 {
		uploadPacerSleep = time.Duration(opt.UploadPacerMinSleep)
	}
	f.uploadPacer = createPacer(ctx, uploadPacerSleep)
	downloadPacerSleep := downloadInfoMinSleep // 使用下载API的默认限制
	if opt.DownloadPacerMinSleep > 0 {
		downloadPacerSleep = time.Duration(opt.DownloadPacerMinSleep)
	}
	f.downloadPacer = createPacer(ctx, downloadPacerSleep)

	// 根据123网盘API限流表初始化新增的pacer
	f.batchPacer = createPacer(ctx, 200*time.Millisecond) // 5 QPS for batch operations
	f.tokenPacer = createPacer(ctx, 1*time.Second)        // 1 QPS for token operations

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

	// Initialize directory cache with persistent support
	configData := map[string]string{
		"root_folder_id": f.rootFolderID,
		"endpoint":       openAPIRootURL,
	}
	f.dirCache = dircache.NewWithPersistent(normalizedRoot, f.rootFolderID, f, "123", configData)

	// 检查持久化缓存是否过期
	if expired, err := f.dirCache.IsExpiredPersistent(); err == nil && expired {
		fs.Debugf(f, "🔄 持久化缓存已过期，将在首次使用时重新构建")
	}

	// 💾 初始化持久化缓存系统
	f.initPersistentCache()

	// 💾 加载持久化缓存
	f.loadPersistentCaches()

	// Initialize authentication
	tokenLoaded := loadTokenFromConfig(f)
	fs.Debugf(f, "从配置加载令牌: %v（过期时间 %v）", tokenLoaded, f.tokenExpiry)

	var tokenRefreshNeeded bool
	if !tokenLoaded {
		tokenRefreshNeeded = true
	} else if time.Now().After(f.tokenExpiry.Add(-tokenRefreshWindow)) {
		tokenRefreshNeeded = true
	}

	if tokenRefreshNeeded {
		if err := f.refreshTokenIfNecessary(true, false); err != nil {
			return nil, fmt.Errorf("初始化认证失败: %w", err)
		}
	}

	// 🔧 标准rclone模式：遵循Google Drive等主流网盘的简单模式
	// Find the current root
	err = f.dirCache.FindRoot(ctx, false)

	if err != nil {
		// Assume it is a file (标准rclone模式)
		newRoot, remote := dircache.SplitPath(f.root)
		// 修复：避免锁复制，创建新的Fs实例而不是复制
		tempF := &Fs{
			name:             f.name,
			originalName:     f.originalName,
			root:             newRoot,
			opt:              f.opt,
			features:         f.features,
			rst:              f.rst,
			token:            f.token,
			tokenExpiry:      f.tokenExpiry,
			tokenRenewer:     f.tokenRenewer,
			m:                f.m,
			rootFolderID:     f.rootFolderID,
			parentDirCache:   make(map[int64]time.Time),
			downloadURLCache: sync.Map{},
			listFileCache:    cache.New(),
			uploadDomain:     f.uploadDomain,
			uploadDomainExp:  f.uploadDomainExp,
			listPacer:        f.listPacer,
			strictPacer:      f.strictPacer,
			uploadPacer:      f.uploadPacer,
			downloadPacer:    f.downloadPacer,
			batchPacer:       f.batchPacer,
			tokenPacer:       f.tokenPacer,
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
		// XXX: update the old f here instead of returning tempF, since
		// `features` were already filled with functions having *f as a receiver.
		// See https://github.com/rclone/rclone/issues/2182
		f.dirCache = tempF.dirCache
		f.root = tempF.root
		return f, fs.ErrorIsFile
	}

	// 初始化CDN故障转移（默认启用）
	fs.Debugf(f, "🔄 CDN故障转移已启用: 最大重试=%d", defaultMaxCDNRetries)

	return f, nil
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	fs.Debugf(f, "🔍 调用NewObject: %s", remote)

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
	isDir := (fileInfo.Type == 1)

	// ✅ 直接使用123网盘API的Type字段，无需额外判断
	// API的Type字段已经很准确：0=文件，1=文件夹
	fs.Debugf(f, "✅ 123网盘NewObject API类型: '%s' Type=%d, isDir=%v", fileInfo.Filename, fileInfo.Type, isDir)

	if isDir {
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
	o := f.createObject(objectRemote, fileIDInt, fileInfo.Size, fileInfo.Etag, time.Now())

	return o, nil
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// Put uploads the object.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	// 🔧 标准rclone模式：遵循Google Drive等主流网盘的简单模式
	existingObj, err := f.NewObject(ctx, src.Remote())
	switch err {
	case nil:
		return existingObj, existingObj.Update(ctx, in, src, options...)
	case fs.ErrorObjectNotFound:
		// Not found so create it
		return f.PutUnchecked(ctx, in, src, options...)
	default:
		return nil, err
	}
}

// PutUnchecked uploads the object without checking for existence first.
func (f *Fs) PutUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	// 规范化远程路径
	normalizedRemote := normalizePath(src.Remote())

	// 使用标准rclone流程：通过dirCache.FindPath处理路径
	leaf, parentID, err := f.dirCache.FindPath(ctx, normalizedRemote, true)
	if err != nil {
		return nil, err
	}

	// 验证并清理文件名
	fileName, warning := f.validateAndCleanFileNameUnified(leaf)
	if warning != nil {
		fs.Logf(f, "⚠️ 文件名验证警告: %v", warning)
	}

	// Convert parentID to int64
	parentFileID, err := parseFileID(parentID)
	if err != nil {
		return nil, fmt.Errorf("解析父目录ID失败: %w", err)
	}

	// Use streaming optimization for cross-cloud transfers
	return f.streamingPut(ctx, in, src, parentFileID, fileName)
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

// readMetaData gets the metadata if it hasn't already been fetched
func (o *Object) readMetaData(ctx context.Context) error {
	if o.hasMetaData {
		return nil
	}

	if o.id == "" {
		return fs.ErrorObjectNotFound
	}

	startTime := time.Now()
	fs.Debugf(o, "🚀 readMetaData开始: fileID=%s", o.id)

	// 使用现有的getFileInfo API获取文件详细信息
	fileInfo, err := o.fs.getFileInfo(ctx, o.id)
	duration := time.Since(startTime)

	if err != nil {
		fs.Debugf(o, "❌ readMetaData失败: %v, 耗时=%v", err, duration)
		return err
	}

	// 更新对象的元数据
	o.size = fileInfo.Size
	o.md5sum = fileInfo.Etag
	// 注意：123网盘API可能不返回准确的修改时间，保持现有的modTime
	o.hasMetaData = true

	fs.Debugf(o, "✅ readMetaData成功: size=%d, md5=%s, 耗时=%v", o.size, o.md5sum, duration)
	return nil
}

// ModTime returns the modification time
func (o *Object) ModTime(ctx context.Context) time.Time {
	// 🚀 优化：只在没有元数据时才获取，避免不必要的API调用
	if !o.hasMetaData && o.id != "" {
		if err := o.readMetaData(ctx); err != nil {
			fs.Debugf(o, "Failed to read metadata for ModTime: %v", err)
		}
	}
	return o.modTime
}

// Size returns the size of the file
func (o *Object) Size() int64 {
	// 🚀 优化：只在没有元数据时才获取，避免不必要的API调用
	if !o.hasMetaData && o.id != "" {
		if err := o.readMetaData(context.TODO()); err != nil {
			fs.Debugf(o, "Failed to read metadata for Size: %v", err)
			return -1
		}
	}
	return o.size
}

// Hash returns the MD5 checksum
func (o *Object) Hash(ctx context.Context, t fshash.Type) (string, error) {
	if t != fshash.MD5 {
		return "", fshash.ErrUnsupported
	}
	// 🚀 优化：只在没有元数据时才获取，避免不必要的API调用
	if !o.hasMetaData && o.id != "" {
		if err := o.readMetaData(ctx); err != nil {
			fs.Debugf(o, "Failed to read metadata for Hash: %v", err)
			return "", err
		}
	}
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

	// 🌊 优先检查流式哈希模式：避免临时文件创建
	if o.fs.opt.StreamHashMode {
		fs.Debugf(o, "🌊 StreamHashMode启用，使用普通下载避免临时文件: %s", o.Remote())
		return o.openWithCDNFailover(ctx, options...)
	}

	// 🚀 优化：检测是否为小范围读取（如预览、MIME检测等）
	var rangeOption *fs.RangeOption
	hasDisableOption := false
	hasRangeOption := false

	for _, option := range options {
		if option.String() == "DisableConcurrentDownload" {
			hasDisableOption = true
		}
		// 检查是否有Range请求
		if ro, ok := option.(*fs.RangeOption); ok {
			hasRangeOption = true
			rangeOption = ro
		}
	}

	// 🚀 智能下载策略：小范围读取优化
	if rangeOption != nil {
		start, end := rangeOption.Decode(o.size)
		requestSize := end - start + 1

		// 如果请求的数据量很小（如前1MB用于预览/MIME检测），优先使用简单下载
		if requestSize <= 1024*1024 { // 1MB阈值
			fs.Debugf(o, "🎯 检测到小范围读取请求: %d-%d (%s)，使用简单下载避免下载整个文件",
				start, end, fs.SizeSuffix(requestSize))
			return o.openWithSimpleRetry(ctx, options...)
		} else {
			fs.Debugf(o, "📥 大范围读取请求: %d-%d (%s)，考虑并发下载",
				start, end, fs.SizeSuffix(requestSize))
		}
	}

	// 跨云传输优化：检测大文件并启用多线程下载
	// 修复：Range请求不使用并发下载，避免下载整个文件
	if !hasDisableOption && !hasRangeOption && o.size >= minFileSizeForConcurrency { // 使用常量定义的阈值
		return o.openWithConcurrency(ctx, options...)
	}

	// 使用简化的重试机制（优化：直接重新获取URL而不是复杂的CDN故障转移）
	return o.openWithSimpleRetry(ctx, options...)
}

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
			return shouldRetry(fmt.Errorf("获取下载链接失败: %w", err))
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
			return shouldRetry(fmt.Errorf("下载请求失败: %w", err))
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return shouldRetry(fmt.Errorf("下载失败，状态码: %d, 响应: %s", resp.StatusCode, string(body)))
		}

		return false, nil
	})

	if err != nil {
		return nil, err
	}

	// 🔧 混合进度显示方案：既修复rclone标准进度，又保留详细进度日志
	// 创建包装的ReadCloser来提供详细的下载进度日志，同时让rclone正确处理标准进度
	return &ProgressReadCloser{
		ReadCloser:      resp.Body,
		fs:              o.fs,
		object:          o,
		totalSize:       o.size,
		transferredSize: 0,
		startTime:       time.Now(),
	}, nil
}

// openWithCustomConcurrency 使用自定义并发下载（当统一下载器不可用时）
func (o *Object) openWithCustomConcurrency(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// 如果 StreamHashMode 启用，则直接回退到普通下载（避免临时文件）
	if o.fs.opt.StreamHashMode {
		fs.Debugf(o, "DisableTempFile is true, falling back to normal download for: %s", o.Remote())
		return o.openNormal(ctx, options...)
	}

	fs.Debugf(o, "Starting custom concurrent download: %s", fs.SizeSuffix(o.size))

	// 创建临时文件用于并发下载
	tempFile, err := os.CreateTemp("", "rclone-123-download-*.tmp")
	if err != nil {
		fs.Debugf(o, "创建临时文件失败，回退到普通下载: %v", err)
		return o.openNormal(ctx, options...)
	}

	// 使用简化的并发下载逻辑
	err = o.downloadWithCustomConcurrency(ctx, tempFile)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		fs.Debugf(o, "⚠️ 自定义并发下载失败，回退到普通下载: %v", err)
		return o.openNormal(ctx, options...)
	}

	// 重置文件指针到开始位置
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return nil, fmt.Errorf("重置临时文件指针失败: %w", err)
	}

	// 返回一个包装的ReadCloser，在关闭时删除临时文件
	return &ConcurrentDownloadReader{
		file:     tempFile,
		tempPath: tempFile.Name(),
	}, nil
}

// downloadWithCustomConcurrency 自定义并发下载实现
func (o *Object) downloadWithCustomConcurrency(ctx context.Context, tempFile *os.File) error {
	downloadStartTime := time.Now()
	// 简化的并发下载配置
	chunkSize := int64(defaultDownloadChunkSize) // 使用常量定义的分片大小
	maxConcurrency := defaultDownloadConcurrency // 使用常量定义的并发数

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

	// 计算总体下载统计
	totalDuration := time.Since(downloadStartTime)
	avgSpeed := float64(o.size) / totalDuration.Seconds()
	fs.Infof(o, "✅ 123网盘下载完成: %s | ⏱️ %v | 🚀 平均 %s/s",
		fs.SizeSuffix(o.size), totalDuration.Truncate(time.Second), fs.SizeSuffix(int64(avgSpeed)))

	return nil
}

// downloadChunk 下载单个分片
func (o *Object) downloadChunk(ctx context.Context, url string, tempFile *os.File, start, end, chunkIndex int64) error {
	startTime := time.Now()
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
	defer func() { _ = resp.Body.Close() }()

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

	// 计算下载速度
	duration := time.Since(startTime)
	speed := float64(totalRead) / duration.Seconds()
	fs.Infof(o, "📥 分片%d下载完成 %s [%d-%d] 🚀 %s/s",
		chunkIndex, fs.SizeSuffix(totalRead), start, end, fs.SizeSuffix(int64(speed)))

	return nil
}

// Update the object with new content.
// 优化版本：使用统一上传系统，支持动态参数调整和多线程上传
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	fs.Debugf(o.fs, "调用Update: %s", o.remote)

	startTime := time.Now()

	// Use dircache to find parent directory
	leaf, parentID, err := o.fs.findPathSafe(ctx, o.remote, false)
	if err != nil {
		return err
	}

	// Convert parentID to int64
	parentFileID, err := parseFileID(parentID)
	if err != nil {
		return fmt.Errorf("解析父目录ID失败: %w", err)
	}

	// 验证并清理文件名
	cleanedFileName, warning := o.fs.validateAndCleanFileNameUnified(leaf)
	if warning != nil {
		fs.Logf(o.fs, "⚠️ 文件名验证警告: %v", warning)
		leaf = cleanedFileName
	}

	// 使用新的统一上传系统进行更新
	fs.Debugf(o.fs, "🔄 使用统一上传系统进行Update操作")
	newObj, err := o.fs.streamingPut(ctx, in, src, parentFileID, leaf)

	// 记录上传完成或错误
	duration := time.Since(startTime)
	if err != nil {
		fs.Errorf(o.fs, "❌ Update操作失败: %v", err)
		return err
	}

	// 更新当前对象的元数据
	o.size = newObj.size
	o.md5sum = newObj.md5sum
	o.modTime = newObj.modTime
	o.id = newObj.id

	fs.Debugf(o.fs, "✅ Update操作成功，耗时: %v", duration)

	return nil
}

// Remove the object.
func (o *Object) Remove(ctx context.Context) error {
	fs.Debugf(o.fs, "🗑️ 调用Remove: %s", o.remote)

	// 检查文件ID是否有效
	if o.id == "" {
		fs.Debugf(o.fs, "⚠️ 文件ID为空，可能是partial文件或上传失败的文件: %s", o.remote)
		// 对于partial文件或上传失败的文件，我们无法删除，但也不应该报错
		// 因为这些文件可能根本不存在于服务器上
		return nil
	}

	// Convert file ID to int64
	fileID, err := parseFileID(o.id)
	if err != nil {
		fs.Debugf(o.fs, "❌ 解析文件ID失败，可能是partial文件: %s, 错误: %v", o.remote, err)
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
	fs.Debugf(f, "🚀 查找叶节点优化版: pathID=%s, leaf=%s", pathID, leaf)

	// ✅ 首先检查dirCache是否已有结果
	parentPath, ok := f.dirCache.GetInv(pathID)
	if ok {
		var itemPath string
		if parentPath == "" {
			itemPath = leaf
		} else {
			itemPath = parentPath + "/" + leaf
		}
		// 检查是否已缓存
		if cachedID, found := f.dirCache.Get(itemPath); found {
			fs.Debugf(f, "🎯 FindLeaf缓存命中: %s -> ID=%s", leaf, cachedID)
			return cachedID, true, nil
		}
	}

	// Convert pathID to int64
	parentFileID, err := parseFileID(pathID)
	if err != nil {
		return "", false, fmt.Errorf("解析父目录ID失败: %w", err)
	}

	// Use pagination to search through all files, similar to pathToFileID
	next := "0"
	maxIterations := 1000 // Prevent infinite loops
	iteration := 0

	for {
		iteration++
		if iteration > maxIterations {
			return "", false, fmt.Errorf("查找叶节点 %s 超过最大迭代次数 %d", leaf, maxIterations)
		}

		lastFileIDInt, err := strconv.Atoi(next)
		if err != nil {
			return "", false, fmt.Errorf("invalid next token: %s", next)
		}

		var response *ListResponse
		// Use list pacer for rate limiting（limit=100是API最大限制）
		err = f.listPacer.Call(func() (bool, error) {
			response, err = f.ListFile(ctx, int(parentFileID), 100, "", "", lastFileIDInt)
			if err != nil {
				return shouldRetry(err)
			}
			return false, nil
		})
		if err != nil {
			return "", false, err
		}

		if response.Code != 0 {
			return "", false, fmt.Errorf("API error %d: %s", response.Code, response.Message)
		}

		// ✅ 批量缓存所有文件，提升后续查询性能
		parentPath, parentPathOk := f.dirCache.GetInv(pathID)
		targetFound := false

		for _, file := range response.Data.FileList {
			// 只处理有效文件（不在回收站且未被审核驳回）
			if file.Trashed == 0 && file.Status < 100 {
				fileID := strconv.FormatInt(file.FileID, 10)

				// 批量缓存所有有效文件
				if parentPathOk {
					var itemPath string
					if parentPath == "" {
						itemPath = file.Filename
					} else {
						itemPath = parentPath + "/" + file.Filename
					}
					f.dirCache.Put(itemPath, fileID)
					fs.Debugf(f, "📦 批量缓存: %s -> ID=%s", file.Filename, fileID)
				}

				// 检查是否是目标文件
				if file.Filename == leaf {
					foundID = fileID
					targetFound = true
					fs.Debugf(f, "✅ FindLeaf找到目标: %s -> ID=%s, Type=%d", leaf, foundID, file.Type)
				}
			} else {
				fs.Debugf(f, "🗑️ 跳过无效文件: %s (trashed=%d, status=%d)", file.Filename, file.Trashed, file.Status)
			}
		}

		if targetFound {
			return foundID, true, nil
		}

		// Check if there are more pages
		if len(response.Data.FileList) == 0 {
			break
		}

		nextRaw := strconv.FormatInt(response.Data.LastFileId, 10)
		if nextRaw == "-1" {
			break
		} else {
			next = nextRaw
		}
	}

	return "", false, nil
}

// findLeafWithForceRefresh 强制刷新缓存后查找叶节点
// 用于解决缓存不一致导致的目录查找失败问题
func (f *Fs) findLeafWithForceRefresh(ctx context.Context, pathID, leaf string) (foundID string, found bool, err error) {
	fs.Debugf(f, "🔄 强制刷新查找叶节点: pathID=%s, leaf=%s", pathID, leaf)

	// Convert pathID to int64
	parentFileID, err := parseFileID(pathID)
	if err != nil {
		return "", false, fmt.Errorf("解析父目录ID失败: %w", err)
	}

	// Use pagination to search through all files with force refresh
	next := "0"
	maxIterations := 1000 // Prevent infinite loops
	iteration := 0

	for {
		iteration++
		if iteration > maxIterations {
			return "", false, fmt.Errorf("强制刷新查找叶节点 %s 超过最大迭代次数 %d", leaf, maxIterations)
		}

		lastFileIDInt, err := strconv.Atoi(next)
		if err != nil {
			return "", false, fmt.Errorf("invalid next token: %s", next)
		}

		var response *ListResponse
		// 🔧 强制刷新：绕过缓存直接调用API
		err = f.listPacer.Call(func() (bool, error) {
			response, err = f.listFileDirectAPI(ctx, int(parentFileID), 100, "", "", lastFileIDInt)
			if err != nil {
				return shouldRetry(err)
			}
			return false, nil
		})
		if err != nil {
			return "", false, fmt.Errorf("强制刷新API调用失败: %w", err)
		}

		if response.Code != 0 {
			return "", false, fmt.Errorf("API error %d: %s", response.Code, response.Message)
		}

		// Search for the leaf in current page
		for _, file := range response.Data.FileList {
			// 🔧 检查文件名匹配且不在回收站且未被审核驳回
			if file.Filename == leaf && file.Trashed == 0 && file.Status < 100 {
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

				fs.Debugf(f, "✅ 强制刷新找到叶节点: %s -> %s, Type=%d", leaf, foundID, file.Type)
				return foundID, found, nil
			}
		}

		// Check if there are more pages
		if len(response.Data.FileList) == 0 {
			break
		}

		nextRaw := strconv.FormatInt(response.Data.LastFileId, 10)
		if nextRaw == "-1" {
			break
		} else {
			next = nextRaw
		}
	}

	fs.Debugf(f, "强制刷新未找到叶节点: %s", leaf)
	return "", false, nil
}

// CreateDir creates a directory with the given name in the parent folder pathID.
// Used by dircache.
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	fs.Debugf(f, "创建目录: pathID=%s, leaf=%s", pathID, leaf)

	// 🔧 缓存一致性修复：在目录创建前清除相关的ListFile缓存
	// 这确保目录创建时使用最新的API数据，而不是缓存的旧数据
	if parentFileID, err := strconv.ParseInt(pathID, 10, 64); err == nil {
		f.clearListFileCache(parentFileID, "目录创建前-CreateDir")
	}

	// 验证参数
	if leaf == "" {
		return "", fmt.Errorf("目录名不能为空")
	}
	if pathID == "" {
		return "", fmt.Errorf("父目录ID不能为空")
	}

	// 验证并清理目录名
	cleanedDirName, warning := f.validateAndCleanFileNameUnified(leaf)
	if warning != nil {
		fs.Logf(f, "⚠️ 目录名验证警告: %v", warning)
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
func loadTokenFromConfig(f *Fs) bool {
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
func (f *Fs) saveToken(m configmap.Mapper) {
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
		fs.Errorf(f, "❌ 保存令牌时序列化失败: %v", err)
		return
	}

	m.Set("token", string(tokenJSON))
	// m.Set does not return an error, so we assume it succeeds.
	// Any errors related to config saving would be handled by the underlying config system.

	fs.Debugf(f, "使用原始名称%q保存令牌到配置文件", f.originalName)
}

// Removed unused function: setupTokenRenewer

// refreshTokenIfNecessary refreshes the token if necessary
func (f *Fs) refreshTokenIfNecessary(refreshTokenExpired bool, forceRefresh bool) error {
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
	f.saveToken(f.m)

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

// GetCode 实现APIResponse接口
func (r *Response) GetCode() int {
	return r.Code
}

// GetMessage 实现APIResponse接口
func (r *Response) GetMessage() string {
	return r.Message
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
	defer func() { _ = resp.Body.Close() }()
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
	parentFileID, err := parseFileID(dirID)
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
		// 使用ListFile API清理目录（limit=100是API最大限制）
		response, err := f.ListFile(ctx, int(parentFileID), 100, "", "", int(lastFileID))
		if err != nil {
			return err
		}

		if response.Code != 0 {
			return fmt.Errorf("API error %d: %s", response.Code, response.Message)
		}

		// 🔧 收集所有有效文件：同时检查回收站和审核状态
		for _, file := range response.Data.FileList {
			// 必须过滤回收站文件 (trashed=1) 和审核驳回文件 (status>=100)
			if file.Trashed == 0 && file.Status < 100 {
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
			fs.Errorf(f, "❌ 删除文件失败 %s: %v", file.Filename, err)
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
	parentFileID, err := parseFileID(dirID)
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
		// 使用ListFile API列出所有文件（limit=100是API最大限制）
		response, err := f.ListFile(ctx, int(parentFileID), 100, "", "", int(lastFileID))
		if err != nil {
			return nil, err
		}

		if response.Code != 0 {
			return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
		}

		// 🔧 完善过滤逻辑：同时检查回收站和审核状态
		for _, file := range response.Data.FileList {
			// 必须过滤回收站文件 (trashed=1) 和审核驳回文件 (status>=100)
			if file.Trashed == 0 && file.Status < 100 {
				fs.Debugf(f, "✅ 文件通过过滤: %s (trashed=%d, status=%d)", file.Filename, file.Trashed, file.Status)
				allFiles = append(allFiles, file)
			} else {
				fs.Debugf(f, "🗑️ 文件被过滤: %s (trashed=%d, status=%d)", file.Filename, file.Trashed, file.Status)
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

		// ✅ 直接使用123网盘API的Type字段，无需额外判断
		isDir := (file.Type == 1)
		fs.Debugf(f, "✅ 123网盘List API类型: '%s' Type=%d, isDir=%v", cleanedFilename, file.Type, isDir)

		if isDir { // Directory
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

	// 🔧 缓存一致性修复：在目录创建前清除相关的ListFile缓存
	// 这确保目录创建时使用最新的API数据，而不是缓存的旧数据
	f.clearListFileCacheByPath(ctx, dir, "目录创建前")

	// Use dircache to create the directory
	_, err := f.dirCache.FindDir(ctx, dir, true)
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

	// 🔧 缓存一致性修复：清除相关的ListFile缓存
	f.clearListFileCacheByPath(ctx, dir, "目录删除")

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
	dstName, dstDirID, err := f.findPathSafe(ctx, normalizedRemote, true)
	if err != nil {
		return nil, err
	}

	// 验证并清理目标文件名
	cleanedDstName, warning := f.validateAndCleanFileNameUnified(dstName)
	if warning != nil {
		fs.Logf(f, "⚠️ 移动操作文件名验证警告: %v", warning)
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
		fs.Logf(f, "⚠️ 检测到文件名冲突，使用唯一名称: %s -> %s", dstName, uniqueDstName)
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
						fs.Logf(f, "⚠️ 使用唯一文件名避免冲突: %s", dstName)
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

	// 🔧 缓存一致性修复：清除相关的ListFile缓存
	// 清除源目录和目标目录的缓存
	f.clearListFileCacheByPath(ctx, path.Dir(srcObj.Remote()), "文件移动-源目录")
	f.clearListFileCacheByPath(ctx, path.Dir(remote), "文件移动-目标目录")

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
	dstName, _, err := f.findPathSafe(ctx, normalizedRemote, true)
	if err != nil {
		return nil, err
	}

	// 验证并清理目标文件名
	_, warning := f.validateAndCleanFileNameUnified(dstName)
	if warning != nil {
		fs.Logf(f, "⚠️ 复制操作文件名验证警告: %v", warning)
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

// Removed unused functions:
// - getHTTPClient, getAdaptiveTimeout, detectNetworkSpeed, getOptimalConcurrency, getOptimalChunkSize
// - smartStreamHashTransfer, memoryHashTransfer, streamHashTransfer (失效的传输函数)

// streamHashTransferWithReader 大文件流式哈希传输（使用已包装的Reader）
// 🔧 修复进度显示问题：使用rclone传递的已被Transfer包装的Reader
// 🔧 性能优化：动态缓冲区大小，支持重试机制和网络中断恢复
func (f *Fs) streamHashTransferWithReader(ctx context.Context, in io.Reader, src fs.Object, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()

	// 🔧 动态缓冲区大小：根据文件大小和可用内存优化
	bufferSize := f.calculateOptimalBufferSize(fileSize)
	fs.Debugf(f, "🎯 优化缓冲区大小: %s (文件大小: %s)", fs.SizeSuffix(bufferSize), fs.SizeSuffix(fileSize))

	// 第一遍：流式计算MD5哈希（为了秒传）
	fs.Infof(f, "🔄 第一遍：流式计算文件哈希用于秒传...")

	// 🔧 关键修复：使用传递的Reader（已被Transfer包装），进度会正确显示
	srcReader := in

	// 🔧 创建跨云传输统一进度跟踪器
	var twoStepProgress *TwoStepProgressReader

	// 检查是否为跨云传输（输入是ReadCloser）
	if rc, ok := in.(io.ReadCloser); ok {
		// 获取Account对象用于进度跟踪
		_, account := accounting.UnWrapAccounting(in)
		twoStepProgress = NewTwoStepProgressReader(rc, f, fileName, src.Size(), account)
		srcReader = twoStepProgress
		fs.Infof(f, "🌐 启用跨云传输统一进度显示: %s", fileName)
		fs.Debugf(f, "🚀 流式哈希模式: 跳过临时文件创建，直接使用内存流式处理 | 文件: %s | 大小: %s", fileName, fs.SizeSuffix(src.Size()))
	} else {
		fs.Debugf(f, "🚀 流式哈希模式: 本地文件上传，跳过临时文件创建 | 文件: %s | 大小: %s", fileName, fs.SizeSuffix(src.Size()))
	}

	// 🔧 带重试机制的流式MD5计算
	var md5Hash string
	maxRetries := 3

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// 重置Reader（如果支持的话）
		if attempt > 1 {
			fs.Infof(f, "🔄 重试哈希计算: 第 %d 次尝试", attempt)

			// 如果Reader支持重新打开
			if reOpenReader, ok := srcReader.(interface{ Seek(int64, int) error }); ok {
				if err := reOpenReader.Seek(0, 0); err != nil {
					fs.Errorf(f, "⚠️ 重置Reader失败: %v", err)
					return nil, fmt.Errorf("failed to reset reader on retry %d: %w", attempt, err)
				}
			} else {
				// 不支持重置，需要重新打开
				if reOpenSrc, ok := src.(interface {
					Open(context.Context) (io.ReadCloser, error)
				}); ok {
					fs.Debugf(f, "🔧 重新打开源文件进行重试...")
					reOpenedReader, err := reOpenSrc.Open(ctx)
					if err != nil {
						return nil, fmt.Errorf("failed to reopen source on retry %d: %w", attempt, err)
					}
					defer reOpenedReader.Close()
					srcReader = reOpenedReader
				} else {
					return nil, fmt.Errorf("source does not support retry after hash calculation failure")
				}
			}
		}

		// 流式计算MD5
		hasher := md5.New()
		buffer := make([]byte, bufferSize)

		totalRead := int64(0)
		readErrors := 0

		for {
			// 🔧 检查上下文取消
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("operation cancelled during hash calculation: %w", ctx.Err())
			default:
			}

			n, err := srcReader.Read(buffer)
			if n > 0 {
				hasher.Write(buffer[:n])
				totalRead += int64(n)

				// 🔧 优化进度显示：使用对数缩放减少日志频率
				if totalRead%(1024*1024) == 0 || err == io.EOF || fileSize < 1024*1024 {
					percentage := float64(totalRead) / float64(fileSize) * 100
					fs.Debugf(f, "📊 流式哈希计算进度: %s/%s (%.1f%%) - 尝试 %d",
						fs.SizeSuffix(totalRead), fs.SizeSuffix(fileSize), percentage, attempt)
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				readErrors++
				fs.Errorf(f, "⚠️ 读取错误 %d/%d: %v", readErrors, 3, err)
				if readErrors >= 3 {
					return nil, fmt.Errorf("failed to read data for hash calculation after %d attempts: %w", attempt, err)
				}
				// 短暂暂停后重试
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}

		// 验证读取完整性
		if totalRead != fileSize {
			return nil, fmt.Errorf("hash calculation failed: expected %d bytes but read %d bytes", fileSize, totalRead)
		}

		md5Hash = fmt.Sprintf("%x", hasher.Sum(nil))
		fs.Infof(f, "✅ 流式哈希计算完成: MD5: %s (尝试 %d)", md5Hash, attempt)
		break // 成功，计算完成
	}

	// 🔧 优化秒传尝试：添加重试机制
	var createResp *UploadCreateResp
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		fs.Infof(f, "🚀 尝试秒传 (第 %d 次)...", attempt)

		createResp, err = f.createUpload(ctx, parentFileID, fileName, md5Hash, fileSize)
		if err == nil {
			break // 成功
		}

		fs.Errorf(f, "⚠️ 秒传尝试 %d 失败: %v", attempt, err)
		if attempt < maxRetries {
			// 短暂延迟后重试
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create upload session after %d attempts: %w", maxRetries, err)
	}

	if createResp.Data.Reuse {
		fs.Infof(f, "🎉 流式哈希计算后秒传成功！")
		return f.createObject(fileName, createResp.Data.FileID, fileSize, md5Hash, time.Now()), nil
	}

	// 秒传失败，需要重新下载并边下边传
	// 注意：此时Reader已经被读完，需要重新打开源文件
	fs.Infof(f, "⬆️ 秒传失败，第二遍：边下边传（服务器分块大小: %s）", fs.SizeSuffix(createResp.Data.SliceSize))

	return f.streamUploadWithSession(ctx, src, createResp, fileName, md5Hash)
}

// calculateOptimalBufferSize 根据文件大小计算最优缓冲区大小
func (f *Fs) calculateOptimalBufferSize(fileSize int64) int {
	// 小文件 (< 1MB): 使用较小的缓冲区避免浪费
	if fileSize < 1024*1024 {
		return 64 * 1024 // 64KB
	}

	// 中等文件 (1MB - 100MB): 使用中等缓冲区
	if fileSize < 100*1024*1024 {
		return 512 * 1024 // 512KB
	}

	// 大文件 (100MB - 1GB): 使用较大缓冲区
	if fileSize < 1024*1024*1024 {
		return 2 * 1024 * 1024 // 2MB
	}

	// 超大文件 (>= 1GB): 使用最大缓冲区但不超过系统限制
	return 4 * 1024 * 1024 // 4MB
}

// uploadFromMemory 函数已删除，小文件现在直接使用 singleStepUpload API

// streamUploadWithSession 使用已有会话进行边下边传
func (f *Fs) streamUploadWithSession(ctx context.Context, srcObj fs.Object, createResp *UploadCreateResp, fileName, md5Hash string) (*Object, error) {
	fileSize := srcObj.Size()
	serverChunkSize := createResp.Data.SliceSize // 使用服务器指定的分片大小

	// 检查是否需要分片上传（与流式哈希模式的阈值保持一致）
	if fileSize <= 10*1024*1024 { // 10MB阈值，与smartStreamHashTransfer保持一致
		// 小文件：不应该走到这里，因为小文件应该直接使用 singleStepUpload
		// 但如果走到这里，说明是从分片上传流程过来的，需要继续使用分片上传完成
		fs.Infof(f, "📤 边下边传：小文件继续使用分片上传流程（已创建分片上传会话）")

		// 下载整个文件
		srcReader, err := srcObj.Open(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to open source file for single upload: %w", err)
		}
		defer srcReader.Close()

		data, err := io.ReadAll(srcReader)
		if err != nil {
			return nil, fmt.Errorf("failed to read file data for single upload: %w", err)
		}

		// 使用分片上传API（因为已经创建了分片上传会话）
		err = f.uploadSinglePart(ctx, createResp.Data.PreuploadID, data)
		if err != nil {
			return nil, fmt.Errorf("failed to upload single part: %w", err)
		}

		// 完成上传并获取真实文件ID
		result, err := f.completeUploadWithResultAndSize(ctx, createResp.Data.PreuploadID, fileSize)
		if err != nil {
			return nil, fmt.Errorf("failed to complete upload: %w", err)
		}

		return f.createObject(fileName, result.FileID, fileSize, md5Hash, time.Now()), nil
	} else {
		// 大文件：真正的边下边传实现
		fs.Infof(f, "📤 真正的边下边传：文件大小=%s，服务器分片大小=%s",
			fs.SizeSuffix(fileSize), fs.SizeSuffix(serverChunkSize))

		totalChunks := (fileSize + serverChunkSize - 1) / serverChunkSize
		fs.Infof(f, "🔢 总分片数: %d", totalChunks)

		// 真正的边下边传：每次只处理一个分片
		for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
			// 计算当前分片的范围
			start := chunkIndex * serverChunkSize
			end := start + serverChunkSize - 1
			if end >= fileSize {
				end = fileSize - 1
			}
			actualChunkSize := end - start + 1

			fs.Infof(f, "📥 下载分片 %d/%d: 范围=%d-%d, 大小=%s",
				chunkIndex+1, totalChunks, start, end, fs.SizeSuffix(actualChunkSize))

			// 下载当前分片
			chunkData, err := f.downloadChunkRange(ctx, srcObj, start, end)
			if err != nil {
				return nil, fmt.Errorf("下载分片 %d 失败: %w", chunkIndex, err)
			}

			fs.Infof(f, "📤 上传分片 %d/%d: 大小=%s",
				chunkIndex+1, totalChunks, fs.SizeSuffix(int64(len(chunkData))))

			// 立即上传当前分片
			err = f.uploadSingleChunk(ctx, createResp.Data.PreuploadID, int(chunkIndex), chunkData)
			if err != nil {
				return nil, fmt.Errorf("上传分片 %d 失败: %w", chunkIndex, err)
			}

			fs.Infof(f, "✅ 分片 %d/%d 边下边传完成，内存占用: %s → 已释放",
				chunkIndex+1, totalChunks, fs.SizeSuffix(int64(len(chunkData))))

			// 立即释放内存
			chunkData = nil
			// 强制垃圾回收，确保内存释放
			if chunkIndex%5 == 4 { // 每5个分片强制GC一次
				runtime.GC()
			}
		}

		fs.Infof(f, "🎯 所有分片边下边传完成，开始完成上传")

		// 完成上传并获取真实文件ID
		result, err := f.completeUploadWithResultAndSize(ctx, createResp.Data.PreuploadID, fileSize)
		if err != nil {
			return nil, fmt.Errorf("完成上传失败: %w", err)
		}

		fs.Infof(f, "🎉 边下边传成功完成！文件ID: %d", result.FileID)
		return f.createObject(fileName, result.FileID, fileSize, md5Hash, time.Now()), nil
	}
}

// downloadChunkRange 下载源文件的指定范围数据
func (f *Fs) downloadChunkRange(ctx context.Context, srcObj fs.Object, start, end int64) ([]byte, error) {
	// 使用Range请求下载指定分片
	options := []fs.OpenOption{
		&fs.RangeOption{Start: start, End: end},
	}

	reader, err := srcObj.Open(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("failed to open source with range %d-%d: %w", start, end, err)
	}
	defer reader.Close()

	// 读取分片数据
	chunkData, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read chunk data: %w", err)
	}

	fs.Debugf(f, "📥 下载分片完成: 范围=%d-%d, 大小=%s", start, end, fs.SizeSuffix(int64(len(chunkData))))
	return chunkData, nil
}

// uploadSingleChunk 上传单个分片到123网盘
func (f *Fs) uploadSingleChunk(ctx context.Context, preuploadID string, chunkIndex int, chunkData []byte) error {
	// 计算分片MD5
	chunkHash := fmt.Sprintf("%x", md5.Sum(chunkData))
	partNumber := int64(chunkIndex + 1) // 123网盘分片编号从1开始

	// 使用现有的分片上传API
	err := f.uploadPartWithMultipart(ctx, preuploadID, partNumber, chunkHash, chunkData)
	if err != nil {
		return fmt.Errorf("failed to upload chunk %d: %w", chunkIndex, err)
	}

	fs.Debugf(f, "📤 上传分片完成: 索引=%d, 分片号=%d, 大小=%s, MD5=%s",
		chunkIndex, partNumber, fs.SizeSuffix(int64(len(chunkData))), chunkHash)
	return nil
}

// measureNetworkLatency removed - use reasonable default latency
// This function is no longer needed as we simplified network detection

// 💾 持久化缓存管理方法

// initPersistentCache 初始化持久化缓存系统
func (f *Fs) initPersistentCache() {
	// 获取缓存目录
	cacheDir := config.GetCacheDir()
	f.persistentCacheDir = filepath.Join(cacheDir, "123-cache", f.name)

	// 创建缓存目录
	if err := os.MkdirAll(f.persistentCacheDir, 0755); err != nil {
		fs.Debugf(f, "⚠️ 创建持久化缓存目录失败: %v", err)
		return
	}

	// 设置缓存文件路径
	f.parentDirCacheFile = filepath.Join(f.persistentCacheDir, "parent_dir_cache.json")
	f.listFileCacheFile = filepath.Join(f.persistentCacheDir, "list_file_cache.json")

	fs.Debugf(f, "💾 持久化缓存目录: %s", f.persistentCacheDir)
}

// loadPersistentCaches 加载持久化缓存
func (f *Fs) loadPersistentCaches() {
	// 加载父目录缓存
	f.loadParentDirCache()

	// 加载ListFile缓存
	f.loadListFileCache()
}

// loadParentDirCache 加载父目录缓存
func (f *Fs) loadParentDirCache() {
	if f.parentDirCacheFile == "" {
		return
	}

	data, err := os.ReadFile(f.parentDirCacheFile)
	if err != nil {
		if !os.IsNotExist(err) {
			fs.Debugf(f, "⚠️ 读取父目录缓存失败: %v", err)
		}
		return
	}

	var cache PersistentParentDirCache
	if err := json.Unmarshal(data, &cache); err != nil {
		fs.Debugf(f, "⚠️ 解析父目录缓存失败: %v", err)
		return
	}

	// 检查缓存是否过期（24小时）
	if time.Since(cache.SavedAt) > 24*time.Hour {
		fs.Debugf(f, "🔄 父目录缓存已过期，跳过加载")
		return
	}

	// 过滤过期的条目（5分钟）
	validCount := 0
	f.cacheMu.Lock()
	for dirID, verifyTime := range cache.Data {
		if time.Since(verifyTime) <= 5*time.Minute {
			f.parentDirCache[dirID] = verifyTime
			validCount++
		}
	}
	f.cacheMu.Unlock()

	fs.Debugf(f, "📁 从持久化缓存加载 %d 个有效父目录条目", validCount)
}

// loadListFileCache 加载ListFile缓存
func (f *Fs) loadListFileCache() {
	if f.listFileCacheFile == "" {
		return
	}

	data, err := os.ReadFile(f.listFileCacheFile)
	if err != nil {
		if !os.IsNotExist(err) {
			fs.Debugf(f, "⚠️ 读取ListFile缓存失败: %v", err)
		}
		return
	}

	var cache PersistentListFileCache
	if err := json.Unmarshal(data, &cache); err != nil {
		fs.Debugf(f, "⚠️ 解析ListFile缓存失败: %v", err)
		return
	}

	// 检查缓存是否过期（24小时）
	if time.Since(cache.SavedAt) > 24*time.Hour {
		fs.Debugf(f, "🔄 ListFile缓存已过期，跳过加载")
		return
	}

	// 过滤过期的条目（5分钟）
	validCount := 0
	for key, cachedResp := range cache.Data {
		if time.Since(cachedResp.CachedAt) <= 5*time.Minute {
			f.listFileCache.Put(key, cachedResp.Response)
			validCount++
		}
	}

	fs.Debugf(f, "📋 从持久化缓存加载 %d 个有效ListFile条目", validCount)
}

// saveParentDirCache 保存父目录缓存到磁盘
func (f *Fs) saveParentDirCache() {
	if f.parentDirCacheFile == "" {
		return
	}

	f.cacheMu.RLock()
	data := make(map[int64]time.Time)
	for k, v := range f.parentDirCache {
		data[k] = v
	}
	f.cacheMu.RUnlock()

	cache := PersistentParentDirCache{
		Data:    data,
		SavedAt: time.Now(),
		Version: "1.0",
	}

	jsonData, err := json.Marshal(cache)
	if err != nil {
		fs.Debugf(f, "⚠️ 序列化父目录缓存失败: %v", err)
		return
	}

	if err := os.WriteFile(f.parentDirCacheFile, jsonData, 0644); err != nil {
		fs.Debugf(f, "⚠️ 保存父目录缓存失败: %v", err)
		return
	}

	fs.Debugf(f, "💾 父目录缓存已保存: %d 个条目", len(data))
}

// saveListFileCacheEntry 保存单个ListFile缓存条目
func (f *Fs) saveListFileCacheEntry(key string, response *ListResponse) {
	if f.listFileCacheFile == "" {
		return
	}

	// 读取现有缓存
	var cache PersistentListFileCache
	if data, err := os.ReadFile(f.listFileCacheFile); err == nil {
		json.Unmarshal(data, &cache)
	}

	// 初始化数据结构
	if cache.Data == nil {
		cache.Data = make(map[string]*CachedListResponse)
	}

	// 添加新条目
	cache.Data[key] = &CachedListResponse{
		Response:  response,
		CachedAt:  time.Now(),
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	cache.SavedAt = time.Now()
	cache.Version = "1.0"

	// 清理过期条目
	for k, v := range cache.Data {
		if time.Since(v.CachedAt) > 5*time.Minute {
			delete(cache.Data, k)
		}
	}

	// 保存到磁盘
	if jsonData, err := json.Marshal(cache); err == nil {
		os.WriteFile(f.listFileCacheFile, jsonData, 0644)
		fs.Debugf(f, "💾 ListFile缓存条目已保存: %s", key)
	}
}

// 🧹 缓存管理方法

// clearPersistentCache 清理持久化缓存
func (f *Fs) clearPersistentCache() error {
	if f.persistentCacheDir == "" {
		return nil
	}

	// 删除整个缓存目录
	if err := os.RemoveAll(f.persistentCacheDir); err != nil {
		fs.Debugf(f, "⚠️ 清理持久化缓存失败: %v", err)
		return err
	}

	// 重新创建缓存目录
	if err := os.MkdirAll(f.persistentCacheDir, 0755); err != nil {
		fs.Debugf(f, "⚠️ 重新创建缓存目录失败: %v", err)
		return err
	}

	fs.Debugf(f, "🧹 持久化缓存已清理: %s", f.persistentCacheDir)
	return nil
}

// getCacheStats 获取缓存统计信息
func (f *Fs) getCacheStats() map[string]interface{} {
	stats := make(map[string]interface{})

	if f.persistentCacheDir == "" {
		return stats
	}

	// 统计缓存文件大小
	var totalSize int64
	fileCount := 0

	filepath.Walk(f.persistentCacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			totalSize += info.Size()
			fileCount++
		}
		return nil
	})

	stats["cache_dir"] = f.persistentCacheDir
	stats["total_size"] = totalSize
	stats["file_count"] = fileCount
	stats["size_mb"] = float64(totalSize) / 1024 / 1024

	// 检查各个缓存文件
	if info, err := os.Stat(f.listFileCacheFile); err == nil {
		stats["list_file_cache_size"] = info.Size()
		stats["list_file_cache_modified"] = info.ModTime()
	}

	if info, err := os.Stat(f.parentDirCacheFile); err == nil {
		stats["parent_dir_cache_size"] = info.Size()
		stats["parent_dir_cache_modified"] = info.ModTime()
	}

	return stats
}

// clearCacheCommand 清理缓存命令
func (f *Fs) clearCacheCommand(ctx context.Context, args []string, opt map[string]string) (any, error) {
	result := make(map[string]interface{})

	// 清理内存缓存
	f.listFileCache.Clear()
	f.cacheMu.Lock()
	f.parentDirCache = make(map[int64]time.Time)
	f.cacheMu.Unlock()

	result["memory_cache_cleared"] = true

	// 清理持久化缓存
	if err := f.clearPersistentCache(); err != nil {
		result["persistent_cache_error"] = err.Error()
		return result, err
	}

	result["persistent_cache_cleared"] = true
	result["cache_dir"] = f.persistentCacheDir

	fs.Infof(f, "🧹 所有缓存已清理完成")
	return result, nil
}

// cacheStatsCommand 缓存统计命令
func (f *Fs) cacheStatsCommand(ctx context.Context, args []string, opt map[string]string) (any, error) {
	stats := f.getCacheStats()

	// 添加内存缓存统计
	f.cacheMu.RLock()
	stats["memory_parent_dir_cache_count"] = len(f.parentDirCache)
	f.cacheMu.RUnlock()

	// 添加ListFile内存缓存统计（估算）
	listFileCacheCount := 0
	// 注意：cache.Cache没有直接的计数方法，这里只是占位
	stats["memory_list_file_cache_count"] = listFileCacheCount

	return stats, nil
}

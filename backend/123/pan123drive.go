package _123

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"golang.org/x/oauth2"

	// 123网盘SDK
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
)

// "Mozilla/5.0 AppleWebKit/600 Safari/600 Chrome/124.0.0.0 Edg/124.0.0.0"
const (
	openAPIRootURL     = "https://open-api.123pan.com"
	defaultUserAgent   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	tokenRefreshWindow = 10 * time.Minute // tokenRefreshWindow 定义了在令牌过期前多长时间尝试刷新令牌

	// 基于123网盘官方限制的API速率限制
	// 不同的API有不同的QPS限制，因此我们使用多个调速器
	listAPIMinSleep   = 334 * time.Millisecond  // ~3 QPS 用于 api/v2/file/list
	strictAPIMinSleep = 1000 * time.Millisecond // 1 QPS 用于 user/info, move, delete
	uploadAPIMinSleep = 500 * time.Millisecond  // 2 QPS 用于上传API
	maxSleep          = 30 * time.Second        // 429退避的最大睡眠时间
	decayConstant     = 2

	// 文件上传相关常量
	singleStepUploadLimit = 1024 * 1024 * 1024 // 1GB - 单步上传API的文件大小限制
	maxMemoryBufferSize   = 512 * 1024 * 1024  // 512MB - 内存缓冲的最大大小，防止内存不足
	maxFileNameBytes      = 255                // 文件名的最大字节长度（UTF-8编码）

	// 文件冲突处理策略
	duplicateHandleKeepBoth  = 1 // 保留两个文件，新文件添加后缀
	duplicateHandleOverwrite = 2 // 覆盖原文件

	// 上传相关常量
	defaultChunkSize    = 5 * fs.Mebi
	minChunkSize        = 100 * fs.Kibi
	maxChunkSize        = 5 * fs.Gibi
	defaultUploadCutoff = 200 * fs.Mebi
	maxUploadParts      = 10000

	// 连接和超时设置 - 优化后的性能参数
	defaultConnTimeout = 10 * time.Second  // 减少连接超时，快速失败
	defaultTimeout     = 120 * time.Second // 减少总体超时，避免长时间等待

	// 重试和退避设置
	maxRetries      = 5                // 最大重试次数
	baseRetryDelay  = 1 * time.Second  // 基础重试延迟
	maxRetryDelay   = 30 * time.Second // 最大重试延迟
	retryMultiplier = 2.0              // 指数退避倍数

	// 文件名验证相关常量
	maxFileNameLength = 256          // 123网盘文件名最大长度（包括扩展名）
	invalidChars      = `"\/:*?|><\` // 123网盘不允许的文件名字符
	replacementChar   = "_"          // 用于替换非法字符的安全字符
)

// Options 定义此后端的配置选项
type Options struct {
	ClientID          string        `config:"client_id"`
	ClientSecret      string        `config:"client_secret"`
	Token             string        `config:"token"`
	UserAgent         string        `config:"user_agent"`
	RootFolderID      string        `config:"root_folder_id"`
	ListChunk         int           `config:"list_chunk"`
	PacerMinSleep     fs.Duration   `config:"pacer_min_sleep"`
	ListPacerMinSleep fs.Duration   `config:"list_pacer_min_sleep"`
	ConnTimeout       fs.Duration   `config:"conn_timeout"`
	Timeout           fs.Duration   `config:"timeout"`
	ChunkSize         fs.SizeSuffix `config:"chunk_size"`
	UploadCutoff      fs.SizeSuffix `config:"upload_cutoff"`
	MaxUploadParts    int           `config:"max_upload_parts"`

	// 性能优化相关选项
	MaxConcurrentUploads   int         `config:"max_concurrent_uploads"`
	MaxConcurrentDownloads int         `config:"max_concurrent_downloads"`
	ProgressUpdateInterval fs.Duration `config:"progress_update_interval"`
	EnableProgressDisplay  bool        `config:"enable_progress_display"`
}

// Fs 表示远程123网盘驱动器实例
type Fs struct {
	name         string       // 此远程实例的名称
	originalName string       // 未修改的原始配置名称
	root         string       // 当前操作的根路径
	opt          Options      // 解析后的配置选项
	features     *fs.Features // 可选功能特性

	// API客户端和身份验证相关
	client       *http.Client     // 用于API调用的HTTP客户端
	token        string           // 缓存的访问令牌
	tokenExpiry  time.Time        // 缓存访问令牌的过期时间
	tokenMu      sync.Mutex       // 保护token和tokenExpiry的互斥锁
	tokenRenewer *oauthutil.Renew // 令牌自动更新器

	// 配置和状态信息
	m            configmap.Mapper // 配置映射器
	rootFolderID string           // 根文件夹的ID
	userID       string           // 来自API的用户ID

	// 性能和速率限制 - 针对不同API限制的多个调速器
	listPacer   *fs.Pacer // 文件列表API的调速器（约3 QPS）
	strictPacer *fs.Pacer // 严格API（如user/info, move, delete）的调速器（1 QPS）
	uploadPacer *fs.Pacer // 上传API的调速器（2 QPS）

	// 目录缓存
	dirCache *dircache.DirCache

	// 性能优化相关
	uploadSemaphore   chan struct{}    // 上传并发控制信号量
	downloadSemaphore chan struct{}    // 下载并发控制信号量
	progressTracker   *ProgressTracker // 进度跟踪器
	progressMu        sync.Mutex       // 保护分片上传进度的互斥锁
}

// ProgressTracker 用于跟踪上传/下载进度
type ProgressTracker struct {
	mu             sync.RWMutex
	operations     map[string]*OperationProgress // 操作ID -> 进度信息
	updateInterval time.Duration                 // 进度更新间隔
	enableDisplay  bool                          // 是否启用进度显示
}

// OperationProgress 表示单个操作的进度信息
type OperationProgress struct {
	OperationID     string        // 操作唯一标识
	FileName        string        // 文件名
	TotalSize       int64         // 总大小
	TransferredSize int64         // 已传输大小
	StartTime       time.Time     // 开始时间
	LastUpdate      time.Time     // 最后更新时间
	Speed           float64       // 传输速度 (bytes/second)
	ETA             time.Duration // 预计剩余时间
	Status          string        // 状态：uploading, downloading, completed, failed
}

// ProgressReadCloser 包装ReadCloser以提供进度跟踪和资源管理
type ProgressReadCloser struct {
	io.ReadCloser
	fs              *Fs
	operationID     string
	totalSize       int64
	transferredSize int64
}

// Read 实现io.Reader接口，同时更新进度
func (prc *ProgressReadCloser) Read(p []byte) (n int, err error) {
	n, err = prc.ReadCloser.Read(p)
	if n > 0 {
		prc.transferredSize += int64(n)
		// 更新进度
		prc.fs.progressTracker.UpdateProgress(prc.operationID, prc.transferredSize)
	}
	return n, err
}

// Close 实现io.Closer接口，同时清理资源
func (prc *ProgressReadCloser) Close() error {
	// 释放下载信号量
	<-prc.fs.downloadSemaphore

	// 完成进度跟踪
	success := prc.transferredSize == prc.totalSize
	prc.fs.progressTracker.CompleteOperation(prc.operationID, success)

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
	createTime  time.Time // 创建时间
	isDir       bool      // 是否为目录
}

// 文件名验证相关的全局变量
var (
	// invalidCharsRegex 用于匹配123网盘不允许的文件名字符的正则表达式
	invalidCharsRegex = regexp.MustCompile(`["\/:*?|><\\]`)
)

// validateFileName 验证文件名是否符合123网盘的限制规则
// 返回验证结果和详细的错误信息
func validateFileName(name string) error {
	if name == "" {
		return fmt.Errorf("文件名不能为空")
	}

	// 检查文件名长度（使用UTF-8字节长度）
	if utf8.RuneCountInString(name) > maxFileNameLength {
		return fmt.Errorf("文件名长度超过限制：当前%d个字符，最大允许%d个字符",
			utf8.RuneCountInString(name), maxFileNameLength)
	}

	// 检查是否包含非法字符
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
			if maxNameLength < 1 {
				maxNameLength = 1
			}
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
// 如果文件名已存在，会添加数字后缀
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
			maxBaseLength := maxFileNameBytes - len([]byte(fmt.Sprintf(" (%d)%s", i, ext)))
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

// normalizePath 规范化路径，处理123网盘路径的特殊要求
// 主要解决以斜杠结尾的路径问题，确保符合dircache的要求
// dircache要求：路径不应该以/开头或结尾
func normalizePath(path string) string {
	if path == "" {
		return ""
	}

	// 移除开头的斜杠（如果有）
	path = strings.TrimPrefix(path, "/")

	// 移除结尾的斜杠（如果有）
	path = strings.TrimSuffix(path, "/")

	// 处理连续的斜杠，替换为单个斜杠
	path = regexp.MustCompile(`/+`).ReplaceAllString(path, "/")

	// 移除空的路径组件
	parts := strings.Split(path, "/")
	var cleanParts []string
	for _, part := range parts {
		// 只保留非空的部分
		if part != "" && part != "." {
			cleanParts = append(cleanParts, part)
		}
	}

	// 重新组合路径
	if len(cleanParts) == 0 {
		return ""
	}

	result := strings.Join(cleanParts, "/")

	// 最终检查：确保结果不以/开头或结尾
	result = strings.Trim(result, "/")

	return result
}

// validateAndNormalizePath 验证并规范化路径，同时清理路径中的文件名
func validateAndNormalizePath(path string) (normalizedPath string, warnings []error) {
	// 首先规范化路径
	normalizedPath = normalizePath(path)

	if normalizedPath == "" {
		return "", nil
	}

	// 分割路径并验证每个组件
	parts := strings.Split(normalizedPath, "/")
	var cleanParts []string

	for _, part := range parts {
		if part == "" {
			continue
		}

		// 验证并清理每个路径组件
		cleanedPart, warning := validateAndCleanFileName(part)
		if warning != nil {
			warnings = append(warnings, fmt.Errorf("路径组件 '%s': %v", part, warning))
		}
		cleanParts = append(cleanParts, cleanedPart)
	}

	// 重新组合清理后的路径
	if len(cleanParts) == 0 {
		return "", warnings
	}

	normalizedPath = strings.Join(cleanParts, "/")
	return normalizedPath, warnings
}

// isRemoteSource 检查源对象是否来自远程云盘（非本地文件）
func (f *Fs) isRemoteSource(src fs.ObjectInfo) bool {
	// 检查源对象的类型，如果不是本地文件系统，则认为是远程源
	// 这可以通过检查源对象的Fs()方法返回的类型来判断
	srcFs := src.Fs()
	if srcFs == nil {
		return false
	}

	// 检查是否是本地文件系统
	fsType := srcFs.Name()
	return fsType != "local" && fsType != ""
}

// init 函数用于向Fs注册123网盘后端
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "pan123drive",
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
			Name:     "list_chunk",
			Help:     "每个列表请求中获取的文件数量。",
			Default:  100,
			Advanced: true,
		}, {
			Name:     "pacer_min_sleep",
			Help:     "严格API调用（user/info, move, delete）之间的最小等待时间。",
			Default:  strictAPIMinSleep,
			Advanced: true,
		}, {
			Name:     "list_pacer_min_sleep",
			Help:     "文件列表API调用之间的最小等待时间。",
			Default:  listAPIMinSleep,
			Advanced: true,
		}, {
			Name:     "conn_timeout",
			Help:     "API请求的连接超时时间。",
			Default:  defaultConnTimeout,
			Advanced: true,
		}, {
			Name:     "timeout",
			Help:     "API请求的整体超时时间。",
			Default:  defaultTimeout,
			Advanced: true,
		}, {
			Name:     "chunk_size",
			Help:     "多部分上传的块大小。",
			Default:  defaultChunkSize,
			Advanced: true,
		}, {
			Name:     "upload_cutoff",
			Help:     "切换到多部分上传的阈值。",
			Default:  defaultUploadCutoff,
			Advanced: true,
		}, {
			Name:     "max_upload_parts",
			Help:     "多部分上传中的最大分片数。",
			Default:  maxUploadParts,
			Advanced: true,
		}, {
			Name:     "max_concurrent_uploads",
			Help:     "最大并发上传数量。",
			Default:  4,
			Advanced: true,
		}, {
			Name:     "max_concurrent_downloads",
			Help:     "最大并发下载数量。",
			Default:  4,
			Advanced: true,
		}, {
			Name:     "progress_update_interval",
			Help:     "进度更新间隔。",
			Default:  fs.Duration(5 * time.Second),
			Advanced: true,
		}, {
			Name:     "enable_progress_display",
			Help:     "是否启用进度显示。",
			Default:  true,
			Advanced: true,
		}},
	})
}

var commandHelp = []fs.CommandHelp{{
	Name:  "getdownloadurlua",
	Short: "通过文件路径获取下载URL",
	Long: `此命令使用文件路径检索文件的下载URL。
用法:
rclone backend getdownloadurlau 123:path/to/file VidHub/1.7.24
该命令返回指定文件的下载URL。请确保文件路径正确。`,
},
}

// Command 执行后端特定的命令。
func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (out any, err error) {
	switch name {

	case "getdownloadurlua":
		path := ""
		ua := ""
		if len(arg) > 0 {
			ua = arg[0]
		}
		return f.getDownloadURLByUA(ctx, path, ua)
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

	// 使用apiCall进行适当的速率限制和错误处理
	var result ListResponse
	err := f.apiCall(ctx, "GET", endpoint, nil, &result)
	if err != nil {
		fs.Debugf(f, "ListFile API调用失败: %v", err)
		return nil, err
	}

	fs.Debugf(f, "ListFile API响应: code=%d, message=%s, fileCount=%d", result.Code, result.Message, len(result.Data.FileList))
	return &result, nil
}

func (f *Fs) pathToFileID(ctx context.Context, filePath string) (string, error) {
	// 根目录
	if filePath == "/" {
		return "0", nil
	}
	if len(filePath) > 1 && strings.HasSuffix(filePath, "/") {
		filePath = filePath[:len(filePath)-1]
	}
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
		next := "0" // 根据API使用字符串作为next
		for {
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

// DataContainer 保存'list'数组
type DataContainer struct {
	List []FileInfo `json:"list"`
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

type Payload struct {
	S3KeyFlag string `json:"s3KeyFlag"`
	FileName  string `json:"fileName"`
	Etag      string `json:"etag"`
	Size      int64  `json:"size"`
	FileID    int64  `json:"fileId"`
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

type UploadUrlResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		PresignedURL string `json:"presignedURL"`
	} `json:"data"`
}

// UploadProgress 跟踪多部分上传的进度，支持断点续传
type UploadProgress struct {
	PreuploadID   string         `json:"preuploadID"`
	TotalParts    int64          `json:"totalParts"`
	ChunkSize     int64          `json:"chunkSize"`
	FileSize      int64          `json:"fileSize"`
	UploadedParts map[int64]bool `json:"uploadedParts"` // partNumber -> uploaded
	FilePath      string         `json:"filePath"`      // local file path for resume
	MD5Hash       string         `json:"md5Hash"`       // file MD5 for verification
	CreatedAt     time.Time      `json:"createdAt"`
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
		SpaceTempExpr  string `json:"spaceTempExpr"`
		Vip            bool   `json:"vip"`
		DirectTraffic  int64  `json:"directTraffic"`
		IsHideUID      bool   `json:"isHideUID"`
	} `json:"data"`
}

func (f *Fs) getDownloadURLByUA(ctx context.Context, filePath string, UA string) (string, error) {
	// Ensure token is valid before making API calls
	err := f.refreshTokenIfNecessary(ctx, false, false)
	if err != nil {
		return "", err
	}

	if UA == "" {
		UA = f.opt.UserAgent
	}

	if filePath == "" {
		filePath = f.root
	}

	fileID, err := f.pathToFileID(ctx, filePath)
	if err != nil {
		return "", fmt.Errorf("failed to get file ID for path %q: %w", filePath, err)
	}

	// Use the standard getDownloadURL method
	return f.getDownloadURL(ctx, fileID)
}

// shouldRetry 根据响应和错误确定是否重试API调用
// 使用智能的指数退避策略
func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}

	// 网络错误时重试
	if err != nil {
		if fserrors.ShouldRetry(err) {
			// 使用基础重试延迟
			return true, fserrors.NewErrorRetryAfter(baseRetryDelay)
		}
		return false, err
	}

	// 检查HTTP状态码
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			// 速率受限 - 使用较长的退避时间
			retryAfter := calculateRetryDelay(3) // 第3次重试的延迟
			fs.Debugf(nil, "速率受限，将在%v后重试", retryAfter)
			return true, fserrors.NewErrorRetryAfter(retryAfter)
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			// 服务器错误 - 使用中等退避时间
			retryAfter := calculateRetryDelay(2) // 第2次重试的延迟
			fs.Debugf(nil, "服务器错误 %d，将在%v后重试", resp.StatusCode, retryAfter)
			return true, fserrors.NewErrorRetryAfter(retryAfter)
		case http.StatusUnauthorized:
			// 令牌可能已过期 - 短暂延迟后让调用者处理令牌刷新
			return false, fserrors.NewErrorRetryAfter(baseRetryDelay)
		}
	}

	return false, err
}

// calculateRetryDelay 计算指数退避的重试延迟
func calculateRetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return baseRetryDelay
	}

	delay := time.Duration(float64(baseRetryDelay) * math.Pow(retryMultiplier, float64(attempt-1)))
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}

	// 添加随机抖动以避免雷群效应
	jitter := time.Duration(rand.Float64() * float64(delay) * 0.1) // 10%的抖动
	return delay + jitter
}

// errorHandler 处理API错误并将其转换为适当的rclone错误
func errorHandler(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read error response: %w", err)
	}

	// 尝试解析为123网盘API错误响应
	var apiError struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	if err := json.Unmarshal(body, &apiError); err == nil && apiError.Code != 0 {
		switch apiError.Code {
		case 401:
			return fmt.Errorf("authentication failed: %s", apiError.Message)
		case 403:
			return fmt.Errorf("permission denied: %s", apiError.Message)
		case 404:
			return fs.ErrorObjectNotFound
		case 429:
			return fserrors.NewErrorRetryAfter(30 * time.Second)
		default:
			return fmt.Errorf("API error %d: %s", apiError.Code, apiError.Message)
		}
	}

	// 回退到HTTP状态码
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("authentication failed: HTTP %d", resp.StatusCode)
	case http.StatusForbidden:
		return fmt.Errorf("permission denied: HTTP %d", resp.StatusCode)
	case http.StatusNotFound:
		return fs.ErrorObjectNotFound
	case http.StatusTooManyRequests:
		return fserrors.NewErrorRetryAfter(30 * time.Second)
	default:
		return fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(body))
	}
}

// getPacerForEndpoint 根据API端点返回适当的调速器
func (f *Fs) getPacerForEndpoint(endpoint string) *fs.Pacer {
	switch {
	case strings.Contains(endpoint, "/api/v1/user/info"):
		return f.strictPacer // 1 QPS
	case strings.Contains(endpoint, "/api/v1/file/move"):
		return f.strictPacer // 1 QPS
	case strings.Contains(endpoint, "/api/v1/file/trash"):
		return f.strictPacer // 1 QPS (delete)
	case strings.Contains(endpoint, "/api/v2/file/list"):
		return f.listPacer // 3 QPS
	case strings.Contains(endpoint, "/api/v1/file/list"):
		return f.listPacer // 4 QPS, but we use the same pacer
	case strings.Contains(endpoint, "/upload/v1/file/"), strings.Contains(endpoint, "/upload/v2/file/"):
		return f.uploadPacer // 2 QPS
	case strings.Contains(endpoint, "/api/v1/access_token"):
		return f.strictPacer // 1 QPS
	default:
		// 为了安全起见，未知端点默认使用严格调速器
		return f.strictPacer
	}
}

// apiCall 进行API调用，具有适当的错误处理和速率限制
func (f *Fs) apiCall(ctx context.Context, method, endpoint string, body io.Reader, result interface{}) error {
	pacer := f.getPacerForEndpoint(endpoint)
	return pacer.Call(func() (bool, error) {
		// 确保令牌在进行API调用前有效
		err := f.refreshTokenIfNecessary(ctx, false, false)
		if err != nil {
			return false, err
		}

		req, err := http.NewRequestWithContext(ctx, method, openAPIRootURL+endpoint, body)
		if err != nil {
			return false, err
		}

		// 设置请求头
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Platform", "open_platform")
		req.Header.Set("User-Agent", f.opt.UserAgent)

		// 为请求添加超时
		if f.opt.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(f.opt.Timeout))
			defer cancel()
			req = req.WithContext(ctx)
		}

		resp, err := f.client.Do(req)
		if err != nil {
			// 检查是否应该重试
			if shouldRetry, retryErr := shouldRetry(ctx, resp, err); shouldRetry {
				return true, retryErr
			}
			return false, err
		}
		defer resp.Body.Close()

		// 首先检查HTTP错误
		if resp.StatusCode >= 400 {
			return f.handleHTTPError(ctx, resp)
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, err
		}

		// 解析基础响应以检查错误
		var baseResp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err = json.Unmarshal(bodyBytes, &baseResp); err != nil {
			return false, err
		}

		// 处理API错误
		switch baseResp.Code {
		case 0:
			// 成功
			if result != nil {
				return false, json.Unmarshal(bodyBytes, result)
			}
			return false, nil
		case 401:
			// 令牌过期，尝试刷新
			fs.Debugf(f, "令牌已过期，尝试刷新")
			err = f.refreshTokenIfNecessary(ctx, false, true)
			if err != nil {
				return false, err
			}
			return true, nil // 使用新令牌重试
		case 429:
			// 速率限制 - 对429错误使用更长的退避时间
			fs.Debugf(f, "速率受限（API错误429），将使用更长退避时间重试")
			return true, fserrors.NewErrorRetryAfter(30 * time.Second)
		default:
			return false, fmt.Errorf("API error %d: %s", baseResp.Code, baseResp.Message)
		}
	})
}

// handleHTTPError 处理HTTP级别的错误并确定重试行为
func (f *Fs) handleHTTPError(ctx context.Context, resp *http.Response) (bool, error) {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		// 令牌可能已过期 - 尝试刷新一次
		fs.Debugf(f, "收到401错误，尝试刷新令牌")
		err := f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			return false, fmt.Errorf("身份验证失败: %w", err)
		}
		return true, nil // 使用新令牌重试
	case http.StatusTooManyRequests:
		// 速率受限 - 应该使用更长退避时间重试
		fs.Debugf(f, "速率受限（HTTP 429），将使用更长退避时间重试")
		return true, fserrors.NewErrorRetryAfter(30 * time.Second)
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		// 服务器错误 - 应该重试
		fs.Debugf(f, "服务器错误 %d，将重试", resp.StatusCode)
		return true, fmt.Errorf("server error: HTTP %d", resp.StatusCode)
	case http.StatusNotFound:
		return false, fs.ErrorObjectNotFound
	case http.StatusForbidden:
		return false, fmt.Errorf("permission denied: HTTP %d", resp.StatusCode)
	default:
		// 读取错误响应体以获取更多详细信息
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(body))
	}
}

// getFileInfo 根据ID获取文件的详细信息
func (f *Fs) getFileInfo(ctx context.Context, fileID string) (*FileListInfoRespDataV2, error) {
	// 验证文件ID
	_, err := strconv.ParseInt(fileID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid file ID: %s", fileID)
	}

	// 使用文件详情API
	var response FileDetailResponse
	endpoint := fmt.Sprintf("/api/v1/file/detail?fileID=%s", fileID)

	err = f.apiCall(ctx, "GET", endpoint, nil, &response)
	if err != nil {
		return nil, err
	}

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

	return fileInfo, nil
}

// getDownloadURL 获取文件的下载URL
func (f *Fs) getDownloadURL(ctx context.Context, fileID string) (string, error) {
	var response DownloadInfoResponse
	endpoint := fmt.Sprintf("/api/v1/file/download_info?fileID=%s", fileID)

	err := f.apiCall(ctx, "GET", endpoint, nil, &response)
	if err != nil {
		return "", err
	}

	if response.Code != 0 {
		return "", fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return response.Data.DownloadURL, nil
}

// createDirectory 创建新目录
func (f *Fs) createDirectory(ctx context.Context, parentID, name string) error {
	// 验证参数
	if name == "" {
		return fmt.Errorf("目录名不能为空")
	}
	if parentID == "" {
		return fmt.Errorf("父目录ID不能为空")
	}

	// 验证目录名
	if err := validateFileName(name); err != nil {
		return fmt.Errorf("目录名验证失败: %w", err)
	}

	// 将父目录ID转换为int64
	parentFileID, err := strconv.ParseInt(parentID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid parent ID: %s", parentID)
	}

	// 准备请求体
	reqBody := map[string]interface{}{
		"parentID": strconv.FormatInt(parentFileID, 10),
		"name":     name,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	// 进行API调用
	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err = f.apiCall(ctx, "POST", "/upload/v1/file/mkdir", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return err
	}

	if response.Code != 0 {
		return fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return nil
}

// createUpload 创建上传会话
func (f *Fs) createUpload(ctx context.Context, parentFileID int64, filename, etag string, size int64) (*UploadCreateResp, error) {
	reqBody := map[string]interface{}{
		"parentFileId": parentFileID,
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

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	var response UploadCreateResp

	// 首先尝试v2 API（更新的API）
	err = f.apiCall(ctx, "POST", "/upload/v2/file/create", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		fs.Debugf(f, "v2 API失败，尝试v1 API: %v", err)

		// 回退到v1 API（使用相同的请求体，因为两个API都期望etag字段）
		fs.Debugf(f, "v1 API请求体: %+v", reqBody)
		err = f.apiCall(ctx, "POST", "/upload/v1/file/create", bytes.NewReader(bodyBytes), &response)
		if err != nil {
			return nil, fmt.Errorf("v2和v1 API都失败了: %w", err)
		}
		fs.Debugf(f, "v1 API作为回退成功")
	} else {
		fs.Debugf(f, "v2 API成功")
	}

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

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
		chunkSize = int64(f.opt.ChunkSize)
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

	// 大文件的多部分上传，支持断点续传
	return f.uploadMultiPartWithResume(ctx, in, createResp.Data.PreuploadID, size, chunkSize, filePath)
}

// uploadSinglePart 单部分上传文件
func (f *Fs) uploadSinglePart(ctx context.Context, preuploadID string, data []byte) error {
	// 获取第1部分的上传URL
	uploadURL, err := f.getUploadURL(ctx, preuploadID, 1)
	if err != nil {
		return fmt.Errorf("failed to get upload URL: %w", err)
	}

	// 上传数据
	err = f.uploadPart(ctx, uploadURL, data)
	if err != nil {
		return fmt.Errorf("failed to upload part: %w", err)
	}

	// 完成上传
	return f.completeUpload(ctx, preuploadID)
}

// uploadMultiPart 多部分上传文件，支持断点续传
func (f *Fs) uploadMultiPart(ctx context.Context, in io.Reader, preuploadID string, size, chunkSize int64) error {
	return f.uploadMultiPartWithResume(ctx, in, preuploadID, size, chunkSize, "")
}

// uploadMultiPartWithResume 多部分上传文件，支持断点续传
func (f *Fs) uploadMultiPartWithResume(ctx context.Context, in io.Reader, preuploadID string, size, chunkSize int64, filePath string) error {
	// 防止除零错误
	if chunkSize <= 0 {
		return fmt.Errorf("无效的分片大小: %d", chunkSize)
	}

	uploadNums := (size + chunkSize - 1) / chunkSize

	// 限制分片数量
	if uploadNums > int64(f.opt.MaxUploadParts) {
		return fmt.Errorf("file too large: would require %d parts, max allowed is %d", uploadNums, f.opt.MaxUploadParts)
	}

	// 尝试加载现有进度
	progress, err := f.loadUploadProgress(preuploadID)
	if err != nil {
		fs.Debugf(f, "加载上传进度失败: %v", err)
		progress = nil
	}

	// 如果不存在进度或参数不匹配，创建新进度
	if progress == nil || progress.TotalParts != uploadNums || progress.ChunkSize != chunkSize || progress.FileSize != size {
		fs.Debugf(f, "为%s创建新的上传进度", preuploadID)
		progress = &UploadProgress{
			PreuploadID:   preuploadID,
			TotalParts:    uploadNums,
			ChunkSize:     chunkSize,
			FileSize:      size,
			UploadedParts: make(map[int64]bool),
			FilePath:      filePath,
			CreatedAt:     time.Now(),
		}
	} else {
		fs.Debugf(f, "恢复上传%s: %d/%d个分片已上传",
			preuploadID, len(progress.UploadedParts), progress.TotalParts)
	}

	// 检查是否可以从文件恢复（如果提供了文件路径且文件存在）
	var fileReader *os.File
	canResumeFromFile := filePath != "" && len(progress.UploadedParts) > 0

	if canResumeFromFile {
		fileReader, err = os.Open(filePath)
		if err != nil {
			fs.Debugf(f, "无法打开文件进行断点续传，回退到流式传输: %v", err)
			canResumeFromFile = false
		} else {
			defer fileReader.Close()
			// 使用文件读取器而不是流以支持断点续传
			in = fileReader
		}
	}

	buffer := make([]byte, chunkSize)

	for partIndex := int64(0); partIndex < uploadNums; partIndex++ {
		partNumber := partIndex + 1

		// 如果此分片已上传则跳过
		if progress.UploadedParts[partNumber] {
			fs.Debugf(f, "跳过已上传的分片 %d/%d", partNumber, uploadNums)

			// 如果可以定位（文件读取器），定位到下一分片
			if canResumeFromFile && fileReader != nil {
				nextOffset := (partIndex + 1) * chunkSize
				if nextOffset < size {
					_, err := fileReader.Seek(nextOffset, io.SeekStart)
					if err != nil {
						return fmt.Errorf("failed to seek to part %d: %w", partNumber+1, err)
					}
				}
			} else {
				// 仍需要读取并丢弃数据以推进读取器
				if partIndex == uploadNums-1 {
					remaining := size - partIndex*chunkSize
					_, err := io.ReadFull(in, make([]byte, remaining))
					if err != nil {
						return fmt.Errorf("failed to skip part %d data: %w", partNumber, err)
					}
				} else {
					_, err := io.ReadFull(in, buffer)
					if err != nil {
						return fmt.Errorf("failed to skip part %d data: %w", partNumber, err)
					}
				}
			}
			continue
		}

		// 如果可以定位且这不是第一个未上传的分片，定位到正确位置
		if canResumeFromFile && fileReader != nil {
			currentOffset := partIndex * chunkSize
			_, err := fileReader.Seek(currentOffset, io.SeekStart)
			if err != nil {
				return fmt.Errorf("failed to seek to part %d: %w", partNumber, err)
			}
		}

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

		// 获取此分片的上传URL
		uploadURL, err := f.getUploadURL(ctx, preuploadID, partNumber)
		if err != nil {
			return fmt.Errorf("failed to get upload URL for part %d: %w", partNumber, err)
		}

		// 使用重试上传此分片
		err = f.uploadPacer.Call(func() (bool, error) {
			err := f.uploadPart(ctx, uploadURL, partData)
			return shouldRetry(ctx, nil, err)
		})
		if err != nil {
			// 返回错误前保存进度
			saveErr := f.saveUploadProgress(progress)
			if saveErr != nil {
				fs.Debugf(f, "上传错误后保存进度失败: %v", saveErr)
			}
			return fmt.Errorf("failed to upload part %d: %w", partNumber, err)
		}

		// 标记此分片已上传并保存进度
		progress.UploadedParts[partNumber] = true
		err = f.saveUploadProgress(progress)
		if err != nil {
			fs.Debugf(f, "保存上传进度失败: %v", err)
		}

		fs.Debugf(f, "已上传分片 %d/%d", partNumber, uploadNums)
	}

	// 完成上传
	err = f.completeUpload(ctx, preuploadID)
	if err != nil {
		// 返回错误前保存进度
		saveErr := f.saveUploadProgress(progress)
		if saveErr != nil {
			fs.Debugf(f, "完成错误后保存进度失败: %v", saveErr)
		}
		return err
	}

	// 上传成功完成，删除进度文件
	f.removeUploadProgress(preuploadID)
	return nil
}

// getUploadURL 获取特定分片的上传URL
func (f *Fs) getUploadURL(ctx context.Context, preuploadID string, sliceNo int64) (string, error) {
	reqBody := map[string]interface{}{
		"preuploadId": preuploadID,
		"sliceNo":     sliceNo,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	var response UploadUrlResp
	// 首先尝试v2 API
	err = f.apiCall(ctx, "POST", "/upload/v2/file/get_upload_url", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		fs.Debugf(f, "v2 get_upload_url失败，尝试v1: %v", err)
		// 回退到v1 API
		err = f.apiCall(ctx, "POST", "/upload/v1/file/get_upload_url", bytes.NewReader(bodyBytes), &response)
		if err != nil {
			return "", fmt.Errorf("both v2 and v1 get_upload_url APIs failed: %w", err)
		}
	}

	if response.Code != 0 {
		return "", fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return response.Data.PresignedURL, nil
}

// uploadPart 将单个分片上传到给定URL
func (f *Fs) uploadPart(ctx context.Context, uploadURL string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create upload request: %w", err)
	}

	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/octet-stream")

	// 为请求添加超时
	if f.opt.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(f.opt.Timeout))
		defer cancel()
		req = req.WithContext(ctx)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("upload request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload part failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// completeUpload 完成多部分上传
func (f *Fs) completeUpload(ctx context.Context, preuploadID string) error {
	reqBody := map[string]interface{}{
		"preuploadID": preuploadID,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal complete upload request: %w", err)
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Async     bool  `json:"async"`
			Completed bool  `json:"completed"`
			FileID    int64 `json:"fileID"`
		} `json:"data"`
	}

	// 首先尝试v2 API
	err = f.apiCall(ctx, "POST", "/upload/v2/file/upload_complete", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		fs.Debugf(f, "v2 upload_complete失败，尝试v1: %v", err)
		// 回退到v1 API
		err = f.apiCall(ctx, "POST", "/upload/v1/file/upload_complete", bytes.NewReader(bodyBytes), &response)
		if err != nil {
			return fmt.Errorf("both v2 and v1 upload_complete APIs failed: %w", err)
		}
	}

	if response.Code != 0 {
		return fmt.Errorf("upload completion failed: API error %d: %s", response.Code, response.Message)
	}

	// 如果是异步处理，我们需要等待完成
	if response.Data.Async && !response.Data.Completed {
		fs.Debugf(f, "上传为异步模式，轮询完成状态，文件ID: %d", response.Data.FileID)
		err = f.pollAsyncUpload(ctx, preuploadID)
		if err != nil {
			return fmt.Errorf("failed to wait for async upload completion: %w", err)
		}
		fs.Debugf(f, "异步上传成功完成，文件ID: %d", response.Data.FileID)
	} else {
		fs.Debugf(f, "同步上传完成，文件ID: %d", response.Data.FileID)
	}

	return nil
}

// uploadPartStream 使用流式方式上传分片，避免大内存占用
func (f *Fs) uploadPartStream(ctx context.Context, uploadURL string, reader io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, reader)
	if err != nil {
		return fmt.Errorf("failed to create stream upload request: %w", err)
	}

	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	// 为请求添加超时
	if f.opt.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(f.opt.Timeout))
		defer cancel()
		req = req.WithContext(ctx)
	}

	resp, err := f.client.Do(req)
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

// pollAsyncUpload 轮询异步上传完成状态
func (f *Fs) pollAsyncUpload(ctx context.Context, preuploadID string) error {
	fs.Debugf(f, "轮询异步上传完成状态，preuploadID: %s", preuploadID)

	// 使用指数退避轮询，增加到30次尝试，最大5分钟
	// 对于大文件，123网盘需要更长的处理时间
	maxAttempts := 30
	for attempt := 0; attempt < maxAttempts; attempt++ {
		reqBody := map[string]interface{}{
			"preuploadID": preuploadID,
		}

		bodyBytes, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("failed to marshal async request: %w", err)
		}

		var response struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Completed bool  `json:"completed"`
				FileID    int64 `json:"fileID"`
			} `json:"data"`
		}

		// 首先尝试v2 API
		err = f.apiCall(ctx, "POST", "/upload/v2/file/upload_async_result", bytes.NewReader(bodyBytes), &response)
		if err != nil {
			fs.Debugf(f, "v2 async_result失败，尝试v1: %v", err)
			// 回退到v1 API
			err = f.apiCall(ctx, "POST", "/upload/v1/file/upload_async_result", bytes.NewReader(bodyBytes), &response)
			if err != nil {
				fs.Debugf(f, "异步轮询第%d次尝试失败（两个API都失败）: %v", attempt+1, err)
				// 即使单个请求失败也继续轮询
			}
		} else if response.Code != 0 {
			fs.Debugf(f, "异步轮询第%d次尝试返回错误: API错误 %d: %s", attempt+1, response.Code, response.Message)
		} else if response.Data.Completed {
			fs.Debugf(f, "异步上传在第%d次尝试后成功完成", attempt+1)
			return nil
		}

		// 下次尝试前等待（优化的指数退避策略）
		// 前几次尝试使用较短间隔，后续增加到最大15秒
		var waitTime time.Duration
		if attempt < 5 {
			// 前5次：1s, 2s, 3s, 4s, 5s
			waitTime = time.Duration(attempt+1) * time.Second
		} else if attempt < 10 {
			// 6-10次：5秒固定间隔
			waitTime = 5 * time.Second
		} else {
			// 11次以后：10-15秒间隔
			waitTime = time.Duration(10+attempt-10) * time.Second
			if waitTime > 15*time.Second {
				waitTime = 15 * time.Second // 最大15秒
			}
		}

		fs.Debugf(f, "异步上传尚未完成，等待%v后进行下次轮询（第%d/%d次尝试）", waitTime, attempt+1, maxAttempts)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitTime):
			// 继续下次尝试
		}
	}

	// 如果轮询失败，记录警告但不返回错误，因为文件可能已经上传成功
	fs.Debugf(f, "异步上传轮询超时，但文件可能已经上传成功，继续处理")
	return fmt.Errorf("异步上传轮询超时，无法确认文件状态")
}

// saveUploadProgress 将上传进度保存到临时文件以支持断点续传
func (f *Fs) saveUploadProgress(progress *UploadProgress) error {
	progressDir := filepath.Join(os.TempDir(), "rclone-123pan-progress")
	err := os.MkdirAll(progressDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create progress directory: %w", err)
	}

	// 使用preuploadID的MD5哈希以避免文件名长度问题
	hasher := md5.New()
	hasher.Write([]byte(progress.PreuploadID))
	shortID := fmt.Sprintf("%x", hasher.Sum(nil))

	progressFile := filepath.Join(progressDir, shortID+".json")
	data, err := json.Marshal(progress)
	if err != nil {
		return fmt.Errorf("failed to marshal progress: %w", err)
	}

	err = os.WriteFile(progressFile, data, 0644)
	if err != nil {
		return fmt.Errorf("failed to save progress file: %w", err)
	}

	fs.Debugf(f, "已保存上传进度%s: %d/%d个分片完成",
		progress.PreuploadID[:20]+"...", len(progress.UploadedParts), progress.TotalParts)
	return nil
}

// loadUploadProgress 从临时文件加载上传进度
func (f *Fs) loadUploadProgress(preuploadID string) (*UploadProgress, error) {
	progressDir := filepath.Join(os.TempDir(), "rclone-123pan-progress")

	// 使用preuploadID的MD5哈希以避免文件名长度问题
	hasher := md5.New()
	hasher.Write([]byte(preuploadID))
	shortID := fmt.Sprintf("%x", hasher.Sum(nil))

	progressFile := filepath.Join(progressDir, shortID+".json")

	data, err := os.ReadFile(progressFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 不存在进度文件
		}
		return nil, fmt.Errorf("failed to read progress file: %w", err)
	}

	var progress UploadProgress
	err = json.Unmarshal(data, &progress)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal progress: %w", err)
	}

	fs.Debugf(f, "已加载上传进度%s: %d/%d个分片完成",
		preuploadID, len(progress.UploadedParts), progress.TotalParts)
	return &progress, nil
}

// removeUploadProgress 在上传成功后删除进度文件
func (f *Fs) removeUploadProgress(preuploadID string) {
	progressDir := filepath.Join(os.TempDir(), "rclone-123pan-progress")

	// 使用preuploadID的MD5哈希以避免文件名长度问题
	hasher := md5.New()
	hasher.Write([]byte(preuploadID))
	shortID := fmt.Sprintf("%x", hasher.Sum(nil))

	progressFile := filepath.Join(progressDir, shortID+".json")

	err := os.Remove(progressFile)
	if err != nil && !os.IsNotExist(err) {
		fs.Debugf(f, "删除进度文件失败 %s: %v", progressFile, err)
	} else {
		fs.Debugf(f, "已删除上传进度文件 %s", preuploadID)
	}
}

// computeMD5FromReader 从io.Reader计算MD5哈希
func (f *Fs) computeMD5FromReader(ctx context.Context, in io.Reader, size int64) (string, error) {
	hasher := md5.New()

	// 将数据从读取器复制到哈希器
	written, err := io.Copy(hasher, in)
	if err != nil {
		return "", fmt.Errorf("failed to read data for MD5 computation: %w", err)
	}

	// 验证我们读取了预期的数量
	if size >= 0 && written != size {
		return "", fmt.Errorf("size mismatch: expected %d bytes, read %d bytes", size, written)
	}

	// 返回十六进制编码的MD5哈希
	return fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

// streamingPut 实现优化的流式上传，支持边传输边计算MD5
// 这消除了跨网盘传输中的双重下载问题，提高传输效率
// 修复了跨云盘传输中"源文件不支持重新打开"的问题
func (f *Fs) streamingPut(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	// 首先尝试获取已存在的MD5哈希值
	// 如果源文件已经有MD5（比如本地文件），直接使用，避免重复计算
	var md5Hash string
	if hashValue, err := src.Hash(ctx, hash.MD5); err == nil && hashValue != "" {
		md5Hash = hashValue
		fs.Debugf(f, "使用已存在的MD5哈希: %s", md5Hash)
		// 使用已知MD5进行上传，可以利用123网盘的秒传功能
		return f.putWithKnownMD5(ctx, in, src, parentFileID, fileName, md5Hash)
	}

	// 对于跨云盘传输，我们需要处理流不可重用的问题
	// 检查是否是跨云盘传输（通过检查源对象类型）
	if f.isRemoteSource(src) {
		fs.Debugf(f, "检测到跨云盘传输，使用缓冲策略避免重新打开问题")
		return f.streamingPutWithBuffer(ctx, in, src, parentFileID, fileName)
	}

	// 对于没有现成MD5的跨网盘传输，使用流式处理方法
	fs.Debugf(f, "没有现成的MD5哈希，使用流式上传并实时计算MD5")

	// 对于跨网盘传输，由于123网盘API严格要求真实MD5，我们使用缓存策略
	// 这避免了临时MD5导致的验证失败问题
	const maxCacheSize = 50 * 1024 * 1024 // 50MB阈值，平衡内存使用和效率
	if src.Size() > 0 && src.Size() <= maxCacheSize {
		// 中等文件策略：缓存到内存，计算真实MD5，然后上传
		// 优点：可以利用秒传功能，避免MD5不匹配错误
		fs.Debugf(f, "使用缓存策略处理中等大小文件（%d字节）", src.Size())
		return f.putSmallFileWithMD5(ctx, in, src, parentFileID, fileName)
	}

	// 对于超大文件（>50MB），使用缓冲策略而不是双重下载
	// 修复"源文件不支持重新打开"的问题
	fs.Debugf(f, "超大文件（%d字节），使用缓冲策略确保稳定性", src.Size())
	return f.streamingPutWithBuffer(ctx, in, src, parentFileID, fileName)
}

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
		return &Object{
			fs:          f,                                             // 文件系统引用
			remote:      fileName,                                      // 使用实际文件名，不包含.partial后缀
			hasMetaData: true,                                          // 已有元数据
			id:          strconv.FormatInt(createResp.Data.FileID, 10), // 123网盘文件ID
			size:        src.Size(),                                    // 文件大小
			md5sum:      md5Hash,                                       // MD5哈希值
			modTime:     time.Now(),                                    // 修改时间
			isDir:       false,                                         // 不是目录
		}, nil
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
	return &Object{
		fs:          f,
		remote:      fileName, // 使用实际的文件名，不包含.partial后缀
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        src.Size(),
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// putSmallFileWithMD5 处理小文件（≤10MB）的上传
// 通过缓存到内存先计算MD5，最大化利用秒传功能，同时控制内存使用
func (f *Fs) putSmallFileWithMD5(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "缓存小文件（%d字节）到内存以计算MD5，尝试秒传", src.Size())

	// 将整个小文件读取到内存
	// 对于≤10MB的文件，内存消耗是可接受的
	data, err := io.ReadAll(in)
	if err != nil {
		return nil, fmt.Errorf("读取小文件数据失败: %w", err)
	}

	// 验证读取的数据大小是否符合预期
	if src.Size() > 0 && int64(len(data)) != src.Size() {
		return nil, fmt.Errorf("文件大小不匹配: 期望 %d 字节，实际 %d 字节", src.Size(), len(data))
	}

	// 计算MD5哈希值
	// 这是关键步骤：先计算MD5，然后尝试秒传
	hasher := md5.New()
	hasher.Write(data)
	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(f, "小文件MD5计算完成: %s", md5Hash)

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
		return &Object{
			fs:          f,                                             // 文件系统引用
			remote:      src.Remote(),                                  // 远程路径
			hasMetaData: true,                                          // 已有元数据
			id:          strconv.FormatInt(createResp.Data.FileID, 10), // 123网盘文件ID
			size:        int64(len(data)),                              // 文件大小
			md5sum:      md5Hash,                                       // MD5哈希值
			modTime:     time.Now(),                                    // 修改时间
			isDir:       false,                                         // 不是目录
		}, nil
	}

	// 秒传失败，上传缓存的数据
	// 使用bytes.NewReader从内存中的数据创建reader
	fs.Debugf(f, "小文件秒传失败，上传缓存的数据")
	err = f.uploadFile(ctx, bytes.NewReader(data), createResp, int64(len(data)))
	if err != nil {
		return nil, err
	}

	// 返回上传成功的文件对象
	return &Object{
		fs:          f,
		remote:      src.Remote(),
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        int64(len(data)),
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// putLargeFileStreaming handles large files using streaming upload with on-the-fly MD5 calculation
// This approach eliminates double download for cross-cloud transfers
func (f *Fs) putLargeFileStreaming(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "对大文件使用流式上传（%d字节）", src.Size())

	// For large files, we need to stream without knowing MD5 upfront
	// We'll use a two-phase approach:
	// 1. Try to create upload session with a placeholder MD5 to check for deduplication
	// 2. If no deduplication, stream the file while calculating MD5

	// First, try with a size-based pseudo-MD5 to check for potential deduplication
	// This is a heuristic that might catch some duplicates based on size
	pseudoMD5 := fmt.Sprintf("%032x", src.Size())
	fs.Debugf(f, "尝试使用基于大小的伪MD5进行去重检查: %s", pseudoMD5)

	createResp, err := f.createUpload(ctx, parentFileID, fileName, pseudoMD5, src.Size())
	if err != nil {
		// 如果伪MD5失败，回退到真实MD5计算的流式上传
		fs.Debugf(f, "伪MD5去重失败，继续进行流式上传: %v", err)
		return f.streamingUploadWithMD5(ctx, in, src, parentFileID, fileName)
	}

	// If deduplication succeeded with pseudo-MD5 (unlikely but possible), return the object
	if createResp.Data.Reuse {
		fs.Debugf(f, "伪MD5意外去重成功")
		return &Object{
			fs:          f,
			remote:      src.Remote(),
			hasMetaData: true,
			id:          strconv.FormatInt(createResp.Data.FileID, 10),
			size:        src.Size(),
			md5sum:      "", // We don't have the real MD5
			modTime:     time.Now(),
			isDir:       false,
		}, nil
	}

	// No deduplication, proceed with streaming upload while calculating real MD5
	return f.streamingUploadWithMD5(ctx, in, src, parentFileID, fileName)
}

// streamingUploadWithMD5 执行流式上传，同时实时计算MD5哈希
// 这是核心优化功能，消除了跨网盘传输中的双重下载问题
func (f *Fs) streamingUploadWithMD5(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "开始流式上传，同时实时计算MD5哈希")

	// 创建MD5哈希计算器
	hasher := md5.New()

	// 创建TeeReader，在读取数据的同时计算MD5
	// TeeReader会将读取的数据同时写入hasher，实现边读边算MD5
	teeReader := io.TeeReader(in, hasher)

	// 创建临时上传会话，使用占位符MD5
	// 流式上传完成后我们会得到真实的MD5值
	tempMD5 := fmt.Sprintf("%032x", time.Now().UnixNano()) // 基于时间戳的唯一临时MD5
	createResp, err := f.createUpload(ctx, parentFileID, fileName, tempMD5, src.Size())
	if err != nil {
		return nil, fmt.Errorf("创建流式上传会话失败: %w", err)
	}

	// 如果临时MD5意外触发了秒传（极不可能），处理这种情况
	if createResp.Data.Reuse {
		fs.Debugf(f, "临时MD5意外触发秒传")
		// 我们仍需要消耗reader来计算真实的MD5
		_, err := io.Copy(hasher, teeReader)
		if err != nil {
			return nil, fmt.Errorf("消耗reader计算MD5失败: %w", err)
		}
		realMD5 := fmt.Sprintf("%x", hasher.Sum(nil))

		return &Object{
			fs:          f,                                             // 文件系统引用
			remote:      src.Remote(),                                  // 远程路径
			hasMetaData: true,                                          // 已有元数据
			id:          strconv.FormatInt(createResp.Data.FileID, 10), // 123网盘文件ID
			size:        src.Size(),                                    // 文件大小
			md5sum:      realMD5,                                       // 真实的MD5哈希值
			modTime:     time.Now(),                                    // 修改时间
			isDir:       false,                                         // 不是目录
		}, nil
	}

	// 使用TeeReader执行流式上传
	// 关键：这里实现了边下载边上传边计算MD5的核心功能
	// teeReader会同时将数据传给上传函数和MD5计算器
	err = f.uploadFile(ctx, teeReader, createResp, src.Size())
	if err != nil {
		return nil, fmt.Errorf("流式上传失败: %w", err)
	}

	// 获取计算完成的MD5哈希值
	// 此时上传已完成，MD5也计算完毕
	realMD5 := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(f, "流式上传完成，计算得到的MD5: %s", realMD5)

	// 返回上传成功的文件对象，包含真实的MD5值
	return &Object{
		fs:          f,                                             // 文件系统引用
		remote:      src.Remote(),                                  // 远程路径
		hasMetaData: true,                                          // 已有元数据
		id:          strconv.FormatInt(createResp.Data.FileID, 10), // 123网盘文件ID
		size:        src.Size(),                                    // 文件大小
		md5sum:      realMD5,                                       // 真实计算的MD5哈希值
		modTime:     time.Now(),                                    // 修改时间
		isDir:       false,                                         // 不是目录
	}, nil
}

// putLargeFileWithDoubleDownload 处理超大文件的传统双重下载方法
// 虽然效率较低，但确保与123网盘API的完全兼容性
func (f *Fs) putLargeFileWithDoubleDownload(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "使用传统双重下载方法处理超大文件")

	// 第一步：完整读取文件计算MD5
	fs.Debugf(f, "第一步：完整读取文件计算真实MD5哈希")
	md5Hash, err := f.computeMD5FromReader(ctx, in, src.Size())
	if err != nil {
		return nil, fmt.Errorf("计算MD5哈希失败: %w", err)
	}
	fs.Debugf(f, "计算得到MD5哈希: %s", md5Hash)

	// 第二步：重新获取reader进行上传
	fs.Debugf(f, "第二步：重新获取文件流进行上传")
	if srcObj, ok := src.(fs.Object); ok {
		freshReader, err := srcObj.Open(ctx)
		if err != nil {
			return nil, fmt.Errorf("重新打开文件失败: %w", err)
		}
		defer func() {
			if closer, ok := freshReader.(io.Closer); ok {
				if closeErr := closer.Close(); closeErr != nil {
					fs.Debugf(f, "关闭文件流失败: %v", closeErr)
				}
			}
		}()

		// 使用真实MD5进行上传
		return f.putWithKnownMD5(ctx, freshReader, src, parentFileID, fileName, md5Hash)
	}

	return nil, fmt.Errorf("源文件不支持重新打开")
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

	// 计算MD5
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
		fs.Debugf(f, "超大文件(%s)，使用分片流式传输策略（支持断点续传）", fs.SizeSuffix(fileSize))
		return f.streamingPutWithChunkedResume(ctx, in, src, parentFileID, fileName)
	}

	// 对于较小的大文件，继续使用临时文件策略
	fs.Debugf(f, "大文件(%s)，使用临时文件缓冲策略", fs.SizeSuffix(fileSize))

	// 创建临时文件
	tempFile, err := os.CreateTemp("", "rclone-123pan-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer func() {
		tempFile.Close()
		os.Remove(tempFile.Name())
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

// streamingPutWithTwoPhase 使用两阶段流式传输处理超大文件
// 第一阶段：快速计算MD5（只读取，不存储）
// 第二阶段：使用已知MD5进行真正的流式上传
// 这种方法资源占用最少，最适合超大文件传输
func (f *Fs) streamingPutWithTwoPhase(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "开始两阶段流式传输：第一阶段计算MD5")

	// 第一阶段：快速计算MD5哈希
	// 这里我们需要重新获取源文件流来计算MD5
	if srcObj, ok := src.(fs.Object); ok {
		// 获取新的流用于MD5计算
		md5Reader, err := srcObj.Open(ctx)
		if err != nil {
			fs.Debugf(f, "无法重新打开源文件进行MD5计算，回退到临时文件策略: %v", err)
			return f.streamingPutWithTempFileForced(ctx, in, src, parentFileID, fileName)
		}
		defer func() {
			if closer, ok := md5Reader.(io.Closer); ok {
				closer.Close()
			}
		}()

		// 快速计算MD5（只读取，不存储）
		hasher := md5.New()
		_, err = io.Copy(hasher, md5Reader)
		if err != nil {
			fs.Debugf(f, "MD5计算失败，回退到临时文件策略: %v", err)
			return f.streamingPutWithTempFileForced(ctx, in, src, parentFileID, fileName)
		}

		md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
		fs.Debugf(f, "第一阶段完成，计算得到MD5: %s", md5Hash)

		// 第二阶段：使用已知MD5进行流式上传
		fs.Debugf(f, "开始第二阶段：使用已知MD5进行流式上传")
		return f.putWithKnownMD5(ctx, in, src, parentFileID, fileName, md5Hash)
	}

	// 如果无法获取源对象，回退到临时文件策略
	fs.Debugf(f, "无法获取源对象，回退到临时文件策略")
	return f.streamingPutWithTempFileForced(ctx, in, src, parentFileID, fileName)
}

// streamingPutWithTempFileForced 强制使用临时文件策略（回退方案）
// 优化版本：先尝试流式传输，失败时才使用临时文件
func (f *Fs) streamingPutWithTempFileForced(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "使用优化的回退策略：先尝试流式传输")

	// 尝试获取源对象进行流式传输
	if srcObj, ok := src.(fs.Object); ok {
		// 尝试流式传输以避免双倍流量
		result, err := f.attemptStreamingUpload(ctx, srcObj, parentFileID, fileName)
		if err == nil {
			fs.Debugf(f, "流式传输成功，避免了临时文件")
			return result, nil
		}
		fs.Debugf(f, "流式传输失败，回退到临时文件策略: %v", err)
	}

	fs.Debugf(f, "使用临时文件策略作为最终回退方案")

	// 创建临时文件
	tempFile, err := os.CreateTemp("", "rclone-123pan-fallback-*")
	if err != nil {
		return nil, fmt.Errorf("创建回退临时文件失败: %w", err)
	}
	defer func() {
		tempFile.Close()
		os.Remove(tempFile.Name())
	}()

	// 边读边写到临时文件，同时计算MD5
	hasher := md5.New()
	multiWriter := io.MultiWriter(tempFile, hasher)

	written, err := io.Copy(multiWriter, in)
	if err != nil {
		return nil, fmt.Errorf("写入回退临时文件失败: %w", err)
	}

	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(f, "回退临时文件缓冲完成，写入: %d 字节, MD5: %s", written, md5Hash)

	// 重新定位到文件开头
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("重新定位回退临时文件失败: %w", err)
	}

	// 使用已知MD5上传
	return f.putWithKnownMD5(ctx, tempFile, src, parentFileID, fileName, md5Hash)
}

// streamingPutWithChunkedResume 使用分片流式传输处理超大文件
// 支持断点续传，资源占用最少（只需要单个分片的临时存储）
// 这是超大文件传输的最佳策略：既节省资源又支持断点续传
func (f *Fs) streamingPutWithChunkedResume(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentFileID int64, fileName string) (*Object, error) {
	fileSize := src.Size()
	chunkSize := int64(100 * 1024 * 1024) // 100MB分片大小，平衡资源占用和传输效率

	fs.Debugf(f, "开始分片流式传输：文件大小 %s，分片大小 %s",
		fs.SizeSuffix(fileSize), fs.SizeSuffix(chunkSize))

	// 检查是否可以重新打开源文件（分片传输需要）
	srcObj, ok := src.(fs.Object)
	if !ok {
		fs.Debugf(f, "无法获取源对象，回退到临时文件策略")
		return f.streamingPutWithTempFileForced(ctx, in, src, parentFileID, fileName)
	}

	// 计算分片数量
	totalChunks := (fileSize + chunkSize - 1) / chunkSize
	fs.Debugf(f, "文件将分为 %d 个分片进行传输", totalChunks)

	// 创建上传会话（使用临时MD5，后续会更新）
	tempMD5 := fmt.Sprintf("%032x", time.Now().UnixNano())
	createResp, err := f.createUpload(ctx, parentFileID, fileName, tempMD5, fileSize)
	if err != nil {
		return nil, fmt.Errorf("创建分片上传会话失败: %w", err)
	}

	// 如果意外触发秒传，回退到临时文件策略
	if createResp.Data.Reuse {
		fs.Debugf(f, "意外触发秒传，回退到临时文件策略")
		return f.streamingPutWithTempFileForced(ctx, in, src, parentFileID, fileName)
	}

	// 开始分片流式传输
	return f.uploadFileInChunksWithResume(ctx, srcObj, createResp, fileSize, chunkSize, fileName)
}

// uploadFileInChunksWithResume 执行分片流式上传，支持断点续传
func (f *Fs) uploadFileInChunksWithResume(ctx context.Context, srcObj fs.Object, createResp *UploadCreateResp, fileSize, chunkSize int64, fileName string) (*Object, error) {
	preuploadID := createResp.Data.PreuploadID
	totalChunks := (fileSize + chunkSize - 1) / chunkSize

	// 尝试加载现有进度
	progress, err := f.loadUploadProgress(preuploadID)
	if err != nil {
		fs.Debugf(f, "加载上传进度失败，创建新进度: %v", err)
		progress = &UploadProgress{
			PreuploadID:   preuploadID,
			TotalParts:    totalChunks,
			ChunkSize:     chunkSize,
			FileSize:      fileSize,
			UploadedParts: make(map[int64]bool),
			CreatedAt:     time.Now(),
		}
	} else {
		fs.Debugf(f, "恢复分片上传进度: %d/%d 个分片已完成",
			len(progress.UploadedParts), totalChunks)
	}

	// 计算整体MD5（用于最终验证）
	overallHasher := md5.New()

	// 检查是否使用多线程分片上传
	maxConcurrency := f.opt.MaxConcurrentUploads
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}

	// 对于大文件且有多个分片时，使用多线程上传
	if totalChunks > 3 && maxConcurrency > 1 {
		fs.Debugf(f, "使用多线程分片上传，并发数: %d", maxConcurrency)
		return f.uploadChunksWithConcurrency(ctx, srcObj, createResp, progress, overallHasher, fileName, maxConcurrency)
	}

	fs.Debugf(f, "使用单线程分片上传")
	// 逐个分片处理（单线程模式）
	for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
		partNumber := chunkIndex + 1

		// 检查此分片是否已上传
		if progress.UploadedParts[partNumber] {
			fs.Debugf(f, "跳过已上传的分片 %d/%d", partNumber, totalChunks)

			// 为了计算整体MD5，需要读取并丢弃这部分数据
			chunkStart := chunkIndex * chunkSize
			chunkEnd := chunkStart + chunkSize
			if chunkEnd > fileSize {
				chunkEnd = fileSize
			}
			actualChunkSize := chunkEnd - chunkStart

			// 读取分片数据用于MD5计算
			chunkReader, err := srcObj.Open(ctx, &fs.RangeOption{Start: chunkStart, End: chunkEnd - 1})
			if err != nil {
				return nil, fmt.Errorf("打开分片 %d 用于MD5计算失败: %w", partNumber, err)
			}

			_, err = io.CopyN(overallHasher, chunkReader, actualChunkSize)
			chunkReader.Close()
			if err != nil {
				return nil, fmt.Errorf("读取分片 %d 用于MD5计算失败: %w", partNumber, err)
			}

			continue
		}

		// 上传新分片
		err = f.uploadSingleChunkWithStream(ctx, srcObj, overallHasher, preuploadID, chunkIndex, chunkSize, fileSize, totalChunks)
		if err != nil {
			// 保存进度后返回错误
			saveErr := f.saveUploadProgress(progress)
			if saveErr != nil {
				fs.Debugf(f, "保存进度失败: %v", saveErr)
			}
			return nil, fmt.Errorf("上传分片 %d 失败: %w", partNumber, err)
		}

		// 标记分片已完成
		progress.UploadedParts[partNumber] = true

		// 保存进度
		err = f.saveUploadProgress(progress)
		if err != nil {
			fs.Debugf(f, "保存进度失败: %v", err)
		}

		fs.Debugf(f, "分片 %d/%d 上传完成", partNumber, totalChunks)
	}

	// 完成上传
	err = f.completeUpload(ctx, preuploadID)
	if err != nil {
		return nil, fmt.Errorf("完成分片上传失败: %w", err)
	}

	// 计算最终MD5
	finalMD5 := fmt.Sprintf("%x", overallHasher.Sum(nil))
	fs.Debugf(f, "分片流式上传完成，最终MD5: %s", finalMD5)

	// 清理进度文件
	f.removeUploadProgress(preuploadID)

	// 返回成功的文件对象
	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        fileSize,
		md5sum:      finalMD5,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// attemptStreamingUpload 尝试流式上传以避免双倍流量消耗
// 这个方法实现真正的边下边上传输，最小化流量使用
func (f *Fs) attemptStreamingUpload(ctx context.Context, srcObj fs.Object, parentFileID int64, fileName string) (*Object, error) {
	fs.Debugf(f, "尝试流式上传，避免临时文件和双倍流量")

	// 第一步：快速计算MD5（只读取，不存储）
	md5Reader, err := srcObj.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("无法打开源文件计算MD5: %w", err)
	}
	defer md5Reader.Close()

	hasher := md5.New()
	_, err = io.Copy(hasher, md5Reader)
	if err != nil {
		return nil, fmt.Errorf("计算MD5失败: %w", err)
	}

	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(f, "流式传输MD5计算完成: %s", md5Hash)

	// 第二步：使用已知MD5创建上传会话
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, srcObj.Size())
	if err != nil {
		return nil, fmt.Errorf("创建流式上传会话失败: %w", err)
	}

	// 如果触发秒传，直接返回成功
	if createResp.Data.Reuse {
		fs.Debugf(f, "流式传输触发秒传，MD5: %s", md5Hash)
		return &Object{
			fs:          f,
			remote:      fileName,
			hasMetaData: true,
			id:          strconv.FormatInt(createResp.Data.FileID, 10),
			size:        srcObj.Size(),
			md5sum:      md5Hash,
			modTime:     time.Now(),
			isDir:       false,
		}, nil
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

	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        srcObj.Size(),
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// uploadSingleChunkWithStream 使用流式方式上传单个分片
// 这是分片流式传输的核心：边下载边上传，只需要分片大小的临时存储
func (f *Fs) uploadSingleChunkWithStream(ctx context.Context, srcObj fs.Object, overallHasher io.Writer, preuploadID string, chunkIndex, chunkSize, fileSize, totalChunks int64) error {
	partNumber := chunkIndex + 1
	chunkStart := chunkIndex * chunkSize
	chunkEnd := chunkStart + chunkSize
	if chunkEnd > fileSize {
		chunkEnd = fileSize
	}
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

	// 获取分片上传URL
	uploadURL, err := f.getUploadURL(ctx, preuploadID, partNumber)
	if err != nil {
		return fmt.Errorf("获取分片 %d 上传URL失败: %w", partNumber, err)
	}

	// 读取临时文件数据用于上传
	chunkData, err := io.ReadAll(tempFile)
	if err != nil {
		return fmt.Errorf("读取分片 %d 临时文件失败: %w", partNumber, err)
	}

	// 上传分片数据
	err = f.uploadPart(ctx, uploadURL, chunkData)
	if err != nil {
		return fmt.Errorf("上传分片 %d 数据失败: %w", partNumber, err)
	}

	chunkMD5 := fmt.Sprintf("%x", chunkHasher.Sum(nil))
	fs.Debugf(f, "分片 %d 流式上传成功，大小: %d, MD5: %s", partNumber, written, chunkMD5)

	return nil
}

// uploadChunksWithConcurrency 使用多线程并发上传分片
func (f *Fs) uploadChunksWithConcurrency(ctx context.Context, srcObj fs.Object, createResp *UploadCreateResp, progress *UploadProgress, overallHasher io.Writer, fileName string, maxConcurrency int) (*Object, error) {
	preuploadID := createResp.Data.PreuploadID
	totalChunks := progress.TotalParts
	chunkSize := progress.ChunkSize
	fileSize := progress.FileSize

	fs.Debugf(f, "开始多线程分片上传：%d个分片，最大并发数：%d", totalChunks, maxConcurrency)

	// 创建可取消的上下文，用于控制goroutine生命周期
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers() // 确保函数退出时取消所有worker

	// 创建工作队列和结果通道
	chunkJobs := make(chan int64, totalChunks)
	results := make(chan chunkResult, totalChunks)

	// 启动工作协程
	for i := 0; i < maxConcurrency; i++ {
		go f.chunkUploadWorker(workerCtx, srcObj, preuploadID, chunkSize, fileSize, totalChunks, chunkJobs, results)
	}

	// 发送需要上传的分片任务
	pendingChunks := int64(0)
	for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
		partNumber := chunkIndex + 1
		if !progress.UploadedParts[partNumber] {
			chunkJobs <- chunkIndex
			pendingChunks++
		}
	}
	close(chunkJobs)

	fs.Debugf(f, "需要上传 %d 个分片", pendingChunks)

	// 如果没有需要上传的分片，直接跳过收集结果
	if pendingChunks == 0 {
		fs.Debugf(f, "所有分片已完成，跳过上传阶段")
	} else {
		// 收集结果
		completedChunks := int64(0)
		for completedChunks < pendingChunks {
			select {
			case result := <-results:
				if result.err != nil {
					fs.Errorf(f, "分片 %d 上传失败: %v", result.chunkIndex+1, result.err)
					// 保存进度后返回错误
					saveErr := f.saveUploadProgress(progress)
					if saveErr != nil {
						fs.Debugf(f, "保存进度失败: %v", saveErr)
					}
					return nil, fmt.Errorf("分片 %d 上传失败: %w", result.chunkIndex+1, result.err)
				}

				// 标记分片完成（线程安全）
				partNumber := result.chunkIndex + 1
				func() {
					// 使用匿名函数确保互斥锁正确释放
					f.progressMu.Lock()
					defer f.progressMu.Unlock()
					progress.UploadedParts[partNumber] = true
				}()
				completedChunks++

				// 保存进度
				err := f.saveUploadProgress(progress)
				if err != nil {
					fs.Debugf(f, "保存进度失败: %v", err)
				}

				fs.Debugf(f, "分片 %d/%d 上传完成 (并发)", partNumber, totalChunks)

			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	// 计算最终MD5（需要按顺序读取所有分片）
	fs.Debugf(f, "所有分片上传完成，计算最终MD5")
	finalHasher := md5.New()
	for chunkIndex := int64(0); chunkIndex < totalChunks; chunkIndex++ {
		chunkStart := chunkIndex * chunkSize
		chunkEnd := chunkStart + chunkSize
		if chunkEnd > fileSize {
			chunkEnd = fileSize
		}
		actualChunkSize := chunkEnd - chunkStart

		chunkReader, err := srcObj.Open(ctx, &fs.RangeOption{Start: chunkStart, End: chunkEnd - 1})
		if err != nil {
			return nil, fmt.Errorf("打开分片 %d 用于MD5计算失败: %w", chunkIndex+1, err)
		}

		// 使用defer确保资源正确释放
		func() {
			defer chunkReader.Close()
			_, err = io.CopyN(finalHasher, chunkReader, actualChunkSize)
		}()

		if err != nil {
			return nil, fmt.Errorf("读取分片 %d 用于MD5计算失败: %w", chunkIndex+1, err)
		}
	}

	// 完成上传
	err := f.completeUpload(ctx, preuploadID)
	if err != nil {
		return nil, fmt.Errorf("完成多线程分片上传失败: %w", err)
	}

	finalMD5 := fmt.Sprintf("%x", finalHasher.Sum(nil))
	fs.Debugf(f, "多线程分片上传完成，最终MD5: %s", finalMD5)

	// 清理进度文件
	f.removeUploadProgress(preuploadID)

	// 返回成功的文件对象
	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        fileSize,
		md5sum:      finalMD5,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// chunkResult 分片上传结果
type chunkResult struct {
	chunkIndex int64
	err        error
}

// chunkUploadWorker 分片上传工作协程
func (f *Fs) chunkUploadWorker(ctx context.Context, srcObj fs.Object, preuploadID string, chunkSize, fileSize, totalChunks int64, jobs <-chan int64, results chan<- chunkResult) {
	for chunkIndex := range jobs {
		err := f.uploadSingleChunkConcurrent(ctx, srcObj, preuploadID, chunkIndex, chunkSize, fileSize, totalChunks)
		results <- chunkResult{
			chunkIndex: chunkIndex,
			err:        err,
		}
	}
}

// uploadSingleChunkConcurrent 并发版本的单个分片上传（不需要MD5计算）
func (f *Fs) uploadSingleChunkConcurrent(ctx context.Context, srcObj fs.Object, preuploadID string, chunkIndex, chunkSize, fileSize, totalChunks int64) error {
	partNumber := chunkIndex + 1
	chunkStart := chunkIndex * chunkSize
	chunkEnd := chunkStart + chunkSize
	if chunkEnd > fileSize {
		chunkEnd = fileSize
	}
	actualChunkSize := chunkEnd - chunkStart

	fs.Debugf(f, "并发上传分片 %d/%d，范围: %d-%d，大小: %d", partNumber, totalChunks, chunkStart, chunkEnd-1, actualChunkSize)

	// 打开分片数据流
	chunkReader, err := srcObj.Open(ctx, &fs.RangeOption{Start: chunkStart, End: chunkEnd - 1})
	if err != nil {
		return fmt.Errorf("打开分片 %d 数据流失败: %w", partNumber, err)
	}
	defer chunkReader.Close()

	// 获取上传URL
	uploadURL, err := f.getUploadURL(ctx, preuploadID, partNumber)
	if err != nil {
		return fmt.Errorf("获取分片 %d 上传URL失败: %w", partNumber, err)
	}

	// 使用流式上传避免大内存占用
	// 对于大分片，直接流式传输而不是全部加载到内存
	err = f.uploadPartStream(ctx, uploadURL, chunkReader, actualChunkSize)
	if err != nil {
		return fmt.Errorf("上传分片 %d 数据失败: %w", partNumber, err)
	}

	fs.Debugf(f, "分片 %d/%d 并发上传成功", partNumber, totalChunks)
	return nil
}

// singleStepUpload 使用123云盘的单步上传API上传小文件（<1GB）
// 这个API专门为小文件设计，一次HTTP请求即可完成上传，效率更高
func (f *Fs) singleStepUpload(ctx context.Context, data []byte, parentFileID int64, fileName, md5Hash string) (*Object, error) {
	startTime := time.Now()
	fs.Debugf(f, "使用单步上传API，文件: %s, 大小: %d bytes, MD5: %s", fileName, len(data), md5Hash)

	// 验证文件大小限制
	if len(data) > singleStepUploadLimit {
		return nil, fmt.Errorf("文件大小超过单步上传限制%d bytes: %d bytes", singleStepUploadLimit, len(data))
	}

	// 验证文件名
	if err := f.validateFileName(fileName); err != nil {
		return nil, fmt.Errorf("文件名验证失败: %w", err)
	}

	// 获取上传域名
	uploadDomain, err := f.getUploadDomain(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取上传域名失败: %w", err)
	}

	// 构建单步上传URL
	uploadURL := uploadDomain + "/upload/v2/file/single/create"

	// 创建multipart form数据
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// 添加表单字段
	if err := writer.WriteField("parentFileID", strconv.FormatInt(parentFileID, 10)); err != nil {
		return nil, fmt.Errorf("写入parentFileID失败: %w", err)
	}

	if err := writer.WriteField("filename", fileName); err != nil {
		return nil, fmt.Errorf("写入filename失败: %w", err)
	}

	if err := writer.WriteField("etag", md5Hash); err != nil {
		return nil, fmt.Errorf("写入etag失败: %w", err)
	}

	if err := writer.WriteField("size", strconv.Itoa(len(data))); err != nil {
		return nil, fmt.Errorf("写入size失败: %w", err)
	}

	// 添加duplicate字段（保留两者，避免覆盖）
	if err := writer.WriteField("duplicate", "1"); err != nil {
		return nil, fmt.Errorf("写入duplicate失败: %w", err)
	}

	// 添加文件数据
	fileWriter, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return nil, fmt.Errorf("创建文件字段失败: %w", err)
	}

	if _, err := fileWriter.Write(data); err != nil {
		return nil, fmt.Errorf("写入文件数据失败: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("关闭multipart writer失败: %w", err)
	}

	// 创建HTTP请求
	req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, &buf)
	if err != nil {
		return nil, fmt.Errorf("创建单步上传请求失败: %w", err)
	}

	// 设置请求头
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Platform", "open_platform")
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// 发送请求（带重试机制）
	var resp *http.Response
	var body []byte
	maxRetries := 3

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// 指数退避重试
			backoff := time.Duration(attempt*attempt) * time.Second
			fs.Debugf(f, "单步上传重试 %d/%d，等待 %v", attempt, maxRetries, backoff)
			time.Sleep(backoff)
		}

		var err error
		resp, err = f.client.Do(req)
		if err != nil {
			if attempt == maxRetries {
				return nil, fmt.Errorf("单步上传请求失败（重试%d次后）: %w", maxRetries, err)
			}
			fs.Debugf(f, "单步上传请求失败，将重试: %v", err)
			continue
		}

		// 读取响应
		body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			if attempt == maxRetries {
				return nil, fmt.Errorf("读取单步上传响应失败（重试%d次后）: %w", maxRetries, err)
			}
			fs.Debugf(f, "读取响应失败，将重试: %v", err)
			continue
		}

		// 检查HTTP状态码
		if resp.StatusCode == http.StatusOK {
			break // 成功，跳出重试循环
		} else if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			// 可重试的错误
			if attempt == maxRetries {
				return nil, fmt.Errorf("单步上传失败（重试%d次后），状态码: %d, 响应: %s", maxRetries, resp.StatusCode, string(body))
			}
			fs.Debugf(f, "单步上传遇到可重试错误，状态码: %d，将重试", resp.StatusCode)
			continue
		} else {
			// 不可重试的错误，直接返回
			return nil, fmt.Errorf("单步上传失败，状态码: %d, 响应: %s", resp.StatusCode, string(body))
		}
	}

	// 解析响应
	var uploadResp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			FileID    int64 `json:"fileID"`
			Completed bool  `json:"completed"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &uploadResp); err != nil {
		return nil, fmt.Errorf("解析单步上传响应失败: %w", err)
	}

	// 检查API响应码
	if uploadResp.Code != 0 {
		return nil, fmt.Errorf("单步上传API错误: code=%d, message=%s", uploadResp.Code, uploadResp.Message)
	}

	// 检查是否上传完成
	if !uploadResp.Data.Completed {
		return nil, fmt.Errorf("单步上传未完成，可能需要使用分片上传")
	}

	duration := time.Since(startTime)
	speed := float64(len(data)) / duration.Seconds() / 1024 / 1024 // MB/s
	fs.Debugf(f, "单步上传成功，文件ID: %d, 耗时: %v, 速度: %.2f MB/s", uploadResp.Data.FileID, duration, speed)

	// 返回Object
	return &Object{
		fs:          f,
		remote:      fileName,
		hasMetaData: true,
		id:          strconv.FormatInt(uploadResp.Data.FileID, 10),
		size:        int64(len(data)),
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
}

// validateFileName 验证文件名是否符合123云盘要求
func (f *Fs) validateFileName(fileName string) error {
	// 检查字节长度限制（UTF-8编码）
	if len([]byte(fileName)) > maxFileNameBytes {
		return fmt.Errorf("文件名字节长度超过%d: %d", maxFileNameBytes, len([]byte(fileName)))
	}

	// 检查是否为空或全部是空格
	if strings.TrimSpace(fileName) == "" {
		return fmt.Errorf("文件名不能为空或全部是空格")
	}

	// 检查是否包含有效的UTF-8字符
	if !utf8.ValidString(fileName) {
		return fmt.Errorf("文件名包含无效的UTF-8字符")
	}

	// 检查禁用字符："\/:*?|><
	invalidChars := []string{"\"", "\\", "/", ":", "*", "?", "|", ">", "<"}
	for _, char := range invalidChars {
		if strings.Contains(fileName, char) {
			return fmt.Errorf("文件名包含禁用字符: %s", char)
		}
	}

	// 检查文件名是否以点开头或结尾（某些系统不支持）
	if strings.HasPrefix(fileName, ".") || strings.HasSuffix(fileName, ".") {
		fs.Debugf(f, "警告：文件名以点开头或结尾可能在某些系统中不被支持: %s", fileName)
	}

	return nil
}

// getUploadDomain 获取上传域名
func (f *Fs) getUploadDomain(ctx context.Context) (string, error) {
	// TODO: 实现动态获取上传域名的API调用
	// 根据123云盘文档，应该先调用获取上传域名的API
	// 目前使用默认域名，但应该添加配置选项允许用户自定义

	// 可以考虑添加域名缓存和健康检查
	defaultDomains := []string{
		"https://openapi-upload.123242.com",
		"https://openapi-upload.123pan.com", // 备用域名
	}

	// 暂时返回第一个域名，后续可以实现负载均衡和故障转移
	return defaultDomains[0], nil
}

// deleteFile 通过移动到回收站来删除文件
func (f *Fs) deleteFile(ctx context.Context, fileID int64) error {
	fs.Debugf(f, "删除文件，ID: %d", fileID)

	reqBody := map[string]interface{}{
		"fileIDs": []int64{fileID},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("序列化删除请求失败: %w", err)
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err = f.apiCall(ctx, "POST", "/api/v1/file/trash", bytes.NewReader(bodyBytes), &response)
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

	reqBody := map[string]interface{}{
		"fileIDs":        []int64{fileID},
		"toParentFileID": toParentFileID,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("序列化移动请求失败: %w", err)
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err = f.apiCall(ctx, "POST", "/api/v1/file/move", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return fmt.Errorf("移动文件失败: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("移动失败: API错误 %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "成功移动文件%d到父目录%d", fileID, toParentFileID)
	return nil
}

// renameFile 重命名文件
func (f *Fs) renameFile(ctx context.Context, fileID int64, fileName string) error {
	fs.Debugf(f, "重命名文件%d为: %s", fileID, fileName)

	// 验证新文件名
	if err := validateFileName(fileName); err != nil {
		return fmt.Errorf("重命名文件名验证失败: %w", err)
	}

	reqBody := map[string]interface{}{
		"fileId":   fileID,
		"fileName": fileName,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("序列化重命名请求失败: %w", err)
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err = f.apiCall(ctx, "PUT", "/api/v1/file/name", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return fmt.Errorf("重命名文件失败: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("重命名失败: API错误 %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "成功重命名文件%d为: %s", fileID, fileName)
	return nil
}

// getUserInfo 获取用户信息包括存储配额
func (f *Fs) getUserInfo(ctx context.Context) (*UserInfoResp, error) {
	var response UserInfoResp

	err := f.apiCall(ctx, "GET", "/api/v1/user/info", nil, &response)
	if err != nil {
		return nil, err
	}

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return &response, nil
}

// newFs 从路径构造Fs，格式为 container:path
func newFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// 解析选项
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	// Validate required options
	if opt.ClientID == "" {
		return nil, fmt.Errorf("client_id is required")
	}
	if opt.ClientSecret == "" {
		return nil, fmt.Errorf("client_secret is required")
	}

	// Set defaults for optional parameters
	if opt.UserAgent == "" {
		opt.UserAgent = defaultUserAgent
	}
	if opt.RootFolderID == "" {
		opt.RootFolderID = "0"
	}
	if opt.ListChunk <= 0 {
		opt.ListChunk = 100
	}
	if opt.PacerMinSleep <= 0 {
		opt.PacerMinSleep = fs.Duration(strictAPIMinSleep)
	}
	if opt.ListPacerMinSleep <= 0 {
		opt.ListPacerMinSleep = fs.Duration(listAPIMinSleep)
	}
	if opt.ConnTimeout <= 0 {
		opt.ConnTimeout = fs.Duration(defaultConnTimeout)
	}
	if opt.Timeout <= 0 {
		opt.Timeout = fs.Duration(defaultTimeout)
	}
	if opt.ChunkSize <= 0 {
		opt.ChunkSize = fs.SizeSuffix(defaultChunkSize)
	}
	if opt.UploadCutoff <= 0 {
		opt.UploadCutoff = fs.SizeSuffix(defaultUploadCutoff)
	}
	if opt.MaxUploadParts <= 0 {
		opt.MaxUploadParts = maxUploadParts
	}

	// 初始化性能优化参数
	if opt.MaxConcurrentUploads <= 0 {
		opt.MaxConcurrentUploads = 4
	}
	if opt.MaxConcurrentDownloads <= 0 {
		opt.MaxConcurrentDownloads = 4
	}
	if opt.ProgressUpdateInterval <= 0 {
		opt.ProgressUpdateInterval = fs.Duration(5 * time.Second)
	}

	// Store the original name before any modifications for config operations
	originalName := name
	if idx := strings.IndexRune(name, '{'); idx > 0 {
		originalName = name[:idx]
	}

	// 规范化root路径，处理绝对路径格式
	normalizedRoot := normalizePath(root)
	fs.Debugf(nil, "NewFs路径规范化: %q -> %q", root, normalizedRoot)

	f := &Fs{
		name:         name,
		originalName: originalName,
		root:         normalizedRoot,
		opt:          *opt,
		m:            m,
		rootFolderID: opt.RootFolderID,
		client:       fshttp.NewClient(ctx),
	}

	// Initialize multiple pacers for different API rate limits
	f.listPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(time.Duration(opt.ListPacerMinSleep)),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	f.strictPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(time.Duration(opt.PacerMinSleep)),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	f.uploadPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(uploadAPIMinSleep), // Use default for upload
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// 初始化性能优化组件
	f.uploadSemaphore = make(chan struct{}, opt.MaxConcurrentUploads)
	f.downloadSemaphore = make(chan struct{}, opt.MaxConcurrentDownloads)
	f.progressTracker = NewProgressTracker(
		time.Duration(opt.ProgressUpdateInterval),
		opt.EnableProgressDisplay,
	)

	fs.Debugf(f, "性能优化初始化完成 - 最大并发上传: %d, 最大并发下载: %d, 进度显示: %v",
		opt.MaxConcurrentUploads, opt.MaxConcurrentDownloads, opt.EnableProgressDisplay)

	f.features = (&fs.Features{
		ReadMimeType:            true,
		CanHaveEmptyDirectories: true,
		BucketBased:             false,
		SetTier:                 false,
		GetTier:                 false,
		DuplicateFiles:          false, // 123Pan handles duplicates
		ReadMetadata:            true,
		WriteMetadata:           false, // Not supported
		UserMetadata:            false, // Not supported
		PartialUploads:          true,  // Supports multipart uploads
		NoMultiThreading:        false, // Supports concurrent operations
		SlowModTime:             true,  // ModTime operations are slow
		SlowHash:                false, // Hash is available from API
	}).Fill(ctx, f)

	// Initialize directory cache
	// Note: We use the configured rootFolderID as the base, and let dircache
	// resolve the actual root path during FindRoot()
	// Use the normalized root path to ensure consistency
	f.dirCache = dircache.New(normalizedRoot, f.rootFolderID, f)

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
		err = f.GetAccessToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("initial login failed: %w", err)
		}
		// Save token to config after successful login
		f.saveToken(ctx, m)
		fs.Debugf(f, "登录成功，令牌已保存")
	}

	// Try to refresh the token if needed
	if tokenRefreshNeeded {
		err = f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			fs.Debugf(f, "令牌刷新失败，尝试完整登录: %v", err)
			// If refresh fails, try full login
			err = f.GetAccessToken(ctx)
			if err != nil {
				return nil, fmt.Errorf("login failed after token refresh failure: %w", err)
			}
		}
		// Save the refreshed/new token
		f.saveToken(ctx, m)
		fs.Debugf(f, "令牌刷新/登录成功，令牌已保存")
	}

	// Setup token renewer for automatic refresh
	fs.Debugf(f, "设置令牌更新器")
	f.setupTokenRenewer(ctx, m)

	// Find the root directory
	err = f.dirCache.FindRoot(ctx, false)
	if err != nil {
		// Assume it is a file
		newRoot, remote := dircache.SplitPath(root)
		tempF := *f
		tempF.dirCache = dircache.New(newRoot, f.rootFolderID, &tempF)
		tempF.root = newRoot
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
		return &tempF, fs.ErrorIsFile
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

	o := &Object{
		fs:          f,
		remote:      remote,
		hasMetaData: true,
		id:          fileID,
		size:        fileInfo.Size,
		md5sum:      fileInfo.Etag,
		modTime:     time.Now(), // We'll parse this properly later
		isDir:       false,
	}

	return o, nil
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// Put uploads the object.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	fs.Debugf(f, "调用Put: %s", src.Remote())

	// 并发控制：获取上传信号量
	select {
	case f.uploadSemaphore <- struct{}{}:
		defer func() { <-f.uploadSemaphore }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// 规范化远程路径
	normalizedRemote := normalizePath(src.Remote())
	fs.Debugf(f, "Put操作规范化路径: %s -> %s", src.Remote(), normalizedRemote)

	// 生成操作ID用于进度跟踪
	operationID := fmt.Sprintf("upload_%s_%d", src.Remote(), time.Now().UnixNano())

	// 开始进度跟踪
	f.progressTracker.StartOperation(operationID, src.Remote(), src.Size(), "上传中")
	defer func() {
		// 这里会在函数返回时调用，但我们需要在实际完成时调用CompleteOperation
	}()

	// Use dircache to find parent directory
	leaf, parentID, err := f.dirCache.FindPath(ctx, normalizedRemote, true)
	if err != nil {
		return nil, err
	}
	fileName := leaf

	// 验证并清理文件名
	cleanedFileName, warning := validateAndCleanFileName(fileName)
	if warning != nil {
		fs.Logf(f, "文件名验证警告: %v", warning)
		fileName = cleanedFileName
	}

	// Convert parentID to int64
	parentFileID, err := strconv.ParseInt(parentID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid parent ID: %s", parentID)
	}

	// Use streaming optimization for cross-cloud transfers
	obj, err := f.streamingPut(ctx, in, src, parentFileID, fileName)

	// 完成进度跟踪
	f.progressTracker.CompleteOperation(operationID, err == nil)

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
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	if t != hash.MD5 {
		return "", hash.ErrUnsupported
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
	fs.Debugf(o.fs, "打开文件: %s", o.remote)

	// 并发控制：获取下载信号量
	select {
	case o.fs.downloadSemaphore <- struct{}{}:
		// 信号量将在ReadCloser关闭时释放
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// 生成操作ID用于进度跟踪
	operationID := fmt.Sprintf("download_%s_%d", o.remote, time.Now().UnixNano())

	// 开始进度跟踪
	o.fs.progressTracker.StartOperation(operationID, o.remote, o.size, "下载中")

	var resp *http.Response
	var err error

	// Use pacer for download with retry (use strict pacer for download URL requests)
	err = o.fs.strictPacer.Call(func() (bool, error) {
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

		// Make the request
		resp, err = o.fs.client.Do(req)
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
		// 释放下载信号量
		<-o.fs.downloadSemaphore
		// 标记下载失败
		o.fs.progressTracker.CompleteOperation(operationID, false)
		return nil, err
	}

	// 创建包装的ReadCloser来处理进度更新和资源清理
	return &ProgressReadCloser{
		ReadCloser:      resp.Body,
		fs:              o.fs,
		operationID:     operationID,
		totalSize:       o.size,
		transferredSize: 0,
	}, nil
}

// Update the object with new content.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	fs.Debugf(o.fs, "调用Update: %s", o.remote)

	// Use dircache to find parent directory
	leaf, parentID, err := o.fs.dirCache.FindPath(ctx, o.remote, false)
	if err != nil {
		return err
	}

	// Convert parentID to int64
	parentFileID, err := strconv.ParseInt(parentID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid parent ID: %s", parentID)
	}

	// Calculate MD5 hash if available
	var md5Hash string
	if hashValue, err := src.Hash(ctx, hash.MD5); err == nil && hashValue != "" {
		md5Hash = hashValue
	}

	// Create upload session
	createResp, err := o.fs.createUpload(ctx, parentFileID, leaf, md5Hash, src.Size())
	if err != nil {
		return err
	}

	// If file already exists (reuse), update the object metadata
	if createResp.Data.Reuse {
		o.size = src.Size()
		o.md5sum = md5Hash
		o.modTime = time.Now()
		return nil
	}

	// Upload the file content
	err = o.fs.uploadFile(ctx, in, createResp, src.Size())
	if err != nil {
		return err
	}

	// Update object metadata
	o.size = src.Size()
	o.md5sum = md5Hash
	o.modTime = time.Now()

	return nil
}

// Remove the object.
func (o *Object) Remove(ctx context.Context) error {
	fs.Debugf(o.fs, "调用Remove: %s", o.remote)

	// Convert file ID to int64
	fileID, err := strconv.ParseInt(o.id, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid file ID: %s", o.id)
	}

	return o.fs.deleteFile(ctx, fileID)
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
	parentFileID, err := strconv.ParseInt(pathID, 10, 64)
	if err != nil {
		return "", false, fmt.Errorf("invalid parent ID: %s", pathID)
	}

	// List files in the parent directory
	response, err := f.ListFile(ctx, int(parentFileID), f.opt.ListChunk, "", "", 0)
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
	err = f.createDirectory(ctx, pathID, leaf)
	if err != nil {
		return "", err
	}

	// After creating, we need to find the newly created directory to get its ID
	// This is a bit inefficient but necessary since createDirectory doesn't return the ID
	foundID, found, err := f.FindLeaf(ctx, pathID, leaf)
	if err != nil {
		return "", fmt.Errorf("failed to find newly created directory: %w", err)
	}
	if !found {
		return "", fmt.Errorf("newly created directory not found")
	}

	return foundID, nil
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
func loadTokenFromConfig(f *Fs, m configmap.Mapper) bool {
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
func (f *Fs) saveToken(ctx context.Context, m configmap.Mapper) {
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
	_, ts, err := oauthutil.NewClientWithBaseClient(ctx, f.originalName, m, config, getHTTPClient(ctx, f.opt.ClientID, f.opt.ClientSecret))
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
	client := &http.Client{}
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

// Name of the remote (e.g. "pan123drive")
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (e.g. "bucket" or "bucket/dir")
func (f *Fs) Root() string {
	return f.root
}

// String returns a description of the FS
func (f *Fs) String() string {
	return fmt.Sprintf("pan123drive root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.MD5)
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
	parentFileID, err := strconv.ParseInt(dirID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid directory ID: %s", dirID)
	}

	// List all files in the directory
	var allFiles []FileListInfoRespDataV2
	lastFileID := int64(0)

	for {
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

	return nil
}

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fs.Debugf(f, "调用列表，目录: %s", dir)

	// 使用目录缓存查找目录ID
	fs.Debugf(f, "查找目录ID: %s", dir)
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		fs.Debugf(f, "查找目录失败 %s: %v", dir, err)
		return nil, err
	}
	fs.Debugf(f, "找到目录ID: %s，目录: %s", dirID, dir)

	// Convert dirID to int64
	parentFileID, err := strconv.ParseInt(dirID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid directory ID: %s", dirID)
	}

	// List files in the directory
	var allFiles []FileListInfoRespDataV2
	lastFileID := int64(0)

	for {
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
		remote := file.Filename
		if dir != "" {
			remote = strings.Trim(dir+"/"+file.Filename, "/")
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

	// Check if we're moving to the same directory
	if srcDirID == dstDirID {
		// Same directory - only rename if name changed
		if dstName != srcObj.Remote() {
			fs.Debugf(f, "同目录移动，仅重命名 %s 为 %s", srcObj.Remote(), dstName)
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
		err = f.moveFile(ctx, fileID, dstParentID)
		if err != nil {
			return nil, err
		}

		// If the name changed, rename the file
		if dstName != srcObj.Remote() {
			err = f.renameFile(ctx, fileID, dstName)
			if err != nil {
				return nil, err
			}
		}
	}

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

	// Move the directory
	err = f.moveFile(ctx, srcFileID, dstParentFileID)
	if err != nil {
		return err
	}

	// If the name changed, rename the directory
	if dstName != srcRemote {
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
	dstName, dstParentID, err := f.dirCache.FindPath(ctx, normalizedRemote, true)
	if err != nil {
		return nil, err
	}

	// 验证并清理目标文件名
	cleanedDstName, warning := validateAndCleanFileName(dstName)
	if warning != nil {
		fs.Logf(f, "复制操作文件名验证警告: %v", warning)
		dstName = cleanedDstName
	}

	// Convert IDs to int64
	srcFileID, err := strconv.ParseInt(srcObj.id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid source file ID: %s", srcObj.id)
	}

	dstParentFileID, err := strconv.ParseInt(dstParentID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid destination parent ID: %s", dstParentID)
	}

	// Try to copy using API (if available)
	newFileID, err := f.copyFile(ctx, srcFileID, dstParentFileID, dstName)
	if err != nil {
		// 如果服务器端复制失败，回退到下载+上传
		fs.Debugf(f, "服务器端复制失败，回退到下载+上传: %v", err)
		return f.copyViaDownloadUpload(ctx, srcObj, remote)
	}

	// Create new object for the copied file
	newObj := &Object{
		fs:          f,
		remote:      remote,
		hasMetaData: true,
		id:          strconv.FormatInt(newFileID, 10),
		size:        srcObj.size,
		md5sum:      srcObj.md5sum,
		modTime:     time.Now(),
		isDir:       false,
	}

	return newObj, nil
}

// copyFile attempts to copy a file using the API
func (f *Fs) copyFile(ctx context.Context, srcFileID, dstParentFileID int64, dstName string) (int64, error) {
	// 123Pan doesn't have a direct copy API, so we'll implement it via download+upload
	return 0, fmt.Errorf("server-side copy not supported by 123Pan API")
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
func (oi *ObjectInfo) Hash(ctx context.Context, t hash.Type) (string, error) {
	if t == hash.MD5 {
		return oi.md5sum, nil
	}
	return "", hash.ErrUnsupported
}

// getHTTPClient makes an http client according to the options
func getHTTPClient(ctx context.Context, clientID, clientSecret string) *http.Client {
	// This function is a placeholder. In a real scenario, you might configure
	// the HTTP client with timeouts, proxies, etc., based on the Fs options.
	// For now, it returns a basic client.
	return fshttp.NewClient(ctx)
}

// ProgressTracker 方法实现

// NewProgressTracker 创建新的进度跟踪器
func NewProgressTracker(updateInterval time.Duration, enableDisplay bool) *ProgressTracker {
	return &ProgressTracker{
		operations:     make(map[string]*OperationProgress),
		updateInterval: updateInterval,
		enableDisplay:  enableDisplay,
	}
}

// StartOperation 开始跟踪一个新操作
func (pt *ProgressTracker) StartOperation(operationID, fileName string, totalSize int64, status string) {
	if pt == nil {
		return
	}

	pt.mu.Lock()
	defer pt.mu.Unlock()

	pt.operations[operationID] = &OperationProgress{
		OperationID:     operationID,
		FileName:        fileName,
		TotalSize:       totalSize,
		TransferredSize: 0,
		StartTime:       time.Now(),
		LastUpdate:      time.Now(),
		Speed:           0,
		ETA:             0,
		Status:          status,
	}

	if pt.enableDisplay {
		fs.Logf(nil, "开始%s: %s (大小: %s)", status, fileName, fs.SizeSuffix(totalSize))
	}
}

// UpdateProgress 更新操作进度
func (pt *ProgressTracker) UpdateProgress(operationID string, transferredSize int64) {
	if pt == nil {
		return
	}

	pt.mu.Lock()
	defer pt.mu.Unlock()

	op, exists := pt.operations[operationID]
	if !exists {
		return
	}

	now := time.Now()
	timeDiff := now.Sub(op.LastUpdate).Seconds()

	// 计算传输速度
	if timeDiff > 0 {
		sizeDiff := transferredSize - op.TransferredSize
		op.Speed = float64(sizeDiff) / timeDiff
	}

	op.TransferredSize = transferredSize
	op.LastUpdate = now

	// 计算预计剩余时间
	if op.Speed > 0 && op.TotalSize > 0 {
		remainingBytes := op.TotalSize - transferredSize
		op.ETA = time.Duration(float64(remainingBytes)/op.Speed) * time.Second
	}

	// 显示进度（如果启用且达到更新间隔）
	if pt.enableDisplay && now.Sub(op.StartTime) >= pt.updateInterval {
		percentage := float64(transferredSize) / float64(op.TotalSize) * 100
		speed := fs.SizeSuffix(int64(op.Speed))
		fs.Logf(nil, "%s进度: %s %.1f%% (%s/s, 剩余: %v)",
			op.Status, op.FileName, percentage, speed, op.ETA.Round(time.Second))
	}
}

// CompleteOperation 完成操作
func (pt *ProgressTracker) CompleteOperation(operationID string, success bool) {
	if pt == nil {
		return
	}

	pt.mu.Lock()
	defer pt.mu.Unlock()

	op, exists := pt.operations[operationID]
	if !exists {
		return
	}

	if success {
		op.Status = "completed"
		op.TransferredSize = op.TotalSize
	} else {
		op.Status = "failed"
	}

	duration := time.Since(op.StartTime)
	var avgSpeed float64
	if duration.Seconds() > 0 {
		avgSpeed = float64(op.TransferredSize) / duration.Seconds()
	}

	if pt.enableDisplay {
		if success {
			fs.Logf(nil, "完成%s: %s (用时: %v, 平均速度: %s/s)",
				op.Status, op.FileName, duration.Round(time.Second), fs.SizeSuffix(int64(avgSpeed)))
		} else {
			fs.Logf(nil, "失败%s: %s (用时: %v)", op.Status, op.FileName, duration.Round(time.Second))
		}
	}

	// 清理已完成的操作
	delete(pt.operations, operationID)
}

// GetOperationProgress 获取操作进度信息
func (pt *ProgressTracker) GetOperationProgress(operationID string) (*OperationProgress, bool) {
	if pt == nil {
		return nil, false
	}

	pt.mu.RLock()
	defer pt.mu.RUnlock()

	op, exists := pt.operations[operationID]
	if !exists {
		return nil, false
	}

	// 返回副本以避免并发问题
	return &OperationProgress{
		OperationID:     op.OperationID,
		FileName:        op.FileName,
		TotalSize:       op.TotalSize,
		TransferredSize: op.TransferredSize,
		StartTime:       op.StartTime,
		LastUpdate:      op.LastUpdate,
		Speed:           op.Speed,
		ETA:             op.ETA,
		Status:          op.Status,
	}, true
}

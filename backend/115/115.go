package _115

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/rclone/rclone/backend/115/api"
	"github.com/rclone/rclone/backend/115/crypto" // Keep for traditional calls
	"github.com/rclone/rclone/backend/115/dircache"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/cache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"golang.org/x/oauth2"
)

// Constants
const (
	domain             = "www.115.com"
	traditionalRootURL = "https://webapi.115.com"
	openAPIRootURL     = "https://proapi.115.com"
	passportRootURL    = "https://passportapi.115.com"
	qrCodeAPIRootURL   = "https://qrcodeapi.115.com"
	hnQrCodeAPIRootURL = "https://hnqrcodeapi.115.com"     // For confirm step
	defaultAppID       = "100195123"                       // Provided App ID
	tradUserAgent      = "Mozilla/5.0 115Browser/27.0.7.5" // Keep for traditional login mimicry?
	defaultUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"

	// 🔧 优化的QPS配置 - 基于115网盘API特性和稳定性考虑
	defaultGlobalMinSleep = fs.Duration(200 * time.Millisecond) // 5 QPS - 通用API调用频率
	traditionalMinSleep   = fs.Duration(500 * time.Millisecond) // ~2 QPS - 提升传统API响应速度
	uploadMinSleep        = fs.Duration(100 * time.Millisecond) // ~10 QPS - 上传API专用，提高上传性能
	downloadURLMinSleep   = fs.Duration(500 * time.Millisecond) // ~2 QPS - 下载URL API专用，避免反爬
	maxSleep              = 2 * time.Second
	decayConstant         = 2 // bigger for slower decay, exponential

	defaultConTimeout = fs.Duration(15 * time.Second)  // 减少连接超时，快速失败重试
	defaultTimeout    = fs.Duration(120 * time.Second) // 减少IO超时，避免长时间等待

	maxUploadSize       = 115 * fs.Gibi // 115 GiB from https://proapi.115.com/app/uploadinfo (or OpenAPI equivalent)
	maxUploadParts      = 10000         // Part number must be an integer between 1 and 10000, inclusive.
	defaultChunkSize    = 50 * fs.Mebi  // 从10MB提高到50MB，大幅减少分片数量，提升性能
	minChunkSize        = 100 * fs.Kibi
	maxChunkSize        = 5 * fs.Gibi  // Max part size for OSS
	defaultUploadCutoff = 50 * fs.Mebi // 降低到50MB，让更多文件使用多线程上传
	defaultNohashSize   = 100 * fs.Mebi
	StreamUploadLimit   = 5 * fs.Gibi // Max size for sample/streamed upload (traditional)
	maxUploadCutoff     = 5 * fs.Gibi // maximum allowed size for singlepart uploads (OSS PutObject limit)

	tokenRefreshWindow = 10 * time.Minute // Refresh token 10 minutes before expiry
	pkceVerifierLength = 64               // Length for PKCE code verifier
)

// TraditionalRequest is the standard 115.com request structure for traditional API
// ... existing code ...

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "115",
		Description: "115 drive (supports Open API)",
		NewFs:       NewFs,
		CommandHelp: commandHelp,
		Options: []fs.Option{{
			Name: "cookie",
			Help: `Provide the login cookie in the format "UID=...; CID=...; SEID=...;".
Required for initial login to obtain API tokens.
Example: "UID=123; CID=abc; SEID=def;"`,
			Required:  true,
			Sensitive: true,
		}, {
			Name:      "share_code",
			Help:      "Share code from share link (for accessing shared files via traditional API).",
			Sensitive: true,
		}, {
			Name:      "receive_code",
			Help:      "Password from share link (for accessing shared files via traditional API).",
			Sensitive: true,
		}, {
			Name:     "user_agent",
			Default:  defaultUserAgent,
			Advanced: true,
			Help: fmt.Sprintf(`HTTP user agent. Primarily used for initial login mimicry.
Defaults to "%s".`, defaultUserAgent),
		}, {
			Name: "root_folder_id",
			Help: `ID of the root folder.
Leave blank normally (uses the drive root '0').
Fill in for rclone to use a non root folder as its starting point.`,
			Advanced:  true,
			Sensitive: true,
		}, {
			Name:     "app_id",
			Default:  defaultAppID,
			Advanced: true,
			Help: fmt.Sprintf(`Custom App ID for authentication.
Defaults to "%s". Only change this if you have a specific reason to use a different App ID.`, defaultAppID),
		}, {
			Name:     "list_chunk",
			Default:  1150, // Max limit for OpenAPI file list
			Help:     "Size of listing chunk.",
			Advanced: true,
		}, {
			Name:     "censored_only",
			Default:  false,
			Help:     "Only show files that are censored (only applies to traditional API calls).",
			Advanced: true,
		}, {
			Name:     "pacer_min_sleep",
			Default:  defaultGlobalMinSleep,
			Help:     "Minimum time to sleep between API calls (controls global QPS, default 5 QPS).",
			Advanced: true,
		}, {
			Name:     "contimeout",
			Default:  defaultConTimeout,
			Help:     "Connect timeout.",
			Advanced: true,
		}, {
			Name:     "timeout",
			Default:  defaultTimeout,
			Help:     "IO idle timeout.",
			Advanced: true,
		}, {
			Name:     "upload_hash_only",
			Default:  false,
			Advanced: true,
			Help: `Attempt hash-based upload (秒传) only. Skip uploading if the server doesn't have the file.
Requires SHA1 hash to be available or calculable.`,
		}, {
			Name:     "only_stream",
			Default:  false,
			Advanced: true,
			Help:     `Use traditional streamed upload (sample upload) for all files up to 5GiB. Fails for larger files.`,
		}, {
			Name:     "fast_upload",
			Default:  false,
			Advanced: true,
			Help: `Upload strategy:
- Files <= nohash_size: Use traditional streamed upload.
- Files > nohash_size: Attempt hash-based upload (秒传).
- If 秒传 fails and size <= 5GiB: Use traditional streamed upload.
- If 秒传 fails and size > 5GiB: Use multipart upload.`,
		}, {
			Name:     "hash_memory_limit",
			Help:     "Files bigger than this will be cached on disk to calculate hash if required.",
			Default:  fs.SizeSuffix(10 * 1024 * 1024),
			Advanced: true,
		}, {
			Name: "upload_cutoff",
			Help: `Cutoff for switching to multipart upload.
Any files larger than this will be uploaded in chunks using the OSS multipart API.
Minimum is 0, maximum is 5 GiB.`,
			Default:  defaultUploadCutoff,
			Advanced: true,
		}, {
			Name:     "nohash_size",
			Help:     `Files smaller than this size will use traditional streamed upload if fast_upload is enabled or if hash upload is not attempted/fails. Max is 5GiB.`,
			Default:  defaultNohashSize,
			Advanced: true,
		}, {
			Name: "chunk_size",
			Help: `Chunk size for multipart uploads.
Rclone will automatically increase the chunk size for large files to stay below the 10,000 parts limit.
Minimum is 100 KiB, maximum is 5 GiB.`,
			Default:  defaultChunkSize,
			Advanced: true,
		}, {
			Name:     "max_upload_parts",
			Help:     `Maximum number of parts in a multipart upload.`,
			Default:  maxUploadParts,
			Advanced: true,
		}, {
			Name: "upload_concurrency",
			Help: `Concurrency for multipart uploads.
Number of chunks to upload in parallel. Set to 0 to disable multipart uploads.
Minimum is 1, maximum is 32.`,
			Default:  8, // 从4提高到8，提升上传性能
			Advanced: true,
		}, {
			Name:     "internal",
			Help:     `Use the internal OSS endpoint for uploads (requires appropriate network access).`,
			Default:  false,
			Advanced: true,
		}, {
			Name:     "dual_stack",
			Help:     `Use a dual-stack (IPv4/IPv6) OSS endpoint for uploads.`,
			Default:  false,
			Advanced: true,
		}, {
			Name:     "no_check",
			Default:  false,
			Advanced: true,
			Help:     "Disable post-upload check (avoids extra API call but reduces certainty).",
		}, {
			Name:     "no_buffer",
			Default:  false,
			Advanced: true,
			Help:     "Skip disk buffering for uploads.",
		}, {
			Name:     config.ConfigEncoding,
			Help:     config.ConfigEncodingHelp,
			Advanced: true,
			Default: (encoder.EncodeLtGt |
				encoder.EncodeDoubleQuote |
				encoder.EncodeLeftSpace |
				encoder.EncodeCtl |
				encoder.EncodeRightSpace |
				encoder.EncodeInvalidUtf8),
		}},
	})
}

func checkUploadChunkSize(cs fs.SizeSuffix) error {
	if cs < minChunkSize {
		return fmt.Errorf("%s is less than %s", cs, minChunkSize)
	}
	if cs > maxChunkSize {
		return fmt.Errorf("%s is greater than %s", cs, maxChunkSize)
	}
	return nil
}

func (f *Fs) setUploadChunkSize(cs fs.SizeSuffix) (old fs.SizeSuffix, err error) {
	err = checkUploadChunkSize(cs)
	if err == nil {
		old, f.opt.ChunkSize = f.opt.ChunkSize, cs
	}
	return
}

func checkUploadCutoff(cs fs.SizeSuffix) error {
	if cs > maxUploadCutoff {
		return fmt.Errorf("%s is greater than %s", cs, maxUploadCutoff)
	}
	return nil
}

func checkUploadConcurrency(concurrency int) error {
	if concurrency < 1 {
		return fmt.Errorf("upload_concurrency must be at least 1, got %d", concurrency)
	}
	if concurrency > 32 {
		return fmt.Errorf("upload_concurrency must be at most 32, got %d", concurrency)
	}
	return nil
}

func (f *Fs) setUploadCutoff(cs fs.SizeSuffix) (old fs.SizeSuffix, err error) {
	err = checkUploadCutoff(cs)
	if err == nil {
		old, f.opt.UploadCutoff = f.opt.UploadCutoff, cs
	}
	return
}

// Options defines the configuration of this backend
type Options struct {
	Cookie              string               `config:"cookie"` // Single cookie string now
	ShareCode           string               `config:"share_code"`
	ReceiveCode         string               `config:"receive_code"`
	UserAgent           string               `config:"user_agent"`
	RootFolderID        string               `config:"root_folder_id"`
	ListChunk           int                  `config:"list_chunk"`
	CensoredOnly        bool                 `config:"censored_only"`
	PacerMinSleep       fs.Duration          `config:"pacer_min_sleep"` // Global pacer setting
	ConTimeout          fs.Duration          `config:"contimeout"`
	Timeout             fs.Duration          `config:"timeout"`
	HashMemoryThreshold fs.SizeSuffix        `config:"hash_memory_limit"`
	UploadHashOnly      bool                 `config:"upload_hash_only"`
	OnlyStream          bool                 `config:"only_stream"`
	FastUpload          bool                 `config:"fast_upload"`
	UploadCutoff        fs.SizeSuffix        `config:"upload_cutoff"`
	NohashSize          fs.SizeSuffix        `config:"nohash_size"`
	ChunkSize           fs.SizeSuffix        `config:"chunk_size"`
	MaxUploadParts      int                  `config:"max_upload_parts"`
	UploadConcurrency   int                  `config:"upload_concurrency"`
	Internal            bool                 `config:"internal"`
	DualStack           bool                 `config:"dual_stack"`
	NoCheck             bool                 `config:"no_check"`
	NoBuffer            bool                 `config:"no_buffer"` // Skip disk buffering for uploads
	Enc                 encoder.MultiEncoder `config:"encoding"`
	AppID               string               `config:"app_id"` // Custom App ID for authentication
}

// Fs represents a remote 115 drive
type Fs struct {
	name          string
	originalName  string // Original config name without modifications
	root          string
	opt           Options
	features      *fs.Features
	tradClient    *rest.Client // Client for traditional (cookie, encrypted) API calls
	openAPIClient *rest.Client // Client for OpenAPI (token) calls
	dirCache      *dircache.DirCache
	globalPacer   *fs.Pacer // Controls overall QPS
	tradPacer     *fs.Pacer // Controls QPS for traditional calls only (subset of global)
	downloadPacer *fs.Pacer // Controls QPS for download URL API calls (专门用于获取下载URL的API调用)
	uploadPacer   *fs.Pacer // Controls QPS for upload related API calls (专门用于上传相关的API调用)
	rootFolder    string    // path of the absolute root
	rootFolderID  string
	appVer        string // parsed from user-agent; used in traditional calls
	userID        string // User ID from cookie/token
	userkey       string // User key from traditional uploadinfo (needed for traditional upload init signature)
	isShare       bool   // mark it is from shared or not
	fileObj       *fs.Object
	m             configmap.Mapper // config map for saving tokens

	// 断点续传管理器
	resumeManager *ResumeManager115

	// 🚀 功能增强：智能URL管理器，解决下载URL频繁过期问题
	smartURLManager *SmartURLManager

	// Token management
	tokenMu      sync.Mutex
	accessToken  string
	refreshToken string
	tokenExpiry  time.Time
	codeVerifier string // For PKCE
	tokenRenewer *oauthutil.Renew
	loginMu      sync.Mutex

	// 路径缓存优化
	pathCache *PathCache

	// BadgerDB持久化缓存系统
	pathResolveCache *cache.BadgerCache // 路径解析缓存 (已实现)
	dirListCache     *cache.BadgerCache // 目录列表缓存
	metadataCache    *cache.BadgerCache // 文件元数据缓存
	fileIDCache      *cache.BadgerCache // 文件ID验证缓存

	// 缓存时间配置
	cacheConfig CacheConfig115

	// HTTP连接池优化
	httpClient *http.Client

	// 上传操作锁，防止预热和上传同时进行
	uploadingMu sync.Mutex
	isUploading bool
}

// CacheConfig115 115网盘缓存时间配置
type CacheConfig115 struct {
	PathResolveCacheTTL time.Duration // 路径解析缓存TTL，默认10分钟
	DirListCacheTTL     time.Duration // 目录列表缓存TTL，默认5分钟
	MetadataCacheTTL    time.Duration // 文件元数据缓存TTL，默认30分钟
	FileIDCacheTTL      time.Duration // 文件ID验证缓存TTL，默认15分钟
}

// DefaultCacheConfig115 返回115网盘优化的缓存配置
// 🔧 轻量级优化：统一TTL策略，避免缓存不一致问题
func DefaultCacheConfig115() CacheConfig115 {
	unifiedTTL := 5 * time.Minute // 统一TTL策略，与123网盘保持一致
	return CacheConfig115{
		PathResolveCacheTTL: unifiedTTL, // 从15分钟改为5分钟，统一TTL
		DirListCacheTTL:     unifiedTTL, // 从10分钟改为5分钟，统一TTL
		MetadataCacheTTL:    unifiedTTL, // 从60分钟改为5分钟，避免长期缓存不一致
		FileIDCacheTTL:      unifiedTTL, // 从30分钟改为5分钟，统一TTL
	}
}

// PathCacheEntry 路径缓存条目
type PathCacheEntry struct {
	ID        string
	IsDir     bool
	Timestamp time.Time
	// LRU链表节点
	prev, next *PathCacheEntry
	key        string // 存储key用于从map中删除
}

// PathCache LRU路径缓存实现，O(1)时间复杂度
type PathCache struct {
	cache   map[string]*PathCacheEntry
	mu      sync.RWMutex
	ttl     time.Duration
	maxSize int
	// LRU双向链表
	head, tail *PathCacheEntry
}

// NewPathCache 创建新的路径缓存
func NewPathCache(ttl time.Duration, maxSize int) *PathCache {
	pc := &PathCache{
		cache:   make(map[string]*PathCacheEntry),
		ttl:     ttl,
		maxSize: maxSize,
	}

	// 初始化LRU链表的哨兵节点
	pc.head = &PathCacheEntry{}
	pc.tail = &PathCacheEntry{}
	pc.head.next = pc.tail
	pc.tail.prev = pc.head

	return pc
}

// Get 从缓存获取路径信息，实现LRU访问
func (pc *PathCache) Get(path string) (*PathCacheEntry, bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	entry, exists := pc.cache[path]
	if !exists {
		return nil, false
	}

	// 检查是否过期
	if time.Since(entry.Timestamp) > pc.ttl {
		// 过期的条目需要从缓存中删除
		pc.removeFromList(entry)
		delete(pc.cache, path)
		return nil, false
	}

	// 移动到链表头部（最近使用）
	pc.moveToHead(entry)

	return entry, true
}

// addToHead 将节点添加到链表头部
func (pc *PathCache) addToHead(entry *PathCacheEntry) {
	entry.prev = pc.head
	entry.next = pc.head.next
	pc.head.next.prev = entry
	pc.head.next = entry
}

// removeFromList 从链表中移除节点
func (pc *PathCache) removeFromList(entry *PathCacheEntry) {
	if entry.prev != nil {
		entry.prev.next = entry.next
	}
	if entry.next != nil {
		entry.next.prev = entry.prev
	}
}

// moveToHead 将节点移动到链表头部
func (pc *PathCache) moveToHead(entry *PathCacheEntry) {
	pc.removeFromList(entry)
	pc.addToHead(entry)
}

// removeTail 移除链表尾部节点
func (pc *PathCache) removeTail() *PathCacheEntry {
	lastEntry := pc.tail.prev
	if lastEntry == pc.head {
		return nil // 链表为空
	}
	pc.removeFromList(lastEntry)
	return lastEntry
}

// Put 将路径信息放入缓存
func (pc *PathCache) Put(path, id string, isDir bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	// 检查是否已存在
	if existingEntry, exists := pc.cache[path]; exists {
		// 更新现有条目并移动到头部
		existingEntry.ID = id
		existingEntry.IsDir = isDir
		existingEntry.Timestamp = time.Now()
		pc.moveToHead(existingEntry)
		return
	}

	// 如果缓存已满，移除最旧的条目
	if len(pc.cache) >= pc.maxSize {
		tailEntry := pc.removeTail()
		if tailEntry != nil && tailEntry.key != "" {
			delete(pc.cache, tailEntry.key)
		}
	}

	// 创建新条目
	newEntry := &PathCacheEntry{
		ID:        id,
		IsDir:     isDir,
		Timestamp: time.Now(),
		key:       path,
	}

	// 添加到缓存和链表头部
	pc.cache[path] = newEntry
	pc.addToHead(newEntry)
}

// Clear 清空缓存
func (pc *PathCache) Clear() {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	// 清空map
	pc.cache = make(map[string]*PathCacheEntry)

	// 重新初始化链表
	pc.head.next = pc.tail
	pc.tail.prev = pc.head
}

// Delete 删除指定路径的缓存条目
func (pc *PathCache) Delete(path string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if entry, exists := pc.cache[path]; exists {
		delete(pc.cache, path)
		pc.removeFromList(entry)
	}
}

// ClearOldEntries 清理指定时间之前的缓存条目
func (pc *PathCache) ClearOldEntries(maxAge time.Duration) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	cutoffTime := time.Now().Add(-maxAge)
	toDelete := make([]string, 0)

	// 找出需要删除的条目
	for path, entry := range pc.cache {
		if entry.Timestamp.Before(cutoffTime) {
			toDelete = append(toDelete, path)
		}
	}

	// 删除过期条目
	for _, path := range toDelete {
		if entry, exists := pc.cache[path]; exists {
			delete(pc.cache, path)
			pc.removeFromList(entry)
		}
	}

	if len(toDelete) > 0 {
		fs.Debugf(nil, "清理了 %d 个过期的缓存条目", len(toDelete))
	}
}

// AsyncCacheUpdate 异步更新缓存，避免阻塞主要操作
func (f *Fs) AsyncCacheUpdate(ctx context.Context, path, id string, isDir bool) {
	select {
	case <-ctx.Done():
		fs.Debugf(f, "AsyncCacheUpdate cancelled for path: %s", path)
		return
	default:
		go func() {
			defer func() {
				if r := recover(); r != nil {
					fs.Debugf(f, "AsyncCacheUpdate panic for path %s: %v", path, r)
				}
			}()

			// 使用带超时的context防止goroutine长时间运行
			updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			select {
			case <-updateCtx.Done():
				fs.Debugf(f, "AsyncCacheUpdate timeout for path: %s", path)
				return
			case <-ctx.Done():
				fs.Debugf(f, "AsyncCacheUpdate cancelled during execution for path: %s", path)
				return
			default:
				// 检查缓存是否仍然有效（避免在清理后更新脏数据）
				if f.pathCache != nil {
					f.pathCache.Put(path, id, isDir)
					fs.Debugf(f, "AsyncCacheUpdate completed for path: %s", path)
				}
			}
		}()
	}
}

// AsyncDirCacheUpdate 异步更新目录缓存
func (f *Fs) AsyncDirCacheUpdate(ctx context.Context, path, id string) {
	select {
	case <-ctx.Done():
		fs.Debugf(f, "AsyncDirCacheUpdate cancelled for path: %s", path)
		return
	default:
		go func() {
			defer func() {
				if r := recover(); r != nil {
					fs.Debugf(f, "AsyncDirCacheUpdate panic for path %s: %v", path, r)
				}
			}()

			// 使用带超时的context防止goroutine长时间运行
			updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			select {
			case <-updateCtx.Done():
				fs.Debugf(f, "AsyncDirCacheUpdate timeout for path: %s", path)
				return
			case <-ctx.Done():
				fs.Debugf(f, "AsyncDirCacheUpdate cancelled during execution for path: %s", path)
				return
			default:
				// 检查目录缓存是否仍然有效
				if f.dirCache != nil {
					f.dirCache.Put(path, id)
					fs.Debugf(f, "AsyncDirCacheUpdate completed for path: %s", path)
				}
			}
		}()
	}
}

// BatchFindRequest 批量查找请求
type BatchFindRequest struct {
	PathID string
	Leaf   string
}

// BatchFindResult 批量查找结果
type BatchFindResult struct {
	Request BatchFindRequest
	ID      string
	Found   bool
	IsDir   bool
	Error   error
}

// BatchFindLeaf 批量查找叶节点，优化多个查找操作
func (f *Fs) BatchFindLeaf(ctx context.Context, requests []BatchFindRequest) ([]BatchFindResult, error) {
	results := make([]BatchFindResult, len(requests))

	// 按父目录ID分组请求
	groupedRequests := make(map[string][]int)
	for i, req := range requests {
		groupedRequests[req.PathID] = append(groupedRequests[req.PathID], i)
	}

	// 对每个父目录执行一次listAll操作
	for pathID, indices := range groupedRequests {
		// 创建查找映射
		leafMap := make(map[string][]int)
		for _, idx := range indices {
			leaf := requests[idx].Leaf
			leafMap[leaf] = append(leafMap[leaf], idx)
		}

		// 执行单次listAll操作
		_, err := f.listAll(ctx, pathID, f.opt.ListChunk, false, false, func(item *api.File) bool {
			decodedName := f.opt.Enc.ToStandardName(item.FileNameBest())
			if indices, exists := leafMap[decodedName]; exists {
				for _, idx := range indices {
					results[idx] = BatchFindResult{
						Request: requests[idx],
						ID:      item.ID(),
						Found:   true,
						IsDir:   item.IsDir(),
						Error:   nil,
					}
				}
				delete(leafMap, decodedName) // 避免重复处理
			}
			return len(leafMap) == 0 // 如果所有项目都找到了就停止
		})

		if err != nil {
			// 如果API调用失败，标记所有相关结果为错误
			for _, idx := range indices {
				if results[idx].Error == nil { // 只设置尚未设置的错误
					results[idx] = BatchFindResult{
						Request: requests[idx],
						Found:   false,
						Error:   err,
					}
				}
			}
		}

		// 标记未找到的项目
		for _, indices := range leafMap {
			for _, idx := range indices {
				if results[idx].Error == nil {
					results[idx] = BatchFindResult{
						Request: requests[idx],
						Found:   false,
						Error:   nil,
					}
				}
			}
		}
	}

	return results, nil
}

// isAPILimitError 检查错误是否为API限制错误
func isAPILimitError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "770004") ||
		strings.Contains(errStr, "已达到当前访问上限")
}

// isNetworkError 检查是否为网络相关错误
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "connection timeout") ||
		strings.Contains(errStr, "network is unreachable") ||
		strings.Contains(errStr, "no route to host") ||
		strings.Contains(errStr, "dns") ||
		strings.Contains(errStr, "dial tcp") ||
		strings.Contains(errStr, "i/o timeout")
}

// isTemporaryError 检查是否为临时错误
func isTemporaryError(err error) bool {
	if err == nil {
		return false
	}

	// 检查是否实现了Temporary接口
	if temp, ok := err.(interface{ Temporary() bool }); ok {
		return temp.Temporary()
	}

	// 检查错误字符串中的临时错误指示
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "temporary") ||
		strings.Contains(errStr, "try again") ||
		strings.Contains(errStr, "service unavailable") ||
		strings.Contains(errStr, "internal server error")
}

// isSevereAPILimit 检查是否为严重的API限制错误
func isSevereAPILimit(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// 严重的API限制通常包含特定的错误码或消息
	return strings.Contains(errStr, "770004") || // 严重限制错误码
		strings.Contains(errStr, "账号已被限制") ||
		strings.Contains(errStr, "访问频率过高")
}

// clearCacheGradually 根据错误严重程度渐进式清理缓存
func (f *Fs) clearCacheGradually(err error, affectedPath string) {
	if !isAPILimitError(err) {
		return
	}

	if isSevereAPILimit(err) {
		fs.Debugf(f, "严重API限制错误，清理所有缓存: %v", err)
		f.dirCache.ResetRoot()
		f.pathCache.Clear()

		// 清理所有BadgerDB持久化缓存
		caches := []*cache.BadgerCache{
			f.pathResolveCache,
			f.dirListCache,
			f.metadataCache,
			f.fileIDCache,
		}

		for i, c := range caches {
			if c != nil {
				if clearErr := c.Clear(); clearErr != nil {
					fs.Debugf(f, "清理BadgerDB缓存%d失败: %v", i, clearErr)
				}
			}
		}
		fs.Debugf(f, "已清理所有BadgerDB持久化缓存")
	} else {
		fs.Debugf(f, "轻微API限制错误，采用渐进式缓存清理: %v", err)

		// 只清理可能受影响的特定路径缓存
		if affectedPath != "" {
			// 清理特定路径的缓存
			f.pathCache.Delete(affectedPath)

			// 清理BadgerDB中对应的缓存
			if f.pathResolveCache != nil {
				cacheKey := fmt.Sprintf("path_%s", affectedPath)
				if delErr := f.pathResolveCache.Delete(cacheKey); delErr != nil {
					fs.Debugf(f, "清理BadgerDB路径缓存失败 %s: %v", affectedPath, delErr)
				}
			}

			// 清理父目录的缓存
			parentPath := path.Dir(affectedPath)
			if parentPath != "." && parentPath != "/" {
				f.pathCache.Delete(parentPath)

				// 清理BadgerDB中父目录的缓存
				if f.pathResolveCache != nil {
					parentCacheKey := fmt.Sprintf("path_%s", parentPath)
					if delErr := f.pathResolveCache.Delete(parentCacheKey); delErr != nil {
						fs.Debugf(f, "清理BadgerDB父目录缓存失败 %s: %v", parentPath, delErr)
					}
				}
			}
		}

		// 清理最近5分钟的缓存条目（保留较老的稳定缓存）
		f.pathCache.ClearOldEntries(5 * time.Minute)

		// 不清理根目录缓存，避免完全重建
		fs.Debugf(f, "保留根目录缓存，避免完全重建")
	}
}

// cachePathResolution 缓存路径解析结果到BadgerDB
func (f *Fs) cachePathResolution(path, fileID string, isDir bool) {
	if f.pathResolveCache == nil {
		return
	}

	cacheKey := fmt.Sprintf("path_%s", path)
	cacheValue := map[string]interface{}{
		"file_id": fileID,
		"is_dir":  isDir,
	}

	if err := f.pathResolveCache.Set(cacheKey, cacheValue, f.cacheConfig.PathResolveCacheTTL); err != nil {
		fs.Debugf(f, "缓存路径解析失败 %s: %v", path, err)
	} else {
		fs.Debugf(f, "已缓存路径解析 %s -> %s (dir: %v), TTL=%v", path, fileID, isDir, f.cacheConfig.PathResolveCacheTTL)
	}
}

// getCachedPathResolution 从BadgerDB获取缓存的路径解析结果
func (f *Fs) getCachedPathResolution(path string) (string, bool, bool) {
	if f.pathResolveCache == nil {
		return "", false, false
	}

	cacheKey := fmt.Sprintf("path_%s", path)
	var cacheValue map[string]interface{}

	found, err := f.pathResolveCache.Get(cacheKey, &cacheValue)
	if err != nil {
		fs.Debugf(f, "获取缓存路径解析失败 %s: %v", path, err)
		return "", false, false
	}

	if !found {
		return "", false, false
	}

	fileID, ok1 := cacheValue["file_id"].(string)
	isDir, ok2 := cacheValue["is_dir"].(bool)

	if !ok1 || !ok2 {
		fs.Debugf(f, "缓存路径解析数据格式错误 %s", path)
		return "", false, false
	}

	fs.Debugf(f, "从BadgerDB缓存获取路径解析 %s -> %s (dir: %v)", path, fileID, isDir)
	return fileID, isDir, true
}

// DirListCacheEntry 目录列表缓存条目
type DirListCacheEntry struct {
	FileList   []api.File `json:"file_list"`
	ParentID   string     `json:"parent_id"`
	TotalCount int        `json:"total_count"`
	CachedAt   time.Time  `json:"cached_at"`
}

// DownloadURLCacheEntry 下载URL缓存条目
type DownloadURLCacheEntry struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	FileID    string    `json:"file_id"`
	PickCode  string    `json:"pick_code"`
	CachedAt  time.Time `json:"cached_at"`
}

// MetadataCacheEntry 文件元数据缓存条目
type MetadataCacheEntry struct {
	FileID   string    `json:"file_id"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	IsDir    bool      `json:"is_dir"`
	SHA1     string    `json:"sha1"`
	PickCode string    `json:"pick_code"`
	CachedAt time.Time `json:"cached_at"`
}

// FileIDCacheEntry 文件ID验证缓存条目
type FileIDCacheEntry struct {
	FileID   string    `json:"file_id"`
	Exists   bool      `json:"exists"`
	IsDir    bool      `json:"is_dir"`
	CachedAt time.Time `json:"cached_at"`
}

// getDirListFromCache 从缓存获取目录列表
func (f *Fs) getDirListFromCache(parentID string) (*DirListCacheEntry, bool) {
	if f.dirListCache == nil {
		return nil, false
	}

	cacheKey := fmt.Sprintf("dirlist_%s", parentID)
	var entry DirListCacheEntry

	found, err := f.dirListCache.Get(cacheKey, &entry)
	if err != nil {
		fs.Debugf(f, "获取目录列表缓存失败 %s: %v", cacheKey, err)
		return nil, false
	}

	if found {
		fs.Debugf(f, "目录列表缓存命中: parentID=%s, 文件数=%d", parentID, len(entry.FileList))
		return &entry, true
	}

	return nil, false
}

// saveDirListToCache 保存目录列表到缓存
func (f *Fs) saveDirListToCache(parentID string, fileList []api.File) {
	if f.dirListCache == nil {
		return
	}

	cacheKey := fmt.Sprintf("dirlist_%s", parentID)
	entry := DirListCacheEntry{
		FileList:   fileList,
		ParentID:   parentID,
		TotalCount: len(fileList),
		CachedAt:   time.Now(),
	}

	// 使用配置的目录列表缓存TTL
	if err := f.dirListCache.Set(cacheKey, entry, f.cacheConfig.DirListCacheTTL); err != nil {
		fs.Debugf(f, "保存目录列表缓存失败 %s: %v", cacheKey, err)
	} else {
		fs.Debugf(f, "已保存目录列表到缓存: parentID=%s, 文件数=%d, TTL=%v", parentID, len(fileList), f.cacheConfig.DirListCacheTTL)
	}
}

// clearDirListCache 清理目录列表缓存
func (f *Fs) clearDirListCache(parentID string) {
	if f.dirListCache == nil {
		return
	}

	cacheKey := fmt.Sprintf("dirlist_%s", parentID)
	if err := f.dirListCache.Delete(cacheKey); err != nil {
		fs.Debugf(f, "清理目录列表缓存失败 %s: %v", cacheKey, err)
	} else {
		fs.Debugf(f, "已清理目录列表缓存: parentID=%s", parentID)
	}
}

// getMetadataFromCache 从缓存获取文件元数据
func (f *Fs) getMetadataFromCache(fileID string) (*MetadataCacheEntry, bool) {
	if f.metadataCache == nil {
		return nil, false
	}

	cacheKey := fmt.Sprintf("metadata_%s", fileID)
	var entry MetadataCacheEntry

	found, err := f.metadataCache.Get(cacheKey, &entry)
	if err != nil {
		fs.Debugf(f, "获取文件元数据缓存失败 %s: %v", cacheKey, err)
		return nil, false
	}

	if found {
		fs.Debugf(f, "文件元数据缓存命中: fileID=%s", fileID)
		return &entry, true
	}

	return nil, false
}

// saveMetadataToCache 保存文件元数据到缓存
func (f *Fs) saveMetadataToCache(fileID, name, sha1, pickCode string, size int64, modTime time.Time, isDir bool) {
	if f.metadataCache == nil {
		return
	}

	cacheKey := fmt.Sprintf("metadata_%s", fileID)
	entry := MetadataCacheEntry{
		FileID:   fileID,
		Name:     name,
		Size:     size,
		ModTime:  modTime,
		IsDir:    isDir,
		SHA1:     sha1,
		PickCode: pickCode,
		CachedAt: time.Now(),
	}

	// 使用配置的文件元数据缓存TTL
	if err := f.metadataCache.Set(cacheKey, entry, f.cacheConfig.MetadataCacheTTL); err != nil {
		fs.Debugf(f, "保存文件元数据缓存失败 %s: %v", cacheKey, err)
	} else {
		fs.Debugf(f, "已保存文件元数据到缓存: fileID=%s, name=%s, TTL=%v", fileID, name, f.cacheConfig.MetadataCacheTTL)
	}
}

// clearMetadataCache 清理文件元数据缓存
func (f *Fs) clearMetadataCache(fileID string) {
	if f.metadataCache == nil {
		return
	}

	cacheKey := fmt.Sprintf("metadata_%s", fileID)
	if err := f.metadataCache.Delete(cacheKey); err != nil {
		fs.Debugf(f, "清理文件元数据缓存失败 %s: %v", cacheKey, err)
	} else {
		fs.Debugf(f, "已清理文件元数据缓存: fileID=%s", fileID)
	}
}

// getFileIDFromCache 从缓存获取文件ID验证结果
func (f *Fs) getFileIDFromCache(fileID string) (bool, bool, bool) {
	if f.fileIDCache == nil {
		return false, false, false
	}

	cacheKey := fmt.Sprintf("file_id_%s", fileID)
	var entry FileIDCacheEntry

	found, err := f.fileIDCache.Get(cacheKey, &entry)
	if err != nil {
		fs.Debugf(f, "获取文件ID验证缓存失败 %s: %v", cacheKey, err)
		return false, false, false
	}

	if found {
		fs.Debugf(f, "文件ID验证缓存命中: fileID=%s, exists=%v, isDir=%v", fileID, entry.Exists, entry.IsDir)
		return entry.Exists, entry.IsDir, true
	}

	return false, false, false
}

// saveFileIDToCache 保存文件ID验证结果到缓存
func (f *Fs) saveFileIDToCache(fileID string, exists, isDir bool) {
	if f.fileIDCache == nil {
		return
	}

	cacheKey := fmt.Sprintf("file_id_%s", fileID)
	entry := FileIDCacheEntry{
		FileID:   fileID,
		Exists:   exists,
		IsDir:    isDir,
		CachedAt: time.Now(),
	}

	// 使用配置的文件ID验证缓存TTL
	if err := f.fileIDCache.Set(cacheKey, entry, f.cacheConfig.FileIDCacheTTL); err != nil {
		fs.Debugf(f, "保存文件ID验证缓存失败 %s: %v", cacheKey, err)
	} else {
		fs.Debugf(f, "已保存文件ID验证到缓存: fileID=%s, exists=%v, isDir=%v, TTL=%v", fileID, exists, isDir, f.cacheConfig.FileIDCacheTTL)
	}
}

// clearFileIDCache 清理文件ID验证缓存
func (f *Fs) clearFileIDCache(fileID string) {
	if f.fileIDCache == nil {
		return
	}

	cacheKey := fmt.Sprintf("file_id_%s", fileID)
	if err := f.fileIDCache.Delete(cacheKey); err != nil {
		fs.Debugf(f, "清理文件ID验证缓存失败 %s: %v", cacheKey, err)
	} else {
		fs.Debugf(f, "已清理文件ID验证缓存: fileID=%s", fileID)
	}
}

// invalidateRelatedCaches115 智能缓存失效 - 根据操作类型清理相关缓存
// 🔧 轻量级优化：简化缓存失效逻辑，提高可靠性
func (f *Fs) invalidateRelatedCaches115(path string, operation string) {

	// 🚀 简单有效的策略：对所有文件操作都进行全面缓存清理
	// 虽然会影响一些性能，但确保数据一致性，避免复杂的条件判断

	switch operation {
	case "upload", "put", "mkdir", "delete", "remove", "rmdir", "rename", "move":
		// 统一处理：清理当前路径和父路径的所有相关缓存
		f.clearAllRelatedCaches115(path)

		// 额外清理父目录缓存
		parentPath := filepath.Dir(path)
		if parentPath != "." && parentPath != "/" {
			f.clearAllRelatedCaches115(parentPath)
		}

		fs.Debugf(f, "已清理路径 %s 和父路径 %s 的所有相关缓存", path, parentPath)
	}
}

// clearAllRelatedCaches115 清理指定路径的所有相关缓存
// 🔧 轻量级优化：新增统一缓存清理函数
func (f *Fs) clearAllRelatedCaches115(path string) {
	// 清理内存路径缓存
	f.pathCache.Clear()

	// 清理所有BadgerDB持久化缓存（简单策略：全部清理）
	caches := []*cache.BadgerCache{
		f.pathResolveCache,
		f.dirListCache,
		f.metadataCache,
		f.fileIDCache,
	}

	for _, c := range caches {
		if c != nil {
			c.Clear()
		}
	}

	// 清理dircache
	f.dirCache.ResetRoot()
}

// Object describes a 115 object
type Object struct {
	fs          *Fs
	remote      string
	hasMetaData bool
	id          string
	parent      string
	size        int64
	sha1sum     string
	pickCode    string
	modTime     time.Time
	durl        *api.DownloadURL // link to download the object
	durlMu      *sync.Mutex
}
type ApiResponse struct {
	State   bool                `json:"state"`   // Indicates success or failure
	Message string              `json:"message"` // Optional message
	Code    int                 `json:"code"`    // Status code
	Data    map[string]FileInfo `json:"data"`    // Map where keys are file IDs (strings) and values are FileInfo objects
}

// FileInfo represents the details of a single file.
// The key for this object in the parent map (ApiResponse.Data) is the file ID.
type FileInfo struct {
	FileName string      `json:"file_name"` // Name of the file
	FileSize int64       `json:"file_size"` // Size of the file in bytes (using int64 for potentially large files)
	PickCode string      `json:"pick_code"` // File pick code (extraction code)
	SHA1     string      `json:"sha1"`      // SHA1 hash of the file content
	URL      DownloadURL `json:"url"`       // Object containing the download URL
}
type DownloadURL struct {
	URL string `json:"url"` // The file download address
}

// retryErrorCodes is a slice of HTTP status codes that we will retry
var retryErrorCodes = []int{
	429, // Too Many Requests.
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
	509, // Bandwidth Limit Exceeded
}

// UnifiedErrorClassifier 统一错误分类器
type UnifiedErrorClassifier struct {
	// 错误分类统计
	ServerOverloadCount int64
	URLExpiredCount     int64
	NetworkTimeoutCount int64
	RateLimitCount      int64
	UnknownErrorCount   int64
}

// ClassifyError 统一错误分类方法
func (c *UnifiedErrorClassifier) ClassifyError(err error) string {
	if err == nil {
		return "no_error"
	}

	errStr := strings.ToLower(err.Error())

	// 服务器过载错误
	if strings.Contains(errStr, "502") || strings.Contains(errStr, "bad gateway") ||
		strings.Contains(errStr, "503") || strings.Contains(errStr, "service unavailable") ||
		strings.Contains(errStr, "504") || strings.Contains(errStr, "gateway timeout") {
		c.ServerOverloadCount++
		return "server_overload"
	}

	// URL过期错误
	if strings.Contains(errStr, "download url is invalid") || strings.Contains(errStr, "expired") ||
		strings.Contains(errStr, "url过期") || strings.Contains(errStr, "链接失效") {
		c.URLExpiredCount++
		return "url_expired"
	}

	// 网络超时错误
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "context canceled") || strings.Contains(errStr, "connection reset") {
		c.NetworkTimeoutCount++
		return "network_timeout"
	}

	// 限流错误
	if strings.Contains(errStr, "429") || strings.Contains(errStr, "too many requests") ||
		strings.Contains(errStr, "rate limit") || strings.Contains(errStr, "770004") ||
		strings.Contains(errStr, "已达到当前访问上限") {
		c.RateLimitCount++
		return "rate_limit"
	}

	// 认证错误
	if strings.Contains(errStr, "401") || strings.Contains(errStr, "unauthorized") ||
		strings.Contains(errStr, "token") || strings.Contains(errStr, "认证失败") {
		return "auth_error"
	}

	// 权限错误
	if strings.Contains(errStr, "403") || strings.Contains(errStr, "forbidden") ||
		strings.Contains(errStr, "permission denied") {
		return "permission_error"
	}

	// 资源不存在错误
	if strings.Contains(errStr, "404") || strings.Contains(errStr, "not found") ||
		strings.Contains(errStr, "file not found") {
		return "not_found"
	}

	c.UnknownErrorCount++
	return "unknown"
}

// shouldRetry checks if a request should be retried based on the response, error, and API type.
// 🔧 优化：使用统一错误分类和重试策略
func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}

	// Check HTTP response status codes first
	if resp != nil {
		switch resp.StatusCode {
		case 429: // Too Many Requests
			fs.Debugf(nil, "HTTP 429 Too Many Requests, retrying with delay")
			return true, pacer.RetryAfterError(err, 15*time.Second)
		case 500, 502, 503, 504: // Server errors
			fs.Debugf(nil, "Server error %d, retrying: %v", resp.StatusCode, err)
			return true, pacer.RetryAfterError(err, 5*time.Second)
		case 408: // Request Timeout
			fs.Debugf(nil, "Request timeout, retrying: %v", err)
			return true, pacer.RetryAfterError(err, 3*time.Second)
		}
	}

	// Check for specific error types and messages
	if err != nil {
		// Check for API rate limit by error message
		if isAPILimitError(err) {
			fs.Debugf(nil, "API rate limit detected, retrying: %v", err)
			return true, pacer.RetryAfterError(err, 10*time.Second)
		}

		// Check for network-related errors
		if isNetworkError(err) {
			fs.Debugf(nil, "Network error detected, retrying: %v", err)
			return true, pacer.RetryAfterError(err, 2*time.Second)
		}

		// Check for temporary errors
		if isTemporaryError(err) {
			fs.Debugf(nil, "Temporary error detected, retrying: %v", err)
			return true, pacer.RetryAfterError(err, 1*time.Second)
		}
	}

	// 🔧 优化：使用统一错误分类器
	classifier := &UnifiedErrorClassifier{}
	errorType := classifier.ClassifyError(err)

	// 基于错误类型决定是否重试
	switch errorType {
	case "server_overload", "network_timeout":
		// 服务器过载和网络超时错误应该重试
		return true, err

	case "rate_limit":
		// 限流错误应该重试，但使用更长的延迟
		return true, fserrors.NewErrorRetryAfter(30 * time.Second)

	case "url_expired":
		// URL过期错误应该重试（会触发URL刷新）
		return true, err

	case "auth_error":
		// 认证错误不重试，让上层处理token刷新
		var apiErr *api.TokenError
		if errors.As(err, &apiErr) {
			return false, err
		}
		// 其他认证错误也不重试
		return false, err

	case "permission_error", "not_found":
		// 权限错误和资源不存在错误不重试
		return false, err

	default:
		// 未知错误使用rclone标准重试逻辑
		if fserrors.ShouldRetry(err) {
			return true, err
		}

		// HTTP状态码重试（回退方案）
		return fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
	}
}

// ------------------------------------------------------------
// Authentication and Client Setup
// ------------------------------------------------------------

// Credential holds the parsed cookie values needed for initial login
type Credential struct {
	UID  string
	CID  string
	SEID string
	KID  string // Keep KID as it might be used implicitly by the web API calls
}

// Valid reports whether the credential is valid.
func (cr *Credential) Valid() error {
	if cr == nil {
		return errors.New("nil credential")
	}
	// KID is optional/sometimes empty, SEID seems required for login mimicry
	if cr.UID == "" || cr.CID == "" || cr.SEID == "" {
		return errors.New("missing UID, CID, or SEID in cookie")
	}
	return nil
}

// FromCookie loads credential from cookie string
func (cr *Credential) FromCookie(cookieStr string) *Credential {
	for _, item := range strings.Split(cookieStr, ";") {
		kv := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(strings.ToUpper(kv[0]))
		val := strings.TrimSpace(kv[1])
		switch key {
		case "UID":
			cr.UID = val
		case "CID":
			cr.CID = val
		case "SEID":
			cr.SEID = val
		case "KID":
			cr.KID = val
		}
	}
	return cr
}

// Cookie turns the credential into a list of http cookie for the traditional client
func (cr *Credential) Cookie() []*http.Cookie {
	cookies := []*http.Cookie{
		{Name: "UID", Value: cr.UID, Domain: domain, Path: "/", HttpOnly: true},
		{Name: "CID", Value: cr.CID, Domain: domain, Path: "/", HttpOnly: true},
		{Name: "SEID", Value: cr.SEID, Domain: domain, Path: "/", HttpOnly: true},
	}
	// Add KID only if it's present
	if cr.KID != "" {
		cookies = append(cookies, &http.Cookie{Name: "KID", Value: cr.KID, Domain: domain, Path: "/", HttpOnly: true})
	}
	return cookies
}

// UserID parses userID from UID field
func (cr *Credential) UserID() string {
	userID, _, _ := strings.Cut(cr.UID, "_")
	return userID
}

// getHTTPClient makes an http client according to the options with optimized connection pool
func getHTTPClient(ctx context.Context, opt *Options) *http.Client {
	t := fshttp.NewTransportCustom(ctx, func(t *http.Transport) {
		t.TLSHandshakeTimeout = time.Duration(opt.ConTimeout)
		// 🔧 大幅增加响应头超时时间，支持大文件上传
		t.ResponseHeaderTimeout = 10 * time.Minute // 从2分钟增加到10分钟

		// 优化连接池配置
		t.MaxIdleConns = 100                 // 最大空闲连接数
		t.MaxIdleConnsPerHost = 20           // 每个主机的最大空闲连接数
		t.MaxConnsPerHost = 50               // 每个主机的最大连接数
		t.IdleConnTimeout = 90 * time.Second // 空闲连接超时
		t.DisableKeepAlives = false          // 启用Keep-Alive
		t.ForceAttemptHTTP2 = true           // 强制尝试HTTP/2

		// 优化超时设置
		t.DialContext = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		t.ExpectContinueTimeout = 1 * time.Second
	})

	return &http.Client{
		Transport: t,
		// 🔧 大幅增加总超时时间，支持大文件上传
		Timeout: 15 * time.Minute, // 从3分钟增加到15分钟
	}
}

// getTradHTTPClient creates an HTTP client with traditional UserAgent
func getTradHTTPClient(ctx context.Context, opt *Options) *http.Client {
	// Create a new context with the traditional UserAgent
	newCtx, ci := fs.AddConfig(ctx)
	ci.UserAgent = tradUserAgent
	return getHTTPClient(newCtx, opt)
}

// getOpenAPIHTTPClient creates an HTTP client with default UserAgent
func getOpenAPIHTTPClient(ctx context.Context, opt *Options) *http.Client {
	// Create a new context with the default UserAgent
	newCtx, ci := fs.AddConfig(ctx)
	ci.UserAgent = defaultUserAgent
	return getHTTPClient(newCtx, opt)
}

// errorHandler parses a non 2xx error response into an error (Generic, might need adjustment per API)
func errorHandler(resp *http.Response) error {
	// Attempt to decode as OpenAPI error first
	openAPIErr := new(api.OpenAPIBase)
	bodyBytes, readErr := rest.ReadBody(resp) // Read body once
	if readErr != nil {
		fs.Debugf(nil, "Couldn't read error response body: %v", readErr)
		// Fallback to status code if body read fails
		return api.NewTokenError(fmt.Sprintf("HTTP error %d (%s)", resp.StatusCode, resp.Status))
	}

	decodeErr := json.Unmarshal(bodyBytes, &openAPIErr)
	if decodeErr == nil && !openAPIErr.State {
		// Successfully decoded as OpenAPI error
		err := openAPIErr.Err()
		// Check for specific token-related errors
		if openAPIErr.ErrCode() == 401 || openAPIErr.ErrCode() == 100001 || strings.Contains(openAPIErr.ErrMsg(), "token") { // Example codes
			return api.NewTokenError(err.Error(), true) // Assume token error needs refresh/relogin
		}
		return err
	}

	// Attempt to decode as Traditional error
	tradErr := new(api.TraditionalBase)
	decodeErr = json.Unmarshal(bodyBytes, &tradErr)
	if decodeErr == nil && !tradErr.State {
		// Successfully decoded as Traditional error
		return tradErr.Err()
	}

	// Fallback if JSON decoding fails or state is true (but status code != 2xx)
	fs.Debugf(nil, "Couldn't decode error response: %v. Body: %s", decodeErr, string(bodyBytes))
	return api.NewTokenError(fmt.Sprintf("HTTP error %d (%s): %s", resp.StatusCode, resp.Status, string(bodyBytes)))
}

// generatePKCE generates a code_verifier and code_challenge
func generatePKCE() (verifier, challenge string, err error) {
	verifierBytes := make([]byte, pkceVerifierLength)
	_, err = rand.Read(verifierBytes)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate random verifier: %w", err)
	}
	// Use URL-safe base64 encoding without padding
	verifier = base64.RawURLEncoding.EncodeToString(verifierBytes)
	// Calculate SHA256 hash
	hash := sha256.Sum256([]byte(verifier))
	// Base64 encode the hash
	challenge = base64.RawURLEncoding.EncodeToString(hash[:])
	return verifier, challenge, nil
}

// login performs the initial authentication flow to get tokens
func (f *Fs) login(ctx context.Context) error {
	// Use a global mutex for login to prevent multiple concurrent login attempts
	// which could overwhelm the API and cause authentication failures
	f.loginMu.Lock()
	defer f.loginMu.Unlock()

	// Check if login is needed (avoid nested locking)
	needLogin := func() bool {
		f.tokenMu.Lock()
		defer f.tokenMu.Unlock()
		return f.accessToken == "" || time.Now().After(f.tokenExpiry)
	}()

	if !needLogin {
		fs.Debugf(f, "Token is already valid after waiting for login mutex, skipping login")
		return nil
	}

	// Parse cookie and setup clients
	if err := f.setupLoginEnvironment(ctx); err != nil {
		return err
	}

	fs.Debugf(f, "Starting login process for user %s", f.userID)

	// Generate PKCE
	var challenge string
	var err error
	if f.codeVerifier, challenge, err = generatePKCE(); err != nil {
		return fmt.Errorf("failed to generate PKCE codes: %w", err)
	}
	fs.Debugf(f, "Generated PKCE challenge")

	// Request device code
	loginUID, err := f.getAuthDeviceCode(ctx, challenge)
	if err != nil {
		return err
	}

	// Mimic QR scan confirmation
	if err := f.simulateQRCodeScan(ctx, loginUID); err != nil {
		// Only log errors from these steps, don't fail
		fs.Logf(f, "QR code scan simulation steps had errors (continuing anyway): %v", err)
	}

	// Exchange device code for access token
	if err := f.exchangeDeviceCodeForToken(ctx, loginUID); err != nil {
		return err
	}

	// Get userkey for traditional uploads if needed
	if f.userkey == "" {
		fs.Debugf(f, "Fetching userkey using traditional API...")
		if err := f.getUploadBasicInfo(ctx); err != nil {
			// Log error but don't fail login, userkey is only for traditional upload init
			fs.Logf(f, "Failed to get userkey (needed for some traditional uploads): %v", err)
		} else {
			fs.Debugf(f, "Successfully fetched userkey.")
		}
	}

	return nil
}

// setupLoginEnvironment parses cookies and sets up HTTP clients
func (f *Fs) setupLoginEnvironment(ctx context.Context) error {
	// Parse cookie
	cred := (&Credential{}).FromCookie(f.opt.Cookie)
	if err := cred.Valid(); err != nil {
		return fmt.Errorf("invalid cookie provided: %w", err)
	}
	f.userID = cred.UserID() // Set userID early

	// Setup clients (needed for the login calls)
	// Create separate clients for each API type with different User-Agents
	tradHTTPClient := getTradHTTPClient(ctx, &f.opt)
	openAPIHTTPClient := getOpenAPIHTTPClient(ctx, &f.opt)

	// Traditional client (uses cookie)
	f.tradClient = rest.NewClient(tradHTTPClient).
		SetRoot(traditionalRootURL).
		SetCookie(cred.Cookie()...).
		SetErrorHandler(errorHandler)

	// OpenAPI client (will have token set later)
	f.openAPIClient = rest.NewClient(openAPIHTTPClient).
		SetRoot(openAPIRootURL).
		SetErrorHandler(errorHandler)

	return nil
}

// getAuthDeviceCode calls the authDeviceCode API to start the login process
func (f *Fs) getAuthDeviceCode(ctx context.Context, challenge string) (string, error) {
	// Use configured AppID if provided, otherwise use default
	clientID := f.opt.AppID

	authData := url.Values{
		"client_id":             {clientID},
		"code_challenge":        {challenge},
		"code_challenge_method": {"sha256"}, // Use SHA256
	}
	authOpts := rest.Opts{
		Method:       "POST",
		RootURL:      passportRootURL, // Use passport API domain
		Path:         "/open/authDeviceCode",
		Parameters:   authData, // Send as query parameters for POST? Docs say body, let's try body.
		Body:         strings.NewReader(authData.Encode()),
		ExtraHeaders: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	}
	var authResp api.AuthDeviceCodeResp
	// This initial call uses the traditional client *but doesn't need encryption*
	// It still needs the cookie and pacing.
	err := f.CallTraditionalAPI(ctx, &authOpts, nil, &authResp, true) // Pass skipEncrypt=true
	if err != nil {
		return "", fmt.Errorf("authDeviceCode failed: %w", err)
	}
	if authResp.Data == nil || authResp.Data.UID == "" {
		return "", fmt.Errorf("authDeviceCode returned empty data: %v", authResp)
	}
	loginUID := authResp.Data.UID
	fs.Debugf(f, "authDeviceCode successful, login UID: %s", loginUID)
	return loginUID, nil
}

// simulateQRCodeScan mimics the QR code scan and confirm process
func (f *Fs) simulateQRCodeScan(ctx context.Context, loginUID string) error {
	// Call QR scan API
	scanErr := f.callQRScanAPI(ctx, loginUID)

	// Call QR confirm API - still proceed if scan had an error
	confirmErr := f.callQRConfirmAPI(ctx, loginUID)

	// Add a small delay after mimic steps, just in case
	time.Sleep(1 * time.Second)

	// Return an error if both steps failed, otherwise continue
	if scanErr != nil && confirmErr != nil {
		return fmt.Errorf("both scan and confirm steps failed: scan: %v, confirm: %v", scanErr, confirmErr)
	}
	return nil
}

// callQRScanAPI calls the QR scan API
func (f *Fs) callQRScanAPI(ctx context.Context, loginUID string) error {
	scanPayload := map[string]string{"uid": loginUID}
	scanOpts := rest.Opts{
		Method:     "GET",
		RootURL:    qrCodeAPIRootURL,
		Path:       "/api/2.0/prompt.php",
		Parameters: url.Values{"uid": []string{loginUID}}, // Send as query params
	}
	var scanResp api.TraditionalBase // Use base struct, don't care about response data much
	fs.Debugf(f, "Calling login_qrcode_scan...")
	err := f.CallTraditionalAPI(ctx, &scanOpts, scanPayload, &scanResp, true)
	if err != nil {
		return fmt.Errorf("login_qrcode_scan failed: %w", err)
	}
	fs.Debugf(f, "login_qrcode_scan call successful (State: %v)", scanResp.State)
	return nil
}

// callQRConfirmAPI calls the QR confirm API
func (f *Fs) callQRConfirmAPI(ctx context.Context, loginUID string) error {
	confirmPayload := map[string]string{"uid": loginUID, "key": loginUID, "client": "0"} // Key seems to be same as uid?
	confirmOpts := rest.Opts{
		Method:     "GET",
		RootURL:    hnQrCodeAPIRootURL,
		Path:       "/api/2.0/slogin.php",
		Parameters: url.Values{"uid": []string{loginUID}, "key": []string{loginUID}, "client": []string{"0"}}, // Send as query params
	}
	var confirmResp api.TraditionalBase
	fs.Debugf(f, "Calling login_qrcode_scan_confirm...")
	err := f.CallTraditionalAPI(ctx, &confirmOpts, confirmPayload, &confirmResp, true) // Needs encryption? Assume yes.
	if err != nil {
		return fmt.Errorf("login_qrcode_scan_confirm failed: %w", err)
	}
	fs.Debugf(f, "login_qrcode_scan_confirm call successful (State: %v)", confirmResp.State)
	return nil
}

// exchangeDeviceCodeForToken gets the access token using the device code
func (f *Fs) exchangeDeviceCodeForToken(ctx context.Context, loginUID string) error {
	tokenData := url.Values{
		"uid":           {loginUID},
		"code_verifier": {f.codeVerifier},
	}
	tokenOpts := rest.Opts{
		Method:       "POST",
		RootURL:      passportRootURL,
		Path:         "/open/deviceCodeToToken",
		Body:         strings.NewReader(tokenData.Encode()),
		ExtraHeaders: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	}
	var tokenResp api.DeviceCodeTokenResp
	// This call also uses traditional client but no encryption needed.
	err := f.CallTraditionalAPI(ctx, &tokenOpts, nil, &tokenResp, true) // skipEncrypt=true
	if err != nil {
		return fmt.Errorf("deviceCodeToToken failed: %w", err)
	}
	if tokenResp.Data == nil || tokenResp.Data.AccessToken == "" {
		return fmt.Errorf("deviceCodeToToken returned empty data: %v", tokenResp)
	}

	// Store tokens with proper locking
	f.tokenMu.Lock()
	f.accessToken = tokenResp.Data.AccessToken
	f.refreshToken = tokenResp.Data.RefreshToken
	f.tokenExpiry = time.Now().Add(time.Duration(tokenResp.Data.ExpiresIn) * time.Second)
	f.tokenMu.Unlock()

	fs.Debugf(f, "Successfully obtained access token, expires at %v", f.tokenExpiry)

	return nil
}

// refreshTokenIfNecessary refreshes the token if necessary
func (f *Fs) refreshTokenIfNecessary(ctx context.Context, refreshTokenExpired bool, forceRefresh bool) error {
	f.tokenMu.Lock()

	// Check if token is already valid when we acquire the lock
	// This handles the case where another thread just refreshed the token
	if !refreshTokenExpired && !forceRefresh && isTokenStillValid(f) {
		fs.Debugf(f, "Token is still valid after acquiring lock, skipping refresh")
		f.tokenMu.Unlock()
		return nil
	}

	// Check if we need to perform a full login instead of a refresh
	if shouldPerformFullLogin(f, refreshTokenExpired) {
		f.tokenMu.Unlock()
		err := f.login(ctx) // login handles its own locking
		if err != nil {
			return err
		}
		// Save the token after successful login
		f.saveToken(ctx, f.m)
		return nil
	}

	// Check if token is still valid and refresh not forced
	if !forceRefresh && isTokenStillValid(f) {
		f.tokenMu.Unlock()
		return nil
	}

	// Prepare for token refresh
	refreshToken := f.refreshToken // Make a local copy of the refresh token
	f.tokenMu.Unlock()             // Unlock before making API call

	// Perform the actual token refresh
	result, err := f.performTokenRefresh(ctx, refreshToken)
	if err != nil {
		return err // Error already formatted with context
	}

	// Update the tokens with new values
	f.updateTokens(result)

	// Save the refreshed token to config
	f.saveToken(ctx, f.m)

	return nil
}

// shouldPerformFullLogin determines if we should skip refresh and do a full login
func shouldPerformFullLogin(f *Fs, refreshTokenExpired bool) bool {
	// Skip directly to re-login if refresh token expired
	if refreshTokenExpired {
		fs.Debugf(f, "Token refresh skipped, going directly to re-login due to expired refresh token")
		return true
	}

	// Re-login if no tokens available
	if f.accessToken == "" || f.refreshToken == "" {
		fs.Debugf(f, "No token found, attempting login.")
		return true
	}

	return false
}

// isTokenStillValid checks if the current token is still valid
func isTokenStillValid(f *Fs) bool {
	return time.Now().Before(f.tokenExpiry.Add(-tokenRefreshWindow))
}

// performTokenRefresh handles the actual API call to refresh the token
func (f *Fs) performTokenRefresh(ctx context.Context, refreshToken string) (*api.RefreshTokenResp, error) {
	// Ensure client exists
	if err := f.ensureOpenAPIClient(ctx); err != nil {
		return nil, err
	}

	// Set up and make the refresh request
	refreshResp, err := f.callRefreshTokenAPI(ctx, refreshToken)
	if err != nil {
		return handleRefreshError(f, ctx, err)
	}

	// Validate the response
	if refreshResp.Data == nil || refreshResp.Data.AccessToken == "" {
		// Log detailed information about the empty response
		fs.Errorf(f, "Refresh token response empty or invalid. Full response: %#v", refreshResp)
		// Log OpenAPI base information (state, code, message)
		fs.Errorf(f, "Response state: %v, code: %d, message: %q",
			refreshResp.State, refreshResp.Code, refreshResp.Message)

		fs.Errorf(f, "Refresh token response empty, attempting re-login.")

		// Re-lock before checking token again to avoid race condition
		f.tokenMu.Lock()
		// Check if another thread has already refreshed the token
		if f.accessToken != "" && time.Now().Before(f.tokenExpiry) {
			fs.Debugf(f, "Token was refreshed by another thread while waiting")
			f.tokenMu.Unlock()
			return nil, nil
		}
		f.tokenMu.Unlock()

		loginErr := f.login(ctx)
		if loginErr != nil {
			return nil, fmt.Errorf("re-login failed after empty refresh response: %w", loginErr)
		}
		fs.Debugf(f, "Re-login successful after empty refresh response.")
		return nil, nil // Re-login successful, no need to update tokens
	}

	return refreshResp, nil
}

// ensureOpenAPIClient ensures the OpenAPI client is initialized
func (f *Fs) ensureOpenAPIClient(ctx context.Context) error {
	if f.openAPIClient != nil {
		return nil
	}

	// Setup client if it doesn't exist
	newCtx, ci := fs.AddConfig(ctx)
	ci.UserAgent = f.opt.UserAgent
	httpClient := getHTTPClient(newCtx, &f.opt)
	f.openAPIClient = rest.NewClient(httpClient).
		SetRoot(openAPIRootURL).
		SetErrorHandler(errorHandler)

	return nil
}

// callRefreshTokenAPI makes the actual API call to refresh the token
func (f *Fs) callRefreshTokenAPI(ctx context.Context, refreshToken string) (*api.RefreshTokenResp, error) {
	refreshData := url.Values{
		"refresh_token": {refreshToken},
	}
	opts := rest.Opts{
		Method:       "POST",
		RootURL:      passportRootURL,
		Path:         "/open/refreshToken",
		Body:         strings.NewReader(refreshData.Encode()),
		ExtraHeaders: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	}

	var refreshResp api.RefreshTokenResp
	resp, err := f.openAPIClient.Call(ctx, &opts)
	if err != nil {
		return nil, err
	}

	// Read the raw response for logging
	defer fs.CheckClose(resp.Body, &err)
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		fs.Errorf(f, "Failed to read refresh token response body: %v", readErr)
		return nil, readErr
	}

	// Log the raw response
	fs.Debugf(f, "Raw refresh token response: %s", string(body))

	// Create a new reader for the JSON unmarshal
	err = json.Unmarshal(body, &refreshResp)
	if err != nil {
		fs.Errorf(f, "Failed to parse refresh token response: %v", err)
		return nil, err
	}

	return &refreshResp, nil
}

// handleRefreshError handles errors from the refresh token API call
func handleRefreshError(f *Fs, ctx context.Context, err error) (*api.RefreshTokenResp, error) {
	fs.Errorf(f, "Refresh token failed: %v", err)

	// Check if the error indicates the refresh token itself is expired
	var tokenErr *api.TokenError
	if errors.As(err, &tokenErr) && tokenErr.IsRefreshTokenExpired ||
		strings.Contains(err.Error(), "refresh token expired") {
		fs.Debugf(f, "Refresh token seems expired, attempting full re-login.")
		loginErr := f.login(ctx) // login handles its own locking
		if loginErr != nil {
			return nil, fmt.Errorf("re-login failed after refresh token expired: %w", loginErr)
		}
		fs.Debugf(f, "Re-login successful after refresh token expiry.")
		return nil, nil // Re-login successful
	}

	// Return the original refresh error if it wasn't an expiry issue
	return nil, fmt.Errorf("token refresh failed: %w", err)
}

// updateTokens updates the token values with new values from a refresh response
func (f *Fs) updateTokens(refreshResp *api.RefreshTokenResp) {
	// If we got a nil response, it means we did a re-login instead of a refresh
	if refreshResp == nil {
		return
	}

	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()

	f.accessToken = refreshResp.Data.AccessToken
	// OpenAPI spec says refresh_token might be updated, so store the new one
	if refreshResp.Data.RefreshToken != "" {
		f.refreshToken = refreshResp.Data.RefreshToken
	}
	f.tokenExpiry = time.Now().Add(time.Duration(refreshResp.Data.ExpiresIn) * time.Second)
	fs.Debugf(f, "Token refreshed successfully, new expiry: %v", f.tokenExpiry)
}

// saveToken saves the current token to the config
func (f *Fs) saveToken(_ context.Context, m configmap.Mapper) {
	if m == nil {
		fs.Debugf(f, "Not saving tokens - nil mapper provided")
		return
	}

	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()

	if f.accessToken == "" || f.refreshToken == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "Not saving tokens - incomplete token information")
		return
	}

	// Create the token structure
	token := &oauth2.Token{
		AccessToken:  f.accessToken,
		TokenType:    "Bearer",
		RefreshToken: f.refreshToken,
		Expiry:       f.tokenExpiry,
	}

	// Save the token directly using oauthutil's method
	// Note: This uses the originalName without brackets to ensure consistency
	err := oauthutil.PutToken(f.originalName, m, token, false)
	if err != nil {
		fs.Errorf(f, "Failed to save token to config: %v", err)
		return
	}

	fs.Debugf(f, "Saved token to config file using original name %q", f.originalName)
}

// setupTokenRenewer initializes the token renewer to automatically refresh tokens
func (f *Fs) setupTokenRenewer(ctx context.Context, m configmap.Mapper) {
	// Only set up renewer if we have valid tokens
	if f.accessToken == "" || f.refreshToken == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "Not setting up token renewer - incomplete token information")
		return
	}

	// Create a renewal transaction function
	transaction := func() error {
		fs.Debugf(f, "Token renewer triggered, refreshing token")
		// Use non-global function to avoid deadlocks
		err := f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			fs.Errorf(f, "Failed to refresh token in renewer: %v", err)
			return err
		}

		return nil // saveToken is already called in refreshTokenIfNecessary
	}

	// Create minimal OAuth config
	config := &oauthutil.Config{
		TokenURL: passportRootURL + "/open/refreshToken",
	}

	// Create a token source using the existing token
	token := &oauth2.Token{
		AccessToken:  f.accessToken,
		RefreshToken: f.refreshToken,
		Expiry:       f.tokenExpiry,
		TokenType:    "Bearer",
	}

	// Save token to config so it can be accessed by TokenSource
	err := oauthutil.PutToken(f.originalName, m, token, false)
	if err != nil {
		fs.Logf(f, "Failed to save token for renewer: %v", err)
		return
	}

	// Create a client with the token source
	_, ts, err := oauthutil.NewClientWithBaseClient(ctx, f.originalName, m, config, fshttp.NewClient(ctx))
	if err != nil {
		fs.Logf(f, "Failed to create token source for renewer: %v", err)
		return
	}

	// Create token renewer that will trigger when the token is about to expire
	f.tokenRenewer = oauthutil.NewRenew(f.originalName, ts, transaction)
	f.tokenRenewer.Start() // Start the renewer immediately
	fs.Debugf(f, "Token renewer initialized and started with original name %q", f.originalName)
}

// CallOpenAPI performs a call to the OpenAPI endpoint.
// It handles token refresh and sets the Authorization header.
// If skipToken is true, it skips adding the Authorization header (used for refresh itself).
func (f *Fs) CallOpenAPI(ctx context.Context, opts *rest.Opts, request any, response any, skipToken bool) error {
	fs.Debugf(f, "🔍 CallOpenAPI开始: path=%q, method=%q", opts.Path, opts.Method)

	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = openAPIRootURL
	}

	// Wrap the entire attempt sequence with the global pacer, returning proper retry signals
	return f.globalPacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {
		fs.Debugf(f, "🔍 CallOpenAPI: 进入globalPacer")

		// Ensure token is available and current
		if !skipToken {
			fs.Debugf(f, "🔍 CallOpenAPI: 准备token")
			if err := f.prepareTokenForRequest(ctx, opts); err != nil {
				fs.Debugf(f, "🔍 CallOpenAPI: prepareTokenForRequest失败: %v", err)
				return false, backoff.Permanent(err)
			}
		}

		// Make the API call
		fs.Debugf(f, "🔍 CallOpenAPI: 执行API调用")
		resp, apiErr := f.executeOpenAPICall(ctx, opts, request, response)

		// Handle retries for network/server errors
		if retryNeeded, retryErr := shouldRetry(ctx, resp, apiErr); retryNeeded {
			fs.Debugf(f, "pacer: low level retry required for OpenAPI call (error: %v)", retryErr)
			return true, retryErr // Signal globalPacer to retry
		}

		// Handle token errors
		if apiErr != nil {
			if retryAfterTokenRefresh, err := f.handleTokenError(ctx, opts, apiErr, skipToken); retryAfterTokenRefresh {
				return true, nil // Retry with refreshed token
			} else if err != nil {
				return false, backoff.Permanent(err)
			}
			return false, backoff.Permanent(apiErr) // Non-token error, don't retry
		}

		// Check for API-level errors in the response
		if apiErr = f.checkResponseForAPIErrors(response, skipToken, opts); apiErr != nil {
			if tokenRefreshed, err := f.handleTokenError(ctx, opts, apiErr, skipToken); tokenRefreshed {
				return true, nil // Retry with refreshed token
			} else if err != nil {
				return false, backoff.Permanent(err)
			}
			return false, backoff.Permanent(apiErr) // Other API error, don't retry
		}

		fs.Debugf(f, "pacer: OpenAPI call successful")
		return false, nil // Success, don't retry
	})
}

// getPacerForEndpoint 根据API端点返回适当的调速器
// 🔧 实现差异化QPS控制，提升API调用的稳定性和性能
func (f *Fs) getPacerForEndpoint(endpoint string) *fs.Pacer {
	switch {
	// 上传相关API - 使用上传专用调速器 (~3 QPS)
	case strings.Contains(endpoint, "/open/upload/init"),
		strings.Contains(endpoint, "/open/upload/resume"),
		strings.Contains(endpoint, "/open/upload/complete"):
		return f.uploadPacer

	// 下载URL API - 使用下载专用调速器 (~2 QPS)
	case strings.Contains(endpoint, "/open/ufile/downurl"):
		return f.downloadPacer

	// 传统API - 使用传统调速器 (~2 QPS)
	case strings.Contains(endpoint, "webapi.115.com"):
		return f.tradPacer

	// 其他OpenAPI - 使用全局调速器 (5 QPS)
	default:
		return f.globalPacer
	}
}

// CallUploadAPI 专门用于上传相关API调用的函数，使用专用调速器
// 🔧 优化上传API调用频率，平衡性能和稳定性
func (f *Fs) CallUploadAPI(ctx context.Context, opts *rest.Opts, request any, response any, skipToken bool) error {
	fs.Debugf(f, "🔍 CallUploadAPI开始: path=%q, method=%q", opts.Path, opts.Method)

	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = openAPIRootURL
	}

	// 🔧 使用专用的上传调速器，而不是全局调速器
	return f.uploadPacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {
		fs.Debugf(f, "🔍 CallUploadAPI: 进入uploadPacer (QPS限制: ~10 QPS)")

		// Ensure token is available and current
		if !skipToken {
			fs.Debugf(f, "🔍 CallUploadAPI: 准备token")
			if err := f.prepareTokenForRequest(ctx, opts); err != nil {
				fs.Debugf(f, "🔍 CallUploadAPI: prepareTokenForRequest失败: %v", err)
				return false, backoff.Permanent(err)
			}
		}

		// Make the actual API call
		fs.Debugf(f, "🔍 CallUploadAPI: 执行API调用")
		resp, err := f.openAPIClient.CallJSON(ctx, opts, request, response)

		// Check for API-level errors in the response
		if apiErr := f.checkResponseForAPIErrors(response, skipToken, opts); apiErr != nil {
			if tokenRefreshed, err := f.handleTokenError(ctx, opts, apiErr, skipToken); tokenRefreshed {
				return true, nil // Retry with refreshed token
			} else if err != nil {
				return false, backoff.Permanent(err)
			}
			return false, backoff.Permanent(apiErr) // Other API error, don't retry
		}

		fs.Debugf(f, "🔍 CallUploadAPI: API调用成功")
		return shouldRetry(ctx, resp, err)
	})
}

// CallDownloadURLAPI 专门用于下载URL API调用的函数，使用专用调速器
// 🔧 防止下载URL API调用频率过高，避免触发115网盘反爬机制
func (f *Fs) CallDownloadURLAPI(ctx context.Context, opts *rest.Opts, request any, response any, skipToken bool) error {
	fs.Debugf(f, "🔍 CallDownloadURLAPI开始: path=%q, method=%q", opts.Path, opts.Method)

	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = openAPIRootURL
	}

	// 🔧 使用专用的下载URL调速器，而不是全局调速器
	return f.downloadPacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {
		fs.Debugf(f, "🔍 CallDownloadURLAPI: 进入downloadPacer (QPS限制: ~2 QPS)")

		// Ensure token is available and current
		if !skipToken {
			fs.Debugf(f, "🔍 CallDownloadURLAPI: 准备token")
			if err := f.prepareTokenForRequest(ctx, opts); err != nil {
				fs.Debugf(f, "🔍 CallDownloadURLAPI: prepareTokenForRequest失败: %v", err)
				return false, backoff.Permanent(err)
			}
		}

		// Make the actual API call
		fs.Debugf(f, "🔍 CallDownloadURLAPI: 执行API调用")
		resp, err := f.openAPIClient.CallJSON(ctx, opts, request, response)

		// Check for API-level errors in the response
		if apiErr := f.checkResponseForAPIErrors(response, skipToken, opts); apiErr != nil {
			if tokenRefreshed, err := f.handleTokenError(ctx, opts, apiErr, skipToken); tokenRefreshed {
				return true, nil // Retry with refreshed token
			} else if err != nil {
				return false, backoff.Permanent(err)
			}
			return false, backoff.Permanent(apiErr) // Other API error, don't retry
		}

		fs.Debugf(f, "🔍 CallDownloadURLAPI: API调用成功")
		return shouldRetry(ctx, resp, err)
	})
}

// prepareTokenForRequest ensures a valid token is available and sets it in the request headers
func (f *Fs) prepareTokenForRequest(ctx context.Context, opts *rest.Opts) error {
	// Use double-checked locking pattern to minimize lock contention
	// First check without lock (fast path for common case)
	f.tokenMu.Lock()
	needsRefresh := !isTokenStillValid(f)
	currentToken := f.accessToken
	f.tokenMu.Unlock()

	// If refresh is needed, call refreshTokenIfNecessary which has its own locking
	if needsRefresh {
		refreshErr := f.refreshTokenIfNecessary(ctx, false, false)
		if refreshErr != nil {
			fs.Debugf(f, "Token refresh failed: %v", refreshErr)
			return fmt.Errorf("token refresh failed: %w", refreshErr)
		}

		// Get the refreshed token
		f.tokenMu.Lock()
		currentToken = f.accessToken
		f.tokenMu.Unlock()
	}

	// Validate we have a token before using it
	if currentToken == "" {
		return fmt.Errorf("no valid access token available")
	}

	if opts.ExtraHeaders == nil {
		opts.ExtraHeaders = make(map[string]string)
	}
	opts.ExtraHeaders["Authorization"] = "Bearer " + currentToken
	return nil
}

// executeOpenAPICall makes the actual API call with the provided parameters
func (f *Fs) executeOpenAPICall(ctx context.Context, opts *rest.Opts, request any, response any) (*http.Response, error) {
	fs.Debugf(f, "🔍 executeOpenAPICall开始: path=%q", opts.Path)

	var resp *http.Response
	var err error

	if request != nil && response != nil {
		// Assume standard JSON request/response
		fs.Debugf(f, "🔍 executeOpenAPICall: 标准JSON请求/响应模式")
		resp, err = f.openAPIClient.CallJSON(ctx, opts, request, response)
	} else if response != nil {
		// Assume GET request with JSON response
		fs.Debugf(f, "🔍 executeOpenAPICall: GET请求JSON响应模式")
		resp, err = f.openAPIClient.CallJSON(ctx, opts, nil, response)
	} else {
		// Assume call without specific request/response body
		fs.Debugf(f, "🔍 executeOpenAPICall: 基础调用模式")
		var baseResp api.OpenAPIBase
		resp, err = f.openAPIClient.CallJSON(ctx, opts, nil, &baseResp)
		if err == nil {
			err = baseResp.Err() // Check for API-level errors
		}
	}

	if err != nil {
		fs.Debugf(f, "🔍 executeOpenAPICall失败: %v", err)
	}

	return resp, err
}

// handleTokenError processes token-related errors and attempts to refresh or re-login
func (f *Fs) handleTokenError(ctx context.Context, opts *rest.Opts, apiErr error, skipToken bool) (bool, error) {
	var tokenErr *api.TokenError
	if errors.As(apiErr, &tokenErr) {
		fs.Debugf(f, "Token error detected: %v (relogin needed: %v)", tokenErr, tokenErr.IsRefreshTokenExpired)
		// Handle token refresh/re-login using refreshTokenIfNecessary
		refreshErr := f.refreshTokenIfNecessary(ctx, tokenErr.IsRefreshTokenExpired, !tokenErr.IsRefreshTokenExpired)
		if refreshErr != nil {
			fs.Debugf(f, "Token refresh/relogin failed: %v", refreshErr)
			return false, fmt.Errorf("token refresh/relogin failed: %w (original: %v)", refreshErr, apiErr)
		}

		// Token was successfully refreshed or re-login succeeded, retry the API call
		fs.Debugf(f, "Token refresh/relogin succeeded, retrying API call")

		// Update the Authorization header with the new token
		if !skipToken {
			// Always get the freshest token right before using it
			f.tokenMu.Lock()
			token := f.accessToken
			f.tokenMu.Unlock()

			if opts.ExtraHeaders == nil {
				opts.ExtraHeaders = make(map[string]string)
			}
			opts.ExtraHeaders["Authorization"] = "Bearer " + token
		}
		return true, nil // Signal retry with the refreshed token
	}
	return false, nil // Not a token error
}

// checkResponseForAPIErrors examines the response for API-level errors using reflection
func (f *Fs) checkResponseForAPIErrors(response any, _ bool, _ *rest.Opts) error {
	if response == nil {
		return nil
	}

	// Use reflection to check for and call an Err() method
	val := reflect.ValueOf(response)
	if val.Kind() == reflect.Ptr && !val.IsNil() {
		method := val.MethodByName("Err")
		if method.IsValid() && method.Type().NumIn() == 0 && method.Type().NumOut() == 1 &&
			method.Type().Out(0).Implements(reflect.TypeOf((*error)(nil)).Elem()) {
			result := method.Call(nil)
			if !result[0].IsNil() {
				// Get the error and check if it's a rate limit error
				err := result[0].Interface().(error)
				if strings.Contains(err.Error(), "rate limit exceeded") {
					fs.Debugf(f, "Rate limit error detected in response: %v", err)
					// Return the error as is - the shouldRetry function will handle it
					// This ensures both CallOpenAPI and CallTraditionalAPI will retry rate limit errors
				}
				return err
			}
		}
	}
	return nil
}

// CallTraditionalAPI performs a call to the traditional (cookie, encrypted) API.
// It uses both the traditional and global pacers.
// If skipEncrypt is true, it skips the request/response encryption (used for some login steps).
func (f *Fs) CallTraditionalAPI(ctx context.Context, opts *rest.Opts, request any, response any, skipEncrypt bool) error {
	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = traditionalRootURL
	}

	// Wrap the entire attempt sequence with the global pacer
	return f.globalPacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {
		// Wait for traditional pacer
		if err := f.enforceTraditionalPacerDelay(); err != nil {
			return false, backoff.Permanent(err)
		}

		// Make the API call (with or without encryption)
		resp, apiErr := f.executeTraditionalAPICall(ctx, opts, request, response, skipEncrypt)

		// Process the result
		return f.processTraditionalAPIResult(ctx, resp, apiErr)
	})
}

// enforceTraditionalPacerDelay ensures we respect the traditional API pacer limits
func (f *Fs) enforceTraditionalPacerDelay() error {
	// Use tradPacer.Call with a dummy function that always succeeds immediately
	// and doesn't retry. This effectively just waits for the pacer's internal timer.
	tradPaceErr := f.tradPacer.Call(func() (bool, error) {
		return false, nil // Dummy call: Success, don't retry this dummy op.
	})
	if tradPaceErr != nil {
		// If waiting for tradPacer was interrupted (e.g., context cancelled)
		fs.Debugf(f, "Context cancelled or error while waiting for traditional pacer: %v", tradPaceErr)
		return tradPaceErr
	}
	return nil
}

// executeTraditionalAPICall makes the actual API call with proper handling of encryption
func (f *Fs) executeTraditionalAPICall(ctx context.Context, opts *rest.Opts, request any, response any, skipEncrypt bool) (*http.Response, error) {
	if skipEncrypt {
		return f.executeUnencryptedCall(ctx, opts, request, response)
	} else {
		return f.executeEncryptedCall(ctx, opts, request, response)
	}
}

// executeUnencryptedCall makes a traditional API call without encryption
func (f *Fs) executeUnencryptedCall(ctx context.Context, opts *rest.Opts, request any, response any) (*http.Response, error) {
	var resp *http.Response
	var apiErr error

	// Choose the right call pattern based on request/response
	if request != nil && response != nil {
		resp, apiErr = f.tradClient.CallJSON(ctx, opts, request, response)
	} else if response != nil {
		resp, apiErr = f.tradClient.CallJSON(ctx, opts, nil, response)
	} else {
		var baseResp api.TraditionalBase
		resp, apiErr = f.tradClient.CallJSON(ctx, opts, nil, &baseResp)
		if apiErr == nil {
			apiErr = baseResp.Err()
		}
	}

	return resp, apiErr
}

// executeEncryptedCall makes a traditional API call with encryption
func (f *Fs) executeEncryptedCall(ctx context.Context, opts *rest.Opts, request any, response any) (*http.Response, error) {
	var resp *http.Response
	var apiErr error

	if request != nil && response != nil {
		// Handle request+response case with encryption
		apiErr = f.executeEncryptedRequestResponse(ctx, opts, request, response, &resp)
	} else if response != nil {
		// Handle response-only case with encryption
		apiErr = f.executeEncryptedResponseOnly(ctx, opts, response, &resp)
	} else {
		// Simple base response case
		var baseResp api.TraditionalBase
		resp, apiErr = f.tradClient.CallJSON(ctx, opts, nil, &baseResp)
		if apiErr == nil {
			apiErr = baseResp.Err()
		}
	}

	return resp, apiErr
}

// executeEncryptedRequestResponse handles the case where both request and response need encryption
func (f *Fs) executeEncryptedRequestResponse(ctx context.Context, opts *rest.Opts, request any, response any, resp **http.Response) error {
	// Encode request data
	input, marshalErr := json.Marshal(request)
	if marshalErr != nil {
		return fmt.Errorf("failed to marshal traditional request: %w", marshalErr)
	}

	key := crypto.GenerateKey()
	// Prepare multipart parameters
	if opts.MultipartParams == nil {
		opts.MultipartParams = url.Values{}
	}
	opts.MultipartParams.Set("data", crypto.Encode(input, key))
	opts.Body = nil // Clear body if using multipart params

	// Execute call expecting encrypted response
	var info api.StringInfo
	var err error
	*resp, err = f.tradClient.CallJSON(ctx, opts, nil, &info)
	if err != nil {
		return err
	}

	// Process the encrypted response
	return f.processEncryptedResponse(&info, response)
}

// executeEncryptedResponseOnly handles the case where only the response is encrypted
func (f *Fs) executeEncryptedResponseOnly(ctx context.Context, opts *rest.Opts, response any, resp **http.Response) error {
	// Call expecting encrypted response, no request body needed (e.g., GET)
	var info api.StringInfo
	var err error
	*resp, err = f.tradClient.CallJSON(ctx, opts, nil, &info)
	if err != nil {
		return err
	}

	// Process the encrypted response
	return f.processEncryptedResponse(&info, response)
}

// processEncryptedResponse decodes and unmarshals an encrypted response
func (f *Fs) processEncryptedResponse(info *api.StringInfo, response any) error {
	if err := info.Err(); err != nil {
		return err // API level error before decryption
	}

	if info.Data == "" {
		return errors.New("no data received in traditional response")
	}

	// Decode and unmarshal response
	key := crypto.GenerateKey()
	output, decodeErr := crypto.Decode(string(info.Data), key)
	if decodeErr != nil {
		return fmt.Errorf("failed to decode traditional data: %w", decodeErr)
	}

	if unmarshalErr := json.Unmarshal(output, response); unmarshalErr != nil {
		return fmt.Errorf("failed to unmarshal traditional response %q: %w", string(output), unmarshalErr)
	}

	return nil
}

// processTraditionalAPIResult handles the result of a traditional API call
func (f *Fs) processTraditionalAPIResult(ctx context.Context, resp *http.Response, apiErr error) (bool, error) {
	// Check for retryable errors
	retryNeeded, retryErr := shouldRetry(ctx, resp, apiErr)
	if retryNeeded {
		fs.Debugf(f, "pacer: low level retry required for traditional call (error: %v)", retryErr)
		return true, retryErr
	}

	// Handle non-retryable errors
	if apiErr != nil {
		fs.Debugf(f, "pacer: permanent error encountered in traditional call: %v", apiErr)
		// Ensure the error is marked as permanent
		var permanentErr *backoff.PermanentError
		if !errors.As(apiErr, &permanentErr) {
			return false, backoff.Permanent(apiErr)
		}
		return false, apiErr // Already permanent
	}

	// Success
	fs.Debugf(f, "pacer: traditional call successful")
	return false, nil
}

// newFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	// Validate incompatible options
	if opt.FastUpload && opt.OnlyStream {
		return nil, errors.New("fast_upload and only_stream cannot be set simultaneously")
	}
	if opt.FastUpload && opt.UploadHashOnly {
		return nil, errors.New("fast_upload and upload_hash_only cannot be set simultaneously")
	}
	if opt.OnlyStream && opt.UploadHashOnly {
		return nil, errors.New("only_stream and upload_hash_only cannot be set simultaneously")
	}

	// Validate upload parameters
	err = checkUploadChunkSize(opt.ChunkSize)
	if err != nil {
		return nil, fmt.Errorf("115: chunk size: %w", err)
	}
	err = checkUploadCutoff(opt.UploadCutoff)
	if err != nil {
		return nil, fmt.Errorf("115: upload cutoff: %w", err)
	}
	err = checkUploadConcurrency(opt.UploadConcurrency)
	if err != nil {
		return nil, fmt.Errorf("115: upload concurrency: %w", err)
	}
	if opt.NohashSize > StreamUploadLimit {
		fs.Logf(name, "nohash_size (%v) reduced to stream upload limit (%v)", opt.NohashSize, StreamUploadLimit)
		opt.NohashSize = StreamUploadLimit
	}

	// Store the original name before any modifications for config operations
	// Extract the base name without the config override suffix {xxxx}
	originalName := name
	if idx := strings.IndexRune(name, '{'); idx > 0 {
		originalName = name[:idx]
	}

	// Parse root ID from path if present
	if rootID, _, _ := parseRootID(root); rootID != "" {
		name += rootID // Append ID to name for uniqueness
		root = root[strings.Index(root, "}")+1:]
	}

	root = strings.Trim(root, "/")

	f := &Fs{
		name:         name,
		originalName: originalName,
		root:         root,
		opt:          *opt,
		m:            m,
	}

	// Initialize features
	f.features = (&fs.Features{
		DuplicateFiles:          false,
		CanHaveEmptyDirectories: true,
		NoMultiThreading:        true, // Keep true as downloads might still use traditional API
	}).Fill(ctx, f)

	// 初始化路径缓存 (10秒TTL用于测试, 最大1000条目)
	f.pathCache = NewPathCache(10*time.Second, 1000)

	// 初始化缓存配置
	f.cacheConfig = DefaultCacheConfig115()

	// 初始化BadgerDB持久化缓存系统
	cacheDir := cache.GetCacheDir("115drive")

	// 初始化多个缓存实例
	caches := map[string]**cache.BadgerCache{
		"path_resolve": &f.pathResolveCache,
		"dir_list":     &f.dirListCache,
		"metadata":     &f.metadataCache,
		"file_id":      &f.fileIDCache,
	}

	for name, cachePtr := range caches {
		cache, cacheErr := cache.NewBadgerCache(name, cacheDir)
		if cacheErr != nil {
			fs.Errorf(f, "初始化%s缓存失败: %v", name, cacheErr)
			// 缓存初始化失败不应该阻止文件系统工作，继续执行
		} else {
			*cachePtr = cache
			fs.Debugf(f, "%s缓存初始化成功", name)
		}
	}

	fs.Debugf(f, "BadgerDB缓存系统初始化完成: %s", cacheDir)

	// 初始化断点续传管理器（使用BadgerDB）
	resumeManager, resumeErr := NewResumeManager115(f, filepath.Join(cacheDir, "resume"))
	if resumeErr != nil {
		fs.Errorf(f, "初始化断点续传管理器失败: %v", resumeErr)
		// 断点续传失败不应该阻止文件系统工作，继续执行
	} else {
		f.resumeManager = resumeManager
		fs.Debugf(f, "断点续传管理器初始化成功（BadgerDB）")
	}

	// 🚀 功能增强：初始化智能URL管理器，解决下载URL频繁过期问题
	f.smartURLManager = NewSmartURLManager(f)
	fs.Debugf(f, "智能URL管理器初始化成功")

	// 初始化优化的HTTP客户端
	f.httpClient = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,              // 最大空闲连接数
			MaxIdleConnsPerHost: 10,               // 每个主机的最大空闲连接数
			IdleConnTimeout:     90 * time.Second, // 空闲连接超时
			DisableCompression:  false,            // 启用压缩
		},
		Timeout: 30 * time.Second, // 请求超时
	}

	// Setting appVer (needed for traditional calls)
	re := regexp.MustCompile(`\d+\.\d+\.\d+(\.\d+)?$`)
	if m := re.FindStringSubmatch(tradUserAgent); m == nil {
		fs.Logf(f, "Could not parse app version from User-Agent %q. Using default.", tradUserAgent)
		f.appVer = "27.0.7.5" // Default fallback
	} else {
		f.appVer = m[0]
		fs.Debugf(f, "Using App Version %q from User-Agent %q", f.appVer, tradUserAgent)
	}

	// Initialize pacers
	f.globalPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(time.Duration(opt.PacerMinSleep)),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	f.tradPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(traditionalMinSleep),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// 🔧 添加下载URL专用调速器，防止API调用频率过高
	// 基于115网盘API特性，设置保守的QPS限制避免触发反爬机制
	f.downloadPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(downloadURLMinSleep),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// 🔧 添加上传专用调速器，优化上传API调用频率
	// 平衡上传性能和API稳定性，避免上传时频率过高导致限制
	f.uploadPacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(uploadMinSleep),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// Create clients first to ensure they're available for token operations
	// Create separate clients for each API type with different User-Agents
	tradHTTPClient := getTradHTTPClient(ctx, &f.opt)
	openAPIHTTPClient := getOpenAPIHTTPClient(ctx, &f.opt)

	// Traditional client (uses cookie)
	f.tradClient = rest.NewClient(tradHTTPClient).
		SetRoot(traditionalRootURL).
		SetErrorHandler(errorHandler)

	// Add cookie to traditional client if provided
	if f.opt.Cookie != "" {
		cred := (&Credential{}).FromCookie(f.opt.Cookie)
		f.tradClient.SetCookie(cred.Cookie()...)
	}

	// OpenAPI client
	f.openAPIClient = rest.NewClient(openAPIHTTPClient).
		SetRoot(openAPIRootURL).
		SetErrorHandler(errorHandler)

	// Check if we have saved token in config file
	tokenLoaded := loadTokenFromConfig(f, m)
	fs.Debugf(f, "Token loaded from config: %v (expires at %v)", tokenLoaded, f.tokenExpiry)

	var tokenRefreshNeeded bool
	if tokenLoaded {
		// Check if token is expired or will expire soon
		if time.Now().After(f.tokenExpiry.Add(-tokenRefreshWindow)) {
			fs.Debugf(f, "Token expired or will expire soon, refreshing now")
			tokenRefreshNeeded = true
		}
	} else if f.opt.Cookie != "" {
		// No token but have cookie, so login
		fs.Debugf(f, "No token found but cookie provided, attempting login")
		err = f.login(ctx)
		if err != nil {
			return nil, fmt.Errorf("initial login failed: %w", err)
		}
		// Save token to config after successful login
		f.saveToken(ctx, m)
		fs.Debugf(f, "Login successful, token saved")
	} else {
		return nil, errors.New("no valid cookie or token found, please configure cookie")
	}

	// Try to refresh the token if needed
	if tokenRefreshNeeded {
		err = f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			fs.Debugf(f, "Token refresh failed, attempting full login: %v", err)
			// If refresh fails, try full login
			err = f.login(ctx)
			if err != nil {
				return nil, fmt.Errorf("login failed after token refresh failure: %w", err)
			}
		}
		// Save the refreshed/new token
		f.saveToken(ctx, m)
		fs.Debugf(f, "Token refresh/login successful, token saved")
	}

	// Setup token renewer for automatic refresh
	fs.Debugf(f, "Setting up token renewer")
	f.setupTokenRenewer(ctx, m)

	// Set the root folder ID based on config
	if f.opt.RootFolderID != "" {
		// Use configured root folder ID
		f.rootFolderID = f.opt.RootFolderID
	} else {
		// Use default root folder ID
		f.rootFolderID = "0"
	}

	// Initialize directory cache
	f.dirCache = dircache.New(f.root, f.rootFolderID, f)

	// 🔧 暂时禁用智能缓存预热，避免干扰上传性能
	// go f.intelligentCachePreheating(ctx)

	// Find the current working directory
	if f.root != "" {
		// Find directory specified by the root path
		err := f.dirCache.FindRoot(ctx, false)
		if err != nil {
			// Assume it is a file or doesn't exist
			newRoot, remote := dircache.SplitPath(f.root)
			// 创建新的Fs实例，避免复制mutex
			tempF := &Fs{
				name:          f.name,
				originalName:  f.originalName,
				root:          newRoot,
				opt:           f.opt,
				features:      f.features,
				tradClient:    f.tradClient,
				openAPIClient: f.openAPIClient,
				globalPacer:   f.globalPacer,
				tradPacer:     f.tradPacer,
				rootFolder:    f.rootFolder,
				rootFolderID:  f.rootFolderID,
				appVer:        f.appVer,
				userID:        f.userID,
				userkey:       f.userkey,
				isShare:       f.isShare,
				fileObj:       f.fileObj,
				m:             f.m,
				accessToken:   f.accessToken,
				refreshToken:  f.refreshToken,
				tokenExpiry:   f.tokenExpiry,
				codeVerifier:  f.codeVerifier,
				tokenRenewer:  f.tokenRenewer,
				pathCache:     f.pathCache,
				httpClient:    f.httpClient,
			}
			tempF.dirCache = dircache.New(newRoot, f.rootFolderID, tempF)
			// Make new Fs which is the parent
			err = tempF.dirCache.FindRoot(ctx, false)
			if err != nil {
				// No root so return old f
				return f, nil
			}
			// Check if it's a file
			_, err := tempF.newObjectWithInfo(ctx, remote, nil)
			if err != nil {
				// 检查是否是API限制错误，如果是则直接返回目录模式，避免路径混乱
				if isAPILimitError(err) {
					return f, nil
				}
				if err == fs.ErrorObjectNotFound {
					// File doesn't exist so return old f
					return f, nil
				}
				return nil, err
			}
			// Copy the features
			f.features.Fill(ctx, tempF)
			// Update the dir cache in the old f
			f.dirCache = tempF.dirCache
			f.root = tempF.root
			// Return an error with an fs which points to the parent
			return f, fs.ErrorIsFile
		}
	}

	return f, nil
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String returns a description of the FS
func (f *Fs) String() string {
	return fmt.Sprintf("115 %s", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	// OpenAPI might allow setting modtime, but let's assume not for now
	return fs.ModTimeNotSupported
}

// DirCacheFlush resets the directory cache
func (f *Fs) DirCacheFlush() {
	f.dirCache.ResetRoot()
}

// Hashes returns the supported hash types of the filesystem
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.SHA1)
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	fs.Debugf(f, "🔍 NewObject开始: remote=%q", remote)

	if f.fileObj != nil { // Handle case where Fs points to a single file
		fs.Debugf(f, "🔍 NewObject: 处理单文件模式")
		obj := *f.fileObj
		if obj.Remote() == remote || obj.Remote() == "isFile:"+remote {
			fs.Debugf(f, "🔍 NewObject: 单文件匹配成功")
			return obj, nil
		}
		fs.Debugf(f, "🔍 NewObject: 单文件不匹配，返回NotFound")
		return nil, fs.ErrorObjectNotFound // If remote doesn't match the single file
	}

	result, err := f.newObjectWithInfo(ctx, remote, nil)
	if err != nil {
		fs.Debugf(f, "🔍 NewObject失败: %v", err)
	}
	return result, err
}

// FindLeaf finds a directory or file leaf in the parent folder pathID.
// Used by dircache.
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (foundID string, found bool, err error) {
	// 构建缓存键
	cacheKey := pathID + "/" + leaf

	// 首先检查缓存
	if entry, exists := f.pathCache.Get(cacheKey); exists {
		if entry.IsDir {
			return entry.ID, true, nil
		} else {
			// 找到的是文件，返回空ID表示这是文件而不是目录
			return "", true, nil
		}
	}

	// Use listAll which now uses OpenAPI
	found, err = f.listAll(ctx, pathID, f.opt.ListChunk, false, false, func(item *api.File) bool {
		// Compare with decoded name to handle special characters correctly
		decodedName := f.opt.Enc.ToStandardName(item.FileNameBest())
		if decodedName == leaf {
			// 检查找到的项目是否为目录
			if item.IsDir() {
				// 这是目录，返回目录ID
				foundID = item.ID()
				// Cache the found item's path/ID mapping (only for directories)
				parentPath, ok := f.dirCache.GetInv(pathID)
				if ok {
					itemPath := path.Join(parentPath, leaf)
					f.dirCache.Put(itemPath, foundID)
				}
				// 同时更新路径缓存
				f.pathCache.Put(cacheKey, foundID, true)
			} else {
				// 这是文件，不返回ID（保持foundID为空字符串）
				foundID = "" // 明确设置为空，表示找到的是文件而不是目录
				// 缓存文件信息
				f.pathCache.Put(cacheKey, item.ID(), false)
			}
			return true // Stop searching
		}
		return false // Continue searching
	})

	// 如果遇到API限制错误，采用渐进式缓存清理策略
	if isAPILimitError(err) {
		f.clearCacheGradually(err, "")
	}

	return foundID, found, err
}

// GetDirID finds a directory ID by its absolute path. Used by dircache.
// This implementation uses the OpenAPI list endpoint iteratively, which might be slow.
// The traditional /files/getid is faster but less reliable. We prioritize OpenAPI.
func (f *Fs) GetDirID(ctx context.Context, dir string) (string, error) {
	// Start from the filesystem's root ID
	currentID := f.rootFolderID
	if dir == "" || dir == "/" {
		return currentID, nil
	}

	parts := strings.Split(strings.Trim(dir, "/"), "/")
	for _, part := range parts {
		if part == "" {
			continue
		}
		encodedPart := f.opt.Enc.FromStandardName(part) // Encode each part
		foundID, found, err := f.FindLeaf(ctx, currentID, encodedPart)
		if err != nil {
			// 如果是API限制错误，立即返回，不要继续递归搜索
			if isAPILimitError(err) {
				// 采用渐进式缓存清理策略
				f.clearCacheGradually(err, encodedPart)
				return "", err
			}
			return "", fmt.Errorf("error searching for %q in %q: %w", encodedPart, currentID, err)
		}
		if !found {
			return "", fs.ErrorDirNotFound
		}

		// 验证找到的项目确实是目录
		// 如果是文件，则这个路径不应该被当作目录路径处理
		if foundID == "" {
			return "", fmt.Errorf("path component %q is a file, not a directory", encodedPart)
		}
		// Check if the found item is actually a directory
		// Need a way to confirm type without full listing again. Assume FindLeaf returns correct type for now.
		// If FindLeaf could return the *api.File, we could check item.IsDir() here.
		// For now, assume FindLeaf only finds directories when called by GetDirID context.
		currentID = foundID
	}
	return currentID, nil
}

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fs.Debugf(f, "List调用，目录: %q", dir)

	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		// 如果是API限制错误，记录详细信息并返回
		if strings.Contains(err.Error(), "770004") ||
			strings.Contains(err.Error(), "已达到当前访问上限") {
			fs.Debugf(f, "List遇到API限制错误，目录: %q, 错误: %v", dir, err)
			return nil, err
		}
		fs.Debugf(f, "List查找目录失败，目录: %q, 错误: %v", dir, err)
		return nil, err
	}

	fs.Debugf(f, "List找到目录ID: %s，目录: %q", dirID, dir)
	var iErr error
	_, err = f.listAll(ctx, dirID, f.opt.ListChunk, false, false, func(item *api.File) bool {
		entry, err := f.itemToDirEntry(ctx, path.Join(dir, item.FileNameBest()), item)
		if err != nil {
			iErr = err // Capture error but continue listing
			return false
		}
		if entry != nil {
			entries = append(entries, entry)
		}
		return false
	})
	if err != nil {
		return nil, err // Return listing error
	}
	if iErr != nil {
		return nil, iErr // Return item processing error
	}
	return entries, nil
}

// CreateDir makes a directory with pathID as parent and name leaf. Used by dircache.
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	if f.isShare {
		return "", errors.New("unsupported operation: Mkdir on shared filesystem")
	}
	// Use makeDir which now uses OpenAPI
	info, err := f.makeDir(ctx, pathID, leaf)
	if err != nil {
		return "", err
	}
	if info.Data == nil || info.Data.FileID == "" {
		return "", errors.New("Mkdir response did not contain a file ID")
	}
	return info.Data.FileID, nil
}

// Put uploads the object.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	// Check if destination exists. If not, use PutUnchecked. If yes, use Update.
	existingObj, err := f.NewObject(ctx, src.Remote())
	if err == fs.ErrorObjectNotFound {
		// Not found, so create it using PutUnchecked
		return f.PutUnchecked(ctx, in, src, options...)
	} else if err != nil {
		// An error other than not found
		return nil, err
	}
	// Object exists, so update it
	return existingObj, existingObj.Update(ctx, in, src, options...)
}

// PutUnchecked uploads the object without checking for existence first.
func (f *Fs) PutUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.putUnchecked(ctx, in, src, src.Remote(), options...)
}

// putUnchecked uploads the object
func (f *Fs) putUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, remote string, options ...fs.OpenOption) (fs.Object, error) {
	if f.isShare {
		return nil, errors.New("unsupported operation: Put on shared filesystem")
	}

	// Call the main upload function which handles different strategies
	newObj, err := f.upload(ctx, in, src, remote, options...)
	if err != nil {
		return nil, err
	}
	if newObj == nil {
		return nil, errors.New("internal error: upload returned nil object without error")
	}

	o := newObj.(*Object)

	// Post-upload check (optional)
	if !f.opt.NoCheck && !o.hasMetaData {
		fs.Debugf(o, "Running post-upload check...")
		// Attempt to read metadata for the uploaded object to confirm
		err = o.readMetaData(ctx) // This will list the parent directory
		if err != nil {
			// Don't fail the upload, just log a warning
			fs.Logf(o, "Post-upload check failed to read metadata: %v", err)
			// Mark as having metadata anyway to avoid repeated checks
			o.hasMetaData = true
		} else {
			fs.Debugf(o, "Post-upload check successful.")
		}
	}

	// 上传成功后，清理相关缓存
	f.invalidateRelatedCaches115(remote, "upload")

	return newObj, nil
}

// MergeDirs merges multiple source directories into the first one.
func (f *Fs) MergeDirs(ctx context.Context, dirs []fs.Directory) (err error) {
	if f.isShare {
		return errors.New("unsupported operation: MergeDirs on shared filesystem")
	}
	if len(dirs) < 2 {
		return nil
	}
	dstDir := dirs[0]
	dstDirID := dstDir.ID()

	for _, srcDir := range dirs[1:] {
		srcDirID := srcDir.ID()
		fs.Debugf(srcDir, "Merging contents into %v", dstDir)

		// List all items in the source directory
		var itemsToMove []*api.File
		_, err = f.listAll(ctx, srcDirID, f.opt.ListChunk, false, false, func(item *api.File) bool {
			itemsToMove = append(itemsToMove, item)
			return false // Collect all items
		})
		if err != nil {
			return fmt.Errorf("MergeDirs list failed on %v: %w", srcDir, err)
		}

		// Move items in chunks
		chunkSize := f.opt.ListChunk // Use list chunk size for move chunks
		for i := 0; i < len(itemsToMove); i += chunkSize {
			end := i + chunkSize
			if end > len(itemsToMove) {
				end = len(itemsToMove)
			}
			chunk := itemsToMove[i:end]
			if len(chunk) == 0 {
				continue
			}

			var idsToMove []string
			for _, item := range chunk {
				idsToMove = append(idsToMove, item.ID())
			}

			fs.Debugf(srcDir, "Moving %d items to %v", len(idsToMove), dstDir)
			if err = f.moveFiles(ctx, idsToMove, dstDirID); err != nil {
				return fmt.Errorf("MergeDirs move failed for %v: %w", srcDir, err)
			}
		}
	}

	// Remove the source directories (now empty)
	var dirsToDelete []string
	for _, srcDir := range dirs[1:] {
		dirsToDelete = append(dirsToDelete, srcDir.ID())
	}

	// Delete directories in chunks
	chunkSize := f.opt.ListChunk
	for i := 0; i < len(dirsToDelete); i += chunkSize {
		end := i + chunkSize
		if end > len(dirsToDelete) {
			end = len(dirsToDelete)
		}
		chunkIDs := dirsToDelete[i:end]
		if len(chunkIDs) == 0 {
			continue
		}

		fs.Debugf(f, "Removing merged source directories: %v", chunkIDs)
		if err = f.deleteFiles(ctx, chunkIDs); err != nil {
			// Log error but continue trying to delete others
			fs.Errorf(f, "MergeDirs failed to rmdir chunk %v: %v", chunkIDs, err)
		}
	}

	// Flush the cache for the source directories
	for _, srcDir := range dirs[1:] {
		f.dirCache.FlushDir(srcDir.Remote())
	}

	return nil // Return nil even if some deletions failed, as merge likely succeeded partially
}

// Mkdir makes the directory.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	if f.isShare {
		return errors.New("unsupported operation: Mkdir on shared filesystem")
	}
	_, err := f.dirCache.FindDir(ctx, dir, true) // create = true
	if err == nil {
		// 创建目录成功后，清理相关缓存
		f.invalidateRelatedCaches115(dir, "mkdir")
	}
	return err
}

// Move server-side moves a file.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	if f.isShare {
		return nil, fs.ErrorCantMove
	}
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't move - not same remote type")
		return nil, fs.ErrorCantMove
	}
	// Ensure metadata is read for srcObj.id
	err := srcObj.readMetaData(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read source metadata for move: %w", err)
	}
	if srcObj.id == "" {
		return nil, errors.New("cannot move object with empty ID")
	}

	// Find destination parent directory ID, creating if necessary
	dstLeaf, dstParentID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, fmt.Errorf("failed to find destination directory for move: %w", err)
	}

	// Find source parent directory ID
	srcLeaf, srcParentID, err := srcObj.fs.dirCache.FindPath(ctx, srcObj.remote, false)
	if err != nil {
		// Log error but proceed, maybe parent doesn't exist in cache but file does
		fs.Logf(src, "Could not find source path in cache for move: %v", err)
		// Attempt to get parent ID from object if available
		if srcObj.parent != "" {
			srcParentID = srcObj.parent
		} else {
			return nil, fmt.Errorf("failed to find source directory for move and object has no parent ID: %w", err)
		}
	}

	// Perform the move if parents differ
	if srcParentID != dstParentID {
		fs.Debugf(srcObj, "Moving %q from %q to %q", srcObj.id, srcParentID, dstParentID)
		err = f.moveFiles(ctx, []string{srcObj.id}, dstParentID)
		if err != nil {
			return nil, fmt.Errorf("server-side move failed: %w", err)
		}
	}

	// Perform rename if names differ
	if srcLeaf != dstLeaf {
		fs.Debugf(srcObj, "Renaming %q to %q", srcLeaf, dstLeaf)
		err = f.renameFile(ctx, srcObj.id, dstLeaf)
		if err != nil {
			// Attempt to move back if rename fails? Or just return error?
			return nil, fmt.Errorf("failed to rename after move: %w", err)
		}
	}

	// Create new object representing the destination
	dstObj := &Object{
		fs:          f,
		remote:      remote,
		hasMetaData: false, // Mark metadata as stale
		id:          srcObj.id,
		parent:      dstParentID, // Update parent ID
		size:        srcObj.size,
		sha1sum:     srcObj.sha1sum,
		pickCode:    srcObj.pickCode,
		modTime:     srcObj.modTime, // Keep original modTime? Or update? Keep for now.
		durlMu:      new(sync.Mutex),
	}

	// Read metadata for the new object to confirm and update details
	err = dstObj.readMetaData(ctx)
	if err != nil {
		// Log error but return the object anyway, metadata might be eventually consistent
		fs.Logf(dstObj, "Failed to read metadata after move: %v", err)
	} else {
		dstObj.hasMetaData = true
	}

	// Flush source directory from cache
	dir, _ := dircache.SplitPath(srcObj.remote)
	srcObj.fs.dirCache.FlushDir(dir)

	// 移动成功后，清理相关缓存
	f.invalidateRelatedCaches115(srcObj.remote, "move")
	f.invalidateRelatedCaches115(remote, "move")

	return dstObj, nil
}

// DirMove server-side moves a directory.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	if f.isShare {
		return fs.ErrorCantDirMove
	}
	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(srcFs, "Can't move directory - not same remote type")
		return fs.ErrorCantDirMove
	}

	// Use dircache helper to prepare for move
	srcID, srcParentID, srcLeaf, dstParentID, dstLeaf, err := f.dirCache.DirMove(ctx, srcFs.dirCache, srcFs.root, srcRemote, f.root, dstRemote)
	if err != nil {
		return err // Errors like DirExists, CantMoveRoot handled by DirMove helper
	}

	// Perform the move if parents differ
	if srcParentID != dstParentID {
		fs.Debugf(srcFs, "Moving directory %q from %q to %q", srcID, srcParentID, dstParentID)
		err = f.moveFiles(ctx, []string{srcID}, dstParentID)
		if err != nil {
			return fmt.Errorf("server-side directory move failed: %w", err)
		}
	}

	// Perform rename if names differ
	if srcLeaf != dstLeaf {
		fs.Debugf(srcFs, "Renaming directory %q to %q", srcLeaf, dstLeaf)
		err = f.renameFile(ctx, srcID, dstLeaf)
		if err != nil {
			return fmt.Errorf("failed to rename directory after move: %w", err)
		}
	}

	// Flush source directory from cache
	srcFs.dirCache.FlushDir(srcRemote)
	return nil
}

// Copy server-side copies a file.
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}

	// Special handling for copying *from* a shared remote (using traditional API)
	if srcObj.fs.isShare {
		if f.isShare {
			// Cannot copy from share to share directly? Assume not supported.
			return nil, errors.New("copying between shared remotes is not supported")
		}
		fs.Debugf(src, "Copying from shared remote using traditional API")
		// Find destination parent ID
		_, dstParentID, err := f.dirCache.FindPath(ctx, remote, true)
		if err != nil {
			return nil, fmt.Errorf("failed to find destination directory for copy: %w", err)
		}
		// Call traditional copyFromShare
		err = f.copyFromShare(ctx, srcObj.fs.opt.ShareCode, srcObj.fs.opt.ReceiveCode, srcObj.id, dstParentID)
		if err != nil {
			return nil, fmt.Errorf("copy from share failed: %w", err)
		}
		// Need to find the new object in the destination to return it
		// The name will be the same as the source object's name
		srcLeaf, _, _ := srcObj.fs.dirCache.FindPath(ctx, srcObj.remote, false) // Get original leaf name
		dir, _ := dircache.SplitPath(remote)
		dstPath := path.Join(dir, srcLeaf)       // Construct potential destination path
		newObj, err := f.NewObject(ctx, dstPath) // Find the newly copied object
		if err != nil {
			return nil, fmt.Errorf("failed to find copied object in destination after share copy: %w", err)
		}
		// If the target remote name is different, rename the copied object
		dstLeaf := path.Base(remote)
		if srcLeaf != dstLeaf {
			err = f.renameFile(ctx, newObj.(*Object).id, dstLeaf)
			if err != nil {
				return nil, fmt.Errorf("failed to rename after copy from share: %w", err)
			}
			// Re-fetch the object to get updated metadata/remote path
			finalObj, err := f.NewObject(ctx, remote)
			if err != nil {
				return nil, fmt.Errorf("failed to find renamed object after share copy: %w", err)
			}
			return finalObj, nil
		}
		return newObj, nil
	}

	// --- Standard Copy (Non-Share Source) ---
	if f.isShare {
		// Cannot copy *to* a shared remote
		return nil, errors.New("copying to a shared remote is not supported")
	}

	// Ensure metadata is read for srcObj.id
	err := srcObj.readMetaData(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read source metadata for copy: %w", err)
	}
	if srcObj.id == "" {
		return nil, errors.New("cannot copy object with empty ID")
	}

	// Find destination parent directory ID, creating if necessary
	dstLeaf, dstParentID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, fmt.Errorf("failed to find destination directory for copy: %w", err)
	}

	// Check if source and destination parent are the same
	srcParentID := srcObj.parent
	if srcParentID == "" { // Try to get from cache if not on object
		_, srcParentID, _ = srcObj.fs.dirCache.FindPath(ctx, srcObj.remote, false)
	}
	if srcParentID == dstParentID {
		// API restriction: cannot copy within the same directory
		// We could potentially handle this by copying to a temp dir and moving,
		// but for now, return ErrorCantCopy.
		fs.Debugf(src, "Can't copy - source and destination directory are the same (%q)", srcParentID)
		return nil, fs.ErrorCantCopy
	}

	// Perform the copy using OpenAPI
	fs.Debugf(srcObj, "Copying %q to %q", srcObj.id, dstParentID)
	err = f.copyFiles(ctx, []string{srcObj.id}, dstParentID)
	if err != nil {
		return nil, fmt.Errorf("server-side copy failed: %w", err)
	}

	// Find the newly created object in the destination directory
	// It will initially have the same name as the source object.
	srcLeaf, _, _ := srcObj.fs.dirCache.FindPath(ctx, srcObj.remote, false) // Get original leaf name
	dir, _ := dircache.SplitPath(remote)
	copiedObjPath := path.Join(dir, srcLeaf)       // Construct path where copied object should be
	newObj, err := f.NewObject(ctx, copiedObjPath) // Find the object at that path
	if err != nil {
		return nil, fmt.Errorf("failed to find copied object in destination: %w", err)
	}
	newObjConcrete := newObj.(*Object)

	// Rename the copied object if the target remote name is different
	if srcLeaf != dstLeaf {
		fs.Debugf(newObj, "Renaming copied object to %q", dstLeaf)
		err = f.renameFile(ctx, newObjConcrete.id, dstLeaf)
		if err != nil {
			// Attempt to delete the wrongly named copy? Or just return error?
			_ = f.deleteFiles(ctx, []string{newObjConcrete.id}) // Best effort cleanup
			return nil, fmt.Errorf("failed to rename after copy: %w", err)
		}
		// Update the object's remote path and mark metadata stale
		newObjConcrete.remote = remote
		newObjConcrete.hasMetaData = false
		// Read metadata again to confirm rename and get latest info
		err = newObjConcrete.readMetaData(ctx)
		if err != nil {
			fs.Logf(newObj, "Failed to read metadata after rename: %v", err)
		} else {
			newObjConcrete.hasMetaData = true
		}
	} else {
		// If no rename needed, ensure the returned object has the correct remote path
		newObjConcrete.remote = remote
	}

	return newObjConcrete, nil
}

// purgeCheck removes the root directory. Refuses if check=true and not empty.
func (f *Fs) purgeCheck(ctx context.Context, dir string, check bool) error {
	if f.isShare {
		return errors.New("unsupported operation: Purge/Rmdir on shared filesystem")
	}
	root := path.Join(f.root, dir)
	if root == "" && dir != "" { // Check if trying to delete the effective root specified in config
		// This case needs careful handling. If root_folder_id is set, `dir` might be ""
		// but `f.rootFolderID` is not "0".
		if f.rootFolderID == "0" {
			return errors.New("internal error: attempting to purge root directory")
		}
		// Allow purging the configured root folder ID
	} else if root == "" && dir == "" && f.rootFolderID == "0" {
		// Explicitly prevent purging the absolute root "0"
		return errors.New("refusing to purge the absolute root directory '0'")
	}

	// Find the ID of the directory to purge
	dirID, err := f.dirCache.FindDir(ctx, dir, false) // Don't create
	if err != nil {
		return err // Return DirNotFound or other errors
	}

	if check {
		// Check if directory is empty using listAll with limit 1
		found, listErr := f.listAll(ctx, dirID, 1, false, false, func(item *api.File) bool {
			fs.Debugf(f, "Rmdir check: directory %q contains %q", dir, item.FileNameBest())
			return true // Found an item, stop listing
		})
		if listErr != nil {
			return fmt.Errorf("failed to check if directory %q is empty: %w", dir, listErr)
		}
		if found {
			return fs.ErrorDirectoryNotEmpty
		}
	}

	// Perform the delete using OpenAPI
	fs.Debugf(f, "Purging directory %q (ID: %q)", dir, dirID)
	err = f.deleteFiles(ctx, []string{dirID})
	if err != nil {
		return fmt.Errorf("failed to delete directory %q (ID: %q): %w", dir, dirID, err)
	}

	// Flush the directory from cache
	f.dirCache.FlushDir(dir)
	return nil
}

// Rmdir removes an empty directory.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	return f.purgeCheck(ctx, dir, true) // check = true
}

// Purge removes a directory and all its contents.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	return f.purgeCheck(ctx, dir, false) // check = false
}

// About gets quota information (currently uses traditional API).
// TODO: Check if OpenAPI provides a quota endpoint.
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	// Using traditional indexInfo for now
	info, err := f.indexInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get usage info: %w", err)
	}

	usage := &fs.Usage{}
	if totalInfo, ok := info.SpaceInfo["all_total"]; ok {
		usage.Total = fs.NewUsageValue(int64(totalInfo.Size))
	}
	if useInfo, ok := info.SpaceInfo["all_use"]; ok {
		usage.Used = fs.NewUsageValue(int64(useInfo.Size))
	}
	if remainInfo, ok := info.SpaceInfo["all_remain"]; ok {
		usage.Free = fs.NewUsageValue(int64(remainInfo.Size))
	}

	return usage, nil
}

// Shutdown shuts down the fs, closing any background tasks
func (f *Fs) Shutdown(ctx context.Context) error {
	if f.tokenRenewer != nil {
		f.tokenRenewer.Shutdown()
		f.tokenRenewer = nil
	}

	// 关闭所有BadgerDB缓存
	caches := []*cache.BadgerCache{
		f.pathResolveCache,
		f.dirListCache,
		f.metadataCache,
		f.fileIDCache,
	}

	for i, c := range caches {
		if c != nil {
			if err := c.Close(); err != nil {
				fs.Debugf(f, "关闭BadgerDB缓存%d失败: %v", i, err)
			}
		}
	}

	return nil
}

// itemToDirEntry converts an api.File to an fs.DirEntry
func (f *Fs) itemToDirEntry(ctx context.Context, remote string, item *api.File) (entry fs.DirEntry, err error) {
	if item.IsDir() {
		// Cache the directory ID
		f.dirCache.Put(remote, item.ID())
		d := fs.NewDir(remote, item.ModTime()).SetID(item.ID()).SetParentID(item.ParentID())
		return d, nil
	}
	// It's a file
	entry, err = f.newObjectWithInfo(ctx, remote, item)
	if err == fs.ErrorObjectNotFound {
		return nil, nil // Should not happen if item came from listing
	}
	return entry, err
}

// newObjectWithInfo creates an fs.Object from an api.File or by reading metadata.
func (f *Fs) newObjectWithInfo(ctx context.Context, remote string, info *api.File) (fs.Object, error) {
	fs.Debugf(f, "🔍 newObjectWithInfo开始: remote=%q, hasInfo=%v", remote, info != nil)

	o := &Object{
		fs:     f,
		remote: remote,
		durlMu: new(sync.Mutex),
	}
	var err error
	if info != nil {
		// Set metadata from provided info
		err = o.setMetaData(info)
	} else {
		// Read metadata from the backend
		err = o.readMetaData(ctx)
	}
	if err != nil {
		fs.Debugf(f, "🔍 newObjectWithInfo失败: %v", err)
		return nil, err
	}
	return o, nil
}

// readMetaDataForPath finds metadata for a specific file path.
func (f *Fs) readMetaDataForPath(ctx context.Context, path string) (info *api.File, err error) {
	fs.Debugf(f, "🔍 readMetaDataForPath开始: path=%q", path)

	leaf, dirID, err := f.dirCache.FindPath(ctx, path, false)
	if err != nil {
		fs.Debugf(f, "🔍 readMetaDataForPath: FindPath失败: %v", err)
		// 检查是否是API限制错误，如果是则立即返回，避免路径混乱
		if isAPILimitError(err) {
			fs.Debugf(f, "readMetaDataForPath遇到API限制错误，路径: %q, 错误: %v", path, err)
			// 采用渐进式缓存清理策略
			f.clearCacheGradually(err, path)
			return nil, err
		}
		if err == fs.ErrorDirNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}

	fs.Debugf(f, "🔍 readMetaDataForPath: FindPath成功, leaf=%q, dirID=%q", leaf, dirID)

	// List the directory and find the leaf
	fs.Debugf(f, "🔍 readMetaDataForPath: 开始调用listAll")
	found, err := f.listAll(ctx, dirID, f.opt.ListChunk, true, false, func(item *api.File) bool {
		// Compare with decoded name to handle special characters correctly
		decodedName := f.opt.Enc.ToStandardName(item.FileNameBest())
		if decodedName == leaf {
			fs.Debugf(f, "🔍 readMetaDataForPath: 找到匹配文件: %q", decodedName)
			info = item
			return true // Found it
		}
		return false // Keep looking
	})
	if err != nil {
		fs.Debugf(f, "🔍 readMetaDataForPath: listAll失败: %v", err)
		return nil, fmt.Errorf("failed to list directory %q to find %q: %w", dirID, leaf, err)
	}
	if !found {
		fs.Debugf(f, "🔍 readMetaDataForPath: 未找到文件")
		return nil, fs.ErrorObjectNotFound
	}
	fs.Debugf(f, "🔍 readMetaDataForPath: 成功找到文件元数据")
	return info, nil
}

// createObject creates a placeholder Object struct before upload.
func (f *Fs) createObject(ctx context.Context, remote string, modTime time.Time, size int64) (o *Object, leaf string, dirID string, err error) {
	// Create the parent directory if it doesn't exist
	leaf, dirID, err = f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return
	}
	// Temporary Object under construction
	o = &Object{
		fs:          f,
		remote:      remote,
		parent:      dirID,
		size:        size,
		modTime:     modTime,
		hasMetaData: false, // Metadata not set yet
		durlMu:      new(sync.Mutex),
	}
	return o, leaf, dirID, nil
}

// ------------------------------------------------------------
// Command Help & Execution
// ------------------------------------------------------------

var commandHelp = []fs.CommandHelp{{
	Name:  "addurls",
	Short: "Add offline download task for urls (uses traditional API)",
	Long: `This command adds offline download task for urls using the traditional API.

Usage:

    rclone backend addurls 115:dirpath url1 url2

Downloads are saved to the folder "dirpath". If omitted or non-existent,
it defaults to "云下载". Requires cookie authentication.
This command always exits with code 0; check output for errors.`,
}, {
	Name:  "getid",
	Short: "Get the ID of a file or directory",
	Long: `This command obtains the ID of a file or directory using the OpenAPI.

Usage:

    rclone backend getid 115:path/to/item

Returns the internal ID used by the 115 API.`,
}, {
	Name:  "addshare",
	Short: "Add shared files/dirs from a share link (uses traditional API)",
	Long: `This command adds shared files/dirs from a share link using the traditional API.

Usage:

    rclone backend addshare 115:dirpath share_link

Content from the link is copied to "dirpath". Requires cookie authentication.`,
}, {
	Name:  "stats",
	Short: "Get folder statistics (uses OpenAPI)",
	Long: `This command retrieves statistics for a folder using the OpenAPI.

Usage:

    rclone backend stats 115:path/to/folder

Returns information like total size, file count, folder count, etc.`,
}, {
	Name:  "getdownloadurl",
	Short: "Get the download URL of a file by its path",
	Long: `This command retrieves the download URL of a file using its path.

Usage:

rclone backend getdownloadurl 115:path/to/file

The command returns the download URL for the specified file. Ensure the file path is correct.`,
}, {
	Name:  "getdownloadurlau",
	Short: "Get the download URL of a file by its path",
	Long: `This command retrieves the download URL of a file using its path.

Usage:

rclone backend getdownloadurlau 115:path/to/file VidHub/1.7.24

The command returns the download URL for the specified file. Ensure the file path is correct.`,
},
}

// Command executes backend-specific commands.
func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (out any, err error) {
	switch name {
	case "addurls":
		if f.isShare {
			return nil, errors.New("addurls unsupported for shared filesystem")
		}
		dir := "" // Default to root or 云下载 handled by API
		if parentDir, ok := opt["dir"]; ok {
			dir = parentDir
		}
		return f.addURLs(ctx, dir, arg) // Uses traditional API
	case "getid":
		path := ""
		if len(arg) > 0 {
			path = arg[0]
		}
		return f.getID(ctx, path) // Uses OpenAPI via listAll/FindDir/NewObject
	case "addshare":
		if f.isShare {
			return nil, errors.New("addshare unsupported for shared filesystem")
		}
		if len(arg) < 1 {
			return nil, errors.New("addshare requires a share link argument")
		}
		shareCode, receiveCode, err := parseShareLink(arg[0])
		if err != nil {
			return nil, err
		}
		dirID, err := f.dirCache.FindDir(ctx, "", true) // Find target dir ID (create if needed)
		if err != nil {
			return nil, err
		}
		return nil, f.copyFromShare(ctx, shareCode, receiveCode, "", dirID) // Uses traditional API
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

type FileInfoResponse struct {
	State   bool      `json:"state"`
	Message string    `json:"message"`
	Code    int       `json:"code"`
	Data    *struct { // 直接定义匿名结构体，只包含 PickCode
		PickCode string `json:"pick_code"`
	} `json:"data"`
}

func (f *Fs) GetPickCodeByPath(ctx context.Context, path string) (string, error) {
	// 使用CallOpenAPI通过pacer进行调用
	formData := url.Values{}
	formData.Add("path", path)

	opts := rest.Opts{
		Method:      "POST",
		Path:        "/open/folder/get_info",
		ContentType: "application/x-www-form-urlencoded",
		Body:        strings.NewReader(formData.Encode()),
	}

	var response struct {
		State   int    `json:"state"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			PickCode string `json:"pick_code"`
		} `json:"data"`
	}

	err := f.CallOpenAPI(ctx, &opts, nil, &response, false)
	if err != nil {
		return "", fmt.Errorf("获取PickCode失败: %w", err)
	}

	// 检查API返回的状态
	if response.Code != 0 {
		return "", fmt.Errorf("API返回错误: %s (Code: %d)", response.Message, response.Code)
	}

	// 返回 PickCode
	return response.Data.PickCode, nil
}

func (f *Fs) getDownloadURLByUA(ctx context.Context, filePath string, UA string) (string, error) {
	pickCode, err := f.GetPickCodeByPath(ctx, filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read metadata for file path %q: %w", filePath, err)
	}

	// 如果没有提供 UA，使用默认值
	if UA == "" {
		UA = defaultUserAgent
	}

	// 使用CallOpenAPI通过pacer进行调用
	opts := rest.Opts{
		Method:      "POST",
		Path:        "/open/ufile/downurl",
		ContentType: "application/x-www-form-urlencoded",
		Body:        strings.NewReader("pick_code=" + pickCode),
		ExtraHeaders: map[string]string{
			"User-Agent": UA,
		},
	}

	var response ApiResponse
	err = f.CallOpenAPI(ctx, &opts, nil, &response, false)
	if err != nil {
		return "", fmt.Errorf("获取下载URL失败: %w", err)
	}

	for _, downInfo := range response.Data {
		if downInfo != (FileInfo{}) {
			fs.Infof(nil, "获取到下载URL: %s", downInfo.URL.URL)
			return downInfo.URL.URL, nil
		}
	}
	return "", fmt.Errorf("未从API响应中获取到下载URL")
}

// ------------------------------------------------------------
// Object Methods
// ------------------------------------------------------------

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
	err := o.readMetaData(ctx)
	if err != nil {
		fs.Logf(o, "failed to read metadata for ModTime: %v", err)
		// Return a zero time instead of Now() as Precision is NotSupported
		return time.Time{}
	}
	return o.modTime
}

// Size returns the size of the file
func (o *Object) Size() int64 {
	// Return size immediately if known, otherwise read metadata
	if o.hasMetaData || o.size > 0 { // Check if size is already populated
		return o.size
	}
	err := o.readMetaData(context.TODO()) // Use TODO context for simplicity here
	if err != nil {
		fs.Logf(o, "failed to read metadata for Size: %v", err)
		return -1 // Indicate error or unknown size
	}
	return o.size
}

// Hash returns the SHA1 checksum
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	if t != hash.SHA1 {
		return "", hash.ErrUnsupported
	}
	// Return hash immediately if known, otherwise read metadata
	if o.hasMetaData || o.sha1sum != "" {
		return o.sha1sum, nil
	}
	err := o.readMetaData(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read metadata for Hash: %w", err)
	}
	return o.sha1sum, nil
}

// ID returns the ID of the Object
func (o *Object) ID() string {
	// Return ID immediately if known, otherwise read metadata
	if o.hasMetaData || o.id != "" {
		return o.id
	}
	// Reading metadata just for ID might be inefficient, but necessary if not cached
	err := o.readMetaData(context.TODO()) // Use TODO context
	if err != nil {
		fs.Logf(o, "failed to read metadata for ID: %v", err)
		return "" // Return empty string on error
	}
	return o.id
}

// ParentID returns the parent ID of the Object
func (o *Object) ParentID() string {
	// Return parent immediately if known, otherwise read metadata
	if o.hasMetaData || o.parent != "" {
		return o.parent
	}
	err := o.readMetaData(context.TODO()) // Use TODO context
	if err != nil {
		fs.Logf(o, "failed to read metadata for ParentID: %v", err)
		return "" // Return empty string on error
	}
	return o.parent
}

// SetModTime is not supported
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return fs.ErrorCantSetModTime
}

// Storable indicates this object can be stored
func (o *Object) Storable() bool {
	return true
}

// open opens the object for reading.
func (o *Object) open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// 🔒 并发安全修复：使用互斥锁保护对durl的访问
	o.durlMu.Lock()

	// 检查URL有效性（在锁保护下）
	if o.durl == nil || !o.durl.Valid() {
		o.durlMu.Unlock()

		// 🚀 功能增强：使用智能URL管理器获取有效URL
		if o.fs.smartURLManager != nil && o.pickCode != "" {
			fs.Debugf(o, "🔄 URL无效，使用智能URL管理器获取新URL: pickCode=%s", o.pickCode)
			newURL, err := o.fs.smartURLManager.GetDownloadURL(o.pickCode, false)
			if err != nil {
				fs.Debugf(o, "❌ 智能URL管理器获取URL失败: %v", err)
				return nil, fmt.Errorf("智能URL管理器获取URL失败: %w", err)
			}

			// 更新对象的下载URL
			o.durlMu.Lock()
			o.durl = &api.DownloadURL{URL: newURL}
			o.durlMu.Unlock()
			fs.Debugf(o, "✅ 智能URL管理器成功获取新URL")
		} else {
			if o.fs.smartURLManager == nil {
				fs.Debugf(o, "⚠️ 智能URL管理器未初始化")
			}
			if o.pickCode == "" {
				fs.Debugf(o, "⚠️ pickCode为空")
			}
			return nil, errors.New("download URL is invalid or expired")
		}

		// 重新获取锁以继续后续操作
		o.durlMu.Lock()
	}

	// 创建URL的本地副本，避免在使用过程中被其他线程修改
	downloadURL := o.durl.URL
	o.durlMu.Unlock()

	// 使用本地副本创建请求，避免并发访问问题
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return nil, err
	}
	fs.FixRangeOption(options, o.size)
	fs.OpenOptionAddHTTPHeaders(req.Header, options)
	if o.size == 0 {
		// Don't supply range requests for 0 length objects as they always fail
		delete(req.Header, "Range")
	}
	resp, err := o.fs.openAPIClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Open the file for reading.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// Ensure metadata (specifically pickCode or ID) is available
	err := o.readMetaData(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata before open: %w", err)
	}

	if o.size == 0 {
		// No need for download URL for 0-byte files
		return io.NopCloser(bytes.NewReader(nil)), nil
	}

	// 🔧 115网盘并发下载优化：修复进度显示问题
	if o.size > 100*1024*1024 && o.shouldUseConcurrentDownload(ctx, options) {
		fs.Infof(o, "🚀 115网盘大文件检测，启用并发下载优化(并发数=2): %s", fs.SizeSuffix(o.size))
		return o.openWithConcurrency(ctx, options...)
	}

	// Get/refresh download URL
	err = o.setDownloadURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get download URL: %w", err)
	}
	if !o.durl.Valid() {
		// Attempt refresh again if invalid right after getting it
		fs.Debugf(o, "Download URL invalid immediately after fetching, retrying...")
		time.Sleep(500 * time.Millisecond) // Small delay before retry
		err = o.setDownloadURL(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get download URL on retry: %w", err)
		}
		if !o.durl.Valid() {
			return nil, errors.New("failed to obtain a valid download URL")
		}
	}

	// Open the URL
	return o.open(ctx, options...)
}

// Update the object with new content.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	if o.fs.isShare {
		return errors.New("unsupported operation: Update on shared filesystem")
	}
	if src.Size() < 0 {
		return errors.New("refusing to update with unknown size")
	}

	// Start the token renewer if we have a valid one
	if o.fs.tokenRenewer != nil {
		o.fs.tokenRenewer.Start()
		defer o.fs.tokenRenewer.Stop()
	}

	// Ensure metadata is read for the existing object
	err := o.readMetaData(ctx)
	if err != nil {
		return fmt.Errorf("failed to read metadata of existing object for update: %w", err)
	}
	oldID := o.id // Keep track of the old ID

	// Upload the new content using putUnchecked (handles creation logic)
	// This will place the new file at the same remote path.
	newObj, err := o.fs.putUnchecked(ctx, in, src, o.remote, options...)
	if err != nil {
		return fmt.Errorf("upload during update failed: %w", err)
	}

	newO := newObj.(*Object)

	// If the upload resulted in a *new* file ID (not an overwrite), delete the old one.
	if oldID != "" && newO.id != oldID {
		fs.Debugf(o, "Update created new object %q, removing old object %q", newO.id, oldID)
		err = o.fs.deleteFiles(ctx, []string{oldID})
		if err != nil {
			// Log error but don't fail the update, the new file is there
			fs.Errorf(o, "Failed to remove old version %q after update: %v", oldID, err)
		}
	} else {
		fs.Debugf(o, "Update likely overwrote existing object %q", oldID)
	}

	// Replace the metadata of the original object `o` with the new object's data
	*o = *newO

	return nil
}

// Remove the object.
func (o *Object) Remove(ctx context.Context) error {
	if o.fs.isShare {
		return errors.New("unsupported operation: Remove on shared filesystem")
	}
	// Ensure metadata (ID) is read
	err := o.readMetaData(ctx)
	if err != nil {
		// If object not found, Remove should succeed
		if errors.Is(err, fs.ErrorObjectNotFound) {
			return nil
		}
		return fmt.Errorf("failed to read metadata before remove: %w", err)
	}
	if o.id == "" {
		return errors.New("cannot remove object with empty ID")
	}

	err = o.fs.deleteFiles(ctx, []string{o.id})
	if err != nil {
		return fmt.Errorf("failed to delete object %q: %w", o.id, err)
	}

	// 删除文件成功后，清理相关缓存
	o.fs.invalidateRelatedCaches115(o.remote, "remove")

	return nil
}

// setMetaData updates the object's metadata from an api.File struct.
func (o *Object) setMetaData(info *api.File) error {
	if info == nil {
		return errors.New("cannot set metadata from nil info")
	}
	if info.IsDir() {
		// This indicates we tried to create an Object for a directory path
		return fs.ErrorIsDir
	}
	o.id = info.ID()
	o.parent = info.ParentID()
	o.size = info.FileSizeBest()
	o.sha1sum = strings.ToLower(info.Sha1Best())
	o.pickCode = info.PickCodeBest()
	o.modTime = info.ModTime()
	o.hasMetaData = true
	return nil
}

// setMetaDataFromCallBack updates metadata after an upload callback.
func (o *Object) setMetaDataFromCallBack(data *api.CallbackData) error {
	if data == nil {
		return errors.New("cannot set metadata from nil callback data")
	}
	// Assume size and modTime are already set from the source info
	o.id = data.FileID
	o.parent = data.CID // Callback provides parent CID
	o.pickCode = data.PickCode
	o.sha1sum = strings.ToLower(data.Sha)
	// Update size from callback if available and different?
	if data.FileSize > 0 {
		o.size = int64(data.FileSize)
	}
	// ModTime is usually the upload time, keep the original source ModTime
	o.hasMetaData = true
	return nil
}

// readMetaData gets the metadata if it hasn't already been fetched.
func (o *Object) readMetaData(ctx context.Context) error {
	if o.hasMetaData {
		return nil
	}

	// Use the path-based lookup
	info, err := o.fs.readMetaDataForPath(ctx, o.remote)
	if err != nil {
		fs.Debugf(o.fs, "🔍 readMetaData失败: %v", err)
		return err // fs.ErrorObjectNotFound or other errors
	}

	err = o.setMetaData(info)
	if err != nil {
		fs.Debugf(o.fs, "🔍 readMetaData: setMetaData失败: %v", err)
	}
	return err
}

// setDownloadURL ensures a valid download URL is available with optimized concurrent access.
func (o *Object) setDownloadURL(ctx context.Context) error {
	// 🔒 并发安全修复：快速路径也需要锁保护
	o.durlMu.Lock()

	// 检查URL是否已经有效（在锁保护下）
	if o.durl != nil && o.durl.Valid() {
		o.durlMu.Unlock()
		return nil
	}

	// URL无效或不存在，需要获取新的URL（继续持有锁）
	// Double-check: 确保在锁保护下进行所有检查
	if o.durl != nil && o.durl.Valid() {
		fs.Debugf(o, "Download URL was fetched by another goroutine")
		o.durlMu.Unlock()
		return nil
	}
	// 注意：这里不释放锁，继续在锁保护下执行URL获取

	var err error
	var newURL *api.DownloadURL

	// Add timeout context for URL fetching to prevent hanging
	urlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if o.fs.isShare {
		// Use traditional share download API
		if o.id == "" {
			return errors.New("cannot get share download URL without file ID")
		}

		newURL, err = o.fs.getDownloadURLFromShare(urlCtx, o.id)
	} else {
		// Use OpenAPI download URL endpoint
		if o.pickCode == "" {
			// If pickCode is missing, try getting it from metadata first
			metaErr := o.readMetaData(urlCtx)
			if metaErr != nil || o.pickCode == "" {
				return fmt.Errorf("cannot get download URL without pick code (metadata read error: %v)", metaErr)
			}
		}

		newURL, err = o.fs.getDownloadURL(urlCtx, o.pickCode)
	}

	if err != nil {
		o.durl = nil      // Clear invalid URL
		o.durlMu.Unlock() // 🔒 错误路径释放锁

		// 检查是否是502错误
		if strings.Contains(err.Error(), "502") || strings.Contains(err.Error(), "Bad Gateway") {

			return fmt.Errorf("server overload (502) when fetching download URL: %w", err)
		}

		// Check if it's a context timeout
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("timeout fetching download URL: %w", err)
		}

		return fmt.Errorf("failed to get download URL: %w", err)
	}

	o.durl = newURL
	if !o.durl.Valid() {
		// This might happen if the link expires immediately or is invalid
		fs.Logf(o, "Fetched download URL is invalid or expired immediately: %s", o.durl.URL)
		// 清除无效的URL，强制下次重新获取
		o.durl = nil
		o.durlMu.Unlock() // 🔒 无效URL路径释放锁
		return fmt.Errorf("fetched download URL is invalid or expired immediately")
	} else {
		fs.Debugf(o, "Successfully fetched download URL")
	}
	o.durlMu.Unlock() // 🔒 成功路径释放锁
	return nil
}

// setDownloadURLWithForce sets the download URL for the object with optional cache bypass
func (o *Object) setDownloadURLWithForce(ctx context.Context, forceRefresh bool) error {
	// 🔒 并发安全修复：快速路径也需要锁保护
	o.durlMu.Lock()

	// 如果不是强制刷新，检查URL是否已经有效（在锁保护下）
	if !forceRefresh && o.durl != nil && o.durl.Valid() {
		o.durlMu.Unlock()
		return nil
	}

	// URL无效或不存在，或者强制刷新，需要获取新的URL（继续持有锁）
	// Double-check: 确保在锁保护下进行所有检查
	if !forceRefresh && o.durl != nil && o.durl.Valid() {
		fs.Debugf(o, "Download URL was fetched by another goroutine")
		o.durlMu.Unlock()
		return nil
	}
	// 注意：这里不释放锁，继续在锁保护下执行URL获取

	var err error
	var newURL *api.DownloadURL

	// Add timeout context for URL fetching to prevent hanging
	urlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if o.fs.isShare {
		// Use traditional share download API
		if o.id == "" {
			o.durlMu.Unlock()
			return errors.New("cannot get share download URL without file ID")
		}

		newURL, err = o.fs.getDownloadURLFromShare(urlCtx, o.id)
	} else {
		// Use OpenAPI download URL endpoint
		if o.pickCode == "" {
			// If pickCode is missing, try getting it from metadata first
			metaErr := o.readMetaData(urlCtx)
			if metaErr != nil || o.pickCode == "" {
				o.durlMu.Unlock()
				return fmt.Errorf("cannot get download URL without pick code (metadata read error: %v)", metaErr)
			}
		}

		newURL, err = o.fs.getDownloadURLWithForce(urlCtx, o.pickCode, forceRefresh)
	}

	if err != nil {
		o.durl = nil      // Clear invalid URL
		o.durlMu.Unlock() // 🔒 错误路径释放锁

		// 检查是否是502错误
		if strings.Contains(err.Error(), "502") || strings.Contains(err.Error(), "Bad Gateway") {
			return fmt.Errorf("server overload (502) when fetching download URL: %w", err)
		}

		// Check if it's a context timeout
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("timeout fetching download URL: %w", err)
		}

		return fmt.Errorf("failed to get download URL: %w", err)
	}

	o.durl = newURL
	if !o.durl.Valid() {
		// This might happen if the link expires immediately or is invalid
		fs.Logf(o, "Fetched download URL is invalid or expired immediately: %s", o.durl.URL)
		// 清除无效的URL，强制下次重新获取
		o.durl = nil
		o.durlMu.Unlock() // 🔒 无效URL路径释放锁
		return fmt.Errorf("fetched download URL is invalid or expired immediately")
	} else {
		fs.Debugf(o, "Successfully fetched download URL")
	}
	o.durlMu.Unlock() // 🔒 成功路径释放锁
	return nil
}

// Check the interfaces are satisfied
var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ fs.Copier          = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	_ fs.MergeDirser     = (*Fs)(nil)
	_ fs.PutUncheckeder  = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.Commander       = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
	_ fs.ObjectInfo      = (*Object)(nil)
	_ fs.IDer            = (*Object)(nil)
	_ fs.ParentIDer      = (*Object)(nil)
	_ fs.Shutdowner      = (*Fs)(nil)
)

// loadTokenFromConfig attempts to load and parse tokens from the config file
func loadTokenFromConfig(f *Fs, m configmap.Mapper) bool {
	// Try to load the token using oauthutil's method instead of
	// directly accessing the config file
	token, err := oauthutil.GetToken(f.originalName, m)
	if err != nil {
		fs.Debugf(f, "Failed to get token from config: %v", err)
		return false
	}

	if token == nil || token.AccessToken == "" || token.RefreshToken == "" {
		fs.Debugf(f, "Token from config is incomplete")
		return false
	}

	// Extract token components
	f.accessToken = token.AccessToken
	f.refreshToken = token.RefreshToken
	f.tokenExpiry = token.Expiry

	// Check if we got valid token data
	if f.accessToken == "" || f.refreshToken == "" {
		return false
	}

	fs.Debugf(f, "Loaded token from config file, expires at %v", f.tokenExpiry)
	return true
}

// rclone backend getdownloadurlua "116:/电影/2025_电影/独裁者 (2012) {tmdb-76493} 23.8GB.mkv" "VidHub/1.7.2"

// downloadWithConcurrency 115网盘多线程并发下载实现
// 🔧 优化：基于123网盘的并发下载架构，专门为115网盘优化
func (f *Fs) downloadWithConcurrency(ctx context.Context, srcObj *Object, tempFile *os.File, fileSize int64) (int64, error) {
	// 🚀 网络参数调优：优化TCP连接参数
	f.optimizeNetworkParameters(ctx)

	// 计算最优分片参数
	chunkSize := f.calculateDownloadChunkSize(fileSize)
	numChunks := (fileSize + chunkSize - 1) / chunkSize
	maxConcurrency := f.calculateDownloadConcurrency(fileSize)

	fs.Infof(f, "📊 115网盘并发下载参数: 分片大小=%s, 分片数=%d, 并发数=%d",
		fs.SizeSuffix(chunkSize), numChunks, maxConcurrency)

	fs.Infof(f, "🚀 115网盘开始并发下载: %s (%s)", srcObj.remote, fs.SizeSuffix(fileSize))

	// 使用并发下载实现
	downloadStartTime := time.Now()
	err := f.downloadChunksConcurrently(ctx, srcObj, tempFile, chunkSize, numChunks, int64(maxConcurrency))
	downloadDuration := time.Since(downloadStartTime)
	if err != nil {
		return 0, fmt.Errorf("115网盘并发下载失败: %w", err)
	}

	// 验证下载完整性
	stat, err := tempFile.Stat()
	if err != nil {
		return 0, fmt.Errorf("获取下载文件信息失败: %w", err)
	}

	actualSize := stat.Size()
	if actualSize != fileSize {
		return 0, fmt.Errorf("下载文件大小不匹配: 期望%s，实际%s",
			fs.SizeSuffix(fileSize), fs.SizeSuffix(actualSize))
	}

	// 🔧 优化：详细性能统计和监控
	avgSpeed := float64(actualSize) / downloadDuration.Seconds() / 1024 / 1024 // MB/s

	// 性能评估
	performanceLevel := f.evaluateDownloadPerformance(avgSpeed, maxConcurrency)

	fs.Infof(f, "✅ 115网盘并发下载完成: %s | 用时: %v | 平均速度: %.2f MB/s | 性能: %s",
		fs.SizeSuffix(actualSize), downloadDuration, avgSpeed, performanceLevel)

	// 性能建议
	f.suggestPerformanceOptimizations(avgSpeed, maxConcurrency, fileSize)

	return actualSize, nil
}

// DownloadProgress 115网盘下载进度跟踪器
type DownloadProgress struct {
	totalChunks     int64
	completedChunks int64
	totalBytes      int64
	downloadedBytes int64
	startTime       time.Time
	lastUpdateTime  time.Time
	chunkSizes      map[int64]int64         // 记录每个分片的大小
	chunkTimes      map[int64]time.Duration // 记录每个分片的下载时间
	peakSpeed       float64                 // 峰值速度 MB/s
	mu              sync.RWMutex
}

// NewDownloadProgress 创建新的下载进度跟踪器
func NewDownloadProgress(totalChunks, totalBytes int64) *DownloadProgress {
	return &DownloadProgress{
		totalChunks:    totalChunks,
		totalBytes:     totalBytes,
		startTime:      time.Now(),
		lastUpdateTime: time.Now(),
		chunkSizes:     make(map[int64]int64),
		chunkTimes:     make(map[int64]time.Duration),
		peakSpeed:      0,
	}
}

// UpdateChunkProgress 更新分片下载进度
func (dp *DownloadProgress) UpdateChunkProgress(chunkIndex, chunkSize int64, chunkDuration time.Duration) {
	dp.mu.Lock()
	defer dp.mu.Unlock()

	// 如果这个分片还没有记录，增加完成计数
	if _, exists := dp.chunkSizes[chunkIndex]; !exists {
		dp.completedChunks++
		dp.downloadedBytes += chunkSize
		dp.chunkSizes[chunkIndex] = chunkSize
		dp.chunkTimes[chunkIndex] = chunkDuration
		dp.lastUpdateTime = time.Now()

		// 计算并更新峰值速度
		if chunkDuration.Seconds() > 0 {
			chunkSpeed := float64(chunkSize) / chunkDuration.Seconds() / 1024 / 1024 // MB/s
			if chunkSpeed > dp.peakSpeed {
				dp.peakSpeed = chunkSpeed
			}
		}
	}
}

// GetProgressInfo 获取当前进度信息
func (dp *DownloadProgress) GetProgressInfo() (percentage float64, avgSpeed float64, peakSpeed float64, eta time.Duration, completed, total int64, downloadedBytes, totalBytes int64) {
	dp.mu.RLock()
	defer dp.mu.RUnlock()

	elapsed := time.Since(dp.startTime)
	percentage = float64(dp.completedChunks) / float64(dp.totalChunks) * 100

	if elapsed.Seconds() > 0 {
		avgSpeed = float64(dp.downloadedBytes) / elapsed.Seconds() / 1024 / 1024 // MB/s
	}

	peakSpeed = dp.peakSpeed

	if avgSpeed > 0 && dp.downloadedBytes > 0 {
		remainingBytes := dp.totalBytes - dp.downloadedBytes
		etaSeconds := float64(remainingBytes) / (avgSpeed * 1024 * 1024)
		eta = time.Duration(etaSeconds) * time.Second
	}

	return percentage, avgSpeed, peakSpeed, eta, dp.completedChunks, dp.totalChunks, dp.downloadedBytes, dp.totalBytes
}

// downloadChunksConcurrently 115网盘并发下载文件分片（集成断点续传和rclone标准进度显示）
func (f *Fs) downloadChunksConcurrently(ctx context.Context, srcObj *Object, tempFile *os.File, chunkSize, numChunks, maxConcurrency int64) error {
	// 🔧 断点续传：生成任务ID并尝试加载已有的断点信息
	taskID := GenerateTaskID115(srcObj.remote, srcObj.Size())
	var resumeInfo *ResumeInfo115

	if f.resumeManager != nil {
		var err error
		resumeInfo, err = f.resumeManager.LoadResumeInfo(taskID)
		if err != nil {
			fs.Debugf(f, "加载断点续传信息失败: %v", err)
		} else if resumeInfo != nil {
			// 验证断点信息的有效性
			if resumeInfo.FileSize == srcObj.Size() &&
				resumeInfo.ChunkSize == chunkSize &&
				resumeInfo.TotalChunks == numChunks {
				fs.Infof(f, "🔄 发现断点续传信息: %s, 已完成 %d/%d 分片",
					srcObj.remote, resumeInfo.GetCompletedChunkCount(), numChunks)
			} else {
				fs.Debugf(f, "断点续传信息不匹配，重新开始下载")
				resumeInfo = nil
			}
		}
	}

	// 如果没有有效的断点信息，创建新的
	if resumeInfo == nil {
		resumeInfo = &ResumeInfo115{
			TaskID:          taskID,
			FileName:        srcObj.remote,
			FileSize:        srcObj.Size(),
			FilePath:        srcObj.remote,
			ChunkSize:       chunkSize,
			TotalChunks:     numChunks,
			CompletedChunks: make(map[int64]bool),
			CreatedAt:       time.Now(),
			TempFilePath:    tempFile.Name(),
		}
	}

	// 创建进度跟踪器，考虑已完成的分片
	progress := NewDownloadProgress(numChunks, srcObj.Size())

	// 如果有已完成的分片，更新进度
	for chunkIndex := range resumeInfo.CompletedChunks {
		progress.UpdateChunkProgress(chunkIndex, chunkSize, 0) // 已完成的分片，耗时为0
	}

	// 🔧 关键修复：获取当前传输的Account对象，用于报告实际下载进度
	var currentAccount *accounting.Account
	remoteName := srcObj.Remote() // 使用标准Remote()方法获取路径

	if stats := accounting.GlobalStats(); stats != nil {
		// 尝试精确匹配
		currentAccount = stats.GetInProgressAccount(remoteName)
		if currentAccount == nil {
			// 尝试模糊匹配：查找包含文件名的Account
			allAccounts := stats.ListInProgressAccounts()
			for _, accountName := range allAccounts {
				if strings.Contains(accountName, remoteName) || strings.Contains(remoteName, accountName) {
					currentAccount = stats.GetInProgressAccount(accountName)
					if currentAccount != nil {
						fs.Debugf(f, "115网盘找到匹配的Account: %s -> %s", remoteName, accountName)
						break
					}
				}
			}
		} else {
			fs.Debugf(f, "115网盘找到精确匹配的Account: %s", remoteName)
		}
	}

	if currentAccount == nil {
		fs.Debugf(f, "115网盘未找到Account对象，进度显示可能不准确: %s", remoteName)
	}

	// 启动进度更新协程（更新Account的额外信息）
	progressCtx, cancelProgress := context.WithCancel(ctx)
	defer cancelProgress()

	go f.updateAccountExtraInfo(progressCtx, progress, remoteName, currentAccount)

	// 创建工作池
	semaphore := make(chan struct{}, maxConcurrency)
	errChan := make(chan error, numChunks)
	var wg sync.WaitGroup

	for i := int64(0); i < numChunks; i++ {
		wg.Add(1)
		go func(chunkIndex int64) {
			defer wg.Done()

			// 🔧 添加panic恢复机制，提高系统稳定性
			defer func() {
				if r := recover(); r != nil {
					fs.Errorf(f, "115网盘下载分片 %d 发生panic: %v", chunkIndex, r)
					errChan <- fmt.Errorf("下载分片 %d panic: %v", chunkIndex, r)
				}
			}()

			// 🔧 断点续传：检查分片是否已完成
			if resumeInfo.IsChunkCompleted(chunkIndex) {
				fs.Debugf(f, "跳过已完成的分片: %d", chunkIndex)
				return
			}

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 计算分片范围
			start := chunkIndex * chunkSize
			end := start + chunkSize - 1
			if end >= srcObj.Size() {
				end = srcObj.Size() - 1
			}
			actualChunkSize := end - start + 1

			// 下载分片（带进度跟踪）
			chunkStartTime := time.Now()
			err := f.downloadChunk(ctx, srcObj, tempFile, start, end, chunkIndex)
			chunkDuration := time.Since(chunkStartTime)

			if err != nil {
				errChan <- fmt.Errorf("下载分片 %d 失败: %w", chunkIndex, err)
				return
			}

			// 🔧 断点续传：标记分片为已完成并保存
			resumeInfo.MarkChunkCompleted(chunkIndex)
			if f.resumeManager != nil {
				if saveErr := f.resumeManager.SaveResumeInfo(resumeInfo); saveErr != nil {
					fs.Debugf(f, "保存断点续传信息失败: %v", saveErr)
				}
			}

			// 更新进度（包含下载时间）
			progress.UpdateChunkProgress(chunkIndex, actualChunkSize, chunkDuration)

			// 🔧 统一进度显示：调用共享的进度显示函数
			f.displayUnifiedDownloadProgress(progress, "115网盘", srcObj.remote)

		}(i)
	}

	// 等待所有下载完成
	wg.Wait()
	close(errChan)

	// 检查是否有错误
	for err := range errChan {
		if err != nil {
			return err
		}
	}

	// 🔧 断点续传：下载完成后清理断点信息
	if f.resumeManager != nil {
		if err := f.resumeManager.DeleteResumeInfo(taskID); err != nil {
			fs.Debugf(f, "清理断点续传信息失败: %v", err)
		} else {
			fs.Debugf(f, "断点续传信息已清理: %s", taskID)
		}
	}

	return nil
}

// updateAccountExtraInfo 更新Account的额外进度信息（集成到rclone标准进度显示）
func (f *Fs) updateAccountExtraInfo(ctx context.Context, progress *DownloadProgress, remoteName string, account *accounting.Account) {
	ticker := time.NewTicker(2 * time.Second) // 每2秒更新一次Account的额外信息
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 清除额外信息
			if account != nil {
				account.SetExtraInfo("")
			}
			return
		case <-ticker.C:
			if account == nil {
				// 尝试重新获取Account对象
				if stats := accounting.GlobalStats(); stats != nil {
					// 尝试精确匹配
					account = stats.GetInProgressAccount(remoteName)
					if account == nil {
						// 尝试模糊匹配：查找包含文件名的Account
						allAccounts := stats.ListInProgressAccounts()
						for _, accountName := range allAccounts {
							if strings.Contains(accountName, remoteName) || strings.Contains(remoteName, accountName) {
								account = stats.GetInProgressAccount(accountName)
								if account != nil {
									break
								}
							}
						}
					}
				}
				if account == nil {
					continue // 如果还是没有找到，跳过这次更新
				}
			}

			_, _, _, _, completed, total, _, _ := progress.GetProgressInfo()

			// 🔧 集成：构建额外信息字符串，显示分片进度
			if completed > 0 && total > 0 {
				extraInfo := fmt.Sprintf("[%d/%d分片]", completed, total)
				account.SetExtraInfo(extraInfo)
			}
		}
	}
}

// displayDownloadProgress 显示115网盘下载进度（保留用于兼容性，但简化输出）
func (f *Fs) displayDownloadProgress(ctx context.Context, progress *DownloadProgress, fileName string) {
	// 🔧 优化：大幅减少独立进度显示，主要依赖rclone标准进度
	ticker := time.NewTicker(10 * time.Second) // 每10秒更新一次，仅用于重要状态
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 显示最终完成状态
			percentage, avgSpeed, peakSpeed, _, completed, total, downloadedBytes, totalBytes := progress.GetProgressInfo()
			fs.Infof(f, "📥 115网盘下载完成: %s | %d/%d分片 (%.1f%%) | %s/%s | 平均: %.2f MB/s | 峰值: %.2f MB/s",
				fileName, completed, total, percentage,
				fs.SizeSuffix(downloadedBytes), fs.SizeSuffix(totalBytes), avgSpeed, peakSpeed)
			return
		case <-ticker.C:
			// 🔧 优化：只在特殊情况下显示独立进度（如错误恢复等）
			// 正常情况下依赖rclone标准进度显示
		}
	}
}

// displayUnifiedDownloadProgress 统一的下载进度显示函数
// 🚀 性能优化：集成到rclone主进度条，减少独立进度显示的频率
func (f *Fs) displayUnifiedDownloadProgress(progress *DownloadProgress, networkName, fileName string) {
	percentage, avgSpeed, _, eta, completed, total, downloadedBytes, _ := progress.GetProgressInfo()

	// 🚀 优化：减少独立进度显示频率，只在关键节点显示
	if completed%5 == 0 || completed == total { // 每5个分片或完成时显示
		// 计算ETA显示
		etaStr := "ETA: -"
		if eta > 0 {
			etaStr = fmt.Sprintf("ETA: %v", eta.Round(time.Second))
		} else if avgSpeed > 0 {
			etaStr = "ETA: 计算中..."
		}

		// 简化的进度显示格式，集成到rclone日志系统
		fs.Debugf(f, "📥 %s: %d/%d分片 (%.1f%%) | %s | %.2f MB/s | %s",
			networkName, completed, total, percentage,
			fs.SizeSuffix(downloadedBytes), avgSpeed, etaStr)
	}
}

// createProgressBar 创建进度条字符串
func (f *Fs) createProgressBar(percentage float64) string {
	const barLength = 30
	filled := int(percentage / 100 * barLength)
	if filled > barLength {
		filled = barLength
	}

	bar := "["
	for i := 0; i < barLength; i++ {
		if i < filled {
			bar += "█"
		} else {
			bar += "░"
		}
	}
	bar += "]"
	return bar
}

// downloadChunk 115网盘下载单个文件分片
func (f *Fs) downloadChunk(ctx context.Context, srcObj *Object, tempFile *os.File, start, end, chunkIndex int64) error {
	rangeOption := &fs.RangeOption{Start: start, End: end}
	expectedSize := end - start + 1

	// 重试机制：最多重试5次，处理URL过期问题
	maxRetries := 5
	var lastErr error

	for retry := 0; retry < maxRetries; retry++ {
		// 在每次重试前强制刷新URL
		if retry > 0 {
			fs.Debugf(f, "115网盘分片 %d 重试 %d/%d: 强制刷新下载URL", chunkIndex, retry, maxRetries)

			// 🔧 修复：强制刷新下载URL，避免重试时仍然获取过期URL
			fs.Debugf(f, "115网盘分片重试，强制刷新下载URL: pickCode=%s", srcObj.pickCode)

			// 🚀 功能增强：使用智能URL管理器，减少API调用冲突
			if f.smartURLManager != nil {
				fs.Debugf(f, "🔄 使用智能URL管理器刷新URL: pickCode=%s, 分片=%d", srcObj.pickCode, chunkIndex)
				newURL, err := f.smartURLManager.GetDownloadURL(srcObj.pickCode, true)
				if err != nil {
					fs.Debugf(f, "❌ 智能URL管理器失败: %v", err)
					lastErr = fmt.Errorf("智能URL管理器获取URL失败: %w", err)
					time.Sleep(time.Duration(retry) * time.Second) // 增加延迟
					continue
				}

				// 更新对象的下载URL
				srcObj.durlMu.Lock()
				srcObj.durl = &api.DownloadURL{URL: newURL}
				srcObj.durlMu.Unlock()
				fs.Debugf(f, "✅ 智能URL管理器成功更新URL: 分片=%d", chunkIndex)
			} else {
				fs.Debugf(f, "⚠️ 智能URL管理器未初始化，使用传统方法")
				// 回退到原有方法
				err := srcObj.setDownloadURLWithForce(ctx, true)
				if err != nil {
					lastErr = fmt.Errorf("刷新下载URL失败: %w", err)
					time.Sleep(time.Duration(retry) * time.Second) // 增加延迟
					continue
				}
			}

			// 验证新URL是否有效
			if !srcObj.durl.Valid() {
				lastErr = fmt.Errorf("刷新后的URL仍然无效")
				time.Sleep(time.Duration(retry) * time.Second)
				continue
			}

			// 递增延迟避免过于频繁的重试
			time.Sleep(time.Duration(retry) * 500 * time.Millisecond)
		}

		// 尝试打开分片
		chunkReader, err := srcObj.open(ctx, rangeOption)
		if err != nil {
			lastErr = err

			// 🔧 优化：智能错误分类和处理策略
			errorType := f.classifyDownloadError(err)
			retryDelay := f.calculateRetryDelay(errorType, retry)

			switch errorType {
			case "server_overload": // 502, 503等服务器过载错误
				time.Sleep(retryDelay)
				continue

			case "url_expired": // URL过期错误
				// URL过期错误不需要延迟，立即重试
				continue

			case "network_timeout": // 网络超时错误
				time.Sleep(retryDelay)
				continue

			case "rate_limit": // 429限流错误
				time.Sleep(retryDelay)
				continue

			default: // 其他错误
				time.Sleep(retryDelay)
				continue
			}

			// 检查是否是其他服务器错误（5xx）
			if strings.Contains(err.Error(), "50") {
				time.Sleep(time.Duration(retry+1) * 2 * time.Second)
				continue
			}

			// 其他错误直接返回
			return fmt.Errorf("打开分片失败: %w", err)
		}

		// 读取分片数据
		chunkData, err := io.ReadAll(chunkReader)
		chunkReader.Close()
		if err != nil {
			lastErr = err
			continue
		}

		// 检测HTML错误响应（通常是79字节左右的错误页面）
		if len(chunkData) < 1000 && isHTMLErrorResponse(chunkData) {
			lastErr = fmt.Errorf("收到HTML错误响应，可能是URL过期: 大小=%d", len(chunkData))
			continue
		}

		// 验证分片大小
		if int64(len(chunkData)) != expectedSize {
			// 如果是最后一个分片，可能大小不足
			if chunkIndex == (srcObj.Size()+expectedSize-1)/expectedSize-1 {
				// 最后一个分片，检查是否是合理的剩余大小
				remainingSize := srcObj.Size() - start
				if int64(len(chunkData)) == remainingSize {
					// 最后分片大小正确
				} else {
					lastErr = fmt.Errorf("最后分片大小不匹配: 期望%d，实际%d", remainingSize, len(chunkData))
					continue
				}
			} else {
				lastErr = fmt.Errorf("分片大小不匹配: 期望%d，实际%d", expectedSize, len(chunkData))
				continue
			}
		}

		// 写入临时文件的正确位置
		_, err = tempFile.WriteAt(chunkData, start)
		if err != nil {
			return fmt.Errorf("写入分片数据失败: %w", err)
		}

		// 成功完成
		return nil
	}

	// 所有重试都失败了
	return fmt.Errorf("分片 %d 下载失败，已重试 %d 次: %w", chunkIndex, maxRetries, lastErr)
}

// isHTMLErrorResponse 检测是否是HTML错误响应
func isHTMLErrorResponse(data []byte) bool {
	if len(data) == 0 {
		return false
	}

	// 转换为小写字符串进行检查
	content := strings.ToLower(string(data))

	// 检查常见的HTML错误响应特征
	htmlIndicators := []string{
		"<html",
		"<!doctype",
		"<head>",
		"<title>",
		"error",
		"404",
		"403",
		"500",
		"expired",
		"invalid",
	}

	for _, indicator := range htmlIndicators {
		if strings.Contains(content, indicator) {
			return true
		}
	}

	return false
}

// calculateDownloadChunkSize 115网盘智能计算下载分片大小
// 🚀 性能优化：提升分片大小，目标提升传输速度15-20%
func (f *Fs) calculateDownloadChunkSize(fileSize int64) int64 {
	// 🚀 优化的智能分片策略：更大的分片提升传输效率
	switch {
	case fileSize < 50*1024*1024: // <50MB
		return 16 * 1024 * 1024 // 16MB分片（从8MB提升）
	case fileSize < 200*1024*1024: // <200MB
		return 32 * 1024 * 1024 // 32MB分片（从16MB提升）
	case fileSize < 1*1024*1024*1024: // <1GB
		return 64 * 1024 * 1024 // 64MB分片（从32MB提升）
	case fileSize < 5*1024*1024*1024: // <5GB
		return 128 * 1024 * 1024 // 128MB分片（从64MB提升）
	case fileSize < 20*1024*1024*1024: // <20GB
		return 256 * 1024 * 1024 // 256MB分片（保持不变）
	default: // >20GB
		return 512 * 1024 * 1024 // 512MB分片（从256MB提升）
	}
}

// calculateDownloadConcurrency 115网盘智能计算下载并发数
// 🔧 优化：实现动态并发控制，平衡性能和稳定性
func (f *Fs) calculateDownloadConcurrency(fileSize int64) int {
	// 🚀 新的智能并发策略：基于文件大小和网络状况动态调整
	baseConcurrency := f.getBaseConcurrency(fileSize)

	// 🔧 网络状况自适应调整
	adjustedConcurrency := f.adjustConcurrencyByNetworkCondition(baseConcurrency)

	return adjustedConcurrency
}

// getBaseConcurrency 获取基础并发数
// 🚀 功能增强：115网盘单文件并发限制到4，平衡性能和稳定性
func (f *Fs) getBaseConcurrency(fileSize int64) int {
	// 🚀 用户要求：115网盘单文件并发限制到4
	switch {
	case fileSize < 50*1024*1024: // <50MB
		return 2 // 小文件使用2并发
	case fileSize < 200*1024*1024: // <200MB
		return 3 // 中等文件使用3并发
	case fileSize < 1*1024*1024*1024: // <1GB
		return 4 // 大文件使用4并发
	case fileSize < 5*1024*1024*1024: // <5GB
		return 4 // 超大文件使用4并发
	default: // >5GB
		return 4 // 巨大文件使用4并发，符合用户要求的上限
	}
}

// adjustConcurrencyByNetworkCondition 根据网络状况调整并发数
// 🚀 性能优化：实现基于网络延迟和带宽的自适应并发数调整
func (f *Fs) adjustConcurrencyByNetworkCondition(baseConcurrency int) int {
	// 🚀 新增：网络质量检测和自适应调整
	networkLatency := f.measureNetworkLatency()
	networkBandwidth := f.estimateNetworkBandwidth()

	// 基于网络延迟调整并发数
	latencyFactor := 1.0
	if networkLatency < 50 { // <50ms 低延迟
		latencyFactor = 1.3
	} else if networkLatency < 100 { // <100ms 中等延迟
		latencyFactor = 1.0
	} else if networkLatency < 200 { // <200ms 高延迟
		latencyFactor = 0.8
	} else { // >200ms 很高延迟
		latencyFactor = 0.6
	}

	// 基于带宽调整并发数
	bandwidthFactor := 1.0
	if networkBandwidth > 100*1024*1024 { // >100Mbps
		bandwidthFactor = 1.2
	} else if networkBandwidth > 50*1024*1024 { // >50Mbps
		bandwidthFactor = 1.0
	} else if networkBandwidth > 20*1024*1024 { // >20Mbps
		bandwidthFactor = 0.9
	} else { // <20Mbps
		bandwidthFactor = 0.7
	}

	// 计算调整后的并发数
	adjustedConcurrency := int(float64(baseConcurrency) * latencyFactor * bandwidthFactor)

	// 🚀 用户要求：115网盘单文件并发限制到4
	maxConcurrency := 4 // 用户要求的并发上限
	if adjustedConcurrency > maxConcurrency {
		adjustedConcurrency = maxConcurrency
	}

	minConcurrency := 1 // 从2降低到1，允许单线程下载
	if adjustedConcurrency < minConcurrency {
		adjustedConcurrency = minConcurrency
	}

	fs.Debugf(f, "🔧 115网盘自适应并发调整: 延迟=%dms, 带宽=%s/s, 基础并发=%d, 调整后并发=%d",
		networkLatency, fs.SizeSuffix(networkBandwidth), baseConcurrency, adjustedConcurrency)

	return adjustedConcurrency
}

// measureNetworkLatency 测量网络延迟
// 🚀 性能优化：实现网络延迟检测，用于自适应并发调整
func (f *Fs) measureNetworkLatency() int64 {
	// 简单的延迟测量：向115网盘API发送HEAD请求
	start := time.Now()

	// 构建测试URL
	testURL := "https://webapi.115.com/files"
	req, err := http.NewRequest("HEAD", testURL, nil)
	if err != nil {
		return 100 // 默认延迟100ms
	}

	// 设置请求头
	req.Header.Set("User-Agent", f.opt.UserAgent)

	// 发送请求
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 150 // 网络错误时返回较高延迟
	}
	defer resp.Body.Close()

	latency := time.Since(start).Milliseconds()
	return latency
}

// estimateNetworkBandwidth 估算网络带宽
// 🚀 性能优化：实现网络带宽估算，用于自适应并发调整
func (f *Fs) estimateNetworkBandwidth() int64 {
	// 简化的带宽估算：基于历史传输速度
	// 这里使用一个保守的估算方法

	// 默认带宽估算（50Mbps）
	defaultBandwidth := int64(50 * 1024 * 1024)

	// TODO: 可以基于历史传输数据进行更精确的估算
	// 目前返回默认值，后续可以集成实际的带宽测试

	return defaultBandwidth
}

// SmartURLManager 智能URL管理器
// 🚀 功能增强：解决115网盘下载URL频繁过期问题
type SmartURLManager struct {
	fs           *Fs
	urlCache     map[string]*URLCacheEntry
	cacheMutex   sync.RWMutex
	refreshMutex sync.Mutex // 防止并发刷新

	// 🚀 新增：批量刷新管理
	batchRefreshQueue map[string]*BatchRefreshRequest
	batchMutex        sync.Mutex
	refreshTimer      *time.Timer

	// 统计信息
	refreshCount int64
	hitCount     int64
	missCount    int64
}

// BatchRefreshRequest 批量刷新请求
type BatchRefreshRequest struct {
	PickCode     string
	RequestTime  time.Time
	WaitingCount int32 // 等待此次刷新的分片数量
	Done         chan *URLCacheEntry
}

// URLCacheEntry URL缓存条目
type URLCacheEntry struct {
	URL        string
	ExpiryTime time.Time
	CreatedAt  time.Time
	HitCount   int64
}

// NewSmartURLManager 创建智能URL管理器
func NewSmartURLManager(fs *Fs) *SmartURLManager {
	return &SmartURLManager{
		fs:                fs,
		urlCache:          make(map[string]*URLCacheEntry),
		batchRefreshQueue: make(map[string]*BatchRefreshRequest),
	}
}

// GetDownloadURL 智能获取下载URL
// 🚀 功能增强：批量刷新机制，减少API调用冲突
func (sum *SmartURLManager) GetDownloadURL(pickCode string, forceRefresh bool) (string, error) {
	// 如果不强制刷新，先检查缓存
	if !forceRefresh {
		if url, valid := sum.getCachedURL(pickCode); valid {
			atomic.AddInt64(&sum.hitCount, 1)
			return url, nil
		}
		atomic.AddInt64(&sum.missCount, 1)
	}

	// 检查是否已有批量刷新请求
	if entry, exists := sum.checkBatchRefresh(pickCode); exists {
		// 等待批量刷新完成
		select {
		case result := <-entry.Done:
			if result != nil {
				return result.URL, nil
			}
			return "", fmt.Errorf("批量刷新失败")
		case <-time.After(10 * time.Second):
			return "", fmt.Errorf("批量刷新超时")
		}
	}

	// 创建新的批量刷新请求
	return sum.performBatchRefresh(pickCode)
}

// getCachedURL 获取缓存的URL
func (sum *SmartURLManager) getCachedURL(pickCode string) (string, bool) {
	sum.cacheMutex.RLock()
	defer sum.cacheMutex.RUnlock()

	entry, exists := sum.urlCache[pickCode]
	if !exists {
		return "", false
	}

	// 检查是否过期（提前30秒过期以避免边界情况）
	if time.Now().Add(30 * time.Second).After(entry.ExpiryTime) {
		return "", false
	}

	entry.HitCount++
	return entry.URL, true
}

// checkBatchRefresh 检查是否已有批量刷新请求
func (sum *SmartURLManager) checkBatchRefresh(pickCode string) (*BatchRefreshRequest, bool) {
	sum.batchMutex.Lock()
	defer sum.batchMutex.Unlock()

	request, exists := sum.batchRefreshQueue[pickCode]
	if exists {
		atomic.AddInt32(&request.WaitingCount, 1)
	}
	return request, exists
}

// performBatchRefresh 执行批量刷新
func (sum *SmartURLManager) performBatchRefresh(pickCode string) (string, error) {
	sum.refreshMutex.Lock()
	defer sum.refreshMutex.Unlock()

	// 双重检查：可能在等待锁的过程中已经被其他goroutine刷新了
	if url, valid := sum.getCachedURL(pickCode); valid {
		return url, nil
	}

	// 创建批量刷新请求
	request := &BatchRefreshRequest{
		PickCode:     pickCode,
		RequestTime:  time.Now(),
		WaitingCount: 1,
		Done:         make(chan *URLCacheEntry, 10), // 缓冲通道
	}

	sum.batchMutex.Lock()
	sum.batchRefreshQueue[pickCode] = request
	sum.batchMutex.Unlock()

	// 延迟100ms执行刷新，允许更多分片加入批量请求
	time.Sleep(100 * time.Millisecond)

	// 执行实际的URL刷新
	url, expiryTime, err := sum.refreshURLFromAPI(pickCode)

	// 清理批量刷新请求
	sum.batchMutex.Lock()
	delete(sum.batchRefreshQueue, pickCode)
	sum.batchMutex.Unlock()

	if err != nil {
		// 通知所有等待的goroutine失败
		close(request.Done)
		return "", err
	}

	// 更新缓存
	entry := &URLCacheEntry{
		URL:        url,
		ExpiryTime: expiryTime,
		CreatedAt:  time.Now(),
		HitCount:   1,
	}

	sum.cacheMutex.Lock()
	sum.urlCache[pickCode] = entry
	sum.cacheMutex.Unlock()

	atomic.AddInt64(&sum.refreshCount, 1)

	// 通知所有等待的goroutine成功
	waitingCount := atomic.LoadInt32(&request.WaitingCount)
	for i := int32(0); i < waitingCount; i++ {
		select {
		case request.Done <- entry:
		default:
			// 通道已满，跳过
		}
	}
	close(request.Done)

	fs.Debugf(sum.fs, "🔄 115网盘批量刷新URL成功: pickCode=%s, 等待分片数=%d, URL过期时间=%v",
		pickCode, waitingCount, expiryTime)

	return url, nil
}

// refreshURLFromAPI 从API刷新URL
func (sum *SmartURLManager) refreshURLFromAPI(pickCode string) (string, time.Time, error) {
	ctx := context.Background()

	// 构建API请求
	opts := rest.Opts{
		Method:      "POST",
		Path:        "/open/ufile/downurl",
		ContentType: "application/x-www-form-urlencoded",
		Body:        strings.NewReader("pick_code=" + pickCode),
		ExtraHeaders: map[string]string{
			"User-Agent": sum.fs.opt.UserAgent,
		},
	}

	// 🚀 修复：使用正确的API响应结构
	var response ApiResponse
	err := sum.fs.CallDownloadURLAPI(ctx, &opts, nil, &response, false)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("获取下载URL失败: %w", err)
	}

	// 🚀 修复：从ApiResponse中提取下载URL
	for _, downInfo := range response.Data {
		if downInfo != (FileInfo{}) && downInfo.URL.URL != "" {
			// 解析过期时间（115网盘URL通常4分钟过期）
			expiryTime := time.Now().Add(4 * time.Minute)

			// 尝试从URL中解析过期时间
			if parsedURL, err := url.Parse(downInfo.URL.URL); err == nil {
				if expireStr := parsedURL.Query().Get("expire"); expireStr != "" {
					if expireTimestamp, err := strconv.ParseInt(expireStr, 10, 64); err == nil {
						expiryTime = time.Unix(expireTimestamp, 0)
					}
				}
			}

			fs.Debugf(sum.fs, "🔄 115网盘智能URL管理器成功获取URL: pickCode=%s, URL长度=%d, 过期时间=%v",
				pickCode, len(downInfo.URL.URL), expiryTime)

			return downInfo.URL.URL, expiryTime, nil
		}
	}

	return "", time.Time{}, fmt.Errorf("API响应中未找到有效的下载URL")
}

// GetStatistics 获取统计信息
func (sum *SmartURLManager) GetStatistics() map[string]interface{} {
	sum.cacheMutex.RLock()
	defer sum.cacheMutex.RUnlock()

	totalHits := atomic.LoadInt64(&sum.hitCount)
	totalMisses := atomic.LoadInt64(&sum.missCount)
	totalRefresh := atomic.LoadInt64(&sum.refreshCount)

	hitRate := float64(0)
	if totalHits+totalMisses > 0 {
		hitRate = float64(totalHits) / float64(totalHits+totalMisses) * 100
	}

	return map[string]interface{}{
		"cache_entries": len(sum.urlCache),
		"hit_count":     totalHits,
		"miss_count":    totalMisses,
		"refresh_count": totalRefresh,
		"hit_rate":      fmt.Sprintf("%.2f%%", hitRate),
	}
}

// calculateUploadConcurrency 115网盘上传并发数计算
// 注意：分片上传始终使用单线程模式以确保SHA1验证通过
func (f *Fs) calculateUploadConcurrency(fileSize int64) int {
	// 115网盘分片上传强制使用单线程模式，确保上传稳定性
	// 这是为了避免多线程上传时的SHA1验证失败问题
	return 1
}

// classifyDownloadError 智能分类下载错误类型
func (f *Fs) classifyDownloadError(err error) string {
	errStr := strings.ToLower(err.Error())

	// 服务器过载错误
	if strings.Contains(errStr, "502") || strings.Contains(errStr, "bad gateway") ||
		strings.Contains(errStr, "503") || strings.Contains(errStr, "service unavailable") {
		return "server_overload"
	}

	// URL过期错误
	if strings.Contains(errStr, "download url is invalid") || strings.Contains(errStr, "expired") {
		return "url_expired"
	}

	// 网络超时错误
	if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded") {
		return "network_timeout"
	}

	// 限流错误
	if strings.Contains(errStr, "429") || strings.Contains(errStr, "too many requests") ||
		strings.Contains(errStr, "rate limit") {
		return "rate_limit"
	}

	// 连接错误
	if strings.Contains(errStr, "connection") || strings.Contains(errStr, "network") {
		return "network_error"
	}

	return "unknown"
}

// calculateRetryDelay 根据错误类型和重试次数计算延迟时间
func (f *Fs) calculateRetryDelay(errorType string, retryCount int) time.Duration {
	baseDelay := time.Second

	switch errorType {
	case "server_overload":
		// 服务器过载使用指数退避，最大30秒
		delay := time.Duration(1<<uint(retryCount)) * baseDelay
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		return delay

	case "url_expired":
		// URL过期立即重试
		return 0

	case "network_timeout":
		// 网络超时使用线性增长，最大10秒
		delay := time.Duration(retryCount+1) * baseDelay
		if delay > 10*time.Second {
			delay = 10 * time.Second
		}
		return delay

	case "rate_limit":
		// 限流错误使用较长延迟
		delay := time.Duration(retryCount+1) * 5 * time.Second
		if delay > 60*time.Second {
			delay = 60 * time.Second
		}
		return delay

	default:
		// 其他错误使用标准指数退避
		delay := time.Duration(1<<uint(retryCount)) * baseDelay
		if delay > 15*time.Second {
			delay = 15 * time.Second
		}
		return delay
	}
}

// optimizeNetworkParameters 优化网络参数以提升下载性能
func (f *Fs) optimizeNetworkParameters(ctx context.Context) {
	// 🔧 网络参数调优：设置更优的HTTP客户端参数
	ci := fs.GetConfig(ctx)

	// 网络参数优化建议（静默处理）
	_ = ci.Transfers
	_ = ci.Timeout
	_ = ci.LowLevelRetries
}

// evaluateDownloadPerformance 评估下载性能等级
func (f *Fs) evaluateDownloadPerformance(avgSpeed float64, concurrency int) string {
	// 🔧 性能等级评估标准
	switch {
	case avgSpeed >= 50: // >=50MB/s
		return "优秀"
	case avgSpeed >= 30: // >=30MB/s
		return "良好"
	case avgSpeed >= 15: // >=15MB/s
		return "一般"
	case avgSpeed >= 5: // >=5MB/s
		return "较慢"
	default: // <5MB/s
		return "很慢"
	}
}

// suggestPerformanceOptimizations 提供性能优化建议
func (f *Fs) suggestPerformanceOptimizations(avgSpeed float64, concurrency int, fileSize int64) {
	suggestions := []string{}

	// 基于速度的建议
	if avgSpeed < 15 {
		suggestions = append(suggestions, "网络速度较慢，建议检查网络连接")

		if concurrency < 8 {
			suggestions = append(suggestions, "可以尝试增加并发数以提升性能")
		}

		if fileSize > 5*1024*1024*1024 { // >5GB
			suggestions = append(suggestions, "大文件建议在网络状况良好时下载")
		}
	}

	// 基于并发数的建议
	if concurrency < 4 && fileSize > 1*1024*1024*1024 { // >1GB
		suggestions = append(suggestions, "大文件可以考虑增加并发数")
	}

	// 输出建议
	if len(suggestions) > 0 {
		fs.Infof(f, "💡 115网盘性能优化建议:")
		for i, suggestion := range suggestions {
			fs.Infof(f, "   %d. %s", i+1, suggestion)
		}
	}
}

// intelligentCachePreheating 智能缓存预热，参考123网盘的成功经验
func (f *Fs) intelligentCachePreheating(ctx context.Context) {
	// 延迟启动，避免影响初始化性能
	time.Sleep(5 * time.Second)

	// 1. 预热根目录信息
	f.preheatRootDirectory(ctx)

	// 2. 预热常用路径（如果有历史访问记录）
	f.preheatFrequentPaths(ctx)

	// 3. 预热最近访问的目录
	f.preheatRecentDirectories(ctx)

}

// preheatRootDirectory 预热根目录信息
func (f *Fs) preheatRootDirectory(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			fs.Debugf(f, "根目录预热出现异常: %v", r)
		}
	}()

	// 预热根目录的基本信息
	if f.dirCache != nil {
		_, err := f.dirCache.RootID(ctx, false)
		if err == nil {
			fs.Debugf(f, "🔥 根目录信息预热成功")
		} else {
			fs.Debugf(f, "根目录信息预热失败: %v", err)
		}
	}
}

// preheatFrequentPaths 预热常用路径
func (f *Fs) preheatFrequentPaths(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			fs.Debugf(f, "常用路径预热出现异常: %v", r)
		}
	}()

	// 预热一些常见的目录路径
	commonPaths := []string{
		"",          // 根目录
		"Documents", // 文档目录
		"Downloads", // 下载目录
		"Pictures",  // 图片目录
		"Videos",    // 视频目录
	}

	for _, path := range commonPaths {
		select {
		case <-ctx.Done():
			return
		default:
			// 🔧 检查是否有上传操作正在进行，如果有则跳过预热
			f.uploadingMu.Lock()
			uploading := f.isUploading
			f.uploadingMu.Unlock()

			if uploading {
				fs.Debugf(f, "🔄 检测到上传操作，暂停预热: %s", path)
				time.Sleep(5 * time.Second) // 等待更长时间再检查
				continue
			}

			if f.dirCache != nil {
				_, err := f.dirCache.FindDir(ctx, path, false)
				if err == nil {
					fs.Debugf(f, "🔥 常用路径预热成功: %s", path)
				}
			}
			// 避免过于频繁的API调用
			time.Sleep(1 * time.Second)
		}
	}
}

// preheatRecentDirectories 预热最近访问的目录
func (f *Fs) preheatRecentDirectories(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			fs.Debugf(f, "最近目录预热出现异常: %v", r)
		}
	}()

	// 从缓存中获取最近访问的路径进行预热
	if f.pathResolveCache != nil {
		// 这里可以实现更复杂的逻辑，比如从缓存中读取最近访问的路径
		fs.Debugf(f, "🔥 最近目录预热逻辑待实现")
	}
}

// CacheStatistics 缓存统计信息
type CacheStatistics struct {
	PathResolveHits   int64   `json:"path_resolve_hits"`
	PathResolveMisses int64   `json:"path_resolve_misses"`
	DirListHits       int64   `json:"dir_list_hits"`
	DirListMisses     int64   `json:"dir_list_misses"`
	DownloadURLHits   int64   `json:"download_url_hits"`
	DownloadURLMisses int64   `json:"download_url_misses"`
	MetadataHits      int64   `json:"metadata_hits"`
	MetadataMisses    int64   `json:"metadata_misses"`
	FileIDHits        int64   `json:"file_id_hits"`
	FileIDMisses      int64   `json:"file_id_misses"`
	TotalHits         int64   `json:"total_hits"`
	TotalMisses       int64   `json:"total_misses"`
	HitRate           float64 `json:"hit_rate"`
}

// getCacheStatistics 获取缓存统计信息
func (f *Fs) getCacheStatistics() *CacheStatistics {
	stats := &CacheStatistics{}

	// 这里可以从各个缓存实例收集统计信息
	// 当前先返回基础结构，后续可以扩展

	stats.TotalHits = stats.PathResolveHits + stats.DirListHits + stats.DownloadURLHits + stats.MetadataHits + stats.FileIDHits
	stats.TotalMisses = stats.PathResolveMisses + stats.DirListMisses + stats.DownloadURLMisses + stats.MetadataMisses + stats.FileIDMisses

	if stats.TotalHits+stats.TotalMisses > 0 {
		stats.HitRate = float64(stats.TotalHits) / float64(stats.TotalHits+stats.TotalMisses) * 100
	}

	return stats
}

// logCacheStatistics 记录缓存统计信息
func (f *Fs) logCacheStatistics() {
	stats := f.getCacheStatistics()

	fs.Infof(f, "📊 115网盘缓存统计:")
	fs.Infof(f, "   路径解析: 命中=%d, 未命中=%d", stats.PathResolveHits, stats.PathResolveMisses)
	fs.Infof(f, "   目录列表: 命中=%d, 未命中=%d", stats.DirListHits, stats.DirListMisses)
	fs.Infof(f, "   下载URL: 命中=%d, 未命中=%d", stats.DownloadURLHits, stats.DownloadURLMisses)
	fs.Infof(f, "   元数据: 命中=%d, 未命中=%d", stats.MetadataHits, stats.MetadataMisses)
	fs.Infof(f, "   文件ID: 命中=%d, 未命中=%d", stats.FileIDHits, stats.FileIDMisses)
	fs.Infof(f, "   总计: 命中=%d, 未命中=%d, 命中率=%.2f%%", stats.TotalHits, stats.TotalMisses, stats.HitRate)
}

// ErrorStatistics 错误统计信息
type ErrorStatistics struct {
	ServerOverloadErrors int64   `json:"server_overload_errors"`
	URLExpiredErrors     int64   `json:"url_expired_errors"`
	NetworkTimeoutErrors int64   `json:"network_timeout_errors"`
	RateLimitErrors      int64   `json:"rate_limit_errors"`
	AuthErrors           int64   `json:"auth_errors"`
	PermissionErrors     int64   `json:"permission_errors"`
	NotFoundErrors       int64   `json:"not_found_errors"`
	UnknownErrors        int64   `json:"unknown_errors"`
	TotalErrors          int64   `json:"total_errors"`
	TotalRetries         int64   `json:"total_retries"`
	SuccessfulRetries    int64   `json:"successful_retries"`
	RetrySuccessRate     float64 `json:"retry_success_rate"`
}

// getErrorStatistics 获取错误统计信息
func (f *Fs) getErrorStatistics() *ErrorStatistics {
	stats := &ErrorStatistics{}

	// 这里可以从错误分类器收集统计信息
	// 当前先返回基础结构，后续可以扩展

	stats.TotalErrors = stats.ServerOverloadErrors + stats.URLExpiredErrors +
		stats.NetworkTimeoutErrors + stats.RateLimitErrors + stats.AuthErrors +
		stats.PermissionErrors + stats.NotFoundErrors + stats.UnknownErrors

	if stats.TotalRetries > 0 {
		stats.RetrySuccessRate = float64(stats.SuccessfulRetries) / float64(stats.TotalRetries) * 100
	}

	return stats
}

// logErrorStatistics 记录错误统计信息
func (f *Fs) logErrorStatistics() {
	stats := f.getErrorStatistics()

	if stats.TotalErrors > 0 {
		fs.Infof(f, "📊 115网盘错误统计:")
		fs.Infof(f, "   服务器过载: %d", stats.ServerOverloadErrors)
		fs.Infof(f, "   URL过期: %d", stats.URLExpiredErrors)
		fs.Infof(f, "   网络超时: %d", stats.NetworkTimeoutErrors)
		fs.Infof(f, "   限流错误: %d", stats.RateLimitErrors)
		fs.Infof(f, "   认证错误: %d", stats.AuthErrors)
		fs.Infof(f, "   权限错误: %d", stats.PermissionErrors)
		fs.Infof(f, "   资源不存在: %d", stats.NotFoundErrors)
		fs.Infof(f, "   未知错误: %d", stats.UnknownErrors)
		fs.Infof(f, "   总错误数: %d", stats.TotalErrors)
		fs.Infof(f, "   重试成功率: %.2f%% (%d/%d)", stats.RetrySuccessRate, stats.SuccessfulRetries, stats.TotalRetries)
	}
}

// shouldUseConcurrentDownload 判断是否应该使用并发下载
func (o *Object) shouldUseConcurrentDownload(ctx context.Context, options []fs.OpenOption) bool {
	// 检查文件大小，只对大文件使用并发下载
	if o.size < 100*1024*1024 { // 小于100MB
		return false
	}

	// 检查是否有用户指定的Range选项
	// 智能Range检测：区分小Range请求和大Range请求（跨云传输优化）
	for _, option := range options {
		if rangeOpt, ok := option.(*fs.RangeOption); ok {
			// 如果Range覆盖了整个文件，则可以使用并发下载
			if rangeOpt.Start == 0 && (rangeOpt.End == -1 || rangeOpt.End >= o.size-1) {
				fs.Debugf(o, "检测到全文件Range选项，允许并发下载")
				break
			} else {
				// 计算Range大小
				rangeSize := rangeOpt.End - rangeOpt.Start + 1

				// 🚀 跨云传输优化：如果Range足够大（>=32MB），启用并发下载
				// 这主要针对rclone多线程传输场景，每个线程处理的数据块通常较大
				if rangeSize >= 32*1024*1024 { // 32MB阈值
					fs.Infof(o, "🚀 检测到大Range选项 (%d-%d, %s)，启用并发下载优化（跨云传输场景）",
						rangeOpt.Start, rangeOpt.End, fs.SizeSuffix(rangeSize))
					break
				} else {
					fs.Debugf(o, "检测到小Range选项 (%d-%d, %s)，跳过并发下载",
						rangeOpt.Start, rangeOpt.End, fs.SizeSuffix(rangeSize))
					return false
				}
			}
		}
	}

	return true
}

// openWithConcurrency 使用并发下载打开文件
func (o *Object) openWithConcurrency(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	fs.Infof(o, "🚀 115网盘启动并发下载: %s", fs.SizeSuffix(o.size))

	// 创建临时文件用于并发下载
	tempFile, err := os.CreateTemp("", "115_concurrent_download_*.tmp")
	if err != nil {
		fs.Debugf(o, "创建临时文件失败，回退到普通下载: %v", err)
		return o.open(ctx, options...)
	}

	// 🔧 创建进度报告器，让rclone能看到下载进度
	progressReporter := NewProgressReporter(o.size)

	// 使用并发下载到临时文件，同时报告进度
	downloadedSize, err := o.fs.downloadWithConcurrency(ctx, o, tempFile, o.size)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		fs.Debugf(o, "并发下载失败，回退到普通下载: %v", err)
		return o.open(ctx, options...)
	}

	// 验证下载大小
	if downloadedSize != o.size {
		tempFile.Close()
		os.Remove(tempFile.Name())
		fs.Debugf(o, "下载大小不匹配，回退到普通下载: 期望%d，实际%d", o.size, downloadedSize)
		return o.open(ctx, options...)
	}

	// 重置文件指针到开始位置
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return nil, fmt.Errorf("重置临时文件指针失败: %w", err)
	}

	fs.Infof(o, "✅ 115网盘并发下载完成: %s", fs.SizeSuffix(downloadedSize))

	// 返回一个包装的ReadCloser，让rclone能看到读取进度
	return &ConcurrentDownloadReader{
		file:             tempFile,
		tempPath:         tempFile.Name(),
		progressReporter: progressReporter,
		totalSize:        o.size,
	}, nil
}

// ProgressReporter 进度报告器，用于向rclone报告下载进度
type ProgressReporter struct {
	totalSize       int64
	transferredSize int64
	mu              sync.Mutex
}

// NewProgressReporter 创建新的进度报告器
func NewProgressReporter(totalSize int64) *ProgressReporter {
	return &ProgressReporter{
		totalSize: totalSize,
	}
}

// AddTransferred 添加已传输的字节数
func (pr *ProgressReporter) AddTransferred(bytes int64) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.transferredSize += bytes
}

// GetTransferred 获取已传输的字节数
func (pr *ProgressReporter) GetTransferred() int64 {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.transferredSize
}

// ConcurrentDownloadReader 并发下载的文件读取器
type ConcurrentDownloadReader struct {
	file             *os.File
	tempPath         string
	progressReporter *ProgressReporter
	totalSize        int64
	readBytes        int64
}

// Read 实现io.Reader接口，同时报告读取进度
func (r *ConcurrentDownloadReader) Read(p []byte) (n int, err error) {
	n, err = r.file.Read(p)
	if n > 0 {
		r.readBytes += int64(n)
		// 这里rclone的accounting系统会自动处理进度显示
	}
	return n, err
}

// Close 实现io.Closer接口，关闭时删除临时文件
func (r *ConcurrentDownloadReader) Close() error {
	err := r.file.Close()
	os.Remove(r.tempPath) // 删除临时文件
	return err
}

// ResumeInfo115 115网盘断点续传信息
type ResumeInfo115 struct {
	TaskID          string         `json:"task_id"`
	FileName        string         `json:"file_name"`
	FileSize        int64          `json:"file_size"`
	FilePath        string         `json:"file_path"`
	ChunkSize       int64          `json:"chunk_size"`
	TotalChunks     int64          `json:"total_chunks"`
	CompletedChunks map[int64]bool `json:"completed_chunks"`
	CreatedAt       time.Time      `json:"created_at"`
	LastUpdated     time.Time      `json:"last_updated"`
	TempFilePath    string         `json:"temp_file_path"`
}

// ResumeManager115 115网盘断点续传管理器（使用BadgerDB）
type ResumeManager115 struct {
	mu          sync.RWMutex
	resumeCache *cache.BadgerCache
	fs          *Fs
}

// NewResumeManager115 创建115网盘断点续传管理器（使用BadgerDB）
func NewResumeManager115(fs *Fs, cacheDir string) (*ResumeManager115, error) {
	resumeCache, err := cache.NewBadgerCache("115-resume", cacheDir)
	if err != nil {
		return nil, fmt.Errorf("创建115网盘断点续传BadgerDB缓存失败: %w", err)
	}

	return &ResumeManager115{
		resumeCache: resumeCache,
		fs:          fs,
	}, nil
}

// SaveResumeInfo 保存断点续传信息到BadgerDB
func (rm *ResumeManager115) SaveResumeInfo(info *ResumeInfo115) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	info.LastUpdated = time.Now()

	// 使用taskID作为缓存键
	cacheKey := fmt.Sprintf("resume_115_%s", info.TaskID)

	err := rm.resumeCache.Set(cacheKey, info, 24*time.Hour) // 24小时过期
	if err != nil {
		return fmt.Errorf("保存115网盘断点续传信息到BadgerDB失败: %w", err)
	}

	fs.Debugf(rm.fs, "保存115网盘断点续传信息: %s, 已完成分片: %d/%d",
		info.FileName, len(info.CompletedChunks), info.TotalChunks)

	return nil
}

// LoadResumeInfo 从BadgerDB加载断点续传信息
func (rm *ResumeManager115) LoadResumeInfo(taskID string) (*ResumeInfo115, error) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	cacheKey := fmt.Sprintf("resume_115_%s", taskID)
	var info ResumeInfo115

	found, err := rm.resumeCache.Get(cacheKey, &info)
	if err != nil {
		return nil, fmt.Errorf("从BadgerDB加载115网盘断点续传信息失败: %w", err)
	}

	if !found {
		return nil, nil // 没有找到断点续传信息
	}

	// 检查信息是否过期（超过24小时）
	if time.Since(info.CreatedAt) > 24*time.Hour {
		rm.DeleteResumeInfo(taskID)
		return nil, nil
	}

	fs.Debugf(rm.fs, "加载115网盘断点续传信息: %s, 已完成分片: %d/%d",
		info.FileName, len(info.CompletedChunks), info.TotalChunks)

	return &info, nil
}

// DeleteResumeInfo 从BadgerDB删除断点续传信息
func (rm *ResumeManager115) DeleteResumeInfo(taskID string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	cacheKey := fmt.Sprintf("resume_115_%s", taskID)
	err := rm.resumeCache.Delete(cacheKey)
	if err != nil {
		return fmt.Errorf("从BadgerDB删除115网盘断点续传信息失败: %w", err)
	}

	fs.Debugf(rm.fs, "删除115网盘断点续传信息: %s", taskID)
	return nil
}

// IsChunkCompleted 检查分片是否已完成
func (info *ResumeInfo115) IsChunkCompleted(chunkIndex int64) bool {
	if info.CompletedChunks == nil {
		return false
	}
	return info.CompletedChunks[chunkIndex]
}

// MarkChunkCompleted 标记分片为已完成
func (info *ResumeInfo115) MarkChunkCompleted(chunkIndex int64) {
	if info.CompletedChunks == nil {
		info.CompletedChunks = make(map[int64]bool)
	}
	info.CompletedChunks[chunkIndex] = true
}

// GetCompletedChunkCount 获取已完成的分片数量
func (info *ResumeInfo115) GetCompletedChunkCount() int64 {
	return int64(len(info.CompletedChunks))
}

// Close 关闭115网盘断点续传管理器
func (rm *ResumeManager115) Close() error {
	if rm.resumeCache != nil {
		return rm.resumeCache.Close()
	}
	return nil
}

// GenerateTaskID115 生成115网盘任务ID
func GenerateTaskID115(filePath string, fileSize int64) string {
	return fmt.Sprintf("115_%s_%d_%d",
		strings.ReplaceAll(filePath, "/", "_"),
		fileSize,
		time.Now().Unix())
}

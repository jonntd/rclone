package _123

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"golang.org/x/oauth2"

	// The SDK for 123 Pan
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
)

const (
	openAPIRootURL     = "https://open-api.123pan.com"
	defaultUserAgent   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	tokenRefreshWindow = 10 * time.Minute // tokenRefreshWindow defines how long before expiry we try to refresh the token

	// API rate limiting based on 123Pan official limits
	// Different APIs have different QPS limits, so we use multiple pacers
	listAPIMinSleep   = 334 * time.Millisecond  // ~3 QPS for api/v2/file/list
	strictAPIMinSleep = 1000 * time.Millisecond // 1 QPS for user/info, move, delete
	uploadAPIMinSleep = 500 * time.Millisecond  // 2 QPS for upload APIs
	maxSleep          = 30 * time.Second        // Longer max sleep for 429 backoff
	decayConstant     = 2

	// Upload constants
	defaultChunkSize    = 5 * fs.Mebi
	minChunkSize        = 100 * fs.Kibi
	maxChunkSize        = 5 * fs.Gibi
	defaultUploadCutoff = 200 * fs.Mebi
	maxUploadParts      = 10000

	// Connection and timeout settings
	defaultConnTimeout = 30 * time.Second
	defaultTimeout     = 300 * time.Second
)

// Options defines the configuration for this backend
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
}

// Fs represents a remote 123Pan drive
type Fs struct {
	name         string       // name of this remote
	originalName string       // Original config name without modifications
	root         string       // the path we are working on
	opt          Options      // parsed options
	features     *fs.Features // optional features

	// API clients and authentication
	client       *http.Client     // HTTP client for API calls
	token        string           // cached access token
	tokenExpiry  time.Time        // expiry of the cached access token
	tokenMu      sync.Mutex       // mutex to protect token and tokenExpiry
	tokenRenewer *oauthutil.Renew // token renewer

	// Configuration and state
	m            configmap.Mapper // config mapper
	rootFolderID string           // ID of the root folder
	userID       string           // User ID from API

	// Performance and rate limiting - multiple pacers for different API limits
	listPacer   *fs.Pacer // pacer for file list APIs (3 QPS)
	strictPacer *fs.Pacer // pacer for strict APIs like user/info, move, delete (1 QPS)
	uploadPacer *fs.Pacer // pacer for upload APIs (2 QPS)

	// Directory cache
	dirCache *dircache.DirCache
}

// Object describes a 123Pan object
type Object struct {
	fs          *Fs       // parent filesystem
	remote      string    // remote path
	hasMetaData bool      // whether metadata has been loaded
	id          string    // file ID
	size        int64     // file size
	md5sum      string    // MD5 hash
	modTime     time.Time // modification time
	createTime  time.Time // creation time
	isDir       bool      // whether this is a directory
}

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "pan123drive",
		Description: "123Pan Drive",
		NewFs:       newFs,
		CommandHelp: commandHelp,
		Options: []fs.Option{{
			Name:     "client_id",
			Help:     "123Pan API Client ID.",
			Required: true,
		}, {
			Name:      "client_secret",
			Help:      "123Pan API Client Secret.",
			Required:  true,
			Sensitive: true,
		}, {
			Name:      "token",
			Help:      "OAuth Access Token as a JSON blob. Leave blank normally.",
			Sensitive: true,
		}, {
			Name:     "user_agent",
			Help:     "User-Agent header for API requests.",
			Default:  defaultUserAgent,
			Advanced: true,
		}, {
			Name:     "root_folder_id",
			Help:     "ID of the root folder. Leave blank for root directory.",
			Default:  "0",
			Advanced: true,
		}, {
			Name:     "list_chunk",
			Help:     "Number of files to request in each list chunk.",
			Default:  100,
			Advanced: true,
		}, {
			Name:     "pacer_min_sleep",
			Help:     "Minimum time to sleep between strict API calls (user/info, move, delete).",
			Default:  strictAPIMinSleep,
			Advanced: true,
		}, {
			Name:     "list_pacer_min_sleep",
			Help:     "Minimum time to sleep between file list API calls.",
			Default:  listAPIMinSleep,
			Advanced: true,
		}, {
			Name:     "conn_timeout",
			Help:     "Connection timeout for API requests.",
			Default:  defaultConnTimeout,
			Advanced: true,
		}, {
			Name:     "timeout",
			Help:     "Timeout for API requests.",
			Default:  defaultTimeout,
			Advanced: true,
		}, {
			Name:     "chunk_size",
			Help:     "Chunk size for multipart uploads.",
			Default:  defaultChunkSize,
			Advanced: true,
		}, {
			Name:     "upload_cutoff",
			Help:     "Cutoff for switching to multipart upload.",
			Default:  defaultUploadCutoff,
			Advanced: true,
		}, {
			Name:     "max_upload_parts",
			Help:     "Maximum number of parts in a multipart upload.",
			Default:  maxUploadParts,
			Advanced: true,
		}},
	})
}

var commandHelp = []fs.CommandHelp{{
	Name:  "getdownloadurlua",
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
	Data    GetFileListRespDataV2 `json:"data"` // Changed to specific type
}

// GetFileListRespDataV2 represents the data structure for file listing response
type GetFileListRespDataV2 struct {
	// -1代表最后一页（无需再翻页查询）, 其他代表下一页开始的文件id，携带到请求参数中
	LastFileId int64 `json:"lastFileId"`
	// 文件列表
	FileList []FileListInfoRespDataV2 `json:"fileList"`
}

// FileListInfoRespDataV2 represents a file or folder item from the API response
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
	// Construct query parameters
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

	// Construct endpoint with query parameters
	endpoint := "/api/v2/file/list?" + params.Encode()

	// Use apiCall for proper rate limiting and error handling
	var result ListResponse
	err := f.apiCall(ctx, "GET", endpoint, nil, &result)
	if err != nil {
		return nil, err
	}

	return &result, nil
}

func (f *Fs) pathToFileID(ctx context.Context, filePath string) (string, error) {
	// Root directory
	if filePath == "/" {
		return "0", nil
	}
	if len(filePath) > 1 && strings.HasSuffix(filePath, "/") {
		filePath = filePath[:len(filePath)-1]
	}
	currentID := "0"
	parts := strings.Split(filePath, "/")
	if len(parts) > 0 && parts[0] == "" { // Remove empty string if path starts with /
		parts = parts[1:]
	}
	for _, part := range parts {
		if part == "" {
			continue
		}
		findPart := false
		next := "0" // Use string for next as per API
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
			// Use list pacer for rate limiting
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
				// No need for manual sleep - pacer handles rate limiting
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

// FileInfo represents a single file's information in the 'list' array
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

// DataContainer holds the 'list' array
type DataContainer struct {
	List []FileInfo `json:"list"`
}

// FileInfosResponse is the top-level structure of the API response

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

// shouldRetry determines whether to retry an API call based on the response and error
func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}

	// Retry on network errors
	if err != nil {
		return fserrors.ShouldRetry(err), err
	}

	// Check HTTP status codes
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			// Rate limited - should retry with backoff
			return true, fserrors.NewErrorRetryAfter(time.Second)
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			// Server errors - should retry
			return true, err
		case http.StatusUnauthorized:
			// Token might be expired - let the caller handle token refresh
			return false, fserrors.NewErrorRetryAfter(time.Second)
		}
	}

	return false, err
}

// errorHandler handles API errors and converts them to appropriate rclone errors
func errorHandler(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read error response: %w", err)
	}

	// Try to parse as 123Pan API error response
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

	// Fallback to HTTP status code
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

// getPacerForEndpoint returns the appropriate pacer based on the API endpoint
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
	case strings.Contains(endpoint, "/upload/v1/file/"):
		return f.uploadPacer // 2 QPS
	case strings.Contains(endpoint, "/api/v1/access_token"):
		return f.strictPacer // 1 QPS
	default:
		// Default to strict pacer for unknown endpoints to be safe
		return f.strictPacer
	}
}

// apiCall makes an API call with proper error handling and rate limiting
func (f *Fs) apiCall(ctx context.Context, method, endpoint string, body io.Reader, result interface{}) error {
	pacer := f.getPacerForEndpoint(endpoint)
	return pacer.Call(func() (bool, error) {
		// Ensure token is valid before making API calls
		err := f.refreshTokenIfNecessary(ctx, false, false)
		if err != nil {
			return false, err
		}

		req, err := http.NewRequestWithContext(ctx, method, openAPIRootURL+endpoint, body)
		if err != nil {
			return false, err
		}

		// Set headers
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Platform", "open_platform")
		req.Header.Set("User-Agent", f.opt.UserAgent)

		// Add timeout to request
		if f.opt.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(f.opt.Timeout))
			defer cancel()
			req = req.WithContext(ctx)
		}

		resp, err := f.client.Do(req)
		if err != nil {
			// Check if we should retry
			if shouldRetry, retryErr := shouldRetry(ctx, resp, err); shouldRetry {
				return true, retryErr
			}
			return false, err
		}
		defer resp.Body.Close()

		// Check for HTTP errors first
		if resp.StatusCode >= 400 {
			return f.handleHTTPError(ctx, resp)
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, err
		}

		// Parse base response to check for errors
		var baseResp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err = json.Unmarshal(bodyBytes, &baseResp); err != nil {
			return false, err
		}

		// Handle API errors
		switch baseResp.Code {
		case 0:
			// Success
			if result != nil {
				return false, json.Unmarshal(bodyBytes, result)
			}
			return false, nil
		case 401:
			// Token expired, try to refresh
			fs.Debugf(f, "Token expired, attempting refresh")
			err = f.refreshTokenIfNecessary(ctx, false, true)
			if err != nil {
				return false, err
			}
			return true, nil // Retry with new token
		case 429:
			// Rate limited - use longer backoff for 429 errors
			fs.Debugf(f, "Rate limited (API error 429), will retry with longer backoff")
			return true, fserrors.NewErrorRetryAfter(30 * time.Second)
		default:
			return false, fmt.Errorf("API error %d: %s", baseResp.Code, baseResp.Message)
		}
	})
}

// handleHTTPError handles HTTP-level errors and determines retry behavior
func (f *Fs) handleHTTPError(ctx context.Context, resp *http.Response) (bool, error) {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		// Token might be expired - try refresh once
		fs.Debugf(f, "Got 401, attempting token refresh")
		err := f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			return false, fmt.Errorf("authentication failed: %w", err)
		}
		return true, nil // Retry with new token
	case http.StatusTooManyRequests:
		// Rate limited - should retry with longer backoff
		fs.Debugf(f, "Rate limited (HTTP 429), will retry with longer backoff")
		return true, fserrors.NewErrorRetryAfter(30 * time.Second)
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		// Server errors - should retry
		fs.Debugf(f, "Server error %d, will retry", resp.StatusCode)
		return true, fmt.Errorf("server error: HTTP %d", resp.StatusCode)
	case http.StatusNotFound:
		return false, fs.ErrorObjectNotFound
	case http.StatusForbidden:
		return false, fmt.Errorf("permission denied: HTTP %d", resp.StatusCode)
	default:
		// Read error body for more details
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(body))
	}
}

// getFileInfo gets detailed information about a file by ID
func (f *Fs) getFileInfo(ctx context.Context, fileID string) (*FileListInfoRespDataV2, error) {
	// Validate fileID
	_, err := strconv.ParseInt(fileID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid file ID: %s", fileID)
	}

	// Use the file detail API
	var response FileDetailResponse
	endpoint := fmt.Sprintf("/api/v1/file/detail?fileID=%s", fileID)

	err = f.apiCall(ctx, "GET", endpoint, nil, &response)
	if err != nil {
		return nil, err
	}

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	// Convert to our standard format
	fileInfo := &FileListInfoRespDataV2{
		FileID:       response.Data.FileID,
		Filename:     response.Data.Filename,
		Type:         response.Data.Type,
		Size:         response.Data.Size,
		Etag:         response.Data.ETag,
		Status:       response.Data.Status,
		ParentFileID: response.Data.ParentFileID,
		Category:     0, // Not provided in detail response
	}

	return fileInfo, nil
}

// getDownloadURL gets the download URL for a file
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

// createDirectory creates a new directory
func (f *Fs) createDirectory(ctx context.Context, parentID, name string) error {
	// Convert parentID to int64
	parentFileID, err := strconv.ParseInt(parentID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid parent ID: %s", parentID)
	}

	// Prepare request body
	reqBody := map[string]interface{}{
		"parentID": strconv.FormatInt(parentFileID, 10),
		"name":     name,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	// Make API call
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

// createUpload creates an upload session
func (f *Fs) createUpload(ctx context.Context, parentFileID int64, filename, etag string, size int64) (*UploadCreateResp, error) {
	reqBody := map[string]interface{}{
		"parentFileId": parentFileID,
		"filename":     filename,
		"etag":         strings.ToLower(etag),
		"size":         size,
		"duplicate":    2, // Allow duplicates
		"containDir":   false,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	var response UploadCreateResp
	err = f.apiCall(ctx, "POST", "/upload/v1/file/create", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return nil, err
	}

	if response.Code != 0 {
		return nil, fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return &response, nil
}

// uploadFile uploads file content using the upload session
func (f *Fs) uploadFile(ctx context.Context, in io.Reader, createResp *UploadCreateResp, size int64) error {
	// Determine chunk size
	chunkSize := createResp.Data.SliceSize
	if chunkSize <= 0 {
		chunkSize = int64(f.opt.ChunkSize)
	}

	// Ensure chunk size is within limits
	if chunkSize < int64(minChunkSize) {
		chunkSize = int64(minChunkSize)
	}
	if chunkSize > int64(maxChunkSize) {
		chunkSize = int64(maxChunkSize)
	}

	// For small files, read all into memory
	if size <= chunkSize {
		data, err := io.ReadAll(in)
		if err != nil {
			return fmt.Errorf("failed to read file data: %w", err)
		}

		if int64(len(data)) != size {
			return fmt.Errorf("size mismatch: expected %d, got %d", size, len(data))
		}

		// Single part upload
		return f.uploadSinglePart(ctx, createResp.Data.PreuploadID, data)
	}

	// Multi-part upload for larger files
	return f.uploadMultiPart(ctx, in, createResp.Data.PreuploadID, size, chunkSize)
}

// uploadSinglePart uploads a file in a single part
func (f *Fs) uploadSinglePart(ctx context.Context, preuploadID string, data []byte) error {
	// Get upload URL for part 1
	uploadURL, err := f.getUploadURL(ctx, preuploadID, 1)
	if err != nil {
		return fmt.Errorf("failed to get upload URL: %w", err)
	}

	// Upload the data
	err = f.uploadPart(ctx, uploadURL, data)
	if err != nil {
		return fmt.Errorf("failed to upload part: %w", err)
	}

	// Complete the upload
	return f.completeUpload(ctx, preuploadID)
}

// uploadMultiPart uploads a file in multiple parts
func (f *Fs) uploadMultiPart(ctx context.Context, in io.Reader, preuploadID string, size, chunkSize int64) error {
	uploadNums := (size + chunkSize - 1) / chunkSize

	// Limit the number of parts
	if uploadNums > int64(f.opt.MaxUploadParts) {
		return fmt.Errorf("file too large: would require %d parts, max allowed is %d", uploadNums, f.opt.MaxUploadParts)
	}

	buffer := make([]byte, chunkSize)

	for partIndex := int64(0); partIndex < uploadNums; partIndex++ {
		partNumber := partIndex + 1

		// Read chunk data
		var partData []byte
		if partIndex == uploadNums-1 {
			// Last part - read remaining data
			remaining := size - partIndex*chunkSize
			partData = make([]byte, remaining)
			_, err := io.ReadFull(in, partData)
			if err != nil {
				return fmt.Errorf("failed to read part %d: %w", partNumber, err)
			}
		} else {
			// Regular part
			_, err := io.ReadFull(in, buffer)
			if err != nil {
				return fmt.Errorf("failed to read part %d: %w", partNumber, err)
			}
			partData = buffer
		}

		// Get upload URL for this part
		uploadURL, err := f.getUploadURL(ctx, preuploadID, partNumber)
		if err != nil {
			return fmt.Errorf("failed to get upload URL for part %d: %w", partNumber, err)
		}

		// Upload this part with retry
		err = f.uploadPacer.Call(func() (bool, error) {
			err := f.uploadPart(ctx, uploadURL, partData)
			return shouldRetry(ctx, nil, err)
		})
		if err != nil {
			return fmt.Errorf("failed to upload part %d: %w", partNumber, err)
		}

		fs.Debugf(f, "Uploaded part %d/%d", partNumber, uploadNums)
	}

	// Complete the upload
	return f.completeUpload(ctx, preuploadID)
}

// getUploadURL gets the upload URL for a specific part
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
	err = f.apiCall(ctx, "POST", "/upload/v1/file/get_upload_url", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return "", err
	}

	if response.Code != 0 {
		return "", fmt.Errorf("API error %d: %s", response.Code, response.Message)
	}

	return response.Data.PresignedURL, nil
}

// uploadPart uploads a single part to the given URL
func (f *Fs) uploadPart(ctx context.Context, uploadURL string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create upload request: %w", err)
	}

	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/octet-stream")

	// Add timeout to request
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

// completeUpload completes the multipart upload
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

	err = f.apiCall(ctx, "POST", "/upload/v1/file/upload_complete", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return fmt.Errorf("failed to complete upload: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("upload completion failed: API error %d: %s", response.Code, response.Message)
	}

	// If async processing, we might need to wait
	if response.Data.Async && !response.Data.Completed {
		fs.Debugf(f, "Upload completed asynchronously, file ID: %d", response.Data.FileID)
		// TODO: In a full implementation, we might want to poll for completion status
		// For now, we assume the upload will complete successfully
	} else {
		fs.Debugf(f, "Upload completed synchronously, file ID: %d", response.Data.FileID)
	}

	return nil
}

// deleteFile deletes a file by moving it to trash
func (f *Fs) deleteFile(ctx context.Context, fileID int64) error {
	fs.Debugf(f, "Deleting file with ID: %d", fileID)

	reqBody := map[string]interface{}{
		"fileIDs": []int64{fileID},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal delete request: %w", err)
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err = f.apiCall(ctx, "POST", "/api/v1/file/trash", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("delete failed: API error %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "Successfully deleted file with ID: %d", fileID)
	return nil
}

// moveFile moves a file to a different directory
func (f *Fs) moveFile(ctx context.Context, fileID, toParentFileID int64) error {
	fs.Debugf(f, "Moving file %d to parent %d", fileID, toParentFileID)

	reqBody := map[string]interface{}{
		"fileIDs":        []int64{fileID},
		"toParentFileID": toParentFileID,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal move request: %w", err)
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err = f.apiCall(ctx, "POST", "/api/v1/file/move", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return fmt.Errorf("failed to move file: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("move failed: API error %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "Successfully moved file %d to parent %d", fileID, toParentFileID)
	return nil
}

// renameFile renames a file
func (f *Fs) renameFile(ctx context.Context, fileID int64, fileName string) error {
	fs.Debugf(f, "Renaming file %d to: %s", fileID, fileName)

	reqBody := map[string]interface{}{
		"fileId":   fileID,
		"fileName": fileName,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal rename request: %w", err)
	}

	var response struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	err = f.apiCall(ctx, "PUT", "/api/v1/file/name", bytes.NewReader(bodyBytes), &response)
	if err != nil {
		return fmt.Errorf("failed to rename file: %w", err)
	}

	if response.Code != 0 {
		return fmt.Errorf("rename failed: API error %d: %s", response.Code, response.Message)
	}

	fs.Debugf(f, "Successfully renamed file %d to: %s", fileID, fileName)
	return nil
}

// getUserInfo gets user information including storage quota
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

// newFs constructs an Fs from the path, container:path
func newFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse options
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

	// Store the original name before any modifications for config operations
	originalName := name
	if idx := strings.IndexRune(name, '{'); idx > 0 {
		originalName = name[:idx]
	}

	f := &Fs{
		name:         name,
		originalName: originalName,
		root:         root,
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
	f.dirCache = dircache.New(root, f.rootFolderID, f)

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
	} else {
		// No token, so login
		fs.Debugf(f, "No token found, attempting login")
		err = f.GetAccessToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("initial login failed: %w", err)
		}
		// Save token to config after successful login
		f.saveToken(ctx, m)
		fs.Debugf(f, "Login successful, token saved")
	}

	// Try to refresh the token if needed
	if tokenRefreshNeeded {
		err = f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			fs.Debugf(f, "Token refresh failed, attempting full login: %v", err)
			// If refresh fails, try full login
			err = f.GetAccessToken(ctx)
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
	fs.Debugf(f, "NewObject called for: %s", remote)

	// Get the full path
	fullPath := strings.Trim(f.root+"/"+remote, "/")
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
	fs.Debugf(f, "Put called for: %s", src.Remote())

	// Use dircache to find parent directory
	leaf, parentID, err := f.dirCache.FindPath(ctx, src.Remote(), true)
	if err != nil {
		return nil, err
	}
	fileName := leaf

	// Convert parentID to int64
	parentFileID, err := strconv.ParseInt(parentID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid parent ID: %s", parentID)
	}

	// Calculate MD5 hash if available
	var md5Hash string
	if hashValue, err := src.Hash(ctx, hash.MD5); err == nil && hashValue != "" {
		md5Hash = hashValue
	}

	// Create upload session
	createResp, err := f.createUpload(ctx, parentFileID, fileName, md5Hash, src.Size())
	if err != nil {
		return nil, err
	}

	// If file already exists (reuse), return the object
	if createResp.Data.Reuse {
		return &Object{
			fs:          f,
			remote:      src.Remote(),
			hasMetaData: true,
			id:          strconv.FormatInt(createResp.Data.FileID, 10),
			size:        src.Size(),
			md5sum:      md5Hash,
			modTime:     time.Now(),
			isDir:       false,
		}, nil
	}

	// Upload the file
	err = f.uploadFile(ctx, in, createResp, src.Size())
	if err != nil {
		return nil, err
	}

	// Return the uploaded object
	return &Object{
		fs:          f,
		remote:      src.Remote(),
		hasMetaData: true,
		id:          strconv.FormatInt(createResp.Data.FileID, 10),
		size:        src.Size(),
		md5sum:      md5Hash,
		modTime:     time.Now(),
		isDir:       false,
	}, nil
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
	fs.Debugf(o.fs, "Opening file: %s", o.remote)

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
			fs.Debugf(o.fs, "Requesting range: %s", rangeHeader)
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
		return nil, err
	}

	return resp.Body, nil
}

// Update the object with new content.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	fs.Debugf(o.fs, "Update called for: %s", o.remote)

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
	fs.Debugf(o.fs, "Remove called for: %s", o.remote)

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
	fs.Debugf(f, "FindLeaf: pathID=%s, leaf=%s", pathID, leaf)

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
	fs.Debugf(f, "CreateDir: pathID=%s, leaf=%s", pathID, leaf)

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
		fs.Debugf(f, "No token found in config (from options)")
		return false
	}

	var tokenData TokenJSON                                                // Use the new TokenJSON struct
	var err error                                                          // Declare err here
	if err = json.Unmarshal([]byte(f.opt.Token), &tokenData); err != nil { // Unmarshal unobscured token
		fs.Debugf(f, "Failed to unmarshal token from config (from options): %v", err)
		return false
	}

	f.token = tokenData.AccessToken
	f.tokenExpiry, err = time.Parse(time.RFC3339, tokenData.Expiry) // Use tokenData.Expiry
	if err != nil {
		fs.Debugf(f, "Failed to parse token expiry from config: %v", err)
		return false
	}

	if f.token == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "Token from config is incomplete")
		return false
	}

	fs.Debugf(f, "Loaded token from config file, expires at %v", f.tokenExpiry)
	return true
}

// saveToken saves the current token to the config
func (f *Fs) saveToken(ctx context.Context, m configmap.Mapper) {
	if m == nil {
		fs.Debugf(f, "Not saving tokens - nil mapper provided")
		return
	}

	if f.token == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "Not saving tokens - incomplete token information")
		return
	}

	tokenData := TokenJSON{ // Use the new TokenJSON struct
		AccessToken: f.token,
		Expiry:      f.tokenExpiry.Format(time.RFC3339), // Use Expiry
	}
	tokenJSON, err := json.Marshal(tokenData)
	if err != nil {
		fs.Errorf(f, "Failed to marshal token for saving: %v", err)
		return
	}

	m.Set("token", string(tokenJSON))
	// m.Set does not return an error, so we assume it succeeds.
	// Any errors related to config saving would be handled by the underlying config system.

	fs.Debugf(f, "Saved token to config file using original name %q", f.originalName)
}

// setupTokenRenewer initializes the token renewer to automatically refresh tokens
func (f *Fs) setupTokenRenewer(ctx context.Context, m configmap.Mapper) {
	// Only set up renewer if we have valid tokens
	if f.token == "" || f.tokenExpiry.IsZero() {
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
		fs.Logf(f, "Failed to save token for renewer: %v", err)
		return
	}

	// Create a client with the token source
	_, ts, err := oauthutil.NewClientWithBaseClient(ctx, f.originalName, m, config, getHTTPClient(ctx, f.opt.ClientID, f.opt.ClientSecret))
	if err != nil {
		fs.Logf(f, "Failed to create token source for renewer: %v", err)
		return
	}

	// Create token renewer that will trigger when the token is about to expire
	f.tokenRenewer = oauthutil.NewRenew(f.originalName, ts, transaction)
	f.tokenRenewer.Start() // Start the renewer immediately
	fs.Debugf(f, "Token renewer initialized and started with original name %q", f.originalName)
}

// refreshTokenIfNecessary refreshes the token if necessary
func (f *Fs) refreshTokenIfNecessary(ctx context.Context, refreshTokenExpired bool, forceRefresh bool) error {
	f.tokenMu.Lock()

	// Check if another thread has successfully refreshed while we were waiting
	if !refreshTokenExpired && !forceRefresh && f.token != "" && time.Now().Before(f.tokenExpiry.Add(-tokenRefreshWindow)) {
		fs.Debugf(f, "Token is still valid after acquiring lock, skipping refresh")
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
	fs.Debugf(f, "About called")

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
	fs.Debugf(f, "Purge called for directory: %s", dir)

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
			fs.Errorf(f, "Failed to delete file %s: %v", file.Filename, err)
			// Continue with other files
		}
	}

	return nil
}

// Shutdown the backend
func (f *Fs) Shutdown(ctx context.Context) error {
	fs.Debugf(f, "Shutdown called")

	// Stop token renewer if it exists
	if f.tokenRenewer != nil {
		f.tokenRenewer.Stop()
		f.tokenRenewer = nil
	}

	return nil
}

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fs.Debugf(f, "List called for directory: %s", dir)

	// Use dircache to find the directory ID
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}

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
	fs.Debugf(f, "Mkdir called for directory: %s", dir)

	// Use dircache to create the directory
	_, err := f.dirCache.FindDir(ctx, dir, true)
	return err
}

// Rmdir removes an empty directory.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	fs.Debugf(f, "Rmdir called for directory: %s", dir)

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
	fs.Debugf(f, "Move called for %s to %s", src.Remote(), remote)

	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}

	// Use dircache to find destination directory
	dstName, dstDirID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}

	// Convert IDs to int64
	fileID, err := strconv.ParseInt(srcObj.id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid file ID: %s", srcObj.id)
	}

	dstParentID, err := strconv.ParseInt(dstDirID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid destination directory ID: %s", dstDirID)
	}

	// Move the file
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
	fs.Debugf(f, "DirMove called for %s to %s", srcRemote, dstRemote)

	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(f, "Can't move directory - not same remote type")
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
	fs.Debugf(f, "Copy called for %s to %s", src.Remote(), remote)

	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(f, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}

	// Use dircache to find destination directory
	dstName, dstParentID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
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
		// If server-side copy fails, fall back to download+upload
		fs.Debugf(f, "Server-side copy failed, falling back to download+upload: %v", err)
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
	fs.Debugf(f, "Copying %s to %s via download+upload", srcObj.remote, remote)

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

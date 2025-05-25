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

	"github.com/rclone/rclone/lib/oauthutil"
	"golang.org/x/oauth2"

	// The SDK for 123 Pan
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
)

const (
	openAPIRootURL     = "https://open-api.123pan.com"
	defaultUserAgent   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	tokenRefreshWindow = 10 * time.Minute // tokenRefreshWindow defines how long before expiry we try to refresh the token
)

type Options struct {
	ClientID     string `config:"client_id"`
	ClientSecret string `config:"client_secret"`
	Token        string `config:"token"`
}

// Fs represents a remote 123Pan drive
type Fs struct {
	name         string       // name of this remote
	originalName string       // Original config name without modifications
	root         string       // the path we are working on
	opt          Options      // parsed options
	features     *fs.Features // optional features
	token        string       // cached access token
	tokenExpiry  time.Time    // expiry of the cached access token
	mu           sync.Mutex   // mutex to protect token and tokenExpiry
	m            configmap.Mapper
	tokenRenewer *oauthutil.Renew
}

// Object describes a 123Pan object
type Object struct {
	fs          *Fs
	remote      string
	hasMetaData bool
	id          string
	size        int64
	md5sum      string
	modTime     time.Time
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
	// Construct request URL
	u, err := url.Parse(openAPIRootURL + "/api/v2/file/list")
	if err != nil {
		return nil, fmt.Errorf("parse url failed: %v", err)
	}

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

	u.RawQuery = params.Encode()

	// Create request
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request failed: %v", err)
	}

	req.Header.Set("Authorization", f.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Platform", "open_platform")
	// fs.Infof(nil, "Authorization: %s, User-Agent: %s", f.token, defaultUserAgent)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request failed: %v", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Parse response
	var result ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response failed: %v", err)
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
		firstFind := true
		for {
			parentFileID, err := strconv.Atoi(currentID)
			if err != nil {
				return "", fmt.Errorf("invalid parentFileId: %s", currentID)
			}

			lastFileIDInt, err := strconv.Atoi(next)
			if err != nil {
				return "", fmt.Errorf("invalid next token: %s", next)
			}

			response, err := f.ListFile(ctx, parentFileID, 100, "", "", lastFileIDInt)
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
				if !firstFind {
					time.Sleep(1 * time.Second)
				}
				firstFind = false
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

func (f *Fs) getDownloadURLByUA(ctx context.Context, filePath string, UA string) (string, error) {
	// Ensure token is valid before making API calls
	err := f.refreshTokenIfNecessary(ctx, false, false)
	if err != nil {
		return "", err
	}
	if UA == "" {
		UA = defaultUserAgent
	}
	// fs.Debugf(f, "Token: %s", f.token)
	// fs.Debugf(f, "Token: %s", f.root)
	if filePath == "" {
		filePath = f.root
	}
	fileID, err := f.pathToFileID(ctx, filePath)
	if err != nil {
		return "", fmt.Errorf("failed to get file ID for path %q: %w", filePath, err)
	}

	req, err := http.NewRequest("GET", "https://open-api.123pan.com/api/v1/file/download_info?fileID="+fileID, nil)
	client := &http.Client{Timeout: 30 * time.Second}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Platform", "open_platform")
	req.Header.Set("Authorization", f.token)
	req.Header.Set("User-Agent", UA)
	// fmt.Printf("Request URL: %s\n", req.URL.String())
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("请求发送失败: %v\n", err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("读取响应体失败: %v\n", err)
	}
	// fmt.Println("原始响应数据:")
	// fmt.Println(string(bodyBytes))
	var result map[string]interface{}
	if err = json.Unmarshal(bodyBytes, &result); err != nil {
		fmt.Printf("解析JSON失败: %v\n", err)
	}
	fs.Debug(f, string(result["data"].(map[string]interface{})["downloadUrl"].(string)))
	// return string(result["data"].(map[string]interface{})["downloadUrl"].(string)), err
	// fs.Debugf(f, "File ID for path %q: %s", filePath, fileID)
	// client := &http.Client{Timeout: 30 * time.Second}
	// req, err := http.NewRequest("GET", "https://open-api.123pan.com/api/v1/file/detail?fileID="+fileID, nil)
	// if err != nil {
	// 	fmt.Printf("创建请求失败: %v\n", err)
	// 	return "", err
	// }
	// req.Header.Set("Content-Type", "application/json")
	// req.Header.Set("Platform", "open_platform")
	// req.Header.Set("Authorization", f.token)
	// req.Header.Set("User-Agent", UA)

	// resp, err := client.Do(req)
	// if err != nil {
	// 	fmt.Printf("请求发送失败: %v\n", err)
	// 	return "", err
	// }
	// defer resp.Body.Close()
	// if resp.StatusCode != http.StatusOK {
	// 	body, _ := io.ReadAll(resp.Body)
	// 	fmt.Printf("请求失败: 状态码=%d, 响应=%s\n", resp.StatusCode, string(body))
	// 	return "", err
	// }
	// var fileDetailResponse FileDetailResponse
	// if err = json.NewDecoder(resp.Body).Decode(&fileDetailResponse); err != nil {
	// 	fmt.Printf("JSON解析失败: %v\n", err)
	// 	return "", err
	// }
	// if fileDetailResponse.Code != 0 {
	// 	fmt.Printf("API返回错误: code=%d, message=%s\n", fileDetailResponse.Code, fileDetailResponse.Message)
	// 	return "", err
	// }
	// fmt.Println("文件详情信息:")
	// fmt.Printf("文件ID: %d\n", fileDetailResponse.Data.FileID)
	// fmt.Printf("文件名: %s\n", fileDetailResponse.Data.Filename)
	// fmt.Printf("类型: %s\n", map[int]string{0: "文件", 1: "文件夹"}[fileDetailResponse.Data.Type])
	// fmt.Printf("大小: %.2f GB\n", float64(fileDetailResponse.Data.Size)/(1024*1024*1024))
	// fmt.Printf("MD5: %s\n", fileDetailResponse.Data.ETag)
	// fmt.Printf("创建时间: %s\n", fileDetailResponse.Data.CreateAt)
	// fmt.Printf("TraceID: %s\n", fileDetailResponse.TraceID)

	// reqPayload := FileInfoRequest{
	// 	FileIDs: []int64{
	// 		15616626,
	// 	},
	// }
	// jsonPayload, err := json.Marshal(reqPayload)
	// if err != nil {
	// 	fmt.Printf("Error marshalling JSON payload: %v\n", err)
	// }
	// client = &http.Client{
	// 	Timeout: 30 * time.Second,
	// }

	// req, err = http.NewRequest("POST", "https://open-api.123pan.com/api/v1/file/infos", bytes.NewBuffer(jsonPayload))
	// if err != nil {
	// 	return "", fmt.Errorf("创建请求失败: %w", err)
	// }
	// req.Header.Set("Content-Type", "application/json")
	// req.Header.Set("Platform", "open_platform")
	// req.Header.Set("Authorization", f.token)
	// req.Header.Set("User-Agent", UA)
	// resp, err = client.Do(req)
	// if err != nil {
	// 	return "", fmt.Errorf("请求发送失败: %w", err)
	// }
	// defer resp.Body.Close()
	// bodyBytes, err = io.ReadAll(resp.Body)
	// if err != nil {
	// 	fmt.Printf("Error reading response body: %v\n", err)
	// }
	// if resp.StatusCode != http.StatusOK {
	// 	fmt.Printf("API request failed with status code %d: %s\n", resp.StatusCode, string(bodyBytes))
	// 	return "", errors.New("API request failed") // 返回空字符串和错误
	// }
	// fmt.Printf("API request failed with status code %d: %s\n", resp.StatusCode, string(bodyBytes))
	// var response FileInfosResponse
	// if err = json.Unmarshal(bodyBytes, &response); err != nil {
	// 	fmt.Printf("Error unmarshalling JSON response: %v\n", err)
	// }
	// fmt.Printf("解析后的响应数据：\n %+v\n", response.Data.FileList[0])
	// payload := Payload{
	// 	S3KeyFlag: response.Data.FileList[0].S3KeyFlag,
	// 	FileName:  response.Data.FileList[0].Filename,
	// 	Etag:      response.Data.FileList[0].ETag,
	// 	Size:      response.Data.FileList[0].Size,
	// 	FileID:    response.Data.FileList[0].FileID,
	// }
	// jsonPayload, err = json.Marshal(payload)
	// if err != nil {
	// 	fmt.Printf("JSON 序列化失败: %v\n", err)
	// }

	// req, err = http.NewRequest("POST", "https://www.123pan.com/api/file/download_info", bytes.NewBuffer(jsonPayload))
	// client = &http.Client{Timeout: 30 * time.Second}
	// req.Header.Set("Content-Type", "application/json")
	// req.Header.Set("Platform", "android")
	// req.Header.Set("Authorization", f.token)
	// req.Header.Set("User-Agent", UA)
	// fmt.Printf("Request URL: %s\n", req.URL.String())
	// resp, err = client.Do(req)
	// if err != nil {
	// 	fmt.Printf("请求发送失败: %v\n", err)
	// }
	// defer resp.Body.Close()
	// fmt.Printf("响应状态码: %d\n", resp.StatusCode)
	// bodyBytes, err = io.ReadAll(resp.Body)
	// if err != nil {
	// 	fmt.Printf("读取响应体失败: %v\n", err)
	// }
	// fmt.Println("原始响应数据:")
	// fmt.Println(string(bodyBytes))
	// var results map[string]interface{}
	// if err = json.Unmarshal(bodyBytes, &results); err != nil {
	// 	fmt.Printf("解析JSON失败: %v\n", err)
	// }
	// fmt.Println(results)
	// return string(result["data"].(map[string]interface{})["downloadUrl"].(string)), err

	return string(result["data"].(map[string]interface{})["downloadUrl"].(string)), nil

}

// newFs constructs an Fs from the path, container:path
func newFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse options
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
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
	}

	f.features = (&fs.Features{
		ReadMimeType: true,
	}).Fill(ctx, f)

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

	return f, nil
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	o := &Object{
		fs:     f,
		remote: remote,
	}
	// Placeholder: In a real implementation, you would fetch metadata for the object here.
	// For now, we just return the object.
	return o, nil
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// PutStream uploads the object.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	// Placeholder: Actual upload logic would go here.
	// For now, we return a dummy object.
	o := &Object{
		fs:     f,
		remote: src.Remote(),
		size:   src.Size(),
	}
	return o, nil
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
	// Placeholder: Actual download logic would go here.
	return io.NopCloser(bytes.NewReader(nil)), nil
}

// Update the object with new content.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	// Placeholder: Actual update logic would go here.
	return nil
}

// Remove the object.
func (o *Object) Remove(ctx context.Context) error {
	// Placeholder: Actual remove logic would go here.
	return nil
}

// SetModTime is not supported
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return fs.ErrorCantSetModTime
}

// Check the interfaces are satisfied
var (
	_ fs.Fs     = (*Fs)(nil)
	_ fs.Object = (*Object)(nil)
	// _ fs.PutStreamer  = (*Fs)(nil) // Removed
	// _ fs.Remover  = (*Object)(nil) // Removed
	_ fs.Mover    = (*Fs)(nil)
	_ fs.DirMover = (*Fs)(nil)
	_ fs.Copier   = (*Fs)(nil)
	// _ fs.Mkdirer      = (*Fs)(nil) // Removed
	// _ fs.Rmdirer = (*Fs)(nil) // Removed
	// _ fs.ListRer      = (*Fs)(nil) // Removed
	_ fs.Abouter     = (*Fs)(nil)
	_ fs.Purger      = (*Fs)(nil)
	_ fs.Shutdowner  = (*Fs)(nil)
	_ fs.ObjectInfo  = (*Object)(nil)
	_ fs.IDer        = (*Object)(nil)
	_ fs.SetModTimer = (*Object)(nil)
	// _ fs.OpenCloser   = (*Object)(nil) // Removed
	// _ fs.UpdateCloser = (*Object)(nil) // Removed
	// _ fs.RemoveCloser = (*Object)(nil) // Removed, already handled by Remover
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
	f.mu.Lock()

	// Check if another thread has successfully refreshed while we were waiting
	if !refreshTokenExpired && !forceRefresh && f.token != "" && time.Now().Before(f.tokenExpiry.Add(-tokenRefreshWindow)) {
		fs.Debugf(f, "Token is still valid after acquiring lock, skipping refresh")
		f.mu.Unlock()
		return nil
	}
	defer f.mu.Unlock()

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

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	// This is a placeholder implementation.
	// Actual listing logic would go here, likely involving API calls to 123Pan.
	fs.Debugf(f, "List called for directory: %s", dir)
	return nil, nil
}

// Mkdir makes the directory.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	// This is a placeholder implementation.
	// Actual directory creation logic would go here, likely involving API calls to 123Pan.
	fs.Debugf(f, "Mkdir called for directory: %s", dir)
	return nil
}

// Rmdir removes an empty directory.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	// Placeholder: Actual rmdir logic would go here.
	fs.Debugf(f, "Rmdir called for directory: %s", dir)
	return nil
}

// Move server-side moves a file.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	// Placeholder: Actual move logic would go here.
	fs.Debugf(f, "Move called for %s to %s", src.Remote(), remote)
	return nil, fs.ErrorCantMove
}

// DirMove server-side moves a directory.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	// Placeholder: Actual directory move logic would go here.
	fs.Debugf(f, "DirMove called for %s to %s", srcRemote, dstRemote)
	return fs.ErrorCantDirMove
}

// Copy server-side copies a file.
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	// Placeholder: Actual copy logic would go here.
	fs.Debugf(f, "Copy called for %s to %s", src.Remote(), remote)
	return nil, fs.ErrorCantCopy
}

// About gets quota information.
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	// Placeholder: Actual quota information retrieval would go here.
	fs.Debugf(f, "About called")
	return nil, nil
}

// Purge removes a directory and all its contents.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	// Placeholder: Actual purge logic would go here.
	fs.Debugf(f, "Purge called for directory: %s", dir)
	return nil
}

// Shutdown shuts down the fs, closing any background tasks
func (f *Fs) Shutdown(ctx context.Context) error {
	if f.tokenRenewer != nil {
		f.tokenRenewer.Shutdown()
		f.tokenRenewer = nil
	}
	return nil
}

// Check the interfaces are satisfied
var (
	_ fs.Fs         = (*Fs)(nil)
	_ fs.Shutdowner = (*Fs)(nil)
)

// getHTTPClient makes an http client according to the options
func getHTTPClient(ctx context.Context, clientID, clientSecret string) *http.Client {
	// This function is a placeholder. In a real scenario, you might configure
	// the HTTP client with timeouts, proxies, etc., based on the Fs options.
	// For now, it returns a basic client.
	return fshttp.NewClient(ctx)
}

// ./rclone ls 123test: --log-level DEBUG
// ./rclone backend getdownloadurlua "123test:/test/独裁者 (2012) {tmdb-76493}.mkv" "test" --log-level DEBUG
// ./rclone backend getdownloadurlua "116:/电影/刮削/4K REMUX/200X/007：大破量子危机（2008）/007：大破量子危机 (2008) 2160p DTSHD-MA.mkv" "test" --log-level DEBUG

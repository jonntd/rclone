// Package _115 provides an interface to 115 cloud storage
package _115

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/cache"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/pool"
	"github.com/rclone/rclone/lib/rest"
	"golang.org/x/oauth2"
)

// API Types and Structures

// Time represents date and time information
type Time time.Time

// MarshalJSON turns a Time into JSON (in UTC)
func (t *Time) MarshalJSON() (out []byte, err error) {
	s := strconv.Itoa(int((*time.Time)(t).Unix()))
	return []byte(s), nil
}

// UnmarshalJSON turns JSON into a Time
func (t *Time) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "null" || s == "" {
		return nil
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// Try parsing RFC3339 format for Expiration in OSSTokenData
		parsedTime, timeErr := time.Parse(time.RFC3339, s)
		if timeErr == nil {
			*t = Time(parsedTime)
			return nil
		}
		// Return original error if RFC3339 parsing also fails
		return err
	}
	newT := time.Unix(i, 0)
	*t = Time(newT)
	return nil
}

type Int int

func (e *Int) UnmarshalJSON(in []byte) (err error) {
	s := strings.Trim(string(in), `"`)
	if s == "" {
		s = "0"
	}
	if i, err := strconv.Atoi(s); err == nil {
		*e = Int(i)
	}
	return
}

type Int64 int64

func (e *Int64) UnmarshalJSON(in []byte) (err error) {
	s := strings.Trim(string(in), `"`)
	if s == "" {
		s = "0"
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		*e = Int64(i)
	}
	return
}

type BoolOrInt bool

func (b *BoolOrInt) UnmarshalJSON(in []byte) error {
	// if in is "true" or "false", unmarshal it as a bool
	// if in is 0 or 1, unmarshal it as an int
	// otherwise, return an error
	var boolVal bool
	err := json.Unmarshal(in, &boolVal)
	if err == nil {
		*b = BoolOrInt(boolVal)
		return nil
	}

	var intVal int
	err = json.Unmarshal(in, &intVal)
	if err == nil {
		*b = BoolOrInt(intVal == 1)
		return nil
	}

	return fmt.Errorf("cannot unmarshal %s into BoolOrInt", string(in))
}

// String ensures JSON unmarshals to a string, handling both quoted and unquoted inputs.
// Unquoted inputs are treated as raw bytes and converted directly to a string.
type String string

func (s *String) UnmarshalJSON(in []byte) error {
	if n := len(in); n > 1 && in[0] == '"' && in[n-1] == '"' {
		return json.Unmarshal(in, (*string)(s))
	}
	*s = String(in)
	return nil
}

// StringOrNumber handles API fields that can be either a string or a number
type StringOrNumber string

func (s *StringOrNumber) UnmarshalJSON(data []byte) error {
	// Try string first
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*s = StringOrNumber(str)
		return nil
	}

	// Try number (int)
	var intVal int
	if err := json.Unmarshal(data, &intVal); err == nil {
		*s = StringOrNumber(strconv.Itoa(intVal))
		return nil
	}

	// Try number (float)
	var floatVal float64
	if err := json.Unmarshal(data, &floatVal); err == nil {
		*s = StringOrNumber(strconv.FormatFloat(floatVal, 'f', -1, 64))
		return nil
	}

	// If it's null or empty
	if string(data) == "null" || string(data) == "" {
		*s = ""
		return nil
	}

	// Last resort: use the raw value
	*s = StringOrNumber(strings.Trim(string(data), "\""))
	return nil
}

// 115网盘API常量
const (
	// API分页限制
	defaultListChunkSize = 1150
)

type TraditionalBase struct {
	Msg   string    `json:"msg,omitempty"`
	Errno Int       `json:"errno,omitempty"` // Base, NewDir, DirID, UploadBasicInfo, ShareSnap
	ErrNo Int       `json:"errNo,omitempty"` // FileList
	Error string    `json:"error,omitempty"` // Base, FileList, NewDir, DirID, UploadBasicInfo, ShareSnap
	State BoolOrInt `json:"state,omitempty"`
}

func (b *TraditionalBase) ErrCode() Int {
	if b.Errno != 0 {
		return b.Errno
	}
	return b.ErrNo
}

func (b *TraditionalBase) ErrMsg() string {
	if b.Error != "" {
		return b.Error
	}
	return b.Msg
}

// Err returns Error or Nil for TraditionalBase
func (b *TraditionalBase) Err() error {
	if b.State {
		return nil
	}
	out := fmt.Sprintf("Traditional API Error(%d)", b.ErrCode())
	if msg := b.ErrMsg(); msg != "" {
		out += fmt.Sprintf(": %q", msg)
	}
	return errors.New(out)
}

// OpenAPIBase is for the new Open API

type OpenAPIBase struct {
	State   BoolOrInt      `json:"state"` // Note: OpenAPI uses boolean state
	Code    Int            `json:"code,omitempty"`
	Message StringOrNumber `json:"message,omitempty"` // Can be either string or number
	Error   string         `json:"error,omitempty"`   // Some endpoints might still use this
	Errno   Int            `json:"errno,omitempty"`   // Some endpoints might still use this
}

func (b *OpenAPIBase) ErrCode() Int {
	if b.Code != 0 {
		return b.Code
	}
	return b.Errno
}

func (b *OpenAPIBase) ErrMsg() string {
	if string(b.Message) != "" {
		return string(b.Message)
	}
	return b.Error
}

// Err returns Error or Nil for OpenAPIBase
func (b *OpenAPIBase) Err() error {
	if b.State {
		return nil
	}
	out := fmt.Sprintf("OpenAPI Error(%d)", b.ErrCode())
	if msg := b.ErrMsg(); msg != "" {
		out += fmt.Sprintf(": %q", msg)
	}

	// Specific error codes to check based on API documentation
	code := b.ErrCode()
	// Check for rate limit error codes in multiple formats
	if code == 0 && (string(b.Message) == "770004" ||
		strings.Contains(b.ErrMsg(), "770004") ||
		strings.Contains(b.ErrMsg(), "已达到当前访问上限")) {
		// This is a rate limit error - return as retryable error
		return fserrors.NewErrorRetryAfter(5 * time.Second)
	}

	// Also check for direct error code 770004
	if code == 770004 {
		return fserrors.NewErrorRetryAfter(5 * time.Second)
	}

	switch code {
	case 40140109: // access permission disabled - 访问权限已停用，无法访问此功能
		return NewTokenError(out, true)
	// Codes that require re-login
	case 40140116: // refresh_token invalid (authorization revoked)
		return NewTokenError(out, true)
	case 40140117: // access_token refreshed too frequently
		return NewTokenError(out, true)
	case 40140119: // refresh_token expired
		return NewTokenError(out, true)
	case 40140120: // refresh_token verification failed (anti-tampering)
		// Check if local refresh token is updated; if not, re-login
		return NewTokenError(out, true)
	case 40140121: // access_token refresh failed
		// This should allow a retry of the refresh token operation
		return NewTokenError(out, false)
	case 40140125: // access_token invalid (expired or authorization revoked)
		// Try refreshing token first
		return NewTokenError(out, false)
	}

	return errors.New(out)
}

// TokenError indicates an issue with the access or refresh token
type TokenError struct {
	msg                   string
	IsRefreshTokenExpired bool
}

// NewTokenError creates a new TokenError with the given message and refresh token status
func NewTokenError(msg string, isRefreshTokenExpired ...bool) *TokenError {
	expired := false
	if len(isRefreshTokenExpired) > 0 {
		expired = isRefreshTokenExpired[0]
	}
	return &TokenError{
		msg:                   msg,
		IsRefreshTokenExpired: expired,
	}
}

func (e *TokenError) Error() string {
	return e.msg
}

// ------------------------------------------------------------
// Authentication related structs
// ------------------------------------------------------------

type AuthDeviceCodeData struct {
	UID    string `json:"uid"`
	Time   int64  `json:"time"`
	Qrcode string `json:"qrcode"`
	Sign   string `json:"sign"`
}

type AuthDeviceCodeResp struct {
	OpenAPIBase
	Data *AuthDeviceCodeData `json:"data"`
}

type DeviceCodeTokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"` // seconds
}

type DeviceCodeTokenResp struct {
	OpenAPIBase
	Data *DeviceCodeTokenData `json:"data"`
}

type RefreshTokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"` // seconds
}

type RefreshTokenResp struct {
	OpenAPIBase
	Data *RefreshTokenData `json:"data"`
}

// ------------------------------------------------------------
// File and Directory related structs
// ------------------------------------------------------------

// File represents a file or folder object from either API
// Uses tags for both traditional and OpenAPI field names
type File struct {
	// Common fields (adapt names if needed)
	Name            string `json:"n,omitempty"`         // Traditional: n, OpenAPI: fn or file_name
	FileName        string `json:"fn,omitempty"`        // OpenAPI: fn (in list), file_name (in details)
	Size            Int64  `json:"size,omitempty"`      // Traditional: s, OpenAPI: file_size
	FileSize        Int64  `json:"fs,omitempty"`        // OpenAPI: file_size
	PickCode        string `json:"pc,omitempty"`        // Traditional: pc
	PickCodeOpenAPI string `json:"pick_code,omitempty"` // OpenAPI: pick_code
	Sha             string `json:"sha,omitempty"`       // Traditional: sha, OpenAPI: sha1
	Sha1            string `json:"sha1,omitempty"`      // OpenAPI: sha1

	// Identifiers
	FID string `json:"fid,omitempty"` // Traditional: fid (file), OpenAPI: fid (file), file_id (folder/file details)
	CID string `json:"cid,omitempty"` // Traditional: cid (folder), OpenAPI: cid (in list query param), file_id (folder details)
	PID string `json:"pid,omitempty"` // Traditional: pid (folder parent), OpenAPI: pid (folder parent)

	// Timestamps
	T   string `json:"t,omitempty"`    // Traditional: representative time? "2024-05-19 03:54" or "1715919337"
	Te  Time   `json:"te,omitempty"`   // Traditional: modify time
	Tp  Time   `json:"tp,omitempty"`   // Traditional: create time
	Tu  Time   `json:"tu,omitempty"`   // Traditional: update time?
	To  Time   `json:"to,omitempty"`   // Traditional: last opened 0 if never accessed or "1716165082"
	Upt Time   `json:"upt,omitempty"`  // OpenAPI: upt (update time)
	Uet Time   `json:"uet,omitempty"`  // OpenAPI: uet (update time alias?)
	Ppt Time   `json:"uppt,omitempty"` // OpenAPI: uppt (upload time)

	// Type/Category
	IsFolder Int    `json:"fc,omitempty"`    // OpenAPI: fc (0 folder, 1 file)
	Ico      string `json:"ico,omitempty"`   // Traditional icon
	Class    string `json:"class,omitempty"` // Traditional class

	// Status/Attributes
	IsMarked Int `json:"ism,omitempty"`  // OpenAPI: ism (starred, 1=yes)
	Star     Int `json:"star,omitempty"` // OpenAPI: star (in update response)
	IsHidden Int `json:"ih,omitempty"`   // OpenAPI: ih (hidden?)
	IsLocked Int `json:"lo,omitempty"`   // OpenAPI: lo (locked?)
	IsCrypt  Int `json:"isp,omitempty"`  // OpenAPI: isp (encrypted, 1=yes)
	Censored Int `json:"c,omitempty"`    // Traditional: censored flag

	// Other fields from OpenAPI list
	Aid   json.Number `json:"aid,omitempty"`   // OpenAPI: aid (area id?)
	Fco   string      `json:"fco,omitempty"`   // OpenAPI: fco (folder cover?)
	Cm    Int         `json:"cm,omitempty"`    // OpenAPI: cm (?)
	Fdesc string      `json:"fdesc,omitempty"` // OpenAPI: fdesc (description?)
	Ispl  Int         `json:"ispl,omitempty"`  // OpenAPI: ispl (play long related?)

	// Fields from traditional API (might be redundant or map differently)
	UID       json.Number `json:"uid,omitempty"` // Traditional user ID
	CheckCode int         `json:"check_code,omitempty"`
	CheckMsg  string      `json:"check_msg,omitempty"`
	Score     int         `json:"score,omitempty"`
	PlayLong  float64     `json:"play_long,omitempty"` // playback secs if media
}

// IsDir checks if the item represents a directory based on OpenAPI fields
func (f *File) IsDir() bool {
	// OpenAPI uses fc=0 for folder, file_category="0" in details
	// Traditional uses fid="" for folder
	// 🔧 标准rclone做法：完全信任服务器返回的类型标识，与Google Drive、OneDrive等一致
	return f.IsFolder == 0 || (f.FID == "" && f.CID != "")
}

// ID returns the best identifier (File ID or Category ID)
func (f *File) ID() string {
	if f.FID != "" { // Prefer FID if available (OpenAPI file, Traditional file)
		return f.FID
	}
	if f.CID != "" { // Use CID if FID is empty (OpenAPI folder, Traditional folder)
		return f.CID
	}
	// Fallback for folder details where file_id is used
	// This might need adjustment based on actual API responses for folder details via OpenAPI
	// if f.FileID != "" {
	// return f.FileID
	// }
	return "" // Should not happen for valid items
}

// ParentID returns the parent directory ID
func (f *File) ParentID() string {
	return f.PID // Both APIs seem to use 'pid'
}

// FileName returns the best name field
func (f *File) FileNameBest() string {
	if f.FileName != "" { // OpenAPI list 'fn', details 'file_name'
		return f.FileName
	}
	return f.Name // Traditional 'n'
}

// FileSizeBest returns the best size field
func (f *File) FileSizeBest() int64 {
	if f.FileSize > 0 { // OpenAPI 'file_size'
		return int64(f.FileSize)
	}
	return int64(f.Size) // Traditional 's'
}

// PickCodeBest returns the best pick code field
func (f *File) PickCodeBest() string {
	// Prefer OpenAPI pick_code field
	if f.PickCodeOpenAPI != "" {
		return f.PickCodeOpenAPI
	}
	return f.PickCode // 回退到传统API的pc字段
}

// Sha1Best returns the best SHA1 field
func (f *File) Sha1Best() string {
	if f.Sha1 != "" { // OpenAPI 'sha1'
		return f.Sha1
	}
	return f.Sha // Traditional 'sha'
}

// ModTime returns the best modification time
func (f *File) ModTime() time.Time {
	// Prefer OpenAPI update times
	if t := time.Time(f.Upt); !t.IsZero() {
		return t
	}
	if t := time.Time(f.Uet); !t.IsZero() {
		return t
	}
	// Fallback to traditional times
	if t := time.Time(f.Te); !t.IsZero() {
		return t
	}
	if t := time.Time(f.Tu); !t.IsZero() {
		return t
	}
	// Fallback for ShareSnap list items
	if ts, err := strconv.ParseInt(f.T, 10, 64); err == nil {
		return time.Unix(ts, 0)
	}
	return time.Time{}
}

// FilePath represents an item in the path hierarchy (used in traditional list)
type FilePath struct {
	Name string      `json:"name,omitempty"`
	AID  json.Number `json:"aid,omitempty"` // area
	CID  json.Number `json:"cid,omitempty"` // category
	PID  json.Number `json:"pid,omitempty"` // parent
	Isp  json.Number `json:"isp,omitempty"`
	PCid string      `json:"p_cid,omitempty"`
	Iss  string      `json:"iss,omitempty"`
	Fv   string      `json:"fv,omitempty"`
	Fvs  string      `json:"fvs,omitempty"`
}

// FileList represents the response from listing files (adaptable for both APIs)
type FileList struct {
	// Use OpenAPIBase for state/code/message
	OpenAPIBase

	// Data payload
	Files []*File `json:"data,omitempty"` // OpenAPI uses 'data', Traditional uses 'data'

	// Pagination and Counts (check OpenAPI names)
	Count       int         `json:"count,omitempty"`        // Traditional total count
	TotalCount  int         `json:"total_count,omitempty"`  // OpenAPI might use a different name
	FileCount   int         `json:"file_count,omitempty"`   // Traditional
	FolderCount int         `json:"folder_count,omitempty"` // Traditional
	PageSize    int         `json:"page_size,omitempty"`    // Traditional
	Limit       json.Number `json:"limit,omitempty"`        // OpenAPI uses 'limit' param, response might confirm
	Offset      json.Number `json:"offset,omitempty"`       // OpenAPI uses 'offset' param, response might confirm

	// Context/Query Info (check OpenAPI names)
	DataSource     string      `json:"data_source,omitempty"`      // Traditional
	SysCount       int         `json:"sys_count,omitempty"`        // Traditional
	AID            json.Number `json:"aid,omitempty"`              // Traditional
	CID            json.Number `json:"cid,omitempty"`              // Traditional context CID
	IsAsc          json.Number `json:"is_asc,omitempty"`           // Traditional sort order
	Star           int         `json:"star,omitempty"`             // Traditional star filter
	IsShare        int         `json:"is_share,omitempty"`         // Traditional
	Type           int         `json:"type,omitempty"`             // Traditional type filter
	IsQ            int         `json:"is_q,omitempty"`             // Traditional
	RAll           int         `json:"r_all,omitempty"`            // Traditional
	Stdir          int         `json:"stdir,omitempty"`            // Traditional
	Cur            int         `json:"cur,omitempty"`              // Traditional
	MinSize        int         `json:"min_size,omitempty"`         // Traditional
	MaxSize        int         `json:"max_size,omitempty"`         // Traditional
	RecordOpenTime string      `json:"record_open_time,omitempty"` // Traditional
	Path           []*FilePath `json:"path,omitempty"`             // Traditional path breadcrumbs
	Fields         string      `json:"fields,omitempty"`           // Traditional
	Order          string      `json:"order,omitempty"`            // Traditional sort field
	FcMix          int         `json:"fc_mix,omitempty"`           // Traditional
	Natsort        int         `json:"natsort,omitempty"`          // Traditional
	UID            json.Number `json:"uid,omitempty"`              // Traditional user ID
	Suffix         string      `json:"suffix,omitempty"`           // Traditional suffix filter
}

// FileInfo represents the response for getting single file info (Traditional)
// OpenAPI might use a different structure or just return a File object in 'data'
type FileInfo struct {
	TraditionalBase
	Data []*File `json:"data,omitempty"`
}
type NewDirData struct {
	FileName string `json:"file_name,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

// NewDir represents the response for creating a directory
type NewDir struct {
	OpenAPIBase
	Data *NewDirData `json:"data,omitempty"`
}

// DirID represents the response for getting a directory ID by path (Traditional)
type DirID struct {
	TraditionalBase
	ID        json.Number `json:"id,omitempty"`
	IsPrivate json.Number `json:"is_private,omitempty"`
}

// FileStats represents the response for getting folder stats (Traditional /category/get)
// OpenAPI has /open/folder/get_info
type FileStats struct {
	OpenAPIBase
	Data *FolderInfoData `json:"data"`
}

type FolderInfoData struct {
	Count        String `json:"count"`          // OpenAPI: string
	Size         String `json:"size"`           // OpenAPI: string
	FolderCount  String `json:"folder_count"`   // OpenAPI: string
	PlayLong     Int64  `json:"play_long"`      // OpenAPI: number (seconds), -1=calculating
	ShowPlayLong Int    `json:"show_play_long"` // OpenAPI: number (bool 0/1)
	Ptime        String `json:"ptime"`          // OpenAPI: string timestamp?
	Utime        String `json:"utime"`          // OpenAPI: string timestamp?
	FileName     string `json:"file_name"`      // OpenAPI: string
	PickCode     string `json:"pick_code"`      // OpenAPI: string
	Sha1         string `json:"sha1"`           // OpenAPI: string
	FileID       string `json:"file_id"`        // OpenAPI: string
	IsMark       String `json:"is_mark"`        // OpenAPI: string (bool 0/1)
	OpenTime     Int64  `json:"open_time"`      // OpenAPI: number timestamp?
	FileCategory String `json:"file_category"`  // OpenAPI: string (0=folder)
	Paths        []struct {
		FileID   String `json:"file_id"`   // OpenAPI: number (as string?)
		FileName string `json:"file_name"` // OpenAPI: string
	} `json:"paths"`
}

// StringInfo is a generic response where data is just a string (Traditional)
type StringInfo struct {
	TraditionalBase
	Data String `json:"data,omitempty"`
}

// IndexInfo represents user quota info (Traditional)
type IndexInfo struct {
	TraditionalBase
	Data *IndexData `json:"data,omitempty"`
}

type IndexData struct {
	SpaceInfo map[string]*SizeInfo `json:"space_info"`
}

type SizeInfo struct {
	Size       float64 `json:"size"`
	SizeFormat string  `json:"size_format"`
}

// ------------------------------------------------------------
// Download related structs
// ------------------------------------------------------------

// DownloadURL represents the URL structure from both APIs
type DownloadURL struct {
	URL       string         `json:"url"`              // Present in both
	Client    Int            `json:"client,omitempty"` // Traditional
	Desc      string         `json:"desc,omitempty"`   // Traditional
	OssID     string         `json:"oss_id,omitempty"` // Traditional
	Cookies   []*http.Cookie // Added manually after request
	CreatedAt time.Time      `json:"created_at"` // 创建时间，用于fallback过期检测
}

func (u *DownloadURL) UnmarshalJSON(data []byte) error {
	if string(data) == "false" {
		*u = DownloadURL{}
		return nil
	}

	type Alias DownloadURL // Use type alias to avoid recursion
	aux := Alias{}
	if err := json.Unmarshal(data, &aux); err != nil {
		// Try unmarshalling just as a string if object fails (OpenAPI might simplify)
		var urlStr string
		if strErr := json.Unmarshal(data, &urlStr); strErr == nil {
			*u = DownloadURL{
				URL:       urlStr,
				CreatedAt: time.Now(), // 设置创建时间用于fallback过期检测
			}
			return nil
		}
		return err // Return original error if string unmarshal also fails
	}
	*u = DownloadURL(aux)
	// 确保从JSON解析的DownloadURL也有创建时间
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	return nil
}

// expiry parses expiry from URL parameter t (Traditional URL format)
func (u *DownloadURL) expiry() time.Time {
	if p, err := url.Parse(u.URL); err == nil {
		if q, err := url.ParseQuery(p.RawQuery); err == nil {
			if t := q.Get("t"); t != "" {
				if i, err := strconv.ParseInt(t, 10, 64); err == nil {
					return time.Unix(i, 0)
				}
			}
			// Check for OSS expiry parameter (might be different)
			if exp := q.Get("Expires"); exp != "" {
				if i, err := strconv.ParseInt(exp, 10, 64); err == nil {
					return time.Unix(i, 0)
				}
			}
		}
	}
	return time.Time{}
}

// expired reports whether the token is expired.
func (u *DownloadURL) expired() bool {
	// 策略1：首先尝试现有的URL解析逻辑（保持不变）
	expiry := u.expiry()
	if !expiry.IsZero() {
		// 修复：115网盘URL有效期通常4-5分钟，使用更合理的缓冲时间
		now := time.Now()
		timeUntilExpiry := expiry.Sub(now)

		// 🔍 调试信息：记录过期检测详情
		fs.Debugf(nil, "🔍 115网盘URL过期检查(URL解析): 当前时间=%v, 过期时间=%v, 剩余时间=%v",
			now.Format("15:04:05"), expiry.Format("15:04:05"), timeUntilExpiry)

		// 如果URL已经过期，直接返回true
		if timeUntilExpiry <= 0 {
			fs.Debugf(nil, "⚠️ 115网盘URL已过期(URL解析)")
			return true
		}

		// 根据剩余时间选择缓冲策略
		var expiryDelta time.Duration
		if timeUntilExpiry < 2*time.Minute {
			expiryDelta = 15 * time.Second // 短期URL使用15秒缓冲
		} else {
			expiryDelta = 30 * time.Second // 长期URL使用30秒缓冲
		}

		isExpired := timeUntilExpiry <= expiryDelta
		fs.Debugf(nil, "🔍 115网盘URL过期决策(URL解析): 缓冲区=%v, 已过期=%v", expiryDelta, isExpired)

		return isExpired
	}

	// 策略2：URL解析失败时的fallback - 使用创建时间 + 4分钟
	if !u.CreatedAt.IsZero() {
		timeSinceCreated := time.Since(u.CreatedAt)
		fallbackExpired := timeSinceCreated > 4*time.Minute

		fs.Debugf(nil, "🔍 115网盘URL过期检查(fallback): 创建时间=%v, 已存在时间=%v, 已过期=%v",
			u.CreatedAt.Format("15:04:05"), timeSinceCreated, fallbackExpired)

		return fallbackExpired
	}

	// 策略3：最后的fallback - 假设不过期（保持原有行为）
	fs.Debugf(nil, "🔍 115网盘URL过期检查(最后fallback): 假设不过期")
	return false
}

// Valid reports whether u is non-nil and is not expired.
func (u *DownloadURL) Valid() bool {
	return u != nil && u.URL != "" && !u.expired()
}

func (u *DownloadURL) Cookie() string {
	cookie := ""
	for _, ck := range u.Cookies {
		cookie += fmt.Sprintf("%s=%s;", ck.Name, ck.Value)
	}
	return cookie
}

// DownloadInfo represents the structure within the DownloadData map (Traditional)
type DownloadInfo struct {
	FileName string      `json:"file_name"`
	FileSize Int64       `json:"file_size"`
	PickCode string      `json:"pick_code"`
	URL      DownloadURL `json:"url"`
}

// DownloadData is the map returned by the traditional download URL endpoint
type DownloadData map[string]*DownloadInfo

// OpenAPI specific download response structure
type OpenAPIDownloadInfo struct {
	FileName string      `json:"file_name"`
	FileSize Int64       `json:"file_size"`
	PickCode string      `json:"pick_code"`
	Sha1     string      `json:"sha1"`
	URL      DownloadURL `json:"url"` // Assumes nested URL object like traditional
}

// OpenAPIDownloadResp represents the response from POST /open/ufile/downurl
// 115网盘API有时返回map格式，有时返回array格式，需要灵活处理
type OpenAPIDownloadResp struct {
	OpenAPIBase
	// Data can be either a map or an array, we'll handle both formats
	Data json.RawMessage `json:"data"`
}

// GetDownloadInfo 从响应中提取下载信息，处理map和array两种格式
func (r *OpenAPIDownloadResp) GetDownloadInfo() (*OpenAPIDownloadInfo, error) {
	if len(r.Data) == 0 {
		return nil, errors.New("empty data field in download response")
	}

	// 尝试解析为map格式
	var mapData map[string]*OpenAPIDownloadInfo
	if err := json.Unmarshal(r.Data, &mapData); err == nil {
		// 成功解析为map，返回第一个有效的下载信息
		for _, info := range mapData {
			if info != nil {
				return info, nil
			}
		}
		return nil, errors.New("no valid download info found in map data")
	}

	// 尝试解析为array格式
	var arrayData []*OpenAPIDownloadInfo
	if err := json.Unmarshal(r.Data, &arrayData); err == nil {
		// 成功解析为array，返回第一个有效的下载信息
		for _, info := range arrayData {
			if info != nil {
				return info, nil
			}
		}
		return nil, errors.New("no valid download info found in array data")
	}

	// 两种格式都解析失败
	return nil, fmt.Errorf("unable to parse data field as either map or array: %s", string(r.Data))
}

// ------------------------------------------------------------
// Upload related structs
// ------------------------------------------------------------

// UploadBasicInfo (Traditional - /app/uploadinfo) - May become obsolete
type UploadBasicInfo struct {
	TraditionalBase
	Uploadinfo       string      `json:"uploadinfo,omitempty"`
	UserID           json.Number `json:"user_id,omitempty"`
	AppVersion       int         `json:"app_version,omitempty"`
	AppID            int         `json:"app_id,omitempty"`
	Userkey          string      `json:"userkey,omitempty"`
	SizeLimit        int64       `json:"size_limit,omitempty"`
	SizeLimitYun     int64       `json:"size_limit_yun,omitempty"`
	MaxDirLevel      int64       `json:"max_dir_level,omitempty"`
	MaxDirLevelYun   int64       `json:"max_dir_level_yun,omitempty"`
	MaxFileNum       int64       `json:"max_file_num,omitempty"`
	MaxFileNumYun    int64       `json:"max_file_num_yun,omitempty"`
	UploadAllowed    bool        `json:"upload_allowed,omitempty"`
	UploadAllowedMsg string      `json:"upload_allowed_msg,omitempty"`
}

// UploadInitInfo represents the response from upload init/resume (adaptable for both)
type UploadInitInfo struct {
	// Common Base - OpenAPI uses state/code/message, Traditional uses statuscode/statusmsg
	OpenAPIBase
	Request   string `json:"request,omitempty"`    // Traditional
	ErrorCode int    `json:"statuscode,omitempty"` // Traditional
	ErrorMsg  string `json:"statusmsg,omitempty"`  // Traditional

	// Data payload (nested under 'data' in OpenAPI)
	Data *UploadInitData `json:"data,omitempty"` // OpenAPI nests the main info

	// Traditional top-level fields (might be moved into Data for OpenAPI)
	Status   Int    `json:"status,omitempty"`   // Traditional: 1=need upload, 2=秒传; OpenAPI: 1=non-秒传, 2=秒传, 6/7/8=auth
	PickCode string `json:"pickcode,omitempty"` // Traditional: pickcode; OpenAPI: pick_code
	Target   string `json:"target,omitempty"`   // Both
	Version  string `json:"version,omitempty"`  // Both?

	// OSS upload fields (Traditional top-level, OpenAPI in 'data')
	Bucket   string          `json:"bucket,omitempty"`   // Both
	Object   string          `json:"object,omitempty"`   // Both
	Callback json.RawMessage `json:"callback,omitempty"` // Both (structure might differ)

	// Useless fields (Traditional)
	FileID   int    `json:"fileid,omitempty"`
	FileInfo string `json:"fileinfo,omitempty"`

	// New fields in upload v4.0 / OpenAPI
	SignKey   string `json:"sign_key,omitempty"`   // Both
	SignCheck string `json:"sign_check,omitempty"` // Both
	FileIDStr string `json:"file_id,omitempty"`    // OpenAPI: file_id (for 秒传 success)

	// Raw data for custom UnmarshalJSON
	rawData json.RawMessage
}

// UnmarshalJSON handles custom unmarshaling for UploadInitInfo
func (ui *UploadInitInfo) UnmarshalJSON(data []byte) error {
	// Define an alias type to avoid infinite recursion
	type Alias UploadInitInfo
	aux := &struct {
		Data json.RawMessage `json:"data"`
		*Alias
	}{
		Alias: (*Alias)(ui),
	}

	// Unmarshal into the auxiliary struct
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}

	// Store raw data for later processing
	ui.rawData = aux.Data

	// Handle different formats for the data field
	if len(aux.Data) > 0 {
		// Try to unmarshal as an object first
		var objData UploadInitData
		if err := json.Unmarshal(aux.Data, &objData); err != nil {
			// If that fails, try as an array
			var arrData []map[string]any
			if err := json.Unmarshal(aux.Data, &arrData); err != nil {
				return fmt.Errorf("data field is neither a valid object nor an array: %w", err)
			}

			// If it's an array and has at least one element, use the first element
			if len(arrData) > 0 {
				// Convert the first array element back to JSON
				firstElem, err := json.Marshal(arrData[0])
				if err != nil {
					return fmt.Errorf("failed to marshal first array element: %w", err)
				}

				// Then unmarshal it into the objData
				if err := json.Unmarshal(firstElem, &objData); err != nil {
					return fmt.Errorf("failed to unmarshal first array element: %w", err)
				}

				ui.Data = &objData
			}
		} else {
			// It was a valid object
			ui.Data = &objData
		}
	}

	return nil
}

// UploadInitData holds the nested data part of the OpenAPI upload init/resume response
type UploadInitData struct {
	PickCode  string          `json:"pick_code"`         // Upload task ID
	Status    Int             `json:"status"`            // 1: non-秒传, 2: 秒传, 6/7/8: auth needed
	SignKey   string          `json:"sign_key"`          // SHA1 ID for secondary auth
	SignCheck string          `json:"sign_check"`        // SHA1 range for secondary auth
	FileID    string          `json:"file_id"`           // File ID if 秒传 success (status=2)
	Target    string          `json:"target"`            // Upload target string
	Bucket    string          `json:"bucket"`            // OSS Bucket
	Object    string          `json:"object"`            // OSS Object ID
	Callback  json.RawMessage `json:"callback"`          // Can be either struct or array
	Version   string          `json:"version,omitempty"` // Optional version info
}

// GetCallback decodes and returns the callback string
func (ui *UploadInitInfo) GetCallback() string {
	// Get the raw callback data first
	var rawCallback string
	data := ui.Data
	if data == nil {
		// Fallback to traditional structure
		// Try to unmarshal as object first
		var callbackStruct struct {
			Callback    string `json:"callback"`
			CallbackVar string `json:"callback_var"`
		}

		if err := json.Unmarshal(ui.Callback, &callbackStruct); err == nil && callbackStruct.Callback != "" {
			rawCallback = callbackStruct.Callback
		} else {
			// Try to unmarshal as array if object failed
			var callbackArray []string
			if err := json.Unmarshal(ui.Callback, &callbackArray); err == nil && len(callbackArray) > 0 {
				rawCallback = callbackArray[0]
			} else {
				// Fall back to string representation if both fail
				rawCallback = string(ui.Callback)
			}
		}
	} else {
		// Try to unmarshal as object first
		var callbackStruct struct {
			Callback    string `json:"callback"`
			CallbackVar string `json:"callback_var"`
		}

		if err := json.Unmarshal(data.Callback, &callbackStruct); err == nil && callbackStruct.Callback != "" {
			rawCallback = callbackStruct.Callback
		} else {
			// Try to unmarshal as array if object failed
			var callbackArray []string
			if err := json.Unmarshal(data.Callback, &callbackArray); err == nil && len(callbackArray) > 0 {
				rawCallback = callbackArray[0]
			} else {
				// Fall back to string representation if both fail
				rawCallback = string(data.Callback)
			}
		}
	}

	// Check if the callback data is already base64 encoded
	if _, err := base64.StdEncoding.DecodeString(rawCallback); err != nil {
		// Not valid base64, so encode it
		return base64.StdEncoding.EncodeToString([]byte(rawCallback))
	}

	// Already base64 encoded, return as is
	return rawCallback
}

// GetCallbackVar decodes and returns the callback variables string
func (ui *UploadInitInfo) GetCallbackVar() string {
	// Get the raw callback var data first
	var rawCallbackVar string
	data := ui.Data
	if data == nil {
		// Fallback to traditional structure
		// Try to unmarshal as object first
		var callbackStruct struct {
			Callback    string `json:"callback"`
			CallbackVar string `json:"callback_var"`
		}

		if err := json.Unmarshal(ui.Callback, &callbackStruct); err == nil && callbackStruct.CallbackVar != "" {
			rawCallbackVar = callbackStruct.CallbackVar
		} else {
			// Try to unmarshal as array if object failed
			var callbackArray []string
			if err := json.Unmarshal(ui.Callback, &callbackArray); err == nil && len(callbackArray) > 1 {
				rawCallbackVar = callbackArray[1]
			} else {
				// No callback var found
				return ""
			}
		}
	} else {
		// Try to unmarshal as object first
		var callbackStruct struct {
			Callback    string `json:"callback"`
			CallbackVar string `json:"callback_var"`
		}

		if err := json.Unmarshal(data.Callback, &callbackStruct); err == nil && callbackStruct.CallbackVar != "" {
			rawCallbackVar = callbackStruct.CallbackVar
		} else {
			// Try to unmarshal as array if object failed
			var callbackArray []string
			if err := json.Unmarshal(data.Callback, &callbackArray); err == nil && len(callbackArray) > 1 {
				rawCallbackVar = callbackArray[1]
			} else {
				// No callback var found
				return ""
			}
		}
	}

	// If we have callback var data, check if it's already base64 encoded
	if rawCallbackVar != "" {
		if _, err := base64.StdEncoding.DecodeString(rawCallbackVar); err != nil {
			// Not valid base64, so encode it
			return base64.StdEncoding.EncodeToString([]byte(rawCallbackVar))
		}
	}

	// Already base64 encoded or empty, return as is
	return rawCallbackVar
}

// GetPickCode returns the pick code from the appropriate field
func (ui *UploadInitInfo) GetPickCode() string {
	if ui.Data != nil {
		return ui.Data.PickCode
	}
	return ui.PickCode
}

// GetStatus returns the status code from the appropriate field
func (ui *UploadInitInfo) GetStatus() Int {
	if ui.Data != nil {
		return ui.Data.Status
	}
	return ui.Status
}

// GetFileID returns the file ID (on 秒传) from the appropriate field
func (ui *UploadInitInfo) GetFileID() string {
	if ui.Data != nil {
		return ui.Data.FileID
	}
	return ui.FileIDStr
}

// GetSignKey returns the sign key from the appropriate field
func (ui *UploadInitInfo) GetSignKey() string {
	if ui.Data != nil {
		return ui.Data.SignKey
	}
	return ui.SignKey
}

// GetSignCheck returns the sign check range from the appropriate field
func (ui *UploadInitInfo) GetSignCheck() string {
	if ui.Data != nil {
		return ui.Data.SignCheck
	}
	return ui.SignCheck
}

// GetBucket returns the OSS bucket from the appropriate field
func (ui *UploadInitInfo) GetBucket() string {
	if ui.Data != nil {
		return ui.Data.Bucket
	}
	return ui.Bucket
}

// GetObject returns the OSS object key from the appropriate field
func (ui *UploadInitInfo) GetObject() string {
	if ui.Data != nil {
		return ui.Data.Object
	}
	return ui.Object
}

// CallbackInfo represents the structure of the callback response after OSS upload (Traditional)
type CallbackInfo struct {
	TraditionalBase
	Data *CallbackData `json:"data,omitempty"`
}

// CallbackData holds the details from the upload callback
type CallbackData struct {
	AID      json.Number `json:"aid"`
	CID      string      `json:"cid"`
	FileID   string      `json:"file_id"`
	FileName string      `json:"file_name"`
	FileSize Int64       `json:"file_size"`
	IsVideo  int         `json:"is_video"`
	PickCode string      `json:"pick_code"`
	Sha      string      `json:"sha1"`
	ThumbURL string      `json:"thumb_url,omitempty"`
}

// OSSToken represents the structure for OSS credentials (adaptable)
type OSSToken struct {
	AccessKeyID     string `json:"AccessKeyId"`     // OpenAPI uses AccessKeyId
	AccessKeySecret string `json:"AccessKeySecret"` // OpenAPI uses AccessKeySecrett (typo in docs?) -> Corrected to AccessKeySecret based on common usage
	Expiration      Time   `json:"Expiration"`      // OpenAPI uses Expiration (RFC3339 format)
	SecurityToken   string `json:"SecurityToken"`   // OpenAPI uses SecurityToken
	Endpoint        string `json:"endpoint"`        // OpenAPI provides endpoint

	// Traditional fields (might be redundant)
	StatusCode   string `json:"StatusCode,omitempty"`
	ErrorCode    string `json:"ErrorCode,omitempty"`
	ErrorMessage string `json:"ErrorMessage,omitempty"`
}

// OSSTokenResp represents the response from GET /open/upload/get_token
type OSSTokenResp struct {
	OpenAPIBase
	Data *OSSToken `json:"data"` // Assuming data holds a single OSSToken object
}

// TimeToExpiry calculates duration until token expiry
func (t *OSSToken) TimeToExpiry() time.Duration {
	if t == nil {
		return 0
	}
	exp := time.Time(t.Expiration)
	if exp.IsZero() {
		// Should not happen with OpenAPI tokens, but handle defensively
		return 3e9 * time.Second // ~95 years
	}
	// Use a safety margin (e.g., 5 minutes)
	return time.Until(exp) - 5*time.Minute
}

// ------------------------------------------------------------
// Sharing related structs (Assume Traditional API for now)
// ------------------------------------------------------------

// NewURL represents the response for adding offline tasks (Traditional)
type NewURL struct {
	State    bool   `json:"state,omitempty"`
	ErrorMsg string `json:"error_msg,omitempty"`
	Errno    int    `json:"errno,omitempty"`
	Result   []struct {
		State    bool   `json:"state,omitempty"`
		ErrorMsg string `json:"error_msg,omitempty"`
		Errno    int    `json:"errno,omitempty"`
		Errtype  string `json:"errtype,omitempty"`
		Errcode  int    `json:"errcode,omitempty"`
		InfoHash string `json:"info_hash,omitempty"`
		URL      string `json:"url,omitempty"`
		Files    []struct {
			ID   string `json:"id,omitempty"`
			Name string `json:"name,omitempty"`
			Size int64  `json:"size,omitempty"`
		} `json:"files,omitempty"`
	} `json:"result,omitempty"`
	Errcode int `json:"errcode,omitempty"`
}

// ShareSnap represents the response for listing shared files (Traditional)
type ShareSnap struct {
	TraditionalBase
	Data *ShareSnapData `json:"data,omitempty"`
}

type ShareSnapData struct {
	Userinfo struct {
		UserID   string `json:"user_id,omitempty"`
		UserName string `json:"user_name,omitempty"`
		Face     string `json:"face,omitempty"`
	} `json:"userinfo,omitempty"`
	Shareinfo struct {
		SnapID           string      `json:"snap_id,omitempty"`
		FileSize         string      `json:"file_size,omitempty"`
		ShareTitle       string      `json:"share_title,omitempty"`
		ShareState       json.Number `json:"share_state,omitempty"`
		ForbidReason     string      `json:"forbid_reason,omitempty"`
		CreateTime       string      `json:"create_time,omitempty"`
		ReceiveCode      string      `json:"receive_code,omitempty"`
		ReceiveCount     string      `json:"receive_count,omitempty"`
		ExpireTime       int         `json:"expire_time,omitempty"`
		FileCategory     int         `json:"file_category,omitempty"`
		AutoRenewal      string      `json:"auto_renewal,omitempty"`
		AutoFillRecvcode string      `json:"auto_fill_recvcode,omitempty"`
		CanReport        int         `json:"can_report,omitempty"`
		CanNotice        int         `json:"can_notice,omitempty"`
		HaveVioFile      int         `json:"have_vio_file,omitempty"`
	} `json:"shareinfo,omitempty"`
	Count      int         `json:"count,omitempty"`
	List       []*File     `json:"list,omitempty"` // Uses the common File struct
	ShareState json.Number `json:"share_state,omitempty"`
	UserAppeal struct {
		CanAppeal       int `json:"can_appeal,omitempty"`
		CanShareAppeal  int `json:"can_share_appeal,omitempty"`
		PopupAppealPage int `json:"popup_appeal_page,omitempty"`
		CanGlobalAppeal int `json:"can_global_appeal,omitempty"`
	} `json:"user_appeal,omitempty"`
}

// ShareDownloadInfo represents the response for getting download URL from share (Traditional)
type ShareDownloadInfo struct {
	FileID   string      `json:"fid"`
	FileName string      `json:"fn"`
	FileSize Int64       `json:"fs"`
	URL      DownloadURL `json:"url"`
}

// SampleInitResp represents the response from sampleinitupload.php (Traditional)
type SampleInitResp struct {
	Object    string `json:"object"`
	AccessID  string `json:"accessid"`
	Host      string `json:"host"`
	Policy    string `json:"policy"`
	Signature string `json:"signature"`
	Expire    int64  `json:"expire"`
	Callback  string `json:"callback"`
	ErrorCode int    `json:"errno,omitempty"`
	Error     string `json:"error,omitempty"`
}

// API类型枚举
type APIType int

const (
	OpenAPI APIType = iota
	TraditionalAPI
)

// Constants
const (
	domain             = "www.115.com"
	traditionalRootURL = "https://webapi.115.com"
	openAPIRootURL     = "https://proapi.115.com"
	passportRootURL    = "https://passportapi.115.com"
	qrCodeAPIRootURL   = "https://qrcodeapi.115.com"
	hnQrCodeAPIRootURL = "https://hnqrcodeapi.115.com"     // For confirm step
	defaultAppID       = "100195741"                       // Provided App ID
	tradUserAgent      = "Mozilla/5.0 115Browser/27.0.7.5" // Keep for traditional login mimicry?
	defaultUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"

	// 🚦 115网盘统一QPS控制：全局账户级别限制，避免770004错误
	unifiedMinSleep = fs.Duration(300 * time.Millisecond) // 🔧 平衡优化：~5 QPS - 平衡性能与稳定性

	maxSleep      = 2 * time.Second
	decayConstant = 2 // bigger for slower decay, exponential

	// 默认配置常量（无需用户配置）
	defaultMaxRetryAttempts   = 5                             // 默认最大重试5次
	defaultRetryBaseDelay     = fs.Duration(2 * time.Second)  // 默认基础重试延迟2秒
	defaultSlowSpeedThreshold = fs.SizeSuffix(100 * 1024)     // 默认慢速阈值100KB/s
	defaultSpeedCheckInterval = fs.Duration(30 * time.Second) // 默认速度检查间隔30秒
	defaultSlowSpeedAction    = "log"                         // 默认慢速处理动作：记录日志
	defaultChunkTimeout       = fs.Duration(5 * time.Minute)  // 默认分片超时5分钟

	maxUploadSize       = 115 * fs.Gibi    // 115 GiB from https://proapi.115.com/app/uploadinfo (or OpenAPI equivalent)
	maxUploadParts      = 10000            // Part number must be an integer between 1 and 10000, inclusive.
	defaultChunkSize    = 100 * fs.Mebi    // 🔧 性能优化：提升到100MB，与123网盘对齐，减少分片数量
	minChunkSize        = 100 * fs.Kibi    // 最小分片大小：100KB
	maxChunkSize        = 5 * fs.Gibi      // 最大分片大小：5GB（OSS限制）
	defaultUploadCutoff = 500 * fs.Mebi    // 🔧 优化上传切换阈值：500MB，与123网盘策略对齐
	defaultNohashSize   = 100 * fs.Mebi    // 无哈希上传阈值：100MB，小文件优先传统上传
	StreamUploadLimit   = 5 * fs.Gibi      // Max size for sample/streamed upload (traditional)
	maxUploadCutoff     = 5 * fs.Gibi      // maximum allowed size for singlepart uploads (OSS PutObject limit)
	tokenRefreshWindow  = 30 * time.Minute // Refresh token 30 minutes before expiry (115网盘令牌有效期较短)
	pkceVerifierLength  = 64               // Length for PKCE code verifier

	// Unified file size judgment constants, consistent with 123 drive
	memoryBufferThreshold = int64(50 * 1024 * 1024) // 50MB memory buffer threshold
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "115",
		Description: "115网盘驱动器 (支持开放API)",
		NewFs:       NewFs,
		CommandHelp: commandHelp,
		Options: []fs.Option{{
			Name: "cookie",
			Help: `提供格式为 "UID=...; CID=...; SEID=...;" 的登录Cookie。
初次登录获取API令牌时必需。
示例: "UID=123; CID=abc; SEID=def;"`,
			Required:  true,
			Sensitive: true,
		}, {
			Name:     "user_agent",
			Default:  defaultUserAgent,
			Advanced: true,
			Help: fmt.Sprintf(`HTTP用户代理。主要用于初次登录模拟。
默认值为 "%s"。`, defaultUserAgent),
		}, {
			Name: "root_folder_id",
			Help: `根文件夹的ID。
通常留空（使用网盘根目录 '0'）。
如需让rclone使用非根文件夹作为起始点，请填入相应ID。`,
			Advanced:  true,
			Sensitive: true,
		}, {
			Name:     "app_id",
			Default:  defaultAppID,
			Advanced: true,
			Help: fmt.Sprintf(`用于身份验证的自定义应用ID。
默认值为 "%s"。除非有特定理由使用不同的应用ID，否则不要更改。`, defaultAppID),
		}, {
			Name:     "upload_hash_only",
			Default:  false,
			Advanced: true,
			Help: `仅尝试基于哈希的上传（秒传）。如果服务器没有该文件则跳过上传。
需要SHA1哈希值可用或可计算。`,
		}, {
			Name:     "only_stream",
			Default:  false,
			Advanced: true,
			Help:     `对所有不超过5 GiB的文件使用传统流式上传（样本上传）。大于此大小的文件会失败。`,
		}, {
			Name:     "fast_upload",
			Default:  false,
			Advanced: true,
			Help: `上传策略：
- 文件 <= nohash_size：使用传统流式上传。
- 文件 > nohash_size：尝试基于哈希的上传（秒传）。
- 如果秒传失败且大小 <= 5 GiB：使用传统流式上传。
- 如果秒传失败且大小 > 5 GiB：使用分片上传。`,
		}, {
			Name:     "hash_memory_limit",
			Help:     "大于此大小的文件将缓存到磁盘以计算哈希值（如需要）。",
			Default:  fs.SizeSuffix(10 * 1024 * 1024),
			Advanced: true,
		}, {
			Name:     "nohash_size",
			Help:     `小于此大小的文件在启用fast_upload或未尝试/失败哈希上传时将使用传统流式上传。最大值为5 GiB。`,
			Default:  defaultNohashSize,
			Advanced: true,
		}, {
			Name: "upload_cutoff",
			Help: `切换到分片上传的文件大小阈值。

大于此大小的文件将使用分片上传。小于此大小的文件将使用OSS PutObject单文件上传。

最小值为0，最大值为5 GiB。`,
			Default:  defaultUploadCutoff,
			Advanced: true,
		}, {
			Name: "chunk_size",
			Help: `分片上传时的分片大小。

当上传大于upload_cutoff的文件或未知大小的文件时，将使用此分片大小进行分片上传。

注意：每个传输会在内存中缓冲此大小的数据块。

最小值为100 KiB，最大值为5 GiB。`,
			Default:  defaultChunkSize,
			Advanced: true,
		}, {
			Name:     "max_upload_parts",
			Help:     `分片上传中的最大分片数量。`,
			Default:  maxUploadParts,
			Advanced: true,
		}, {
			Name:     "internal",
			Help:     `使用内部OSS端点进行上传（需要适当的网络访问权限）。`,
			Default:  false,
			Advanced: true,
		}, {
			Name:     "dual_stack",
			Help:     `使用双栈（IPv4/IPv6）OSS端点进行上传。`,
			Default:  false,
			Advanced: true,
		}, {
			Name:     "no_check",
			Default:  false,
			Advanced: true,
			Help:     "禁用上传后检查（避免额外API调用但降低确定性）。",
		}, {
			Name:     "no_buffer",
			Default:  false,
			Advanced: true,
			Help:     "跳过上传时的磁盘缓冲。",
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
			Default: (encoder.EncodeLtGt |
				encoder.EncodeDoubleQuote |
				encoder.EncodeLeftSpace |
				encoder.EncodeCtl |
				encoder.EncodeRightSpace |
				encoder.EncodeSlash | // 新增：默认编码斜杠
				encoder.EncodeInvalidUtf8), // 保留：编码无效UTF-8字符
		}},
	})
}

type Options struct {
	Cookie              string        `config:"cookie"` // Single cookie string now
	UserAgent           string        `config:"user_agent"`
	RootFolderID        string        `config:"root_folder_id"`
	HashMemoryThreshold fs.SizeSuffix `config:"hash_memory_limit"`
	UploadHashOnly      bool          `config:"upload_hash_only"`
	OnlyStream          bool          `config:"only_stream"`
	FastUpload          bool          `config:"fast_upload"`
	NohashSize          fs.SizeSuffix `config:"nohash_size"`
	UploadCutoff        fs.SizeSuffix `config:"upload_cutoff"`
	ChunkSize           fs.SizeSuffix `config:"chunk_size"`
	MaxUploadParts      int           `config:"max_upload_parts"`
	StreamHashMode      bool          `config:"stream_hash_mode"`

	Internal  bool                 `config:"internal"`
	DualStack bool                 `config:"dual_stack"`
	NoCheck   bool                 `config:"no_check"`
	NoBuffer  bool                 `config:"no_buffer"` // Skip disk buffering for uploads
	Enc       encoder.MultiEncoder `config:"encoding"`
	AppID     string               `config:"app_id"` // Custom App ID for authentication

	// API限流控制选项
	QPSLimit fs.Duration `config:"qps_limit"` // API调用间隔时间 (例如: 250ms = 4 QPS)
}

// CachedDownloadURL 缓存的下载URL结构
type CachedDownloadURL struct {
	URL       string    // 下载URL
	ExpiresAt time.Time // 过期时间
}

// 💾 115网盘持久化缓存数据结构
type PersistentDownloadURLCache struct {
	Data    map[string]CachedDownloadURL `json:"data"`
	SavedAt time.Time                    `json:"saved_at"`
	Version string                       `json:"version"`
}

// 💾 115网盘listAll持久化缓存数据结构
type PersistentListAllCache struct {
	Data    map[string]*CachedListAllResponse `json:"data"`
	SavedAt time.Time                         `json:"saved_at"`
	Version string                            `json:"version"`
}

type CachedListAllResponse struct {
	Files     []*File   `json:"files"`
	CachedAt  time.Time `json:"cached_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Valid 检查缓存的URL是否仍然有效
func (c CachedDownloadURL) Valid() bool {
	return time.Now().Before(c.ExpiresAt)
}

// NearExpiry 检查缓存是否即将过期（剩余时间少于1分钟）
func (c CachedDownloadURL) NearExpiry() bool {
	return time.Until(c.ExpiresAt) < time.Minute
}

// cleanExpiredURLCache 清理过期的下载URL缓存
func (f *Fs) cleanExpiredURLCache() {
	f.downloadURLCache.Range(func(key, value interface{}) bool {
		if cached, ok := value.(CachedDownloadURL); ok {
			if !cached.Valid() {
				f.downloadURLCache.Delete(key)
				fs.Debugf(f, "🧹 清理过期的下载URL缓存: %v", key)
			}
		}
		return true
	})
}

// refreshDownloadURLCache 异步刷新下载URL缓存
func (o *Object) refreshDownloadURLCache(ctx context.Context) error {
	fs.Debugf(o, "🔄 异步刷新下载URL缓存")

	// 强制刷新URL（跳过缓存检查）
	o.durlMu.Lock()
	o.durl = nil // 清除当前URL，强制重新获取
	o.durlMu.Unlock()

	// 重新获取URL，这会自动更新缓存
	return o.setDownloadURL(ctx)
}

// TransferSpeedMonitor 传输速度监控器
type TransferSpeedMonitor struct {
	name           string              // 传输名称
	startTime      time.Time           // 传输开始时间
	lastCheckTime  time.Time           // 上次检查时间
	lastBytes      int64               // 上次检查时的字节数
	slowSpeedCount int                 // 连续慢速检测次数
	account        *accounting.Account // 关联的Account对象
	fs             *Fs                 // 文件系统实例
	ctx            context.Context     // 传输上下文
	cancel         context.CancelFunc  // 取消函数
	done           chan struct{}       // 完成信号
	mu             sync.Mutex          // 保护并发访问
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
	pacer         *fs.Pacer // 统一的API调速器，符合rclone标准模式
	rootFolder    string    // path of the absolute root
	rootFolderID  string
	appVer        string // parsed from user-agent; used in traditional calls
	userID        string // User ID from cookie/token
	userkey       string // User key from traditional uploadinfo (needed for traditional upload init signature)
	isShare       bool   // mark it is from shared or not
	fileObj       *fs.Object
	m             configmap.Mapper // config map for saving tokens
	tokenMu       sync.Mutex
	accessToken   string
	refreshToken  string
	tokenExpiry   time.Time
	codeVerifier  string // For PKCE
	tokenRenewer  *oauthutil.Renew
	isRefreshing  atomic.Bool // 防止递归调用的标志
	loginMu       sync.Mutex
	uploadingMu   sync.Mutex
	isUploading   bool

	// API限流智能控制
	apiLimitMu          sync.Mutex
	consecutiveAPILimit int       // 连续API限流错误计数
	lastAPILimitTime    time.Time // 最后一次API限流错误时间
	currentQPSLimit     float64   // 当前动态QPS限制

	// 传输速度监控
	speedMonitorMu  sync.Mutex
	activeTransfers map[string]*TransferSpeedMonitor // 活跃传输的速度监控器

	// 🔧 性能优化：下载URL缓存系统
	downloadURLCache sync.Map // 下载URL缓存 (map[string]CachedDownloadURL)

	// 🚀 性能优化：listAll结果缓存系统（类似123网盘的listFileCache）
	listAllCache *cache.Cache // listAll结果缓存，避免重复API调用

	// 💾 持久化缓存系统
	persistentCacheDir   string // 持久化缓存目录
	downloadURLCacheFile string // 下载URL缓存文件路径
	listAllCacheFile     string // listAll缓存文件路径
}

// NewTransferSpeedMonitor 创建新的传输速度监控器
func NewTransferSpeedMonitor(name string, account *accounting.Account, fs *Fs, ctx context.Context) *TransferSpeedMonitor {
	monitorCtx, cancel := context.WithCancel(ctx)

	monitor := &TransferSpeedMonitor{
		name:          name,
		startTime:     time.Now(),
		lastCheckTime: time.Now(),
		lastBytes:     0,
		account:       account,
		fs:            fs,
		ctx:           monitorCtx,
		cancel:        cancel,
		done:          make(chan struct{}),
	}

	// 启动监控goroutine
	go monitor.monitorLoop()

	return monitor
}

// monitorLoop 监控循环，定期检查传输速度
func (m *TransferSpeedMonitor) monitorLoop() {
	defer close(m.done)

	// 传输速度监控默认启用，无需检查配置

	ticker := time.NewTicker(time.Duration(defaultSpeedCheckInterval))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.checkSpeed()
		case <-m.ctx.Done():
			return
		}
	}
}

// checkSpeed 检查当前传输速度
func (m *TransferSpeedMonitor) checkSpeed() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.account == nil {
		return
	}

	// 由于Account的很多方法都是私有的，我们采用简化的方法
	// 通过反射或者其他方式来获取速度信息比较复杂，
	// 这里我们采用一个更简单的方法：检查传输是否在指定时间内有进展

	now := time.Now()
	threshold := float64(defaultSlowSpeedThreshold)

	// 检查传输是否长时间没有进展
	// 这是一个简化的实现，主要检测传输是否卡住
	timeSinceLastCheck := now.Sub(m.lastCheckTime)

	// 如果检查间隔过短，跳过
	if timeSinceLastCheck < time.Duration(defaultSpeedCheckInterval)/2 {
		return
	}

	// 简化的慢速检测：如果传输时间过长，认为可能存在问题
	totalTime := now.Sub(m.startTime)
	if totalTime > 10*time.Minute { // 如果传输超过10分钟
		m.slowSpeedCount++
		fs.Debugf(m.fs, "📉 检测到长时间传输: %s, 已用时=%v, 连续检测次数=%d",
			m.name, totalTime.Truncate(time.Second), m.slowSpeedCount)

		// 根据配置的动作处理慢速传输
		m.handleSlowSpeed(0, threshold) // 传入0作为当前速度
	} else if m.slowSpeedCount > 0 {
		// 传输时间正常，重置计数器
		fs.Debugf(m.fs, "📈 传输时间正常: %s, 已用时=%v",
			m.name, totalTime.Truncate(time.Second))
		m.slowSpeedCount = 0
	}

	// 更新检查时间
	m.lastCheckTime = now
}

// handleSlowSpeed 处理慢速传输
func (m *TransferSpeedMonitor) handleSlowSpeed(currentSpeed, threshold float64) {
	switch defaultSlowSpeedAction {
	case "log":
		// 只记录日志，不采取其他动作
		if m.slowSpeedCount == 1 {
			fs.Infof(m.fs, "⚠️ 传输速度过慢: %s, 当前速度=%.2f KB/s, 阈值=%.2f KB/s",
				m.name, currentSpeed/1024, threshold/1024)
		}

	case "abort":
		// 连续3次慢速后中止传输
		if m.slowSpeedCount >= 3 {
			fs.Errorf(m.fs, "❌ 传输速度持续过慢，中止传输: %s, 速度=%.2f KB/s",
				m.name, currentSpeed/1024)
			m.cancel() // 取消传输上下文
		}

	case "retry":
		// 连续5次慢速后标记需要重试（由上层处理）
		if m.slowSpeedCount >= 5 {
			fs.Logf(m.fs, "🔄 传输速度持续过慢，建议重试: %s, 速度=%.2f KB/s",
				m.name, currentSpeed/1024)
			// 这里可以设置一个标志，由上层代码检查并处理重试
		}
	}
}

// Stop 停止监控
func (m *TransferSpeedMonitor) Stop() {
	m.cancel()
	<-m.done // 等待监控goroutine结束
}

// startTransferMonitor 为传输启动速度监控
func (f *Fs) startTransferMonitor(name string, account *accounting.Account, ctx context.Context) *TransferSpeedMonitor {
	// 传输速度监控默认启用

	f.speedMonitorMu.Lock()
	defer f.speedMonitorMu.Unlock()

	// 创建新的监控器
	monitor := NewTransferSpeedMonitor(name, account, f, ctx)

	// 添加到活跃传输列表
	f.activeTransfers[name] = monitor

	fs.Debugf(f, "📊 启动传输速度监控: %s", name)
	return monitor
}

// stopTransferMonitor 停止传输速度监控
func (f *Fs) stopTransferMonitor(name string) {
	f.speedMonitorMu.Lock()
	defer f.speedMonitorMu.Unlock()

	if monitor, exists := f.activeTransfers[name]; exists {
		monitor.Stop()
		delete(f.activeTransfers, name)
		fs.Debugf(f, "📊 停止传输速度监控: %s", name)
	}
}

// Object describes a 115 object
type Object struct {
	fs             *Fs
	remote         string
	hasMetaData    bool
	id             string
	parent         string
	size           int64
	sha1sum        string
	pickCode       string
	modTime        time.Time
	durl           *DownloadURL // link to download the object
	durlMu         *sync.Mutex
	durlRefreshing bool       // 标记是否正在刷新URL，防止并发刷新
	pickCodeMu     sync.Mutex // 新增：保护pickCode获取的并发访问
}

// DownloadFileInfo represents the details of a single file in download URL response
type DownloadFileInfo struct {
	FileName string      `json:"file_name"` // Name of the file
	FileSize int64       `json:"file_size"` // Size of the file in bytes
	PickCode string      `json:"pick_code"` // File pick code (extraction code)
	SHA1     string      `json:"sha1"`      // SHA1 hash of the file content
	URL      DownloadURL `json:"url"`       // Object containing the download URL
}

type ApiResponse struct {
	State   bool                        `json:"state"`   // Indicates success or failure
	Message string                      `json:"message"` // Optional message
	Code    int                         `json:"code"`    // Status code
	Data    map[string]DownloadFileInfo `json:"data"`    // Map where keys are file IDs (strings) and values are DownloadFileInfo objects
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

// shouldRetry checks if a request should be retried based on the response, error, and API type.
// Use unified error classification and retry strategy
func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}

	// Check HTTP response status codes first
	if resp != nil {
		switch resp.StatusCode {
		case 429: // Too Many Requests
			fs.Debugf(nil, "🔄 HTTP 429 请求过多，延迟重试")
			return true, pacer.RetryAfterError(err, 15*time.Second)
		case 500, 502, 503, 504: // Server errors
			fs.Debugf(nil, "❌ 服务器错误 %d，重试中: %v", resp.StatusCode, err)
			return true, pacer.RetryAfterError(err, 5*time.Second)
		case 408: // Request Timeout
			fs.Debugf(nil, "⏰ 请求超时，重试中: %v", err)
			return true, pacer.RetryAfterError(err, 3*time.Second)
		}
	}

	// Use rclone standard error handling
	if err != nil {
		if fserrors.ShouldRetry(err) {
			return true, err
		}
	}

	// Use rclone standard HTTP status code retry logic
	return fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
}

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

// getOpenAPIHTTPClient creates an HTTP client with default UserAgent
func getOpenAPIHTTPClient(ctx context.Context) *http.Client {
	// Create a new context with the default UserAgent
	newCtx, ci := fs.AddConfig(ctx)
	ci.UserAgent = defaultUserAgent
	return fshttp.NewClient(newCtx)
}

// errorHandler parses a non 2xx error response into an error (Generic, might need adjustment per API)
func errorHandler(resp *http.Response) error {
	// Attempt to decode as OpenAPI error first
	openAPIErr := new(OpenAPIBase)
	bodyBytes, readErr := rest.ReadBody(resp) // Read body once
	if readErr != nil {
		fs.Debugf(nil, "❌ 无法读取错误响应体: %v", readErr)
		// Fallback to status code if body read fails
		return NewTokenError(fmt.Sprintf("HTTP error %d (%s)", resp.StatusCode, resp.Status))
	}

	decodeErr := json.Unmarshal(bodyBytes, &openAPIErr)
	if decodeErr == nil && !openAPIErr.State {
		// Successfully decoded as OpenAPI error
		err := openAPIErr.Err()
		// Check for specific token-related errors
		if openAPIErr.ErrCode() == 401 || openAPIErr.ErrCode() == 100001 || strings.Contains(openAPIErr.ErrMsg(), "token") { // Example codes
			return NewTokenError(err.Error(), true) // Assume token error needs refresh/relogin
		}
		return err
	}

	// Attempt to decode as Traditional error
	tradErr := new(TraditionalBase)
	decodeErr = json.Unmarshal(bodyBytes, &tradErr)
	if decodeErr == nil && !tradErr.State {
		// Successfully decoded as Traditional error
		return tradErr.Err()
	}

	// Fallback if JSON decoding fails or state is true (but status code != 2xx)
	fs.Debugf(nil, "❌ 无法解码错误响应: %v. 响应体: %s", decodeErr, string(bodyBytes))
	return NewTokenError(fmt.Sprintf("HTTP error %d (%s): %s", resp.StatusCode, resp.Status, string(bodyBytes)))
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
		fs.Debugf(f, "✅ 等待登录互斥锁后令牌仍然有效，跳过登录")
		return nil
	}

	// Parse cookie and setup clients
	if err := f.setupLoginEnvironment(ctx); err != nil {
		return err
	}

	fs.Debugf(f, "🔐 开始用户登录流程: %s", f.userID)

	// Generate PKCE
	var challenge string
	var err error
	if f.codeVerifier, challenge, err = generatePKCE(); err != nil {
		return fmt.Errorf("failed to generate PKCE codes: %w", err)
	}
	fs.Debugf(f, "🔑 生成PKCE挑战码成功")

	// Request device code
	loginUID, err := f.getAuthDeviceCode(ctx, challenge)
	if err != nil {
		return err
	}

	// Mimic QR scan confirmation
	if err := f.simulateQRCodeScan(ctx, loginUID); err != nil {
		// Only log errors from these steps, don't fail
		fs.Logf(f, "⚠️ 二维码扫描模拟步骤有错误（继续执行）: %v", err)
	}

	// Exchange device code for access token
	if err := f.exchangeDeviceCodeForToken(ctx, loginUID); err != nil {
		return err
	}

	// Get userkey for traditional uploads if needed
	if f.userkey == "" {
		fs.Debugf(f, "📤 使用传统API获取userkey...")
		if err := f.getUploadBasicInfo(ctx); err != nil {
			// Log error but don't fail login, userkey is only for traditional upload init
			fs.Logf(f, "⚠️ 获取userkey失败（某些传统上传需要）: %v", err)
		} else {
			fs.Debugf(f, "✅ 成功获取userkey")
		}
	}

	// 🔐 登录成功后自动保存令牌到配置
	if f.m != nil {
		f.saveToken(f.m)
		fs.Debugf(f, "💾 登录成功，令牌已自动保存到配置")
	}

	fs.Debugf(f, "✅ 登录流程完成")
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
	// 创建OpenAPI客户端
	httpClient := getOpenAPIHTTPClient(ctx)

	// OpenAPI客户端，用于token相关操作
	f.openAPIClient = rest.NewClient(httpClient).SetErrorHandler(errorHandler)

	return nil
}

// getAuthDeviceCode calls the authDeviceCode API to start the login process
func (f *Fs) getAuthDeviceCode(ctx context.Context, challenge string) (string, error) {
	// Use configured AppID if provided, otherwise use default
	clientID := f.opt.AppID

	authData := url.Values{
		"client_id":             []string{clientID},
		"code_challenge":        []string{challenge},
		"code_challenge_method": []string{"sha256"}, // Use SHA256
	}
	authOpts := rest.Opts{
		Method:       "POST",
		RootURL:      passportRootURL, // Use passport API domain
		Path:         "/open/authDeviceCode",
		Parameters:   authData, // Send as query parameters for POST? Docs say body, let's try body.
		Body:         strings.NewReader(authData.Encode()),
		ExtraHeaders: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	}
	var authResp AuthDeviceCodeResp
	// This initial call uses the traditional client *but doesn't need encryption*
	// It still needs the cookie and pacing.
	err := f.CallTraditionalAPI(ctx, &authOpts, nil, &authResp, true) // Pass skipEncrypt=true
	if err != nil {
		return "", fmt.Errorf("authDeviceCode failed: %w", err)
	}

	// Check for API-level errors first (including 40140109 access permission disabled)
	if apiErr := authResp.Err(); apiErr != nil {
		return "", fmt.Errorf("authDeviceCode API error: %w", apiErr)
	}

	if authResp.Data == nil || authResp.Data.UID == "" {
		return "", fmt.Errorf("authDeviceCode returned empty data: %v", authResp)
	}
	loginUID := authResp.Data.UID
	fs.Debugf(f, "✅ 设备码认证成功，登录UID: %s", loginUID)
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
	var scanResp TraditionalBase // Use base struct, don't care about response data much
	fs.Debugf(f, "📱 调用二维码扫描接口...")
	err := f.CallTraditionalAPI(ctx, &scanOpts, scanPayload, &scanResp, true)
	if err != nil {
		return fmt.Errorf("login_qrcode_scan failed: %w", err)
	}
	fs.Debugf(f, "✅ 二维码扫描调用成功 (状态: %v)", scanResp.State)
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
	var confirmResp TraditionalBase
	fs.Debugf(f, "📱 调用二维码扫描确认接口...")
	err := f.CallTraditionalAPI(ctx, &confirmOpts, confirmPayload, &confirmResp, true) // Needs encryption? Assume yes.
	if err != nil {
		return fmt.Errorf("login_qrcode_scan_confirm failed: %w", err)
	}
	fs.Debugf(f, "✅ 二维码扫描确认调用成功 (状态: %v)", confirmResp.State)
	return nil
}

// exchangeDeviceCodeForToken gets the access token using the device code
func (f *Fs) exchangeDeviceCodeForToken(ctx context.Context, loginUID string) error {
	tokenData := url.Values{
		"uid":           []string{loginUID},
		"code_verifier": []string{f.codeVerifier},
	}
	tokenOpts := rest.Opts{
		Method:       "POST",
		RootURL:      passportRootURL,
		Path:         "/open/deviceCodeToToken",
		Body:         strings.NewReader(tokenData.Encode()),
		ExtraHeaders: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	}
	var tokenResp DeviceCodeTokenResp
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

	fs.Debugf(f, "✅ 成功获取访问令牌，过期时间: %v", f.tokenExpiry)

	return nil
}

// refreshTokenIfNecessary refreshes the token if necessary
func (f *Fs) refreshTokenIfNecessary(ctx context.Context, refreshTokenExpired bool, forceRefresh bool) error {
	// Fix recursive calls: set refresh flag to prevent triggering refresh again during token refresh
	if !f.isRefreshing.CompareAndSwap(false, true) {
		fs.Debugf(f, "🔄 令牌刷新已在进行中，跳过")
		return nil
	}
	defer f.isRefreshing.Store(false)

	f.tokenMu.Lock()

	// Check if token is already valid when we acquire the lock
	// This handles the case where another thread just refreshed the token
	if !refreshTokenExpired && !forceRefresh && isTokenStillValid(f) {
		fs.Debugf(f, "✅ 获取锁后令牌仍然有效，跳过刷新")
		f.tokenMu.Unlock()
		return nil
	}

	// 如果需要执行完整登录，则执行完整登录
	if shouldPerformFullLogin(f, refreshTokenExpired) {
		f.tokenMu.Unlock()
		err := f.login(ctx) // login handles its own locking
		if err != nil {
			return err
		}
		// 保存成功登录后的令牌
		f.saveToken(f.m)
		return nil
	}

	// 如果令牌仍然有效且未强制刷新，则跳过刷新
	if !forceRefresh && isTokenStillValid(f) {
		f.tokenMu.Unlock()
		return nil
	}

	// 如果令牌无效或强制刷新，则尝试刷新令牌
	// 此时 shouldPerformFullLogin 已经返回 false，说明 refreshToken 存在且未过期
	// 因此可以直接尝试刷新 accessToken

	// Prepare for token refresh
	refreshToken := f.refreshToken // Make a local copy of the refresh token
	f.tokenMu.Unlock()             // Unlock before making API call

	// Perform the actual token refresh
	result, err := f.performTokenRefresh(ctx, refreshToken)
	if err != nil {
		// Check if this is a refresh token expired error that requires re-login
		var tokenErr *TokenError
		if errors.As(err, &tokenErr) && tokenErr.IsRefreshTokenExpired {
			fs.Debugf(f, "🔄 刷新令牌已过期，尝试完整重新登录")

			// Clear token information before re-login
			f.tokenMu.Lock()
			f.clearTokenInfo()
			f.tokenMu.Unlock()

			// Attempt re-login
			loginErr := f.login(ctx)
			if loginErr != nil {
				return fmt.Errorf("re-login failed after refresh token expired: %w", loginErr)
			}

			// Save the new token after successful login
			f.saveToken(f.m)
			fs.Debugf(f, "✅ 刷新令牌过期后重新登录成功")
			return nil
		}

		return err // Error already formatted with context
	}

	// Update the tokens with new values
	f.updateTokens(result)

	// Save the refreshed token to config
	f.saveToken(f.m)
	fs.Debugf(f, "💾 令牌刷新后已保存到配置文件")

	return nil
}

// shouldPerformFullLogin determines if we should skip refresh and do a full login
func shouldPerformFullLogin(f *Fs, refreshTokenExpired bool) bool {
	// 如果刷新令牌已过期，则需要完整重新登录
	if refreshTokenExpired {
		fs.Debugf(f, "🔄 刷新令牌已过期，需要完整重新登录")
		return true
	}

	// 如果没有访问令牌或刷新令牌，则需要完整重新登录
	if f.accessToken == "" || f.refreshToken == "" {
		fs.Debugf(f, "🔐 未找到访问令牌或刷新令牌，尝试完整登录")
		return true
	}

	// 否则，不需要完整登录，可以尝试刷新访问令牌
	return false
}

// isTokenStillValid checks if the current token is still valid
func isTokenStillValid(f *Fs) bool {
	return time.Now().Before(f.tokenExpiry.Add(-tokenRefreshWindow))
}

// clearTokenInfo clears all token information (must be called with tokenMu locked)
func (f *Fs) clearTokenInfo() {
	f.accessToken = ""
	f.refreshToken = ""
	f.tokenExpiry = time.Time{}
	fs.Debugf(f, "🧹 已清空所有令牌信息")
}

// performTokenRefresh handles the actual API call to refresh the token
func (f *Fs) performTokenRefresh(ctx context.Context, refreshToken string) (*RefreshTokenResp, error) {
	// 使用统一客户端，无需单独初始化

	// Set up and make the refresh request
	refreshResp, err := f.callRefreshTokenAPI(ctx, refreshToken)
	if err != nil {
		return handleRefreshError(f, ctx, err)
	}

	// Validate the response
	if refreshResp.Data == nil || refreshResp.Data.AccessToken == "" {
		// Log detailed information about the empty response
		fs.Errorf(f, "❌ 刷新令牌响应为空或无效。完整响应: %#v", refreshResp)
		// Log OpenAPI base information (state, code, message)
		fs.Errorf(f, "❌ 响应状态: %v, 代码: %d, 消息: %q",
			refreshResp.State, refreshResp.Code, refreshResp.Message)

		// Check if this is a refresh_token invalid error (40140116)
		if refreshResp.Code == 40140116 {
			fs.Errorf(f, "❌ 刷新令牌已失效(40140116)，清空令牌信息并触发重新登录")

			// Clear all token information to force re-login
			f.tokenMu.Lock()
			f.clearTokenInfo()
			f.tokenMu.Unlock()

			// Return a specific error to trigger re-login
			return nil, NewTokenError("refresh token expired, need re-login", true)
		}

		fs.Errorf(f, "❌ 刷新令牌响应为空，尝试重新登录")

		// Re-lock before checking token again to avoid race condition
		f.tokenMu.Lock()
		// Only check if another thread refreshed if this wasn't a refresh_token invalid error
		if f.accessToken != "" && time.Now().Before(f.tokenExpiry) {
			fs.Debugf(f, "✅ 等待期间令牌已被其他线程刷新")
			f.tokenMu.Unlock()
			return nil, nil
		}
		f.tokenMu.Unlock()

		loginErr := f.login(ctx)
		if loginErr != nil {
			return nil, fmt.Errorf("re-login failed after empty refresh response: %w", loginErr)
		}
		fs.Debugf(f, "✅ 空刷新响应后重新登录成功")
		return nil, nil // Re-login successful, no need to update tokens
	}

	return refreshResp, nil
}

// CallAPI 统一API调用方法，支持OpenAPI和传统API
func (f *Fs) CallAPI(ctx context.Context, apiType APIType, opts *rest.Opts, request any, response any) error {
	// Fix null pointer issue: ensure client exists
	if err := f.ensureOpenAPIClient(ctx); err != nil {
		return fmt.Errorf("failed to ensure OpenAPI client: %w", err)
	}

	// 根据API类型动态配置
	switch apiType {
	case OpenAPI:
		if opts.RootURL == "" {
			opts.RootURL = openAPIRootURL
		}
		// 准备Token认证
		if err := f.prepareTokenForRequest(ctx, opts); err != nil {
			return err
		}
	case TraditionalAPI:
		if opts.RootURL == "" {
			opts.RootURL = traditionalRootURL
		}
		// 准备Cookie认证
		if err := f.prepareCookieForRequest(opts); err != nil {
			return err
		}
	}

	// 使用统一调速器和客户端
	return f.pacer.Call(func() (bool, error) {
		var resp *http.Response
		var err error

		if request != nil && response != nil {
			resp, err = f.openAPIClient.CallJSON(ctx, opts, request, response)
		} else if response != nil {
			resp, err = f.openAPIClient.CallJSON(ctx, opts, nil, response)
		} else {
			resp, err = f.openAPIClient.Call(ctx, opts)
		}

		return shouldRetry(ctx, resp, err)
	})
}

// prepareCookieForRequest 为传统API准备Cookie认证
func (f *Fs) prepareCookieForRequest(opts *rest.Opts) error {
	if f.opt.Cookie != "" {
		cred := (&Credential{}).FromCookie(f.opt.Cookie)
		if opts.ExtraHeaders == nil {
			opts.ExtraHeaders = make(map[string]string)
		}
		// 设置Cookie头
		for _, cookie := range cred.Cookie() {
			if opts.ExtraHeaders["Cookie"] == "" {
				opts.ExtraHeaders["Cookie"] = cookie.String()
			} else {
				opts.ExtraHeaders["Cookie"] += "; " + cookie.String()
			}
		}
	}
	return nil
}

// callRefreshTokenAPI makes the actual API call to refresh the token
func (f *Fs) callRefreshTokenAPI(ctx context.Context, refreshToken string) (*RefreshTokenResp, error) {
	refreshData := url.Values{
		"refresh_token": []string{refreshToken},
	}
	opts := rest.Opts{
		Method:       "POST",
		RootURL:      passportRootURL,
		Path:         "/open/refreshToken",
		Body:         strings.NewReader(refreshData.Encode()),
		ExtraHeaders: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	}

	var refreshResp RefreshTokenResp
	// Fix recursive calls: create a new rest client to completely avoid any token validation mechanism
	// because this method itself is refreshing the token and cannot trigger token validation again
	httpClient := getOpenAPIHTTPClient(ctx)
	if httpClient == nil {
		return nil, fmt.Errorf("failed to create HTTP client for token refresh")
	}
	tempClient := rest.NewClient(httpClient).SetErrorHandler(errorHandler)
	if tempClient == nil {
		return nil, fmt.Errorf("failed to create REST client for token refresh")
	}

	resp, err := tempClient.CallJSON(ctx, &opts, nil, &refreshResp)
	if err != nil {
		// Check if this is a 40140116 error (refresh_token invalid)
		if strings.Contains(err.Error(), "40140116") || strings.Contains(err.Error(), "no auth") {
			fs.Debugf(f, "🔐 检测到刷新令牌无效错误: %v", err)
			return nil, NewTokenError("refresh token invalid (40140116)", true)
		}
		return nil, err
	}

	// 检查HTTP状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("refresh token API returned status %d", resp.StatusCode)
	}

	// 检查响应中的错误码
	if !refreshResp.State && refreshResp.Code == 40140116 {
		fs.Debugf(f, "🔐 响应中检测到刷新令牌无效错误(40140116)")
		return nil, NewTokenError("refresh token invalid (40140116)", true)
	}

	// 响应已经通过CallJSON解析到refreshResp中
	fs.Debugf(f, "✅ 令牌刷新响应接收成功")

	return &refreshResp, nil
}

// ensureOpenAPIClient ensures the OpenAPI client is initialized
func (f *Fs) ensureOpenAPIClient(ctx context.Context) error {
	if f.openAPIClient != nil {
		return nil
	}

	// Setup client if it doesn't exist
	httpClient := fshttp.NewClient(ctx)
	f.openAPIClient = rest.NewClient(httpClient).SetErrorHandler(errorHandler)
	if f.openAPIClient == nil {
		return fmt.Errorf("failed to create OpenAPI client")
	}

	return nil
}

// handleRefreshError handles errors from the refresh token API call
// Completely rewritten based on reference code, remove recursive checks, rely on correct API calls to avoid recursion
func handleRefreshError(f *Fs, ctx context.Context, err error) (*RefreshTokenResp, error) {
	fs.Errorf(f, "❌ 刷新令牌失败: %v", err)

	// Check if the error indicates the refresh token itself is expired or invalid
	var tokenErr *TokenError
	if errors.As(err, &tokenErr) && tokenErr.IsRefreshTokenExpired ||
		strings.Contains(err.Error(), "refresh token expired") ||
		strings.Contains(err.Error(), "40140116") ||
		strings.Contains(err.Error(), "no auth") {

		fs.Debugf(f, "🔄 刷新令牌已失效，清空令牌信息并尝试完整重新登录")

		// Clear all token information to ensure clean state
		f.tokenMu.Lock()
		f.clearTokenInfo()
		f.tokenMu.Unlock()

		loginErr := f.login(ctx) // login handles its own locking
		if loginErr != nil {
			return nil, fmt.Errorf("re-login failed after refresh token expired: %w", loginErr)
		}
		fs.Debugf(f, "✅ 刷新令牌过期后重新登录成功")
		return nil, nil // Re-login successful
	}

	// Return the original refresh error if it wasn't an expiry issue
	return nil, fmt.Errorf("token refresh failed: %w", err)
}

// updateTokens updates the token values with new values from a refresh response
func (f *Fs) updateTokens(refreshResp *RefreshTokenResp) {
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
	fs.Debugf(f, "✅ 令牌刷新成功，新过期时间: %v", f.tokenExpiry)
}

// saveToken saves the current token to the config
func (f *Fs) saveToken(m configmap.Mapper) {
	if m == nil {
		fs.Debugf(f, "💾 不保存令牌 - 映射器为空")
		return
	}

	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()

	if f.accessToken == "" || f.refreshToken == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "💾 不保存令牌 - 令牌信息不完整")
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
		fs.Errorf(f, "❌ 保存令牌到配置失败: %v", err)
		return
	}

	fs.Debugf(f, "💾 使用原始名称%q保存令牌到配置文件", f.originalName)
}

// setupTokenRenewer initializes the token renewer to automatically refresh tokens
func (f *Fs) setupTokenRenewer(ctx context.Context, m configmap.Mapper) {
	// Only set up renewer if we have valid tokens
	if f.accessToken == "" || f.refreshToken == "" || f.tokenExpiry.IsZero() {
		fs.Debugf(f, "🔄 不设置令牌续期器 - 令牌信息不完整")
		return
	}

	// Create a renewal transaction function
	transaction := func() error {
		fs.Debugf(f, "🔄 令牌续期器触发，刷新令牌")
		// Use non-global function to avoid deadlocks
		err := f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			fs.Errorf(f, "❌ 续期器中刷新令牌失败: %v", err)
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
		fs.Logf(f, "❌ 为续期器保存令牌失败: %v", err)
		return
	}

	// Create a client with the token source
	_, ts, err := oauthutil.NewClientWithBaseClient(ctx, f.originalName, m, config, fshttp.NewClient(ctx))
	if err != nil {
		fs.Logf(f, "❌ 为续期器创建令牌源失败: %v", err)
		return
	}

	// Create token renewer that will trigger when the token is about to expire
	f.tokenRenewer = oauthutil.NewRenew(f.originalName, ts, transaction)
	f.tokenRenewer.Start() // Start the renewer immediately
	fs.Debugf(f, "🔄 令牌续期器已初始化并启动，使用原始名称%q", f.originalName)
}

// CallOpenAPI performs a call to the OpenAPI endpoint.
// It handles token refresh and sets the Authorization header.
// If skipToken is true, it skips adding the Authorization header (used for refresh itself).
func (f *Fs) CallOpenAPI(ctx context.Context, opts *rest.Opts, request any, response any, skipToken bool) error {
	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = openAPIRootURL
	}

	// Smart QPS management: select appropriate pacer based on API endpoint
	smartPacer := f.getPacerForEndpoint()
	fs.Debugf(f, "🎯 智能QPS选择: %s -> 使用智能调速器", opts.Path)

	// Wrap the entire attempt sequence with the smart pacer, returning proper retry signals
	return smartPacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {
		// Ensure token is available and current
		if !skipToken {
			if err := f.prepareTokenForRequest(ctx, opts); err != nil {
				fs.Debugf(f, "🔐 CallOpenAPI: 准备请求令牌失败: %v", err)
				return false, err
			}
		}

		// Make the API call
		resp, apiErr := f.executeOpenAPICall(ctx, opts, request, response)

		// Handle retries for network/server errors
		if retryNeeded, retryErr := shouldRetry(ctx, resp, apiErr); retryNeeded {
			fs.Debugf(f, "🔄 调速器: OpenAPI调用需要底层重试 (错误: %v)", retryErr)
			return true, retryErr // Signal globalPacer to retry
		}

		// Handle token errors
		if apiErr != nil {
			if retryAfterTokenRefresh, err := f.handleTokenError(ctx, opts, apiErr, skipToken); retryAfterTokenRefresh {
				return true, nil // Retry with refreshed token
			} else if err != nil {
				return false, err
			}
			return false, apiErr // Non-token error, don't retry
		}

		// Check for API-level errors in the response
		if apiErr = f.checkResponseForAPIErrors(response); apiErr != nil {
			if tokenRefreshed, err := f.handleTokenError(ctx, opts, apiErr, skipToken); tokenRefreshed {
				return true, nil // Retry with refreshed token
			} else if err != nil {
				return false, err
			}
			return false, apiErr // Other API error, don't retry
		}

		fs.Debugf(f, "✅ 调速器: OpenAPI调用成功")
		return false, nil // Success, don't retry
	})
}

// CallUploadAPI function specifically for upload-related API calls, using dedicated pacer
// Optimize upload API call frequency, balance performance and stability
func (f *Fs) CallUploadAPI(ctx context.Context, opts *rest.Opts, request any, response any, skipToken bool) error {
	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = openAPIRootURL
	}

	// Use unified pacer for all API calls
	return f.pacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {
		// Ensure token is available and current
		if !skipToken {
			if err := f.prepareTokenForRequest(ctx, opts); err != nil {
				fs.Debugf(f, "🔐 CallUploadAPI: 准备请求令牌失败: %v", err)
				return false, err
			}
		}

		// Make the actual API call
		fs.Debugf(f, "📤 CallUploadAPI: 执行API调用")
		err := f.CallAPI(ctx, OpenAPI, opts, request, response)

		// Check for API-level errors in the response
		if apiErr := f.checkResponseForAPIErrors(response); apiErr != nil {
			if tokenRefreshed, err := f.handleTokenError(ctx, opts, apiErr, skipToken); tokenRefreshed {
				return true, nil // Retry with refreshed token
			} else if err != nil {
				return false, err
			}
			return false, apiErr // Other API error, don't retry
		}

		fs.Debugf(f, "✅ CallUploadAPI: API调用成功")
		return shouldRetry(ctx, nil, err)
	})
}

// CallDownloadURLAPI function specifically for download URL API calls, using dedicated pacer
// 修复文件级并发：通过MaxConnections支持并发下载URL获取
func (f *Fs) CallDownloadURLAPI(ctx context.Context, opts *rest.Opts, request any, response any, skipToken bool) error {
	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = openAPIRootURL
	}

	// 使用统一调速器
	return f.pacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {
		// Ensure token is available and current
		if !skipToken {
			if err := f.prepareTokenForRequest(ctx, opts); err != nil {
				fs.Debugf(f, "🔐 CallDownloadURLAPI: 准备请求令牌失败: %v", err)
				return false, err
			}
		}

		// Make the actual API call
		fs.Debugf(f, "📥 CallDownloadURLAPI: 执行API调用")
		err := f.CallAPI(ctx, OpenAPI, opts, request, response)

		// Check for API-level errors in the response
		if apiErr := f.checkResponseForAPIErrors(response); apiErr != nil {
			if tokenRefreshed, err := f.handleTokenError(ctx, opts, apiErr, skipToken); tokenRefreshed {
				return true, nil // Retry with refreshed token
			} else if err != nil {
				return false, err
			}
			return false, apiErr // Other API error, don't retry
		}

		fs.Debugf(f, "✅ CallDownloadURLAPI: API调用成功")
		return shouldRetry(ctx, nil, err)
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
	// Fix recursive calls: avoid infinite recursion caused by triggering token refresh in API calls
	if needsRefresh {
		// 检查是否已经在token刷新过程中，避免递归调用
		if f.isRefreshing.Load() {
			fs.Debugf(f, "🔄 令牌刷新已在进行中，使用当前令牌")
			f.tokenMu.Lock()
			currentToken = f.accessToken
			f.tokenMu.Unlock()
		} else {
			refreshErr := f.refreshTokenIfNecessary(ctx, false, false)
			if refreshErr != nil {
				fs.Debugf(f, "❌ 令牌刷新失败: %v", refreshErr)
				return fmt.Errorf("token refresh failed: %w", refreshErr)
			}

			// Get the refreshed token
			f.tokenMu.Lock()
			currentToken = f.accessToken
			f.tokenMu.Unlock()
		}
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
	// Fix circular calls: use openAPIClient directly, not through CallAPI
	// 确保客户端存在
	if err := f.ensureOpenAPIClient(ctx); err != nil {
		return nil, fmt.Errorf("failed to ensure OpenAPI client: %w", err)
	}

	var resp *http.Response
	var err error

	if request != nil && response != nil {
		// Assume standard JSON request/response
		resp, err = f.openAPIClient.CallJSON(ctx, opts, request, response)
	} else if response != nil {
		// Assume GET request with JSON response
		resp, err = f.openAPIClient.CallJSON(ctx, opts, nil, response)
	} else {
		// Assume call without specific request/response body
		var baseResp OpenAPIBase
		resp, err = f.openAPIClient.CallJSON(ctx, opts, nil, &baseResp)
		if err == nil {
			err = baseResp.Err() // Check for API-level errors
		}
	}

	if err != nil {
		fs.Debugf(f, "❌ executeOpenAPICall失败: %v", err)
	}

	return resp, err
}

// handleTokenError processes token-related errors and attempts to refresh or re-login
func (f *Fs) handleTokenError(ctx context.Context, opts *rest.Opts, apiErr error, skipToken bool) (bool, error) {
	var tokenErr *TokenError
	if errors.As(apiErr, &tokenErr) {
		fs.Debugf(f, "🔐 检测到令牌错误: %v (需要重新登录: %v)", tokenErr, tokenErr.IsRefreshTokenExpired)

		// Check if we're already in a refresh cycle to prevent infinite loops
		if f.isRefreshing.Load() {
			fs.Debugf(f, "⚠️ 令牌刷新已在进行中，避免递归调用")
			return false, fmt.Errorf("token refresh already in progress, avoiding recursion")
		}

		// Handle token refresh/re-login using refreshTokenIfNecessary
		refreshErr := f.refreshTokenIfNecessary(ctx, tokenErr.IsRefreshTokenExpired, !tokenErr.IsRefreshTokenExpired)
		if refreshErr != nil {
			fs.Debugf(f, "❌ 令牌刷新/重新登录失败: %v", refreshErr)
			return false, fmt.Errorf("token refresh/relogin failed: %w (original: %v)", refreshErr, apiErr)
		}

		// Token was successfully refreshed or re-login succeeded, retry the API call
		fs.Debugf(f, "✅ 令牌刷新/重新登录成功，重试API调用")

		// Update the Authorization header with the new token
		if !skipToken {
			// Always get the freshest token right before using it
			f.tokenMu.Lock()
			token := f.accessToken
			f.tokenMu.Unlock()

			// Validate we have a valid token
			if token == "" {
				fs.Errorf(f, "❌ 刷新后仍然没有有效的访问令牌")
				return false, fmt.Errorf("no valid access token after refresh")
			}

			if opts.ExtraHeaders == nil {
				opts.ExtraHeaders = make(map[string]string)
			}
			opts.ExtraHeaders["Authorization"] = "Bearer " + token
			fs.Debugf(f, "🔑 已更新Authorization头部，令牌长度: %d", len(token))
		}
		return true, nil // Signal retry with the refreshed token
	}
	return false, nil // Not a token error
}

// checkResponseForAPIErrors examines the response for API-level errors using reflection
func (f *Fs) checkResponseForAPIErrors(response any) error {
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
					fs.Debugf(f, "🔄 响应中检测到速率限制错误: %v", err)
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

	// Use unified pacer for all API calls
	return f.pacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {

		// Make the API call (with or without encryption)
		resp, apiErr := f.executeTraditionalAPICall(ctx, opts, request, response, skipEncrypt)

		// Process the result
		return f.processTraditionalAPIResult(ctx, resp, apiErr)
	})
}

// 移除enforceTraditionalPacerDelay函数，使用统一pacer不需要额外延迟

// executeTraditionalAPICall makes the actual API call with proper handling of encryption
func (f *Fs) executeTraditionalAPICall(ctx context.Context, opts *rest.Opts, request any, response any, skipEncrypt bool) (*http.Response, error) {
	if skipEncrypt {
		return f.executeUnencryptedCall(ctx, opts, request, response)
	}
	return f.executeEncryptedCall(ctx, opts, request, response)
}

// executeUnencryptedCall makes a traditional API call without encryption
func (f *Fs) executeUnencryptedCall(ctx context.Context, opts *rest.Opts, request any, response any) (*http.Response, error) {
	var resp *http.Response
	// 使用统一API调用方法
	apiErr := f.CallAPI(ctx, TraditionalAPI, opts, request, response)

	return resp, apiErr
}

// executeEncryptedCall makes a traditional API call (encryption removed)
// 重构：移除加密功能，直接使用标准JSON调用
func (f *Fs) executeEncryptedCall(ctx context.Context, opts *rest.Opts, request any, response any) (*http.Response, error) {
	var resp *http.Response
	// 重构：使用统一API调用方法
	apiErr := f.CallAPI(ctx, TraditionalAPI, opts, request, response)

	return resp, apiErr
}

// processTraditionalAPIResult handles the result of a traditional API call
func (f *Fs) processTraditionalAPIResult(ctx context.Context, resp *http.Response, apiErr error) (bool, error) {
	// Check for retryable errors
	retryNeeded, retryErr := shouldRetry(ctx, resp, apiErr)
	if retryNeeded {
		fs.Debugf(f, "🔄 调速器: 传统调用需要底层重试 (错误: %v)", retryErr)
		return true, retryErr
	}

	// Handle non-retryable errors
	if apiErr != nil {
		fs.Debugf(f, "❌ 调速器: 传统调用遇到永久错误: %v", apiErr)
		return false, apiErr
	}

	// Success
	fs.Debugf(f, "✅ 调速器: 传统调用成功")
	return false, nil
}

// initializeOptions115 初始化和验证115网盘配置选项
func initializeOptions115(name, root string, m configmap.Mapper) (*Options, string, string, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, "", "", err
	}

	// Validate incompatible options
	if opt.FastUpload && opt.OnlyStream {
		return nil, "", "", errors.New("fast_upload and only_stream cannot be set simultaneously")
	}
	if opt.FastUpload && opt.UploadHashOnly {
		return nil, "", "", errors.New("fast_upload and upload_hash_only cannot be set simultaneously")
	}
	if opt.OnlyStream && opt.UploadHashOnly {
		return nil, "", "", errors.New("only_stream and upload_hash_only cannot be set simultaneously")
	}

	if opt.NohashSize > StreamUploadLimit {
		fs.Logf(name, "⚠️ nohash_size (%v) 减少到流式上传限制 (%v)", opt.NohashSize, StreamUploadLimit)
		opt.NohashSize = StreamUploadLimit
	}

	// 设置API限流控制的默认值
	if opt.QPSLimit == 0 {
		opt.QPSLimit = unifiedMinSleep // 默认使用200ms间隔 (约5 QPS)
	}

	// 其他增强功能已设置为合理的默认值，无需用户配置

	// Store the original name before any modifications for config operations
	// Extract the base name without the config override suffix {xxxx}
	originalName := name
	if idx := strings.IndexRune(name, '{'); idx > 0 {
		originalName = name[:idx]
	}
	root = strings.Trim(root, "/")

	return opt, originalName, root, nil
}

// createBasicFs115 创建基础115网盘文件系统对象
func createBasicFs115(name, originalName, root string, opt *Options, m configmap.Mapper) *Fs {
	return &Fs{
		name:            name,
		originalName:    originalName,
		root:            root,
		opt:             *opt,
		m:               m,
		activeTransfers: make(map[string]*TransferSpeedMonitor),
		listAllCache:    cache.New().SetExpireDuration(30 * time.Minute), // 🚀 初始化listAll结果缓存，30分钟过期时间
	}
}

// newFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// 初始化和验证配置选项
	opt, originalName, normalizedRoot, err := initializeOptions115(name, root, m)
	if err != nil {
		return nil, err
	}

	// 创建基础文件系统对象
	f := createBasicFs115(name, originalName, normalizedRoot, opt, m)

	// Fix null pointer issue: initialize client immediately, following standard practice of other backends
	httpClient := fshttp.NewClient(ctx)
	f.openAPIClient = rest.NewClient(httpClient).SetErrorHandler(errorHandler)
	if f.openAPIClient == nil {
		return nil, fmt.Errorf("failed to create OpenAPI REST client")
	}
	fs.Debugf(f, "✅ OpenAPI REST客户端初始化成功")

	// 初始化功能特性 - 使用标准Fill模式
	f.features = (&fs.Features{
		DuplicateFiles:          false,
		CanHaveEmptyDirectories: true,
		NoMultiThreading:        true, // 115网盘使用单线程上传
	}).Fill(ctx, f)

	// Setting appVer (needed for traditional calls)
	re := regexp.MustCompile(`\d+\.\d+\.\d+(\.\d+)?$`)
	if m := re.FindStringSubmatch(tradUserAgent); m == nil {
		fs.Logf(f, "⚠️ 无法从User-Agent %q 解析应用版本。使用默认值。", tradUserAgent)
		f.appVer = "27.0.7.5" // Default fallback
	} else {
		f.appVer = m[0]
		fs.Debugf(f, "🌐 从User-Agent %q 使用应用版本 %q", tradUserAgent, f.appVer)
	}

	// Initialize single pacer，使用用户配置的QPS限制
	// 115网盘所有API都受到相同的全局QPS限制，使用单一pacer符合rclone标准模式
	f.pacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(time.Duration(opt.QPSLimit)),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// 初始化增强功能（默认启用）
	qpsValue := float64(time.Second) / float64(opt.QPSLimit)
	fs.Debugf(f, "🚦 API限流控制: 间隔=%v (约%.1f QPS), 最大重试=%d, 基础延迟=%v",
		opt.QPSLimit, qpsValue, defaultMaxRetryAttempts, defaultRetryBaseDelay)
	fs.Debugf(f, "📊 传输速度监控已启用: 慢速阈值=%v/s, 检查间隔=%v, 处理动作=%s",
		defaultSlowSpeedThreshold, defaultSpeedCheckInterval, defaultSlowSpeedAction)
	fs.Debugf(f, "⏰ 分片超时控制已启用: 超时时间=%v", defaultChunkTimeout)

	// Check if we have saved token in config file
	tokenLoaded := loadTokenFromConfig(f, m)
	fs.Debugf(f, "💾 从配置加载令牌: %v (过期时间 %v)", tokenLoaded, f.tokenExpiry)

	var tokenRefreshNeeded bool
	if tokenLoaded {
		// Check if token is expired or will expire soon
		if time.Now().After(f.tokenExpiry.Add(-tokenRefreshWindow)) {
			fs.Debugf(f, "⏰ 令牌已过期或即将过期，立即刷新")
			tokenRefreshNeeded = true
		}
	} else if f.opt.Cookie != "" {
		// No token but have cookie, so login
		fs.Debugf(f, "🔐 未找到令牌但提供了Cookie，尝试登录")
		err = f.login(ctx)
		if err != nil {
			return nil, fmt.Errorf("initial login failed: %w", err)
		}
		// Save token to config after successful login
		f.saveToken(m)
		fs.Debugf(f, "✅ 登录成功，令牌已保存")
	} else {
		return nil, errors.New("no valid cookie or token found, please configure cookie")
	}

	// Try to refresh the token if needed
	if tokenRefreshNeeded {
		err = f.refreshTokenIfNecessary(ctx, false, true)
		if err != nil {
			fs.Debugf(f, "❌ 令牌刷新失败，尝试完整登录: %v", err)
			// If refresh fails, try full login
			err = f.login(ctx)
			if err != nil {
				return nil, fmt.Errorf("login failed after token refresh failure: %w", err)
			}
		}
		// Save the refreshed/new token
		f.saveToken(m)
		fs.Debugf(f, "✅ 令牌刷新/登录成功，令牌已保存")
	}

	// Setup token renewer for automatic refresh
	fs.Debugf(f, "🔄 设置令牌续期器")
	f.setupTokenRenewer(ctx, m)

	// Set the root folder ID based on config
	if f.opt.RootFolderID != "" {
		// Use configured root folder ID
		f.rootFolderID = f.opt.RootFolderID
	} else {
		// Use default root folder ID
		f.rootFolderID = "0"
	}

	// Initialize directory cache with persistent support
	configData := map[string]string{
		"root_folder_id": f.rootFolderID,
		"endpoint":       openAPIRootURL,
	}
	f.dirCache = dircache.NewWithPersistent(f.root, f.rootFolderID, f, "115", configData)

	// 💾 初始化持久化缓存系统
	f.initPersistentCache115()

	// 💾 加载持久化缓存
	f.loadPersistentCaches115()

	// 🔧 优化的rclone模式：结合标准模式和115网盘特性
	// Find the current root
	err = f.dirCache.FindRoot(ctx, false)

	if err != nil {
		// Assume it is a file (标准rclone模式)
		newRoot, remote := dircache.SplitPath(f.root)
		// 修复：避免锁复制，创建新的Fs实例而不是复制
		tempF := &Fs{
			name:          f.name,
			originalName:  f.originalName,
			root:          newRoot,
			opt:           f.opt,
			features:      f.features,
			tradClient:    f.tradClient,
			openAPIClient: f.openAPIClient,
			pacer:         f.pacer,
			rootFolder:    f.rootFolder,
			rootFolderID:  f.rootFolderID,
			appVer:        f.appVer,
			userID:        f.userID,
			userkey:       f.userkey,
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
	// 🔧 115网盘特殊处理：即使FindRoot成功，也要检查是否是文件路径
	// 这是115网盘的特殊需求，因为115网盘的目录结构特殊
	if strings.Contains(f.root, ".") && !strings.HasSuffix(f.root, ".") {
		newRoot, remote := dircache.SplitPath(f.root)

		// 修复：保存原始的根目录ID，避免在创建新dirCache时丢失
		originalRootID, err := f.dirCache.RootID(ctx, false)
		if err != nil {
			fs.Debugf(f, "⚠️ 无法获取原始根目录ID，使用默认: %v", err)
			originalRootID = f.rootFolderID
		}
		fs.Debugf(f, "🔧 文件路径检测: newRoot=%s, remote=%s, originalRootID=%s", newRoot, remote, originalRootID)

		// 修复：避免锁复制，创建新的Fs实例而不是复制
		tempF := &Fs{
			name:          f.name,
			originalName:  f.originalName,
			root:          newRoot,
			opt:           f.opt,
			features:      f.features,
			tradClient:    f.tradClient,
			openAPIClient: f.openAPIClient,
			pacer:         f.pacer,
			rootFolder:    f.rootFolder,
			rootFolderID:  f.rootFolderID,
			appVer:        f.appVer,
			userID:        f.userID,
			userkey:       f.userkey,
		}
		tempF.dirCache = dircache.New(newRoot, originalRootID, tempF)
		// Make new Fs which is the parent
		err = tempF.dirCache.FindRoot(ctx, false)
		if err != nil {
			// No root so return old f
			return f, nil
		}
		_, err = tempF.NewObject(ctx, remote)
		if err != nil {
			// File doesn't exist, return original f as directory
			return f, nil
		}
		// File exists, return as file
		f.dirCache = tempF.dirCache
		f.root = tempF.root
		return f, fs.ErrorIsFile
	}

	return f, nil
}

// PathInfo 115网盘路径信息结构
type PathInfo struct {
	FileCategory string `json:"file_category"` // "1"=文件, "0"=文件夹
	FileName     string `json:"file_name"`
	FileID       string `json:"file_id"`
	Size         string `json:"size"`
	SizeByte     int64  `json:"size_byte"`
}

// PathInfoResponse 115网盘路径信息API响应
// 🔧 修复：API可能返回数组或对象，需要灵活处理
type PathInfoResponse struct {
	State   bool        `json:"state"`
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data"` // 使用interface{}来处理数组或对象
}

// isFromRemoteSource 判断输入流是否来自远程源（用于跨云传输检测）
func (f *Fs) isFromRemoteSource(in io.Reader) bool {
	if ro, ok := in.(*RereadableObject); ok {
		fs.Debugf(f, "🔍 检测到RereadableObject，可能是跨云传输: %v", ro)
		return true
	}
	readerType := fmt.Sprintf("%T", in)
	if strings.Contains(readerType, "Accounted") || strings.Contains(readerType, "Cross") {
		fs.Debugf(f, "🔍 检测到特殊Reader类型，可能是跨云传输: %s", readerType)
		return true
	}

	return false
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
	if f.fileObj != nil { // Handle case where Fs points to a single file
		obj := *f.fileObj
		if obj.Remote() == remote || obj.Remote() == "isFile:"+remote {
			fs.Debugf(f, "✅ NewObject: 单文件匹配成功")
			return obj, nil
		}
		fs.Debugf(f, "❌ NewObject: 单文件不匹配，返回NotFound")
		return nil, fs.ErrorObjectNotFound // If remote doesn't match the single file
	}

	result, err := f.newObjectWithInfo(ctx, remote, nil)
	if err != nil {
		fs.Debugf(f, "❌ NewObject失败: %v", err)
	}
	return result, err
}

// FindLeaf finds a directory or file leaf in the parent folder pathID.
// Used by dircache.
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (foundID string, found bool, err error) {
	// 🔧 性能优化：首先检查dirCache是否已有结果
	parentPath, ok := f.dirCache.GetInv(pathID)
	if ok {
		var itemPath string
		if parentPath == "" {
			itemPath = leaf
		} else {
			itemPath = path.Join(parentPath, leaf)
		}
		// 检查是否已缓存
		if cachedID, found := f.dirCache.Get(itemPath); found {
			fs.Debugf(f, "🎯 FindLeaf缓存命中: %s -> ID=%s", leaf, cachedID)
			return cachedID, true, nil
		}
	}

	// 🔧 优化：使用常量定义的limit，最大化性能
	listChunk := defaultListChunkSize // 115网盘OpenAPI的最大限制

	// 🔧 性能优化：批量缓存机制，类似123网盘的实现
	parentPath, parentPathOk := f.dirCache.GetInv(pathID)

	found, err = f.listAll(ctx, pathID, listChunk, false, func(item *File) bool {
		// Compare with decoded name to handle special characters correctly
		decodedName := f.opt.Enc.ToStandardName(item.FileNameBest())

		// 🔧 批量缓存：缓存所有找到的目录项
		if item.IsDir() && parentPathOk {
			var itemPath string
			if parentPath == "" {
				itemPath = decodedName
			} else {
				itemPath = path.Join(parentPath, decodedName)
			}
			f.dirCache.Put(itemPath, item.ID())
			fs.Debugf(f, "📦 批量缓存: %s -> ID=%s", decodedName, item.ID())
		}

		if decodedName == leaf {
			// 检查找到的项目是否为目录
			if item.IsDir() {
				// 这是目录，返回目录ID
				foundID = item.ID()
				fs.Debugf(f, "✅ FindLeaf找到目标: %s -> ID=%s, Type=目录", leaf, foundID)
			} else {
				foundID = "" // 明确设置为空，表示找到的是文件而不是目录
				fs.Debugf(f, "✅ FindLeaf找到目标: %s -> Type=文件", leaf)
			}
			return true // Stop searching
		}
		return false // Continue searching
	})

	// 重构：如果遇到API限制错误，清理dirCache而不是pathCache
	if err != nil && (strings.Contains(err.Error(), "770004") || strings.Contains(err.Error(), "已达到当前访问上限")) {
		f.dirCache.Flush()
	}

	return foundID, found, err
}

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fs.Debugf(f, "📋 List调用，目录: %q", dir)

	// 🔧 处理空目录路径：当dir为空时，使用dirCache的根目录ID
	var dirID string
	if dir == "" {
		// 使用dirCache的根目录ID，这是当前Fs对象所代表的目录
		// 修复：如果根目录不存在，尝试创建它
		dirID, err = f.dirCache.RootID(ctx, true) // create = true
		if err != nil {
			fs.Debugf(f, "❌ List获取根目录ID失败: %v", err)
			// 如果仍然失败，尝试刷新缓存后重试
			f.dirCache.Flush()
			dirID, err = f.dirCache.RootID(ctx, true)
			if err != nil {
				fs.Debugf(f, "❌ List刷新缓存后仍然失败: %v", err)
				return nil, err
			}
		}
		fs.Debugf(f, "✅ List使用当前根目录ID: %s", dirID)
	} else {
		// 修复：如果目录不存在，尝试创建它
		dirID, err = f.dirCache.FindDir(ctx, dir, true) // create = true
		if err != nil {
			// If it's an API limit error, log details and return
			if fserrors.IsRetryError(err) {
				fs.Debugf(f, "🔄 List遇到API限制错误，目录: %q, 错误: %v", dir, err)
				return nil, err
			}
			fs.Debugf(f, "❌ List查找目录失败，目录: %q, 错误: %v", dir, err)
			// 尝试刷新缓存后重试
			f.dirCache.FlushDir(dir)
			dirID, err = f.dirCache.FindDir(ctx, dir, true)
			if err != nil {
				fs.Debugf(f, "❌ List刷新缓存后仍然失败，目录: %q, 错误: %v", dir, err)
				return nil, err
			}
		}
		fs.Debugf(f, "✅ List找到目录ID: %s，目录: %q", dirID, dir)
	}

	// Call API to get directory listing
	var fileList []File
	var iErr error

	// 🔧 优化：设置固定的limit为1150，最大化性能
	listChunk := defaultListChunkSize // 115网盘OpenAPI的最大限制
	_, err = f.listAll(ctx, dirID, listChunk, false, func(item *File) bool {
		// 保存到临时列表用于缓存
		fileList = append(fileList, *item)

		// 构建entries
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
	// 🔧 优化文件存在性检查：减少重复的NewObject调用
	// 对于上传操作，直接使用PutUnchecked，让115网盘服务器处理重复文件检查
	// 这样可以避免不必要的API调用，提高上传效率

	fs.Debugf(f, "📤 优化上传模式：跳过客户端文件存在性检查，减少API调用")
	return f.PutUnchecked(ctx, in, src, options...)
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

	// Handle cross-cloud transfers
	if f.isRemoteSource(src) {
		fs.Debugf(f, "☁️ 检测到跨云传输: %s -> 115 (大小: %s)", src.Fs().Name(), fs.SizeSuffix(src.Size()))

		// 检查是否启用流式哈希模式
		if f.opt.StreamHashMode {
			fs.Infof(f, "🌊 启用流式哈希模式: %s (%s)", remote, fs.SizeSuffix(src.Size()))
			// 🔧 关键修复：使用传递的Reader（已被rclone Transfer包装），确保进度正确显示
			return f.streamHashTransfer115WithReader(ctx, in, src, remote, options...)
		} else {
			return f.crossCloudUploadWithLocalCache(ctx, in, src, remote, options...)
		}
	}

	// Call the main upload function
	newObj, err := f.upload(ctx, in, src, remote, options...)
	if err != nil {
		return nil, err
	}
	if newObj == nil {
		return nil, errors.New("internal error: upload returned nil object without error")
	}

	o := newObj.(*Object)

	// Post-upload metadata check
	if !f.opt.NoCheck && !o.hasMetaData {
		fs.Debugf(o, "🔍 运行上传后元数据检查")
		err = o.readMetaData(ctx)
		if err != nil {
			fs.Logf(o, "❌ 上传后检查失败: %v", err)
			o.hasMetaData = true // Mark as having metadata to avoid repeated checks
		}
	}
	f.dirCache.FlushDir(path.Dir(remote))

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
		fs.Debugf(srcDir, "🔄 合并内容到 %v", dstDir)

		// List all items in the source directory
		var itemsToMove []*File
		// 🔧 优化：设置固定的limit为1150，最大化性能
		listChunk := defaultListChunkSize // 115网盘OpenAPI的最大限制
		_, err = f.listAll(ctx, srcDirID, listChunk, false, func(item *File) bool {
			itemsToMove = append(itemsToMove, item)
			return false // Collect all items
		})
		if err != nil {
			return fmt.Errorf("MergeDirs list failed on %v: %w", srcDir, err)
		}

		// Move items in chunks
		chunkSize := listChunk // Use list chunk size for move chunks
		for i := 0; i < len(itemsToMove); i += chunkSize {
			end := min(i+chunkSize, len(itemsToMove))
			chunk := itemsToMove[i:end]
			if len(chunk) == 0 {
				continue
			}

			var idsToMove []string
			for _, item := range chunk {
				idsToMove = append(idsToMove, item.ID())
			}

			fs.Debugf(srcDir, "📦 移动 %d 个项目到 %v", len(idsToMove), dstDir)
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
	chunkSize := fs.GetConfig(ctx).Checkers
	if chunkSize <= 0 {
		chunkSize = 1150 // 115网盘OpenAPI的最大限制
	}
	for i := 0; i < len(dirsToDelete); i += chunkSize {
		end := min(i+chunkSize, len(dirsToDelete))
		chunkIDs := dirsToDelete[i:end]
		if len(chunkIDs) == 0 {
			continue
		}

		fs.Debugf(f, "🗑️ 删除已合并的源目录: %v", chunkIDs)
		if err = f.deleteFiles(ctx, chunkIDs); err != nil {
			// Log error but continue trying to delete others
			fs.Errorf(f, "❌ MergeDirs删除分片目录失败 %v: %v", chunkIDs, err)
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
		// 重构：创建目录成功后，清理dirCache相关缓存
		// 新目录创建可能影响父目录的缓存
		f.dirCache.FlushDir(path.Dir(dir))
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
		fs.Debugf(src, "❌ 无法移动 - 不是相同的远程类型")
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

	// Fix: 正确分割路径，避免将文件名当作目录创建
	dstDirectory, dstLeaf := dircache.SplitPath(remote)
	dstParentID, err := f.dirCache.FindDir(ctx, dstDirectory, true)
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
		fs.Debugf(srcObj, "📦 移动 %q 从 %q 到 %q", srcObj.id, srcParentID, dstParentID)
		err = f.moveFiles(ctx, []string{srcObj.id}, dstParentID)
		if err != nil {
			return nil, fmt.Errorf("server-side move failed: %w", err)
		}
	}

	// Perform rename if names differ
	if srcLeaf != dstLeaf {
		fs.Debugf(srcObj, "📝 重命名 %q 为 %q", srcLeaf, dstLeaf)
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

	// 重构：移动成功后，清理dirCache相关缓存
	// 移动操作影响源和目标目录，清理相关缓存
	f.dirCache.FlushDir(path.Dir(srcObj.remote))
	f.dirCache.FlushDir(path.Dir(remote))

	return dstObj, nil
}

// DirMove server-side moves a directory.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	if f.isShare {
		return fs.ErrorCantDirMove
	}
	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(srcFs, "❌ 无法移动目录 - 不是相同的远程类型")
		return fs.ErrorCantDirMove
	}

	// Use dircache helper to prepare for move
	srcID, srcParentID, srcLeaf, dstParentID, dstLeaf, err := f.dirCache.DirMove(ctx, srcFs.dirCache, srcFs.root, srcRemote, f.root, dstRemote)
	if err != nil {
		return err // Errors like DirExists, CantMoveRoot handled by DirMove helper
	}

	// Perform the move if parents differ
	if srcParentID != dstParentID {
		fs.Debugf(srcFs, "📁 移动目录 %q 从 %q 到 %q", srcID, srcParentID, dstParentID)
		err = f.moveFiles(ctx, []string{srcID}, dstParentID)
		if err != nil {
			return fmt.Errorf("server-side directory move failed: %w", err)
		}
	}

	// Perform rename if names differ
	if srcLeaf != dstLeaf {
		fs.Debugf(srcFs, "📝 重命名目录 %q 为 %q", srcLeaf, dstLeaf)
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
		fs.Debugf(src, "❌ 无法复制 - 不是相同的远程类型")
		return nil, fs.ErrorCantCopy
	}

	// Special handling for copying *from* a shared remote (using traditional API)
	if srcObj.fs.isShare {
		return nil, errors.New("copying from shared remotes is not supported")
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

	// Fix: 正确分割路径，避免将文件名当作目录创建
	dstDirectory, dstLeaf := dircache.SplitPath(remote)
	dstParentID, err := f.dirCache.FindDir(ctx, dstDirectory, true)
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
		fs.Debugf(src, "❌ 无法复制 - 源目录和目标目录相同 (%q)", srcParentID)
		return nil, fs.ErrorCantCopy
	}

	// Perform the copy using OpenAPI
	fs.Debugf(srcObj, "📋 复制 %q 到 %q", srcObj.id, dstParentID)
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
		fs.Debugf(newObj, "📝 重命名复制的对象为 %q", dstLeaf)
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
		found, listErr := f.listAll(ctx, dirID, 1, false, func(item *File) bool {
			fs.Debugf(f, "🔍 Rmdir检查: 目录 %q 包含 %q", dir, item.FileNameBest())
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
	fs.Debugf(f, "🗑️ 清空目录 %q (ID: %q)", dir, dirID)
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

	// 移除BadgerDB缓存关闭，不再使用自定义缓存

	return nil
}

// itemToDirEntry converts an File to an fs.DirEntry
func (f *Fs) itemToDirEntry(ctx context.Context, remote string, item *File) (entry fs.DirEntry, err error) {
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

// newObjectWithInfo creates an fs.Object from an File or by reading metadata.
func (f *Fs) newObjectWithInfo(ctx context.Context, remote string, info *File) (fs.Object, error) {

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
		fs.Debugf(f, " newObjectWithInfo失败: %v", err)
		return nil, err
	}
	return o, nil
}

// readMetaDataForPath finds metadata for a specific file path.
func (f *Fs) readMetaDataForPath(ctx context.Context, path string) (info *File, err error) {
	fs.Debugf(f, " readMetaDataForPath开始: path=%q", path)

	leaf, dirID, err := f.dirCache.FindPath(ctx, path, false)
	if err != nil {
		fs.Debugf(f, " readMetaDataForPath: FindPath失败: %v", err)
		// 检查是否是API限制错误，如果是则立即返回，避免路径混乱
		if strings.Contains(err.Error(), "770004") || strings.Contains(err.Error(), "已达到当前访问上限") {
			fs.Debugf(f, "readMetaDataForPath遇到API限制错误，路径: %q, 错误: %v", path, err)
			// 重构：清理dirCache而不是pathCache
			f.dirCache.Flush()
			return nil, err
		}
		if err == fs.ErrorDirNotFound {
			// 修复：在跨云传输时，目录可能不存在，这是正常情况
			fs.Debugf(f, " readMetaDataForPath: 目录不存在，路径: %q", path)
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}

	fs.Debugf(f, " readMetaDataForPath: FindPath成功, leaf=%q, dirID=%q", leaf, dirID)

	// 🔧 处理空目录ID：当dirID为空时，使用当前Fs的rootFolderID
	if dirID == "" {
		dirID = f.rootFolderID
		fs.Debugf(f, "🔧 readMetaDataForPath: 使用根目录ID: %s, 路径: %q, 叶节点: %q", dirID, path, leaf)
	}

	// List the directory and find the leaf
	fs.Debugf(f, "readMetaDataForPath: starting listAll call, dirID=%q, leaf=%q", dirID, leaf)
	// 🔧 优化：使用常量定义的limit，最大化性能
	listChunk := defaultListChunkSize // 115网盘OpenAPI的最大限制
	found, err := f.listAll(ctx, dirID, listChunk, true, func(item *File) bool {
		// Compare with decoded name to handle special characters correctly
		decodedName := f.opt.Enc.ToStandardName(item.FileNameBest())
		if decodedName == leaf {
			info = item
			return true // Found it
		}
		return false // Keep looking
	})
	if err != nil {
		fs.Debugf(f, " readMetaDataForPath: listAll失败: %v", err)
		return nil, fmt.Errorf("failed to list directory %q to find %q: %w", dirID, leaf, err)
	}
	if !found {
		return nil, fs.ErrorObjectNotFound
	}
	return info, nil
}

// getFileInfoByID 通过文件ID直接获取文件信息（避免listAll的低效查询）
// 使用115网盘的 /open/folder/get_info API
func (f *Fs) getFileInfoByID(ctx context.Context, fileID string) (*File, error) {
	startTime := time.Now()
	fs.Debugf(f, "🚀 getFileInfoByID: 使用直接API获取文件信息, fileID=%s", fileID)

	if fileID == "" {
		return nil, errors.New("文件ID不能为空")
	}

	// 🚀 性能优化：检查缓存（如果启用了listAllCache）
	if f.listAllCache != nil {
		cacheKey := fmt.Sprintf("fileinfo_%s", fileID)
		if cached, found := f.listAllCache.GetMaybe(cacheKey); found {
			if fileInfo, ok := cached.(*File); ok {
				fs.Debugf(f, "🎯 getFileInfoByID缓存命中: fileID=%s, 耗时=%v", fileID, time.Since(startTime))
				return fileInfo, nil
			}
		}
	}

	// 使用115网盘的直接文件信息API
	opts := rest.Opts{
		Method: "GET",
		Path:   "/open/folder/get_info",
		Parameters: url.Values{
			"file_id": {fileID},
		},
	}

	var response struct {
		State int    `json:"state"`
		Error string `json:"error,omitempty"`
		Data  *File  `json:"data,omitempty"`
	}

	err := f.CallOpenAPI(ctx, &opts, nil, &response, false)
	duration := time.Since(startTime)

	if err != nil {
		fs.Debugf(f, "❌ getFileInfoByID API调用失败: %v, 耗时=%v", err, duration)
		return nil, fmt.Errorf("failed to get file info by ID %s: %w", fileID, err)
	}

	if response.State != 1 {
		fs.Debugf(f, "❌ getFileInfoByID API返回错误: state=%d, error=%s, 耗时=%v", response.State, response.Error, duration)
		return nil, fmt.Errorf("API error for file ID %s: state=%d, error=%s", fileID, response.State, response.Error)
	}

	if response.Data == nil {
		fs.Debugf(f, "❌ getFileInfoByID API返回空数据: fileID=%s, 耗时=%v", fileID, duration)
		return nil, fmt.Errorf("no data returned for file ID %s", fileID)
	}

	// 🚀 性能优化：缓存结果（5分钟有效期）
	if f.listAllCache != nil {
		cacheKey := fmt.Sprintf("fileinfo_%s", fileID)
		f.listAllCache.Put(cacheKey, response.Data)
		fs.Debugf(f, "💾 getFileInfoByID结果已缓存: fileID=%s", fileID)
	}

	fs.Debugf(f, "✅ getFileInfoByID成功: fileID=%s, name=%s, size=%d, 耗时=%v",
		fileID, response.Data.FileNameBest(), response.Data.Size, duration)
	return response.Data, nil
}

// createObject creates a placeholder Object struct before upload.
func (f *Fs) createObject(ctx context.Context, remote string, modTime time.Time, size int64) (o *Object, leaf string, dirID string, err error) {
	// Fix: 正确分割路径，避免将文件名当作目录创建
	// 参考其他网盘backend的正确做法
	directory, leaf := dircache.SplitPath(remote)

	// 只创建目录部分，不要让FindPath处理完整的文件路径
	dirID, err = f.dirCache.FindDir(ctx, directory, true)
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
	Name:  "media-sync",
	Short: "同步媒体库并创建优化的.strm文件",
	Long: `将115网盘中的视频文件同步到本地目录，创建对应的.strm文件。
.strm文件将包含优化的pick_code格式，支持直接播放和媒体库管理。

用法示例:
rclone backend media-sync 115:Movies /local/media/movies
rclone backend media-sync 115:Videos /local/media/videos -o min-size=200M -o strm-format=true

支持的视频格式: mp4, mkv, avi, mov, wmv, flv, webm, m4v, 3gp, ts, m2ts
.strm文件内容格式: 115://pick_code (可通过strm-format选项调整)`,
	Opts: map[string]string{
		"min-size":    "最小文件大小过滤，小于此大小的文件将被忽略 (默认: 100M)",
		"strm-format": ".strm文件内容格式: true(优化格式)/false(路径格式) (默认: true，兼容: fileid/pickcode/path)",
		"include":     "包含的文件扩展名，逗号分隔 (默认: mp4,mkv,avi,mov,wmv,flv,webm,m4v,3gp,ts,m2ts)",
		"exclude":     "排除的文件扩展名，逗号分隔",
		"update-mode": "更新模式: full/incremental (默认: full)",
		"sync-delete": "同步删除: false(仅创建.strm文件)/true(删除孤立文件和空目录) (默认: false，安全模式)",
		"dry-run":     "预览模式，显示将要创建的文件但不实际创建 (true/false)",
		"target-path": "目标路径，如果不在参数中指定则必须通过此选项提供",
	},
}, {
	Name:  "get-download-url",
	Short: "通过pick_code或.strm内容获取下载URL",
	Long: `通过115网盘的pick_code或.strm文件内容获取实际的下载URL。
支持多种输入格式，特别适用于媒体服务器和.strm文件处理。
返回格式与getdownloadurlua保持一致。

用法示例:
rclone backend get-download-url 115: "115://eybr9y4jowdenzff0"
rclone backend get-download-url 115: "eybr9y4jowdenzff0"
rclone backend get-download-url 115: "/path/to/file.mp4"

输入格式支持:
- 115://pick_code 格式 (来自.strm文件)
- 纯pick_code
- 文件路径 (自动解析为pick_code)`,
}, {
	Name:  "media-sync",
	Short: "同步媒体库并创建优化的.strm文件",
	Long: `将115网盘中的视频文件同步到本地目录，创建对应的.strm文件。
.strm文件将包含优化的pick_code格式，支持直接播放和媒体库管理。

用法示例:
rclone backend media-sync 115:Movies /local/media/movies
rclone backend media-sync 115:Videos /local/media/videos -o min-size=200M -o strm-format=pickcode

支持的视频格式: mp4, mkv, avi, mov, wmv, flv, webm, m4v, 3gp, ts, m2ts
.strm文件内容格式: 115://pick_code (可通过strm-format选项调整)`,
	Opts: map[string]string{
		"min-size":    "最小文件大小过滤，小于此大小的文件将被忽略 (默认: 100M)",
		"strm-format": ".strm文件内容格式: pickcode/path (默认: pickcode)",
		"include":     "包含的文件扩展名，逗号分隔 (默认: mp4,mkv,avi,mov,wmv,flv,webm,m4v,3gp,ts,m2ts)",
		"exclude":     "排除的文件扩展名，逗号分隔",
		"update-mode": "更新模式: full/incremental (默认: full)",
		"dry-run":     "预览模式，显示将要创建的文件但不实际创建 (true/false)",
		"target-path": "目标路径，如果不在参数中指定则必须通过此选项提供",
	},
},
}

// Command executes backend-specific commands.
func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (out any, err error) {
	switch name {

	case "media-sync":
		// New: Media library sync functionality
		return f.mediaSyncCommand(ctx, arg, opt)

	case "get-download-url":
		// New: Get download URL via pick_code
		return f.getDownloadURLCommand(ctx, arg, opt)

	case "refresh-cache":
		// 🔄 新增：刷新目录缓存
		return f.refreshCacheCommand(ctx, arg)

	case "clear-cache":
		// 🧹 新增：清理持久化缓存
		return f.clearCacheCommand115(ctx, arg, opt)

	case "cache-stats":
		// 📊 新增：查看缓存统计
		return f.cacheStatsCommand115(ctx, arg, opt)

	default:
		return nil, fs.ErrorCommandNotFound
	}
}

func (f *Fs) GetPickCodeByPath(ctx context.Context, path string) (string, error) {
	// Fix: Use correct parameter types and methods according to official API documentation
	fs.Debugf(f, "🔍 通过路径获取PickCode: %s", path)

	// 根据API文档，可以使用GET方法，也可以使用POST方法
	// 这里使用GET方法更简洁
	opts := rest.Opts{
		Method: "GET",
		Path:   "/open/folder/get_info",
		Parameters: url.Values{
			"path": []string{path},
		},
	}

	var response struct {
		State   bool   `json:"state"` // Fix: According to API docs, state is boolean type
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			PickCode     string `json:"pick_code"`
			FileName     string `json:"file_name"`
			FileID       string `json:"file_id"`
			FileCategory string `json:"file_category"` // 1:文件, 0:文件夹
			Size         string `json:"size"`
			SizeByte     int64  `json:"size_byte"`
		} `json:"data"`
	}

	err := f.CallOpenAPI(ctx, &opts, nil, &response, false)
	if err != nil {
		return "", fmt.Errorf("failed to get PickCode: %w", err)
	}

	// Check API response status
	if response.Code != 0 {
		return "", fmt.Errorf("API returned error: %s (Code: %d)", response.Message, response.Code)
	}

	fs.Debugf(f, "✅ 获取文件信息: %s -> PickCode: %s, name: %s, size: %d bytes",
		path, response.Data.PickCode, response.Data.FileName, response.Data.SizeByte)

	// 返回 PickCode
	return response.Data.PickCode, nil
}

// getPickCodeByFileID 通过文件ID获取pickCode
func (f *Fs) getPickCodeByFileID(ctx context.Context, fileID string) (string, error) {
	// 修复：使用正确的API路径获取文件详情
	opts := rest.Opts{
		Method: "GET",
		Path:   "/open/folder/get_info",
		Parameters: url.Values{
			"file_id": []string{fileID},
		},
	}

	var response struct {
		State   bool   `json:"state"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Count        any    `json:"count"` // 可能是字符串或数字
			Size         any    `json:"size"`  // 可能是字符串或数字
			SizeByte     int64  `json:"size_byte"`
			FolderCount  any    `json:"folder_count"` // 可能是字符串或数字
			PlayLong     int64  `json:"play_long"`
			ShowPlayLong int    `json:"show_play_long"`
			Ptime        string `json:"ptime"`
			Utime        string `json:"utime"`
			FileName     string `json:"file_name"`
			PickCode     string `json:"pick_code"`
			Sha1         string `json:"sha1"`
			FileID       string `json:"file_id"`
			IsMark       any    `json:"is_mark"` // 可能是字符串或数字
			OpenTime     int64  `json:"open_time"`
			FileCategory any    `json:"file_category"` // 可能是字符串或数字
		} `json:"data"`
	}

	err := f.CallOpenAPI(ctx, &opts, nil, &response, false)
	if err != nil {
		return "", fmt.Errorf("failed to get pickCode via file ID: %w", err)
	}

	if !response.State || response.Code != 0 {
		return "", fmt.Errorf("API returned error: %s (Code: %d, State: %t)", response.Message, response.Code, response.State)
	}

	if response.Data.PickCode == "" {
		return "", fmt.Errorf("API returned empty pickCode for file ID: %s", fileID)
	}

	fs.Debugf(f, " 成功通过文件ID获取pickCode: fileID=%s, pickCode=%s, fileName=%s",
		fileID, response.Data.PickCode, response.Data.FileName)

	return response.Data.PickCode, nil
}

func (f *Fs) getDownloadURLByUA(ctx context.Context, filePath string, UA string) (string, error) {
	// Fix: Add path processing logic consistent with 123 backend
	if filePath == "" {
		filePath = f.root
	}

	// Path cleaning logic, referencing 123 backend
	if filePath == "/" {
		return "", fmt.Errorf("root directory is not a file")
	}
	if len(filePath) > 1 && strings.HasSuffix(filePath, "/") {
		filePath = filePath[:len(filePath)-1]
	}

	fs.Debugf(f, "🔧 处理文件路径: %s", filePath)

	// Performance optimization: Direct HTTP call to get PickCode, avoiding rclone framework overhead
	pickCode, err := f.getPickCodeByPathDirect(ctx, filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read metadata for file path %q: %w", filePath, err)
	}

	fs.Debugf(f, "✅ 成功通过路径获取pick_code: %s -> %s", filePath, pickCode)

	// 使用通用的原始HTTP实现
	return f.getDownloadURLByPickCodeHTTP(ctx, pickCode, UA)
}

// getPickCodeByPathDirect 使用rclone标准方式获取PickCode
func (f *Fs) getPickCodeByPathDirect(ctx context.Context, path string) (string, error) {
	fs.Debugf(f, "🔧 使用rclone标准方式获取PickCode: %s", path)

	// 构建请求选项
	opts := rest.Opts{
		Method: "GET",
		Path:   "/open/folder/get_info",
		Parameters: url.Values{
			"path": []string{path},
		},
	}

	// 准备认证信息
	_ = f.prepareTokenForRequest(ctx, &opts)

	// 解析响应
	var response struct {
		State   bool   `json:"state"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			PickCode     string `json:"pick_code"`
			FileName     string `json:"file_name"`
			FileID       string `json:"file_id"`
			FileCategory string `json:"file_category"`
		} `json:"data"`
	}

	// 使用统一API调用方法
	err := f.CallAPI(ctx, OpenAPI, &opts, nil, &response)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}

	if response.Code != 0 {
		return "", fmt.Errorf("API returned error: %s (Code: %d)", response.Message, response.Code)
	}

	fs.Debugf(f, "✅ 直接HTTP获取PickCode成功: %s -> %s", path, response.Data.PickCode)
	return response.Data.PickCode, nil
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

// GetPickCode returns the pick code for STRM file generation
func (o *Object) GetPickCode() string {
	return o.pickCode
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// ModTime returns the modification time
func (o *Object) ModTime(ctx context.Context) time.Time {
	// 🚀 优化：只在没有元数据时才调用readMetaData，避免重复API调用
	if !o.hasMetaData {
		err := o.readMetaData(ctx)
		if err != nil {
			// 在跨云传输时，目标文件不存在是正常情况，降级为调试信息
			if err == fs.ErrorObjectNotFound {
				fs.Debugf(o, "目标文件不存在，ModTime将使用零值: %v", err)
			} else {
				fs.Logf(o, "failed to read metadata for ModTime: %v", err)
			}
			// Return a zero time instead of Now() as Precision is NotSupported
			return time.Time{}
		}
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
		// 关键修复：rclone期望小写哈希值，但115网盘内部使用大写
		// 对外返回小写，对内保持大写用于API调用
		return strings.ToLower(o.sha1sum), nil
	}
	err := o.readMetaData(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read metadata for Hash: %w", err)
	}
	// 关键修复：rclone期望小写哈希值，但115网盘内部使用大写
	// 对外返回小写，对内保持大写用于API调用
	return strings.ToLower(o.sha1sum), nil
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

	// 🚀 优化：检测是否为小范围读取（如预览、MIME检测等）
	var rangeOption *fs.RangeOption
	for _, option := range options {
		if ro, ok := option.(*fs.RangeOption); ok {
			rangeOption = ro
			break
		}
	}

	// 🚀 智能下载策略：小范围读取优化
	if rangeOption != nil {
		start, end := rangeOption.Decode(o.size)
		requestSize := end - start + 1

		// 如果请求的数据量很小（如前1MB用于预览/MIME检测），使用范围下载
		if requestSize <= 1024*1024 { // 1MB阈值
			fs.Debugf(o, "🎯 检测到小范围读取请求: %d-%d (%s)，使用范围下载避免下载整个文件",
				start, end, fs.SizeSuffix(requestSize))
		} else {
			fs.Debugf(o, "📥 大范围读取请求: %d-%d (%s)，使用普通下载",
				start, end, fs.SizeSuffix(requestSize))
		}
	} else {
		fs.Debugf(o, "📥 115下载策略: 禁用并发下载 (1TB阈值 + 1GB分片，强制普通下载)")
	}

	// Get/refresh download URL
	err = o.setDownloadURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get download URL: %w", err)
	}
	if !o.durl.Valid() {
		// Attempt refresh again if invalid right after getting it
		fs.Debugf(o, "⚠️ 下载URL获取后立即无效，重试中...")
		// 使用rclone标准pacer而不是直接sleep
		_ = o.fs.pacer.Call(func() (bool, error) {
			return false, nil // 只是为了利用pacer的延迟机制
		})
		err = o.setDownloadURLWithForce(ctx, true) // Force refresh
		if err != nil {
			return nil, fmt.Errorf("failed to get download URL on retry: %w", err)
		}
		if !o.durl.Valid() {
			return nil, errors.New("failed to obtain a valid download URL")
		}
	}

	// 并发安全修复：使用互斥锁保护对durl的访问
	o.durlMu.Lock()

	// 检查URL有效性（在锁保护下）
	if o.durl == nil || !o.durl.Valid() {
		o.durlMu.Unlock()

		// 使用rclone标准方法重新获取下载URL
		err := o.setDownloadURLWithForce(ctx, true)
		if err != nil {
			return nil, fmt.Errorf("failed to re-obtain download URL: %w", err)
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

	// Range请求调试和处理
	fs.FixRangeOption(options, o.size)
	fs.OpenOptionAddHTTPHeaders(req.Header, options)

	// 调试Range请求信息
	if rangeHeader := req.Header.Get("Range"); rangeHeader != "" {
		fs.Debugf(o, " 115网盘Range请求: %s (文件大小: %s)", rangeHeader, fs.SizeSuffix(o.size))
	}

	if o.size == 0 {
		// Don't supply range requests for 0 length objects as they always fail
		delete(req.Header, "Range")
	}

	// 使用标准HTTP客户端进行请求
	httpClient := fshttp.NewClient(ctx)
	resp, err := httpClient.Do(req)
	if err != nil {
		fs.Debugf(o, "❌ 115 HTTP请求失败: %v", err)
		return nil, err
	}

	// Range响应验证和智能fallback处理
	if rangeHeader := req.Header.Get("Range"); rangeHeader != "" {
		contentLength := resp.Header.Get("Content-Length")
		contentRange := resp.Header.Get("Content-Range")
		fs.Debugf(o, "📥 115 Range响应: Status=%d, Content-Length=%s, Content-Range=%s",
			resp.StatusCode, contentLength, contentRange)

		// Check if Range request was handled correctly
		if resp.StatusCode != 206 {
			fs.Debugf(o, "⚠️ 115 Range请求失败: %d，115网盘下载URL不支持Range请求", resp.StatusCode)
			resp.Body.Close() // 关闭失败的响应

			// 智能fallback: 下载完整文件并模拟Range行为
			fs.Debugf(o, "🔄 Range请求失败，使用客户端Range模拟")
			req.Header.Del("Range") // 移除Range头

			// 重新发起完整文件请求
			resp, err = httpClient.Do(req)
			if err != nil {
				fs.Debugf(o, "❌ 115 完整文件下载请求失败: %v", err)
				return nil, err
			}

			// 解析原始Range请求
			var start, end int64
			if n, parseErr := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); parseErr == nil && n == 2 {
				fs.Debugf(o, "🎯 客户端Range模拟: 跳过前%d字节，读取%d字节", start, end-start+1)

				// 包装响应体，实现客户端Range模拟
				resp.Body = &RangeSimulatorReader{
					ReadCloser: resp.Body,
					start:      start,
					end:        end,
					current:    0,
				}

				// 修改响应状态码和头部，模拟206响应
				resp.StatusCode = 206
				resp.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, o.size))
				resp.Header.Set("Content-Length", fmt.Sprintf("%d", end-start+1))
			}

			fs.Debugf(o, "✅ 115 Range模拟设置完成: Status=%d", resp.StatusCode)
		}
	}

	// 🔧 混合进度显示方案：既修复rclone标准进度，又保留详细进度日志
	// 创建包装的ReadCloser来提供详细的下载进度日志，同时让rclone正确处理标准进度
	return &ProgressReadCloser115{
		ReadCloser: resp.Body,
		fs:         o.fs,
		object:     o,
		totalSize:  o.size,
		startTime:  time.Now(),
	}, nil
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
		fs.Debugf(o, "🔄 Update创建了新对象 %q，删除旧对象 %q", newO.id, oldID)
		err = o.fs.deleteFiles(ctx, []string{oldID})
		if err != nil {
			// Log error but don't fail the update, the new file is there
			fs.Errorf(o, "❌ 更新后删除旧版本失败 %q: %v", oldID, err)
		}
	} else {
		fs.Debugf(o, "🔄 Update可能覆盖了现有对象 %q", oldID)
	}

	// Replace the metadata of the original object `o` with the new object's data
	// Note: We must copy fields individually to avoid copying mutex fields
	o.fs = newO.fs
	o.remote = newO.remote
	o.hasMetaData = newO.hasMetaData
	o.id = newO.id
	o.parent = newO.parent
	o.size = newO.size
	o.sha1sum = newO.sha1sum
	o.pickCode = newO.pickCode
	o.modTime = newO.modTime
	o.durl = newO.durl
	o.durlRefreshing = newO.durlRefreshing
	// Note: durlMu and pickCodeMu are preserved from the original object

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
	o.fs.dirCache.FlushDir(path.Dir(o.remote))

	return nil
}

// setMetaData updates the object's metadata from an File struct.
func (o *Object) setMetaData(info *File) error {
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

	pickCode := info.PickCodeBest()
	fileID := info.ID()

	fs.Debugf(o, " setMetaData调试: fileName=%s, pickCode=%s, fileID=%s",
		info.FileNameBest(), pickCode, fileID)

	if pickCode != "" && pickCode == fileID {
		fs.Debugf(o, "⚠️ 检测到pickCode与fileID相同，这是错误的: %s", pickCode)
		pickCode = "" // Clear incorrect pickCode
	}

	if pickCode == "" {
		fs.Debugf(o, "📝 文件元数据pickCode为空，需要时将动态获取")
	} else {
		isAllDigits := true
		for _, r := range pickCode {
			if r < '0' || r > '9' {
				isAllDigits = false
				break
			}
		}

		if len(pickCode) > 15 && isAllDigits {
			fs.Debugf(o, "⚠️ 检测到疑似文件ID而非pickCode: %s，需要时将重新获取", pickCode)
			pickCode = "" // Clear incorrect pickCode, force re-obtain
		}
	}

	o.pickCode = pickCode
	o.modTime = info.ModTime()
	o.hasMetaData = true
	return nil
}

// setMetaDataFromCallBack updates metadata after an upload callback.
func (o *Object) setMetaDataFromCallBack(data *CallbackData) error {
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

	// 🚀 优化：如果有文件ID，优先使用直接API获取文件信息
	if o.id != "" {
		fs.Debugf(o.fs, "🚀 readMetaData: 使用直接API获取元数据, fileID=%s", o.id)
		info, err := o.fs.getFileInfoByID(ctx, o.id)
		if err == nil {
			// 直接API成功，设置元数据
			err = o.setMetaData(info)
			if err != nil {
				fs.Debugf(o.fs, "readMetaData: setMetaData失败: %v", err)
			} else {
				fs.Debugf(o.fs, "✅ readMetaData: 直接API成功获取元数据")
			}
			return err
		}
		// 直接API失败，记录日志但继续尝试路径查找
		fs.Debugf(o.fs, "⚠️ readMetaData: 直接API失败，回退到路径查找: %v", err)
	}

	// 回退到原来的路径查找方法
	fs.Debugf(o.fs, "🔄 readMetaData: 使用路径查找方法")
	info, err := o.fs.readMetaDataForPath(ctx, o.remote)
	if err != nil {
		fs.Debugf(o.fs, "readMetaData失败: %v", err)
		return err // fs.ErrorObjectNotFound or other errors
	}

	err = o.setMetaData(info)
	if err != nil {
		fs.Debugf(o.fs, "readMetaData: setMetaData失败: %v", err)
	}
	return err
}

// setDownloadURL ensures a valid download URL is available with optimized concurrent access.
func (o *Object) setDownloadURL(ctx context.Context) error {
	// 🔧 性能优化：首先检查缓存（无锁检查）
	cacheKey := fmt.Sprintf("%s:%s", o.id, o.pickCode)
	if cached, ok := o.fs.downloadURLCache.Load(cacheKey); ok {
		if cachedURL, ok := cached.(CachedDownloadURL); ok && cachedURL.Valid() {
			// 检查是否即将过期，如果是则异步刷新
			if cachedURL.NearExpiry() {
				fs.Debugf(o, "⚠️ 缓存即将过期，异步刷新中...")
				go func() {
					// 异步刷新缓存，不阻塞当前请求
					_ = o.refreshDownloadURLCache(context.Background())
				}()
			}
			fs.Debugf(o, "✅ 使用缓存的下载URL")
			o.durlMu.Lock()
			o.durl = &DownloadURL{
				URL:       cachedURL.URL,
				CreatedAt: time.Now(), // 设置创建时间
			}
			o.durlMu.Unlock()
			return nil
		}
	}

	// 🔧 优化锁竞争：使用简化的锁结构，避免嵌套锁
	o.durlMu.Lock()

	// 检查URL是否已经有效（在锁保护下）
	if o.durl != nil && o.durl.Valid() {
		o.durlMu.Unlock()
		return nil
	}

	// URL无效或不存在，需要获取新的URL
	fs.Debugf(o, "🔄 下载URL无效或不存在，重新获取")

	var err error
	var newURL *DownloadURL

	// Add timeout context for URL fetching to prevent hanging
	urlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if o.fs.isShare {
		// Share download is no longer supported
		return errors.New("share download is not supported")
	} else {
		// Use OpenAPI download URL endpoint
		if o.pickCode == "" {
			// If pickCode is missing, try getting it from metadata first
			metaErr := o.readMetaData(urlCtx)
			if metaErr != nil || o.pickCode == "" {
				// 修复：尝试通过文件ID获取pickCode
				if o.id != "" {
					fs.Debugf(o, "🔍 尝试通过文件ID获取pickCode: fileID=%s", o.id)
					pickCode, pickErr := o.fs.getPickCodeByFileID(urlCtx, o.id)
					if pickErr != nil {
						o.durlMu.Unlock()
						return fmt.Errorf("cannot get pick code by file ID %s: %w", o.id, pickErr)
					}
					o.pickCode = pickCode
					fs.Debugf(o, "✅ 成功通过文件ID获取pickCode: %s", pickCode)
				} else {
					o.durlMu.Unlock()
					return fmt.Errorf("cannot get download URL without pick code (metadata read error: %v)", metaErr)
				}
			}
		}

		// 修复并发问题：确保pickCode有效后再调用
		if o.pickCode == "" {
			o.durlMu.Unlock()
			return fmt.Errorf("cannot get download URL: pickCode is still empty after all attempts")
		}

		// 根本性修复：使用验证过的pickCode
		validPickCode, validationErr := o.fs.validateAndCorrectPickCode(urlCtx, o.pickCode)
		if validationErr != nil {
			o.durlMu.Unlock()
			return fmt.Errorf("pickCode validation failed: %w", validationErr)
		}

		// 如果pickCode被修正，更新Object中的pickCode
		if validPickCode != o.pickCode {
			fs.Debugf(o, "🔧 Object pickCode已修正: %s -> %s", o.pickCode, validPickCode)
			o.pickCode = validPickCode
		}

		newURL, err = o.fs.getDownloadURL(urlCtx, validPickCode)
	}

	if err != nil {
		o.durl = nil // Clear invalid URL
		o.durlMu.Unlock()
		return fmt.Errorf("failed to get download URL: %w", err)
	}

	o.durl = newURL
	if !o.durl.Valid() {
		// This might happen if the link expires immediately or is invalid
		fs.Logf(o, "Fetched download URL is invalid or expired immediately: %s", o.durl.URL)
		// 清除无效的URL，强制下次重新获取
		o.durl = nil
		o.durlMu.Unlock()
		return fmt.Errorf("fetched download URL is invalid or expired immediately")
	}

	// 🔧 紧急修复：115网盘下载链接只有5分钟有效期，使用保守的缓存策略
	cacheKey = fmt.Sprintf("%s:%s", o.id, o.pickCode)
	cachedURL := CachedDownloadURL{
		URL:       o.durl.URL,
		ExpiresAt: time.Now().Add(3 * time.Minute), // 3分钟缓存，留2分钟安全边际
	}
	o.fs.downloadURLCache.Store(cacheKey, cachedURL)
	// 💾 异步保存到持久化缓存
	go o.fs.saveDownloadURLCache()
	fs.Debugf(o, "✅ 成功获取下载URL并缓存（3分钟有效期）")

	o.durlMu.Unlock()
	return nil
}

// setDownloadURLWithForce sets the download URL for the object with optional cache bypass
func (o *Object) setDownloadURLWithForce(ctx context.Context, forceRefresh bool) error {
	o.durlMu.Lock()

	// 如果不是强制刷新，检查URL是否已经有效（在锁保护下）
	if !forceRefresh && o.durl != nil && o.durl.Valid() {
		o.durlMu.Unlock()
		return nil
	}

	// URL无效或不存在，或者强制刷新，需要获取新的URL（继续持有锁）
	// Double-check: 确保在锁保护下进行所有检查
	if !forceRefresh && o.durl != nil && o.durl.Valid() {
		fs.Debugf(o, "✅ 下载URL已被其他协程获取")
		o.durlMu.Unlock()
		return nil
	}
	// 注意：这里不释放锁，继续在锁保护下执行URL获取

	var err error
	var newURL *DownloadURL

	// Add timeout context for URL fetching to prevent hanging
	urlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if o.fs.isShare {
		// Share download is no longer supported
		o.durlMu.Unlock()
		return errors.New("share download is not supported")
	} else {
		// Use OpenAPI download URL endpoint
		// 并发安全：获取pickCode时使用锁保护
		if o.pickCode == "" {
			o.pickCodeMu.Lock()
			// Double-check: 确保在锁保护下进行检查
			if o.pickCode == "" {
				// If pickCode is missing, try getting it from metadata first
				metaErr := o.readMetaData(urlCtx)
				if metaErr != nil || o.pickCode == "" {
					// 修复：尝试通过文件ID获取pickCode
					if o.id != "" {
						fs.Debugf(o, " 尝试通过文件ID获取pickCode: fileID=%s", o.id)
						pickCode, pickErr := o.fs.getPickCodeByFileID(urlCtx, o.id)
						if pickErr != nil {
							o.pickCodeMu.Unlock()
							o.durlMu.Unlock()
							return fmt.Errorf("cannot get pick code by file ID %s: %w", o.id, pickErr)
						}
						o.pickCode = pickCode
						fs.Debugf(o, " 成功通过文件ID获取pickCode: %s", pickCode)
					} else {
						o.pickCodeMu.Unlock()
						o.durlMu.Unlock()
						return fmt.Errorf("cannot get download URL without pick code (metadata read error: %v)", metaErr)
					}
				}
			}
			o.pickCodeMu.Unlock()
		}

		// 修复并发问题：确保pickCode有效后再调用
		if o.pickCode == "" {
			o.durlMu.Unlock()
			return fmt.Errorf("cannot get download URL: pickCode is still empty after all attempts")
		}

		// 根本性修复：使用验证过的pickCode
		validPickCode, validationErr := o.fs.validateAndCorrectPickCode(urlCtx, o.pickCode)
		if validationErr != nil {
			o.durlMu.Unlock()
			return fmt.Errorf("pickCode validation failed: %w", validationErr)
		}

		// 如果pickCode被修正，更新Object中的pickCode
		if validPickCode != o.pickCode {
			fs.Debugf(o, " Object pickCode已修正: %s -> %s", o.pickCode, validPickCode)
			o.pickCode = validPickCode
		}

		newURL, err = o.fs.getDownloadURLWithForce(urlCtx, validPickCode, forceRefresh)
	}

	if err != nil {
		o.durl = nil      // Clear invalid URL
		o.durlMu.Unlock() // 错误路径释放锁

		// Check for server errors
		if fserrors.ShouldRetry(err) {
			return fmt.Errorf("server error when fetching download URL: %w", err)
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
		o.durlMu.Unlock() // 无效URL路径释放锁
		return fmt.Errorf("fetched download URL is invalid or expired immediately")
	} else {
		fs.Debugf(o, "✅ 成功获取下载URL")
	}
	o.durlMu.Unlock() // 成功路径释放锁
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
		fs.Debugf(f, "❌ 从配置获取令牌失败: %v", err)
		return false
	}

	if token == nil || token.AccessToken == "" || token.RefreshToken == "" {
		fs.Debugf(f, "⚠️ 配置中的令牌不完整")
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

	fs.Debugf(f, "✅ 从配置文件加载令牌，过期时间 %v", f.tokenExpiry)
	return true
}

// ProgressReadCloser115 混合进度显示包装器
// 🔧 既提供详细的进度日志，又兼容rclone的accounting系统
type ProgressReadCloser115 struct {
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
func (prc *ProgressReadCloser115) Read(p []byte) (n int, err error) {
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

			fs.Debugf(prc.object, "📥 115下载进度: %s/%s (%d%%) 速度: %.2f MB/s",
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

// Close 实现io.Closer接口
func (prc *ProgressReadCloser115) Close() error {
	// 输出最终下载统计
	elapsed := time.Since(prc.startTime)
	var avgSpeed float64
	if elapsed.Seconds() > 0 {
		avgSpeed = float64(prc.transferredSize) / elapsed.Seconds() / 1024 / 1024 // MB/s
	}

	fs.Debugf(prc.object, "✅ 115下载完成: %s, 耗时: %v, 平均速度: %.2f MB/s",
		fs.SizeSuffix(prc.transferredSize),
		elapsed.Truncate(time.Second),
		avgSpeed)

	return prc.ReadCloser.Close()
}

// TwoStepProgressReader115 跨云传输统一进度显示
// 🔧 重新实现：解决跨云传输进度跳跃和不连续的问题
type TwoStepProgressReader115 struct {
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

// NewTwoStepProgressReader115 创建跨云传输进度跟踪器
func NewTwoStepProgressReader115(rc io.ReadCloser, fs *Fs, remote string, size int64, account *accounting.Account) *TwoStepProgressReader115 {
	return &TwoStepProgressReader115{
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
func (tpr *TwoStepProgressReader115) Read(p []byte) (n int, err error) {
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
func (tpr *TwoStepProgressReader115) SwitchToUpload() {
	tpr.phase = "upload"
	tpr.uploadBytes = 0
	fs.Debugf(tpr.fs, "🔄 跨云传输切换到上传阶段: %s", tpr.remote)
}

// UpdateUploadProgress 更新上传进度
func (tpr *TwoStepProgressReader115) UpdateUploadProgress(uploaded int64) {
	if tpr.phase == "upload" {
		tpr.uploadBytes = uploaded
		tpr.updateProgress()
	}
}

// updateProgress 统一更新进度显示
func (tpr *TwoStepProgressReader115) updateProgress() {
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
func (tpr *TwoStepProgressReader115) Close() error {
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

// calculateOptimalChunkSize 智能计算115网盘上传分片大小
// 基于文件大小选择最优分片策略，提高传输效率
func (f *Fs) calculateOptimalChunkSize(fileSize int64) fs.SizeSuffix {
	// 如果用户配置了chunk_size，优先使用用户配置
	userChunkSize := int64(f.opt.ChunkSize)
	if userChunkSize > 0 {
		// 用户明确配置了分片大小，直接使用
		fs.Debugf(f, "🔧 使用用户配置的分片大小: %s", fs.SizeSuffix(userChunkSize))
		return fs.SizeSuffix(f.normalizeChunkSize(userChunkSize))
	}

	// 用户未配置，使用智能分片策略
	// Based on 2MB/s target speed optimization: use larger chunks to reduce API call overhead
	// Larger chunks can better utilize network bandwidth and reduce inter-chunk overhead
	var partSize int64

	// 🔧 优化分片策略：与123网盘对齐，提高中小文件上传效率
	// 参考123网盘的成功策略，减少小文件的分片数量
	switch {
	case fileSize <= 500*1024*1024: // ≤500MB
		partSize = fileSize // 🔧 中小文件直接单分片上传，避免分片开销，提高效率
	case fileSize <= 2*1024*1024*1024: // ≤2GB
		partSize = 200 * 1024 * 1024 // 200MB分片，减少分片数量
	case fileSize <= 10*1024*1024*1024: // ≤10GB
		partSize = 500 * 1024 * 1024 // 500MB分片
	case fileSize <= 100*1024*1024*1024: // ≤100GB
		partSize = 1 * 1024 * 1024 * 1024 // 1GB分片
	default: // >100GB
		partSize = 2 * 1024 * 1024 * 1024 // 2GB分片
	}

	fs.Debugf(f, "🎯 智能分片策略: 文件大小 %s -> 分片大小 %s", fs.SizeSuffix(fileSize), fs.SizeSuffix(partSize))
	return fs.SizeSuffix(f.normalizeChunkSize(partSize))
}

// normalizeChunkSize 确保分片大小在合理范围内
func (f *Fs) normalizeChunkSize(partSize int64) int64 {
	if partSize < int64(minChunkSize) {
		partSize = int64(minChunkSize)
	}
	if partSize > int64(maxChunkSize) {
		partSize = int64(maxChunkSize)
	}
	return partSize
}

// localFileInfo 本地文件信息结构，用于跨云传输
type localFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	remote  string
}

func (l *localFileInfo) String() string                        { return l.remote }
func (l *localFileInfo) Remote() string                        { return l.remote }
func (l *localFileInfo) ModTime(ctx context.Context) time.Time { return l.modTime }
func (l *localFileInfo) Size() int64                           { return l.size }
func (l *localFileInfo) Fs() fs.Info                           { return nil }
func (l *localFileInfo) Hash(ctx context.Context, t hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}
func (l *localFileInfo) Storable() bool { return true }

// crossCloudUploadWithLocalCache 跨云传输专用上传方法，智能检查已有下载避免重复
func (f *Fs) crossCloudUploadWithLocalCache(ctx context.Context, in io.Reader, src fs.ObjectInfo, remote string, options ...fs.OpenOption) (fs.Object, error) {
	fileSize := src.Size()
	fs.Infof(f, "☁️ 开始跨云传输本地化: %s (大小: %s)", remote, fs.SizeSuffix(fileSize))

	// 🌊 StreamHashMode检查：如果启用则回退到流式哈希传输
	if f.opt.StreamHashMode {
		fs.Infof(f, "🌊 StreamHashMode启用，回退到流式哈希传输避免临时文件")
		return f.smartStreamHashTransfer115(ctx, src, remote, options...)
	}

	// 修复重复下载：检查是否已有完整的临时文件
	if tempReader, tempSize, tempCleanup := f.checkExistingTempFile(src); tempReader != nil {
		fs.Infof(f, "✅ 发现现有完整下载文件，跳过重复下载: %s", fs.SizeSuffix(tempSize))

		// 关键修复：将临时文件包装成带Account对象的Reader
		if tempFile, ok := tempReader.(*os.File); ok {
			accountedReader := NewAccountedFileReader(ctx, tempFile, tempSize, remote, tempCleanup)
			return f.upload(ctx, accountedReader, src, remote, options...)
		} else {
			defer tempCleanup()
			return f.upload(ctx, tempReader, src, remote, options...)
		}
	}

	// 智能下载策略：检查是否可以从失败的下载中恢复
	var localDataSource io.Reader
	var cleanup func()
	var localFileSize int64

	// 内存优化：使用内存优化器判断是否使用内存缓冲
	maxMemoryBufferSize := memoryBufferThreshold // 使用统一的内存缓冲阈值
	useMemoryBuffer := false

	// 简化：对于小文件直接使用内存缓冲，不需要复杂的内存优化器
	if fileSize <= maxMemoryBufferSize {
		useMemoryBuffer = true
	}

	if useMemoryBuffer {
		// 小文件：下载到内存
		fs.Infof(f, "📝 小文件跨云传输，下载到内存: %s", fs.SizeSuffix(fileSize))
		data, err := io.ReadAll(in)
		if err != nil {
			return nil, fmt.Errorf("cross-cloud transfer download to memory failed: %w", err)
		}
		localDataSource = bytes.NewReader(data)
		localFileSize = int64(len(data))
		cleanup = func() {
			// 简化：不需要复杂的内存管理
		}
		fs.Infof(f, "✅ 跨云传输内存下载完成: %s", fs.SizeSuffix(localFileSize))
	} else {
		// 大文件或内存不足：下载到临时文件
		if fileSize <= maxMemoryBufferSize {
			fs.Infof(f, "⚠️ 内存不足，回退到临时文件: %s", fs.SizeSuffix(fileSize))
		} else {
			fs.Infof(f, "📁 大文件跨云传输，下载到临时文件: %s", fs.SizeSuffix(fileSize))
		}
		tempFile, err := os.CreateTemp("", "rclone-115-crosscloud-*.tmp")
		if err != nil {
			return nil, fmt.Errorf("failed to create temp file: %w", err)
		}
		fs.Debugf(f, "💾 创建临时文件: %s | 原因: 跨云传输大文件缓存 | 目标大小: %s", tempFile.Name(), fs.SizeSuffix(fileSize))

		fs.Debugf(f, "✍️ 开始写入临时文件: %s", tempFile.Name())
		written, err := io.Copy(tempFile, in)
		if err != nil {
			_ = tempFile.Close()
			_ = os.Remove(tempFile.Name())
			fs.Debugf(f, "⚠️ 写入临时文件失败，已清理: %s", tempFile.Name())
			return nil, fmt.Errorf("cross-cloud transfer download to temp file failed: %w", err)
		}
		fs.Debugf(f, "✅ 临时文件写入完成: %s | 写入字节: %s", tempFile.Name(), fs.SizeSuffix(written))

		// 重置文件指针到开头
		if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
			_ = tempFile.Close()
			_ = os.Remove(tempFile.Name())
			return nil, fmt.Errorf("failed to reset temp file pointer: %w", err)
		}

		// 关键修复：创建带Account对象的文件读取器
		cleanupFunc := func() {
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
		localDataSource = NewAccountedFileReader(ctx, tempFile, written, remote, cleanupFunc)
		localFileSize = written
		cleanup = func() {
			// AccountedFileReader会在Close时调用cleanupFunc
		}
		fs.Infof(f, "✅ 跨云传输临时文件下载完成: %s", fs.SizeSuffix(localFileSize))
	}
	defer cleanup()

	// Verify download integrity
	if localFileSize != fileSize {
		return nil, fmt.Errorf("cross-cloud transfer download incomplete: expected %d bytes, got %d bytes", fileSize, localFileSize)
	}

	// 使用本地数据进行上传，创建本地文件信息对象
	localSrc := &localFileInfo{
		name:    filepath.Base(remote),
		size:    localFileSize,
		modTime: src.ModTime(ctx),
		remote:  remote,
	}

	fs.Infof(f, "🚀 开始上传到115网盘: %s", fs.SizeSuffix(localFileSize))

	// 使用主上传函数，但此时源已经是本地数据
	return f.upload(ctx, localDataSource, localSrc, remote, options...)
}

// 115网盘所有API都受到相同的全局QPS限制，不需要复杂的选择逻辑
func (f *Fs) getPacerForEndpoint() *fs.Pacer {
	// 所有API使用统一的pacer，符合115网盘的全局QPS限制
	return f.pacer
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
	if r.file != nil {
		if closeErr := r.file.Close(); closeErr != nil {
			fs.Debugf(nil, "⚠️ 关闭并发下载临时文件失败: %v", closeErr)
		} else {
			fs.Debugf(nil, "📁 关闭并发下载临时文件成功: %s", r.tempPath)
		}
	}
	if r.tempPath != "" {
		if removeErr := os.Remove(r.tempPath); removeErr != nil {
			fs.Debugf(nil, "⚠️ 删除并发下载临时文件失败: %v", removeErr)
		} else {
			fs.Debugf(nil, "🧹 删除并发下载临时文件成功: %s", r.tempPath)
		}
	}
	return nil
}

// RangeSimulatorReader 客户端Range模拟器，用于不支持Range请求的服务器
type RangeSimulatorReader struct {
	io.ReadCloser
	start   int64 // Range开始位置
	end     int64 // Range结束位置
	current int64 // 当前读取位置
}

// Read 实现Range模拟：跳过前面的数据，只返回指定范围的数据
func (r *RangeSimulatorReader) Read(p []byte) (n int, err error) {
	// 如果还没到达Range开始位置，跳过数据
	if r.current < r.start {
		skipBytes := r.start - r.current
		if skipBytes > int64(len(p)) {
			skipBytes = int64(len(p))
		}

		// 读取但丢弃数据
		tempBuf := make([]byte, skipBytes)
		n, err = r.ReadCloser.Read(tempBuf)
		r.current += int64(n)

		if err != nil {
			return 0, err
		}

		// 如果还没到达Range开始位置，继续跳过
		if r.current < r.start {
			return r.Read(p) // 递归调用继续跳过
		}

		// 到达Range开始位置，开始正常读取
		return r.Read(p)
	}

	// 已经到达Range开始位置，检查是否超过Range结束位置
	if r.current > r.end {
		return 0, io.EOF
	}

	// 计算还能读取多少字节
	remainingInRange := r.end - r.current + 1
	if remainingInRange <= 0 {
		return 0, io.EOF
	}

	// 限制读取长度不超过Range范围
	if int64(len(p)) > remainingInRange {
		p = p[:remainingInRange]
	}

	// 正常读取数据
	n, err = r.ReadCloser.Read(p)
	r.current += int64(n)

	return n, err
}

// listAll retrieves directory listings, using OpenAPI if possible.
// User function fn should return true to stop processing.
type listAllFn func(*File) bool

func (f *Fs) listAll(ctx context.Context, dirID string, limit int, filesOnly bool, fn listAllFn) (found bool, err error) {
	fs.Debugf(f, "📋 listAll开始: dirID=%q, limit=%d, filesOnly=%v", dirID, limit, filesOnly)

	// 🚀 性能优化：智能listAll缓存（类似123网盘的listFileCache）
	// 只缓存标准的列表查询（limit=defaultListChunkSize，无特殊限制）
	if limit == defaultListChunkSize && !filesOnly {
		cacheKey := fmt.Sprintf("listall_%s_%d_%v", dirID, limit, filesOnly)
		if cached, found := f.listAllCache.GetMaybe(cacheKey); found {
			if cachedFiles, ok := cached.([]*File); ok {
				fs.Debugf(f, "🎯 listAll缓存命中: dirID=%s (%d个文件)", dirID, len(cachedFiles))
				// 使用缓存的结果调用回调函数
				for _, file := range cachedFiles {
					if fn(file) {
						return true, nil // 找到目标，停止处理
					}
				}
				return false, nil // 处理完所有缓存文件，未找到目标
			}
		}

		// 🧠 智能缓存：检查完整目录缓存
		fullCacheKey := fmt.Sprintf("listall_full_%s", dirID)
		if cached, found := f.listAllCache.GetMaybe(fullCacheKey); found {
			if cachedFiles, ok := cached.([]*File); ok {
				fs.Debugf(f, "🎯 listAll完整目录缓存命中: dirID=%s (%d个文件)", dirID, len(cachedFiles))
				// 使用缓存的结果调用回调函数
				for _, file := range cachedFiles {
					if fn(file) {
						return true, nil // 找到目标，停止处理
					}
				}
				return false, nil // 处理完所有缓存文件，未找到目标
			}
		}
	}

	// 验证目录ID
	if dirID == "" {
		fs.Errorf(f, "❌ listAll: 目录ID为空，这可能导致查询根目录")
		// 不要直接返回错误，而是记录警告并继续，因为根目录查询可能是合法的
	}

	if f.isShare {
		// Share listing is no longer supported
		return false, errors.New("share listing is not supported")
	}

	// Use OpenAPI listing
	fs.Debugf(f, "🌐 listAll: 使用OpenAPI模式")
	params := url.Values{}
	params.Set("cid", dirID)
	params.Set("limit", strconv.Itoa(limit))
	params.Set("offset", "0")
	params.Set("show_dir", "1") // Include directories in the listing

	// Sorting (OpenAPI uses query params)
	params.Set("o", "user_utime") // Default sort: update time
	params.Set("asc", "0")        // Default sort: descending

	offset := 0
	var allFiles []*File // 🚀 收集所有文件用于缓存

	fs.Debugf(f, "🔄 listAll: 开始分页循环")
	for {
		fs.Debugf(f, " listAll: 处理offset=%d", offset)
		params.Set("offset", strconv.Itoa(offset))
		opts := rest.Opts{
			Method:     "GET",
			Path:       "/open/ufile/files",
			Parameters: params,
		}

		fs.Debugf(f, "🔧 listAll: 准备调用CallOpenAPI")
		var info FileList
		err = f.CallOpenAPI(ctx, &opts, nil, &info, false) // Use OpenAPI call
		if err != nil {
			fs.Debugf(f, "❌ listAll: CallOpenAPI失败: %v", err)
			// 处理API限制错误
			if err = f.handleAPILimitError(ctx, err, &opts, &info, dirID); err != nil {
				return found, err
			}
		}

		fs.Debugf(f, "✅ listAll: CallOpenAPI成功，返回%d个文件", len(info.Files))

		if len(info.Files) == 0 {
			break // No more items
		}

		for _, item := range info.Files {
			isDir := item.IsDir()
			// Apply client-side filtering if needed
			if filesOnly && isDir {
				continue
			}

			// Decode name
			item.FileName = f.opt.Enc.ToStandardName(item.FileNameBest()) // Use best name getter

			// 🚀 收集文件用于缓存（只在可缓存的查询中收集）
			if limit == defaultListChunkSize && !filesOnly {
				allFiles = append(allFiles, item)
			}

			if fn(item) {
				found = true
				// 🚀 即使找到目标，也要缓存已收集的文件
				if limit == defaultListChunkSize && !filesOnly && len(allFiles) > 0 {
					cacheKey := fmt.Sprintf("listall_%s_%d_%v", dirID, limit, filesOnly)
					f.listAllCache.Put(cacheKey, allFiles)
					// 💾 同时保存到持久化缓存
					f.saveListAllCacheEntry(cacheKey, allFiles)
					fs.Debugf(f, "💾 listAll结果已缓存(早期退出): dirID=%s (%d个文件)", dirID, len(allFiles))
				}
				return found, nil // Early exit
			}
		}

		// Check if we have fetched all items based on total count from response
		currentOffset, _ := strconv.Atoi(params.Get("offset"))
		offset = currentOffset + len(info.Files)

		// Stop listing when we've reached the total count
		if info.Count > 0 && offset >= info.Count {
			break // We've reached or exceeded the total count
		}
	}

	// 🚀 性能优化：智能缓存存储（类似123网盘的实现）
	if limit == defaultListChunkSize && !filesOnly && len(allFiles) > 0 {
		cacheKey := fmt.Sprintf("listall_%s_%d_%v", dirID, limit, filesOnly)
		f.listAllCache.Put(cacheKey, allFiles)
		// 💾 同时保存到持久化缓存
		f.saveListAllCacheEntry(cacheKey, allFiles)
		fs.Debugf(f, "💾 listAll结果已缓存: dirID=%s (%d个文件)", dirID, len(allFiles))

		// 🧠 智能缓存：如果返回的文件数少于limit，说明是完整目录，额外缓存
		if len(allFiles) < limit {
			fullCacheKey := fmt.Sprintf("listall_full_%s", dirID)
			f.listAllCache.Put(fullCacheKey, allFiles)
			f.saveListAllCacheEntry(fullCacheKey, allFiles)
			fs.Debugf(f, "💾 listAll完整目录已缓存: dirID=%s (%d个文件)", dirID, len(allFiles))
		}
	}

	return found, nil
}

// clearListAllCache 清除指定目录的listAll缓存
func (f *Fs) clearListAllCache(dirID string, reason string) {
	cacheKey := fmt.Sprintf("listall_%s_%d_%v", dirID, defaultListChunkSize, false)
	if f.listAllCache.Delete(cacheKey) {
		fs.Debugf(f, "🗑️ 清除listAll缓存: dirID=%s (%s)", dirID, reason)
	}
}

// updateAPILimitStats 更新API限流统计信息
func (f *Fs) updateAPILimitStats() {
	f.apiLimitMu.Lock()
	defer f.apiLimitMu.Unlock()

	now := time.Now()

	// 如果距离上次API限流错误超过5分钟，重置计数器
	if now.Sub(f.lastAPILimitTime) > 5*time.Minute {
		f.consecutiveAPILimit = 0
	}

	f.consecutiveAPILimit++
	f.lastAPILimitTime = now

	fs.Debugf(f, "📊 API限流统计: 连续错误=%d, 当前QPS=%.1f",
		f.consecutiveAPILimit, f.currentQPSLimit)
}

// adjustQPSLimit 根据API限流情况动态调整QPS限制
func (f *Fs) adjustQPSLimit() time.Duration {
	f.apiLimitMu.Lock()
	defer f.apiLimitMu.Unlock()

	// 根据连续错误次数调整QPS限制
	if f.consecutiveAPILimit >= 3 {
		// 连续3次以上错误，大幅降低QPS
		f.currentQPSLimit = math.Max(0.5, f.currentQPSLimit*0.5)
	} else if f.consecutiveAPILimit >= 2 {
		// 连续2次错误，适度降低QPS
		f.currentQPSLimit = math.Max(1.0, f.currentQPSLimit*0.7)
	}

	// 计算新的最小睡眠时间
	newMinSleep := time.Duration(float64(time.Second) / f.currentQPSLimit)

	fs.Debugf(f, "🚦 动态调整QPS: %.1f -> 最小间隔=%v", f.currentQPSLimit, newMinSleep)

	return newMinSleep
}

// calculateRetryDelay 计算智能重试延迟
func (f *Fs) calculateRetryDelay(attempt int) time.Duration {
	baseDelay := time.Duration(defaultRetryBaseDelay)

	// 指数退避：baseDelay * 2^attempt，最大不超过60秒
	delay := baseDelay * time.Duration(1<<uint(attempt))
	if delay > 60*time.Second {
		delay = 60 * time.Second
	}

	// 添加随机抖动，避免雷群效应
	jitter := time.Duration(time.Now().UnixNano()%1000) * time.Millisecond
	return delay + jitter
}

// handleAPILimitError 处理API限制错误，实现智能重试机制
func (f *Fs) handleAPILimitError(ctx context.Context, err error, opts *rest.Opts, info *FileList, dirID string) error {
	// Check if it's an API limit error
	if !fserrors.IsRetryError(err) {
		return fmt.Errorf("OpenAPI list failed for dir %s: %w", dirID, err)
	}

	// 更新API限流统计
	f.updateAPILimitStats()

	// 智能重试默认启用
	return f.handleAPILimitErrorWithSmartRetry(ctx, err, opts, info, dirID)
}

// handleAPILimitErrorWithSmartRetry 使用智能重试机制处理API限流错误
func (f *Fs) handleAPILimitErrorWithSmartRetry(ctx context.Context, err error, opts *rest.Opts, info *FileList, dirID string) error {
	fs.Infof(f, "🧠 启用智能重试处理API限流错误...")

	for attempt := 0; attempt < defaultMaxRetryAttempts; attempt++ {
		// 动态调整QPS限制
		newMinSleep := f.adjustQPSLimit()

		// 计算重试延迟
		retryDelay := f.calculateRetryDelay(attempt)

		fs.Infof(f, "🔄 智能重试 %d/%d: 等待=%v, 新QPS间隔=%v",
			attempt+1, defaultMaxRetryAttempts, retryDelay, newMinSleep)

		// 等待重试延迟
		select {
		case <-time.After(retryDelay):
			// 继续重试
		case <-ctx.Done():
			return fmt.Errorf("context cancelled during smart retry: %w", ctx.Err())
		}

		// 执行重试
		retryErr := f.CallOpenAPI(ctx, opts, nil, info, false)
		if retryErr == nil {
			fs.Infof(f, "✅ 智能重试成功 (尝试 %d/%d)", attempt+1, defaultMaxRetryAttempts)

			// 重试成功，重置连续错误计数
			f.apiLimitMu.Lock()
			f.consecutiveAPILimit = 0
			f.apiLimitMu.Unlock()

			return nil
		}

		// 检查是否仍然是API限流错误
		if !fserrors.IsRetryError(retryErr) {
			return fmt.Errorf("OpenAPI list failed for dir %s with non-retryable error: %w", dirID, retryErr)
		}

		fs.Debugf(f, "⚠️ 智能重试 %d/%d 失败，继续尝试...", attempt+1, defaultMaxRetryAttempts)
	}

	return fmt.Errorf("OpenAPI list failed for dir %s after %d smart retries: %w", dirID, defaultMaxRetryAttempts, err)
}

// makeDir creates a directory using OpenAPI.
func (f *Fs) makeDir(ctx context.Context, pid, name string) (info *NewDir, err error) {
	if f.isShare {
		return nil, errors.New("makeDir unsupported for shared filesystem")
	}
	form := url.Values{}
	form.Set("pid", pid)
	form.Set("file_name", f.opt.Enc.FromStandardName(name))

	opts := rest.Opts{
		Method: "POST",
		Path:   "/open/folder/add",
		Body:   strings.NewReader(form.Encode()), // Send as form data in body
		ExtraHeaders: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
		},
	}

	info = new(NewDir)
	err = f.CallOpenAPI(ctx, &opts, nil, info, false)
	if err != nil {
		// Check for specific "already exists" error code from OpenAPI
		// Assuming code 20004 or similar based on traditional API
		if info.ErrCode() == 20004 || strings.Contains(info.ErrMsg(), "exists") || strings.Contains(info.ErrMsg(), "已存在") {
			// Try to find the existing directory's ID
			existingID, found, findErr := f.FindLeaf(ctx, pid, f.opt.Enc.FromStandardName(name))
			if findErr == nil && found {
				// Return info for the existing directory
				return &NewDir{
					OpenAPIBase: OpenAPIBase{State: true}, // Mark as success
					Data: &NewDirData{
						FileID:   existingID,
						FileName: name,
					},
				}, fs.ErrorDirExists // Return specific error
			}
			// If finding fails, return the original Mkdir error
			return nil, fmt.Errorf("makeDir failed and could not find existing dir: %w", err)
		}
		return nil, fmt.Errorf("OpenAPI makeDir failed: %w", err)
	}
	// Ensure FileID is populated
	if info.Data == nil || info.Data.FileID == "" {
		return nil, errors.New("OpenAPI makeDir response missing file_id")
	}
	return info, nil
}

// renameFile renames a file or folder using OpenAPI.
func (f *Fs) renameFile(ctx context.Context, fid, newName string) (err error) {
	if f.isShare {
		return errors.New("renameFile unsupported for shared filesystem")
	}
	form := url.Values{}
	form.Set("file_id", fid)
	form.Set("file_name", f.opt.Enc.FromStandardName(newName))

	opts := rest.Opts{
		Method: "POST",
		Path:   "/open/ufile/update", // Endpoint for renaming/starring
		Body:   strings.NewReader(form.Encode()),
		ExtraHeaders: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
		},
	}

	var baseResp OpenAPIBase
	err = f.CallOpenAPI(ctx, &opts, nil, &baseResp, false)
	if err != nil {
		return fmt.Errorf("OpenAPI rename failed for ID %s: %w", fid, err)
	}
	return nil
}

// deleteFiles deletes files or folders by ID using OpenAPI.
func (f *Fs) deleteFiles(ctx context.Context, fids []string) (err error) {
	if f.isShare {
		return errors.New("deleteFiles unsupported for shared filesystem")
	}
	if len(fids) == 0 {
		return nil
	}
	form := url.Values{}
	form.Set("file_ids", strings.Join(fids, ","))
	// parent_id is optional according to docs

	opts := rest.Opts{
		Method: "POST",
		Path:   "/open/ufile/delete",
		Body:   strings.NewReader(form.Encode()),
		ExtraHeaders: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
		},
	}

	var baseResp OpenAPIBase
	err = f.CallOpenAPI(ctx, &opts, nil, &baseResp, false)
	if err != nil {
		// Check for "not found" errors if possible, otherwise return generic error
		return fmt.Errorf("OpenAPI delete failed for IDs %v: %w", fids, err)
	}
	return nil
}

// moveFiles moves files or folders by ID using OpenAPI.
func (f *Fs) moveFiles(ctx context.Context, fids []string, pid string) (err error) {
	if f.isShare {
		return errors.New("moveFiles unsupported for shared filesystem")
	}
	if len(fids) == 0 {
		return nil
	}
	form := url.Values{}
	form.Set("file_ids", strings.Join(fids, ","))
	form.Set("to_cid", pid) // Target directory ID

	opts := rest.Opts{
		Method: "POST",
		Path:   "/open/ufile/move",
		Body:   strings.NewReader(form.Encode()),
		ExtraHeaders: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
		},
	}

	var baseResp OpenAPIBase
	err = f.CallOpenAPI(ctx, &opts, nil, &baseResp, false)
	if err != nil {
		return fmt.Errorf("OpenAPI move failed for IDs %v to %s: %w", fids, pid, err)
	}
	return nil
}

// copyFiles copies files or folders by ID using OpenAPI.
func (f *Fs) copyFiles(ctx context.Context, fids []string, pid string) (err error) {
	if f.isShare {
		return errors.New("copyFiles unsupported for shared filesystem")
	}
	if len(fids) == 0 {
		return nil
	}
	form := url.Values{}
	form.Set("file_id", strings.Join(fids, ",")) // Note: param name is file_id (singular)
	form.Set("pid", pid)                         // Target directory ID
	form.Set("nodupli", "0")                     // Allow duplicates by default

	opts := rest.Opts{
		Method: "POST",
		Path:   "/open/ufile/copy",
		Body:   strings.NewReader(form.Encode()),
		ExtraHeaders: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
		},
	}

	var baseResp OpenAPIBase
	err = f.CallOpenAPI(ctx, &opts, nil, &baseResp, false)
	if err != nil {
		return fmt.Errorf("OpenAPI copy failed for IDs %v to %s: %w", fids, pid, err)
	}
	return nil
}

// indexInfo gets user quota info (Traditional API).
func (f *Fs) indexInfo(ctx context.Context) (data *IndexData, err error) {
	if f.isShare {
		return nil, errors.New("indexInfo unsupported for shared filesystem")
	}
	opts := rest.Opts{
		Method: "GET",
		Path:   "/files/index_info", // Traditional endpoint
	}

	var info *IndexInfo
	// Use traditional API call
	err = f.CallTraditionalAPI(ctx, &opts, nil, &info, false) // Not skipping encryption
	if err != nil {
		return nil, fmt.Errorf("traditional indexInfo failed: %w", err)
	}
	if info.Data == nil {
		return nil, errors.New("traditional indexInfo returned no data")
	}
	return info.Data, nil
}

// getDownloadURL gets a download URL using OpenAPI.
func (f *Fs) getDownloadURL(ctx context.Context, pickCode string) (durl *DownloadURL, err error) {
	return f.getDownloadURLWithForce(ctx, pickCode, false)
}

// getDownloadURLWithForce gets a download URL using OpenAPI with optional cache bypass.
func (f *Fs) getDownloadURLWithForce(ctx context.Context, pickCode string, forceRefresh bool) (durl *DownloadURL, err error) {
	originalPickCode := pickCode
	pickCode, err = f.validateAndCorrectPickCode(ctx, pickCode)
	if err != nil {
		return nil, fmt.Errorf("pickCode validation failed: %w", err)
	}

	if originalPickCode != pickCode {
		fs.Infof(f, "✅ pickCode已纠正: %s -> %s", originalPickCode, pickCode)
	}

	// Download URL cache removed, call API directly
	if forceRefresh {
		fs.Debugf(f, "🔄 115强制刷新下载URL: pickCode=%s", pickCode)
	} else {
		fs.Debugf(f, "📥 115网盘获取下载URL: pickCode=%s", pickCode)
	}

	form := url.Values{}
	form.Set("pick_code", pickCode)

	opts := rest.Opts{
		Method: "POST",
		Path:   "/open/ufile/downurl",
		Body:   strings.NewReader(form.Encode()),
		ExtraHeaders: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
		},
	}

	var respData OpenAPIDownloadResp
	// 使用专用的下载URL调速器，防止API调用频率过高
	err = f.CallDownloadURLAPI(ctx, &opts, nil, &respData, false)
	if err != nil {
		return nil, fmt.Errorf("OpenAPI downurl failed for pickcode %s: %w", pickCode, err)
	}

	// 使用新的GetDownloadInfo方法处理map和array两种格式
	downInfo, err := respData.GetDownloadInfo()
	if err != nil {
		fs.Debugf(f, "❌ 115网盘下载URL响应解析失败: %v, 原始数据: %s", err, string(respData.Data))
		if string(respData.Data) == "[]" || string(respData.Data) == "{}" {
			return nil, fmt.Errorf("115 API returned empty data, file may be deleted or access denied. pickCode: %s", pickCode)
		}

		return nil, fmt.Errorf("failed to parse download URL response for pickcode %s: %w", pickCode, err)
	}

	if downInfo == nil {
		return nil, fmt.Errorf("no download info found for pickcode %s", pickCode)
	}

	fs.Debugf(f, "✅ 115网盘成功获取下载URL: pickCode=%s, fileName=%s, fileSize=%d",
		pickCode, downInfo.FileName, int64(downInfo.FileSize))

	// 设置创建时间，用于fallback过期检测
	downloadURL := downInfo.URL
	downloadURL.CreatedAt = time.Now()

	return &downloadURL, nil
}

// validateAndCorrectPickCode 验证并修正pickCode格式
func (f *Fs) validateAndCorrectPickCode(ctx context.Context, pickCode string) (string, error) {
	// 空pickCode直接返回错误
	if pickCode == "" {
		return "", fmt.Errorf("empty pickCode provided")
	}
	isAllDigits := true
	for _, r := range pickCode {
		if r < '0' || r > '9' {
			isAllDigits = false
			break
		}
	}

	// 如果是疑似文件ID，尝试获取正确的pickCode
	if len(pickCode) > 15 && isAllDigits {
		fs.Debugf(f, "⚠️ 检测到疑似文件ID而非pickCode: %s", pickCode)

		correctPickCode, err := f.getPickCodeByFileID(ctx, pickCode)
		if err != nil {
			return "", fmt.Errorf("invalid pickCode format (appears to be file ID): %s, failed to get correct pickCode: %w", pickCode, err)
		}

		fs.Debugf(f, "✅ 成功通过文件ID获取正确的pickCode: %s -> %s", pickCode, correctPickCode)
		return correctPickCode, nil
	}

	// pickCode格式看起来正确，直接返回
	return pickCode, nil
}

// CallTraditionalAPIWithResp is a variant that returns the http.Response for cookie access.
func (f *Fs) CallTraditionalAPIWithResp(ctx context.Context, opts *rest.Opts, request any, response any, skipEncrypt bool) (*http.Response, error) {
	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = traditionalRootURL
	}

	var httpResp *http.Response
	err := f.pacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {

		// Make the API call (with or without encryption)
		var apiErr error
		httpResp, apiErr = f.executeTraditionalAPICall(ctx, opts, request, response, skipEncrypt)

		// Check for retryable errors
		retryNeeded, retryErr := shouldRetry(ctx, httpResp, apiErr)
		if retryNeeded {
			fs.Debugf(f, "🔄 调速器: 传统调用响应需要底层重试 (错误: %v)", retryErr)
			return true, retryErr // Signal globalPacer to retry
		}

		// Handle non-retryable errors
		if apiErr != nil {
			fs.Debugf(f, "❌ 调速器: 传统调用响应遇到永久错误: %v", apiErr)
			return false, apiErr
		}

		// Check API-level errors in response struct
		if errResp := f.checkResponseForAPIErrors(response); errResp != nil {
			fs.Debugf(f, "❌ 调速器: 传统调用响应遇到永久API错误: %v", errResp)
			return false, errResp
		}

		fs.Debugf(f, "✅ 调速器: 传统调用响应成功")
		return false, nil // Success, don't retry
	})

	return httpResp, err
}

// Globals
const (
	cachePrefix  = "rclone-115-sha1sum-"
	md5Salt      = "Qclm8MGWUv59TnrR0XPg"     // Salt for traditional token generation
	OSSRegion    = "cn-shenzhen"              // Default OSS region
	OSSUserAgent = "aliyun-sdk-android/2.9.1" // Keep or update as needed
)

// RereadableObject represents a source that can be re-opened for multiple reads
// 关键修复：实现Accounter接口，确保Account对象能被正确识别
type RereadableObject struct {
	src        fs.ObjectInfo
	ctx        context.Context
	currReader io.Reader
	options    []fs.OpenOption
	size       int64                // Track the source size for accounting
	acc        *accounting.Account  // Store the accounting object
	fsInfo     fs.Info              // Source filesystem info
	transfer   *accounting.Transfer // Keep track of the transfer object
	// 新增：支持共享主传输的会计系统
	parentTransfer      *accounting.Transfer // 主传输对象，用于统一进度显示
	useParentAccounting bool                 // 是否使用父传输的会计系统
}

// NewRereadableObject creates a wrapper that supports re-opening the source
func NewRereadableObject(ctx context.Context, src fs.ObjectInfo, options ...fs.OpenOption) (*RereadableObject, error) {
	// Try to extract the filesystem info from the source
	var fsInfo fs.Info

	// Try different ways of getting the filesystem info
	if o, ok := src.(fs.Object); ok {
		// If it's a direct Object
		fsInfo = o.Fs()
	} else if unwrapped := fs.UnWrapObjectInfo(src); unwrapped != nil {
		// Try to unwrap it first, only if it actually unwrapped something
		fsInfo = unwrapped.Fs()
	} else if i, ok := src.(interface{ Fs() fs.Info }); ok {
		// If it has an Fs() method that returns fs.Info
		fsInfo = i.Fs()
	}

	r := &RereadableObject{
		src:     src,
		ctx:     ctx,
		options: options,
		size:    src.Size(), // Remember the size for accounting
		fsInfo:  fsInfo,     // Store the filesystem info
	}

	// Open it once to make sure it works and assign to currReader
	reader, err := r.Open()
	if err != nil {
		return nil, err
	}
	r.currReader = reader
	return r, nil
}

// retryWithExponentialBackoff provides a standard implementation of sophisticated retry logic
// with exponential backoff for handling rate limiting issues.
func retryWithExponentialBackoff(
	ctx context.Context,
	description string, // Description of the operation being retried (for logging)
	loggingObj any, // Object to log against
	operation func() error, // Operation to execute and retry
	maxRetries int, // Maximum number of retries
	initialDelay time.Duration, // Initial delay between retries
	maxDelay time.Duration, // Maximum delay between retries
	maxElapsedTime time.Duration, // Maximum total retry time
) error {
	// Set up simple exponential backoff
	var retryCount int
	currentDelay := initialDelay

	// Create retry context with timeout based on maxElapsedTime
	// Give some buffer beyond MaxElapsedTime for the final attempt + operation time
	retryCtx, cancelRetry := context.WithTimeout(ctx, maxElapsedTime+maxDelay+10*time.Second)
	defer cancelRetry()

	// Define the retry wrapper
	retryOperation := func() error {
		// Check context cancellation before proceeding
		if err := retryCtx.Err(); err != nil {
			fs.Debugf(loggingObj, "❌ 重试上下文已取消 '%s': %v", description, err)
			return fmt.Errorf("retry cancelled for '%s': %w", description, err)
		}

		if retryCount > 0 {
			// Check context cancellation again before sleeping
			if err := retryCtx.Err(); err != nil {
				fs.Debugf(loggingObj, "❌ 延迟前重试上下文已取消 '%s': %v", description, err)
				return fmt.Errorf("retry cancelled before delay for '%s': %w", description, err)
			}

			// Calculate next delay with exponential backoff
			if currentDelay > maxDelay {
				currentDelay = maxDelay
			}

			// Use more visible logging for retries
			fs.Logf(loggingObj, "Retrying '%s': Waiting %v before retry %d/%d",
				description, currentDelay, retryCount+1, maxRetries)

			// Wait for the delay, but honor context cancellation
			select {
			case <-time.After(currentDelay):
				// Continue after delay
			case <-retryCtx.Done():
				fs.Logf(loggingObj, "Retry context cancelled while waiting for delay in '%s': %v", description, retryCtx.Err())
				return fmt.Errorf("retry cancelled while waiting for delay in '%s': %w", description, retryCtx.Err())
			}

			// Exponential backoff for next iteration
			currentDelay = time.Duration(float64(currentDelay) * 2.5)
			if currentDelay > maxDelay {
				currentDelay = maxDelay
			}
		}
		retryCount++ // Increment retry count *after* the first attempt (retryCount=0)

		// Execute the actual operation
		err := operation()

		// On success, return nil to break the retry loop
		if err == nil {
			return nil
		}

		// Check if we've hit max retries (retryCount is now 1 for the first attempt, so compare with >= maxRetries)
		if retryCount >= maxRetries {
			fs.Logf(loggingObj, "Giving up '%s' after %d attempts: %v",
				description, retryCount, err)
			return fmt.Errorf("giving up '%s' after %d attempts: %w", description, retryCount, err)
		}

		// Check if this is a rate limit error or other error that we should retry
		shouldRetryErr, classification := shouldRetry(retryCtx, nil, err) // Pass retryCtx
		if !shouldRetryErr {
			// Non-retryable error
			fs.Debugf(loggingObj, "❌ 不可重试错误 (%s) 当 '%s': %v", classification, description, err)
			return err // Wrap in Permanent to stop retries
		}

		// Log retryable errors
		fs.Debugf(loggingObj, "🔄 可重试错误 (%s) 第%d次尝试 '%s'，将重试: %v", classification, retryCount, description, err)

		return err // Return the retryable error to trigger the next retry
	}

	// Execute the retry logic
	return retryOperation()
}

// Open (re)opens the source file
func (r *RereadableObject) Open() (io.Reader, error) {
	// Close existing reader if it's a ReadCloser
	if r.currReader != nil {
		if rc, ok := r.currReader.(io.ReadCloser); ok {
			_ = rc.Close() // Ignore errors on close
		}
	}

	// Try to get the original fs.Object if it's an fs.Object
	// This is for supporting direct methods like RangeSeek later
	obj := unWrapObjectInfo(r.src)
	if obj != nil {
		var rc io.ReadCloser
		var err error

		// 使用统一调速器
		var pacer *fs.Pacer
		if fsObj, ok := obj.Fs().(*Fs); ok {
			pacer = fsObj.pacer // 使用统一调速器
		}

		// Set retry parameters
		maxRetries := 15
		initialDelay := 1 * time.Second
		maxDelay := 300 * time.Second
		maxElapsedTime := 30 * time.Minute

		// Define the operation to retry
		openOperation := func() error {
			var openErr error
			if pacer != nil {
				// Apply pacer to handle rate limiting (429 errors)
				err = pacer.Call(func() (bool, error) {
					rc, openErr = obj.Open(r.ctx, r.options...)
					if openErr != nil {
						// Check for 429 rate limit error
						retry, _ := shouldRetry(r.ctx, nil, openErr)
						if retry {
							fs.Debugf(obj, "🔄 调速器: 速率限制错误后重试Open: %v", openErr)
							return true, openErr // Let pacer handle retry timing
						}
					}
					return false, openErr
				})
			} else {
				// Fall back to direct open if we can't use pacer
				rc, err = obj.Open(r.ctx, r.options...)
			}
			return err
		}

		// Retry with exponential backoff
		err = retryWithExponentialBackoff(
			r.ctx,
			"reopening object",
			obj,
			openOperation,
			maxRetries,
			initialDelay,
			maxDelay,
			maxElapsedTime,
		)

		if err != nil {
			return nil, fmt.Errorf("failed to reopen source object after retries: %w", err)
		}

		// Check if we already have an accounting wrapper
		// If this is a fresh Open, extract the existing accounting if any
		if r.acc == nil {
			// Get the underlying accounting.Account if there is one
			// The accounting system might wrap our reader with its own accounting
			// We need to preserve this for speed tracking to work
			_, acc := accounting.UnWrapAccounting(rc)
			if acc != nil {
				r.acc = acc
				fs.Debugf(nil, "💾 为RereadableObject保留现有会计包装器")
			}
		}

		// Always wrap with accounting, even if we found an existing one
		// The accounting system will handle this properly
		stats := accounting.Stats(r.ctx)
		if stats == nil {
			fs.Debugf(nil, "⚠️ 未找到Stats - 会计可能无法正常工作")
			r.currReader = rc
			return rc, nil
		}

		name := ""
		if obj != nil {
			name = obj.Remote()
		} else if r.src != nil {
			name = r.src.String()
		}

		// Try to get real Fs objects if available by downcasting fs.Info to fs.Fs
		var srcFs, dstFs fs.Fs
		if r.fsInfo != nil {
			if f, ok := r.fsInfo.(fs.Fs); ok {
				srcFs = f
			}
		}

		// 修复：优先使用父传输的会计系统，实现统一进度显示
		var accReader *accounting.Account
		if r.useParentAccounting && r.parentTransfer != nil {
			// 使用父传输的会计系统，实现统一进度显示
			fs.Debugf(nil, "📊 RereadableObject使用父传输会计: %s", name)
			accReader = r.parentTransfer.Account(r.ctx, rc).WithBuffer()
			r.transfer = r.parentTransfer // 引用父传输
		} else {
			// 关键修复：检测跨云传输场景
			if srcFs != nil && srcFs.Name() == "123" {
				fs.Debugf(nil, "☁️ 检测到123跨云传输场景")
			}

			// 创建独立传输（回退方案）
			if r.transfer == nil {
				fs.Debugf(nil, "📊 RereadableObject创建独立传输: %s", name)
				r.transfer = stats.NewTransferRemoteSize(name, r.size, srcFs, dstFs)
			}
			accReader = r.transfer.Account(r.ctx, rc).WithBuffer()
		}

		r.currReader = accReader

		// Extract the accounting object for later use
		_, r.acc = accounting.UnWrapAccounting(accReader)
		fs.Debugf(nil, "📊 RereadableObject Account状态: acc=%v, currReader类型=%T",
			r.acc != nil, r.currReader)

		return r.currReader, nil
	}

	return nil, errors.New("source doesn't support reopening")
}

// Read reads from the current reader
func (r *RereadableObject) Read(p []byte) (n int, err error) {
	if r.currReader == nil {
		return 0, errors.New("no current reader available")
	}

	// 关键修复：确保读取操作能够被Account对象正确跟踪
	n, err = r.currReader.Read(p)

	// 调试信息：记录读取操作
	if n > 0 {
		fs.Debugf(nil, "📖 RereadableObject读取: %d字节, Account=%v", n, r.acc != nil)
	}

	return n, err
}

// 关键修复：实现Accounter接口，确保Account对象能被正确识别

// OldStream returns the underlying stream (实现Accounter接口)
func (r *RereadableObject) OldStream() io.Reader {
	if r.currReader != nil {
		return r.currReader
	}
	return nil
}

// SetStream sets the underlying stream (实现Accounter接口)
func (r *RereadableObject) SetStream(in io.Reader) {
	r.currReader = in
}

// WrapStream wraps the stream with accounting (实现Accounter接口)
func (r *RereadableObject) WrapStream(in io.Reader) io.Reader {
	if r.acc != nil {
		return r.acc.WrapStream(in)
	}
	return in
}

func (r *RereadableObject) GetAccount() *accounting.Account {
	return r.acc
}

type AccountedFileReader struct {
	file    *os.File
	acc     *accounting.Account
	size    int64
	name    string
	cleanup func()
}

// NewAccountedFileReader 创建带Account对象的文件读取器
func NewAccountedFileReader(ctx context.Context, file *os.File, size int64, name string, cleanup func()) *AccountedFileReader {

	return &AccountedFileReader{
		file:    file,
		acc:     nil, // 不创建Account对象，避免重复计数
		size:    size,
		name:    name,
		cleanup: cleanup,
	}
}

func (a *AccountedFileReader) Read(p []byte) (n int, err error) {
	return a.file.Read(p)
}

// Close 实现io.Closer接口
func (a *AccountedFileReader) Close() error {
	err := a.file.Close()
	if a.cleanup != nil {
		a.cleanup()
	}
	return err
}

func (a *AccountedFileReader) GetAccount() *accounting.Account {
	return nil
}

// Seek 实现io.Seeker接口
func (a *AccountedFileReader) Seek(offset int64, whence int) (int64, error) {
	return a.file.Seek(offset, whence)
}

// MarkComplete marks the transfer as complete with success
func (r *RereadableObject) MarkComplete(ctx context.Context) {
	if r.transfer != nil {
		// Mark the transfer as successful and done
		r.transfer.Done(ctx, nil)
		r.transfer = nil // Clear to avoid double completion
	}
}

// Close closes the current reader if it's a ReadCloser
func (r *RereadableObject) Close() error {
	var err error

	// Close the current reader
	if r.currReader != nil {
		if rc, ok := r.currReader.(io.ReadCloser); ok {
			err = rc.Close()
		}
		r.currReader = nil
	}

	// Don't finalize the transfer here - this is just closing a read
	// The transfer should be finalized by the caller when the operation is complete
	// via MarkComplete() or by creating a new transfer

	return err
}

// getUploadBasicInfo retrieves userkey using the traditional API (needed for traditional initUpload signature).
func (f *Fs) getUploadBasicInfo(ctx context.Context) error {
	if f.userkey != "" {
		return nil // Already have it
	}
	opts := rest.Opts{
		Method:  "GET",
		RootURL: "https://proapi.115.com/app/uploadinfo", // Traditional endpoint
	}
	var info *UploadBasicInfo

	// Use traditional API call (requires cookie)
	// Assume no encryption needed for this GET request? Let's try skipping.
	err := f.CallTraditionalAPI(ctx, &opts, nil, &info, true) // skipEncrypt = true
	if err != nil {
		return fmt.Errorf("traditional uploadinfo call failed: %w", err)
	}
	if err = info.Err(); err != nil {
		return fmt.Errorf("traditional uploadinfo API error: %s (%d)", info.Error, info.Errno)
	}
	userID := info.UserID.String()
	// Verify userID matches the one from login if possible
	if f.userID != "" && userID != f.userID {
		fs.Logf(f, "Warning: UserID from uploadinfo (%s) differs from login UserID (%s)", userID, f.userID)
		// Don't fail, but log discrepancy. Use the login UserID.
	} else if f.userID == "" {
		f.userID = userID // Set userID if not already set
	}

	if info.Userkey == "" {
		return errors.New("traditional uploadinfo returned empty userkey")
	}
	f.userkey = info.Userkey
	return nil
}

// bufferIOWithAccount handles buffering with explicit Account object preservation
func bufferIOWithAccount(f *Fs, in io.Reader, size, threshold int64, account *accounting.Account) (out io.Reader, cleanup func(), err error) {
	cleanup = func() {} // Default no-op cleanup

	// If NoBuffer option is enabled, don't buffer to disk or memory
	if f.opt.NoBuffer {
		// Just return the original reader
		fs.Debugf(f, "⚠️ 由于no_buffer选项跳过缓冲")
		return in, cleanup, nil
	}

	// If size is unknown or below threshold, read into memory
	if size < 0 || size <= threshold {
		inData, err := io.ReadAll(in)
		if err != nil {
			return nil, cleanup, fmt.Errorf("failed to read input to memory buffer: %w", err)
		}
		return bytes.NewReader(inData), cleanup, nil
	}

	// Above threshold: buffer to temporary file
	tempFile, err := os.CreateTemp("", "rclone-115-buffer-*.tmp")
	if err != nil {
		return nil, cleanup, fmt.Errorf("failed to create temporary file: %w", err)
	}

	// Copy input to temporary file
	written, err := io.Copy(tempFile, in)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return nil, cleanup, fmt.Errorf("failed to copy input to temporary file: %w", err)
	}

	// Seek back to the beginning of the temp file
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return nil, cleanup, fmt.Errorf("failed to seek temp file: %w", err)
	}

	// 关键修复：如果提供了Account对象，用AccountedFileReader包装临时文件
	if account != nil {
		fs.Debugf(f, "📊 用AccountedFileReader包装临时文件，保持Account对象传递")
		cleanupFunc := func() {
			closeErr := tempFile.Close()
			removeErr := os.Remove(tempFile.Name())
			if closeErr != nil {
				fs.Errorf(nil, "❌ 关闭临时文件失败 %s: %v", tempFile.Name(), closeErr)
			}
			if removeErr != nil {
				fs.Errorf(nil, "❌ 删除临时文件失败 %s: %v", tempFile.Name(), removeErr)
			} else {
				fs.Debugf(nil, "🧹 清理临时文件: %s", tempFile.Name())
			}
		}

		accountedReader := &AccountedFileReader{
			file:    tempFile,
			acc:     account,
			size:    written,
			name:    "buffered_temp_file_with_account",
			cleanup: cleanupFunc,
		}

		newCleanup := func() {
		}

		return accountedReader, newCleanup, nil
	}

	// Set up cleanup function for regular case
	cleanup = func() {
		tempFile.Close()
		os.Remove(tempFile.Name())
	}

	fs.Debugf(f, "💾 缓冲 %d 字节到临时文件", written)
	return tempFile, cleanup, nil
}

// bufferIOwithSHA1 buffers the input and calculates its SHA-1 hash.
// Returns the SHA-1 hash, the potentially buffered reader, and a cleanup function.
func bufferIOwithSHA1(f *Fs, in io.Reader, src fs.ObjectInfo, size, threshold int64, ctx context.Context, options ...fs.OpenOption) (sha1sum string, out io.Reader, cleanup func(), err error) {
	cleanup = func() {} // Default no-op cleanup

	// First check if the source object already has the SHA1 hash
	if srcObj, ok := src.(fs.Object); ok {
		hashVal, hashErr := srcObj.Hash(ctx, hash.SHA1)
		if hashErr == nil && hashVal != "" {
			fs.Debugf(srcObj, "🔐 使用预计算的SHA1: %s", hashVal)
			return hashVal, in, cleanup, nil
		}
	}

	// 🌊 StreamHashMode检查：避免缓冲到临时文件
	if f.opt.StreamHashMode {
		fs.Debugf(f, "🌊 StreamHashMode启用，跳过SHA1缓冲直接返回原始流")
		// 在流式哈希模式下，不在这里计算SHA1，让上层流式哈希传输函数处理
		// 这避免了内存溢出和重复哈希计算
		return "", in, cleanup, nil
	}

	// If NoBuffer option is enabled, calculate the SHA1 using a non-buffering approach
	if f.opt.NoBuffer {
		fs.Debugf(f, "🔐 由于no_buffer选项无缓冲计算SHA1")

		// Create a rereadable source if it's not already one
		var rereadable *RereadableObject
		if ro, ok := in.(*RereadableObject); ok {
			rereadable = ro
		} else {
			// Try to create a new rereadable source
			ro, roErr := NewRereadableObject(ctx, src, options...)
			if roErr != nil {
				return "", in, cleanup, fmt.Errorf("failed to create rereadable object: %w", roErr)
			}
			rereadable = ro
			cleanup = func() { _ = rereadable.Close() }
		}

		// Calculate SHA1 hash
		hasher := sha1.New()
		if _, hashErr := io.Copy(hasher, rereadable); hashErr != nil {
			return "", in, cleanup, fmt.Errorf("failed to calculate SHA1: %w", hashErr)
		}
		sha1Hash := fmt.Sprintf("%x", hasher.Sum(nil))

		// Reopen the source for the actual upload
		var newReader io.Reader
		var reopenErr error

		// Set retry parameters for reopening
		maxRetries := 12
		initialDelay := 1 * time.Second
		maxDelay := 180 * time.Second
		maxElapsedTime := 20 * time.Minute

		// Define the reopening operation
		reopenOperation := func() error {
			var openErr error
			newReader, openErr = rereadable.Open()
			return openErr
		}

		// Retry with exponential backoff
		reopenErr = retryWithExponentialBackoff(
			ctx,
			"reopening after SHA1",
			f,
			reopenOperation,
			maxRetries,
			initialDelay,
			maxDelay,
			maxElapsedTime,
		)

		if reopenErr != nil {
			return "", in, cleanup, fmt.Errorf("failed to reopen source after SHA1 calculation: %w", reopenErr)
		}

		sha1sum = sha1Hash
		fs.Debugf(f, "✅ 无缓冲计算SHA1完成: %s", sha1sum)
		return sha1sum, newReader, cleanup, nil
	}

	// 使用标准SHA1计算，简化逻辑

	// Check and save Account object before creating TeeReader
	var originalAccount *accounting.Account
	if accountedReader, ok := in.(*AccountedFileReader); ok {
		originalAccount = accountedReader.GetAccount()
	}

	// 使用更简单可靠的方法：先缓冲数据，然后从缓冲区计算SHA1
	// 这避免了goroutine同步问题，特别是在跨网盘复制的复杂场景下

	// Buffer the input first
	out, cleanup, err = bufferIOWithAccount(f, in, size, threshold, originalAccount)
	if err != nil {
		return "", nil, cleanup, fmt.Errorf("failed to buffer input for SHA1 calculation: %w", err)
	}

	// Calculate SHA1 from the buffered data
	var sha1Hash string
	if seeker, ok := out.(io.Seeker); ok {
		// If the buffered output is seekable, calculate SHA1 from it
		hasher := sha1.New()
		_, err := io.Copy(hasher, out)
		if err != nil {
			return "", nil, cleanup, fmt.Errorf("failed to calculate SHA1 from buffered data: %w", err)
		}
		sha1Hash = fmt.Sprintf("%x", hasher.Sum(nil))

		// Seek back to the beginning for the actual upload
		_, err = seeker.Seek(0, io.SeekStart)
		if err != nil {
			return "", nil, cleanup, fmt.Errorf("failed to seek back after SHA1 calculation: %w", err)
		}
	} else {
		// If not seekable, we need to read the data twice
		// This should not happen with our current bufferIO implementation, but handle it gracefully
		return "", nil, cleanup, fmt.Errorf("buffered output is not seekable, cannot calculate SHA1")
	}

	sha1sum = sha1Hash
	fs.Debugf(f, "✅ 从缓冲数据计算SHA1完成: %s", sha1sum)
	return sha1sum, out, cleanup, nil
}

// initUploadOpenAPI calls the OpenAPI /open/upload/init endpoint.
func (f *Fs) initUploadOpenAPI(ctx context.Context, size int64, name, dirID, sha1sum, preSha1, signKey, signVal string) (*UploadInitInfo, error) {
	form := url.Values{}

	cleanName := f.opt.Enc.FromStandardName(name)
	form.Set("file_name", cleanName)
	form.Set("file_size", strconv.FormatInt(size, 10))
	// 根据115网盘官方API文档，target格式为"U_1_"+dirID
	form.Set("target", "U_1_"+dirID)
	if sha1sum != "" {
		form.Set("fileid", strings.ToUpper(sha1sum)) // fileid is the full SHA1
	}
	if preSha1 != "" {
		form.Set("preid", preSha1) // preid is the 128k SHA1
	}

	if signKey != "" && signVal != "" {
		form.Set("sign_key", signKey)
		form.Set("sign_val", signVal) // Value should be uppercase SHA1 of range
	}
	// 根据115网盘官方API文档，添加topupload参数
	// 0：单文件上传任务标识一条单独的文件上传记录
	form.Set("topupload", "0")

	// Log parameters for debugging, but mask sensitive values
	fs.Debugf(f, "Initializing upload for file_name=%q (cleaned: %q), size=%d, target=U_1_%s, has_fileid=%v, has_preid=%v, has_sign=%v",
		name, cleanName, size, dirID, sha1sum != "", preSha1 != "", signKey != "")

	// Log API parameters
	fs.Debugf(f, "API params: file_name=%q, file_size=%d, target=%q, has_fileid=%v, has_preid=%v",
		cleanName, size, "U_1_"+dirID, sha1sum != "", preSha1 != "")

	// Create request options
	opts := rest.Opts{
		Method: "POST",
		Path:   "/open/upload/init",
		Body:   strings.NewReader(form.Encode()),
		ExtraHeaders: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
		},
	}

	var info UploadInitInfo // Response structure includes nested Data for OpenAPI
	// 使用专用的上传调速器，优化上传初始化API调用频率
	err := f.CallUploadAPI(ctx, &opts, nil, &info, false)
	if err != nil {
		// Use standard error handling instead of hardcoded error checks

		// General error handling
		return nil, fmt.Errorf("OpenAPI initUpload failed: %w", err)
	}

	// Error checking is handled by CallOpenAPI using info.Err()
	// Log API response
	fs.Debugf(f, "API response: State=%v, Code=%d, Message=%q, Status=%d",
		info.State, info.ErrCode(), info.ErrMsg(), info.GetStatus())

	if info.Data == nil {
		// If Data is nil but call succeeded, maybe it's a 秒传 response where fields are top-level?
		if info.State {
			if info.GetFileID() != "" && info.GetPickCode() != "" {
				// This is likely a successful 秒传 response with top-level fields
				fs.Debugf(f, "Detected direct 秒传 success response with top-level fields")
				return &info, nil
			}
			// 修复：即使没有足够的顶级字段，也要检查是否有其他有用信息
			fs.Debugf(f, "API returned success but data incomplete, attempting to continue processing...")
			return &info, nil
		}

		// If state is false, CallOpenAPI should have returned an error, but let's add an extra check
		errMsg := info.ErrMsg()
		if info.ErrCode() != 0 || errMsg != "" {
			return nil, fmt.Errorf("OpenAPI initUpload failed with error code %d: %s",
				info.ErrCode(), errMsg)
		}

		return nil, errors.New("internal error: OpenAPI initUpload failed but CallOpenAPI returned no error")
	}

	// Log successful initialization
	statusMsg := "normal upload"
	if info.GetStatus() == 2 {
		statusMsg = "秒传 success"
	}
	fs.Debugf(f, "Upload initialized: status=%d (%s), bucket=%q, object=%q",
		info.GetStatus(), statusMsg, info.GetBucket(), info.GetObject())

	return &info, nil
}

// postUpload processes the JSON callback after an upload to OSS.
// The callback result from OSS SDK v2 is already a map[string]any.
func (f *Fs) postUpload(callbackResult map[string]any) (*CallbackData, error) {
	if callbackResult == nil {
		return nil, errors.New("received nil callback result from OSS")
	}

	// Check for standard OSS callback status if present
	if statusVal, ok := callbackResult["Status"]; ok {
		if statusStr, ok := statusVal.(string); ok && !strings.HasPrefix(statusStr, "OK") {
			// Try to get more info from the map
			errMsg := fmt.Sprintf("OSS callback failed with Status: %s", statusStr)
			if bodyVal, ok := callbackResult["body"]; ok {
				if bodyStr, ok := bodyVal.(string); ok {
					errMsg += fmt.Sprintf(", Body: %s", bodyStr)
				}
			}
			return nil, errors.New(errMsg)
		}
	}

	// Check for OpenAPI format (with state/code/message/data structure)
	var cbData *CallbackData

	// First, check if this is an OpenAPI format response
	if stateVal, ok := callbackResult["state"]; ok {
		// Check if the state indicates failure
		if state, ok := stateVal.(bool); ok && !state {
			// This is a failed OpenAPI response, extract error information
			var errCode int
			var errMsg string

			if codeVal, ok := callbackResult["code"]; ok {
				if code, ok := codeVal.(float64); ok {
					errCode = int(code)
				}
			}

			if msgVal, ok := callbackResult["message"]; ok {
				if msg, ok := msgVal.(string); ok {
					errMsg = msg
				}
			}

			// Handle specific error codes
			switch errCode {
			case 10002:
				return nil, fmt.Errorf("115 file validation failed (10002): %s - file may be corrupted during transfer, retry recommended", errMsg)
			case 10001:
				return nil, fmt.Errorf("115 upload parameter error (10001): %s", errMsg)
			default:
				return nil, fmt.Errorf("115 upload failed (error code: %d): %s", errCode, errMsg)
			}
		}

		// This could be a successful OpenAPI format response
		if dataVal, ok := callbackResult["data"]; ok {
			if dataMap, ok := dataVal.(map[string]any); ok {
				// Try to extract file_id and pick_code from the data field
				fileID, fileIDExists := dataMap["file_id"].(string)
				pickCode, pickCodeExists := dataMap["pick_code"].(string)

				if fileIDExists && pickCodeExists {
					// Create a CallbackData from the nested data map
					cbData = &CallbackData{
						FileID:   fileID,
						PickCode: pickCode,
					}

					// Copy other fields if they exist
					if fileName, ok := dataMap["file_name"].(string); ok {
						cbData.FileName = fileName
					}
					if fileSize, ok := dataMap["file_size"].(string); ok {
						if size, err := strconv.ParseInt(fileSize, 10, 64); err == nil {
							cbData.FileSize = Int64(size)
						}
					}
					if sha1, ok := dataMap["sha1"].(string); ok {
						cbData.Sha = sha1
					}

					// Log the values for debugging
					fs.Debugf(f, "📋 OpenAPI回调数据解析: file_id=%q, pick_code=%q", cbData.FileID, cbData.PickCode)

					// Early return if we successfully extracted data
					if cbData.FileID != "" && cbData.PickCode != "" {
						return cbData, nil
					}
				}
			}
		}
	}

	// If we couldn't extract from OpenAPI format, try traditional format
	// Need to marshal it back to JSON and then unmarshal to CallbackData for validation/typing
	callbackJSON, err := json.Marshal(callbackResult)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal OSS callback result: %w", err)
	}

	cbData = &CallbackData{}
	if err := json.Unmarshal(callbackJSON, cbData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal OSS callback result into CallbackData: %w. JSON: %s", err, string(callbackJSON))
	}

	// Basic validation
	if cbData.FileID == "" || cbData.PickCode == "" {
		// Debugging information
		fs.Debugf(f, "⚠️ 回调数据缺少必需字段: file_id=%q, pick_code=%q, raw JSON: %s",
			cbData.FileID, cbData.PickCode, string(callbackJSON))

		// Additional debugging for error analysis
		if strings.Contains(string(callbackJSON), "10002") {
			fs.Logf(f, "115 error analysis: Error code 10002 usually indicates file corruption during transfer")
			fs.Logf(f, "Possible solutions: 1) Check network stability 2) Retry upload 3) Verify source file integrity")
		}

		return nil, fmt.Errorf("OSS callback data missing required fields (file_id or pick_code). JSON: %s", string(callbackJSON))
	}

	fs.Debugf(f, "📋 传统回调数据解析: file_id=%q, pick_code=%q", cbData.FileID, cbData.PickCode)
	return cbData, nil
}

// getOSSToken fetches OSS credentials using the OpenAPI.
func (f *Fs) getOSSToken(ctx context.Context) (*OSSToken, error) {
	opts := rest.Opts{
		Method: "GET",
		Path:   "/open/upload/get_token",
	}
	var info OSSTokenResp
	err := f.CallOpenAPI(ctx, &opts, nil, &info, false)
	if err != nil {
		return nil, fmt.Errorf("OpenAPI get_token failed: %w", err)
	}
	if info.Data == nil {
		return nil, errors.New("OpenAPI get_token returned no data")
	}

	if info.Data.AccessKeyID == "" || info.Data.AccessKeySecret == "" || info.Data.SecurityToken == "" {
		return nil, errors.New("OpenAPI get_token response missing essential credential fields")
	}
	return info.Data, nil
}

// newOSSClient builds an OSS client with dynamic credentials from OpenAPI.
func (f *Fs) newOSSClient() (*oss.Client, error) {
	// Use CredentialsFetcherProvider from SDK v2
	provider := credentials.NewCredentialsFetcherProvider(
		credentials.CredentialsFetcherFunc(func(ctx context.Context) (credentials.Credentials, error) {
			fs.Debugf(f, "Fetching new OSS credentials via OpenAPI...")
			t, err := f.getOSSToken(ctx)
			if err != nil {
				fs.Errorf(f, "❌ 获取OSS令牌失败: %v", err)
				return credentials.Credentials{}, fmt.Errorf("failed to fetch OSS token: %w", err)
			}
			fs.Debugf(f, "Successfully fetched OSS credentials, expires at %v", time.Time(t.Expiration))
			return credentials.Credentials{
				AccessKeyID:     t.AccessKeyID,
				AccessKeySecret: t.AccessKeySecret,
				SecurityToken:   t.SecurityToken,
				Expires:         (*time.Time)(&t.Expiration), // Convert Time to *time.Time
			}, nil
		}),
	)

	// 修复：115网盘OSS上传性能优化配置
	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(provider).
		WithRegion(OSSRegion). // Use constant region
		WithUserAgent(OSSUserAgent).
		WithUseDualStackEndpoint(f.opt.DualStack).
		WithUseInternalEndpoint(f.opt.Internal).
		// 修复：优化连接超时，快速建立连接
		WithConnectTimeout(10 * time.Second). // 增加到10秒，避免连接超时
		// 修复：优化读写超时，适合大文件上传
		WithReadWriteTimeout(300 * time.Second) // 增加到300秒（5分钟），适合大文件上传

	// 使用rclone标准HTTP客户端
	httpClient := fshttp.NewClient(context.Background())

	// 将HTTP客户端应用到OSS配置
	cfg = cfg.WithHttpClient(httpClient)

	// 关键修复：设置OSS专用User-Agent，可能影响上传速度
	cfg = cfg.WithUserAgent(OSSUserAgent)

	// Create the client
	client := oss.NewClient(cfg)
	if client == nil {
		return nil, errors.New("failed to create OSS client")
	}
	return client, nil
}

// unWrapObjectInfo attempts to unwrap the underlying fs.Object from fs.ObjectInfo.
func unWrapObjectInfo(oi fs.ObjectInfo) fs.Object {
	if o, ok := oi.(fs.Object); ok {
		return fs.UnWrapObject(o) // Use standard unwrapper
	}
	// Handle specific wrappers like OverrideRemote if necessary
	// if do, ok := oi.(*fs.OverrideRemote); ok {
	// return do.UnWrap()
	// }
	return nil
}

// calcBlockSHA1 calculates SHA-1 for a specified byte range from a source reader.
// The reader `in` should ideally be seekable (e.g., buffered file).
func calcBlockSHA1(ctx context.Context, in io.Reader, src fs.ObjectInfo, rangeSpec string) (string, error) {
	var start, end int64
	// OpenAPI range is "start-end" (inclusive)
	if n, err := fmt.Sscanf(rangeSpec, "%d-%d", &start, &end); err != nil || n != 2 {
		return "", fmt.Errorf("invalid range spec format %q: %w", rangeSpec, err)
	}
	if start < 0 || end < start {
		return "", fmt.Errorf("invalid range spec values %q", rangeSpec)
	}
	length := end - start + 1

	var sectionReader io.Reader

	// Check if input is a RereadableObject first
	if ro, ok := in.(*RereadableObject); ok {
		// Try to open a new reader with robust retries for rate limiting
		var reader io.Reader // Holds the successfully opened reader
		var err error

		// Set retry parameters for reopening within calcBlockSHA1
		maxRetries := 10                       // Max retries for opening range
		initialDelay := 500 * time.Millisecond // Start with 500ms
		maxDelay := 60 * time.Second           // Max delay 1 minute
		maxElapsedTime := 5 * time.Minute      // Max total time

		// Define the reopening operation
		reopenOperation := func() error {
			var openErr error
			reader, openErr = ro.Open() // Attempt to open
			if openErr != nil {
				fs.Debugf(src, "Open attempt failed in calcBlockSHA1: %v", openErr)
			}
			return openErr // Return error for retry logic
		}

		// Retry with exponential backoff
		err = retryWithExponentialBackoff(
			ctx,
			"opening object for range SHA1",
			src, // Log against the source object info
			reopenOperation,
			maxRetries,
			initialDelay,
			maxDelay,
			maxElapsedTime,
		)

		if err != nil {
			return "", fmt.Errorf("failed to open RereadableObject reader for range SHA1 after retries: %w", err)
		}

		// If the obtained reader is an io.ReadCloser, ensure it's closed when calcBlockSHA1 finishes.
		// This does NOT close the parent RereadableObject 'ro'.
		closeReader := func() {} // No-op default
		if rc, ok := reader.(io.ReadCloser); ok {
			closeReader = func() { _ = rc.Close() }
		}
		defer closeReader() // Close the specific reader obtained for this SHA1 calc

		// Skip to the start position using the successfully opened 'reader'
		if seeker, ok := reader.(io.Seeker); ok {
			if _, err := seeker.Seek(start, io.SeekStart); err != nil {
				// Attempt to close the reader before returning error
				closeReader()
				return "", fmt.Errorf("failed to seek RereadableObject reader to %d: %w", start, err)
			}
			sectionReader = io.LimitReader(reader, length)
		} else {
			// If not seekable, try skipping bytes
			if start > 0 {
				// Use io.CopyN for skipping
				skipped, err := io.CopyN(io.Discard, reader, start)
				if err != nil {
					// Attempt to close the reader before returning error
					closeReader()
					return "", fmt.Errorf("failed to skip %d bytes in RereadableObject reader (skipped %d): %w", start, skipped, err)
				}
				if skipped != start {
					// Attempt to close the reader before returning error
					closeReader()
					return "", fmt.Errorf("failed to skip requested %d bytes in RereadableObject reader, only skipped %d", start, skipped)
				}
			}
			sectionReader = io.LimitReader(reader, length)
		}
	} else if seeker, ok := in.(io.Seeker); ok {
		// Try to create a SectionReader if the input is seekable
		// IMPORTANT: Seek back to start after reading the section
		currentOffset, seekErr := seeker.Seek(0, io.SeekCurrent)
		if seekErr != nil {
			return "", fmt.Errorf("failed to get current offset for SHA1 range: %w", seekErr)
		}
		defer func() {
			_, _ = seeker.Seek(currentOffset, io.SeekStart) // Restore original position
		}()

		// Seek to the start of the required section
		_, seekErr = seeker.Seek(start, io.SeekStart)
		if seekErr != nil {
			// Maybe the buffer doesn't contain the required range?
			return "", fmt.Errorf("failed to seek to start %d for SHA1 range: %w", start, seekErr)
		}
		sectionReader = io.LimitReader(in, length)
	} else {
		// If not seekable, we cannot reliably calculate the hash for an arbitrary range.
		// This might happen if the input is a direct network stream and hasn't been buffered.
		// Try opening the source object directly if possible.
		srcObj := unWrapObjectInfo(src)
		if srcObj != nil {
			fs.Debugf(src, "Input reader not seekable for SHA1 range, opening source object directly.")

			// Try to open with retries for rate limiting
			var rc io.ReadCloser
			var err error

			// Define maximum retries and use exponential backoff
			maxRetries := 10                       // Max retries
			initialDelay := 500 * time.Millisecond // Start delay
			maxDelay := 60 * time.Second           // Max delay
			maxElapsedTime := 5 * time.Minute      // Max total time

			// Define the operation
			openRangeOperation := func() error {
				var openErr error
				// Use RangeOption for efficiency if supported
				rc, openErr = srcObj.Open(ctx, &fs.RangeOption{Start: start, End: end})
				if openErr != nil {
					fs.Debugf(srcObj, "Open range attempt failed: %v", openErr)
				}
				return openErr
			}

			// Retry with exponential backoff
			err = retryWithExponentialBackoff(
				ctx,
				"opening source object range directly",
				srcObj,
				openRangeOperation,
				maxRetries,
				initialDelay,
				maxDelay,
				maxElapsedTime,
			)

			if err != nil {
				return "", fmt.Errorf("failed to open source object for SHA1 range %q after retries: %w", rangeSpec, err)
			}
			// Defer closing the reader obtained specifically for this operation
			defer fs.CheckClose(rc, &err)
			sectionReader = rc
		} else {
			return "", fmt.Errorf("cannot calculate SHA1 for range %q: input reader not seekable and source object unavailable", rangeSpec)
		}
	}

	// 使用标准SHA1计算
	hasher := sha1.New()
	_, err := io.Copy(hasher, sectionReader)
	if err != nil {
		return "", fmt.Errorf("failed to calculate SHA1 for range %q: %w", rangeSpec, err)
	}
	// 关键修复：sign_val需要大写SHA1，根据115网盘官方API文档要求
	sha1Hash := fmt.Sprintf("%X", hasher.Sum(nil))
	return sha1Hash, nil
}

// sampleInitUpload prepares a traditional "simple form" upload (for smaller files).
func (f *Fs) sampleInitUpload(ctx context.Context, size int64, name, dirID string) (*SampleInitResp, error) {
	// Try to get userID if not already set (e.g., during initial login)
	if f.userID == "" {
		// Get userID from uploadinfo API
		if err := f.getUploadBasicInfo(ctx); err != nil {
			return nil, fmt.Errorf("failed to get userID: %w", err)
		}
	}

	form := url.Values{}
	form.Set("userid", f.userID)
	form.Set("filename", f.opt.Enc.FromStandardName(name))
	form.Set("filesize", strconv.FormatInt(size, 10))
	form.Set("target", "U_1_"+dirID)

	opts := rest.Opts{
		Method:      "POST",
		RootURL:     "https://uplb.115.com/3.0/sampleinitupload.php", // Traditional endpoint
		ContentType: "application/x-www-form-urlencoded",
		Body:        strings.NewReader(form.Encode()),
	}
	var info *SampleInitResp
	// Use traditional API call (requires cookie, assume no encryption needed for this endpoint?)
	err := f.CallTraditionalAPI(ctx, &opts, nil, &info, true) // skipEncrypt = true
	if err != nil {
		return nil, fmt.Errorf("traditional sampleInitUpload call failed: %w", err)
	}
	if info.ErrorCode != 0 {
		return nil, fmt.Errorf("traditional sampleInitUpload API error: %s (%d)", info.Error, info.ErrorCode)
	}
	if info.Host == "" || info.Object == "" {
		return nil, errors.New("traditional sampleInitUpload response missing required fields (host or object)")
	}
	return info, nil
}

// sampleUploadForm uses multipart form to upload smaller files via traditional sample upload flow.
func (f *Fs) sampleUploadForm(ctx context.Context, in io.Reader, initResp *SampleInitResp, name string, options ...fs.OpenOption) (*CallbackData, error) {
	// Safety check for nil input
	if in == nil {
		return nil, errors.New("nil input reader provided to sampleUploadForm")
	}
	if initResp == nil {
		return nil, errors.New("nil initResp provided to sampleUploadForm")
	}

	pipeReader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	errChan := make(chan error, 1)

	// Start goroutine to write multipart data to the pipe
	go func() {
		var err error
		defer func() {
			closeErr := multipartWriter.Close()
			if err == nil {
				err = closeErr // Assign close error if no previous error
			}
			writeCloseErr := pipeWriter.CloseWithError(err) // Close pipe with error status
			if err == nil {
				err = writeCloseErr
			}
			errChan <- err // Send final status
		}()

		// Write standard fields
		fields := map[string]string{
			"name":                  name, // Use original name for form field?
			"key":                   initResp.Object,
			"policy":                initResp.Policy,
			"OSSAccessKeyId":        initResp.AccessID,
			"success_action_status": "200", // OSS expects 200 for success
			"callback":              initResp.Callback,
			"signature":             initResp.Signature,
		}
		for k, v := range fields {
			if v == "" {
				fs.Debugf(f, "Warning: empty value for form field %q", k)
				// Continue anyway, some fields might be optional
			}
			if err = multipartWriter.WriteField(k, v); err != nil {
				err = fmt.Errorf("failed to write field %s: %w", k, err)
				return
			}
		}

		// Add optional headers from fs.OpenOption
		for _, opt := range options {
			k, v := opt.Header()
			lowerK := strings.ToLower(k)
			// Include headers supported by OSS PostObject policy/form
			if lowerK == "cache-control" || lowerK == "content-disposition" || lowerK == "content-encoding" || lowerK == "content-type" || strings.HasPrefix(lowerK, "x-oss-meta-") {
				if err = multipartWriter.WriteField(k, v); err != nil {
					err = fmt.Errorf("failed to write optional field %s: %w", k, err)
					return
				}
			}
		}

		// Write file data
		filePart, err := multipartWriter.CreateFormFile("file", f.opt.Enc.FromStandardName(name)) // Use encoded name for file part?
		if err != nil {
			err = fmt.Errorf("failed to create form file: %w", err)
			return
		}

		// Double-check in is not nil again (just being extra careful)
		if in == nil {
			err = errors.New("input reader became nil before copy in sampleUploadForm")
			return
		}

		// Copy file data
		if _, err = io.Copy(filePart, in); err != nil {
			err = fmt.Errorf("failed to copy file data to form: %w", err)
			return
		}
	}()

	// Create and send the HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", initResp.Host, pipeReader)
	if err != nil {
		_ = pipeWriter.CloseWithError(err) // Ensure goroutine exits
		return nil, fmt.Errorf("failed to create sample upload request: %w", err)
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	// Set Content-Length? OSS might require it for POST uploads.
	// However, with pipeReader, length is unknown beforehand. Let http client handle chunked encoding.
	// req.ContentLength = -1 // Indicate unknown length

	// 使用统一调速器，优化样本上传API调用频率
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		// 使用rclone标准HTTP客户端
		httpClient := fshttp.NewClient(ctx)
		resp, err = httpClient.Do(req)
		if err != nil {
			retry, retryErr := shouldRetry(ctx, resp, err)
			if retry {
				return true, retryErr
			}
			return false, fmt.Errorf("sample upload POST failed: %w", err)
		}
		return false, nil // Success
	})

	// Wait for the goroutine writing to the pipe to finish
	writeErr := <-errChan
	if err == nil { // If HTTP call succeeded, check for writer error
		err = writeErr
	}
	if err != nil {
		// If there was an error during HTTP or writing, close response body if non-nil
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("sample upload failed: %w", err)
	}

	// Process the response
	defer fs.CheckClose(resp.Body, &err)
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("failed to read sample upload response body: %w", readErr)
	}

	// OSS POST upload returns 200 on success with callback body, or other status on error
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("sample upload failed: status %s, body: %s", resp.Status, string(respBody))
	}

	// Response body contains the result from the 115 callback URL
	var respMap map[string]any
	if err := json.Unmarshal(respBody, &respMap); err != nil {
		// Sometimes the body might not be JSON if callback failed internally on 115 side
		fs.Logf(f, "Failed to unmarshal sample upload callback response as JSON: %v. Body: %s", err, string(respBody))
		// Try to find essential info heuristically? Risky. Return error.
		return nil, fmt.Errorf("failed to parse sample upload callback response: %w", err)
	}

	// Process the callback map using the existing postUpload function
	return f.postUpload(respMap)
}

// ------------------------------------------------------------
// Main Upload Logic
// ------------------------------------------------------------

// tryHashUpload attempts 秒传 using OpenAPI.
// Returns (found bool, uploadInitInfo *UploadInitInfo, potentiallyBufferedInput io.Reader, cleanup func(), err error)
func (f *Fs) tryHashUpload(
	ctx context.Context,
	in io.Reader,
	src fs.ObjectInfo,
	o *Object,
	leaf, dirID string,
	size int64,
	options ...fs.OpenOption,
) (found bool, ui *UploadInitInfo, newIn io.Reader, cleanup func(), err error) {
	cleanup = func() {} // Default no-op cleanup
	newIn = in          // Assume input reader doesn't change unless buffered

	defer func() {
		if err != nil && cleanup != nil {
			cleanup() // Ensure cleanup happens on error exit
		}
	}()

	fs.Infof(o, "⚡ 尝试秒传 (文件已存在则跳过上传)...")

	// 1. Get SHA1 hash
	hashStr, err := src.Hash(ctx, hash.SHA1)
	if err != nil || hashStr == "" {
		fs.Debugf(o, "Source SHA1 not available, calculating locally...")
		var localCleanup func()
		// Buffer the input while calculating hash
		hashStr, newIn, localCleanup, err = bufferIOwithSHA1(f, in, src, size, int64(f.opt.HashMemoryThreshold), ctx, options...)
		cleanup = localCleanup // Assign the cleanup function from bufferIO
		if err != nil {
			return false, nil, newIn, cleanup, fmt.Errorf("failed to calculate SHA1: %w", err)
		}
		fs.Debugf(o, "Calculated SHA1: %s", hashStr)

		// 关键修复：将计算出的SHA1设置到Object中，确保后续使用
		if o != nil {
			o.sha1sum = strings.ToUpper(hashStr)
			fs.Debugf(o, "SHA1 set to Object: %s", o.sha1sum)
		}
	} else {
		fs.Debugf(o, "Using provided SHA1: %s", hashStr)

		// 确保Object中也有SHA1信息
		if o != nil && o.sha1sum == "" {
			o.sha1sum = strings.ToUpper(hashStr)
			fs.Debugf(o, "SHA1 set to Object: %s", o.sha1sum)
		}

		// If NoBuffer is enabled, wrap the input in a RereadableObject
		if f.opt.NoBuffer {
			// 修复：检测跨云传输并尝试集成父传输
			var ro *RereadableObject
			var roErr error

			// 检测是否为跨云传输（特别是123网盘源）
			if f.isRemoteSource(src) {
				fs.Debugf(o, "☁️ 检测到跨云传输，尝试查找父传输对象")

				// Try to get current transfer from accounting stats
				if stats := accounting.Stats(ctx); stats != nil {
					// Use standard method with cross-cloud transfer marker
					ro, roErr = NewRereadableObject(ctx, src, options...)
				}
			}

			// 如果不是跨云传输或者上面的逻辑失败，使用标准方法
			if ro == nil {
				ro, roErr = NewRereadableObject(ctx, src, options...)
			}

			if roErr != nil {
				// Continue with original reader if failed
				fs.Debugf(o, "Failed to create rereadable object: %v", roErr)
			} else {
				newIn = ro
				cleanup = func() { _ = ro.Close() }
			}
		}
	}
	// 关键修复：115网盘使用大写SHA1，保持一致性以支持秒传功能
	o.sha1sum = strings.ToUpper(hashStr) // Store hash in object (uppercase for 115 API consistency)

	// 2. Calculate PreID (128KB SHA1) as required by 115网盘官方API
	var preID string
	if size > 0 {
		// 计算前128KB的SHA1作为PreID
		const preHashSize int64 = 128 * 1024 // 128KB
		hashSize := min(size, preHashSize)

		// 尝试从newIn读取前128KB计算PreID
		if seeker, ok := newIn.(io.ReadSeeker); ok {
			// 如果newIn支持Seek（比如临时文件），直接使用
			seeker.Seek(0, io.SeekStart) // 重置到文件开头
			preData := make([]byte, hashSize)
			n, err := io.ReadFull(seeker, preData)
			if err != nil && err != io.ErrUnexpectedEOF {
				fs.Debugf(o, "读取前128KB数据失败: %v", err)
			} else if n > 0 {
				hasher := sha1.New()
				hasher.Write(preData[:n])
				// 关键修复：PreID也需要大写格式，与115网盘API保持一致
				preID = fmt.Sprintf("%X", hasher.Sum(nil))
				fs.Debugf(o, "计算PreID成功: %s (前%d字节)", preID, n)
			}
			seeker.Seek(0, io.SeekStart) // 重置到文件开头供后续使用
		} else {
			// 输入流不支持Seek，尝试从源文件计算PreID
			fs.Debugf(o, "输入流不支持Seek操作，尝试从源文件计算PreID")
			if preIDFromSrc, err := f.calculatePreIDFromSource(ctx, src); err == nil && preIDFromSrc != "" {
				preID = preIDFromSrc
				fs.Debugf(o, "从源文件计算PreID成功: %s", preID)
			} else {
				// 在跨云传输时，源文件元数据读取失败是正常情况
				if strings.Contains(err.Error(), "object not found") || strings.Contains(err.Error(), "failed to read metadata") {
					fs.Debugf(o, "跨云传输时源文件元数据不可用，PreID将为空: %v", err)
				} else {
					fs.Debugf(o, "从源文件计算PreID失败: %v", err)
				}
			}
		}
	}

	// 3. Call OpenAPI initUpload with SHA1 and PreID
	ui, err = f.initUploadOpenAPI(ctx, size, leaf, dirID, hashStr, preID, "", "")
	if err != nil {
		return false, nil, newIn, cleanup, fmt.Errorf("OpenAPI initUpload for hash check failed: %w", err)
	}

	// 3. Handle response status
	signKey, signVal := "", ""
	for {
		status := ui.GetStatus()
		switch status {
		case 2: // 秒传 success!
			fs.Infof(o, "🎉 秒传成功！文件已存在服务器，无需重复上传")
			fs.Debugf(o, "✅ 秒传完成")
			// Mark accounting as server-side copy
			reader, _ := accounting.UnWrap(newIn)
			if acc, ok := reader.(*accounting.Account); ok && acc != nil {
				acc.ServerSideTransferStart() // Mark start
				acc.ServerSideCopyEnd(size)   // Mark end immediately
			}
			// Update object metadata from response (FileID is important)
			o.id = ui.GetFileID()
			o.pickCode = ui.GetPickCode() // Get pick code too
			o.hasMetaData = true          // Mark as having basic metadata

			// Mark complete on RereadableObject if applicable
			if ro, ok := newIn.(*RereadableObject); ok {
				ro.MarkComplete(ctx)
				fs.Debugf(o, "Marked RereadableObject transfer as complete after hash upload")
			}

			// Optionally, call getFile to get full metadata, but might be slow/costly
			// info, getErr := f.getFile(ctx, o.id, "")
			// if getErr == nil { o.setMetaData(info) }
			return true, ui, newIn, cleanup, nil // Found = true

		case 1: // Non-秒传, need actual upload
			fs.Infof(o, "⚡ 秒传不可用，开始正常上传")
			return false, ui, newIn, cleanup, nil // Found = false

		case 7: // Need secondary auth (sign_check)
			fs.Debugf(o, "Hash upload requires secondary auth (status 7). Calculating range SHA1...")
			signKey = ui.GetSignKey()
			signCheckRange := ui.GetSignCheck()
			if signKey == "" || signCheckRange == "" {
				return false, nil, newIn, cleanup, errors.New("hash upload status 7 but sign_key or sign_check missing")
			}
			// Calculate SHA1 for the specified range
			signVal, err = calcBlockSHA1(ctx, newIn, src, signCheckRange)
			if err != nil {
				return false, nil, newIn, cleanup, fmt.Errorf("failed to calculate SHA1 for range %q: %w", signCheckRange, err)
			}
			fs.Debugf(o, "Calculated range SHA1: %s for range %s", signVal, signCheckRange)

			// Retry initUpload with sign_key and sign_val with exponential backoff for network errors
			var retryErr error
			// Define retry parameters
			maxRetries := 12
			initialDelay := 1 * time.Second
			maxDelay := 60 * time.Second
			maxElapsedTime := 10 * time.Minute

			// Define the operation to be retried
			initUploadOperation := func() error {
				var initErr error
				ui, initErr = f.initUploadOpenAPI(ctx, size, leaf, dirID, hashStr, "", signKey, signVal)
				return initErr
			}

			// Execute with exponential backoff
			retryErr = retryWithExponentialBackoff(
				ctx,
				"OpenAPI initUpload with signature",
				o,
				initUploadOperation,
				maxRetries,
				initialDelay,
				maxDelay,
				maxElapsedTime,
			)

			if retryErr != nil {
				return false, nil, newIn, cleanup, fmt.Errorf("OpenAPI initUpload retry with signature failed after multiple attempts: %w", retryErr)
			}
			continue // Re-evaluate the new status

		case 6: // Auth-related status, treat as failure
			fs.Errorf(o, "❌ 哈希上传失败，认证状态 %d。消息: %s", status, ui.ErrMsg())
			return false, nil, newIn, cleanup, fmt.Errorf("hash upload failed with status %d: %s", status, ui.ErrMsg())

		case 8: // 状态8：特殊认证状态，但可以继续上传
			fs.Debugf(o, "Hash upload returned status 8 (special auth), continuing with upload. Message: %s", ui.ErrMsg())
			// 状态8不是错误，而是表示需要继续上传流程
			// 返回false表示没有秒传成功，但ui包含了继续上传所需的信息
			return false, ui, newIn, cleanup, nil

		default: // Unexpected status
			fs.Errorf(o, "❌ 哈希上传失败，意外状态 %d。消息: %s", status, ui.ErrMsg())
			return false, nil, newIn, cleanup, fmt.Errorf("unexpected hash upload status %d: %s", status, ui.ErrMsg())
		}
	}
}

// calculatePreIDFromSource 从源文件计算PreID（前128KB的SHA1）
func (f *Fs) calculatePreIDFromSource(ctx context.Context, src fs.ObjectInfo) (string, error) {
	const preHashSize int64 = 128 * 1024 // 128KB

	// 尝试打开源文件
	srcObj, ok := src.(fs.Object)
	if !ok {
		return "", fmt.Errorf("源对象不支持读取")
	}

	reader, err := srcObj.Open(ctx)
	if err != nil {
		return "", fmt.Errorf("打开源文件失败: %w", err)
	}
	defer reader.Close()

	// 读取前128KB数据
	hashSize := min(src.Size(), preHashSize)
	preData := make([]byte, hashSize)
	n, err := io.ReadFull(reader, preData)
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", fmt.Errorf("读取前128KB数据失败: %w", err)
	}

	if n == 0 {
		return "", fmt.Errorf("无法读取任何数据")
	}

	// 计算SHA1
	hasher := sha1.New()
	hasher.Write(preData[:n])
	// 关键修复：PreID需要大写格式，与115网盘API保持一致
	preID := fmt.Sprintf("%X", hasher.Sum(nil))

	fs.Debugf(f, "从源文件计算PreID: %s (前%d字节)", preID, n)
	return preID, nil
}

// shouldFallbackToTraditional 检查错误是否应该降级到传统上传
func (f *Fs) shouldFallbackToTraditional(err error) bool {
	if err == nil {
		return false
	}

	// 首先检查115网盘特定的错误码
	code, _ := f.parse115ErrorCode(err)
	if code > 0 {
		return f.should115ErrorFallback(code)
	}

	// 检查OSS相关错误
	errStr := err.Error()

	// OSS服务不可用相关错误
	if strings.Contains(errStr, "OSS service unavailable") ||
		strings.Contains(errStr, "bucket not found") ||
		strings.Contains(errStr, "invalid credentials") ||
		strings.Contains(errStr, "invalid field, OperationInput.Bucket") ||
		strings.Contains(errStr, "multipart upload not available") {
		return true
	}

	// 网络相关错误
	if strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "network") {
		return true
	}

	return false
}

// parse115ErrorCode 解析115网盘API错误码
func (f *Fs) parse115ErrorCode(err error) (int, string) {
	if err == nil {
		return 0, ""
	}

	errStr := err.Error()

	// 尝试从错误信息中提取错误码
	re := regexp.MustCompile(`code[:\s]*(\d+)`)
	matches := re.FindStringSubmatch(errStr)
	if len(matches) >= 2 {
		if code, parseErr := strconv.Atoi(matches[1]); parseErr == nil {
			return code, errStr
		}
	}

	return 0, errStr
}

// should115ErrorFallback 根据115网盘错误码决定是否应该降级
func (f *Fs) should115ErrorFallback(code int) bool {
	switch code {
	// 认证相关错误 - 允许降级
	case 700: // 签名认证失败
		return true
	case 10009: // 上传频率限制
		return true
	case 10002: // SHA1不匹配
		return true

	// 服务器错误 - 允许降级
	case 500, 502, 503, 504:
		return true

	default:
		return false
	}
}

// ensureOptimalMemoryConfig 确保rclone有足够的内存配置来处理大文件上传
// VPS友好：尊重用户的内存限制设置
func (f *Fs) ensureOptimalMemoryConfig(fileSize int64) {
	ci := fs.GetConfig(context.Background())

	// VPS友好：如果用户已经设置了内存限制，不要强制覆盖
	currentMaxMemory := int64(ci.BufferSize)
	if currentMaxMemory > 0 {
		// 用户已经设置了内存限制，尊重用户的VPS配置
		fs.Debugf(f, "VPS模式：尊重用户内存限制 %s，文件大小 %s",
			fs.SizeSuffix(currentMaxMemory), fs.SizeSuffix(fileSize))
		return
	}

	// 只有在用户没有设置内存限制时，才进行自动调整
	var requiredMemory int64

	if fileSize > 0 {
		// 对于大文件，确保有足够内存处理分片上传
		chunkSize := int64(f.calculateOptimalChunkSize(fileSize))
		// 需要至少2个分片的内存：当前分片 + 下一个分片
		requiredMemory = chunkSize * 2

		// 🔧 性能优化：提升内存配置上限，与123网盘对齐
		maxDefaultMemory := int64(512 * 1024 * 1024) // 512MB默认上限（从256MB提升）
		if requiredMemory > maxDefaultMemory {
			requiredMemory = maxDefaultMemory
		}
	} else {
		// 未知大小文件，使用保守的默认值
		requiredMemory = int64(128 * 1024 * 1024) // 128MB
	}

	// 设置合理的默认内存限制
	fs.Infof(f, "115网盘自动配置：文件大小 %s，设置内存限制 %s",
		fs.SizeSuffix(fileSize), fs.SizeSuffix(requiredMemory))
	ci.BufferSize = fs.SizeSuffix(requiredMemory)
}

// uploadToOSS performs the actual upload to OSS using multipart via OpenAPI info.
func (f *Fs) uploadToOSS(
	ctx context.Context,
	in io.Reader,
	src fs.ObjectInfo,
	o *Object,
	leaf, dirID string,
	size int64,
	ui *UploadInitInfo, // Pre-fetched info (e.g., from failed hash upload)
	options ...fs.OpenOption,
) (fs.Object, error) {
	fs.Debugf(o, "Starting OSS multipart upload...")

	// Initialize upload and get upload info if not provided
	uploadInfo, err := f.getUploadInfo(ctx, ui, leaf, dirID, size, o, in, src)
	if err != nil {
		return nil, err
	}

	// 修复空指针：确保uploadInfo不为空
	if uploadInfo == nil {
		return nil, fmt.Errorf("getUploadInfo returned nil UploadInitInfo")
	}

	// Handle case where initUpload resulted in instant upload
	if uploadInfo.GetStatus() == 2 {
		return o, nil
	}

	// Create OSS client
	ossClient, err := f.newOSSClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create OSS client: %w", err)
	}

	// Configure and perform the upload
	callbackData, err := f.performOSSUpload(ctx, ossClient, in, src, o, leaf, dirID, size, uploadInfo, options...)
	if err != nil {
		return nil, err
	}

	// Update object metadata
	if err = o.setMetaDataFromCallBack(callbackData); err != nil {
		return nil, fmt.Errorf("failed to set metadata from callback: %w", err)
	}

	// 7. Mark complete on RereadableObject if applicable
	if ro, ok := in.(*RereadableObject); ok {
		ro.MarkComplete(ctx)
		fs.Debugf(o, "Marked RereadableObject transfer as complete")
	}

	fs.Infof(o, "✅ 分片上传完成: %s", fs.SizeSuffix(size))
	fs.Debugf(o, "OSS multipart upload successful.")
	return o, nil
}

// getUploadInfo gets or validates upload information
func (f *Fs) getUploadInfo(
	ctx context.Context,
	ui *UploadInitInfo,
	leaf, dirID string,
	size int64,
	o *Object,
	in io.Reader,
	src fs.ObjectInfo,
) (*UploadInitInfo, error) {
	// If upload info is already provided, use it
	if ui != nil {
		return ui, nil
	}

	// 修复OSS multipart上传：需要计算SHA1用于API调用
	// 根据115网盘官方API文档，fileid（SHA1）是必需参数
	fs.Debugf(o, "OSS multipart上传需要计算SHA1...")

	// 获取文件的SHA1哈希
	var sha1sum string
	if o != nil {
		if hash, err := o.Hash(ctx, hash.SHA1); err == nil && hash != "" {
			sha1sum = strings.ToUpper(hash)
			fs.Debugf(o, "使用已有SHA1: %s", sha1sum)
		}
	}

	// 关键修复：如果没有SHA1，必须返回错误，因为115网盘API要求fileid参数
	if sha1sum == "" {
		return nil, fmt.Errorf("OSS multipart upload requires SHA1 hash (fileid parameter) - this should be calculated by tryHashUpload first")
	}

	// Initialize upload with SHA1 (if available)
	ui, err := f.initUploadOpenAPI(ctx, size, leaf, dirID, sha1sum, "", "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize multipart upload: %w", err)
	}

	// 修复空指针：确保ui不为空
	if ui == nil {
		return nil, fmt.Errorf("initUploadOpenAPI returned nil UploadInitInfo")
	}

	// Handle response status with authentication loop
	signKey, signVal := "", ""
	for {
		status := ui.GetStatus()
		switch status {
		case 2: // 秒传 success!
			fs.Infof(o, "🎉 Multipart upload: 秒传成功！文件已存在服务器")
			o.id = ui.GetFileID()
			o.pickCode = ui.GetPickCode()
			o.hasMetaData = true
			return ui, nil

		case 1: // Non-秒传, need actual upload
			fs.Debugf(o, "📤 Multipart upload: 秒传不可用，继续OSS上传")
			// Check if we have valid OSS credentials
			if ui.GetBucket() == "" || ui.GetObject() == "" {
				return nil, fmt.Errorf("status 1 with empty bucket/object, multipart upload not available")
			}
			return ui, nil

		case 7: // Need secondary auth (sign_check)
			fs.Debugf(o, "🔐 分片上传需要二次认证 (状态 7)。计算范围SHA1...")
			signKey = ui.GetSignKey()
			signCheckRange := ui.GetSignCheck()
			if signKey == "" || signCheckRange == "" {
				return nil, errors.New("multipart upload status 7 but sign_key or sign_check missing")
			}
			// Calculate SHA1 for the specified range
			var err error
			signVal, err = calcBlockSHA1(ctx, in, src, signCheckRange)
			if err != nil {
				return nil, fmt.Errorf("failed to calculate SHA1 for range %q: %w", signCheckRange, err)
			}
			fs.Debugf(o, "✅ 计算范围SHA1: %s 用于范围 %s", signVal, signCheckRange)

			// Retry initUpload with sign_key and sign_val
			ui, err = f.initUploadOpenAPI(ctx, size, leaf, dirID, sha1sum, "", signKey, signVal)
			if err != nil {
				return nil, fmt.Errorf("multipart upload retry with signature failed: %w", err)
			}
			continue // Re-evaluate the new status

		case 8: // 状态8：特殊认证状态，但可以继续上传
			fs.Debugf(o, "🔐 分片上传返回状态 8 (特殊认证)，检查OSS凭据")
			// Check if we have valid OSS credentials
			if ui.GetBucket() == "" || ui.GetObject() == "" {
				return nil, fmt.Errorf("status 8 with empty bucket/object, multipart upload not available")
			}
			return ui, nil

		case 6: // Auth-related status, treat as failure
			fs.Errorf(o, "❌ 分片上传失败，认证状态 %d。消息: %s", status, ui.ErrMsg())
			return nil, fmt.Errorf("multipart upload failed with status %d: %s", status, ui.ErrMsg())

		default: // Unexpected status
			fs.Errorf(o, "❌ 分片上传失败，意外状态 %d。消息: %s", status, ui.ErrMsg())
			return nil, fmt.Errorf("unexpected multipart upload status %d: %s", status, ui.ErrMsg())
		}
	}
}

// performTraditionalUpload handles upload when OSS is not available (status 7/8 with empty bucket/object)
func (f *Fs) performTraditionalUpload(
	ctx context.Context,
	in io.Reader,
	o *Object,
	leaf string,
	dirID string,
	size int64,
	ui *UploadInitInfo,
	options ...fs.OpenOption,
) (fs.Object, error) {
	fs.Debugf(o, "📤 由于状态 %d 且bucket/object为空，执行传统上传", ui.GetStatus())

	// Use the existing traditional sample upload method
	// This will handle the complete traditional upload flow
	return f.doSampleUpload(ctx, in, o, leaf, dirID, size, options...)
}

// performOSSUpload handles the actual upload process
func (f *Fs) performOSSUpload(
	ctx context.Context,
	ossClient *oss.Client,
	in io.Reader,
	src fs.ObjectInfo,
	o *Object,
	leaf, dirID string,
	size int64,
	ui *UploadInitInfo,
	options ...fs.OpenOption,
) (*CallbackData, error) {
	// 🎯 智能选择上传策略：使用用户配置的切换阈值
	uploadCutoff := int64(f.opt.UploadCutoff) // 用户配置的上传切换阈值

	// 修复：使用统一的分片大小计算函数，确保与分片上传一致
	optimalPartSize := int64(f.calculateOptimalChunkSize(size))
	fs.Debugf(o, "🎯 智能分片大小计算: 文件大小=%s, 最优分片大小=%s",
		fs.SizeSuffix(size), fs.SizeSuffix(optimalPartSize))

	// 参考OpenList：极简进度回调，最大化减少开销
	var lastLoggedPercent int
	var lastLogTime time.Time

	// Get Account object for progress integration
	var currentAccount *accounting.Account
	if unwrappedIn, acc := accounting.UnWrapAccounting(in); acc != nil {
		currentAccount = acc
		_ = unwrappedIn // Avoid unused variable warning
	}

	// 参考阿里云OSS示例：创建115网盘专用的上传管理器配置
	uploaderConfig := &Upload115Config{
		PartSize:    optimalPartSize, // 使用智能计算的分片大小
		ParallelNum: 1,               // 115网盘强制单线程上传
		ProgressFn: func(increment, transferred, total int64) {
			// 修复120%进度问题：移除重复的Account更新
			// AccountedFileReader已经通过Read()方法自动更新Account进度
			// 这里再次更新会导致重复计数，造成进度超过100%

			if total > 0 {
				currentPercent := int(float64(transferred) / float64(total) * 100)
				now := time.Now()

				// 减少日志频率，避免刷屏
				if (currentPercent >= lastLoggedPercent+10 || transferred == total) &&
					(now.Sub(lastLogTime) > 5*time.Second || transferred == total) {
					fs.Infof(o, "📊 115分片上传: %d%% (%s/%s)",
						currentPercent, fs.SizeSuffix(transferred), fs.SizeSuffix(total))
					lastLoggedPercent = currentPercent
					lastLogTime = now
				}
			}

			// 修复：只保留调试日志，不重复更新Account对象
			// 减少调试日志频率，避免刷屏
			if currentAccount != nil && increment > 0 && transferred == total {
				fs.Debugf(o, " ProgressFn完成: increment=%s, transferred=%s, total=%s",
					fs.SizeSuffix(increment), fs.SizeSuffix(transferred), fs.SizeSuffix(total))
			}
		},
	}

	if size >= 0 && size < uploadCutoff {
		// 📤 小文件OSS单文件上传策略
		fs.Infof(o, "📤 115网盘OSS单文件上传: %s (%s)", leaf, fs.SizeSuffix(size))
		return f.performOSSPutObject(ctx, ossClient, in, src, o, leaf, dirID, size, ui, uploaderConfig, options...)
	} else {
		// Large file: use OSS multipart upload
		fs.Debugf(o, "115 OSS multipart upload: %s (%s)", leaf, fs.SizeSuffix(size))
		return f.performOSSMultipart(ctx, ossClient, in, src, o, leaf, dirID, size, ui, options...)
	}
}

// Upload115Config 115网盘上传管理器配置
// 参考阿里云OSS UploadFile示例的配置思想
type Upload115Config struct {
	PartSize    int64                                     // 分片大小
	ParallelNum int                                       // 并行数（115网盘固定为1）
	ProgressFn  func(increment, transferred, total int64) // 进度回调函数
}

// calculateOptimalPartSize function removed, unified to use calculateOptimalChunkSize

// performOSSPutObject 执行OSS单文件上传
func (f *Fs) performOSSPutObject(
	ctx context.Context,
	ossClient *oss.Client,
	in io.Reader,
	_ fs.ObjectInfo,
	o *Object,
	_, _ string,
	size int64,
	ui *UploadInitInfo,
	config *Upload115Config,
	options ...fs.OpenOption,
) (*CallbackData, error) {
	// 参考阿里云OSS UploadFile示例：准备PutObject请求
	req := &oss.PutObjectRequest{
		Bucket:      oss.Ptr(ui.GetBucket()),
		Key:         oss.Ptr(ui.GetObject()),
		Body:        in,
		Callback:    oss.Ptr(ui.GetCallback()),
		CallbackVar: oss.Ptr(ui.GetCallbackVar()),
		// 使用配置中的进度回调函数，参考阿里云OSS UploadFile示例
		ProgressFn: config.ProgressFn,
	}

	// Apply upload options
	for _, option := range options {
		key, value := option.Header()
		lowerKey := strings.ToLower(key)
		switch lowerKey {
		case "":
			// ignore
		case "cache-control":
			req.CacheControl = oss.Ptr(value)
		case "content-disposition":
			req.ContentDisposition = oss.Ptr(value)
		case "content-encoding":
			req.ContentEncoding = oss.Ptr(value)
		case "content-type":
			req.ContentType = oss.Ptr(value)
		}
	}

	// 🚀 OSS单文件上传：使用完整重试机制确保稳定性
	fs.Debugf(f, "🚀 OSS单文件上传开始: bucket=%q, object=%q", ui.GetBucket(), ui.GetObject())
	var putRes *oss.PutObjectResult
	err := f.pacer.Call(func() (bool, error) {
		var putErr error
		putRes, putErr = ossClient.PutObject(ctx, req)

		if putErr != nil {
			retry, retryErr := shouldRetry(ctx, nil, putErr)
			if retry {
				// 🔄 重置流位置准备重试
				if seeker, ok := in.(io.Seeker); ok {
					if _, seekErr := seeker.Seek(0, io.SeekStart); seekErr != nil {
						return false, fmt.Errorf("🚫 无法重置流进行重试: %w", seekErr)
					}
				} else {
					return false, fmt.Errorf("🚫 不可重置流无法重试: %w", putErr)
				}
				fs.Debugf(f, "🔄 OSS单文件上传重试: %v", putErr)
				return true, retryErr
			}
			return false, putErr
		}
		return false, nil
	})

	if err != nil {
		fs.Errorf(f, "❌ OSS单文件上传失败: %v", err)
		return nil, fmt.Errorf("❌ OSS单文件上传失败: %w", err)
	}

	// Process callback
	callbackData, err := f.postUpload(putRes.CallbackResult)
	if err != nil {
		return nil, fmt.Errorf("failed to process PutObject callback: %w", err)
	}

	// Mark complete on RereadableObject if applicable
	if ro, ok := in.(*RereadableObject); ok {
		ro.MarkComplete(ctx)
		fs.Debugf(o, "Marked RereadableObject transfer as complete after PutObject upload")
	}

	fs.Infof(o, "✅ 单文件上传完成: %s", fs.SizeSuffix(size))
	return callbackData, nil
}

// performOSSMultipart 执行OSS分片上传
func (f *Fs) performOSSMultipart(
	ctx context.Context,
	ossClient *oss.Client,
	in io.Reader,
	src fs.ObjectInfo,
	o *Object,
	_, _ string,
	_ int64,
	ui *UploadInitInfo,
	options ...fs.OpenOption,
) (*CallbackData, error) {
	// Use configuration for OSS multipart upload

	// 关键修复：分片上传也使用优化后的OSS客户端
	// Create the chunk writer with optimized OSS client
	chunkWriter, err := f.newChunkWriterWithClient(ctx, src, ui, in, o, ossClient, options...)
	if err != nil {
		return nil, fmt.Errorf("failed to create chunk writer: %w", err)
	}

	// TODO: 将config.ProgressFn集成到chunkWriter中

	// Perform the upload
	if err := chunkWriter.Upload(ctx); err != nil {
		return nil, fmt.Errorf("OSS multipart upload failed: %w", err)
	}

	// Process upload callback
	callbackData, err := f.postUpload(chunkWriter.callbackRes)
	if err != nil {
		return nil, fmt.Errorf("failed to process OSS upload callback: %w", err)
	}

	// Mark complete on RereadableObject if applicable
	if ro, ok := in.(*RereadableObject); ok {
		ro.MarkComplete(ctx)
		fs.Debugf(o, "Marked RereadableObject transfer as complete")
	}

	return callbackData, nil
}

// doSampleUpload performs the traditional streamed upload.
func (f *Fs) doSampleUpload(
	ctx context.Context,
	in io.Reader,
	o *Object,
	leaf, dirID string,
	size int64,
	options ...fs.OpenOption,
) (fs.Object, error) {
	fs.Debugf(o, "Starting traditional sample upload for size=%d", size)
	// 1. Initialize sample upload (traditional API)
	initResp, err := f.sampleInitUpload(ctx, size, leaf, dirID)
	if err != nil {
		return nil, fmt.Errorf("traditional sampleInitUpload failed: %w", err)
	}

	// 2. Perform the form upload
	callbackData, err := f.sampleUploadForm(ctx, in, initResp, leaf, options...)
	if err != nil {
		return nil, fmt.Errorf("traditional sampleUploadForm failed: %w", err)
	}

	// 3. Update object metadata from callback
	err = o.setMetaDataFromCallBack(callbackData)
	if err != nil {
		return nil, fmt.Errorf("failed to set metadata from sample upload callback: %w", err)
	}

	// 4. Mark complete on RereadableObject if applicable
	if ro, ok := in.(*RereadableObject); ok {
		ro.MarkComplete(ctx)
		fs.Debugf(o, "Marked RereadableObject transfer as complete after sample upload")
	}

	fs.Infof(o, "✅ 传统上传完成: %s", fs.SizeSuffix(size))
	fs.Debugf(o, "Traditional sample upload successful.")
	return o, nil
}

// isRemoteSource 检查源对象是否来自远程云盘（非本地文件）
func (f *Fs) isRemoteSource(src fs.ObjectInfo) bool {
	// 检查源对象的类型，如果不是本地文件系统，则认为是远程源
	srcFs := src.Fs()
	if srcFs == nil {
		fs.Debugf(f, " isRemoteSource: srcFs为nil，返回false")
		return false
	}

	// 检查是否是本地文件系统
	fsType := srcFs.Name()
	isRemote := fsType != "local" && fsType != ""

	fs.Debugf(f, " isRemoteSource检测: fsType='%s', isRemote=%v", fsType, isRemote)

	// 特别检测123网盘和其他云盘
	if strings.Contains(fsType, "123") || strings.Contains(fsType, "pan") {
		fs.Debugf(f, " 明确识别为云盘源: %s", fsType)
		return true
	}

	return isRemote
}

// 🔧 移除非标准的启发式文件判断函数
// 遵循Google Drive、OneDrive等主流网盘的做法：完全信任服务器返回的类型标识

// checkExistingTempFile 检查是否已有完整的临时下载文件，避免重复下载
func (f *Fs) checkExistingTempFile(src fs.ObjectInfo) (io.Reader, int64, func()) {
	// 修复重复下载：检查是否有匹配的临时文件
	// 这里可以基于文件名、大小、修改时间等生成临时文件路径
	expectedSize := src.Size()

	// 生成可能的临时文件路径（基于文件名和大小）
	tempPattern := fmt.Sprintf("*%s*%d*.tmp", src.Remote(), expectedSize)
	tempDir := os.TempDir()

	matches, err := filepath.Glob(filepath.Join(tempDir, tempPattern))
	if err != nil || len(matches) == 0 {
		return nil, 0, nil
	}

	// 检查最新的匹配文件
	for _, tempPath := range matches {
		stat, err := os.Stat(tempPath)
		if err != nil {
			continue
		}

		// 检查文件大小是否匹配
		if stat.Size() == expectedSize {
			// 检查文件是否是最近创建的（1小时内）
			if time.Since(stat.ModTime()) < time.Hour {
				file, err := os.Open(tempPath)
				if err != nil {
					continue
				}

				fs.Debugf(f, "Found matching temp file: %s", tempPath)

				cleanup := func() {
					file.Close()
					// 可选：使用后删除临时文件
					// os.Remove(tempPath)
				}

				return file, stat.Size(), cleanup
			}
		}
	}

	return nil, 0, nil
}

// upload is the main entry point that decides which upload strategy to use.
func (f *Fs) upload(ctx context.Context, in io.Reader, src fs.ObjectInfo, remote string, options ...fs.OpenOption) (fs.Object, error) {
	if f.isShare {
		return nil, errors.New("upload unsupported for shared filesystem")
	}

	// 关键修复：在上传开始时检查和优化内存配置
	f.ensureOptimalMemoryConfig(src.Size())

	// 设置上传标志，防止预热干扰上传
	f.uploadingMu.Lock()
	f.isUploading = true
	f.uploadingMu.Unlock()

	defer func() {
		f.uploadingMu.Lock()
		f.isUploading = false
		f.uploadingMu.Unlock()
	}()

	// Start the token renewer if we have a valid one
	if f.tokenRenewer != nil {
		f.tokenRenewer.Start()
		defer f.tokenRenewer.Stop()
	}

	size := src.Size()

	// If NoBuffer option is enabled, try to wrap the input in a RereadableObject early
	if f.opt.NoBuffer && size >= 0 {
		fs.Debugf(src, "Using no_buffer option: file will be read multiple times instead of buffered to disk")
		if _, ok := in.(*RereadableObject); !ok {
			// Create a rereadable wrapper if not already wrapped
			ro, roErr := NewRereadableObject(ctx, src, options...)
			if roErr != nil {
				// Log but continue with original reader
				fs.Logf(src, "Warning: Failed to create rereadable source: %v - will attempt to continue with original reader", roErr)
			} else {
				in = ro
			}
		}
	}

	// Check size limits
	if size > int64(maxUploadSize) {
		return nil, fmt.Errorf("file size %v exceeds upload limit %v", fs.SizeSuffix(size), maxUploadSize)
	}
	if size < 0 {
		// Streaming upload with unknown size - check if allowed
		if f.opt.OnlyStream {
			fs.Logf(src, "Streaming upload with unknown size using traditional sample upload (limit %v)", StreamUploadLimit)
			// Proceed with sample upload, it might fail if size > limit
		} else if f.opt.FastUpload {
			fs.Logf(src, "Streaming upload with unknown size using traditional sample upload (limit %v) due to fast_upload", StreamUploadLimit)
			// Proceed with sample upload
		} else {
			// Default behavior: Use OSS multipart for unknown size
			fs.Logf(src, "Streaming upload with unknown size using OSS multipart upload")
			// Proceed with OSS multipart
		}
		// Note: Hash upload is not possible for unknown size.
	}

	// Create placeholder object and ensure parent directory exists
	o, leaf, dirID, err := f.createObject(ctx, remote, src.ModTime(ctx), size)
	if err != nil {
		return nil, fmt.Errorf("failed to create object placeholder: %w", err)
	}

	// Defer cleanup for any temporary buffers created
	var cleanup func()
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	skipHashUpload := false
	// 🌐 跨云盘传输检测：优先尝试秒传，忽略大小限制
	if f.isRemoteSource(src) && size >= 0 {
		fs.Infof(o, "🌐 检测到跨云盘传输，强制尝试秒传...")
		gotIt, _, newIn, localCleanup, err := f.tryHashUpload(ctx, in, src, o, leaf, dirID, size, options...)
		cleanup = localCleanup // 设置清理函数
		if err != nil {
			fs.Logf(o, "🌐 跨云盘秒传失败，回退到正常上传: %v", err)
			// Set flag to skip subsequent instant upload attempts
			skipHashUpload = true
			// Reset state, continue normal upload process
			gotIt = false
			if !f.opt.NoBuffer {
				newIn = in // 恢复原始输入
				if cleanup != nil {
					cleanup()
					cleanup = nil
				}
			}
		} else if gotIt {
			fs.Infof(o, "🚀 跨云秒传成功！文件已存在于115服务器")
			return o, nil
		} else {
			// Set flag to skip subsequent instant upload attempts
			skipHashUpload = true
			fs.Infof(o, "☁️ 跨云传输直接进行OSS上传，避免重复处理")
			// Continue using newIn for subsequent upload
			in = newIn
		}
	}

	// 1. OnlyStream flag
	if f.opt.OnlyStream {
		if size < 0 || size <= int64(StreamUploadLimit) {
			return f.doSampleUpload(ctx, in, o, leaf, dirID, size, options...)
		}
		return nil, fserrors.NoRetryError(fmt.Errorf("only_stream is enabled but file size %v exceeds stream upload limit %v", fs.SizeSuffix(size), StreamUploadLimit))
	}

	// 2. FastUpload flag
	if f.opt.FastUpload {
		noHashSize := int64(f.opt.NohashSize)
		streamLimit := int64(StreamUploadLimit)

		if size >= 0 && size <= noHashSize {
			// Small file: Use sample upload directly
			return f.doSampleUpload(ctx, in, o, leaf, dirID, size, options...)
		}

		// Larger file or unknown size: Try hash upload first
		var gotIt bool
		var ui *UploadInitInfo
		var newIn io.Reader = in // Assume input doesn't change initially

		if size >= 0 { // Hash upload only possible for known size
			var localCleanup func()
			gotIt, ui, newIn, localCleanup, err = f.tryHashUpload(ctx, in, src, o, leaf, dirID, size, options...)
			cleanup = localCleanup // Assign cleanup function
			if err != nil {
				// Log hash upload error but fallback based on size
				fs.Logf(o, "FastUpload: Hash upload failed, falling back: %v", err)
				// Reset gotIt, ui, and newIn to ensure fallback happens correctly
				// with the original reader since bufferIO may have failed
				gotIt = false
				ui = nil

				// If no_buffer is not enabled, revert to original input
				if !f.opt.NoBuffer {
					newIn = in // Important: revert to original input if hash calculation failed
					if cleanup != nil {
						cleanup()     // Clean up any partial buffers
						cleanup = nil // Mark as cleaned up
					}
				} else {
					// With no_buffer, we need to explicitly reopen the RereadableObject
					// to ensure we're at the beginning of the stream for the next upload attempt
					if ro, ok := newIn.(*RereadableObject); ok {
						fs.Debugf(o, "Reopening RereadableObject after failed hash calculation")
						reopenedReader, reopenErr := ro.Open()
						if reopenErr != nil {
							return nil, fmt.Errorf("failed to reopen source after hash upload failure: %w", reopenErr)
						}
						// IMPORTANT: Actually use the returned reader instead of assuming internal state is reset
						newIn = reopenedReader
						fs.Debugf(o, "Successfully reopened RereadableObject for upload attempt")
					} else {
						// If not a RereadableObject, fall back to original input as a last resort
						fs.Logf(o, "Warning: Expected RereadableObject in no_buffer mode but got %T, reverting to original input", newIn)
						newIn = in
					}
				}
			}
			if gotIt {
				return o, nil // Hash upload successful
			}
		} else {
			fs.Debugf(o, "FastUpload: Skipping hash upload for unknown size.")
		}

		// Hash upload failed or skipped: Fallback based on size
		if size < 0 || size <= streamLimit {
			// Use sample upload if within limit or size unknown
			return f.doSampleUpload(ctx, newIn, o, leaf, dirID, size, options...)
		}
		// Size > streamLimit: Try OSS multipart, fallback to sample upload if OSS not available
		result, err := f.uploadToOSS(ctx, newIn, src, o, leaf, dirID, size, ui, options...)
		if err != nil && f.shouldFallbackToTraditional(err) {
			fs.Debugf(o, "OSS multipart failed (%v), falling back to sample upload", err)
			return f.doSampleUpload(ctx, newIn, o, leaf, dirID, size, options...)
		}
		return result, err
	}

	// 3. UploadHashOnly flag
	if f.opt.UploadHashOnly {
		if size < 0 {
			return nil, fserrors.NoRetryError(errors.New("upload_hash_only requires known file size"))
		}
		gotIt, _, _, localCleanup, err := f.tryHashUpload(ctx, in, src, o, leaf, dirID, size, options...)
		// Handle cleanup of any temporary resources
		if localCleanup != nil {
			defer localCleanup()
		}
		if err != nil {
			return nil, fmt.Errorf("upload_hash_only: hash upload attempt failed: %w", err)
		}
		if gotIt {
			return o, nil // Success
		}
		// Hash upload didn't find the file on server
		return nil, fserrors.NoRetryError(errors.New("upload_hash_only: file not found on server via hash check, skipping upload"))
	}

	// Cross-cloud transfer check is already handled above

	// 🎯 默认上传策略：优先使用OSS上传，智能选择单文件或分片
	uploadCutoff := int64(f.opt.UploadCutoff) // 用户配置的OSS优化阈值

	// 🔧 性能优化：进一步扩大传统上传的使用范围，与123网盘策略对齐
	// 对于中等文件（≤200MB），优先使用传统上传，避免OSS分片开销
	if size >= 0 && size <= int64(200*fs.Mebi) {
		fs.Debugf(o, "🚀 中等文件 (%s) 使用传统上传，提高效率", fs.SizeSuffix(size))
		return f.doSampleUpload(ctx, in, o, leaf, dirID, size, options...)
	}

	// Known size >= noHashSize OR unknown size: Try hash upload first (if size known)
	var gotIt bool
	var ui *UploadInitInfo
	var newIn io.Reader = in

	if size >= 0 && !skipHashUpload {
		var localCleanup func()
		gotIt, ui, newIn, localCleanup, err = f.tryHashUpload(ctx, in, src, o, leaf, dirID, size, options...)
		cleanup = localCleanup
		if err != nil {
			fs.Logf(o, "Normal Upload: Hash upload failed, falling back to OSS/Sample: %v", err)
			// Reset gotIt, ui, and newIn for fallback
			gotIt = false
			ui = nil

			// If no_buffer is not enabled, revert to original input
			if !f.opt.NoBuffer {
				newIn = in // Important: revert to original input if hash calculation failed
				if cleanup != nil {
					cleanup()     // Clean up any partial buffers
					cleanup = nil // Mark as cleaned up
				}
			} else {
				// With no_buffer, we need to explicitly reopen the RereadableObject
				// to ensure we're at the beginning of the stream for the next upload attempt
				if ro, ok := newIn.(*RereadableObject); ok {
					fs.Debugf(o, "Reopening RereadableObject after failed hash calculation")
					reopenedReader, reopenErr := ro.Open()
					if reopenErr != nil {
						return nil, fmt.Errorf("failed to reopen source after hash upload failure: %w", reopenErr)
					}
					// IMPORTANT: Actually use the returned reader instead of assuming internal state is reset
					newIn = reopenedReader
					fs.Debugf(o, "Successfully reopened RereadableObject for upload attempt")
				} else {
					// If not a RereadableObject, fall back to original input as a last resort
					fs.Logf(o, "Warning: Expected RereadableObject in no_buffer mode but got %T, reverting to original input", newIn)
					newIn = in
				}
			}
		}
		if gotIt {
			return o, nil // Hash upload successful
		}
	} else {
		fs.Debugf(o, "Normal Upload: Skipping hash upload for unknown size.")
	}

	// Reset state if hash upload was skipped
	if skipHashUpload {
		fs.Debugf(o, "Skipping hash upload (already attempted), proceeding to OSS upload")
		gotIt = false
		ui = nil
		newIn = in
	}

	// Hash upload failed or skipped: Decide between Sample and OSS Multipart
	// Note: OpenAPI doesn't support traditional sample upload.
	// If hash upload failed, we *must* use OSS multipart upload via OpenAPI.
	// The only time sample upload is used now is with OnlyStream or FastUpload flags for small files.
	// Therefore, the default path always leads to OSS multipart here.

	// Check against UploadCutoff to decide multipart (though logic above mostly covers this)
	if size < 0 || size >= uploadCutoff {
		// Try OSS multipart upload, fallback to sample upload if OSS not available
		result, err := f.uploadToOSS(ctx, newIn, src, o, leaf, dirID, size, ui, options...)
		if err != nil && f.shouldFallbackToTraditional(err) {
			fs.Debugf(o, "OSS multipart failed (%v), falling back to sample upload", err)
			return f.doSampleUpload(ctx, newIn, o, leaf, dirID, size, options...)
		}
		return result, err
	}

	// Size is known, >= noHashSize, and < uploadCutoff
	// This case implies hash upload failed, and we should use OSS multipart (PutObject)
	// because sample upload is traditional only.
	fs.Debugf(o, "Normal Upload: Using OSS PutObject (single part) for size %v", fs.SizeSuffix(size))
	// Need a function similar to uploadToOSS but using PutObject instead of multipart.
	// Let's adapt uploadToOSS slightly or create a helper.
	// For simplicity, let uploadToOSS handle the PutObject case internally based on size < cutoff.
	// We need to modify uploadToOSS or multipart.go to handle this.
	// Let's assume uploadToOSS handles it for now. Revisit if multipart.go doesn't.
	// *** Correction: The current multipart.go doesn't handle single PutObject. ***
	// We need to implement the PutObject path here.

	// --- OSS PutObject Implementation ---
	fs.Debugf(o, "Executing OSS PutObject...")
	// 1. Get UploadInitInfo if not already available (from failed hash check)
	if ui == nil {
		// 关键修复：OSS PutObject也需要SHA1，必须先计算
		var sha1sum string
		if o.sha1sum != "" {
			sha1sum = o.sha1sum
		} else {
			// 尝试从源对象获取SHA1
			if hash, hashErr := src.Hash(ctx, hash.SHA1); hashErr == nil && hash != "" {
				sha1sum = strings.ToUpper(hash)
			} else {
				// 跨网盘复制时源对象可能无法提供SHA1，需要从数据流计算
				fs.Debugf(o, "Source SHA1 not available, will calculate from data stream during upload")
				// 暂时使用空SHA1，在实际上传时计算
				sha1sum = ""
			}
		}

		var initErr error
		ui, initErr = f.initUploadOpenAPI(ctx, size, leaf, dirID, sha1sum, "", "", "")
		if initErr != nil {
			return nil, fmt.Errorf("failed to initialize PutObject upload: %w", initErr)
		}
		// 🔍 检查上传初始化状态和OSS信息
		if ui.GetStatus() == 1 {
			// 状态1：需要上传，检查OSS信息是否完整
			if ui.GetBucket() == "" || ui.GetObject() == "" {
				fs.Debugf(o, "🔧 状态1但OSS信息缺失 (bucket=%q, object=%q)，降级到传统上传",
					ui.GetBucket(), ui.GetObject())
				return f.performTraditionalUpload(ctx, in, o, leaf, dirID, size, ui, options...)
			}
			fs.Debugf(o, "✅ 状态1且OSS信息完整，继续OSS上传: bucket=%q, object=%q",
				ui.GetBucket(), ui.GetObject())
		} else if ui.GetStatus() == 2 {
			// 状态2：意外的秒传成功
			fs.Infof(o, "🎉 初始化时发现文件已存在，秒传成功")
			o.id = ui.GetFileID()
			o.pickCode = ui.GetPickCode()
			o.hasMetaData = true
			return o, nil
		} else if ui.GetStatus() == 7 {
			// 状态7：需要二次认证
			fs.Debugf(o, "OSS PutObject requires secondary auth (status 7). Calculating range SHA1...")
			signKey := ui.GetSignKey()
			signCheckRange := ui.GetSignCheck()
			if signKey == "" || signCheckRange == "" {
				return nil, errors.New("OSS PutObject status 7 but sign_key or sign_check missing")
			}

			// 关键修复：创建可重读的对象来计算range SHA1，避免消耗原始流
			var rereadableObj *RereadableObject
			var signVal string

			if ro, ok := in.(*RereadableObject); ok {
				// 已经是RereadableObject，直接使用
				rereadableObj = ro
			} else {
				// 创建新的RereadableObject
				var roErr error
				rereadableObj, roErr = NewRereadableObject(ctx, src, options...)
				if roErr != nil {
					return nil, fmt.Errorf("failed to create rereadable object for range SHA1: %w", roErr)
				}
				defer func() { _ = rereadableObj.Close() }()
			}

			// Calculate SHA1 for the specified range using rereadable object
			signVal, err = calcBlockSHA1(ctx, rereadableObj, src, signCheckRange)
			if err != nil {
				return nil, fmt.Errorf("failed to calculate SHA1 for range %q: %w", signCheckRange, err)
			}
			fs.Debugf(o, "Calculated range SHA1: %s for range %s", signVal, signCheckRange)

			// Retry initUpload with sign_key and sign_val
			ui, err = f.initUploadOpenAPI(ctx, size, leaf, dirID, sha1sum, "", signKey, signVal)
			if err != nil {
				return nil, fmt.Errorf("OSS PutObject retry with signature failed: %w", err)
			}
			// Re-check the new status
			if ui.GetStatus() == 2 {
				// 意外触发秒传
				fs.Infof(o, "🎉 二次认证后触发秒传成功")
				o.id = ui.GetFileID()
				o.pickCode = ui.GetPickCode()
				o.hasMetaData = true
				return o, nil
			} else if ui.GetStatus() != 1 && ui.GetStatus() != 8 {
				return nil, fmt.Errorf("OSS PutObject auth retry failed with status %d: %s", ui.GetStatus(), ui.ErrMsg())
			}
			fs.Debugf(o, "✅ 二次认证成功，状态%d，继续OSS上传", ui.GetStatus())
		} else if ui.GetStatus() == 8 {
			// 状态8：特殊认证状态，检查OSS信息
			fs.Debugf(o, "🔄 状态8，检查OSS信息: bucket=%q, object=%q",
				ui.GetBucket(), ui.GetObject())
			if ui.GetBucket() == "" || ui.GetObject() == "" {
				fs.Debugf(o, "🔄 状态8且OSS信息缺失，降级到传统上传")
				return f.performTraditionalUpload(ctx, in, o, leaf, dirID, size, ui, options...)
			}
			fs.Debugf(o, "✅ 状态8且OSS信息完整，继续OSS上传")
		} else {
			return nil, fmt.Errorf("❌ OSS上传初始化状态异常: 状态=%d, 消息=%s",
				ui.GetStatus(), ui.ErrMsg())
		}
	}

	// 🎯 智能OSS策略：优先尝试获取OSS信息，必要时降级
	if ui.GetBucket() == "" || ui.GetObject() == "" {
		fs.Infof(o, "🔧 OSS信息缺失 (bucket=%q, object=%q, status=%d)，尝试获取完整OSS信息",
			ui.GetBucket(), ui.GetObject(), ui.GetStatus())

		// 对于状态7/8，先尝试获取OSS信息，失败后再降级
		if ui.GetStatus() == 7 || ui.GetStatus() == 8 {
			fs.Debugf(o, "🔍 状态%d通常无OSS信息，但仍尝试获取", ui.GetStatus())
		}

		// 🎯 策略1：尝试无SHA1初始化，强制获取OSS信息
		fs.Debugf(o, "🎯 策略1：无SHA1初始化，强制获取OSS信息")
		newUI, initErr := f.initUploadOpenAPI(ctx, size, leaf, dirID, "", "", "", "")
		if initErr != nil {
			fs.Debugf(o, "🎯 策略1失败: %v", initErr)

			// 🎯 策略2：尝试使用正确SHA1重新初始化
			var correctSHA1 string
			if o.sha1sum != "" {
				correctSHA1 = o.sha1sum
			} else if hash, hashErr := src.Hash(ctx, hash.SHA1); hashErr == nil && hash != "" {
				correctSHA1 = strings.ToUpper(hash)
			}

			if correctSHA1 != "" {
				fs.Debugf(o, "🎯 策略2：使用正确SHA1重新初始化: %s", correctSHA1)
				newUI, initErr = f.initUploadOpenAPI(ctx, size, leaf, dirID, correctSHA1, "", "", "")
			}

			if initErr != nil {
				fs.Infof(o, "📋 无法获取OSS信息，使用传统上传: %v", initErr)
				return f.performTraditionalUpload(ctx, in, o, leaf, dirID, size, ui, options...)
			}
		}

		// 🔍 详细检查获取结果
		fs.Infof(o, "🔍 详细API响应分析:")
		fs.Infof(o, "   状态码: %d", newUI.GetStatus())
		fs.Infof(o, "   Bucket: %q", newUI.GetBucket())
		fs.Infof(o, "   Object: %q", newUI.GetObject())
		fs.Infof(o, "   FileID: %q", newUI.GetFileID())
		fs.Infof(o, "   PickCode: %q", newUI.GetPickCode())
		fs.Infof(o, "   Callback: %q", newUI.GetCallback())
		fs.Infof(o, "   CallbackVar: %q", newUI.GetCallbackVar())
		fs.Infof(o, "   SignKey: %q", newUI.GetSignKey())
		fs.Infof(o, "   SignCheck: %q", newUI.GetSignCheck())
		fs.Infof(o, "   错误信息: %q", newUI.ErrMsg())

		if newUI.GetStatus() == 2 {
			// 意外触发秒传
			fs.Infof(o, "🎉 获取OSS信息时触发秒传成功")
			o.id = newUI.GetFileID()
			o.pickCode = newUI.GetPickCode()
			o.hasMetaData = true
			return o, nil
		} else if newUI.GetBucket() != "" && newUI.GetObject() != "" {
			// 成功获取OSS信息
			fs.Infof(o, "✅ OSS信息获取成功: bucket=%q, object=%q",
				newUI.GetBucket(), newUI.GetObject())
			ui = newUI // 使用新的UI信息
		} else {
			// 仍然无法获取OSS信息，降级到传统上传
			fs.Infof(o, "📋 115网盘此文件不支持OSS上传 (状态%d)，使用传统上传",
				newUI.GetStatus())
			return f.performTraditionalUpload(ctx, in, o, leaf, dirID, size, ui, options...)
		}
	}

	// 2. Create OSS client
	ossClient, err := f.newOSSClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create OSS client for PutObject: %w", err)
	}

	// 3. Prepare PutObject request
	// 参考阿里云OSS示例，优化PutObject请求配置
	req := &oss.PutObjectRequest{
		Bucket:      oss.Ptr(ui.GetBucket()),
		Key:         oss.Ptr(ui.GetObject()),
		Body:        newIn, // Use potentially buffered reader
		Callback:    oss.Ptr(ui.GetCallback()),
		CallbackVar: oss.Ptr(ui.GetCallbackVar()),
		// 修复进度回调：确保total参数有效，避免负数或零值导致的异常
		ProgressFn: func() func(increment, transferred, total int64) {
			var (
				lastTransferred int64
				lastLogTime     time.Time
				lastLogPercent  int
			)
			return func(increment, transferred, total int64) {
				// 修复120%进度问题：移除重复的Account更新
				// AccountedFileReader已经通过Read()方法自动更新Account进度
				// 这里再次更新会导致重复计数，造成进度超过100%

				// 只保留调试日志，不更新Account对象
				if transferred > lastTransferred {
					// 修复：检查total参数有效性，避免除零或负数
					var percentage int
					if total > 0 {
						percentage = int(float64(transferred) / float64(total) * 100)
					} else {
						// 如果total无效，使用文件大小作为参考
						if size > 0 {
							percentage = int(float64(transferred) / float64(size) * 100)
						} else {
							percentage = 0
						}
					}

					now := time.Now()
					// 减少日志频率：只在进度变化超过10%或时间间隔超过5秒时输出日志
					shouldLog := (percentage >= lastLogPercent+10) ||
						(now.Sub(lastLogTime) > 5*time.Second) ||
						(transferred == total) ||
						(percentage == 100)

					if shouldLog {
						fs.Debugf(o, "📊 OSS上传进度: %s/%s (%d%%) [文件大小: %s]",
							fs.SizeSuffix(transferred),
							fs.SizeSuffix(max(total, size)),
							percentage,
							fs.SizeSuffix(size))
						lastLogTime = now
						lastLogPercent = percentage
					}
					lastTransferred = transferred
				}
			}
		}(),
	}
	// Apply headers from options
	for _, option := range options {
		key, value := option.Header()
		lowerKey := strings.ToLower(key)
		switch lowerKey {
		case "cache-control":
			req.CacheControl = oss.Ptr(value)
		case "content-disposition":
			req.ContentDisposition = oss.Ptr(value)
		case "content-encoding":
			req.ContentEncoding = oss.Ptr(value)
		case "content-type":
			req.ContentType = oss.Ptr(value)
		}
	}

	// 🚀 开始OSS单文件上传
	fs.Infof(o, "🚀 开始上传: %s (%s)",
		leaf, fs.SizeSuffix(size))

	// 平衡性能优化：使用OSS专用调速器，避免过度请求导致限制
	// OSS上传虽然直连阿里云，但仍需要合理的QPS控制避免触发反制措施

	var putRes *oss.PutObjectResult
	err = f.pacer.Call(func() (bool, error) {
		var putErr error
		putRes, putErr = ossClient.PutObject(ctx, req)
		retry, retryErr := shouldRetry(ctx, nil, putErr)
		if retry {
			// Rewind body if possible before retry
			if seeker, ok := newIn.(io.Seeker); ok {
				_, _ = seeker.Seek(0, io.SeekStart)
			} else {
				// Cannot retry non-seekable stream after partial read
				return false, fmt.Errorf("cannot retry PutObject with non-seekable stream: %w", putErr)
			}
			return true, retryErr
		}
		if putErr != nil {
			return false, putErr
		}
		return false, nil // Success
	})
	if err != nil {
		return nil, fmt.Errorf("OSS PutObject failed: %w", err)
	}

	// 5. Process callback
	callbackData, err := f.postUpload(putRes.CallbackResult)
	if err != nil {
		return nil, fmt.Errorf("failed to process PutObject callback: %w", err)
	}

	// 6. Update metadata
	err = o.setMetaDataFromCallBack(callbackData)
	if err != nil {
		return nil, fmt.Errorf("failed to set metadata from PutObject callback: %w", err)
	}

	// 7. Mark complete on RereadableObject if applicable
	if ro, ok := newIn.(*RereadableObject); ok {
		ro.MarkComplete(ctx)
		fs.Debugf(o, "✅ PutObject上传后标记RereadableObject传输完成")
	}

	fs.Infof(o, "✅ 文件上传完成: %s", fs.SizeSuffix(size))
	fs.Debugf(o, "✅ OSS PutObject成功")
	return o, nil
}

// ============================================================================
// Functions from multipart.go
// ============================================================================

// 115网盘分片上传实现
// 采用单线程顺序上传模式，确保上传稳定性和SHA1验证通过

const (
	// 💾 性能优化缓冲区配置：512MB缓冲区，提升跨云传输性能
	bufferSize           = 512 * 1024 * 1024 // 512MB缓冲区，性能优化
	bufferCacheFlushTime = 5 * time.Second   // 缓冲区清理时间
)

// bufferPool is a global pool of buffers
var (
	bufferPool     *pool.Pool
	bufferPoolOnce sync.Once
)

// get a buffer pool
func getPool() *pool.Pool {
	bufferPoolOnce.Do(func() {
		ci := fs.GetConfig(context.Background())

		// VPS友好：根据用户的--max-buffer-memory设置动态调整
		maxMemory := int64(ci.BufferSize)

		// 计算合适的缓冲区配置
		actualBufferSize := int64(bufferSize) // 默认512MB
		bufferCount := 4                      // 默认4个

		// 🔧 性能优化：智能内存配置策略，支持更高性能配置
		if maxMemory > 0 {
			// 根据用户内存限制调整缓冲区大小和数量
			if maxMemory <= 64*1024*1024 { // ≤64MB：超低配VPS
				actualBufferSize = 16 * 1024 * 1024 // 16MB缓冲区
				bufferCount = 2                     // 2个 = 32MB总内存
				fs.Infof(nil, "115网盘超低配模式：用户内存限制 %s，使用 %s × %d 缓冲区",
					fs.SizeSuffix(maxMemory), fs.SizeSuffix(actualBufferSize), bufferCount)
			} else if maxMemory <= 128*1024*1024 { // ≤128MB：低配VPS
				actualBufferSize = 32 * 1024 * 1024 // 32MB缓冲区
				bufferCount = 2                     // 2个 = 64MB总内存
				fs.Infof(nil, "115网盘低配模式：用户内存限制 %s，使用 %s × %d 缓冲区",
					fs.SizeSuffix(maxMemory), fs.SizeSuffix(actualBufferSize), bufferCount)
			} else if maxMemory <= 256*1024*1024 { // ≤256MB：中配VPS
				actualBufferSize = 64 * 1024 * 1024 // 64MB缓冲区
				bufferCount = 2                     // 2个 = 128MB总内存
				fs.Infof(nil, "115网盘中配模式：用户内存限制 %s，使用 %s × %d 缓冲区",
					fs.SizeSuffix(maxMemory), fs.SizeSuffix(actualBufferSize), bufferCount)
			} else if maxMemory <= 512*1024*1024 { // ≤512MB：高配VPS
				actualBufferSize = 128 * 1024 * 1024 // 128MB缓冲区
				bufferCount = 2                      // 2个 = 256MB总内存
				fs.Infof(nil, "115网盘高配模式：用户内存限制 %s，使用 %s × %d 缓冲区",
					fs.SizeSuffix(maxMemory), fs.SizeSuffix(actualBufferSize), bufferCount)
			} else if maxMemory <= 1024*1024*1024 { // ≤1GB：超高配
				actualBufferSize = 256 * 1024 * 1024 // 256MB缓冲区
				bufferCount = 2                      // 2个 = 512MB总内存
				fs.Infof(nil, "115网盘超高配模式：用户内存限制 %s，使用 %s × %d 缓冲区",
					fs.SizeSuffix(maxMemory), fs.SizeSuffix(actualBufferSize), bufferCount)
			} else { // >1GB：极高配
				// 使用默认的512MB × 4个配置 = 2GB总内存
				fs.Infof(nil, "115网盘极高配模式：用户内存充足 %s，使用 %s × %d 缓冲区",
					fs.SizeSuffix(maxMemory), fs.SizeSuffix(actualBufferSize), bufferCount)
			}
		} else {
			// 未设置内存限制，使用默认高性能配置
			fs.Infof(nil, "115网盘默认模式：未设置内存限制，使用 %s × %d 缓冲区",
				fs.SizeSuffix(actualBufferSize), bufferCount)
		}

		// 创建缓冲区池
		bufferPool = pool.New(bufferCacheFlushTime, int(actualBufferSize), bufferCount, ci.UseMmap)
		totalMemory := actualBufferSize * int64(bufferCount)
		fs.Debugf(nil, "115网盘缓冲区池：%s × %d = %s 总内存",
			fs.SizeSuffix(actualBufferSize), bufferCount, fs.SizeSuffix(totalMemory))
	})
	return bufferPool
}

// NewRW gets a pool.RW using the multipart pool
func NewRW() *pool.RW {
	return pool.NewRW(getPool())
}

// Upload 执行115网盘单线程分片上传
// 参考阿里云OSS示例优化：采用顺序上传模式，确保上传稳定性和SHA1验证通过
func (w *ossChunkWriter) Upload(ctx context.Context) (err error) {
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 🧹 确保失败时清理OSS资源
	defer func() {
		if err != nil && w.imur != nil {
			fs.Debugf(w.o, "🧹 上传失败，清理OSS分片上传: %v", err)
			// 🔒 使用独立context进行清理，避免被取消的context影响清理操作
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()

			if abortErr := w.Abort(cleanupCtx); abortErr != nil {
				fs.Errorf(w.o, "❌ 清理OSS分片上传失败: %v", abortErr)
			} else {
				fs.Debugf(w.o, "✅ OSS分片上传清理成功")
			}
		}
	}()

	// 初始化上传参数
	uploadParams := w.initializeUploadParams()

	// 获取Account对象和输入流
	acc, in := w.setupAccountingAndInput()

	// 执行分片上传循环
	actualParts, err := w.performChunkedUpload(uploadCtx, in, acc, uploadParams)
	if err != nil {
		return err
	}

	// 完成上传并显示统计信息
	return w.finalizeUpload(ctx, actualParts, uploadParams.startTime)
}

// uploadParams 上传参数结构体
type uploadParams struct {
	size       int64
	chunkSize  int64
	totalParts int64
	startTime  time.Time
}

// initializeUploadParams 初始化上传参数
func (w *ossChunkWriter) initializeUploadParams() *uploadParams {
	params := &uploadParams{
		size:      w.size,
		chunkSize: w.chunkSize,
		startTime: time.Now(),
	}

	// 计算总分片数用于进度显示
	if params.size > 0 {
		params.totalParts = (params.size + params.chunkSize - 1) / params.chunkSize
	} else {
		fs.Debugf(w.o, "File size unknown, will calculate chunk count dynamically")
	}

	return params
}

// setupAccountingAndInput 设置Account对象和输入流
func (w *ossChunkWriter) setupAccountingAndInput() (*accounting.Account, io.Reader) {
	var acc *accounting.Account
	var in io.Reader

	// 🔧 修复Account对象识别问题：增加更多识别方法

	// 方法1：检查是否为RereadableObject类型
	if rereadableObj, ok := w.in.(*RereadableObject); ok {
		acc = rereadableObj.GetAccount()
		in = rereadableObj.OldStream()
		if acc != nil {
			fs.Debugf(w.o, "✅ 从RereadableObject获取Account对象，进度将正确显示")
			return acc, in
		}
		fs.Debugf(w.o, "⚠️ RereadableObject中无Account对象，尝试其他方法")
	}

	// 方法2：检查是否为AccountedFileReader类型
	if accountedReader, ok := w.in.(*AccountedFileReader); ok {
		// AccountedFileReader的进度由rclone标准机制管理
		fs.Debugf(w.o, "✅ 检测到AccountedFileReader，使用rclone标准进度管理")
		return nil, accountedReader
	}

	// 方法3：尝试标准的UnWrapAccounting方法
	in, acc = accounting.UnWrapAccounting(w.in)
	if acc != nil {
		fs.Debugf(w.o, "✅ 通过UnWrapAccounting获取Account对象")
		return acc, in
	}

	// 方法4：检查是否为accountStream类型（更深层的检查）
	if accountStream, ok := w.in.(interface{ GetAccount() *accounting.Account }); ok {
		if acc := accountStream.GetAccount(); acc != nil {
			fs.Debugf(w.o, "✅ 通过GetAccount接口获取Account对象")
			return acc, w.in
		}
	}

	// 方法5：检查是否实现了Accounter接口
	if accounter, ok := w.in.(accounting.Accounter); ok {
		unwrapped := accounter.OldStream()
		if unwrapped != w.in {
			// 递归检查unwrapped的Reader
			if innerIn, innerAcc := accounting.UnWrapAccounting(unwrapped); innerAcc != nil {
				fs.Debugf(w.o, "✅ 通过Accounter接口递归获取Account对象")
				return innerAcc, innerIn
			}
		}
	}

	// 🔧 方法6：特殊处理123网盘的ProgressReadCloser类型
	inputType := fmt.Sprintf("%T", w.in)
	if strings.Contains(inputType, "ProgressReadCloser") {
		fs.Debugf(w.o, "🔍 检测到ProgressReadCloser类型: %s，尝试特殊处理", inputType)

		// 对于ProgressReadCloser，我们直接使用它，让rclone的accounting系统处理进度
		// 这样可以避免重复的进度跟踪
		fs.Debugf(w.o, "✅ ProgressReadCloser将由rclone标准accounting系统处理进度")
		return nil, w.in
	}

	// 所有方法都失败，但不影响上传功能
	fs.Debugf(w.o, "⚠️ 无法获取Account对象 (输入类型: %T)，上传将继续但进度可能不显示", w.in)
	fs.Debugf(w.o, "💡 这通常发生在本地文件上传时，不影响上传功能")

	return nil, w.in
}

// performChunkedUpload 执行分片上传循环
func (w *ossChunkWriter) performChunkedUpload(ctx context.Context, in io.Reader, acc *accounting.Account, params *uploadParams) (int64, error) {
	fs.Infof(w.o, "Starting 115 single-threaded chunk upload")
	fs.Infof(w.o, "Upload parameters: file size=%v, chunk size=%v, estimated chunks=%d",
		fs.SizeSuffix(params.size), fs.SizeSuffix(params.chunkSize), params.totalParts)

	// 启动传输速度监控
	var monitor *TransferSpeedMonitor
	if acc != nil {
		monitor = w.f.startTransferMonitor(w.o.remote, acc, ctx)
	}

	// 确保在函数结束时停止监控
	defer func() {
		if monitor != nil {
			w.f.stopTransferMonitor(w.o.remote)
		}
	}()

	var (
		finished = false
		off      int64
		partNum  int64
	)

	for partNum = 0; !finished; partNum++ {
		// 检查上下文是否已取消
		if ctx.Err() != nil {
			fs.Debugf(w.o, " 上传被取消，停止分片上传")
			break
		}

		// 分片上传健康检查（默认启用）
		if partNum > 0 {
			timeoutCount := w.getTimeoutStats()
			// 如果超时次数过多，提供警告
			if timeoutCount >= 5 {
				fs.Infof(w.o, "⚠️ 分片上传健康检查: 已发生 %d 次超时，建议检查网络连接", timeoutCount)
			}
		}

		// 上传单个分片
		n, chunkFinished, err := w.uploadSinglePart(ctx, in, acc, partNum, params, off)
		if err != nil {
			return partNum, err
		}

		finished = chunkFinished
		off += n
	}

	return partNum, nil
}

// uploadSinglePart 上传单个分片
func (w *ossChunkWriter) uploadSinglePart(ctx context.Context, in io.Reader, acc *accounting.Account, partNum int64, params *uploadParams, off int64) (int64, bool, error) {
	// 获取内存缓冲区
	rw := NewRW()
	defer rw.Close()

	if acc != nil {
		rw.SetAccounting(acc.AccountRead)
	}

	// 读取分片数据
	chunkStartTime := time.Now()
	fs.Debugf(w.o, "开始读取分片 %d，目标大小: %s", partNum+1, fs.SizeSuffix(params.chunkSize))
	n, err := io.CopyN(rw, in, params.chunkSize)
	fs.Debugf(w.o, "分片 %d 读取完成，实际大小: %s，耗时: %v", partNum+1, fs.SizeSuffix(n), time.Since(chunkStartTime))
	finished := false

	if err == io.EOF {
		if n == 0 && partNum != 0 {
			// All chunks read complete
			return 0, true, nil
		}
		finished = true
	} else if err != nil {
		return 0, false, fmt.Errorf("failed to read chunk data: %w", err)
	}

	// 显示进度信息
	w.logUploadProgress(partNum, n, off, params)

	// Upload chunk
	currentPart := partNum + 1

	chunkSize, err := w.uploadChunkWithTimeout(ctx, int32(partNum), rw)
	if err != nil {
		return 0, false, fmt.Errorf("upload chunk %d failed (size: %v, offset: %v, timeout/retried): %w",
			currentPart, fs.SizeSuffix(n), fs.SizeSuffix(off), err)
	}

	// 记录分片上传成功信息
	w.logChunkSuccess(partNum, chunkSize, chunkStartTime)

	return n, finished, nil
}

// logUploadProgress 记录上传进度
func (w *ossChunkWriter) logUploadProgress(partNum, n, off int64, params *uploadParams) {
	currentPart := partNum + 1
	elapsed := time.Since(params.startTime)

	if params.totalParts > 0 {
		percentage := float64(currentPart) / float64(params.totalParts) * 100

		// 每2个分片或关键节点显示进度
		shouldLog := (currentPart == 1) || (currentPart%2 == 0) || (currentPart == params.totalParts)

		if shouldLog {
			avgTimePerPart := elapsed / time.Duration(currentPart)
			remainingParts := params.totalParts - currentPart
			estimatedRemaining := avgTimePerPart * time.Duration(remainingParts)

			// 减少进度日志频率：只在每10个分片或关键节点时输出
			if currentPart%10 == 0 || currentPart == 1 || currentPart == params.totalParts {
				fs.Infof(w.o, "📤 上传进度: %d/%d (%.1f%%) | %v | ⏱️ %v | 🕒 剩余 %v",
					currentPart, params.totalParts, percentage, fs.SizeSuffix(n),
					elapsed.Truncate(time.Second), estimatedRemaining.Truncate(time.Second))
			} else {
				fs.Debugf(w.o, "📤 上传分片: %d/%d", currentPart, params.totalParts)
			}
		}
	} else if currentPart == 1 {
		// 未知大小时只在第一个分片输出日志
		fs.Infof(w.o, "📤 上传分片: %d | %v | 偏移: %v | ⏱️ %v",
			currentPart, fs.SizeSuffix(n), fs.SizeSuffix(off), elapsed.Truncate(time.Second))
	}
}

// logChunkSuccess records chunk upload success (simplified)
func (w *ossChunkWriter) logChunkSuccess(partNum, chunkSize int64, chunkStartTime time.Time) {
	// Chunk upload success logged at higher level
}

// finalizeUpload 完成上传并显示统计信息
func (w *ossChunkWriter) finalizeUpload(ctx context.Context, actualParts int64, startTime time.Time) error {
	totalDuration := time.Since(startTime)

	fs.Infof(w.o, "🏁 完成分片上传: 共%d个分片 | ⏱️ 总用时 %v", actualParts, totalDuration.Truncate(time.Second))

	err := w.Close(ctx)
	if err != nil {
		return fmt.Errorf("failed to complete chunk upload: %w", err)
	}

	// 显示上传完成统计信息
	timeoutCount := w.getTimeoutStats()

	if w.size > 0 && totalDuration.Seconds() > 0 {
		avgSpeed := float64(w.size) / totalDuration.Seconds() / 1024 / 1024 // MB/s
		fs.Infof(w.o, "115 multipart upload completed")

		if timeoutCount > 0 {
			fs.Infof(w.o, "Upload stats: size=%v, parts=%d, duration=%v, speed=%.2fMB/s, timeouts=%d",
				fs.SizeSuffix(w.size), actualParts, totalDuration.Truncate(time.Second), avgSpeed, timeoutCount)
		} else {
			fs.Infof(w.o, "Upload stats: size=%v, parts=%d, duration=%v, speed=%.2fMB/s",
				fs.SizeSuffix(w.size), actualParts, totalDuration.Truncate(time.Second), avgSpeed)
		}
	} else {
		if timeoutCount > 0 {
			fs.Infof(w.o, "115 multipart upload completed: parts=%d, duration=%v, timeouts=%d",
				actualParts, totalDuration.Truncate(time.Second), timeoutCount)
		} else {
			fs.Infof(w.o, "115 multipart upload completed: parts=%d, duration=%v",
				actualParts, totalDuration.Truncate(time.Second))
		}
	}

	// 如果有超时，提供建议
	if timeoutCount > 0 {
		fs.Infof(w.o, "💡 建议：如果经常出现超时，可以尝试增加chunk_timeout配置或检查网络连接")
	}

	return nil
}

var warnStreamUpload sync.Once

// ossChunkWriter 115网盘分片上传写入器
// 简化版本：单线程顺序上传模式，无断点续传功能
type ossChunkWriter struct {
	chunkSize     int64                              // 分片大小
	size          int64                              // 文件总大小
	f             *Fs                                // 文件系统实例
	o             *Object                            // 对象实例
	in            io.Reader                          // 输入流
	uploadedParts []oss.UploadPart                   // 已上传的分片列表
	client        *oss.Client                        // OSS客户端
	callback      string                             // 115网盘回调URL
	callbackVar   string                             // 115网盘回调变量
	callbackRes   map[string]any                     // 回调结果
	imur          *oss.InitiateMultipartUploadResult // 分片上传初始化结果

	// 超时控制统计
	timeoutCount int        // 超时次数统计
	mu           sync.Mutex // 保护统计数据
}

// newChunkWriterWithClient 创建分片写入器，使用指定的优化OSS客户端
func (f *Fs) newChunkWriterWithClient(ctx context.Context, src fs.ObjectInfo, ui *UploadInitInfo, in io.Reader, o *Object, ossClient *oss.Client, options ...fs.OpenOption) (w *ossChunkWriter, err error) {
	uploadParts := min(max(1, f.opt.MaxUploadParts), maxUploadParts)
	size := src.Size()

	// 参考OpenList：使用简化的分片大小计算
	chunkSize := f.calculateOptimalChunkSize(size)

	// 处理未知文件大小的情况（流式上传）
	if size == -1 {
		warnStreamUpload.Do(func() {
			fs.Infof(f, "流式上传使用分片大小 %v，最大文件大小限制为 %v",
				chunkSize, fs.SizeSuffix(int64(chunkSize)*int64(uploadParts)))
		})
	} else {
		// 简化分片计算：直接使用OpenList风格的分片大小，减少复杂性
		// 保持OpenList的简单有效策略
		fs.Debugf(f, "115网盘分片上传: 使用OpenList风格分片大小 %v", chunkSize)
	}

	// 115网盘采用单线程分片上传模式，确保稳定性
	fs.Debugf(f, "115网盘分片上传: 文件大小 %v, 分片大小 %v", fs.SizeSuffix(size), fs.SizeSuffix(int64(chunkSize)))

	// Fix 10002 error: use internal two-step transfer for cross-cloud transfers
	// 🌊 但在StreamHashMode下应该已经在上层处理，这里不应该到达
	if f.isFromRemoteSource(in) && !f.opt.StreamHashMode {
		fs.Debugf(o, "Cross-cloud transfer detected, using internal two-step transfer to avoid 10002 error")
		fs.Debugf(o, "Step 1: Download to local, Step 2: Upload to 115 from local")

		// 执行内部两步传输
		return f.internalTwoStepTransfer(ctx, src, in, o, size, options...)
	}

	// Simplified: no resume support
	fs.Debugf(f, "Simplified mode: skip resume management")

	w = &ossChunkWriter{
		chunkSize: int64(chunkSize),
		size:      size,
		f:         f,
		o:         o,
		in:        in,
	}

	// 关键优化：使用传入的优化OSS客户端，而不是创建新的
	w.client = ossClient
	fs.Debugf(o, "Chunk upload using optimized OSS client")

	req := &oss.InitiateMultipartUploadRequest{
		Bucket: oss.Ptr(ui.GetBucket()),
		Key:    oss.Ptr(ui.GetObject()),
	}
	// 115网盘OSS使用Sequential模式，确保分片按顺序处理
	req.Parameters = map[string]string{
		"sequential": "",
	}
	// Apply upload options
	for _, option := range options {
		key, value := option.Header()
		lowerKey := strings.ToLower(key)
		switch lowerKey {
		case "":
			// ignore
		case "cache-control":
			req.CacheControl = oss.Ptr(value)
		case "content-disposition":
			req.ContentDisposition = oss.Ptr(value)
		case "content-encoding":
			req.ContentEncoding = oss.Ptr(value)
		case "content-type":
			req.ContentType = oss.Ptr(value)
		}
	}
	// 超激进优化：分片上传初始化也绕过QPS限制
	fs.Debugf(w.f, "Chunk upload initialization: bypassing QPS limits")
	// 直接调用，不使用调速器
	w.imur, err = w.client.InitiateMultipartUpload(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("create multipart upload failed: %w", err)
	}
	w.callback, w.callbackVar = ui.GetCallback(), ui.GetCallbackVar()
	fs.Debugf(w.o, "115 multipart upload initialized: %q", *w.imur.UploadId)
	return
}

// shouldRetry returns a boolean as to whether this err
// deserve to be retried. Uses rclone standard error handling with OSS-specific logic.
func (w *ossChunkWriter) shouldRetry(ctx context.Context, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	if fserrors.ShouldRetry(err) {
		return true, err
	}

	// 🔍 OSS错误详细分析和处理
	if opErr, ok := err.(*oss.OperationError); ok {
		fs.Debugf(w.o, "🔍 OSS操作错误: %v", opErr)

		// ✅ 检查是否是可重试的错误
		unwrappedErr := opErr.Unwrap()
		if fserrors.ShouldRetry(unwrappedErr) {
			return true, opErr
		}

		// 🛡️ OSS网络错误保守重试策略
		errStr := opErr.Error()
		if strings.Contains(errStr, "timeout") ||
			strings.Contains(errStr, "connection") ||
			strings.Contains(errStr, "network") ||
			strings.Contains(errStr, "503") ||
			strings.Contains(errStr, "502") ||
			strings.Contains(errStr, "500") {
			return true, opErr
		}

		// 🔄 其他错误使用默认逻辑
		err = unwrappedErr
	}

	if ossErr, ok := err.(*oss.ServiceError); ok {
		// 处理PartAlreadyExist错误：分片已存在，不需要重试
		if ossErr.Code == "PartAlreadyExist" {
			fs.Debugf(w.o, "分片已存在，跳过重试: %v", err)
			return false, nil // 不重试，视为成功
		}

		// Don't retry authentication errors
		if ossErr.StatusCode == 403 && (ossErr.Code == "InvalidAccessKeyId" || ossErr.Code == "SecurityTokenExpired") {
			return false, fserrors.FatalError(err)
		}
		// Retry server errors
		if ossErr.StatusCode >= 500 && ossErr.StatusCode < 600 {
			return true, err
		}
	}
	return false, err
}

// uploadChunkWithTimeout 带超时控制的分片上传
func (w *ossChunkWriter) uploadChunkWithTimeout(ctx context.Context, chunkNumber int32, reader io.ReadSeeker) (currentChunkSize int64, err error) {
	// 分片超时控制默认启用

	startTime := time.Now()

	// 创建带超时的context
	chunkCtx, cancel := context.WithTimeout(ctx, time.Duration(defaultChunkTimeout))
	defer cancel()

	// 使用channel来处理超时
	type result struct {
		size int64
		err  error
	}

	done := make(chan result, 1)

	// 在goroutine中执行上传
	go func() {
		size, err := w.uploadChunkWithRetry(chunkCtx, chunkNumber, reader)
		done <- result{size: size, err: err}
	}()

	// 等待完成或超时
	select {
	case res := <-done:
		elapsed := time.Since(startTime)
		if res.err == nil {
			fs.Debugf(w.o, "✅ 分片 %d 上传成功 (耗时: %v)", chunkNumber+1, elapsed.Truncate(time.Millisecond))
		}
		return res.size, res.err

	case <-chunkCtx.Done():
		elapsed := time.Since(startTime)

		// 更新超时统计
		w.mu.Lock()
		w.timeoutCount++
		timeoutCount := w.timeoutCount
		w.mu.Unlock()

		if chunkCtx.Err() == context.DeadlineExceeded {
			fs.Errorf(w.o, "⏰ 分片 %d 上传超时 (耗时: %v, 超时阈值: %v, 累计超时: %d次)",
				chunkNumber+1, elapsed.Truncate(time.Millisecond), defaultChunkTimeout, timeoutCount)

			// 如果超时次数过多，提供额外的诊断信息
			if timeoutCount >= 3 {
				fs.Logf(w.o, "⚠️ 检测到多次分片上传超时，可能存在网络问题或服务器响应慢")
			}

			return 0, fmt.Errorf("chunk %d upload timeout after %v (attempt %d)",
				chunkNumber+1, defaultChunkTimeout, timeoutCount)
		}
		return 0, chunkCtx.Err()
	}
}

// uploadChunkWithRetry uploads a chunk with retry logic
func (w *ossChunkWriter) uploadChunkWithRetry(ctx context.Context, chunkNumber int32, reader io.ReadSeeker) (currentChunkSize int64, err error) {
	// Use rclone's pacer for retry logic instead of custom implementation
	err = w.f.pacer.Call(func() (bool, error) {
		// Reset reader position
		if _, seekErr := reader.Seek(0, io.SeekStart); seekErr != nil {
			return false, fmt.Errorf("failed to reset reader: %w", seekErr)
		}

		// Upload chunk
		currentChunkSize, err = w.WriteChunk(ctx, chunkNumber, reader)
		if err != nil {
			return w.shouldRetry(ctx, err)
		}
		return false, nil
	})

	return currentChunkSize, err
}

// getTimeoutStats 获取超时统计信息
func (w *ossChunkWriter) getTimeoutStats() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.timeoutCount
}

// addCompletedPart 添加已完成的分片到列表
// 单线程模式下按顺序添加，无需复杂的并发控制
func (w *ossChunkWriter) addCompletedPart(part oss.UploadPart) {
	w.uploadedParts = append(w.uploadedParts, part)
}

// WriteChunk uploads a chunk to OSS
func (w *ossChunkWriter) WriteChunk(ctx context.Context, chunkNumber int32, reader io.ReadSeeker) (currentChunkSize int64, err error) {
	if chunkNumber < 0 {
		return -1, fmt.Errorf("invalid chunk number: %v", chunkNumber)
	}

	ossPartNumber := chunkNumber + 1

	// Get chunk size
	currentChunkSize, err = reader.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("failed to get chunk %d size: %w", ossPartNumber, err)
	}

	// Reset reader position
	_, err = reader.Seek(0, io.SeekStart)
	if err != nil {
		return 0, fmt.Errorf("failed to reset chunk %d reader position: %w", ossPartNumber, err)
	}

	fs.Debugf(w.o, "Uploading chunk %d to OSS: size=%v", ossPartNumber, fs.SizeSuffix(currentChunkSize))

	// 📤 OSS分片上传
	fs.Debugf(w.o, "📤 上传分片 %d (大小: %v)", ossPartNumber, fs.SizeSuffix(currentChunkSize))

	res, err := w.client.UploadPart(ctx, &oss.UploadPartRequest{
		Bucket:     oss.Ptr(*w.imur.Bucket),
		Key:        oss.Ptr(*w.imur.Key),
		UploadId:   w.imur.UploadId,
		PartNumber: ossPartNumber,
		Body:       reader,
	})

	if err != nil {
		fs.Errorf(w.o, "❌ OSS分片上传失败: 分片=%d, 大小=%v, 错误=%v",
			ossPartNumber, fs.SizeSuffix(currentChunkSize), err)
		return 0, fmt.Errorf("❌ 分片 %d 上传失败 (大小: %v): %w", ossPartNumber, fs.SizeSuffix(currentChunkSize), err)
	}

	// Record completed part
	part := oss.UploadPart{
		PartNumber: ossPartNumber,
		ETag:       res.ETag,
	}
	w.addCompletedPart(part)

	fs.Debugf(w.o, "Chunk %d upload completed: size=%v, ETag=%s", ossPartNumber, fs.SizeSuffix(currentChunkSize), *res.ETag)
	return currentChunkSize, nil
}

// Abort the multipart upload
func (w *ossChunkWriter) Abort(ctx context.Context) (err error) {
	// 使用统一调速器，优化分片上传中止API调用频率
	err = w.f.pacer.Call(func() (bool, error) {
		_, err = w.client.AbortMultipartUpload(ctx, &oss.AbortMultipartUploadRequest{
			Bucket:   oss.Ptr(*w.imur.Bucket),
			Key:      oss.Ptr(*w.imur.Key),
			UploadId: w.imur.UploadId,
		})
		return w.shouldRetry(ctx, err)
	})
	if err != nil {
		return fmt.Errorf("failed to abort multipart upload %q: %w", *w.imur.UploadId, err)
	}
	// w.shutdownRenew()
	fs.Debugf(w.o, "multipart upload: %q aborted", *w.imur.UploadId)
	return
}

// Close 完成并确认分片上传
func (w *ossChunkWriter) Close(ctx context.Context) (err error) {
	// 单线程模式下分片已按顺序添加，但为保险起见仍进行排序
	sort.Slice(w.uploadedParts, func(i, j int) bool {
		return w.uploadedParts[i].PartNumber < w.uploadedParts[j].PartNumber
	})

	fs.Infof(w.o, "准备完成分片上传: 共 %d 个分片", len(w.uploadedParts))

	// 完成分片上传
	var res *oss.CompleteMultipartUploadResult
	req := &oss.CompleteMultipartUploadRequest{
		Bucket:   oss.Ptr(*w.imur.Bucket),
		Key:      oss.Ptr(*w.imur.Key),
		UploadId: w.imur.UploadId,
		CompleteMultipartUpload: &oss.CompleteMultipartUpload{
			Parts: w.uploadedParts,
		},
		Callback:    oss.Ptr(w.callback),
		CallbackVar: oss.Ptr(w.callbackVar),
	}

	// 🎯 OSS分片上传完成：使用完整重试机制确保稳定性
	fs.Debugf(w.o, "🎯 OSS分片上传完成中")

	err = w.f.pacer.Call(func() (bool, error) {
		var completeErr error
		res, completeErr = w.client.CompleteMultipartUpload(ctx, req)

		if completeErr != nil {
			retry, retryErr := w.shouldRetry(ctx, completeErr)
			if retry {
				fs.Debugf(w.o, "🔄 OSS分片上传完成重试: %v", completeErr)
				return true, retryErr
			}
			return false, completeErr
		}
		return false, nil
	})

	if err != nil {
		return fmt.Errorf("❌ OSS分片上传完成失败: %w", err)
	}

	w.callbackRes = res.CallbackResult
	fs.Infof(w.o, "✅ 115网盘上传完成")
	return
}

// getDownloadURLCommand 通过pick_code或.strm内容获取下载URL
func (f *Fs) getDownloadURLCommand(ctx context.Context, args []string, opt map[string]string) (any, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("pick_code, 115://pick_code format, or file path required")
	}

	input := args[0]
	fs.Debugf(f, "Processing download URL request: %s", input)

	// Parse input format
	if strings.HasPrefix(input, "/") {
		// File path format, use getDownloadURLByUA method (supports 302 redirect)
		fs.Debugf(f, "Using file path with UA method: %s", input)

		userAgent := opt["user-agent"]
		if userAgent == "" {
			userAgent = defaultUserAgent
		}

		fs.Debugf(f, "Getting download URL with UA: path=%s, UA=%s", input, userAgent)
		downloadURL, err := f.getDownloadURLByUA(ctx, input, userAgent)
		if err != nil {
			return nil, fmt.Errorf("failed to get download URL with UA: %w", err)
		}

		fs.Debugf(f, "Successfully got 115 playable URL: path=%s", input)
		return downloadURL, nil
	}

	// pick_code 格式，使用原始HTTP实现
	var pickCode string
	if code, found := strings.CutPrefix(input, "115://"); found {
		// 115://pick_code 格式 (来自.strm文件)
		pickCode = code
		fs.Debugf(f, "Parsed .strm format: pick_code=%s", pickCode)
	} else {
		// Assume pure pick_code
		pickCode = input
		fs.Debugf(f, "Using pure pick_code: %s", pickCode)
	}

	// Validate pick_code format
	if pickCode == "" {
		return nil, fmt.Errorf("invalid pick_code: %s", input)
	}

	// 使用原始HTTP实现获取下载URL
	fs.Debugf(f, "Using raw HTTP method to get download URL: pick_code=%s", pickCode)
	downloadURL, err := f.getDownloadURLByPickCodeHTTP(ctx, pickCode, opt["user-agent"])
	if err != nil {
		return nil, fmt.Errorf("failed to get download URL: %w", err)
	}

	fs.Infof(f, "Successfully obtained 115 download URL: pick_code=%s", pickCode)

	// 返回下载URL字符串（与getdownloadurlua保持一致）
	return downloadURL, nil
}

// refreshCacheCommand 刷新目录缓存
func (f *Fs) refreshCacheCommand(ctx context.Context, args []string) (any, error) {
	fs.Infof(f, "🔄 开始刷新缓存...")

	// 清除持久化dirCache
	if err := f.dirCache.ForceRefreshPersistent(); err != nil {
		fs.Errorf(f, "❌ 清除持久化缓存失败: %v", err)
	} else {
		fs.Debugf(f, "✅ 已清除持久化dirCache")
	}

	// 重置dirCache
	f.dirCache.Flush()
	fs.Debugf(f, "✅ 已重置内存dirCache")

	// 清理listAll缓存
	if f.listAllCache != nil {
		f.listAllCache.Clear()
		fs.Debugf(f, "✅ 已清理listAll内存缓存")
	}

	// 如果指定了路径，尝试重新构建该路径的缓存
	if len(args) > 0 && args[0] != "" {
		targetPath := args[0]
		fs.Debugf(f, "🔄 重新构建路径缓存: %s", targetPath)

		// 尝试查找目录以重新构建缓存
		if _, err := f.dirCache.FindDir(ctx, targetPath, false); err != nil {
			fs.Errorf(f, "❌ 重新构建路径缓存失败: %v", err)
		} else {
			fs.Debugf(f, "✅ 路径缓存重新构建成功: %s", targetPath)
		}
	}

	return map[string]any{
		"status":  "success",
		"message": "缓存刷新完成",
	}, nil
}

// getDownloadURLByPickCodeHTTP 使用原生HTTP请求通过pick_code获取下载URL
func (f *Fs) getDownloadURLByPickCodeHTTP(ctx context.Context, pickCode string, userAgent string) (string, error) {
	// 如果没有提供 UA，使用默认值
	if userAgent == "" {
		userAgent = defaultUserAgent
	}

	fs.Debugf(f, "Using native HTTP method to get download URL: pick_code=%s, UA=%s", pickCode, userAgent)

	// 创建原生HTTP客户端
	client := &http.Client{}
	req, err := http.NewRequest("POST", openAPIRootURL+"/open/ufile/downurl", strings.NewReader("pick_code="+pickCode))
	if err != nil {
		fs.Errorf(nil, "创建请求失败: %v", err)
		return "", err
	}

	// 准备认证信息
	opts := rest.Opts{}
	f.prepareTokenForRequest(ctx, &opts)

	// 设置请求头
	req.Header.Set("Authorization", opts.ExtraHeaders["Authorization"])
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	fs.Infof(nil, "Authorization: %s, User-Agent: %s", opts.ExtraHeaders["Authorization"], userAgent)

	// 发送请求并处理响应
	res, err := client.Do(req)
	if err != nil {
		fs.Logf(nil, "请求失败: %v", err)
		return "", err
	}
	defer res.Body.Close()

	// 解析响应
	var response ApiResponse
	if decodeErr := json.NewDecoder(res.Body).Decode(&response); decodeErr != nil {
		fs.Logf(nil, "解析响应失败: %v", decodeErr)
		return "", decodeErr
	}

	for _, downInfo := range response.Data {
		if downInfo.URL.URL != "" {
			fs.Infof(nil, "获取到下载URL: %s", downInfo.URL.URL)
			return downInfo.URL.URL, nil
		}
	}
	return "", fmt.Errorf("download URL not found")
}

// internalTwoStepTransfer 内部两步传输：先完整下载到本地，再从本地上传
// 修复10002错误：彻底解决跨云传输SHA1不一致问题
func (f *Fs) internalTwoStepTransfer(ctx context.Context, src fs.ObjectInfo, in io.Reader, o *Object, size int64, options ...fs.OpenOption) (*ossChunkWriter, error) {
	fs.Infof(o, "Starting internal two-step transfer: %s (%s)", src.Remote(), fs.SizeSuffix(size))

	// Step 1: Complete download to local temp file
	fs.Infof(o, "Step 1: Downloading to local temp file...")

	// 创建临时文件
	tempFile, err := os.CreateTemp("", "rclone-115-twostep-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	fs.Debugf(o, "💾 创建临时文件: %s | 原因: 内部两步传输Step1下载缓存 | 目标大小: %s", tempPath, fs.SizeSuffix(size))

	// 确保清理临时文件
	defer func() {
		if tempFile != nil {
			if closeErr := tempFile.Close(); closeErr != nil {
				fs.Debugf(o, "⚠️ 关闭临时文件失败: %v", closeErr)
			} else {
				fs.Debugf(o, "📁 关闭临时文件成功: %s", tempPath)
			}
		}
		if removeErr := os.Remove(tempPath); removeErr != nil {
			fs.Debugf(o, "⚠️ 删除临时文件失败: %v", removeErr)
		} else {
			fs.Debugf(o, "🧹 删除临时文件成功: %s", tempPath)
		}
	}()

	// 使用传入的Reader进行完整下载，并显示下载进度
	// 注意：in已经是打开的Reader，直接使用
	fs.Infof(o, "Downloading: %s -> %s", src.Remote(), tempPath)

	// 🔧 修复进度显示问题：直接使用原始Reader，避免重复进度跟踪
	// 不使用TwoStepProgressReader115，让rclone的accounting系统处理进度
	// 内部两步传输的进度由主传输进度显示，不需要额外的进度跟踪
	fs.Debugf(o, "📥 Step 1: 开始下载到临时文件")
	fs.Debugf(o, "✍️ 开始写入临时文件: %s", tempPath)

	written, err := io.Copy(tempFile, in)
	if err != nil {
		fs.Debugf(o, "⚠️ 写入临时文件失败: %s", tempPath)
		return nil, fmt.Errorf("failed to download to temp file: %w", err)
	}
	fs.Debugf(o, "✅ 临时文件写入完成: %s | 写入字节: %s", tempPath, fs.SizeSuffix(written))

	// Verify download size
	if written != size {
		return nil, fmt.Errorf("download size mismatch: expected %d, got %d", size, written)
	}

	fs.Infof(o, "Step 1 completed: downloaded %s", fs.SizeSuffix(written))

	// 重置文件指针到开头
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to reset file pointer: %w", err)
	}

	// 第二步：计算本地文件的SHA1
	fs.Infof(o, "Calculating local file SHA1...")
	hasher := sha1.New()
	_, err = io.Copy(hasher, tempFile)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate SHA1: %w", err)
	}
	sha1sum := fmt.Sprintf("%x", hasher.Sum(nil))
	fs.Debugf(o, "Local file SHA1: %s", sha1sum)

	// Set SHA1 to Object
	if o != nil {
		o.sha1sum = sha1sum
	}

	// Reset file pointer to beginning for upload
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to reset file pointer: %w", err)
	}

	// Step 3: Upload from local file to 115
	fs.Debugf(o, "Step 2: Upload from local to 115...")

	// 重新初始化上传（使用正确的SHA1）
	newUI, err := f.initUploadOpenAPI(ctx, size, o.remote, "", sha1sum, "", "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to reinitialize upload: %w", err)
	}

	// Check if instant upload succeeded
	if newUI.GetStatus() == 2 {
		fs.Debugf(o, "Two-step transfer instant upload successful!")
		// Return special error to indicate instant upload success
		// Due to function signature limitations, we need to modify the calling method
		return nil, fmt.Errorf("INSTANT_UPLOAD_SUCCESS:%s", newUI.GetFileID())
	}

	// 创建OSS客户端
	ossClient, err := f.newOSSClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create OSS client: %w", err)
	}

	// 使用本地文件创建标准的分片写入器
	return f.newChunkWriterWithClient(ctx, src, newUI, tempFile, o, ossClient, options...)
}

// smartStreamHashTransfer115 115网盘智能流式哈希传输
// 根据文件大小选择最优策略：小文件内存缓存，大文件流式哈希计算
func (f *Fs) smartStreamHashTransfer115(ctx context.Context, src fs.ObjectInfo, remote string, options ...fs.OpenOption) (fs.Object, error) {
	fileSize := src.Size()

	// 🌊 StreamHashMode下使用更保守的内存策略
	// 小文件（≤10MB）：内存缓存 + 单步上传API
	// 大文件（>10MB）：流式哈希 + 分片上传API
	memoryThreshold := int64(10 * 1024 * 1024) // 10MB，更安全的内存使用

	if fileSize <= memoryThreshold {
		fs.Infof(f, "📝 小文件使用内存缓存模式: %s", fs.SizeSuffix(fileSize))
		return f.memoryHashTransfer115(ctx, src, remote, options...)
	} else {
		fs.Infof(f, "🌊 大文件使用流式哈希模式: %s", fs.SizeSuffix(fileSize))
		return f.streamHashTransfer115(ctx, src, remote, options...)
	}
}

// memoryHashTransfer115 115网盘小文件内存缓存哈希传输
func (f *Fs) memoryHashTransfer115(ctx context.Context, src fs.ObjectInfo, remote string, options ...fs.OpenOption) (fs.Object, error) {
	// 转换为fs.Object以便调用Open方法
	srcObj, ok := src.(fs.Object)
	if !ok {
		return nil, fmt.Errorf("source is not a valid fs.Object")
	}

	// 打开源文件
	srcReader, err := srcObj.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open source file: %w", err)
	}
	defer func() {
		if srcReader != nil {
			if closeErr := srcReader.Close(); closeErr != nil {
				fs.Debugf(f, "⚠️ 关闭源文件失败: %v", closeErr)
			}
		}
	}()

	// 读取整个文件到内存并计算SHA1
	data, err := io.ReadAll(srcReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read file data: %w", err)
	}

	// 计算SHA1哈希（115网盘需要SHA1）
	hasher := sha1.New()
	hasher.Write(data)
	sha1Hash := fmt.Sprintf("%X", hasher.Sum(nil)) // 115网盘需要大写SHA1

	fs.Infof(f, "📊 内存哈希计算完成: %s, SHA1: %s", fs.SizeSuffix(int64(len(data))), sha1Hash)

	// 尝试秒传
	leaf, dirID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}

	// 创建对象用于上传
	o := &Object{
		fs:      f,
		remote:  remote,
		size:    int64(len(data)),
		sha1sum: sha1Hash,
	}

	// 尝试哈希上传（秒传）
	gotIt, _, _, cleanup, err := f.tryHashUpload(ctx, bytes.NewReader(data), src, o, leaf, dirID, int64(len(data)), options...)
	if cleanup != nil {
		defer cleanup()
	}

	if err == nil && gotIt {
		fs.Infof(f, "🚀 内存哈希计算后秒传成功！")
		return o, nil
	}

	// 秒传失败，使用内存数据直接上传
	fs.Infof(f, "⬆️ 秒传失败，使用内存数据上传")
	return f.uploadFromMemory115(ctx, data, o, options...)
}

// streamHashTransfer115 115网盘大文件流式哈希传输
func (f *Fs) streamHashTransfer115(ctx context.Context, src fs.ObjectInfo, remote string, options ...fs.OpenOption) (fs.Object, error) {
	// 转换为fs.Object以便调用Open方法
	srcObj, ok := src.(fs.Object)
	if !ok {
		return nil, fmt.Errorf("source is not a valid fs.Object")
	}

	// 第一遍：流式计算SHA1哈希（为了秒传）
	fs.Infof(f, "🔄 第一遍：流式计算文件哈希用于秒传...")

	srcReader, err := srcObj.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open source file for hash calculation: %w", err)
	}
	defer srcReader.Close()

	// 流式计算SHA1，不保存数据
	hasher := sha1.New()
	buffer := make([]byte, 1024*1024) // 🔧 简单修复：增大缓冲区到1MB，提高rclone进度更新频率

	totalRead := int64(0)
	for {
		n, err := srcReader.Read(buffer)
		if n > 0 {
			hasher.Write(buffer[:n])
			totalRead += int64(n)

			// 保留原有的详细进度日志，每1MB输出一次
			if totalRead%(1024*1024) == 0 || err == io.EOF {
				percentage := float64(totalRead) / float64(src.Size()) * 100
				fs.Debugf(f, "📊 流式哈希计算进度: %s/%s (%.1f%%)",
					fs.SizeSuffix(totalRead), fs.SizeSuffix(src.Size()), percentage)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read data for hash calculation: %w", err)
		}
	}

	sha1Hash := fmt.Sprintf("%X", hasher.Sum(nil)) // 115网盘需要大写SHA1
	fs.Infof(f, "📊 流式哈希计算完成: SHA1: %s", sha1Hash)

	// 尝试秒传
	leaf, dirID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}

	// 创建对象用于上传
	o := &Object{
		fs:      f,
		remote:  remote,
		size:    src.Size(),
		sha1sum: sha1Hash,
	}

	// 尝试秒传：直接使用已计算的SHA1哈希调用initUploadOpenAPI
	fs.Infof(f, "🔍 尝试使用已计算SHA1进行秒传...")
	ui, err := f.initUploadOpenAPI(ctx, src.Size(), leaf, dirID, sha1Hash, "", "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to init upload for instant upload: %w", err)
	}

	// 检查秒传状态
	if ui.GetStatus() == 2 {
		fs.Infof(f, "🚀 流式哈希计算后秒传成功！")
		o.id = ui.GetFileID()
		o.pickCode = ui.GetPickCode()
		o.hasMetaData = true
		return o, nil
	}

	// 处理需要二次验证的情况（状态7）
	if ui.GetStatus() == 7 {
		fs.Infof(f, "🔐 需要二次验证，计算签名...")
		signKey := ui.GetSignKey()
		signCheckRange := ui.GetSignCheck()

		// 计算指定范围的SHA1
		signVal, err := f.calculateRangeHashFromSource(ctx, srcObj, signCheckRange)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate range hash for verification: %w", err)
		}

		// 重新提交带签名的请求
		ui, err = f.initUploadOpenAPI(ctx, src.Size(), leaf, dirID, sha1Hash, "", signKey, signVal)
		if err != nil {
			return nil, fmt.Errorf("failed to retry upload with signature: %w", err)
		}

		// 再次检查秒传状态
		if ui.GetStatus() == 2 {
			fs.Infof(f, "🚀 二次验证后秒传成功！")
			o.id = ui.GetFileID()
			o.pickCode = ui.GetPickCode()
			o.hasMetaData = true
			return o, nil
		}
	}

	// 秒传失败，第二遍：重新下载并边下边传
	fs.Infof(f, "⬆️ 秒传失败，第二遍：边下边传上传")
	return f.streamUploadWithHash115(ctx, srcObj, o, nil, options...)
}

// streamHashTransfer115WithReader 115网盘大文件流式哈希传输（使用已包装的Reader）
// 🔧 修复进度显示问题：使用rclone传递的已被Transfer包装的Reader + TwoStepProgressReader
func (f *Fs) streamHashTransfer115WithReader(ctx context.Context, in io.Reader, src fs.ObjectInfo, remote string, options ...fs.OpenOption) (fs.Object, error) {
	// 🔧 创建跨云传输统一进度跟踪器
	var twoStepProgress *TwoStepProgressReader115
	var srcReader io.Reader = in

	// 检查是否为跨云传输（输入是ReadCloser）
	if rc, ok := in.(io.ReadCloser); ok {
		// 获取Account对象用于进度跟踪
		_, account := accounting.UnWrapAccounting(in)
		twoStepProgress = NewTwoStepProgressReader115(rc, f, remote, src.Size(), account)
		srcReader = twoStepProgress
		fs.Infof(f, "🌐 启用跨云传输统一进度显示: %s", remote)
		fs.Debugf(f, "🚀 流式哈希模式: 跳过临时文件创建，直接使用内存流式处理 | 文件: %s | 大小: %s", remote, fs.SizeSuffix(src.Size()))
	} else {
		fs.Debugf(f, "🚀 流式哈希模式: 本地文件上传，跳过临时文件创建 | 文件: %s | 大小: %s", remote, fs.SizeSuffix(src.Size()))
	}

	// 第一遍：流式计算SHA1哈希（为了秒传）
	fs.Infof(f, "🔄 第一遍：流式计算文件哈希用于秒传...")

	// 流式计算SHA1，不保存数据
	hasher := sha1.New()
	buffer := make([]byte, 1024*1024) // 1MB缓冲区，提高进度更新频率

	totalRead := int64(0)
	for {
		n, err := srcReader.Read(buffer)
		if n > 0 {
			hasher.Write(buffer[:n])
			totalRead += int64(n)

			// 保留原有的详细进度日志，每1MB输出一次
			if totalRead%(1024*1024) == 0 || err == io.EOF {
				percentage := float64(totalRead) / float64(src.Size()) * 100
				fs.Debugf(f, "📊 流式哈希计算进度: %s/%s (%.1f%%)",
					fs.SizeSuffix(totalRead), fs.SizeSuffix(src.Size()), percentage)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read data for hash calculation: %w", err)
		}
	}

	sha1Hash := fmt.Sprintf("%X", hasher.Sum(nil)) // 115网盘需要大写SHA1
	fs.Infof(f, "📊 流式哈希计算完成: SHA1: %s", sha1Hash)

	// 尝试秒传
	leaf, dirID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}

	// 创建对象用于上传
	o := &Object{
		fs:      f,
		remote:  remote,
		size:    src.Size(),
		sha1sum: sha1Hash,
	}

	// 尝试秒传：直接使用已计算的SHA1哈希调用initUploadOpenAPI
	fs.Infof(f, "🔍 尝试使用已计算SHA1进行秒传...")
	ui, err := f.initUploadOpenAPI(ctx, src.Size(), leaf, dirID, sha1Hash, "", "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to init upload for instant upload: %w", err)
	}

	// 检查秒传状态
	if ui.GetStatus() == 2 {
		fs.Infof(f, "🚀 流式哈希计算后秒传成功！")
		o.id = ui.GetFileID()
		o.pickCode = ui.GetPickCode()
		o.hasMetaData = true
		return o, nil
	}

	// 处理需要二次验证的情况（状态7）
	if ui.GetStatus() == 7 {
		fs.Infof(f, "🔐 需要二次验证，计算签名...")
		signKey := ui.GetSignKey()
		signCheckRange := ui.GetSignCheck()

		// 注意：此时Reader已经被读完，需要重新打开源文件进行Range计算
		srcObj, ok := src.(fs.Object)
		if !ok {
			return nil, fmt.Errorf("source is not a valid fs.Object")
		}

		// 计算指定范围的SHA1
		signVal, err := f.calculateRangeHashFromSource(ctx, srcObj, signCheckRange)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate range hash for verification: %w", err)
		}

		// 重新提交带签名的请求
		ui, err = f.initUploadOpenAPI(ctx, src.Size(), leaf, dirID, sha1Hash, "", signKey, signVal)
		if err != nil {
			return nil, fmt.Errorf("failed to retry upload with signature: %w", err)
		}

		// 再次检查秒传状态
		if ui.GetStatus() == 2 {
			fs.Infof(f, "🚀 二次验证后秒传成功！")
			o.id = ui.GetFileID()
			o.pickCode = ui.GetPickCode()
			o.hasMetaData = true
			return o, nil
		}
	}

	// 秒传失败，需要重新下载并边下边传
	// 注意：此时Reader已经被读完，需要重新打开源文件
	fs.Infof(f, "⬆️ 秒传失败，第二遍：边下边传上传")

	// 🔧 如果使用了TwoStepProgressReader，切换到上传阶段
	if twoStepProgress != nil {
		twoStepProgress.SwitchToUpload()
	}

	// 转换为fs.Object以便重新打开
	srcObj, ok := src.(fs.Object)
	if !ok {
		return nil, fmt.Errorf("source is not a valid fs.Object")
	}

	return f.streamUploadWithHash115(ctx, srcObj, o, twoStepProgress, options...)
}

// uploadFromMemory115 115网盘从内存数据上传文件
func (f *Fs) uploadFromMemory115(ctx context.Context, data []byte, o *Object, options ...fs.OpenOption) (fs.Object, error) {
	// 使用现有的上传逻辑，传入内存数据
	return f.upload(ctx, bytes.NewReader(data), o, o.remote, options...)
}

// streamUploadWithHash115 115网盘使用已计算哈希进行流式上传
func (f *Fs) streamUploadWithHash115(ctx context.Context, srcObj fs.Object, o *Object, twoStepProgress *TwoStepProgressReader115, options ...fs.OpenOption) (fs.Object, error) {
	// 重新打开源文件进行上传
	srcReader, err := srcObj.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to reopen source file for upload: %w", err)
	}
	defer srcReader.Close()

	var uploadReader io.Reader = srcReader

	// 🔧 如果有TwoStepProgressReader，包装上传流以跟踪上传进度
	if twoStepProgress != nil {
		// 创建一个进度跟踪包装器
		uploadReader = &uploadProgressWrapper{
			Reader:          srcReader,
			twoStepProgress: twoStepProgress,
		}
	}

	// 使用现有的上传逻辑
	return f.upload(ctx, uploadReader, o, o.remote, options...)
}

// uploadProgressWrapper 上传进度包装器
type uploadProgressWrapper struct {
	io.Reader
	twoStepProgress *TwoStepProgressReader115
	uploadedBytes   int64
}

func (upw *uploadProgressWrapper) Read(p []byte) (n int, err error) {
	n, err = upw.Reader.Read(p)
	if n > 0 {
		upw.uploadedBytes += int64(n)
		if upw.twoStepProgress != nil {
			upw.twoStepProgress.UpdateUploadProgress(upw.uploadedBytes)
		}
	}
	return n, err
}

// calculateRangeHashFromSource 从源对象计算指定范围的SHA1哈希
// 修复：使用Range请求只下载指定范围，避免完整下载
func (f *Fs) calculateRangeHashFromSource(ctx context.Context, srcObj fs.Object, rangeSpec string) (string, error) {
	var start, end int64
	// OpenAPI range is "start-end" (inclusive)
	if n, err := fmt.Sscanf(rangeSpec, "%d-%d", &start, &end); err != nil || n != 2 {
		return "", fmt.Errorf("invalid range spec format %q: %w", rangeSpec, err)
	}
	if start < 0 || end < start {
		return "", fmt.Errorf("invalid range spec values %q", rangeSpec)
	}
	length := end - start + 1

	fs.Debugf(f, "🔐 计算二次验证范围SHA1: 范围=%s, 字节=%d-%d, 长度=%s",
		rangeSpec, start, end, fs.SizeSuffix(length))

	// 关键修复：使用Range请求只下载指定范围
	rangeOption := &fs.RangeOption{Start: start, End: end}
	srcReader, err := srcObj.Open(ctx, rangeOption)
	if err != nil {
		return "", fmt.Errorf("failed to open source file with range %s: %w", rangeSpec, err)
	}
	defer srcReader.Close()

	// 直接读取Range请求返回的数据
	hasher := sha1.New()
	bytesRead, err := io.Copy(hasher, srcReader)
	if err != nil {
		return "", fmt.Errorf("failed to read range data for hash: %w", err)
	}

	fs.Debugf(f, "✅ 二次验证范围SHA1计算完成: 实际读取=%s, 预期长度=%s",
		fs.SizeSuffix(bytesRead), fs.SizeSuffix(length))

	if bytesRead != length {
		fs.Debugf(f, "⚠️ 读取长度不匹配，但继续计算SHA1: 实际=%d, 预期=%d", bytesRead, length)
	}

	sha1Hash := fmt.Sprintf("%X", hasher.Sum(nil))
	fs.Debugf(f, "🔐 二次验证SHA1: %s (范围: %s)", sha1Hash, rangeSpec)

	return sha1Hash, nil
}

// 💾 115网盘持久化缓存管理方法

// initPersistentCache115 初始化115网盘持久化缓存系统
func (f *Fs) initPersistentCache115() {
	// 获取缓存目录
	cacheDir := config.GetCacheDir()
	f.persistentCacheDir = filepath.Join(cacheDir, "115-cache", f.name)

	// 创建缓存目录
	if err := os.MkdirAll(f.persistentCacheDir, 0755); err != nil {
		fs.Debugf(f, "⚠️ 创建115持久化缓存目录失败: %v", err)
		return
	}

	// 设置缓存文件路径
	f.downloadURLCacheFile = filepath.Join(f.persistentCacheDir, "download_url_cache.json")
	f.listAllCacheFile = filepath.Join(f.persistentCacheDir, "list_all_cache.json")

	fs.Debugf(f, "💾 115持久化缓存目录: %s", f.persistentCacheDir)
	fs.Debugf(f, "💾 listAll缓存文件: %s", f.listAllCacheFile)
}

// loadPersistentCaches115 加载115网盘持久化缓存
func (f *Fs) loadPersistentCaches115() {
	// 加载下载URL缓存
	f.loadDownloadURLCache()
	// 加载listAll缓存
	f.loadListAllCache()
}

// loadDownloadURLCache 加载下载URL缓存
func (f *Fs) loadDownloadURLCache() {
	if f.downloadURLCacheFile == "" {
		return
	}

	data, err := os.ReadFile(f.downloadURLCacheFile)
	if err != nil {
		if !os.IsNotExist(err) {
			fs.Debugf(f, "⚠️ 读取下载URL缓存失败: %v", err)
		}
		return
	}

	var cache PersistentDownloadURLCache
	if err := json.Unmarshal(data, &cache); err != nil {
		fs.Debugf(f, "⚠️ 解析下载URL缓存失败: %v", err)
		return
	}

	// 检查缓存是否过期（24小时）
	if time.Since(cache.SavedAt) > 24*time.Hour {
		fs.Debugf(f, "🔄 下载URL缓存已过期，跳过加载")
		return
	}

	// 过滤过期的条目（1小时）
	validCount := 0
	for pickCode, cachedURL := range cache.Data {
		if cachedURL.Valid() && !cachedURL.NearExpiry() {
			f.downloadURLCache.Store(pickCode, cachedURL)
			validCount++
		}
	}

	fs.Debugf(f, "📥 从持久化缓存加载 %d 个有效下载URL条目", validCount)

	// 🧹 加载后清理可能的过期缓存
	go f.cleanExpiredURLCache()
}

// saveDownloadURLCache 保存下载URL缓存到磁盘
func (f *Fs) saveDownloadURLCache() {
	if f.downloadURLCacheFile == "" {
		return
	}

	// 从sync.Map中提取数据
	data := make(map[string]CachedDownloadURL)
	f.downloadURLCache.Range(func(key, value interface{}) bool {
		if keyStr, ok := key.(string); ok {
			if cachedURL, ok := value.(CachedDownloadURL); ok {
				// 只保存有效且未即将过期的URL
				if cachedURL.Valid() && !cachedURL.NearExpiry() {
					data[keyStr] = cachedURL
				}
			}
		}
		return true
	})

	cache := PersistentDownloadURLCache{
		Data:    data,
		SavedAt: time.Now(),
		Version: "1.0",
	}

	jsonData, err := json.Marshal(cache)
	if err != nil {
		fs.Debugf(f, "⚠️ 序列化下载URL缓存失败: %v", err)
		return
	}

	if err := os.WriteFile(f.downloadURLCacheFile, jsonData, 0644); err != nil {
		fs.Debugf(f, "⚠️ 保存下载URL缓存失败: %v", err)
		return
	}

	fs.Debugf(f, "💾 下载URL缓存已保存: %d 个条目", len(data))
}

// saveListAllCacheEntry 保存单个listAll缓存条目
func (f *Fs) saveListAllCacheEntry(key string, files []*File) {
	if f.listAllCacheFile == "" {
		return
	}

	// 读取现有缓存
	var cache PersistentListAllCache
	if data, err := os.ReadFile(f.listAllCacheFile); err == nil {
		json.Unmarshal(data, &cache)
	}

	// 初始化数据结构
	if cache.Data == nil {
		cache.Data = make(map[string]*CachedListAllResponse)
	}

	// 添加新条目
	cache.Data[key] = &CachedListAllResponse{
		Files:     files,
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
		os.WriteFile(f.listAllCacheFile, jsonData, 0644)
		fs.Debugf(f, "💾 listAll缓存条目已保存: %s", key)
	}
}

// loadListAllCache 加载listAll缓存
func (f *Fs) loadListAllCache() {
	if f.listAllCacheFile == "" {
		return
	}

	data, err := os.ReadFile(f.listAllCacheFile)
	if err != nil {
		if !os.IsNotExist(err) {
			fs.Debugf(f, "⚠️ 读取listAll缓存失败: %v", err)
		}
		return
	}

	var cache PersistentListAllCache
	if err := json.Unmarshal(data, &cache); err != nil {
		fs.Debugf(f, "⚠️ 解析listAll缓存失败: %v", err)
		return
	}

	// 检查缓存是否过期（24小时）
	if time.Since(cache.SavedAt) > 24*time.Hour {
		fs.Debugf(f, "🔄 listAll缓存已过期，跳过加载")
		return
	}

	// 过滤过期的条目（5分钟）
	validCount := 0
	for key, cachedResp := range cache.Data {
		if time.Since(cachedResp.CachedAt) <= 5*time.Minute {
			f.listAllCache.Put(key, cachedResp.Files)
			validCount++
		}
	}

	fs.Debugf(f, "📋 从持久化缓存加载 %d 个有效listAll条目", validCount)
}

// 🧹 115网盘缓存管理方法

// clearPersistentCache115 清理115网盘持久化缓存
func (f *Fs) clearPersistentCache115() error {
	if f.persistentCacheDir == "" {
		return nil
	}

	// 删除整个缓存目录
	if err := os.RemoveAll(f.persistentCacheDir); err != nil {
		fs.Debugf(f, "⚠️ 清理115持久化缓存失败: %v", err)
		return err
	}

	// 重新创建缓存目录
	if err := os.MkdirAll(f.persistentCacheDir, 0755); err != nil {
		fs.Debugf(f, "⚠️ 重新创建115缓存目录失败: %v", err)
		return err
	}

	fs.Debugf(f, "🧹 115持久化缓存已清理: %s", f.persistentCacheDir)
	return nil
}

// getCacheStats115 获取115网盘缓存统计信息
func (f *Fs) getCacheStats115() map[string]interface{} {
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

	// 检查下载URL缓存文件
	if info, err := os.Stat(f.downloadURLCacheFile); err == nil {
		stats["download_url_cache_size"] = info.Size()
		stats["download_url_cache_modified"] = info.ModTime()
	}

	// 检查listAll缓存文件
	if info, err := os.Stat(f.listAllCacheFile); err == nil {
		stats["listall_cache_size"] = info.Size()
		stats["listall_cache_modified"] = info.ModTime()

		// 读取并统计listAll缓存条目
		if data, err := os.ReadFile(f.listAllCacheFile); err == nil {
			var cache PersistentListAllCache
			if err := json.Unmarshal(data, &cache); err == nil {
				stats["listall_cache_entries"] = len(cache.Data)
				stats["listall_cache_saved_at"] = cache.SavedAt

				// 统计有效条目
				validEntries := 0
				for _, cachedResp := range cache.Data {
					if time.Since(cachedResp.CachedAt) <= 5*time.Minute {
						validEntries++
					}
				}
				stats["listall_cache_valid_entries"] = validEntries
			}
		}
	}

	// 统计内存中的下载URL缓存
	urlCacheCount := 0
	f.downloadURLCache.Range(func(key, value interface{}) bool {
		urlCacheCount++
		return true
	})
	stats["memory_url_cache_count"] = urlCacheCount

	// 统计内存中的listAll缓存
	if f.listAllCache != nil {
		// 由于cache.Cache没有直接的计数方法，我们提供一个状态指示
		stats["memory_listall_cache_count"] = "available"
	}

	return stats
}

// clearCacheCommand115 清理115网盘缓存命令
func (f *Fs) clearCacheCommand115(ctx context.Context, args []string, opt map[string]string) (any, error) {
	result := make(map[string]interface{})

	// 清理内存中的下载URL缓存
	f.downloadURLCache = sync.Map{}
	result["memory_download_url_cache_cleared"] = true

	// 清理内存中的listAll缓存
	if f.listAllCache != nil {
		f.listAllCache.Clear()
		result["memory_listall_cache_cleared"] = true
	}

	// 清理持久化缓存
	if err := f.clearPersistentCache115(); err != nil {
		result["persistent_cache_error"] = err.Error()
		return result, err
	}

	result["persistent_cache_cleared"] = true
	result["cache_dir"] = f.persistentCacheDir

	// 清理DirCache（如果需要）
	if f.dirCache != nil {
		f.dirCache.ResetRoot()
		result["dir_cache_reset"] = true
	}

	fs.Infof(f, "🧹 115网盘所有缓存已清理完成")
	return result, nil
}

// cacheStatsCommand115 115网盘缓存统计命令
func (f *Fs) cacheStatsCommand115(ctx context.Context, args []string, opt map[string]string) (any, error) {
	stats := f.getCacheStats115()

	// 添加DirCache统计
	if f.dirCache != nil {
		stats["dir_cache_entries"] = "available" // DirCache没有直接的计数方法
	}

	return stats, nil
}

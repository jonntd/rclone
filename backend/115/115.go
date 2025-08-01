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
	"github.com/rclone/rclone/lib/atexit"
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

// ------------------------------------------------------------
// Base response structures
// ------------------------------------------------------------

// TraditionalBase is for the old cookie/encrypted API
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
		// This is a rate limit error
		return fmt.Errorf("%s: rate limit exceeded", out)
	}

	switch code {
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
	URL     string         `json:"url"`              // Present in both
	Client  Int            `json:"client,omitempty"` // Traditional
	Desc    string         `json:"desc,omitempty"`   // Traditional
	OssID   string         `json:"oss_id,omitempty"` // Traditional
	Cookies []*http.Cookie // Added manually after request
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
			*u = DownloadURL{URL: urlStr}
			return nil
		}
		return err // Return original error if string unmarshal also fails
	}
	*u = DownloadURL(aux)
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
	expiry := u.expiry()
	if expiry.IsZero() {
		return false // Assume non-expiring if no expiry found
	}

	// 修复：115网盘URL有效期通常4-5分钟，使用更合理的缓冲时间
	now := time.Now()
	timeUntilExpiry := expiry.Sub(now)

	// 调试信息：记录过期检测详情
	fs.Debugf(nil, "🕐 115网盘URL过期检测: 当前时间=%v, 过期时间=%v, 剩余时间=%v",
		now.Format("15:04:05"), expiry.Format("15:04:05"), timeUntilExpiry)

	// 如果URL已经过期，直接返回true
	if timeUntilExpiry <= 0 {
		fs.Debugf(nil, "❌ 115网盘URL已过期")
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
	fs.Debugf(nil, "🔍 115网盘URL过期判断: 缓冲时间=%v, 是否过期=%v", expiryDelta, isExpired)

	return isExpired
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

// ============================================================================
// Main Backend Implementation (from 115.go)
// ============================================================================

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
	defaultAppID       = "100195123"                       // Provided App ID
	tradUserAgent      = "Mozilla/5.0 115Browser/27.0.7.5" // Keep for traditional login mimicry?
	defaultUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"

	// 115 drive unified QPS configuration - global account level limit
	// 115 drive specificity: all APIs share the same QPS quota, need unified management to avoid 770004 errors

	// 参考rclone_mod优化：大幅提升QPS限制，解决性能瓶颈
	// 基于rclone_mod源码分析，4 QPS是根本性能瓶颈
	unifiedMinSleep = fs.Duration(300 * time.Millisecond) // ~20 QPS - 激进性能优化

	maxSleep      = 2 * time.Second
	decayConstant = 2 // bigger for slower decay, exponential

	maxUploadSize       = 115 * fs.Gibi // 115 GiB from https://proapi.115.com/app/uploadinfo (or OpenAPI equivalent)
	maxUploadParts      = 10000         // Part number must be an integer between 1 and 10000, inclusive.
	defaultChunkSize    = 20 * fs.Mebi  // Reference OpenList: set to 20MB, consistent with OpenList
	minChunkSize        = 100 * fs.Kibi
	maxChunkSize        = 5 * fs.Gibi   // Max part size for OSS
	defaultUploadCutoff = 50 * fs.Mebi  // 参考rclone_mod：设置为50MB，与源码保持一致
	defaultNohashSize   = 100 * fs.Mebi // Set to 100MB, small files prefer traditional upload
	StreamUploadLimit   = 5 * fs.Gibi   // Max size for sample/streamed upload (traditional)
	maxUploadCutoff     = 5 * fs.Gibi   // maximum allowed size for singlepart uploads (OSS PutObject limit)

	// 115网盘特定常量 (从common包迁移)
	// 移除未使用的default115QPS常量

	// 移除未使用的常量：crossCloudTransferPrefix, localTransferPrefix, 哈希长度常量

	tokenRefreshWindow = 10 * time.Minute // Refresh token 10 minutes before expiry
	pkceVerifierLength = 64               // Length for PKCE code verifier

	// Unified file size judgment constants, consistent with 123 drive
	memoryBufferThreshold = int64(50 * 1024 * 1024) // 50MB memory buffer threshold
)

// TraditionalRequest is the standard 115.com request structure for traditional API
// ... existing code ...

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

// 移除未使用的验证函数，使用rclone全局配置

// Options defines the configuration of this backend
// 简化版本：移除与rclone核心功能重复的配置项
type Options struct {
	Cookie              string        `config:"cookie"` // Single cookie string now
	UserAgent           string        `config:"user_agent"`
	RootFolderID        string        `config:"root_folder_id"`
	HashMemoryThreshold fs.SizeSuffix `config:"hash_memory_limit"`
	UploadHashOnly      bool          `config:"upload_hash_only"`
	OnlyStream          bool          `config:"only_stream"`
	FastUpload          bool          `config:"fast_upload"`
	NohashSize          fs.SizeSuffix `config:"nohash_size"`
	MaxUploadParts      int           `config:"max_upload_parts"`

	Internal  bool                 `config:"internal"`
	DualStack bool                 `config:"dual_stack"`
	NoCheck   bool                 `config:"no_check"`
	NoBuffer  bool                 `config:"no_buffer"` // Skip disk buffering for uploads
	Enc       encoder.MultiEncoder `config:"encoding"`
	AppID     string               `config:"app_id"` // Custom App ID for authentication

	// 移除缓存优化配置，使用rclone标准缓存
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

	// 移除统一错误处理器，使用rclone标准错误处理

	// Token management
	tokenMu      sync.Mutex
	accessToken  string
	refreshToken string
	tokenExpiry  time.Time
	codeVerifier string // For PKCE
	tokenRenewer *oauthutil.Renew
	isRefreshing atomic.Bool // 防止递归调用的标志
	loginMu      sync.Mutex

	// 重构：删除pathCache，统一使用dirCache进行目录缓存

	// 移除BadgerDB持久化缓存系统，使用rclone标准缓存

	// 移除httpClient字段，使用rclone标准HTTP客户端

	// 上传操作锁，防止预热和上传同时进行
	uploadingMu sync.Mutex
	isUploading bool
}

// 本地工具函数，替代common包的功能

// isAPILimitError 检查错误是否为API限制错误
func isAPILimitError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "770004") ||
		strings.Contains(errStr, "已达到当前访问上限")
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

// 移除未使用的crossCloudObjectInfo结构体

// 移除未使用的crossCloudFsInfo结构体

type ApiResponse struct {
	State   bool                `json:"state"`   // Indicates success or failure
	Message string              `json:"message"` // Optional message
	Code    int                 `json:"code"`    // Status code
	Data    map[string]FileInfo `json:"data"`    // Map where keys are file IDs (strings) and values are FileInfo objects
}

// Note: FileInfo and DownloadURL are already defined above in the API types section

// retryErrorCodes is a slice of HTTP status codes that we will retry
var retryErrorCodes = []int{
	429, // Too Many Requests.
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
	509, // Bandwidth Limit Exceeded
}

// 注意：CloudDriveErrorClassifier已删除，使用rclone标准错误处理替代

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

	// 使用rclone标准错误处理和网络错误检测
	if err != nil {
		// 回退到rclone标准错误处理
		if fserrors.ShouldRetry(err) {
			return true, err
		}

		// 简化网络错误检测
		if err != nil && (strings.Contains(err.Error(), "connection") ||
			strings.Contains(err.Error(), "timeout") ||
			strings.Contains(err.Error(), "network")) {
			return true, err
		}

		// 认证错误不重试，让上层处理
		if strings.Contains(strings.ToLower(err.Error()), "401") ||
			strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
			return false, err
		}
	}

	// 回退到rclone标准HTTP状态码重试
	return fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
}

// 移除未使用的shouldRetryWithHandler方法

// 移除executeWithUnifiedRetry函数，使用rclone标准错误处理

// 移除getUnifiedConfig方法，不再使用UnifiedBackendConfig

// 移除UnifiedBackendConfig相关的未使用方法

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

// 移除未使用的getTradHTTPClient方法

// getOpenAPIHTTPClient creates an HTTP client with default UserAgent
func getOpenAPIHTTPClient(ctx context.Context, _ *Options) *http.Client {
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
		fs.Debugf(nil, "Couldn't read error response body: %v", readErr)
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
	fs.Debugf(nil, "Couldn't decode error response: %v. Body: %s", decodeErr, string(bodyBytes))
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
	// 创建OpenAPI客户端
	httpClient := getOpenAPIHTTPClient(ctx, &f.opt)

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
	var scanResp TraditionalBase // Use base struct, don't care about response data much
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
	var confirmResp TraditionalBase
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

	fs.Debugf(f, "Successfully obtained access token, expires at %v", f.tokenExpiry)

	return nil
}

// refreshTokenIfNecessary refreshes the token if necessary
func (f *Fs) refreshTokenIfNecessary(ctx context.Context, refreshTokenExpired bool, forceRefresh bool) error {
	// 🚨 修复递归调用：设置刷新标志，防止在token刷新过程中再次触发刷新
	if !f.isRefreshing.CompareAndSwap(false, true) {
		fs.Debugf(f, "Token refresh already in progress, skipping")
		return nil
	}
	defer f.isRefreshing.Store(false)

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
		f.saveToken(f.m)
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
	f.saveToken(f.m)

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

// CallAPI 统一API调用方法，支持OpenAPI和传统API
func (f *Fs) CallAPI(ctx context.Context, apiType APIType, opts *rest.Opts, request any, response any) error {
	// 🚨 修复空指针问题：确保客户端存在
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
		if err := f.prepareCookieForRequest(ctx, opts); err != nil {
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

		return f.shouldRetry(ctx, resp, err)
	})
}

// prepareCookieForRequest 为传统API准备Cookie认证
func (f *Fs) prepareCookieForRequest(ctx context.Context, opts *rest.Opts) error {
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

// shouldRetry 判断是否应该重试请求
func (f *Fs) shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	return fserrors.ShouldRetry(err), err
}

// 移除ensureOpenAPIClient，使用统一客户端

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
	// 🚨 修复递归调用：创建一个新的rest客户端，完全避免任何token验证机制
	// 因为这个方法本身就是在刷新token，不能再触发token验证
	httpClient := getOpenAPIHTTPClient(ctx, &f.opt)
	if httpClient == nil {
		return nil, fmt.Errorf("failed to create HTTP client for token refresh")
	}
	tempClient := rest.NewClient(httpClient).SetErrorHandler(errorHandler)
	if tempClient == nil {
		return nil, fmt.Errorf("failed to create REST client for token refresh")
	}

	resp, err := tempClient.CallJSON(ctx, &opts, nil, &refreshResp)
	if err != nil {
		return nil, err
	}

	// 检查HTTP状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("refresh token API returned status %d", resp.StatusCode)
	}

	// 响应已经通过CallJSON解析到refreshResp中
	fs.Debugf(f, "Token refresh response received successfully")

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
// 🚨 完全参考参考代码重写，移除递归检查，依靠正确的API调用避免递归
func handleRefreshError(f *Fs, ctx context.Context, err error) (*RefreshTokenResp, error) {
	fs.Errorf(f, "Refresh token failed: %v", err)

	// Check if the error indicates the refresh token itself is expired
	var tokenErr *TokenError
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
	fs.Debugf(f, "Token refreshed successfully, new expiry: %v", f.tokenExpiry)
}

// saveToken saves the current token to the config
func (f *Fs) saveToken(m configmap.Mapper) {
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
	// Ensure root URL is set if not provided in opts
	if opts.RootURL == "" {
		opts.RootURL = openAPIRootURL
	}

	// Smart QPS management: select appropriate pacer based on API endpoint
	smartPacer := f.getPacerForEndpoint(opts.Path)
	fs.Debugf(f, " 智能QPS选择: %s -> 使用智能调速器", opts.Path)

	// Wrap the entire attempt sequence with the smart pacer, returning proper retry signals
	return smartPacer.Call(func() (shouldRetryGlobal bool, errGlobal error) {
		// Ensure token is available and current
		if !skipToken {
			if err := f.prepareTokenForRequest(ctx, opts); err != nil {
				fs.Debugf(f, " CallOpenAPI: prepareTokenForRequest失败: %v", err)
				return false, err
			}
		}

		// Make the API call
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

		fs.Debugf(f, "pacer: OpenAPI call successful")
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
				fs.Debugf(f, " CallUploadAPI: prepareTokenForRequest失败: %v", err)
				return false, err
			}
		}

		// Make the actual API call
		fs.Debugf(f, " CallUploadAPI: 执行API调用")
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

		fs.Debugf(f, " CallUploadAPI: API调用成功")
		return f.shouldRetry(ctx, nil, err)
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
				fs.Debugf(f, " CallDownloadURLAPI: prepareTokenForRequest失败: %v", err)
				return false, err
			}
		}

		// Make the actual API call
		fs.Debugf(f, " CallDownloadURLAPI: 执行API调用")
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

		fs.Debugf(f, " CallDownloadURLAPI: API调用成功")
		return f.shouldRetry(ctx, nil, err)
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
	// 🚨 修复递归调用：避免在API调用中触发token刷新导致的无限递归
	if needsRefresh {
		// 检查是否已经在token刷新过程中，避免递归调用
		if f.isRefreshing.Load() {
			fs.Debugf(f, "Token refresh already in progress, using current token")
			f.tokenMu.Lock()
			currentToken = f.accessToken
			f.tokenMu.Unlock()
		} else {
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
	// 🚨 修复循环调用：直接使用openAPIClient，不通过CallAPI
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
		fs.Debugf(f, " executeOpenAPICall失败: %v", err)
	}

	return resp, err
}

// handleTokenError processes token-related errors and attempts to refresh or re-login
func (f *Fs) handleTokenError(ctx context.Context, opts *rest.Opts, apiErr error, skipToken bool) (bool, error) {
	var tokenErr *TokenError
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
	} else {
		return f.executeEncryptedCall(ctx, opts, request, response)
	}
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
		fs.Debugf(f, "pacer: low level retry required for traditional call (error: %v)", retryErr)
		return true, retryErr
	}

	// Handle non-retryable errors
	if apiErr != nil {
		fs.Debugf(f, "pacer: permanent error encountered in traditional call: %v", apiErr)
		return false, apiErr
	}

	// Success
	fs.Debugf(f, "pacer: traditional call successful")
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

	// 移除对已删除配置项的验证，使用rclone全局配置

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

	// 简化：移除复杂的根ID解析，使用标准路径处理

	root = strings.Trim(root, "/")

	return opt, originalName, root, nil
}

// createBasicFs115 创建基础115网盘文件系统对象
// 简化版本：移除UnifiedBackendConfig抽象层
func createBasicFs115(name, originalName, root string, opt *Options, m configmap.Mapper) *Fs {
	return &Fs{
		name:         name,
		originalName: originalName,
		root:         root,
		opt:          *opt,
		m:            m,
		// 移除errorHandler初始化，使用rclone标准错误处理
	}
}

// 移除initializeFeatures115函数，已内联到NewFs中使用标准Fill模式

// 移除initializeCaches115函数，不再使用自定义缓存

// 简化：移除过度复杂的统一组件初始化函数

// newFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// 初始化和验证配置选项
	opt, originalName, normalizedRoot, err := initializeOptions115(name, root, m)
	if err != nil {
		return nil, err
	}

	// 创建基础文件系统对象
	f := createBasicFs115(name, originalName, normalizedRoot, opt, m)

	// 🚨 修复空指针问题：立即初始化客户端，参考其他backend的标准做法
	// 使用rclone标准HTTP客户端创建方式
	httpClient := fshttp.NewClient(ctx)
	f.openAPIClient = rest.NewClient(httpClient).SetErrorHandler(errorHandler)
	if f.openAPIClient == nil {
		return nil, fmt.Errorf("failed to create OpenAPI REST client")
	}
	fs.Debugf(f, "OpenAPI REST client initialized successfully")

	// 初始化功能特性 - 使用标准Fill模式
	f.features = (&fs.Features{
		DuplicateFiles:          false,
		CanHaveEmptyDirectories: true,
		NoMultiThreading:        true, // Keep true as downloads might still use traditional API
	}).Fill(ctx, f)

	// 移除缓存系统初始化，使用rclone标准缓存

	// 简化：移除统一组件初始化

	// 注意：统一组件已在initializeUnifiedComponents115中初始化，此处不需要重复初始化

	// 移除httpClient字段，使用rclone标准HTTP客户端

	// Setting appVer (needed for traditional calls)
	re := regexp.MustCompile(`\d+\.\d+\.\d+(\.\d+)?$`)
	if m := re.FindStringSubmatch(tradUserAgent); m == nil {
		fs.Logf(f, "Could not parse app version from User-Agent %q. Using default.", tradUserAgent)
		f.appVer = "27.0.7.5" // Default fallback
	} else {
		f.appVer = m[0]
		fs.Debugf(f, "Using App Version %q from User-Agent %q", f.appVer, tradUserAgent)
	}

	// Initialize single pacer，使用115网盘统一QPS限制
	// 115网盘所有API都受到相同的全局QPS限制（4 QPS），使用单一pacer符合rclone标准模式
	f.pacer = fs.NewPacer(ctx, pacer.NewDefault(
		pacer.MinSleep(time.Duration(unifiedMinSleep)),
		pacer.MaxSleep(maxSleep),
		pacer.DecayConstant(decayConstant)))

	// 显示当前QPS配置
	qps := 1000.0 / float64(time.Duration(unifiedMinSleep)/time.Millisecond)
	fs.Debugf(f, "115网盘统一QPS配置: %v (~%.1f QPS)", time.Duration(unifiedMinSleep), qps)

	// 移除多个pacer实例，使用单一pacer符合rclone标准模式

	// 客户端已在前面初始化，无需重复创建

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
		f.saveToken(m)
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
		f.saveToken(m)
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

	// 暂时禁用智能缓存预热，避免干扰上传性能
	// go f.intelligentCachePreheating(ctx)

	// 全新设计：智能路径处理策略
	// 参考123网盘的简洁设计，但针对115网盘的特点进行优化
	if f.root != "" {
		// 移除缓存检查，直接进行路径解析

		// 策略2：缓存未命中，使用智能检测
		if isLikelyFilePath(f.root) {
			return f.handleAsFile(ctx)
		} else {
			return f.handleAsDirectory(ctx)
		}
	}

	return f, nil
}

// isFromRemoteSource 判断输入流是否来自远程源（用于跨云传输检测）
// 修复：支持跨云传输断点续传TaskID一致性
func (f *Fs) isFromRemoteSource(in io.Reader) bool {
	// 简单的跨云传输检测：检查输入流类型
	// 如果是RereadableObject或其他特殊类型，可能是跨云传输
	if ro, ok := in.(*RereadableObject); ok {
		fs.Debugf(f, "🌐 检测到RereadableObject，可能是跨云传输: %v", ro)
		return true
	}

	// 检查是否是特殊的Reader类型（通常用于跨云传输）
	// 简化检测：如果不是基本的文件类型，可能是跨云传输
	readerType := fmt.Sprintf("%T", in)
	if strings.Contains(readerType, "Accounted") || strings.Contains(readerType, "Cross") {
		fs.Debugf(f, "🌐 检测到特殊Reader类型，可能是跨云传输: %s", readerType)
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
			fs.Debugf(f, " NewObject: 单文件匹配成功")
			return obj, nil
		}
		fs.Debugf(f, " NewObject: 单文件不匹配，返回NotFound")
		return nil, fs.ErrorObjectNotFound // If remote doesn't match the single file
	}

	result, err := f.newObjectWithInfo(ctx, remote, nil)
	if err != nil {
		fs.Debugf(f, " NewObject失败: %v", err)
	}
	return result, err
}

// FindLeaf finds a directory or file leaf in the parent folder pathID.
// Used by dircache.
// 重构：移除pathCache，统一使用dirCache进行目录缓存
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (foundID string, found bool, err error) {
	// 重构：直接使用listAll查找，不再使用pathCache
	// dirCache已经在上层提供了路径缓存功能，避免重复缓存

	// Use listAll which now uses OpenAPI，使用rclone全局配置的检查器数量
	listChunk := fs.GetConfig(ctx).Checkers
	if listChunk <= 0 {
		listChunk = 1150 // 115网盘OpenAPI的最大限制
	}
	found, err = f.listAll(ctx, pathID, listChunk, false, false, func(item *File) bool {
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
				// 重构：移除pathCache.Put()调用，统一使用dirCache
			} else {
				// 这是文件，不返回ID（保持foundID为空字符串）
				foundID = "" // 明确设置为空，表示找到的是文件而不是目录
				// 重构：移除pathCache.Put()调用，文件信息不需要缓存在目录缓存中
			}
			return true // Stop searching
		}
		return false // Continue searching
	})

	// 重构：如果遇到API限制错误，清理dirCache而不是pathCache
	if isAPILimitError(err) {
		f.dirCache.Flush()
	}

	return foundID, found, err
}

// 重构：删除GetDirID()方法，标准lib/dircache不需要此方法
// GetDirID()功能已通过FindLeaf()和CreateDir()的递归调用实现

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

	// 移除缓存查询，直接调用API

	// Call API to get directory listing
	var fileList []File
	var iErr error

	// 使用rclone全局配置的检查器数量
	listChunk := fs.GetConfig(ctx).Checkers
	if listChunk <= 0 {
		listChunk = 1150 // 115网盘OpenAPI的最大限制
	}
	_, err = f.listAll(ctx, dirID, listChunk, false, false, func(item *File) bool {
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

	// 移除缓存保存

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

	// 跨云传输检测和本地化处理
	if f.isRemoteSource(src) {
		fs.Infof(f, "🌐 检测到跨云传输: %s → 115网盘 (大小: %s)", src.Fs().Name(), fs.SizeSuffix(src.Size()))
		fs.Infof(f, "📥 强制下载到本地后上传，确保数据完整性...")
		return f.crossCloudUploadWithLocalCache(ctx, in, src, remote, options...)
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

	// 重构：上传成功后，清理dirCache相关缓存
	// 上传可能影响目录结构，清理缓存确保一致性
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
		fs.Debugf(srcDir, "Merging contents into %v", dstDir)

		// List all items in the source directory
		var itemsToMove []*File
		listChunk := fs.GetConfig(ctx).Checkers
		if listChunk <= 0 {
			listChunk = 1150 // 115网盘OpenAPI的最大限制
		}
		_, err = f.listAll(ctx, srcDirID, listChunk, false, false, func(item *File) bool {
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
		found, listErr := f.listAll(ctx, dirID, 1, false, false, func(item *File) bool {
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
		if isAPILimitError(err) {
			fs.Debugf(f, "readMetaDataForPath遇到API限制错误，路径: %q, 错误: %v", path, err)
			// 重构：清理dirCache而不是pathCache
			f.dirCache.Flush()
			return nil, err
		}
		if err == fs.ErrorDirNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}

	fs.Debugf(f, " readMetaDataForPath: FindPath成功, leaf=%q, dirID=%q", leaf, dirID)

	// 验证目录ID是否有效
	if dirID == "" {
		fs.Errorf(f, "🔍 readMetaDataForPath: 目录ID为空，路径: %q, leaf: %q", path, leaf)
		// 清理可能损坏的缓存
		f.dirCache.Flush()
		return nil, fmt.Errorf("invalid directory ID for path %q", path)
	}

	// List the directory and find the leaf
	fs.Debugf(f, " readMetaDataForPath: 开始调用listAll, dirID=%q, leaf=%q", dirID, leaf)
	listChunk := fs.GetConfig(ctx).Checkers
	if listChunk <= 0 {
		listChunk = 1150 // 115网盘OpenAPI的最大限制
	}
	found, err := f.listAll(ctx, dirID, listChunk, true, false, func(item *File) bool {
		// Compare with decoded name to handle special characters correctly
		decodedName := f.opt.Enc.ToStandardName(item.FileNameBest())
		if decodedName == leaf {
			fs.Debugf(f, " readMetaDataForPath: 找到匹配文件: %q", decodedName)
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
		fs.Debugf(f, " readMetaDataForPath: 未找到文件")
		return nil, fs.ErrorObjectNotFound
	}
	fs.Debugf(f, " readMetaDataForPath: 成功找到文件元数据")
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
		"sync-delete": "同步删除: false(仅创建.strm文件)/true(删除孤立文件和空目录) (默认: true，类似rclone sync)",
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
		// 🎬 新增：媒体库同步功能
		return f.mediaSyncCommand(ctx, arg, opt)

	case "get-download-url":
		// 🔗 新增：通过pick_code获取下载URL
		return f.getDownloadURLCommand(ctx, arg, opt)

	default:
		return nil, fs.ErrorCommandNotFound
	}
}

func (f *Fs) GetPickCodeByPath(ctx context.Context, path string) (string, error) {
	// 🔧 修复：根据官方API文档使用正确的参数类型和方法
	fs.Debugf(f, "通过路径获取PickCode: %s", path)

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
		State   bool   `json:"state"` // 🔧 修复：根据API文档，state是boolean类型
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
		return "", fmt.Errorf("获取PickCode失败: %w", err)
	}

	// 检查API返回的状态
	if response.Code != 0 {
		return "", fmt.Errorf("API返回错误: %s (Code: %d)", response.Message, response.Code)
	}

	fs.Debugf(f, "Got file info: %s -> PickCode: %s, name: %s, size: %d bytes",
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
		return "", fmt.Errorf("通过文件ID获取pickCode失败: %w", err)
	}

	if !response.State || response.Code != 0 {
		return "", fmt.Errorf("API返回错误: %s (Code: %d, State: %t)", response.Message, response.Code, response.State)
	}

	if response.Data.PickCode == "" {
		return "", fmt.Errorf("API返回空的pickCode，文件ID: %s", fileID)
	}

	fs.Debugf(f, " 成功通过文件ID获取pickCode: fileID=%s, pickCode=%s, fileName=%s",
		fileID, response.Data.PickCode, response.Data.FileName)

	return response.Data.PickCode, nil
}

func (f *Fs) getDownloadURLByUA(ctx context.Context, filePath string, UA string) (string, error) {
	// 🔧 修复：添加与123后端一致的路径处理逻辑
	if filePath == "" {
		filePath = f.root
	}

	// 路径清理逻辑，参考123后端
	if filePath == "/" {
		return "", fmt.Errorf("根目录不是文件")
	}
	if len(filePath) > 1 && strings.HasSuffix(filePath, "/") {
		filePath = filePath[:len(filePath)-1]
	}

	fs.Debugf(f, "处理文件路径: %s", filePath)

	// 🔧 性能优化：直接HTTP调用获取PickCode，避免rclone框架开销
	pickCode, err := f.getPickCodeByPathDirect(ctx, filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read metadata for file path %q: %w", filePath, err)
	}

	fs.Debugf(f, "🔄 通过路径获取pick_code成功: %s -> %s", filePath, pickCode)

	// 使用通用的原始HTTP实现
	return f.getDownloadURLByPickCodeHTTP(ctx, pickCode, UA)
}

// getPickCodeByPathDirect 使用rclone标准方式获取PickCode
func (f *Fs) getPickCodeByPathDirect(ctx context.Context, path string) (string, error) {
	fs.Debugf(f, "使用rclone标准方式获取PickCode: %s", path)

	// 构建请求选项
	opts := rest.Opts{
		Method: "GET",
		Path:   "/open/folder/get_info",
		Parameters: url.Values{
			"path": []string{path},
		},
	}

	// 准备认证信息
	f.prepareTokenForRequest(ctx, &opts)

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
		return "", fmt.Errorf("请求失败: %w", err)
	}

	if response.Code != 0 {
		return "", fmt.Errorf("API返回错误: %s (Code: %d)", response.Message, response.Code)
	}

	fs.Debugf(f, "直接HTTP获取PickCode成功: %s -> %s", path, response.Data.PickCode)
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

// 移除冗余的open方法，已内联到Open方法中

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

	// 115网盘下载策略说明：完全禁用并发策略
	fs.Debugf(o, "📋 115网盘下载策略：完全禁用并发（1TB门槛+1GB分片，强制使用普通下载）")

	// 简化：移除复杂的并发下载逻辑，使用rclone标准的单线程下载

	// Get/refresh download URL
	err = o.setDownloadURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get download URL: %w", err)
	}
	if !o.durl.Valid() {
		// Attempt refresh again if invalid right after getting it
		fs.Debugf(o, "Download URL invalid immediately after fetching, retrying...")
		time.Sleep(500 * time.Millisecond)         // Small delay before retry
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
			return nil, fmt.Errorf("重新获取下载URL失败: %w", err)
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
		fs.Debugf(o, "❌ 115网盘HTTP请求失败: %v", err)
		return nil, err
	}

	// Range响应验证
	if rangeHeader := req.Header.Get("Range"); rangeHeader != "" {
		contentLength := resp.Header.Get("Content-Length")
		contentRange := resp.Header.Get("Content-Range")
		fs.Debugf(o, " 115网盘Range响应: Status=%d, Content-Length=%s, Content-Range=%s",
			resp.StatusCode, contentLength, contentRange)

		// 检查Range请求是否被正确处理
		if resp.StatusCode != 206 {
			fs.Debugf(o, "⚠️ 115网盘Range请求未返回206状态码: %d", resp.StatusCode)
		}
	}

	return resp.Body, nil
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

	// 重构：删除文件成功后，清理dirCache相关缓存
	// 删除文件可能影响父目录的缓存
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

	// 根本性修复：彻底解决pickCode和fileID混淆问题
	pickCode := info.PickCodeBest()
	fileID := info.ID()

	fs.Debugf(o, " setMetaData调试: fileName=%s, pickCode=%s, fileID=%s",
		info.FileNameBest(), pickCode, fileID)

	// 关键修复：检测pickCode是否实际上是fileID
	if pickCode != "" && pickCode == fileID {
		fs.Debugf(o, "⚠️ 检测到pickCode与fileID相同，这是错误的: %s", pickCode)
		pickCode = "" // 清空错误的pickCode
	}

	if pickCode == "" {
		fs.Debugf(o, "⚠️ 文件元数据中pickCode为空，将在需要时动态获取")
	} else {
		// 验证pickCode格式（115网盘的pickCode通常是字母数字组合）
		isAllDigits := true
		for _, r := range pickCode {
			if r < '0' || r > '9' {
				isAllDigits = false
				break
			}
		}

		if len(pickCode) > 15 && isAllDigits {
			fs.Debugf(o, "⚠️ 检测到疑似文件ID而非pickCode: %s，将在需要时重新获取", pickCode)
			pickCode = "" // 清空错误的pickCode，强制重新获取
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

	// Use the path-based lookup
	info, err := o.fs.readMetaDataForPath(ctx, o.remote)
	if err != nil {
		fs.Debugf(o.fs, " readMetaData失败: %v", err)
		return err // fs.ErrorObjectNotFound or other errors
	}

	err = o.setMetaData(info)
	if err != nil {
		fs.Debugf(o.fs, " readMetaData: setMetaData失败: %v", err)
	}
	return err
}

// setDownloadURL ensures a valid download URL is available with optimized concurrent access.
func (o *Object) setDownloadURL(ctx context.Context) error {
	// 并发安全修复：快速路径也需要锁保护
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
	var newURL *DownloadURL

	// Add timeout context for URL fetching to prevent hanging
	urlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if o.fs.isShare {
		// Share download is no longer supported
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
							return fmt.Errorf("cannot get pick code by file ID %s: %w", o.id, pickErr)
						}
						o.pickCode = pickCode
						fs.Debugf(o, " 成功通过文件ID获取pickCode: %s", pickCode)
					} else {
						o.pickCodeMu.Unlock()
						return fmt.Errorf("cannot get download URL without pick code (metadata read error: %v)", metaErr)
					}
				}
			}
			o.pickCodeMu.Unlock()
		}

		// 修复并发问题：确保pickCode有效后再调用
		if o.pickCode == "" {
			return fmt.Errorf("cannot get download URL: pickCode is still empty after all attempts")
		}

		// 根本性修复：使用验证过的pickCode
		validPickCode, validationErr := o.fs.validateAndCorrectPickCode(urlCtx, o.pickCode)
		if validationErr != nil {
			return fmt.Errorf("pickCode validation failed: %w", validationErr)
		}

		// 如果pickCode被修正，更新Object中的pickCode
		if validPickCode != o.pickCode {
			fs.Debugf(o, " Object pickCode已修正: %s -> %s", o.pickCode, validPickCode)
			o.pickCode = validPickCode
		}

		newURL, err = o.fs.getDownloadURL(urlCtx, validPickCode)
	}

	if err != nil {
		o.durl = nil      // Clear invalid URL
		o.durlMu.Unlock() // 错误路径释放锁

		// 简化：直接返回错误，不使用复杂的错误处理器
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
		fs.Debugf(o, "Successfully fetched download URL")
	}
	o.durlMu.Unlock() // 成功路径释放锁
	return nil
}

// setDownloadURLWithForce sets the download URL for the object with optional cache bypass
func (o *Object) setDownloadURLWithForce(ctx context.Context, forceRefresh bool) error {
	// 并发安全修复：快速路径也需要锁保护
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
		o.durlMu.Unlock() // 无效URL路径释放锁
		return fmt.Errorf("fetched download URL is invalid or expired immediately")
	} else {
		fs.Debugf(o, "Successfully fetched download URL")
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

// calculateOptimalChunkSize intelligently calculates upload chunk size for 115 drive
// Use larger chunk sizes to reduce chunk count and improve overall transfer efficiency
func (f *Fs) calculateOptimalChunkSize(fileSize int64) fs.SizeSuffix {
	// Based on 2MB/s target speed optimization: use larger chunks to reduce API call overhead
	// Larger chunks can better utilize network bandwidth and reduce inter-chunk overhead
	var partSize int64

	// 修复：优化分片策略，减少小文件的分片数量
	// 小文件使用更大的分片，减少网络开销和API调用次数
	switch {
	case fileSize <= 100*1024*1024: // ≤100MB
		partSize = fileSize // 小文件直接单分片上传，避免分片开销
	case fileSize <= 500*1024*1024: // ≤500MB
		partSize = 100 * 1024 * 1024 // 100MB分片，减少分片数量
	case fileSize <= 2*1024*1024*1024: // ≤2GB
		partSize = 200 * 1024 * 1024 // 200MB分片
	case fileSize <= 10*1024*1024*1024: // ≤10GB
		partSize = 500 * 1024 * 1024 // 500MB分片
	case fileSize <= 100*1024*1024*1024: // ≤100GB
		partSize = 1 * 1024 * 1024 * 1024 // 1GB分片
	default: // >100GB
		partSize = 2 * 1024 * 1024 * 1024 // 2GB分片
	}

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
	fs.Infof(f, "🌐 开始跨云传输本地化处理: %s (大小: %s)", remote, fs.SizeSuffix(fileSize))

	// 修复重复下载：检查是否已有完整的临时文件
	if tempReader, tempSize, tempCleanup := f.checkExistingTempFile(src); tempReader != nil {
		fs.Infof(f, "🎯 发现已有完整下载文件，跳过重复下载: %s", fs.SizeSuffix(tempSize))

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
			return nil, fmt.Errorf("跨云传输下载到内存失败: %w", err)
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
			fs.Infof(f, "🗂️  大文件跨云传输，下载到临时文件: %s", fs.SizeSuffix(fileSize))
		}
		tempFile, err := os.CreateTemp("", "115_cross_cloud_*.tmp")
		if err != nil {
			return nil, fmt.Errorf("创建临时文件失败: %w", err)
		}

		written, err := io.Copy(tempFile, in)
		if err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("跨云传输下载到临时文件失败: %w", err)
		}

		// 重置文件指针到开头
		if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("重置临时文件指针失败: %w", err)
		}

		// 关键修复：创建带Account对象的文件读取器
		cleanupFunc := func() {
			tempFile.Close()
			os.Remove(tempFile.Name())
		}
		localDataSource = NewAccountedFileReader(ctx, tempFile, written, remote, cleanupFunc)
		localFileSize = written
		cleanup = func() {
			// AccountedFileReader会在Close时调用cleanupFunc
		}
		fs.Infof(f, "✅ 跨云传输临时文件下载完成: %s", fs.SizeSuffix(localFileSize))
	}
	defer cleanup()

	// 验证下载完整性
	if localFileSize != fileSize {
		return nil, fmt.Errorf("跨云传输下载不完整: 期望%d字节，实际%d字节", fileSize, localFileSize)
	}

	// 使用本地数据进行上传，创建本地文件信息对象
	localSrc := &localFileInfo{
		name:    filepath.Base(remote),
		size:    localFileSize,
		modTime: src.ModTime(ctx),
		remote:  remote,
	}

	fs.Infof(f, "🚀 开始从本地数据上传到115网盘: %s", fs.SizeSuffix(localFileSize))

	// 使用主上传函数，但此时源已经是本地数据
	return f.upload(ctx, localDataSource, localSrc, remote, options...)
}

// 新增：智能文件路径检测函数
// isLikelyFilePath 判断给定路径是否可能是文件路径而非目录路径
func isLikelyFilePath(path string) bool {
	if path == "" {
		return false
	}

	// 如果路径以斜杠结尾，很可能是目录
	if strings.HasSuffix(path, "/") {
		return false
	}

	// 检查是否包含文件扩展名（包含点且点后面有字符）
	lastSlash := strings.LastIndex(path, "/")
	fileName := path
	if lastSlash >= 0 {
		fileName = path[lastSlash+1:]
	}

	// 文件名包含点且不是隐藏文件（不以点开头）
	if strings.Contains(fileName, ".") && !strings.HasPrefix(fileName, ".") {
		dotIndex := strings.LastIndex(fileName, ".")
		// 确保点不是最后一个字符，且扩展名不为空
		if dotIndex > 0 && dotIndex < len(fileName)-1 {
			// 添加调试信息
			fs.Debugf(nil, " isLikelyFilePath: 路径 %q 被识别为文件路径 (fileName=%q)", path, fileName)
			return true
		}
	}

	fs.Debugf(nil, " isLikelyFilePath: 路径 %q 被识别为目录路径 (fileName=%q)", path, fileName)
	return false
}

// 移除savePathTypeToCache函数，不再使用自定义缓存

// 新增：dir_list缓存支持函数，参考123网盘实现

// 移除getDirListFromCache和saveDirListToCache函数，不再使用自定义缓存

// 新增：路径预填充缓存功能
// 移除preloadPathCache函数，不再使用自定义缓存

// 新设计：智能路径处理方法

// 移除checkPathTypeFromCache函数，不再使用自定义缓存

// 移除未使用的setupFileFromCache函数

// handleAsFile 将路径作为文件处理
func (f *Fs) handleAsFile(ctx context.Context) (*Fs, error) {
	// 分割路径获取父目录和文件名
	newRoot, remote := dircache.SplitPath(f.root)

	// 创建临时Fs指向父目录
	tempF := &Fs{
		name:          f.name,
		originalName:  f.originalName,
		root:          newRoot,
		opt:           f.opt,
		features:      f.features,
		openAPIClient: f.openAPIClient,
		tradClient:    f.tradClient,
		pacer:         f.pacer,
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
		// 移除缓存实例复制，不再使用自定义缓存
		// 简化：移除resumeManager
	}

	tempF.dirCache = dircache.New(newRoot, f.rootFolderID, tempF)

	// 尝试找到父目录
	err := tempF.dirCache.FindRoot(ctx, false)
	if err != nil {
		fs.Debugf(f, " 文件处理：父目录不存在，回退到目录模式: %v", err)
		return f.handleAsDirectory(ctx)
	}

	// 关键：使用轻量级验证，参考123网盘策略
	_, err = tempF.NewObject(ctx, remote)
	if err == nil {
		fs.Debugf(f, " 文件验证成功，设置文件模式: %s", remote)

		// 设置文件模式
		f.root = newRoot
		f.dirCache = tempF.dirCache

		obj := &Object{
			fs:          f,
			remote:      remote,
			hasMetaData: false,
			durlMu:      new(sync.Mutex),
		}

		var fsObj fs.Object = obj
		f.fileObj = &fsObj

		// 🔧 修复：对于backend命令，不返回ErrorIsFile，而是返回正常的Fs实例
		// 这样可以让backend命令正常工作，同时保持文件对象的引用
		fs.Debugf(f, "文件路径处理：创建文件模式Fs实例，文件: %s", obj.Remote())
		return f, nil
	} else {
		fs.Debugf(f, " 文件验证失败，回退到目录模式: %v", err)
		return f.handleAsDirectory(ctx)
	}
}

// 115网盘统一QPS管理 - 使用单一调速器
// getPacerForEndpoint 返回统一的调速器，符合rclone标准模式
// 115网盘所有API都受到相同的全局QPS限制，不需要复杂的选择逻辑
func (f *Fs) getPacerForEndpoint(endpoint string) *fs.Pacer {
	// 所有API使用统一的pacer，符合115网盘的全局QPS限制
	return f.pacer
}

// handleAsDirectory 将路径作为目录处理
func (f *Fs) handleAsDirectory(ctx context.Context) (*Fs, error) {
	// 尝试找到目录
	err := f.dirCache.FindRoot(ctx, false)
	if err != nil {
		fs.Debugf(f, " 目录处理：目录不存在: %v", err)
		return f, nil
	}

	// 移除缓存保存

	fs.Debugf(f, " 目录模式设置完成: %s", f.root)
	return f, nil
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
		r.file.Close()
	}
	if r.tempPath != "" {
		os.Remove(r.tempPath)
	}
	return nil
}

// 移除未使用的Pan115DownloadAdapter，简化下载逻辑

// Remove GetConfig function - no longer needed without DownloadConfig

// ============================================================================
// Functions from helper.go
// ============================================================================

// listAll retrieves directory listings, using OpenAPI if possible.
// User function fn should return true to stop processing.
type listAllFn func(*File) bool

func (f *Fs) listAll(ctx context.Context, dirID string, limit int, filesOnly, dirsOnly bool, fn listAllFn) (found bool, err error) {
	fs.Debugf(f, " listAll开始: dirID=%q, limit=%d, filesOnly=%v, dirsOnly=%v", dirID, limit, filesOnly, dirsOnly)

	// 验证目录ID
	if dirID == "" {
		fs.Errorf(f, "🔍 listAll: 目录ID为空，这可能导致查询根目录")
		// 不要直接返回错误，而是记录警告并继续，因为根目录查询可能是合法的
	}

	if f.isShare {
		// Share listing is no longer supported
		return false, errors.New("share listing is not supported")
	}

	// Use OpenAPI listing
	fs.Debugf(f, " listAll: 使用OpenAPI模式")
	params := url.Values{}
	params.Set("cid", dirID)
	params.Set("limit", strconv.Itoa(limit))
	params.Set("offset", "0")
	params.Set("show_dir", "1") // Include directories in the listing

	// Sorting (OpenAPI uses query params)
	params.Set("o", "user_utime") // Default sort: update time
	params.Set("asc", "0")        // Default sort: descending

	offset := 0

	fs.Debugf(f, " listAll: 开始分页循环")
	for {
		fs.Debugf(f, " listAll: 处理offset=%d", offset)
		params.Set("offset", strconv.Itoa(offset))
		opts := rest.Opts{
			Method:     "GET",
			Path:       "/open/ufile/files",
			Parameters: params,
		}

		fs.Debugf(f, " listAll: 准备调用CallOpenAPI")
		var info FileList
		err = f.CallOpenAPI(ctx, &opts, nil, &info, false) // Use OpenAPI call
		if err != nil {
			fs.Debugf(f, " listAll: CallOpenAPI失败: %v", err)
			// 处理API限制错误
			if err = f.handleAPILimitError(ctx, err, &opts, &info, dirID); err != nil {
				return found, err
			}
		}

		fs.Debugf(f, " listAll: CallOpenAPI成功，返回%d个文件", len(info.Files))

		if len(info.Files) == 0 {
			break // No more items
		}

		for _, item := range info.Files {
			isDir := item.IsDir()
			// Apply client-side filtering if needed
			if filesOnly && isDir {
				continue
			}
			if dirsOnly && !isDir {
				continue
			}

			// Decode name
			item.FileName = f.opt.Enc.ToStandardName(item.FileNameBest()) // Use best name getter

			if fn(item) {
				found = true
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

	return found, nil
}

// handleAPILimitError 处理API限制错误，减少嵌套
func (f *Fs) handleAPILimitError(ctx context.Context, err error, opts *rest.Opts, info *FileList, dirID string) error {
	// 检查是否是API限制错误
	if !strings.Contains(err.Error(), "770004") && !strings.Contains(err.Error(), "已达到当前访问上限") {
		return fmt.Errorf("OpenAPI list failed for dir %s: %w", dirID, err)
	}

	fs.Infof(f, "⚠️  遇到115网盘API限制(770004)，使用统一等待策略...")

	// 115网盘统一QPS管理：使用固定等待时间避免复杂性
	waitTime := 30 * time.Second // 统一等待30秒
	fs.Infof(f, "⏰ API限制等待 %v 后重试...", waitTime)

	// 创建带超时的等待
	select {
	case <-time.After(waitTime):
		// 重试一次
		fs.Debugf(f, "🔄 API限制重试...")
		retryErr := f.CallOpenAPI(ctx, opts, nil, info, false)
		if retryErr != nil {
			return fmt.Errorf("OpenAPI list failed for dir %s after QPS retry: %w", dirID, retryErr)
		}
		fs.Debugf(f, " API限制重试成功")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("context cancelled while waiting for API limit: %w", ctx.Err())
	}
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

	// 根本性修复：在函数入口就验证和修正pickCode
	originalPickCode := pickCode
	pickCode, err = f.validateAndCorrectPickCode(ctx, pickCode)
	if err != nil {
		return nil, fmt.Errorf("pickCode validation failed: %w", err)
	}

	if originalPickCode != pickCode {
		fs.Infof(f, "✅ pickCode已修正: %s -> %s", originalPickCode, pickCode)
	}

	// ️ 下载URL缓存已删除，直接调用API获取
	if forceRefresh {
		fs.Debugf(f, "115网盘强制刷新下载URL: pickCode=%s", pickCode)
	} else {
		fs.Debugf(f, "115网盘获取下载URL: pickCode=%s", pickCode)
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
		fs.Debugf(f, "115网盘下载URL响应解析失败: %v, 原始数据: %s", err, string(respData.Data))

		// 修复：如果是空数据，提供更有用的错误信息
		if string(respData.Data) == "[]" || string(respData.Data) == "{}" {
			return nil, fmt.Errorf("115网盘API返回空数据，文件可能已删除或无权限访问。pickCode: %s", pickCode)
		}

		return nil, fmt.Errorf("failed to parse download URL response for pickcode %s: %w", pickCode, err)
	}

	if downInfo == nil {
		return nil, fmt.Errorf("no download info found for pickcode %s", pickCode)
	}

	fs.Debugf(f, "115网盘成功获取下载URL: pickCode=%s, fileName=%s, fileSize=%d",
		pickCode, downInfo.FileName, int64(downInfo.FileSize))

	// 从URL中解析真实的过期时间（仅用于日志记录）
	if realExpiresAt := f.parseURLExpiry(downInfo.URL.URL); realExpiresAt.IsZero() {
		fs.Debugf(f, "115网盘无法解析URL过期时间，使用默认1小时: pickCode=%s", pickCode)
	} else {
		fs.Debugf(f, "115网盘解析到URL过期时间: pickCode=%s, 过期时间=%v", pickCode, realExpiresAt)
	}

	// ️ 下载URL缓存已删除，不再保存到缓存
	fs.Debugf(f, "115网盘下载URL获取成功，不使用缓存: pickCode=%s", pickCode)

	return &downInfo.URL, nil
}

// validateAndCorrectPickCode 验证并修正pickCode格式
func (f *Fs) validateAndCorrectPickCode(ctx context.Context, pickCode string) (string, error) {
	// 空pickCode直接返回错误
	if pickCode == "" {
		return "", fmt.Errorf("empty pickCode provided")
	}

	// 检查是否为纯数字且过长（可能是文件ID而不是pickCode）
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

		fs.Debugf(f, " 成功通过文件ID获取正确的pickCode: %s -> %s", pickCode, correctPickCode)
		return correctPickCode, nil
	}

	// pickCode格式看起来正确，直接返回
	return pickCode, nil
}

// parseURLExpiry 从URL中解析过期时间
func (f *Fs) parseURLExpiry(urlStr string) time.Time {
	if p, err := url.Parse(urlStr); err == nil {
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

// ------------------------------------------------------------
// Traditional API Helpers (Sharing, Offline Download)
// ------------------------------------------------------------

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
			fs.Debugf(f, "pacer: low level retry required for traditional call with response (error: %v)", retryErr)
			return true, retryErr // Signal globalPacer to retry
		}

		// Handle non-retryable errors
		if apiErr != nil {
			fs.Debugf(f, "pacer: permanent error encountered in traditional call with response: %v", apiErr)
			return false, apiErr
		}

		// Check API-level errors in response struct
		if errResp := f.checkResponseForAPIErrors(response); errResp != nil {
			fs.Debugf(f, "pacer: permanent API error encountered in traditional call with response: %v", errResp)
			return false, errResp
		}

		fs.Debugf(f, "pacer: traditional call with response successful")
		return false, nil // Success, don't retry
	})

	return httpResp, err
}

// ============================================================================
// Functions from upload.go
// ============================================================================

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
			fs.Debugf(loggingObj, "Retry context cancelled for '%s': %v", description, err)
			return fmt.Errorf("retry cancelled for '%s': %w", description, err)
		}

		if retryCount > 0 {
			// Check context cancellation again before sleeping
			if err := retryCtx.Err(); err != nil {
				fs.Debugf(loggingObj, "Retry context cancelled before delay for '%s': %v", description, err)
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
			fs.Debugf(loggingObj, "Non-retryable error (%s) when '%s': %v", classification, description, err)
			return err // Wrap in Permanent to stop retries
		}

		// Log retryable errors
		fs.Debugf(loggingObj, "Retryable error (%s) on attempt %d for '%s', will retry: %v", classification, retryCount, description, err)

		return err // Return the retryable error to trigger the next retry
	}

	// Define notify function for logging retry attempts (optional, handled within retryOperation now)
	// notify := func(err error, delay time.Duration) {
	// if err != nil {
	// fs.Debugf(loggingObj, "Error during '%s': %v. Retrying in %v...", description, err, delay)
	// }
	// }

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
							fs.Debugf(obj, "Pacer: Retrying Open after rate limit error: %v", openErr)
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
				fs.Debugf(nil, "Preserved existing accounting wrapper for RereadableObject")
			}
		}

		// Always wrap with accounting, even if we found an existing one
		// The accounting system will handle this properly
		stats := accounting.Stats(r.ctx)
		if stats == nil {
			fs.Debugf(nil, "No Stats found - accounting may not work correctly")
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
			fs.Debugf(nil, "🔗 RereadableObject使用父传输会计系统: %s", name)
			accReader = r.parentTransfer.Account(r.ctx, rc).WithBuffer()
			r.transfer = r.parentTransfer // 引用父传输
		} else {
			// 关键修复：检测跨云传输场景
			if srcFs != nil && srcFs.Name() == "123" {
				fs.Debugf(nil, "🌐 检测到123网盘跨云传输场景")
			}

			// 创建独立传输（回退方案）
			if r.transfer == nil {
				fs.Debugf(nil, "⚠️ RereadableObject创建独立传输: %s", name)
				r.transfer = stats.NewTransferRemoteSize(name, r.size, srcFs, dstFs)
			}
			accReader = r.transfer.Account(r.ctx, rc).WithBuffer()
		}

		r.currReader = accReader

		// Extract the accounting object for later use
		_, r.acc = accounting.UnWrapAccounting(accReader)

		// 关键修复：确保Account对象能够被后续的上传逻辑正确识别
		fs.Debugf(nil, " RereadableObject Account状态: acc=%v, currReader类型=%T",
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
		fs.Debugf(nil, " RereadableObject读取: %d字节, Account=%v", n, r.acc != nil)
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

// GetAccount 直接返回Account对象（解决UnWrapAccounting限制）
// 关键修复：UnWrapAccounting只能识别*accountStream类型，
// 我们需要提供直接访问Account对象的方法
func (r *RereadableObject) GetAccount() *accounting.Account {
	return r.acc
}

// AccountedFileReader 包装*os.File以支持Account对象
// 关键修复：解决跨云传输中临时文件丢失Account对象的问题
type AccountedFileReader struct {
	file    *os.File
	acc     *accounting.Account
	size    int64
	name    string
	cleanup func()
}

// NewAccountedFileReader 创建带Account对象的文件读取器
// 修复进度重复计数：不创建独立Transfer，让rclone标准机制处理
func NewAccountedFileReader(ctx context.Context, file *os.File, size int64, name string, cleanup func()) *AccountedFileReader {
	// 修复120%进度问题：不创建独立的Transfer对象
	// 跨云传输的进度应该由主传输流程统一管理
	// 这里只是一个简单的文件包装器

	return &AccountedFileReader{
		file:    file,
		acc:     nil, // 不创建Account对象，避免重复计数
		size:    size,
		name:    name,
		cleanup: cleanup,
	}
}

// Read 实现io.Reader接口
// 修复：AccountedFileReader不应该重复更新Account进度
// Account进度应该由rclone的标准机制自动处理
func (a *AccountedFileReader) Read(p []byte) (n int, err error) {
	// 直接读取文件，不手动更新Account
	// rclone的Transfer.Account()机制会自动处理进度更新
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

// GetAccount 返回Account对象
// 修复120%进度问题：返回nil，让rclone标准机制处理进度
func (a *AccountedFileReader) GetAccount() *accounting.Account {
	// 不返回Account对象，避免重复计数
	// 进度应该由rclone的Transfer.Account()机制统一管理
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
		fs.Debugf(f, "Skipping buffering due to no_buffer option")
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
	tempFile, err := os.CreateTemp("", "rclone_buffer_*.tmp")
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
		fs.Debugf(f, " 用AccountedFileReader包装临时文件，保持Account对象传递")
		cleanupFunc := func() {
			closeErr := tempFile.Close()
			removeErr := os.Remove(tempFile.Name())
			if closeErr != nil {
				fs.Errorf(nil, "Failed to close temp file %s: %v", tempFile.Name(), closeErr)
			}
			if removeErr != nil {
				fs.Errorf(nil, "Failed to remove temp file %s: %v", tempFile.Name(), removeErr)
			} else {
				fs.Debugf(nil, "Cleaned up temp file: %s", tempFile.Name())
			}
		}

		accountedReader := &AccountedFileReader{
			file:    tempFile,
			acc:     account,
			size:    written,
			name:    "buffered_temp_file_with_account",
			cleanup: cleanupFunc,
		}

		// 重新定义cleanup，让AccountedFileReader处理清理
		newCleanup := func() {
			// AccountedFileReader会在Close时调用cleanupFunc
		}

		return accountedReader, newCleanup, nil
	}

	// Set up cleanup function for regular case
	cleanup = func() {
		tempFile.Close()
		os.Remove(tempFile.Name())
	}

	fs.Debugf(f, "Buffered %d bytes to temporary file", written)
	return tempFile, cleanup, nil
}

// bufferIO已废弃，统一使用bufferIOWithAccount

// bufferIOwithSHA1 buffers the input and calculates its SHA-1 hash.
// Returns the SHA-1 hash, the potentially buffered reader, and a cleanup function.
func bufferIOwithSHA1(f *Fs, in io.Reader, src fs.ObjectInfo, size, threshold int64, ctx context.Context, options ...fs.OpenOption) (sha1sum string, out io.Reader, cleanup func(), err error) {
	cleanup = func() {} // Default no-op cleanup

	// First check if the source object already has the SHA1 hash
	if srcObj, ok := src.(fs.Object); ok {
		hashVal, hashErr := srcObj.Hash(ctx, hash.SHA1)
		if hashErr == nil && hashVal != "" {
			fs.Debugf(srcObj, "Using precalculated SHA1: %s", hashVal)
			return hashVal, in, cleanup, nil
		}
	}

	// If NoBuffer option is enabled, calculate the SHA1 using a non-buffering approach
	if f.opt.NoBuffer {
		fs.Debugf(f, "Computing SHA1 without buffering due to no_buffer option")

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
		fs.Debugf(f, "Calculated SHA1 without buffering: %s", sha1sum)
		return sha1sum, newReader, cleanup, nil
	}

	// 使用标准SHA1计算，简化逻辑

	// 关键修复：在创建TeeReader之前检查并保存Account对象
	var originalAccount *accounting.Account
	if accountedReader, ok := in.(*AccountedFileReader); ok {
		originalAccount = accountedReader.GetAccount()
		fs.Debugf(f, " bufferIOwithSHA1检测到AccountedFileReader，将保持Account对象传递")
	}

	// 创建一个管道来同时计算SHA1和缓冲数据
	pr, pw := io.Pipe()
	var sha1Hash string
	var sha1Err error

	// 在goroutine中计算SHA1
	go func() {
		defer pw.Close()
		hasher := sha1.New()
		_, err := io.Copy(hasher, pr)
		if err != nil {
			sha1Err = err
			return
		}
		sha1Hash = fmt.Sprintf("%x", hasher.Sum(nil))
	}()

	// 创建TeeReader将数据同时写入管道和缓冲区
	tee := io.TeeReader(in, pw)

	// Buffer the input using the tee reader，传递Account对象信息
	out, cleanup, err = bufferIOWithAccount(f, tee, size, threshold, originalAccount)
	if err != nil {
		pw.Close() // 确保管道关闭
		// Cleanup is handled by bufferIO on error
		return "", nil, cleanup, fmt.Errorf("failed to buffer input for SHA1 calculation: %w", err)
	}

	// 等待SHA1计算完成
	pw.Close() // 关闭写端，让SHA1计算完成
	if sha1Err != nil {
		return "", nil, cleanup, fmt.Errorf("failed to calculate SHA1: %w", sha1Err)
	}

	sha1sum = sha1Hash
	return sha1sum, out, cleanup, nil
}

// initUploadOpenAPI calls the OpenAPI /open/upload/init endpoint.
func (f *Fs) initUploadOpenAPI(ctx context.Context, size int64, name, dirID, sha1sum, preSha1, pickCode, signKey, signVal string) (*UploadInitInfo, error) {
	form := url.Values{}

	// 修复文件名参数错误：清理文件名中的特殊字符
	cleanName := f.opt.Enc.FromStandardName(name)
	// 115网盘API对某些字符敏感，进行额外清理
	// cleanName = strings.ReplaceAll(cleanName, " - ", "_") // 替换 " - " 为 "_"
	// cleanName = strings.ReplaceAll(cleanName, " ", "_")   // 替换空格为下划线
	// cleanName = strings.ReplaceAll(cleanName, "(", "_")   // 替换括号
	// cleanName = strings.ReplaceAll(cleanName, ")", "_")   // 替换括号
	fs.Debugf(f, " 文件名清理: %q -> %q", name, cleanName)

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
	if pickCode != "" {
		form.Set("pick_code", pickCode) // For resuming? Docs are unclear if init uses this. Resume endpoint definitely does.
	}
	if signKey != "" && signVal != "" {
		form.Set("sign_key", signKey)
		form.Set("sign_val", signVal) // Value should be uppercase SHA1 of range
	}
	// 根据115网盘官方API文档，添加topupload参数
	// 0：单文件上传任务标识一条单独的文件上传记录
	form.Set("topupload", "0")

	// Log parameters for debugging, but mask sensitive values
	fs.Debugf(f, "Initializing upload for file_name=%q (cleaned: %q), size=%d, target=U_1_%s, has_fileid=%v, has_preid=%v, has_pickcode=%v, has_sign=%v",
		name, cleanName, size, dirID, sha1sum != "", preSha1 != "", pickCode != "", signKey != "")

	// Log API parameters
	fs.Debugf(f, "API params: file_name=%q, file_size=%d, target=%q, has_fileid=%v, has_preid=%v, has_pickcode=%v",
		cleanName, size, "U_1_"+dirID, sha1sum != "", preSha1 != "", pickCode != "")

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
		// Try to extract more specific error information
		// If it's a parameter error (code 1001), provide more context
		if strings.Contains(err.Error(), "参数错误") || strings.Contains(err.Error(), "1001") {
			return nil, fmt.Errorf("OpenAPI initUpload failed with parameter error: %w (file_name=%q, size=%d, dirID=%s)",
				err, name, size, dirID)
		}

		// If it's a rate limit error, provide advice
		if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "Too Many Requests") {
			return nil, fmt.Errorf("OpenAPI initUpload failed due to rate limiting: %w. Consider using --low-level-retries flag to increase retries",
				err)
		}

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
			fs.Debugf(f, "⚠️ API返回成功但数据不完整，尝试继续处理...")
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
				return nil, fmt.Errorf("115网盘文件校验失败(10002): %s - 可能是文件传输过程中损坏，建议重试", errMsg)
			case 10001:
				return nil, fmt.Errorf("115网盘上传参数错误(10001): %s", errMsg)
			default:
				return nil, fmt.Errorf("115网盘上传失败(错误码:%d): %s", errCode, errMsg)
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
					fs.Debugf(f, "OpenAPI callback data parsed: file_id=%q, pick_code=%q", cbData.FileID, cbData.PickCode)

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
		fs.Debugf(f, "Callback data missing required fields: file_id=%q, pick_code=%q, raw JSON: %s",
			cbData.FileID, cbData.PickCode, string(callbackJSON))

		// Additional debugging for error analysis
		if strings.Contains(string(callbackJSON), "10002") {
			fs.Logf(f, "🔍 115网盘错误分析: 错误码10002通常表示文件在传输过程中损坏")
			fs.Logf(f, "💡 可能的解决方案: 1) 检查网络稳定性 2) 重试上传 3) 验证源文件完整性")
		}

		return nil, fmt.Errorf("OSS callback data missing required fields (file_id or pick_code). JSON: %s", string(callbackJSON))
	}

	fs.Debugf(f, "Traditional callback data parsed: file_id=%q, pick_code=%q", cbData.FileID, cbData.PickCode)
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
				fs.Errorf(f, "Failed to fetch OSS token: %v", err)
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
		WithConnectTimeout(5 * time.Second). // 减少到5秒，快速失败重试
		// 修复：优化读写超时，适合分片上传
		WithReadWriteTimeout(60 * time.Second) // 减少到60秒，适合20MB分片

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
	sha1Hash := fmt.Sprintf("%x", hasher.Sum(nil))
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

	fs.Debugf(o, "Attempting hash upload (秒传) via OpenAPI...")

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
			fs.Debugf(o, "✅ SHA1已设置到Object: %s", o.sha1sum)
		}
	} else {
		fs.Debugf(o, "Using provided SHA1: %s", hashStr)

		// 确保Object中也有SHA1信息
		if o != nil && o.sha1sum == "" {
			o.sha1sum = strings.ToUpper(hashStr)
			fs.Debugf(o, "✅ SHA1已设置到Object: %s", o.sha1sum)
		}

		// If NoBuffer is enabled, wrap the input in a RereadableObject
		if f.opt.NoBuffer {
			// 修复：检测跨云传输并尝试集成父传输
			var ro *RereadableObject
			var roErr error

			// 检测是否为跨云传输（特别是123网盘源）
			if f.isRemoteSource(src) {
				fs.Debugf(o, "🌐 检测到跨云传输，尝试查找父传输对象")

				// 尝试从accounting统计中获取当前传输
				if stats := accounting.Stats(ctx); stats != nil {
					// 这里我们暂时使用标准方法，但添加了跨云传输标记
					// 后续可以通过其他方式获取父传输对象
					fs.Debugf(o, " 跨云传输场景，创建增强RereadableObject")
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
	o.sha1sum = strings.ToLower(hashStr) // Store hash in object

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
				preID = fmt.Sprintf("%x", hasher.Sum(nil))
				fs.Debugf(o, "计算PreID成功: %s (前%d字节)", preID, n)
			}
			seeker.Seek(0, io.SeekStart) // 重置到文件开头供后续使用
		} else {
			fs.Debugf(o, "无法计算PreID：输入流不支持Seek操作")
		}
	}

	// 3. Call OpenAPI initUpload with SHA1 and PreID
	ui, err = f.initUploadOpenAPI(ctx, size, leaf, dirID, hashStr, preID, "", "", "")
	if err != nil {
		return false, nil, newIn, cleanup, fmt.Errorf("OpenAPI initUpload for hash check failed: %w", err)
	}

	// 3. Handle response status
	signKey, signVal := "", ""
	for {
		status := ui.GetStatus()
		switch status {
		case 2: // 秒传 success!
			fs.Infof(o, "🎉 秒传成功！文件已存在于服务器，无需重复上传")
			fs.Debugf(o, "Hash upload (秒传) successful.")
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
			fs.Debugf(o, "Hash upload (秒传) not available (status 1). Proceeding with normal upload.")
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
				ui, initErr = f.initUploadOpenAPI(ctx, size, leaf, dirID, hashStr, "", "", signKey, signVal)
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

		case 6, 8: // Other auth-related statuses? Treat as failure for now.
			fs.Errorf(o, "Hash upload failed with unexpected auth status %d. Message: %s", status, ui.ErrMsg())
			return false, nil, newIn, cleanup, fmt.Errorf("hash upload failed with status %d: %s", status, ui.ErrMsg())

		default: // Unexpected status
			fs.Errorf(o, "Hash upload failed with unexpected status %d. Message: %s", status, ui.ErrMsg())
			return false, nil, newIn, cleanup, fmt.Errorf("unexpected hash upload status %d: %s", status, ui.ErrMsg())
		}
	}
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
	uploadInfo, err := f.getUploadInfo(ctx, ui, leaf, dirID, size, o)
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

	fs.Infof(o, "🎉 多部分上传完成！文件大小: %s", fs.SizeSuffix(size))
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
	ui, err := f.initUploadOpenAPI(ctx, size, leaf, dirID, sha1sum, "", "", "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize multipart upload: %w", err)
	}

	// 修复空指针：确保ui不为空
	if ui == nil {
		return nil, fmt.Errorf("initUploadOpenAPI returned nil UploadInitInfo")
	}

	// Handle unexpected status
	if ui.GetStatus() != 1 {
		if ui.GetStatus() == 2 { // Instant upload success (秒传)
			fs.Logf(o, "Warning: initUpload without hash resulted in 秒传 (status 2), handling...")
			o.id = ui.GetFileID()
			o.pickCode = ui.GetPickCode()
			o.hasMetaData = true
		} else {
			return nil, fmt.Errorf("expected status 1 from initUpload for multipart, got %d: %s", ui.GetStatus(), ui.ErrMsg())
		}
	}

	return ui, nil
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
	// 参考阿里云OSS UploadFile示例：智能选择上传策略
	// 使用rclone全局配置的多线程切换阈值
	uploadCutoff := int64(fs.GetConfig(context.Background()).MultiThreadCutoff)

	// 修复：使用统一的分片大小计算函数，确保与分片上传一致
	optimalPartSize := int64(f.calculateOptimalChunkSize(size))
	fs.Debugf(o, "🚀 智能分片大小计算: 文件大小=%s, 最优分片大小=%s",
		fs.SizeSuffix(size), fs.SizeSuffix(optimalPartSize))

	// 参考OpenList：极简进度回调，最大化减少开销
	var lastLoggedPercent int
	var lastLogTime time.Time

	// 关键修复：获取Account对象用于进度集成
	var currentAccount *accounting.Account
	if unwrappedIn, acc := accounting.UnWrapAccounting(in); acc != nil {
		currentAccount = acc
		fs.Debugf(o, " 找到Account对象，将集成115网盘上传进度")
		_ = unwrappedIn // 避免未使用变量警告
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
					fs.Infof(o, "📤 115网盘分片上传: %d%% (%s/%s)",
						currentPercent, fs.SizeSuffix(transferred), fs.SizeSuffix(total))
					lastLoggedPercent = currentPercent
					lastLogTime = now
				}
			}

			// 修复：只保留调试日志，不重复更新Account对象
			if currentAccount != nil && increment > 0 {
				fs.Debugf(o, " ProgressFn: increment=%s, transferred=%s, total=%s",
					fs.SizeSuffix(increment), fs.SizeSuffix(transferred), fs.SizeSuffix(total))
			}
		},
	}

	if size >= 0 && size < uploadCutoff {
		// Small file: use OSS PutObject (single file upload)
		fs.Debugf(o, "115 OSS single file upload: %s (%s)", leaf, fs.SizeSuffix(size))
		return f.performOSSPutObject(ctx, ossClient, in, src, o, leaf, dirID, size, ui, uploaderConfig, options...)
	} else {
		// Large file: use OSS multipart upload
		fs.Debugf(o, "115 OSS multipart upload: %s (%s)", leaf, fs.SizeSuffix(size))
		return f.performOSSMultipart(ctx, ossClient, in, src, o, leaf, dirID, size, ui, uploaderConfig, options...)
	}
}

// Upload115Config 115网盘上传管理器配置
// 参考阿里云OSS UploadFile示例的配置思想
type Upload115Config struct {
	PartSize    int64                                     // 分片大小
	ParallelNum int                                       // 并行数（115网盘固定为1）
	ProgressFn  func(increment, transferred, total int64) // 进度回调函数
}

// ️ 已删除calculateOptimalPartSize函数，统一使用calculateOptimalChunkSize

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

	// 添加详细的调试信息，参考阿里云OSS示例
	fs.Debugf(o, " OSS PutObject配置: Bucket=%s, Key=%s", ui.GetBucket(), ui.GetObject())

	// 超激进优化：OSS上传完全绕过QPS限制，直接上传
	fs.Debugf(f, "🚀 OSS直传模式：绕过所有QPS限制，最大化上传速度")
	putRes, err := ossClient.PutObject(ctx, req)
	if err != nil {
		fs.Errorf(f, "OSS PutObject failed: %v", err)
		return nil, fmt.Errorf("OSS PutObject failed: %w", err)
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

	fs.Infof(o, "✅ 115网盘OSS单文件上传完成: %s", fs.SizeSuffix(size))
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
	config *Upload115Config,
	options ...fs.OpenOption,
) (*CallbackData, error) {
	// 参考阿里云OSS UploadFile示例：使用配置信息进行分片上传
	fs.Debugf(o, "使用配置信息进行OSS分片上传: PartSize=%s, ParallelNum=%d",
		fs.SizeSuffix(config.PartSize), config.ParallelNum)

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

	// Process upload callback with retry for specific errors
	callbackData, err := f.postUpload(chunkWriter.callbackRes)
	if err != nil {
		// Check if this is a file validation error (10002) that might benefit from retry
		if strings.Contains(err.Error(), "10002") || strings.Contains(err.Error(), "校验文件失败") {
			fs.Logf(o, "115 file validation failed, possibly due to network transmission issues")
			fs.Logf(o, "Suggestion: 1) Check network stability 2) Retry upload 3) Recalculate file hash if problem persists")
		}
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

	fs.Infof(o, "Traditional upload completed! File size: %s", fs.SizeSuffix(size))
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

				fs.Debugf(f, " 找到匹配的临时文件: %s (大小: %s, 修改时间: %v)",
					tempPath, fs.SizeSuffix(stat.Size()), stat.ModTime())

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

	// 设置上传标志，防止预热干扰上传
	f.uploadingMu.Lock()
	f.isUploading = true
	f.uploadingMu.Unlock()

	defer func() {
		f.uploadingMu.Lock()
		f.isUploading = false
		f.uploadingMu.Unlock()
		fs.Debugf(f, " 上传完成，清除上传标志")
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
		return nil, fmt.Errorf("file size %v exceeds upload limit %v", fs.SizeSuffix(size), fs.SizeSuffix(maxUploadSize))
	}
	if size < 0 {
		// Streaming upload with unknown size - check if allowed
		if f.opt.OnlyStream {
			fs.Logf(src, "Streaming upload with unknown size using traditional sample upload (limit %v)", fs.SizeSuffix(StreamUploadLimit))
			// Proceed with sample upload, it might fail if size > limit
		} else if f.opt.FastUpload {
			fs.Logf(src, "Streaming upload with unknown size using traditional sample upload (limit %v) due to fast_upload", fs.SizeSuffix(StreamUploadLimit))
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

	// --- Upload Strategy Logic ---

	// 跨云传输标志：用于跳过后续的重复秒传尝试
	skipHashUpload := false

	// 跨云盘传输检测：优先尝试秒传，忽略大小限制
	if f.isRemoteSource(src) && size >= 0 {
		fs.Infof(o, "🌐 检测到跨云盘传输，强制尝试秒传...")
		gotIt, _, newIn, localCleanup, err := f.tryHashUpload(ctx, in, src, o, leaf, dirID, size, options...)
		cleanup = localCleanup // 设置清理函数
		if err != nil {
			fs.Logf(o, "跨云盘秒传尝试失败，回退到正常上传: %v", err)
			// 关键修复：设置标志跳过后续的秒传尝试
			skipHashUpload = true
			fs.Debugf(o, " 跨云传输秒传失败，跳过后续秒传尝试")
			// 重置状态，继续正常上传流程
			gotIt = false
			if !f.opt.NoBuffer {
				newIn = in // 恢复原始输入
				if cleanup != nil {
					cleanup()
					cleanup = nil
				}
			}
		} else if gotIt {
			fs.Infof(o, "🎉 跨云盘秒传成功！文件已存在于115网盘服务器")
			return o, nil
		} else {
			fs.Debugf(o, "跨云盘秒传未命中，继续正常上传流程")
			// 关键修复：设置标志跳过后续的秒传尝试
			skipHashUpload = true
			fs.Debugf(o, " 跨云传输秒传未命中，跳过后续秒传尝试")
			// 修复重复下载：跨云传输秒传失败后，直接进入OSS上传，跳过后续的秒传尝试
			fs.Infof(o, "🚀 跨云传输直接进入OSS上传，避免重复处理")
			// 继续使用newIn进行后续上传
			in = newIn
		}
	}

	// 1. OnlyStream flag
	if f.opt.OnlyStream {
		if size < 0 || size <= int64(StreamUploadLimit) {
			return f.doSampleUpload(ctx, in, o, leaf, dirID, size, options...)
		}
		return nil, fserrors.NoRetryError(fmt.Errorf("only_stream is enabled but file size %v exceeds stream upload limit %v", fs.SizeSuffix(size), fs.SizeSuffix(StreamUploadLimit)))
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
		// Size > streamLimit: Use OSS multipart
		return f.uploadToOSS(ctx, newIn, src, o, leaf, dirID, size, ui, options...) // Pass ui in case init was already done
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

	// 修复重复下载：检查是否为跨云传输且已经尝试过秒传
	// skipHashUpload 已在上面定义，这里只需要检查和设置
	if f.isRemoteSource(src) && size >= 0 && !skipHashUpload {
		// 如果是跨云传输但还没有尝试过秒传，这里不需要额外设置
		fs.Debugf(o, " 跨云传输检查：skipHashUpload=%v", skipHashUpload)
	}

	// 4. Default (Normal) Logic - 优先使用OSS上传策略
	// 使用rclone全局配置的多线程切换阈值
	uploadCutoff := int64(fs.GetConfig(context.Background()).MultiThreadCutoff)

	// For very small files (<1MB), use traditional upload for efficiency
	if size >= 0 && size < int64(1*fs.Mebi) {
		fs.Debugf(o, "Small file (%s) using traditional upload", fs.SizeSuffix(size))
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

	// 修复重复下载：如果跳过秒传，设置默认状态
	if skipHashUpload {
		fs.Debugf(o, " 跳过秒传尝试（跨云传输已尝试），直接进入OSS上传")
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
		// Use OSS multipart with retry for validation errors
		const maxRetries = 2
		var lastErr error

		for retry := 0; retry <= maxRetries; retry++ {
			if retry > 0 {
				fs.Logf(o, "🔄 115网盘上传重试 %d/%d (上次错误: %v)", retry, maxRetries, lastErr)

				// 检查是否需要重新计算哈希
				needsHashRecalculation := false
				if strings.Contains(lastErr.Error(), "10002") || strings.Contains(lastErr.Error(), "校验文件失败") {
					fs.Logf(o, "🔧 检测到文件校验失败，需要重新计算哈希")
					needsHashRecalculation = true
				} else if strings.Contains(lastErr.Error(), "requires SHA1 hash") {
					fs.Logf(o, "🔧 检测到SHA1哈希缺失，需要重新计算哈希")
					needsHashRecalculation = true
				}

				if needsHashRecalculation {
					// 清除可能损坏的哈希信息，强制重新计算
					ui = nil
					o.sha1sum = ""

					// 如果输入是可重新读取的，重置到开头
					if seeker, ok := newIn.(io.Seeker); ok {
						if _, seekErr := seeker.Seek(0, io.SeekStart); seekErr != nil {
							fs.Debugf(o, "⚠️ 无法重置输入流到开头: %v", seekErr)
						} else {
							fs.Debugf(o, "✅ 输入流已重置到开头，准备重新计算哈希")
						}
					}

					// 重新尝试哈希上传以计算SHA1
					fs.Debugf(o, "🔧 重试前重新计算SHA1哈希...")
					if size >= 0 {
						var localCleanup func()
						_, retryUI, retryNewIn, localCleanup, hashErr := f.tryHashUpload(ctx, newIn, src, o, leaf, dirID, size, options...)
						if hashErr == nil && retryUI != nil {
							ui = retryUI
							newIn = retryNewIn
							if cleanup != nil {
								cleanup() // 清理之前的资源
							}
							cleanup = localCleanup
							fs.Debugf(o, "✅ 重新计算SHA1成功，准备重试上传")
						} else {
							fs.Debugf(o, "⚠️ 重新计算SHA1失败: %v，继续使用原有流程", hashErr)
							if localCleanup != nil {
								localCleanup()
							}
						}
					}
				}
			}

			result, err := f.uploadToOSS(ctx, newIn, src, o, leaf, dirID, size, ui, options...) // Pass ui if available
			if err == nil {
				return result, nil
			}

			lastErr = err

			// Check if this error is worth retrying
			if !strings.Contains(err.Error(), "10002") && !strings.Contains(err.Error(), "校验文件失败") {
				// Not a validation error, don't retry
				break
			}

			if retry < maxRetries {
				fs.Logf(o, "⚠️ 115网盘文件校验失败，将在重试时重新计算哈希")
			}
		}

		return nil, fmt.Errorf("115网盘上传失败，已重试%d次: %w", maxRetries, lastErr)
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
				return nil, fmt.Errorf("OSS PutObject requires SHA1 hash but none available - should calculate hash first")
			}
		}

		var initErr error
		ui, initErr = f.initUploadOpenAPI(ctx, size, leaf, dirID, sha1sum, "", "", "", "")
		if initErr != nil {
			return nil, fmt.Errorf("failed to initialize PutObject upload: %w", initErr)
		}
		if ui.GetStatus() != 1 { // Should be 1 if hash upload failed/skipped
			if ui.GetStatus() == 2 { // Unexpected 秒传
				fs.Logf(o, "Warning: initUpload for PutObject resulted in 秒传 (status 2), handling...")
				o.id = ui.GetFileID()
				o.pickCode = ui.GetPickCode()
				o.hasMetaData = true
				return o, nil
			}
			return nil, fmt.Errorf("expected status 1 from initUpload for PutObject, got %d: %s", ui.GetStatus(), ui.ErrMsg())
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
		// 平衡优化：使用极简进度回调，保持rclone进度跟踪但最小化开销
		ProgressFn: func() func(increment, transferred, total int64) {
			var lastTransferred int64
			return func(increment, transferred, total int64) {
				// 修复120%进度问题：移除重复的Account更新
				// AccountedFileReader已经通过Read()方法自动更新Account进度
				// 这里再次更新会导致重复计数，造成进度超过100%

				// 只保留调试日志，不更新Account对象
				if transferred > lastTransferred {
					fs.Debugf(o, " Sample upload progress: %s/%s (%d%%)",
						fs.SizeSuffix(transferred), fs.SizeSuffix(total),
						int(float64(transferred)/float64(total)*100))
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

	// 开始OSS单文件上传
	fs.Infof(o, "🚀 115网盘开始OSS单文件上传: %s (%s)", leaf, fs.SizeSuffix(size))

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
		fs.Debugf(o, "Marked RereadableObject transfer as complete after PutObject upload")
	}

	fs.Infof(o, "🎉 文件上传完成！文件大小: %s", fs.SizeSuffix(size))
	fs.Debugf(o, "OSS PutObject successful.")
	return o, nil
}

// ============================================================================
// Functions from multipart.go
// ============================================================================

// 115网盘分片上传实现
// 采用单线程顺序上传模式，确保上传稳定性和SHA1验证通过

const (
	// 优化缓冲区大小，适合单分片上传
	bufferSize           = 8 * 1024 * 1024 // 8MB缓冲区，适合单分片上传
	bufferCacheFlushTime = 5 * time.Second // flush the cached buffers after this long
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
		// Initialise the buffer pool when used
		bufferPool = pool.New(bufferCacheFlushTime, bufferSize, 32, ci.UseMmap) // 32个缓冲区
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
	defer atexit.OnError(&err, func() {
		cancel()
		fs.Debugf(w.o, " 分片上传取消中...")
		errCancel := w.Abort(ctx)
		if errCancel != nil {
			fs.Debugf(w.o, "❌ 取消分片上传失败: %v", errCancel)
		}
	})()

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
		fs.Debugf(w.o, "⚠️ 文件大小未知，将动态计算分片数")
	}

	return params
}

// setupAccountingAndInput 设置Account对象和输入流
func (w *ossChunkWriter) setupAccountingAndInput() (*accounting.Account, io.Reader) {
	var acc *accounting.Account
	var in io.Reader

	// 首先检查是否为RereadableObject类型
	if rereadableObj, ok := w.in.(*RereadableObject); ok {
		acc = rereadableObj.GetAccount()
		in = rereadableObj.OldStream()
		if acc != nil {
			fs.Debugf(w.o, " 从RereadableObject获取到Account对象，上传进度将正确显示")
		} else {
			fs.Debugf(w.o, "⚠️ RereadableObject中没有Account对象")
		}
	} else if accountedReader, ok := w.in.(*AccountedFileReader); ok {
		// AccountedFileReader不再提供Account对象，进度由rclone标准机制管理
		acc = nil
		in = accountedReader
		fs.Debugf(w.o, " 检测到AccountedFileReader，进度由rclone标准机制管理")
	} else {
		// 回退到标准方法
		in, acc = accounting.UnWrapAccounting(w.in)
		if acc != nil {
			fs.Debugf(w.o, " 通过UnWrapAccounting获取到Account对象")
		} else {
			fs.Debugf(w.o, "⚠️ 未找到Account对象，输入类型: %T", w.in)
		}
	}

	return acc, in
}

// performChunkedUpload 执行分片上传循环
func (w *ossChunkWriter) performChunkedUpload(ctx context.Context, in io.Reader, acc *accounting.Account, params *uploadParams) (int64, error) {
	fs.Infof(w.o, "🚀 开始115网盘单线程分片上传")
	fs.Infof(w.o, "📊 上传参数: 文件大小=%v, 分片大小=%v, 预计分片数=%d",
		fs.SizeSuffix(params.size), fs.SizeSuffix(params.chunkSize), params.totalParts)
	fs.Debugf(w.o, " OSS配置: Bucket=%s, Key=%s, UploadId=%s",
		*w.imur.Bucket, *w.imur.Key, *w.imur.UploadId)

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
	n, err := io.CopyN(rw, in, params.chunkSize)
	finished := false

	if err == io.EOF {
		if n == 0 && partNum != 0 {
			fs.Debugf(w.o, " 所有分片读取完成")
			return 0, true, nil
		}
		finished = true
	} else if err != nil {
		return 0, false, fmt.Errorf("读取分片数据失败: %w", err)
	}

	// 显示进度信息
	w.logUploadProgress(partNum, n, off, params)

	// 上传分片
	currentPart := partNum + 1
	fs.Debugf(w.o, " 开始上传分片%d: 大小=%v, 偏移=%v", currentPart, fs.SizeSuffix(n), fs.SizeSuffix(off))

	chunkSize, err := w.uploadChunkWithRetry(ctx, int32(partNum), rw)
	if err != nil {
		return 0, false, fmt.Errorf("上传分片%d失败 (大小:%v, 偏移:%v, 已重试): %w",
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

			fs.Infof(w.o, "📤 115网盘单线程上传: 分片%d/%d (%.1f%%) | %v | 已用时:%v | 预计剩余:%v",
				currentPart, params.totalParts, percentage, fs.SizeSuffix(n),
				elapsed.Truncate(time.Second), estimatedRemaining.Truncate(time.Second))
		}
	} else {
		// 未知大小时只在第一个分片输出日志
		if currentPart == 1 {
			fs.Infof(w.o, "📤 115网盘单线程上传: 分片%d | %v | 偏移:%v | 已用时:%v",
				currentPart, fs.SizeSuffix(n), fs.SizeSuffix(off), elapsed.Truncate(time.Second))
		}
	}
}

// logChunkSuccess 记录分片上传成功信息
func (w *ossChunkWriter) logChunkSuccess(partNum, chunkSize int64, chunkStartTime time.Time) {
	currentPart := partNum + 1
	chunkDuration := time.Since(chunkStartTime)

	if chunkSize > 0 {
		speed := float64(chunkSize) / chunkDuration.Seconds() / 1024 / 1024 // MB/s
		fs.Debugf(w.o, " 分片%d上传成功: 实际大小=%v, 用时=%v, 速度=%.2fMB/s",
			currentPart, fs.SizeSuffix(chunkSize), chunkDuration.Truncate(time.Millisecond), speed)
	}
}

// finalizeUpload 完成上传并显示统计信息
func (w *ossChunkWriter) finalizeUpload(ctx context.Context, actualParts int64, startTime time.Time) error {
	totalDuration := time.Since(startTime)

	fs.Infof(w.o, "🔧 开始完成分片上传: 总分片数=%d, 总用时=%v", actualParts, totalDuration.Truncate(time.Second))

	err := w.Close(ctx)
	if err != nil {
		return fmt.Errorf("完成分片上传失败: %w", err)
	}

	// 显示上传完成统计信息
	if w.size > 0 && totalDuration.Seconds() > 0 {
		avgSpeed := float64(w.size) / totalDuration.Seconds() / 1024 / 1024 // MB/s
		fs.Infof(w.o, "115 multipart upload completed")
		fs.Infof(w.o, "Upload stats: size=%v, parts=%d, duration=%v, speed=%.2fMB/s",
			fs.SizeSuffix(w.size), actualParts, totalDuration.Truncate(time.Second), avgSpeed)
	} else {
		fs.Infof(w.o, "115 multipart upload completed: parts=%d, duration=%v",
			actualParts, totalDuration.Truncate(time.Second))
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
			fs.Logf(f, "流式上传使用分片大小 %v，最大文件大小限制为 %v",
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
	if f.isFromRemoteSource(in) {
		fs.Debugf(o, "Cross-cloud transfer detected, using internal two-step transfer to avoid 10002 error")
		fs.Debugf(o, "Step 1: Download to local, Step 2: Upload to 115 from local")

		// 执行内部两步传输
		return f.internalTwoStepTransfer(ctx, src, in, o, ui, size, options...)
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
	fs.Debugf(o, "🚀 分片上传使用优化OSS客户端")

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
	fs.Debugf(w.f, "🚀 分片上传初始化：绕过QPS限制")
	// 直接调用，不使用调速器
	w.imur, err = w.client.InitiateMultipartUpload(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("create multipart upload failed: %w", err)
	}
	w.callback, w.callbackVar = ui.GetCallback(), ui.GetCallbackVar()
	fs.Debugf(w.o, "115网盘分片上传初始化成功: %q", *w.imur.UploadId)
	return
}

// shouldRetry returns a boolean as to whether this err
// deserve to be retried. It returns the err as a convenience
func (w *ossChunkWriter) shouldRetry(ctx context.Context, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	if fserrors.ShouldRetry(err) {
		return true, err
	}

	// Since alibabacloud-oss-go-sdk-v2, oss.ServiceError is wrapped by oss.OperationError
	// so we need to unwrap
	if opErr, ok := err.(*oss.OperationError); ok {
		err = opErr.Unwrap()
	}

	switch ossErr := err.(type) {
	case *oss.ServiceError:
		if ossErr.StatusCode == 403 && (ossErr.Code == "InvalidAccessKeyId" || ossErr.Code == "SecurityTokenExpired") {
			// oss: service returned error: StatusCode=403, ErrorCode=InvalidAccessKeyId,
			// ErrorMessage="The OSS Access Key Id you provided does not exist in our records.",

			// oss: service returned error: StatusCode=403, ErrorCode=SecurityTokenExpired,
			// ErrorMessage="The security token you provided has expired."

			// These errors cannot be handled once token is expired. Should update token proactively.
			return false, fserrors.FatalError(err)
		}
	}
	return false, err
}

// 移除未使用的OSS分片状态验证和缓存方法，简化上传逻辑

// 简化：移除复杂的OSS状态同步逻辑

// 已移除：isOSSNetworkError 函数已本地实现
// 使用统一的OSS网络错误检测机制，提高准确性和一致性

// isOSSRetryableError 判断OSS错误是否可重试
// 增强：基于OSS API文档的错误代码分类
func (w *ossChunkWriter) isOSSRetryableError(err error) bool {
	if err == nil {
		return false
	}

	// 简化网络错误检测
	if err != nil && (strings.Contains(err.Error(), "connection") ||
		strings.Contains(err.Error(), "timeout") ||
		strings.Contains(err.Error(), "network")) {
		return true
	}

	// 检查OSS特定的可重试错误
	if ossErr, ok := err.(*oss.ServiceError); ok {
		switch ossErr.Code {
		case "InternalError", "ServiceUnavailable", "RequestTimeout":
			return true
		case "InvalidAccessKeyId", "SecurityTokenExpired", "SignatureDoesNotMatch":
			return false // 认证错误不可重试
		case "NoSuchUpload":
			return false // 上传会话不存在，不可重试
		}

		// HTTP状态码判断
		switch ossErr.StatusCode {
		case 500, 502, 503, 504: // 服务端错误
			return true
		case 401, 403: // 认证错误
			return false
		}
	}

	return false
}

// uploadChunkWithRetry 带重试的分片上传
// 增强：智能重试机制，提升网络不稳定环境下的成功率
func (w *ossChunkWriter) uploadChunkWithRetry(ctx context.Context, chunkNumber int32, reader io.ReadSeeker) (currentChunkSize int64, err error) {
	maxRetries := 3
	baseDelay := 1 * time.Second // 参考OpenList：使用1秒基础延迟，配合指数退避

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// 重置reader位置
		if attempt > 0 {
			if _, seekErr := reader.Seek(0, io.SeekStart); seekErr != nil {
				return 0, fmt.Errorf("重置reader失败: %w", seekErr)
			}

			// 指数退避延迟
			delay := time.Duration(1<<uint(attempt-1)) * baseDelay
			delay = min(delay, 30*time.Second)

			fs.Debugf(w.o, "分片 %d 重试 %d/%d，延迟 %v", chunkNumber+1, attempt, maxRetries, delay)

			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(delay):
				// 继续重试
			}
		}

		// 尝试上传分片
		currentChunkSize, err = w.WriteChunk(ctx, chunkNumber, reader)
		if err == nil {
			if attempt > 0 {
				fs.Infof(w.o, "✅ 分片 %d 重试成功", chunkNumber+1)
			}
			return currentChunkSize, nil
		}

		// 使用rclone标准错误处理
		if !w.isOSSRetryableError(err) {
			fs.Debugf(w.o, "分片 %d 遇到不可重试错误: %v", chunkNumber+1, err)
			return 0, err
		}

		if attempt == maxRetries {
			fs.Errorf(w.o, "❌ 分片 %d 重试 %d 次后仍失败: %v", chunkNumber+1, maxRetries, err)
			return 0, err
		}

		// 参考OpenList：使用指数退避策略进行默认延迟
		delay := time.Duration(1<<uint(attempt-1)) * baseDelay // 1s, 2s, 4s
		fs.Debugf(w.o, "分片 %d 上传失败，%v后重试 (第%d次重试): %v", chunkNumber+1, delay, attempt, err)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
			// 继续重试
		}
	}

	return 0, err
}

// addCompletedPart 添加已完成的分片到列表
// 单线程模式下按顺序添加，无需复杂的并发控制
func (w *ossChunkWriter) addCompletedPart(part oss.UploadPart) {
	w.uploadedParts = append(w.uploadedParts, part)
}

// WriteChunk will write chunk number with reader bytes, where chunk number >= 0
// 参考阿里云OSS示例优化分片上传逻辑
func (w *ossChunkWriter) WriteChunk(ctx context.Context, chunkNumber int32, reader io.ReadSeeker) (currentChunkSize int64, err error) {
	if chunkNumber < 0 {
		err := fmt.Errorf("无效的分片编号: %v", chunkNumber)
		return -1, err
	}

	ossPartNumber := chunkNumber + 1
	var res *oss.UploadPartResult
	chunkStartTime := time.Now() // 记录分片上传开始时间

	// 激进性能优化：OSS分片上传完全绕过QPS限制
	// OSS UploadPart直连阿里云，不受115 API QPS限制，应该以最大速度上传
	fs.Debugf(w.o, "🚀 OSS分片直连模式：完全绕过QPS限制，最大化上传速度")

	// 获取分片大小
	currentChunkSize, err = reader.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("获取分片%d大小失败: %w", ossPartNumber, err)
	}

	// 重置读取位置
	_, err = reader.Seek(0, io.SeekStart)
	if err != nil {
		return 0, fmt.Errorf("重置分片%d读取位置失败: %w", ossPartNumber, err)
	}

	// 参考阿里云OSS示例：创建UploadPart请求
	fs.Debugf(w.o, " 开始上传分片%d到OSS: 大小=%v", ossPartNumber, fs.SizeSuffix(currentChunkSize))

	// 直接调用OSS UploadPart，不使用任何调速器
	res, err = w.client.UploadPart(ctx, &oss.UploadPartRequest{
		Bucket:     oss.Ptr(*w.imur.Bucket),
		Key:        oss.Ptr(*w.imur.Key),
		UploadId:   w.imur.UploadId,
		PartNumber: ossPartNumber,
		Body:       reader,
		// 参考阿里云OSS示例：添加进度回调（虽然在分片级别，但可以提供更细粒度的反馈）
	})

	// 简化错误处理：直接处理OSS上传结果
	if err != nil {
		fs.Debugf(w.o, "❌ 分片%d上传失败: %v", ossPartNumber, err)
		return 0, fmt.Errorf("分片%d上传失败 (大小:%v): %w", ossPartNumber, fs.SizeSuffix(currentChunkSize), err)
	}

	// 记录成功信息
	chunkDuration := time.Since(chunkStartTime)
	if currentChunkSize > 0 && chunkDuration.Seconds() > 0 {
		speed := float64(currentChunkSize) / chunkDuration.Seconds() / 1024 / 1024 // MB/s
		fs.Debugf(w.o, " 分片%d OSS上传成功: 大小=%v, 用时=%v, 速度=%.2fMB/s, ETag=%s",
			ossPartNumber, fs.SizeSuffix(currentChunkSize),
			chunkDuration.Truncate(time.Millisecond), speed, *res.ETag)
	}

	// 记录已完成的分片
	part := oss.UploadPart{
		PartNumber: ossPartNumber,
		ETag:       res.ETag,
	}
	w.addCompletedPart(part)

	// 最终成功日志
	totalDuration := time.Since(chunkStartTime)
	fs.Debugf(w.o, " 分片%d完成: 大小=%v, 总用时=%v",
		ossPartNumber, fs.SizeSuffix(currentChunkSize), totalDuration.Truncate(time.Millisecond))

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

	// 激进性能优化：OSS CompleteMultipartUpload完全绕过QPS限制
	// OSS完成分片上传直连阿里云，不受115 API QPS限制
	fs.Debugf(w.o, "🚀 OSS完成分片上传：完全绕过QPS限制")

	res, err = w.client.CompleteMultipartUpload(ctx, req)

	// 简化重试逻辑：只在网络错误时重试一次
	if err != nil {
		shouldRetry, _ := w.shouldRetry(ctx, err)
		if shouldRetry {
			fs.Debugf(w.o, "🔄 OSS完成分片上传重试: %v", err)
			res, err = w.client.CompleteMultipartUpload(ctx, req)
		}
	}
	if err != nil {
		return fmt.Errorf("完成分片上传失败: %w", err)
	}

	w.callbackRes = res.CallbackResult
	fs.Infof(w.o, "115网盘分片上传完成: %q", *w.imur.UploadId)
	return
}

// ============================================================================
// Functions from mod.go
// ============================================================================

// ------------------------------------------------------------
// Modifications and Helper Functions
// ------------------------------------------------------------

// 移除未使用的parseRootID函数

// getCacheStatistics115 获取缓存统计信息
func (f *Fs) getCacheStatistics115(_ context.Context) (any, error) {
	result := map[string]any{
		"backend": "115",
		"caches":  make(map[string]any),
	}

	// 缓存功能已移除，使用rclone标准dircache
	result["message"] = "缓存功能已移除，使用rclone标准dircache"

	return result, nil
}

// getDownloadURLCommand 通过pick_code或.strm内容获取下载URL
func (f *Fs) getDownloadURLCommand(ctx context.Context, args []string, opt map[string]string) (any, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("需要提供pick_code、115://pick_code格式或文件路径")
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
		fs.Debugf(f, "✅ 解析.strm格式: pick_code=%s", pickCode)
	} else {
		// 假设是纯pick_code
		pickCode = input
		fs.Debugf(f, "✅ 使用纯pick_code: %s", pickCode)
	}

	// 验证pick_code格式
	if pickCode == "" {
		return nil, fmt.Errorf("无效的pick_code: %s", input)
	}

	// 使用原始HTTP实现获取下载URL
	fs.Debugf(f, "🌐 使用原始HTTP方式获取下载URL: pick_code=%s", pickCode)
	downloadURL, err := f.getDownloadURLByPickCodeHTTP(ctx, pickCode, opt["user-agent"])
	if err != nil {
		return nil, fmt.Errorf("获取下载URL失败: %w", err)
	}

	fs.Infof(f, "✅ 成功获取115网盘下载URL: pick_code=%s", pickCode)

	// 返回下载URL字符串（与getdownloadurlua保持一致）
	return downloadURL, nil
}

// getDownloadURLByPickCodeHTTP 使用rclone标准方式通过pick_code获取下载URL
func (f *Fs) getDownloadURLByPickCodeHTTP(ctx context.Context, pickCode string, userAgent string) (string, error) {
	// 如果没有提供 UA，使用默认值
	if userAgent == "" {
		userAgent = defaultUserAgent
	}

	fs.Debugf(f, "🌐 使用rclone标准方式获取下载URL: pick_code=%s, UA=%s", pickCode, userAgent)

	// 使用rclone标准rest客户端
	opts := rest.Opts{
		Method: "POST",
		Path:   "/open/ufile/downurl",
		Body:   strings.NewReader("pick_code=" + pickCode),
		ExtraHeaders: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
			"User-Agent":   userAgent,
		},
	}

	// 准备认证信息
	f.prepareTokenForRequest(ctx, &opts)

	// 发送请求并处理响应
	res, err := f.openAPIClient.Call(ctx, &opts)
	if err != nil {
		fs.Errorf(f, "请求失败: %v", err)
		return "", err
	}
	defer res.Body.Close()

	// 解析响应 - 使用原始代码中的响应结构
	var response struct {
		State   bool   `json:"state"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    map[string]struct {
			URL struct {
				URL string `json:"url"`
			} `json:"url"`
		} `json:"data"`
	}

	if decodeErr := json.NewDecoder(res.Body).Decode(&response); decodeErr != nil {
		fs.Errorf(f, "解析响应失败: %v", decodeErr)
		return "", decodeErr
	}

	for _, downInfo := range response.Data {
		if downInfo.URL.URL != "" {
			fs.Debugf(f, "✅ 获取到下载URL: %s", downInfo.URL.URL)
			return downInfo.URL.URL, nil
		}
	}
	return "", fmt.Errorf("未找到下载URL")
}

// internalTwoStepTransfer 内部两步传输：先完整下载到本地，再从本地上传
// 修复10002错误：彻底解决跨云传输SHA1不一致问题
func (f *Fs) internalTwoStepTransfer(ctx context.Context, src fs.ObjectInfo, in io.Reader, o *Object, ui *UploadInitInfo, size int64, options ...fs.OpenOption) (*ossChunkWriter, error) {
	fs.Infof(o, "🌐 开始内部两步传输: %s (%s)", src.Remote(), fs.SizeSuffix(size))

	// 第一步：完整下载到本地临时文件
	fs.Infof(o, "📥 第一步：下载到本地临时文件...")

	// 创建临时文件
	tempFile, err := os.CreateTemp("", "rclone_two_step_*.tmp")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tempPath := tempFile.Name()

	// 确保清理临时文件
	defer func() {
		tempFile.Close()
		os.Remove(tempPath)
		fs.Debugf(o, "🧹 清理临时文件: %s", tempPath)
	}()

	// 使用传入的Reader进行完整下载
	// 注意：in已经是打开的Reader，直接使用
	fs.Infof(o, "📥 正在下载: %s → %s", src.Remote(), tempPath)
	written, err := io.Copy(tempFile, in)
	if err != nil {
		return nil, fmt.Errorf("下载到临时文件失败: %w", err)
	}

	// 验证下载大小
	if written != size {
		return nil, fmt.Errorf("下载大小不匹配: 期望%d, 实际%d", size, written)
	}

	fs.Infof(o, "✅ 第一步完成：下载 %s", fs.SizeSuffix(written))

	// 重置文件指针到开头
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("重置文件指针失败: %w", err)
	}

	// 第二步：计算本地文件的SHA1
	fs.Infof(o, "🔢 计算本地文件SHA1...")
	hasher := sha1.New()
	_, err = io.Copy(hasher, tempFile)
	if err != nil {
		return nil, fmt.Errorf("计算SHA1失败: %w", err)
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
	newUI, err := f.initUploadOpenAPI(ctx, size, o.remote, "", sha1sum, "", "", "", "")
	if err != nil {
		return nil, fmt.Errorf("重新初始化上传失败: %w", err)
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
		return nil, fmt.Errorf("创建OSS客户端失败: %w", err)
	}

	// 使用本地文件创建标准的分片写入器
	return f.newChunkWriterWithClient(ctx, src, newUI, tempFile, o, ossClient, options...)
}

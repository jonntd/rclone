package _115

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"github.com/cenkalti/backoff/v4"
	"github.com/rclone/rclone/backend/115/api"
	"github.com/rclone/rclone/backend/115/cipher"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/rest"
)

// Globals
const (
	cachePrefix  = "rclone-115-sha1sum-"
	md5Salt      = "Qclm8MGWUv59TnrR0XPg"
	OSSRegion    = "cn-shenzhen" // https://uplb.115.com/3.0/getuploadinfo.php
	OSSUserAgent = "aliyun-sdk-android/2.9.1"
)

func remote(o *Object) string {
	return o.fs.root + o.remote
}

// getUploadBasicInfo retrieves basic upload information (userID, userkey, etc.).
func (f *Fs) getUploadBasicInfo(ctx context.Context) error {
	opts := rest.Opts{
		Method:  "GET",
		RootURL: "https://proapi.115.com/app/uploadinfo",
	}
	var info *api.UploadBasicInfo

	err := f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return err
	}
	if !info.State {
		return fmt.Errorf("API Error: %s (%d)", info.Error, info.Errno)
	}
	userID := info.UserID.String()
	if userID == "0" {
		return errors.New("invalid user id")
	}
	f.userID = userID
	f.userkey = info.Userkey
	return nil
}

// bufferIO handles buffering of input streams based on size thresholds.
func bufferIO(in io.Reader, size, threshold int64) (out io.Reader, cleanup func(), err error) {
	cleanup = func() {}
	if size > threshold {
		tempFile, errF := os.CreateTemp("", cachePrefix)
		if errF != nil {
			return nil, cleanup, errF
		}
		// Attempt to remove right away (on non-Windows).
		_ = os.Remove(tempFile.Name())

		cleanup = func() {
			_ = tempFile.Close()
			_ = os.Remove(tempFile.Name())
		}
		if _, errC := io.Copy(tempFile, in); errC != nil {
			return nil, cleanup, errC
		}
		if _, errS := tempFile.Seek(0, io.SeekStart); errS != nil {
			return nil, cleanup, errS
		}
		out = tempFile
	} else {
		inData, errR := io.ReadAll(in)
		if errR != nil {
			return nil, cleanup, errR
		}
		out = bytes.NewReader(inData)
	}
	return out, cleanup, nil
}

// bufferIOwithSHA1 buffers the input and calculates its SHA-1
func bufferIOwithSHA1(in io.Reader, size, threshold int64) (sha1sum string, out io.Reader, cleanup func(), err error) {
	hashVal := sha1.New()
	tee := io.TeeReader(in, hashVal)

	out, cleanup, err = bufferIO(tee, size, threshold)
	if err != nil {
		return "", nil, cleanup, err
	}
	sha1sum = hex.EncodeToString(hashVal.Sum(nil))
	return sha1sum, out, cleanup, nil
}

// generateSignature for 115 initupload
func generateSignature(userID, fileID, target, userKey string) string {
	sum1 := sha1.Sum([]byte(userID + fileID + target + "0"))
	sigStr := userKey + hex.EncodeToString(sum1[:]) + "000000"
	sum2 := sha1.Sum([]byte(sigStr))
	return strings.ToUpper(hex.EncodeToString(sum2[:]))
}

// generateToken for 115 initupload
func generateToken(userID, fileID, fileSize, signKey, signVal, timeStamp, appVer string) string {
	userIDMd5 := md5.Sum([]byte(userID))
	tokenMd5 := md5.Sum([]byte(md5Salt + fileID + fileSize + signKey + signVal + userID + timeStamp + hex.EncodeToString(userIDMd5[:]) + appVer))
	return hex.EncodeToString(tokenMd5[:])
}

// initUpload calls 115's initupload endpoint. This is used for both 秒传 checks and actual uploads.
func (f *Fs) initUpload(ctx context.Context, size int64, name, dirID, sha1sum, signKey, signVal string) (*api.UploadInitInfo, error) {
	operation := func() (*api.UploadInitInfo, error) {
		filename := f.opt.Enc.FromStandardName(name)
		filesize := strconv.FormatInt(size, 10)
		fileID := strings.ToUpper(sha1sum) // if sha1 not known, pass ""
		target := "U_1_" + dirID           // 115 style
		ts := time.Now().UnixMilli()
		t := strconv.FormatInt(ts, 10)

		ecdhCipher, err := cipher.NewEcdhCipher()
		if err != nil {
			return nil, err
		}
		encodedToken, err := ecdhCipher.EncodeToken(ts)
		if err != nil {
			return nil, err
		}

		form := url.Values{}
		form.Set("appid", "0")
		form.Set("appversion", f.appVer)
		form.Set("userid", f.userID)
		form.Set("filename", filename)
		form.Set("filesize", filesize)
		form.Set("fileid", fileID) // uppercase
		form.Set("target", target)
		form.Set("sig", generateSignature(f.userID, fileID, target, f.userkey))
		form.Set("t", t)
		form.Set("token", generateToken(f.userID, fileID, filesize, signKey, signVal, t, f.appVer))
		if signKey != "" && signVal != "" {
			form.Set("sign_key", signKey)
			form.Set("sign_val", signVal)
		}
		encryptedBody, err := ecdhCipher.Encrypt([]byte(form.Encode()))
		if err != nil {
			return nil, err
		}

		opts := rest.Opts{
			Method:      "POST",
			RootURL:     "https://uplb.115.com/4.0/initupload.php",
			ContentType: "application/x-www-form-urlencoded",
			Parameters:  url.Values{"k_ec": {encodedToken}},
			Body:        bytes.NewReader(encryptedBody),
		}
		var resp *http.Response
		err = f.pacer.Call(func() (bool, error) {
			resp, err = f.srv.Call(ctx, &opts)
			return shouldRetry(ctx, resp, nil, err)
		})
		if err != nil {
			return nil, err
		}

		// Ensure resp is not nil before proceeding
		if resp == nil {
			return nil, errors.New("initUpload: received nil response without error")
		}

		body, err := rest.ReadBody(resp)
		if err != nil {
			return nil, err
		}
		decrypted, err := ecdhCipher.Decrypt(body)
		if err != nil {
			return nil, err
		}
		var info api.UploadInitInfo
		if err = json.Unmarshal(decrypted, &info); err != nil {
			return nil, err
		}
		if info.ErrorCode != 0 && info.ErrorCode != 701 {
			return nil, fmt.Errorf("%s (%d)", info.ErrorMsg, info.ErrorCode)
		}
		return &info, nil
	}

	var ui *api.UploadInitInfo
	operationWithRetry := func() (*api.UploadInitInfo, error) {
		expBackoff := backoff.NewExponentialBackOff()
		expBackoff.MaxElapsedTime = 2 * time.Minute
		err := backoff.RetryNotify(func() error {
			var err error
			ui, err = operation()
			if err != nil {
				return err
			}
			return nil
		}, expBackoff, func(err error, duration time.Duration) {
			fs.Logf(nil, "initUpload failed: %v. Retrying in %v", err, duration)
		})
		return ui, err
	}

	ui, err := operationWithRetry()
	if err != nil {
		return nil, err
	}
	return ui, nil
}

// postUpload processes the JSON callback after an upload to OSS or sample upload.
func (f *Fs) postUpload(v map[string]any) (*api.CallbackData, error) {
	callbackJson, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var info api.CallbackInfo
	if err := json.Unmarshal(callbackJson, &info); err != nil {
		return nil, err
	}
	if !info.State {
		return nil, fmt.Errorf("API Error: %s (%d)", info.Message, info.Code)
	}
	return info.Data, nil
}

// getOSSToken to build dynamic credentials
func (f *Fs) getOSSToken(ctx context.Context) (*api.OSSToken, error) {
	opts := rest.Opts{
		Method:  "GET",
		RootURL: "https://uplb.115.com/3.0/gettoken.php",
	}
	var info *api.OSSToken
	err := f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return nil, err
	}
	if info.StatusCode != "200" {
		return nil, fmt.Errorf("failed to get OSS token: %s (%s)", info.ErrorMessage, info.ErrorCode)
	}
	return info, nil
}

// newOSSClient builds an OSS client with dynamic credentials that auto-refresh.
func (f *Fs) newOSSClient() *oss.Client {
	fetcher := credentials.CredentialsFetcherFunc(func(ctx context.Context) (credentials.Credentials, error) {
		t, err := f.getOSSToken(ctx)
		if err != nil {
			return credentials.Credentials{}, err
		}
		return credentials.Credentials{
			AccessKeyID:     t.AccessKeyID,
			AccessKeySecret: t.AccessKeySecret,
			SecurityToken:   t.SecurityToken,
			Expires:         &t.Expiration,
		}, nil
	})
	provider := credentials.NewCredentialsFetcherProvider(fetcher)
	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(provider).
		WithRegion(OSSRegion).
		WithUserAgent(OSSUserAgent)
	return oss.NewClient(cfg)
}

// unWrapObjectInfo attempts to unwrap the underlying fs.Object
func unWrapObjectInfo(oi fs.ObjectInfo) fs.Object {
	if o, ok := oi.(fs.Object); ok {
		return fs.UnWrapObject(o)
	} else if do, ok := oi.(*fs.OverrideRemote); ok {
		return do.UnWrap()
	}
	return nil
}

// calcBlockSHA1 calculates SHA-1 for a specified range from a source.
func calcBlockSHA1(ctx context.Context, in io.Reader, src fs.ObjectInfo, rangeSpec string) (string, error) {
	var start, end int64
	if _, err := fmt.Sscanf(rangeSpec, "%d-%d", &start, &end); err != nil {
		return "", err
	}

	var reader io.Reader
	if ra, ok := in.(io.ReaderAt); ok {
		reader = io.NewSectionReader(ra, start, end-start+1)
	} else if srcObj := unWrapObjectInfo(src); srcObj != nil {
		rc, err := srcObj.Open(ctx, &fs.RangeOption{Start: start, End: end})
		if err != nil {
			return "", fmt.Errorf("failed to open source: %w", err)
		}
		defer fs.CheckClose(rc, &err)
		reader = rc
	} else {
		return "", fmt.Errorf("failed to get reader for range from source %s", src)
	}

	hashVal := sha1.New()
	if _, err := io.Copy(hashVal, reader); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(hashVal.Sum(nil))), nil
}

// sampleInitUpload prepares a "simple form" upload (for smaller files).
func (f *Fs) sampleInitUpload(ctx context.Context, size int64, name, dirID string) (*api.SampleInitResp, error) {
	form := url.Values{}
	form.Set("userid", f.userID)
	form.Set("filename", f.opt.Enc.FromStandardName(name))
	form.Set("filesize", strconv.FormatInt(size, 10))
	form.Set("target", "U_1_"+dirID)

	opts := rest.Opts{
		Method:      "POST",
		RootURL:     "https://uplb.115.com/3.0/sampleinitupload.php",
		ContentType: "application/x-www-form-urlencoded",
		Body:        strings.NewReader(form.Encode()),
	}
	var info *api.SampleInitResp
	err := f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return nil, fmt.Errorf("sampleInitUpload error: %w", err)
	}
	if info.ErrorCode != 0 {
		return nil, fmt.Errorf("sampleInitUpload error: %s (%d)", info.Error, info.ErrorCode)
	}
	return info, nil
}

// sampleUploadForm uses multipart form to upload smaller files to OSS in one shot.
func (f *Fs) sampleUploadForm(ctx context.Context, in io.Reader, initResp *api.SampleInitResp, name string, size int64, options ...fs.OpenOption) (*api.CallbackData, error) {
	pipeReader, pipeWriter := io.Pipe()
	errChan := make(chan error, 1)
	multipartWriter := multipart.NewWriter(pipeWriter)

	go func() {
		defer func() {
			if err := multipartWriter.Close(); err != nil {
				errChan <- fmt.Errorf("failed to close multipart writer: %w", err)
				return
			}
			if err := pipeWriter.Close(); err != nil {
				errChan <- fmt.Errorf("failed to close pipe writer: %w", err)
				return
			}
			errChan <- nil
		}()

		fields := map[string]string{
			"name":                  name,
			"key":                   initResp.Object,
			"policy":                initResp.Policy,
			"OSSAccessKeyId":        initResp.AccessID,
			"success_action_status": "200",
			"callback":              initResp.Callback,
			"signature":             initResp.Signature,
		}
		for k, v := range fields {
			if err := multipartWriter.WriteField(k, v); err != nil {
				errChan <- fmt.Errorf("failed to write field %s: %w", k, err)
				return
			}
		}

		// Additional headers from options
		for _, opt := range options {
			k, v := opt.Header()
			switch strings.ToLower(k) {
			case "cache-control", "content-disposition", "content-encoding", "content-type":
				if err := multipartWriter.WriteField(k, v); err != nil {
					errChan <- fmt.Errorf("failed to write field %s: %w", k, err)
					return
				}
			}
		}

		filePart, err := multipartWriter.CreateFormFile("file", name)
		if err != nil {
			errChan <- fmt.Errorf("failed to create form file: %w", err)
			return
		}
		if _, err := io.Copy(filePart, in); err != nil {
			errChan <- fmt.Errorf("failed to copy file data: %w", err)
			return
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "POST", initResp.Host, pipeReader)
	if err != nil {
		_ = pipeWriter.CloseWithError(err) // avoid goroutine leak
		return nil, fmt.Errorf("failed to build upload request: %w", err)
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())

	resp, err := f.srv.client().Do(req)
	if err != nil {
		_ = pipeWriter.CloseWithError(err)
		return nil, fmt.Errorf("post form error: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	if werr := <-errChan; werr != nil {
		return nil, werr
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("simple upload error: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var respMap map[string]any
	if err := json.Unmarshal(respBody, &respMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return f.postUpload(respMap)
}

// ─────────────────────────────────────────────────────────────────────────────────────
//                          NEWLY REFACTORED FUNCTIONS
// ─────────────────────────────────────────────────────────────────────────────────────

// tryHashUpload attempts 秒传 by checking if the file already exists on the server.
// Returns (found, UploadInitInfo, error)
func (f *Fs) tryHashUpload(
	ctx context.Context,
	in io.Reader,
	src fs.ObjectInfo,
	o *Object,
	leaf, dirID string,
	size int64,
) (bool, *api.UploadInitInfo, io.Reader, error) {
	fs.Debugf(o, "tryHashUpload: attempting 秒传...")

	// 1) Get or compute the file's SHA-1
	hashStr, err := src.Hash(ctx, hash.SHA1)
	var cleanup func()
	if err != nil || hashStr == "" {
		fs.Debugf(o, "tryHashUpload: computing SHA1 locally...")
		hashStr, in, cleanup, err = bufferIOwithSHA1(in, size, int64(f.opt.HashMemoryThreshold))
		defer cleanup()
		if err != nil {
			return false, nil, nil, fmt.Errorf("failed to calculate SHA1: %w", err)
		}
	} else {
		fs.Debugf(o, "tryHashUpload: using precomputed SHA1=%s", hashStr)
	}
	o.sha1sum = strings.ToLower(hashStr)

	// 2) Call initUpload with that SHA-1
	ui, err := f.initUpload(ctx, size, leaf, dirID, hashStr, "", "")
	if err != nil {
		return false, nil, nil, fmt.Errorf("秒传 initUpload failed: %w", err)
	}

	// 3) Handle different statuses
	signKey, signVal := "", ""
	for {
		switch ui.Status {
		case 2:
			// status=2 => server found a match => no upload needed
			fs.Debugf(o, "秒传 success => setting metadata")
			// Mark accounting as server-side copy if needed
			if acc, ok := in.(*accounting.Account); ok && acc != nil {
				acc.ServerSideTransferStart()
				acc.ServerSideCopyEnd(size)
			}
			// Optionally fetch final info
			if info, err2 := f.getFile(ctx, "", ui.PickCode); err2 == nil {
				_ = o.setMetaData(info)
			}
			return true, ui, in, nil

		case 1:
			// status=1 => server doesn't have file => need actual upload
			fs.Debugf(o, "tryHashUpload: 秒传 not possible => server requires real upload.")
			return false, ui, in, nil

		case 7:
			// partial-block check
			fs.Debugf(o, "tryHashUpload: 秒传 partial-block check => signCheck=%q", ui.SignCheck)
			signKey = ui.SignKey
			if signVal, err = calcBlockSHA1(ctx, in, src, ui.SignCheck); err != nil {
				return false, nil, nil, fmt.Errorf("calcBlockSHA1 error: %w", err)
			}
			ui, err = f.initUpload(ctx, size, leaf, dirID, hashStr, signKey, signVal)
			if err != nil {
				return false, nil, nil, fmt.Errorf("tryHashUpload: 秒传 re-init error: %w", err)
			}
			continue

		default:
			// Unexpected status => treat as error
			return false, nil, nil, fmt.Errorf("tryHashUpload: 秒传 error: unexpected status=%d", ui.Status)
		}
	}
}

// uploadToOSS performs the actual upload to OSS using the provided UploadInitInfo.
func (f *Fs) uploadToOSS(
	ctx context.Context,
	in io.Reader,
	src fs.ObjectInfo,
	o *Object,
	leaf, dirID string,
	size int64,
	ui *api.UploadInitInfo, // Existing parameter
	options ...fs.OpenOption,
) (fs.Object, error) {
	// Check if ui is nil and retry fetching UploadInitInfo if necessary
	if ui == nil {
		return nil, fmt.Errorf("uploadToOSS: UploadInitInfo is nil")
	}

	// Define the upload operation
	operation := func() error {
		// Decide between single-part and multipart uploads
		cutoff := int64(o.fs.opt.UploadCutoff)
		if size < cutoff {
			// Single-part upload
			fs.Debugf(o, "uploadToOSS: single-part => size=%d < cutoff=%d", size, cutoff)
			client := f.newOSSClient()
			req := &oss.PutObjectRequest{
				Bucket:      oss.Ptr(ui.Bucket),
				Key:         oss.Ptr(ui.Object),
				Body:        in,
				Callback:    oss.Ptr(ui.GetCallback()),
				CallbackVar: oss.Ptr(ui.GetCallbackVar()),
			}
			// Apply options
			for _, opt := range options {
				k, v := opt.Header()
				switch strings.ToLower(k) {
				case "cache-control":
					req.CacheControl = oss.Ptr(v)
				case "content-disposition":
					req.ContentDisposition = oss.Ptr(v)
				case "content-encoding":
					req.ContentEncoding = oss.Ptr(v)
				case "content-type":
					req.ContentType = oss.Ptr(v)
				}
			}
			res, err := client.PutObject(ctx, req)
			if err != nil {
				return fmt.Errorf("uploadToOSS: single-part putObject error: %w", err)
			}
			data, err := f.postUpload(res.CallbackResult)
			if err != nil {
				return fmt.Errorf("uploadToOSS: single-part finalize error: %w", err)
			}
			if err = o.setMetaDataFromCallBack(data); err != nil {
				return fmt.Errorf("uploadToOSS: setting metadata error: %w", err)
			}
			return nil
		}

		// Multipart upload
		fs.Debugf(o, "uploadToOSS: starting multipart for size=%d >= cutoff=%d", size, cutoff)
		mu, err := f.newChunkWriter(ctx, remote(o), src, ui, in, options...)
		if err != nil {
			return fmt.Errorf("uploadToOSS: multipart init error: %w", err)
		}
		if err = mu.Upload(ctx); err != nil {
			// Handle context cancellation
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return backoff.Permanent(err)
			}

			// Check if the error is PartAlreadyExist
			var ossErr *oss.ServiceError
			if errors.As(err, &ossErr) && ossErr.Code == "PartAlreadyExist" {
				fs.Debugf(o, "uploadToOSS: part already exists, skipping upload")
				return nil // Skip re-uploading this part
			}
			return fmt.Errorf("uploadToOSS: multipart upload error: %w", err)
		}
		data, err := f.postUpload(mu.callbackRes)
		if err != nil {
			return fmt.Errorf("uploadToOSS: multipart finalize error: %w", err)
		}
		if err = o.setMetaDataFromCallBack(data); err != nil {
			return fmt.Errorf("uploadToOSS: setting metadata error: %w", err)
		}
		return nil
	}

	// Execute the upload operation with retries
	expBackoff := backoff.NewExponentialBackOff()
	expBackoff.MaxElapsedTime = 2 * time.Minute
	err := backoff.RetryNotify(operation, expBackoff, func(err error, duration time.Duration) {
		fs.Logf(o, "uploadToOSS failed: %v. Retrying in %v", err, duration)
	})
	if err != nil {
		return nil, err
	}

	return o, nil
}

// doSampleUpload is a helper to perform the "simple form" approach for small files.
func (f *Fs) doSampleUpload(
	ctx context.Context,
	in io.Reader,
	o *Object,
	leaf, dirID string,
	size int64,
	options ...fs.OpenOption,
) (fs.Object, error) {
	fs.Debugf(o, "doSampleUpload: simple form upload for size=%d", size)
	initResp, err := f.sampleInitUpload(ctx, size, leaf, dirID)
	if err != nil {
		return nil, fmt.Errorf("doSampleUpload init error: %w", err)
	}
	callbackData, err := f.sampleUploadForm(ctx, in, initResp, leaf, size, options...)
	if err != nil {
		return nil, fmt.Errorf("doSampleUpload form error: %w", err)
	}
	return o, o.setMetaDataFromCallBack(callbackData)
}

// upload is the main entry point that decides which upload strategy to use.
func (f *Fs) upload(
	ctx context.Context,
	in io.Reader,
	src fs.ObjectInfo,
	remote string,
	options ...fs.OpenOption,
) (fs.Object, error) {
	if f.isShare {
		return nil, errors.New("unsupported for shared filesystem")
	}
	size := src.Size()

	// Ensure we have userID/userkey
	if f.userkey == "" {
		if err := f.getUploadBasicInfo(ctx); err != nil {
			return nil, fmt.Errorf("failed to get upload basic info: %w", err)
		}
		if f.userID == "" || f.userkey == "" {
			return nil, fmt.Errorf("empty userid or userkey")
		}
	}

	if size > int64(maxUploadSize) {
		return nil, fmt.Errorf("file size exceeds upload limit: %d > %d", size, maxUploadSize)
	}

	o, leaf, dirID, err := f.createObject(ctx, remote, src.ModTime(ctx), size)
	if err != nil {
		return nil, err
	}

	//----------------------------------------------------------------
	// 1) OnlyStream
	//----------------------------------------------------------------
	if f.opt.OnlyStream {
		if size <= int64(StreamUploadLimit) {
			obj, err := f.doSampleUpload(ctx, in, o, leaf, dirID, size, options...)
			if err != nil {
				return nil, err
			}
			return obj, nil
		}
		return nil, fserrors.NoRetryError(fmt.Errorf("OnlyStream is enabled but file size %d exceeds StreamUploadLimit %d",
			size, StreamUploadLimit))
	}

	//----------------------------------------------------------------
	// 2) FastUpload
	//----------------------------------------------------------------
	if f.opt.FastUpload {
		noHashSize := int64(f.opt.NohashSize)
		if size <= noHashSize {
			// Tiny files use sample upload
			obj, err := f.doSampleUpload(ctx, in, o, leaf, dirID, size, options...)
			if err != nil {
				return nil, err
			}
			return obj, nil
		}
		// Attempt fast upload (秒传)
		gotIt, ui, newIn, err := f.tryHashUpload(ctx, in, src, o, leaf, dirID, size)
		if err != nil {
			return nil, fmt.Errorf("FastUpload: 秒传 error: %w", err)
		}
		if gotIt {
			return o, nil
		}
		// Fallback to uploadToOSS using the obtained UploadInitInfo
		if ui != nil {
			if size <= int64(StreamUploadLimit) {
				obj, err := f.doSampleUpload(ctx, newIn, o, leaf, dirID, size, options...)
				if err != nil {
					return nil, err
				}
				return obj, nil
			}
			obj, err := f.uploadToOSS(ctx, newIn, src, o, leaf, dirID, size, ui, options...)
			if err != nil {
				return nil, err
			}
			return obj, nil
		}
		// If ui is nil, fallback to standard upload
		if size <= int64(StreamUploadLimit) {
			obj, err := f.doSampleUpload(ctx, newIn, o, leaf, dirID, size, options...)
			if err != nil {
				return nil, err
			}
			return obj, nil
		}
		obj, err := f.uploadToOSS(ctx, newIn, src, o, leaf, dirID, size, nil, options...)
		if err != nil {
			return nil, err
		}
		return obj, nil
	}

	//----------------------------------------------------------------
	// 3) UploadHashOnly
	//----------------------------------------------------------------
	if f.opt.UploadHashOnly {
		hashStr, _ := src.Hash(ctx, hash.SHA1)
		if hashStr == "" {
			return nil, fserrors.NoRetryError(errors.New("UploadHashOnly: skipping since no SHA1"))
		}
		gotIt, _, _, err := f.tryHashUpload(ctx, in, src, o, leaf, dirID, size)
		if err != nil {
			return nil, err
		}
		if gotIt {
			return o, nil
		}
		// No fallback for UploadHashOnly
		return nil, fserrors.NoRetryError(errors.New("UploadHashOnly: server does not have file => skipping"))
	}

	//----------------------------------------------------------------
	// 4) Normal logic: if file < nohash_size => sample; else 秒传 => fallback to upload
	//----------------------------------------------------------------
	if size >= 0 && size < int64(f.opt.NohashSize) && !f.opt.UploadHashOnly {
		// Use sample upload for small files
		obj, err := f.doSampleUpload(ctx, in, o, leaf, dirID, size, options...)
		if err != nil {
			return nil, err
		}
		return obj, nil
	}

	// Attempt fast upload (秒传)
	gotIt, ui, newIn, err := f.tryHashUpload(ctx, in, src, o, leaf, dirID, size)
	if err != nil {
		fs.Debugf(o, "normal: 秒传 error => fallback to uploadToOSS: %v", err)
		obj, uploadErr := f.uploadToOSS(ctx, newIn, src, o, leaf, dirID, size, ui, options...)
		if uploadErr != nil {
			return nil, uploadErr
		}
		return obj, nil
	}
	if gotIt {
		// Fast upload successful
		return o, nil
	}
	// Fallback to actual upload to OSS
	obj, err := f.uploadToOSS(ctx, newIn, src, o, leaf, dirID, size, ui, options...)
	if err != nil {
		return nil, err
	}
	return obj, nil
}

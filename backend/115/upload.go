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

// getUploadBasicInfo retrieves basic upload information from the API
func (f *Fs) getUploadBasicInfo(ctx context.Context) (err error) {
	opts := rest.Opts{
		Method:  "GET",
		RootURL: "https://proapi.115.com/app/uploadinfo",
	}
	var info *api.UploadBasicInfo
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return
	} else if !info.State {
		return fmt.Errorf("API Error: %s (%d)", info.Error, info.Errno)
	}
	userID := info.UserID.String()
	if userID == "0" {
		return errors.New("invalid user id")
	}
	f.userID = userID
	f.userkey = info.Userkey
	return
}

// bufferIO handles buffering of input streams based on size thresholds
func bufferIO(in io.Reader, size, threshold int64) (out io.Reader, cleanup func(), err error) {
	// nothing to clean up by default
	cleanup = func() {}

	// don't cache small files on disk to reduce wear of the disk
	if size > threshold {
		var tempFile *os.File

		// create the cache file
		tempFile, err = os.CreateTemp("", cachePrefix)
		if err != nil {
			return
		}

		_ = os.Remove(tempFile.Name()) // Delete the file - may not work on Windows

		// clean up the file after we are done downloading
		cleanup = func() {
			// the file should normally already be close, but just to make sure
			_ = tempFile.Close()
			_ = os.Remove(tempFile.Name()) // delete the cache file after we are done - may be deleted already
		}

		// copy the ENTIRE file to disc and calculate the SHA1 in the process
		if _, err = io.Copy(tempFile, in); err != nil {
			return
		}
		// jump to the start of the local file so we can pass it along
		if _, err = tempFile.Seek(0, io.SeekStart); err != nil {
			return
		}

		// replace the already read source with a reader of our cached file
		out = tempFile
	} else {
		// that's a small file, just read it into memory
		var inData []byte
		inData, err = io.ReadAll(in)
		if err != nil {
			return
		}

		// set the reader to our read memory block
		out = bytes.NewReader(inData)
	}
	return out, cleanup, nil
}

// bufferIOwithSHA1 buffers the input and calculates its SHA1
func bufferIOwithSHA1(in io.Reader, size, threshold int64) (sha1sum string, out io.Reader, cleanup func(), err error) { // we need an SHA1
	hash := sha1.New()
	// use the tee to write to the local file AND calculate the SHA1 while doing so
	tee := io.TeeReader(in, hash)
	out, cleanup, err = bufferIO(tee, size, threshold)
	if err != nil {
		return
	}
	sha1sum = hex.EncodeToString(hash.Sum(nil))
	return
}

// generateSignature creates a signature for the upload
func generateSignature(userID, fileID, target, userKey string) string {
	sha1sum := sha1.Sum([]byte(userID + fileID + target + "0"))
	sigStr := userKey + hex.EncodeToString(sha1sum[:]) + "000000"
	sh1Sig := sha1.Sum([]byte(sigStr))
	return strings.ToUpper(hex.EncodeToString(sh1Sig[:]))
}

// generateToken creates a token for the upload
func generateToken(userID, fileID, fileSize, signKey, signVal, timeStamp, appVer string) string {
	userIDMd5 := md5.Sum([]byte(userID))
	tokenMd5 := md5.Sum([]byte(md5Salt + fileID + fileSize + signKey + signVal + userID + timeStamp + hex.EncodeToString(userIDMd5[:]) + appVer))
	return hex.EncodeToString(tokenMd5[:])
}

// initUpload initializes a chunked upload
func (f *Fs) initUpload(ctx context.Context, size int64, name, dirID, sha1sum, signKey, signVal string) (info *api.UploadInitInfo, err error) {
	var (
		filename     = f.opt.Enc.FromStandardName(name)
		filesize     = strconv.FormatInt(size, 10)
		fileID       = strings.ToUpper(sha1sum)
		target       = "U_1_" + dirID // target id
		ts           = time.Now().UnixMilli()
		t            = strconv.FormatInt(ts, 10)
		ecdhCipher   *cipher.EcdhCipher
		encodedToken string
		encrypted    []byte
		decrypted    []byte
	)

	if ecdhCipher, err = cipher.NewEcdhCipher(); err != nil {
		return
	}

	// url parameter
	if encodedToken, err = ecdhCipher.EncodeToken(ts); err != nil {
		return
	}

	// form that will be encrypted
	form := url.Values{}
	form.Set("appid", "0")
	form.Set("appversion", f.appVer)
	form.Set("userid", f.userID)
	form.Set("filename", filename)
	form.Set("filesize", filesize)
	form.Set("fileid", fileID)
	form.Set("target", target)
	form.Set("sig", generateSignature(f.userID, fileID, target, f.userkey))
	form.Set("t", t)
	form.Set("token", generateToken(f.userID, fileID, filesize, signKey, signVal, t, f.appVer))
	if signKey != "" && signVal != "" {
		form.Set("sign_key", signKey)
		form.Set("sign_val", signVal)
	}
	if encrypted, err = ecdhCipher.Encrypt([]byte(form.Encode())); err != nil {
		return
	}

	opts := rest.Opts{
		Method:      "POST",
		RootURL:     "https://uplb.115.com/4.0/initupload.php",
		ContentType: "application/x-www-form-urlencoded",
		Parameters:  url.Values{"k_ec": {encodedToken}},
		Body:        bytes.NewReader(encrypted),
	}
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.Call(ctx, &opts)
		return shouldRetry(ctx, resp, nil, err)
	})
	if err != nil {
		return
	}
	body, err := rest.ReadBody(resp)
	if err != nil {
		return
	}
	if decrypted, err = ecdhCipher.Decrypt(body); err != nil {
		// FIXME failed to decrypt intermittenly
		// seems to be caused by corrupted body
		// low level retry doesn't help
		return
	}
	if err = json.Unmarshal(decrypted, &info); err != nil {
		return
	}
	switch info.ErrorCode {
	case 0:
		return
	case 701: // when status == 7
		return
	default:
		return nil, fmt.Errorf("%s (%d)", info.ErrorMsg, info.ErrorCode)
	}
}

// postUpload processes the callback data after upload
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

// getOSSToken retrieves OSS token information
func (f *Fs) getOSSToken(ctx context.Context) (info *api.OSSToken, err error) {
	opts := rest.Opts{
		Method:  "GET",
		RootURL: "https://uplb.115.com/3.0/gettoken.php", // https://uplb.115.com/3.0/getuploadinfo.php
	}
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err == nil && info.StatusCode != "200" {
		return nil, fmt.Errorf("failed to get OSS token: %s (%s)", info.ErrorMessage, info.ErrorCode)
	}
	return
}

// newOSSClient creates a new OSS client with dynamic credentials
func (f *Fs) newOSSClient() (client *oss.Client) {
	fetcher := credentials.CredentialsFetcherFunc(func(ctx context.Context) (credentials.Credentials, error) {
		t, err := f.getOSSToken(ctx)
		if err != nil {
			return credentials.Credentials{}, err
		}
		return credentials.Credentials{
			AccessKeyID:     t.AccessKeyID,
			AccessKeySecret: t.AccessKeySecret,
			SecurityToken:   t.SecurityToken,
			Expires:         &t.Expiration}, nil
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
		// Unwrap if it is an operations.OverrideRemote
		return do.UnWrap()
	}
	return nil
}

// calcBlockSHA1 calculates the SHA1 for a specified range
func calcBlockSHA1(ctx context.Context, in io.Reader, src fs.ObjectInfo, rangeSpec string) (sha1sum string, err error) {
	var start, end int64
	if _, err = fmt.Sscanf(rangeSpec, "%d-%d", &start, &end); err != nil {
		return
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
		return "", fmt.Errorf("failed to get reader from source %s", src)
	}

	hash := sha1.New()
	if _, err = io.Copy(hash, reader); err == nil {
		sha1sum = strings.ToUpper(hex.EncodeToString(hash.Sum(nil)))
	}
	return
}

// sampleInitUpload initiates the simple upload process for small files
func (f *Fs) sampleInitUpload(ctx context.Context, size int64, name, dirID string) (info *api.SampleInitResp, err error) {
	form := url.Values{}
	form.Set("userid", f.userID)
	form.Set("filename", f.opt.Enc.FromStandardName(name))
	form.Set("filesize", strconv.FormatInt(size, 10))
	form.Set("target", "U_1_"+dirID) // same style as normal “target”

	opts := rest.Opts{
		Method:      "POST",
		RootURL:     "https://uplb.115.com/3.0/sampleinitupload.php",
		ContentType: "application/x-www-form-urlencoded",
		Body:        strings.NewReader(form.Encode()),
	}
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return nil, fmt.Errorf("sampleInitUpload call error: %w", err)
	}
	// If server returns an error code, handle it
	if info.ErrorCode != 0 {
		return nil, fmt.Errorf("sampleInitUpload error: %s (%d)", info.Error, info.ErrorCode)
	}
	return info, nil
}

// sampleUploadForm performs the multipart form upload to OSS using streaming to limit memory usage
func (f *Fs) sampleUploadForm(ctx context.Context, in io.Reader, initResp *api.SampleInitResp, name string, size int64, options ...fs.OpenOption) (*api.CallbackData, error) {
	// Create a pipe for streaming multipart data
	pipeReader, pipeWriter := io.Pipe()
	// Channel to capture any errors from the writer goroutine
	errChan := make(chan error, 1)

	// Create a multipart writer that writes to the pipe writer
	multipartWriter := multipart.NewWriter(pipeWriter)

	// Start a goroutine to write the multipart form data
	go func() {
		// Ensure that both the multipart writer and pipe writer are closed properly
		defer func() {
			// Close the multipart writer and send any errors to errChan
			if err := multipartWriter.Close(); err != nil {
				errChan <- fmt.Errorf("failed to close multipart writer: %w", err)
				return
			}
			// Close the pipe writer and send any errors to errChan
			if err := pipeWriter.Close(); err != nil {
				errChan <- fmt.Errorf("failed to close pipe writer: %w", err)
				return
			}
			// If everything is successful, send nil to indicate no errors
			errChan <- nil
		}()

		// Add normal form fields
		fields := map[string]string{
			"name":                  name,
			"key":                   initResp.Object,
			"policy":                initResp.Policy,
			"OSSAccessKeyId":        initResp.AccessID,
			"success_action_status": "200",
			"callback":              initResp.Callback,
			"signature":             initResp.Signature,
		}

		for key, value := range fields {
			if err := multipartWriter.WriteField(key, value); err != nil {
				errChan <- fmt.Errorf("failed to write field %s: %w", key, err)
				return
			}
		}

		// Apply additional upload options (e.g., headers like Cache-Control)
		for _, option := range options {
			key, value := option.Header()
			switch strings.ToLower(key) {
			case "cache-control", "content-disposition", "content-encoding", "content-type":
				if err := multipartWriter.WriteField(key, value); err != nil {
					errChan <- fmt.Errorf("failed to write field %s: %w", key, err)
					return
				}
			}
		}

		// Add the actual file part
		filePart, err := multipartWriter.CreateFormFile("file", name)
		if err != nil {
			errChan <- fmt.Errorf("failed to create form file: %w", err)
			return
		}

		// Stream the file content directly to the multipart writer
		if _, err := io.Copy(filePart, in); err != nil {
			errChan <- fmt.Errorf("failed to copy file data: %w", err)
			return
		}
	}()

	// Build the HTTP request with the pipe reader as the body
	req, err := http.NewRequestWithContext(ctx, "POST", initResp.Host, pipeReader)
	if err != nil {
		return nil, fmt.Errorf("failed to build upload request: %w", err)
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())

	// Perform the HTTP request
	resp, err := f.srv.client().Do(req)
	if err != nil {
		// Ensure that the pipe writer is closed to avoid goroutine leaks
		_ = pipeWriter.CloseWithError(err)
		return nil, fmt.Errorf("post form error: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	// Wait for the writer goroutine to finish and check for errors
	writeErr := <-errChan
	if writeErr != nil {
		return nil, writeErr
	}

	// Read the response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("simple upload error: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// **New Code to Unmarshal respBody into a map before passing to postUpload**
	var respMap map[string]any
	if err := json.Unmarshal(respBody, &respMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response body: %w", err)
	}

	// The response is the JSON callback from 115
	// Parse it using postUpload
	return f.postUpload(respMap)
}

// tryHashUpload tries the hash-based upload.
// If successful, it returns nil. If it fails, return an error so the caller can fallback to something else.
func (f *Fs) tryHashUpload(ctx context.Context, in io.Reader, src fs.ObjectInfo, o *Object, leaf, dirID string, size int64, options ...fs.OpenOption) error {
	// 1) Check if src already has a SHA1
	hashStr, err := src.Hash(ctx, hash.SHA1)
	if err != nil || hashStr == "" {
		// If no SHA1, let's attempt to compute it ourselves.
		// (Or you might choose to bail out – up to you.)
		fs.Debugf(o, "tryHashUpload: Computing SHA1 locally...")
		var cleanup func()
		hashStr, in, cleanup, err = bufferIOwithSHA1(in, size, int64(f.opt.HashMemoryThreshold))
		defer cleanup()
		if err != nil {
			return fmt.Errorf("failed to calculate SHA1: %w", err)
		}
	} else {
		fs.Debugf(o, "tryHashUpload: Using precomputed SHA1=%s", hashStr)
	}

	// 2) Do the actual “hash-based” init + single- or multi- part logic.
	//    If it fails, return an error.
	_, err = f.doHashBasedUpload(ctx, in, src, o, leaf, dirID, size, hashStr, options...)
	return err
}

// doHashBasedUpload does your existing hashed initialization and upload flow.
func (f *Fs) doHashBasedUpload(ctx context.Context, in io.Reader, src fs.ObjectInfo, o *Object,
	leaf, dirID string, size int64, knownSha1 string, options ...fs.OpenOption) (fs.Object, error) {

	//----------------------------------------------------------------
	// 1) initUpload with knownSha1 (or compute if empty).
	//----------------------------------------------------------------
	hashStr := knownSha1
	if hashStr == "" {
		// compute if not provided
		fs.Debugf(o, "doHashBasedUpload: no knownSha1 => computing...")
		var cleanup func()
		var err error
		hashStr, in, cleanup, err = bufferIOwithSHA1(in, size, int64(f.opt.HashMemoryThreshold))
		defer cleanup()
		if err != nil {
			return nil, fmt.Errorf("failed to calculate SHA1: %w", err)
		}
	}
	o.sha1sum = strings.ToLower(hashStr)

	ui, err := f.initUpload(ctx, size, leaf, dirID, hashStr, "", "")
	if err != nil {
		return o, fmt.Errorf("failed to init upload: %w", err)
	}

	//----------------------------------------------------------------
	// 2) Possibly handle repeated status=7 block checks
	//    (the exact logic you have for repeated init calls).
	//----------------------------------------------------------------
	signKey, signVal := "", ""
	for retry := true; retry; {
		retry = ui.Status == 7
		switch ui.Status {
		case 1:
			fs.Debugf(o, "Hash-based: Ready to upload to OSS (status=1)")
		case 2:
			fs.Debugf(o, "Hash-based: Upload finished instantly (status=2). No traffic!")
			if acc, ok := in.(*accounting.Account); ok && acc != nil {
				acc.ServerSideTransferStart()
				acc.ServerSideCopyEnd(size)
			}
			if info, err2 := f.getFile(ctx, "", ui.PickCode); err2 == nil {
				return o, o.setMetaData(info)
			}
			return o, nil
		case 7:
			// do the partial block hash check
			signKey = ui.SignKey
			if signVal, err = calcBlockSHA1(ctx, in, src, ui.SignCheck); err != nil {
				return nil, fmt.Errorf("failed to calculate block hash: %w", err)
			}
			fs.Debugf(o, "Retrying init upload: Status 7 with signKey=%q signVal=%q", signKey, signVal)

			// re-init with updated signKey, signVal
			ui, err = f.initUpload(ctx, size, leaf, dirID, hashStr, signKey, signVal)
			if err != nil {
				return nil, fmt.Errorf("failed to init upload: %w", err)
			}
		default:
			return nil, fmt.Errorf("unexpected status: %#v", ui)
		}
	}

	//----------------------------------------------------------------
	// 3) If size < cutoff => single part. Else => chunked/multipart.
	//----------------------------------------------------------------
	if size < 0 || size >= int64(o.fs.opt.UploadCutoff) {
		// chunked
		mu, err := f.newChunkWriter(ctx, remote(o), src, ui, in, options...)
		if err != nil {
			return nil, fmt.Errorf("multipart upload init error: %w", err)
		}
		if err = mu.Upload(ctx); err != nil {
			return nil, err
		}
		data, err := f.postUpload(mu.callbackRes)
		if err != nil {
			return nil, fmt.Errorf("multipart finalize error: %w", err)
		}
		return o, o.setMetaDataFromCallBack(data)
	}

	// single part
	client := f.newOSSClient()
	req := &oss.PutObjectRequest{
		Bucket:      oss.Ptr(ui.Bucket),
		Key:         oss.Ptr(ui.Object),
		Body:        in,
		Callback:    oss.Ptr(ui.GetCallback()),
		CallbackVar: oss.Ptr(ui.GetCallbackVar()),
	}
	// Apply upload options
	for _, option := range options {
		key, value := option.Header()
		switch strings.ToLower(key) {
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

	res, err := client.PutObject(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("single-part putObject error: %w", err)
	}
	data, err := f.postUpload(res.CallbackResult)
	if err != nil {
		return nil, fmt.Errorf("single-part finalize error: %w", err)
	}
	return o, o.setMetaDataFromCallBack(data)
}

// doMultipartUpload is a smaller helper to unify chunked upload logic
func (f *Fs) doMultipartUpload(
	ctx context.Context,
	in io.Reader,
	src fs.ObjectInfo,
	o *Object,
	leaf, dirID string,
	size int64,
	options ...fs.OpenOption,
) (fs.Object, error) {
	fs.Debugf(o, "Performing chunked/multipart upload for size=%d", size)
	ui, err := f.initUpload(ctx, size, leaf, dirID, "", "", "") // no hash check here
	if err != nil {
		return nil, fmt.Errorf("multipart init error: %w", err)
	}
	mu, err := f.newChunkWriter(ctx, remote(o), src, ui, in, options...)
	if err != nil {
		return nil, fmt.Errorf("multipart upload failed to initialise: %w", err)
	}
	if err = mu.Upload(ctx); err != nil {
		return nil, err
	}
	data, err := f.postUpload(mu.callbackRes)
	if err != nil {
		return nil, fmt.Errorf("multipart finalize error: %w", err)
	}
	return o, o.setMetaDataFromCallBack(data)
}

// remote is just a helper to get the full path or object name if needed.
func remote(o *Object) string {
	return o.fs.root + o.remote
}

// upload uploads the object with or without using a temporary file name
func (f *Fs) upload(ctx context.Context, in io.Reader, src fs.ObjectInfo, remote string, options ...fs.OpenOption) (fs.Object, error) {
	if f.isShare {
		return nil, errors.New("unsupported for shared filesystem")
	}
	size := src.Size()

	// Ensure upload basics
	if f.userkey == "" {
		if err := f.getUploadBasicInfo(ctx); err != nil {
			return nil, fmt.Errorf("failed to get upload basic info: %w", err)
		}
		if f.userID == "" || f.userkey == "" {
			return nil, fmt.Errorf("empty userid or userkey")
		}
	}

	// If file exceeds the maximum upload size, bail out immediately (same as original).
	if size > maxUploadSize {
		return nil, fmt.Errorf("file size exceeds the upload limit: %d > %d", size, int64(maxUploadSize))
	}

	// Create the object (and parent directory if needed).
	o, leaf, dirID, err := f.createObject(ctx, remote, src.ModTime(ctx), size)
	if err != nil {
		return nil, err
	}

	//----------------------------------------------------------------
	// 1) If OnlyStream is set:
	//
	//    - If file size <= StreamUploadLimit => sample upload.
	//    - If file size above => bail out.
	//----------------------------------------------------------------
	if f.opt.OnlyStream {
		if size <= int64(StreamUploadLimit) {
			fs.Debugf(o, "OnlyStream: Using simple sample upload mode (<= StreamUploadLimit).")
			initResp, err := f.sampleInitUpload(ctx, size, leaf, dirID)
			if err != nil {
				return o, fmt.Errorf("simple init upload error: %w", err)
			}
			callbackData, err := f.sampleUploadForm(ctx, in, initResp, leaf, size, options...)
			if err != nil {
				return o, fmt.Errorf("simple upload form error: %w", err)
			}
			// Set metadata from callback.
			return o, o.setMetaDataFromCallBack(callbackData)
		} else {
			return nil, fmt.Errorf("OnlyStream is enabled but file size %d exceeds StreamUploadLimit %d",
				size, StreamUploadLimit)
		}
	}

	//----------------------------------------------------------------
	// 2) If FastUpload is set:
	//
	//    - If size <= nohash_size => use sample upload right away.
	//    - If size > nohash_size => try hash-based upload:
	//         * If initUpload or block check fails:
	//              - if nohash_size < size < StreamUploadLimit => sample upload
	//              - else if StreamUploadLimit <= size < maxUploadSize => do chunked/multipart upload
	//              - else => bail out
	//----------------------------------------------------------------
	if f.opt.FastUpload {
		noHashSize := int64(f.opt.NohashSize) // define how/where you store this
		if size <= noHashSize {
			fs.Debugf(o, "FastUpload: size <= nohash_size => simple sample upload.")
			initResp, err := f.sampleInitUpload(ctx, size, leaf, dirID)
			if err != nil {
				return o, fmt.Errorf("simple init upload error: %w", err)
			}
			callbackData, err := f.sampleUploadForm(ctx, in, initResp, leaf, size, options...)
			if err != nil {
				return o, fmt.Errorf("simple upload form error: %w", err)
			}
			return o, o.setMetaDataFromCallBack(callbackData)
		}

		// Otherwise, attempt hash-based logic below in a helper
		hashUpErr := f.tryHashUpload(ctx, in, src, o, leaf, dirID, size, options...)
		if hashUpErr == nil {
			// Hash upload succeeded
			return o, nil
		}

		// If the hash-based upload fails, fall back to sample/multipart if possible:
		fs.Debugf(o, "FastUpload: hash-based upload failed => fallback logic.")
		if size <= int64(StreamUploadLimit) && size > noHashSize {
			// fallback to simple sample
			fs.Debugf(o, "Fallback to sample upload: nohash_size < size <= StreamUploadLimit")
			initResp, err := f.sampleInitUpload(ctx, size, leaf, dirID)
			if err != nil {
				return o, fmt.Errorf("sample init upload error: %w", err)
			}
			callbackData, err := f.sampleUploadForm(ctx, in, initResp, leaf, size, options...)
			if err != nil {
				return o, fmt.Errorf("sample upload form error: %w", err)
			}
			return o, o.setMetaDataFromCallBack(callbackData)
		} else if size < maxUploadSize {
			// chunked/multipart upload
			fs.Debugf(o, "Fallback to multipart upload: size=%d < maxUploadSize", size)
			return f.doMultipartUpload(ctx, in, src, o, leaf, dirID, size, options...)
		} else {
			return nil, fmt.Errorf("FastUpload fallback: file size %d exceeds maximum upload limit", size)
		}
	}

	//----------------------------------------------------------------
	// 3) If UploadHashOnly is set:
	//
	//    - Maintain original logic: if there's no precomputed SHA1 => skip.
	//    - Otherwise do hash-based upload only, no sample fallback.
	//----------------------------------------------------------------
	if f.opt.UploadHashOnly {
		fs.Debugf(o, "UploadHashOnly: Checking if src has a SHA1.")
		hashStr, err := src.Hash(ctx, hash.SHA1)
		if err != nil || hashStr == "" {
			return nil, fserrors.NoRetryError(errors.New("UploadHashOnly: skipping as src has no SHA1"))
		}
		// If we do have the SHA1, run the existing hash-based upload.
		fs.Debugf(o, "UploadHashOnly: src has SHA1=%s -> continuing hash-based upload", hashStr)
		return f.doHashBasedUpload(ctx, in, src, o, leaf, dirID, size, hashStr, options...)
	}

	//----------------------------------------------------------------
	// 4) If none of them are set (the original logic):
	//
	//    - "Simple / sample upload if under NohashSize and not forcing SHA1,
	//       otherwise do the existing “hash-based” logic"
	//----------------------------------------------------------------
	if size >= 0 && size < int64(f.opt.NohashSize) && !f.opt.UploadHashOnly {
		fs.Debugf(o, "Normal: Using simple sample upload mode for small file.")
		initResp, err := f.sampleInitUpload(ctx, size, leaf, dirID)
		if err != nil {
			return o, fmt.Errorf("simple init upload error: %w", err)
		}
		callbackData, err := f.sampleUploadForm(ctx, in, initResp, leaf, size, options...)
		if err != nil {
			return o, fmt.Errorf("simple upload form error: %w", err)
		}
		return o, o.setMetaDataFromCallBack(callbackData)
	}

	// Otherwise do the existing “hash-based” logic
	fs.Debugf(o, "Normal: Doing existing hash-based logic (size >= nohash_size).")
	return f.doHashBasedUpload(ctx, in, src, o, leaf, dirID, size, "", options...)
}

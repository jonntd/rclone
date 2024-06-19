package _115

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rclone/rclone/backend/115/api"
	"github.com/rclone/rclone/backend/115/crypto"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/rest"
)

const (
	ListLimit = 100
)

// list the objects into the function supplied
//
// If directories is set it only sends directories
// User function to process a File item from listAll
//
// Should return true to finish processing
type listAllFn func(*api.File) bool

// Lists the directory required calling the user function on each item found
//
// If the user fn ever returns true then it early exits with found = true
func (f *Fs) listAll(ctx context.Context, dirID string, fn listAllFn) (found bool, err error) {
	// Url Parameters
	params := url.Values{}
	params.Set("aid", "1")
	params.Set("cid", dirID)
	params.Set("o", "user_ptime") // order by time or "file_type", "file_size", "file_name"
	params.Set("asc", "0")        // ascending order? "0" or "1"
	params.Set("show_dir", "1")   // "0" or "1"
	params.Set("limit", strconv.Itoa(ListLimit))
	params.Set("snap", "0")
	params.Set("record_open_time", "1")
	params.Set("count_folders", "1")
	params.Set("format", "json")
	params.Set("fc_mix", "0")

	opts := rest.Opts{
		Method:     "GET",
		Path:       "/files",
		Parameters: params,
	}

	offset := 0
OUTER:
	for {
		params.Set("offset", strconv.Itoa(offset))

		var info api.FileList
		var resp *http.Response
		err = f.pacer.Call(func() (bool, error) {
			resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
			return shouldRetry(ctx, resp, &info, err)
		})
		if err != nil {
			return found, fmt.Errorf("couldn't list files: %w", err)
		} else if !info.State {
			return found, fmt.Errorf("API State false: %q (%d)", info.Error, info.ErrNo)
		}
		if len(info.Files) == 0 {
			break
		}
		for _, item := range info.Files {
			item.Name = f.opt.Enc.ToStandardName(item.Name)
			if fn(item) {
				found = true
				break OUTER
			}
		}
		offset = info.Offset + ListLimit
		if offset >= info.Count {
			break
		}
	}
	return
}

func (f *Fs) makeDir(ctx context.Context, pid, name string) (info *api.NewDir, err error) {
	form := url.Values{}
	form.Set("pid", pid)
	form.Set("cname", f.opt.Enc.FromStandardName(name))

	opts := rest.Opts{
		Method:          "POST",
		Path:            "/files/add",
		MultipartParams: form,
	}

	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return
	} else if !info.State {
		if info.Errno == 20004 {
			return nil, fs.ErrorDirExists
		}
		return nil, fmt.Errorf("API State false: %s (%d)", info.Error, info.Errno)
	}
	return
}

func (f *Fs) renameFile(ctx context.Context, fid, newName string) (err error) {
	form := url.Values{}
	form.Set("fid", fid)
	form.Set("file_name", newName)
	form.Set(fmt.Sprintf("files_new_name[%s]", fid), newName)

	opts := rest.Opts{
		Method:          "POST",
		Path:            "/files/batch_rename",
		MultipartParams: form,
	}
	var info *api.Base
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return
	} else if !info.State {
		return fmt.Errorf("API State false: %s (%d)", info.Error, info.Errno)
	}
	return
}

func (f *Fs) deleteFiles(ctx context.Context, fids []string) (err error) {
	form := url.Values{}
	for i, fid := range fids {
		form.Set(fmt.Sprintf("fid[%d]", i), fid)
	}

	opts := rest.Opts{
		Method:          "POST",
		Path:            "/rb/delete",
		MultipartParams: form,
	}
	var info *api.Base
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return
	} else if !info.State {
		return fmt.Errorf("API State false: %s (%d)", info.Error, info.Errno)
	}
	return
}

func (f *Fs) moveFiles(ctx context.Context, fids []string, pid string) (err error) {
	form := url.Values{}
	for i, fid := range fids {
		form.Set(fmt.Sprintf("fid[%d]", i), fid)
	}
	form.Set("pid", pid)

	opts := rest.Opts{
		Method:          "POST",
		Path:            "/files/move",
		MultipartParams: form,
	}

	var info *api.Base
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return
	} else if !info.State {
		return fmt.Errorf("API State false: %s (%d)", info.Error, info.Errno)
	}
	return
}

func (f *Fs) copyFiles(ctx context.Context, fids []string, pid string) (err error) {
	form := url.Values{}
	for i, fid := range fids {
		form.Set(fmt.Sprintf("fid[%d]", i), fid)
	}
	form.Set("pid", pid)

	opts := rest.Opts{
		Method:          "POST",
		Path:            "/files/copy",
		MultipartParams: form,
	}

	var info *api.Base
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return
	} else if !info.State {
		return fmt.Errorf("API State false: %s (%d)", info.Error, info.Errno)
	}
	return
}

func (f *Fs) indexInfo(ctx context.Context) (data *api.IndexInfo, err error) {
	opts := rest.Opts{
		Method: "GET",
		Path:   "/files/index_info",
	}

	var info *api.Base
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return
	} else if !info.State {
		return nil, fmt.Errorf("API State false: %s (%d)", info.Error, info.Errno)
	}
	if data = info.Data.IndexInfo; data == nil {
		return nil, errors.New("no data")
	}
	return
}

func (f *Fs) _getDownloadURL(ctx context.Context, input []byte) (output []byte, cookies []*http.Cookie, err error) {
	key := crypto.GenerateKey()
	t := strconv.Itoa(int(time.Now().Unix()))
	opts := rest.Opts{
		Method:          "POST",
		RootURL:         "https://proapi.115.com/app/chrome/downurl",
		Parameters:      url.Values{"t": {t}},
		MultipartParams: url.Values{"data": {crypto.Encode(input, key)}},
	}
	var info *api.Base
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return
	} else if !info.State {
		return nil, nil, fmt.Errorf("API State false: %s (%d)", info.Error, info.Errno)
	}
	if info.Data.EncodedData == "" {
		return nil, nil, errors.New("no data")
	}
	output, err = crypto.Decode(info.Data.EncodedData, key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode data: %w", err)
	}
	cookies = append(cookies, resp.Cookies()...)         // including uid, cid, and seid
	cookies = append(cookies, resp.Request.Cookies()...) // including access key value pari with Max-Age=900
	return
}

func (f *Fs) getDownloadURL(ctx context.Context, pickCode string) (durl *api.DownloadURL, err error) {
	// pickCode -> data -> reqData
	input, _ := json.Marshal(map[string]string{"pickcode": pickCode})
	output, cookies, err := f._getDownloadURL(ctx, input)
	if err != nil {
		return
	}
	downData := api.DownloadData{}
	if err := json.Unmarshal(output, &downData); err != nil {
		return nil, fmt.Errorf("failed to json.Unmarshal %q", string(output))
	}

	for _, downInfo := range downData {
		durl = &downInfo.URL
		durl.Cookies = cookies
		durl.CreateTime = time.Now()
		return
	}
	return nil, fs.ErrorObjectNotFound
}

func (f *Fs) getDirID(ctx context.Context, remoteDir string) (cid string, err error) {
	params := url.Values{}
	params.Set("path", f.opt.Enc.FromStandardPath(remoteDir))
	opts := rest.Opts{
		Method:     "GET",
		Path:       "/files/getid",
		Parameters: params,
	}

	var info *api.DirID
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return
	} else if !info.State {
		return "", fmt.Errorf("API State false: %s (%d)", info.Error, info.Errno)
	}
	cid = info.ID.String()
	if cid == "0" && remoteDir != "/" {
		return "", fs.ErrorDirNotFound
	}
	return
}

// getFile gets information of a file or directory by its ID
func (f *Fs) getFile(ctx context.Context, fid string) (file *api.File, err error) {
	params := url.Values{}
	params.Set("file_id", fid)
	opts := rest.Opts{
		Method:     "GET",
		Path:       "/files/get_info",
		Parameters: params,
	}

	var info *api.FileInfo
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return
	} else if !info.State {
		return nil, fmt.Errorf("API State false: %s (%d)", info.Message, info.Code)
	}
	if len(info.Data) > 0 {
		file = info.Data[0]
		file.Name = f.opt.Enc.ToStandardName(file.Name)
		return
	}
	return nil, fmt.Errorf("no data")
}

// getStats gets information of a file or directory by its ID
func (f *Fs) getStats(ctx context.Context, cid string) (info *api.FileStats, err error) {
	params := url.Values{}
	params.Set("cid", cid)
	opts := rest.Opts{
		Method:     "GET",
		Path:       "/category/get",
		Parameters: params,
	}

	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return
	}
	info.FileName = f.opt.Enc.ToStandardName(info.FileName)
	for n, parent := range info.Paths {
		info.Paths[n].FileName = f.opt.Enc.ToStandardName(parent.FileName)
	}
	return
}

// ------------------------------------------------------------

// add offline download task for multiple urls
func (f *Fs) _addURLs(ctx context.Context, input []byte) (output []byte, err error) {
	key := crypto.GenerateKey()
	opts := rest.Opts{
		Method:          "POST",
		RootURL:         "https://lixian.115.com/lixianssp/",
		Parameters:      url.Values{"ac": {"add_task_urls"}},
		MultipartParams: url.Values{"data": {crypto.Encode(input, key)}},
	}
	var info *api.Base
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(ctx, &opts, nil, &info)
		return shouldRetry(ctx, resp, info, err)
	})
	if err != nil {
		return
	}
	if info.Data.EncodedData == "" {
		return nil, errors.New("no data")
	}
	output, err = crypto.Decode(info.Data.EncodedData, key)
	if err != nil {
		return nil, fmt.Errorf("failed to decode data: %w", err)
	}
	return
}

// add offline download task for multiple urls
func (f *Fs) addURLs(ctx context.Context, path string, urls []string) (info *api.NewURL, err error) {
	if f.userID == "" {
		if err := f.getUploadBasicInfo(ctx); err != nil {
			return nil, fmt.Errorf("failed to get user id: %w", err)
		}
	}
	parentID, _ := f.dirCache.FindDir(ctx, path, false)
	payload := map[string]string{
		"ac":         "add_task_urls",
		"app_ver":    appVer,
		"uid":        f.userID,
		"wp_path_id": parentID,
	}
	for ind, url := range urls {
		payload[fmt.Sprintf("url[%d]", ind)] = url
	}
	input, _ := json.Marshal(payload)
	output, err := f._addURLs(ctx, input)
	if err != nil {
		return
	}
	if err = json.Unmarshal(output, &info); err != nil {
		return nil, fmt.Errorf("failed to json.Unmarshal %q", string(output))
	}
	return
}
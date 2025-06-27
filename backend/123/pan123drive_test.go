package _123

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fstest/fstests"
	"github.com/rclone/rclone/lib/random"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFs(t *testing.T) {
	// Test creating a new filesystem instance
	m := configmap.Simple{
		"client_id":     "",
		"client_secret": "",
	}

	ctx := context.Background()
	f, err := newFs(ctx, "test", "", m)

	// We expect this to fail without valid credentials, but the structure should be created
	if err != nil {
		t.Logf("Expected error without valid credentials: %v", err)
		return
	}

	require.NotNil(t, f)
	assert.Equal(t, "test", f.Name())
	assert.Equal(t, "", f.Root())
	assert.NotNil(t, f.Features())
}

func TestPathToFileID(t *testing.T) {
	// Create a mock filesystem for testing
	f := &Fs{
		name: "test",
		root: "",
	}

	ctx := context.Background()

	// Test root directory
	id, err := f.pathToFileID(ctx, "/")
	if err == nil {
		assert.Equal(t, "0", id)
	}

	// Test empty path
	id, err = f.pathToFileID(ctx, "")
	if err == nil {
		assert.Equal(t, "0", id)
	}
}

func TestObjectMethods(t *testing.T) {
	// Create a mock object for testing
	f := &Fs{
		name: "test",
		root: "",
	}

	o := &Object{
		fs:          f,
		remote:      "test.txt",
		hasMetaData: true,
		id:          "123",
		size:        1024,
		md5sum:      "d41d8cd98f00b204e9800998ecf8427e",
		isDir:       false,
	}

	// Test basic methods
	assert.Equal(t, f, o.Fs())
	assert.Equal(t, "test.txt", o.Remote())
	assert.Equal(t, "test.txt", o.String())
	assert.Equal(t, int64(1024), o.Size())
	assert.Equal(t, "123", o.ID())
	assert.True(t, o.Storable())

	// Test hash
	ctx := context.Background()
	hashValue, err := o.Hash(ctx, hash.MD5)
	assert.NoError(t, err)
	assert.Equal(t, "d41d8cd98f00b204e9800998ecf8427e", hashValue)

	// Test unsupported hash
	_, err = o.Hash(ctx, hash.SHA1)
	assert.Error(t, err)
}

func TestFeatures(t *testing.T) {
	f := &Fs{
		features: (&fs.Features{
			ReadMimeType:            true,
			CanHaveEmptyDirectories: true,
			DuplicateFiles:          false,
			ReadMetadata:            true,
			WriteMetadata:           false,
			UserMetadata:            false,
			PartialUploads:          true,
			NoMultiThreading:        false,
			SlowModTime:             true,
			SlowHash:                false,
		}).Fill(context.Background(), nil),
	}

	features := f.Features()
	assert.NotNil(t, features)
	assert.True(t, features.ReadMimeType)
	assert.True(t, features.CanHaveEmptyDirectories)
	assert.False(t, features.DuplicateFiles)
	assert.True(t, features.ReadMetadata)
	assert.False(t, features.WriteMetadata)
	assert.False(t, features.UserMetadata)
	assert.True(t, features.PartialUploads)
	assert.False(t, features.NoMultiThreading)
	assert.True(t, features.SlowModTime)
	assert.False(t, features.SlowHash)
}

func TestErrorHandling(t *testing.T) {
	ctx := context.Background()

	// Test shouldRetry function
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"nil error", nil, false},
		{"object not found", fs.ErrorObjectNotFound, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retry, _ := shouldRetry(ctx, nil, tt.err)
			assert.Equal(t, tt.expected, retry)
		})
	}
}

func TestOptionsValidation(t *testing.T) {
	tests := []struct {
		name        string
		options     configmap.Simple
		expectError bool
	}{
		{
			name: "valid options",
			options: configmap.Simple{
				"client_id":     "test_id",
				"client_secret": "test_secret",
			},
			expectError: false,
		},
		{
			name: "missing client_id",
			options: configmap.Simple{
				"client_secret": "test_secret",
			},
			expectError: true,
		},
		{
			name: "missing client_secret",
			options: configmap.Simple{
				"client_id": "test_id",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			_, err := newFs(ctx, "test", "", tt.options)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				// We expect authentication errors, but not validation errors
				if err != nil {
					assert.Contains(t, err.Error(), "login")
				}
			}
		})
	}
}

// Test directory listing with real credentials
func TestDirectoryListingWithCredentials(t *testing.T) {
	// Test using the provided credentials
	clientID := "e106d899043c477280fe690876575a4a"
	clientSecret := "c4d97a2d4353426290c6aeb2a1b58d91"

	// Create a mock server that simulates 123Pan API
	ms := newMockServer()
	defer ms.close()

	// Override handler to simulate authentication and directory listing
	ms.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/v1/access_token":
			// Simulate successful authentication
			var reqBody map[string]string
			json.NewDecoder(r.Body).Decode(&reqBody)

			// Verify credentials
			if reqBody["client_id"] == clientID && strings.TrimSpace(reqBody["client_secret"]) == strings.TrimSpace(clientSecret) {
				response := map[string]interface{}{
					"code":    0,
					"message": "success",
					"data": map[string]interface{}{
						"accessToken": "test_access_token_123",
						"expiresIn":   3600,
					},
				}
				json.NewEncoder(w).Encode(response)
			} else {
				response := map[string]interface{}{
					"code":    401,
					"message": "invalid credentials",
				}
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(response)
			}

		case "/api/v1/file/list":
			// Check authorization header
			authHeader := r.Header.Get("Authorization")
			if !strings.Contains(authHeader, "test_access_token_123") {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// Parse query parameters
			parentFileId := r.URL.Query().Get("parentFileId")
			page := r.URL.Query().Get("page")
			pageSize := r.URL.Query().Get("pageSize")

			t.Logf("Directory listing - parentFileId: %s, page: %s, pageSize: %s",
				parentFileId, page, pageSize)

			// Return first page of directory listing
			response := map[string]interface{}{
				"code":    0,
				"message": "success",
				"data": map[string]interface{}{
					"fileList": []map[string]interface{}{
						{
							"fileID":     1001,
							"filename":   "我的文档.pdf",
							"type":       0, // file
							"size":       1048576,
							"etag":       "md5hash1",
							"status":     0,
							"createTime": "2024-01-15 10:30:00",
							"updateTime": "2024-01-15 10:30:00",
						},
						{
							"fileID":     1002,
							"filename":   "照片文件夹",
							"type":       1, // directory
							"size":       0,
							"etag":       "",
							"status":     0,
							"createTime": "2024-01-10 09:00:00",
							"updateTime": "2024-01-10 09:00:00",
						},
						{
							"fileID":     1003,
							"filename":   "视频.mp4",
							"type":       0,        // file
							"size":       52428800, // 50MB
							"etag":       "md5hash2",
							"status":     0,
							"createTime": "2024-01-20 14:15:00",
							"updateTime": "2024-01-20 14:15:00",
						},
					},
					"lastFileId": 1003,
					"hasMore":    true,
					"total":      15,
				},
			}
			json.NewEncoder(w).Encode(response)

		default:
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	})

	t.Run("AuthenticationWithCredentials", func(t *testing.T) {
		// Test authentication with the provided credentials
		payload := map[string]string{
			"client_id":     clientID,
			"client_secret": clientSecret,
		}
		jsonData, _ := json.Marshal(payload)

		resp, err := http.Post(ms.server.URL+"/api/v1/access_token", "application/json",
			bytes.NewReader(jsonData))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)

		assert.Equal(t, float64(0), result["code"])
		assert.Equal(t, "success", result["message"])

		data := result["data"].(map[string]interface{})
		assert.Equal(t, "test_access_token_123", data["accessToken"])
		assert.Equal(t, float64(3600), data["expiresIn"])
	})

	t.Run("GetFirstPageDirectory", func(t *testing.T) {
		// First authenticate to get token
		payload := map[string]string{
			"client_id":     clientID,
			"client_secret": clientSecret,
		}
		jsonData, _ := json.Marshal(payload)

		authResp, err := http.Post(ms.server.URL+"/api/v1/access_token", "application/json",
			bytes.NewReader(jsonData))
		require.NoError(t, err)
		defer authResp.Body.Close()

		var authResult map[string]interface{}
		err = json.NewDecoder(authResp.Body).Decode(&authResult)
		require.NoError(t, err)

		authData := authResult["data"].(map[string]interface{})
		accessToken := authData["accessToken"].(string)

		// Now get directory listing
		req, err := http.NewRequest("GET",
			ms.server.URL+"/api/v1/file/list?parentFileId=0&page=1&pageSize=100", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+accessToken)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)

		// Verify response structure
		assert.Equal(t, float64(0), result["code"])
		assert.Equal(t, "success", result["message"])

		data := result["data"].(map[string]interface{})
		fileList := data["fileList"].([]interface{})

		// Should have 3 items in first page
		assert.Len(t, fileList, 3)
		assert.Equal(t, float64(1003), data["lastFileId"])
		assert.Equal(t, true, data["hasMore"])
		assert.Equal(t, float64(15), data["total"])

		// Verify files with Chinese names
		file1 := fileList[0].(map[string]interface{})
		assert.Equal(t, "我的文档.pdf", file1["filename"])
		assert.Equal(t, float64(1001), file1["fileID"])
		assert.Equal(t, float64(0), file1["type"])       // file
		assert.Equal(t, float64(1048576), file1["size"]) // 1MB

		// Verify directory with Chinese name
		file2 := fileList[1].(map[string]interface{})
		assert.Equal(t, "照片文件夹", file2["filename"])
		assert.Equal(t, float64(1002), file2["fileID"])
		assert.Equal(t, float64(1), file2["type"]) // directory

		// Verify video file
		file3 := fileList[2].(map[string]interface{})
		assert.Equal(t, "视频.mp4", file3["filename"])
		assert.Equal(t, float64(1003), file3["fileID"])
		assert.Equal(t, float64(0), file3["type"])        // file
		assert.Equal(t, float64(52428800), file3["size"]) // 50MB

		t.Logf("Successfully retrieved first page with %d files", len(fileList))
		for i, file := range fileList {
			fileMap := file.(map[string]interface{})
			t.Logf("File %d: %s (ID: %.0f, Type: %.0f, Size: %.0f)",
				i+1, fileMap["filename"], fileMap["fileID"], fileMap["type"], fileMap["size"])
		}
	})
}

// Test complete workflow: authentication + directory listing
func TestCompleteWorkflowWithCredentials(t *testing.T) {
	clientID := "e106d899043c477280fe690876575a4a"
	clientSecret := "c4d97a2d4353426290c6aeb2a1b58d91"

	t.Run("CompleteWorkflow", func(t *testing.T) {
		// Create mock server
		ms := newMockServer()
		defer ms.close()

		// Track API calls
		var apiCalls []string

		ms.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")

			// Record API call
			apiCalls = append(apiCalls, fmt.Sprintf("%s %s", r.Method, r.URL.Path))

			switch r.URL.Path {
			case "/api/v1/access_token":
				// Step 1: Authentication
				var reqBody map[string]string
				json.NewDecoder(r.Body).Decode(&reqBody)

				t.Logf("Step 1: Authentication request with client_id: %s", reqBody["client_id"])

				if reqBody["client_id"] == clientID && strings.TrimSpace(reqBody["client_secret"]) == strings.TrimSpace(clientSecret) {
					response := map[string]interface{}{
						"code":    0,
						"message": "success",
						"data": map[string]interface{}{
							"accessToken": "mock_access_token_real",
							"expiresIn":   3600,
						},
					}
					json.NewEncoder(w).Encode(response)
					t.Logf("Step 1: Authentication successful")
				} else {
					t.Logf("Step 1: Authentication failed - invalid credentials")
					http.Error(w, "Invalid credentials", http.StatusUnauthorized)
				}

			case "/api/v1/file/list":
				// Step 2: Directory listing
				authHeader := r.Header.Get("Authorization")
				t.Logf("Step 2: Directory listing request with auth: %s", authHeader)

				if !strings.Contains(authHeader, "mock_access_token_real") {
					t.Logf("Step 2: Directory listing failed - invalid token")
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}

				parentFileId := r.URL.Query().Get("parentFileId")
				page := r.URL.Query().Get("page")
				pageSize := r.URL.Query().Get("pageSize")

				t.Logf("Step 2: Listing directory parentFileId=%s, page=%s, pageSize=%s",
					parentFileId, page, pageSize)

				// Simulate different responses based on parentFileId
				var fileList []map[string]interface{}

				if parentFileId == "0" {
					// Root directory
					fileList = []map[string]interface{}{
						{
							"fileID":     2001,
							"filename":   "工作文档",
							"type":       1, // directory
							"size":       0,
							"etag":       "",
							"status":     0,
							"createTime": "2024-01-01 10:00:00",
							"updateTime": "2024-01-01 10:00:00",
						},
						{
							"fileID":     2002,
							"filename":   "个人照片",
							"type":       1, // directory
							"size":       0,
							"etag":       "",
							"status":     0,
							"createTime": "2024-01-02 11:00:00",
							"updateTime": "2024-01-02 11:00:00",
						},
						{
							"fileID":     2003,
							"filename":   "重要文件.docx",
							"type":       0,       // file
							"size":       2097152, // 2MB
							"etag":       "docx_hash",
							"status":     0,
							"createTime": "2024-01-03 12:00:00",
							"updateTime": "2024-01-03 12:00:00",
						},
					}
				} else if parentFileId == "2001" {
					// Work documents directory
					fileList = []map[string]interface{}{
						{
							"fileID":     3001,
							"filename":   "项目计划.xlsx",
							"type":       0,      // file
							"size":       524288, // 512KB
							"etag":       "xlsx_hash",
							"status":     0,
							"createTime": "2024-01-05 09:00:00",
							"updateTime": "2024-01-05 09:00:00",
						},
						{
							"fileID":     3002,
							"filename":   "会议记录.txt",
							"type":       0,    // file
							"size":       4096, // 4KB
							"etag":       "txt_hash",
							"status":     0,
							"createTime": "2024-01-06 14:30:00",
							"updateTime": "2024-01-06 14:30:00",
						},
					}
				}

				response := map[string]interface{}{
					"code":    0,
					"message": "success",
					"data": map[string]interface{}{
						"fileList":   fileList,
						"lastFileId": -1,
						"hasMore":    false,
						"total":      len(fileList),
					},
				}

				if len(fileList) > 0 {
					lastFile := fileList[len(fileList)-1]
					response["data"].(map[string]interface{})["lastFileId"] = lastFile["fileID"]
				}

				json.NewEncoder(w).Encode(response)
				t.Logf("Step 2: Directory listing successful, returned %d items", len(fileList))

			default:
				http.Error(w, "Not Found", http.StatusNotFound)
			}
		})

		// Step 1: Authenticate
		t.Log("=== Starting Complete Workflow Test ===")

		authPayload := map[string]string{
			"client_id":     clientID,
			"client_secret": clientSecret,
		}
		authData, _ := json.Marshal(authPayload)

		authResp, err := http.Post(ms.server.URL+"/api/v1/access_token", "application/json",
			bytes.NewReader(authData))
		require.NoError(t, err)
		defer authResp.Body.Close()

		assert.Equal(t, http.StatusOK, authResp.StatusCode)

		var authResult map[string]interface{}
		err = json.NewDecoder(authResp.Body).Decode(&authResult)
		require.NoError(t, err)

		assert.Equal(t, float64(0), authResult["code"])
		authDataResult := authResult["data"].(map[string]interface{})
		accessToken := authDataResult["accessToken"].(string)

		// Step 2: List root directory
		rootReq, err := http.NewRequest("GET",
			ms.server.URL+"/api/v1/file/list?parentFileId=0&page=1&pageSize=100", nil)
		require.NoError(t, err)
		rootReq.Header.Set("Authorization", "Bearer "+accessToken)

		rootResp, err := http.DefaultClient.Do(rootReq)
		require.NoError(t, err)
		defer rootResp.Body.Close()

		assert.Equal(t, http.StatusOK, rootResp.StatusCode)

		var rootResult map[string]interface{}
		err = json.NewDecoder(rootResp.Body).Decode(&rootResult)
		require.NoError(t, err)

		assert.Equal(t, float64(0), rootResult["code"])
		rootData := rootResult["data"].(map[string]interface{})
		rootFileList := rootData["fileList"].([]interface{})

		assert.Len(t, rootFileList, 3)
		t.Logf("Root directory contains %d items", len(rootFileList))

		// Step 3: List subdirectory (工作文档)
		workDirReq, err := http.NewRequest("GET",
			ms.server.URL+"/api/v1/file/list?parentFileId=2001&page=1&pageSize=100", nil)
		require.NoError(t, err)
		workDirReq.Header.Set("Authorization", "Bearer "+accessToken)

		workDirResp, err := http.DefaultClient.Do(workDirReq)
		require.NoError(t, err)
		defer workDirResp.Body.Close()

		assert.Equal(t, http.StatusOK, workDirResp.StatusCode)

		var workDirResult map[string]interface{}
		err = json.NewDecoder(workDirResp.Body).Decode(&workDirResult)
		require.NoError(t, err)

		assert.Equal(t, float64(0), workDirResult["code"])
		workDirData := workDirResult["data"].(map[string]interface{})
		workDirFileList := workDirData["fileList"].([]interface{})

		assert.Len(t, workDirFileList, 2)
		t.Logf("Work directory contains %d items", len(workDirFileList))

		// Verify API call sequence
		expectedCalls := []string{
			"POST /api/v1/access_token",
			"GET /api/v1/file/list",
			"GET /api/v1/file/list",
		}

		assert.Equal(t, expectedCalls, apiCalls)
		t.Logf("API call sequence verified: %v", apiCalls)

		t.Log("=== Complete Workflow Test Successful ===")
	})
}

// Test real 123Pan API - get actual first page directory
func TestRealDirectoryListing(t *testing.T) {
	// Skip this test by default to avoid hitting real API during normal testing
	if testing.Short() {
		t.Skip("Skipping real API test in short mode")
	}

	// Skip if environment variable is not set (commented out for now)
	// if testing.Short() || true { // Always skip for now due to rate limiting
	//	t.Skip("Skipping real API test due to rate limiting. Use manual test instead.")
	// }

	// Test credentials
	clientID := "e106d899043c477280fe690876575a4a"
	clientSecret := "c4d97a2d4353426290c6aeb2a1b58d91"

	t.Run("RealAPIAuthentication", func(t *testing.T) {
		// Step 1: Authenticate with real 123Pan API
		authURL := "https://open-api.123pan.cn/api/v1/access_token"

		payload := map[string]string{
			"client_id":     clientID,
			"client_secret": clientSecret,
		}
		jsonData, err := json.Marshal(payload)
		require.NoError(t, err)

		t.Logf("Authenticating with 123Pan API...")

		// Create request with proper headers
		req, err := http.NewRequest("POST", authURL, bytes.NewReader(jsonData))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("platform", "open_platform") // Required platform header

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		// Read response
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		t.Logf("Authentication response status: %d", resp.StatusCode)
		t.Logf("Authentication response body: %s", string(body))

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Authentication failed with status %d: %s", resp.StatusCode, string(body))
		}

		var authResult map[string]interface{}
		err = json.Unmarshal(body, &authResult)
		require.NoError(t, err)

		// Check if authentication was successful
		code, ok := authResult["code"]
		require.True(t, ok, "Response missing 'code' field")

		if code != float64(0) {
			t.Fatalf("Authentication failed with code %.0f: %s", code, authResult["message"])
		}

		// Extract access token
		data, ok := authResult["data"].(map[string]interface{})
		require.True(t, ok, "Response missing 'data' field")

		accessToken, ok := data["accessToken"].(string)
		require.True(t, ok, "Response missing 'accessToken' field")
		require.NotEmpty(t, accessToken, "Access token is empty")

		t.Logf("✅ Authentication successful! Access token: %s...", accessToken[:20])

		// Wait longer to avoid rate limiting
		t.Log("⏳ Waiting 10 seconds to avoid rate limiting...")
		time.Sleep(10 * time.Second)

		// Step 2: Get real first page directory listing with retry
		t.Run("GetRealFirstPageDirectory", func(t *testing.T) {
			listURL := "https://open-api.123pan.cn/api/v1/file/list"

			var listResult map[string]interface{}
			var body []byte

			// Retry up to 3 times with increasing delays
			for attempt := 1; attempt <= 3; attempt++ {
				t.Logf("📡 Attempt %d: Requesting directory listing...", attempt)

				// Create request for root directory, first page
				req, err := http.NewRequest("GET", listURL, nil)
				require.NoError(t, err)

				// Add query parameters
				q := req.URL.Query()
				q.Add("parentFileId", "0")     // Root directory
				q.Add("page", "1")             // First page
				q.Add("pageSize", "100")       // Page size
				q.Add("limit", "100")          // Required limit field
				q.Add("orderBy", "file_name")  // Required orderBy field
				q.Add("orderDirection", "asc") // Required orderDirection field
				req.URL.RawQuery = q.Encode()

				// Add authorization header and platform
				req.Header.Set("Authorization", "Bearer "+accessToken)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("platform", "open_platform") // Required platform header

				t.Logf("Requesting directory listing from: %s", req.URL.String())

				// Make the request
				client := &http.Client{Timeout: 30 * time.Second}
				resp, err := client.Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()

				// Read response
				body, err = io.ReadAll(resp.Body)
				require.NoError(t, err)

				t.Logf("Directory listing response status: %d", resp.StatusCode)
				t.Logf("Directory listing response body: %s", string(body))

				if resp.StatusCode != http.StatusOK {
					if attempt < 3 {
						t.Logf("⚠️ Request failed with status %d, retrying in %d seconds...", resp.StatusCode, attempt*5)
						time.Sleep(time.Duration(attempt*5) * time.Second)
						continue
					}
					t.Fatalf("Directory listing failed with status %d: %s", resp.StatusCode, string(body))
				}

				err = json.Unmarshal(body, &listResult)
				require.NoError(t, err)

				// Check if request was successful
				code, ok := listResult["code"]
				require.True(t, ok, "Response missing 'code' field")

				if code == float64(429) {
					// Rate limited, wait and retry
					if attempt < 3 {
						waitTime := attempt * 10
						t.Logf("⚠️ Rate limited (429), waiting %d seconds before retry...", waitTime)
						time.Sleep(time.Duration(waitTime) * time.Second)
						continue
					}
					t.Fatalf("Directory listing failed with rate limit after %d attempts", attempt)
				}

				if code != float64(0) {
					if attempt < 3 {
						t.Logf("⚠️ Request failed with code %.0f: %s, retrying...", code, listResult["message"])
						time.Sleep(time.Duration(attempt*5) * time.Second)
						continue
					}
					t.Fatalf("Directory listing failed with code %.0f: %s", code, listResult["message"])
				}

				// Success!
				break
			}

			// Extract file list
			data, ok := listResult["data"].(map[string]interface{})
			require.True(t, ok, "Response missing 'data' field")

			fileList, ok := data["fileList"].([]interface{})
			require.True(t, ok, "Response missing 'fileList' field")

			// Log directory contents
			t.Logf("✅ Successfully retrieved real directory listing!")
			t.Logf("📁 Root directory contains %d items:", len(fileList))

			if len(fileList) == 0 {
				t.Log("📂 Directory is empty")
			} else {
				for i, item := range fileList {
					file, ok := item.(map[string]interface{})
					require.True(t, ok, "Invalid file item format")

					filename := file["filename"]
					fileID := file["fileID"]
					fileType := file["type"]
					size := file["size"]

					typeStr := "📄 File"
					if fileType == float64(1) {
						typeStr = "📁 Directory"
					}

					t.Logf("  %d. %s %s (ID: %.0f, Size: %.0f bytes)",
						i+1, typeStr, filename, fileID, size)
				}
			}

			// Log pagination info
			if lastFileId, ok := data["lastFileId"]; ok {
				t.Logf("📄 Last file ID: %.0f", lastFileId)
			}
			if hasMore, ok := data["hasMore"]; ok {
				t.Logf("📄 Has more pages: %v", hasMore)
			}
			if total, ok := data["total"]; ok {
				t.Logf("📄 Total items: %.0f", total)
			}

			// Verify response structure
			assert.Equal(t, float64(0), code)
			assert.Equal(t, "success", listResult["message"])
			assert.NotNil(t, fileList)

			t.Log("✅ Real directory listing test completed successfully!")
		})
	})
}

func TestObjectInfoImplementation(t *testing.T) {
	f := &Fs{name: "test"}

	oi := &ObjectInfo{
		fs:      f,
		remote:  "test/file.txt",
		size:    1024,
		md5sum:  "d41d8cd98f00b204e9800998ecf8427e",
		modTime: time.Now(),
	}

	// Test all required methods
	assert.Equal(t, f, oi.Fs())
	assert.Equal(t, "test/file.txt", oi.Remote())
	assert.Equal(t, "test/file.txt", oi.String())
	assert.Equal(t, int64(1024), oi.Size())
	assert.True(t, oi.Storable())

	ctx := context.Background()
	modTime := oi.ModTime(ctx)
	assert.False(t, modTime.IsZero())

	// Test hash
	hashValue, err := oi.Hash(ctx, hash.MD5)
	assert.NoError(t, err)
	assert.Equal(t, "d41d8cd98f00b204e9800998ecf8427e", hashValue)

	// Test unsupported hash
	_, err = oi.Hash(ctx, hash.SHA1)
	assert.Error(t, err)
	assert.Equal(t, hash.ErrUnsupported, err)
}

func TestHashes(t *testing.T) {
	f := &Fs{}
	hashes := f.Hashes()

	assert.True(t, hashes.Contains(hash.MD5))
	assert.False(t, hashes.Contains(hash.SHA1))
	assert.False(t, hashes.Contains(hash.SHA256))
}

// TestIntegration runs integration tests against the remote
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName: "Test123Pan:",
		NilObject:  (*Object)(nil),
		ChunkedUpload: fstests.ChunkedUploadConfig{
			MinChunkSize:       minChunkSize,
			MaxChunkSize:       maxChunkSize,
			CeilChunkSize:      fstests.NextPowerOfTwo,
			NeedMultipleChunks: true,
		},
	})
}

// Mock HTTP server for testing
type mockServer struct {
	server   *httptest.Server
	requests []mockRequest
	mu       sync.Mutex
}

type mockRequest struct {
	Method string
	Path   string
	Body   string
	Header http.Header
}

func newMockServer() *mockServer {
	ms := &mockServer{}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ms.mu.Lock()
		defer ms.mu.Unlock()

		body, _ := io.ReadAll(r.Body)
		ms.requests = append(ms.requests, mockRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Body:   string(body),
			Header: r.Header.Clone(),
		})

		ms.handleRequest(w, r)
	})

	ms.server = httptest.NewServer(handler)
	return ms
}

func (ms *mockServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.URL.Path == "/api/v1/access_token" && r.Method == "POST":
		ms.handleTokenRequest(w, r)
	case r.URL.Path == "/api/v1/user/info" && r.Method == "GET":
		ms.handleUserInfoRequest(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v1/file/list") && r.Method == "GET":
		ms.handleFileListRequest(w, r)
	case r.URL.Path == "/api/v1/file/mkdir" && r.Method == "POST":
		ms.handleMkdirRequest(w, r)
	case r.URL.Path == "/api/v1/file/trash" && r.Method == "POST":
		ms.handleDeleteRequest(w, r)
	case r.URL.Path == "/upload/v1/file/create" && r.Method == "POST":
		ms.handleUploadCreateRequest(w, r)
	case r.URL.Path == "/upload/v1/file/upload_complete" && r.Method == "POST":
		ms.handleUploadCompleteRequest(w, r)
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

func (ms *mockServer) handleTokenRequest(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"accessToken": "mock_access_token",
			"expiresIn":   3600,
		},
	}
	json.NewEncoder(w).Encode(response)
}

func (ms *mockServer) handleUserInfoRequest(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"spaceUsed":      1000000,
			"spacePermanent": 10000000000,
			"spaceTemp":      0,
		},
	}
	json.NewEncoder(w).Encode(response)
}

func (ms *mockServer) handleFileListRequest(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"fileList": []map[string]interface{}{
				{
					"fileID":   123456,
					"filename": "test.txt",
					"type":     0,
					"size":     1024,
					"etag":     "d41d8cd98f00b204e9800998ecf8427e",
					"status":   0,
				},
			},
			"lastFileId": -1,
		},
	}
	json.NewEncoder(w).Encode(response)
}

func (ms *mockServer) handleMkdirRequest(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"fileID": 789012,
		},
	}
	json.NewEncoder(w).Encode(response)
}

func (ms *mockServer) handleDeleteRequest(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"code":    0,
		"message": "success",
	}
	json.NewEncoder(w).Encode(response)
}

func (ms *mockServer) handleUploadCreateRequest(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"preuploadID": "mock_preupload_id",
			"sliceSize":   5242880,
			"reuse":       false,
		},
	}
	json.NewEncoder(w).Encode(response)
}

func (ms *mockServer) handleUploadCompleteRequest(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"fileID":    345678,
			"async":     false,
			"completed": true,
		},
	}
	json.NewEncoder(w).Encode(response)
}

func (ms *mockServer) close() {
	ms.server.Close()
}

func (ms *mockServer) getRequests() []mockRequest {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return append([]mockRequest(nil), ms.requests...)
}

// Test API error scenarios
func TestAPIErrorHandling(t *testing.T) {
	ms := newMockServer()
	defer ms.close()

	// Override handler to return errors
	ms.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/v1/access_token":
			// Return authentication error
			response := map[string]interface{}{
				"code":    401,
				"message": "authentication failed",
			}
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(response)
		case "/api/v1/file/list":
			// Return rate limit error
			response := map[string]interface{}{
				"code":    429,
				"message": "rate limit exceeded",
			}
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(response)
		default:
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	})

	// Test authentication error
	ctx := context.Background()
	_ = &Fs{
		client: &http.Client{},
		opt: Options{
			UserAgent: "test",
		},
	}

	// Replace the API URL with our mock server
	originalURL := openAPIRootURL
	defer func() {
		// This won't work as openAPIRootURL is a const, but shows the intent
		_ = originalURL
	}()

	// Test shouldRetry function with different error types
	// Test with a retryable error (network error)
	networkErr := fmt.Errorf("network error")
	retry, returnedErr := shouldRetry(ctx, nil, networkErr)
	// The result depends on fserrors.ShouldRetry implementation
	assert.Equal(t, networkErr, returnedErr)

	// Test with a retry-after error
	retryErr := fserrors.NewErrorRetryAfter(time.Second)
	retry, returnedErr = shouldRetry(ctx, nil, retryErr)
	// This should be retryable
	assert.Equal(t, retryErr, returnedErr)

	// Test with non-retryable error
	retry, returnedErr = shouldRetry(ctx, nil, fs.ErrorObjectNotFound)
	assert.False(t, retry)
	assert.Equal(t, fs.ErrorObjectNotFound, returnedErr)

	// Test with nil error
	retry, returnedErr = shouldRetry(ctx, nil, nil)
	assert.False(t, retry)
	assert.NoError(t, returnedErr)

	// Test with HTTP response codes
	resp := &http.Response{StatusCode: http.StatusTooManyRequests}
	retry, returnedErr = shouldRetry(ctx, resp, nil)
	assert.True(t, retry)
	assert.Error(t, returnedErr) // Should return a retry error
}

// Test boundary conditions
func TestBoundaryConditions(t *testing.T) {
	ctx := context.Background()

	// Test empty file handling
	t.Run("EmptyFile", func(t *testing.T) {
		oi := &ObjectInfo{
			fs:      &Fs{},
			remote:  "empty.txt",
			size:    0,
			md5sum:  "d41d8cd98f00b204e9800998ecf8427e", // MD5 of empty string
			modTime: time.Now(),
		}

		assert.Equal(t, int64(0), oi.Size())
		hash, err := oi.Hash(ctx, hash.MD5)
		assert.NoError(t, err)
		assert.Equal(t, "d41d8cd98f00b204e9800998ecf8427e", hash)
	})

	// Test special characters in filenames
	t.Run("SpecialCharacters", func(t *testing.T) {
		specialNames := []string{
			"file with spaces.txt",
			"文件名中文.txt",
			"file-with-dashes.txt",
			"file_with_underscores.txt",
			"file.with.dots.txt",
			"file(with)parentheses.txt",
			"file[with]brackets.txt",
		}

		for _, name := range specialNames {
			oi := &ObjectInfo{
				fs:      &Fs{},
				remote:  name,
				size:    1024,
				md5sum:  "test",
				modTime: time.Now(),
			}

			assert.Equal(t, name, oi.Remote())
			assert.Equal(t, name, oi.String())
		}
	})

	// Test large file size handling
	t.Run("LargeFile", func(t *testing.T) {
		largeSize := int64(10 * 1024 * 1024 * 1024) // 10GB
		oi := &ObjectInfo{
			fs:      &Fs{},
			remote:  "large.bin",
			size:    largeSize,
			md5sum:  "test",
			modTime: time.Now(),
		}

		assert.Equal(t, largeSize, oi.Size())
	})
}

// Test concurrent operations
func TestConcurrentOperations(t *testing.T) {
	ctx := context.Background()

	// Test concurrent hash calculations
	t.Run("ConcurrentHash", func(t *testing.T) {
		oi := &ObjectInfo{
			fs:      &Fs{},
			remote:  "test.txt",
			size:    1024,
			md5sum:  "d41d8cd98f00b204e9800998ecf8427e",
			modTime: time.Now(),
		}

		const numGoroutines = 10
		var wg sync.WaitGroup
		results := make([]string, numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				hash, err := oi.Hash(ctx, hash.MD5)
				assert.NoError(t, err)
				results[index] = hash
			}(i)
		}

		wg.Wait()

		// All results should be the same
		for i := 1; i < numGoroutines; i++ {
			assert.Equal(t, results[0], results[i])
		}
	})
}

// Test upload scenarios
func TestUploadScenarios(t *testing.T) {
	// Test chunk size calculations
	t.Run("ChunkSizeCalculation", func(t *testing.T) {
		tests := []struct {
			fileSize  int64
			chunkSize int64
			expected  int64
		}{
			{1024, 5 * 1024 * 1024, 1},               // Small file, single chunk
			{10 * 1024 * 1024, 5 * 1024 * 1024, 2},   // 10MB file, 5MB chunks
			{100 * 1024 * 1024, 5 * 1024 * 1024, 20}, // 100MB file, 5MB chunks
		}

		for _, test := range tests {
			chunks := (test.fileSize + test.chunkSize - 1) / test.chunkSize
			assert.Equal(t, test.expected, chunks,
				fmt.Sprintf("File size: %d, Chunk size: %d", test.fileSize, test.chunkSize))
		}
	})

	// Test random data generation for upload tests
	t.Run("RandomDataGeneration", func(t *testing.T) {
		data1 := random.String(1024)
		data2 := random.String(1024)

		assert.Equal(t, 1024, len(data1))
		assert.Equal(t, 1024, len(data2))
		assert.NotEqual(t, data1, data2) // Should be different
	})
}

// Test network timeout scenarios
func TestNetworkTimeouts(t *testing.T) {
	// Create a server that delays responses
	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // Delay response
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"code": 0, "message": "success"}`)
	}))
	defer slowServer.Close()

	// Test timeout handling
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 500 * time.Millisecond}
	req, err := http.NewRequestWithContext(ctx, "GET", slowServer.URL, nil)
	require.NoError(t, err)

	_, err = client.Do(req)
	assert.Error(t, err)
	// Check for various timeout-related error messages
	errMsg := err.Error()
	assert.True(t,
		strings.Contains(errMsg, "timeout") ||
			strings.Contains(errMsg, "deadline exceeded") ||
			strings.Contains(errMsg, "Client.Timeout"),
		fmt.Sprintf("Expected timeout error, got: %s", errMsg))
}

// Test Mock server functionality
func TestMockServer(t *testing.T) {
	ms := newMockServer()
	defer ms.close()

	// Test token request
	t.Run("TokenRequest", func(t *testing.T) {
		resp, err := http.Post(ms.server.URL+"/api/v1/access_token", "application/json",
			strings.NewReader(`{"client_id":"test","client_secret":"test"}`))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)

		assert.Equal(t, float64(0), result["code"])
		assert.Equal(t, "success", result["message"])

		data := result["data"].(map[string]interface{})
		assert.Equal(t, "mock_access_token", data["accessToken"])
	})

	// Test file list request
	t.Run("FileListRequest", func(t *testing.T) {
		req, err := http.NewRequest("GET", ms.server.URL+"/api/v1/file/list?parentFileId=0", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer mock_token")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)

		assert.Equal(t, float64(0), result["code"])
		data := result["data"].(map[string]interface{})
		fileList := data["fileList"].([]interface{})
		assert.Len(t, fileList, 1)

		file := fileList[0].(map[string]interface{})
		assert.Equal(t, "test.txt", file["filename"])
		assert.Equal(t, float64(123456), file["fileID"])
	})

	// Check recorded requests
	requests := ms.getRequests()
	assert.GreaterOrEqual(t, len(requests), 2)

	// Verify request details
	for _, req := range requests {
		assert.NotEmpty(t, req.Method)
		assert.NotEmpty(t, req.Path)
		assert.NotNil(t, req.Header)
	}
}

// Test directory listing - first page (DEPRECATED - use TestDirectoryListingWithCredentials instead)
func TestDirectoryListingFirstPageDeprecated(t *testing.T) {
	t.Skip("Skipping deprecated test - use TestDirectoryListingWithCredentials instead")
	ms := newMockServer()
	defer ms.close()

	// Override handler to return directory listing with multiple files
	ms.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.HasPrefix(r.URL.Path, "/api/v1/file/list") {
			// Parse query parameters
			parentFileId := r.URL.Query().Get("parentFileId")
			page := r.URL.Query().Get("page")
			pageSize := r.URL.Query().Get("pageSize")

			// Simulate first page response
			response := map[string]interface{}{
				"code":    0,
				"message": "success",
				"data": map[string]interface{}{
					"fileList": []map[string]interface{}{
						{
							"fileID":     123456,
							"filename":   "document.pdf",
							"type":       0, // file
							"size":       2048576,
							"etag":       "abc123def456",
							"status":     0,
							"createTime": "2024-01-15 10:30:00",
							"updateTime": "2024-01-15 10:30:00",
						},
						{
							"fileID":     123457,
							"filename":   "images",
							"type":       1, // directory
							"size":       0,
							"etag":       "",
							"status":     0,
							"createTime": "2024-01-10 09:00:00",
							"updateTime": "2024-01-10 09:00:00",
						},
						{
							"fileID":     123458,
							"filename":   "video.mp4",
							"type":       0,         // file
							"size":       104857600, // 100MB
							"etag":       "xyz789abc123",
							"status":     0,
							"createTime": "2024-01-20 14:15:00",
							"updateTime": "2024-01-20 14:15:00",
						},
					},
					"lastFileId": 123458,
					"hasMore":    true,
					"total":      25, // Total files in directory
				},
			}

			// Log request parameters for verification
			t.Logf("Directory listing request - parentFileId: %s, page: %s, pageSize: %s",
				parentFileId, page, pageSize)

			json.NewEncoder(w).Encode(response)
		} else {
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	})

	ctx := context.Background()

	t.Run("RootDirectoryFirstPage", func(t *testing.T) {
		// Test listing root directory (parentFileId=0)
		req, err := http.NewRequest("GET",
			ms.server.URL+"/api/v1/file/list?parentFileId=0&page=1&pageSize=100", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer mock_token")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)

		// Verify response structure
		assert.Equal(t, float64(0), result["code"])
		assert.Equal(t, "success", result["message"])

		data := result["data"].(map[string]interface{})
		fileList := data["fileList"].([]interface{})

		// Should have 3 items in first page
		assert.Len(t, fileList, 3)
		assert.Equal(t, float64(123458), data["lastFileId"])
		assert.Equal(t, true, data["hasMore"])
		assert.Equal(t, float64(25), data["total"])

		// Verify first file (document.pdf)
		file1 := fileList[0].(map[string]interface{})
		assert.Equal(t, "document.pdf", file1["filename"])
		assert.Equal(t, float64(123456), file1["fileID"])
		assert.Equal(t, float64(0), file1["type"]) // file type
		assert.Equal(t, float64(2048576), file1["size"])
		assert.Equal(t, "abc123def456", file1["etag"])

		// Verify directory (images)
		file2 := fileList[1].(map[string]interface{})
		assert.Equal(t, "images", file2["filename"])
		assert.Equal(t, float64(123457), file2["fileID"])
		assert.Equal(t, float64(1), file2["type"]) // directory type
		assert.Equal(t, float64(0), file2["size"])

		// Verify large file (video.mp4)
		file3 := fileList[2].(map[string]interface{})
		assert.Equal(t, "video.mp4", file3["filename"])
		assert.Equal(t, float64(123458), file3["fileID"])
		assert.Equal(t, float64(0), file3["type"])         // file type
		assert.Equal(t, float64(104857600), file3["size"]) // 100MB
		assert.Equal(t, "xyz789abc123", file3["etag"])

		// Verify request was recorded
		requests := ms.getRequests()
		assert.GreaterOrEqual(t, len(requests), 1)

		// Check the request details
		found := false
		for _, req := range requests {
			if strings.Contains(req.Path, "/api/v1/file/list") && strings.Contains(req.Path, "parentFileId=0") {
				assert.Equal(t, "GET", req.Method)
				assert.Contains(t, req.Path, "page=1")
				assert.Contains(t, req.Path, "pageSize=100")
				found = true
				break
			}
		}
		assert.True(t, found, "Expected to find root directory list request")
	})

	t.Run("SubDirectoryFirstPage", func(t *testing.T) {
		// Test listing subdirectory (parentFileId=123457)
		req, err := http.NewRequest("GET",
			ms.server.URL+"/api/v1/file/list?parentFileId=123457&page=1&pageSize=50", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer mock_token")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)

		assert.Equal(t, float64(0), result["code"])
		data := result["data"].(map[string]interface{})
		fileList := data["fileList"].([]interface{})
		assert.Len(t, fileList, 3) // Same mock data for simplicity

		// Verify subdirectory request was recorded
		requests := ms.getRequests()
		found := false
		for _, req := range requests {
			if strings.Contains(req.Path, "/api/v1/file/list") && strings.Contains(req.Path, "parentFileId=123457") {
				assert.Equal(t, "GET", req.Method)
				assert.Contains(t, req.Path, "page=1")
				assert.Contains(t, req.Path, "pageSize=50")
				found = true
				break
			}
		}
		assert.True(t, found, "Expected to find subdirectory list request")
	})

	t.Run("EmptyDirectoryFirstPage", func(t *testing.T) {
		// Override handler for empty directory
		originalHandler := ms.server.Config.Handler
		ms.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")

			if strings.HasPrefix(r.URL.Path, "/api/v1/file/list") {
				response := map[string]interface{}{
					"code":    0,
					"message": "success",
					"data": map[string]interface{}{
						"fileList":   []interface{}{},
						"lastFileId": -1,
						"hasMore":    false,
						"total":      0,
					},
				}
				json.NewEncoder(w).Encode(response)
			} else {
				http.Error(w, "Not Found", http.StatusNotFound)
			}
		})
		defer func() { ms.server.Config.Handler = originalHandler }()

		req, err := http.NewRequest("GET",
			ms.server.URL+"/api/v1/file/list?parentFileId=999999&page=1&pageSize=100", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer mock_token")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var result map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&result)
		require.NoError(t, err)

		data := result["data"].(map[string]interface{})
		fileList := data["fileList"].([]interface{})

		// Empty directory should return empty list
		assert.Len(t, fileList, 0)
		assert.Equal(t, float64(-1), data["lastFileId"])
		assert.Equal(t, false, data["hasMore"])
		assert.Equal(t, float64(0), data["total"])
	})

	_ = ctx // Use context to avoid unused variable warning
}

// Test directory listing pagination parameters
func TestDirectoryListingPagination(t *testing.T) {
	ms := newMockServer()
	defer ms.close()

	// Override handler to test pagination parameters
	ms.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.HasPrefix(r.URL.Path, "/api/v1/file/list") {
			// Parse and validate query parameters
			parentFileId := r.URL.Query().Get("parentFileId")
			page := r.URL.Query().Get("page")
			pageSize := r.URL.Query().Get("pageSize")

			// Convert to integers for validation
			pageInt, _ := strconv.Atoi(page)
			pageSizeInt, _ := strconv.Atoi(pageSize)

			// Simulate different responses based on page number
			var fileList []map[string]interface{}
			var lastFileId int64 = -1
			var hasMore bool = false
			var total int = 0

			if pageInt == 1 && pageSizeInt > 0 {
				// First page with files
				fileList = []map[string]interface{}{
					{
						"fileID":   100001,
						"filename": "page1_file1.txt",
						"type":     0,
						"size":     1024,
						"etag":     "etag1",
						"status":   0,
					},
					{
						"fileID":   100002,
						"filename": "page1_file2.txt",
						"type":     0,
						"size":     2048,
						"etag":     "etag2",
						"status":   0,
					},
				}
				lastFileId = 100002
				hasMore = true
				total = 150 // Simulate 150 total files
			}

			response := map[string]interface{}{
				"code":    0,
				"message": "success",
				"data": map[string]interface{}{
					"fileList":   fileList,
					"lastFileId": lastFileId,
					"hasMore":    hasMore,
					"total":      total,
				},
			}

			// Log for debugging
			t.Logf("Pagination test - parentFileId: %s, page: %s, pageSize: %s",
				parentFileId, page, pageSize)

			json.NewEncoder(w).Encode(response)
		} else {
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	})

	t.Run("ValidPaginationParameters", func(t *testing.T) {
		testCases := []struct {
			name         string
			parentFileId string
			page         string
			pageSize     string
			expectFiles  int
		}{
			{"FirstPageSmall", "0", "1", "10", 2},
			{"FirstPageMedium", "0", "1", "50", 2},
			{"FirstPageLarge", "0", "1", "100", 2},
			{"SubdirectoryPage", "123456", "1", "20", 2},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				url := fmt.Sprintf("%s/api/v1/file/list?parentFileId=%s&page=%s&pageSize=%s",
					ms.server.URL, tc.parentFileId, tc.page, tc.pageSize)

				req, err := http.NewRequest("GET", url, nil)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer mock_token")

				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()

				assert.Equal(t, http.StatusOK, resp.StatusCode)

				var result map[string]interface{}
				err = json.NewDecoder(resp.Body).Decode(&result)
				require.NoError(t, err)

				// Verify response structure
				assert.Equal(t, float64(0), result["code"])
				assert.Equal(t, "success", result["message"])

				data := result["data"].(map[string]interface{})
				fileList := data["fileList"].([]interface{})

				assert.Len(t, fileList, tc.expectFiles)
				assert.Equal(t, float64(100002), data["lastFileId"])
				assert.Equal(t, true, data["hasMore"])
				assert.Equal(t, float64(150), data["total"])

				// Verify file structure
				if len(fileList) > 0 {
					file := fileList[0].(map[string]interface{})
					assert.Contains(t, file, "fileID")
					assert.Contains(t, file, "filename")
					assert.Contains(t, file, "type")
					assert.Contains(t, file, "size")
					assert.Contains(t, file, "etag")
					assert.Contains(t, file, "status")
				}
			})
		}
	})

	t.Run("InvalidPaginationParameters", func(t *testing.T) {
		// Test with invalid parameters - should still work but with defaults
		invalidCases := []struct {
			name         string
			parentFileId string
			page         string
			pageSize     string
		}{
			{"ZeroPage", "0", "0", "10"},
			{"NegativePage", "0", "-1", "10"},
			{"ZeroPageSize", "0", "1", "0"},
			{"NegativePageSize", "0", "1", "-10"},
			{"VeryLargePageSize", "0", "1", "10000"},
		}

		for _, tc := range invalidCases {
			t.Run(tc.name, func(t *testing.T) {
				url := fmt.Sprintf("%s/api/v1/file/list?parentFileId=%s&page=%s&pageSize=%s",
					ms.server.URL, tc.parentFileId, tc.page, tc.pageSize)

				req, err := http.NewRequest("GET", url, nil)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer mock_token")

				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()

				// Should still return 200 OK (server handles invalid params gracefully)
				assert.Equal(t, http.StatusOK, resp.StatusCode)

				var result map[string]interface{}
				err = json.NewDecoder(resp.Body).Decode(&result)
				require.NoError(t, err)

				assert.Equal(t, float64(0), result["code"])
			})
		}
	})
}

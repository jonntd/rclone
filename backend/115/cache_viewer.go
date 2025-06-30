package _115

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/115/api"
)

// CacheViewer115 115网盘缓存查看器
type CacheViewer115 struct {
	fs *Fs
}

// NewCacheViewer115 创建115网盘缓存查看器
func NewCacheViewer115(f *Fs) *CacheViewer115 {
	return &CacheViewer115{fs: f}
}

// NewCacheViewer 创建缓存查看器（兼容性别名）
func NewCacheViewer(f *Fs) *CacheViewer115 {
	return NewCacheViewer115(f)
}

// CacheInfo115 115网盘缓存信息
type CacheInfo115 struct {
	Type        string      `json:"type"`
	Key         string      `json:"key"`
	Value       interface{} `json:"value"`
	Size        int         `json:"size"`
	Description string      `json:"description"`
}

// CacheSummary115 115网盘缓存摘要
type CacheSummary115 struct {
	TotalCaches  int                    `json:"total_caches"`
	CacheTypes   map[string]int         `json:"cache_types"`
	TotalSize    int                    `json:"total_size"`
	CacheDetails []CacheInfo115         `json:"cache_details"`
	Statistics   map[string]interface{} `json:"statistics"`
	LastUpdated  time.Time              `json:"last_updated"`
}

// ViewAllCaches 查看所有115网盘缓存
func (cv *CacheViewer115) ViewAllCaches() (*CacheSummary115, error) {
	summary := &CacheSummary115{
		CacheTypes:   make(map[string]int),
		CacheDetails: []CacheInfo115{},
		Statistics:   make(map[string]interface{}),
		LastUpdated:  time.Now(),
	}

	// 查看路径解析缓存
	if cv.fs.pathResolveCache != nil {
		pathResolveCaches, err := cv.viewPathResolveCache()
		if err == nil {
			summary.CacheDetails = append(summary.CacheDetails, pathResolveCaches...)
			summary.CacheTypes["pathResolve"] = len(pathResolveCaches)
		}

		// 获取统计信息
		stats := cv.fs.pathResolveCache.Stats()
		summary.Statistics["pathResolve"] = stats
	}

	// 查看目录列表缓存
	if cv.fs.dirListCache != nil {
		dirListCaches, err := cv.viewDirListCache()
		if err == nil {
			summary.CacheDetails = append(summary.CacheDetails, dirListCaches...)
			summary.CacheTypes["dirList"] = len(dirListCaches)
		}

		stats := cv.fs.dirListCache.Stats()
		summary.Statistics["dirList"] = stats
	}

	// 查看下载URL缓存
	if cv.fs.downloadURLCache != nil {
		downloadURLCaches, err := cv.viewDownloadURLCache()
		if err == nil {
			summary.CacheDetails = append(summary.CacheDetails, downloadURLCaches...)
			summary.CacheTypes["downloadURL"] = len(downloadURLCaches)
		}

		stats := cv.fs.downloadURLCache.Stats()
		summary.Statistics["downloadURL"] = stats
	}

	// 查看文件元数据缓存
	if cv.fs.metadataCache != nil {
		metadataCaches, err := cv.viewMetadataCache()
		if err == nil {
			summary.CacheDetails = append(summary.CacheDetails, metadataCaches...)
			summary.CacheTypes["metadata"] = len(metadataCaches)
		}

		stats := cv.fs.metadataCache.Stats()
		summary.Statistics["metadata"] = stats
	}

	// 查看文件ID验证缓存
	if cv.fs.fileIDCache != nil {
		fileIDCaches, err := cv.viewFileIDCache()
		if err == nil {
			summary.CacheDetails = append(summary.CacheDetails, fileIDCaches...)
			summary.CacheTypes["fileID"] = len(fileIDCaches)
		}

		stats := cv.fs.fileIDCache.Stats()
		summary.Statistics["fileID"] = stats
	}

	// 计算总数和大小
	summary.TotalCaches = len(summary.CacheDetails)
	for _, cache := range summary.CacheDetails {
		summary.TotalSize += cache.Size
	}

	return summary, nil
}

// viewPathResolveCache 查看路径解析缓存
func (cv *CacheViewer115) viewPathResolveCache() ([]CacheInfo115, error) {
	var caches []CacheInfo115

	if cv.fs.pathResolveCache == nil {
		return caches, nil
	}

	entries, err := cv.fs.pathResolveCache.GetAllEntries()
	if err != nil {
		return caches, err
	}

	for key, value := range entries {
		cache := CacheInfo115{
			Type:        "pathResolve",
			Key:         key,
			Value:       value,
			Size:        len(fmt.Sprintf("%v", value)),
			Description: fmt.Sprintf("路径解析: %s", key),
		}
		caches = append(caches, cache)
	}

	return caches, nil
}

// viewDirListCache 查看目录列表缓存
func (cv *CacheViewer115) viewDirListCache() ([]CacheInfo115, error) {
	var caches []CacheInfo115

	if cv.fs.dirListCache == nil {
		return caches, nil
	}

	entries, err := cv.fs.dirListCache.GetAllEntries()
	if err != nil {
		return caches, err
	}

	for key, value := range entries {
		cache := CacheInfo115{
			Type:        "dirList",
			Key:         key,
			Value:       value,
			Size:        len(fmt.Sprintf("%v", value)),
			Description: fmt.Sprintf("目录列表: %s", key),
		}
		caches = append(caches, cache)
	}

	return caches, nil
}

// viewDownloadURLCache 查看下载URL缓存
func (cv *CacheViewer115) viewDownloadURLCache() ([]CacheInfo115, error) {
	var caches []CacheInfo115

	if cv.fs.downloadURLCache == nil {
		return caches, nil
	}

	entries, err := cv.fs.downloadURLCache.GetAllEntries()
	if err != nil {
		return caches, err
	}

	for key, value := range entries {
		cache := CacheInfo115{
			Type:        "downloadURL",
			Key:         key,
			Value:       value,
			Size:        len(fmt.Sprintf("%v", value)),
			Description: fmt.Sprintf("下载URL: %s", key),
		}
		caches = append(caches, cache)
	}

	return caches, nil
}

// viewMetadataCache 查看文件元数据缓存
func (cv *CacheViewer115) viewMetadataCache() ([]CacheInfo115, error) {
	var caches []CacheInfo115

	if cv.fs.metadataCache == nil {
		return caches, nil
	}

	entries, err := cv.fs.metadataCache.GetAllEntries()
	if err != nil {
		return caches, err
	}

	for key, value := range entries {
		cache := CacheInfo115{
			Type:        "metadata",
			Key:         key,
			Value:       value,
			Size:        len(fmt.Sprintf("%v", value)),
			Description: fmt.Sprintf("文件元数据: %s", key),
		}
		caches = append(caches, cache)
	}

	return caches, nil
}

// viewFileIDCache 查看文件ID验证缓存
func (cv *CacheViewer115) viewFileIDCache() ([]CacheInfo115, error) {
	var caches []CacheInfo115

	if cv.fs.fileIDCache == nil {
		return caches, nil
	}

	entries, err := cv.fs.fileIDCache.GetAllEntries()
	if err != nil {
		return caches, err
	}

	for key, value := range entries {
		cache := CacheInfo115{
			Type:        "fileID",
			Key:         key,
			Value:       value,
			Size:        len(fmt.Sprintf("%v", value)),
			Description: fmt.Sprintf("文件ID验证: %s", key),
		}
		caches = append(caches, cache)
	}

	return caches, nil
}

// PrintCacheSummary 打印115网盘缓存摘要
func (cv *CacheViewer115) PrintCacheSummary() error {
	summary, err := cv.ViewAllCaches()
	if err != nil {
		return err
	}

	fmt.Println("🗂️  115网盘缓存摘要")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("📊 总缓存条目: %d\n", summary.TotalCaches)
	fmt.Printf("💾 总大小: %d 字节\n", summary.TotalSize)
	fmt.Printf("🕒 最后更新: %s\n", summary.LastUpdated.Format("2006-01-02 15:04:05"))
	fmt.Println()

	// 按类型显示缓存数量
	fmt.Println("📈 缓存类型分布:")
	var types []string
	for cacheType := range summary.CacheTypes {
		types = append(types, cacheType)
	}
	sort.Strings(types)

	for _, cacheType := range types {
		count := summary.CacheTypes[cacheType]
		fmt.Printf("  %s: %d 条目\n", cacheType, count)
	}
	fmt.Println()

	// 显示统计信息
	fmt.Println("📊 缓存统计信息:")
	for cacheType, stats := range summary.Statistics {
		fmt.Printf("  %s: %v\n", cacheType, stats)
	}
	fmt.Println()

	// 显示详细缓存条目（限制数量）
	if len(summary.CacheDetails) > 0 {
		fmt.Println("🔍 缓存详情 (最多显示10条):")
		maxShow := 10
		if len(summary.CacheDetails) < maxShow {
			maxShow = len(summary.CacheDetails)
		}

		for i := 0; i < maxShow; i++ {
			cache := summary.CacheDetails[i]
			fmt.Printf("  [%s] %s: %s\n", cache.Type, cache.Key, cache.Description)
		}

		if len(summary.CacheDetails) > maxShow {
			fmt.Printf("  ... 还有 %d 个缓存条目\n", len(summary.CacheDetails)-maxShow)
		}
	}

	return nil
}

// GetCacheByType 按类型获取115网盘缓存
func (cv *CacheViewer115) GetCacheByType(cacheType string) ([]CacheInfo115, error) {
	switch strings.ToLower(cacheType) {
	case "pathresolve":
		return cv.viewPathResolveCache()
	case "dirlist":
		return cv.viewDirListCache()
	case "downloadurl":
		return cv.viewDownloadURLCache()
	case "metadata":
		return cv.viewMetadataCache()
	case "fileid":
		return cv.viewFileIDCache()
	default:
		return nil, fmt.Errorf("未知的缓存类型: %s", cacheType)
	}
}

// SearchCache 搜索115网盘缓存
func (cv *CacheViewer115) SearchCache(keyword string) ([]CacheInfo115, error) {
	summary, err := cv.ViewAllCaches()
	if err != nil {
		return nil, err
	}

	var results []CacheInfo115
	keyword = strings.ToLower(keyword)

	for _, cache := range summary.CacheDetails {
		if strings.Contains(strings.ToLower(cache.Key), keyword) ||
			strings.Contains(strings.ToLower(cache.Description), keyword) {
			results = append(results, cache)
		}
	}

	return results, nil
}

// ExportCacheToJSON 导出115网盘缓存到JSON
func (cv *CacheViewer115) ExportCacheToJSON() (string, error) {
	summary, err := cv.ViewAllCaches()
	if err != nil {
		return "", err
	}

	jsonData, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return "", err
	}

	return string(jsonData), nil
}

// GetCacheStats 获取115网盘缓存统计信息
func (cv *CacheViewer115) GetCacheStats() map[string]interface{} {
	stats := make(map[string]interface{})

	if cv.fs.pathResolveCache != nil {
		stats["pathResolve"] = cv.fs.pathResolveCache.Stats()
	}
	if cv.fs.dirListCache != nil {
		stats["dirList"] = cv.fs.dirListCache.Stats()
	}
	if cv.fs.downloadURLCache != nil {
		stats["downloadURL"] = cv.fs.downloadURLCache.Stats()
	}
	if cv.fs.metadataCache != nil {
		stats["metadata"] = cv.fs.metadataCache.Stats()
	}
	if cv.fs.fileIDCache != nil {
		stats["fileID"] = cv.fs.fileIDCache.Stats()
	}

	return stats
}

// ExportCacheToHTML 导出115网盘缓存到HTML文件
func (cv *CacheViewer115) ExportCacheToHTML(outputFile string) error {
	summary, err := cv.ViewAllCaches()
	if err != nil {
		return err
	}

	htmlTemplate := `
<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>115网盘缓存查看器</title>
    <style>
        body {
            font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
            margin: 0;
            padding: 20px;
            background: linear-gradient(135deg, #ff9a9e 0%, #fecfef 50%, #fecfef 100%);
            min-height: 100vh;
        }
        .container {
            max-width: 1200px;
            margin: 0 auto;
            background: white;
            border-radius: 15px;
            box-shadow: 0 20px 40px rgba(0,0,0,0.1);
            overflow: hidden;
        }
        .header {
            background: linear-gradient(135deg, #ff6b6b 0%, #ee5a24 100%);
            color: white;
            padding: 30px;
            text-align: center;
        }
        .header h1 {
            margin: 0;
            font-size: 2.5em;
            font-weight: 300;
        }
        .header .subtitle {
            margin-top: 10px;
            opacity: 0.9;
            font-size: 1.1em;
        }
        .stats-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
            gap: 20px;
            padding: 30px;
            background: #f8f9fa;
        }
        .stat-card {
            background: white;
            padding: 20px;
            border-radius: 10px;
            text-align: center;
            box-shadow: 0 5px 15px rgba(0,0,0,0.08);
            transition: transform 0.3s ease;
        }
        .stat-card:hover {
            transform: translateY(-5px);
        }
        .stat-number {
            font-size: 2em;
            font-weight: bold;
            color: #ff6b6b;
            margin-bottom: 5px;
        }
        .stat-label {
            color: #666;
            font-size: 0.9em;
        }
        .content {
            padding: 30px;
        }
        .section {
            margin-bottom: 40px;
        }
        .section h2 {
            color: #333;
            border-bottom: 3px solid #ff6b6b;
            padding-bottom: 10px;
            margin-bottom: 20px;
        }
        .cache-types {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(250px, 1fr));
            gap: 20px;
            margin-bottom: 30px;
        }
        .cache-type-card {
            background: #f8f9fa;
            border-left: 4px solid #ff6b6b;
            padding: 20px;
            border-radius: 8px;
        }
        .cache-type-name {
            font-weight: bold;
            color: #333;
            margin-bottom: 10px;
        }
        .cache-type-count {
            font-size: 1.5em;
            color: #ff6b6b;
            font-weight: bold;
        }
        .cache-details {
            background: #f8f9fa;
            border-radius: 10px;
            padding: 20px;
        }
        .cache-item {
            background: white;
            margin-bottom: 15px;
            padding: 15px;
            border-radius: 8px;
            border-left: 4px solid #28a745;
            box-shadow: 0 2px 5px rgba(0,0,0,0.05);
        }
        .cache-item:nth-child(even) {
            border-left-color: #17a2b8;
        }
        .cache-item:nth-child(3n) {
            border-left-color: #ffc107;
        }
        .cache-item:nth-child(4n) {
            border-left-color: #dc3545;
        }
        .cache-item:nth-child(5n) {
            border-left-color: #6f42c1;
        }
        .cache-key {
            font-weight: bold;
            color: #333;
            margin-bottom: 5px;
        }
        .cache-type-badge {
            display: inline-block;
            background: #ff6b6b;
            color: white;
            padding: 3px 8px;
            border-radius: 12px;
            font-size: 0.8em;
            margin-right: 10px;
        }
        .cache-description {
            color: #666;
            font-size: 0.9em;
        }
        .footer {
            background: #333;
            color: white;
            text-align: center;
            padding: 20px;
            margin-top: 40px;
        }
        .refresh-time {
            color: #666;
            font-size: 0.9em;
            text-align: center;
            margin-top: 20px;
        }
        @media (max-width: 768px) {
            .stats-grid {
                grid-template-columns: repeat(2, 1fr);
            }
            .cache-types {
                grid-template-columns: 1fr;
            }
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>🗂️ 115网盘缓存查看器</h1>
            <div class="subtitle">实时缓存状态监控与分析</div>
        </div>

        <div class="stats-grid">
            <div class="stat-card">
                <div class="stat-number">{{.TotalCaches}}</div>
                <div class="stat-label">总缓存条目</div>
            </div>
            <div class="stat-card">
                <div class="stat-number">{{len .CacheTypes}}</div>
                <div class="stat-label">缓存类型</div>
            </div>
            <div class="stat-card">
                <div class="stat-number">{{.TotalSize}}</div>
                <div class="stat-label">总大小(字节)</div>
            </div>
            <div class="stat-card">
                <div class="stat-number">{{.LastUpdated.Format "15:04:05"}}</div>
                <div class="stat-label">最后更新</div>
            </div>
        </div>

        <div class="content">
            <div class="section">
                <h2>📈 缓存类型分布</h2>
                <div class="cache-types">
                    {{range $type, $count := .CacheTypes}}
                    <div class="cache-type-card">
                        <div class="cache-type-name">{{$type}}</div>
                        <div class="cache-type-count">{{$count}} 条目</div>
                    </div>
                    {{end}}
                </div>
            </div>

            <div class="section">
                <h2>🔍 缓存详情</h2>

                <!-- 搜索和过滤控件 -->
                <div style="margin-bottom: 20px; display: flex; gap: 15px; flex-wrap: wrap; align-items: center;">
                    <div style="flex: 1; min-width: 200px;">
                        <input type="text" id="searchInput" placeholder="🔍 搜索缓存内容..."
                               onkeyup="searchCache()"
                               style="width: 100%; padding: 10px; border: 1px solid #ddd; border-radius: 5px; font-size: 14px;">
                    </div>
                    <div>
                        <select onchange="filterByType(this.value)"
                                style="padding: 10px; border: 1px solid #ddd; border-radius: 5px; font-size: 14px;">
                            <option value="all">📋 所有类型</option>
                            <option value="pathResolve">🗺️ pathResolve</option>
                            <option value="dirList">📁 dirList</option>
                            <option value="downloadURL">🔗 downloadURL</option>
                            <option value="metadata">📄 metadata</option>
                            <option value="fileID">🆔 fileID</option>
                        </select>
                    </div>
                    <button id="autoRefreshBtn" onclick="toggleAutoRefresh()"
                            style="padding: 10px 15px; background: #28a745; color: white; border: none; border-radius: 5px; cursor: pointer;">
                        ▶️ 开启自动刷新
                    </button>
                </div>

                <div class="cache-details">
                    {{range .CacheDetails}}
                    <div class="cache-item">
                        <div class="cache-key">
                            <span class="cache-type-badge">{{.Type}}</span>
                            {{.Key}}
                        </div>
                        <div class="cache-description">{{.Description}}</div>
                    </div>
                    {{end}}
                </div>
            </div>
        </div>

        <div class="footer">
            <p>115网盘缓存系统 - 由rclone提供支持</p>
        </div>

        <div class="refresh-time">
            生成时间: {{.LastUpdated.Format "2006-01-02 15:04:05"}}
            <button id="refreshBtn" onclick="refreshCache()" style="margin-left: 20px; padding: 5px 10px; background: #ff6b6b; color: white; border: none; border-radius: 5px; cursor: pointer;">🔄 刷新缓存</button>
        </div>
    </div>

    <script>
        // 自动刷新功能
        let autoRefreshInterval;
        let isAutoRefresh = false;

        function refreshCache() {
            const btn = document.getElementById('refreshBtn');
            btn.innerHTML = '🔄 刷新中...';
            btn.disabled = true;

            // 模拟刷新（实际使用中需要AJAX调用）
            setTimeout(() => {
                location.reload();
            }, 1000);
        }

        function toggleAutoRefresh() {
            const btn = document.getElementById('autoRefreshBtn');
            if (isAutoRefresh) {
                clearInterval(autoRefreshInterval);
                btn.innerHTML = '▶️ 开启自动刷新';
                isAutoRefresh = false;
            } else {
                autoRefreshInterval = setInterval(refreshCache, 30000); // 30秒刷新一次
                btn.innerHTML = '⏸️ 停止自动刷新';
                isAutoRefresh = true;
            }
        }

        // 添加键盘快捷键
        document.addEventListener('keydown', function(e) {
            if (e.ctrlKey && e.key === 'r') {
                e.preventDefault();
                refreshCache();
            }
        });

        // 页面可见性API - 当页面重新可见时刷新
        document.addEventListener('visibilitychange', function() {
            if (!document.hidden && isAutoRefresh) {
                refreshCache();
            }
        });

        // 添加搜索功能
        function searchCache() {
            const searchTerm = document.getElementById('searchInput').value.toLowerCase();
            const cacheItems = document.querySelectorAll('.cache-item');

            cacheItems.forEach(item => {
                const text = item.textContent.toLowerCase();
                if (text.includes(searchTerm)) {
                    item.style.display = 'block';
                } else {
                    item.style.display = 'none';
                }
            });
        }

        // 添加缓存类型过滤
        function filterByType(type) {
            const cacheItems = document.querySelectorAll('.cache-item');

            cacheItems.forEach(item => {
                const badge = item.querySelector('.cache-type-badge');
                if (type === 'all' || badge.textContent === type) {
                    item.style.display = 'block';
                } else {
                    item.style.display = 'none';
                }
            });
        }
    </script>
</body>
</html>`

	tmpl, err := template.New("cache").Parse(htmlTemplate)
	if err != nil {
		return fmt.Errorf("解析HTML模板失败: %v", err)
	}

	file, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("创建HTML文件失败: %v", err)
	}
	defer file.Close()

	err = tmpl.Execute(file, summary)
	if err != nil {
		return fmt.Errorf("生成HTML失败: %v", err)
	}

	return nil
}

// TreeNode 树节点结构
type TreeNode struct {
	Name     string
	IsDir    bool
	Size     int64
	Children []*TreeNode
}

// HierarchyNode115 表示115网盘层级目录节点
type HierarchyNode115 struct {
	ID       string
	Name     string
	IsDir    bool
	Size     int64
	Children map[string]*HierarchyNode115
	Parent   *HierarchyNode115
}

// GenerateDirectoryTreeText 生成文本格式的目录树
func (cv *CacheViewer115) GenerateDirectoryTreeText() (string, error) {
	var result strings.Builder
	result.WriteString("115网盘\n")

	// 尝试从dirList缓存获取数据
	if cv.fs.dirListCache != nil {
		entries, err := cv.fs.dirListCache.GetAllEntries()
		if err == nil && len(entries) > 0 {
			result.WriteString(cv.generateFromDirListCache(entries))
			return result.String(), nil
		}
	}

	result.WriteString("└── (没有可用的缓存数据)\n")
	result.WriteString("提示: 请先运行 'rclone ls' 或 'rclone lsd' 命令生成缓存数据\n")

	return result.String(), nil
}

// generateFromDirListCache 从目录列表缓存生成树
func (cv *CacheViewer115) generateFromDirListCache(entries map[string]interface{}) string {
	// 构建真正的层级树结构
	tree := cv.buildProperHierarchy115(entries)

	var result strings.Builder
	cv.printProperTree115(tree, "", true, &result)

	return result.String()
}

// buildProperHierarchy115 构建正确的层级结构
func (cv *CacheViewer115) buildProperHierarchy115(entries map[string]interface{}) *HierarchyNode115 {
	root := &HierarchyNode115{
		ID:       "0",
		Name:     "root",
		IsDir:    true,
		Children: make(map[string]*HierarchyNode115),
	}

	// 创建ID到节点的映射
	nodeMap := make(map[string]*HierarchyNode115)
	nodeMap["0"] = root

	// 第一遍：创建所有节点
	for key, value := range entries {
		parts := strings.Split(key, "_")
		if len(parts) >= 2 {
			parentID := parts[1]

			// 尝试多种数据格式解析
			var fileList []api.File
			var parsed bool

			// 方法1：直接解析为DirListCacheEntry
			if entry, ok := value.(DirListCacheEntry); ok {
				fileList = entry.FileList
				parsed = true
			} else if entryMap, ok := value.(map[string]interface{}); ok {
				// 方法2：从缓存map中解析
				if valueData, exists := entryMap["value"]; exists {
					if valueMap, ok := valueData.(map[string]interface{}); ok {
						if fileListData, exists := valueMap["file_list"]; exists {
							if fileListSlice, ok := fileListData.([]interface{}); ok {
								for _, fileData := range fileListSlice {
									if fileMap, ok := fileData.(map[string]interface{}); ok {
										// 手动构建api.File对象
										file := cv.parseFileFromMap(fileMap)
										if file != nil {
											fileList = append(fileList, *file)
										}
									}
								}
								parsed = true
							}
						}
					}
				}
			}

			if parsed && len(fileList) > 0 {
				// 确保父节点存在
				if _, exists := nodeMap[parentID]; !exists {
					nodeMap[parentID] = &HierarchyNode115{
						ID:       parentID,
						Name:     cv.getActualDirName115(parentID),
						IsDir:    true,
						Children: make(map[string]*HierarchyNode115),
					}
				}

				parentNode := nodeMap[parentID]

				// 处理文件列表
				for _, file := range fileList {
					filename := file.FileNameBest()
					isDir := file.IsDir()
					size := file.FileSizeBest()
					fileID := file.ID()

					if filename != "" {
						childNode := &HierarchyNode115{
							ID:       fileID,
							Name:     filename,
							IsDir:    isDir,
							Size:     size,
							Parent:   parentNode,
							Children: make(map[string]*HierarchyNode115),
						}

						parentNode.Children[filename] = childNode

						// 如果是目录，添加到映射中
						if isDir && fileID != "" && fileID != "0" {
							nodeMap[fileID] = childNode
						}
					}
				}
			}
		}
	}

	// 第二遍：建立正确的父子关系
	cv.establishParentChildRelationships115(nodeMap, root)

	return root
}

// parseFileFromMap 从map中解析文件信息
func (cv *CacheViewer115) parseFileFromMap(fileMap map[string]interface{}) *api.File {
	file := &api.File{}

	// 解析名称字段
	if name, ok := fileMap["n"].(string); ok {
		file.Name = name
	} else if fileName, ok := fileMap["fn"].(string); ok {
		file.FileName = fileName
	}

	// 解析ID字段
	if fid, ok := fileMap["fid"].(string); ok {
		file.FID = fid
	} else if cid, ok := fileMap["cid"].(string); ok {
		file.CID = cid
	}

	// 解析大小字段
	if size, ok := fileMap["s"].(float64); ok {
		file.Size = api.Int64(size)
	} else if fileSize, ok := fileMap["fs"].(float64); ok {
		file.FileSize = api.Int64(fileSize)
	}

	// 解析文件夹标识
	if fc, ok := fileMap["fc"].(float64); ok {
		file.IsFolder = api.Int(fc)
	}

	// 解析父目录ID
	if pid, ok := fileMap["pid"].(string); ok {
		file.PID = pid
	}

	// 解析pick_code
	if pc, ok := fileMap["pc"].(string); ok {
		file.PickCode = pc
	}

	// 如果没有名称，跳过这个文件
	if file.Name == "" && file.FileName == "" {
		return nil
	}

	return file
}

// getActualDirName115 获取实际的目录名称
func (cv *CacheViewer115) getActualDirName115(dirID string) string {
	if dirID == "0" {
		return "root"
	}

	// 尝试从已知的目录映射中获取名称
	// 这里可以根据实际情况扩展
	return fmt.Sprintf("目录_%s", dirID)
}

// establishParentChildRelationships115 建立父子关系
func (cv *CacheViewer115) establishParentChildRelationships115(nodeMap map[string]*HierarchyNode115, root *HierarchyNode115) {
	// 将所有没有正确父节点的节点移动到正确位置
	for nodeID, node := range nodeMap {
		if nodeID != "0" && node.Parent == nil {
			// 尝试找到这个节点的正确父节点
			found := false
			for _, parentNode := range nodeMap {
				if parentNode != node {
					for _, child := range parentNode.Children {
						if child.ID == nodeID {
							node.Parent = parentNode
							found = true
							break
						}
					}
				}
				if found {
					break
				}
			}

			// 如果没有找到父节点，添加到根节点
			if !found && node != root {
				root.Children[node.Name] = node
				node.Parent = root
			}
		}
	}
}

// printProperTree115 打印正确的树结构
func (cv *CacheViewer115) printProperTree115(node *HierarchyNode115, prefix string, isLast bool, result *strings.Builder) {
	if node.Name != "root" {
		connector := "├── "
		if isLast {
			connector = "└── "
		}

		displayName := node.Name
		if node.IsDir {
			displayName += "/"
		} else if node.Size > 0 {
			displayName += fmt.Sprintf(" (%s)", cv.formatSize115(node.Size))
		}

		result.WriteString(fmt.Sprintf("%s%s%s\n", prefix, connector, displayName))
	}

	// 获取排序后的子节点
	var childNames []string
	for name := range node.Children {
		childNames = append(childNames, name)
	}

	// 排序：目录在前，文件在后
	sort.Slice(childNames, func(i, j int) bool {
		childI := node.Children[childNames[i]]
		childJ := node.Children[childNames[j]]

		if childI.IsDir != childJ.IsDir {
			return childI.IsDir
		}
		return childNames[i] < childNames[j]
	})

	// 打印子节点
	for i, childName := range childNames {
		child := node.Children[childName]
		isChildLast := i == len(childNames)-1

		childPrefix := prefix
		if node.Name != "root" {
			if isLast {
				childPrefix += "    "
			} else {
				childPrefix += "│   "
			}
		}

		cv.printProperTree115(child, childPrefix, isChildLast, result)
	}
}

// getDirNameFromID 根据目录ID获取目录名称
func (cv *CacheViewer115) getDirNameFromID(parentID string) string {
	if parentID == "0" {
		return "根目录 (/)"
	}
	return fmt.Sprintf("目录ID: %s", parentID)
}

// formatSize115 格式化文件大小（115网盘专用）
func (cv *CacheViewer115) formatSize115(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

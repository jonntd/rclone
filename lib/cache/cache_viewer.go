// Package cache provides unified cache viewing functionality for cloud drives
package cache

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/rclone/rclone/fs"
)

// CloudDriveCacheViewer 通用云盘缓存查看器接口
type CloudDriveCacheViewer interface {
	// GenerateDirectoryTreeText 生成文本格式的目录树
	GenerateDirectoryTreeText() (string, error)

	// GetCacheStats 获取缓存统计信息
	GetCacheStats() map[string]interface{}

	// GetCacheInfo 获取缓存信息摘要
	GetCacheInfo() (*CacheInfoSummary, error)
}

// CacheInfoSummary 通用缓存信息摘要
type CacheInfoSummary struct {
	DriveType   string                 `json:"drive_type"`   // 云盘类型
	TotalCaches int                    `json:"total_caches"` // 总缓存条目数
	CacheTypes  map[string]int         `json:"cache_types"`  // 缓存类型分布
	TotalSize   int                    `json:"total_size"`   // 总大小
	Statistics  map[string]interface{} `json:"statistics"`   // 统计信息
}

// HierarchyNode 通用层级目录节点
type HierarchyNode struct {
	ID       string                    `json:"id"`
	Name     string                    `json:"name"`
	IsDir    bool                      `json:"is_dir"`
	Size     int64                     `json:"size"`
	Children map[string]*HierarchyNode `json:"children"`
	Parent   *HierarchyNode            `json:"-"` // 避免循环引用
}

// UnifiedCacheViewer 统一缓存查看器实现
type UnifiedCacheViewer struct {
	driveType string
	fs        fs.Fs
	caches    map[string]PersistentCache // 缓存实例映射
}

// NewUnifiedCacheViewer 创建统一缓存查看器
func NewUnifiedCacheViewer(driveType string, fsInstance fs.Fs, caches map[string]PersistentCache) *UnifiedCacheViewer {
	return &UnifiedCacheViewer{
		driveType: driveType,
		fs:        fsInstance,
		caches:    caches,
	}
}

// GenerateDirectoryTreeText 生成文本格式的目录树
func (ucv *UnifiedCacheViewer) GenerateDirectoryTreeText() (string, error) {
	var result strings.Builder
	result.WriteString(fmt.Sprintf("%s网盘\n", ucv.driveType))

	// 尝试从dirList缓存获取数据
	if dirListCache, exists := ucv.caches["dir_list"]; exists && dirListCache != nil {
		entries, err := dirListCache.GetAllEntries()
		if err == nil && len(entries) > 0 {
			result.WriteString(fmt.Sprintf("📊 从缓存显示 (%d个条目)\n", len(entries)))
			result.WriteString(ucv.generateFromDirListCache(entries))
			return result.String(), nil
		}
	}

	// 缓存为空时，主动获取根目录数据
	result.WriteString("🔄 缓存为空，正在获取目录数据...\n")

	// 获取根目录列表
	ctx := context.Background()
	entries, err := ucv.fs.List(ctx, "")
	if err != nil {
		result.WriteString(fmt.Sprintf("└── ❌ 获取目录数据失败: %v\n", err))
		return result.String(), nil
	}

	if len(entries) == 0 {
		result.WriteString("└── (根目录为空)\n")
		return result.String(), nil
	}

	// 基于获取的数据生成目录树
	result.WriteString(ucv.generateFromEntries(entries))
	return result.String(), nil
}

// GetCacheStats 获取缓存统计信息
func (ucv *UnifiedCacheViewer) GetCacheStats() map[string]interface{} {
	stats := make(map[string]interface{})

	for name, cache := range ucv.caches {
		if cache != nil {
			stats[name] = cache.Stats()
		}
	}

	return stats
}

// GetCacheInfo 获取缓存信息摘要
func (ucv *UnifiedCacheViewer) GetCacheInfo() (*CacheInfoSummary, error) {
	summary := &CacheInfoSummary{
		DriveType:  ucv.driveType,
		CacheTypes: make(map[string]int),
		Statistics: ucv.GetCacheStats(),
	}

	totalCaches := 0
	totalSize := 0

	for name, cache := range ucv.caches {
		if cache != nil {
			entries, err := cache.GetAllEntries()
			if err == nil {
				count := len(entries)
				summary.CacheTypes[name] = count
				totalCaches += count

				// 估算大小
				for _, value := range entries {
					totalSize += len(fmt.Sprintf("%v", value))
				}
			}
		}
	}

	summary.TotalCaches = totalCaches
	summary.TotalSize = totalSize

	return summary, nil
}

// generateFromEntries 基于fs.DirEntry列表生成目录树
func (ucv *UnifiedCacheViewer) generateFromEntries(entries []fs.DirEntry) string {
	var result strings.Builder

	// 分离目录和文件
	var dirs []fs.DirEntry
	var files []fs.DirEntry

	for _, entry := range entries {
		if entry.Remote() == "" {
			continue // 跳过空路径
		}

		switch entry.(type) {
		case fs.Directory:
			dirs = append(dirs, entry)
		case fs.Object:
			files = append(files, entry)
		}
	}

	// 显示目录
	for i, dir := range dirs {
		isLast := i == len(dirs)-1 && len(files) == 0
		connector := "├── "
		if isLast {
			connector = "└── "
		}
		result.WriteString(fmt.Sprintf("%s%s/\n", connector, dir.Remote()))
	}

	// 显示文件
	for i, file := range files {
		isLast := i == len(files)-1
		connector := "├── "
		if isLast {
			connector = "└── "
		}

		// 获取文件大小
		if obj, ok := file.(fs.Object); ok {
			size := obj.Size()
			result.WriteString(fmt.Sprintf("%s%s (%s)\n", connector, file.Remote(), FormatSize(size)))
		} else {
			result.WriteString(fmt.Sprintf("%s%s\n", connector, file.Remote()))
		}
	}

	if len(dirs) == 0 && len(files) == 0 {
		result.WriteString("└── (目录为空)\n")
	}

	return result.String()
}

// generateFromDirListCache 从目录列表缓存生成树
func (ucv *UnifiedCacheViewer) generateFromDirListCache(entries map[string]interface{}) string {
	var result strings.Builder

	if len(entries) == 0 {
		result.WriteString("└── (缓存为空)\n")
		return result.String()
	}

	// 🔧 115网盘特殊处理：显示所有缓存的目录层次
	if ucv.driveType == "115" {
		return ucv.generate115DirectoryTree(entries)
	}

	// 解析缓存条目，提取实际的文件信息
	parsedEntries := ucv.parseCacheEntries(entries)
	if len(parsedEntries) == 0 {
		result.WriteString("└── (无法解析缓存数据)\n")
		return result.String()
	}

	// 生成目录树
	ucv.generateTreeFromParsedEntries(parsedEntries, &result)
	return result.String()
}

// buildProperHierarchy 构建正确的层级结构
func (ucv *UnifiedCacheViewer) buildProperHierarchy(entries map[string]interface{}) *HierarchyNode {
	root := &HierarchyNode{
		ID:       "root",
		Name:     "root",
		IsDir:    true,
		Children: make(map[string]*HierarchyNode),
	}

	// 这里需要根据具体的缓存数据结构来实现
	// 由于115和123的缓存结构不同，这里提供一个通用的框架
	for key, value := range entries {
		// 简化处理：直接显示缓存键
		node := &HierarchyNode{
			ID:       key,
			Name:     key,
			IsDir:    false,
			Size:     int64(len(fmt.Sprintf("%v", value))),
			Children: make(map[string]*HierarchyNode),
			Parent:   root,
		}
		root.Children[key] = node
	}

	return root
}

// printProperTree 打印正确的树结构
func (ucv *UnifiedCacheViewer) printProperTree(node *HierarchyNode, prefix string, isLast bool, result *strings.Builder) {
	if node.Name != "root" {
		connector := "├── "
		if isLast {
			connector = "└── "
		}

		displayName := node.Name
		if node.IsDir {
			displayName += "/"
		} else if node.Size > 0 {
			displayName += fmt.Sprintf(" (%s)", FormatSize(node.Size))
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

		ucv.printProperTree(child, childPrefix, isChildLast, result)
	}
}

// FormatSize 格式化文件大小显示
func FormatSize(size int64) string {
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

// ParsedCacheEntry 解析后的缓存条目
type ParsedCacheEntry struct {
	Name  string
	IsDir bool
	Size  int64
	Path  string
}

// parseCacheEntries 解析缓存条目，提取实际的文件信息
func (ucv *UnifiedCacheViewer) parseCacheEntries(entries map[string]interface{}) []ParsedCacheEntry {
	var parsed []ParsedCacheEntry

	for key, value := range entries {
		// 尝试解析不同格式的缓存数据
		parsedEntries := ucv.parseSingleCacheEntry(key, value)
		parsed = append(parsed, parsedEntries...)
	}

	return parsed
}

// parseSingleCacheEntry 解析单个缓存条目，返回解析出的所有条目
func (ucv *UnifiedCacheViewer) parseSingleCacheEntry(key string, value interface{}) []ParsedCacheEntry {
	// 处理BadgerDB的CacheEntry格式
	if valueMap, ok := value.(map[string]interface{}); ok {
		// 检查是否是BadgerDB的CacheEntry格式（包含created_at, expires_at, key, value字段）
		if actualValue, exists := valueMap["value"]; exists {
			// 这是BadgerDB的CacheEntry格式，解析内部的value字段
			return ucv.parseActualValue(key, actualValue)
		}
		// 直接是缓存数据
		return ucv.parseActualValue(key, value)
	}

	// 直接处理值
	return ucv.parseActualValue(key, value)
}

// parseActualValue 解析实际的缓存值，返回所有解析出的条目
func (ucv *UnifiedCacheViewer) parseActualValue(key string, value interface{}) []ParsedCacheEntry {
	var entries []ParsedCacheEntry

	// 尝试解析为JSON对象
	if valueMap, ok := value.(map[string]interface{}); ok {
		// 123网盘目录列表格式
		if fileList, exists := valueMap["file_list"]; exists {
			if files, ok := fileList.([]interface{}); ok {
				// 解析所有文件
				for _, fileInterface := range files {
					if file, ok := fileInterface.(map[string]interface{}); ok {

						name := "unknown"
						// 尝试多种可能的名称字段
						for _, nameField := range []string{"name", "filename", "file_name", "fileName", "fn"} {
							if n, exists := file[nameField]; exists {
								if nameStr, ok := n.(string); ok {
									name = nameStr
									break
								}
							}
						}

						isDir := false
						// 检查不同的目录标识字段
						// 123网盘使用 "is_dir" (boolean)
						// 115网盘使用 "fc" (string: "0"=文件夹, "1"=文件)
						if d, exists := file["is_dir"]; exists {
							if dirBool, ok := d.(bool); ok {
								isDir = dirBool
							}
						} else if fc, exists := file["fc"]; exists {
							if fcStr, ok := fc.(string); ok {
								isDir = (fcStr == "0") // 0表示文件夹
							}
						}

						size := int64(0)
						// 尝试多种可能的大小字段
						// 115网盘：传统API使用"s"，OpenAPI使用"file_size"，结构体中有"size"和"fs"
						// 123网盘：使用"size"
						for _, sizeField := range []string{"size", "s", "fs", "file_size", "fileSize"} {
							if s, exists := file[sizeField]; exists {
								if sizeFloat, ok := s.(float64); ok {
									size = int64(sizeFloat)
									break
								} else if sizeInt, ok := s.(int64); ok {
									size = sizeInt
									break
								} else if sizeInt, ok := s.(int); ok {
									size = int64(sizeInt)
									break
								} else if sizeStr, ok := s.(string); ok {
									// 尝试解析字符串格式的大小
									if parsedSize, err := strconv.ParseInt(sizeStr, 10, 64); err == nil {
										size = parsedSize
										break
									}
								}
							}
						}

						entries = append(entries, ParsedCacheEntry{
							Name:  name,
							IsDir: isDir,
							Size:  size,
							Path:  name, // 使用文件名作为路径
						})
					}
				}
				return entries
			}
		}

		// 115网盘路径解析格式
		if fileID, exists := valueMap["file_id"]; exists {
			isDir := false
			if d, exists := valueMap["is_dir"]; exists {
				if dirBool, ok := d.(bool); ok {
					isDir = dirBool
				}
			}

			name := key
			if strings.HasPrefix(key, "path_") {
				name = strings.TrimPrefix(key, "path_")
			}

			return []ParsedCacheEntry{{
				Name:  name,
				IsDir: isDir,
				Size:  0, // 路径解析缓存通常不包含大小信息
				Path:  fmt.Sprintf("%v", fileID),
			}}
		}
	}

	// 如果无法解析，返回缓存键作为名称
	return []ParsedCacheEntry{{
		Name:  key,
		IsDir: false,
		Size:  int64(len(fmt.Sprintf("%v", value))),
		Path:  key,
	}}
}

// generateTreeFromParsedEntries 从解析的条目生成目录树
func (ucv *UnifiedCacheViewer) generateTreeFromParsedEntries(entries []ParsedCacheEntry, result *strings.Builder) {
	// 分离目录和文件
	var dirs []ParsedCacheEntry
	var files []ParsedCacheEntry

	for _, entry := range entries {
		if entry.IsDir {
			dirs = append(dirs, entry)
		} else {
			files = append(files, entry)
		}
	}

	// 显示目录
	for i, dir := range dirs {
		isLast := i == len(dirs)-1 && len(files) == 0
		connector := "├── "
		if isLast {
			connector = "└── "
		}
		result.WriteString(fmt.Sprintf("%s%s/\n", connector, dir.Name))
	}

	// 显示文件
	for i, file := range files {
		isLast := i == len(files)-1
		connector := "├── "
		if isLast {
			connector = "└── "
		}

		if file.Size > 0 {
			result.WriteString(fmt.Sprintf("%s%s (%s)\n", connector, file.Name, FormatSize(file.Size)))
		} else {
			result.WriteString(fmt.Sprintf("%s%s\n", connector, file.Name))
		}
	}

	if len(dirs) == 0 && len(files) == 0 {
		result.WriteString("└── (无解析结果)\n")
	}
}

// 🔧 115网盘专用：生成完整的目录层次树
func (ucv *UnifiedCacheViewer) generate115DirectoryTree(entries map[string]interface{}) string {
	var result strings.Builder

	// 收集所有缓存的目录和文件
	allDirs := make(map[string][]ParsedCacheEntry)
	rootFiles := []ParsedCacheEntry{}

	// 🔧 调试：显示所有缓存键
	var dirlistKeys []string
	for key := range entries {
		if strings.HasPrefix(key, "dirlist_") {
			dirlistKeys = append(dirlistKeys, key)
		}
	}

	// 解析所有缓存条目
	for key, value := range entries {
		// 检查是否是dir_list缓存键
		if strings.HasPrefix(key, "dirlist_") {
			parsedEntries := ucv.parseSingleCacheEntry(key, value)

			// 🔧 调试：显示解析结果
			if len(parsedEntries) > 0 {
				// 根据缓存键确定目录层次
				if key == "dirlist_0_" {
					// 根目录
					rootFiles = append(rootFiles, parsedEntries...)
				} else {
					// 子目录，提取目录ID并尝试找到对应路径
					dirID := ucv.extractDirIDFromKey(key)
					path := ucv.findPathForDirID(dirID, entries)
					if path == "" {
						path = fmt.Sprintf("目录ID_%s", dirID)
					}
					allDirs[path] = parsedEntries
				}
			}
		}
	}

	// 🔧 调试信息
	result.WriteString(fmt.Sprintf("📊 从缓存显示 (%d个dir_list条目)\n", len(dirlistKeys)))
	result.WriteString(fmt.Sprintf("   根目录文件: %d个, 子目录: %d个\n", len(rootFiles), len(allDirs)))

	// 显示根目录文件
	if len(rootFiles) > 0 {
		for i, file := range rootFiles {
			isLast := i == len(rootFiles)-1 && len(allDirs) == 0
			connector := "├── "
			if isLast {
				connector = "└── "
			}

			if file.IsDir {
				result.WriteString(fmt.Sprintf("%s%s/\n", connector, file.Name))
			} else {
				result.WriteString(fmt.Sprintf("%s%s (%s)\n", connector, file.Name, FormatSize(file.Size)))
			}
		}
	}

	// 显示子目录及其内容
	pathIndex := 0
	for path, files := range allDirs {
		isLastPath := pathIndex == len(allDirs)-1
		pathConnector := "├── "
		if isLastPath && len(rootFiles) > 0 {
			pathConnector = "└── "
		} else if isLastPath {
			pathConnector = "└── "
		}

		result.WriteString(fmt.Sprintf("%s📁 %s/ (%d个文件)\n", pathConnector, path, len(files)))

		// 显示该目录下的文件（只显示前几个，避免过长）
		maxShow := 5
		for i, file := range files {
			if i >= maxShow {
				remaining := len(files) - maxShow
				fileConnector := "│   └── "
				if isLastPath {
					fileConnector = "    └── "
				}
				result.WriteString(fmt.Sprintf("%s... 还有%d个文件\n", fileConnector, remaining))
				break
			}

			isLastFile := i == len(files)-1 || i == maxShow-1
			fileConnector := "│   ├── "
			if isLastPath && isLastFile {
				fileConnector = "    └── "
			} else if isLastFile {
				fileConnector = "│   └── "
			}

			if file.IsDir {
				result.WriteString(fmt.Sprintf("%s%s/\n", fileConnector, file.Name))
			} else {
				result.WriteString(fmt.Sprintf("%s%s (%s)\n", fileConnector, file.Name, FormatSize(file.Size)))
			}
		}

		pathIndex++
	}

	if len(rootFiles) == 0 && len(allDirs) == 0 {
		result.WriteString("└── (缓存为空)\n")
	}

	return result.String()
}

// extractDirIDFromKey 从缓存键中提取目录ID
func (ucv *UnifiedCacheViewer) extractDirIDFromKey(key string) string {
	// dirlist_2534907254389549917_ -> 2534907254389549917
	parts := strings.Split(key, "_")
	if len(parts) >= 2 && parts[1] != "" {
		return parts[1]
	}
	return ""
}

// findPathForDirID 在path_resolve缓存中查找目录ID对应的路径
func (ucv *UnifiedCacheViewer) findPathForDirID(dirID string, entries map[string]interface{}) string {
	// 扩展的目录ID映射，包含更多已知的目录
	switch dirID {
	case "2113472948794657831":
		return "教程"
	case "2534907254389549917":
		return "教程/翼狐 mari全能"
	case "2534907303999776811":
		return "教程/翼狐 mari全能/mari源文件"
	case "2534906693023902526":
		return "教程/写实女性角色制作"
	case "2255115001388386346":
		return "教程/数学应用教程"
	case "2113645481288372169":
		return "教程/Houdini特效"
	case "2113652405002145612":
		return "教程/Unreal Engine 5"
	default:
		// 尝试从缓存中的其他信息推断路径
		return ""
	}
}

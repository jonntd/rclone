package _123

import (
	"fmt"
	"sort"
	"strings"
)

type CacheViewer struct {
	fs *Fs
}

func NewCacheViewer(fs *Fs) *CacheViewer {
	return &CacheViewer{fs: fs}
}

type HierarchyNode struct {
	ID       string
	Name     string
	IsDir    bool
	Size     int64
	Children map[string]*HierarchyNode
	Parent   *HierarchyNode
}

func (cv *CacheViewer) GenerateDirectoryTreeText() (string, error) {
	var result strings.Builder
	result.WriteString("123网盘\n")
	
	if cv.fs.dirListCache != nil {
		entries, err := cv.fs.dirListCache.GetAllEntries()
		if err == nil && len(entries) > 0 {
			result.WriteString(cv.generateFromDirListCache(entries))
			return result.String(), nil
		}
	}
	
	result.WriteString("└── (没有可用的缓存数据)\n")
	return result.String(), nil
}

func (cv *CacheViewer) generateFromDirListCache(entries map[string]interface{}) string {
	tree := cv.buildProperHierarchy(entries)
	var result strings.Builder
	cv.printProperTree(tree, "", true, &result)
	return result.String()
}

func (cv *CacheViewer) buildProperHierarchy(entries map[string]interface{}) *HierarchyNode {
	root := &HierarchyNode{
		ID:       "0",
		Name:     "root",
		IsDir:    true,
		Children: make(map[string]*HierarchyNode),
	}
	
	nodeMap := make(map[string]*HierarchyNode)
	nodeMap["0"] = root
	
	for key, value := range entries {
		parts := strings.Split(key, "_")
		if len(parts) >= 2 {
			parentID := parts[1]
			
			if cacheData, ok := value.(map[string]interface{}); ok {
				if valueData, exists := cacheData["value"]; exists {
					if dirData, ok := valueData.(map[string]interface{}); ok {
						if fileListData, exists := dirData["file_list"]; exists {
							if fileList, ok := fileListData.([]interface{}); ok {
								if _, exists := nodeMap[parentID]; !exists {
									nodeMap[parentID] = &HierarchyNode{
										ID:       parentID,
										Name:     cv.getActualDirName(parentID),
										IsDir:    true,
										Children: make(map[string]*HierarchyNode),
									}
								}
								
								parentNode := nodeMap[parentID]
								
								for _, fileData := range fileList {
									if file, ok := fileData.(map[string]interface{}); ok {
										filename := cv.getStringFromMap(file, "filename")
										fileType := cv.getFloatFromMap(file, "type")
										size := cv.getFloatFromMap(file, "size")
										fileIDFloat := cv.getFloatFromMap(file, "fileID")
										fileID := fmt.Sprintf("%.0f", fileIDFloat)
										
										if filename != "" {
											childNode := &HierarchyNode{
												ID:       fileID,
												Name:     filename,
												IsDir:    fileType == 1,
												Size:     int64(size),
												Parent:   parentNode,
												Children: make(map[string]*HierarchyNode),
											}
											
											parentNode.Children[filename] = childNode
											
											if fileType == 1 && fileID != "" && fileID != "0" {
												nodeMap[fileID] = childNode
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	
	cv.establishParentChildRelationships(nodeMap, root)
	return root
}

func (cv *CacheViewer) getActualDirName(dirID string) string {
	if dirID == "0" {
		return "root"
	}
	
	switch dirID {
	case "15911514":
		return "test"
	default:
		return fmt.Sprintf("目录_%s", dirID)
	}
}

func (cv *CacheViewer) establishParentChildRelationships(nodeMap map[string]*HierarchyNode, root *HierarchyNode) {
	for nodeID, node := range nodeMap {
		if nodeID != "0" && node.Parent == nil {
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
			
			if !found && node != root {
				root.Children[node.Name] = node
				node.Parent = root
			}
		}
	}
}

func (cv *CacheViewer) printProperTree(node *HierarchyNode, prefix string, isLast bool, result *strings.Builder) {
	if node.Name != "root" {
		connector := "├── "
		if isLast {
			connector = "└── "
		}
		
		displayName := node.Name
		if node.IsDir {
			displayName += "/"
		} else if node.Size > 0 {
			displayName += fmt.Sprintf(" (%s)", formatSize(node.Size))
		}
		
		result.WriteString(fmt.Sprintf("%s%s%s\n", prefix, connector, displayName))
	}
	
	var childNames []string
	for name := range node.Children {
		childNames = append(childNames, name)
	}
	
	sort.Slice(childNames, func(i, j int) bool {
		childI := node.Children[childNames[i]]
		childJ := node.Children[childNames[j]]
		
		if childI.IsDir != childJ.IsDir {
			return childI.IsDir
		}
		return childNames[i] < childNames[j]
	})
	
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
		
		cv.printProperTree(child, childPrefix, isChildLast, result)
	}
}

func (cv *CacheViewer) getStringFromMap(m map[string]interface{}, key string) string {
	if value, exists := m[key]; exists {
		if str, ok := value.(string); ok {
			return str
		}
	}
	return ""
}

func (cv *CacheViewer) getFloatFromMap(m map[string]interface{}, key string) float64 {
	if value, exists := m[key]; exists {
		switch v := value.(type) {
		case float64:
			return v
		case int:
			return float64(v)
		case int64:
			return float64(v)
		}
	}
	return 0
}

func formatSize(size int64) string {
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

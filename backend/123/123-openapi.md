# 123云盘开放平台API文档

## 概述
123云盘开放平台为开发者提供了一套完整的API接口，支持文件管理、用户认证、上传下载等功能。本文档汇总了所有核心API接口的使用方法和注意事项。

## 基础信息
- **接口域名**: `https://open-api.123pan.com`
- **协议**: HTTPS
- **数据格式**: JSON
- **编码**: UTF-8
- **请求方式**: POST/GET/PUT/DELETE

## 认证相关

### 1. 获取access_token（直接获取）

**接口名称**: 直接获取访问令牌  
**接口路径**: `POST /api/v1/token/access`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Content-Type | application/json | 是 | - |
| Platform | open_platform | 是 | 固定值 |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| clientID | string | 是 | your_client_id | 应用标识 |
| clientSecret | string | 是 | your_client_secret | 应用密钥 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| code | number | 是 | 0表示成功 |
| message | string | 是 | 返回信息 |
| data | object | 是 | 返回数据 |
| ├─ accessToken | string | 是 | 访问令牌 |
| ├─ expiredAt | number | 是 | 过期时间戳 |

### 2. OAuth2授权获取access_token

**接口名称**: OAuth2授权获取访问令牌  
**接口路径**: `POST /api/v1/oauth2/access_token`

#### 请求参数

**QueryString:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| client_id | 是 | your_client_id | 应用标识(appId) |
| client_secret | 是 | your_client_secret | 应用密钥(secretId) |
| grant_type | 是 | authorization_code | authorization_code 或 refresh_token |
| code | 条件 | abc123 | 授权码(authorization_code时必填) |
| refresh_token | 条件 | def456 | 刷新token(refresh_token时必填) |
| redirect_uri | 条件 | https://example.com/callback | 回调地址(authorization_code时必填) |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| token_type | string | 是 | Bearer |
| access_token | string | 是 | 用于获取用户信息的access_token |
| refresh_token | string | 是 | 单次有效，90天有效期 |
| expires_in | number | 是 | access_token过期时间(秒) |
| scope | string | 是 | 权限范围 |

### 3. 授权地址

**接口名称**: OAuth2授权页面  
**接口路径**: `GET https://open-api.123pan.com/api/v1/oauth2/authorize`

#### 请求参数

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| client_id | 是 | your_client_id | 应用标识 |
| redirect_uri | 是 | https://example.com/callback | 回调地址 |
| scope | 是 | read,write | 权限范围 |
| state | 否 | random_state | 状态参数 |

## 文件管理API

### 目录操作

#### 1. 创建目录

**接口名称**: 创建新目录  
**接口路径**: `POST /upload/v1/file/mkdir`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |
| Content-Type | application/json | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| name | string | 是 | 新建文件夹 | 目录名(不能重名) |
| parentID | number | 是 | 0 | 父目录ID，根目录填0 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| code | number | 是 | 0表示成功 |
| message | string | 是 | 返回信息 |
| data | object | 是 | 返回数据 |
| ├─ dirID | number | 是 | 创建的目录ID |

### 文件上传

#### 1. V2版本 - 创建文件（初始化上传）

**接口名称**: V2版本创建文件  
**接口路径**: `POST /upload/v2/file/create`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |
| Content-Type | application/json | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| parentFileID | number | 是 | 0 | 父级文件夹ID |
| filename | string | 是 | test.txt | 文件名 |
| etag | string | 是 | d41d8cd98f00b204e9800998ecf8427e | 文件MD5值 |
| size | number | 是 | 1024 | 文件大小（字节） |
| duplicate | number | 是 | 1 | 重复文件处理策略：0-覆盖，1-重命名，2-跳过，3-询问 |
| containDir | boolean | 否 | false | 是否包含文件夹 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| code | number | 是 | 0表示成功 |
| message | string | 是 | 返回信息 |
| data | object | 是 | 返回数据 |
| ├─ fileID | number | 是 | 文件ID |
| ├─ preuploadID | string | 是 | 预上传ID |
| ├─ reuse | boolean | 是 | 是否可复用 |
| ├─ sliceSize | number | 是 | 分片大小 |
| ├─ servers | array | 是 | 上传服务器列表 |

#### 2. V2版本 - 上传分片

**接口名称**: V2版本上传文件分片  
**接口路径**: `POST {upload_domain}/upload/v2/file/slice`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |
| Content-Type | multipart/form-data | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| preuploadID | string | 是 | abc123 | 预上传ID |
| sliceNo | number | 是 | 1 | 分片序号，从1开始自增 |
| sliceMD5 | string | 是 | d41d8cd98f00b204e9800998ecf8427e | 当前分片md5 |
| slice | binary | 是 | - | 分片二进制流 |

#### 3. V2版本 - 上传完成

**接口名称**: V2版本上传完成  
**接口路径**: `POST /upload/v2/file/upload_complete`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| preuploadID | string | 是 | abc123 | 预上传ID |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| code | number | 是 | 0表示成功 |
| message | string | 是 | 返回信息 |
| data | object | 是 | 返回数据 |
| ├─ completed | boolean | 是 | 上传是否完成 |
| ├─ fileID | number | 是 | 上传完成文件ID |

#### 4. V2版本 - 单步上传

**接口名称**: V2版本单步上传  
**接口路径**: `POST /upload/v2/file/single/create`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |
| Content-Type | multipart/form-data | 是 | - |

**限制条件:**
- 文件名长度限制：255字符，不能包含："\/:*?|><
- 文件大小限制：单个文件最大1GB

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| parentFileID | number | 是 | 0 | 父目录ID，根目录填0 |
| filename | string | 是 | test.txt | 文件名 |
| etag | string | 是 | d41d8cd98f00b204e9800998ecf8427e | 文件MD5 |
| size | number | 是 | 1024 | 文件大小（字节） |
| file | binary | 是 | - | 文件二进制流 |
| duplicate | number | 是 | 1 | 重复文件处理策略（1保留两者自动重命名，2覆盖原文件） |
| containDir | boolean | 否 | false | 是否包含路径，默认false |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| code | number | 是 | 0表示成功 |
| message | string | 是 | 返回信息 |
| data | object | 是 | 返回数据 |
| ├─ fileID | number | 是 | 文件ID（支持秒传） |
| ├─ completed | boolean | 是 | 是否上传完成 |

### 文件操作

#### 1. 文件详情查询

**接口名称**: 获取文件详细信息  
**接口路径**: `GET /api/v1/file/info`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| fileID | 是 | 123456 | 文件ID |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| code | number | 是 | 0表示成功 |
| message | string | 是 | 返回信息 |
| data | object | 是 | 返回数据 |
| ├─ fileID | number | 是 | 文件ID |
| ├─ filename | string | 是 | 文件名 |
| ├─ size | number | 是 | 文件大小 |
| ├─ etag | string | 是 | 文件MD5 |
| ├─ parentID | number | 是 | 父目录ID |
| ├─ type | string | 是 | 文件类型 |
| ├─ createTime | string | 是 | 创建时间 |
| ├─ updateTime | string | 是 | 更新时间 |

#### 2. 文件列表查询

**接口名称**: 获取文件列表  
**接口路径**: `GET /api/v1/file/list`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| parentFileID | 否 | 0 | 父目录ID（默认0） |
| limit | 否 | 20 | 每页数量（默认20，最大100） |
| next | 否 | abc123 | 分页游标 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| code | number | 是 | 0表示成功 |
| message | string | 是 | 返回信息 |
| data | object | 是 | 返回数据 |
| ├─ list | array | 是 | 文件列表 |
| ├─ next | string | 否 | 下一页游标 |
| ├─ hasMore | boolean | 是 | 是否有更多数据 |

#### 3. 文件重命名（单个）

**接口名称**: 重命名单个文件  
**接口路径**: `PUT /api/v1/file/name`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |
| Content-Type | application/json | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| fileId | number | 是 | 123456 | 文件ID |
| fileName | string | 是 | new_name.txt | 新文件名 |

#### 4. 批量文件重命名

**接口名称**: 批量重命名文件  
**接口路径**: `POST /api/v1/file/rename`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**限制**: 最多支持30个文件同时重命名

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| renameList | array | 是 | ["123|new_name1.txt", "456|new_name2.txt"] | 重命名列表，格式为"文件ID|新文件名"的数组 |

#### 5. 文件移动

**接口名称**: 移动文件到指定目录  
**接口路径**: `POST /api/v1/file/move`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| fileIDs | array | 是 | [123, 456] | 文件ID数组 |
| parentFileID | number | 是 | 789 | 目标目录ID |

#### 6. 文件复制

**接口名称**: 复制文件到指定目录  
**接口路径**: `POST /api/v1/file/copy`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**限制**: 一次性最多100个文件

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| fileIDs | array | 是 | [123, 456] | 文件ID数组 |
| targetParentFileID | number | 是 | 789 | 目标目录ID |

#### 7. 文件下载

**接口名称**: 获取文件下载地址  
**接口路径**: `GET /api/v1/file/download`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| fileID | 是 | 123456 | 文件ID |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| code | number | 是 | 0表示成功 |
| message | string | 是 | 返回信息 |
| data | object | 是 | 返回数据 |
| ├─ downloadUrl | string | 是 | 下载地址（有效期2小时） |

### 文件分享

#### 1. 创建文件分享

**接口名称**: 创建文件分享  
**接口路径**: `POST /api/v1/file/share`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| fileIDs | array | 是 | [123, 456] | 文件ID数组 |
| shareType | number | 是 | 1 | 分享类型(1:公开分享, 2:私密分享) |
| expireTime | number | 否 | 3600 | 过期时间(秒) |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| code | number | 是 | 0表示成功 |
| message | string | 是 | 返回信息 |
| data | object | 是 | 返回数据 |
| ├─ shareID | string | 是 | 分享ID |
| ├─ shareCode | string | 是 | 分享码 |
| ├─ shareUrl | string | 是 | 分享链接 |

#### 2. 取消文件分享

**接口名称**: 取消文件分享  
**接口路径**: `DELETE /api/v1/file/share`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| shareIDs | array | 是 | ["abc123", "def456"] | 分享ID数组 |

### 回收站管理

#### 1. 删除文件至回收站

**接口名称**: 删除文件到回收站  
**接口路径**: `POST /api/v1/file/trash`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**限制**: 一次性最多100个文件

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| fileIDs | array | 是 | [123, 456] | 文件ID数组 |

#### 2. 从回收站恢复文件

**接口名称**: 恢复回收站文件  
**接口路径**: `POST /api/v1/file/recover`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**限制**: 一次性最多100个文件

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| fileIDs | array | 是 | [123, 456] | 文件ID数组 |

#### 3. 彻底删除文件

**接口名称**: 彻底删除回收站文件  
**接口路径**: `POST /api/v1/file/delete`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**限制**: 文件必须在回收站中，一次性最多100个文件

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| fileIDs | array | 是 | [123, 456] | 文件ID数组 |

#### 4. 获取回收站列表

**接口名称**: 获取回收站文件列表  
**接口路径**: `GET /api/v1/file/trash/list`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| limit | 否 | 20 | 每页数量（默认20，最大100） |
| next | 否 | abc123 | 分页游标 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| code | number | 是 | 0表示成功 |
| message | string | 是 | 返回信息 |
| data | object | 是 | 返回数据 |
| ├─ list | array | 是 | 回收站文件列表 |
| ├─ next | string | 否 | 下一页游标 |
| ├─ hasMore | boolean | 是 | 是否有更多数据 |

#### 5. 清空回收站

**接口名称**: 清空回收站  
**接口路径**: `DELETE /api/v1/file/trash/clear`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

### 搜索功能

#### 1. 搜索文件

**接口名称**: 搜索文件  
**接口路径**: `GET /api/v1/file/search`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | - |
| Platform | open_platform | 是 | - |

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| keyword | 是 | test | 搜索关键词 |
| parentFileID | 否 | 0 | 搜索范围目录ID（可选） |
| limit | 否 | 20 | 每页数量（默认20，最大100） |
| next | 否 | abc123 | 分页游标 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| code | number | 是 | 0表示成功 |
| message | string | 是 | 返回信息 |
| data | object | 是 | 返回数据 |
| ├─ list | array | 是 | 搜索结果列表 |
| ├─ next | string | 否 | 下一页游标 |
| ├─ hasMore | boolean | 是 | 是否有更多数据 |

## 上传流程说明

### V2版本推荐流程

```
第三方客户端
123云盘服务器

1. 创建文件
   POST /upload/v2/file/create
   发送：parentFileID, filename, etag, size
2. 返回：preuploadID, upload domains
3. 上传分片
   POST {domain}/upload/v2/file/slice
   发送：preuploadID, sliceNo, sliceMD5, slice
4. 返回：200 OK
5. 上传完成
   POST /upload/v2/file/upload_complete
   发送：preuploadID
6. 返回：completed, fileID
```

### V1版本兼容流程

```
第三方客户端
123云盘服务器

1. 创建文件
   POST /upload/v1/file/create
2. 返回：preuploadID
3. 上传分片
   PUT /upload/v1/file/upload?preuploadID=xxx&partNumber=1
   Body: 二进制数据
4. 上传完成
   POST /upload/v1/file/upload_complete
5. 返回：fileID, completed
```

### 单步上传流程

对于小文件（<1GB V2，<100MB V1），可直接使用单步上传接口：
- V2: `POST /upload/v2/file/single/create`
- V1: `POST /upload/v1/file/upload`

## 限流规则

### API限流表
| API接口 | QPS限制 | 每分钟限制 | 备注 |
|---|---|---|---|
| 获取access_token | 1 | 60 | 单IP限制 |
| 创建文件 | 10 | 600 | - |
| 上传分片 | 50 | 3000 | - |
| 上传完成 | 10 | 600 | - |
| 单步上传 | 10 | 600 | - |
| 获取上传域名 | 10 | 600 | - |
| 文件重命名 | 10 | 600 | - |
| 批量重命名 | 5 | 300 | - |
| 创建目录 | 10 | 600 | - |
| 文件详情查询 | 10 | 600 | - |
| 文件列表查询 | 10 | 600 | - |
| 文件移动 | 10 | 600 | - |
| 文件复制 | 10 | 600 | - |
| 文件下载 | 10 | 600 | - |
| 文件分享 | 10 | 600 | - |
| 取消分享 | 10 | 600 | - |
| 获取分享列表 | 10 | 600 | - |
| 搜索文件 | 10 | 600 | - |
| 获取回收站列表 | 10 | 600 | - |
| 清空回收站 | 5 | 300 | - |
| 删除文件 | 10 | 600 | - |
| 恢复文件 | 10 | 600 | - |
| 彻底删除文件 | 10 | 600 | - |

### OAuth2授权相关限流
| 接口 | 频率限制 |
|---|---|
| 授权code获取access_token | 10次/分钟 |
| 刷新token | 10次/分钟 |

## 错误码说明

### 通用错误码
| 状态码 | 说明 | 解决方案 |
|---|---|---|
| 0 | 成功 | - |
| 400 | 参数错误 | 检查请求参数格式 |
| 401 | 未授权 | 检查access_token是否有效 |
| 403 | 权限不足 | 检查权限范围 |
| 404 | 资源不存在 | 检查文件ID是否正确 |
| 429 | 请求过于频繁 | 降低请求频率 |
| 500 | 服务器内部错误 | 稍后重试或联系客服 |

## 注意事项

### 文件上传注意事项
1. **版本选择**: 推荐使用V2版本接口，功能更完善
2. **分片上传**: 适合大文件上传，需先创建文件获取preuploadID和上传域名
3. **单步上传**: 仅适用于小文件（<1GB V2，<100MB V1）
4. **上传分片**: V2版本需要Authorization和Platform头，V1版本PUT上传不需要
5. **上传完成**: 分片上传后务必调用upload_complete接口确认上传完成
6. **域名获取**: 使用V2版本时，务必先获取上传域名
7. **文件名限制**: 文件名长度限制255字符，不能包含："\/:*?|><

### 文件操作注意事项
1. **批量操作限制**: 批量重命名、删除、恢复、移动等操作最多支持100个文件
2. **删除流程**: 文件删除分为两步：先移动到回收站，再彻底删除
3. **恢复文件**: 只能恢复回收站中的文件，恢复到删除前的位置
4. **下载地址**: 下载地址有效期为2小时，过期需重新获取
5. **分享功能**: 分享文件可设置过期时间，支持公开和私密分享
6. **搜索功能**: 支持按关键词搜索文件，可指定搜索范围
7. **回收站**: 清空回收站操作不可恢复，请谨慎操作

### 认证注意事项
1. 所有API请求必须携带有效的access_token
2. access_token有效期为2小时，过期需重新获取
3. 使用refresh_token可以刷新access_token，refresh_token有效期90天
4. client_secret必须安全保存，不能泄露

## 常见问题解答

### 1. 首次申请OpenAPI后没有收到邮件
- 检查邮箱填写是否正确
- 检查邮件是否在垃圾邮件中
- 联系客服处理

### 2. 忘记client_id或client_secret
- 联系客服处理
- 如发生泄漏，请立即联系客服更换client_secret

### 3. PUT请求没有返回值
- PUT请求上传文件分片时，上传完成后HTTP请求响应200即可，无任何返回值

### 4. PUT请求异常处理
- 提供截图给客服排查异常响应

### 5. PUT请求是否需要携带Authorization和Platform
- 不需要

### 6. 接口提示"tokens number has exceeded the limit"
- 账号登录数量过多，请及时退出登录以保证当前设备可用性

### 7. API是否有QPS限制
- 不同API的QPS限制不同，详见限流规则表

## 联系方式
- **客服邮箱**: 官方文档中提供的客服邮箱
- **技术支持**: 通过官方渠道联系技术支持团队

---
*最后更新时间：2024年*
*文档版本：v2.0*
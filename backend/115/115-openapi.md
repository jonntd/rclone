# 115开放平台 - 手机扫码授权PKCE模式 API文档

## 概述
此模式适用于无后端服务的第三方客户端，使用 OAuth 2.0 + PKCE 模式授权，无需提供 AppSecret。
整理
5. 生成Markdown格式文档## 授权流程
6. 保存到115-api.md文件中
7. 添加目录和索引
8. 包含示例代码和调用说明

 
### 流程图
```
第三方客户端
用户(115客户端)
115授权服务器
115资源服务器

设备授权流程(PKCE+两步轮询)
1. 生成 CODE_VERIFIER 和 CODE_CHALLENGE
2. 获取设备码和二维码内容
   发送CLIENT_ID,CODE_CHALLENGE 和 CODE_CHALLENGE_METHOD
3. 返回UID(设备码),QRCODE(二维码内容)和相关签名参数
4. 生成并显示二维码
5. 用户使用115客户端扫码并授权给应用
6. 用户授权成功
   [轮询授权状态]
   LOOP
7. 轮询授权状态
   发送UID和相关签名参数
8. 返回授权状态(等待/成功)
9. 换取访问令牌
   发送UID和CODE_VERIFIER
10. 返回访问令牌
11. 使用访问令牌访问资源
12. 返回资源数据
```

## API接口详情

### 1. 获取设备码和二维码内容

**接口名称：** 设备码方式授权  
**接口路径：** `POST https://passportapi.115.com/open/authDeviceCode`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Content-Type | application/x-www-form-urlencoded | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| client_id | text | 是 | - | APP ID |
| code_challenge | text | 是 | THHodGWg-FZfv8XYz7QArNGIK_aVomSHPldlSOTUtkw | PKCE 相关参数，计算如下：<br>$code_verifier = <43~128为随机字符串>;<br>$code_challenge = url_safe(base64_encode(sha256($code_verifier)));<br>注意 hash 的结果是二进制格式 |
| code_challenge_method | - | 是 | sha256 | 计算 code_challenge 的 hash算法，支持 md5, sha1, sha256 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 | 其他信息 |
|------|------|----------|------|----------|
| state | number | 非必须 | - | - |
| code | number | 非必须 | - | - |
| message | string | 非必须 | - | - |
| data | object | 非必须 | - | - |
| ├─ uid | string | 非必须 | 设备码 | - |
| ├─ time | number | 非必须 | 校验用的时间戳，轮询设备码状态用到 | - |
| ├─ qrcode | string | 非必须 | 二维码内容，第三方客户端需要根据此内容生成设备二维码，提供给115客户端扫码 | - |
| ├─ sign | string | 非必须 | 校验用的签名，轮询设备码状态用到 | - |
| error | string | 非必须 | - | - |
| errno | number | 非必须 | - | - |

### 2. 轮询二维码状态

**接口名称：** 轮询二维码状态  
**接口路径：** `GET https://qrcodeapi.115.com/get/status/`

> 此为长轮询接口。注意：当二维码状态没有更新时，此接口不会立即响应，直到接口超时或者二维码状态有更新。

#### 请求参数

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| uid | 是 | - | 二维码ID/设备码，从 /open/authDeviceCode 接口 data.uid 获取 |
| time | 是 | - | 校验参数，从 /open/authDeviceCode 接口 data.time 获取 |
| sign | 是 | - | 校验签名，从 /open/authDeviceCode 接口 data.sign 获取 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 | 其他信息 |
|------|------|----------|------|----------|
| state | number | 非必须 | 0.二维码无效，结束轮询；1.继续轮询； | - |
| code | number | 非必须 | - | - |
| message | string | 非必须 | - | - |
| data | object | 非必须 | 115客户端扫码或者输入设备码后才有值 | - |
| ├─ msg | string | 非必须 | 操作提示 | - |
| ├─ status | number | 非必须 | 二维码状态；1.扫码成功，等待确认；2.确认登录/授权，结束轮询; | - |
| ├─ version | string | 非必须 | - | - |

### 3. 获取 access_token

**接口名称：** 用设备码换取 access_token  
**接口路径：** `POST https://passportapi.115.com/open/deviceCodeToToken`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Content-Type | application/x-www-form-urlencoded | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| uid | text | 是 | - | 二维码ID/设备码 |
| code_verifier | text | 是 | IGKN6CJanWxCDPDhHZJrhswQdlcPBGLqExkhyujysXaQ4fJKBk_6dlPJo47s | 上一步计算 code_challenge 的原值 code_verifier |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 | 其他信息 |
|------|------|----------|------|----------|
| state | number | 非必须 | - | - |
| code | number | 非必须 | - | - |
| message | string | 非必须 | - | - |
| data | object | 非必须 | - | - |
| ├─ access_token | string | 非必须 | 用于访问资源接口的凭证 | - |
| ├─ refresh_token | string | 非必须 | 用于刷新 access_token，有效期1年 | - |
| ├─ expires_in | number | 非必须 | access_token 有效期，单位秒 | mock: 7200 |
| error | string | 非必须 | - | - |
| errno | number | 非必须 | - | - |

https://www.yuque.com/115yun/open/lvas49ar94n47bbk## 授权码模式

### 概述
本模式建议开发者服务端参与授权，适用于有后端服务的应用。

### 授权流程

#### 流程图
```
第三方服务端
115授权服务器
第三方客户端
115资源服务器

授权码授权流程
1. 授权码方式请求授权
2. 返回服务端生成的 STATE
3. /OPEN/AUTHORIZE 请求授权
   发送 CLIENT_ID, REDIRECT_URI, RESPONSE_TYPE, STATE
4. 判断已经登录115账号，重定向到开发者指定的REDIRECT_URI(一般为开发者服务端)
   重定向地址里带上授权码 CODE，开发者传的STATE
   如果没登录会跳转到登录页，完成登录后自动跳到第3步流程
5. 重定向到开发者自己的服务端
6. 验证STATE是否一致
7. /OPEN/AUTHCODE TOKEN 用授权码换取ACCESS TOKEN
   发送CLIENT_ID,CLIENT_SECRET,CODE,REDIRECT_URI,GRANT_TYPE
8. 返回访问令牌
9. 返回访问令牌
10. 使用访问令牌访问资源
11. 返回资源
```

### API接口详情

#### 1. 授权码方式请求授权

**接口路径：** `GET https://passportapi.115.com/open/authorize`

**接口描述：**
用户未登录情况下，会重定向到登录页面。
用户在已登录情况下，会自动完成授权并重定向到 redirect_uri 指定的地址

##### 请求参数

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| client_id | 是 | - | APP ID |
| redirect_uri | 是 | https://foo.com/bar | 授权成功后重定向到指定的地址(记得要urlencode处理一下)并附上授权码 code，如果本接口有传 state 参数，会附带到重定向地址去。重定向地址的域名，需要先到 https://open.115.com/ 应用管理应用域名设置 |
| response_type | 是 | code | 授权模式，固定为code，表示授权码模式 |
| state | 否 | 123456 | 随机值，会通过 redirect_uri 原样返回，防止CSRF攻击，强烈建议开发者带上并在请求换取access_token接口之前验证state是否一致 |

##### 返回数据（接口调用失败时候返回）
| 名称 | 类型 | 是否必须 | 备注 | 其他信息 |
|------|------|----------|------|----------|
| state | number | 非必须 | 0.失败；1.成功 | - |
| code | number | 非必须 | - | - |
| errno | number | 非必须 | - | - |
| data | object | 非必须 | - | - |
| message | string | 非必须 | - | - |
| error | string | 非必须 | - | - |

#### 2. 用授权码换取 access_token

**接口路径：** `POST https://passportapi.115.com/open/authCodeToToken`

**接口描述：**
强烈建议在服务端请求本接口，防止 APP Secret 泄露！

##### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Content-Type | application/x-www-form-urlencoded | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| client_id | text | 是 | - | APP ID |
| client_secret | text | 是 | - | APP Secret |
| code | text | 是 | - | 授权码，/open/authCodeToToken 重定向地址里面 |
| redirect_uri | text | 是 | https://foo.com?state=123456 | 与 /open/authCodeToToken 传的 redirect_uri 一致，防 MITM, CSRF |
| grant_type | text | 是 | authorization_code | 授权类型，固定为authorization_code，表示授权码类型 |

##### 返回数据
| 名称 | 类型 | 是否必须 | 备注 | 其他信息 |
|------|------|----------|------|----------|
| state | number | 非必须 | 0.失败；1.成功 | - |
| code | number | 非必须 | - | - |
| message | string | 非必须 | - | - |
| data | object | 非必须 | - | - |
| ├─ access_token | string | 非必须 | 用于访问资源接口的凭证 | - |
| ├─ refresh_token | string | 非必须 | 用于刷新access_token，有效期1年 | - |
| ├─ expires_in | number | 非必须 | access_token有效期，单位秒 | mock: 7200 |
| error | string | 非必须 | - | - |
| errno | number | 非必须 | - | - |

## 刷新 access_token

### 基本信息

**接口名称：** 刷新 access_token

**接口路径：** `POST https://passportapi.115.com/open/refreshToken`

**备注：** 请勿频繁刷新，否则列入频控。

### API接口详情

#### 1. 刷新 access_token

**接口路径：** `POST https://passportapi.115.com/open/refreshToken`

**接口描述：**
使用refresh_token刷新access_token，获取新的访问令牌

##### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Content-Type | application/x-www-form-urlencoded | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 备注 |
|----------|----------|----------|------|
| refresh_token | text | 是 | 刷新的凭证 |

##### 返回数据
| 名称 | 类型 | 是否必须 | 备注 | 其他信息 |
|------|------|----------|------|----------|
| state | number | 非必须 | 0.失败；1.成功 | - |
| code | number | 非必须 | - | - |
| message | string | 非必须 | - | - |
| data | object | 非必须 | - | - |
| ├─ access_token | string | 非必须 | 新的 access_token，同时刷新有效期 | - |
| ├─ refresh_token | string | 非必须 | 新的 refresh_token，有效期不延长不改变 | - |
| ├─ expires_in | number | 非必须 | access_token有效期，单位秒 | mock: 2592000 |
| error | string | 非必须 | - | - |
| errno | number | 非必须 | - | - |

## 用户管理

### 获取用户信息

**接口路径：** `GET https://webapi.115.com/open/user/info`

**接口描述：**
获取用户空间和VIP信息

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer {access_token} | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | boolean | 非必须 | 接口状态；true正常；false：异常 |
| message | string | 非必须 | 异常信息 |
| code | string | 非必须 | 异常码 |
| data | object | 非必须 | 数据 |
| ├─ user_id | string | 非必须 | 用户ID |
| ├─ user_name | string | 非必须 | 用户名称 |
| ├─ user_face_s | string | 非必须 | 小尺寸用户头像 |
| ├─ user_face_m | string | 非必须 | 中尺寸用户头像 |
| ├─ user_face_l | string | 非必须 | 大尺寸用户头像 |
| ├─ rt_space_info | object | 非必须 | 用户空间信息 |
| ├─ all_total | object | 非必须 | 用户总空间 |
| │ ├─ size | int | 非必须 | 用户总空间大小(字节) |
| │ ├─ size_format | string | 非必须 | 用户总空间大小(格式化) |
| ├─ all_remain | object | 非必须 | 用户剩余空间 |
| │ ├─ size | int | 非必须 | 用户剩余空间大小(字节) |
| │ ├─ size_format | string | 非必须 | 用户剩余空间大小(格式化) |
| ├─ all_use | object | 非必须 | 用户已使用空间 |
| │ ├─ size | int | 非必须 | 用户已使用空间大小(字节) |
| │ ├─ size_format | string | 非必须 | 用户已使用空间大小(格式化) |
| ├─ vip_info | object | 非必须 | 用户VIP等级信息 |
| │ ├─ level_name | string | 非必须 | VIP等级名称 |
| │ ├─ expire | int | 非必须 | VIP到期时间 |

## 文件管理

### 获取文件列表

**接口路径：** `GET https://webapi.115.com/open/ufile/files`

**接口描述：**
获取文件列表

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer {access_token} | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| cid | 否 | - | 目录ID，对应parent_id |
| type | 否 | - | 文件类型；1.文档；2.图片；3.音乐；4.视频；5.压缩；6.应用；7.书籍 |
| limit | 否 | - | 查询数量，默认20，最大1150 |
| offset | 否 | - | 查询起始位，默认0 |
| suffix | 否 | - | 文件后缀名 |
| asc | 否 | - | 排序，1：升序 0：降序 |
| o | 否 | - | 排序字段，file_name：文件名 file_size：文件大小 user_utime：更新时间 file_type 文件类型 |
| custom_order | 否 | - | 是否使用记忆排序。1 使用自定义排序，不使用记忆排序,0 使用记忆排序，自定义排序失效,2自定义排序，非文件夹置顶 |
| stdir | 否 | - | 筛选文件时，是否显示文件夹；1:要展示文件夹 0不展示 |
| star | 否 | - | 筛选星标文件，1:是 0全部 |
| cur | 否 | - | 是否只显示当前文件夹内文件 |
| show_dir | 否 | - | 是否显示目录；0 或 1，默认为0 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 | 其他信息 |
|------|------|----------|------|----------|
| data | object [] | - | - | item 类型: object |
| ├─ fid | string | - | 文件ID | - |
| ├─ aid | string | - | 文件的状态，aid 的别名。1 正常，7 删除(回收站)，120 彻底删除 | - |
| ├─ pid | string | - | 父目录ID | - |
| ├─ fc | string | - | 文件分类。0 文件夹，1 文件 | - |
| ├─ fn | string | - | 文件(夹)名称 | - |
| ├─ fco | string | - | 文件夹封面 | - |
| ├─ ism | string | - | 是否星标，1：星标 | - |
| ├─ isp | number | - | 是否加密；1：加密 | - |
| ├─ pc | string | - | 文件提取码 | - |
| ├─ upt | int | - | 修改时间 | - |
| ├─ uet | int | - | 修改时间 | - |
| ├─ uppt | int | - | 上传时间 | - |
| ├─ cm | number | - | - | - |
| ├─ fdesc | string | - | 文件备注 | - |
| ├─ ispl | number | - | 是否统计文件夹下视频时长开关 | - |
| ├─ fl | object [] | - | 文件标签 | item 类型: object |
| │ ├─ id | string | - | 文件标签id | - |
| │ ├─ name | string | - | 文件标签名称 | - |
| │ ├─ sort | string | - | 文件标签排序 | - |
| │ ├─ color | string | - | 文件标签颜色 | - |
| │ ├─ is_default | int | - | 文件标签类型；0：最近使用；1：非最近使用；2：为默认标签 | - |
| │ ├─ update_time | int | - | 文件标签更新时间 | - |
| │ ├─ create_time | int | - | 文件标签创建时间 | - |
| ├─ sha1 | string | - | sha1值 | - |
| ├─ fs | int | - | 文件大小 | - |
| ├─ fta | string | - | 文件状态 0/2 未上传完成，1 已上传完成 | - |
| ├─ ico | string | - | 文件后缀名 | - |
| ├─ fatr | string | - | 音频长度 | - |
| ├─ isv | number | - | 是否为视频 | - |
| ├─ def | number | - | 视频清晰度；1:标清 2:高清 3:超清 4:1080P 5:4k;100:原画 | - |
| ├─ def2 | number | - | 视频清晰度；1:标清 2:高清 3:超清 4:1080P 5:4k;100:原画 | - |
| ├─ play_long | number | - | 音视频时长 | - |
| ├─ v_img | string | - | - | - |
| ├─ thumb | string | - | 图片缩略图 | - |
| ├─ uo | string | - | 原图地址 | - |

### 获取文件(夹)详情

**接口路径：** `GET https://webapi.115.com/open/folder/get_info`

**接口描述：**
获取文件(夹)详情

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer {access_token} | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| file_id | 否 | 1288444975268439877 | 文件(夹)ID。和path需必传一个 |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| path | string | 否 | /a/b/c.png 或 >a>b>c | 文件路径；分隔符支持 / > 两种符号，最前面需分隔符开头，以分隔符分隔目录层级；和file_id必传一个 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | boolean | 非必须 | - |
| message | string | 非必须 | - |
| code | number | 非必须 | - |
| data | object | 非必须 | - |
| ├─ count | string | 非必须 | 包含文件总数量 |
| ├─ size | string | 非必须 | 文件(夹)总大小 |
| ├─ size_byte | int | 非必须 | 文件(夹)总大小(字节单位) |
| ├─ folder_count | string | 非必须 | 包含文件夹总数量 |
| ├─ play_long | number | 非必须 | 视频时长；-1：正在统计，其他数值为视频时长的数值(单位秒) |
| ├─ show_play_long | number | 非必须 | 是否开启展示视频时长 |
| ├─ ptime | string | 非必须 | 上传时间 |
| ├─ utime | string | 非必须 | 修改时间 |
| ├─ file_name | string | 非必须 | 文件名 |
| ├─ pick_code | string | 非必须 | 文件提取码 |
| ├─ sha1 | string | 非必须 | sha1值 |

### 获取文件下载地址

**接口路径：** `POST https://proapi.115.com/app/chrome/downurl`

**接口描述：**
获取文件的下载地址

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | 访问令牌 |
| Content-Type | application/x-www-form-urlencoded | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| pickcode | text | 是 | - | 文件提取码 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | number | 非必须 | 0.失败；1.成功 |
| data | object | 非必须 | 下载信息对象 |
| ├─ file_url | string | 非必须 | 文件下载URL |
| ├─ file_name | string | 非必须 | 文件名 |
| ├─ file_size | number | 非必须 | 文件大小 |

### 文件上传

#### 1. 获取上传凭证（断点续传初始化）

**接口名称：** 断点续传上传初始化调度接口  
**接口路径：** `POST https://webapi.115.com/open/upload/init`

**接口描述：**
断点续传上传初始化调度接口，支持秒传检测

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz | - |

**Body(form-data):**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| file_name | 是 | 图片.jpg | 文件名 |
| file_size | 是 | 5335 | 文件大小(字节) |
| target | 是 | U_1_0 | 文件上传目标约定：U_1_是固定约定，0代表根目录，其他数字是文件夹ID |
| fileid | 是 | - | 文件sha1值 |
| preid | 否 | - | 文件前128Ksha1 |
| pick_code | 否 | - | 上传任务key[非秒传的调度接口返回的pick_code字段] |
| topupload | 否 | 0 | 上传调度文件类型调度标记：0-单文件上传，1-文件夹任务第一个子文件，2-文件夹任务其余子文件，-1-无该参数 |
| sign_key | 否 | - | 二次认证需要 |
| sign_val | 否 | - | 二次认证需要(大写) |

#### 返回数据
| 名称 | 类型 | 备注 | 其他信息 |
|------|------|------|----------|
| state | boolean | 状态；true：正常；false：错误 | - |
| message | string | 异常信息 | - |
| code | number | 异常码 | - |
| data | object[] | - | - |
| ├─ pick_code | string | 上传任务唯一ID,用于续传 | - |
| ├─ status | number | 上传状态；1：非秒传；2：秒传 | - |
| ├─ sign_key | string | 本次计算的sha1标识（二次认证） | 参照二次认证说明 |
| ├─ sign_check | string | 本次计算本地文件sha1区间范围（二次认证） | 参照二次认证说明 |
| ├─ file_id | string | 秒传成功返回的新增文件ID | - |
| ├─ target | string | 文件上传目标约定 | - |
| ├─ bucket | string | 上传的bucket | - |
| ├─ object | string | OSS objectID | - |
| ├─ callback | object[] | 上传时间 | - |

#### 二次认证说明
| code | status | 说明 | 后续处理 | 第二次验证 |
|------|--------|------|----------|------------|
| 700 | 6 | 签名认证后失败 | sign_check（用下划线隔开，截取上传文内容的sha1） | 获取指定字节范围的sha1 |
| 701 | 7 | 需要认证签名 | - | sign_key + sign_val |
| 702 | 8 | 签名认证失败 | - | - |

#### 2. 断点续传

**接口名称：** 断点续传上传续传调度接口  
**接口路径：** `POST https://webapi.115.com/open/upload/resume`

**接口描述：**
断点续传上传续传调度接口

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz | - |

**Body(form-data):**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| file_size | 是 | 5335 | 文件大小(字节) |
| target | 是 | U_1_0 | 文件上传目标约定 |
| fileid | 是 | - | 文件sha1值 |
| pick_code | 是 | - | 上传任务key[非秒传的调度接口返回的pick_code字段] |

#### 返回数据
| 名称 | 类型 | 备注 | 其他信息 |
|------|------|------|----------|
| state | boolean | 状态；true：正常；false：错误 | - |
| message | string | 异常信息 | - |
| code | number | 异常码 | - |
| data | object[] | - | - |
| ├─ pick_code | string | 上传任务唯一ID,用于续传 | - |
| ├─ target | string | 文件上传目标约定 | - |
| ├─ version | string | 接口版本 | - |
| ├─ bucket | string | 上传的bucket | - |
| ├─ object | string | OSS objectID | - |
| ├─ callback | object[] | 上传时间 | - |

#### 3. 传统文件上传（简化版）

**接口路径：** `POST https://uplb.115.com/3.0/initupload.php`

**接口描述：**
初始化文件上传，支持秒传检测

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | 访问令牌 |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| filename | text | 是 | test.txt | 文件名 |
| filesize | text | 是 | 1024 | 文件大小 |
| target | text | 否 | U_0_0 | 目标目录ID |
| hash | text | 是 | abcdef123456 | 文件SHA1哈希 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | boolean | 非必须 | true成功，false失败 |
| data | object | 非必须 | 上传信息 |
| ├─ status | number | 非必须 | 状态：1秒传成功，2需要上传 |
| ├─ object_id | string | 非必须 | 文件对象ID |

## 视频播放

### 获取视频播放地址

**接口路径：** `POST https://proapi.115.com/app/chrome/video`

**接口描述：**
获取视频的播放地址和元数据

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | 访问令牌 |
| Content-Type | application/x-www-form-urlencoded | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| pickcode | text | 是 | - | 文件提取码 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | number | 非必须 | 0.失败；1.成功 |
| data | object | 非必须 | 视频信息 |
| ├─ video_url | string | 非必须 | 视频播放URL |
| ├─ thumb_url | string | 非必须 | 缩略图URL |
| ├─ duration | number | 非必须 | 视频时长（秒） |
| ├─ width | number | 非必须 | 视频宽度 |
| ├─ height | number | 非必须 | 视频高度 |

## 云下载

### 添加云下载任务

**接口路径：** `POST https://webapi.115.com/lixian/addtask`

**接口描述：**
添加BT、磁力链接等云下载任务

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | 访问令牌 |
| Content-Type | application/x-www-form-urlencoded | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| url | text | 是 | magnet:?xt=urn:btih:... | 下载链接（BT、磁力） |
| savepath | text | 否 | /Downloads | 保存路径 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | number | 非必须 | 0.失败；1.成功 |
| data | object | 非必须 | 任务信息 |
| ├─ task_id | string | 非必须 | 任务ID |
| ├─ task_name | string | 非必须 | 任务名称 |

### 获取云下载任务列表

**接口路径：** `GET https://webapi.115.com/lixian/?ct=lixian&ac=task_lists`

**接口描述：**
获取当前用户的云下载任务列表

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer {access_token} | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | number | 非必须 | 0.失败；1.成功 |
| data | object[] | 非必须 | 任务列表 |
| ├─ task_id | string | 非必须 | 任务ID |
| ├─ task_name | string | 非必须 | 任务名称 |
| ├─ progress | number | 非必须 | 下载进度百分比 |
| ├─ status | number | 非必须 | 任务状态：0等待，1下载中，2暂停，3完成，4失败 |
| ├─ create_time | string | 非必须 | 任务创建时间 |

## 文件管理扩展API

### 新建文件夹
**接口名称：** 新建文件夹  
**接口路径：** `POST https://webapi.115.com/open/folder/add`

**接口描述：**
创建新的文件夹

#### 请求参数
**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| pid | text | 是 | 3073323042143855813 | 新建文件夹所在的父目录ID (根目录的ID为0) |
| file_name | text | 是 | 新建文件夹名称 | 新建文件夹名称，限制255个字符 |

#### 返回数据
| 名称 | 类型 | 备注 | 其他信息 |
|------|------|------|----------|
| state | boolean | 接口状态；true正常；false：异常 | - |
| message | string | 异常信息 | - |
| code | number | 异常码 | - |
| data | object | - | - |
| ├─ file_name | string | 新建的文件夹名称 | - |
| ├─ file_id | string | 新建的文件夹ID | - |

### 文件操作

#### 文件移动
**接口描述：** 将文件或文件夹移动到指定目录
**接口路径：** `POST https://webapi.115.com/open/folder/move`

**请求参数：**
- **Headers:** Authorization: Bearer {access_token}
- **Body:**
  - file_id: 要移动的文件或文件夹ID（多个用逗号分隔）
  - target_id: 目标目录ID

#### 文件删除
**接口描述：** 将文件或文件夹移动到回收站
**接口路径：** `POST https://webapi.115.com/open/folder/del`

**请求参数：**
- **Headers:** Authorization: Bearer {access_token}
- **Body:**
  - file_id: 要删除的文件或文件夹ID（多个用逗号分隔）
  - pid: 父目录ID（可选）

#### 文件重命名
**接口描述：** 重命名文件或文件夹
**接口路径：** `POST https://webapi.115.com/open/folder/rename`

**请求参数：**
- **Headers:** Authorization: Bearer {access_token}
- **Body:**
  - file_id: 文件或文件夹ID
  - file_name: 新文件名

#### 文件搜索
**接口描述：** 搜索文件和文件夹
**接口路径：** `GET https://webapi.115.com/open/ufile/search`

**请求参数：**
- **Headers:** Authorization: Bearer {access_token}
- **Query:**
  - search_value: 搜索关键词
  - limit: 返回数量限制（默认20，最大1000）
  - offset: 偏移量（默认0）
  - type: 文件类型筛选（1-文档，2-图片，3-音乐，4-视频等）

### 回收站操作

#### 获取回收站列表
**接口描述：** 获取回收站中的文件列表
**接口路径：** `GET https://webapi.115.com/open/trash/list`

**请求参数：**
- **Headers:** Authorization: Bearer {access_token}
- **Query:**
  - limit: 返回数量限制
  - offset: 偏移量

#### 回收站恢复
**接口描述：** 从回收站恢复文件
**接口路径：** `POST https://webapi.115.com/open/trash/restore`

**请求参数：**
- **Headers:** Authorization: Bearer {access_token}
- **Body:**
  - file_id: 要恢复的文件ID（多个用逗号分隔）

#### 清空回收站
**接口描述：** 清空回收站中的所有文件
**接口路径：** `POST https://webapi.115.com/open/trash/clear`

**请求参数：**
- **Headers:** Authorization: Bearer {access_token}
- **Body:** 无
获取当前用户的云下载任务列表

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Authorization | Bearer {access_token} | 是 | 访问令牌 |

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| page | 否 | 1 | 页码 |
| limit | 否 | 20 | 每页数量 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | number | 非必须 | 0.失败；1.成功 |
| data | array | 非必须 | 任务列表 |
| ├─ task_id | string | 非必须 | 任务ID |
| ├─ task_name | string | 非必须 | 任务名称 |
| ├─ status | number | 非必须 | 任务状态 |
| ├─ progress | number | 非必须 | 下载进度 |

# 文件上传API文档

## 上传流程概述

115开放平台提供了完整的文件上传解决方案，支持秒传、普通上传、断点续传等多种上传方式。

### 上传流程图
```
客户端
115服务器
对象存储

文件上传完整流程
1. 请求文件上传接口初始化
2. 返回上传状态
   ├─ status=2: 秒传成功，流程结束
   ├─ status=1: 需要上传，返回callback
   └─ 需要二次认证: 返回认证要求
3. 获取上传凭证
4. 上传文件到对象存储
5. 返回上传结果
6. 上传完成

断点续传流程
1. 请求断点续传接口
2. 返回续传参数
3. 继续上传剩余部分
4. 完成上传
```

## 文件上传相关接口

### 1. 文件上传初始化

**接口名称：** 文件上传初始化  
**接口路径：** `POST https://webapi.115.com/files/upload`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Content-Type | application/x-www-form-urlencoded | 是 | - |
| Authorization | Bearer {access_token} | 是 | 访问令牌 |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| filename | text | 是 | test.jpg | 文件名 |
| filesize | number | 是 | 1024000 | 文件大小(字节) |
| target | string | 是 | U_1_0 | 上传目标目录ID |
| sign | string | 否 | - | 文件签名(用于秒传) |
| sign_key | string | 否 | - | 签名密钥 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 | 其他信息 |
|------|------|----------|------|----------|
| state | number | 必须 | 状态码 | 0:失败, 1:成功 |
| code | number | 必须 | 业务码 | - |
| message | string | 必须 | 返回信息 | - |
| data | object | 非必须 | 返回数据 | - |
| ├─ status | number | 必须 | 上传状态 | 1:需要上传, 2:秒传成功 |
| ├─ pickcode | string | 条件必须 | 文件提取码 | status=1时返回 |
| ├─ callback | object | 条件必须 | 上传回调参数 | status=1时返回 |
| ├─ file_id | string | 条件必须 | 文件ID | status=2时返回 |

### 2. 获取上传凭证

**接口名称：** 获取上传凭证  
**接口路径：** `POST https://uplb.115.com/3.0/getuploadinfo`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Content-Type | application/x-www-form-urlencoded | 是 | - |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| pickcode | string | 是 | abc123 | 从文件上传接口获取的提取码 |
| callback | object | 是 | - | 上传回调参数 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 | 其他信息 |
|------|------|----------|------|----------|
| state | number | 必须 | 状态码 | 0:失败, 1:成功 |
| data | object | 非必须 | 上传凭证信息 | - |
| ├─ endpoint | string | 必须 | 上传端点 | 对象存储地址 |
| ├─ policy | string | 必须 | 上传策略 | Base64编码 |
| ├─ signature | string | 必须 | 签名信息 | - |
| ├─ access_key | string | 必须 | 访问密钥 | - |
| ├─ key | string | 必须 | 对象键名 | 上传路径 |

### 3. 断点续传

**接口名称：** 断点续传  
**接口路径：** `POST https://webapi.115.com/files/resume_upload`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 备注 |
|----------|--------|----------|------|
| Content-Type | application/x-www-form-urlencoded | 是 | - |
| Authorization | Bearer {access_token} | 是 | 访问令牌 |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| pickcode | string | 是 | abc123 | 文件提取码 |
| filesize | number | 是 | 1024000 | 文件总大小 |
| offset | number | 是 | 524288 | 续传起始位置 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 | 其他信息 |
|------|------|----------|------|----------|
| state | number | 必须 | 状态码 | 0:失败, 1:成功 |
| data | object | 非必须 | 续传信息 | - |
| ├─ offset | number | 必须 | 续传位置 | 字节偏移量 |
| ├─ callback | object | 必须 | 上传回调参数 | - |

### 新建文件夹

**接口名称：** 新建文件夹  
**接口路径：** `POST https://webapi.115.com/files/add`

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Content-Type | application/x-www-form-urlencoded | 是 | - |
| Authorization | Bearer {access_token} | 是 | 访问令牌 |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| pid | string | 是 | U_1_0 | 父目录ID |
| cname | string | 是 | 新建文件夹 | 文件夹名称 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 | 其他信息 |
|------|------|----------|------|----------|
| state | number | 必须 | 状态码 | 0:失败, 1:成功 |
| data | object | 非必须 | 文件夹信息 | - |
| ├─ file_id | string | 必须 | 文件夹ID | - |
| ├─ file_name | string | 必须 | 文件夹名称 | - |

### 文件搜索

**接口路径：** `GET https://webapi.115.com/open/ufile/search`

**接口描述：**
文件搜索

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer {access_token} | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| search_value | 是 | test | 搜索关键词 |
| limit | 否 | 115 | 返回数量；默认：56 |
| offset | 否 | 0 | 偏移量；默认：0 |
| file_label | 否 | 0 | 文件标签；0.全部；1.文档；2.图片；3.音频；4.视频；5.压缩包；6.应用；7.书籍；8.其它 |
| cid | 否 | 0 | 文件夹ID；默认：0 |
| gte_day | 否 | 7 | 搜索创建时间范围(起始天数)；0.今天；1.昨天；7.近7天；30.近30天；365.近一年 |
| lte_day | 否 | 0 | 搜索创建时间范围(结束天数)；0.今天；1.昨天；7.近7天；30.近30天；365.近一年 |
| fc | 否 | 0 | 搜索范围；0.全部；1.仅文件名；2.仅备注 |
| type | 否 | 0 | 文件类型；0.全部；1.仅文件；2.仅文件夹 |
| suffix | 否 | txt | 文件后缀 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| count | int | 非必须 | 搜索到的文件总数量 |
| data | array | 非必须 | - |
| ├─ file_id | string | 非必须 | 文件(夹)ID |
| ├─ user_id | string | 非必须 | 用户ID |
| ├─ sha1 | string | 非必须 | 文件sha1值 |
| ├─ file_name | string | 非必须 | 文件名 |
| ├─ file_size | string | 非必须 | 文件大小 |
| ├─ user_ptime | string | 非必须 | 文件上传时间 |
| ├─ user_utime | string | 非必须 | 文件修改时间 |
| ├─ pick_code | string | 非必须 | 文件提取码 |
| ├─ parent_id | string | 非必须 | 父级文件夹ID |
| ├─ area_id | string | 非必须 | 区域ID |
| ├─ is_private | string | 非必须 | 是否私密；0.否；1.是 |

### 文件复制

**接口路径：** `POST https://webapi.115.com/open/ufile/copy`

**接口描述：**
复制文件

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer {access_token} | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| pid | string | 是 | 0 | 目标文件夹ID |
| file_id | string | 是 | 1288444975268439877 | 文件(夹)ID |
| nodupli | number | 否 | 1 | 是否跳过重复文件：0.重命名；1.跳过；2.覆盖；3.询问 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | int | 必须 | 状态码：0失败，1成功 |
| message | string | 必须 | 返回信息 |
| data | object | 非必须 | 复制结果信息 |

### 文件移动

**接口路径：** `POST https://webapi.115.com/open/ufile/move`

**接口描述：**
移动文件或文件夹到指定目录

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer {access_token} | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| pid | string | 是 | 0 | 目标文件夹ID |
| file_id | string | 是 | 1288444975268439877 | 文件(夹)ID |
| nodupli | number | 否 | 1 | 是否跳过重复文件：0.重命名；1.跳过；2.覆盖；3.询问 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | int | 必须 | 状态码：0失败，1成功 |
| message | string | 必须 | 返回信息 |
| data | object | 非必须 | 移动结果信息 |

### 文件删除

**接口路径：** `POST https://webapi.115.com/open/ufile/delete`

**接口描述：**
删除文件或文件夹

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer {access_token} | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| file_id | string | 是 | 1288444975268439877 | 文件(夹)ID，多个用逗号分隔 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | int | 必须 | 状态码：0失败，1成功 |
| message | string | 必须 | 返回信息 |
| data | object | 非必须 | 删除结果信息 |

### 文件重命名

**接口路径：** `POST https://webapi.115.com/open/ufile/rename`

**接口描述：**
重命名文件或文件夹

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer {access_token} | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| file_id | string | 是 | 1288444975268439877 | 文件(夹)ID |
| file_name | string | 是 | 新文件名 | 新的文件或文件夹名称 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | int | 必须 | 状态码：0失败，1成功 |
| message | string | 必须 | 返回信息 |
| data | object | 非必须 | 重命名结果信息 |

### 获取回收站列表

**接口路径：** `GET https://webapi.115.com/open/ufile/recycle`

**接口描述：**
获取回收站中的文件列表

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer {access_token} | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| limit | 否 | 56 | 返回数量；默认：56 |
| offset | 否 | 0 | 偏移量；默认：0 |
| cid | 否 | 0 | 文件夹ID；默认：0 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| count | int | 非必须 | 回收站文件总数量 |
| data | array | 非必须 | 回收站文件列表 |
| ├─ file_id | string | 非必须 | 文件(夹)ID |
| ├─ file_name | string | 非必须 | 文件名 |
| ├─ file_size | string | 非必须 | 文件大小 |
| ├─ delete_time | string | 非必须 | 删除时间 |

### 回收站恢复

**接口路径：** `POST https://webapi.115.com/open/ufile/recycle/restore`

**接口描述：**
从回收站恢复文件

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer {access_token} | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| file_id | string | 是 | 1288444975268439877 | 文件(夹)ID，多个用逗号分隔 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | int | 必须 | 状态码：0失败，1成功 |
| message | string | 必须 | 返回信息 |
| data | object | 非必须 | 恢复结果信息 |

### 清空回收站

**接口路径：** `POST https://webapi.115.com/open/ufile/recycle/clear`

**接口描述：**
清空回收站中的所有文件

#### 请求参数

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 | 备注 |
|----------|--------|----------|------|------|
| Authorization | Bearer {access_token} | 是 | Bearer abcdefghijklmnopqrstuvwxyz | 访问令牌 |

#### 返回数据
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | int | 必须 | 状态码：0失败，1成功 |
| message | string | 必须 | 返回信息 |
| data | object | 非必须 | 清空结果信息 |
| 名称 | 类型 | 是否必须 | 备注 |
|------|------|----------|------|
| state | boolean | 非必须 | - |
| message | string | 非必须 | - |
| code | number | 非必须 | - |
| data | object | 非必须 | - |

## 上传状态码说明

| 状态码 | 含义 | 说明 |
|--------|------|------|
| status=1 | 需要上传 | 文件需要正常上传 |
| status=2 | 秒传成功 | 文件已存在，秒传完成 |
| state=0 | 失败 | 请求失败 |
| state=1 | 成功 | 请求成功 |

## 二次认证说明

当文件上传接口返回需要二次认证时，需要：
1. 调用二次认证接口完成验证
2. 重新提交上传请求
3. 认证成功后继续上传流程

## 目录

- [手机扫码授权PKCE模式](#手机扫码授权pkce模式)
- [授权码模式](#授权码模式)
- [刷新 access_token](#刷新-accesstoken)
- [用户管理](#用户管理)
  - [获取用户信息](#获取用户信息)
- [文件管理](#文件管理)
  - [获取文件列表](#获取文件列表)
  - [获取文件(夹)详情](#获取文件夹详情)
  - [文件搜索](#文件搜索)
  - [文件复制](#文件复制)
  - [文件移动](#文件移动)
  - [文件删除](#文件删除)
  - [文件重命名](#文件重命名)
  - [获取文件下载地址](#获取文件下载地址)
  - [获取回收站列表](#获取回收站列表)
  - [回收站恢复](#回收站恢复)
  - [清空回收站](#清空回收站)
- [视频播放](#视频播放)
  - [获取视频播放地址](#获取视频播放地址)
- [云下载](#云下载)
  - [添加云下载任务](#添加云下载任务)
  - [获取云下载任务列表](#获取云下载任务列表)
- [文件上传API文档](#文件上传api文档)
  - [上传流程概述](#上传流程概述)
  - [文件上传初始化](#1-文件上传初始化)
  - [获取上传凭证](#2-获取上传凭证)
  - [断点续传](#3-断点续传)
  - [新建文件夹](#新建文件夹)
  - [上传状态码说明](#上传状态码说明)
  - [二次认证说明](#二次认证说明)

## 更新记录

| 更新时间 | 更新内容 |
|----------|----------|
| 2025年4月8日周二 | 使用MCP Puppeteer工具访问官方文档，新增文件移动、文件删除、文件重命名、回收站管理(获取回收站列表、恢复、清空)等API文档 |
| 2025年4月8日周二 | 新增文件搜索、文件复制、获取文件(夹)详情API文档 |
| 2025年4月7日周一 | 接口 /open/authDeviceCode code_challenge 参数兼容调整，兼容 url safe |
| 2025年4月7日周一 | 新增文件上传相关API接口文档 |

## 使用示例

### Python 示例代码

#### 1. 获取文件列表
```python
import requests

# 获取文件列表
url = "https://webapi.115.com/open/ufile/files"
headers = {
    "Authorization": f"Bearer {access_token}"
}
params = {
    "cid": "0",  # 根目录
    "limit": 50,
    "offset": 0,
    "show_dir": 1
}

response = requests.get(url, headers=headers, params=params)
data = response.json()
if data.get("state") == 1:
    files = data.get("data", [])
    for file in files:
        print(f"文件名: {file['file_name']}, 大小: {file['size']}")
```

#### 2. 文件上传流程
```python
import requests
import os

# 1. 初始化上传
init_url = "https://webapi.115.com/files/upload"
headers = {
    "Authorization": f"Bearer {access_token}",
    "Content-Type": "application/x-www-form-urlencoded"
}

data = {
    "filename": "test.txt",
    "filesize": os.path.getsize("test.txt"),
    "target": "U_1_0"  # 上传目录ID
}

response = requests.post(init_url, headers=headers, data=data)
result = response.json()

if result.get("data", {}).get("status") == 1:
    pickcode = result["data"]["pickcode"]
    print(f"需要上传，pickcode: {pickcode}")
    # 继续获取上传凭证并上传...
elif result.get("data", {}).get("status") == 2:
    print("文件已存在，秒传成功")
```

#### 3. 文件搜索
```python
search_url = "https://webapi.115.com/open/ufile/search"
params = {
    "search_value": "文档",
    "limit": 20,
    "file_label": 1,  # 文档类型
    "type": 1  # 仅文件
}

response = requests.get(search_url, headers=headers, params=params)
results = response.json()
```

### 错误处理

#### 常见错误码
| 错误码 | 描述 | 解决方案 |
|--------|------|----------|
| 401 | 未授权 | 检查access_token是否有效 |
| 403 | 权限不足 | 检查应用权限设置 |
| 404 | 资源不存在 | 检查文件ID是否正确 |
| 429 | 请求过于频繁 | 降低请求频率 |
| 500 | 服务器错误 | 稍后重试 |

### 最佳实践

1. **Token管理**
   - 缓存access_token，避免频繁刷新
   - 在token过期前使用refresh_token获取新token
   - 实现自动重试机制

2. **分页处理**
   - 使用limit和offset进行分页
   - 合理设置每页大小(建议50-100条)
   - 处理大数据集时考虑异步加载

3. **文件操作**
   - 批量操作时使用逗号分隔多个文件ID
   - 操作前验证文件存在性
   - 重要操作前先备份

4. **上传优化**
   - 使用秒传功能减少上传时间
   - 大文件使用分片上传
   - 实现断点续传功能

---

## 补充API文档（基于官方最新文档整理）

### 详细文件管理API

#### 获取文件列表（详细版）
**接口路径：** `GET https://webapi.115.com/open/folder/files`

**请求参数：**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| cid | 否 | 0 | 文件夹ID；默认：0（根目录） |
| type | 否 | 0 | 文件类型；0.全部；1.仅文件；2.仅文件夹 |
| limit | 否 | 56 | 返回数量；默认：56 |
| offset | 否 | 0 | 偏移量；默认：0 |
| suffix | 否 | txt | 文件后缀；支持多个后缀，用逗号分隔 |
| asc | 否 | 1 | 排序方式；0.降序；1.升序 |
| o | 否 | file_name | 排序字段；file_name.文件名；user_ptime.上传时间；file_size.文件大小 |
| custom_order | 否 | - | 自定义排序 |
| stdir | 否 | 0 | 是否显示文件夹；0.不显示；1.显示 |
| star | 否 | 0 | 是否星标；0.全部；1.星标文件 |
| cur | 否 | 0 | 当前页码 |
| show_dir | 否 | 1 | 是否显示文件夹；0.不显示；1.显示 |

#### 获取用户信息（补充）
**接口路径：** `GET https://webapi.115.com/open/user/info`

**请求参数：**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| Authorization | 是 | Bearer xxx | 访问令牌 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态 |
| message | string | 消息 |
| code | number | 状态码 |
| data | object | 用户数据 |
| ├─ user_id | string | 用户ID |
| ├─ user_name | string | 用户名 |
| ├─ user_face_s | string | 小头像 |
| ├─ user_face_m | string | 中头像 |
| ├─ user_face_l | string | 大头像 |
| ├─ rt_space_info | object | 空间信息 |
| ├─ all_total | string | 总空间 |
| ├─ all_remain | string | 剩余空间 |
| ├─ all_use | string | 已用空间 |
| ├─ vip_info | object | VIP信息 |
| ├─ level_name | string | 等级名称 |
| ├─ expire | string | 到期时间 |

### 二次认证详细说明

当上传接口返回需要二次认证时，响应格式：
```json
{
  "state": true,
  "data": {
    "status": 1,
    "sign_key": "abc123",
    "sign_check": "0-1024"
  }
}
```

**二次认证流程：**
1. 获取sign_key和sign_check参数
2. 根据sign_check指定的文件范围计算sha1
3. 使用sign_key和计算结果进行认证
4. 重新提交上传请求

### 上传状态码详细说明

| 状态码 | 含义 | 说明 | 处理建议 |
|--------|------|------|----------|
| status=1 | 需要上传 | 文件需要正常上传 | 继续获取上传凭证 |
| status=2 | 秒传成功 | 文件已存在，秒传完成 | 上传完成 |
| status=3 | 需要二次认证 | 触发安全验证 | 完成二次认证后继续 |
| state=0 | 失败 | 请求失败 | 检查参数和权限 |
| state=1 | 成功 | 请求成功 | 继续后续流程 |

### 文件类型映射表

| 类型值 | 文件类型 | 描述 |
|--------|----------|------|
| 0 | 全部 | 所有文件 |
| 1 | 文档 | txt, doc, pdf等 |
| 2 | 图片 | jpg, png, gif等 |
| 3 | 音频 | mp3, wav等 |
| 4 | 视频 | mp4, avi等 |
| 5 | 压缩包 | zip, rar等 |
| 6 | 应用 | exe, apk等 |
| 7 | 书籍 | epub, mobi等 |
| 8 | 其他 | 其他类型 |

### 错误码详细说明

| 错误码 | 描述 | 解决方案 |
|--------|------|----------|
| 10001 | 参数错误 | 检查请求参数格式 |
| 10002 | 权限不足 | 检查应用权限设置 |
| 10003 | 文件不存在 | 检查文件ID是否正确 |
| 10004 | 空间不足 | 清理空间或升级套餐 |
| 10005 | 文件已存在 | 重命名或覆盖上传 |
| 10006 | 文件夹不存在 | 检查目标文件夹ID |
| 10007 | 文件名非法 | 检查文件名格式 |
| 10008 | 文件大小超限 | 检查文件大小限制 |
| 10009 | 上传频率限制 | 降低上传频率 |
| 10010 | 二次认证失败 | 重新计算文件签名 |

---

*本文档基于115开放平台官方文档整理，最后更新时间：2025-04-08*

*补充文档基于官方最新API文档整理，更新时间：2025-04-08*

---

## 云下载相关API文档

### 获取用户云下载任务列表

**接口路径：** `GET 域名 + /open/offline/get_task_list`

**官方文档：** https://www.yuque.com/115yun/open/av2mluz7uwigz74k

**接口描述：** 获取用户云下载任务列表

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| page | 否 | 1 | 页码，默认为1 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态；true：正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | object | 数据 |
| ├─ page | number | 当前页码 |
| ├─ page_count | number | 总页数 |
| ├─ count | number | 总任务数 |
| ├─ tasks | array | 任务列表 |
| ├─ info_hash | string | 任务sha1 |
| ├─ add_time | number | 任务添加时间戳 |
| ├─ percentDone | string | 任务完成百分比 |
| ├─ size | string | 任务大小 |
| ├─ name | string | 任务名称 |
| ├─ last_update | number | 最后更新时间戳 |
| ├─ file_id | string | 文件id |
| ├─ delete_file_id | string | 删除文件id |
| ├─ status | number | 任务状态 |
| ├─ url | string | 任务下载地址 |
| ├─ wp_path_id | string | 保存目录id |
| ├─ def2 | number | 默认标记 |
| ├─ play_long | number | 播放时长 |
| ├─ can_appeal | number | 是否可申诉 |

### 获取云下载配额信息

**接口路径：** `GET 域名 + /open/offline/get_quota_info`

**官方文档：** https://www.yuque.com/115yun/open/gif2n3smh54kyg0p

**接口描述：** 获取当前用户各个配额类型明细

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态；true：正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | object | 数据 |
| ├─ package | array | 配额类型明细 |
| ├─ surplus | number | 剩余配额数量 |
| ├─ used | number | 已用配额数量 |
| ├─ count | number | 配额总数 |
| ├─ name | string | 配额类型名称 |
| ├─ expire_info | string | 过期信息 |
| ├─ total_surplus | number | 用户总剩余配额数量 |
| ├─ total_used | number | 用户总已用配额数量 |
| ├─ total_count | number | 用户总配额数量 |

### 清空云下载任务

**接口路径：** `POST 域名 + /open/offline/clear_task`

**官方文档：** https://www.yuque.com/115yun/open/uu5i4urb5ylqwfy4

**接口描述：** 清空云下载任务

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| flag | number | 是 | 0 | 清除任务类型：0清空已完成、1清空全部、2清空失败、3清空进行中、4清空已完成任务并清空对应源文件、5清空全部任务并清空对应源文件 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态；true：正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | object | 数据 |

### 添加云下载链接任务

**接口路径：** `POST 域名 + /open/offline/add_task_urls`

**官方文档：** https://www.yuque.com/115yun/open/zkyfq2499gdn3mty

**接口描述：** 添加云下载链接任务，支持HTTP(S)、FTP、磁力链和电驴链接

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| urls | string | 是 | ["magnet:?xt=urn:btih:..."] | 下载链接数组，JSON字符串格式 |
| wp_path_id | string | 否 | 0 | 保存目录id，默认为0（根目录） |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态；true：正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | array | 数据 |
| ├─ state | boolean | 链接任务添加状态 |
| ├─ code | number | 状态码 |
| ├─ message | string | 状态描述 |
| ├─ info_hash | string | 任务sha1 |
| ├─ url | string | 任务url |

### 删除用户云下载任务

**接口路径：** `POST 域名 + /open/offline/del_task`

**官方文档：** https://www.yuque.com/115yun/open/pmgwc86lpcy238nw

**接口描述：** 删除用户云下载任务

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| info_hash | string | 是 | abcdefghijklmnopqrstuvwxyz1234567890abcd | 任务sha1，多个用逗号分隔 |
| del_source_file | number | 否 | 0 | 是否删除源文件：0不删除，1删除 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态；true：正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | object | 数据 |

### 添加云下载BT任务

**接口路径：** `POST 域名 + /open/offline/add_task_bt`

**官方文档：** https://www.yuque.com/115yun/open/svfe4unlhayvluly

**接口描述：** 添加云下载BT任务

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| info_hash | string | 是 | abcdefghijklmnopqrstuvwxyz1234567890abcd | 任务sha1 |
| wanted | array | 是 | [0,1,2] | 选择的文件索引数组，JSON字符串格式 |
| save_path | string | 否 | /下载/test | 保存路径，默认为根目录 |
| torrent_sha1 | string | 是 | abcdefghijklmnopqrstuvwxyz1234567890abcd | BT种子文件sha1 |
| pick_code | string | 是 | abcdefghijklmnopqrstuvwxyz1234567890abcd | BT种子文件提取码 |
| wp_path_id | string | 否 | 0 | 保存目录id，与save_path二选一，优先使用wp_path_id |

**注意事项：**
- `wp_path_id` 和 `save_path` 二选一，优先使用 `wp_path_id`
- 如果都不传，默认保存到根目录

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态；true：正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | object | 数据 |

## 授权相关API文档

### 手机扫码授权PKCE模式

#### 概述
PKCE（Proof Key for Code Exchange）是一种更安全的授权模式，适用于移动应用和单页应用。该模式通过扫码授权，无需输入账号密码。

#### 授权流程
1. 生成CODE_VERIFIER和CODE_CHALLENGE
2. 获取设备码和二维码内容
3. 轮询授权状态
4. 换取访问令牌

#### 获取设备码和二维码内容

**接口路径：** `POST https://passportapi.115.com/open/authDeviceCode`

**官方文档：** https://www.yuque.com/115yun/open/shtpzfhewv5nag11

**接口描述：** 获取设备码和二维码内容

**请求参数：**

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| client_id | string | 是 | your_client_id | 应用ID |
| code_challenge | string | 是 | abcdefghijklmnopqrstuvwxyz | 由CODE_VERIFIER生成的挑战码 |
| code_challenge_method | string | 是 | S256 | 固定值：S256 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态；true：正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | object | 数据 |
| ├─ uid | string | 设备uid |
| ├─ time | number | 时间戳 |
| ├─ sign | string | 签名 |
| ├─ qrcode_content | string | 二维码内容 |

#### 轮询二维码状态

**接口路径：** `GET https://qrcodeapi.115.com/get/status/`

**官方文档：** https://www.yuque.com/115yun/open/shtpzfhewv5nag11

**接口描述：** 轮询二维码授权状态

**请求参数：**

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| uid | 是 | device_uid | 设备uid |
| time | 是 | 1234567890 | 时间戳 |
| sign | 是 | signature | 签名 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态；true：正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | object | 数据 |
| ├─ status | number | 二维码状态：0待扫描，1已扫描待确认，2已确认，-1已过期 |

#### 获取access_token

**接口路径：** `POST https://passportapi.115.com/open/deviceCodeToToken`

**官方文档：** https://www.yuque.com/115yun/open/shtpzfhewv5nag11

**接口描述：** 用设备码换取访问令牌

**请求参数：**

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| uid | string | 是 | device_uid | 设备uid |
| code_verifier | string | 是 | abcdefghijklmnopqrstuvwxyz | 原始CODE_VERIFIER |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态；true：正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | object | 数据 |
| ├─ access_token | string | 访问令牌 |
| ├─ refresh_token | string | 刷新令牌 |
| ├─ expires_in | number | 令牌有效期（秒） |

### 授权码模式

#### 概述
授权码模式适用于有服务端的应用，通过授权码换取访问令牌。

#### 授权流程
1. 授权码方式请求授权
2. 返回服务端生成的STATE
3. `/open/authorize` 请求授权
4. 重定向到 `redirect_uri` 并带上授权码CODE和STATE
5. 验证STATE
6. `/open/authCodeToToken` 用授权码换取ACCESS TOKEN
7. 返回访问令牌
8. 使用访问令牌访问资源

#### 授权码方式请求授权

**接口路径：** `GET 域名 + /open/authorize`

**官方文档：** https://www.yuque.com/115yun/open/okr2cq0wywelscpe

**接口描述：** 授权码方式请求授权

**请求参数：**

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| client_id | 是 | your_client_id | 应用ID |
| redirect_uri | 是 | https://yourapp.com/callback | 重定向URI |
| response_type | 是 | code | 固定值：code |
| state | 是 | random_state | 随机字符串，用于防止CSRF攻击 |

**返回数据：**
重定向到指定的redirect_uri，并带上参数：
- code: 授权码
- state: 原样返回的state参数

#### 用授权码换取access_token

**接口路径：** `POST 域名 + /open/authCodeToToken`

**官方文档：** https://www.yuque.com/115yun/open/okr2cq0wywelscpe

**接口描述：** 用授权码换取访问令牌

**请求参数：**

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| client_id | string | 是 | your_client_id | 应用ID |
| client_secret | string | 是 | your_client_secret | 应用密钥 |
| code | string | 是 | authorization_code | 授权码 |
| redirect_uri | string | 是 | https://yourapp.com/callback | 重定向URI |
| grant_type | string | 是 | authorization_code | 固定值：authorization_code |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态；true：正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | object | 数据 |
| ├─ access_token | string | 访问令牌 |
| ├─ refresh_token | string | 刷新令牌 |
| ├─ expires_in | number | 令牌有效期（秒） |

### 刷新access_token

**接口路径：** `POST https://passportapi.115.com/open/refreshToken`

**官方文档：** https://www.yuque.com/115yun/open/opnx8yezo4at2be6

**接口描述：** 刷新访问令牌

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Content-Type | application/x-www-form-urlencoded | 是 | application/x-www-form-urlencoded |

**Body:**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| refresh_token | string | 是 | refresh_token_string | 刷新令牌 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态；true：正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | object | 数据 |
| ├─ access_token | string | 新的访问令牌 |
| ├─ refresh_token | string | 新的刷新令牌 |
| ├─ expires_in | number | 令牌有效期（秒） |

## 上传流程

**官方文档：** https://www.yuque.com/115yun/open/xb89onhdxsfpwsyc

### 概述
上传流程包含五个步骤：
1. 请求【文件上传】接口初始化上传文件（秒传或返回预上传callback）
2. 处理二次认证
3. 非秒传时，携带callback和参数调用【获取上传凭证】接口，然后调用【对象存储】开始上传
4. 断点续传时，携带pickcode和文件信息请求【断点续传】接口获取callback和上传凭证数据，然后调用【对象存储】开始上传
5. 【对象存储】返回上传成功

### 流程图
```
开发者 → 115: 1. 请求文件上传接口
115 → 开发者: 返回秒传结果或预上传callback
开发者 → 115: 2. 处理二次认证（如需要）
开发者 → 115: 3. 获取上传凭证
115 → 开发者: 返回上传凭证
开发者 → 对象存储: 4. 开始上传文件
对象存储 → 开发者: 返回上传成功
```

### 交互说明
- **开发者**：调用API的应用程序
- **115**：115云盘服务端
- **对象存储**：115使用的云存储服务

---

*本文档基于115开放平台官方API文档整理，最后更新时间：2025-04-08*

*文档包含：云下载API、授权API、上传流程等完整接口信息*

### 文件复制

**接口路径：** `POST 域名 + /open/ufile/copy`

**接口描述：** 批量复制文件

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| pid | text | 是 | 1054251402869818368 | 目标目录，即所需移动到的目录 |
| file_id | text | 是 | 2323423573680609857 | 所复制的文件和目录ID，多个文件和目录请以 , 隔开 |
| nodupli | text | 否 | 1 | 复制的文件在目标目录是否允许重复，默认0：0：可以；1：不可以 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 接口状态；true正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | array | 数据 |

### 文件移动

**接口路径：** `POST 域名 + /open/ufile/move`

**接口描述：** 批量移动文件

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| file_ids | string | 是 | 3073323042143855813,3073323042143855822 | 需要移动的文件(夹)ID |
| to_cid | string | 是 | 3073311192547189943 | 要移动所在的目录ID，根目录为0 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 接口状态；true正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | array | 数据 |

### 获取文件下载地址

**接口路径：** `POST 域名 + /open/ufile/downurl`

**接口描述：** 根据文件提取码取文件下载地址

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | - |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| pick_code | text | 是 | dtctprlmfkl4exiok | 文件提取码 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态 |
| message | string | 消息 |
| code | number | 状态码 |
| data | object | 数据 |
| ├─ [file_id] | object | 文件ID |
| ├─ file_name | string | 文件名 |
| ├─ file_size | number | 文件大小 |
| ├─ pick_code | string | 文件提取码 |
| ├─ sha1 | string | 文件sha1值 |
| ├─ url | object | 下载地址信息 |
| ├─ url | string | 文件下载地址 |

### 文件(夹)更新

**接口路径：** `POST 域名 + /open/ufile/update`

**接口描述：** 更新文件名或星标文件

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| file_id | text | 是 | 3073323042143855813 | 需要更改名字的文件(夹)ID |
| file_name | text | 否 | 新的名字 | 新的文件(夹)名字(文件夹名称限制255字节) |
| star | text | 否 | 1 | 是否星标；1：星标；0：取消星标 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态；true：正常；false：异常 |
| message | string | 异常信息 |
| code | number | 异常码 |
| data | object | 数据 |
| ├─ file_name | string | 新的文件(夹)名字 |
| ├─ star | string | 文件星标状态 |

### 删除文件

**接口路径：** `POST 域名 + /open/ufile/delete`

**接口描述：** 批量删除文件(夹)

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| file_ids | text | 是 | 3073323042143855813,3073323042143855822 | 需要删除的文件(夹)ID |
| parent_id | text | 否 | 3073311192547189943 | 删除的文件(夹)ID所在的父目录ID |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 接口状态；true正常；false：异常 |
| message | string | 异常信息 |
| code | string | 异常码 |
| data | string[] | 数据 |

### 回收站列表

**接口路径：** `GET 域名 + /open/rb/list`

**接口描述：** 回收站列表

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Query:**
| 参数名称 | 是否必须 | 示例 | 备注 |
|----------|----------|------|------|
| limit | 是 | 30 | 单页记录数，int，默认30，最大200 |
| offset | 是 | 0 | 数据显示偏移量 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态 |
| message | string | 消息 |
| code | number | 状态码 |
| data | object | 数据 |
| ├─ offset | number | 偏移量 |
| ├─ limit | number | 分页量 |
| ├─ count | string | 回收站文件总数 |
| ├─ rb_pass | number | 是否设置回收站密码 |
| ├─ [file_id] | object | 文件(夹)回收站ID |
| ├─ id | string | 文件(夹)回收站ID |
| ├─ file_name | string | 文件(夹)名称 |
| ├─ type | string | 类型（1：文件，2：目录） |
| ├─ file_size | string | 文件大小 |
| ├─ dtime | string | 删除日期 |
| ├─ thumb_url | string | 缩略图地址 |
| ├─ status | string | 还原状态，-1 表示还原中，0 表示正常状态 |
| ├─ cid | number | 原文件的父目录id |
| ├─ parent_name | string | 原文件的父目录名称 |
| ├─ pick_code | string | 文件提取码 |

### 回收站还原

**接口路径：** `POST 域名 + /open/rb/revert`

**接口描述：** 还原回收站文件(夹)

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| tid | text | 是 | 111,222,333,444 | 需要还原的ID，可多个，用半角逗号分开，最多1150个 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| data | object | 数据 |
| ├─ [recycle_id] | object | 还原的回收站ID |
| ├─ state | boolean | 状态 |
| ├─ error | string | 错误信息 |
| ├─ errno | number | 错误码 |
| state | boolean | 状态 |
| message | string | 消息 |
| code | number | 状态码 |

### 删除/清空回收站

**接口路径：** `POST 域名 + /open/rb/del`

**接口描述：** 批量删除回收站文件、清空回收站

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 示例 | 备注 |
|----------|----------|----------|------|------|
| tid | text | 否 | 1,2,3,4 | 需要删除的文件的Id,如若不传就是清空回收站(最多支持1150个) |

**返回数据：**
| 名称 | 类型 | 是否必须 | 默认值 | 备注 |
|------|------|----------|--------|------|
| state | boolean | 非必须 | - | 接口状态；true正常；false：异常 |
| message | string | 非必须 | - | 异常信息 |
| code | number | 非必须 | - | 异常码 |
| data | string[] | 非必须 | - | 数据 |

### 解析BT种子

**接口路径：** `POST 域名 + /open/offline/torrent`

**官方文档：** https://www.yuque.com/115yun/open/svfe4unlhayvluly

**接口描述：** 解析BT种子

**请求参数：**

**Headers:**
| 参数名称 | 参数值 | 是否必须 | 示例 |
|----------|--------|----------|------|
| Authorization | Bearer access_token | 是 | Bearer abcdefghijklmnopqrstuvwxyz |

**Body(form-data):**
| 参数名称 | 参数类型 | 是否必须 | 备注 |
|----------|----------|----------|------|
| torrent_sha1 | string | 是 | BT种子文件sha1，需先上传到"云下载/种子文件"文件夹下(非硬性要求) |
| pick_code | string | 是 | BT种子文件提取码 |

**返回数据：**
| 名称 | 类型 | 备注 |
|------|------|------|
| state | boolean | 状态 |
| message | string | 消息 |
| code | number | 状态码 |
| data | array | 数据 |
| ├─ file_size | int | 任务大小 |
| ├─ torrent_name | string | 任务名 |
| ├─ file_count | int | 文件数 |
| ├─ info_hash | string | 任务sha1 |
| ├─ torrent_filelist | array | 文件列表 |
| ├─ size | int | 文件大小 |
| ├─ path | string | 文件路径 |
| ├─ wanted | int | 文件是否默认选中 |

---

*本文档基于115开放平台官方文档整理，最后更新时间：2025-04-08*

*补充文档基于官方最新API文档整理，更新时间：2025-04-08*
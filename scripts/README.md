# STRM-Mount 卸载脚本

这些脚本用于安全地卸载所有 STRM 挂载点并终止相关进程。

## 📁 文件说明

| 文件 | 平台 | 描述 |
|------|------|------|
| `unmount-strm.sh` | Linux/macOS | Bash 脚本，支持所有 Unix-like 系统 |
| `unmount-strm.bat` | Windows | 批处理脚本，兼容性最好 |
| `Unmount-STRM.ps1` | Windows | PowerShell 脚本，功能最强大 |

## 🚀 快速使用

### Linux/macOS
```bash
# 交互式卸载
./scripts/unmount-strm.sh

# 强制卸载（无需确认）
./scripts/unmount-strm.sh -f

# 详细模式
./scripts/unmount-strm.sh -v
```

### Windows (批处理)
```cmd
REM 交互式卸载
scripts\unmount-strm.bat

REM 强制卸载
scripts\unmount-strm.bat -f
```

### Windows (PowerShell)
```powershell
# 交互式卸载
.\scripts\Unmount-STRM.ps1

# 强制卸载
.\scripts\Unmount-STRM.ps1 -Force

# 预览模式（不实际执行）
.\scripts\Unmount-STRM.ps1 -WhatIf
```

## 🔧 功能特性

### 共同功能
- ✅ **智能检测**: 自动发现所有 STRM 相关挂载点和进程
- ✅ **安全卸载**: 优雅终止 → 强制终止 → 懒惰卸载的三级策略
- ✅ **进程管理**: 优雅终止 rclone 进程，失败时强制终止
- ✅ **临时清理**: 自动清理临时文件和目录
- ✅ **结果验证**: 验证清理结果，报告残留项目
- ✅ **彩色输出**: 清晰的彩色日志输出
- ✅ **错误处理**: 完善的错误处理和恢复机制

### 平台特定功能

#### Linux/macOS (Bash)
- 🔍 **挂载点检测**: 检测 FUSE 挂载点
- 🔒 **权限检查**: 检测 root 权限
- 📊 **详细统计**: 显示清理统计信息
- 🧹 **深度清理**: 清理 `/tmp` 下的所有相关文件

#### Windows (PowerShell)
- 🔍 **多种挂载**: 支持网络驱动器和 WinFsp 挂载点
- 👑 **管理员检测**: 检测管理员权限
- 📋 **WhatIf 支持**: 预览模式，不实际执行操作
- 🎯 **精确匹配**: 使用 WMI 精确查找挂载点
- 🔧 **WinFsp 集成**: 原生支持 WinFsp 挂载点管理

## 📋 命令行选项

### 通用选项
| 选项 | 长选项 | 描述 |
|------|--------|------|
| `-h` | `--help` | 显示帮助信息 |
| `-f` | `--force` | 强制模式，跳过确认提示 |
| `-v` | `--verbose` | 详细模式，显示更多信息 |
| `-q` | `--quiet` | 安静模式，只显示错误信息 |

### PowerShell 专用选项
| 选项 | 描述 |
|------|------|
| `-WhatIf` | 预览模式，显示将要执行的操作但不实际执行 |

## 🔍 检测范围

### 挂载点检测
- **Linux/macOS**: 检测包含 `strm`、`123:`、`115:` 的 FUSE 挂载点
- **Windows**: 检测网络驱动器和 WinFsp 挂载点

### 进程检测
- 查找所有 `rclone` 进程中包含 `strm-mount` 的进程
- 显示进程 PID 和命令行参数

### 临时文件清理
- **Linux/macOS**: `/tmp/strm-*`、`/tmp/*strm*.log`、`/tmp/*cache*.log`
- **Windows**: `%TEMP%\strm-*`、`%SystemRoot%\Temp\strm-*`、相关日志文件

## 🛡️ 安全特性

### 三级卸载策略
1. **优雅卸载**: 使用标准 `umount` 命令
2. **强制卸载**: 使用 `umount -f` 强制卸载
3. **懒惰卸载**: 使用 `umount -l` 延迟卸载

### 进程终止策略
1. **优雅终止**: 发送 SIGTERM 信号，等待进程自然退出
2. **强制终止**: 发送 SIGKILL 信号强制终止

### 权限检查
- 检测当前用户权限级别
- 对于需要特殊权限的操作给出警告

## 📊 输出示例

### 成功输出
```
╔══════════════════════════════════════════════════════════════╗
║                    STRM-Mount 卸载脚本                        ║
║                                                              ║
║  功能：安全卸载所有 STRM 挂载点并终止相关进程                  ║
║  版本：1.0.0                                                 ║
╚══════════════════════════════════════════════════════════════╝

[STEP] 🔍 查找 STRM 相关挂载点...
[INFO] 发现以下 STRM 挂载点：
  → /tmp/strm-123-test
  → /tmp/strm-115-test

[STEP] 🔌 开始卸载 STRM 挂载点...
[SUCCESS] ✅ 成功卸载: /tmp/strm-123-test
[SUCCESS] ✅ 成功卸载: /tmp/strm-115-test

[STEP] 🔪 开始终止 rclone 进程...
[INFO] 发现以下 rclone 进程：
  PID 12345: rclone strm-mount 123:Download /tmp/strm-123-test
[SUCCESS] ✅ 进程已优雅退出: PID 12345

[STEP] 🧹 清理临时文件和目录...
[SUCCESS] ✅ 已清理: /tmp/strm-123-test
[SUCCESS] ✅ 已清理: /tmp/cache-test.log

[STEP] 🔍 验证清理结果...
[SUCCESS] ✅ 清理验证通过：无残留挂载点和进程

[SUCCESS] 🎉 STRM-Mount 卸载完成！所有挂载点和进程已清理
```

### 警告输出
```
[WARNING] ⚠️ 发现残留项目：
[WARNING] 残留挂载点：
  → /tmp/strm-stuck-mount
[WARNING] 残留进程：
  PID 67890: rclone strm-mount 115:Videos /tmp/strm-stuck-mount

[WARNING] ⚠️ STRM-Mount 卸载完成，但存在部分问题，请检查上述警告信息
```

## 🔧 故障排除

### 常见问题

#### 1. 挂载点卸载失败
**原因**: 挂载点正在被使用
**解决**: 
- 关闭所有访问挂载点的程序
- 使用 `lsof` (Linux/macOS) 或 `handle` (Windows) 查找占用进程
- 手动终止占用进程后重新运行脚本

#### 2. 进程终止失败
**原因**: 进程可能处于不可中断状态
**解决**:
- 等待一段时间后重新运行脚本
- 重启系统（最后手段）

#### 3. 权限不足
**原因**: 某些操作需要管理员权限
**解决**:
- Linux/macOS: 使用 `sudo ./scripts/unmount-strm.sh`
- Windows: 以管理员身份运行 PowerShell 或命令提示符

#### 4. WinFsp 挂载点问题 (Windows)
**原因**: WinFsp 服务异常
**解决**:
- 重启 WinFsp 服务
- 重新安装 WinFsp

### 手动清理

如果脚本无法完全清理，可以手动执行以下操作：

#### Linux/macOS
```bash
# 查找并终止所有 rclone 进程
pkill -f "rclone.*strm-mount"

# 强制卸载所有挂载点
sudo umount -f /tmp/strm-*

# 清理临时文件
rm -rf /tmp/strm-*
rm -f /tmp/*strm*.log /tmp/*cache*.log
```

#### Windows
```cmd
REM 终止所有 rclone 进程
taskkill /im rclone.exe /f /t

REM 卸载网络驱动器
net use * /delete /y

REM 清理临时文件
rmdir /s /q "%TEMP%\strm-*"
del /q "%TEMP%\*strm*.log" "%TEMP%\*cache*.log"
```

## 📝 日志记录

脚本会在控制台输出详细的操作日志，包括：
- 🔍 **发现阶段**: 显示找到的挂载点和进程
- 🔧 **操作阶段**: 显示每个操作的执行结果
- ✅ **验证阶段**: 显示清理结果验证
- 📊 **统计信息**: 显示操作统计和耗时

## 🤝 贡献

欢迎提交 Issue 和 Pull Request 来改进这些脚本！

## 📄 许可证

这些脚本遵循与 STRM-Mount 项目相同的许可证。

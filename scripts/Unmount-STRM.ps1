#Requires -Version 5.1

<#
.SYNOPSIS
    STRM-Mount 卸载脚本 (PowerShell 版本)

.DESCRIPTION
    安全地卸载所有 STRM 挂载点并终止相关进程
    支持网络驱动器、WinFsp 挂载点和进程管理

.PARAMETER Force
    强制模式，跳过确认提示

.PARAMETER Verbose
    详细模式，显示更多信息

.PARAMETER Quiet
    安静模式，只显示错误信息

.PARAMETER WhatIf
    预览模式，显示将要执行的操作但不实际执行

.EXAMPLE
    .\Unmount-STRM.ps1
    交互式卸载所有 STRM 挂载点

.EXAMPLE
    .\Unmount-STRM.ps1 -Force
    强制卸载，无需确认

.EXAMPLE
    .\Unmount-STRM.ps1 -WhatIf
    预览将要执行的操作

.NOTES
    作者: STRM-Mount 项目
    版本: 1.0.0
    需要: PowerShell 5.1+
#>

[CmdletBinding(SupportsShouldProcess)]
param(
    [switch]$Force,
    [switch]$Quiet,
    [switch]$WhatIf
)

# 设置错误处理
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

# 颜色定义
$Colors = @{
    Red    = 'Red'
    Green  = 'Green'
    Yellow = 'Yellow'
    Blue   = 'Blue'
    Cyan   = 'Cyan'
    Magenta = 'Magenta'
}

# 日志函数
function Write-LogInfo {
    param([string]$Message)
    if (-not $Quiet) {
        Write-Host "[INFO] $Message" -ForegroundColor $Colors.Blue
    }
}

function Write-LogSuccess {
    param([string]$Message)
    if (-not $Quiet) {
        Write-Host "[SUCCESS] $Message" -ForegroundColor $Colors.Green
    }
}

function Write-LogWarning {
    param([string]$Message)
    Write-Host "[WARNING] $Message" -ForegroundColor $Colors.Yellow
}

function Write-LogError {
    param([string]$Message)
    Write-Host "[ERROR] $Message" -ForegroundColor $Colors.Red
}

function Write-LogStep {
    param([string]$Message)
    if (-not $Quiet) {
        Write-Host "[STEP] $Message" -ForegroundColor $Colors.Magenta
    }
}

# 显示脚本头部信息
function Show-Header {
    if (-not $Quiet) {
        Write-Host @"
╔══════════════════════════════════════════════════════════════╗
║                    STRM-Mount 卸载脚本                        ║
║                                                              ║
║  功能：安全卸载所有 STRM 挂载点并终止相关进程                  ║
║  版本：1.0.0 (PowerShell)                                    ║
╚══════════════════════════════════════════════════════════════╝
"@ -ForegroundColor $Colors.Cyan
        Write-Host ""
    }
}

# 检查管理员权限
function Test-AdminRights {
    $currentUser = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($currentUser)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

# 查找 rclone 进程
function Find-RcloneProcesses {
    Write-LogStep "🔍 查找 rclone 相关进程..."
    
    try {
        $processes = Get-Process -Name "rclone*" -ErrorAction SilentlyContinue | 
                    Where-Object { $_.CommandLine -like "*strm-mount*" }
        
        if ($processes) {
            Write-LogInfo "发现以下 rclone 进程："
            foreach ($proc in $processes) {
                Write-Host "  → PID $($proc.Id): $($proc.ProcessName)" -ForegroundColor $Colors.Yellow
                if ($VerbosePreference -eq 'Continue') {
                    Write-Host "    命令行: $($proc.CommandLine)" -ForegroundColor Gray
                }
            }
            return $processes
        } else {
            Write-LogInfo "未发现 rclone strm-mount 进程"
            return $null
        }
    } catch {
        Write-LogWarning "查找进程时出错: $($_.Exception.Message)"
        return $null
    }
}

# 查找网络驱动器
function Find-NetworkDrives {
    Write-LogStep "🔍 查找 STRM 相关网络驱动器..."
    
    try {
        $drives = Get-WmiObject -Class Win32_MappedLogicalDisk | 
                 Where-Object { $_.ProviderName -match "(123|115|strm)" }
        
        if ($drives) {
            Write-LogInfo "发现以下网络驱动器："
            foreach ($drive in $drives) {
                Write-Host "  → $($drive.DeviceID) -> $($drive.ProviderName)" -ForegroundColor $Colors.Yellow
            }
            return $drives
        } else {
            Write-LogInfo "未发现 STRM 相关网络驱动器"
            return $null
        }
    } catch {
        Write-LogWarning "查找网络驱动器时出错: $($_.Exception.Message)"
        return $null
    }
}

# 查找 WinFsp 挂载点
function Find-WinFspMounts {
    Write-LogStep "🔍 查找 WinFsp 挂载点..."
    
    $winFspPath = "${env:ProgramFiles}\WinFsp\bin\launchctl-x64.exe"
    
    if (-not (Test-Path $winFspPath)) {
        Write-LogInfo "未安装 WinFsp"
        return $null
    }
    
    try {
        $output = & $winFspPath list 2>$null
        $mounts = $output | Where-Object { $_ -match "rclone" }
        
        if ($mounts) {
            Write-LogInfo "发现以下 WinFsp 挂载点："
            foreach ($mount in $mounts) {
                Write-Host "  → $mount" -ForegroundColor $Colors.Yellow
            }
            return $mounts
        } else {
            Write-LogInfo "未发现 WinFsp 挂载点"
            return $null
        }
    } catch {
        Write-LogWarning "查找 WinFsp 挂载点时出错: $($_.Exception.Message)"
        return $null
    }
}

# 终止 rclone 进程
function Stop-RcloneProcesses {
    param([array]$Processes)
    
    if (-not $Processes) { return $true }
    
    Write-LogStep "🔪 开始终止 rclone 进程..."
    
    $success = $true
    
    foreach ($proc in $Processes) {
        try {
            Write-LogInfo "尝试优雅终止进程: PID $($proc.Id)"
            
            if ($PSCmdlet.ShouldProcess("PID $($proc.Id)", "终止进程")) {
                # 尝试优雅终止
                $proc.CloseMainWindow() | Out-Null
                
                # 等待进程退出
                $timeout = 10
                $count = 0
                while (-not $proc.HasExited -and $count -lt $timeout) {
                    Start-Sleep -Seconds 1
                    $count++
                    $proc.Refresh()
                }
                
                if ($proc.HasExited) {
                    Write-LogSuccess "✅ 进程已优雅退出: PID $($proc.Id)"
                } else {
                    Write-LogWarning "⚠️ 进程未在预期时间内退出，尝试强制终止: PID $($proc.Id)"
                    $proc.Kill()
                    Write-LogSuccess "✅ 进程已强制终止: PID $($proc.Id)"
                }
            }
        } catch {
            Write-LogError "❌ 终止进程失败: PID $($proc.Id) - $($_.Exception.Message)"
            $success = $false
        }
    }
    
    return $success
}

# 卸载网络驱动器
function Dismount-NetworkDrives {
    param([array]$Drives)
    
    if (-not $Drives) { return $true }
    
    Write-LogStep "🔌 开始卸载网络驱动器..."
    
    $success = $true
    
    foreach ($drive in $Drives) {
        try {
            Write-LogInfo "尝试卸载网络驱动器: $($drive.DeviceID)"
            
            if ($PSCmdlet.ShouldProcess($drive.DeviceID, "卸载网络驱动器")) {
                $result = & net use $drive.DeviceID /delete /y 2>$null
                
                if ($LASTEXITCODE -eq 0) {
                    Write-LogSuccess "✅ 成功卸载: $($drive.DeviceID)"
                } else {
                    Write-LogError "❌ 卸载失败: $($drive.DeviceID)"
                    $success = $false
                }
            }
        } catch {
            Write-LogError "❌ 卸载网络驱动器失败: $($drive.DeviceID) - $($_.Exception.Message)"
            $success = $false
        }
    }
    
    return $success
}

# 卸载 WinFsp 挂载点
function Dismount-WinFspMounts {
    param([array]$Mounts)
    
    if (-not $Mounts) { return $true }
    
    Write-LogStep "🔌 开始卸载 WinFsp 挂载点..."
    
    $winFspPath = "${env:ProgramFiles}\WinFsp\bin\launchctl-x64.exe"
    
    try {
        if ($PSCmdlet.ShouldProcess("WinFsp rclone 服务", "停止服务")) {
            & $winFspPath stop rclone 2>$null
            
            if ($LASTEXITCODE -eq 0) {
                Write-LogSuccess "✅ WinFsp 挂载点已卸载"
                return $true
            } else {
                Write-LogWarning "⚠️ WinFsp 挂载点卸载可能失败"
                return $false
            }
        }
    } catch {
        Write-LogError "❌ 卸载 WinFsp 挂载点失败: $($_.Exception.Message)"
        return $false
    }
    
    return $true
}

# 清理临时文件
function Clear-TempFiles {
    Write-LogStep "🧹 清理临时文件和目录..."
    
    $tempPaths = @(
        "$env:TEMP\strm-*",
        "$env:SystemRoot\Temp\strm-*",
        "$env:TEMP\*strm*.log",
        "$env:TEMP\*cache*.log"
    )
    
    $cleaned = $false
    
    foreach ($pattern in $tempPaths) {
        $items = Get-ChildItem -Path $pattern -ErrorAction SilentlyContinue
        
        foreach ($item in $items) {
            try {
                Write-LogInfo "正在清理: $($item.FullName)"
                
                if ($PSCmdlet.ShouldProcess($item.FullName, "删除")) {
                    if ($item.PSIsContainer) {
                        Remove-Item -Path $item.FullName -Recurse -Force
                    } else {
                        Remove-Item -Path $item.FullName -Force
                    }
                    Write-LogSuccess "✅ 已清理: $($item.FullName)"
                    $cleaned = $true
                }
            } catch {
                Write-LogWarning "⚠️ 清理失败: $($item.FullName) - $($_.Exception.Message)"
            }
        }
    }
    
    if (-not $cleaned) {
        Write-LogInfo "未发现需要清理的临时文件"
    }
}

# 验证清理结果
function Test-CleanupResult {
    Write-LogStep "🔍 验证清理结果..."
    
    $success = $true
    
    # 检查残留进程
    $remainingProcesses = Get-Process -Name "rclone*" -ErrorAction SilentlyContinue |
                         Where-Object { $_.CommandLine -like "*strm-mount*" }
    
    if ($remainingProcesses) {
        Write-LogWarning "⚠️ 发现残留的 rclone 进程:"
        foreach ($proc in $remainingProcesses) {
            Write-Host "  → PID $($proc.Id): $($proc.ProcessName)" -ForegroundColor $Colors.Yellow
        }
        $success = $false
    } else {
        Write-LogSuccess "✅ 无残留 rclone 进程"
    }
    
    # 检查残留网络驱动器
    $remainingDrives = Get-WmiObject -Class Win32_MappedLogicalDisk |
                      Where-Object { $_.ProviderName -match "(123|115|strm)" }
    
    if ($remainingDrives) {
        Write-LogWarning "⚠️ 发现残留的网络驱动器:"
        foreach ($drive in $remainingDrives) {
            Write-Host "  → $($drive.DeviceID) -> $($drive.ProviderName)" -ForegroundColor $Colors.Yellow
        }
        $success = $false
    } else {
        Write-LogSuccess "✅ 无残留网络驱动器"
    }
    
    return $success
}

# 主函数
function Main {
    Show-Header
    
    # 检查管理员权限
    if (Test-AdminRights) {
        Write-LogWarning "检测到管理员权限，请谨慎操作"
    } else {
        Write-LogInfo "以普通用户权限运行"
    }
    
    Write-Host ""
    
    # 非强制模式下询问确认
    if (-not $Force -and -not $WhatIf) {
        Write-Host "此操作将：" -ForegroundColor $Colors.Yellow
        Write-Host "  1. 卸载所有 STRM 相关挂载点"
        Write-Host "  2. 终止所有 rclone strm-mount 进程"
        Write-Host "  3. 清理临时文件和目录"
        Write-Host ""
        
        $confirm = Read-Host "确认继续？(y/N)"
        if ($confirm -notmatch '^[Yy]$') {
            Write-LogInfo "操作已取消"
            return 0
        }
        Write-Host ""
    }
    
    $overallSuccess = $true
    
    try {
        # 查找所有需要清理的项目
        $processes = Find-RcloneProcesses
        $networkDrives = Find-NetworkDrives
        $winFspMounts = Find-WinFspMounts
        
        Write-Host ""
        
        # 执行清理操作
        if (-not (Stop-RcloneProcesses -Processes $processes)) {
            $overallSuccess = $false
        }
        
        if (-not (Dismount-NetworkDrives -Drives $networkDrives)) {
            $overallSuccess = $false
        }
        
        if (-not (Dismount-WinFspMounts -Mounts $winFspMounts)) {
            $overallSuccess = $false
        }
        
        Clear-TempFiles
        
        Write-Host ""
        
        if (-not (Test-CleanupResult)) {
            $overallSuccess = $false
        }
        
        Write-Host ""
        
        # 显示最终结果
        if ($overallSuccess) {
            Write-LogSuccess "🎉 STRM-Mount 卸载完成！所有挂载点和进程已清理"
            return 0
        } else {
            Write-LogWarning "⚠️ STRM-Mount 卸载完成，但存在部分问题，请检查上述警告信息"
            return 1
        }
        
    } catch {
        Write-LogError "执行过程中发生错误: $($_.Exception.Message)"
        return 1
    }
}

# 脚本入口点
if ($MyInvocation.InvocationName -ne '.') {
    exit (Main)
}

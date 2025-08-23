@echo off
setlocal enabledelayedexpansion

REM STRM-Mount 卸载脚本 (Windows 版本)
REM 功能：安全地卸载所有 STRM 挂载点并终止相关进程
REM 作者：STRM-Mount 项目
REM 版本：1.0.0

title STRM-Mount 卸载脚本

REM 颜色代码 (Windows 10+)
set "RED=[91m"
set "GREEN=[92m"
set "YELLOW=[93m"
set "BLUE=[94m"
set "PURPLE=[95m"
set "CYAN=[96m"
set "NC=[0m"

REM 显示脚本头部信息
echo %CYAN%
echo ╔══════════════════════════════════════════════════════════════╗
echo ║                    STRM-Mount 卸载脚本                        ║
echo ║                                                              ║
echo ║  功能：安全卸载所有 STRM 挂载点并终止相关进程                  ║
echo ║  版本：1.0.0 (Windows)                                       ║
echo ╚══════════════════════════════════════════════════════════════╝
echo %NC%
echo.

REM 检查管理员权限
net session >nul 2>&1
if %errorLevel% == 0 (
    echo %YELLOW%[WARNING]%NC% 检测到管理员权限，请谨慎操作
    echo.
) else (
    echo %BLUE%[INFO]%NC% 以普通用户权限运行
    echo.
)

REM 解析命令行参数
set "FORCE_MODE=false"
set "VERBOSE_MODE=false"
set "QUIET_MODE=false"

:parse_args
if "%~1"=="" goto :args_done
if /i "%~1"=="-h" goto :show_help
if /i "%~1"=="--help" goto :show_help
if /i "%~1"=="-f" set "FORCE_MODE=true"
if /i "%~1"=="--force" set "FORCE_MODE=true"
if /i "%~1"=="-v" set "VERBOSE_MODE=true"
if /i "%~1"=="--verbose" set "VERBOSE_MODE=true"
if /i "%~1"=="-q" set "QUIET_MODE=true"
if /i "%~1"=="--quiet" set "QUIET_MODE=true"
shift
goto :parse_args

:args_done

REM 非强制模式下询问确认
if "%FORCE_MODE%"=="false" (
    echo %YELLOW%此操作将：%NC%
    echo   1. 卸载所有 STRM 相关挂载点
    echo   2. 终止所有 rclone strm-mount 进程
    echo   3. 清理临时文件和目录
    echo.
    set /p "confirm=确认继续？(y/N): "
    if /i not "!confirm!"=="y" (
        echo %BLUE%[INFO]%NC% 操作已取消
        pause
        exit /b 0
    )
)

echo %PURPLE%[STEP]%NC% 🔍 查找 rclone 相关进程...

REM 查找并终止 rclone 进程
set "found_processes=false"
for /f "tokens=2" %%i in ('tasklist /fi "imagename eq rclone*" /fo csv ^| find /c /v ""') do (
    if %%i gtr 1 set "found_processes=true"
)

if "%found_processes%"=="true" (
    echo %BLUE%[INFO]%NC% 发现 rclone 进程，正在终止...
    
    REM 优雅终止
    taskkill /im rclone.exe /t >nul 2>&1
    timeout /t 3 /nobreak >nul
    
    REM 强制终止
    taskkill /im rclone.exe /f /t >nul 2>&1
    
    REM 验证是否还有残留进程
    tasklist /fi "imagename eq rclone*" | find "rclone" >nul
    if !errorlevel! equ 0 (
        echo %RED%[ERROR]%NC% 部分 rclone 进程可能仍在运行
    ) else (
        echo %GREEN%[SUCCESS]%NC% ✅ 所有 rclone 进程已成功终止
    )
) else (
    echo %BLUE%[INFO]%NC% 未发现 rclone 进程
)

echo.
echo %PURPLE%[STEP]%NC% 🔌 检查并卸载挂载点...

REM Windows 下检查网络驱动器和挂载点
set "found_mounts=false"

REM 检查网络驱动器
for /f "tokens=1,2" %%i in ('net use ^| findstr /i "123\|115\|strm"') do (
    set "found_mounts=true"
    echo %BLUE%[INFO]%NC% 发现挂载点: %%i %%j
    echo %BLUE%[INFO]%NC% 正在卸载: %%i
    net use %%i /delete /y >nul 2>&1
    if !errorlevel! equ 0 (
        echo %GREEN%[SUCCESS]%NC% ✅ 成功卸载: %%i
    ) else (
        echo %RED%[ERROR]%NC% ❌ 卸载失败: %%i
    )
)

REM 检查 WinFsp 挂载点 (如果使用 WinFsp)
if exist "%ProgramFiles%\WinFsp\bin\launchctl-x64.exe" (
    "%ProgramFiles%\WinFsp\bin\launchctl-x64.exe" list | findstr /i "rclone" >nul 2>&1
    if !errorlevel! equ 0 (
        set "found_mounts=true"
        echo %BLUE%[INFO]%NC% 发现 WinFsp 挂载点，正在卸载...
        "%ProgramFiles%\WinFsp\bin\launchctl-x64.exe" stop rclone >nul 2>&1
        if !errorlevel! equ 0 (
            echo %GREEN%[SUCCESS]%NC% ✅ WinFsp 挂载点已卸载
        ) else (
            echo %YELLOW%[WARNING]%NC% ⚠️ WinFsp 挂载点卸载可能失败
        )
    )
)

if "%found_mounts%"=="false" (
    echo %BLUE%[INFO]%NC% 未发现 STRM 相关挂载点
)

echo.
echo %PURPLE%[STEP]%NC% 🧹 清理临时文件和目录...

REM 清理临时目录
set "temp_cleaned=false"

REM 清理用户临时目录
if exist "%TEMP%\strm-*" (
    set "temp_cleaned=true"
    echo %BLUE%[INFO]%NC% 清理用户临时目录中的 STRM 文件...
    for /d %%i in ("%TEMP%\strm-*") do (
        echo %BLUE%[INFO]%NC% 正在清理: %%i
        rmdir /s /q "%%i" >nul 2>&1
        if !errorlevel! equ 0 (
            echo %GREEN%[SUCCESS]%NC% ✅ 已清理: %%i
        ) else (
            echo %YELLOW%[WARNING]%NC% ⚠️ 清理失败: %%i
        )
    )
)

REM 清理系统临时目录
if exist "%SystemRoot%\Temp\strm-*" (
    set "temp_cleaned=true"
    echo %BLUE%[INFO]%NC% 清理系统临时目录中的 STRM 文件...
    for /d %%i in ("%SystemRoot%\Temp\strm-*") do (
        echo %BLUE%[INFO]%NC% 正在清理: %%i
        rmdir /s /q "%%i" >nul 2>&1
        if !errorlevel! equ 0 (
            echo %GREEN%[SUCCESS]%NC% ✅ 已清理: %%i
        ) else (
            echo %YELLOW%[WARNING]%NC% ⚠️ 清理失败: %%i
        )
    )
)

REM 清理日志文件
for %%i in ("%TEMP%\*strm*.log" "%TEMP%\*cache*.log") do (
    if exist "%%i" (
        set "temp_cleaned=true"
        echo %BLUE%[INFO]%NC% 正在清理日志: %%i
        del /q "%%i" >nul 2>&1
        if !errorlevel! equ 0 (
            echo %GREEN%[SUCCESS]%NC% ✅ 已清理: %%i
        ) else (
            echo %YELLOW%[WARNING]%NC% ⚠️ 清理失败: %%i
        )
    )
)

if "%temp_cleaned%"=="false" (
    echo %BLUE%[INFO]%NC% 未发现需要清理的临时文件
)

echo.
echo %PURPLE%[STEP]%NC% 🔍 验证清理结果...

REM 验证进程清理
tasklist /fi "imagename eq rclone*" | find "rclone" >nul
if !errorlevel! equ 0 (
    echo %YELLOW%[WARNING]%NC% ⚠️ 发现残留的 rclone 进程
    set "cleanup_success=false"
) else (
    echo %GREEN%[SUCCESS]%NC% ✅ 无残留 rclone 进程
    set "cleanup_success=true"
)

REM 验证挂载点清理
net use | findstr /i "123\|115\|strm" >nul
if !errorlevel! equ 0 (
    echo %YELLOW%[WARNING]%NC% ⚠️ 发现残留的网络驱动器
    set "cleanup_success=false"
) else (
    echo %GREEN%[SUCCESS]%NC% ✅ 无残留网络驱动器
)

echo.
REM 显示最终结果
if "%cleanup_success%"=="true" (
    echo %GREEN%[SUCCESS]%NC% 🎉 STRM-Mount 卸载完成！所有挂载点和进程已清理
    set "exit_code=0"
) else (
    echo %YELLOW%[WARNING]%NC% ⚠️ STRM-Mount 卸载完成，但存在部分问题，请检查上述警告信息
    set "exit_code=1"
)

echo.
echo 按任意键退出...
pause >nul
exit /b %exit_code%

:show_help
echo 用法: %~nx0 [选项]
echo.
echo 选项:
echo   -h, --help     显示此帮助信息
echo   -f, --force    强制模式，跳过确认提示
echo   -v, --verbose  详细模式，显示更多信息
echo   -q, --quiet    安静模式，只显示错误信息
echo.
echo 示例:
echo   %~nx0              # 交互式卸载
echo   %~nx0 -f           # 强制卸载，无需确认
echo   %~nx0 -v           # 详细模式卸载
echo.
pause
exit /b 0

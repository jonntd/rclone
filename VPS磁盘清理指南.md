# 🧹 Linux VPS 磁盘清理完整指南

## 📋 快速开始

### 1. 上传脚本到VPS
```bash
# 方法1: 使用wget下载（如果脚本在网上）
# wget https://your-server.com/linux_disk_cleanup.sh

# 方法2: 使用scp上传
# scp linux_disk_cleanup.sh user@your-vps-ip:/home/user/

# 方法3: 直接在VPS上创建文件
nano linux_disk_cleanup.sh
# 然后复制粘贴脚本内容
```

### 2. 给脚本执行权限
```bash
chmod +x linux_disk_cleanup.sh
chmod +x quick_linux_cleanup.sh
```

### 3. 运行清理脚本
```bash
# 快速清理（推荐新手）
sudo ./quick_linux_cleanup.sh

# 详细清理（可选择清理项目）
sudo ./linux_disk_cleanup.sh
```

## 🎯 清理脚本功能

### 快速清理脚本 (`quick_linux_cleanup.sh`)
- ✅ **一键清理**，无需用户交互
- ✅ 清理包管理器缓存
- ✅ 清理系统日志（保留7天）
- ✅ 清理临时文件
- ✅ 清理用户缓存
- ✅ 清理Docker资源（如果安装）
- ✅ 清理缩略图缓存

### 详细清理脚本 (`linux_disk_cleanup.sh`)
- 🔍 **交互式清理**，用户可选择清理项目
- 📊 显示详细的空间占用信息
- 🗂️ 查找大文件
- 🐳 Docker深度清理
- 🔧 清理旧内核
- 📈 显示清理前后对比

## 🚨 紧急清理命令

如果磁盘空间严重不足，可以直接运行这些命令：

```bash
# 1. 清理包缓存
sudo apt-get clean && sudo apt-get autoclean

# 2. 清理日志
sudo journalctl --vacuum-time=3d

# 3. 清理临时文件
sudo rm -rf /tmp/* /var/tmp/*

# 4. 清理用户缓存
rm -rf ~/.cache/*

# 5. 查看磁盘使用情况
df -h
du -h --max-depth=1 / | sort -hr | head -10
```

## 📊 常见占用空间的目录

### 系统目录
- `/var/log/` - 系统日志
- `/var/cache/` - 系统缓存
- `/tmp/` - 临时文件
- `/var/tmp/` - 系统临时文件

### 用户目录
- `~/.cache/` - 用户缓存
- `~/.local/share/Trash/` - 回收站
- `~/Downloads/` - 下载文件

### 应用相关
- `/var/lib/docker/` - Docker数据
- `/var/lib/mysql/` - MySQL数据
- `/var/www/` - Web文件

## 🔧 手动清理大文件

### 查找大文件
```bash
# 查找大于100MB的文件
find / -type f -size +100M -exec ls -lh {} \; 2>/dev/null

# 查看目录大小排序
du -h --max-depth=1 / 2>/dev/null | sort -hr

# 查看当前目录下最大的文件
ls -lhS
```

### 安全删除文件
```bash
# 删除前先查看文件内容
file /path/to/large-file
head /path/to/large-file

# 确认后删除
rm /path/to/large-file
```

## ⚠️ 注意事项

### 不要删除的重要文件/目录
- `/etc/` - 系统配置
- `/boot/` - 启动文件
- `/usr/` - 系统程序
- `/lib/` - 系统库
- `/bin/`, `/sbin/` - 系统命令

### 安全清理原则
1. **先备份重要数据**
2. **了解文件用途再删除**
3. **使用脚本自动清理常见垃圾**
4. **定期清理，避免积累**

## 🔄 定期维护

### 设置定时清理
```bash
# 编辑crontab
crontab -e

# 添加每周清理任务
0 2 * * 0 /path/to/quick_linux_cleanup.sh
```

### 监控磁盘使用
```bash
# 添加磁盘监控脚本
echo 'df -h | grep -E "(Filesystem|/dev/)" | grep -v tmpfs' >> ~/.bashrc
```

## 🆘 紧急情况处理

### 磁盘100%满的情况
```bash
# 1. 立即清理最大的日志文件
sudo truncate -s 0 /var/log/syslog
sudo truncate -s 0 /var/log/kern.log

# 2. 清理journal日志
sudo journalctl --vacuum-size=100M

# 3. 清理包缓存
sudo apt-get clean

# 4. 重启服务释放文件句柄
sudo systemctl restart rsyslog
```

## 📞 获取帮助

如果遇到问题：
1. 查看脚本运行日志
2. 检查磁盘使用情况：`df -h`
3. 查看最大文件：`du -h --max-depth=1 / | sort -hr`
4. 联系系统管理员或VPS提供商

---

**记住：定期清理比紧急清理更安全有效！**

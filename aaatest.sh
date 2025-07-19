
rclone mount 123: /Users/jonntd/cloud/test --vfs-cache-mode off


./rclone mount 123: ~/mnt/123pan --vfs-cache-mode writes --allow-other --daemon -v

diskutil unmount force ~/mnt/123pan


rclone lsd

go build -tags cmount -o rclone .


rclone copy /Users/jonntd/Downloads/Doubao-main 123:test1 -P

./rclone lsf "115:test123/app/" --local-encoding "Slash,InvalidUtf8" --115-encoding "Slash,InvalidUtf8"


./rclone copy "115:/__download/250613/拜金女与她的男闺蜜.mp4" "/Users/jonntd/Downloads/nebius/aaa/"  -vv -P


./rclone copy "115:/教程/翼狐网_新版Arnold5.0 for maya—渲染系统教学【灯光材质应用+典型案例讲解】/" "123:test1/bbb/"  -vv  -P --stats 1s  \


./rclone copy '/Volumes/desk/transmission/downloads/complete/Udemy - Unreal Engine 5 (UE5) Blueprints for Beginners - Pixel Helmet' "115:test1/bbb/ddd"  -vv  -P --stats 1s



./rclone copy "115:/__download/250613/拜金女与她的男闺蜜.mp4" "/Users/jonntd/Downloads/nebius/aaa/"  -vv -P



./rclone copy '/Users/jonntd/rclone_mod/random_file_144MB_2.bin' "123:test1/bbb/ddd"  -vv  -P --stats 1s

./rclone copy '/Users/jonntd/rclone_mod/random_file_144MB_2.bin' "115:test1/bbb/ddd"  -vv  -P --stats 1s



./rclone help backend pan123drive

./rclone copy "116:/bbb/" "123test:test1/copy_test/" --115-encoding "Slash,InvalidUtf8" -vv -P --stats 1s


./rclone copy "/Volumes/Untitled/新建文件夹/" "123test:test1/fixed_test/tsse/"  --115-encoding "Slash,InvalidUtf8" -vv -P --stats 1s


./rclone lsd 116:
./rclone ls 116:test115/
./rclone tree115 116:


./rclone lsd 123test:
./rclone ls 123test:test123/
./rclone tree123 123test:



rclone copy "115:/bbb/" "123:test1/copy_test/"  -vv -P --stats 1s

./rclone copy "115:/电影/刮削/人人大包.720p/零城 (1988) {tmdb-74711} 1.6GB.mkv" "123:test1/copy_test/"  -vv -P

./rclone copy "115:/bbb/aaaa/猫与桃花源 (2018) {tmdb-513386}/猫与桃花源 (2018) {tmdb-513386} 2.3GB.mp4" "123:test1/chunk_test_2_3gb/" -P -vv --transfers=1



export ANTHROPIC_AUTH_TOKEN=sk-S0lRHDky2xNt2dRFHGm24dtYmYiAz15W4P8WtSQoTMr2MXzC
export ANTHROPIC_BASE_URL=https://anyrouter.top
claude



./rclone copy "123:test1/bbb/aaaa/"  "115:/test1/bbb/aaaa/" \
  -P \
  -vv \
  --transfers=1 \
  --checkers=1 \
  --buffer-size=32M \
  --timeout=15m \
  --retries=5 





./rclone copy "115:/bbb/" "123:test1/bbb/" \
  -P \
  -vv \
  --transfers=2 \
  --checkers=4 \
  --buffer-size=32M \
  --timeout=15m \
  --retries=5 




./rclone copy "115:/bbb/" "123:test1/bbb/" \
  --transfers 6 \
  --checkers 12 \
  --multi-thread-streams=4 \
  --multi-thread-cutoff 50M \
  --multi-thread-chunk-size 32M \
  --buffer-size 32M \
  --123-max-concurrent-uploads=8 \
  --123-max-concurrent-downloads=8 \
  --123-chunk-size=200M \
  --123-upload-pacer-min-sleep=200ms \
  --115-pacer-min-sleep 300ms \
  --timeout 120s \
  --progress \
  -vv



# 平衡性能和稳定性的推荐配置
rclone copy 115:source_path 123:dest_path \
  --123-max-concurrent-downloads 3 \
  --transfers 4 \
  --checkers 6 \
  --buffer-size 128M \
  --123-download-pacer-min-sleep 300ms \
  --115-pacer-min-sleep 350ms \
  --progress \
  --stats 10s




./rclone copy "115:__download/Anal-Beauty.22.07.14.Hot.Pearl.XXX.1080p.HEVC.x265.PRT.mkv" "/tmp/test_115_simple/" --progress --log-level DEBUG --stats 1s --transfers 1 --checkers 1




for i in {1..3}; do dd_size=$(( RANDOM % 101 + 100 )); FILE_NAME="random_file_${dd_size}MB_${i}.bin"; dd if=/dev/urandom of="$FILE_NAME" bs=1m count=${dd_size} status=none; echo "Generated $FILE_NAME"; done








  # 停止rclone进程
sudo pkill -f rclone

# 删除所有rclone临时文件（释放31.2GB）
sudo rm -f /tmp/cross_cloud_*.tmp
sudo rm -f /tmp/rclone-*
sudo rm -rf /tmp/rclone-*-progress

# 检查结果
df -h

基于之前对115网盘和123网盘rclone后端的深度技术分析，请按照以下优先级顺序实施具体的代码改进：

**阶段1：统一错误处理机制（高优先级）**
- 创建共享的错误分类器模块，合并backend/115/115.go和backend/123/pan123drive.go中的UnifiedErrorClassifier和UnifiedErrorClassifier123
- 统一shouldRetry函数的实现逻辑，消除两个后端在错误重试策略上的差异
- 实现统一的错误码映射和重试延迟计算算法
- 在lib/errors/或backend/common/目录下创建共享错误处理包

**阶段2：改善进度显示统一性（中优先级）**
- 统一ProgressReadCloser的实现，确保115网盘和123网盘使用相同的进度显示格式
- 标准化分片上传/下载的进度报告格式，包括"分片 X/Y"的显示方式
- 实现跨云传输时的双阶段进度显示（下载进度+上传进度）
- 添加ETA（预计完成时间）计算功能

**阶段3：完善断点续传机制（中优先级）**
- 设计统一的断点续传接口，支持115网盘下载和123网盘上传的断点续传
- 实现持久化的断点信息存储机制，使用BadgerDB或文件系统
- 优化跨云传输中的断点续传逻辑，避免重复下载已传输的部分
- 添加断点续传的自动检测和恢复功能

**阶段4：代码重构和架构优化（长期目标）**
- 提取公共的并发控制、资源管理、性能监控代码到backend/common/包
- 分离backend/123/pan123drive.go中的业务逻辑（文件操作）和性能优化代码（并发控制、缓存管理）
- 重构超过1000行的大函数，提高代码可读性和可维护性
- 实现插件化的API版本管理机制

**阶段5：测试体系建设（持续进行）**
- 为错误处理、进度显示、断点续传功能编写单元测试
s'sh
- 创建跨云传输的集成测试套件，覆盖各种文件大小和网络条件
- 建立性能基准测试，监控优化效果
- 添加并发安全性测试，确保多线程操作的正确性

请优先实施阶段1的统一错误处理机制，因为这是影响用户体验最直接的问题，并且为后续优化奠定基础。每个阶段完成后，请提供具体的代码实现和测试验证结果。


go build -o rclone_test .



./rclone copy "115:/test115/课时44%3A破旧的钢筋混凝土墙的油漆色彩调节.mp4" "123:test1/" -vv -P --transfers 1 --local-encoding Slash,InvalidUtf8 --115-encoding Slash,InvalidUtf8 --log-file 

使用 ./rclone copy "115:/test115/课时44%3A破旧的钢筋混凝土墙的油漆色彩调节.mp4" "123:test1/" -vv -P --transfers 1 --local-encoding Slash,InvalidUtf8 --115-encoding Slash,InvalidUtf8 测试 --log-file 输出到本地


./rclone_test copy "123:/rclone/Mukuru - character rig for maya/Mukuru - character rig for maya.rar" "115:test115/" -vv -P --transfers 1




./rclone copy "115:/教程/翼狐  mari全能/生物案例54-61.rar" "/Users/jonntd/rclone_mod/logs_cross_cloud/" -vv -P --transfers 1





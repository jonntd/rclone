#!/bin/bash

# STRM-Mount VPS管理脚本
# 适配VPS环境的云盘挂载管理

# 配置文件路径
CONFIG_FILE="/etc/rclone-strm/config"

# 默认配置 (可通过配置文件覆盖)
RCLONE="/usr/local/bin/rclone"
BASE_MOUNT_DIR="/mnt/cloud"     # 基础挂载目录
MOUNT_115="$BASE_MOUNT_DIR/115"
MOUNT_123="$BASE_MOUNT_DIR/123"
DATA_DIR="/var/lib/rclone-strm"  # 统一数据目录
CACHE_DIR="$DATA_DIR/cache"      # 临时缓存
LOG_DIR="$DATA_DIR/logs"         # 日志文件
PERSISTENT_CACHE_DIR="$DATA_DIR/persistent"  # 持久化缓存

# 系统用户配置
RCLONE_USER="rclone"
RCLONE_GROUP="rclone"

# 加载配置文件
load_config() {
    if [[ -f "$CONFIG_FILE" ]]; then
        source "$CONFIG_FILE"
        info "已加载配置文件: $CONFIG_FILE"

        # 重新计算挂载点
        MOUNT_115="$BASE_MOUNT_DIR/115"
        MOUNT_123="$BASE_MOUNT_DIR/123"
    fi
}

# 颜色
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# 日志函数
info() {
    echo -e "${BLUE}[INFO]${NC} $1"
    logger -t "strm-vps" "INFO: $1"
}

success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
    logger -t "strm-vps" "SUCCESS: $1"
}

warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
    logger -t "strm-vps" "WARNING: $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
    logger -t "strm-vps" "ERROR: $1"
}

# 检查权限
check_permissions() {
    if [[ $EUID -ne 0 ]]; then
        error "此脚本需要root权限运行"
        info "请使用: sudo $0 $*"
        exit 1
    fi
}

# 检查并创建用户
setup_user() {
    if ! id "$RCLONE_USER" &>/dev/null; then
        info "创建rclone用户..."
        useradd -r -s /bin/false -d /var/lib/rclone -c "Rclone Service User" "$RCLONE_USER"
        success "✅ rclone用户创建完成"
    fi
}

# 检查并安装依赖
check_dependencies() {
    local missing_deps=()
    
    # 检查fuse
    if ! command -v fusermount &> /dev/null; then
        missing_deps+=("fuse")
    fi
    
    # 检查rclone
    if [[ ! -f "$RCLONE" ]]; then
        missing_deps+=("rclone")
    fi
    
    if [[ ${#missing_deps[@]} -gt 0 ]]; then
        error "缺少依赖: ${missing_deps[*]}"
        info "请安装依赖:"
        info "  Ubuntu/Debian: apt update && apt install -y fuse"
        info "  CentOS/RHEL: yum install -y fuse"
        info "  rclone: curl https://rclone.org/install.sh | sudo bash"
        exit 1
    fi
}

# 创建配置文件
create_config_file() {
    info "创建配置文件..."

    mkdir -p "$(dirname "$CONFIG_FILE")"

    cat > "$CONFIG_FILE" << EOF
# Rclone STRM-Mount VPS 配置文件
# 修改此文件后需要重启服务

# rclone 可执行文件路径
RCLONE="/usr/local/bin/rclone"

# 基础挂载目录 (所有网盘将挂载在此目录下)
BASE_MOUNT_DIR="/mnt/cloud"

# 统一数据目录 (包含缓存、日志、持久化数据)
DATA_DIR="/var/lib/rclone-strm"

# 子目录 (自动基于DATA_DIR计算)
CACHE_DIR="\$DATA_DIR/cache"
LOG_DIR="\$DATA_DIR/logs"
PERSISTENT_CACHE_DIR="\$DATA_DIR/persistent"

# 系统用户
RCLONE_USER="rclone"
RCLONE_GROUP="rclone"

# 网盘配置 (可以添加更多网盘)
# 格式: REMOTE_NAME:DISPLAY_NAME:MIN_SIZE
REMOTES="115:115网盘:50M 123:123网盘:50M"
EOF

    chmod 644 "$CONFIG_FILE"
    success "✅ 配置文件创建完成: $CONFIG_FILE"
}

# 初始化目录结构
init_directories() {
    info "初始化目录结构..."

    # 创建基础挂载目录
    mkdir -p "$BASE_MOUNT_DIR"

    # 创建具体挂载点
    mkdir -p "$MOUNT_115" "$MOUNT_123"

    # 创建统一数据目录及子目录
    mkdir -p "$DATA_DIR"
    mkdir -p "$CACHE_DIR" "$LOG_DIR" "$PERSISTENT_CACHE_DIR"

    # 设置权限
    chown -R "$RCLONE_USER:$RCLONE_GROUP" "$DATA_DIR"
    chmod 755 "$BASE_MOUNT_DIR" "$MOUNT_115" "$MOUNT_123"

    success "✅ 目录结构初始化完成"
    info "挂载目录: $BASE_MOUNT_DIR"
    info "数据目录: $DATA_DIR"
    info "  ├── 缓存: $CACHE_DIR"
    info "  ├── 日志: $LOG_DIR"
    info "  └── 持久化: $PERSISTENT_CACHE_DIR"
}

# 检查挂载状态
is_mounted() {
    local mount_point="$1"
    mount | grep -q "$mount_point"
}

# 清理旧挂载和进程
cleanup_mount() {
    local mount_point="$1"
    local name="$2"

    if is_mounted "$mount_point"; then
        warning "$name 发现旧挂载，正在卸载..."
        
        # 尝试正常卸载
        umount "$mount_point" 2>/dev/null || true
        sleep 2
        
        # 强制卸载
        if is_mounted "$mount_point"; then
            warning "$name 尝试强制卸载..."
            umount -f "$mount_point" 2>/dev/null || umount -l "$mount_point" 2>/dev/null || true
            sleep 1
        fi
    fi

    # 终止相关进程
    local remote=$(echo "$name" | grep -o '[0-9]\+')
    pkill -f "rclone strm-mount ${remote}:" 2>/dev/null || true
    sleep 1
}

# 验证挂载
verify_mount() {
    local mount_point="$1"
    local name="$2"
    local timeout=30

    info "验证 $name 挂载状态..."

    local count=0
    while [ $count -lt $timeout ]; do
        if is_mounted "$mount_point"; then
            if timeout 10 ls "$mount_point" >/dev/null 2>&1; then
                success "✅ $name 挂载成功"
                return 0
            fi
        fi
        sleep 1
        count=$((count + 1))
        if [ $((count % 5)) -eq 0 ]; then
            echo -n "."
        fi
    done

    echo
    if is_mounted "$mount_point"; then
        warning "⚠️ $name 已挂载但验证超时"
        return 0
    else
        error "$name 挂载失败"
        return 1
    fi
}

# 启动115网盘
start_115() {
    local cache_dir="$CACHE_DIR/115"
    local log_file="$LOG_DIR/115.log"  # 简化日志文件名

    info "🚀 启动 115 网盘 STRM-Mount..."
    info "挂载点: $MOUNT_115"
    info "日志文件: $log_file"

    cleanup_mount "$MOUNT_115" "115网盘"
    mkdir -p "$cache_dir"

    # VPS优化配置
    sudo -u "$RCLONE_USER" nohup "$RCLONE" strm-mount 115: "$MOUNT_115" \
        --min-size 50M \
        --persistent-cache=true \
        --cache-ttl=24h \
        --dir-cache-time=2h \
        --attr-timeout=60s \
        --vfs-cache-mode=minimal \
        --max-cache-size=100000 \
        --poll-interval=10m \
        --checkers=2 \
        --transfers=1 \
        --buffer-size=32M \
        --timeout=60s \
        --retries=3 \
        --low-level-retries=3 \
        --allow-other \
        --default-permissions \
        --daemon \
        --log-file="$log_file" \
        --log-level=INFO \
        > /dev/null 2>&1 &

    if verify_mount "$MOUNT_115" "115网盘"; then
        success "✅ 115 网盘启动完成！"
        info "挂载点: $MOUNT_115"
        info "日志: $log_file"
        return 0
    else
        error "❌ 115 网盘启动失败"
        return 1
    fi
}

# 启动123网盘
start_123() {
    local cache_dir="$CACHE_DIR/123"
    local log_file="$LOG_DIR/123.log"  # 简化日志文件名

    info "🚀 启动 123 网盘 STRM-Mount..."
    info "挂载点: $MOUNT_123"
    info "日志文件: $log_file"

    cleanup_mount "$MOUNT_123" "123网盘"
    mkdir -p "$cache_dir"

    sudo -u "$RCLONE_USER" nohup "$RCLONE" strm-mount 123: "$MOUNT_123" \
        --min-size 50M \
        --persistent-cache=true \
        --cache-ttl=24h \
        --dir-cache-time=2h \
        --attr-timeout=60s \
        --vfs-cache-mode=minimal \
        --max-cache-size=50000 \
        --poll-interval=10m \
        --checkers=1 \
        --transfers=1 \
        --buffer-size=16M \
        --timeout=60s \
        --retries=3 \
        --low-level-retries=3 \
        --allow-other \
        --default-permissions \
        --daemon \
        --log-file="$log_file" \
        --log-level=INFO \
        > /dev/null 2>&1 &

    if verify_mount "$MOUNT_123" "123网盘"; then
        success "✅ 123 网盘启动完成！"
        info "挂载点: $MOUNT_123"
        info "日志: $log_file"
        return 0
    else
        error "❌ 123 网盘启动失败"
        return 1
    fi
}

# 卸载所有
umount_all() {
    info "🛑 卸载所有 STRM-Mount..."

    local unmounted=0

    # 卸载115
    if is_mounted "$MOUNT_115"; then
        info "卸载 115 网盘: $MOUNT_115"
        if umount "$MOUNT_115" 2>/dev/null; then
            success "✅ 115 网盘卸载成功"
            ((unmounted++))
        else
            umount -f "$MOUNT_115" 2>/dev/null || umount -l "$MOUNT_115" 2>/dev/null || true
            if ! is_mounted "$MOUNT_115"; then
                success "✅ 115 网盘强制卸载成功"
                ((unmounted++))
            fi
        fi
    fi

    # 卸载123
    if is_mounted "$MOUNT_123"; then
        info "卸载 123 网盘: $MOUNT_123"
        if umount "$MOUNT_123" 2>/dev/null; then
            success "✅ 123 网盘卸载成功"
            ((unmounted++))
        else
            umount -f "$MOUNT_123" 2>/dev/null || umount -l "$MOUNT_123" 2>/dev/null || true
            if ! is_mounted "$MOUNT_123"; then
                success "✅ 123 网盘强制卸载成功"
                ((unmounted++))
            fi
        fi
    fi

    # 终止进程
    pkill -f "rclone strm-mount" 2>/dev/null || true

    if [[ $unmounted -gt 0 ]]; then
        success "🎉 卸载完成！($unmounted 个挂载点)"
    else
        info "没有需要卸载的挂载点"
    fi
}

# 显示状态
show_status() {
    echo -e "${BLUE}=== STRM-Mount VPS 状态 ===${NC}"
    echo

    # 115网盘状态
    echo -e "${YELLOW}115 网盘:${NC}"
    if is_mounted "$MOUNT_115"; then
        echo -e "  状态: ${GREEN}已挂载${NC}"
        echo "  挂载点: $MOUNT_115"
        if [[ -d "$MOUNT_115" ]]; then
            local count=$(timeout 10 find "$MOUNT_115" -maxdepth 2 -name "*.strm" 2>/dev/null | wc -l | tr -d ' ')
            if [[ $? -eq 0 ]]; then
                echo "  STRM 文件数: $count"
            else
                echo "  STRM 文件数: 扫描中..."
            fi
        fi
    else
        echo -e "  状态: ${RED}未挂载${NC}"
    fi
    echo

    # 123网盘状态
    echo -e "${YELLOW}123 网盘:${NC}"
    if is_mounted "$MOUNT_123"; then
        echo -e "  状态: ${GREEN}已挂载${NC}"
        echo "  挂载点: $MOUNT_123"
        if [[ -d "$MOUNT_123" ]]; then
            local count=$(timeout 10 find "$MOUNT_123" -maxdepth 2 -name "*.strm" 2>/dev/null | wc -l | tr -d ' ')
            if [[ $? -eq 0 ]]; then
                echo "  STRM 文件数: $count"
            else
                echo "  STRM 文件数: 扫描中..."
            fi
        fi
    else
        echo -e "  状态: ${RED}未挂载${NC}"
    fi
    echo

    # 进程状态
    echo -e "${YELLOW}进程状态:${NC}"
    local processes=$(pgrep -f "rclone strm-mount" 2>/dev/null | wc -l)
    if [[ $processes -gt 0 ]]; then
        echo -e "  rclone 进程: ${GREEN}$processes 个运行中${NC}"
        pgrep -f "rclone strm-mount" | while read pid; do
            local cmd=$(ps -p $pid -o cmd --no-headers 2>/dev/null | cut -c1-60)
            echo "    PID $pid: $cmd..."
        done
    else
        echo -e "  rclone 进程: ${RED}未运行${NC}"
    fi
    echo

    # 系统资源
    echo -e "${YELLOW}系统资源:${NC}"
    local cpu_usage=$(top -bn1 | grep "Cpu(s)" | awk '{print $2}' | cut -d'%' -f1)
    local mem_usage=$(free | grep Mem | awk '{printf "%.1f", $3/$2 * 100.0}')
    local disk_usage=$(df -h / | awk 'NR==2{print $5}')
    echo "  CPU 使用率: ${cpu_usage}%"
    echo "  内存使用率: ${mem_usage}%"
    echo "  磁盘使用率: ${disk_usage}"
    echo

    # 日志文件
    echo -e "${YELLOW}日志文件:${NC}"
    for log in "$LOG_DIR/115.log" "$LOG_DIR/123.log"; do
        if [[ -f "$log" ]]; then
            local size=$(du -h "$log" | cut -f1)
            local name=$(basename "$log" .log)
            echo "  ${name}网盘: $log ($size)"
        fi
    done
    echo

    # 缓存状态
    echo -e "${YELLOW}缓存状态:${NC}"
    if [[ -d "$CACHE_DIR" ]]; then
        local cache_size=$(du -sh "$CACHE_DIR" 2>/dev/null | cut -f1)
        echo "  缓存目录: $cache_size"
    fi
    if [[ -d "$PERSISTENT_CACHE_DIR" ]]; then
        local cache_files=$(find "$PERSISTENT_CACHE_DIR" -name "*.json" 2>/dev/null | wc -l)
        echo "  持久化缓存: $cache_files 个文件"
    fi
}

# 创建systemd服务
create_systemd_service() {
    info "创建 systemd 服务..."

    # 115网盘服务
    cat > /etc/systemd/system/rclone-strm-115.service << EOF
[Unit]
Description=Rclone STRM Mount for 115 Cloud
After=network.target

[Service]
Type=notify
User=$RCLONE_USER
Group=$RCLONE_GROUP
ExecStart=$RCLONE strm-mount 115: $MOUNT_115 \\
    --min-size 50M \\
    --persistent-cache=true \\
    --cache-ttl=24h \\
    --dir-cache-time=2h \\
    --attr-timeout=60s \\
    --vfs-cache-mode=minimal \\
    --max-cache-size=100000 \\
    --poll-interval=10m \\
    --checkers=2 \\
    --transfers=1 \\
    --buffer-size=32M \\
    --timeout=60s \\
    --retries=3 \\
    --low-level-retries=3 \\
    --allow-other \\
    --default-permissions \\
    --log-file=$LOG_DIR/115.log \\
    --log-level=INFO
ExecStop=/bin/fusermount -u $MOUNT_115
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

    # 123网盘服务
    cat > /etc/systemd/system/rclone-strm-123.service << EOF
[Unit]
Description=Rclone STRM Mount for 123 Cloud
After=network.target

[Service]
Type=notify
User=$RCLONE_USER
Group=$RCLONE_GROUP
ExecStart=$RCLONE strm-mount 123: $MOUNT_123 \\
    --min-size 50M \\
    --persistent-cache=true \\
    --cache-ttl=24h \\
    --dir-cache-time=2h \\
    --attr-timeout=60s \\
    --vfs-cache-mode=minimal \\
    --max-cache-size=50000 \\
    --poll-interval=10m \\
    --checkers=1 \\
    --transfers=1 \\
    --buffer-size=16M \\
    --timeout=60s \\
    --retries=3 \\
    --low-level-retries=3 \\
    --allow-other \\
    --default-permissions \\
    --log-file=$LOG_DIR/123.log \\
    --log-level=INFO
ExecStop=/bin/fusermount -u $MOUNT_123
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    success "✅ systemd 服务创建完成"
    info "启用服务: systemctl enable rclone-strm-115 rclone-strm-123"
    info "启动服务: systemctl start rclone-strm-115 rclone-strm-123"
}

# 配置管理
config_set() {
    local key="$1"
    local value="$2"

    if [[ -z "$key" || -z "$value" ]]; then
        error "用法: $0 config-set <键> <值>"
        info "可配置项:"
        info "  BASE_MOUNT_DIR    基础挂载目录"
        info "  LOG_DIR          日志目录"
        info "  CACHE_DIR        缓存目录"
        return 1
    fi

    if [[ ! -f "$CONFIG_FILE" ]]; then
        error "配置文件不存在，请先运行: $0 init"
        return 1
    fi

    # 更新配置文件
    if grep -q "^$key=" "$CONFIG_FILE"; then
        sed -i "s|^$key=.*|$key=\"$value\"|" "$CONFIG_FILE"
    else
        echo "$key=\"$value\"" >> "$CONFIG_FILE"
    fi

    success "✅ 配置已更新: $key = $value"
    warning "⚠️ 需要重启服务使配置生效"
}

# 交互式配置菜单
config_interactive() {
    while true; do
        echo -e "${BLUE}=== STRM-Mount 配置管理 ===${NC}"
        echo
        echo -e "${YELLOW}当前配置:${NC}"
        echo "  1. rclone 路径: $RCLONE"
        echo "  2. 基础挂载目录: $BASE_MOUNT_DIR"
        echo "  3. 数据目录: $DATA_DIR"
        echo "  4. 运行用户: $RCLONE_USER"
        echo
        echo -e "${YELLOW}操作选项:${NC}"
        echo "  [1-4] 修改对应配置项"
        echo "  [s]   保存配置并退出"
        echo "  [r]   重置为默认配置"
        echo "  [q]   退出不保存"
        echo
        read -p "请选择操作 [1-4/s/r/q]: " choice

        case "$choice" in
            1)
                echo
                echo -e "${YELLOW}当前 rclone 路径: $RCLONE${NC}"
                read -p "输入新的 rclone 路径 (回车保持不变): " new_value
                if [[ -n "$new_value" ]]; then
                    if [[ -f "$new_value" && -x "$new_value" ]]; then
                        RCLONE="$new_value"
                        success "✅ rclone 路径已更新"
                    else
                        error "❌ 文件不存在或无执行权限: $new_value"
                    fi
                fi
                ;;
            2)
                echo
                echo -e "${YELLOW}当前基础挂载目录: $BASE_MOUNT_DIR${NC}"
                read -p "输入新的基础挂载目录 (回车保持不变): " new_value
                if [[ -n "$new_value" ]]; then
                    BASE_MOUNT_DIR="$new_value"
                    MOUNT_115="$BASE_MOUNT_DIR/115"
                    MOUNT_123="$BASE_MOUNT_DIR/123"
                    success "✅ 基础挂载目录已更新"
                    info "115挂载点: $MOUNT_115"
                    info "123挂载点: $MOUNT_123"
                fi
                ;;
            3)
                echo
                echo -e "${YELLOW}当前数据目录: $DATA_DIR${NC}"
                echo "  ├── 缓存: $CACHE_DIR"
                echo "  ├── 日志: $LOG_DIR"
                echo "  └── 持久化: $PERSISTENT_CACHE_DIR"
                read -p "输入新的数据目录 (回车保持不变): " new_value
                if [[ -n "$new_value" ]]; then
                    DATA_DIR="$new_value"
                    CACHE_DIR="$DATA_DIR/cache"
                    LOG_DIR="$DATA_DIR/logs"
                    PERSISTENT_CACHE_DIR="$DATA_DIR/persistent"
                    success "✅ 数据目录已更新"
                    info "新的子目录:"
                    info "  ├── 缓存: $CACHE_DIR"
                    info "  ├── 日志: $LOG_DIR"
                    info "  └── 持久化: $PERSISTENT_CACHE_DIR"
                fi
                ;;
            4)
                echo
                echo -e "${YELLOW}当前运行用户: $RCLONE_USER${NC}"
                read -p "输入新的运行用户 (回车保持不变): " new_value
                if [[ -n "$new_value" ]]; then
                    if id "$new_value" &>/dev/null; then
                        RCLONE_USER="$new_value"
                        RCLONE_GROUP="$new_value"
                        success "✅ 运行用户已更新"
                    else
                        error "❌ 用户不存在: $new_value"
                    fi
                fi
                ;;
            s|S)
                echo
                info "保存配置到文件..."
                save_config_to_file
                success "🎉 配置已保存！"
                warning "⚠️ 需要重启服务使配置生效: $0 restart"
                break
                ;;
            r|R)
                echo
                warning "⚠️ 这将重置所有配置为默认值"
                read -p "确认重置？(y/N): " -n 1 -r
                echo
                if [[ $REPLY =~ ^[Yy]$ ]]; then
                    reset_to_default_config
                    success "✅ 配置已重置为默认值"
                fi
                ;;
            q|Q)
                echo
                info "退出配置管理"
                break
                ;;
            *)
                echo
                error "无效选择，请重新输入"
                ;;
        esac

        echo
        read -p "按回车键继续..." -r
        clear
    done
}

# 保存配置到文件
save_config_to_file() {
    mkdir -p "$(dirname "$CONFIG_FILE")"

    cat > "$CONFIG_FILE" << EOF
# Rclone STRM-Mount VPS 配置文件
# 修改此文件后需要重启服务

# rclone 可执行文件路径
RCLONE="$RCLONE"

# 基础挂载目录 (所有网盘将挂载在此目录下)
BASE_MOUNT_DIR="$BASE_MOUNT_DIR"

# 缓存目录
CACHE_DIR="$CACHE_DIR"

# 日志目录 (所有日志文件统一存放)
LOG_DIR="$LOG_DIR"

# 持久化缓存目录
PERSISTENT_CACHE_DIR="$PERSISTENT_CACHE_DIR"

# 系统用户
RCLONE_USER="$RCLONE_USER"
RCLONE_GROUP="$RCLONE_GROUP"

# 网盘配置 (可以添加更多网盘)
REMOTES="115:115网盘:50M 123:123网盘:50M"
EOF

    chmod 644 "$CONFIG_FILE"
}

# 重置为默认配置
reset_to_default_config() {
    RCLONE="/usr/local/bin/rclone"
    BASE_MOUNT_DIR="/mnt/cloud"
    MOUNT_115="$BASE_MOUNT_DIR/115"
    MOUNT_123="$BASE_MOUNT_DIR/123"
    DATA_DIR="/var/lib/rclone-strm"
    CACHE_DIR="$DATA_DIR/cache"
    LOG_DIR="$DATA_DIR/logs"
    PERSISTENT_CACHE_DIR="$DATA_DIR/persistent"
    RCLONE_USER="rclone"
    RCLONE_GROUP="rclone"
}

# 快速配置向导
config_wizard() {
    echo -e "${BLUE}=== STRM-Mount 快速配置向导 ===${NC}"
    echo
    echo "这个向导将帮助您快速配置常见的部署场景"
    echo

    echo -e "${YELLOW}请选择部署场景:${NC}"
    echo "  1. 默认配置 (系统标准路径)"
    echo "  2. 数据盘配置 (独立数据盘)"
    echo "  3. 高性能配置 (SSD缓存)"
    echo "  4. 自定义配置"
    echo
    read -p "请选择 [1-4]: " scenario

    case "$scenario" in
        1)
            info "使用默认配置..."
            reset_to_default_config
            ;;
        2)
            echo
            echo -e "${YELLOW}数据盘配置${NC}"
            read -p "请输入数据盘挂载点 (如: /data): " data_mount
            if [[ -n "$data_mount" && -d "$data_mount" ]]; then
                BASE_MOUNT_DIR="$data_mount/cloud"
                DATA_DIR="$data_mount/rclone-strm"
                CACHE_DIR="$DATA_DIR/cache"
                LOG_DIR="$DATA_DIR/logs"
                PERSISTENT_CACHE_DIR="$DATA_DIR/persistent"
                MOUNT_115="$BASE_MOUNT_DIR/115"
                MOUNT_123="$BASE_MOUNT_DIR/123"
                success "✅ 数据盘配置完成"
            else
                error "❌ 无效的数据盘路径"
                return 1
            fi
            ;;
        3)
            echo
            echo -e "${YELLOW}高性能配置${NC}"
            read -p "请输入SSD挂载点 (如: /ssd): " ssd_mount
            read -p "请输入HDD挂载点 (如: /hdd): " hdd_mount
            if [[ -n "$ssd_mount" && -d "$ssd_mount" && -n "$hdd_mount" && -d "$hdd_mount" ]]; then
                BASE_MOUNT_DIR="$hdd_mount/cloud"
                DATA_DIR="$hdd_mount/rclone-strm"
                CACHE_DIR="$ssd_mount/cache"           # 临时缓存放SSD
                LOG_DIR="$DATA_DIR/logs"               # 日志放HDD
                PERSISTENT_CACHE_DIR="$ssd_mount/persistent"  # 持久化缓存放SSD
                MOUNT_115="$BASE_MOUNT_DIR/115"
                MOUNT_123="$BASE_MOUNT_DIR/123"
                success "✅ 高性能配置完成"
                info "挂载点: $BASE_MOUNT_DIR (HDD)"
                info "数据目录: $DATA_DIR (HDD)"
                info "缓存: $CACHE_DIR (SSD)"
                info "持久化缓存: $PERSISTENT_CACHE_DIR (SSD)"
            else
                error "❌ 无效的SSD或HDD路径"
                return 1
            fi
            ;;
        4)
            echo
            echo -e "${YELLOW}自定义配置${NC}"
            config_interactive
            return 0
            ;;
        *)
            error "无效选择"
            return 1
            ;;
    esac

    echo
    echo -e "${YELLOW}配置预览:${NC}"
    echo "  基础挂载目录: $BASE_MOUNT_DIR"
    echo "  缓存目录: $CACHE_DIR"
    echo "  日志目录: $LOG_DIR"
    echo "  持久化缓存: $PERSISTENT_CACHE_DIR"
    echo

    read -p "确认保存此配置？(Y/n): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Nn]$ ]]; then
        info "配置已取消"
        return 1
    fi

    save_config_to_file
    success "🎉 配置已保存！"

    echo
    info "下一步操作:"
    echo "  1. 创建目录: $0 init"
    echo "  2. 配置rclone: sudo -u rclone rclone config"
    echo "  3. 启动服务: $0 start"
}

config_show() {
    echo -e "${BLUE}=== 当前配置 ===${NC}"
    echo

    if [[ -f "$CONFIG_FILE" ]]; then
        echo -e "${YELLOW}配置文件: $CONFIG_FILE${NC}"
        cat "$CONFIG_FILE" | grep -v "^#" | grep -v "^$"
    else
        warning "配置文件不存在: $CONFIG_FILE"
    fi

    echo
    echo -e "${YELLOW}运行时配置:${NC}"
    echo "  rclone 路径: $RCLONE"
    echo "  基础挂载目录: $BASE_MOUNT_DIR"
    echo "  115 挂载点: $MOUNT_115"
    echo "  123 挂载点: $MOUNT_123"
    echo "  缓存目录: $CACHE_DIR"
    echo "  日志目录: $LOG_DIR"
    echo "  运行用户: $RCLONE_USER"
}

# 显示帮助
show_help() {
    echo -e "${BLUE}STRM-Mount VPS管理脚本${NC}"
    echo
    echo "用法: $0 <命令> [参数]"
    echo
    echo "基础命令:"
    echo "  init         初始化VPS环境 (首次运行)"
    echo "  start        启动所有网盘"
    echo "  start-115    只启动 115 网盘"
    echo "  start-123    只启动 123 网盘"
    echo "  stop         停止所有网盘"
    echo "  restart      重启所有网盘"
    echo "  status       显示挂载状态"
    echo
    echo "系统管理:"
    echo "  install-service  安装 systemd 服务"
    echo "  enable-service   启用开机自启"
    echo "  disable-service  禁用开机自启"
    echo "  logs            显示服务日志"
    echo "  clean           清理缓存"
    echo
    echo "配置管理:"
    echo "  config-wizard    快速配置向导 (首次推荐)"
    echo "  config           交互式配置管理"
    echo "  config-show      显示当前配置"
    echo "  config-set <key> <value>  设置配置项"
    echo "  config-edit      编辑配置文件"
    echo
    echo "示例:"
    echo "  $0 config                              # 交互式配置"
    echo "  $0 config-set BASE_MOUNT_DIR /data/cloud"
    echo "  $0 config-set LOG_DIR /data/logs/rclone"
    echo
    echo "当前配置:"
    echo "  配置文件: $CONFIG_FILE"
    echo "  基础挂载目录: $BASE_MOUNT_DIR"
    echo "  日志目录: $LOG_DIR"
}

# 主函数
main() {
    # 加载配置 (除了init命令)
    if [[ "$1" != "init" ]]; then
        load_config
    fi

    case "$1" in
        init)
            check_permissions
            check_dependencies
            setup_user
            create_config_file
            init_directories
            success "🎉 VPS环境初始化完成！"
            info "配置文件: $CONFIG_FILE"
            info "下一步: $0 start"
            ;;
        start)
            check_permissions
            info "🚀 启动所有网盘..."
            start_115
            echo
            start_123
            echo
            show_status
            ;;
        start-115)
            check_permissions
            start_115
            ;;
        start-123)
            check_permissions
            start_123
            ;;
        stop)
            check_permissions
            umount_all
            ;;
        restart)
            check_permissions
            umount_all
            sleep 3
            info "🚀 重新启动所有网盘..."
            start_115
            echo
            start_123
            echo
            show_status
            ;;
        status)
            show_status
            ;;
        install-service)
            check_permissions
            create_systemd_service
            ;;
        enable-service)
            check_permissions
            systemctl enable rclone-strm-115 rclone-strm-123
            success "✅ 开机自启已启用"
            ;;
        disable-service)
            check_permissions
            systemctl disable rclone-strm-115 rclone-strm-123
            success "✅ 开机自启已禁用"
            ;;
        logs)
            journalctl -u rclone-strm-115 -u rclone-strm-123 -f
            ;;
        clean)
            check_permissions
            info "清理缓存..."
            rm -rf "$CACHE_DIR"/* 2>/dev/null || true
            rm -rf "$PERSISTENT_CACHE_DIR"/*.json 2>/dev/null || true
            success "✅ 缓存清理完成"
            ;;
        config-wizard)
            check_permissions
            config_wizard
            ;;
        config)
            check_permissions
            config_interactive
            ;;
        config-show)
            config_show
            ;;
        config-set)
            check_permissions
            config_set "$2" "$3"
            ;;
        config-edit)
            check_permissions
            if command -v nano &> /dev/null; then
                nano "$CONFIG_FILE"
            elif command -v vim &> /dev/null; then
                vim "$CONFIG_FILE"
            else
                info "请手动编辑配置文件: $CONFIG_FILE"
            fi
            ;;
        help|--help|-h)
            show_help
            ;;
        "")
            show_help
            ;;
        *)
            error "未知命令: $1"
            echo
            show_help
            exit 1
            ;;
    esac
}

# 运行主函数
main "$@"

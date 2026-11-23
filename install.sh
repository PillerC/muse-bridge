#!/bin/sh
# Muse Bridge 一键安装脚本。
#
# 两种安装方式，都在这个脚本里：
#
#   1. 装预编译二进制（默认，推荐）——从 GitHub Releases 下载对应平台的
#      bridge-<os>-<arch>，校验 sha256 后安装。用户机器不需要 Go 工具链。
#
#   2. 从源码编译（--from-source）——git clone 仓库，用本机 Go 编译安装。
#      适合想改代码、或者目标平台没有预编译包的场景。
#
# 设计上的几条约束：
#   - 用 POSIX sh 而不是 bash：Alpine 这类系统默认没有 bash，
#     装个服务不该先让人装 shell。
#   - 幂等：重复执行不会覆盖已有配置、不会重复生成主密钥、不会丢数据。
#   - 失败可回滚：任何一步失败都退出且不留半装状态，已有服务保持运行。
#   - 配置只写一次：BRIDGE_SECRET 丢了等于所有机器人 token 作废，
#     所以默认绝不重新生成。
#
# 用法：
#   curl -fsSL https://raw.githubusercontent.com/goojia/muse-bridge/main/install.sh | sh
#   sh install.sh --domain bridge.example.com --email you@example.com
#   sh install.sh --from-source --domain bridge.example.com
#   sh install.sh --help
set -eu

# ---------- 默认值 ----------

REPO="${BRIDGE_REPO:-goojia/muse-bridge}"
VERSION="${BRIDGE_VERSION:-latest}"
# BASE_URL 可覆盖下载源。默认走 GitHub Releases；自建镜像、内网分发、
# 或者想锁定某台机器时用得上（也方便离线环境里先同步一份）。
BASE_URL="${BRIDGE_BASE_URL:-}"

# 安装目录布局。默认放在 /opt/muse-bridge，和 DEPLOY.md 的 systemd 示例一致。
PREFIX="${BRIDGE_PREFIX:-/opt/muse-bridge}"
DATA_DIR=""
ENV_FILE=""
SERVICE_NAME="muse-bridge"

DOMAIN=""
ACME_EMAIL=""
ADMIN_USER=""
TLS_MODE=""

FROM_SOURCE=0
NO_SERVICE=0
FORCE=0
DRY_RUN=0
UNINSTALL=0

# 下载工具在运行时装填。
DOWNLOAD=""

# ---------- 输出 ----------

# 只在真的连终端时上色：输出被管道接走时（CI、日志）彩色转义码只会碍事。
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
    C_RESET=$(printf '\033[0m')
    C_BOLD=$(printf '\033[1m')
    C_RED=$(printf '\033[31m')
    C_GREEN=$(printf '\033[32m')
    C_YELLOW=$(printf '\033[33m')
    C_BLUE=$(printf '\033[34m')
else
    C_RESET=''; C_BOLD=''; C_RED=''; C_GREEN=''; C_YELLOW=''; C_BLUE=''
fi

info()  { printf '%s\n' "${C_BLUE}==>${C_RESET} $*"; }
ok()    { printf '%s\n' "${C_GREEN}  ✓${C_RESET} $*"; }
warn()  { printf '%s\n' "${C_YELLOW}  !${C_RESET} $*" >&2; }
step()  { printf '\n%s\n' "${C_BOLD}$*${C_RESET}"; }
die()   { printf '%s\n' "${C_RED}错误:${C_RESET} $*" >&2; exit 1; }

# 临时目录在退出时清理。
#
# 用一个变量 + 统一的 EXIT trap，而不是在下载函数里再 trap 一次：
# 后设的 EXIT trap 会覆盖前面的，导致"失败提示"在下载阶段失效。
TMPDIR_CLEANUP=""

cleanup_tmp() {
    if [ -n "$TMPDIR_CLEANUP" ] && [ -d "$TMPDIR_CLEANUP" ]; then
        rm -rf "$TMPDIR_CLEANUP"
    fi
}
trap cleanup_tmp EXIT

# 失败时给出回滚提示，而不是让人对着半截安装发呆。
on_error() {
    printf '\n%s\n' "${C_RED}安装失败。${C_RESET}" >&2
    printf '已完成的步骤保留在 %s，可以重新执行本脚本继续。\n' "$PREFIX" >&2
    if [ -f "$ENV_FILE" ]; then
        printf '配置未被破坏：%s\n' "$ENV_FILE" >&2
    fi
    exit 1
}
trap on_error INT TERM HUP

usage() {
    cat <<'EOF'
Muse Bridge 安装脚本

用法：
  install.sh [选项]

安装方式：
  （默认）从 GitHub Releases 下载预编译二进制，校验 sha256 后安装
  --from-source        从源码编译安装（需要 git 和 Go 1.27+）

配置：
  --domain <域名>      webhook 用的域名，例如 bridge.example.com
                       不填则装好后手动编辑配置文件
  --email <邮箱>       Let's Encrypt 通知邮箱（申请证书用）
  --admin-user <名>    管理员用户名（默认 admin，仅用于忘记密码时重置）
  --tls <模式>         autocert | cert | off（默认 autocert）
  --prefix <目录>      安装目录（默认 /opt/muse-bridge）

其他：
  --version <版本>     要安装的版本标签，例如 v1.3.0（默认最新）
  --base-url <地址>    自定义下载源（默认 GitHub Releases）。地址下需有
                       bridge-<os>-<arch> 和 checksums.txt
  --no-service         只装二进制和配置，不注册系统服务
  --force              覆盖已存在的二进制（配置和数据永远不覆盖）
  --dry-run            只打印将要做什么，不实际改动
  --uninstall          卸载（保留数据目录，除非再加 --purge）
  --purge              与 --uninstall 一起用，连数据一起删
  -h, --help           显示本帮助

示例：
  # 最简单：装最新版，装完按提示填域名
  sh install.sh

  # 一步到位
  sh install.sh --domain bridge.example.com --email you@example.com

  # 源码编译
  sh install.sh --from-source --domain bridge.example.com
EOF
}

# ---------- 参数解析 ----------

# take_value <选项名>：取出当前选项的值。
#
# 不能直接写 shift 2：参数缺失时 dash 会先报 "can't shift that many"
# 这种内部错误，用户看到的是 shell 的抱怨而不是"你少给了个值"。
take_value() {
    [ $# -ge 2 ] || die "$1 需要一个值"
    printf '%s' "$2"
}

PURGE=0
while [ $# -gt 0 ]; do
    case "$1" in
        --domain)      DOMAIN=$(take_value "$@"); shift 2 ;;
        --email)       ACME_EMAIL=$(take_value "$@"); shift 2 ;;
        --admin-user)  ADMIN_USER=$(take_value "$@"); shift 2 ;;
        --tls)         TLS_MODE=$(take_value "$@"); shift 2 ;;
        --prefix)      PREFIX=$(take_value "$@"); shift 2 ;;
        --version)     VERSION=$(take_value "$@"); shift 2 ;;
        --base-url)    BASE_URL=$(take_value "$@"); shift 2 ;;
        --repo)        REPO=$(take_value "$@"); shift 2 ;;
        --from-source) FROM_SOURCE=1; shift ;;
        --no-service)  NO_SERVICE=1; shift ;;
        --force)       FORCE=1; shift ;;
        --dry-run)     DRY_RUN=1; shift ;;
        --uninstall)   UNINSTALL=1; shift ;;
        --purge)       PURGE=1; shift ;;
        -h|--help)     usage; exit 0 ;;
        *)             die "未知参数: $1（用 --help 查看用法）" ;;
    esac
done

DATA_DIR="$PREFIX/bridge-data"
ENV_FILE="$PREFIX/muse-bridge.env"
LOG_DIR="$PREFIX/log"

# ---------- 环境探测 ----------

# need_root 检查权限。装系统服务必须 root，但 --no-service 时只要目录可写。
need_root() {
    if [ "$(id -u)" -ne 0 ]; then
        die "需要 root 权限（请用 sudo 执行）"
    fi
}

# check_writable 确认能往 PREFIX 里写。
check_writable() {
    if [ -d "$PREFIX" ]; then
        [ -w "$PREFIX" ] || die "没有写入 $PREFIX 的权限（用 sudo，或 --prefix 换目录）"
        return 0
    fi
    # 目录还不存在：检查最近的已存在祖先是否可写。
    probe="$PREFIX"
    while [ ! -d "$probe" ]; do
        parent=$(dirname "$probe")
        [ "$parent" = "$probe" ] && break
        probe="$parent"
    done
    [ -w "$probe" ] || die "没有创建 $PREFIX 的权限（$probe 不可写；用 sudo，或 --prefix 换目录）"
}

# detect_downloader 选一个下载工具。
#
# 优先 curl：它能用 -f 让 HTTP 错误直接失败，
# 而 wget 在某些版本上会把 404 页面当成文件存下来。
detect_downloader() {
    if command -v curl >/dev/null 2>&1; then
        DOWNLOAD="curl"
    elif command -v wget >/dev/null 2>&1; then
        DOWNLOAD="wget"
    else
        die "需要 curl 或 wget 来下载安装包"
    fi
}

# fetch <url> <输出文件>：下载到文件，失败返回非零。
fetch() {
    case "$DOWNLOAD" in
        curl) curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1" ;;
        wget) wget -q -T 30 -O "$2" "$1" ;;
    esac
}

# fetch_stdout <url>：下载到标准输出。
fetch_stdout() {
    case "$DOWNLOAD" in
        curl) curl -fsSL --retry 3 --connect-timeout 15 "$1" ;;
        wget) wget -q -T 30 -O - "$1" ;;
    esac
}

# detect_platform 返回 os/arch，与发布产物的命名保持一致。
detect_platform() {
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    case "$os" in
        linux) ;;
        darwin) ;;
        *) die "不支持的系统: $os（只支持 Linux 和 macOS）" ;;
    esac

    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) die "不支持的架构: $arch（只支持 amd64 和 arm64）" ;;
    esac
    printf '%s/%s' "$os" "$arch"
}

# detect_service_manager 判断用 systemd 还是 OpenRC。
detect_service_manager() {
    if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
        printf 'systemd'
    elif command -v rc-service >/dev/null 2>&1; then
        printf 'openrc'
    else
        printf 'none'
    fi
}

# ---------- 卸载 ----------

do_uninstall() {
    need_root
    info "卸载 Muse Bridge"
    [ "$DRY_RUN" = "1" ] && { info "[dry-run] 将停止并删除服务"; return 0; }

    mgr=$(detect_service_manager)
    case "$mgr" in
        systemd)
            systemctl stop "$SERVICE_NAME" 2>/dev/null || true
            systemctl disable "$SERVICE_NAME" 2>/dev/null || true
            rm -f "/etc/systemd/system/$SERVICE_NAME.service"
            systemctl daemon-reload 2>/dev/null || true
            ;;
        openrc)
            rc-service "$SERVICE_NAME" stop 2>/dev/null || true
            rc-update del "$SERVICE_NAME" default 2>/dev/null || true
            rm -f "/etc/init.d/$SERVICE_NAME"
            ;;
    esac
    ok "服务已停止并移除"

    rm -f "$PREFIX/bridge"
    ok "二进制已删除"

    if [ "$PURGE" = "1" ]; then
        rm -rf "$DATA_DIR" "$ENV_FILE" "$LOG_DIR"
        ok "数据、配置、日志已删除"
        warn "主密钥已随配置一起删除，之前备份的数据库将无法解密机器人 token"
    else
        [ -f "$ENV_FILE" ] && warn "配置保留在 $ENV_FILE（含主密钥，删之前请确认不需要了）"
        [ -d "$DATA_DIR" ] && warn "数据保留在 $DATA_DIR（删之前请先备份）"
        printf '\n连数据一起删：sh install.sh --uninstall --purge\n'
    fi
}

# ---------- 安装：目录与配置 ----------

prepare_dirs() {
    step "1/5 准备目录"
    if [ "$DRY_RUN" = "1" ]; then
        info "[dry-run] 将创建 $PREFIX 及其子目录"
        return 0
    fi
    mkdir -p "$PREFIX" "$DATA_DIR" "$LOG_DIR"
    # 数据目录 700：里面有机器人 token 密文和证书私钥。
    chmod 700 "$DATA_DIR"
    chmod 755 "$LOG_DIR"
    ok "安装目录 $PREFIX"
    ok "数据目录 $DATA_DIR"
}

# write_env 生成配置文件。
#
# 已存在就绝不覆盖：主密钥和机器人 token 是绑定的，覆盖等于让所有桥失效。
write_env() {
    step "2/5 生成配置"

    if [ -f "$ENV_FILE" ]; then
        ok "配置已存在，保持不变：$ENV_FILE"
        # 补上可能缺失的项（老版本装的、手工建的），但不改已有的值。
        ensure_env_defaults
        return 0
    fi

    if [ "$DRY_RUN" = "1" ]; then
        info "[dry-run] 将生成 $ENV_FILE"
        return 0
    fi

    # 主密钥：32 字节随机数的 base64。这是加密机器人 token 的唯一钥匙。
    if command -v openssl >/dev/null 2>&1; then
        secret=$(head -c 32 /dev/urandom | openssl base64 -A)
    else
        # 没有 openssl 时退回 coreutils 的 base64（Alpine 需 coreutils）。
        secret=$(head -c 32 /dev/urandom | base64 | tr -d '\n')
    fi
    [ -n "$secret" ] || die "无法生成主密钥（需要 openssl 或 base64）"

    # 域名没给就留占位，装完提示用户改。
    public_url=""
    if [ -n "$DOMAIN" ]; then
        public_url="https://$DOMAIN"
    fi

    tls="$TLS_MODE"
    if [ -z "$tls" ]; then
        if [ -n "$public_url" ]; then tls="autocert"; else tls="autocert"; fi
    fi

    umask 077
    cat > "$ENV_FILE" <<EOF
# Muse Bridge 配置。由 install.sh 生成于 $(date '+%Y-%m-%d %H:%M:%S')。
# 本文件含主密钥，权限必须是 600，不要提交到任何仓库。
# 改完重启服务生效：$(service_restart_hint)

# 数据目录：SQLite、证书缓存
BRIDGE_DATA_DIR=$DATA_DIR

# 管理后台兜底监听（只绑本机，可 SSH 隧道访问）
BRIDGE_ADDR=127.0.0.1:8080

# 公网监听
BRIDGE_PUBLIC_ADDR=0.0.0.0:443
BRIDGE_PUBLIC_URL=${public_url:-https://bridge.example.com}

# TLS：autocert 自动申请并续期证书（需要 80 端口从公网可达）
BRIDGE_TLS=$tls
BRIDGE_ACME_EMAIL=${ACME_EMAIL:-}

# 加密机器人 token 的主密钥。丢失或更换会导致所有桥的 token 无法解密。
BRIDGE_SECRET=$secret

# 新桥默认收消息方式：webhook 或 poll
BRIDGE_MODE=webhook
EOF
    chmod 600 "$ENV_FILE"
    ok "配置已生成：$ENV_FILE（权限 600）"
}

# ensure_env_defaults 给已有配置补上后加的项，不动已有的值。
ensure_env_defaults() {
    changed=0
    add_if_missing() {
        key="$1"; value="$2"
        if ! grep -q "^$key=" "$ENV_FILE" 2>/dev/null; then
            printf '%s=%s\n' "$key" "$value" >> "$ENV_FILE"
            changed=1
        fi
    }
    add_if_missing BRIDGE_MODE webhook
    add_if_missing BRIDGE_TG_PROXY ""
    add_if_missing BRIDGE_TYPING 1
    [ "$changed" = "1" ] && ok "已补齐缺失的配置项"
    return 0
}

# ---------- 安装：二进制 ----------

install_binary_release() {
    step "3/5 下载二进制"

    # 先检查再下载：已经装了就早点报错，别白下一遍 14MB。
    check_not_installed

    platform=$(detect_platform)
    asset="bridge-$(printf '%s' "$platform" | tr '/' '-')"
    ok "目标平台 $platform，资产 $asset"

    if [ -n "$BASE_URL" ]; then
        # 自定义源：地址本身就是"文件所在目录"，直接拼文件名。
        base="${BASE_URL%/}"
    elif [ "$VERSION" = "latest" ]; then
        base="https://github.com/$REPO/releases/latest/download"
    else
        base="https://github.com/$REPO/releases/download/$VERSION"
    fi

    if [ "$DRY_RUN" = "1" ]; then
        info "[dry-run] 将从 $base/$asset 下载"
        return 0
    fi

    tmpdir=$(mktemp -d)
    TMPDIR_CLEANUP="$tmpdir"

    info "下载 $base/$asset"
    fetch "$base/$asset" "$tmpdir/bridge" || die "下载失败。检查版本号是否存在、网络是否可达 GitHub。"
    [ -s "$tmpdir/bridge" ] || die "下载的文件是空的"

    # 校验摘要。发布里没有 checksums.txt 就拒绝安装：
    # 下载下来的是要直接以 root 执行的程序，只靠 HTTPS 不足以信任。
    info "校验 sha256"
    if ! fetch "$base/checksums.txt" "$tmpdir/checksums.txt"; then
        die "下载 checksums.txt 失败，无法校验完整性。为安全起见拒绝安装。"
    fi
    expected=$(grep " $asset\$" "$tmpdir/checksums.txt" | awk '{print $1}' | head -1)
    [ -n "$expected" ] || die "checksums.txt 里没有 $asset 的摘要"
    actual=$(sha256_of "$tmpdir/bridge")
    [ "$expected" = "$actual" ] || die "sha256 不匹配（期望 $expected，实际 $actual）。下载可能被篡改或损坏。"
    ok "sha256 校验通过"

    install_file "$tmpdir/bridge"
}

install_binary_source() {
    step "3/5 从源码编译"

    check_not_installed

    command -v git >/dev/null 2>&1 || die "源码安装需要 git"
    command -v go >/dev/null 2>&1 || die "源码安装需要 Go 1.27+（https://go.dev/dl/）"

    go_version=$(go version | awk '{print $3}' | sed 's/^go//')
    ok "Go $go_version"

    if [ "$DRY_RUN" = "1" ]; then
        info "[dry-run] 将 clone $REPO 并编译"
        return 0
    fi

    tmpdir=$(mktemp -d)
    TMPDIR_CLEANUP="$tmpdir"

    # GIT_URL 允许指向内网镜像或本地路径；默认是 GitHub。
    git_url="${GIT_URL:-https://github.com/$REPO.git}"
    info "克隆仓库 $git_url"
    if [ "$VERSION" = "latest" ]; then
        git clone --depth 1 "$git_url" "$tmpdir/src" 2>&1 | tail -2
    else
        git clone --depth 1 --branch "$VERSION" "$git_url" "$tmpdir/src" 2>&1 | tail -2
    fi
    ok "已克隆"

    info "编译（这一步可能要几分钟）"
    # 版本号取 tag，让在线升级能正确比较。
    build_version=$(cd "$tmpdir/src" && git describe --tags --always 2>/dev/null || echo dev)
    ( cd "$tmpdir/src/bridge" && \
      CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X muse-bridge/internal/version.Version=$build_version" \
        -o "$tmpdir/bridge" ./cmd/bridge ) || die "编译失败"
    ok "编译完成（版本 $build_version）"

    install_file "$tmpdir/bridge"
}

# install_file 把二进制放到安装目录。已存在时按 --force 决定是否覆盖。
# check_not_installed 在下载/编译之前就挡住重复安装。
check_not_installed() {
    [ -f "$PREFIX/bridge" ] || return 0
    [ "$FORCE" = "1" ] && return 0

    existing=$("$PREFIX/bridge" --version 2>/dev/null | head -1 || true)
    [ -n "$existing" ] || existing="版本未知"
    die "已经装过（$existing）。要覆盖请加 --force。
覆盖二进制不影响数据，但正在运行的服务需要重启。"
}

install_file() {
    src="$1"

    # 先写到同目录临时文件再 mv：mv 在同一文件系统上是原子的，
    # 不会出现"文件写了一半被服务读到"的情况。
    cp "$src" "$PREFIX/bridge.new"
    chmod 755 "$PREFIX/bridge.new"
    mv "$PREFIX/bridge.new" "$PREFIX/bridge"
    ok "二进制已安装：$PREFIX/bridge"
}

# sha256_of 跨平台算摘要。
sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "$1" | awk '{print $NF}'
    else
        die "需要 sha256sum / shasum / openssl 之一来校验下载"
    fi
}

# ---------- 安装：服务 ----------

service_restart_hint() {
    case "$(detect_service_manager)" in
        systemd) printf 'systemctl restart %s' "$SERVICE_NAME" ;;
        openrc)  printf 'rc-service %s restart' "$SERVICE_NAME" ;;
        *)       printf '手动重启进程' ;;
    esac
}

install_service() {
    step "4/5 注册系统服务"

    if [ "$NO_SERVICE" = "1" ]; then
        ok "已指定 --no-service，跳过"
        return 0
    fi

    mgr=$(detect_service_manager)
    case "$mgr" in
        systemd) install_systemd ;;
        openrc)  install_openrc ;;
        *)       warn "没有检测到 systemd 或 OpenRC，跳过服务注册"
                 printf '    可以手动启动：%s\n' "$PREFIX/bridge"
                 return 0 ;;
    esac
}

install_systemd() {
    unit="/etc/systemd/system/$SERVICE_NAME.service"
    if [ "$DRY_RUN" = "1" ]; then
        info "[dry-run] 将写入 $unit"
        return 0
    fi
    cat > "$unit" <<EOF
[Unit]
Description=Muse Bridge (Telegram <-> Muse)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENV_FILE
ExecStart=$PREFIX/bridge
Restart=always
RestartSec=5
LimitNOFILE=65535
StandardOutput=append:$LOG_DIR/muse-bridge.log
StandardError=append:$LOG_DIR/muse-bridge.err

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
    ok "systemd 服务已注册：$unit"
}

install_openrc() {
    initd="/etc/init.d/$SERVICE_NAME"
    if [ "$DRY_RUN" = "1" ]; then
        info "[dry-run] 将写入 $initd"
        return 0
    fi
    cat > "$initd" <<EOF
#!/sbin/openrc-run
# 由 install.sh 生成。

name="$SERVICE_NAME"
description="Muse Bridge (Telegram <-> Muse)"

command="$PREFIX/bridge"
command_background=true
pidfile="/run/\${RC_SVCNAME}.pid"
respawn_delay=5
respawn_max=0

output_log="$LOG_DIR/muse-bridge.log"
error_log="$LOG_DIR/muse-bridge.err"

depend() {
    need net
    after firewall
}

start_pre() {
    checkpath --directory --owner root --mode 0755 $LOG_DIR
    checkpath --directory --owner root --mode 0700 $DATA_DIR
    # 密钥只放在 0600 的文件里，不写进 conf.d。
    if [ -r $ENV_FILE ]; then
        set -a
        # shellcheck disable=SC1091
        . $ENV_FILE
        set +a
    fi
}
EOF
    chmod 755 "$initd"
    rc-update add "$SERVICE_NAME" default >/dev/null 2>&1 || true
    ok "OpenRC 服务已注册：$initd"
}

start_service() {
    step "5/5 启动服务"

    if [ "$NO_SERVICE" = "1" ]; then
        ok "已指定 --no-service，跳过启动"
        return 0
    fi
    if [ "$DRY_RUN" = "1" ]; then
        info "[dry-run] 将启动服务"
        return 0
    fi

    mgr=$(detect_service_manager)
    case "$mgr" in
        systemd) systemctl restart "$SERVICE_NAME" ;;
        openrc)  rc-service "$SERVICE_NAME" restart ;;
        *)       return 0 ;;
    esac
    sleep 2

    # 起来没起来必须验证：配置错了进程会立刻退出，
    # 而 systemctl start 在 Type=simple 下照样返回 0。
    case "$mgr" in
        systemd)
            if ! systemctl is-active --quiet "$SERVICE_NAME"; then
                printf '\n最近日志：\n' >&2
                tail -20 "$LOG_DIR/muse-bridge.err" 2>/dev/null >&2 || true
                die "服务启动失败，看上面的日志"
            fi
            ;;
        openrc)
            if ! rc-service "$SERVICE_NAME" status >/dev/null 2>&1; then
                printf '\n最近日志：\n' >&2
                tail -20 "$LOG_DIR/muse-bridge.err" 2>/dev/null >&2 || true
                die "服务启动失败，看上面的日志"
            fi
            ;;
    esac
    ok "服务已启动"
}

# ---------- 收尾提示 ----------

print_summary() {
    printf '\n%s\n' "${C_BOLD}安装完成${C_RESET}"
    printf '  安装目录  %s\n' "$PREFIX"
    printf '  数据目录  %s\n' "$DATA_DIR"
    printf '  配置文件  %s\n' "$ENV_FILE"
    printf '  日志      %s/muse-bridge.err\n' "$LOG_DIR"
    printf '\n'

    if [ -z "$DOMAIN" ] && [ "$UNINSTALL" != "1" ]; then
        printf '%s\n' "${C_YELLOW}下一步：填域名${C_RESET}"
        printf '  编辑 %s，把 BRIDGE_PUBLIC_URL 改成你的域名：\n' "$ENV_FILE"
        printf '    BRIDGE_PUBLIC_URL=https://bridge.example.com\n'
        printf '    BRIDGE_ACME_EMAIL=you@example.com\n'
        printf '  然后 %s\n' "$(service_restart_hint)"
        printf '\n'
        printf '  域名必须解析到这台机器，且 80 和 443 端口从公网可达（申请证书要用）。\n'
        printf '\n'
    fi

    if [ -n "$DOMAIN" ]; then
        printf '%s\n' "${C_BOLD}打开后台${C_RESET}"
        printf '  https://%s/\n' "$DOMAIN"
        printf '\n'
        printf '  首次打开会引导你创建管理员账号（没有默认密码）。\n'
        printf '  %s\n' "${C_YELLOW}请立刻打开创建：还没建号的实例，谁先访问谁就是管理员。${C_RESET}"
        printf '\n'
    fi

    printf '%s\n' "${C_BOLD}常用命令${C_RESET}"
    printf '  看日志    tail -f %s/muse-bridge.err\n' "$LOG_DIR"
    printf '  重启      %s\n' "$(service_restart_hint)"
    printf '  升级      后台「升级」页，或重跑本脚本加 --force\n'
    printf '\n'
    printf '备份配置（含主密钥，丢了所有机器人 token 都得重填）：\n'
    printf '  cp %s ~/muse-bridge.env.bak\n' "$ENV_FILE"
    printf '\n'
}

# ---------- 主流程 ----------

main() {
    if [ "$UNINSTALL" = "1" ]; then
        do_uninstall
        return 0
    fi

    printf '%s\n' "${C_BOLD}Muse Bridge 安装脚本${C_RESET}"
    printf '  仓库      %s\n' "$REPO"
    printf '  版本      %s\n' "$VERSION"
    printf '  安装到    %s\n' "$PREFIX"
    if [ "$FROM_SOURCE" = "1" ]; then
        printf '  方式      源码编译\n'
    else
        printf '  方式      预编译二进制\n'
    fi
    printf '\n'

    detect_downloader

    if [ "$NO_SERVICE" != "1" ]; then
        need_root
    else
        # 不装服务时只要目录可写。注意要分别判断"目录已存在"和"需要创建"：
        # 直接对不存在的路径做 -w 判断会永远返回假。
        check_writable
    fi

    prepare_dirs
    write_env

    if [ "$FROM_SOURCE" = "1" ]; then
        install_binary_source
    else
        install_binary_release
    fi

    install_service
    start_service

    if [ "$DRY_RUN" != "1" ]; then
        print_summary
    else
        printf '\n%s\n' "${C_YELLOW}dry-run 结束，没有做任何改动。${C_RESET}"
    fi
}

main

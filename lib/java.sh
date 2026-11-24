#!/usr/bin/env bash
# JDK 21。
#
# Nova 后端 build.gradle.kts 里写死了 JavaLanguageVersion.of(21)，
# 所以必须是 21，装 17 会在启动时报 UnsupportedClassVersionError。
#
# 服务器只需要 JRE（跑 jar），不需要 JDK（编译）。headless 版还省掉一堆
# 图形库依赖，内存占用也更低。

JAVA_PKG_DEBIAN="openjdk-21-jre-headless"
JAVA_PKG_RHEL="java-21-openjdk-headless"

install_java() {
    step "安装 JDK 21"

    # 已有 21 就跳过
    if command -v java >/dev/null 2>&1; then
        local ver
        ver="$(java -version 2>&1 | head -1 | grep -oE '[0-9]+' | head -1)"
        if [[ "$ver" == "21" ]]; then
            ok "已安装 JDK 21，跳过"
            JAVA_HOME="$(dirname "$(dirname "$(readlink -f "$(command -v java)")")")"
            return 0
        fi
        warn "检测到 Java $ver，需要 21。将安装 21 并切换默认版本。"
    fi

    case "$OS_FAMILY" in
        debian)
            # Ubuntu 24.04 / Debian 12 的官方源里有 openjdk-21
            if ! pkg_install "$JAVA_PKG_DEBIAN"; then
                # Ubuntu 22.04 的源里没有 21，需要加 PPA
                warn "官方源没有 JDK 21，尝试添加 PPA"
                pkg_install software-properties-common || die "无法安装 software-properties-common"
                add-apt-repository -y ppa:openjdk-r/ppa >/dev/null 2>&1 || true
                pkg_update || true
                pkg_install "$JAVA_PKG_DEBIAN" || die "JDK 21 安装失败。
Ubuntu 22.04 及更早版本可能需要手动安装：
  sudo apt install openjdk-21-jre-headless
或参考 https://adoptium.net/installation/linux/"
            fi
            ;;
        rhel)
            pkg_install "$JAVA_PKG_RHEL" || die "JDK 21 安装失败。
CentOS Stream 8 等较老系统可能需要先启用 AppStream 源。"
            ;;
    esac

    require_cmd java "JDK 安装后仍找不到 java 命令"

    local ver
    ver="$(java -version 2>&1 | head -1 | grep -oE '[0-9]+' | head -1)"
    if [[ "$ver" != "21" ]]; then
        die "安装后 Java 版本仍为 $ver，需要 21。请检查 update-alternatives。"
    fi

    # 解析 JAVA_HOME，systemd unit 里要用绝对路径
    local java_bin
    java_bin="$(readlink -f "$(command -v java)")"
    JAVA_HOME="$(dirname "$(dirname "$java_bin")")"

    ok "JDK 21 就绪：$JAVA_HOME"
}

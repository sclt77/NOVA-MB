# NOVA-MB

Nova 的一键部署面板。**独立项目**，与 Nova 业务代码（`D:/NOVA`）分开维护、分开发版。

```bash
curl -sSO https://raw.githubusercontent.com/sclt77/NOVA-MB/main/install.sh
sudo bash install.sh
```

跑完终端会打印面板地址、初始账号与密码。

## 它能做什么

| 功能 | 说明 |
| --- | --- |
| **一键安装** | 识别发行版，装 JDK21 + MySQL8 + Nginx，建目录建用户，注册 systemd |
| **部署管理** | 上传 zip 包自动校验，一键部署，**健康检查失败自动回滚**，保留最近 5 个版本 |
| **备份恢复** | 数据库 + uploads 全量备份，保留策略，**一键验证备份可恢复** |
| **域名证书** | 绑定域名写 Nginx 配置，Let's Encrypt 证书申请与自动续期 |
| **服务控制** | 启停重启 nova-api / nginx / mysql |
| **数据库信息** | 表列表、行数、容量（只读，不提供 SQL 控制台） |
| **定时任务** | 备份、验证、健康检查，只允许预定义任务 |
| **日志查看** | 应用、Nginx、定时任务日志，白名单固定 |
| **审计日志** | 所有写操作记录操作人、时间、IP、结果 |
| **资源监控** | 内存、磁盘、负载，超阈值告警 |

## 项目结构

```
mb/
├── install.sh              一键安装
├── lib/                    安装模块
│   ├── common.sh           日志、幂等工具、下载校验
│   ├── detect.sh           发行版/架构/资源识别
│   ├── deps.sh             基础依赖
│   ├── java.sh             JDK 21
│   ├── mysql.sh            MySQL 8
│   ├── nginx.sh            Nginx
│   ├── panel.sh            面板与业务：用户、目录、systemd
│   ├── firewall.sh         防火墙
│   └── summary.sh          安装汇总
├── scripts/                运维脚本（面板调用的实体）
│   ├── deploy.sh           部署 + 回滚
│   ├── backup.sh           备份 + 验证
│   ├── restore.sh          恢复
│   ├── healthcheck.sh      健康检查
│   └── cron.sh             定时任务
├── panel/                  面板程序（Go）
│   ├── cmd/nova-panel/     入口
│   ├── internal/
│   │   ├── config/         配置加载
│   │   ├── runner/         **命令执行白名单（安全核心）**
│   │   ├── auth/           密码、会话、CSRF、限流
│   │   ├── store/          SQLite：管理员、会话、审计
│   │   ├── service/        业务：系统/部署/备份/域名/数据库
│   │   └── api/            HTTP 接口
│   └── Makefile            交叉编译
└── docs/
    ├── ARCHITECTURE.md     设计说明与安全边界
    └── OPERATIONS.md       运维手册
```

## 设计原则

**这不是宝塔。** 面板只提供 Nova 部署所需的固定动作，不提供任意 shell、
任意文件管理、任意软件安装。

| 原则 | 实现 |
| --- | --- |
| 白名单动作 | `internal/runner/actions.go` 穷举所有能执行的动作，没有自由命令入口 |
| 不拼 shell | 一律 `exec.Command(bin, args...)` 参数数组形式 |
| 独立进程 | 面板与 nova-api 分离，业务挂了面板还能救 |
| 双系统用户 | 面板 `nova-panel` 权限大于业务 `nova`，业务被攻破拿不到面板 |
| 幂等 | 安装与部署脚本重复执行不破坏环境 |
| 可回滚 | 软链切版本，健康检查失败自动切回 |
| 全量备份 | 数据库 + uploads，可一键试恢复到临时库 |
| 可审计 | 所有写操作记录操作人与 IP |

详见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

## 安装参数

```bash
sudo bash install.sh --help

  --panel-port PORT     面板端口，默认 8888
  --domain DOMAIN       直接绑定域名
  --db-password PASS    指定数据库密码
  --skip-firewall       不配置防火墙
  --uninstall           卸载（保留数据）
```

## 安装后

```bash
# 面板默认只监听 127.0.0.1，本地访问需 SSH 端口转发
ssh -L 8888:127.0.0.1:8888 root@你的服务器
# 然后浏览器打开 http://127.0.0.1:8888
```

初始密码在首次启动时打印，也写在 `/var/lib/nova/panel/initial-password.txt`。

## 开发

```bash
cd panel

# 本地编译（当前平台）
make dev

# 交叉编译 Linux 两个架构
make linux-amd64 linux-arm64

# 产物在 ../dist/，含 SHA256SUMS
```

面板用 Go 单二进制 + embed 静态资源，部署时只有一个文件，无运行时依赖。

## 目录约定（安装后）

| 路径 | 用途 |
| --- | --- |
| `/opt/nova/panel/` | 面板程序与运维脚本 |
| `/opt/nova/app/` | Nova 后端 jar 与前端静态文件（软链到 current） |
| `/opt/nova/releases/` | 历史部署包 |
| `/var/lib/nova/uploads/` | 用户上传的图片 |
| `/var/lib/nova/backup/` | 备份文件 |
| `/var/lib/nova/panel/` | 面板数据（SQLite） |
| `/etc/nova/` | 配置与环境变量（含数据库密码，权限 640） |
| `/var/log/nova/` | 日志 |

## 系统要求

- Linux，systemd（Ubuntu 20.04+ / Debian 11+ / AlmaLinux 8+ / Rocky 8+）
- x86_64 或 aarch64
- 内存 ≥ 1.5GB（推荐 2 核 4G）
- 磁盘 ≥ 20GB（图片不压缩，按 4GB/月增长估算）

## 许可

私有项目，未授权请勿分发。

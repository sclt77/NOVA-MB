# NOVA-MB

Nova 的一键部署面板。**独立项目**，与 Nova 业务代码（`D:/NOVA`）分开维护、分开发版。

```
mb/
├── install.sh              一键安装（curl | bash 那种）
├── lib/                    安装脚本的模块，被 install.sh source
│   ├── common.sh           日志、颜色、错误处理、幂等工具
│   ├── detect.sh           发行版 / 架构 / 系统资源识别
│   ├── deps.sh             基础依赖（curl、tar、cron 等）
│   ├── java.sh             JDK 21
│   ├── mysql.sh            MySQL 8
│   ├── nginx.sh            Nginx
│   ├── panel.sh            面板本体：用户、目录、二进制、systemd
│   ├── firewall.sh         防火墙放行
│   └── summary.sh          安装结果汇总
├── scripts/                运维脚本（被面板和 CLI 共用）
│   ├── deploy.sh           部署 Nova 包
│   ├── rollback.sh         回滚到上一个版本
│   ├── backup.sh           备份数据库 + uploads
│   ├── restore.sh          从备份恢复
│   └── healthcheck.sh      部署后健康检查
└── docs/
    ├── ARCHITECTURE.md     设计说明与安全边界
    └── OPERATIONS.md       日常运维手册
```

## 快速开始

```bash
# 一行安装（脚本地址在仓库发版后填这里）
curl -sSO https://raw.githubusercontent.com/sclt77/NOVA-MB/main/install.sh
sudo bash install.sh
```

安装完成后终端会打印面板地址、初始账号与密码。

## 设计原则

**这不是宝塔。** 面板只提供 Nova 部署所需的固定动作，不提供任意 shell、
任意文件管理、任意软件安装。原因见 `docs/ARCHITECTURE.md`。

| 原则 | 含义 |
| --- | --- |
| 白名单动作 | 每个按钮背后是写死的命令，参数经过校验，不拼 shell |
| 独立进程 | 面板与 nova-api 分离，业务挂了面板还能救 |
| 幂等 | 安装与部署脚本重复执行不会破坏已有环境 |
| 可回滚 | 每次部署保留历史版本，健康检查失败自动切回 |
| 全量备份 | 数据库 + uploads 一起备份，且可一键试恢复 |
| 可审计 | 所有写操作记日志，含操作人与时间 |

## 目录约定（安装后）

| 路径 | 用途 |
| --- | --- |
| `/opt/nova/panel/` | 面板程序与静态资源 |
| `/opt/nova/app/` | Nova 后端 jar 与前端静态文件 |
| `/opt/nova/releases/` | 历史部署包，回滚用 |
| `/var/lib/nova/uploads/` | 用户上传的图片 |
| `/var/lib/nova/backup/` | 备份文件 |
| `/etc/nova/` | 配置与环境变量（含数据库密码，权限 600） |
| `/var/log/nova/` | 应用与面板日志 |

## 开发状态

- [x] 仓库与目录规划
- [ ] `install.sh` 及 lib 模块
- [ ] 运维脚本（deploy / backup / rollback）
- [ ] 面板后端（Go）
- [ ] 面板前端
- [ ] 发版打包

## 许可

私有项目，未授权请勿分发。

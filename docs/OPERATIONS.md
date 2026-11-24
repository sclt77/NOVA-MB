# 运维手册

## 部署 Nova 业务包

### 打包

在 Nova 业务仓库根目录执行：

```bash
cd /d/NOVA

# 1. 构建后端
cd HD/nova-api && gradle bootJar
# 产物：build/libs/nova-api-0.1.0.jar

# 2. 构建前端
cd ../../APP && flutter build web
# 产物：build/web/

# 3. 组装部署包
cd /d/NOVA
rm -rf dist && mkdir -p dist/web
cp HD/nova-api/build/libs/nova-api-0.1.0.jar dist/nova-api.jar
cp -r APP/build/web/* dist/web/
cat > dist/manifest.json <<EOF
{
  "version": "$(date +%Y%m%d-%H%M%S)",
  "built_at": "$(date -Iseconds)",
  "commit": "$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
}
EOF

cd dist && zip -qr ../nova-deploy-$(date +%Y%m%d-%H%M%S).zip .
```

### 包结构约定

```
nova-api.jar          后端
web/index.html        前端入口（必须有）
web/...               前端其他资源
manifest.json         版本信息（可选）
```

缺少 `nova-api.jar` 或 `web/index.html` 会被拒绝部署。

### 部署

```bash
# 面板里上传，或 SSH 执行
sudo /opt/nova/panel/scripts/deploy.sh /path/to/nova-deploy-xxx.zip
```

部署流程会自动：

1. 校验包结构与路径安全
2. 备份数据库
3. 解压到 `/opt/nova/releases/<时间戳>/`
4. 停服 → 切软链 → 启动
5. 健康检查（连续 2 次 200 才算通过）
6. **失败自动回滚**

### 回滚

```bash
# 回到上一个版本
sudo /opt/nova/panel/scripts/deploy.sh --rollback

# 查看历史版本
sudo /opt/nova/panel/scripts/deploy.sh --list
```

只保留最近 5 个版本，更早的自动清理。

## 备份

### 手动备份

```bash
# 全量（数据库 + 上传文件）
sudo /opt/nova/panel/scripts/backup.sh

# 只备份数据库（快，部署前用）
sudo /opt/nova/panel/scripts/backup.sh --quick

# 列出已有备份
sudo /opt/nova/panel/scripts/backup.sh --list

# 验证最近一次备份是否真的能恢复
sudo /opt/nova/panel/scripts/backup.sh --verify
```

### 定时备份

面板里开启，或手动加 crontab：

```bash
# 每天凌晨 3 点全量备份
0 3 * * * /opt/nova/panel/scripts/backup.sh >> /var/log/nova/backup.log 2>&1

# 每周日凌晨 4 点验证一次备份
0 4 * * 0 /opt/nova/panel/scripts/backup.sh --verify >> /var/log/nova/backup.log 2>&1
```

### 恢复

```bash
# 交互选择备份
sudo /opt/nova/panel/scripts/restore.sh

# 指定时间戳，只恢复数据库
sudo /opt/nova/panel/scripts/restore.sh --db 20261124-030000

# 恢复数据库 + 上传文件
sudo /opt/nova/panel/scripts/restore.sh --all 20261124-030000
```

**恢复前会自动备份当前状态**，选错备份还能回来。

### 异地备份

本地备份挡不住磁盘损坏。建议同步到对象存储：

```bash
# 以阿里云 OSS 为例，装 ossutil 后加到 crontab
30 3 * * * ossutil cp -r /var/lib/nova/backup/ oss://your-bucket/nova-backup/ --update
```

或者在面板的备份设置里填对象存储参数（如果已实现该功能）。

## 健康检查

```bash
# 完整检查
sudo /opt/nova/panel/scripts/healthcheck.sh

# 持续监控（每 30 秒刷新）
sudo /opt/nova/panel/scripts/healthcheck.sh --watch

# JSON 输出（给监控系统用）
/opt/nova/panel/scripts/healthcheck.sh --json
```

检查项：

| 项 | 说明 |
| --- | --- |
| 服务状态 | nova-api / nginx / mysql 是否 active |
| 接口 | `/api/health` 返回 200 |
| 数据库 | 能否连通 |
| 磁盘 | 使用率超过 80% 告警、90% 报错 |
| 上传目录 | 存在且可写 |
| 备份时效 | 超过 48 小时未备份报警 |
| 端口安全 | 3306/8080 是否意外暴露到公网 |

## 常见故障

### 服务起不来

```bash
# 看日志
journalctl -u nova-api -n 100 --no-pager
tail -100 /var/log/nova/nova-api.log

# 常见原因：
# - 数据库连不上：检查 /etc/nova/env 里的密码与 MySQL 是否一致
# - 端口被占：ss -tlnp | grep 8080
# - 迁移失败：Flyway 报错，看具体哪条 SQL
```

### 磁盘满了

```bash
# 找大文件
du -sh /var/lib/nova/* | sort -h
du -sh /var/log/nova/* | sort -h

# 清理旧日志
journalctl --vacuum-time=7d

# 清理旧备份（保留最近 3 份）
ls -t /var/lib/nova/backup/db-*.sql.gz | tail -n +4 | xargs -r rm -f
```

Nova 的图片不压缩，是磁盘增长的主要来源。如果图片太多，考虑：

- 加数据盘，把 `/var/lib/nova/uploads` 挂到新盘
- 迁移到对象存储

### 数据库连不上

```bash
# 检查 MySQL 是否在跑
systemctl status mysql

# 测试连接
mysql -u nova -p -h 127.0.0.1 nova -e "SELECT 1"

# 密码忘了？从 /etc/nova/env 里读
grep NOVA_DB_PASSWORD /etc/nova/env
```

### 部署后前端白屏

```bash
# 1. 检查 web 目录软链是否正常
ls -l /opt/nova/app/web

# 2. 检查 index.html 是否存在
ls -l /opt/nova/app/web/index.html

# 3. 检查 nginx 配置
nginx -t

# 4. 看浏览器控制台报错（多半是 API 地址不对）
```

前端连的后端地址在 `APP/lib/data/api_client.dart` 的 `API_BASE_URL`，
必须是**相对路径**（`/api`）或与域名一致的绝对地址。

### 域名绑定后打不开

```bash
# 1. DNS 是否生效
dig +short your-domain.com

# 2. nginx 配置里的 server_name 是否对
grep server_name /etc/nginx/conf.d/nova.conf

# 3. 防火墙是否放行 80/443
ufw status

# 4. 后端是否在跑
curl -I http://127.0.0.1/api/health
```

## 环境变量

配置在 `/etc/nova/env`，权限 640。改完要重启服务：

```bash
sudo systemctl restart nova-api
```

| 变量 | 说明 |
| --- | --- |
| `NOVA_DB_PASSWORD` | 数据库密码，**备份这个文件** |
| `NOVA_JWT_SECRET` | JWT 密钥，换掉会让所有用户掉线 |
| `NOVA_DEV_MODE` | 生产必须为 `false` |
| `NOVA_TRUST_PROXY` | 在 Nginx 后面必须为 `true` |
| `NOVA_PUBLIC_BASE_URL` | 对外域名，邮件里的链接按它拼 |
| `NOVA_UPLOAD_DIR` | 上传目录 |
| `NOVA_TRASH_RETENTION_DAYS` | 回收站保留天数，默认 15 |

## 与 Nova 业务仓库的约定

面板**不 import** 任何业务代码，只依赖这些约定：

| 约定 | 值 |
| --- | --- |
| jar 文件名 | `nova-api.jar` |
| 前端入口 | `web/index.html` |
| 健康检查 | `GET /api/health` 返回 `{"code":0,...}` |
| 后端端口 | `8080`（只监听 127.0.0.1） |
| 环境变量文件 | `/etc/nova/env` |
| 上传目录 | `/var/lib/nova/uploads` |

只要这些不变，业务代码怎么改都不影响面板。

## 面板本身

### 启动与停止

```bash
systemctl start nova-panel
systemctl stop nova-panel
systemctl status nova-panel
journalctl -u nova-panel -n 50 --no-pager
```

### 忘记密码

```bash
# 停服务后重置，新密码会打印到终端
systemctl stop nova-panel
/opt/nova/panel/nova-panel --reset-password
systemctl start nova-panel
```

### 面板访问不了

面板默认只监听 `127.0.0.1`，这是**有意为之**——不对外暴露才安全。

```bash
# 方式一：SSH 端口转发（推荐）
ssh -L 8888:127.0.0.1:8888 root@你的服务器
# 本地浏览器打开 http://127.0.0.1:8888

# 方式二：确实要对外暴露（不推荐）
# 编辑 /etc/systemd/system/nova-panel.service，改 ExecStart 加 --listen 0.0.0.0
# 然后用防火墙限制来源 IP，并配置 HTTPS
```

### 面板升级

```bash
# 下载新版本
curl -sSL -o /tmp/nova-panel \
  https://github.com/sclt77/NOVA-MB/releases/latest/download/nova-panel-linux-amd64
chmod +x /tmp/nova-panel

# 替换并重启
systemctl stop nova-panel
install -m 755 /tmp/nova-panel /opt/nova/panel/nova-panel
chown nova-panel:nova-panel /opt/nova/panel/nova-panel
systemctl start nova-panel
```

面板数据在 `/var/lib/nova/panel/panel.db`，升级不丢账号与审计记录。

## 面板的边界

**能做的**：部署、回滚、备份、恢复、域名、证书、服务启停、看日志、看数据库容量。

**不能做的**（有意为之）：

- 执行任意 shell 命令
- 浏览任意目录文件
- 安装任意软件
- 执行任意 SQL
- 编辑 Nginx 配置原文

需要这些能力时请用 SSH。面板的定位是「让常见的运维动作更省事」，
不是「替代 SSH」。

# VNext 部署:Docker 镜像、Compose 三种模式与一键脚本

需求 §13(DEPLOY01–10)。镜像 `kjoner/litebox`(linux/amd64 + linux/arm64),由 `v*` tag 触发的
`.github/workflows/release.yml` 构建推送;**普通提交与 PR 不发布任何镜像**。

## 1. 镜像里有什么

| 路径 | 内容 |
|---|---|
| `/usr/local/bin/litebox` | 主控(内嵌前端,时区库编进二进制) |
| `/opt/litebox/assets/` | 要分发到节点的产物:精简构建的 sing-box(固定构建标签)、litebox-probe、mita / mieru、realm,两种架构各一份 |
| `/opt/litebox/assets/release-manifest.json` | 发布清单:每个文件的组件、架构、版本、能力、大小、sha256。**主控分发前逐个核对**,对不上就拒绝分发 |
| `/opt/litebox/deploy/` | 与这个镜像同版本的 Compose 文件(部署脚本升级时从新镜像里取出来覆盖) |
| `/data`(卷) | 库、备份、节点访问私钥 |

容器以 uid 10001 运行,不需要 privileged、host 网络或 Docker socket;根文件系统可只读(`/tmp` 用 tmpfs)。
容器里没有 Go / Node / git,启动时不现场编译任何东西。

## 2. 三种模式

| 模式 | 文件 | 起来的容器 | 用途 |
|---|---|---|---|
| SQLite(默认) | `compose.yaml` | 只有主控 | 单机个人使用,依赖最少 |
| 快捷 PostgreSQL | `compose.yaml` + `compose.postgres.yaml` | 主控 + 专用 PG 17 | 想顺手装好数据库 |
| 已有 PostgreSQL | `compose.yaml` + `compose.external-postgres.yaml`(+ `compose.pg-network.yaml`) | 只有主控 | 共享一个已有的 PG 实例 |

* **SQLite 模式不会启动 PG、不要求 PG 的口令、也不依赖它的健康检查**(PG 的一切都只在叠加文件里);
* 快捷 PG:不向宿主发布 5432;健康检查通过后主控才启动并自己迁移;集群管理员(postgres)与应用账号(litebox)分开,
  应用账号由 `pg-init/10-litebox.sh` 建,**不是超级用户、没有 CREATEDB**;镜像用 digest 锁死在测过的 17.x;
  **PG 大版本升级是独立的迁移任务**(pg_dump / 恢复),不能只改标签;PG 18 官方镜像的数据目录改成了
  `/var/lib/postgresql`,换 18 时挂载点也要跟着改;
* 已有 PG:**不创建、不修改、不删除共享实例上的任何东西**。库与应用账号由管理员预先建好(SQL 见 `migration.md` §2);
  PG 在同一台宿主的另一个 Compose 项目里时,地址写那个项目里的服务名(不要写 localhost,那是容器自己),
  并叠加 `compose.pg-network.yaml` 加入它的 external network。跨主机用可验证的 TLS(`sslmode=verify-full`)。

## 3. 一键脚本 `deploy/docker/litebox.sh`

```bash
# 第一次安装(版本必须明确,不提供 latest)
litebox.sh install --dir /opt/litebox-docker --mode sqlite --version v1.0.0 --base-url https://panel.example.com
litebox.sh install --dir /opt/litebox-docker --mode postgres --version v1.0.0 --base-url https://panel.example.com
litebox.sh install --dir /opt/litebox-docker --mode external-postgres --version v1.0.0 \
    --base-url https://panel.example.com --dsn-file ./dsn [--pg-network shared-pg]

litebox.sh status | logs
litebox.sh backup                      # 应用库 + .env + secrets + override,验证通过后保留最近 3 份
litebox.sh verify-backup <文件>        # 断网的隔离容器里打开:完整性 + 用备份里的主密钥抽查解密
litebox.sh restore <文件>              # 先验证 → 停主控 → 当前数据挪到一边 → 恢复 → 启动
litebox.sh update --version v1.1.0     # 拉镜像 → 数据副本上演练迁移 → 备份 → 换版本 → 就绪检查,失败自动回滚
```

* 重复执行沿用已有的 `.env`、`secrets/` 与 `compose.override.yaml`,**绝不重新生成主密钥、不覆盖数据**;版本相同且健康时不重启;
* 第一次安装时的管理员口令在日志里:`litebox.sh logs | grep 初始密码`;
* 自定义(反代、资源上限、额外挂载)写进 `compose.override.yaml`,升级不会动它;
* 备份按敏感数据处理:里面有主密钥。新备份验证通过之前不删任何旧备份,没通过的改名成 `.unverified`;
* `update` 失败回滚的是**应用与应用数据库**;恢复主控库不会回滚节点上的东西 —— 恢复之后先核对配置版本、撤权与计量,再恢复调度;
* 迁移不重启任何节点核心。

## 4. 原生安装(不用 Docker)

`scripts/litebox-install.sh` 默认下载 GitHub Release 里的预编译发布包(`litebox_<版本>_linux_<架构>.tar.gz`),
核对 `SHA256SUMS` 之后安装;`LITEBOX_VERSION=vX.Y.Z` 指定版本,不给时取最新的正式版。
要像以前那样在本机构建:加 `--build-from-source`(那条路径一字未改)。

## 5. 从 systemd 原生部署迁到 Compose(DEPLOY09)

1. 先在隔离副本上演练:`litebox backup` 做快照,在另一台机器(或另一个目录、另一个端口)用 Compose 起一个主控、
   把快照放进它的 `data/litebox.db`、主密钥放进 `secrets/master_key`,**断网或不配节点地址**确认能登录、数据都在;
2. 正式切换:`systemctl stop litebox && systemctl disable litebox`(防止两个主控同时写 —— 就算忘了,运行锁也会让第二个起不来);
3. `/var/lib/litebox/litebox.db` 拷进 Compose 目录的 `data/`(属主改成 10001),`/etc/litebox/litebox.env` 里的主密钥写进 `secrets/master_key`;
4. `docker compose up -d`;核对之后再考虑删除原服务定义。**节点上的服务一个都不动。**
   回退:停 Compose,`systemctl enable --now litebox`(在切换之后没有新写入的前提下;有的话先把 `data/litebox.db` 拷回去)。

## 6. 兼容矩阵(构建时固定)

| 组件 | 版本 |
|---|---|
| 构建用 Go / Node.js | golang:1.26.10-alpine / node:22.22.0-alpine |
| 运行时 | alpine:3.22.2 |
| 快捷 PG | postgres:17-alpine(17.11,digest 锁死在 `compose.postgres.yaml`) |
| 节点 sing-box | v1.14.0(`with_utls,with_v2ray_api,badlinkname,tfogo_checklinkname0`) |
| mita / mieru | v3.36.0(上游发布,校验和写死在 `scripts/fetch-mieru.sh`) |
| realm | v2.9.4(同上) |

每次发布的实际版本与哈希以发布包与镜像里的 `release-manifest.json` 为准(也可以在面板上 `GET /api/release-manifest`)。

## 7. 本地验证过的(Docker Desktop,2026-11-07)

* 镜像构建(本机单架构)276 MB,uid 10001,清单 10 个文件;
* SQLite 模式:安装 → 登录 → 重复安装不重启 → 备份并在断网容器里验证 → 连续备份只留 3 份 → `down` / `up` 之后数据还在 → 从备份恢复;
* 快捷 PG 模式:主控等 PG 健康后启动,5432 没有发布到宿主,应用账号 `litebox` 非超级用户、无 CREATEDB;pg_dump 备份可被 pg_restore 读出;
  同版本 `update` 什么都不做;
* 已有 PG 模式:主控只在给定的库里迁移、没有建任何库;共享实例上两个应用账号互相连不进对方的库;
* **没有验证**:多架构推送与 GitHub Release(需要 Docker Hub 凭据与一次真实的 tag)、arm64 镜像实际运行、
  跨版本 `update` 的真实回滚(目前只有一个版本)、在真实 Linux 宿主上跑脚本(本机是 Git Bash + Docker Desktop)。

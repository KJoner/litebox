# VNext 升级与迁移说明

本文随各批次更新。面向管理员的完整升级步骤在 `docs/升级说明.md`,这里记录 VNext 的迁移契约与回退边界。

## 1. SQLite 原地升级(默认路径)

* 旧库原地升级,迁移只追加;启动时自动执行,已执行的版本跳过(A01);
* **库的版本比程序新时拒绝启动**:回滚到旧二进制之前必须先恢复与它匹配的备份;
* 每一个新迁移都有升级测试(`internal/database/*_upgrade_test.go`),逐列、逐行、逐视图比对升级前后。

## 2. SQLite → PostgreSQL

设计见 `design.md` §1。**全部是离线操作**:工具会去取运行锁,主控开着时拒绝。源库只读,目标必须是刚建好的空库。

**准备目标**(DB06):管理员预先建好库与应用账号,应用账号不需要 CREATEDB,也不应是超级用户。共享实例上:

```sql
-- 用管理员账号执行一次;口令另行生成,不要复用别的应用的
CREATE ROLE litebox LOGIN PASSWORD '...';
CREATE DATABASE litebox OWNER litebox;
REVOKE ALL ON DATABASE litebox FROM PUBLIC;          -- 别的应用的账号连不进来
-- 另一个应用(例如 Nexus Portal)同理建自己的库与账号,两边互不授权
```

**配置**(`litebox.yaml` 或环境变量,环境变量优先):

```yaml
database:
  driver: postgres
  dsn_file: /run/secrets/litebox_dsn   # 文件里一行连接串;也可以用 dsn: 直接写(不推荐)
  max_open: 5                         # 默认 5
  connect_timeout: 10s
  statement_timeout: 60s
```

跨主机连接请用可验证的 TLS:`postgres://litebox:...@db.example.com:5432/litebox?sslmode=verify-full&sslrootcert=/etc/litebox/pg-ca.pem`。
同宿主不同 Compose 项目之间走显式的 external network 与服务名,**不要写 localhost**(那是容器自己)。

**步骤**:

1. 停主控:`systemctl stop litebox`(或 `docker compose stop`);
2. 取一致性快照:`litebox backup --config /etc/litebox/litebox.yaml --output /var/backups/litebox-pre-pg.db`;
3. 演练:`litebox db import --config pg.yaml --from-sqlite /var/backups/litebox-pre-pg.db` —— 输出源与目标的版本、表与行数,**不写任何东西**;
   源库的结构版本必须等于程序认识的最新版本(不是的话,先用这个版本的程序对快照执行一次 `litebox migrate --config <指向快照的配置>`);
4. 正式导入:同一条命令加 `--execute`。逐表复制(保留 id 与密文)→ 校正序列 → 逐表逐行比对 → 比对有效权限视图 → 用当前主密钥抽查解密;
   任何一处不一致都报出来并返回非零;
5. 改配置 `database.driver: postgres` 启动主控,核对登录、订阅、节点列表、流量;
6. 源 SQLite 快照与旧版本程序保留到验收完成。

单独再比一次:`litebox db verify --config pg.yaml --from-sqlite <快照>`。看一个库的概况:`litebox db inspect`(配置里那个库)或 `--from-sqlite <文件>`。

**回退**(DB11):

* 切换前失败:原 SQLite 没动过,用原配置启动即可;
* 切换后已有新写入:停主控 → `litebox db export --config pg.yaml --to-sqlite /var/lib/litebox/litebox-from-pg.db`
  (导出到一个新文件,同样逐行校验)→ 把配置指回这个新文件启动。**不能直接拿切换前的旧 SQLite 启动** —— 那会丢掉切换之后的全部写入;
* 首版不支持在线双向切换,导入导出都要停机。

**本地验证过的**:PG 17(Docker,postgres:17-alpine):全部业务测试在两种引擎上各跑一遍通过;
SQLite → PG → 新 SQLite 往返逐行一致(含超过 2^53 的流量值);用 SQLite 时代建的管理员口令登录 PG 上的主控;
主控开着时导入被拒;目标非空时导入被拒;同一个库起第二个主控被拒。**没有验证**:PG 16 / 18、跨主机 TLS、
真实生产库的迁移时长。

## 3. 主控换成 Docker 部署(S3 交付后可用)

见 `docs/vnext/deploy.md`(S3 写)。要点:先在隔离目录用数据副本试运行;切换时停掉旧的 systemd 服务防止两个主控同时写;
节点上的服务一个都不动。

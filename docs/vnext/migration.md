# VNext 升级与迁移说明

本文随各批次更新。面向管理员的完整升级步骤在 `docs/升级说明.md`,这里记录 VNext 的迁移契约与回退边界。

## 1. SQLite 原地升级(默认路径)

* 旧库原地升级,迁移只追加;启动时自动执行,已执行的版本跳过(A01);
* **库的版本比程序新时拒绝启动**:回滚到旧二进制之前必须先恢复与它匹配的备份;
* 每一个新迁移都有升级测试(`internal/database/*_upgrade_test.go`),逐列、逐行、逐视图比对升级前后。

## 2. SQLite → PostgreSQL(S2 交付后可用)

见 `design.md` §1。顺序:

1. 准备目标:管理员预先建好 `litebox` 库与应用账号(应用账号不需要 CREATEDB,也不应是超级用户);
2. `litebox db inspect --source <sqlite 路径>`:源版本、表数量、行数、预计空间;
3. 停主控(或进入维护,暂停调度与远程写入);
4. `litebox db backup` 取一致性快照(`VACUUM INTO`);
5. `litebox db import --source <快照> --dsn <目标>`:目标必须为空,建结构 → 逐表复制(保留 id 与密文)→ 校正序列;
6. `litebox db verify --source <快照> --dsn <目标>`:行数、逐行哈希、有效权限集合;
7. 改配置 `database.driver: postgres` 启动主控,核对登录、订阅、节点列表;
8. 源 SQLite 与旧版本二进制保留到验收完成。

**回退**(DB11):切换前失败 → 原 SQLite 没动过,直接用旧配置启动;切换后已有新写入 → 停主控,用 `litebox db export` 把 PG 导回一个新 SQLite
(同一工具反方向),核对后再切回;**不能直接拿切换前的旧 SQLite 启动**,那会丢掉切换之后的全部写入。

## 3. 主控换成 Docker 部署(S3 交付后可用)

见 `docs/vnext/deploy.md`(S3 写)。要点:先在隔离目录用数据副本试运行;切换时停掉旧的 systemd 服务防止两个主控同时写;
节点上的服务一个都不动。

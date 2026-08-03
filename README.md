# LiteBox Panel

面向个人使用的轻量级 sing-box 管理面板。

* 最多 10 个用户,支持多台 Linux VPS;
* 节点只需 sing-box + systemd,无 Docker、无数据库、无常驻 Agent;
* 节点内存低至 128MB 可用(Phase 0 已实测);
* V1 支持 VLESS + REALITY + XTLS Vision,含用户级流量统计、额度限制与到期停用。

## 当前状态

* Phase 0 技术验证 —— 已完成,见 [`docs/Phase0-技术验证报告.md`](docs/Phase0-技术验证报告.md)
* Phase 1 项目骨架 —— 已完成
* Phase 2 节点能力 —— 已完成
* Phase 3 用户与配置生成 —— 已完成
* Phase 4 流量与额度 —— 已完成
* Phase 5 订阅 —— 已完成
* Phase 6 管理页面 —— 已完成
* **Phase 7 部署与加固 —— 已完成**

**V1 全部验收标准通过**,见 [`docs/V1-验收报告.md`](docs/V1-验收报告.md)。
日常运维见 [`docs/运维手册.md`](docs/运维手册.md)。

Phase 1 交付:配置加载、主密钥与字段加密、SQLite 迁移框架与完整表结构、
管理员登录与会话、登录失败限流、审计日志、REST API、Vue 3 前端骨架(登录页 +
仪表盘 + 审计日志 + 设置)、systemd 与 Nginx 部署文件。

Phase 2 交付:SSH 连接池(按节点复用长连接、主机密钥 TOFU 固定)、参数化远程命令、
节点探测与构建标签断言、REALITY 密钥生成、从节点出口实测握手目标、
sing-box 配置渲染与强校验、节点二进制分发与 systemd 单元安装、
部署事务(同步→check→备份→原子替换→重启→三步健康检查→失败自动回滚)、部署记录。

Phase 3 交付:用户 CRUD 与不可复用的 user_code 分配、UUID 与订阅 Token 生成、
节点分配、按状态过滤的配置生成(停用/过期/超额用户自动从节点配置中移除)、
用户变更的合并部署协调器(连续变更只触发一次重启)、配置 diff。

Phase 4 交付:V2Ray Stats gRPC 客户端、经 SSH 通道的流量采集、
基线差值入账与幂等 ledger、三信号重启判定(启动时刻前移 / uptime 小于同步间隔 /
计数器回退兜底)、每日聚合、额度与到期检查、超额自动停用并重新部署、
月度流量重置、60 秒定时同步。部署事务的"重启前强制同步"至此真正生效。

Phase 5 交付:VLESS 分享链接、sing-box 客户端配置、base64 聚合格式、
公开订阅端点 `/sub/{token}`(免认证、不可缓存、按来源限流)、
只下发已成功部署过的节点、`Subscription-Userinfo` 头(客户端显示流量与到期)、
不可用用户返回可读原因而非空订阅、订阅访问记录。

订阅格式:

```
GET /sub/{token}                  # base64,v2rayN/Shadowrocket 等通用格式
GET /sub/{token}?format=uri       # 明文 VLESS URI,便于核对
GET /sub/{token}?format=sing-box  # 完整 sing-box 客户端配置
```

Phase 6 交付:仪表盘(用户/节点/今日与本月流量/告警卡片/最近失败部署/同步状态)、
用户管理(列表、增删改、启停、节点分配、额度与到期、重置流量、重生成凭据、
订阅地址、流量趋势、操作记录)、节点管理(列表、新增、测试 SSH、探测、安装、
握手目标扫描与应用、配置比对、部署、同步流量、重启、重置主机密钥、部署记录)、
部署记录页(可展开步骤明细)、审计日志(中文动作名、搜索、只看失败)。

流量趋势图是内联 SVG,不引图表库 —— 页面要嵌进 Go 二进制,一个库就是几百 KB。

**一台机器只能承载一个节点** —— 节点上的路径(`/opt/litebox`)与服务名
(`litebox-singbox`)是固定的,两个节点记录指向同一主机会互相覆盖配置。

**节点支持端口转发** —— 公网代理端口(写进订阅)与主机代理端口
(sing-box 实际监听)可以不同,用于 NAT 主机与 nginx stream 同机转发的场景,
例如公网 443 转发到主机 20443。主机端口留空即与公网端口一致,也就是直连形态。
转发规则由使用者自行配置,面板不介入。节点的名称、地址、SSH 参数与各端口
都可以在节点管理里编辑;涉及节点配置的改动会提示需要重新部署。

## 生产部署

### 一键安装

在主控机器上以 root 执行:

```bash
bash <(wget -qO- https://raw.githubusercontent.com/KJoner/litebox/master/scripts/litebox-install.sh)
```

自动补齐 make / Node.js 22+ / Go 1.26+,拉源码,构建主控与节点用的 sing-box,
安装 systemd 服务并启动,最后打印初始密码与面板 SSH 公钥。
**再跑一遍就是升级**,依赖不重装,数据库与主密钥不动,升级前自动备份。

装完记得:

```bash
# 1. 立刻备份主密钥 —— 丢失后全部用户与节点凭据不可恢复
sudo cat /etc/litebox/litebox.env

# 2. 开启每日自动备份
sudo cp deploy/systemd/litebox-backup.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now litebox-backup.timer
```

订阅域名在页面「设置 → 订阅地址」里填,不用改配置文件。

### 手工安装

完整说明见 **[`docs/部署与使用指南.md`](docs/部署与使用指南.md)** ——
含主控安装、Nginx 与 HTTPS、添加节点、添加用户、客户端使用与常见问题。

```bash
# 开发机上构建
make build-linux                  # 主控二进制(含前端)
bash scripts/build-singbox.sh     # 节点用的 sing-box

# 主控机器上(把 bin/ scripts/ assets/ deploy/ 保持目录结构拷过去)
sudo ./scripts/install.sh
```

升级用 `sudo ./scripts/upgrade.sh`(自动先备份、失败自动回退),
恢复用 `sudo ./scripts/restore.sh <备份文件>`。

### 节点接入

面板持有**一把自己的 SSH 密钥**(首次用到时生成,主密钥加密存库)。
新增节点时有三种方式把它的公钥装进节点:

* **节点密码** —— 填一次节点 root 口令,面板用完即弃,不落库、不写日志;
* **主控本机私钥** —— 用主控上已有的私钥去装,适合禁用了密码登录的节点;
* **手工指定私钥** —— 给这个节点单配一把。

之后所有操作都走面板专用密钥。公钥可在「设置」页复制,或在主控上打印:

```bash
sudo -u litebox env "$(grep '^LITEBOX_MASTER_KEY=' /etc/litebox/litebox.env)" \
  /opt/litebox/litebox ssh-key --config /etc/litebox/litebox.yaml
```

主密钥在 `/etc/litebox/litebox.env` 而不是配置文件里,手工执行子命令要自己带上。

## 本地开发

需要 Go 1.26+ 与 Node.js 22+。

```bash
# 1. 构建前端(产物会被 Go embed 嵌入二进制)
cd web && npm ci && npm run build && cd ..

# 2. 生成主密钥
go run ./cmd/litebox genkey

# 3. 配置
cp litebox.example.yaml litebox.yaml
export LITEBOX_MASTER_KEY="上一步生成的密钥"

# 4. 启动
go run ./cmd/litebox serve
```

首次启动会创建初始管理员账号并把随机密码打印到终端,登录后请立即修改。
面板默认监听 `127.0.0.1:8080`。

主密钥用于加密用户 UUID 与节点 REALITY 私钥,**丢失后这些数据无法还原**,
请务必备份。

## 开发

```bash
make run    # 启动后端(127.0.0.1:8080)
make dev    # 另开终端启动前端开发服务器(5173,API 代理到后端)
make test   # 运行 Go 测试
make lint   # go vet + gofmt
```

## 命令

```bash
litebox serve            # 启动服务(默认)
litebox migrate          # 只执行迁移并做一致性检查
litebox backup           # 生成 WAL 安全的数据库备份
litebox check            # 数据库完整性与外键自检
litebox genkey           # 生成主密钥
litebox ssh-key          # 打印面板专用的节点访问公钥(--rotate 轮换)
litebox reset-password   # 重置管理员密码(会使所有会话失效)
litebox version          # 打印版本
```

## 部署

```bash
make build-linux   # 交叉编译 linux/amd64 与 linux/arm64
```

部署文件见 `deploy/`:

* `deploy/systemd/litebox.service` —— 含安全加固参数,主密钥通过
  `EnvironmentFile` 提供(不要写进单元文件,`systemctl show` 会明文打印);
* `deploy/nginx/litebox.conf` —— TLS 终止与反代示例,含登录接口限速。

经 HTTPS 反代时必须设置 `http.secure_cookie: true`。

## 文档

面向使用:

* **[部署与使用指南](docs/部署与使用指南.md)** —— 从零到能用,含 Nginx、节点、用户、客户端
* [运维手册](docs/运维手册.md) —— 备份恢复、升级、故障处理、安全要点、检查清单

面向开发:

* [V1 验收报告](docs/V1-验收报告.md) —— 18 条验收标准的逐条核对与实测数据
* [Phase 0 技术验证报告](docs/Phase0-技术验证报告.md) —— 十项验证的实测数据,
  以及六个会导致线上静默故障的 sing-box 行为
* [V1 开发计划 v2](docs/LiteBox-Panel-V1-开发计划-v2.md) —— 经 Phase 0 修订的详细计划
* [CLAUDE.md](CLAUDE.md) —— 强制开发约束

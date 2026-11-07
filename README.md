# LiteBox

自用的 sing-box 管理面板。一条命令装好,在网页上加机器、加人、发订阅。

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)
[![Release](https://img.shields.io/github/v/release/KJoner/litebox?include_prereleases)](https://github.com/KJoner/litebox/releases)
[![Docker](https://img.shields.io/badge/docker-kjoner%2Flitebox-2496ED?logo=docker&logoColor=white)](https://hub.docker.com/r/kjoner/litebox)

面向**自己和身边几个人**:一台主控 + 几台 VPS,最多十来个用户。
不是商业机场面板 —— 没有注册、套餐、支付、工单,账号一律由管理员开。

```
你的浏览器 ──► LiteBox 主控 ──SSH──► VPS 上的 sing-box / mita / nginx / realm
                    │
                    └── 订阅链接 ──► 用户的客户端
```

节点上默认只跑上游软件(sing-box,以及用到时才装的 mita、nginx、realm、vnStat),
**不装 Docker、不装数据库,面板自己一行代码都不常驻**。配置经 SSH 下发,128MB 的小鸡也带得动。
可选的 Agent 只在你对某台机器显式启用、而且内存够用时才装。

## 能做什么

**节点与入口**
- 加机器只需填地址与一次 root 口令(口令只用于装面板公钥,不落库);公网 IP 会变的机器填域名(动态 DNS)
- 一台机器可以开多个入口,各自的协议、端口、访问等级与出口互不相干。自建协议:
  **VLESS + REALITY**、**Trojan + REALITY**、**Shadowsocks**(2022 与传统 AEAD)、**Mieru**、**Snell**
- REALITY 的握手目标从节点本机实测挑选
- 部署是一次事务:校验 → 备份 → 替换 → 重启 → 健康检查(含真实拨测),**不通过自动回滚**;
  配置没变时不重启
- 中转:nginx / realm 透传,或 sing-box 链式出口(可以链到另一台自建节点或外部代理),支持一步组链
- 一键 TCP 调优(按实测内存现算,可还原)、服务巡检与自动恢复、配置不落盘(只放在内存里)
- 资源与网卡流量(vnStat)趋势、节点流量额度预警、阿里云 CDT 用量与自动停机、商家到期与续费提醒

**用户**
- 流量统计到人;额度、到期时间、每月重置;计费口径可选(上行 / 下行 / 合计 / 取大,可设倍率)
- 超额或过期自动从节点配置里移除,续期后自动恢复;连续改多个用户只触发一次重启
- 三档访问等级决定谁能用哪些入口,也可以把单个入口直接授权给某个人、或设成「仅指定用户」

**订阅**
- 通用 base64、明文链接、sing-box 配置、Clash / mihomo 原生配置
- Surge、Loon、Quantumult X、Stash(**实验性**:没有在真实客户端上验证过,支持不了的线路会跳过并写明原因)
- **配置文件订阅**:把你自己调好的整份客户端配置放进面板,面板只替换里面的占位符
- 一台机器可以有多个订阅地址(IPv4 / IPv6),每个入口按地址各配端口与名称
- **外部代理**:机场订阅或朋友给的链接(Shadowsocks / VMess / VLESS / Trojan / Hysteria2 / TUIC)
  合并进用户的同一条订阅

**网络监测**
- 入口连通性(主控 → 节点)、出站网络质量(节点本机测公共目标:ICMP、TCP、HTTP(S))、自建节点互测
- 可选接入 Boce 付费拨测,看各省各运营商到入口的 TCPing / Ping(默认关闭,有每日 / 每月预算上限)

**推送与门户**
- Bark 与 Telegram 推送:服务停止、自动恢复、部署失败、额度告警、探测失败、到期提醒
- 用户门户:查流量与到期、复制订阅、看可用节点的状态、自助改密码。与管理后台是两套独立的登录

## 安装

**目前只有预发布版 `v23.0.0-rc.1`**,安装时要显式给出版本号(不写版本的话脚本只找正式版,会报找不到)。
全部发布在 [Releases](https://github.com/KJoner/litebox/releases)。

### 方式一:Docker(推荐)

镜像 [`kjoner/litebox`](https://hub.docker.com/r/kjoner/litebox),amd64 / arm64。需要 Docker 与 Compose v2。

```bash
wget -O litebox.sh https://raw.githubusercontent.com/KJoner/litebox/master/deploy/docker/litebox.sh
sudo bash litebox.sh install --version v23.0.0-rc.1 --base-url https://panel.example.com
```

`--base-url` 是订阅地址的站点根。默认装在 `/opt/litebox-docker`,只监听 `127.0.0.1:8080`
(`--port` 可改),前面要有一层 HTTPS 反代。存储引擎三选一,装好之后不能直接改:

| `--mode` | 说明 |
| --- | --- |
| `sqlite`(默认) | 只有主控一个容器 |
| `postgres` | 同时起一个 PostgreSQL 17 容器,口令自动生成 |
| `external-postgres` | 连你已有的 PostgreSQL:`--dsn-file` 给一行连接串的文件,库与账号要预先建好 |

日常运维:

```bash
sudo bash litebox.sh status                 # 版本、模式、健康状态
sudo bash litebox.sh logs | grep 初始密码    # 第一次启动时的管理员口令
sudo bash litebox.sh backup                 # 数据库 + 配置 + 主密钥,验证通过才删旧的(保留 3 份)
sudo bash litebox.sh update --version vX.Y.Z  # 先在数据副本上演练迁移,失败自动回滚
sudo bash litebox.sh restore 备份文件
```

重复执行 `install` 会沿用已有的配置与主密钥,不会重新生成、也不会覆盖数据。

### 方式二:原生安装(systemd)

主控机器上以 root 执行:

```bash
LITEBOX_VERSION=v23.0.0-rc.1 bash <(wget -qO- https://raw.githubusercontent.com/KJoner/litebox/master/scripts/litebox-install.sh)
```

脚本下载预编译发布包、核对 `SHA256SUMS` 之后安装成 systemd 服务,最后打印初始密码;
生产机上不装 Go / Node.js、不现场构建(要从源码构建就加 `--build-from-source`)。
**再跑一遍就是升级** —— 数据库与主密钥不动,升级前自动备份。

装完先做两件事:

```bash
# 1. 备份主密钥。丢了之后全部用户与节点的凭据都不可恢复
sudo cat /etc/litebox/litebox.env

# 2. 打开每日自动备份
sudo cp deploy/systemd/litebox-backup.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now litebox-backup.timer
```

面板默认只监听 `127.0.0.1:8080`,用 Nginx 反代并配上 HTTPS(示例在 `deploy/nginx/litebox.conf`)。
经 HTTPS 反代时把配置里的 `http.secure_cookie` 设成 `true`,否则浏览器不会保存登录状态。
反代的 `proxy_read_timeout` 不要低于 10 分钟(示例里已经设好),面板的长操作上限就是 10 分钟。

### PostgreSQL

默认用 SQLite。要换 PostgreSQL,在配置里写 `database.driver: postgres` 与 `database.dsn`
(或 `dsn_file`,环境变量 `LITEBOX_DB_DSN` / `LITEBOX_DB_DSN_FILE`)。PostgreSQL 只是第二种存储引擎,
**不代表支持多个主控** —— 同一个库只能有一个主控在跑,第二个会拒绝启动。

已有数据要搬过去,先停掉主控,离线迁移:

```bash
litebox db import --from-sqlite /var/lib/litebox/litebox.db            # 默认只演练
litebox db import --from-sqlite /var/lib/litebox/litebox.db --execute  # 目标必须是空库
litebox db verify --from-sqlite /var/lib/litebox/litebox.db            # 逐表逐行核对
```

## 上手三步

**1. 加一台节点**

「自建节点 → 添加节点 → 自建主机」,填地址、SSH 端口和一次 root 口令,保存时面板会装好自己的公钥并真连一次。
保存之后可以选「安装并部署」,也可以稍后在节点详情的「入口与转发」里分步做:

```
探测          看架构、内存、系统,确认这台机器能跑
安装          上传 sing-box 并写好服务定义(顺带打开 sshd 的 TCP 转发)
扫描握手目标   从节点本机实测候选域名,选一个应用(仅 REALITY 需要)
部署          把配置下发下去
```

只是想登记一条别人家的线路(没有 SSH 权限),选「已有外部节点」,会转到「外部代理」去登记。

**2. 加一个用户**

「用户管理 → 添加用户」,设流量额度和到期时间。保存后面板会自动把凭据下发到他能用的节点上。
要给他开门户账号的话,在详情里点「门户登录」。

**3. 把订阅给他**

用户详情里复制订阅地址,或者让他自己登录门户去拿。

## 客户端

| 客户端 | 怎么用 |
| --- | --- |
| sing-box | 订阅地址加 `?format=sing-box`,或用面板里配好的配置文件订阅 |
| Clash / mihomo | 订阅地址加 `?format=clash`;要带分流规则就用配置文件订阅 |
| Shadowrocket | 用「Clash 配置」那条地址(通用订阅里没有 Snell);配置文件另在「配置」里加 |
| v2rayN / NekoBox | 直接填订阅地址 |
| Surge / Loon / Quantumult X / Stash | `?format=surge` / `loon` / `quanx` / `stash`,实验性 |

```
GET /sub/{token}                  # 通用 base64
GET /sub/{token}?format=uri       # 明文链接,方便核对
GET /sub/{token}?format=sing-box  # sing-box 客户端配置
GET /sub/{token}?format=clash     # Clash / mihomo 原生配置
```

每种客户端能用哪些协议,在用户详情的「客户端兼容」里按他的实际线路列出来;
完整的能力矩阵在 `GET /api/capabilities`。

## 几点要知道的

- **一台机器只能承载一个节点。** 节点上的路径与服务名是固定的,两个节点指向同一台机器会互相覆盖配置。
- **主密钥丢了不可恢复。** 用户凭据与节点私钥都用它加密,务必备份
  (原生安装在 `/etc/litebox/litebox.env`,Docker 在 `/opt/litebox-docker/secrets/master_key`)。
- **库的版本比程序新时拒绝启动。** 回滚到旧版本要恢复与它匹配的备份。
- **外部代理统计不到流量。** 那些流量走的是上游的服务器,面板看不到,也不计进用户额度。
- **节点额度只预警不停服。** 唯一会自动停机的是你显式选了「超阈值停机」的阿里云 CDT 实例。
- 支持公网端口与监听端口不同(NAT 小鸡、端口转发),转发规则自己配,面板不介入。

## 命令

```bash
litebox serve            # 启动服务(默认)
litebox migrate          # 只执行迁移并做一致性检查
litebox backup           # SQLite 的一致性备份(PostgreSQL 用 pg_dump)
litebox check            # 数据库完整性、外键与密文抽查
litebox db <子命令>       # inspect / import / verify / export,SQLite 与 PostgreSQL 之间离线迁移
litebox genkey           # 生成主密钥
litebox ssh-key          # 打印面板专用的节点访问公钥(--rotate 轮换)
litebox reset-password   # 重置管理员密码(会使所有会话失效)
litebox version          # 打印版本
```

## 从源码构建

需要 Go 1.26.10+(以 `go.mod` 为准)与 Node.js 22+。

```bash
cd web && npm ci && npm run build && cd ..   # 前端产物会被嵌进二进制
make build                                   # 单个可执行文件
make build-linux                             # 交叉编译 linux amd64/arm64
bash scripts/build-singbox.sh                # 节点用的 sing-box(带 with_v2ray_api)
bash scripts/build-probe.sh                  # 出站探测辅助程序 litebox-probe
bash scripts/build-agent.sh                  # 可选的节点 Agent
bash scripts/fetch-mieru.sh                  # mita / mieru(版本与 sha256 写死在脚本里)
bash scripts/fetch-realm.sh                  # realm
```

本地跑一份:

```bash
go run ./cmd/litebox genkey                  # 生成主密钥
cp litebox.example.yaml litebox.yaml
export LITEBOX_MASTER_KEY="上一步的密钥"
go run ./cmd/litebox serve
```

首次启动会创建管理员账号并把随机密码打印到终端。配置项的说明都在 `litebox.example.yaml` 里;
设计上的取舍与每一条「不这么做会静默出错」的约束写在 [CLAUDE.md](CLAUDE.md)。

## 协议

[MIT](LICENSE)

节点上运行的 [sing-box](https://github.com/SagerNet/sing-box)、[mieru](https://github.com/enfein/mieru)、
[realm](https://github.com/zhboner/realm) 都是独立的项目,按各自的协议分发。

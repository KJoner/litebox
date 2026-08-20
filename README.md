# LiteBox

自用的 sing-box 管理面板。一条命令装好,在网页上加机器、加人、发订阅。

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)

面向**自己和身边几个人**:一台主控 + 几台 VPS,最多十来个用户。
不是商业机场面板 —— 没有注册、套餐、支付、工单,账号一律由管理员开。

```
你的浏览器 ──► LiteBox 主控 ──SSH──► VPS 上的 sing-box
                    │
                    └── 订阅链接 ──► 用户的客户端
```

节点上只有 **sing-box 一个进程**:不装 Docker、不装数据库、没有常驻探针。
面板通过 SSH 下发配置,128MB 的小鸡也带得动。

## 能做什么

**节点**
- 一台机器一个节点,加机器只需填 IP 与 root 口令(口令用完即弃,不落库)
- 公网 IP 会变的机器填域名(动态 DNS):面板每次操作前重新解析,
  订阅里下发域名本身 —— IP 变了用户不用重新拉订阅
- 落地协议按**入口**选:**VLESS + REALITY + XTLS Vision** 或 **Shadowsocks 2022**
- 一台机器可以开多个入口:各占一个端口,协议、访问等级与出口去向互不相干
- REALITY 的握手目标从节点本机实测挑选,不是随便填一个域名
- 部署是一次事务:校验 → 备份 → 原子替换 → 重启 → 三步健康检查,**不通过自动回滚**
- 一键 TCP 调优:按这台机器的实际内存现算内核参数,可一键还原
- CPU / 内存 / 磁盘 / 网速趋势,节点流量额度与周期预警

**用户**
- 流量统计到人,额度、到期时间、每月重置
- 超额或过期自动从节点配置里移除,续期后自动恢复
- 三档访问等级(普通 / VIP / ROOT)决定谁能用哪些节点
- 连续改多个用户只会触发一次节点重启

**订阅**
- 通用 base64、明文链接、sing-box 客户端配置
- **配置文件订阅**:把你自己调好的整份客户端配置(含分流规则)放进面板,
  用户按客户端类型各拉各的那一份,面板只替换里面的占位符
- 可选 IPv6:同一台机器在订阅里多出一条 `xxx-IPV6` 条目
- **外部代理**:机场订阅或朋友给的链接也能登记进来,合并进用户的同一条订阅

**用户门户**
- 面板首页就是给用户的:查流量、查到期、复制订阅、自助改密码、下线登录设备
- 与管理后台是两套完全独立的登录,界面上不出现 UUID、IP、内存占用这些东西

## 安装

主控机器上以 root 执行:

```bash
bash <(wget -qO- https://raw.githubusercontent.com/KJoner/litebox/master/scripts/litebox-install.sh)
```

脚本会补齐依赖、拉源码、构建、装成 systemd 服务并启动,最后打印初始密码。
**再跑一遍就是升级** —— 数据库与主密钥不动,升级前自动备份。

装完先做两件事:

```bash
# 1. 备份主密钥。丢了之后全部用户与节点的凭据都不可恢复
sudo cat /etc/litebox/litebox.env

# 2. 打开每日自动备份
sudo cp deploy/systemd/litebox-backup.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now litebox-backup.timer
```

面板默认只监听 `127.0.0.1:8080`,用 Nginx 反代并配上 HTTPS
(示例在 `deploy/nginx/litebox.conf`)。经 HTTPS 反代时把配置里的
`http.secure_cookie` 设成 `true`,否则浏览器不会保存登录状态。

订阅用的域名在页面「设置 → 订阅地址」里填,不用改配置文件。

## 上手三步

**1. 加一台节点**

「自建节点 → 添加节点」,填 IP、SSH 端口和一次 root 口令。之后依次点:

```
探测        看架构、内存、系统,确认这台机器能跑
安装        上传 sing-box 并写好服务定义(顺带打开 sshd 的 TCP 转发)
扫描握手目标  从节点本机实测候选域名,选一个应用(仅 REALITY 需要)
部署        把配置下发下去
```

面板有一把自己的 SSH 密钥,第一次用到时生成。之后所有操作都用它,
你的 root 口令只在装公钥那一次用到,不会被保存。

**2. 加一个用户**

「用户管理 → 添加用户」,设流量额度和到期时间。保存后面板会自动把凭据
下发到他能用的节点上。要给他开门户账号的话,在详情里点「门户登录」。

**3. 把订阅给他**

用户详情里复制订阅地址,或者让他自己登录门户去拿。

## 客户端

| 客户端 | 怎么用 |
| --- | --- |
| sing-box | 订阅地址加 `?format=sing-box`,或用面板里配好的配置文件订阅 |
| v2rayN / NekoBox | 直接填订阅地址 |
| Shadowrocket | 订阅地址直接加;配置文件另在「配置」里加 |
| Clash / mihomo | 用配置文件订阅(模板里的 `proxy-providers` 会自动指向该用户的订阅) |

```
GET /sub/{token}                  # 通用 base64
GET /sub/{token}?format=uri       # 明文链接,方便核对
GET /sub/{token}?format=sing-box  # sing-box 客户端配置
```

## 几点要知道的

- **一台机器只能承载一个节点。** 节点上的路径与服务名是固定的,
  两个节点指向同一台机器会互相覆盖配置。
- **主密钥丢了不可恢复。** 用户 UUID 与节点私钥都用它加密,务必备份
  `/etc/litebox/litebox.env`。
- **外部代理统计不到流量。** 那些流量走的是上游机场的服务器,面板看不到,
  也不计进用户额度。
- **节点额度只预警不停服。** 同步有间隔、各家 VPS 的计量口径也不同,
  自动关掉一个共享节点会同时打断上面全部用户。
- 支持公网端口与主机监听端口不同(NAT 小鸡、nginx stream 转发),
  转发规则自己配,面板不介入。

## 文档

- **[部署与使用指南](docs/部署与使用指南.md)** —— 从零到能用,含 Nginx、HTTPS、节点、用户、客户端与常见问题
- [运维手册](docs/运维手册.md) —— 备份恢复、升级、故障处理、安全要点
- [升级说明](docs/升级说明.md) —— 各版本升级会发生什么、要做什么、怎么回滚
- [API 说明](docs/API说明.md) / [数据库说明](docs/数据库说明.md) —— 想二次开发时看

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

## 从源码构建

需要 Go 1.26+ 与 Node.js 22+。

```bash
cd web && npm ci && npm run build && cd ..   # 前端产物会被嵌进二进制
make build                                   # 单个可执行文件
make build-linux                             # 交叉编译 linux amd64/arm64
bash scripts/build-singbox.sh                # 节点用的 sing-box(带 with_v2ray_api)
```

本地跑一份:

```bash
go run ./cmd/litebox genkey                  # 生成主密钥
cp litebox.example.yaml litebox.yaml
export LITEBOX_MASTER_KEY="上一步的密钥"
go run ./cmd/litebox serve
```

首次启动会创建管理员账号并把随机密码打印到终端。

## 协议

[MIT](LICENSE)

节点上运行的 [sing-box](https://github.com/SagerNet/sing-box) 是独立的项目,
按其自身协议分发。

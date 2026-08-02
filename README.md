# LiteBox Panel

面向个人使用的轻量级 sing-box 管理面板。

* 最多 10 个用户,支持多台 Linux VPS;
* 节点只需 sing-box + systemd,无 Docker、无数据库、无常驻 Agent;
* 节点内存低至 128MB 可用(Phase 0 已实测);
* V1 支持 VLESS + REALITY + XTLS Vision,含用户级流量统计、额度限制与到期停用。

## 当前状态

* Phase 0 技术验证 —— 已完成,见 [`docs/Phase0-技术验证报告.md`](docs/Phase0-技术验证报告.md)
* **Phase 1 项目骨架 —— 已完成**
* Phase 2 及之后 —— 未开始,见 [`docs/LiteBox-Panel-V1-开发计划-v2.md`](docs/LiteBox-Panel-V1-开发计划-v2.md)

Phase 1 交付:配置加载、主密钥与字段加密、SQLite 迁移框架与完整表结构、
管理员登录与会话、登录失败限流、审计日志、REST API、Vue 3 前端骨架(登录页 +
仪表盘 + 审计日志 + 设置)、systemd 与 Nginx 部署文件。

## 快速开始

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
litebox genkey           # 生成主密钥
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

* [Phase 0 技术验证报告](docs/Phase0-技术验证报告.md) —— 十项验证的实测数据,
  以及六个会导致线上静默故障的 sing-box 行为
* [V1 开发计划 v2](docs/LiteBox-Panel-V1-开发计划-v2.md) —— 经 Phase 0 修订的详细计划
* [CLAUDE.md](CLAUDE.md) —— 强制开发约束

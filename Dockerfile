# syntax=docker/dockerfile:1.7
#
# LiteBox 主控镜像(VNext DEPLOY01)。镜像里是:主控二进制(内嵌前端)+ 要分发到节点的全部产物
# (精简构建的 sing-box、litebox-probe、mita / mieru、realm)与发布清单。容器里没有 Go / Node / git,
# 启动时不现场编译任何东西。多架构:产物在构建平台上交叉编译一次(与目标架构无关的阶段只跑一遍)。
#
# 基础镜像版本写死;换版本在这里改并在 docs/vnext/deploy.md 的兼容矩阵里记一笔。
ARG GO_IMAGE=golang:1.26.10-alpine
ARG NODE_IMAGE=node:22.22.0-alpine
ARG RUNTIME_IMAGE=alpine:3.22.2
# 模块代理与 npm 源可以换成本地镜像源(网络受限的构建机);CI 用默认值。
ARG GOPROXY=https://proxy.golang.org,direct
ARG NPM_REGISTRY=

# ---------- 前端 ----------
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS web
ARG NPM_REGISTRY
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi  && npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# ---------- 要分发到节点的产物(两种架构都要,与镜像自己的架构无关)----------
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS assets
ARG GOPROXY
ENV GOPROXY=${GOPROXY}
# Agent 把版本编进二进制(litebox-agent -version、上报里的 version),要与发布清单里写的同一个 ——
# 不传的话 build-agent.sh 编成 dev,清单却写着发布版本,包里两个说法对不上(复核 6.1)。
ARG VERSION=dev
RUN apk add --no-cache bash git curl coreutils tar
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# sing-box 用固定版本与固定构建标签(with_v2ray_api 是整套流量统计的前提,见 CLAUDE.md);
# mita / mieru / realm 是上游发布的原样二进制,脚本里写死了版本与校验和。
RUN bash scripts/build-probe.sh \
 && VERSION="${VERSION}" bash scripts/build-agent.sh \
 && bash scripts/build-singbox.sh \
 && bash scripts/fetch-mieru.sh \
 && bash scripts/fetch-realm.sh

# ---------- 主控 ----------
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG GOPROXY
ENV GOPROXY=${GOPROXY}
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
COPY --from=assets /src/assets ./assets
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X main.Version=${VERSION}" -o /out/litebox ./cmd/litebox
# 发布清单:每个可分发文件的组件、架构、版本、能力、大小与 sha256。主控分发前逐个核对。
RUN SB=$(sed -n 's/^SINGBOX_VERSION="\${SINGBOX_VERSION:-\(.*\)}"$/\1/p' scripts/build-singbox.sh) \
 && MI=$(sed -n 's/^MIERU_VERSION="\${MIERU_VERSION:-\(.*\)}"$/\1/p' scripts/fetch-mieru.sh) \
 && RE=$(sed -n 's/^REALM_VERSION="\${REALM_VERSION:-\(.*\)}"$/\1/p' scripts/fetch-realm.sh) \
 && go run ./cmd/litebox manifest --dir assets --version "${VERSION}" --commit "${COMMIT}" \
      --singbox "$SB" --mieru "$MI" --realm "$RE"

# ---------- 运行 ----------
FROM ${RUNTIME_IMAGE}
ARG VERSION=dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="LiteBox" \
      org.opencontainers.image.source="https://github.com/KJoner/litebox" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.licenses="MIT"
# ca-certificates:拨测终点、机场订阅、Boce、阿里云都是 HTTPS。时区库已编进二进制。
RUN apk add --no-cache ca-certificates \
 && addgroup -g 10001 -S litebox \
 && adduser -u 10001 -S -G litebox -h /data -s /sbin/nologin litebox \
 && mkdir -p /data /opt/litebox \
 && chown litebox:litebox /data
COPY --from=build /out/litebox /usr/local/bin/litebox
COPY --from=build /src/assets /opt/litebox/assets
# 与这个镜像同版本的 Compose 文件:部署脚本升级时从新镜像里取出来覆盖旧的,不去网上另找一份。
COPY deploy/docker /opt/litebox/deploy
# 数据(库、备份、节点访问私钥)都在 /data;主密钥用 LITEBOX_MASTER_KEY 或 LITEBOX_MASTER_KEY_FILE 给。
ENV LITEBOX_HTTP_LISTEN=0.0.0.0:8080 \
    LITEBOX_DB_SQLITE_PATH=/data/litebox.db \
    LITEBOX_NODE_BINARY_DIR=/opt/litebox/assets/singbox \
    LITEBOX_NODE_MIERU_BINARY_DIR=/opt/litebox/assets/mieru \
    LITEBOX_NODE_REALM_BINARY_DIR=/opt/litebox/assets/realm \
    LITEBOX_NODE_PROBE_BINARY_DIR=/opt/litebox/assets/probe \
    LITEBOX_NODE_AGENT_BINARY_DIR=/opt/litebox/assets/agent
USER 10001:10001
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/api/health >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/litebox"]
CMD ["serve", "--config", "/data/litebox.yaml"]

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X 'main.Version=$(VERSION)'
GOFLAGS = -trimpath

.PHONY: all build build-linux web test lint clean run dev tidy

all: web build

## build: 构建当前平台的二进制(需先执行 make web)
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/litebox ./cmd/litebox

## build-linux: 交叉编译 linux/amd64 与 linux/arm64
build-linux: web
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/litebox-linux-amd64 ./cmd/litebox
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/litebox-linux-arm64 ./cmd/litebox

## web: 构建前端并输出到 web/dist(由 Go embed 嵌入)
web:
	cd web && npm ci && npm run build

## test: 运行全部 Go 测试
test:
	go test ./... -count=1

## test-race: 带竞态检测运行测试
test-race:
	go test ./... -race -count=1

## lint: 静态检查
lint:
	go vet ./...
	gofmt -l -d .

## tidy: 整理依赖
tidy:
	go mod tidy

## run: 本地启动后端(前端用 make dev 另开)
run:
	go run ./cmd/litebox serve --config litebox.yaml

## dev: 启动前端开发服务器,API 代理到本地后端
dev:
	cd web && npm run dev

clean:
	rm -rf bin web/dist

# Nexus Agent Hub — build & test entrypoints

BINARY_NEXUS := bin/nexus
BINARY_ECHOD := bin/echod
FE_DIR := frontend
FE_PM ?= npm   # 可换 bun：make build FE_PM=bun

.PHONY: all build echod fe-install fe-dev fe-build fe-test test vet fmt check clean

all: build

# 单二进制：前端产物先构建（web/dist），再 go build（go:embed all:dist）。
# Go/embed 源变更无法用文件时间戳表达，始终重编（秒级开销）。
build: fe-build
	go build -o $(BINARY_NEXUS) ./cmd/nexus
	go build -o $(BINARY_ECHOD) ./cmd/echod

# ---------- 前端（Vue 3 + Vite，frontend/）----------

fe-install:
	$(FE_PM) --prefix $(FE_DIR) install

# 开发期前后端分离：vite dev server（HMR）+ /api 反代到本地 nexus（NEXUS_BACKEND 可改端口）
fe-dev:
	$(FE_PM) --prefix $(FE_DIR) run dev

fe-build: fe-install
	$(FE_PM) --prefix $(FE_DIR) run build
	@touch web/dist/.gitkeep  # emptyOutDir 会清掉占位文件，补回以保 fresh clone 的 go:embed 可编译

fe-test:
	$(FE_PM) --prefix $(FE_DIR) run test

test:
	go test ./...

test-e2e:
	go test -tags=e2e ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# CI gate: gofmt -l must be empty
check: fmt vet test
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed on: $$out"; exit 1; fi

clean:
	rm -rf bin/ data/

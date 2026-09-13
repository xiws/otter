.PHONY: build clean run test lint vet

BINARY=otter
BUILD_DIR=build

build: vet
	@echo "🔨 构建 $(BINARY)..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/cli/
	@echo "✅ 构建完成: $(BUILD_DIR)/$(BINARY)"

clean:
	@echo "🧹 清理..."
	@rm -rf $(BUILD_DIR)
	@echo "✅ 清理完成"

run:
	go run ./cmd/cli/ $(ARGS)

test:
	@echo "🧪 运行测试..."
	go test ./... -v -count=1

lint:
	@echo "🔍 代码检查..."
	@which golangci-lint > /dev/null 2>&1 || (echo "⚠️  golangci-lint 未安装，跳过 lint" && exit 0)
	golangci-lint run ./...

vet:
	@echo "🔍 go vet..."
	go vet ./...

fmt:
	@echo "🎨 格式化代码..."
	go fmt ./...

install: build
	@echo "📦 安装 $(BINARY) 到 GOPATH/bin..."
	@cp $(BUILD_DIR)/$(BINARY) $(GOPATH)/bin/$(BINARY)
	@echo "✅ 安装完成: $(GOPATH)/bin/$(BINARY)"

.DEFAULT_GOAL := build
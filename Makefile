# mars-ai-agent 的独立构建入口（从 mars 主仓拆分出来的沙箱侧 AI 助手）。
BIN_DIR ?= bin

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

# agent 构建 mars-agent wrapper 二进制。
.PHONY: agent
agent: | $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/mars-agent .

# agent_test 跑本模块全部测试。
.PHONY: agent_test
agent_test:
	go test ./... -race -count=1

# 钉版 opencode（镜像的 agent 内核）。版本与 sha256 均取自 GitHub release API 的权威 digest，
# 一一对应；只钉版本不校验和等于没钉。升级版本时两个 digest 必须同步更新。
OPENCODE_VERSION      ?= 1.19.2
OPENCODE_REPO         ?= anomalyco/opencode
OPENCODE_BASE_URL     := https://github.com/$(OPENCODE_REPO)/releases/download/v$(OPENCODE_VERSION)
OPENCODE_SHA256_amd64 := 4eff8d2cf8614ead97eadf1123d46033dc975eadeac3df04644a64bae0f9e819
OPENCODE_SHA256_arm64 := 3f73a474065b4b7c4a64cef2395a6123ae1039c2e41f0acbe29ae6521b50df66
AGENT_VENDOR_DIR      := vendor/opencode

# agent_opencode 预置钉版 opencode 二进制（默认 linux glibc x64/arm64）到构建上下文，逐个校验 sha256。
# ARCHS 可裁剪架构：本地只想构建单架构镜像时用 `make agent_opencode ARCHS=amd64` 省一半下载。
ARCHS ?= amd64 arm64

.PHONY: agent_opencode
agent_opencode:
	@mkdir -p $(AGENT_VENDOR_DIR)
	@set -e; \
	for arch in $(ARCHS); do \
	  if [ "$$arch" = "amd64" ]; then slug=x64; sha=$(OPENCODE_SHA256_amd64); else slug=arm64; sha=$(OPENCODE_SHA256_arm64); fi; \
	  url=$(OPENCODE_BASE_URL)/opencode-linux-$$slug.tar.gz; \
	  echo ">> 下载 opencode v$(OPENCODE_VERSION) $$arch: $$url"; \
	  curl -fsSL --http1.1 --retry 3 --retry-delay 2 "$$url" -o $(AGENT_VENDOR_DIR)/oc-$$arch.tar.gz; \
	  if command -v sha256sum >/dev/null 2>&1; then echo "$$sha  $(AGENT_VENDOR_DIR)/oc-$$arch.tar.gz" | sha256sum -c -; \
	  else echo "$$sha  $(AGENT_VENDOR_DIR)/oc-$$arch.tar.gz" | shasum -a 256 -c -; fi; \
	  tar -xzf $(AGENT_VENDOR_DIR)/oc-$$arch.tar.gz -C $(AGENT_VENDOR_DIR); \
	  mv $(AGENT_VENDOR_DIR)/opencode $(AGENT_VENDOR_DIR)/opencode-linux-$$arch; \
	  command rm -f $(AGENT_VENDOR_DIR)/oc-$$arch.tar.gz; \
	done
	@echo "完成：$(AGENT_VENDOR_DIR)/opencode-linux-{amd64,arm64}"

# apt 源（镜像构建期装工具用）。默认走上游；受限网络换国内镜像，例如：
#   make agent_image APT_MIRROR=mirrors.ustc.edu.cn
APT_MIRROR ?= deb.debian.org

# agent_image 构建沙箱镜像；需先 make agent_opencode 预置二进制（见 README.md）。
.PHONY: agent_image
agent_image:
	docker build -t mars-agent:dev --build-arg APT_MIRROR=$(APT_MIRROR) .

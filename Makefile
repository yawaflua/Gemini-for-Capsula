# Copyright 2026 The Marusya-Gemini Authors.
# SPDX-License-Identifier: Apache-2.0

BIN        := bin
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X main.version=$(VERSION)
AGENT_ARCH ?= arm64
DEVICE     ?= root@192.168.31.190
# Writable directory on the speaker. The stock rootfs is a full read-only
# ubifs, so the default is tmpfs (wiped on reboot).
DEVICE_DIR ?= /tmp/marusya
# Server endpoint as seen from the speaker, e.g. ws://192.168.31.10:8080/v1/stream
SERVER     ?= ws://192.168.31.102:8080/v1/stream
TOKEN      ?= secret

.PHONY: all agent server test vet fmt clean deploy-agent push-agent try-agent docker

all: agent server

## agent: static linux/$(AGENT_ARCH) binary for the speaker (no cgo).
agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(AGENT_ARCH) go build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(BIN)/marusya-agent-linux-$(AGENT_ARCH) ./cmd/marusya-agent

## server: binary for the host platform.
server:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/marusya-server ./cmd/marusya-server

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

## deploy-agent: install the OpenRC service on the speaker (~2 KB of flash).
## The service downloads the agent from marusya-server into tmpfs on start,
## so run the server with -agent_dir bin. One ssh session, one password.
##   make deploy-agent DEVICE=root@192.168.31.190 SERVER=ws://<pc-ip>:8080/v1/stream TOKEN=secret
## SERVER/TOKEN update /etc/conf.d/marusya-agent; other settings there are kept.
deploy-agent:
	@test -n "$(SERVER)" || { echo "set SERVER=ws://<server-ip>:8080/v1/stream"; exit 1; }
	{ echo '__INIT__'; cat deploy/agent/marusya-agent.openrc; echo '__CONF__'; cat configs/agent.conf.d.example; } | \
	ssh $(DEVICE) 'set -e; d=$$(mktemp -d); \
		awk -v d="$$d" "/^__INIT__\$$/{f=d\"/init\";next} /^__CONF__\$$/{f=d\"/conf\";next} {print > f}" ; \
		cat "$$d/init" > /etc/init.d/marusya-agent; chmod 0755 /etc/init.d/marusya-agent; \
		[ -f /etc/conf.d/marusya-agent ] || cat "$$d/conf" > /etc/conf.d/marusya-agent; \
		sed -i -e "s#^MARUSYA_SERVER=.*#MARUSYA_SERVER=$(SERVER)#" \
			-e "s#^MARUSYA_AUTH_TOKEN=.*#MARUSYA_AUTH_TOKEN=$(TOKEN)#" /etc/conf.d/marusya-agent; \
		rm -rf "$$d"; sync; \
		rc-update add marusya-agent default >/dev/null; rc-service marusya-agent restart'

## push-agent: upload the agent into DEVICE_DIR (works on a read-only rootfs).
## Uses cat over ssh, so no sftp-server is needed on the speaker.
push-agent: agent
	ssh $(DEVICE) 'mkdir -p $(DEVICE_DIR) && cat > $(DEVICE_DIR)/marusya-agent.new && \
		chmod +x $(DEVICE_DIR)/marusya-agent.new && mv $(DEVICE_DIR)/marusya-agent.new $(DEVICE_DIR)/marusya-agent' \
		< $(BIN)/marusya-agent-linux-$(AGENT_ARCH)

## try-agent: upload and run the agent in the foreground (Ctrl-C to stop).
##   make try-agent DEVICE=root@192.168.31.190 SERVER=ws://<server-ip>:8080/v1/stream TOKEN=secret
try-agent: push-agent
	@test -n "$(SERVER)" || { echo "set SERVER=ws://<server-ip>:8080/v1/stream"; exit 1; }
	ssh -t $(DEVICE) '$(DEVICE_DIR)/marusya-agent -server $(SERVER) -token "$(TOKEN)" -log_level debug'

docker:
	docker build --build-arg VERSION=$(VERSION) -f deploy/server/Dockerfile -t marusya-server:$(VERSION) -t marusya-server:latest .

clean:
	rm -rf $(BIN)

BINARY   := weight-tracker
DB       := weight-tracker.db
ADDR     := :8080
BIN_DIR  := bin
PID_FILE := $(BIN_DIR)/$(BINARY).pid
LOG_FILE := $(BIN_DIR)/$(BINARY).log

# What 'make start' polls to decide the daemon is actually serving. Override
# alongside ADDR if you bind to something localhost can't reach.
HEALTH_URL ?= http://localhost$(ADDR)/

PI_HOST ?= jcwpi
PI_USER ?= jcw
# set PI_ARCH=armv6 for 32-bit Raspberry Pi OS
PI_ARCH ?= arm64

.PHONY: run build build-pi start stop restart status logs clean fmt vet tidy test check deploy help

help:
	@echo "make run         - go run the app locally on $(ADDR), attached to this terminal"
	@echo "make start       - build, then run detached in the background (daemon)"
	@echo "make stop        - stop the background daemon started by 'make start'"
	@echo "make restart     - stop then start the daemon"
	@echo "make status      - check whether the daemon is running"
	@echo "make logs        - tail the daemon's log file"
	@echo "make build       - build a local binary into $(BIN_DIR)/"
	@echo "make build-pi    - cross-compile for Raspberry Pi (PI_ARCH=$(PI_ARCH))"
	@echo "make deploy      - build-pi, then scp + restart the systemd service on PI_HOST=$(PI_HOST)"
	@echo "make clean       - stop the daemon, then remove build output and the local dev database"
	@echo "make test        - go test ./... with the race detector"
	@echo "make check       - everything CI runs: gofmt check, vet, tests"
	@echo "make fmt         - gofmt all source files"
	@echo "make vet         - go vet ./..."
	@echo "make tidy        - go mod tidy"

run:
	go run . -addr $(ADDR) -db $(DB)

build:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY) .

start: build
	@if [ -f $(PID_FILE) ] && kill -0 `cat $(PID_FILE)` 2>/dev/null; then \
		echo "already running (pid `cat $(PID_FILE)`)"; \
	else \
		nohup $(BIN_DIR)/$(BINARY) -addr $(ADDR) -db $(DB) > $(LOG_FILE) 2>&1 & \
		pid=$$!; \
		echo $$pid > $(PID_FILE); \
		i=0; \
		while [ $$i -lt 100 ]; do \
			if ! kill -0 $$pid 2>/dev/null; then \
				echo "failed to start, see $(LOG_FILE):"; \
				tail -n 20 $(LOG_FILE); \
				rm -f $(PID_FILE); \
				exit 1; \
			fi; \
			if ! command -v curl > /dev/null 2>&1; then sleep 1; break; fi; \
			if curl -sf -o /dev/null $(HEALTH_URL) 2>/dev/null; then break; fi; \
			i=`expr $$i + 1`; \
			sleep 0.1; \
		done; \
		echo "started (pid $$pid) on $(ADDR), logs at $(LOG_FILE)"; \
	fi

stop:
	@if [ -f $(PID_FILE) ] && kill -0 `cat $(PID_FILE)` 2>/dev/null; then \
		kill `cat $(PID_FILE)`; \
		rm -f $(PID_FILE); \
		echo "stopped"; \
	else \
		echo "not running"; \
		rm -f $(PID_FILE); \
	fi

restart: stop start

status:
	@if [ -f $(PID_FILE) ] && kill -0 `cat $(PID_FILE)` 2>/dev/null; then \
		echo "running (pid `cat $(PID_FILE)`)"; \
	else \
		echo "not running"; \
	fi

logs:
	tail -f $(LOG_FILE)

build-pi:
	mkdir -p $(BIN_DIR)
ifeq ($(PI_ARCH),arm64)
	GOOS=linux GOARCH=arm64 go build -o $(BIN_DIR)/$(BINARY)-arm64 .
else
	GOOS=linux GOARCH=arm GOARM=6 go build -o $(BIN_DIR)/$(BINARY)-armv6 .
endif

# The transfer goes over a plain `ssh ... 'cat > ...'` rather than scp:
# modern scp defaults to the SFTP subsystem, which a restricted deploy key's
# authorized_keys forced-command can't cleanly allowlist (unlike a plain
# exec command, which SSH_ORIGINAL_COMMAND captures verbatim). This changes
# nothing for a human's own unrestricted key — it's just a different way to
# move the same bytes — but it's what lets the CI-only deploy key in the
# README's "Continuous deployment" section be restricted to exactly this
# command and the one below, rather than needing full shell access.
deploy: build-pi
	ssh $(PI_USER)@$(PI_HOST) 'cat > ~/$(BINARY)-new' < $(BIN_DIR)/$(BINARY)-$(PI_ARCH)
	ssh $(PI_USER)@$(PI_HOST) '\
		sudo systemctl stop $(BINARY) 2>/dev/null; \
		mv ~/$(BINARY)-new ~/$(BINARY)-$(PI_ARCH); \
		chmod +x ~/$(BINARY)-$(PI_ARCH); \
		sudo systemctl start $(BINARY)'

clean: stop
	rm -rf $(BIN_DIR)
	rm -f $(DB) $(DB)-journal $(DB)-wal $(DB)-shm

test:
	go test -race -cover ./...

# Mirrors the CI workflow, so a green 'make check' locally means a green CI.
check:
	@unformatted=`gofmt -l .`; \
	if [ -n "$$unformatted" ]; then \
		echo "these files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...
	go test -race -cover ./...

fmt:
	gofmt -l -w .

vet:
	go vet ./...

tidy:
	go mod tidy

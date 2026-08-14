BINARY   := weight-tracker
DB       := weight-tracker.db
ADDR     := :8080
BIN_DIR  := bin
PID_FILE := $(BIN_DIR)/$(BINARY).pid
LOG_FILE := $(BIN_DIR)/$(BINARY).log

PI_HOST ?= jcwpi
PI_USER ?= jcw
# set PI_ARCH=armv6 for 32-bit Raspberry Pi OS
PI_ARCH ?= arm64

.PHONY: run build build-pi start stop restart status logs clean fmt vet tidy deploy help

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
		echo $$! > $(PID_FILE); \
		sleep 0.3; \
		echo "started (pid `cat $(PID_FILE)`) on $(ADDR), logs at $(LOG_FILE)"; \
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

deploy: build-pi
	scp $(BIN_DIR)/$(BINARY)-$(PI_ARCH) $(PI_USER)@$(PI_HOST):~/$(BINARY)-new
	ssh $(PI_USER)@$(PI_HOST) '\
		sudo systemctl stop $(BINARY) 2>/dev/null; \
		mv ~/$(BINARY)-new ~/$(BINARY)-$(PI_ARCH); \
		chmod +x ~/$(BINARY)-$(PI_ARCH); \
		sudo systemctl start $(BINARY)'

clean: stop
	rm -rf $(BIN_DIR)
	rm -f $(DB) $(DB)-journal $(DB)-wal $(DB)-shm

fmt:
	gofmt -l -w .

vet:
	go vet ./...

tidy:
	go mod tidy

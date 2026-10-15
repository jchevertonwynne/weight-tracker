ADDR ?= :8080
DB ?= weight-tracker.db
# There is no Cloudflare Access in front of a local server, so every request
# arrives with no identity header and would be refused. DEV_USER supplies one.
DEV_USER ?= dev@example.com
# The addresses Access admits, which in production come from a ConfigMap the
# homelab repo generates from its access-policy.json. Empty here, so a local
# run admits whoever it is told to be. Set it to try the closed set (DEV_USER
# is admitted either way): make run ALLOWED='a@example.com,b@example.com'
ALLOWED ?=
# The zone the morning/evening split is judged in. The Deployment sets the
# same; without it the split follows this machine's own zone.
TZ ?= Europe/London

.PHONY: run build test test-zones check lint fmt clean help

help:
	@echo "make run        - run locally on $(ADDR) as $(DEV_USER), in $(TZ)"
	@echo "                  ALLOWED='a@x.com,b@x.com' to try the closed allowlist"
	@echo "make build      - release build into target/release/"
	@echo "make check      - everything CI runs: rustfmt, clippy, tests, tests in other zones"
	@echo "make test       - cargo test"
	@echo "make test-zones - the tests again under four zones, as CI runs them"
	@echo "make lint       - cargo clippy, warnings as errors"
	@echo "make fmt        - cargo fmt"
	@echo "make clean      - remove build output and the local dev database"

run:
	TZ=$(TZ) cargo run -- -addr $(ADDR) -db $(DB) -dev-user $(DEV_USER) -allowed-emails '$(ALLOWED)'

build:
	cargo build --release

test:
	cargo test --locked

# A half-hour and a 45-minute offset among them, where a timezone assumption
# would show.
test-zones:
	@for tz in Europe/London America/New_York Australia/Sydney Asia/Kathmandu; do \
		echo "TZ=$$tz"; TZ=$$tz cargo test --locked -q || exit 1; \
	done

# Mirrors the CI workflow, so a green 'make check' locally means a green CI.
check:
	cargo fmt --check
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) test-zones

fmt:
	cargo fmt

lint:
	cargo clippy --locked --all-targets -- -D warnings

clean:
	cargo clean
	rm -f $(DB) $(DB)-wal $(DB)-shm

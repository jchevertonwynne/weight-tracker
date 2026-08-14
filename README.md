# weight-tracker

A tiny self-hosted weight tracker for morning/evening weigh-ins, built to run on a Raspberry Pi.

- Go + `net/http` (no web framework)
- htmx for interactivity, no JS build step
- Hand-rolled Material-inspired CSS
- SQLite via `modernc.org/sqlite` (pure Go, no cgo — cross-compiles trivially)

When you log a weight, the time-of-day field defaults to morning (before noon)
or evening (noon or later) based on the server's clock, but you can change it
before submitting or edit any entry afterwards. Each morning entry shows the
overnight delta versus the most recent evening entry.

## Run locally

```sh
go run . -addr :8080 -db weight-tracker.db
```

Then open http://localhost:8080.

## Build for Raspberry Pi

No cgo and no cross-compilation toolchain needed — `modernc.org/sqlite` is pure Go.

```sh
# 64-bit Raspberry Pi OS (Pi 4/5, most current images)
GOOS=linux GOARCH=arm64 go build -o weight-tracker-arm64 .

# 32-bit Raspberry Pi OS (older images / Pi Zero W, Pi 2)
GOOS=linux GOARCH=arm GOARM=6 go build -o weight-tracker-armv6 .
```

Copy the binary to the Pi (`scp weight-tracker-arm64 jcw@jcwpi:~/`) and run it.
Port 8090 is used here since 8080 is already taken by Pi-hole's admin
dashboard on this Pi — adjust if that's not the case for you:

```sh
./weight-tracker-arm64 -addr :8090 -db /home/jcw/weight-tracker.db
```

## Run as a systemd service

Create `/etc/systemd/system/weight-tracker.service`:

```ini
[Unit]
Description=Weight tracker
After=network.target

[Service]
ExecStart=/home/jcw/weight-tracker-arm64 -addr :8080 -db /home/jcw/weight-tracker.db
Restart=on-failure
User=jcw

[Install]
WantedBy=multi-user.target
```

Then:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now weight-tracker
```

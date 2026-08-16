# weight-tracker

A tiny self-hosted weight tracker for morning/evening weigh-ins, built to run on a Raspberry Pi.

- Go + `net/http` (no web framework)
- htmx for interactivity, no JS build step
- Hand-rolled Material-inspired CSS
- SQLite via `modernc.org/sqlite` (pure Go, no cgo — cross-compiles trivially)
- Weights are stored as whole grams and displayed in kilograms; a REAL
  kilogram column could not represent a value like 82.4 exactly, which
  leaked into the CSV export as `82.400000000000006`

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
ExecStart=/home/jcw/weight-tracker-arm64 -addr :8090 -db /home/jcw/weight-tracker.db
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

## Backups

Settings → **Download backup** (or `GET /backup.db`) returns a consistent
snapshot of the whole database, taken with SQLite's `VACUUM INTO` — no need
to stop the app, and unlike copying the file off disk it captures writes
still sitting in the write-ahead log. The result is a single self-contained
file with no companion `-wal`/`-shm`, so restoring is just:

```sh
sudo systemctl stop weight-tracker
cp weight-tracker-backup-2026-08-16.db ~/weight-tracker.db
rm -f ~/weight-tracker.db-wal ~/weight-tracker.db-shm
sudo systemctl start weight-tracker
```

Prefer this over `export.csv` for backups: the CSV holds weigh-ins only,
while the snapshot keeps goals, markers, period overrides and row ids too.

To pull one from another machine on a schedule:

```sh
curl -f -o "weights-$(date +%F).db" http://jcwpi:8090/backup.db
```

The database runs in WAL mode, so a reader never blocks the writer and a
crash mid-write replays the log rather than risking a torn database file.

## Upgrading

Schema changes are applied automatically on startup, so upgrading is just
replacing the binary. Migrations that rewrite a table (the kilogram-to-gram
conversion) run inside a transaction and are skipped once applied, so
restarting is safe and repeatable.

They still rewrite your only copy of the data, so take a backup of the
database file first — the app is stopped at that point anyway:

```sh
sudo systemctl stop weight-tracker
cp ~/weight-tracker.db ~/weight-tracker.db.bak
```

If a migration fails, the app refuses to start rather than serving against a
half-converted database; check `journalctl -u weight-tracker` and restore the
backup.

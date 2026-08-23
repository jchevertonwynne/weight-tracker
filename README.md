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
./weight-tracker-arm64 -addr :8090 -db /var/lib/weight-tracker/weight-tracker.db
```

## Deployment

Runs on the k3s cluster described in
[homelab](https://github.com/jchevertonwynne/homelab), not as a systemd
service. Push to `main`: CI builds an arm64 image, Flux notices the new tag,
commits it to the homelab repo and rolls the pod. Nothing here touches the Pi
directly.

The database lives on a `hostPath` at `/var/lib/weight-tracker`, owned
`jcw:grafana` mode `2770` — setgid so Grafana can read the SQLite file. The
Deployment runs as `1000:126` to match. Newer apps use a PersistentVolumeClaim
instead; this one keeps the hostPath because the data was already there when
it moved into the cluster.

`TZ=Europe/London` is set in the manifest and is not cosmetic. This app splits
weigh-ins into morning and evening on local wall-clock time, and while the
binary embeds the zone database (`import _ "time/tzdata"`), Go still reads `TZ`
to decide what `time.Local` is. Without it the pod runs in UTC and the split
shifts by up to an hour, silently.

`weight.jchevertonwynne.uk` is served through a Cloudflare tunnel and sits
behind Cloudflare Access with an email allowlist. Access is enforced at
Cloudflare's edge, per hostname — nothing in this repo or the cluster
authenticates anything.

`make build-pi` still cross-compiles a bare binary, which is occasionally
useful for testing on the Pi directly, but it is not how this gets deployed.

## Backups

Settings → **Download backup** (or `GET /backup.db`) returns a consistent
snapshot of the whole database, taken with SQLite's `VACUUM INTO` — no need
to stop the app, and unlike copying the file off disk it captures writes
still sitting in the write-ahead log. The result is a single self-contained
file with no companion `-wal`/`-shm`, so restoring is just:

```sh
kubectl -n apps scale deploy/weight-tracker --replicas=0
sudo cp weight-tracker-backup-2026-08-16.db /var/lib/weight-tracker/weight-tracker.db
sudo rm -f /var/lib/weight-tracker/weight-tracker.db-wal \
           /var/lib/weight-tracker/weight-tracker.db-shm
kubectl -n apps scale deploy/weight-tracker --replicas=1
```

Prefer this over `export.csv` for backups: the CSV holds weigh-ins only,
while the snapshot keeps goals, markers, period overrides and row ids too.

To pull one from another machine on a schedule:

```sh
kubectl -n apps port-forward deploy/weight-tracker 8090:8090 &
curl -f -o "weights-$(date +%F).db" http://localhost:8090/backup.db
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
kubectl -n apps scale deploy/weight-tracker --replicas=0
sudo cp /var/lib/weight-tracker/weight-tracker.db \
        /var/lib/weight-tracker/weight-tracker.db.bak
```

If a migration fails, the app refuses to start rather than serving against a
half-converted database; check `kubectl -n apps logs deploy/weight-tracker` and
restore the backup.

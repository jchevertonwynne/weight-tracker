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
make run
```

Then open http://localhost:8080. `make run` passes `-dev-user`, which stands in
for the Cloudflare Access identity a local server never gets; without it, or an
`-allowed-emails` list, the app refuses to start (see Deployment).

## Build a bare binary for the Pi

This is not how the app is deployed (see below), but a bare binary is
occasionally useful for trying something on the Pi directly. No cgo and no
cross-compilation toolchain is needed — `modernc.org/sqlite` is pure Go:

```sh
make build-pi                  # 64-bit Raspberry Pi OS (Pi 4/5)
make build-pi PI_ARCH=armv6    # 32-bit Raspberry Pi OS (Pi Zero W, Pi 2)
```

Pick a port other than 8080, which Pi-hole's admin dashboard holds there, and a
database path other than the cluster's — two processes writing one SQLite file
is the split-brain `make deploy` refuses to cause.

## Deployment

Runs on the k3s cluster described in
[homelab](https://github.com/jchevertonwynne/homelab), not as a systemd
service. Push to `main`: CI builds an arm64 image, Flux notices the new tag,
commits it to the homelab repo and rolls the pod. Nothing here touches the Pi
directly.

The database lives on a `local-path` PersistentVolumeClaim, so on the node it
is under `/var/lib/rancher/k3s/storage/pvc-*_apps_weight-tracker/`, owned
`1000:1000`. It used to be a `hostPath` at `/var/lib/weight-tracker`; nothing
reads that directory any more, which is worth knowing before restoring into it.

`TZ=Europe/London` is set in the manifest and is not cosmetic. This app splits
weigh-ins into morning and evening on local wall-clock time, and while the
binary embeds the zone database (`import _ "time/tzdata"`), Go still reads `TZ`
to decide what `time.Local` is. Without it the pod runs in UTC and the split
shifts by up to an hour, silently.

`weight.jchevertonwynne.uk` is served through a Cloudflare tunnel and sits
behind Cloudflare Access with an email allowlist, enforced at Cloudflare's
edge, per hostname. The app checks that allowlist a second time, against the
`Cf-Access-Authenticated-User-Email` header Access adds and a copy of the list
in `-allowed-emails`, fed by a ConfigMap the homelab repo generates from the
policy file. That is not belt-and-braces: Access consults its policy only when
it issues a session, so someone removed from the policy keeps a working session
for up to a month, and this is what ends it — when the pod restarts onto the
new list, a minute or two after the change. A request with no header at all is
refused too, so that Access being removed or bypassed cannot read as "no
identity required".

Everything is behind that check except `/healthz`, `/metrics`, `/static/`,
`/sw.js` and `/backup.db`, each named in `routes` in `main.go` with its reason.
`/backup.db` is only half an exception: a caller with an Access identity is held
to the allowlist like anywhere else, and one without needs the bearer token in
`-backup-token-file`, which only the in-cluster backup job holds. Without that
token, a request with no identity is refused there too.
There is still no login, no accounts and no per-user data: everyone the
allowlist admits sees and edits the same entries.

Locally there is no Access at all, so `make run` passes `-dev-user`, which
stands in for the header and is admitted whether or not it is on the list.

## Backups

Settings → **Download backup** (or `GET /backup.db`) returns a consistent
snapshot of the whole database, taken with SQLite's `VACUUM INTO` — no need
to stop the app, and unlike copying the file off disk it captures writes
still sitting in the write-ahead log. The result is a single self-contained
file with no companion `-wal`/`-shm`.

The cluster already takes one every hour: a CronJob in the homelab repo
fetches it and commits it to the private `homelab-backups` repo, so
`data/weight-tracker.db` there is at most an hour old and its git history goes
back to any hour before that.

To restore one, scale the app to zero (it is the only writer, and the database
is WAL, so copying underneath a running pod gives you a file that is present but
wrong), put the snapshot into the volume, and remove the old log files with it:

```sh
kubectl -n apps scale deploy/weight-tracker --replicas=0
dir=$(sudo sh -c 'echo /var/lib/rancher/k3s/storage/pvc-*_apps_weight-tracker')
sudo install -o 1000 -g 1000 -m 0644 weight-tracker-backup-2026-08-16.db "$dir/weight-tracker.db"
sudo rm -f "$dir/weight-tracker.db-wal" "$dir/weight-tracker.db-shm"
kubectl -n apps scale deploy/weight-tracker --replicas=1
```

The `sudo sh -c` is there because `/var/lib/rancher/k3s/storage` is not
readable by an ordinary user, so the glob has to expand as root. Check `$dir`
names exactly one directory before going further.

Prefer this over `export.csv` for backups: the CSV holds weigh-ins only,
while the snapshot keeps goals, markers, period overrides and row ids too.

The database runs in WAL mode, so a reader never blocks the writer and a
crash mid-write replays the log rather than risking a torn database file.

## Upgrading

Schema changes are applied automatically on startup, so upgrading is just
deploying the new image. Migrations that rewrite a table (the kilogram-to-gram
conversion) run inside a transaction and are skipped once applied, so
restarting is safe and repeatable.

They still rewrite the live copy of the data. The hourly snapshot in
`homelab-backups` is a backup already, but if a commit carries a migration,
take a fresh one just before it lands: Settings → **Download backup**. That is
the same `VACUUM INTO` snapshot, so it includes whatever is still in the WAL —
which copying `weight-tracker.db` off the volume would not, even with the app
stopped, since SQLite does not always checkpoint the log away on shutdown.

If a migration fails, the app refuses to start rather than serving against a
half-converted database; check `kubectl -n apps logs deploy/weight-tracker` and
restore the backup as described under Backups.

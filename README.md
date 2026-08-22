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

## Expose it on the internet

Tailscale already reaches the app, but only from devices running the
Tailscale client. A Cloudflare tunnel puts it on `weight.jchevertonwynne.uk`
for any browser.

`cloudflared` runs alongside the app and makes an *outbound* connection to
Cloudflare, which routes the hostname back down it. No port forwarding, no
dynamic DNS, no inbound port on the router. TLS terminates at the edge on a
certificate Cloudflare renews, so the origin stays plain HTTP on loopback.

Service workers need a secure context, so `static/app.js`'s registration of
`/sw.js` now succeeds over the tunnel where it silently failed over
`http://jcwpi:8090`.

### One-time setup

Install cloudflared from Cloudflare's apt repo, so it updates with
everything else:

```sh
curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg \
  | sudo tee /usr/share/keyrings/cloudflare-main.gpg >/dev/null
echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared bookworm main' \
  | sudo tee /etc/apt/sources.list.d/cloudflared.list
sudo apt-get update && sudo apt-get install -y cloudflared
```

Authorize against the zone, then create the tunnel. `login` opens a browser
and writes `~/.cloudflared/cert.pem`; `create` prints the tunnel UUID and
writes the matching credentials JSON beside it:

```sh
cloudflared tunnel login
cloudflared tunnel create weight-tracker
```

Pass `login` the zone you actually own. Given a hostname outside every zone
on the account, `route dns` below will not error — it appends the zone it
does find and creates a nonsense record.

Copy `deploy/cloudflared-config.yml` to `/etc/cloudflared/config.yml`,
replacing both `TUNNEL_UUID` placeholders. Copy `deploy/cloudflared.service`
to `/etc/systemd/system/`. Then:

```sh
cloudflared --config /etc/cloudflared/config.yml tunnel ingress validate
sudo systemctl daemon-reload
sudo systemctl enable --now cloudflared
cloudflared tunnel route dns weight-tracker weight.jchevertonwynne.uk
```

`route dns` creates the proxied CNAME, so nothing needs adding by hand in
the dashboard. It comes last deliberately: until that record exists the
hostname does not resolve, so a misconfigured tunnel is never briefly
public.

Afterwards, `make deploy-tunnel` re-installs both files and restarts the
tunnel, reading the UUID back off the Pi so it never has to live in the
repo. It validates the rendered config before moving it into place, and is
kept out of `make deploy` so a routine binary deploy does not restart the
tunnel. CI cannot run it — the deploy key's forced-command wrapper allows
only the two commands `deploy` issues.

### Put Access in front of it first

**Before the DNS record exists, not after.** The app has no authentication —
every route in `handlers.RegisterRoutes` is open. Public with nothing in
front, any visitor can read every weigh-in, pull the database from
`GET /backup.db`, and wipe it with `POST /settings/delete-all`. New
hostnames appear in Certificate Transparency logs within seconds, so the
gap is not quiet time.

In the Zero Trust dashboard under Access → Applications, add a self-hosted
application. Its destination must be a **public hostname** —
`weight` + `jchevertonwynne.uk` — not a private IP, which routes at the
network layer and would require the WARP client on every device, leaving
the public hostname unprotected. Set the policy to *allow* / *emails* /
your address and enable one-time PIN or Google. Free up to 50 users, so
sharing later is one more email in the policy.

Access challenges unauthenticated requests at the edge; they never reach
the tunnel. Building sessions into the app instead would mean a login
handler, session storage, and CSRF on every htmx `POST`/`PUT`/`DELETE` —
worth it if the app ever grows per-user data, but not today.

### Checking it

```sh
systemctl status cloudflared
cloudflared tunnel info weight-tracker
curl -sI https://weight.jchevertonwynne.uk/backup.db | head -1
```

That last one must be a `302` to a `cloudflareaccess.com` login URL. A `200`
means requests are reaching the app unauthenticated — stop the tunnel until
the Access policy is attached.

Note that `make deploy` restarts `weight-tracker` but not `cloudflared`, so
a deploy shows up as a few failed origin requests in the tunnel log rather
than an outage.

## Continuous deployment

Pushing to `main` deploys automatically, once this one-time setup is done:

1. Generate a dedicated deploy-only SSH keypair (not your personal one) and
   add its public half to `jcw`'s `authorized_keys` on the Pi, restricted via
   a `command=` forced-command wrapper (`~/bin/wt-deploy-wrapper.sh`) to
   *only* the two commands `make deploy` issues — an upload of the new
   binary and the stop/replace/start sequence. This matters because `jcw`
   already has passwordless sudo for everything, so a sudoers-only
   restriction on this key would not actually restrict anything; the
   forced-command wrapper is what caps a leaked key's blast radius at
   "restart this one service."
2. In the Tailscale admin console, add a `tag:ci` entry to `tagOwners` in
   Policy file management, plus an ACL rule restricting `tag:ci` to the
   Pi's `:22` (SSH) only — nothing else on the tailnet. Then generate a
   reusable, ephemeral auth key tagged `tag:ci` (Settings → Keys → Generate
   auth key) — this account doesn't expose OAuth clients, just classic auth
   keys, so unlike an OAuth client's auto-minted short-lived keys, this one
   has a fixed expiration and needs manual rotation before then.
3. Add `TAILSCALE_AUTHKEY` and `PI_DEPLOY_SSH_KEY` (the deploy key's private
   half) as repository secrets (Settings → Secrets and variables → Actions).

From then on, every push to `main` that passes the `test`/`build` jobs joins
the tailnet, runs `make build-pi` and `make deploy` exactly as a human would
locally, then health-checks the Pi and fails loudly if it doesn't come back
up — see the `deploy` job in `.github/workflows/ci.yml`. There's no
auto-rollback: a failed health check just fails the CI run for a human to
investigate, the same as any bad deploy today.

`make deploy` still works locally too — for a one-off deploy from a branch
that hasn't been merged, or if CI/Tailscale/GitHub is unavailable.

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

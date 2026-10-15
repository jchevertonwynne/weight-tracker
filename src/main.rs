//! A tiny self-hosted weight tracker for morning and evening weigh-ins, run
//! on the k3s cluster described in jchevertonwynne/homelab and served at
//! weight.jchevertonwynne.uk behind Cloudflare Access.

mod chart;
mod clock;
mod db;
mod goals;
mod history;
mod importer;
mod markers;
mod overnight;
mod summary;
#[cfg(test)]
mod testsupport;
mod timerange;
mod web;
mod weight;

use std::sync::Arc;

use anyhow::{Context as _, Result, anyhow};

#[tokio::main]
async fn main() {
    let flags = homelab::flags::Flags::new()
        .string("addr", ":8080", "listen address")
        .string("db", "weight-tracker.db", "path to sqlite database file")
        .string(
            "otel-endpoint",
            "",
            "host:port of an OTLP/gRPC trace collector; tracing is disabled if empty",
        )
        .string(
            "pprof-addr",
            ":6060",
            "listen address for pprof debug endpoints; never expose this outside the cluster",
        )
        .string(
            "dev-user",
            "",
            "email to assume when no Access header is present; local development only",
        )
        .string(
            "allowed-emails",
            "",
            "comma-separated addresses this hostname's Cloudflare Access policy admits; anyone else is refused even with a live Access session",
        )
        .string(
            "backup-token-file",
            "",
            "file holding the bearer token the in-cluster backup job presents for /backup.db; without it, /backup.db needs an Access identity like every other route",
        )
        .parse();

    homelab::logging::init();

    if let Err(err) = run(&flags).await {
        tracing::error!(error = format!("{err:#}"), "exiting");
        std::process::exit(1);
    }
}

/// Everything after the flags, so that the one exiting path is in main and
/// a failure here still flushes the traces on its way out.
async fn run(flags: &homelab::flags::Parsed) -> Result<()> {
    let dev_user = flags.get("dev-user");
    let allow = homelab::access::Allowlist::new(flags.get("allowed-emails"), dev_user);
    if !dev_user.is_empty() {
        // The address is deliberately not logged: it is the identity every
        // header-less request is about to be treated as, and these lines go
        // to Loki, where an email is a worse thing to have than a line
        // telling you to go and read the flags.
        tracing::warn!(
            "-dev-user is set; every request without an Access header is treated as that user"
        );
    }
    // Fatal, like a database that will not open. The allowlist is this app's
    // entire authorisation model, so an empty one admits everyone Access
    // admits — including a session issued before someone was taken off the
    // policy, which is the case it exists for. In production that state means
    // the ConfigMap never reached the flag, and a pod that crashloops says so
    // where a pod serving everybody does not.
    allow.check().map_err(|e| {
        anyhow!("access config: {e} (pass -allowed-emails, or -dev-user for a local run)")
    })?;

    let backup_token = read_backup_token(flags.get("backup-token-file"))
        .await
        .context("backup token")?;
    if backup_token.is_empty() {
        // Not fatal: the failure is contained. /backup.db then refuses every
        // caller without an Access identity, so the hourly backup fails and
        // says so (KubeJobFailed, then BackupStale), and nothing is exposed.
        tracing::warn!("no -backup-token-file; /backup.db will refuse the backup job");
    }

    homelab::profiling::spawn(flags.get("pprof-addr").to_string());

    let tracing = homelab::trace::init("weight-tracker", flags.get("otel-endpoint"))
        .context("init tracing")?;
    let served = serve(flags, allow, &backup_token).await;
    // After the server has drained, so the last batch of spans — a rollout's
    // final requests — reaches Alloy.
    tracing.shutdown().await;
    served
}

async fn serve(
    flags: &homelab::flags::Parsed,
    allow: homelab::access::Allowlist,
    backup_token: &str,
) -> Result<()> {
    let path = flags.get("db").to_string();
    let db = tokio::task::spawn_blocking(move || db::Db::open(path))
        .await
        .context("open database")?
        .context("open database")?;

    let journal_mode = db.journal_mode().context("read journal mode")?;
    if journal_mode != "wal" {
        // Not fatal — the rollback journal is still correct — but worth
        // saying, since it usually means a filesystem that cannot do WAL.
        tracing::warn!(journal_mode, "journal mode is not WAL");
    }

    let server = web::Server::new(db, Arc::new(jiff::Zoned::now), allow, backup_token);
    homelab::serve::run_with(
        flags.get("addr"),
        server.router(),
        homelab::serve::Options {
            log_attrs: vec![
                ("db", flags.get("db").to_string()),
                ("journal_mode", journal_mode),
            ],
            ..Default::default()
        },
    )
    .await
    .context("serve")
}

/// The backup job's token from `path`, or "" with no path. A path given but
/// empty is an error rather than "no token": it means the Secret is mounted
/// and wrong, and a pod that crashloops says so where one quietly refusing
/// every backup takes an hour and an alert to notice.
async fn read_backup_token(path: &str) -> Result<String> {
    if path.is_empty() {
        return Ok(String::new());
    }
    let raw = tokio::fs::read_to_string(path)
        .await
        .with_context(|| format!("open {path}"))?;
    let token = raw.trim();
    if token.is_empty() {
        return Err(anyhow!("{path} is empty"));
    }
    Ok(token.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn reads_the_backup_token() {
        assert_eq!(read_backup_token("").await.unwrap(), "");
        let dir = tempfile::tempdir().unwrap();
        let good = dir.path().join("token");
        std::fs::write(&good, "s3cret\n").unwrap();
        assert_eq!(
            read_backup_token(good.to_str().unwrap()).await.unwrap(),
            "s3cret"
        );
        let empty = dir.path().join("empty");
        std::fs::write(&empty, "\n").unwrap();
        assert!(read_backup_token(empty.to_str().unwrap()).await.is_err());
        assert!(
            read_backup_token(dir.path().join("missing").to_str().unwrap())
                .await
                .is_err()
        );
    }
}

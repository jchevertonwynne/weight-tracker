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
mod model;
mod overnight;
mod summary;
#[cfg(test)]
mod testsupport;
mod timeline;
mod timerange;
mod web;

use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use anyhow::{Context as _, Result, anyhow};
use clap::Parser;

/// A self-hosted weight tracker, behind Cloudflare Access.
#[derive(Parser)]
struct Args {
    /// Address to listen on.
    #[arg(long, default_value = "0.0.0.0:8080")]
    addr: SocketAddr,
    /// The SQLite database file.
    #[arg(long, default_value = "weight-tracker.db")]
    db: PathBuf,
    /// host:port of an OTLP/gRPC trace collector; tracing is off when empty.
    #[arg(long, default_value = "")]
    otel_endpoint: String,
    /// Address for the pprof endpoints. Never expose it outside the cluster.
    #[arg(long, default_value = "0.0.0.0:6060")]
    pprof_addr: SocketAddr,
    /// Email to assume when no Access header is present; local development
    /// only.
    #[arg(long, default_value = "")]
    dev_user: String,
    /// Comma-separated addresses this hostname's Cloudflare Access policy
    /// admits; anyone else is refused even with a live Access session.
    #[arg(long, default_value = "")]
    allowed_emails: String,
    /// File holding the bearer token the in-cluster backup job presents for
    /// /backup.db; without it, /backup.db needs an Access identity like
    /// every other route.
    #[arg(long)]
    backup_token_file: Option<PathBuf>,
}

#[tokio::main]
async fn main() -> Result<()> {
    let args = Args::parse();
    let tracing = homelab::init("weight-tracker", &args.otel_endpoint).context("init tracing")?;

    let allow = homelab::access::Allowlist::new(&args.allowed_emails, &args.dev_user);
    if !args.dev_user.is_empty() {
        // The address is deliberately not logged: it is the identity every
        // header-less request is about to be treated as, and these lines go
        // to Loki, where an email is a worse thing to have than a line
        // telling you to go and read the flags.
        tracing::warn!(
            "--dev-user is set; every request without an Access header is treated as that user"
        );
    }
    // Fatal, like a database that will not open. The allowlist is this app's
    // entire authorisation model, so an empty one admits everyone Access
    // admits — including a session issued before someone was taken off the
    // policy, which is the case it exists for. In production that state means
    // the ConfigMap never reached the flag, and a pod that crashloops says so
    // where a pod serving everybody does not.
    allow.check().map_err(|e| {
        anyhow!("access config: {e} (pass --allowed-emails, or --dev-user for a local run)")
    })?;

    let backup_token = read_backup_token(args.backup_token_file.as_deref())
        .await
        .context("backup token")?;
    if backup_token.is_empty() {
        // Not fatal: the failure is contained. /backup.db then refuses every
        // caller without an Access identity, so the hourly backup fails and
        // says so (KubeJobFailed, then BackupStale), and nothing is exposed.
        tracing::warn!("no --backup-token-file; /backup.db will refuse the backup job");
    }

    homelab::profiling::spawn(args.pprof_addr);

    let served = serve(&args, allow, &backup_token).await;
    // After the server has drained, so the last batch of spans — a rollout's
    // final requests — reaches Alloy.
    tracing.shutdown().await;
    served
}

async fn serve(args: &Args, allow: homelab::access::Allowlist, backup_token: &str) -> Result<()> {
    let path = args.db.clone();
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
        args.addr,
        server.router(),
        homelab::serve::Options {
            log_attrs: vec![
                ("db", args.db.display().to_string()),
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
async fn read_backup_token(path: Option<&Path>) -> Result<String> {
    let Some(path) = path else {
        return Ok(String::new());
    };
    let raw = tokio::fs::read_to_string(path)
        .await
        .with_context(|| format!("open {}", path.display()))?;
    let token = raw.trim();
    if token.is_empty() {
        return Err(anyhow!("{} is empty", path.display()));
    }
    Ok(token.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn reads_the_backup_token() {
        assert_eq!(read_backup_token(None).await.unwrap(), "");
        let dir = tempfile::tempdir().unwrap();
        let good = dir.path().join("token");
        std::fs::write(&good, "s3cret\n").unwrap();
        assert_eq!(read_backup_token(Some(&good)).await.unwrap(), "s3cret");
        let empty = dir.path().join("empty");
        std::fs::write(&empty, "\n").unwrap();
        assert!(read_backup_token(Some(&empty)).await.is_err());
        assert!(
            read_backup_token(Some(&dir.path().join("missing")))
                .await
                .is_err()
        );
    }
}

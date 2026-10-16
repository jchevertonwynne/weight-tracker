//! The HTTP surface: the route table, who may call it, and the handlers.

mod auth;
mod error;
mod form;
mod handlers;
mod pages;
#[cfg(test)]
mod tests;

use std::sync::Arc;

use axum::Router;
use axum::extract::DefaultBodyLimit;
use axum::http::header;
use axum::middleware::{from_fn, from_fn_with_state};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use homelab::access::Allowlist;
use jiff::Zoned;

use crate::db::Db;
use error::Error;

/// The clock, injected so tests can pin it; production passes
/// [`Zoned::now`].
pub type Clock = Arc<dyn Fn() -> Zoned + Send + Sync>;

/// What every handler needs.
#[derive(Clone)]
pub struct Server {
    db: Db,
    now: Clock,
    /// This hostname's Access allowlist: the whole of this app's
    /// authorisation model, since it has no accounts and no per-user data.
    allow: Arc<Allowlist>,
    /// What the in-cluster backup job presents for /backup.db. Empty means
    /// none is configured, and then nothing gets in that way at all.
    backup_token: Arc<str>,
}

/// The CSV upload, whole. A decade of twice-daily weigh-ins is a few hundred
/// KB, so this is generous; it is what the default 2 MB limit would have
/// refused a long export at.
const MAX_IMPORT_BYTES: usize = 32 << 20;

impl Server {
    pub fn new(db: Db, now: Clock, allow: Allowlist, backup_token: &str) -> Self {
        Server {
            db,
            now,
            allow: Arc::new(allow),
            backup_token: Arc::from(backup_token),
        }
    }

    fn now(&self) -> Zoned {
        (self.now)()
    }

    /// The route table, with every application route behind authenticate.
    ///
    /// The split is the point. Application routes are one router with one
    /// wrapper, its fallback included, so a route added later is
    /// authenticated without anyone remembering to make it so. The
    /// exceptions are named here, each for a reason:
    ///
    /// - /healthz is probed by the kubelet, which arrives as the node with no
    ///   Access session, and a probe that depends on Access would restart the
    ///   pod whenever Access broke rather than when the app did.
    /// - /metrics is scraped by Alloy, straight at the pod IP, likewise.
    /// - /static/ and /sw.js are the same bytes for everyone and carry nothing
    ///   about anybody.
    /// - /backup.db is fetched hourly by the in-cluster backup CronJob, which
    ///   has no session and presents a token instead — and by the Download
    ///   backup link on the settings page, which has a session. See
    ///   [`auth::allow_backup`].
    pub fn router(&self) -> Router {
        let statics = memory_serve::load!()
            .index_file(None)
            .cache_control(memory_serve::CacheControl::NoCache)
            .into_router()
            // /static/ is outside authenticate, so a miss is a plain 404
            // here rather than falling through to the app router, which
            // would ask who is calling.
            .fallback(|| async { Error::NotFound });
        let outer = Router::new()
            .route("/healthz", get(healthz))
            .route("/metrics", get(homelab::metrics::handler))
            .route("/sw.js", get(service_worker))
            .route(
                "/backup.db",
                get(handlers::backup).layer(from_fn_with_state(self.clone(), auth::allow_backup)),
            )
            .nest("/static", statics)
            .with_state(self.clone());

        // refuse_cross_origin outermost: a cross-site request is refused
        // before anyone asks who sent it.
        let app = Router::new()
            .route("/", get(handlers::index))
            .route("/chart", get(handlers::chart))
            .route("/summary", get(handlers::summary))
            .route("/overnight", get(handlers::overnight))
            .route("/overnight/windows", get(handlers::overnight_timescales))
            .route("/entries", get(handlers::entries).post(handlers::add_entry))
            .route(
                "/entries/{id}",
                get(handlers::show_entry)
                    .put(handlers::update_entry)
                    .delete(handlers::delete_entry),
            )
            .route("/entries/{id}/edit", get(handlers::edit_entry))
            .route("/goals", post(handlers::add_goal))
            .route(
                "/goals/{id}",
                get(handlers::show_goal)
                    .put(handlers::update_goal)
                    .delete(handlers::delete_goal),
            )
            .route("/goals/{id}/edit", get(handlers::edit_goal))
            .route("/markers", post(handlers::add_marker))
            .route(
                "/markers/{id}",
                get(handlers::show_marker)
                    .put(handlers::update_marker)
                    .delete(handlers::delete_marker),
            )
            .route("/markers/{id}/edit", get(handlers::edit_marker))
            .route("/export.csv", get(handlers::export))
            .route(
                "/import",
                get(handlers::import_form)
                    .post(handlers::import)
                    .layer(DefaultBodyLimit::max(MAX_IMPORT_BYTES)),
            )
            .route("/settings/delete-all", post(handlers::delete_everything))
            .fallback(|| async { Error::NotFound })
            .layer(from_fn_with_state(self.clone(), auth::authenticate))
            .layer(from_fn(auth::refuse_cross_origin))
            .with_state(self.clone());

        homelab::http::instrument_layered(outer, app, &[])
    }
}

/// For the Kubernetes probes, so they do not hit "/", which lists every
/// entry, goal and marker and rebuilds the summary — several times a minute,
/// forever, on a Raspberry Pi. It does not touch the database: this answers
/// "is the process serving?", and a broken database should be a 500 on a
/// real request, not a restart loop.
async fn healthz() -> Response {
    (
        [
            (header::CONTENT_TYPE, "text/plain; charset=utf-8"),
            (header::CACHE_CONTROL, "no-store"),
        ],
        "ok",
    )
        .into_response()
}

static SERVICE_WORKER: &[u8] = include_bytes!("../../static/sw.js");

/// The service worker from a top-level path, so its default scope is the
/// whole app rather than /static/. Revalidated on every load: it is what
/// teaches an installed browser a new caching strategy, so a stale copy can
/// pin an old one indefinitely.
async fn service_worker() -> Response {
    (
        [
            (
                header::CONTENT_TYPE,
                "application/javascript; charset=utf-8",
            ),
            (header::CACHE_CONTROL, "no-cache"),
        ],
        SERVICE_WORKER,
    )
        .into_response()
}

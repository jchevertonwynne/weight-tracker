use axum::extract::{Request, State};
use axum::http::{HeaderMap, HeaderValue, Method, StatusCode, header};
use axum::middleware::Next;
use axum::response::Response;
use homelab::access;
use subtle::ConstantTimeEq as _;

use super::Server;

/// Resolves the caller or refuses the request.
///
/// This app stores one person's weigh-ins and has no login, no accounts and
/// no per-user data: everyone who gets in sees and edits the same entries.
/// So the only question it asks is whether the caller is on this hostname's
/// Access allowlist, which is also why that list is the whole of its
/// authorisation model.
///
/// Refusing a request with no header at all matters as much as refusing an
/// unlisted one. If the Access application in front of this hostname were
/// removed, misconfigured or bypassed, the header would simply stop
/// arriving, and treating that as "no identity required" would put a year of
/// weigh-ins on the public internet. The NetworkPolicy is what makes the
/// header itself trustworthy: apps/weight-tracker/networkpolicy.yaml in the
/// homelab repo admits the cloudflared pod and the backup job, so nothing
/// else in the cluster can set it to anything it likes.
pub async fn authenticate(State(s): State<Server>, req: Request, next: Next) -> Response {
    let mut res = match refusal(&s, req.headers()) {
        Some(refused) => refused,
        None => next.run(req).await,
    };
    // Every page here is one person's weight history. None of it may be
    // served from a shared cache, Cloudflare's edge included.
    res.headers_mut()
        .entry(header::CACHE_CONTROL)
        .or_insert(HeaderValue::from_static("no-store"));
    res
}

fn refusal(s: &Server, headers: &HeaderMap) -> Option<Response> {
    let Some(email) = access::email(headers, s.allow.dev_user()) else {
        tracing::error!(
            header = access::EMAIL_HEADER,
            "no Access header and no -dev-user configured"
        );
        return Some(homelab::http::error(
            StatusCode::FORBIDDEN,
            "not authenticated",
        ));
    };
    // Access checks its policy when it issues a session and not again, so
    // someone taken off the policy keeps a working header until that session
    // expires — a month, at this hostname's session length. This is what
    // ends it, once the pod has restarted onto the new ConfigMap.
    if !s.allow.admits(&email) {
        // The address is not logged: these lines go to Loki, where an email
        // is a worse thing to have than the fact of a refusal.
        tracing::warn!("refusing a caller who is not on this hostname's allowlist");
        return Some(homelab::http::error(
            StatusCode::FORBIDDEN,
            "not on this app's allowlist",
        ));
    }
    None
}

/// Guards /backup.db, which has two legitimate callers with nothing in
/// common.
///
/// The Download backup link on the settings page comes through cloudflared,
/// so it carries an Access identity and is held to the allowlist like any
/// other route: a header naming someone off the list is a person whose
/// session outlived their removal, and this file is every weigh-in in one
/// download.
///
/// The hourly CronJob reaches the Service from inside the cluster, so it has
/// no Access session and sends no header. It used to be admitted for exactly
/// that, which also admitted everyone else with no header — including the
/// whole internet, through cloudflared, the moment the Access application
/// were removed or bypassed. So the job presents a bearer token, and a
/// request with neither an identity nor that token is refused. With no token
/// configured, nothing gets in that way at all.
pub async fn allow_backup(State(s): State<Server>, req: Request, next: Next) -> Response {
    let mut res = match backup_refusal(&s, req.headers()) {
        Some(refused) => refused,
        None => next.run(req).await,
    };
    // The whole database, so never a shared cache either.
    res.headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    res
}

fn backup_refusal(s: &Server, headers: &HeaderMap) -> Option<Response> {
    if let Some(email) = access::email(headers, s.allow.dev_user()) {
        if s.allow.admits(&email) {
            return None;
        }
        tracing::warn!("refusing a backup for a caller who is not on this hostname's allowlist");
        return Some(homelab::http::error(
            StatusCode::FORBIDDEN,
            "not on this app's allowlist",
        ));
    }
    if bearer_matches(headers, &s.backup_token) {
        return None;
    }
    tracing::warn!("refusing a backup with neither an Access identity nor the backup token");
    Some(homelab::http::error(
        StatusCode::FORBIDDEN,
        "not authenticated",
    ))
}

/// Whether the request presents `token` as a bearer credential, compared in
/// constant time. An empty token matches nothing, so an unconfigured app
/// cannot be satisfied by an empty Authorization header.
fn bearer_matches(headers: &HeaderMap, token: &str) -> bool {
    if token.is_empty() {
        return false;
    }
    headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.as_bytes().strip_prefix(b"Bearer "))
        .is_some_and(|got| bool::from(got.ct_eq(token.as_bytes())))
}

/// Refuses any state-changing request that another site made the browser
/// send.
///
/// This app has no login of its own. Cloudflare Access decides who is
/// calling from its CF_Authorization cookie, and the browser attaches that
/// cookie to any request to this hostname, including a form on another site
/// that posts here. The Access application leaves SameSite unset, and Safari
/// and Firefox then send the cookie on a cross-site POST. Without this, a
/// page visited while signed in could auto-submit a form to POST
/// /settings/delete-all and wipe every entry, or to POST /import and fill
/// the chart with its own data.
///
/// This is net/http's CrossOriginProtection, which the Go version used, rule
/// for rule: a request other than GET, HEAD or OPTIONS whose Sec-Fetch-Site
/// says another site sent it, or, from a browser too old to send that
/// header, whose Origin names another host. The app's own htmx requests and
/// forms are same-origin and pass, and a request with neither header is not
/// a browser and so has no cookie to borrow. GET is never refused, which is
/// why no GET here may change anything.
pub async fn refuse_cross_origin(req: Request, next: Next) -> Response {
    if cross_origin(&req) {
        tracing::warn!(
            method = %req.method(),
            path = decoded_path(req.uri().path()),
            "refusing a cross-origin request"
        );
        return homelab::http::error(StatusCode::FORBIDDEN, "cross-origin request refused");
    }
    next.run(req).await
}

fn cross_origin(req: &Request) -> bool {
    if matches!(*req.method(), Method::GET | Method::HEAD | Method::OPTIONS) {
        return false;
    }
    let header = |name| {
        req.headers()
            .get(name)
            .map(|v| String::from_utf8_lossy(v.as_bytes()).into_owned())
            .unwrap_or_default()
    };
    match header("sec-fetch-site").as_str() {
        // An older browser: fall back to Origin.
        "" => {}
        "same-origin" | "none" => return false,
        // cross-site, and same-site too: another subdomain of
        // jchevertonwynne.uk is not this origin.
        _ => return true,
    }
    let origin = header("origin");
    if origin.is_empty() {
        return false;
    }
    let host = req
        .uri()
        .authority()
        .map(|a| a.to_string())
        .unwrap_or_else(|| header("host"));
    // url.Parse's Host: what follows the scheme, up to any path.
    let origin_host = origin
        .split_once("://")
        .map(|(_, rest)| rest.split(['/', '?', '#']).next().unwrap_or_default());
    origin_host != Some(host.as_str())
}

/// Go's r.URL.Path is the decoded path, and the log line carried that.
fn decoded_path(raw: &str) -> String {
    percent_encoding::percent_decode_str(raw)
        .decode_utf8_lossy()
        .into_owned()
}

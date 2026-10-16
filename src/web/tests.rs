//! The route table and handlers, exercised through the real router.

use std::sync::Arc;

use axum::body::Body;
use axum::extract::Request;
use axum::http::{HeaderMap, StatusCode};
use homelab::access::{Allowlist, EMAIL_HEADER};
use http_body_util::BodyExt as _;
use tower::ServiceExt as _;

use super::Server;
use crate::db::Db;

const ALLOWED: &str = "allowed@example.com";
const REMOVED: &str = "removed@example.com";
const BACKUP_TOKEN: &str = "test-backup-token";

/// A server on a throwaway database, with the real clock.
struct T {
    _dir: tempfile::TempDir,
    db: Db,
    server: Server,
}

fn server_with(allowed: &str, dev_user: &str, token: &str) -> T {
    let dir = tempfile::tempdir().unwrap();
    let db = Db::open(dir.path().join("test.db")).unwrap();
    let server = Server::new(
        db.clone(),
        Arc::new(jiff::Zoned::now),
        Allowlist::new(allowed, dev_user),
        token,
    );
    T {
        _dir: dir,
        db,
        server,
    }
}

fn server() -> T {
    server_with(ALLOWED, "", BACKUP_TOKEN)
}

struct Res {
    status: StatusCode,
    headers: HeaderMap,
    body: Vec<u8>,
}

impl Res {
    fn text(&self) -> String {
        String::from_utf8_lossy(&self.body).into_owned()
    }
}

impl T {
    async fn send(&self, req: Request) -> Res {
        let res = self.server.router().oneshot(req).await.unwrap();
        let (parts, body) = res.into_parts();
        Res {
            status: parts.status,
            headers: parts.headers,
            body: body.collect().await.unwrap().to_bytes().to_vec(),
        }
    }

    /// A GET as `email`, or with no Access header when it is empty.
    async fn ask(&self, path: &str, email: &str) -> Res {
        let mut req = Request::get(path);
        if !email.is_empty() {
            req = req.header(EMAIL_HEADER, email);
        }
        self.send(req.body(Body::empty()).unwrap()).await
    }

    /// A form request as the allowed address.
    async fn form(&self, method: &str, path: &str, fields: &[(&str, &str)]) -> Res {
        let body = form_urlencoded::Serializer::new(String::new())
            .extend_pairs(fields)
            .finish();
        let req = Request::builder()
            .method(method)
            .uri(path)
            .header(EMAIL_HEADER, ALLOWED)
            .header("Content-Type", "application/x-www-form-urlencoded")
            .body(Body::from(body))
            .unwrap();
        self.send(req).await
    }

    async fn create(&self, fields: &[(&str, &str)]) -> Res {
        self.form("POST", "/entries", fields).await
    }

    async fn upload(&self, csv: &str, unit: &str) -> Res {
        let boundary = "XBOUNDARYX";
        let body = format!(
            "--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"weights.csv\"\r\nContent-Type: text/csv\r\n\r\n{csv}\r\n\
             --{boundary}\r\nContent-Disposition: form-data; name=\"unit\"\r\n\r\n{unit}\r\n--{boundary}--\r\n"
        );
        let req = Request::post("/import")
            .header(EMAIL_HEADER, ALLOWED)
            .header(
                "Content-Type",
                format!("multipart/form-data; boundary={boundary}"),
            )
            .body(Body::from(body))
            .unwrap();
        self.send(req).await
    }
}

/// Held by every test that downloads a backup: they stage in the shared
/// temp directory, and one of them counts what is left there.
static BACKUPS: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

/// A valid weigh-in, for tests that vary one field.
fn entry_form(weight: &str) -> Vec<(&'static str, String)> {
    vec![
        ("weight_kg", weight.to_string()),
        ("recorded_at_date", "2026-08-16".into()),
        ("recorded_at_time", "07:30".into()),
        ("period_override", String::new()),
    ]
}

fn pairs<'a>(v: &'a [(&'static str, String)]) -> Vec<(&'static str, &'a str)> {
    v.iter().map(|(k, v)| (*k, v.as_str())).collect()
}

// --- Who may call what --------------------------------------------------

// The case the allowlist exists for: someone removed from the policy keeps a
// valid header until their session expires.
#[tokio::test]
async fn removed_address_is_refused() {
    let t = server();
    for path in [
        "/",
        "/entries",
        "/summary",
        "/chart",
        "/export.csv",
        "/import",
        "/overnight",
    ] {
        assert_eq!(
            t.ask(path, REMOVED).await.status,
            403,
            "GET {path} as removed"
        );
        assert_ne!(
            t.ask(path, ALLOWED).await.status,
            403,
            "GET {path} as allowed"
        );
    }
}

// A route added later is behind the fallback, and so authenticated.
#[tokio::test]
async fn unknown_path_is_still_authenticated() {
    let t = server();
    assert_eq!(t.ask("/whatever-comes-next", REMOVED).await.status, 403);
    assert_eq!(t.ask("/whatever-comes-next", ALLOWED).await.status, 404);
}

// If Access were bypassed the header would simply stop arriving.
#[tokio::test]
async fn no_header_is_refused() {
    let res = server().ask("/", "").await;
    assert_eq!(res.status, 403);
    assert_eq!(res.text(), "not authenticated");
}

#[tokio::test]
async fn dev_user_stands_in_for_the_header() {
    let _serial = BACKUPS.lock().await;
    let t = server_with(ALLOWED, "dev@local", "");
    for path in ["/", "/backup.db"] {
        assert_ne!(t.ask(path, "").await.status, 403, "GET {path}");
    }
}

// The kubelet, Alloy and the browser's asset requests have no session.
#[tokio::test]
async fn unauthenticated_routes_stay_open() {
    let t = server();
    for path in ["/healthz", "/metrics", "/sw.js", "/static/style.css"] {
        assert_eq!(t.ask(path, "").await.status, 200, "GET {path}");
    }
    // And a missing asset is a plain 404, not a refusal.
    assert_eq!(t.ask("/static/nope.js", "").await.status, 404);
}

async fn ask_with_bearer(t: &T, token: &str, email: &str) -> Res {
    let mut req = Request::get("/backup.db").header("Authorization", format!("Bearer {token}"));
    if !email.is_empty() {
        req = req.header(EMAIL_HEADER, email);
    }
    t.send(req.body(Body::empty()).unwrap()).await
}

// /backup.db: the CronJob with its token, the settings page with a session.
#[tokio::test]
async fn backup_serves_the_cron_job_and_allowed_addresses_only() {
    let _serial = BACKUPS.lock().await;
    let t = server();
    assert_eq!(ask_with_bearer(&t, BACKUP_TOKEN, "").await.status, 200);
    assert_eq!(t.ask("/backup.db", ALLOWED).await.status, 200);
    assert_eq!(t.ask("/backup.db", REMOVED).await.status, 403);
    assert_eq!(
        t.ask("/backup.db", ALLOWED).await.headers["cache-control"],
        "no-store"
    );
}

// With Access gone, a header-less request must not read as the backup job.
#[tokio::test]
async fn backup_refuses_a_headerless_request_without_the_token() {
    let t = server();
    assert_eq!(t.ask("/backup.db", "").await.status, 403);
    assert_eq!(ask_with_bearer(&t, "wrong", "").await.status, 403);
    // The identity, when there is one, is what is judged.
    assert_eq!(ask_with_bearer(&t, BACKUP_TOKEN, REMOVED).await.status, 403);
}

#[tokio::test]
async fn backup_with_no_token_configured_admits_no_headerless_caller() {
    let t = server_with(ALLOWED, "", "");
    for token in ["", BACKUP_TOKEN] {
        assert_eq!(
            ask_with_bearer(&t, token, "").await.status,
            403,
            "{token:?}"
        );
    }
}

// Unconfigured admits anyone Access admits — but the header has to be there.
#[tokio::test]
async fn empty_allowlist_admits_any_access_caller() {
    let t = server_with("", "", BACKUP_TOKEN);
    assert_ne!(t.ask("/", REMOVED).await.status, 403);
    assert_eq!(t.ask("/", "").await.status, 403);
}

// Refusals included, nothing authenticated reaches Cloudflare's cache.
#[tokio::test]
async fn authenticated_responses_are_not_cached() {
    let t = server();
    for email in [ALLOWED, REMOVED] {
        assert_eq!(
            t.ask("/", email).await.headers["cache-control"],
            "no-store",
            "{email}"
        );
    }
}

// The attack refuse_cross_origin exists for: a form on another site posting
// to /settings/delete-all with the browser's Access cookie.
#[tokio::test]
async fn cross_site_delete_all_is_refused() {
    let t = server();
    assert_eq!(t.create(&pairs(&entry_form("80.5"))).await.status, 200);
    for (h, v) in [
        ("Sec-Fetch-Site", "cross-site"),
        // Another subdomain is the same site but not the same origin.
        ("Sec-Fetch-Site", "same-site"),
        ("Origin", "https://evil.example"),
    ] {
        let req = Request::post("/settings/delete-all")
            .header(EMAIL_HEADER, ALLOWED)
            .header(h, v)
            .body(Body::empty())
            .unwrap();
        assert_eq!(t.send(req).await.status, 403, "{h}: {v}");
    }
    assert_eq!(t.db.entries().await.unwrap().len(), 1);

    // The app's own button is a same-origin form, and still works.
    let req = Request::post("/settings/delete-all")
        .header(EMAIL_HEADER, ALLOWED)
        .header("Sec-Fetch-Site", "same-origin")
        .body(Body::empty())
        .unwrap();
    let res = t.send(req).await;
    assert_eq!(res.status, 303);
    assert_eq!(res.headers["location"], "/");
    assert!(t.db.entries().await.unwrap().is_empty());
}

// --- Handlers -----------------------------------------------------------

// The form's min/step are client-side only, so a direct POST could store a
// negative or non-finite weight.
#[tokio::test]
async fn create_rejects_invalid_weights() {
    for w in ["-50", "0", "abc", "", "5000", "NaN", "Inf", "-Inf"] {
        let t = server();
        assert_eq!(t.create(&pairs(&entry_form(w))).await.status, 400, "{w:?}");
        assert!(t.db.entries().await.unwrap().is_empty(), "{w:?} was stored");
    }
}

#[tokio::test]
async fn create_accepts_valid_weights() {
    for w in ["82.4", "0.1", "1000"] {
        let t = server();
        let res = t.create(&pairs(&entry_form(w))).await;
        assert_eq!(res.status, 200, "{w}: {}", res.text());
        assert_eq!(t.db.entries().await.unwrap().len(), 1);
    }
}

// Only a write announces entries-changed; the summary and every chart redraw
// on it.
#[tokio::test]
async fn entries_list_only_triggers_entries_changed_on_writes() {
    let t = server();
    let res = t.create(&pairs(&entry_form("82.4"))).await;
    assert_eq!(res.headers["hx-trigger"], "entries-changed");
    let res = t.ask("/entries", ALLOWED).await;
    assert_eq!(res.status, 200);
    assert!(!res.headers.contains_key("hx-trigger"));
}

#[tokio::test]
async fn create_rejects_invalid_period_override_and_timestamp() {
    let t = server();
    let mut f = entry_form("82.4");
    f[3].1 = "afternoon".into();
    assert_eq!(t.create(&pairs(&f)).await.status, 400);
    let mut f = entry_form("82.4");
    f[1].1 = "not-a-date".into();
    assert_eq!(t.create(&pairs(&f)).await.status, 400);
}

#[tokio::test]
async fn goal_create_rejects_invalid_weights() {
    for w in ["-10", "0", "abc", "NaN"] {
        let t = server();
        let res = t
            .form(
                "POST",
                "/goals",
                &[("weight_kg", w), ("effective_from", "2026-08-01")],
            )
            .await;
        assert_eq!(res.status, 400, "{w}");
        assert!(t.db.goals().await.unwrap().is_empty());
    }
}

#[tokio::test]
async fn marker_create_requires_a_note() {
    for note in ["", "   "] {
        let t = server();
        let res = t
            .form(
                "POST",
                "/markers",
                &[("date", "2026-08-10"), ("note", note)],
            )
            .await;
        assert_eq!(res.status, 400, "{note:?}");
    }
}

// Deleting an already-deleted id used to report success.
#[tokio::test]
async fn delete_missing_row_returns_404() {
    let t = server();
    for path in ["/entries/999", "/goals/999", "/markers/999"] {
        assert_eq!(t.form("DELETE", path, &[]).await.status, 404, "{path}");
    }
    assert_eq!(
        t.form("PUT", "/entries/999", &pairs(&entry_form("80")))
            .await
            .status,
        404
    );
    let goal = [("weight_kg", "78"), ("effective_from", "2026-08-01")];
    assert_eq!(t.form("PUT", "/goals/999", &goal).await.status, 404);
    assert_eq!(t.form("DELETE", "/entries/abc", &[]).await.status, 400);
}

#[tokio::test]
async fn delete_existing_entry_returns_200_and_removes_it() {
    let t = server();
    assert_eq!(t.create(&pairs(&entry_form("82.4"))).await.status, 200);
    let id = t.db.entries().await.unwrap()[0].id;
    assert_eq!(
        t.form("DELETE", &format!("/entries/{id}"), &[])
            .await
            .status,
        200
    );
    assert!(t.db.entries().await.unwrap().is_empty());
    assert_eq!(
        t.form("DELETE", &format!("/entries/{id}"), &[])
            .await
            .status,
        404
    );
}

// A mutation's own response is filtered to the range the user is looking
// at, exactly as hx-include="#entries-filter" sends it.
#[tokio::test]
async fn mutations_preserve_the_selected_date_range() {
    let range = [
        ("range", "custom"),
        ("from", "2026-08-01"),
        ("until", "2026-08-31"),
    ];
    async fn seeded() -> (T, i64) {
        let t = server();
        for (date, weight) in [
            ("2026-01-01", "60.0"),
            ("2026-08-05", "70.0"),
            ("2026-08-10", "75.0"),
        ] {
            let mut f = entry_form(weight);
            f[1].1 = date.into();
            assert_eq!(t.create(&pairs(&f)).await.status, 200);
        }
        let id =
            t.db.entries()
                .await
                .unwrap()
                .iter()
                .find(|e| e.weight.0 == 70000)
                .unwrap()
                .id;
        (t, id)
    }

    // Delete: htmx sends the included fields in the query string.
    let (t, id) = seeded().await;
    let q = form_urlencoded::Serializer::new(String::new())
        .extend_pairs(range)
        .finish();
    let body = t
        .form("DELETE", &format!("/entries/{id}?{q}"), &[])
        .await
        .text();
    assert!(!body.contains("60.0"), "the range was not applied");
    assert!(!body.contains("70.0"), "the deleted entry is shown");
    assert!(body.contains("75.0"));

    // Update: in the body.
    let (t, id) = seeded().await;
    let owned = entry_form("71.0");
    let mut f = pairs(&owned);
    f[1].1 = "2026-08-05";
    f.extend(range);
    let body = t.form("PUT", &format!("/entries/{id}"), &f).await.text();
    assert!(!body.contains("60.0") && body.contains("71.0"));

    // Create.
    let (t, _) = seeded().await;
    let owned = entry_form("80.0");
    let mut f = pairs(&owned);
    f[1].1 = "2026-08-15";
    f.extend(range);
    let body = t.create(&f).await.text();
    assert!(!body.contains("60.0") && body.contains("80.0"));
}

#[tokio::test]
async fn edit_and_cancel_render_the_row() {
    let t = server();
    t.create(&pairs(&entry_form("82.4"))).await;
    let id = t.db.entries().await.unwrap()[0].id;
    let edit = t.ask(&format!("/entries/{id}/edit"), ALLOWED).await.text();
    assert!(edit.contains(r#"value="82.4""#) && edit.contains("entry-row-editing"));
    let row = t.ask(&format!("/entries/{id}"), ALLOWED).await.text();
    assert!(row.contains(&format!(r#"id="entry-{id}""#)) && !row.contains("entry-row-editing"));
    assert_eq!(t.ask("/entries/999/edit", ALLOWED).await.status, 404);
    assert_eq!(t.ask("/entries/999", ALLOWED).await.status, 404);
}

#[tokio::test]
async fn goal_and_marker_lists_announce_their_changes() {
    let t = server();
    let res = t
        .form(
            "POST",
            "/goals",
            &[("weight_kg", "78"), ("effective_from", "2026-08-01")],
        )
        .await;
    assert_eq!(
        (res.status.as_u16(), &res.headers["hx-trigger"]),
        (
            200,
            &"goals-changed".parse::<axum::http::HeaderValue>().unwrap()
        )
    );
    assert!(res.text().contains("78.0 kg"));
    let res = t
        .form(
            "POST",
            "/markers",
            &[("date", "2026-08-10"), ("note", "started")],
        )
        .await;
    assert_eq!(res.headers["hx-trigger"], "markers-changed");
    assert!(res.text().contains("started"));
}

// The form offered "grams" while the handler once accepted only kg and lb.
#[tokio::test]
async fn import_accepts_every_unit_the_form_offers() {
    for (unit, csv, want) in [
        (
            "kg",
            "recorded_at,weight\n2026-08-14T07:00:00Z,81.5\n",
            81500,
        ),
        (
            "lb",
            "recorded_at,weight\n2026-08-14T07:00:00Z,180\n",
            81647,
        ),
        (
            "g",
            "recorded_at,weight\n2026-08-14T07:00:00Z,81500\n",
            81500,
        ),
    ] {
        let t = server();
        let res = t.upload(csv, unit).await;
        assert_eq!(res.status, 200, "{unit}: {}", res.text());
        let entries = t.db.entries().await.unwrap();
        assert_eq!((entries.len(), entries[0].weight.0), (1, want), "{unit}");
    }
    let t = server();
    assert_eq!(
        t.upload("recorded_at,weight\n2026-08-14T07:00:00Z,81.5\n", "stone")
            .await
            .status,
        400
    );
}

#[tokio::test]
async fn import_is_idempotent() {
    let t = server();
    let csv = "recorded_at,weight_kg\n2026-08-14T07:00:00Z,81.5\n2026-08-15T07:00:00Z,81.2\n";
    for _ in 0..2 {
        assert_eq!(t.upload(csv, "kg").await.status, 200);
    }
    assert_eq!(t.db.entries().await.unwrap().len(), 2);
}

#[tokio::test]
async fn export_round_trips_through_import() {
    let t = server();
    for (w, time) in [("82.4", "07:30"), ("81.9", "21:15")] {
        let mut f = entry_form(w);
        f[2].1 = time.into();
        assert_eq!(t.create(&pairs(&f)).await.status, 200);
    }
    let res = t.ask("/export.csv", ALLOWED).await;
    assert_eq!(res.status, 200);
    assert!(
        res.headers["content-type"]
            .to_str()
            .unwrap()
            .starts_with("text/csv")
    );
    assert!(
        res.headers["content-disposition"]
            .to_str()
            .unwrap()
            .contains("attachment")
    );
    let exported = res.text();
    assert!(
        exported.starts_with("recorded_at,weight_kg\n"),
        "{exported}"
    );

    let fresh = server();
    assert_eq!(fresh.upload(&exported, "kg").await.status, 200);
    let (a, b) = (
        t.db.entries().await.unwrap(),
        fresh.db.entries().await.unwrap(),
    );
    assert_eq!(a.len(), b.len());
    for (x, y) in a.iter().zip(&b) {
        assert_eq!(
            (x.weight, x.recorded_at.timestamp()),
            (y.weight, y.recorded_at.timestamp())
        );
    }
}

#[tokio::test]
async fn index_renders_a_whole_page() {
    let t = server();
    assert_eq!(t.create(&pairs(&entry_form("82.4"))).await.status, 200);
    let res = t.ask("/", ALLOWED).await;
    assert_eq!(res.status, 200);
    assert_eq!(res.headers["content-type"], "text/html; charset=utf-8");
    let body = res.text();
    assert!(body.contains("82.4 kg") && body.contains("Weight Tracker"));
    assert!(
        body.trim_end().ends_with("</html>"),
        "a partial page was sent"
    );
}

// Each picker reads its range off the URL, so a reload lands on the same
// view.
#[tokio::test]
async fn index_reads_each_pickers_range_from_the_url() {
    let t = server();
    let body = t
        .ask(
            "/?history=7&overnight=custom&overnight_from=now-5d&overnight_until=now",
            ALLOWED,
        )
        .await
        .text();
    assert!(body.contains(r#"data-url-param="history" data-default-range="all""#));
    assert!(body.contains(r#"value="now-5d""#));
    assert!(body.contains("Last 7 days"));
}

#[tokio::test]
async fn chart_and_overnight_windows_return_json() {
    let t = server();
    t.create(&pairs(&entry_form("82.4"))).await;
    let res = t.ask("/chart?range=all&series=all", ALLOWED).await;
    assert_eq!(res.status, 200);
    assert_eq!(res.headers["content-type"], "application/json");
    assert!(res.text().contains(r#""hasData":true"#));
    let res = t.ask("/overnight/windows", ALLOWED).await;
    assert_eq!(res.headers["content-type"], "application/json");
    assert!(res.text().contains(r#""hasData":false"#));
    let res = t.ask("/overnight?range=all", ALLOWED).await;
    assert!(res.text().contains(r#"id="overnight-content""#));
    let res = t.ask("/summary", ALLOWED).await;
    assert!(res.text().contains("today-status"));
}

#[tokio::test]
async fn delete_all_clears_everything() {
    let t = server();
    t.create(&pairs(&entry_form("82.4"))).await;
    t.form(
        "POST",
        "/goals",
        &[("weight_kg", "78"), ("effective_from", "2026-08-01")],
    )
    .await;
    t.form(
        "POST",
        "/markers",
        &[("date", "2026-08-10"), ("note", "started")],
    )
    .await;
    assert_eq!(
        t.form("POST", "/settings/delete-all", &[]).await.status,
        303
    );
    assert!(t.db.entries().await.unwrap().is_empty());
    assert!(t.db.goals().await.unwrap().is_empty());
    assert!(t.db.markers().await.unwrap().is_empty());
}

#[tokio::test]
async fn backup_serves_a_restorable_database() {
    let _serial = BACKUPS.lock().await;
    let t = server();
    t.create(&pairs(&entry_form("82.4"))).await;
    t.form(
        "POST",
        "/markers",
        &[("date", "2026-08-10"), ("note", "started cutting")],
    )
    .await;

    let res = t.ask("/backup.db", ALLOWED).await;
    assert_eq!(res.status, 200, "{}", res.text());
    assert_eq!(res.headers["content-type"], "application/vnd.sqlite3");
    let cd = res.headers["content-disposition"].to_str().unwrap();
    assert!(cd.contains("attachment") && cd.contains(".db"), "{cd}");
    assert_eq!(
        res.headers["content-length"],
        res.body.len().to_string().as_str()
    );
    assert!(res.body.starts_with(b"SQLite format 3\0"));

    // Open the downloaded bytes, as restoring from them would.
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("restored.db");
    std::fs::write(&path, &res.body).unwrap();
    let restored = Db::open(&path).unwrap();
    let entries = restored.entries().await.unwrap();
    assert_eq!((entries.len(), entries[0].weight.0), (1, 82400));
    assert_eq!(restored.markers().await.unwrap()[0].note, "started cutting");

    // The live database is undisturbed.
    let mut f = entry_form("81.9");
    f[2].1 = "21:00".into();
    assert_eq!(t.create(&pairs(&f)).await.status, 200);
    assert_eq!(t.db.entries().await.unwrap().len(), 2);
}

#[tokio::test]
async fn backup_on_an_empty_database_and_leaves_nothing_behind() {
    let _serial = BACKUPS.lock().await;
    let staging = || {
        std::fs::read_dir(std::env::temp_dir())
            .unwrap()
            .filter_map(Result::ok)
            .filter(|e| {
                e.file_name()
                    .to_string_lossy()
                    .starts_with("weight-tracker-backup")
            })
            .count()
    };
    let before = staging();
    let res = server().ask("/backup.db", ALLOWED).await;
    assert_eq!(res.status, 200);
    assert!(res.body.starts_with(b"SQLite format 3\0"));
    assert_eq!(staging(), before, "left a staging directory behind");
}

// The worker must never come from a stale cache: it is what teaches an
// installed browser the new caching strategy.
#[tokio::test]
async fn service_worker() {
    let res = server().ask("/sw.js", "").await;
    assert_eq!(res.status, 200);
    assert!(
        res.headers["content-type"]
            .to_str()
            .unwrap()
            .starts_with("application/javascript")
    );
    assert_eq!(res.headers["cache-control"], "no-cache");
    let body = res.text();
    assert!(body.contains("addEventListener('fetch'") && body.contains("fetch(event.request)"));
}

#[tokio::test]
async fn healthz() {
    let res = server().ask("/healthz", "").await;
    assert_eq!((res.status.as_u16(), res.text().as_str()), (200, "ok"));
}

// Without a validator browsers applied heuristic freshness and kept the old
// JavaScript after a deploy while the HTML updated.
#[tokio::test]
async fn static_assets_are_revalidatable() {
    let t = server();
    let res = t.ask("/static/chart-app.js", "").await;
    assert_eq!(res.status, 200);
    let etag = res.headers["etag"].to_str().unwrap().to_string();
    assert_eq!(res.headers["cache-control"], "no-cache");
    assert!(!res.body.is_empty());

    let req = |tag: &str| {
        Request::get("/static/chart-app.js")
            .header("If-None-Match", tag)
            .body(Body::empty())
            .unwrap()
    };
    let res = t.send(req(&etag)).await;
    assert_eq!(res.status, 304);
    assert!(res.body.is_empty());
    let res = t.send(req("\"an-earlier-build\"")).await;
    assert_eq!(res.status, 200);
    assert!(!res.body.is_empty());

    // Two assets sharing an ETag would let one satisfy the other's cache.
    let mut seen = std::collections::HashSet::new();
    for path in [
        "/static/chart-app.js",
        "/static/app.js",
        "/static/style.css",
        "/static/sw.js",
    ] {
        let tag = t.ask(path, "").await.headers["etag"]
            .to_str()
            .unwrap()
            .to_string();
        assert!(seen.insert(tag), "{path} shares an ETag");
    }
}

// The metrics label each request by the method and the app route it was
// for, and a request no route matched as unmatched.
#[tokio::test]
async fn metrics_label_each_route() {
    let t = server();
    t.ask("/", ALLOWED).await;
    t.ask("/entries/999/edit", ALLOWED).await;
    t.ask("/nope", ALLOWED).await;
    let body = t.ask("/metrics", "").await.text();
    for series in [
        r#"http_request_duration_seconds_count{method="GET",route="GET /",status="200"}"#,
        r#"http_request_duration_seconds_count{method="GET",route="GET /entries/{id}/edit",status="404"}"#,
        r#"http_request_duration_seconds_count{method="GET",route="unmatched",status="404"}"#,
    ] {
        assert!(body.contains(series), "missing {series}");
    }
}

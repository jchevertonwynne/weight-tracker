//! Every handler. A database failure answers 500 with its message, as the Go
//! version did: this is one person's app, and the message is what they need
//! to see.

use axum::extract::{Multipart, Path, RawQuery, State};
use axum::http::{HeaderValue, Method, StatusCode, header};
use axum::response::{IntoResponse, Response};

use super::Server;
use super::form::Form;
use super::pages::{self, render};
use crate::db::{self, NewEntry};
use crate::{
    chart, clock, goals, history, importer, markers, overnight, summary, timerange, weight,
};

type S = State<Server>;

fn error(status: StatusCode, msg: impl AsRef<str>) -> Response {
    homelab::http::error(status, msg.as_ref())
}

fn internal(err: anyhow::Error) -> Response {
    error(StatusCode::INTERNAL_SERVER_ERROR, format!("{err:#}"))
}

/// A failed delete: a row that is not there is the client's problem (404),
/// anything else is ours (500). Without this, deleting an already-deleted id
/// reported success.
fn delete_error(err: anyhow::Error) -> Response {
    if db::is_not_found(&err) {
        error(StatusCode::NOT_FOUND, format!("{err:#}"))
    } else {
        internal(err)
    }
}

fn parse_id(id: &str) -> Result<i64, Response> {
    id.parse()
        .map_err(|_| error(StatusCode::BAD_REQUEST, "invalid id"))
}

/// An upper sanity bound on any weight stored, well clear of the heaviest
/// recorded human but low enough to catch a mistyped or unit-confused value.
const MAX_WEIGHT_KG: f64 = 1000.0;

/// The weight_kg field shared by the entry and goal forms, as the whole
/// grams the database stores. The inputs carry min and step attributes, but
/// those are client-side only: anything posting directly could otherwise
/// store a negative, zero or non-finite weight, which then flows into the
/// chart, the deltas and the export.
fn parse_weight_g(form: &Form) -> Result<i64, Response> {
    let bad = |msg: String| error(StatusCode::BAD_REQUEST, msg);
    let kg: f64 = form
        .get("weight_kg")
        .parse()
        .map_err(|e| bad(format!("weight_kg is not a number: {e}")))?;
    // "NaN" and "inf" parse, and survive neither a trend line nor a
    // conversion to grams.
    if !kg.is_finite() {
        return Err(bad("weight_kg must be a finite number".into()));
    }
    if kg <= 0.0 || kg > MAX_WEIGHT_KG {
        return Err(bad(format!(
            "weight_kg must be between 0 and {MAX_WEIGHT_KG}, got {kg}"
        )));
    }
    let grams = db::kg_to_grams(kg);
    if grams <= 0 {
        return Err(bad(format!("weight_kg {kg} rounds to zero grams")));
    }
    Ok(grams)
}

/// A date input ("2006-01-02") and a time input ("15:04") joined, as local
/// wall-clock time. Separate inputs rather than datetime-local avoid a clunky
/// two-screen native picker on some mobile browsers.
fn parse_date_time(form: &Form, prefix: &str) -> Option<jiff::Zoned> {
    let combined = format!(
        "{}T{}",
        form.get(&format!("{prefix}_date")),
        form.get(&format!("{prefix}_time"))
    );
    clock::parse_in(&combined, "%Y-%m-%dT%H:%M", &clock::tz())
}

/// A lone date input, for fields with no meaningful time of day.
fn parse_date(form: &Form, field: &str) -> Option<jiff::Zoned> {
    clock::parse_in(form.get(field), "%Y-%m-%d", &clock::tz())
}

pub async fn index(State(s): S, RawQuery(query): RawQuery) -> Response {
    let (entries, goal_list, marker_list) =
        match tokio::try_join!(s.db.list_entries(), s.db.list_goals(), s.db.list_markers()) {
            Ok(lists) => lists,
            Err(err) => return internal(err),
        };
    let now = (s.now)();
    // Each picker's range comes off the URL, so a reload — or a shared link —
    // lands on the same view. The naming ("overnight", "overnight_from",
    // "overnight_until") is the half of the contract the server owns;
    // static/app.js writes the same shape back.
    let q = Form::from_query(query.as_deref());
    let picker = |param: &'static str, def: &'static str| {
        timerange::picker(
            param,
            def,
            q.get(param),
            q.get(&format!("{param}_from")),
            q.get(&format!("{param}_until")),
        )
    };
    let chart_range = picker("chart", "30");
    let history_range = picker("history", "all");
    let overnight_range = picker("overnight", "30");

    // Filtering through the pickers' own windows is what keeps the render
    // agreeing with what each picker claims to show. The chart needs none:
    // it is drawn client-side from /chart with these same inputs.
    let pairs = overnight::windowed_pairs(&entries, &overnight_range.window(&now));
    // The period select has no URL parameter, so it is at its "All" default.
    let rows = history::filter_rows(
        history::build_rows(&entries, &now),
        "",
        &history_range.window(&now),
    );
    render(
        "index",
        &pages::Index {
            now_date: clock::ymd(&now),
            now_time: clock::hm(&now),
            rows,
            goals: goals::build_rows(&goal_list, &now),
            markers: markers::build_rows(&marker_list),
            summary: summary::build(&entries, &goal_list, &now),
            chart_range,
            history_range,
            overnight_range,
            overnight: overnight::build_summary(&pairs),
            pairs,
        },
    )
}

/// Chart data as JSON for the client-side Chart.js instance.
pub async fn chart(State(s): S, RawQuery(query): RawQuery) -> Response {
    let q = Form::from_query(query.as_deref());
    let or = |v: &str, d: &'static str| {
        if v.is_empty() {
            d.to_string()
        } else {
            v.to_string()
        }
    };
    let range = or(q.get("range"), "30");
    let series = or(q.get("series"), "all");
    // The projection widens the axis, so — unlike the raw/trend toggles,
    // which only hide client-side datasets — it changes what the server
    // draws. The client sends the checkbox, "on" when ticked.
    let show_projection = q.get("show-projection") == "on";
    let (entries, goal_list, marker_list) =
        match tokio::try_join!(s.db.list_entries(), s.db.list_goals(), s.db.list_markers()) {
            Ok(lists) => lists,
            Err(err) => return internal(err),
        };
    let data = chart::build(
        &entries,
        &goal_list,
        &marker_list,
        &range,
        &series,
        q.get("from"),
        q.get("until"),
        show_projection,
        &(s.now)(),
    );
    json(&data)
}

/// JSON with the trailing newline json.Encoder wrote.
fn json(v: &impl serde::Serialize) -> Response {
    match serde_json::to_string(v) {
        Ok(mut body) => {
            body.push('\n');
            ([(header::CONTENT_TYPE, "application/json")], body).into_response()
        }
        Err(err) => error(StatusCode::INTERNAL_SERVER_ERROR, err.to_string()),
    }
}

pub async fn summary(State(s): S) -> Response {
    let (entries, goal_list) = match tokio::try_join!(s.db.list_entries(), s.db.list_goals()) {
        Ok(lists) => lists,
        Err(err) => return internal(err),
    };
    let data = summary::build(&entries, &goal_list, &(s.now)());
    render("summary", &pages::Summary { summary: data })
}

/// The Overnight tab's stats and pairs table, filtered by the same
/// range/from/until the other pickers submit. The calculator cards are not
/// part of it: they compare fixed windows that ignore the filter.
pub async fn overnight_tab(State(s): S, RawQuery(query): RawQuery) -> Response {
    let entries = match s.db.list_entries().await {
        Ok(e) => e,
        Err(err) => return internal(err),
    };
    let q = Form::from_query(query.as_deref());
    let range = if q.get("range").is_empty() {
        "30"
    } else {
        q.get("range")
    };
    let window = timerange::resolve(range, q.get("from"), q.get("until"), &(s.now)());
    let pairs = overnight::windowed_pairs(&entries, &window);
    render(
        "overnight-content",
        &pages::OvernightContent {
            overnight: overnight::build_summary(&pairs),
            pairs,
        },
    )
}

/// The "Range by timescale" chart data, as JSON.
pub async fn overnight_windows(State(s): S) -> Response {
    let (entries, goal_list) = match tokio::try_join!(s.db.list_entries(), s.db.list_goals()) {
        Ok(lists) => lists,
        Err(err) => return internal(err),
    };
    json(&overnight::build_window_chart(
        &entries,
        &goal_list,
        &(s.now)(),
    ))
}

/// The history list filtered to whatever period/range/from/until the request
/// carries. Used as GET /entries, and by create, update and delete, so their
/// own response reflects the filtered view rather than reverting to the
/// default range.
async fn render_entries_list(s: &Server, method: &Method, form: &Form) -> Response {
    let entries = match s.db.list_entries().await {
        Ok(e) => e,
        Err(err) => return internal(err),
    };
    let now = (s.now)();
    let range = if form.get("range").is_empty() {
        "all"
    } else {
        form.get("range")
    };
    let window = timerange::resolve(range, form.get("from"), form.get("until"), &now);
    let rows = history::filter_rows(
        history::build_rows(&entries, &now),
        form.get("period"),
        &window,
    );
    let mut res = render("entries-list", &pages::EntriesList { rows });
    // Only a write changes what the summary and charts are drawn from. A
    // plain GET — a filter change, or the periodic refresh that keeps the
    // "Today"/"Yesterday" labels current — would otherwise redraw them all
    // for nothing.
    if method != Method::GET && method != Method::HEAD {
        res.headers_mut()
            .insert("HX-Trigger", HeaderValue::from_static("entries-changed"));
    }
    res
}

pub async fn entries_list(State(s): S, method: Method, form: Form) -> Response {
    render_entries_list(&s, &method, &form).await
}

/// Reads and validates an entry form's weight, timestamp and override.
fn entry_fields(form: &Form) -> Result<(i64, jiff::Zoned, String), Response> {
    let weight_g = parse_weight_g(form)?;
    let recorded_at = parse_date_time(form, "recorded_at")
        .ok_or_else(|| error(StatusCode::BAD_REQUEST, "invalid recorded_at"))?;
    let period_override = form.get("period_override");
    if !weight::valid_period_override(period_override) {
        return Err(error(StatusCode::BAD_REQUEST, "invalid period_override"));
    }
    Ok((weight_g, recorded_at, period_override.to_string()))
}

pub async fn entry_create(State(s): S, method: Method, form: Form) -> Response {
    let (weight_g, recorded_at, period_override) = match entry_fields(&form) {
        Ok(f) => f,
        Err(res) => return res,
    };
    if let Err(err) =
        s.db.create_entry(&recorded_at, weight_g, &period_override, &(s.now)())
            .await
    {
        return internal(err);
    }
    render_entries_list(&s, &method, &form).await
}

pub async fn entry_edit(State(s): S, Path(id): Path<String>) -> Response {
    let id = match parse_id(&id) {
        Ok(id) => id,
        Err(res) => return res,
    };
    let entry = match s.db.get_entry(id).await {
        Ok(e) => e,
        Err(err) => return error(StatusCode::NOT_FOUND, format!("{err:#}")),
    };
    let row = history::build_rows(&[entry], &(s.now)()).remove(0);
    render("row-edit", &pages::EntryRowEdit { row })
}

pub async fn entry_cancel_edit(State(s): S, Path(id): Path<String>) -> Response {
    let id = match parse_id(&id) {
        Ok(id) => id,
        Err(res) => return res,
    };
    let entries = match s.db.list_entries().await {
        Ok(e) => e,
        Err(err) => return internal(err),
    };
    // From the full list, so the row keeps its delta chips.
    match history::build_rows(&entries, &(s.now)())
        .into_iter()
        .find(|r| r.id == id)
    {
        Some(row) => render("row", &pages::EntryRow { row }),
        None => homelab::http::plain_not_found(),
    }
}

pub async fn entry_update(
    State(s): S,
    Path(id): Path<String>,
    method: Method,
    form: Form,
) -> Response {
    let id = match parse_id(&id) {
        Ok(id) => id,
        Err(res) => return res,
    };
    let (weight_g, recorded_at, period_override) = match entry_fields(&form) {
        Ok(f) => f,
        Err(res) => return res,
    };
    if let Err(err) =
        s.db.update_entry(id, &recorded_at, weight_g, &period_override)
            .await
    {
        return internal(err);
    }
    render_entries_list(&s, &method, &form).await
}

pub async fn entry_delete(
    State(s): S,
    Path(id): Path<String>,
    method: Method,
    form: Form,
) -> Response {
    let id = match parse_id(&id) {
        Ok(id) => id,
        Err(res) => return res,
    };
    if let Err(err) = s.db.delete_entry(id).await {
        return delete_error(err);
    }
    render_entries_list(&s, &method, &form).await
}

/// The goals card, firing goals-changed so the chart controls — which draw
/// the goal line — refresh too.
async fn render_goals_list(s: &Server) -> Response {
    let goal_list = match s.db.list_goals().await {
        Ok(g) => g,
        Err(err) => return internal(err),
    };
    let goals = goals::build_rows(&goal_list, &(s.now)());
    let mut res = render("goals-list", &pages::GoalsList { goals });
    res.headers_mut()
        .insert("HX-Trigger", HeaderValue::from_static("goals-changed"));
    res
}

fn goal_fields(form: &Form) -> Result<(i64, jiff::Zoned), Response> {
    let weight_g = parse_weight_g(form)?;
    let from = parse_date(form, "effective_from")
        .ok_or_else(|| error(StatusCode::BAD_REQUEST, "invalid effective_from"))?;
    Ok((weight_g, from))
}

pub async fn goal_create(State(s): S, form: Form) -> Response {
    let (weight_g, from) = match goal_fields(&form) {
        Ok(f) => f,
        Err(res) => return res,
    };
    if let Err(err) = s.db.create_goal(weight_g, &from, &(s.now)()).await {
        return internal(err);
    }
    render_goals_list(&s).await
}

pub async fn goal_edit(State(s): S, Path(id): Path<String>) -> Response {
    let id = match parse_id(&id) {
        Ok(id) => id,
        Err(res) => return res,
    };
    let goal = match s.db.get_goal(id).await {
        Ok(g) => g,
        Err(err) => return error(StatusCode::NOT_FOUND, format!("{err:#}")),
    };
    let goal = goals::build_rows(&[goal], &(s.now)()).remove(0);
    render("goal-row-edit", &pages::GoalRowEdit { goal })
}

pub async fn goal_cancel_edit(State(s): S, Path(id): Path<String>) -> Response {
    let id = match parse_id(&id) {
        Ok(id) => id,
        Err(res) => return res,
    };
    let goal_list = match s.db.list_goals().await {
        Ok(g) => g,
        Err(err) => return internal(err),
    };
    // From the full list, so "Current" is judged against the others.
    match goals::build_rows(&goal_list, &(s.now)())
        .into_iter()
        .find(|g| g.id == id)
    {
        Some(goal) => render("goal-row", &pages::GoalRow { goal }),
        None => homelab::http::plain_not_found(),
    }
}

pub async fn goal_update(State(s): S, Path(id): Path<String>, form: Form) -> Response {
    let id = match parse_id(&id) {
        Ok(id) => id,
        Err(res) => return res,
    };
    let (weight_g, from) = match goal_fields(&form) {
        Ok(f) => f,
        Err(res) => return res,
    };
    if let Err(err) = s.db.update_goal(id, weight_g, &from).await {
        return internal(err);
    }
    render_goals_list(&s).await
}

pub async fn goal_delete(State(s): S, Path(id): Path<String>) -> Response {
    let id = match parse_id(&id) {
        Ok(id) => id,
        Err(res) => return res,
    };
    if let Err(err) = s.db.delete_goal(id).await {
        return delete_error(err);
    }
    render_goals_list(&s).await
}

/// The markers card, firing markers-changed so the chart refreshes too.
async fn render_markers_list(s: &Server) -> Response {
    let marker_list = match s.db.list_markers().await {
        Ok(m) => m,
        Err(err) => return internal(err),
    };
    let markers = markers::build_rows(&marker_list);
    let mut res = render("markers-list", &pages::MarkersList { markers });
    res.headers_mut()
        .insert("HX-Trigger", HeaderValue::from_static("markers-changed"));
    res
}

fn marker_fields(form: &Form) -> Result<(jiff::Zoned, String), Response> {
    let date =
        parse_date(form, "date").ok_or_else(|| error(StatusCode::BAD_REQUEST, "invalid date"))?;
    let note = form.get("note").trim();
    if note.is_empty() {
        return Err(error(StatusCode::BAD_REQUEST, "note is required"));
    }
    Ok((date, note.to_string()))
}

pub async fn marker_create(State(s): S, form: Form) -> Response {
    let (date, note) = match marker_fields(&form) {
        Ok(f) => f,
        Err(res) => return res,
    };
    if let Err(err) = s.db.create_marker(&date, &note, &(s.now)()).await {
        return internal(err);
    }
    render_markers_list(&s).await
}

pub async fn marker_edit(State(s): S, Path(id): Path<String>) -> Response {
    let id = match parse_id(&id) {
        Ok(id) => id,
        Err(res) => return res,
    };
    let marker = match s.db.get_marker(id).await {
        Ok(m) => m,
        Err(err) => return error(StatusCode::NOT_FOUND, format!("{err:#}")),
    };
    let marker = markers::build_rows(&[marker]).remove(0);
    render("marker-row-edit", &pages::MarkerRowEdit { marker })
}

pub async fn marker_cancel_edit(State(s): S, Path(id): Path<String>) -> Response {
    let id = match parse_id(&id) {
        Ok(id) => id,
        Err(res) => return res,
    };
    let marker_list = match s.db.list_markers().await {
        Ok(m) => m,
        Err(err) => return internal(err),
    };
    match markers::build_rows(&marker_list)
        .into_iter()
        .find(|m| m.id == id)
    {
        Some(marker) => render("marker-row", &pages::MarkerRow { marker }),
        None => homelab::http::plain_not_found(),
    }
}

pub async fn marker_update(State(s): S, Path(id): Path<String>, form: Form) -> Response {
    let id = match parse_id(&id) {
        Ok(id) => id,
        Err(res) => return res,
    };
    let (date, note) = match marker_fields(&form) {
        Ok(f) => f,
        Err(res) => return res,
    };
    if let Err(err) = s.db.update_marker(id, &date, &note).await {
        return internal(err);
    }
    render_markers_list(&s).await
}

pub async fn marker_delete(State(s): S, Path(id): Path<String>) -> Response {
    let id = match parse_id(&id) {
        Ok(id) => id,
        Err(res) => return res,
    };
    if let Err(err) = s.db.delete_marker(id).await {
        return delete_error(err);
    }
    render_markers_list(&s).await
}

/// Every weigh-in as CSV, oldest first — how a spreadsheet reads naturally
/// and how the importer expects rows.
///
/// The header stays weight_kg: kilograms are what the app shows and a
/// spreadsheet reader expects, the importer recognises the column, and three
/// decimals of a kilogram are whole grams exactly, so a round trip back
/// through import is lossless.
pub async fn export(State(s): S) -> Response {
    let mut entries = match s.db.list_entries().await {
        Ok(e) => e,
        Err(err) => return internal(err),
    };
    entries.sort_by_key(|e| e.recorded_at.timestamp());

    let mut w = csv::WriterBuilder::new()
        .terminator(csv::Terminator::Any(b'\n'))
        .from_writer(Vec::new());
    let written = std::iter::once(["recorded_at".to_string(), "weight_kg".to_string()])
        .chain(entries.iter().map(|e| {
            [
                clock::rfc3339(&e.recorded_at),
                weight::format_kg_input(e.weight_g),
            ]
        }))
        .try_for_each(|record| w.write_record(&record));
    let body = match written
        .map_err(anyhow::Error::from)
        .and_then(|()| w.into_inner().map_err(|e| anyhow::anyhow!("{e}")))
    {
        Ok(b) => b,
        Err(err) => return internal(err),
    };
    let filename = format!("weight-tracker-export-{}.csv", clock::ymd(&(s.now)()));
    (
        [
            (header::CONTENT_TYPE, "text/csv; charset=utf-8".to_string()),
            (
                header::CONTENT_DISPOSITION,
                format!("attachment; filename=\"{filename}\""),
            ),
        ],
        body,
    )
        .into_response()
}

pub async fn import_form() -> Response {
    render("import", &pages::Import)
}

/// How many unparseable rows the result page lists before summarising.
const MAX_ERRORS_SHOWN: usize = 20;

pub async fn import(State(s): S, mut multipart: Multipart) -> Response {
    let mut file = None;
    let mut unit = String::new();
    loop {
        match multipart.next_field().await {
            Ok(Some(field)) => match field.name() {
                Some("file") if file.is_none() => match field.bytes().await {
                    Ok(b) => file = Some(b),
                    Err(e) => return error(StatusCode::BAD_REQUEST, e.body_text()),
                },
                Some("unit") if unit.is_empty() => match field.text().await {
                    Ok(t) => unit = t,
                    Err(e) => return error(StatusCode::BAD_REQUEST, e.body_text()),
                },
                _ => {}
            },
            Ok(None) => break,
            Err(e) => return error(StatusCode::BAD_REQUEST, e.body_text()),
        }
    }
    let Some(file) = file else {
        return error(StatusCode::BAD_REQUEST, "missing file");
    };
    // Must stay in step with the unit select in templates/import.html and
    // the units the importer accepts — "g" was once offered and refused.
    if !matches!(unit.as_str(), "kg" | "lb" | "g") {
        return error(StatusCode::BAD_REQUEST, "invalid unit");
    }
    let parsed = match importer::parse(&file[..], &unit) {
        Ok(p) => p,
        Err(err) => {
            return error(
                StatusCode::BAD_REQUEST,
                format!("could not parse CSV: {err:#}"),
            );
        }
    };

    let existing = match s.db.existing_recorded_at_set().await {
        Ok(e) => e,
        Err(err) => return internal(err),
    };
    let parsed_rows = parsed.rows.len();
    let rows = parsed
        .rows
        .into_iter()
        .map(|r| NewEntry {
            recorded_at: r.recorded_at,
            weight_g: r.weight_g,
        })
        .collect();
    let inserted = match s.db.bulk_create_entries(rows, existing, &(s.now)()).await {
        Ok((n, _)) => n,
        Err(err) => return internal(err),
    };

    let invalid = parsed.skipped.len();
    let mut errors = parsed.skipped;
    let truncated = errors.len().saturating_sub(MAX_ERRORS_SHOWN);
    errors.truncate(MAX_ERRORS_SHOWN);
    render(
        "import-result",
        &pages::ImportResult {
            inserted,
            duplicates: parsed_rows - inserted,
            invalid,
            errors,
            truncated,
        },
    )
}

pub async fn delete_all(State(s): S) -> Response {
    if let Err(err) = s.db.delete_all_data().await {
        return internal(err);
    }
    // http.Redirect's 303 to a POST: the Location and nothing else.
    (StatusCode::SEE_OTHER, [(header::LOCATION, "/")]).into_response()
}

/// A consistent snapshot of the whole database as a download. Unlike the
/// CSV — weigh-ins only — this is the SQLite file itself, so it restores
/// goals, markers, period overrides and ids too: copy it into place and the
/// app picks up where the snapshot was taken.
///
/// Built with VACUUM INTO rather than by copying the file off disk, which
/// would miss or tear what is still in the write-ahead log. It is staged in
/// the temp directory — the Deployment mounts a writable /tmp for it — and
/// read back into memory before the directory goes: the database is a few
/// MB, and the staging copy is then gone however the download ends.
pub async fn backup(State(s): S) -> Response {
    let dir = match tempfile::Builder::new()
        .prefix("weight-tracker-backup")
        .tempdir()
    {
        Ok(d) => d,
        Err(e) => {
            return error(
                StatusCode::INTERNAL_SERVER_ERROR,
                format!("could not create a staging directory: {e}"),
            );
        }
    };
    // VACUUM INTO refuses to overwrite, and the directory is new and empty.
    let snapshot = dir.path().join("snapshot.db");
    if let Err(err) = s.db.backup_to(&snapshot).await {
        return error(
            StatusCode::INTERNAL_SERVER_ERROR,
            format!("could not create the snapshot: {err:#}"),
        );
    }
    let body = match tokio::fs::read(&snapshot).await {
        Ok(b) => b,
        Err(e) => return error(StatusCode::INTERNAL_SERVER_ERROR, e.to_string()),
    };
    drop(dir);

    let filename = format!("weight-tracker-backup-{}.db", clock::ymd(&(s.now)()));
    (
        [
            (header::CONTENT_TYPE, "application/vnd.sqlite3".to_string()),
            (
                header::CONTENT_DISPOSITION,
                format!("attachment; filename=\"{filename}\""),
            ),
        ],
        body,
    )
        .into_response()
}

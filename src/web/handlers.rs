//! Every route's handler.

use axum::extract::{Multipart, RawQuery, State};
use axum::http::{HeaderValue, Method, StatusCode, header};
use axum::response::{IntoResponse, Response};
use serde::Serialize;

use super::Server;
use super::error::{Error, Id, Result, bad_request, found};
use super::form::Fields;
use super::pages::{self, html};
use crate::clock::Show as _;
use crate::importer::{self, Unit};
use crate::model::Period;
use crate::timeline::Timeline;
use crate::timerange::{Picker, Range};
use crate::{chart, goals, history, overnight, summary};

type S = State<Server>;

pub async fn index(State(s): S, RawQuery(query): RawQuery) -> Result {
    let (entries, goal_list, markers) =
        tokio::try_join!(s.db.entries(), s.db.goals(), s.db.markers())?;
    let now = s.now();
    let timeline = Timeline::new(&entries);
    // Each picker's range is in the URL as `<param>`, `<param>_from` and
    // `<param>_until` — what static/app.js writes when a range is applied —
    // so a reload or a shared link shows the same view.
    let q = Fields::from_query(query.as_deref());
    let picker = |param: &'static str, default: &'static str| {
        let at = |suffix: &str| q.get(&format!("{param}{suffix}")).to_string();
        Picker::from_query(param, default, &at(""), &at("_from"), &at("_until"))
    };
    let (chart_range, history_range, overnight_range) = (
        picker("chart", "30"),
        picker("history", "all"),
        picker("overnight", "30"),
    );

    // Through each picker's own window, so the content is what the button
    // says. The chart is drawn by the browser from /chart, with the same
    // range.
    let nights = overnight::nights(&timeline, &overnight_range.window(&now));
    let rows = history::filter(
        history::rows(&timeline, &now),
        None,
        &history_range.window(&now),
    );
    html(&pages::Index {
        now_date: now.ymd(),
        now_time: now.hm(),
        rows,
        goals: goals::rows(&goal_list, &now),
        summary: summary::summary(&entries, &goal_list, &now),
        markers,
        chart_range,
        history_range,
        overnight_range,
        stats: overnight::stats(&nights),
        nights,
    })
}

pub async fn chart(State(s): S, RawQuery(query): RawQuery) -> Result {
    let q = Fields::from_query(query.as_deref());
    let (entries, goal_list, markers) =
        tokio::try_join!(s.db.entries(), s.db.goals(), s.db.markers())?;
    let range = Range::parse(or(q.get("range"), "30"), q.get("from"), q.get("until"));
    let data = chart::chart(
        &Timeline::new(&entries),
        &goal_list,
        &markers,
        chart::Series::parse(q.get("series")),
        &range,
        // The projection widens the axis, so unlike the raw and trend
        // toggles, which only hide datasets, it changes what is drawn.
        q.get("show-projection") == "on",
        &s.now(),
    );
    json(&data)
}

fn or<'a>(value: &'a str, default: &'a str) -> &'a str {
    if value.is_empty() { default } else { value }
}

fn json(data: &impl Serialize) -> Result {
    Ok((
        [(header::CONTENT_TYPE, "application/json")],
        serde_json::to_string(data)?,
    )
        .into_response())
}

pub async fn summary(State(s): S) -> Result {
    let (entries, goal_list) = tokio::try_join!(s.db.entries(), s.db.goals())?;
    html(&pages::SummaryCard {
        summary: summary::summary(&entries, &goal_list, &s.now()),
    })
}

/// The Overnight tab's figures and table, for its range filter. The
/// calculator above them compares fixed timescales and is left alone.
pub async fn overnight(State(s): S, RawQuery(query): RawQuery) -> Result {
    let q = Fields::from_query(query.as_deref());
    let range = Range::parse(or(q.get("range"), "30"), q.get("from"), q.get("until"));
    let nights = overnight::nights(
        &Timeline::new(&s.db.entries().await?),
        &range.window(&s.now()),
    );
    html(&pages::OvernightContent {
        stats: overnight::stats(&nights),
        nights,
    })
}

pub async fn overnight_timescales(State(s): S) -> Result {
    let (entries, goal_list) = tokio::try_join!(s.db.entries(), s.db.goals())?;
    json(&overnight::timescales(
        &Timeline::new(&entries),
        &goal_list,
        &s.now(),
    ))
}

/// The history list, filtered as the request's period and range say — which
/// is what the history filter sends, and what a change made with the list
/// open includes, so the list comes back the way it was being viewed.
async fn history(s: &Server, method: &Method, fields: &Fields) -> Result {
    let now = s.now();
    let range = Range::parse(
        or(fields.get("range"), "all"),
        fields.get("from"),
        fields.get("until"),
    );
    let period = fields.get("period").parse::<Period>().ok();
    let rows = history::rows(&Timeline::new(&s.db.entries().await?), &now);
    let mut res = html(&pages::EntriesList {
        rows: history::filter(rows, period, &range.window(&now)),
    })?;
    // Only a change to the data redraws the summary and the charts; a plain
    // GET — a filter change, or the refresh that keeps "Today" current —
    // changes nothing they show.
    if *method != Method::GET {
        res.headers_mut()
            .insert("HX-Trigger", HeaderValue::from_static("entries-changed"));
    }
    Ok(res)
}

pub async fn entries(State(s): S, method: Method, fields: Fields) -> Result {
    history(&s, &method, &fields).await
}

pub async fn add_entry(State(s): S, method: Method, fields: Fields) -> Result {
    s.db.add_entry(fields.entry()?, &s.now()).await?;
    history(&s, &method, &fields).await
}

pub async fn update_entry(State(s): S, Id(id): Id, method: Method, fields: Fields) -> Result {
    let entry = fields.entry()?;
    found(s.db.update_entry(id, entry).await?)?;
    history(&s, &method, &fields).await
}

pub async fn delete_entry(State(s): S, Id(id): Id, method: Method, fields: Fields) -> Result {
    found(s.db.delete_entry(id).await?)?;
    history(&s, &method, &fields).await
}

/// One history row, with its change worked out against the rest.
async fn history_row(s: &Server, id: i64) -> Result<history::Row> {
    let timeline = Timeline::new(&s.db.entries().await?);
    history::rows(&timeline, &s.now())
        .into_iter()
        .find(|r| r.id == id)
        .ok_or(Error::NotFound)
}

pub async fn edit_entry(State(s): S, Id(id): Id) -> Result {
    html(&pages::EntryRowEdit {
        row: history_row(&s, id).await?,
    })
}

pub async fn show_entry(State(s): S, Id(id): Id) -> Result {
    html(&pages::EntryRow {
        row: history_row(&s, id).await?,
    })
}

/// The goals card. goals-changed redraws the chart, which draws the goal.
async fn goals_list(s: &Server) -> Result {
    let goals = goals::rows(&s.db.goals().await?, &s.now());
    let mut res = html(&pages::GoalsList { goals })?;
    res.headers_mut()
        .insert("HX-Trigger", HeaderValue::from_static("goals-changed"));
    Ok(res)
}

pub async fn add_goal(State(s): S, fields: Fields) -> Result {
    let (weight, from) = (fields.weight()?, fields.date("effective_from")?);
    s.db.add_goal(weight, from, &s.now()).await?;
    goals_list(&s).await
}

pub async fn update_goal(State(s): S, Id(id): Id, fields: Fields) -> Result {
    let (weight, from) = (fields.weight()?, fields.date("effective_from")?);
    found(s.db.update_goal(id, weight, from).await?)?;
    goals_list(&s).await
}

pub async fn delete_goal(State(s): S, Id(id): Id) -> Result {
    found(s.db.delete_goal(id).await?)?;
    goals_list(&s).await
}

/// One goal row, marked current against the others.
async fn goal_row(s: &Server, id: i64) -> Result<goals::Row> {
    goals::rows(&s.db.goals().await?, &s.now())
        .into_iter()
        .find(|r| r.goal.id == id)
        .ok_or(Error::NotFound)
}

pub async fn edit_goal(State(s): S, Id(id): Id) -> Result {
    html(&pages::GoalRowEdit {
        row: goal_row(&s, id).await?,
    })
}

pub async fn show_goal(State(s): S, Id(id): Id) -> Result {
    html(&pages::GoalRow {
        row: goal_row(&s, id).await?,
    })
}

/// The markers card. markers-changed redraws the chart.
async fn markers_list(s: &Server) -> Result {
    let markers = s.db.markers().await?;
    let mut res = html(&pages::MarkersList { markers })?;
    res.headers_mut()
        .insert("HX-Trigger", HeaderValue::from_static("markers-changed"));
    Ok(res)
}

fn marker_fields(fields: &Fields) -> Result<(jiff::Zoned, String)> {
    let date = fields.date("date")?;
    match fields.get("note").trim() {
        "" => Err(bad_request("note is required")),
        note => Ok((date, note.to_string())),
    }
}

pub async fn add_marker(State(s): S, fields: Fields) -> Result {
    let (date, note) = marker_fields(&fields)?;
    s.db.add_marker(date, note, &s.now()).await?;
    markers_list(&s).await
}

pub async fn update_marker(State(s): S, Id(id): Id, fields: Fields) -> Result {
    let (date, note) = marker_fields(&fields)?;
    found(s.db.update_marker(id, date, note).await?)?;
    markers_list(&s).await
}

pub async fn delete_marker(State(s): S, Id(id): Id) -> Result {
    found(s.db.delete_marker(id).await?)?;
    markers_list(&s).await
}

pub async fn edit_marker(State(s): S, Id(id): Id) -> Result {
    let marker = s.db.marker(id).await?.ok_or(Error::NotFound)?;
    html(&pages::MarkerRowEdit { marker })
}

pub async fn show_marker(State(s): S, Id(id): Id) -> Result {
    let marker = s.db.marker(id).await?.ok_or(Error::NotFound)?;
    html(&pages::MarkerRow { marker })
}

/// Every weigh-in as CSV, oldest first. The weight column stays in
/// kilograms — what the app shows and a spreadsheet expects — and three
/// decimals of a kilogram are whole grams, so a round trip through import
/// loses nothing.
pub async fn export(State(s): S) -> Result {
    let timeline = Timeline::new(&s.db.entries().await?);
    let mut csv = csv::WriterBuilder::new()
        .terminator(csv::Terminator::Any(b'\n'))
        .from_writer(Vec::new());
    csv.write_record(["recorded_at", "weight_kg"])?;
    for e in timeline.entries() {
        csv.write_record([e.recorded_at.rfc3339(), e.weight.input()])?;
    }
    let body = csv.into_inner().map_err(|e| e.into_error())?;
    Ok(download(
        "text/csv; charset=utf-8",
        &format!("weight-tracker-export-{}.csv", s.now().ymd()),
        body,
    ))
}

fn download(content_type: &'static str, filename: &str, body: Vec<u8>) -> Response {
    let disposition = format!("attachment; filename=\"{filename}\"");
    (
        [
            (header::CONTENT_TYPE, content_type.to_string()),
            (header::CONTENT_DISPOSITION, disposition),
        ],
        body,
    )
        .into_response()
}

pub async fn import_form() -> Result {
    html(&pages::ImportForm)
}

/// How many unreadable rows the result lists before saying how many more.
const SHOWN_SKIPS: usize = 20;

pub async fn import(State(s): S, mut form: Multipart) -> Result {
    let (mut file, mut unit) = (None, None);
    while let Some(field) = form
        .next_field()
        .await
        .map_err(|e| bad_request(e.body_text()))?
    {
        match field.name() {
            Some("file") => {
                file = Some(
                    field
                        .bytes()
                        .await
                        .map_err(|e| bad_request(e.body_text()))?,
                )
            }
            Some("unit") => {
                unit = Some(field.text().await.map_err(|e| bad_request(e.body_text()))?)
            }
            _ => {}
        }
    }
    let file = file.ok_or_else(|| bad_request("missing file"))?;
    let unit: Unit = unit
        .unwrap_or_default()
        .parse()
        .map_err(|_| bad_request("invalid unit"))?;
    let import = importer::parse(&file[..], unit)
        .map_err(|e| bad_request(format!("could not parse CSV: {e:#}")))?;

    let read = import.readings.len();
    let added = s.db.import(import.readings, &s.now()).await?;
    let unreadable = import.skipped.len();
    let mut shown = import.skipped;
    shown.truncate(SHOWN_SKIPS);
    html(&pages::ImportResult {
        added,
        duplicates: read - added,
        unreadable,
        not_shown: unreadable - shown.len(),
        shown,
    })
}

pub async fn delete_everything(State(s): S) -> Result {
    s.db.delete_everything().await?;
    Ok((StatusCode::SEE_OTHER, [(header::LOCATION, "/")]).into_response())
}

/// A snapshot of the whole database to download: goals, markers, overrides
/// and ids included, which the CSV cannot carry. Copied into place, the app
/// carries on from it.
///
/// It is written to the temp directory (the Deployment mounts a writable
/// /tmp for it) and read back before the directory goes: the database is a
/// few MB, and the copy is then gone however the download ends.
pub async fn backup(State(s): S) -> Result {
    let dir = tempfile::Builder::new()
        .prefix("weight-tracker-backup")
        .tempdir()?;
    let path = dir.path().join("snapshot.db");
    s.db.backup_to(&path).await?;
    let body = tokio::fs::read(&path).await?;
    Ok(download(
        "application/vnd.sqlite3",
        &format!("weight-tracker-backup-{}.db", s.now().ymd()),
        body,
    ))
}

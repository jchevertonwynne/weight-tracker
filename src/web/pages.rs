//! The pages and fragments, each a template compiled into the binary with
//! the data it is given — so a template that reads something the handler
//! does not supply is a build failure rather than a 500 on the one page
//! someone needed.
//!
//! A fragment that htmx swaps on its own and that the full page also embeds
//! (a row, the goals list) is one template file reading one named variable:
//! the struct here supplies it alone, the index page supplies it in a loop or
//! as a field.

use askama::Template;
use axum::http::{StatusCode, header};
use axum::response::{IntoResponse, Response};

use crate::importer::SkippedRow;
use crate::overnight::{self, Pair};
use crate::summary::WeeklySummary;
use crate::timerange::PickerConfig;
use crate::{goals, history, markers};

#[derive(Template)]
#[template(path = "index.html")]
pub struct Index {
    pub now_date: String,
    pub now_time: String,
    pub rows: Vec<history::Row>,
    pub goals: Vec<goals::Row>,
    pub markers: Vec<markers::Row>,
    pub summary: WeeklySummary,
    pub chart_range: PickerConfig,
    pub history_range: PickerConfig,
    pub overnight_range: PickerConfig,
    pub overnight: overnight::Summary,
    pub pairs: Vec<Pair>,
}

#[derive(Template)]
#[template(path = "entries_list.html")]
pub struct EntriesList {
    pub rows: Vec<history::Row>,
}

#[derive(Template)]
#[template(path = "row.html")]
pub struct EntryRow {
    pub row: history::Row,
}

#[derive(Template)]
#[template(path = "row_edit.html")]
pub struct EntryRowEdit {
    pub row: history::Row,
}

#[derive(Template)]
#[template(path = "goals_list.html")]
pub struct GoalsList {
    pub goals: Vec<goals::Row>,
}

#[derive(Template)]
#[template(path = "goal_row.html")]
pub struct GoalRow {
    pub goal: goals::Row,
}

#[derive(Template)]
#[template(path = "goal_row_edit.html")]
pub struct GoalRowEdit {
    pub goal: goals::Row,
}

#[derive(Template)]
#[template(path = "markers_list.html")]
pub struct MarkersList {
    pub markers: Vec<markers::Row>,
}

#[derive(Template)]
#[template(path = "marker_row.html")]
pub struct MarkerRow {
    pub marker: markers::Row,
}

#[derive(Template)]
#[template(path = "marker_row_edit.html")]
pub struct MarkerRowEdit {
    pub marker: markers::Row,
}

#[derive(Template)]
#[template(path = "summary.html")]
pub struct Summary {
    pub summary: WeeklySummary,
}

#[derive(Template)]
#[template(path = "overnight_content.html")]
pub struct OvernightContent {
    pub overnight: overnight::Summary,
    pub pairs: Vec<Pair>,
}

#[derive(Template)]
#[template(path = "import.html")]
pub struct Import;

#[derive(Template)]
#[template(path = "import_result.html")]
pub struct ImportResult {
    pub inserted: usize,
    pub duplicates: usize,
    pub invalid: usize,
    pub errors: Vec<SkippedRow>,
    pub truncated: usize,
}

/// Writes a page, rendered in full before anything is sent: a template that
/// failed halfway used to leave a committed 200 and a truncated page the
/// client could not tell was broken. Now it is the whole page or a clean
/// 500.
pub fn render(name: &'static str, t: &impl Template) -> Response {
    match t.render() {
        Ok(body) => ([(header::CONTENT_TYPE, "text/html; charset=utf-8")], body).into_response(),
        Err(err) => {
            tracing::error!(error = %err, "render {name}");
            homelab::http::error(StatusCode::INTERNAL_SERVER_ERROR, "internal server error")
        }
    }
}

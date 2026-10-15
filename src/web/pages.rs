//! The pages and the fragments htmx swaps in, each a template compiled in
//! with the data it is given.
//!
//! A fragment the full page also contains (a row, the goals list) is one
//! template file reading one named variable, supplied by the fragment's own
//! struct here or by the page as it loops.

use askama::Template;
use axum::http::header;
use axum::response::IntoResponse;

// Used by the templates.
#[allow(unused_imports)]
use crate::clock::Show as _;
use crate::goals;
use crate::history;
use crate::importer::Skipped;
use crate::model::Marker;
use crate::overnight::{Night, Stats};
#[allow(unused_imports)]
use crate::summary::{Eta, Outlook, Summary};
use crate::timerange::Picker;

use super::error::Result;

#[derive(Template)]
#[template(path = "index.html")]
pub struct Index {
    pub now_date: String,
    pub now_time: String,
    pub rows: Vec<history::Row>,
    pub goals: Vec<goals::Row>,
    pub markers: Vec<Marker>,
    pub summary: Summary,
    pub chart_range: Picker,
    pub history_range: Picker,
    pub overnight_range: Picker,
    pub stats: Option<Stats>,
    pub nights: Vec<Night>,
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
    pub row: goals::Row,
}

#[derive(Template)]
#[template(path = "goal_row_edit.html")]
pub struct GoalRowEdit {
    pub row: goals::Row,
}

#[derive(Template)]
#[template(path = "markers_list.html")]
pub struct MarkersList {
    pub markers: Vec<Marker>,
}

#[derive(Template)]
#[template(path = "marker_row.html")]
pub struct MarkerRow {
    pub marker: Marker,
}

#[derive(Template)]
#[template(path = "marker_row_edit.html")]
pub struct MarkerRowEdit {
    pub marker: Marker,
}

#[derive(Template)]
#[template(path = "summary.html")]
pub struct SummaryCard {
    pub summary: Summary,
}

#[derive(Template)]
#[template(path = "overnight_content.html")]
pub struct OvernightContent {
    pub stats: Option<Stats>,
    pub nights: Vec<Night>,
}

#[derive(Template)]
#[template(path = "import.html")]
pub struct ImportForm;

#[derive(Template)]
#[template(path = "import_result.html")]
pub struct ImportResult {
    pub added: usize,
    pub duplicates: usize,
    pub unreadable: usize,
    pub shown: Vec<Skipped>,
    pub not_shown: usize,
}

/// Renders a page in full before sending any of it, so a template that fails
/// halfway is a clean 500 rather than a 200 with half a page.
pub fn html(page: &impl Template) -> Result {
    let body = page.render()?;
    Ok(([(header::CONTENT_TYPE, "text/html; charset=utf-8")], body).into_response())
}

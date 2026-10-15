//! The display-ready form of a marker, and the points the chart's marker
//! lines are drawn from.

use jiff::Zoned;
use serde::Serialize;

use crate::clock;
use crate::db::Marker;

#[derive(Clone, Debug)]
pub struct Row {
    pub id: i64,
    pub date_label: String,
    pub date_input: String,
    pub note: String,
}

/// Assumes `markers` is newest first.
pub fn build_rows(markers: &[Marker]) -> Vec<Row> {
    markers
        .iter()
        .map(|m| Row {
            id: m.id,
            date_label: clock::date_label(&m.date),
            date_input: clock::ymd(&m.date),
            note: m.note.clone(),
        })
        .collect()
}

/// A marker clipped to the chart's visible range. The id lets the client
/// pick a colour per marker that stays put across range changes.
#[derive(Clone, Debug, Serialize)]
pub struct Point {
    pub id: i64,
    pub x: i64,
    pub date: String,
    pub note: String,
}

/// Markers within the calendar days spanned by [from, until]. Markers are
/// date-only, so whole days are compared rather than the exact instants of
/// the first and last visible entries — a marker on the same day as the
/// first entry sits earlier in that day than the entry itself.
pub fn visible(markers: &[Marker], from: &Zoned, until: &Zoned) -> Vec<Point> {
    let start = clock::start_of_day(from).timestamp();
    let end = clock::end_of_day(until).timestamp();
    markers
        .iter()
        .filter(|m| m.date.timestamp() >= start && m.date.timestamp() <= end)
        .map(|m| Point {
            id: m.id,
            x: clock::ms(&m.date),
            date: clock::date_label(&m.date),
            note: m.note.clone(),
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, marker};

    #[test]
    fn build_rows_cases() {
        let rows = build_rows(&[marker(1, "2026-08-10", "started cutting")]);
        assert_eq!(rows[0].date_label, "Aug 10, 2026");
        assert_eq!(rows[0].date_input, "2026-08-10");
        assert_eq!(rows[0].note, "started cutting");
    }

    #[test]
    fn visible_cases() {
        let markers = [
            marker(1, "2026-08-01", "before"),
            marker(2, "2026-08-10", "inside"),
            marker(3, "2026-08-20", "after"),
        ];
        let got = visible(&markers, &at("2026-08-05 07:00"), &at("2026-08-15 21:00"));
        assert_eq!(got.len(), 1);
        assert_eq!(
            (got[0].note.as_str(), got[0].id, got[0].date.as_str()),
            ("inside", 2, "Aug 10, 2026")
        );
        // A marker sharing the first or last entry's day is kept.
        assert_eq!(
            visible(&markers, &at("2026-08-10 07:30"), &at("2026-08-15 21:00")).len(),
            1
        );
        assert_eq!(
            visible(&markers, &at("2026-08-05 07:00"), &at("2026-08-10 07:30")).len(),
            1
        );
        assert!(visible(&markers, &at("2026-09-01 07:00"), &at("2026-09-30 21:00")).is_empty());
        assert!(visible(&[], &at("2026-08-05 07:00"), &at("2026-08-15 21:00")).is_empty());
    }
}

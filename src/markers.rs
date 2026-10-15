//! Markers on the chart.

use jiff::Zoned;
use serde::Serialize;

use crate::clock::Show as _;
use crate::model::Marker;

/// A marker as chart-app.js draws it. The id gives each marker a colour
/// that stays put as the range changes.
#[derive(Clone, Debug, Serialize)]
pub struct Point {
    pub id: i64,
    pub x: i64,
    pub date: String,
    pub note: String,
}

/// The markers on the days [from, until] touch. A marker is a date, so whole
/// days are compared: a marker on the first visible reading's day sits at
/// midnight, before the reading, and still belongs on the chart.
pub fn visible(markers: &[Marker], from: &Zoned, until: &Zoned) -> Vec<Point> {
    let (Ok(start), Ok(end)) = (from.start_of_day(), until.end_of_day()) else {
        return Vec::new();
    };
    markers
        .iter()
        .filter(|m| m.date >= start && m.date <= end)
        .map(|m| Point {
            id: m.id,
            x: m.date.ms(),
            date: m.date.date_label(),
            note: m.note.clone(),
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, marker};

    #[test]
    fn visible_compares_whole_days() {
        let markers = [
            marker(1, "2026-08-01", "before"),
            marker(2, "2026-08-10", "inside"),
            marker(3, "2026-08-20", "after"),
        ];
        let notes = |from, until| -> Vec<String> {
            visible(&markers, &at(from), &at(until))
                .into_iter()
                .map(|p| p.note)
                .collect()
        };
        assert_eq!(notes("2026-08-05 07:00", "2026-08-15 21:00"), ["inside"]);
        // On the same day as the first or last reading.
        assert_eq!(notes("2026-08-10 07:30", "2026-08-15 21:00"), ["inside"]);
        assert_eq!(notes("2026-08-05 07:00", "2026-08-10 07:30"), ["inside"]);
        assert!(notes("2026-09-01 07:00", "2026-09-30 21:00").is_empty());
        let p = &visible(&markers, &at("2026-08-05 07:00"), &at("2026-08-15 21:00"))[0];
        assert_eq!((p.id, p.date.as_str()), (2, "Aug 10, 2026"));
    }
}

//! The display-ready form of the weigh-in list — the History tab's table —
//! with each row's overnight/daily delta chips and the period/date-range
//! filter applied to it.

use jiff::Zoned;

use crate::db::Entry;
use crate::timerange::Window;
use crate::{clock, weight};

#[derive(Clone, Debug)]
pub struct Row {
    pub id: i64,
    /// Kept for [`filter_rows`]; not rendered directly.
    pub recorded_at: Zoned,
    pub recorded_at_label: String,
    /// For the edit form's date and time inputs.
    pub recorded_at_date: String,
    pub recorded_at_time: String,
    pub period: String,
    pub period_label: &'static str,
    /// "", "morning" or "evening", for the edit form's select.
    pub period_override: String,
    pub weight_kg_raw: String,
    pub weight_kg_str: String,
    /// Set on morning entries: against the prior evening.
    pub overnight_delta: String,
    pub overnight_loss: bool,
    /// Set on evening entries: against that same day's morning.
    pub daily_delta: String,
    pub daily_gain: bool,
}

/// Whether `a` and `b` fall on the same calendar day, both read in `now`'s
/// zone, so a weigh-in stored in another zone is judged against the day the
/// user is in.
fn same_day(a: &Zoned, b: &Zoned, now: &Zoned) -> bool {
    a.with_time_zone(now.time_zone().clone()).date()
        == b.with_time_zone(now.time_zone().clone()).date()
}

/// A weigh-in's timestamp relative to now: today's and yesterday's rows say
/// "Today"/"Yesterday" in place of the date, and anything older keeps the
/// full date. The most-looked-at rows stay scannable, and whether today has
/// been tracked is obvious at a glance, without dropping the year older
/// readings need.
fn recorded_at_label(recorded_at: &Zoned, now: &Zoned) -> String {
    if same_day(recorded_at, now, now) {
        format!("Today {}", clock::hm(recorded_at))
    } else if same_day(recorded_at, &clock::add_date(now, 0, 0, -1), now) {
        format!("Yesterday {}", clock::hm(recorded_at))
    } else {
        clock::datetime_label(recorded_at)
    }
}

/// Entries as display-ready rows, with delta chips and now-relative labels.
pub fn build_rows(entries: &[Entry], now: &Zoned) -> Vec<Row> {
    let (_, overnight, daily) = weight::chronological_with_deltas(entries);
    entries
        .iter()
        .map(|e| {
            let period = weight::entry_period(e).to_string();
            let mut row = Row {
                id: e.id,
                recorded_at: e.recorded_at.clone(),
                recorded_at_label: recorded_at_label(&e.recorded_at, now),
                recorded_at_date: clock::ymd(&e.recorded_at),
                recorded_at_time: clock::hm(&e.recorded_at),
                period_label: if period == "morning" {
                    "Morning"
                } else {
                    "Evening"
                },
                period,
                period_override: e.period_override.clone(),
                weight_kg_raw: weight::format_kg_input(e.weight_g),
                weight_kg_str: weight::format_kg(e.weight_g),
                overnight_delta: String::new(),
                overnight_loss: false,
                daily_delta: String::new(),
                daily_gain: false,
            };
            if let Some(&d) = overnight.get(&e.id) {
                row.overnight_loss = d < 0;
                row.overnight_delta = weight::format_kg_delta(d);
            }
            if let Some(&d) = daily.get(&e.id) {
                row.daily_gain = d > 0;
                row.daily_delta = weight::format_kg_delta(d);
            }
            row
        })
        .collect()
}

/// Rows whose period matches (""/"all" for any) and whose time falls in the
/// window.
///
/// This filters [`build_rows`]' output rather than the entries, since the
/// deltas need the full chronology: filtering to mornings first would starve
/// the overnight delta of the evenings it is computed against.
pub fn filter_rows(rows: Vec<Row>, period: &str, window: &Window) -> Vec<Row> {
    rows.into_iter()
        .filter(|r| period.is_empty() || period == "all" || r.period == period)
        .filter(|r| window.contains(&r.recorded_at))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, entry};

    #[test]
    fn build_rows_cases() {
        let entries = [
            // Newest first, as list_entries returns them.
            entry(2, at("2026-08-16 07:15"), 82.04, ""),
            entry(1, at("2026-08-15 21:00"), 83.1, ""),
        ];
        let rows = build_rows(&entries, &at("2026-09-01 12:00"));
        assert_eq!(rows.len(), 2);
        let m = &rows[0];
        assert_eq!((m.period.as_str(), m.period_label), ("morning", "Morning"));
        assert_eq!(m.weight_kg_str, "82.0", "display rounds to 1dp");
        assert_eq!(m.weight_kg_raw, "82.04", "edit form keeps full precision");
        assert_eq!(
            (m.recorded_at_date.as_str(), m.recorded_at_time.as_str()),
            ("2026-08-16", "07:15")
        );
        assert_eq!(m.recorded_at_label, "Aug 16, 2026 07:15");
        assert_eq!(m.overnight_delta, "-1.1 kg");
        assert!(m.overnight_loss);
        assert_eq!(rows[1].period_label, "Evening");
        assert_eq!(rows[1].overnight_delta, "");
    }

    #[test]
    fn build_rows_formats_gains_with_a_sign() {
        let entries = [
            entry(2, at("2026-08-15 21:00"), 83.1, ""),
            entry(1, at("2026-08-15 07:30"), 82.4, ""),
        ];
        let rows = build_rows(&entries, &at("2026-09-01 12:00"));
        assert_eq!(rows[0].daily_delta, "+0.7 kg");
        assert!(rows[0].daily_gain);
    }

    #[test]
    fn build_rows_labels_dates_relative_to_now() {
        let entries = [
            entry(3, at("2026-09-20 08:12"), 81.0, ""),
            // Hours away, but the day before.
            entry(2, at("2026-09-19 23:50"), 81.4, ""),
            entry(1, at("2026-08-15 07:30"), 83.1, ""),
        ];
        let rows = build_rows(&entries, &at("2026-09-20 21:30"));
        assert_eq!(rows[0].recorded_at_label, "Today 08:12");
        assert_eq!(rows[1].recorded_at_label, "Yesterday 23:50");
        assert_eq!(rows[2].recorded_at_label, "Aug 15, 2026 07:30");
    }
}

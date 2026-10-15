//! The History tab: every weigh-in, with its change from the reading it pairs
//! with, filtered by period and date range.

use jiff::Zoned;

use crate::clock::Show as _;
use crate::model::{Grams, Period};
use crate::timeline::Timeline;
use crate::timerange::Window;

#[derive(Clone, Debug)]
pub struct Row {
    pub id: i64,
    pub recorded_at: Zoned,
    pub period: Period,
    pub period_override: Option<Period>,
    pub weight: Grams,
    /// "Today 07:15", "Yesterday 21:40", or the full date.
    pub when: String,
    /// On a morning: the change from the evening before.
    pub overnight: Option<Grams>,
    /// On an evening: the change from that day's morning.
    pub daily: Option<Grams>,
}

impl Row {
    /// The period select's value: "" for "follow the clock".
    pub fn override_value(&self) -> &'static str {
        self.period_override.map_or("", Period::as_str)
    }
}

/// Every entry as a row, newest first. Rows are built from the whole
/// timeline and only then filtered, so a row keeps its change even when the
/// reading it pairs with is filtered out.
pub fn rows(timeline: &Timeline, now: &Zoned) -> Vec<Row> {
    timeline
        .entries()
        .iter()
        .rev()
        .map(|e| Row {
            id: e.id,
            recorded_at: e.recorded_at.clone(),
            period: e.period(),
            period_override: e.period_override,
            weight: e.weight,
            when: when(&e.recorded_at, now),
            overnight: timeline.overnight(e.id),
            daily: timeline.daily(e.id),
        })
        .collect()
}

pub fn filter(rows: Vec<Row>, period: Option<Period>, window: &Window) -> Vec<Row> {
    rows.into_iter()
        .filter(|r| period.is_none_or(|p| r.period == p))
        .filter(|r| window.contains(&r.recorded_at))
        .collect()
}

/// Today's and yesterday's readings say so in place of the date — the rows
/// looked at most, and the quickest way to see whether today is logged.
/// Older ones keep the year.
fn when(t: &Zoned, now: &Zoned) -> String {
    let day = t.with_time_zone(now.time_zone().clone()).date();
    if day == now.date() {
        format!("Today {}", t.hm())
    } else if now.date().yesterday().ok() == Some(day) {
        format!("Yesterday {}", t.hm())
    } else {
        t.datetime_label()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, entry};

    #[test]
    fn rows_carry_their_changes() {
        let t = Timeline::new(&[
            entry(1, "2026-08-15 21:00", 83.1),
            entry(2, "2026-08-16 07:15", 82.04),
            entry(3, "2026-08-16 21:00", 82.7),
        ]);
        let rows = rows(&t, &at("2026-09-01 12:00"));
        assert_eq!(
            rows.iter().map(|r| r.id).collect::<Vec<_>>(),
            [3, 2, 1],
            "newest first"
        );
        let morning = &rows[1];
        assert_eq!(morning.period, Period::Morning);
        assert_eq!(morning.weight.to_string(), "82.0");
        assert_eq!(morning.weight.input(), "82.04");
        assert_eq!(morning.when, "Aug 16, 2026 07:15");
        assert_eq!(
            morning.overnight.map(|g| g.delta().to_string()),
            Some("-1.1 kg".into())
        );
        assert_eq!(rows[0].daily, Some(Grams(660)));
        assert_eq!((rows[2].overnight, rows[2].daily), (None, None));
    }

    #[test]
    fn recent_rows_say_today_and_yesterday() {
        let t = Timeline::new(&[
            entry(1, "2026-08-15 07:30", 83.1),
            entry(2, "2026-09-19 23:50", 81.4),
            entry(3, "2026-09-20 08:12", 81.0),
        ]);
        let when: Vec<String> = rows(&t, &at("2026-09-20 21:30"))
            .into_iter()
            .map(|r| r.when)
            .collect();
        assert_eq!(
            when,
            ["Today 08:12", "Yesterday 23:50", "Aug 15, 2026 07:30"]
        );
    }

    #[test]
    fn filtering_keeps_changes_whose_partner_is_filtered_out() {
        let t = Timeline::new(&[
            entry(1, "2026-08-15 21:00", 83.1),
            entry(2, "2026-08-16 07:15", 82.0),
        ]);
        let all = rows(&t, &at("2026-09-01 12:00"));
        let mornings = filter(all.clone(), Some(Period::Morning), &Window::default());
        assert_eq!(mornings.len(), 1);
        assert_eq!(mornings[0].overnight, Some(Grams(-1100)));
        let window = Window {
            from: Some(at("2026-08-16 00:00")),
            until: None,
        };
        assert_eq!(filter(all, None, &window).len(), 1);
    }
}

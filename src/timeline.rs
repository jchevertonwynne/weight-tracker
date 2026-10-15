//! Weigh-ins in time order, and the changes between neighbouring ones.

use std::collections::HashMap;

use jiff::Zoned;
use jiff::civil::Date;

use crate::model::{Entry, Grams, Period};

/// The day a reading counts towards. Morning starts at 4am, so a reading
/// before then is still last night: a 1:25am weigh-in belongs with the
/// evening before it, not the morning a few hours later.
pub fn logical_date(t: &Zoned) -> Date {
    let date = t.date();
    if t.hour() < 4 {
        date.yesterday().expect("a date in range")
    } else {
        date
    }
}

/// Every entry, oldest first, with two changes worked out between them:
///
/// - overnight: a morning against the evening before it, when that evening
///   is on the previous day — a gap in logging means no pair;
/// - daily: an evening against that same day's morning.
#[derive(Debug, Default)]
pub struct Timeline {
    entries: Vec<Entry>,
    /// (evening, morning) indices into `entries`, oldest first.
    nights: Vec<(usize, usize)>,
    overnight: HashMap<i64, Grams>,
    daily: HashMap<i64, Grams>,
}

impl Timeline {
    pub fn new(entries: &[Entry]) -> Timeline {
        let mut entries = entries.to_vec();
        // Stable, so readings at the same instant keep their order.
        entries.sort_by_key(|e| e.recorded_at.timestamp());

        let mut timeline = Timeline::default();
        let (mut last_morning, mut last_evening) = (None::<usize>, None::<usize>);
        for (i, e) in entries.iter().enumerate() {
            let day = logical_date(&e.recorded_at);
            match e.period() {
                Period::Morning => {
                    if let Some(ev) = last_evening
                        && logical_date(&entries[ev].recorded_at).tomorrow().ok() == Some(day)
                    {
                        timeline.nights.push((ev, i));
                        timeline
                            .overnight
                            .insert(e.id, e.weight - entries[ev].weight);
                    }
                    last_morning = Some(i);
                }
                Period::Evening => {
                    if let Some(m) = last_morning
                        && logical_date(&entries[m].recorded_at) == day
                    {
                        timeline.daily.insert(e.id, e.weight - entries[m].weight);
                    }
                    last_evening = Some(i);
                }
            }
        }
        timeline.entries = entries;
        timeline
    }

    pub fn entries(&self) -> &[Entry] {
        &self.entries
    }

    /// A morning's change from the evening before.
    pub fn overnight(&self, id: i64) -> Option<Grams> {
        self.overnight.get(&id).copied()
    }

    /// An evening's change from that day's morning.
    pub fn daily(&self, id: i64) -> Option<Grams> {
        self.daily.get(&id).copied()
    }

    /// Each night that has both ends logged, as (evening, morning), oldest
    /// first.
    pub fn nights(&self) -> impl DoubleEndedIterator<Item = (&Entry, &Entry)> {
        self.nights
            .iter()
            .map(|&(e, m)| (&self.entries[e], &self.entries[m]))
    }

    /// Each entry of `period` with its change from the previous one of the
    /// same period, however long before — a plain day-over-day comparison.
    pub fn sequential(&self, period: Period) -> impl Iterator<Item = (&Entry, Grams)> {
        let same = self.entries.iter().filter(move |e| e.period() == period);
        same.clone()
            .zip(same.skip(1))
            .map(|(prev, e)| (e, e.weight - prev.weight))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, entry, entry_as};

    #[test]
    fn logical_dates() {
        for (t, want) in [
            ("2026-08-16 12:30", "2026-08-16"),
            ("2026-08-16 23:45", "2026-08-16"),
            ("2026-08-16 01:25", "2026-08-15"),
            ("2026-08-16 03:59", "2026-08-15"),
            ("2026-08-16 04:00", "2026-08-16"),
            ("2026-09-01 02:00", "2026-08-31"), // across a month
            ("2026-01-01 00:30", "2025-12-31"), // across a year
        ] {
            assert_eq!(logical_date(&at(t)).to_string(), want, "{t}");
        }
    }

    #[test]
    fn sorts_without_touching_the_input() {
        let input = [
            entry(3, "2026-08-16 07:15", 82.0),
            entry(1, "2026-08-15 07:30", 82.4),
            entry(2, "2026-08-15 21:00", 83.1),
        ];
        let t = Timeline::new(&input);
        assert_eq!(
            t.entries().iter().map(|e| e.id).collect::<Vec<_>>(),
            [1, 2, 3]
        );
        assert_eq!(input[0].id, 3);
    }

    #[test]
    fn overnight_and_daily_changes() {
        let t = Timeline::new(&[
            entry(1, "2026-08-15 21:00", 83.1),
            entry(2, "2026-08-16 07:15", 82.0),
        ]);
        assert_eq!((t.overnight(2), t.daily(1)), (Some(Grams(-1100)), None));

        let t = Timeline::new(&[
            entry(1, "2026-08-15 07:30", 82.4),
            entry(2, "2026-08-15 21:00", 83.1),
        ]);
        assert_eq!((t.daily(2), t.nights().count()), (Some(Grams(700)), 0));

        // A 1:25am evening pairs with the morning hours later.
        let t = Timeline::new(&[
            entry(1, "2026-08-16 01:25", 83.5),
            entry(2, "2026-08-16 07:15", 82.6),
        ]);
        assert_eq!(t.overnight(2), Some(Grams(-900)));

        // A gap means no pair, either way round.
        let t = Timeline::new(&[
            entry(1, "2026-08-13 21:00", 83.1),
            entry(2, "2026-08-16 07:15", 82.0),
        ]);
        assert_eq!(t.overnight(2), None);
        let t = Timeline::new(&[
            entry(1, "2026-08-15 07:30", 82.4),
            entry(2, "2026-08-16 21:00", 83.1),
        ]);
        assert_eq!(t.daily(2), None);

        // An override redirects the pairing.
        let t = Timeline::new(&[
            entry_as(1, "2026-08-15 09:00", 83.0, Period::Evening),
            entry(2, "2026-08-16 07:15", 82.0),
        ]);
        assert_eq!(t.overnight(2), Some(Grams(-1000)));
    }

    #[test]
    fn a_run_of_days() {
        let t = Timeline::new(&[
            entry(1, "2026-08-14 07:00", 84.0),
            entry(2, "2026-08-14 21:00", 85.0),
            entry(3, "2026-08-15 07:00", 83.5),
            entry(4, "2026-08-15 21:00", 84.2),
            entry(5, "2026-08-16 07:00", 83.0),
        ]);
        let changes = |f: fn(&Timeline, i64) -> Option<Grams>| {
            (1..=5)
                .filter_map(|id| f(&t, id).map(|g| (id, g.0)))
                .collect::<Vec<_>>()
        };
        assert_eq!(changes(Timeline::overnight), [(3, -1500), (5, -1200)]);
        assert_eq!(changes(Timeline::daily), [(2, 1000), (4, 700)]);
        let nights: Vec<_> = t.nights().map(|(e, m)| (e.id, m.id)).collect();
        assert_eq!(nights, [(2, 3), (4, 5)]);
        assert!(Timeline::new(&[]).nights().next().is_none());
    }

    #[test]
    fn sequential_changes() {
        let t = Timeline::new(&[
            entry(1, "2026-08-14 07:00", 84.0),
            entry(2, "2026-08-14 21:00", 85.0),
            entry(3, "2026-08-16 07:00", 83.0), // the 15th skipped
            entry(4, "2026-08-16 21:00", 84.5),
        ]);
        let of = |p| {
            t.sequential(p)
                .map(|(e, g)| (e.id, g.0))
                .collect::<Vec<_>>()
        };
        assert_eq!(of(Period::Morning), [(3, -1000)]);
        assert_eq!(of(Period::Evening), [(4, -500)]);
    }
}

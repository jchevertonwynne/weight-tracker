//! The Overnight tab: how much weight goes between an evening weigh-in and
//! the next morning's.

use jiff::Zoned;
use serde::Serialize;

use crate::model::{Entry, Goal, Grams, current_goal};
use crate::timeline::Timeline;
use crate::timerange::{Range, Window};

/// One night: an evening reading and the next morning's.
#[derive(Clone, Debug)]
pub struct Night {
    pub evening: Entry,
    pub morning: Entry,
}

impl Night {
    pub fn change(&self) -> Grams {
        self.morning.weight - self.evening.weight
    }
}

/// The nights whose morning is inside `window`, newest first. Pairs are made
/// across the whole timeline first, so the first morning in the window keeps
/// an evening that falls just outside it.
pub fn nights(timeline: &Timeline, window: &Window) -> Vec<Night> {
    timeline
        .nights()
        .rev()
        .filter(|(_, morning)| window.contains(&morning.recorded_at))
        .map(|(evening, morning)| Night {
            evening: evening.clone(),
            morning: morning.clone(),
        })
        .collect()
}

/// The figures over a set of nights.
#[derive(Clone, Debug)]
pub struct Stats {
    pub count: usize,
    pub last: Grams,
    /// The mean, truncated to the gram.
    pub typical: Grams,
    /// The smallest loss, or a gain: the night to plan for when the weight
    /// has to be made.
    pub safest: Grams,
    /// The largest loss.
    pub biggest_loss: Grams,
}

/// None for no nights at all. `nights` is newest first.
pub fn stats(nights: &[Night]) -> Option<Stats> {
    let changes: Vec<Grams> = nights.iter().map(Night::change).collect();
    Some(Stats {
        count: changes.len(),
        last: *changes.first()?,
        typical: mean(&changes)?,
        safest: *changes.iter().max()?,
        biggest_loss: *changes.iter().min()?,
    })
}

fn mean(changes: &[Grams]) -> Option<Grams> {
    let n = i64::try_from(changes.len()).ok().filter(|&n| n > 0)?;
    Some(Grams(changes.iter().copied().sum::<Grams>().0 / n))
}

/// The timescales compared side by side in the calculator — fixed, and not
/// governed by the range filter, because comparing them is the point.
const TIMESCALES: [(&str, &str, i64); 4] = [
    ("7d", "7 days", 7),
    ("30d", "30 days", 30),
    ("90d", "90 days", 90),
    ("1y", "1 year", 365),
];

/// One timescale's column, as chart-app.js reads it. The column spans the
/// smallest and largest change; the mean and ±1 sample standard deviation
/// place its midpoint and fill the tooltip. With a single night there is no
/// spread, and every figure is the mean.
#[derive(Clone, Debug, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Column {
    pub label: &'static str,
    pub name: &'static str,
    pub count: usize,
    pub has_range: bool,
    pub mean_kg: f64,
    pub mean_label: String,
    pub low_kg: f64,
    pub high_kg: f64,
    pub min_kg: f64,
    pub max_kg: f64,
}

/// The /overnight/windows response.
#[derive(Clone, Debug, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Timescales {
    pub has_data: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub empty: Option<&'static str>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub points: Vec<Column>,
    /// The goal in effect, so the "tonight's weight" calculator can judge a
    /// projected morning against it.
    pub has_goal: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub goal_kg: Option<f64>,
}

pub fn timescales(timeline: &Timeline, goals: &[Goal], now: &Zoned) -> Timescales {
    let mut columns: Vec<Column> = Vec::new();
    for (label, name, days) in TIMESCALES {
        let nights = nights(timeline, &Range::Days(days).window(now));
        // A timescale holding the same nights as the last one shown would be
        // the same column again.
        if columns.last().is_some_and(|c| c.count == nights.len()) {
            continue;
        }
        let mut column = Column {
            label,
            name,
            count: nights.len(),
            ..Default::default()
        };
        let changes: Vec<Grams> = nights.iter().map(Night::change).collect();
        if let Some(mean) = mean(&changes) {
            column.mean_kg = mean.kg();
            column.mean_label = mean.delta().to_string();
            column.min_kg = changes.iter().min().map_or(0.0, |g| g.kg());
            column.max_kg = changes.iter().max().map_or(0.0, |g| g.kg());
            let sd = sample_sd(&changes, mean).map(|sd| Grams(sd.round() as i64));
            column.has_range = sd.is_some();
            let sd = sd.unwrap_or_default();
            column.low_kg = Grams(mean.0 - sd.0).kg();
            column.high_kg = Grams(mean.0 + sd.0).kg();
        }
        columns.push(column);
    }

    if columns.iter().all(|c| c.count == 0) {
        return Timescales {
            empty: Some(
                "Not enough data yet — log some overnight pairs to see how the range compares across timescales.",
            ),
            ..Default::default()
        };
    }
    let goal = current_goal(goals, now);
    Timescales {
        has_data: true,
        empty: None,
        points: columns,
        has_goal: goal.is_some(),
        goal_kg: goal.map(|g| g.weight.kg()),
    }
}

/// The sample standard deviation (n − 1) in grams, for two or more values.
fn sample_sd(changes: &[Grams], mean: Grams) -> Option<f64> {
    if changes.len() < 2 {
        return None;
    }
    let squares: f64 = changes.iter().map(|g| (g.0 - mean.0).pow(2) as f64).sum();
    Some((squares / (changes.len() - 1) as f64).sqrt())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, entry, goal};

    fn all(entries: &[Entry]) -> Vec<Night> {
        nights(&Timeline::new(entries), &Window::default())
    }

    #[test]
    fn nights_need_both_ends() {
        let got = all(&[
            entry(1, "2026-08-14 20:00", 84.0),
            entry(2, "2026-08-15 07:00", 82.5),
        ]);
        assert_eq!(got.len(), 1);
        assert_eq!(
            (got[0].evening.id, got[0].morning.id, got[0].change()),
            (1, 2, Grams(-1500))
        );
        assert!(
            all(&[
                entry(1, "2026-08-10 20:00", 84.0),
                entry(2, "2026-08-14 07:00", 82.5)
            ])
            .is_empty()
        );
        assert!(all(&[entry(1, "2026-08-15 07:00", 82.5)]).is_empty());

        let got = all(&[
            entry(1, "2026-08-14 20:00", 84.0),
            entry(2, "2026-08-15 07:00", 82.5),
            entry(3, "2026-08-17 20:00", 85.0),
            entry(4, "2026-08-18 07:00", 83.9),
        ]);
        assert_eq!(
            got.iter().map(|n| n.morning.id).collect::<Vec<_>>(),
            [4, 2],
            "newest first"
        );
    }

    #[test]
    fn a_window_keeps_an_evening_just_outside_it() {
        let t = Timeline::new(&[
            entry(1, "2026-08-14 20:00", 84.0),
            entry(2, "2026-08-15 07:00", 82.5),
        ]);
        let from = |s| Window {
            from: Some(at(s)),
            until: None,
        };
        assert_eq!(nights(&t, &from("2026-08-15 00:00")).len(), 1);
        assert!(nights(&t, &from("2026-08-16 00:00")).is_empty());
    }

    #[test]
    fn stats_over_nights() {
        assert!(stats(&[]).is_none());
        let got = stats(&all(&[
            entry(1, "2026-08-14 20:00", 84.0),
            entry(2, "2026-08-15 07:00", 82.5), // -1.5
            entry(3, "2026-08-15 20:00", 84.0),
            entry(4, "2026-08-16 07:00", 82.9), // -1.1, the latest
        ]))
        .unwrap();
        assert_eq!(got.count, 2);
        assert_eq!(
            (got.last, got.typical, got.safest, got.biggest_loss),
            (Grams(-1100), Grams(-1300), Grams(-1100), Grams(-1500))
        );
    }

    #[test]
    fn sample_standard_deviation() {
        let g = |v: &[i64]| v.iter().map(|&x| Grams(x)).collect::<Vec<_>>();
        assert!(sample_sd(&g(&[]), Grams(0)).is_none());
        assert!(sample_sd(&g(&[-1500]), Grams(-1500)).is_none());
        // By hand: about 251.66 g.
        let sd = sample_sd(&g(&[-1000, -1500, -1300]), Grams(-1266)).unwrap();
        assert!((sd - 251.66).abs() < 0.5, "{sd}");
    }

    #[test]
    fn timescale_columns() {
        let now = at("2026-08-20 12:00");
        let got = timescales(&Timeline::default(), &[], &now);
        assert!(!got.has_data && got.empty.is_some());

        let one = Timeline::new(&[
            entry(1, "2026-08-19 20:00", 84.0),
            entry(2, "2026-08-20 07:00", 82.5),
        ]);
        let got = timescales(&one, &[], &now);
        let c = &got.points[0];
        assert_eq!((c.label, c.count, c.has_range), ("7d", 1, false));
        assert_eq!([c.low_kg, c.high_kg, c.min_kg, c.max_kg], [c.mean_kg; 4]);
        assert!(!got.has_goal);
        let got = timescales(&one, &[goal(1, 80.0, "2026-01-01")], &now);
        assert_eq!((got.has_goal, got.goal_kg), (true, Some(80.0)));

        let three = Timeline::new(&[
            entry(1, "2026-08-18 20:00", 84.0), // -1.5
            entry(2, "2026-08-19 07:00", 82.5),
            entry(3, "2026-07-25 20:00", 84.0), // -3.0
            entry(4, "2026-07-26 07:00", 81.0),
            entry(5, "2026-06-01 20:00", 84.0), // +0.5
            entry(6, "2026-06-02 07:00", 84.5),
        ]);
        let got = timescales(&three, &[], &now);
        let counts: Vec<_> = got.points.iter().map(|c| (c.label, c.count)).collect();
        assert_eq!(
            counts,
            [("7d", 1), ("30d", 2), ("90d", 3)],
            "1y repeats 90d"
        );
        assert_eq!((got.points[2].min_kg, got.points[2].max_kg), (-3.0, 0.5));
        assert!(got.points.iter().all(|c| !c.name.is_empty()));
    }

    // A timescale is only shown when it covers more nights than the one
    // before.
    #[test]
    fn repeated_timescales_are_dropped() {
        let now = at("2026-08-22 12:00");
        let history = |days: i64| {
            let mut entries = Vec::new();
            for d in (1..=days).rev() {
                let evening = now.checked_sub(jiff::Span::new().days(d)).unwrap();
                let morning = evening.tomorrow().unwrap();
                let at = |t: &Zoned, h: i8| t.with().hour(h).minute(0).build().unwrap();
                let id = entries.len() as i64;
                entries.push(Entry {
                    id,
                    recorded_at: at(&evening, 21),
                    weight: Grams(84000),
                    period_override: None,
                });
                entries.push(Entry {
                    id: id + 1,
                    recorded_at: at(&morning, 7),
                    weight: Grams(83000),
                    period_override: None,
                });
            }
            Timeline::new(&entries)
        };
        for (days, want) in [
            (3, vec!["7d"]),
            (10, vec!["7d", "30d"]),
            (45, vec!["7d", "30d", "90d"]),
            (400, vec!["7d", "30d", "90d", "1y"]),
        ] {
            let got: Vec<_> = timescales(&history(days), &[], &now)
                .points
                .iter()
                .map(|c| c.label)
                .collect();
            assert_eq!(got, want, "{days} nights");
        }
    }

    // The field names chart-app.js reads.
    #[test]
    fn json_shape() {
        let empty = serde_json::to_value(timescales(
            &Timeline::default(),
            &[],
            &at("2026-08-20 12:00"),
        ))
        .unwrap();
        assert_eq!(empty["hasData"], false);
        assert!(empty.get("points").is_none() && empty.get("goalKg").is_none());
        let column = serde_json::to_value(Column::default()).unwrap();
        for key in [
            "label",
            "name",
            "count",
            "hasRange",
            "meanKg",
            "meanLabel",
            "lowKg",
            "highKg",
            "minKg",
            "maxKg",
        ] {
            assert!(column.get(key).is_some(), "{key}");
        }
    }
}

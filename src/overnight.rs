//! The evening-to-morning weight change: the per-pair deltas, a summary over
//! a range, and the fixed 7d/30d/90d/1y comparison on the Overnight tab.

use std::collections::HashMap;

use jiff::Zoned;
use serde::Serialize;

use crate::db::{self, Entry, Goal};
use crate::timerange::{self, Window};
use crate::{clock, goals, weight};

/// One evening-to-morning pair: the adjacency
/// [`weight::chronological_with_deltas`] establishes, with both sides kept
/// for display.
#[derive(Clone, Debug, Default)]
pub struct Pair {
    /// What identifies a pair in the tests; the page shows the labels.
    #[cfg_attr(not(test), allow(dead_code))]
    pub morning_id: i64,
    /// Kept for windowing; not rendered directly.
    pub morning_recorded_at: Option<Zoned>,
    pub evening_label: String,
    pub evening_weight_str: String,
    pub morning_label: String,
    pub morning_weight_str: String,
    pub delta_g: i64,
    pub delta_str: String,
    pub is_loss: bool,
}

/// Walks `chrono` (ascending), pairing each morning that has an overnight
/// delta with the evening immediately before it. Newest first, as the
/// History list is.
pub fn build_pairs(chrono: &[Entry], overnight: &HashMap<i64, i64>) -> Vec<Pair> {
    let mut pairs = Vec::new();
    let mut last_evening: Option<&Entry> = None;
    for e in chrono {
        match weight::entry_period(e) {
            "morning" => {
                if let (Some(&delta), Some(ev)) = (overnight.get(&e.id), last_evening) {
                    pairs.push(Pair {
                        morning_id: e.id,
                        morning_recorded_at: Some(e.recorded_at.clone()),
                        evening_label: clock::datetime_label(&ev.recorded_at),
                        evening_weight_str: weight::format_kg(ev.weight_g),
                        morning_label: clock::datetime_label(&e.recorded_at),
                        morning_weight_str: weight::format_kg(e.weight_g),
                        delta_g: delta,
                        delta_str: weight::format_kg_delta(delta),
                        is_loss: delta < 0,
                    });
                }
            }
            "evening" => last_evening = Some(e),
            _ => {}
        }
    }
    pairs.reverse();
    pairs
}

/// The overnight deltas within a range, formatted for display.
#[derive(Clone, Debug, Default)]
pub struct Summary {
    /// Set when there are no pairs in range.
    pub empty: String,
    pub count: usize,
    /// The "Typical" figure.
    pub avg_str: String,
    pub avg_is_loss: bool,
    /// The newest pair: how last night compared.
    pub last_str: String,
    pub last_is_loss: bool,
    /// max(deltas): the smallest loss (or a gain) on record — "Safe", since
    /// it is the worst case for making weight.
    pub best_case_str: String,
    pub best_case_is_loss: bool,
    /// min(deltas): the largest loss on record.
    pub worst_case_str: String,
    pub worst_case_is_loss: bool,
}

pub fn build_summary(pairs: &[Pair]) -> Summary {
    let Some(first) = pairs.first() else {
        return Summary {
            empty: "Not enough data yet — log an evening weigh-in followed by the next morning's to see overnight stats.".into(),
            ..Default::default()
        };
    };
    let sum: i64 = pairs.iter().map(|p| p.delta_g).sum();
    let best = pairs.iter().map(|p| p.delta_g).max().unwrap_or_default();
    let worst = pairs.iter().map(|p| p.delta_g).min().unwrap_or_default();
    // Integer division, truncating toward zero as Go's does.
    let avg = sum / pairs.len() as i64;
    let last = first.delta_g; // newest first
    Summary {
        empty: String::new(),
        count: pairs.len(),
        avg_str: weight::format_kg_delta(avg),
        avg_is_loss: avg < 0,
        last_str: weight::format_kg_delta(last),
        last_is_loss: last < 0,
        best_case_str: weight::format_kg_delta(best),
        best_case_is_loss: best < 0,
        worst_case_str: weight::format_kg_delta(worst),
        worst_case_is_loss: worst < 0,
    }
}

/// Every pair from the full entry set, then filtered to `window` — deltas
/// must be computed over everything first, or an evening just outside the
/// window would starve the first visible morning of its partner.
pub fn windowed_pairs(entries: &[Entry], window: &Window) -> Vec<Pair> {
    let (chrono, overnight, _) = weight::chronological_with_deltas(entries);
    build_pairs(&chrono, &overnight)
        .into_iter()
        .filter(|p| {
            p.morning_recorded_at
                .as_ref()
                .is_some_and(|t| window.contains(t))
        })
        .collect()
}

/// The fixed trailing windows compared side by side — deliberately not
/// governed by the range picker, since seeing the timescales together is the
/// point. Shortest to longest, which lets [`build_window_chart`] drop a
/// window covering no more nights than the one before.
const WINDOW_SPANS: [(&str, &str, &str); 4] = [
    // (axis label, tooltip name, range)
    ("7d", "7 days", "7"),
    ("30d", "30 days", "30"),
    ("90d", "90 days", "90"),
    ("1y", "1 year", "365"),
];

/// One window's column. Min and Max are the actual smallest and largest
/// deltas, and are what the column spans; Mean and the ±1 sample-SD Low and
/// High place the gradient's midpoint and fill the tooltip. HasRange is
/// false below two pairs, since a standard deviation needs two samples —
/// Low/High (and Min/Max) then collapse onto Mean.
#[derive(Clone, Debug, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WindowPoint {
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

#[derive(Clone, Debug, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct WindowChart {
    pub has_data: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub empty: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub points: Vec<WindowPoint>,
    /// The active goal, so the client's "tonight's weight" calculator can
    /// judge a projected morning range against it without another request.
    pub has_goal: bool,
    #[serde(skip_serializing_if = "is_zero")]
    pub goal_kg: f64,
}

fn is_zero(v: &f64) -> bool {
    *v == 0.0
}

/// The sample standard deviation (n-1) of `deltas` around `mean_g`; None
/// below two values.
fn sample_std_dev(deltas: &[i64], mean_g: f64) -> Option<f64> {
    if deltas.len() < 2 {
        return None;
    }
    let sum_sq: f64 = deltas.iter().map(|&d| (d as f64 - mean_g).powi(2)).sum();
    Some((sum_sq / (deltas.len() - 1) as f64).sqrt())
}

pub fn build_window_chart(entries: &[Entry], all_goals: &[Goal], now: &Zoned) -> WindowChart {
    let mut points = Vec::new();
    let mut any_data = false;
    // The count of the last window kept, so each is compared with what is on
    // the chart: with ten days logged, 90d is dropped for matching 30d, and
    // 1y is then compared with 30d too.
    let mut last_count = None;
    for (label, name, range) in WINDOW_SPANS {
        let pairs = windowed_pairs(entries, &timerange::resolve(range, "", "", now));
        // The same nights as the previous window would be the same column,
        // inviting the reader to find a difference that is not there.
        if last_count == Some(pairs.len()) {
            continue;
        }
        last_count = Some(pairs.len());

        let mut point = WindowPoint {
            label,
            name,
            count: pairs.len(),
            ..Default::default()
        };
        if !pairs.is_empty() {
            any_data = true;
            let deltas: Vec<i64> = pairs.iter().map(|p| p.delta_g).collect();
            let mean_g = deltas.iter().sum::<i64>() / deltas.len() as i64;
            point.mean_kg = db::grams_to_kg(mean_g);
            point.mean_label = weight::format_kg_delta(mean_g);
            point.min_kg = db::grams_to_kg(*deltas.iter().min().expect("non-empty"));
            point.max_kg = db::grams_to_kg(*deltas.iter().max().expect("non-empty"));
            if let Some(sd) = sample_std_dev(&deltas, mean_g as f64) {
                let sd = sd.round() as i64;
                point.has_range = true;
                point.low_kg = db::grams_to_kg(mean_g - sd);
                point.high_kg = db::grams_to_kg(mean_g + sd);
            } else {
                point.low_kg = point.mean_kg;
                point.high_kg = point.mean_kg;
            }
        }
        points.push(point);
    }

    if !any_data {
        return WindowChart {
            empty: "Not enough data yet — log some overnight pairs to see how the range compares across timescales.".into(),
            ..Default::default()
        };
    }
    let mut chart = WindowChart {
        has_data: true,
        points,
        ..Default::default()
    };
    if let Some(goal) = goals::current(all_goals, now) {
        chart.has_goal = true;
        chart.goal_kg = db::grams_to_kg(goal.weight_g);
    }
    chart
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, entry, goal};

    fn pairs_of(entries: &[Entry]) -> Vec<Pair> {
        let (chrono, overnight, _) = weight::chronological_with_deltas(entries);
        build_pairs(&chrono, &overnight)
    }

    #[test]
    fn build_pairs_cases() {
        let pairs = pairs_of(&[
            entry(1, at("2026-08-14 20:00"), 84.0, ""),
            entry(2, at("2026-08-15 07:00"), 82.5, ""),
        ]);
        assert_eq!(pairs.len(), 1);
        let p = &pairs[0];
        assert_eq!(p.morning_id, 2);
        assert_eq!(
            (p.evening_weight_str.as_str(), p.morning_weight_str.as_str()),
            ("84.0", "82.5")
        );
        assert_eq!(p.delta_str, "-1.5 kg");
        assert!(p.is_loss);

        assert!(
            pairs_of(&[
                entry(1, at("2026-08-10 20:00"), 84.0, ""),
                entry(2, at("2026-08-14 07:00"), 82.5, ""),
            ])
            .is_empty()
        );
        assert!(pairs_of(&[entry(1, at("2026-08-15 07:00"), 82.5, "")]).is_empty());

        let pairs = pairs_of(&[
            entry(1, at("2026-08-14 20:00"), 84.0, ""),
            entry(2, at("2026-08-15 07:00"), 82.5, ""),
            entry(3, at("2026-08-17 20:00"), 85.0, ""),
            entry(4, at("2026-08-18 07:00"), 83.9, ""),
        ]);
        assert_eq!(
            pairs.iter().map(|p| p.morning_id).collect::<Vec<_>>(),
            [4, 2]
        );
    }

    fn pair(id: i64, delta: i64) -> Pair {
        Pair {
            morning_id: id,
            delta_g: delta,
            is_loss: delta < 0,
            ..Default::default()
        }
    }

    #[test]
    fn build_summary_cases() {
        assert!(!build_summary(&[]).empty.is_empty());

        let got = build_summary(&[pair(3, -1100), pair(2, -1500)]);
        assert_eq!(got.count, 2);
        assert_eq!((got.avg_str.as_str(), got.avg_is_loss), ("-1.3 kg", true));
        assert_eq!((got.last_str.as_str(), got.last_is_loss), ("-1.1 kg", true));
        assert_eq!(
            (got.best_case_str.as_str(), got.best_case_is_loss),
            ("-1.1 kg", true)
        );
        assert_eq!(
            (got.worst_case_str.as_str(), got.worst_case_is_loss),
            ("-1.5 kg", true)
        );

        let got = build_summary(&[pair(1, 200)]);
        assert!(
            !(got.avg_is_loss
                || got.best_case_is_loss
                || got.worst_case_is_loss
                || got.last_is_loss)
        );
    }

    #[test]
    fn windowed_pairs_cases() {
        let entries = [
            entry(1, at("2026-08-14 20:00"), 84.0, ""),
            entry(2, at("2026-08-15 07:00"), 82.5, ""),
        ];
        // The morning keeps its delta though its evening is out of window.
        let w = Window {
            from: Some(at("2026-08-15 00:00")),
            until: None,
        };
        let pairs = windowed_pairs(&entries, &w);
        assert_eq!(pairs.len(), 1);
        assert_eq!(pairs[0].delta_str, "-1.5 kg");
        let w = Window {
            from: Some(at("2026-08-16 00:00")),
            until: None,
        };
        assert!(windowed_pairs(&entries, &w).is_empty());
        assert_eq!(windowed_pairs(&entries, &Window::default()).len(), 1);
    }

    #[test]
    fn sample_std_dev_cases() {
        assert!(sample_std_dev(&[], 0.0).is_none());
        assert!(sample_std_dev(&[-1500], -1500.0).is_none());
        // Computed by hand: ~251.66 g.
        let got = sample_std_dev(&[-1000, -1500, -1300], -1266.6666666666667).unwrap();
        assert!((got - 251.66).abs() < 0.5, "{got}");
    }

    #[test]
    fn build_window_chart_cases() {
        let now = at("2026-08-20 12:00");
        let got = build_window_chart(&[], &[], &now);
        assert!(!got.has_data && !got.empty.is_empty());

        let one = [
            entry(1, at("2026-08-19 20:00"), 84.0, ""),
            entry(2, at("2026-08-20 07:00"), 82.5, ""),
        ];
        let got = build_window_chart(&one, &[], &now);
        assert!(got.has_data);
        let p = &got.points[0];
        assert_eq!((p.label, p.count, p.has_range), ("7d", 1, false));
        assert_eq!(
            (p.low_kg, p.high_kg, p.min_kg, p.max_kg),
            (p.mean_kg, p.mean_kg, p.mean_kg, p.mean_kg)
        );
        assert!(!got.has_goal);

        let got = build_window_chart(&one, &[goal(1, 80.0, "2026-01-01")], &now);
        assert!(got.has_goal);
        assert_eq!(got.goal_kg, 80.0);

        let entries = [
            entry(1, at("2026-08-18 20:00"), 84.0, ""), // -1.5
            entry(2, at("2026-08-19 07:00"), 82.5, ""),
            entry(3, at("2026-07-25 20:00"), 84.0, ""), // -3.0
            entry(4, at("2026-07-26 07:00"), 81.0, ""),
            entry(5, at("2026-06-01 20:00"), 84.0, ""), // +0.5
            entry(6, at("2026-06-02 07:00"), 84.5, ""),
        ];
        let got = build_window_chart(&entries, &[], &now);
        let count = |l| got.points.iter().find(|p| p.label == l).map(|p| p.count);
        assert_eq!(
            (count("7d"), count("30d"), count("90d")),
            (Some(1), Some(2), Some(3))
        );
        let ninety = got.points.iter().find(|p| p.label == "90d").unwrap();
        assert_eq!((ninety.min_kg, ninety.max_kg), (-3.0, 0.5));
    }

    // A timescale is only offered when it covers more nights than the one
    // before: 90d over ten days of history would be the 30d column again.
    #[test]
    fn build_window_chart_drops_windows_matching_the_one_before() {
        let now = at("2026-08-22 12:00");
        let nights = |n: i64| {
            let mut entries = Vec::new();
            let mut id = 0;
            for d in (1..=n).rev() {
                let evening = clock::add_date(&now, 0, 0, -d);
                let morning = clock::add_date(&evening, 0, 0, 1);
                let on = |t: &Zoned, h| {
                    clock::date(
                        t.year().into(),
                        t.month().into(),
                        t.day().into(),
                        h,
                        0,
                        0,
                        0,
                        t.time_zone(),
                    )
                };
                id += 1;
                entries.push(entry(id, on(&evening, 21), 84.0, ""));
                id += 1;
                entries.push(entry(id, on(&morning, 7), 83.0, ""));
            }
            entries
        };
        for (days, want) in [
            (3, vec!["7d"]),
            (10, vec!["7d", "30d"]),
            (45, vec!["7d", "30d", "90d"]),
            (120, vec!["7d", "30d", "90d", "1y"]),
            (400, vec!["7d", "30d", "90d", "1y"]),
        ] {
            let got: Vec<&str> = build_window_chart(&nights(days), &[], &now)
                .points
                .iter()
                .map(|p| p.label)
                .collect();
            assert_eq!(got, want, "{days} nights");
        }
    }

    #[test]
    fn every_window_has_a_name() {
        let entries = [
            entry(1, at("2026-08-20 21:00"), 84.0, ""),
            entry(2, at("2026-08-21 07:00"), 83.0, ""),
        ];
        for p in build_window_chart(&entries, &[], &at("2026-08-22 12:00")).points {
            assert!(!p.name.is_empty(), "{}", p.label);
        }
    }

    // The JSON field names are what chart-app.js reads.
    #[test]
    fn window_chart_json_shape() {
        let got =
            serde_json::to_value(build_window_chart(&[], &[], &at("2026-08-20 12:00"))).unwrap();
        assert_eq!(got["hasData"], false);
        assert!(got.get("points").is_none() && got.get("goalKg").is_none());
        let p = serde_json::to_value(WindowPoint::default()).unwrap();
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
            assert!(p.get(key).is_some(), "{key}");
        }
    }
}

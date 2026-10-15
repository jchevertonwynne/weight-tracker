//! The trailing 7-day average morning weight against the 7 days before it,
//! a goal projection from that rate, and whether today has been logged.

use jiff::Zoned;

use crate::db::{self, Entry, Goal};
use crate::{clock, goals, weight};

/// Morning entries only: the fasted reading is less noisy than an evening
/// one, and is what the rest of the app treats as the reference point.
#[derive(Clone, Debug, Default)]
pub struct WeeklySummary {
    /// Set, with the week fields blank, when there is no data this week.
    pub empty: String,
    pub today: TodayStatus,
    pub this_week_avg: String,
    /// False when last week has no qualifying entries.
    pub has_comparison: bool,
    pub last_week_avg: String,
    pub delta: String,
    pub delta_is_loss: bool,
    pub goal: GoalProgress,
}

/// The Log tab's at-a-glance answer to "have I tracked today?": whether the
/// weigh-in for the current time of day (see [`db::detect_period`]) is in.
/// Independent of the week comparison, so it shows on a brand-new tracker.
#[derive(Clone, Debug, Default)]
pub struct TodayStatus {
    pub logged: bool,
    pub message: String,
}

/// This week's average projected onward to the active goal, at the same
/// week-over-week rate the delta is built from. Default (no goal) when there
/// is no active goal or no data this week to project from.
#[derive(Clone, Debug, Default)]
pub struct GoalProgress {
    pub has_goal: bool,
    /// The goal weight, kg.
    pub weight_str: String,
    pub reached: bool,
    /// "2.2 kg to go"
    pub remaining_str: String,
    /// "Sep 30, 2026"; empty if not computable.
    pub eta_label: String,
    /// Shown in place of the ETA when there is none.
    pub stalled: String,
}

/// How close this week's average has to be to call the goal reached, rather
/// than reporting a razor-thin "0.0 kg to go".
const REACHED_EPSILON_KG: f64 = 0.05;

/// The smallest weekly rate treated as a trend. Below it the ETA would be
/// years out — precision the data does not support — so it is "stalled".
const MIN_RATE_KG_PER_WEEK: f64 = 0.01;

fn today_status(entries: &[Entry], now: &Zoned) -> TodayStatus {
    let period = db::detect_period(now);
    let logged = entries
        .iter()
        .any(|e| weight::entry_period(e) == period && same_day(&e.recorded_at, now));
    TodayStatus {
        logged,
        message: if logged {
            format!("Today's {period} weigh-in is logged.")
        } else {
            format!("No {period} weigh-in logged yet today.")
        },
    }
}

/// Whether `a` and `b` share a calendar day, `a` read in `b`'s zone.
fn same_day(a: &Zoned, b: &Zoned) -> bool {
    a.with_time_zone(b.time_zone().clone()).date() == b.date()
}

pub fn build(entries: &[Entry], all_goals: &[Goal], now: &Zoned) -> WeeklySummary {
    let today = today_status(entries, now);
    let this_start = clock::add_date(now, 0, 0, -7).timestamp();
    let last_start = clock::add_date(now, 0, 0, -14).timestamp();
    let now_ts = now.timestamp();

    let mut this_week = Vec::new();
    let mut last_week = Vec::new();
    for e in entries {
        if weight::entry_period(e) != "morning" {
            continue;
        }
        let t = e.recorded_at.timestamp();
        if t >= this_start && t <= now_ts {
            this_week.push(e.weight_g);
        } else if t >= last_start && t < this_start {
            last_week.push(e.weight_g);
        }
    }

    if this_week.is_empty() {
        return WeeklySummary {
            empty: "Not enough morning weigh-ins this week yet for a trend comparison.".into(),
            today,
            ..Default::default()
        };
    }
    let this_avg = mean_kg(&this_week);
    let mut result = WeeklySummary {
        this_week_avg: format!("{this_avg:.1} kg"),
        today,
        ..Default::default()
    };

    let last_avg = (!last_week.is_empty()).then(|| mean_kg(&last_week));
    if let Some(last_avg) = last_avg {
        result.has_comparison = true;
        result.last_week_avg = format!("{last_avg:.1} kg");
        result.delta = format!("{:+.1} kg", this_avg - last_avg);
        result.delta_is_loss = this_avg < last_avg;
    }
    if let Some(goal) = goals::current(all_goals, now) {
        result.goal = goal_progress(this_avg, goal, last_avg, now);
    }
    result
}

fn goal_progress(this_avg: f64, goal: &Goal, last_avg: Option<f64>, now: &Zoned) -> GoalProgress {
    let goal_kg = db::grams_to_kg(goal.weight_g);
    let mut progress = GoalProgress {
        has_goal: true,
        weight_str: weight::format_kg(goal.weight_g),
        ..Default::default()
    };
    let remaining = this_avg - goal_kg;
    if remaining.abs() <= REACHED_EPSILON_KG {
        progress.reached = true;
        return progress;
    }
    progress.remaining_str = format!("{:.1} kg to go", remaining.abs());

    let Some(last_avg) = last_avg else {
        progress.stalled = "Not enough data yet for a goal estimate.".into();
        return progress;
    };
    // Above the goal needs a falling rate, below it a rising one: either way
    // progress_per_week is positive exactly when moving toward it.
    let rate = this_avg - last_avg;
    let progress_per_week = if remaining < 0.0 { rate } else { -rate };
    if progress_per_week <= MIN_RATE_KG_PER_WEEK {
        progress.stalled = "Not currently trending toward your goal.".into();
        return progress;
    }
    let weeks = remaining.abs() / progress_per_week;
    let days = (weeks * 7.0).round() as i64;
    progress.eta_label = clock::try_add_date(now, 0, 0, days)
        .map(|t| clock::date_label(&t))
        .unwrap_or_default();
    progress
}

/// The mean of gram values, in kilograms. Summed in integer grams, so it is
/// exact however many weigh-ins it covers; only the division is floating.
fn mean_kg(grams: &[i64]) -> f64 {
    db::grams_to_kg(grams.iter().sum()) / grams.len() as f64
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, entry, goal, nearly_equal};

    #[test]
    fn build_cases() {
        let now = at("2026-08-16 12:00");

        let got = build(
            &[
                entry(1, at("2026-08-15 07:00"), 82.0, ""),
                entry(2, at("2026-08-13 07:00"), 82.4, ""),
                entry(3, at("2026-08-08 07:00"), 83.0, ""),
                entry(4, at("2026-08-06 07:00"), 83.4, ""),
            ],
            &[],
            &now,
        );
        assert_eq!(got.empty, "");
        assert!(got.has_comparison);
        assert_eq!(got.this_week_avg, "82.2 kg");
        assert_eq!(got.last_week_avg, "83.2 kg");
        assert_eq!(got.delta, "-1.0 kg");
        assert!(got.delta_is_loss);

        // Evenings do not skew it.
        let got = build(
            &[
                entry(1, at("2026-08-15 07:00"), 82.0, ""),
                entry(2, at("2026-08-15 21:00"), 90.0, ""),
            ],
            &[],
            &now,
        );
        assert_eq!(got.this_week_avg, "82.0 kg");

        // An override counts.
        let got = build(
            &[entry(1, at("2026-08-15 21:00"), 82.0, "morning")],
            &[],
            &now,
        );
        assert_eq!(got.this_week_avg, "82.0 kg");

        let got = build(
            &[
                entry(1, at("2026-08-15 07:00"), 83.0, ""),
                entry(2, at("2026-08-08 07:00"), 82.0, ""),
            ],
            &[],
            &now,
        );
        assert_eq!(got.delta, "+1.0 kg");
        assert!(!got.delta_is_loss);

        let got = build(&[entry(1, at("2026-08-15 07:00"), 82.0, "")], &[], &now);
        assert!(!got.this_week_avg.is_empty() && !got.has_comparison && got.delta.is_empty());

        let got = build(
            &[
                entry(1, at("2026-08-15 07:00"), 82.0, ""),
                entry(2, at("2026-06-01 07:00"), 95.0, ""),
            ],
            &[],
            &now,
        );
        assert!(!got.has_comparison, "the old entry is outside both windows");

        let got = build(&[], &[], &now);
        assert!(!got.empty.is_empty() && got.this_week_avg.is_empty() && !got.has_comparison);

        let got = build(&[entry(1, at("2026-08-08 07:00"), 83.0, "")], &[], &now);
        assert!(!got.empty.is_empty(), "only last week has data");
    }

    #[test]
    fn today_status_cases() {
        let got = build(
            &[entry(1, at("2026-08-15 21:00"), 82.0, "")],
            &[],
            &at("2026-08-16 08:00"),
        )
        .today;
        assert!(!got.logged);
        assert_eq!(got.message, "No morning weigh-in logged yet today.");

        let got = build(
            &[entry(1, at("2026-08-16 07:15"), 82.0, "")],
            &[],
            &at("2026-08-16 09:30"),
        )
        .today;
        assert!(got.logged);
        assert_eq!(got.message, "Today's morning weigh-in is logged.");

        let got = build(
            &[entry(1, at("2026-08-16 07:15"), 82.0, "")],
            &[],
            &at("2026-08-16 20:00"),
        )
        .today;
        assert!(!got.logged);
        assert_eq!(got.message, "No evening weigh-in logged yet today.");

        let got = build(
            &[entry(1, at("2026-08-15 07:15"), 82.0, "")],
            &[],
            &at("2026-08-16 08:00"),
        )
        .today;
        assert!(!got.logged, "that entry was yesterday");

        let got = build(&[], &[], &at("2026-08-16 08:00"));
        assert!(!got.empty.is_empty() && !got.today.message.is_empty());
    }

    #[test]
    fn goal_progress_cases() {
        let now = at("2026-08-16 12:00");
        let this_week = vec![
            entry(1, at("2026-08-15 07:00"), 82.0, ""),
            entry(2, at("2026-08-13 07:00"), 82.0, ""),
        ];

        assert!(!build(&this_week, &[], &now).goal.has_goal);
        assert!(
            !build(&this_week, &[goal(1, 78.0, "2026-12-01")], &now)
                .goal
                .has_goal
        );

        let got = build(&this_week, &[goal(1, 82.02, "2026-01-01")], &now).goal;
        assert!(got.has_goal && got.reached && got.eta_label.is_empty());

        let got = build(&this_week, &[goal(1, 78.0, "2026-01-01")], &now).goal;
        assert!(!got.reached);
        assert_eq!(got.remaining_str, "4.0 kg to go");
        assert_eq!(got.stalled, "Not enough data yet for a goal estimate.");
        assert_eq!(got.eta_label, "");

        // 83.0 last week, so -1.0 kg/week, and 78.0 is 4 weeks out.
        let mut entries = this_week.clone();
        entries.push(entry(3, at("2026-08-08 07:00"), 83.0, ""));
        entries.push(entry(4, at("2026-08-06 07:00"), 83.0, ""));
        let got = build(&entries, &[goal(1, 78.0, "2026-01-01")], &now).goal;
        assert!(!got.reached);
        assert_eq!(
            got.eta_label,
            clock::date_label(&clock::add_date(&now, 0, 0, 28))
        );
        assert_eq!(got.stalled, "");

        // Moving away: stalled rather than a nonsense ETA.
        let mut entries = this_week;
        entries.push(entry(3, at("2026-08-08 07:00"), 81.0, ""));
        entries.push(entry(4, at("2026-08-06 07:00"), 81.0, ""));
        let got = build(&entries, &[goal(1, 78.0, "2026-01-01")], &now).goal;
        assert_eq!(got.eta_label, "");
        assert_eq!(got.stalled, "Not currently trending toward your goal.");
    }

    #[test]
    fn mean_kg_cases() {
        assert!(nearly_equal(mean_kg(&[80000, 82000, 84000]), 82.0));
        assert!(nearly_equal(mean_kg(&[81500]), 81.5));
        assert!(nearly_equal(mean_kg(&[80001, 80001, 80001]), 80.001));
    }
}

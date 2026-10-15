//! The weekly summary on the Log tab: this week's average morning weight
//! against last week's, where that rate leads relative to the goal, and
//! whether today's weigh-in is in.

use jiff::{Span, Zoned};

use crate::model::{Entry, Goal, Grams, Period, current_goal};

/// Mornings only: the fasted reading is the steadier one.
#[derive(Clone, Debug)]
pub struct Summary {
    pub today: Today,
    /// None when there is no morning this week to average.
    pub week: Option<Week>,
}

/// Whether the weigh-in for the current time of day is logged — shown even
/// on a tracker too new for a weekly comparison.
#[derive(Clone, Debug)]
pub struct Today {
    pub period: Period,
    pub logged: bool,
}

#[derive(Clone, Debug)]
pub struct Week {
    /// This week's average, in kg.
    pub average: f64,
    /// Last week's, when there was a morning to average.
    pub last_week: Option<f64>,
    pub goal: Option<GoalProgress>,
}

#[derive(Clone, Debug)]
pub struct GoalProgress {
    pub weight: Grams,
    pub outlook: Outlook,
}

#[derive(Clone, Debug, PartialEq)]
pub enum Outlook {
    Reached,
    /// `remaining` kg off, and when this week's rate would get there.
    Remaining {
        remaining: f64,
        eta: Eta,
    },
}

#[derive(Clone, Debug, PartialEq)]
pub enum Eta {
    By(Zoned),
    /// No week before this one to take a rate from.
    NoRateYet,
    /// The rate is flat, or heading away from the goal.
    NotTrending,
}

/// Within this of the goal counts as there, rather than "0.0 kg to go".
const REACHED_KG: f64 = 0.05;

/// The slowest weekly progress taken as a trend. Below it an ETA would be
/// years away — more precision than the data has.
const MIN_KG_PER_WEEK: f64 = 0.01;

pub fn summary(entries: &[Entry], goals: &[Goal], now: &Zoned) -> Summary {
    let period = Period::at(now);
    let today = Today {
        period,
        logged: entries.iter().any(|e| {
            e.period() == period
                && e.recorded_at.with_time_zone(now.time_zone().clone()).date() == now.date()
        }),
    };

    let week_ago = now.checked_sub(Span::new().days(7)).ok();
    let fortnight_ago = now.checked_sub(Span::new().days(14)).ok();
    let mornings = || entries.iter().filter(|e| e.period() == Period::Morning);
    let this_week: Vec<Grams> = mornings()
        .filter(|e| week_ago.as_ref().is_some_and(|w| e.recorded_at >= *w) && e.recorded_at <= *now)
        .map(|e| e.weight)
        .collect();
    let last_week: Vec<Grams> = mornings()
        .filter(|e| {
            fortnight_ago.as_ref().is_some_and(|f| e.recorded_at >= *f)
                && week_ago.as_ref().is_some_and(|w| e.recorded_at < *w)
        })
        .map(|e| e.weight)
        .collect();

    let week = average_kg(&this_week).map(|average| {
        let last_week = average_kg(&last_week);
        Week {
            average,
            last_week,
            goal: current_goal(goals, now).map(|g| progress(g.weight, average, last_week, now)),
        }
    });
    Summary { today, week }
}

/// Where this week's average is heading, relative to `goal`, at the rate it
/// moved since last week.
fn progress(goal: Grams, average: f64, last_week: Option<f64>, now: &Zoned) -> GoalProgress {
    let remaining = average - goal.kg();
    let outlook = if remaining.abs() <= REACHED_KG {
        Outlook::Reached
    } else {
        let eta = match last_week {
            None => Eta::NoRateYet,
            Some(last) => {
                // Above the goal it has to fall, below it rise: either way
                // this is positive only when moving towards it.
                let towards = (last - average) * remaining.signum();
                if towards <= MIN_KG_PER_WEEK {
                    Eta::NotTrending
                } else {
                    let days = (remaining.abs() / towards * 7.0).round() as i64;
                    Span::new()
                        .try_days(days)
                        .ok()
                        .and_then(|span| now.checked_add(span).ok())
                        .map_or(Eta::NotTrending, Eta::By)
                }
            }
        };
        Outlook::Remaining {
            remaining: remaining.abs(),
            eta,
        }
    };
    GoalProgress {
        weight: goal,
        outlook,
    }
}

/// The mean in kg, summed in whole grams so it is exact however many
/// readings there are.
fn average_kg(weights: &[Grams]) -> Option<f64> {
    (!weights.is_empty())
        .then(|| weights.iter().copied().sum::<Grams>().kg() / weights.len() as f64)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, entry, entry_as, goal, nearly_equal};

    fn week(entries: &[Entry], goals: &[Goal]) -> Option<Week> {
        summary(entries, goals, &at("2026-08-16 12:00")).week
    }

    #[test]
    fn compares_this_weeks_mornings_with_last_weeks() {
        let w = week(
            &[
                entry(1, "2026-08-15 07:00", 82.0),
                entry(2, "2026-08-13 07:00", 82.4),
                entry(3, "2026-08-08 07:00", 83.0),
                entry(4, "2026-08-06 07:00", 83.4),
                entry(5, "2026-08-15 21:00", 90.0), // an evening, ignored
                entry(6, "2026-06-01 07:00", 95.0), // older than either week
            ],
            &[],
        )
        .unwrap();
        assert!(nearly_equal(w.average, 82.2));
        assert!(nearly_equal(w.last_week.unwrap(), 83.2));
        assert!(nearly_equal(w.average - w.last_week.unwrap(), -1.0));
    }

    #[test]
    fn an_override_counts_as_its_period() {
        let w = week(
            &[entry_as(1, "2026-08-15 21:00", 82.0, Period::Morning)],
            &[],
        )
        .unwrap();
        assert!(nearly_equal(w.average, 82.0) && w.last_week.is_none());
    }

    #[test]
    fn nothing_this_week_is_no_week() {
        assert!(week(&[], &[]).is_none());
        assert!(
            week(&[entry(1, "2026-08-08 07:00", 83.0)], &[]).is_none(),
            "only last week"
        );
    }

    #[test]
    fn today() {
        let today = |entries: &[Entry], now| summary(entries, &[], &at(now)).today;
        let t = today(&[entry(1, "2026-08-15 21:00", 82.0)], "2026-08-16 08:00");
        assert_eq!((t.period, t.logged), (Period::Morning, false));
        assert!(today(&[entry(1, "2026-08-16 07:15", 82.0)], "2026-08-16 09:30").logged);
        // This morning's reading does not answer for this evening.
        let t = today(&[entry(1, "2026-08-16 07:15", 82.0)], "2026-08-16 20:00");
        assert_eq!((t.period, t.logged), (Period::Evening, false));
        assert!(!today(&[entry(1, "2026-08-15 07:15", 82.0)], "2026-08-16 08:00").logged);
    }

    #[test]
    fn goal_progress() {
        let this_week = vec![
            entry(1, "2026-08-15 07:00", 82.0),
            entry(2, "2026-08-13 07:00", 82.0),
        ];
        let outlook = |extra: &[Entry], g: Goal| {
            let mut entries = this_week.clone();
            entries.extend_from_slice(extra);
            week(&entries, &[g]).unwrap().goal.map(|p| p.outlook)
        };

        assert!(week(&this_week, &[]).unwrap().goal.is_none());
        assert!(
            outlook(&[], goal(1, 78.0, "2026-12-01")).is_none(),
            "not in effect yet"
        );
        assert_eq!(
            outlook(&[], goal(1, 82.02, "2026-01-01")),
            Some(Outlook::Reached)
        );
        assert_eq!(
            outlook(&[], goal(1, 78.0, "2026-01-01")),
            Some(Outlook::Remaining {
                remaining: 4.0,
                eta: Eta::NoRateYet
            })
        );

        // Last week 83.0, so a kilo a week, and 78.0 is four weeks off.
        let falling = [
            entry(3, "2026-08-08 07:00", 83.0),
            entry(4, "2026-08-06 07:00", 83.0),
        ];
        let Some(Outlook::Remaining {
            eta: Eta::By(when), ..
        }) = outlook(&falling, goal(1, 78.0, "2026-01-01"))
        else {
            panic!("no ETA");
        };
        assert_eq!(when.date().to_string(), "2026-09-13");

        let rising = [
            entry(3, "2026-08-08 07:00", 81.0),
            entry(4, "2026-08-06 07:00", 81.0),
        ];
        assert_eq!(
            outlook(&rising, goal(1, 78.0, "2026-01-01")),
            Some(Outlook::Remaining {
                remaining: 4.0,
                eta: Eta::NotTrending
            })
        );
    }

    #[test]
    fn averages_are_exact_in_grams() {
        assert!(nearly_equal(
            average_kg(&[Grams(80001); 3]).unwrap(),
            80.001
        ));
        assert!(average_kg(&[]).is_none());
    }
}

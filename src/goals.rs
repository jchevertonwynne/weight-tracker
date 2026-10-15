//! Goals as the goal list shows them, and as the stepped line the chart
//! draws.

use jiff::Zoned;

use crate::model::{Goal, Grams, current_goal};

#[derive(Clone, Debug)]
pub struct Row {
    pub goal: Goal,
    pub current: bool,
}

/// Goals as the list shows them, marking the one in effect.
pub fn rows(goals: &[Goal], now: &Zoned) -> Vec<Row> {
    let current = current_goal(goals, now).map(|g| g.id);
    goals
        .iter()
        .map(|g| Row {
            goal: g.clone(),
            current: current == Some(g.id),
        })
        .collect()
}

/// One goal's stretch of the chart: [from, until).
#[derive(Clone, Debug, PartialEq)]
pub struct Segment {
    pub weight: Grams,
    pub from: Zoned,
    pub until: Zoned,
}

/// The goal line across [from, until]: each goal from its own start until
/// the next one's, cut to the window. Stretches with nothing inside the
/// window are left out.
pub fn segments(goals: &[Goal], from: &Zoned, until: &Zoned) -> Vec<Segment> {
    let mut sorted: Vec<&Goal> = goals.iter().collect();
    sorted.sort_by_key(|g| g.effective_from.timestamp());
    let ends = sorted
        .iter()
        .skip(1)
        .map(|g| Some(&g.effective_from))
        .chain([None]);
    sorted
        .iter()
        .zip(ends)
        .filter_map(|(g, next)| {
            let start = (&g.effective_from).max(from);
            let end = next.map_or(until, |n| n.min(until));
            (start < end).then(|| Segment {
                weight: g.weight,
                from: start.clone(),
                until: end.clone(),
            })
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, goal};

    #[test]
    fn rows_mark_the_goal_in_effect() {
        let now = at("2026-08-16 12:00");
        let goals = [
            goal(3, 76.0, "2026-12-01"), // future
            goal(2, 78.0, "2026-08-01"), // current
            goal(1, 80.0, "2026-01-01"), // superseded
        ];
        let current: Vec<bool> = rows(&goals, &now).iter().map(|r| r.current).collect();
        assert_eq!(current, [false, true, false]);
        assert!(!rows(&goals[..1], &now)[0].current, "only a future goal");
    }

    #[test]
    fn segments_step_between_goals_within_the_window() {
        let goals = [goal(2, 78.0, "2026-06-01"), goal(1, 80.0, "2026-01-01")];
        let weights = |from, until| -> Vec<(i64, String, String)> {
            segments(&goals, &at(from), &at(until))
                .into_iter()
                .map(|s| {
                    (
                        s.weight.0,
                        s.from.strftime("%m-%d").to_string(),
                        s.until.strftime("%m-%d").to_string(),
                    )
                })
                .collect()
        };
        assert_eq!(
            weights("2026-05-01 00:00", "2026-08-16 00:00"),
            [
                (80000, "05-01".into(), "06-01".into()),
                (78000, "06-01".into(), "08-16".into())
            ]
        );
        assert_eq!(
            weights("2026-07-01 00:00", "2026-08-16 00:00"),
            [(78000, "07-01".into(), "08-16".into())]
        );
        assert!(weights("2025-01-01 00:00", "2025-06-01 00:00").is_empty());
        // A window ending where the first goal starts has nothing in it.
        assert!(weights("2025-06-01 00:00", "2026-01-01 00:00").is_empty());
        assert!(segments(&[], &at("2026-01-01 00:00"), &at("2026-02-01 00:00")).is_empty());
    }
}

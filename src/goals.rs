//! The display-ready form of a goal, the current in-effect goal, and the
//! time-bounded segments that draw the chart's goal line.

use jiff::Zoned;

use crate::db::Goal;
use crate::{clock, weight};

#[derive(Clone, Debug)]
pub struct Row {
    pub id: i64,
    pub weight_kg_raw: String,
    pub weight_kg_str: String,
    pub effective_from_label: String,
    /// For the edit form's date input.
    pub effective_from_date: String,
    pub current: bool,
}

/// The goal in effect at `now`. `goals` is newest first, as list_goals
/// returns it, so the first already in effect is the current one.
pub fn current<'a>(goals: &'a [Goal], now: &Zoned) -> Option<&'a Goal> {
    goals
        .iter()
        .find(|g| g.effective_from.timestamp() <= now.timestamp())
}

/// Assumes `goals` is newest first.
pub fn build_rows(goals: &[Goal], now: &Zoned) -> Vec<Row> {
    let current_id = current(goals, now).map(|g| g.id);
    goals
        .iter()
        .map(|g| Row {
            id: g.id,
            weight_kg_raw: weight::format_kg_input(g.weight_g),
            weight_kg_str: weight::format_kg(g.weight_g),
            effective_from_label: clock::date_label(&g.effective_from),
            effective_from_date: clock::ymd(&g.effective_from),
            current: current_id == Some(g.id),
        })
        .collect()
}

/// One goal's validity: [from, until), until None for the latest goal.
#[derive(Clone, Debug)]
pub struct Segment {
    pub weight_g: i64,
    pub from: Zoned,
    pub until: Option<Zoned>,
}

/// Goals (any order) as non-overlapping segments, each valid from its own
/// effective_from until the next one's.
pub fn build_segments(goals: &[Goal]) -> Vec<Segment> {
    let mut sorted = goals.to_vec();
    sorted.sort_by(|a, b| {
        a.effective_from
            .timestamp()
            .cmp(&b.effective_from.timestamp())
    });
    let nexts: Vec<Option<Zoned>> = sorted
        .iter()
        .skip(1)
        .map(|g| Some(g.effective_from.clone()))
        .chain([None])
        .collect();
    sorted
        .into_iter()
        .zip(nexts)
        .map(|(g, until)| Segment {
            weight_g: g.weight_g,
            from: g.effective_from,
            until,
        })
        .collect()
}

/// The part of each segment overlapping [from, until], the open-ended one
/// closed off at `until`. Segments with no overlap are dropped.
pub fn clip_segments(segs: &[Segment], from: &Zoned, until: &Zoned) -> Vec<Segment> {
    segs.iter()
        .filter_map(|s| {
            let seg_until = match &s.until {
                Some(u) if u.timestamp() <= until.timestamp() => u.clone(),
                _ => until.clone(),
            };
            let seg_from = if s.from.timestamp() < from.timestamp() {
                from.clone()
            } else {
                s.from.clone()
            };
            (seg_from.timestamp() < seg_until.timestamp()).then(|| Segment {
                weight_g: s.weight_g,
                from: seg_from,
                until: Some(seg_until),
            })
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, goal};

    #[test]
    fn build_rows_cases() {
        let now = at("2026-08-16 12:00");
        let goals = [
            goal(3, 76.0, "2026-12-01"), // future
            goal(2, 78.0, "2026-08-01"), // current
            goal(1, 80.0, "2026-01-01"), // superseded
        ];
        let rows = build_rows(&goals, &now);
        assert_eq!(
            rows.iter().map(|r| r.current).collect::<Vec<_>>(),
            [false, true, false]
        );
        assert_eq!(rows[1].weight_kg_str, "78.0");
        assert_eq!(rows[1].effective_from_label, "Aug 1, 2026");
        assert_eq!(rows[1].effective_from_date, "2026-08-01");

        let rows = build_rows(
            &[goal(2, 78.0, "2026-08-01"), goal(1, 80.0, "2026-07-01")],
            &now,
        );
        assert_eq!(rows.iter().filter(|r| r.current).count(), 1);

        let rows = build_rows(&[goal(1, 76.0, "2026-12-01")], &now);
        assert!(
            !rows[0].current,
            "a goal not yet in effect is marked current"
        );
    }

    #[test]
    fn build_segments_cases() {
        let segs = build_segments(&[
            goal(2, 78.0, "2026-06-01"),
            goal(1, 80.0, "2026-01-01"),
            goal(3, 76.0, "2026-09-01"),
        ]);
        assert_eq!(segs.len(), 3);
        assert_eq!(segs[0].weight_g, 80000);
        assert_eq!(
            segs[0].until.as_ref().unwrap().timestamp(),
            segs[1].from.timestamp()
        );
        assert!(segs[2].until.is_none());
        assert!(build_segments(&[]).is_empty());
        let segs = build_segments(&[goal(1, 78.0, "2026-01-01")]);
        assert!(segs.len() == 1 && segs[0].until.is_none());
    }

    #[test]
    fn clip_segments_cases() {
        let segs = build_segments(&[goal(1, 80.0, "2026-01-01"), goal(2, 78.0, "2026-06-01")]);

        let (from, until) = (at("2026-05-01 00:00"), at("2026-08-16 00:00"));
        let got = clip_segments(&segs, &from, &until);
        assert_eq!(got.len(), 2);
        assert_eq!(got[0].from.timestamp(), from.timestamp());
        assert_eq!(
            got[1].until.as_ref().unwrap().timestamp(),
            until.timestamp()
        );

        let got = clip_segments(&segs, &at("2026-07-01 00:00"), &at("2026-08-16 00:00"));
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].weight_g, 78000);

        assert!(clip_segments(&segs, &at("2025-01-01 00:00"), &at("2025-06-01 00:00")).is_empty());

        // The range ends exactly where the first segment starts.
        for s in clip_segments(&segs, &at("2025-06-01 00:00"), &at("2026-01-01 00:00")) {
            assert!(
                s.from.timestamp() < s.until.unwrap().timestamp(),
                "zero-width segment"
            );
        }
    }
}

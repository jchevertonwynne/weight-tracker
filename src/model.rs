//! The things the app keeps: weigh-ins, goals and markers, and the two types
//! everything about them is measured in.

use std::fmt;
use std::iter::Sum;
use std::ops::Sub;
use std::str::FromStr;

use jiff::Zoned;

/// A weight, or a difference between two, in whole grams.
///
/// Weights are stored as grams rather than floating-point kilograms: a
/// binary float cannot hold most one-decimal kilogram values exactly, so
/// 82.4 came back as 82.400000000000006, and a unit conversion on import
/// (180 lb to 81.64662660000001 kg) made it worse. Grams are exact and finer
/// than any bathroom scale; kilograms are only for display and for the float
/// arithmetic (averages, the trend) that the chart needs.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct Grams(pub i64);

impl Grams {
    /// Kilograms to the nearest gram — the only place a weight loses
    /// precision, and it loses none a scale provides.
    pub fn from_kg(kg: f64) -> Self {
        Grams((kg * 1000.0).round() as i64)
    }

    pub fn kg(self) -> f64 {
        self.0 as f64 / 1000.0
    }

    pub fn is_loss(self) -> bool {
        self.0 < 0
    }

    /// Every stored digit, in the shortest form that reads back exactly, for
    /// a number input: re-saving an unedited row must not round an imported
    /// 82.437 to the 82.4 on display.
    pub fn input(self) -> String {
        self.kg().to_string()
    }

    /// As a signed change, "+0.7 kg".
    pub fn delta(self) -> Delta {
        Delta(self)
    }
}

/// Kilograms to one decimal, "82.4" — the precision a scale reports.
impl fmt::Display for Grams {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{:.1}", self.kg())
    }
}

impl Sub for Grams {
    type Output = Grams;
    fn sub(self, rhs: Grams) -> Grams {
        Grams(self.0 - rhs.0)
    }
}

impl Sum for Grams {
    fn sum<I: Iterator<Item = Grams>>(iter: I) -> Grams {
        Grams(iter.map(|g| g.0).sum())
    }
}

/// A [`Grams`] difference shown with its sign: "-1.1 kg", "+0.0 kg".
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Delta(pub Grams);

impl fmt::Display for Delta {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{:+.1} kg", self.0.kg())
    }
}

/// Which weigh-in of the day a reading is.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Period {
    Morning,
    Evening,
}

impl Period {
    /// The period a clock time falls in. Morning runs from 4am to noon: a
    /// weigh-in between midnight and 4am is still last night, not a fresh
    /// morning.
    pub fn at(t: &Zoned) -> Period {
        if (4..12).contains(&t.hour()) {
            Period::Morning
        } else {
            Period::Evening
        }
    }

    /// How it is stored, and how forms and query strings spell it.
    pub fn as_str(self) -> &'static str {
        match self {
            Period::Morning => "morning",
            Period::Evening => "evening",
        }
    }

    pub fn label(self) -> &'static str {
        match self {
            Period::Morning => "Morning",
            Period::Evening => "Evening",
        }
    }
}

impl fmt::Display for Period {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

#[derive(Debug, PartialEq, Eq, thiserror::Error)]
#[error("a period is \"morning\" or \"evening\"")]
pub struct UnknownPeriod;

impl FromStr for Period {
    type Err = UnknownPeriod;
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "morning" => Ok(Period::Morning),
            "evening" => Ok(Period::Evening),
            _ => Err(UnknownPeriod),
        }
    }
}

/// A weigh-in.
#[derive(Clone, Debug)]
pub struct Entry {
    pub id: i64,
    pub recorded_at: Zoned,
    pub weight: Grams,
    /// Set when the user moved a reading near the boundary into the other
    /// period; otherwise the period follows the clock.
    pub period_override: Option<Period>,
}

impl Entry {
    pub fn period(&self) -> Period {
        self.period_override
            .unwrap_or_else(|| Period::at(&self.recorded_at))
    }
}

/// A target weight, in effect from a date until the next goal's.
#[derive(Clone, Debug)]
pub struct Goal {
    pub id: i64,
    pub weight: Grams,
    pub effective_from: Zoned,
}

/// A dated note drawn on the chart, giving context for why the weight moved
/// ("started new diet").
#[derive(Clone, Debug)]
pub struct Marker {
    pub id: i64,
    pub date: Zoned,
    pub note: String,
}

/// The goal in effect at `now`, from goals in any order.
pub fn current_goal<'a>(goals: &'a [Goal], now: &Zoned) -> Option<&'a Goal> {
    goals
        .iter()
        .filter(|g| g.effective_from <= *now)
        .max_by_key(|g| g.effective_from.timestamp())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::at;

    #[test]
    fn grams_and_kilograms() {
        for (kg, g) in [
            (82.0, 82000),
            (82.4, 82400),
            (81.647, 81647),
            (81.6465, 81647), // half a gram rounds up
            (81.64649, 81646),
            (180.0 * 0.45359237, 81647), // a float with representation error
            (0.001, 1),
        ] {
            assert_eq!(Grams::from_kg(kg), Grams(g), "{kg}");
        }
        // Grams are canonical, so grams -> kg -> grams must hold.
        for g in [1, 999, 82000, 82400, 81647, 250000] {
            assert_eq!(Grams::from_kg(Grams(g).kg()), Grams(g));
        }
    }

    #[test]
    fn formats() {
        for (g, want) in [
            (82400, "82.4"),
            (82000, "82.0"),
            (81647, "81.6"),
            (81650, "81.7"),
            (0, "0.0"),
        ] {
            assert_eq!(Grams(g).to_string(), want);
        }
        for (g, want) in [
            (-1100, "-1.1 kg"),
            (700, "+0.7 kg"),
            (0, "+0.0 kg"),
            (-50, "-0.1 kg"),
        ] {
            assert_eq!(Grams(g).delta().to_string(), want);
        }
        // The edit form round-trips the stored value exactly.
        for (g, want) in [
            (82400, "82.4"),
            (82000, "82"),
            (81647, "81.647"),
            (81600, "81.6"),
            (1, "0.001"),
        ] {
            assert_eq!(Grams(g).input(), want);
            assert_eq!(Grams::from_kg(want.parse().unwrap()), Grams(g));
        }
    }

    #[test]
    fn period_at() {
        for (t, want) in [
            ("2026-08-16 00:00", Period::Evening), // midnight is still last night
            ("2026-08-16 03:59", Period::Evening),
            ("2026-08-16 04:00", Period::Morning),
            ("2026-08-16 11:59", Period::Morning),
            ("2026-08-16 12:00", Period::Evening),
            ("2026-08-16 23:59", Period::Evening),
        ] {
            assert_eq!(Period::at(&at(t)), want, "{t}");
        }
        assert_eq!("morning".parse(), Ok(Period::Morning));
        for bad in ["", "Morning", "noon", " "] {
            assert!(bad.parse::<Period>().is_err(), "{bad:?}");
        }
    }

    #[test]
    fn an_override_wins() {
        let e = |t, o| Entry {
            id: 1,
            recorded_at: at(t),
            weight: Grams(80000),
            period_override: o,
        };
        assert_eq!(e("2026-08-16 07:30", None).period(), Period::Morning);
        assert_eq!(
            e("2026-08-16 07:30", Some(Period::Evening)).period(),
            Period::Evening
        );
        assert_eq!(
            e("2026-08-16 21:00", Some(Period::Morning)).period(),
            Period::Morning
        );
    }

    #[test]
    fn current_goal_is_the_latest_in_effect() {
        let g = |id, from: &str| Goal {
            id,
            weight: Grams(80000),
            effective_from: at(&format!("{from} 00:00")),
        };
        let goals = [g(3, "2026-12-01"), g(1, "2026-01-01"), g(2, "2026-08-01")];
        let now = at("2026-08-16 12:00");
        assert_eq!(current_goal(&goals, &now).map(|g| g.id), Some(2));
        assert!(
            current_goal(&goals[..1], &now).is_none(),
            "only a future goal"
        );
    }
}

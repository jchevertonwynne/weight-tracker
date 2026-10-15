//! Fixtures for the tests.

use jiff::Zoned;

use crate::clock;
use crate::model::{Entry, Goal, Grams, Marker, Period};

/// A local wall-clock time, "2026-08-16 07:30".
pub fn at(s: &str) -> Zoned {
    clock::parse(s, "%Y-%m-%d %H:%M", &clock::tz()).unwrap_or_else(|| panic!("a test time: {s:?}"))
}

pub fn entry(id: i64, recorded_at: &str, kg: f64) -> Entry {
    Entry {
        id,
        recorded_at: at(recorded_at),
        weight: Grams::from_kg(kg),
        period_override: None,
    }
}

pub fn entry_as(id: i64, recorded_at: &str, kg: f64, period: Period) -> Entry {
    Entry {
        period_override: Some(period),
        ..entry(id, recorded_at, kg)
    }
}

/// A goal from midnight on `from` ("2026-08-01").
pub fn goal(id: i64, kg: f64, from: &str) -> Goal {
    Goal {
        id,
        weight: Grams::from_kg(kg),
        effective_from: at(&format!("{from} 00:00")),
    }
}

/// A marker on midnight of `date`.
pub fn marker(id: i64, date: &str, note: &str) -> Marker {
    Marker {
        id,
        date: at(&format!("{date} 00:00")),
        note: note.to_string(),
    }
}

pub fn nearly_equal(a: f64, b: f64) -> bool {
    (a - b).abs() < 1e-9
}

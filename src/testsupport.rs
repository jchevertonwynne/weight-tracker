//! Fixtures shared by the domain tests, as internal/testsupport was.

use jiff::Zoned;

use crate::db::{self, Entry, Goal, Marker};

/// A local wall-clock time, "2006-01-02 15:04".
pub fn at(s: &str) -> Zoned {
    crate::clock::tests::at(s)
}

pub fn entry(id: i64, recorded_at: Zoned, weight_kg: f64, period_override: &str) -> Entry {
    Entry {
        id,
        recorded_at,
        weight_g: db::kg_to_grams(weight_kg),
        period_override: period_override.to_string(),
    }
}

/// A goal effective from midnight on `effective_from` ("2006-01-02").
pub fn goal(id: i64, weight_kg: f64, effective_from: &str) -> Goal {
    let from = at(&format!("{effective_from} 00:00"));
    Goal {
        id,
        weight_g: db::kg_to_grams(weight_kg),
        effective_from: from,
    }
}

/// A marker on midnight of `date` ("2006-01-02").
pub fn marker(id: i64, date: &str, note: &str) -> Marker {
    let date = at(&format!("{date} 00:00"));
    Marker {
        id,
        date,
        note: note.to_string(),
    }
}

pub fn nearly_equal(a: f64, b: f64) -> bool {
    (a - b).abs() < 1e-9
}

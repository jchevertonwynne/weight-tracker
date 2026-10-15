//! The morning/evening period logic, the overnight/daily delta engine built
//! on it, and the kilogram formatting every other module shares.

use std::collections::HashMap;

use jiff::Zoned;
use jiff::civil::Date;

use crate::db::{self, Entry};

/// An entry's effective period: its override if the user set one, otherwise
/// detected from recorded_at.
pub fn entry_period(e: &Entry) -> &str {
    if e.period_override.is_empty() {
        db::detect_period(&e.recorded_at)
    } else {
        &e.period_override
    }
}

/// Whether `s` is a legal period_override: "" (auto), "morning" or
/// "evening".
pub fn valid_period_override(s: &str) -> bool {
    matches!(s, "" | "morning" | "evening")
}

/// Entries sorted by recorded_at, and two deltas keyed by entry id, in grams:
///
/// - overnight: a morning's weight against the most recent evening, but only
///   if that evening was the preceding logical day — otherwise there is a
///   gap (no evening logged in between) and the two are not "overnight" from
///   each other.
/// - daily: an evening's weight against the most recent morning on the same
///   logical day, i.e. weight gained that day.
///
/// "Logical day" (see [`logical_date`]) rather than calendar date, since the
/// morning cutoff is 4am: a 1:25am reading is "evening" but shares its
/// calendar date with the morning a few hours later.
pub fn chronological_with_deltas(
    entries: &[Entry],
) -> (Vec<Entry>, HashMap<i64, i64>, HashMap<i64, i64>) {
    let mut chrono = entries.to_vec();
    // Stable, as sort.SliceStable: equal timestamps keep their input order.
    chrono.sort_by_key(|e| e.recorded_at.timestamp());

    let mut overnight = HashMap::new();
    let mut daily = HashMap::new();
    let mut last_morning: Option<&Entry> = None;
    let mut last_evening: Option<&Entry> = None;
    for e in &chrono {
        match entry_period(e) {
            "morning" => {
                if let Some(ev) = last_evening
                    && is_next_calendar_day(&ev.recorded_at, &e.recorded_at)
                {
                    overnight.insert(e.id, e.weight_g - ev.weight_g);
                }
                last_morning = Some(e);
            }
            "evening" => {
                if let Some(m) = last_morning
                    && same_date(&m.recorded_at, &e.recorded_at)
                {
                    daily.insert(e.id, e.weight_g - m.weight_g);
                }
                last_evening = Some(e);
            }
            _ => {}
        }
    }
    (chrono, overnight, daily)
}

/// The calendar date `t` counts as for same-day and next-day comparisons,
/// on the same 4am boundary as the morning cutoff: before 4am is still last
/// night.
pub fn logical_date(t: &Zoned) -> Date {
    let date = t.date();
    if t.hour() < 4 {
        date.yesterday().expect("a date in range")
    } else {
        date
    }
}

fn same_date(a: &Zoned, b: &Zoned) -> bool {
    logical_date(a) == logical_date(b)
}

/// Whether `b` falls on the logical day straight after `a`'s — a genuine
/// "yesterday evening to this morning" adjacency.
fn is_next_calendar_day(a: &Zoned, b: &Zoned) -> bool {
    logical_date(a).tomorrow().ok() == Some(logical_date(b))
}

/// Stored grams as the one-decimal kilogram string the UI shows — the
/// precision a bathroom scale reports.
pub fn format_kg(grams: i64) -> String {
    format!("{:.1}", db::grams_to_kg(grams))
}

/// A gram difference as a signed kilogram string.
pub fn format_kg_delta(grams: i64) -> String {
    format!("{:+.1} kg", db::grams_to_kg(grams))
}

/// Stored grams for a number input, keeping every digit stored (at most
/// three decimals), so re-saving an unedited row cannot round it: a
/// display-rounded 82.4 would otherwise overwrite an imported 82.437. The
/// shortest form that reads back exactly, as strconv's -1 precision gave.
pub fn format_kg_input(grams: i64) -> String {
    format!("{}", db::grams_to_kg(grams))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, entry};

    #[test]
    fn entry_period_cases() {
        for (name, t, o, want) in [
            (
                "mid-morning auto-detects morning",
                "2026-08-16 07:30",
                "",
                "morning",
            ),
            (
                "4am is the first morning minute",
                "2026-08-16 04:00",
                "",
                "morning",
            ),
            (
                "3:59am is still last night",
                "2026-08-16 03:59",
                "",
                "evening",
            ),
            ("noon flips to evening", "2026-08-16 12:00", "", "evening"),
            (
                "11:59am is still morning",
                "2026-08-16 11:59",
                "",
                "morning",
            ),
            (
                "override wins over detection",
                "2026-08-16 07:30",
                "evening",
                "evening",
            ),
            (
                "override wins the other way too",
                "2026-08-16 21:00",
                "morning",
                "morning",
            ),
        ] {
            assert_eq!(entry_period(&entry(1, at(t), 80.0, o)), want, "{name}");
        }
    }

    #[test]
    fn valid_period_override_cases() {
        for s in ["", "morning", "evening"] {
            assert!(valid_period_override(s), "{s:?}");
        }
        for s in ["noon", "Morning", "afternoon", " "] {
            assert!(!valid_period_override(s), "{s:?}");
        }
    }

    #[test]
    fn logical_date_cases() {
        for (name, t, want) in [
            (
                "midday belongs to its own date",
                "2026-08-16 12:30",
                "2026-08-16",
            ),
            (
                "late evening belongs to its own date",
                "2026-08-16 23:45",
                "2026-08-16",
            ),
            (
                "1am belongs to the previous date",
                "2026-08-16 01:25",
                "2026-08-15",
            ),
            (
                "3:59am still belongs to the previous date",
                "2026-08-16 03:59",
                "2026-08-15",
            ),
            ("4am starts the new date", "2026-08-16 04:00", "2026-08-16"),
            (
                "pre-4am across a month boundary",
                "2026-09-01 02:00",
                "2026-08-31",
            ),
            (
                "pre-4am across a year boundary",
                "2026-01-01 00:30",
                "2025-12-31",
            ),
        ] {
            assert_eq!(logical_date(&at(t)).to_string(), want, "{name}");
        }
    }

    #[test]
    fn same_date_and_next_calendar_day() {
        for (name, a, b, same, next) in [
            (
                "same day, morning then evening",
                "2026-08-16 07:00",
                "2026-08-16 21:00",
                true,
                false,
            ),
            (
                "1am counts as the previous day's evening",
                "2026-08-16 01:25",
                "2026-08-15 22:00",
                true,
                false,
            ),
            (
                "consecutive days",
                "2026-08-15 21:00",
                "2026-08-16 07:00",
                false,
                true,
            ),
            (
                "1:25am evening then that morning is consecutive",
                "2026-08-16 01:25",
                "2026-08-16 07:15",
                false,
                true,
            ),
            (
                "two-day gap is neither",
                "2026-08-14 21:00",
                "2026-08-16 07:00",
                false,
                false,
            ),
            (
                "reversed order is not next-day",
                "2026-08-16 07:00",
                "2026-08-15 21:00",
                false,
                false,
            ),
            (
                "month boundary is consecutive",
                "2026-08-31 21:00",
                "2026-09-01 07:00",
                false,
                true,
            ),
        ] {
            let (a, b) = (at(a), at(b));
            assert_eq!(same_date(&a, &b), same, "{name}: same");
            assert_eq!(is_next_calendar_day(&a, &b), next, "{name}: next");
        }
    }

    fn deltas(entries: &[Entry]) -> (HashMap<i64, i64>, HashMap<i64, i64>) {
        let (_, o, d) = chronological_with_deltas(entries);
        (o, d)
    }

    #[test]
    fn chronological_with_deltas_cases() {
        // Sorts ascending regardless of input order, without touching the
        // caller's slice.
        let entries = [
            entry(3, at("2026-08-16 07:15"), 82.0, ""),
            entry(1, at("2026-08-15 07:30"), 82.4, ""),
            entry(2, at("2026-08-15 21:00"), 83.1, ""),
        ];
        let (chrono, _, _) = chronological_with_deltas(&entries);
        assert_eq!(chrono.iter().map(|e| e.id).collect::<Vec<_>>(), [1, 2, 3]);
        assert_eq!(entries[0].id, 3);

        // Overnight pairs an evening with the next morning.
        let (o, d) = deltas(&[
            entry(1, at("2026-08-15 21:00"), 83.1, ""),
            entry(2, at("2026-08-16 07:15"), 82.0, ""),
        ]);
        assert_eq!((o.get(&2), d.len()), (Some(&-1100), 0));

        // Daily pairs a morning with that same day's evening.
        let (o, d) = deltas(&[
            entry(1, at("2026-08-15 07:30"), 82.4, ""),
            entry(2, at("2026-08-15 21:00"), 83.1, ""),
        ]);
        assert_eq!((d.get(&2), o.len()), (Some(&700), 0));

        // A 1:25am evening pairs with the morning hours later — a literal
        // same-date comparison missed this.
        let (o, _) = deltas(&[
            entry(1, at("2026-08-16 01:25"), 83.5, ""),
            entry(2, at("2026-08-16 07:15"), 82.6, ""),
        ]);
        assert_eq!(o.get(&2), Some(&-900));

        // A gap suppresses both.
        let (o, _) = deltas(&[
            entry(1, at("2026-08-13 21:00"), 83.1, ""),
            entry(2, at("2026-08-16 07:15"), 82.0, ""),
        ]);
        assert!(o.is_empty());
        let (_, d) = deltas(&[
            entry(1, at("2026-08-15 07:30"), 82.4, ""),
            entry(2, at("2026-08-16 21:00"), 83.1, ""),
        ]);
        assert!(d.is_empty());

        // An override redirects the pairing.
        let (o, _) = deltas(&[
            entry(1, at("2026-08-15 09:00"), 83.0, "evening"),
            entry(2, at("2026-08-16 07:15"), 82.0, ""),
        ]);
        assert_eq!(o.get(&2), Some(&-1000));

        // A full multi-day run pairs every adjacent reading.
        let (o, d) = deltas(&[
            entry(1, at("2026-08-14 07:00"), 84.0, ""),
            entry(2, at("2026-08-14 21:00"), 85.0, ""),
            entry(3, at("2026-08-15 07:00"), 83.5, ""),
            entry(4, at("2026-08-15 21:00"), 84.2, ""),
            entry(5, at("2026-08-16 07:00"), 83.0, ""),
        ]);
        assert_eq!(o, HashMap::from([(3, -1500), (5, -1200)]));
        assert_eq!(d, HashMap::from([(2, 1000), (4, 700)]));

        let (chrono, o, d) = chronological_with_deltas(&[]);
        assert!(chrono.is_empty() && o.is_empty() && d.is_empty());
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
            assert_eq!(format_kg(g), want);
        }
        for (g, want) in [
            (-1100, "-1.1 kg"),
            (700, "+0.7 kg"),
            (0, "+0.0 kg"),
            (-50, "-0.1 kg"),
        ] {
            assert_eq!(format_kg_delta(g), want);
        }
        // The edit form round-trips the stored value exactly.
        for (g, want) in [
            (82400, "82.4"),
            (82000, "82"),
            (81647, "81.647"),
            (81600, "81.6"),
            (1, "0.001"),
        ] {
            let got = format_kg_input(g);
            assert_eq!(got, want);
            assert_eq!(db::kg_to_grams(got.parse().unwrap()), g);
        }
    }
}

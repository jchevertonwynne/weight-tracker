//! Parses weight-log CSV exports from other sources into rows to bulk-insert.
//! Confirmed against a real Google Health (Takeout) export — header
//! `timestamp,weight grams,data source`, UTC timestamps, integer grams. The
//! split date+time "Fitbit CSV" path remains a best-effort, unverified
//! fallback for a plain, ambiguous `weight` column.

use std::collections::HashMap;
use std::io::Read;

use anyhow::{Result, anyhow, bail};
use jiff::{Timestamp, Zoned};

use crate::clock;

pub struct ParsedRow {
    pub recorded_at: Zoned,
    /// Whole grams, rounded from whatever unit the file used.
    pub weight_g: i64,
}

#[derive(Clone, Debug)]
pub struct SkippedRow {
    pub line_number: usize,
    pub reason: String,
}

#[derive(Default)]
pub struct Parsed {
    pub rows: Vec<ParsedRow>,
    pub skipped: Vec<SkippedRow>,
}

const LB_TO_KG: f64 = 0.45359237;
const G_TO_KG: f64 = 0.001;

/// Recognised weight columns in priority order, with the unit the header
/// itself declares. An empty unit is the ambiguous bare "weight" column, for
/// which the form's unit applies — self-describing data beats a guess picked
/// from a select.
const WEIGHT_COLUMNS: [(&str, &str); 5] = [
    ("weight_kg", "kg"),   // this app's own export
    ("weight grams", "g"), // Google Health (Takeout)
    ("weight_grams", "g"), // tolerate the underscore variant
    ("weight_lb", "lb"),
    ("weight", ""), // ambiguous: the form's unit
];

fn to_kg(value: f64, unit: &str) -> Result<f64> {
    match unit {
        "kg" => Ok(value),
        "lb" => Ok(value * LB_TO_KG),
        "g" => Ok(value * G_TO_KG),
        _ => bail!("unrecognized weight unit {unit:?}"),
    }
}

/// Formats tried, in order, against one combined date+time column (this
/// app's own recorded_at included). RFC 3339, carrying its own offset, is
/// tried first and keeps that instant.
const COMBINED_FORMATS: [&str; 4] = [
    "%Y-%m-%dT%H:%M:%S",
    "%Y-%m-%d %H:%M:%S",
    "%m/%d/%y %H:%M:%S",
    "%m/%d/%Y %H:%M:%S",
];

/// Tried when date and time are separate columns, as Fitbit's export had
/// them.
const DATE_FORMATS: [&str; 3] = ["%m/%d/%y", "%m/%d/%Y", "%Y-%m-%d"];
const TIME_FORMATS: [&str; 4] = ["%H:%M:%S", "%H:%M", "%I:%M:%S %p", "%I:%M %p"];

/// Reads a weight CSV: Google Health, this app's own export, or a
/// Fitbit-style split date and time. `unit` ("kg", "lb" or "g") applies only
/// when the weight column's header does not declare its own. Bad rows are
/// skipped and reported rather than aborting the import.
pub fn parse(r: impl Read, unit: &str) -> Result<Parsed> {
    if !matches!(unit, "kg" | "lb" | "g") {
        bail!("invalid unit {unit:?}");
    }
    let mut reader = csv::ReaderBuilder::new()
        .has_headers(false)
        // Ragged rows are tolerated, and skipped one by one below.
        .flexible(true)
        .from_reader(r);
    let mut records = reader.records();

    let header = match records.next() {
        Some(Ok(h)) => h,
        Some(Err(e)) => return Err(anyhow!("read header row: {e}")),
        None => bail!("read header row: EOF"),
    };
    let header: Vec<String> = header.iter().map(str::to_string).collect();
    let cols: HashMap<String, usize> = header
        .iter()
        .enumerate()
        .map(|(i, h)| (h.trim().to_lowercase(), i))
        .collect();

    let Some((weight_idx, implied_unit)) = WEIGHT_COLUMNS
        .iter()
        .find_map(|(name, u)| cols.get(*name).map(|&i| (i, *u)))
    else {
        bail!("unrecognized CSV header, no weight column found: {header:?}");
    };
    let effective_unit = if implied_unit.is_empty() {
        unit
    } else {
        implied_unit
    };
    let first_present = |names: &[&str]| names.iter().find_map(|n| cols.get(*n).copied());
    let datetime_idx = first_present(&["recorded_at", "date time", "datetime", "timestamp"]);
    let date_idx = first_present(&["date"]);
    let time_idx = first_present(&["time"]);
    if datetime_idx.is_none() && !(date_idx.is_some() && time_idx.is_some()) {
        bail!("unrecognized CSV header, no date/time column found: {header:?}");
    }

    let tz = clock::tz();
    let mut result = Parsed::default();
    // 1-based, and the header was line 1.
    for (line_number, record) in (2..).zip(records) {
        let skip = |reason: String| SkippedRow {
            line_number,
            reason,
        };
        let record = match record {
            Ok(r) => r,
            Err(e) => {
                result.skipped.push(skip(e.to_string()));
                continue;
            }
        };
        let field = |idx: Option<usize>| idx.and_then(|i| record.get(i)).unwrap_or_default();

        let recorded_at = match datetime_idx {
            Some(i) => parse_combined(field(Some(i)), &tz),
            None => parse_split(field(date_idx), field(time_idx), &tz),
        };
        let Some(recorded_at) = recorded_at else {
            result.skipped.push(skip("invalid date/time".into()));
            continue;
        };

        let weight = field(Some(weight_idx))
            .trim()
            .parse::<f64>()
            .ok()
            .filter(|w| w.is_finite() && *w > 0.0)
            .and_then(|w| to_kg(w, effective_unit).ok());
        let Some(weight_kg) = weight else {
            result.skipped.push(skip("invalid weight".into()));
            continue;
        };
        result.rows.push(ParsedRow {
            recorded_at,
            weight_g: crate::db::kg_to_grams(weight_kg),
        });
    }
    Ok(result)
}

/// A combined column. A timestamp carrying its own zone (Google Health's
/// UTC "Z") keeps its instant and is then read in the local zone, so every
/// entry is represented as manually entered ones are.
fn parse_combined(value: &str, tz: &jiff::tz::TimeZone) -> Option<Zoned> {
    let value = value.trim();
    if value.contains('T')
        && let Ok(ts) = value.parse::<Timestamp>()
    {
        return Some(ts.to_zoned(tz.clone()));
    }
    COMBINED_FORMATS
        .iter()
        .find_map(|f| clock::parse_in(value, f, tz))
}

fn parse_split(date: &str, time: &str, tz: &jiff::tz::TimeZone) -> Option<Zoned> {
    let combined = format!("{} {}", date.trim(), time.trim());
    DATE_FORMATS.iter().find_map(|d| {
        TIME_FORMATS
            .iter()
            .find_map(|t| clock::parse_in(&combined, &format!("{d} {t}"), tz))
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::at;

    fn parse_ok(csv: &str, unit: &str) -> Parsed {
        parse(csv.as_bytes(), unit).expect("parse")
    }

    #[test]
    fn rejects_invalid_unit() {
        for unit in ["", "stone", "KG", "pounds"] {
            assert!(parse("weight\n80\n".as_bytes(), unit).is_err(), "{unit:?}");
        }
    }

    #[test]
    fn google_health_export() {
        let csv = "timestamp,weight grams,data source\n2026-08-14T06:00:00Z,81500,Some Scale\n2026-08-15T06:30:00Z,81200,Some Scale\n";
        // The form's unit is deliberately wrong; the header must win.
        let got = parse_ok(csv, "lb");
        assert_eq!((got.rows.len(), got.skipped.len()), (2, 0));
        assert_eq!(got.rows[0].weight_g, 81500);
        assert_eq!(
            got.rows[0].recorded_at.timestamp().to_string(),
            "2026-08-14T06:00:00Z"
        );
        assert_eq!(got.rows[0].recorded_at.time_zone(), &clock::tz());
    }

    #[test]
    fn apps_own_export() {
        let got = parse_ok(
            "recorded_at,weight_kg\n2026-08-14T07:00:00+01:00,81.5\n",
            "lb",
        );
        assert_eq!(got.rows.len(), 1);
        assert_eq!(got.rows[0].weight_g, 81500);
    }

    #[test]
    fn ambiguous_weight_column_uses_the_form_unit() {
        let csv = "recorded_at,weight\n2026-08-14T07:00:00Z,180\n";
        assert_eq!(parse_ok(csv, "kg").rows[0].weight_g, 180000);
        assert_eq!(parse_ok(csv, "lb").rows[0].weight_g, 81647);
        assert_eq!(parse_ok(csv, "g").rows[0].weight_g, 180);
    }

    #[test]
    fn weight_column_priority() {
        let got = parse_ok(
            "recorded_at,weight,weight_kg\n2026-08-14T07:00:00Z,999,81.5\n",
            "lb",
        );
        assert_eq!(got.rows[0].weight_g, 81500);
    }

    #[test]
    fn split_date_and_time_columns() {
        let got = parse_ok("Date,Time,Weight\n08/14/26,07:00:00,180\n", "lb");
        assert_eq!(got.rows.len(), 1, "{:?}", got.skipped);
        assert_eq!(got.rows[0].weight_g, 81647);
        let t = &got.rows[0].recorded_at;
        assert_eq!(
            (t.year(), t.month(), t.day()),
            (2026, 8, 14),
            "MM/DD/YY ordering"
        );
    }

    #[test]
    fn split_date_time_layout_variants() {
        for (name, date, time) in [
            ("two-digit year", "08/14/26", "07:00:00"),
            ("four-digit year", "08/14/2026", "07:00:00"),
            ("ISO date", "2026-08-14", "07:00:00"),
            ("no seconds", "2026-08-14", "07:00"),
            ("12-hour clock", "2026-08-14", "7:00:00 AM"),
            ("12-hour clock without seconds", "2026-08-14", "7:00 AM"),
        ] {
            let got = parse_ok(&format!("Date,Time,Weight\n{date},{time},80\n"), "kg");
            assert_eq!(got.rows.len(), 1, "{name}: {:?}", got.skipped);
            let t = &got.rows[0].recorded_at;
            assert_eq!(
                (t.year(), t.month(), t.day(), t.hour()),
                (2026, 8, 14, 7),
                "{name}"
            );
        }
    }

    #[test]
    fn combined_datetime_layout_variants() {
        let local7 = at("2026-08-14 07:00").timestamp();
        for (name, value, want) in [
            (
                "RFC3339 with zone",
                "2026-08-14T07:00:00+01:00",
                "2026-08-14T06:00:00Z".parse().unwrap(),
            ),
            ("ISO without zone", "2026-08-14T07:00:00", local7),
            ("space separated", "2026-08-14 07:00:00", local7),
            ("US two-digit year", "08/14/26 07:00:00", local7),
            ("US four-digit year", "08/14/2026 07:00:00", local7),
        ] {
            let got = parse_ok(&format!("datetime,weight\n{value},80\n"), "kg");
            assert_eq!(got.rows.len(), 1, "{name}: {:?}", got.skipped);
            assert_eq!(got.rows[0].recorded_at.timestamp(), want, "{name}");
        }
    }

    #[test]
    fn header_is_case_and_space_insensitive() {
        let got = parse_ok(
            "  TimeStamp , Weight Grams ,Source\n2026-08-14T06:00:00Z,81500,x\n",
            "kg",
        );
        assert_eq!(got.rows.len(), 1, "{:?}", got.skipped);
        assert_eq!(got.rows[0].weight_g, 81500);
    }

    #[test]
    fn rejects_unrecognized_headers() {
        for csv in [
            "timestamp,steps\n2026-08-14T06:00:00Z,1000\n",
            "weight_kg,source\n81.5,scale\n",
            "date,weight\n2026-08-14,81.5\n",
        ] {
            assert!(parse(csv.as_bytes(), "kg").is_err(), "{csv:?}");
        }
        assert!(parse("".as_bytes(), "kg").is_err());
    }

    #[test]
    fn header_only_yields_no_rows() {
        let got = parse_ok("recorded_at,weight_kg\n", "kg");
        assert!(got.rows.is_empty() && got.skipped.is_empty());
    }

    #[test]
    fn skips_bad_rows_without_aborting() {
        let csv = "recorded_at,weight_kg\n\
                   2026-08-14T07:00:00Z,81.5\n\
                   not-a-date,80\n\
                   2026-08-15T07:00:00Z,abc\n\
                   2026-08-16T07:00:00Z,-5\n\
                   2026-08-17T07:00:00Z,0\n\
                   2026-08-18T07:00:00Z,80.1\n";
        let got = parse_ok(csv, "kg");
        assert_eq!(got.rows.len(), 2);
        // 1-based and counting the header, so the reader can find the row.
        let lines: Vec<usize> = got.skipped.iter().map(|s| s.line_number).collect();
        assert_eq!(lines, [3, 4, 5, 6]);
        assert!(got.skipped.iter().all(|s| !s.reason.is_empty()));
    }

    #[test]
    fn tolerates_ragged_rows() {
        let csv = "recorded_at,weight_kg,source\n2026-08-14T07:00:00Z,81.5,scale\n2026-08-15T07:00:00Z\n2026-08-16T07:00:00Z,81.2,scale\n";
        let got = parse_ok(csv, "kg");
        assert_eq!((got.rows.len(), got.skipped.len()), (2, 1));
    }

    #[test]
    fn to_kg_cases() {
        assert!((to_kg(81.5, "kg").unwrap() - 81.5).abs() < 1e-6);
        assert!((to_kg(1000.0, "g").unwrap() - 1.0).abs() < 1e-6);
        assert!((to_kg(180.0, "lb").unwrap() - 81.6466266).abs() < 1e-6);
        assert!(to_kg(80.0, "stone").is_err());
    }
}

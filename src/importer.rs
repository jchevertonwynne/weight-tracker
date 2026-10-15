//! Reads weight logs exported from elsewhere: Google Health (Takeout), this
//! app's own export, and a Fitbit-style file with the date and time in
//! separate columns.
//!
//! Google Health's format is the one checked against real data —
//! `timestamp,weight grams,data source`, UTC timestamps, whole grams. The
//! split date and time path is a best effort for a bare `weight` column.

use std::collections::HashMap;
use std::io::Read;
use std::str::FromStr;

use anyhow::{Result, anyhow, bail};
use jiff::tz::TimeZone;
use jiff::{Timestamp, Zoned};

use crate::clock;
use crate::model::Grams;

/// The unit a file's weights are in.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Unit {
    Kg,
    Lb,
    G,
}

impl Unit {
    fn grams(self, value: f64) -> Grams {
        Grams::from_kg(match self {
            Unit::Kg => value,
            Unit::Lb => value * 0.45359237,
            Unit::G => value / 1000.0,
        })
    }
}

impl FromStr for Unit {
    type Err = anyhow::Error;
    /// As the import form's unit select spells them.
    fn from_str(s: &str) -> Result<Unit> {
        match s {
            "kg" => Ok(Unit::Kg),
            "lb" => Ok(Unit::Lb),
            "g" => Ok(Unit::G),
            _ => bail!("unknown unit {s:?}"),
        }
    }
}

/// A row that could not be read, and why.
#[derive(Clone, Debug)]
pub struct Skipped {
    /// 1-based, counting the header, so it matches the file.
    pub line: usize,
    pub reason: String,
}

#[derive(Debug, Default)]
pub struct Import {
    pub readings: Vec<(Zoned, Grams)>,
    pub skipped: Vec<Skipped>,
}

/// Weight columns in order of preference. A header that names its unit is
/// believed over the form; only a bare "weight" falls back to it.
const WEIGHT_COLUMNS: [(&str, Option<Unit>); 5] = [
    ("weight_kg", Some(Unit::Kg)),   // this app's own export
    ("weight grams", Some(Unit::G)), // Google Health
    ("weight_grams", Some(Unit::G)),
    ("weight_lb", Some(Unit::Lb)),
    ("weight", None),
];

/// Columns holding the date and time together, in order of preference.
const DATETIME_COLUMNS: [&str; 4] = ["recorded_at", "date time", "datetime", "timestamp"];

/// Formats tried on a combined column with no offset of its own.
const DATETIME_FORMATS: [&str; 4] = [
    "%Y-%m-%dT%H:%M:%S",
    "%Y-%m-%d %H:%M:%S",
    "%m/%d/%y %H:%M:%S",
    "%m/%d/%Y %H:%M:%S",
];

/// Formats tried on separate date and time columns.
const DATE_FORMATS: [&str; 3] = ["%m/%d/%y", "%m/%d/%Y", "%Y-%m-%d"];
const TIME_FORMATS: [&str; 4] = ["%H:%M:%S", "%H:%M", "%I:%M:%S %p", "%I:%M %p"];

/// Where the date and time are.
enum When {
    Combined(usize),
    Split { date: usize, time: usize },
}

/// Reads a CSV of weights. `unit` applies where the header does not name its
/// own. A row that cannot be read is skipped and reported; only a header the
/// importer cannot make sense of fails the whole file.
pub fn parse(r: impl Read, unit: Unit) -> Result<Import> {
    let mut rows = csv::ReaderBuilder::new()
        .has_headers(false)
        .flexible(true)
        .from_reader(r)
        .into_records();
    let header = rows.next().ok_or_else(|| anyhow!("the file is empty"))??;
    let columns: HashMap<String, usize> = header
        .iter()
        .enumerate()
        .map(|(i, name)| (name.trim().to_lowercase(), i))
        .collect();
    let column = |name: &str| columns.get(name).copied();

    let (weight_col, unit) = WEIGHT_COLUMNS
        .iter()
        .find_map(|&(name, declared)| Some((column(name)?, declared.unwrap_or(unit))))
        .ok_or_else(|| {
            anyhow!(
                "no weight column in the header: {:?}",
                header.iter().collect::<Vec<_>>()
            )
        })?;
    let when = match DATETIME_COLUMNS.iter().find_map(|name| column(name)) {
        Some(i) => When::Combined(i),
        None => match (column("date"), column("time")) {
            (Some(date), Some(time)) => When::Split { date, time },
            _ => bail!(
                "no date and time columns in the header: {:?}",
                header.iter().collect::<Vec<_>>()
            ),
        },
    };

    let tz = clock::tz();
    let mut import = Import::default();
    for (line, row) in (2..).zip(rows) {
        let row = match row {
            Ok(row) => row,
            Err(e) => {
                import.skipped.push(Skipped {
                    line,
                    reason: e.to_string(),
                });
                continue;
            }
        };
        let field = |i: usize| row.get(i).unwrap_or_default().trim();
        let at = match when {
            When::Combined(i) => combined(field(i), &tz),
            When::Split { date, time } => split(field(date), field(time), &tz),
        };
        let Some(at) = at else {
            import.skipped.push(Skipped {
                line,
                reason: "invalid date/time".into(),
            });
            continue;
        };
        match field(weight_col).parse::<f64>() {
            Ok(w) if w.is_finite() && w > 0.0 => import.readings.push((at, unit.grams(w))),
            _ => import.skipped.push(Skipped {
                line,
                reason: "invalid weight".into(),
            }),
        }
    }
    Ok(import)
}

/// A combined date and time. One carrying its own offset (Google Health's
/// "Z") keeps that instant; otherwise it is a local wall-clock time.
fn combined(s: &str, tz: &TimeZone) -> Option<Zoned> {
    if let Ok(ts) = s.parse::<Timestamp>() {
        return Some(ts.to_zoned(tz.clone()));
    }
    DATETIME_FORMATS.iter().find_map(|f| clock::parse(s, f, tz))
}

fn split(date: &str, time: &str, tz: &TimeZone) -> Option<Zoned> {
    let s = format!("{date} {time}");
    DATE_FORMATS
        .iter()
        .flat_map(|d| TIME_FORMATS.iter().map(move |t| format!("{d} {t}")))
        .find_map(|f| clock::parse(&s, &f, tz))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::at;

    fn read(csv: &str, unit: Unit) -> Import {
        parse(csv.as_bytes(), unit).expect("a readable file")
    }

    fn grams(csv: &str, unit: Unit) -> Vec<i64> {
        read(csv, unit).readings.iter().map(|(_, g)| g.0).collect()
    }

    #[test]
    fn units() {
        assert_eq!("lb".parse::<Unit>().unwrap(), Unit::Lb);
        for bad in ["", "stone", "KG", "pounds"] {
            assert!(bad.parse::<Unit>().is_err(), "{bad:?}");
        }
        assert_eq!(Unit::Lb.grams(180.0), Grams(81647));
        assert_eq!(Unit::G.grams(1000.0), Grams(1000));
    }

    #[test]
    fn google_health() {
        let got = read(
            "timestamp,weight grams,data source\n2026-08-14T06:00:00Z,81500,Scale\n2026-08-15T06:30:00Z,81200,Scale\n",
            Unit::Lb, // wrong, and the header wins
        );
        assert!(got.skipped.is_empty());
        assert_eq!(
            got.readings.iter().map(|(_, g)| g.0).collect::<Vec<_>>(),
            [81500, 81200]
        );
        let (when, _) = &got.readings[0];
        assert_eq!(when.timestamp(), "2026-08-14T06:00:00Z".parse().unwrap());
        assert_eq!(when.time_zone(), &clock::tz(), "read in the local zone");
    }

    #[test]
    fn weight_columns() {
        assert_eq!(
            grams(
                "recorded_at,weight_kg\n2026-08-14T07:00:00+01:00,81.5\n",
                Unit::Lb
            ),
            [81500]
        );
        let bare = "recorded_at,weight\n2026-08-14T07:00:00Z,180\n";
        assert_eq!(grams(bare, Unit::Kg), [180000]);
        assert_eq!(grams(bare, Unit::Lb), [81647]);
        assert_eq!(grams(bare, Unit::G), [180]);
        assert_eq!(
            grams(
                "recorded_at,weight,weight_kg\n2026-08-14T07:00:00Z,999,81.5\n",
                Unit::Lb
            ),
            [81500]
        );
        assert_eq!(
            grams(
                "  TimeStamp , Weight Grams ,Source\n2026-08-14T06:00:00Z,81500,x\n",
                Unit::Kg
            ),
            [81500]
        );
    }

    #[test]
    fn date_and_time_formats() {
        for (date, time) in [
            ("08/14/26", "07:00:00"),
            ("08/14/2026", "07:00:00"),
            ("2026-08-14", "07:00:00"),
            ("2026-08-14", "07:00"),
            ("2026-08-14", "7:00:00 AM"),
            ("2026-08-14", "7:00 AM"),
        ] {
            let got = read(&format!("Date,Time,Weight\n{date},{time},80\n"), Unit::Kg);
            assert_eq!(got.readings.len(), 1, "{date} {time}: {:?}", got.skipped);
            assert_eq!(got.readings[0].0, at("2026-08-14 07:00"), "{date} {time}");
        }
        let seven = at("2026-08-14 07:00").timestamp();
        for (value, want) in [
            (
                "2026-08-14T07:00:00+01:00",
                "2026-08-14T06:00:00Z".parse().unwrap(),
            ),
            ("2026-08-14T07:00:00", seven),
            ("2026-08-14 07:00:00", seven),
            ("08/14/26 07:00:00", seven),
            ("08/14/2026 07:00:00", seven),
        ] {
            let got = read(&format!("datetime,weight\n{value},80\n"), Unit::Kg);
            assert_eq!(
                got.readings.first().map(|(t, _)| t.timestamp()),
                Some(want),
                "{value}"
            );
        }
    }

    #[test]
    fn unreadable_headers_fail_the_file() {
        for csv in [
            "",
            "timestamp,steps\n2026-08-14T06:00:00Z,1000\n",
            "weight_kg,source\n81.5,scale\n",
            "date,weight\n2026-08-14,81.5\n", // a date but no time
        ] {
            assert!(parse(csv.as_bytes(), Unit::Kg).is_err(), "{csv:?}");
        }
        let got = read("recorded_at,weight_kg\n", Unit::Kg);
        assert!(got.readings.is_empty() && got.skipped.is_empty());
    }

    #[test]
    fn unreadable_rows_are_skipped_and_reported() {
        let got = read(
            "recorded_at,weight_kg\n\
             2026-08-14T07:00:00Z,81.5\n\
             not-a-date,80\n\
             2026-08-15T07:00:00Z,abc\n\
             2026-08-16T07:00:00Z,-5\n\
             2026-08-17T07:00:00Z,0\n\
             2026-08-17T08:00:00Z\n\
             2026-08-18T07:00:00Z,80.1\n",
            Unit::Kg,
        );
        assert_eq!(got.readings.len(), 2);
        let lines: Vec<usize> = got.skipped.iter().map(|s| s.line).collect();
        assert_eq!(lines, [3, 4, 5, 6, 7]);
        assert!(got.skipped.iter().all(|s| !s.reason.is_empty()));
    }
}

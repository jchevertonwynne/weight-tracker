//! Local wall-clock time, and how timestamps are written down.
//!
//! The app splits weigh-ins into morning and evening on local time, so
//! "local" has to be the real zone: the one `TZ` names. homelab-rs bundles
//! the zone database, so that resolves on a scratch image with no zoneinfo;
//! without `TZ` the pod is UTC and the split quietly shifts by up to an hour.
//! Every time in the app is a [`Zoned`] in that zone.

use jiff::fmt::strtime;
use jiff::tz::TimeZone;
use jiff::{Timestamp, Zoned};

/// The zone `TZ` names, or UTC without one.
pub fn tz() -> TimeZone {
    TimeZone::system()
}

/// How a timestamp is stored: RFC 3339 in UTC, to the second —
/// "2026-08-14T06:00:00Z". Existing rows are in exactly this shape, the
/// importer de-duplicates on the string, and queries sort it as text, so it
/// must not drift.
pub fn to_stored(t: &Zoned) -> String {
    t.timestamp().strftime("%Y-%m-%dT%H:%M:%SZ").to_string()
}

/// A stored timestamp, read back in the local zone.
pub fn from_stored(s: &str) -> Result<Zoned, jiff::Error> {
    Ok(s.parse::<Timestamp>()?.to_zoned(tz()))
}

/// Parses `s` with a strptime `format` as a time in `tz`. A string carrying
/// its own offset keeps that instant.
///
/// A wall-clock time the zone skips or repeats — 01:30 on the mornings the
/// clocks change — resolves to the later of its two readings. Every such
/// weigh-in already in the database was stored that way, and the edit form
/// shows only the wall clock: resolving it any other way would move an
/// existing reading an hour when it is saved unchanged.
pub fn parse(s: &str, format: &str, tz: &TimeZone) -> Option<Zoned> {
    let parsed = strtime::parse(format, s).ok()?;
    if parsed.offset().is_some() {
        return Some(parsed.to_timestamp().ok()?.to_zoned(tz.clone()));
    }
    tz.to_ambiguous_zoned(parsed.to_datetime().ok()?)
        .later()
        .ok()
}

/// The ways the pages show a time.
pub trait Show {
    /// "Aug 2, 2026"
    fn date_label(&self) -> String;
    /// "Aug 2, 2026 07:05"
    fn datetime_label(&self) -> String;
    /// "2026-08-02", for a date input.
    fn ymd(&self) -> String;
    /// "07:05", for a time input.
    fn hm(&self) -> String;
    /// RFC 3339 in its own offset, "Z" when that is zero: the export's
    /// recorded_at.
    fn rfc3339(&self) -> String;
    /// Unix milliseconds, the x-axis unit Chart.js expects.
    fn ms(&self) -> i64;
}

impl Show for Zoned {
    fn date_label(&self) -> String {
        self.strftime("%b %-d, %Y").to_string()
    }

    fn datetime_label(&self) -> String {
        self.strftime("%b %-d, %Y %H:%M").to_string()
    }

    fn ymd(&self) -> String {
        self.strftime("%Y-%m-%d").to_string()
    }

    fn hm(&self) -> String {
        self.strftime("%H:%M").to_string()
    }

    fn rfc3339(&self) -> String {
        let local = self.strftime("%Y-%m-%dT%H:%M:%S");
        if self.offset().seconds() == 0 {
            format!("{local}Z")
        } else {
            format!("{local}{}", self.strftime("%:z"))
        }
    }

    fn ms(&self) -> i64 {
        self.timestamp().as_millisecond()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stored_is_utc_to_the_second() {
        let t: Zoned = "2026-08-14T07:00:00.75+01:00[Europe/London]"
            .parse()
            .unwrap();
        assert_eq!(to_stored(&t), "2026-08-14T06:00:00Z");
        let back = from_stored("2026-08-14T06:00:00Z").unwrap();
        assert_eq!(back.timestamp(), "2026-08-14T06:00:00Z".parse().unwrap());
    }

    #[test]
    fn rfc3339_writes_z_for_a_zero_offset() {
        for (t, want) in [
            ("2026-01-05T07:00[Europe/London]", "2026-01-05T07:00:00Z"),
            (
                "2026-07-05T07:00[Europe/London]",
                "2026-07-05T07:00:00+01:00",
            ),
            (
                "2026-07-05T07:00[Asia/Kathmandu]",
                "2026-07-05T07:00:00+05:45",
            ),
        ] {
            assert_eq!(t.parse::<Zoned>().unwrap().rfc3339(), want);
        }
    }

    // Saving a reading from the repeated hour unchanged must not move it.
    #[test]
    fn a_repeated_hour_round_trips_through_the_edit_form() {
        let london = TimeZone::get("Europe/London").unwrap();
        for stored in ["2026-10-25T01:30:00Z", "2026-03-29T01:30:00Z"] {
            let t = stored
                .parse::<Timestamp>()
                .unwrap()
                .to_zoned(london.clone());
            let typed = format!("{}T{}", t.ymd(), t.hm());
            let saved = parse(&typed, "%Y-%m-%dT%H:%M", &london).unwrap();
            assert_eq!(to_stored(&saved), stored);
        }
    }

    #[test]
    fn labels() {
        let t: Zoned = "2026-08-06T07:05[UTC]".parse().unwrap();
        assert_eq!(t.date_label(), "Aug 6, 2026");
        assert_eq!(t.datetime_label(), "Aug 6, 2026 07:05");
        assert_eq!(
            (t.ymd(), t.hm()),
            ("2026-08-06".to_string(), "07:05".to_string())
        );
    }
}

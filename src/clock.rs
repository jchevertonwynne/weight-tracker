//! Local wall-clock time, as Go's `time` package gave it to the Go version.
//!
//! This app reads and writes local wall-clock times and splits weigh-ins into
//! morning and evening on them, so "local" has to be the real zone. That is
//! the zone `TZ` names: jiff reads it as Go did, and homelab-rs bundles the
//! zone database, so it resolves on a scratch image with no zoneinfo. Without
//! `TZ` the pod is UTC and the split quietly shifts by up to an hour — the
//! Deployment sets it, and that is not cosmetic.
//!
//! Every time in the app is a [`jiff::Zoned`] in that zone. The helpers here
//! reproduce Go's semantics where they differ from jiff's own, because the
//! stored strings and the calendar arithmetic have to come out the same as
//! they did: `time.Date` and `AddDate` normalise an overflowing day (31 Jan
//! plus a month is 3 March) where jiff would clamp it (28 February).

use jiff::civil::{Date, DateTime, Time};
use jiff::tz::TimeZone;
use jiff::{Timestamp, Zoned};

/// The zone `TZ` names, or UTC without one.
pub fn tz() -> TimeZone {
    TimeZone::system()
}

/// An instant, read in the local zone.
pub fn local(ts: Timestamp) -> Zoned {
    ts.to_zoned(tz())
}

/// Go's `time.Date`: the fields are normalised rather than rejected, so month
/// 13 is January of the next year and day 0 is the last day of the month
/// before. A wall-clock time the zone skips or repeats resolves to the later
/// instant, as Go's does — see [`to_zoned`].
#[allow(clippy::too_many_arguments)]
pub fn date(
    year: i64,
    month: i64,
    day: i64,
    hour: i8,
    minute: i8,
    second: i8,
    nanos: i32,
    tz: &TimeZone,
) -> Zoned {
    try_date(year, month, day, hour, minute, second, nanos, tz).expect("a local time in range")
}

/// [`date`], for fields that come from a request: None where the result
/// would fall outside the years jiff represents (-9999 to 9999), which Go
/// would have carried on computing with.
#[allow(clippy::too_many_arguments)]
pub fn try_date(
    year: i64,
    month: i64,
    day: i64,
    hour: i8,
    minute: i8,
    second: i8,
    nanos: i32,
    tz: &TimeZone,
) -> Option<Zoned> {
    let month0 = month.checked_sub(1)?;
    let year = year.checked_add(month0.div_euclid(12))?;
    let month = month0.rem_euclid(12) + 1;
    let first = Date::new(i16::try_from(year).ok()?, month as i8, 1).ok()?;
    let civil = first
        .checked_add(jiff::Span::new().try_days(day.checked_sub(1)?).ok()?)
        .ok()?;
    to_zoned(
        DateTime::from_parts(civil, Time::new(hour, minute, second, nanos).ok()?),
        tz,
    )
}

/// A civil time in `tz`, resolved as Go resolves it where the zone skips or
/// repeats it: the later instant. In a skipped hour (01:30 on the spring
/// morning) that is jiff's default too; in a repeated one (01:30 when the
/// clocks go back) jiff would take the earlier, BST reading, and a weigh-in
/// typed then would be stored an hour from where the Go version put it.
fn to_zoned(dt: DateTime, tz: &TimeZone) -> Option<Zoned> {
    tz.to_ambiguous_zoned(dt).later().ok()
}

/// Go's `t.AddDate(years, months, days)`: adds to the date fields, keeps the
/// clock time, and normalises.
pub fn add_date(t: &Zoned, years: i64, months: i64, days: i64) -> Zoned {
    try_add_date(t, years, months, days).expect("a date in range")
}

/// [`add_date`] for an offset that comes from a request.
pub fn try_add_date(t: &Zoned, years: i64, months: i64, days: i64) -> Option<Zoned> {
    let tm = t.time();
    try_date(
        i64::from(t.year()).checked_add(years)?,
        i64::from(t.month()).checked_add(months)?,
        i64::from(t.day()).checked_add(days)?,
        tm.hour(),
        tm.minute(),
        tm.second(),
        tm.subsec_nanosecond(),
        t.time_zone(),
    )
}

/// Go's `t.Add(d)`: a fixed duration, wall clock be damned.
#[cfg(test)]
pub fn add(t: &Zoned, d: jiff::SignedDuration) -> Zoned {
    t.checked_add(d).expect("an instant in range")
}

/// Midnight at the start of `t`'s local day.
pub fn start_of_day(t: &Zoned) -> Zoned {
    date(
        i64::from(t.year()),
        i64::from(t.month()),
        i64::from(t.day()),
        0,
        0,
        0,
        0,
        t.time_zone(),
    )
}

/// The last nanosecond of `t`'s local day.
pub fn end_of_day(t: &Zoned) -> Zoned {
    date(
        i64::from(t.year()),
        i64::from(t.month()),
        i64::from(t.day()),
        23,
        59,
        59,
        999_999_999,
        t.time_zone(),
    )
}

/// How a timestamp is stored: RFC 3339 in UTC, to the second, as
/// `t.UTC().Format(time.RFC3339)` wrote it — "2026-08-14T06:00:00Z". The
/// strings are compared exactly (the importer de-duplicates on them) and
/// sorted as text, so the shape cannot drift.
pub fn stored(t: &Zoned) -> String {
    t.timestamp().strftime("%Y-%m-%dT%H:%M:%SZ").to_string()
}

/// Reads a stored timestamp back in the local zone, where the period and
/// same-day comparisons happen.
pub fn parse_stored(s: &str) -> Result<Zoned, jiff::Error> {
    Ok(local(s.parse::<Timestamp>()?))
}

/// `t.Format(time.RFC3339)` in `t`'s own zone: the offset, or "Z" when it is
/// zero, which Go writes for a UTC-offset local time too (London in winter).
pub fn rfc3339(t: &Zoned) -> String {
    let base = t.strftime("%Y-%m-%dT%H:%M:%S").to_string();
    let offset = t.offset().seconds();
    if offset == 0 {
        return base + "Z";
    }
    let sign = if offset < 0 { '-' } else { '+' };
    let offset = offset.abs();
    format!(
        "{base}{sign}{:02}:{:02}",
        offset / 3600,
        (offset % 3600) / 60
    )
}

/// "Jan 2, 2006"
pub fn date_label(t: &Zoned) -> String {
    t.strftime("%b %-d, %Y").to_string()
}

/// "Jan 2, 2006 15:04"
pub fn datetime_label(t: &Zoned) -> String {
    t.strftime("%b %-d, %Y %H:%M").to_string()
}

/// "2006-01-02", for a date input.
pub fn ymd(t: &Zoned) -> String {
    t.strftime("%Y-%m-%d").to_string()
}

/// "15:04", for a time input.
pub fn hm(t: &Zoned) -> String {
    t.strftime("%H:%M").to_string()
}

/// Unix milliseconds, the x-axis unit Chart.js expects.
pub fn ms(t: &Zoned) -> i64 {
    t.timestamp().as_millisecond()
}

/// Parses a civil date-time in `tz` with a strptime format, as
/// `time.ParseInLocation` did with a layout. A string carrying its own offset
/// (`%z`/`%:z`) keeps that instant whatever `tz` is.
pub fn parse_in(s: &str, format: &str, tz: &TimeZone) -> Option<Zoned> {
    let parsed = jiff::fmt::strtime::parse(format, s).ok()?;
    if parsed.offset().is_some() {
        return parsed.to_timestamp().ok().map(|ts| ts.to_zoned(tz.clone()));
    }
    to_zoned(parsed.to_datetime().ok()?, tz)
}

#[cfg(test)]
pub mod tests {
    use super::*;

    /// A local wall-clock time, "2006-01-02 15:04", as the Go tests' `at`.
    pub fn at(s: &str) -> Zoned {
        parse_in(s, "%Y-%m-%d %H:%M", &tz()).unwrap_or_else(|| panic!("parse test time {s:?}"))
    }

    #[test]
    fn add_date_normalises_as_go_does() {
        let tz = TimeZone::UTC;
        let jan31 = date(2026, 1, 31, 7, 30, 0, 0, &tz);
        assert_eq!(ymd(&add_date(&jan31, 0, 1, 0)), "2026-03-03");
        assert_eq!(ymd(&add_date(&jan31, 0, 0, -31)), "2025-12-31");
        assert_eq!(hm(&add_date(&jan31, 1, 0, 0)), "07:30");
        assert_eq!(ymd(&date(2026, 13, 0, 0, 0, 0, 0, &tz)), "2026-12-31");
    }

    #[test]
    fn stored_is_utc_rfc3339_to_the_second() {
        let t = local("2026-08-14T06:00:00.75Z".parse().unwrap());
        assert_eq!(stored(&t), "2026-08-14T06:00:00Z");
        assert_eq!(
            parse_stored(&stored(&t)).unwrap().timestamp().as_second(),
            t.timestamp().as_second()
        );
    }

    #[test]
    fn rfc3339_writes_z_for_a_zero_offset() {
        let london: TimeZone = TimeZone::get("Europe/London").unwrap();
        let winter = date(2026, 1, 5, 7, 0, 0, 0, &london);
        let summer = date(2026, 7, 5, 7, 0, 0, 0, &london);
        assert_eq!(rfc3339(&winter), "2026-01-05T07:00:00Z");
        assert_eq!(rfc3339(&summer), "2026-07-05T07:00:00+01:00");
        let kathmandu = TimeZone::get("Asia/Kathmandu").unwrap();
        assert_eq!(
            rfc3339(&date(2026, 7, 5, 7, 0, 0, 0, &kathmandu)),
            "2026-07-05T07:00:00+05:45"
        );
    }

    // Where London skips or repeats an hour, the later instant, as Go.
    #[test]
    fn ambiguous_times_resolve_as_go_resolves_them() {
        let london = TimeZone::get("Europe/London").unwrap();
        let fold = parse_in("2026-10-25T01:30", "%Y-%m-%dT%H:%M", &london).unwrap();
        assert_eq!(stored(&fold), "2026-10-25T01:30:00Z");
        let gap = parse_in("2026-03-29T01:30", "%Y-%m-%dT%H:%M", &london).unwrap();
        assert_eq!(stored(&gap), "2026-03-29T01:30:00Z");
        assert_eq!(
            stored(&date(2026, 10, 25, 1, 30, 0, 0, &london)),
            "2026-10-25T01:30:00Z"
        );
    }

    #[test]
    fn labels() {
        let t = date(2026, 8, 6, 7, 5, 0, 0, &TimeZone::UTC);
        assert_eq!(date_label(&t), "Aug 6, 2026");
        assert_eq!(datetime_label(&t), "Aug 6, 2026 07:05");
    }
}

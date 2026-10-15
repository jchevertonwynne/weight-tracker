//! The time ranges the chart, the history and the overnight views are
//! filtered by: what the range picker offers, how a range reads from and
//! writes to the URL, and the window of time it covers.

use std::sync::LazyLock;

use jiff::{SignedDuration, Span, Zoned};
use regex::Regex;

use crate::clock;

/// A span of time with either end open.
#[derive(Clone, Debug, Default)]
pub struct Window {
    pub from: Option<Zoned>,
    pub until: Option<Zoned>,
}

impl Window {
    pub fn contains(&self, t: &Zoned) -> bool {
        self.from.as_ref().is_none_or(|from| t >= from)
            && self.until.as_ref().is_none_or(|until| t <= until)
    }

    #[cfg(test)]
    pub fn is_unbounded(&self) -> bool {
        self.from.is_none() && self.until.is_none()
    }
}

/// A range as the query string spells it: `range` with, for a custom range,
/// `from` and `until`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Range {
    /// The last n days, today included.
    Days(i64),
    ThisYear,
    All,
    /// Each bound a date ("2026-01-01"), a time relative to now ("now-5d"),
    /// or relative to the other bound ("to-5d", "from+5d"); either may be
    /// blank.
    Custom {
        from: String,
        until: String,
    },
}

impl Range {
    /// Anything unrecognised is all time.
    pub fn parse(range: &str, from: &str, until: &str) -> Range {
        match range {
            "this-year" => Range::ThisYear,
            "custom" => Range::Custom {
                from: from.to_string(),
                until: until.to_string(),
            },
            _ => match range.parse() {
                Ok(days) if days > 0 => Range::Days(days),
                _ => Range::All,
            },
        }
    }

    /// The `range` value in the query string.
    pub fn value(&self) -> String {
        match self {
            Range::Days(n) => n.to_string(),
            Range::ThisYear => "this-year".into(),
            Range::All => "all".into(),
            Range::Custom { .. } => "custom".into(),
        }
    }

    /// The window this range covers as of `now`. Day counts, "this year" and
    /// custom bounds all snap to whole days, start to midnight and end to the
    /// last instant: a range is a span of dates, so every weigh-in on the days
    /// it names is in it, whatever the time of day it is asked at.
    pub fn window(&self, now: &Zoned) -> Window {
        match self {
            Range::All => Window::default(),
            Range::Days(n) => Window {
                from: now
                    .checked_sub(Span::new().days(n - 1))
                    .and_then(|t| t.start_of_day())
                    .ok(),
                until: None,
            },
            Range::ThisYear => Window {
                from: now.first_of_year().and_then(|t| t.start_of_day()).ok(),
                until: None,
            },
            Range::Custom { from, until } => custom(from, until, now),
        }
    }
}

/// Resolves custom bounds. Each resolves alone if it can (a date or a time
/// relative to now); then a bound written relative to the other one ("to-5d"
/// in from, "from+5d" in until) resolves against it. A bound that cannot
/// resolve — blank, unparseable, or relative to a bound that is itself
/// relative — leaves that end open.
fn custom(from: &str, until: &str, now: &Zoned) -> Window {
    let mut from_t = absolute(from, now);
    let mut until_t = absolute(until, now);
    if from_t.is_none() {
        from_t = relative_to(from, "to", until_t.as_ref());
    }
    if until_t.is_none() {
        until_t = relative_to(until, "from", from_t.as_ref());
    }
    Window {
        from: from_t.and_then(|t| t.start_of_day().ok()),
        until: until_t.and_then(|t| t.end_of_day().ok()),
    }
}

/// "now", "now-<count><unit>" (only back: now never looks forward). The units
/// are Grafana's, case-sensitive: m is minutes and M is months.
static NOW: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^now(?:-([0-9]+)([smhdwMy]))?$").expect("a valid pattern"));

/// "to" or "from", a sign, a count and a unit.
static OTHER: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^(to|from)([+-])([0-9]+)([smhdwMy])$").expect("a valid pattern"));

fn absolute(bound: &str, now: &Zoned) -> Option<Zoned> {
    let bound = bound.trim();
    if let Some(caps) = NOW.captures(bound) {
        return match (caps.get(1), caps.get(2)) {
            (Some(n), Some(unit)) => shift(now, -n.as_str().parse::<i64>().ok()?, unit.as_str()),
            _ => Some(now.clone()),
        };
    }
    clock::parse(bound, "%Y-%m-%d", now.time_zone())
}

fn relative_to(bound: &str, other: &str, anchor: Option<&Zoned>) -> Option<Zoned> {
    let caps = OTHER.captures(bound.trim())?;
    if &caps[1] != other {
        return None;
    }
    let n: i64 = caps[3].parse().ok()?;
    shift(anchor?, if &caps[2] == "-" { -n } else { n }, &caps[4])
}

/// `t` moved by `n` units. Days and longer are calendar arithmetic: a month
/// from the 31st lands on the last day of a shorter month, and a day across a
/// clock change keeps the time of day. None if the result is out of range.
fn shift(t: &Zoned, n: i64, unit: &str) -> Option<Zoned> {
    let fixed = |secs: i64| {
        t.checked_add(SignedDuration::from_secs(n.checked_mul(secs)?))
            .ok()
    };
    let calendar = |span: Result<Span, jiff::Error>| t.checked_add(span.ok()?).ok();
    match unit {
        "s" => fixed(1),
        "m" => fixed(60),
        "h" => fixed(3600),
        "d" => calendar(Span::new().try_days(n)),
        "w" => calendar(Span::new().try_weeks(n)),
        "M" => calendar(Span::new().try_months(n)),
        "y" => calendar(Span::new().try_years(n)),
        _ => None,
    }
}

/// A quick choice in the picker's popover. The list is here rather than in
/// the markup because the server reads a range back from the URL, and must
/// know which ones exist and what each is called.
#[derive(Clone, Copy, Debug)]
pub struct Preset {
    pub range: &'static str,
    pub label: &'static str,
}

pub const PRESETS: [Preset; 6] = [
    Preset {
        range: "7",
        label: "Last 7 days",
    },
    Preset {
        range: "30",
        label: "Last 30 days",
    },
    Preset {
        range: "90",
        label: "Last 90 days",
    },
    Preset {
        range: "365",
        label: "Last year",
    },
    Preset {
        range: "this-year",
        label: "This year",
    },
    Preset {
        range: "all",
        label: "All time",
    },
];

/// The longest custom bound taken from a URL. Bounds are free text echoed
/// into the page, so length is what is worth limiting; static/app.js applies
/// the same cap to a pasted range.
const MAX_BOUND_LEN: usize = 64;

/// One range picker — the chart's, the history filter's, the overnight
/// filter's — as the page renders it.
#[derive(Clone, Debug)]
pub struct Picker {
    /// The query parameter it reads and writes.
    pub param: &'static str,
    /// The preset shown when the URL says nothing.
    pub default: &'static str,
    pub range: Range,
}

impl Picker {
    /// The picker's state from its query parameters (`chart`, `chart_from`,
    /// `chart_until`). A range the popover does not offer falls back to the
    /// default rather than becoming all time under a button that says
    /// otherwise; a custom range needs at least one bound, and short ones.
    pub fn from_query(
        param: &'static str,
        default: &'static str,
        range: &str,
        from: &str,
        until: &str,
    ) -> Picker {
        let custom = range == "custom"
            && !(from.is_empty() && until.is_empty())
            && from.len() <= MAX_BOUND_LEN
            && until.len() <= MAX_BOUND_LEN;
        let offered = PRESETS.iter().any(|p| p.range == range);
        let chosen = if custom || offered { range } else { default };
        Picker {
            param,
            default,
            range: Range::parse(chosen, from, until),
        }
    }

    /// The `range` input's value.
    pub fn value(&self) -> String {
        self.range.value()
    }

    /// What the button says. A custom range's label is written by
    /// static/app.js from its bounds; this is what shows until it runs.
    pub fn label(&self) -> &'static str {
        let value = self.value();
        PRESETS
            .iter()
            .find(|p| p.range == value)
            .map_or("Custom range", |p| p.label)
    }

    pub fn from(&self) -> &str {
        match &self.range {
            Range::Custom { from, .. } => from,
            _ => "",
        }
    }

    pub fn until(&self) -> &str {
        match &self.range {
            Range::Custom { until, .. } => until,
            _ => "",
        }
    }

    pub fn presets(&self) -> &'static [Preset] {
        &PRESETS
    }

    /// What the picker is showing. Rendering through this is what keeps the
    /// content and the button describing it from disagreeing.
    pub fn window(&self, now: &Zoned) -> Window {
        self.range.window(now)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::at;

    fn window(range: &str, from: &str, until: &str) -> Window {
        Range::parse(range, from, until).window(&at("2026-08-16 14:30"))
    }

    fn show(t: &Option<Zoned>) -> String {
        t.as_ref()
            .expect("bounded")
            .strftime("%Y-%m-%d %H:%M:%S%.3f")
            .to_string()
    }

    #[test]
    fn presets_are_whole_trailing_days() {
        let w = window("7", "", "");
        assert_eq!(show(&w.from), "2026-08-10 00:00:00.000");
        assert!(w.until.is_none(), "open, so today's later entries show");
        assert_eq!(show(&window("1", "", "").from), "2026-08-16 00:00:00.000");
        let w = window("this-year", "", "");
        assert_eq!(show(&w.from), "2026-01-01 00:00:00.000");
        assert!(w.contains(&at("2026-01-01 00:00")) && !w.contains(&at("2025-12-31 23:59")));
    }

    #[test]
    fn anything_else_is_all_time() {
        for range in ["all", "", "nonsense", "0", "-5", "7.5"] {
            assert!(window(range, "", "").is_unbounded(), "{range:?}");
        }
    }

    #[test]
    fn contains_includes_its_ends() {
        let w = Window {
            from: Some(at("2026-08-10 00:00")),
            until: Some(at("2026-08-20 00:00")),
        };
        for (t, want) in [
            ("2026-08-15 12:00", true),
            ("2026-08-10 00:00", true),
            ("2026-08-20 00:00", true),
            ("2026-08-09 23:59", false),
            ("2026-08-20 00:01", false),
        ] {
            assert_eq!(w.contains(&at(t)), want, "{t}");
        }
        assert!(Window::default().contains(&at("1999-01-01 00:00")));
    }

    #[test]
    fn custom_dates_cover_whole_days() {
        let w = window("custom", "2026-08-01", "2026-08-10");
        assert_eq!(show(&w.from), "2026-08-01 00:00:00.000");
        assert_eq!(show(&w.until), "2026-08-10 23:59:59.999");
        let w = window("custom", "2026-08-10", "2026-08-10");
        assert!(w.contains(&at("2026-08-10 07:00")) && w.contains(&at("2026-08-10 21:00")));
        assert!(!w.contains(&at("2026-08-09 23:59")) && !w.contains(&at("2026-08-11 00:00")));
        // Either end may be open.
        assert!(window("custom", "2026-08-01", "").until.is_none());
        assert!(window("custom", "", "2026-08-10").from.is_none());
        assert!(window("custom", "not-a-date", "next tuesday").is_unbounded());
    }

    #[test]
    fn custom_bounds_relative_to_now() {
        let w = window("custom", "now-5d", "now");
        assert_eq!(show(&w.from), "2026-08-11 00:00:00.000");
        assert_eq!(show(&w.until), "2026-08-16 23:59:59.999");
        assert!(w.contains(&at("2026-08-11 07:00")) && w.contains(&at("2026-08-16 21:00")));
        assert_eq!(
            show(&window("custom", "2026-01-01", "now-1M").until),
            "2026-07-16 23:59:59.999"
        );
        // Sub-day units snap to the same day boundaries.
        assert_eq!(
            show(&window("custom", "now-6h", "now").from),
            "2026-08-16 00:00:00.000"
        );
        // Months are calendar months: a month before 31 March is 28 February.
        let w = Range::parse("custom", "now-1M", "").window(&at("2026-03-31 09:00"));
        assert_eq!(show(&w.from), "2026-02-28 00:00:00.000");
    }

    #[test]
    fn relative_syntax() {
        let now = at("2026-08-16 14:30");
        for (bound, want) in [
            ("now", "2026-08-16 14:30"),
            ("now-45m", "2026-08-16 13:45"),
            ("now-6m", "2026-08-16 14:24"), // m is minutes
            ("now-6h", "2026-08-16 08:30"),
            ("now-2w", "2026-08-02 14:30"),
            ("now-1y", "2025-08-16 14:30"),
        ] {
            let got = absolute(bound, &now).unwrap_or_else(|| panic!("{bound}"));
            assert_eq!(got.strftime("%Y-%m-%d %H:%M").to_string(), want, "{bound}");
        }
        for bad in [
            "now+5d", "now-5", "now-d", "5d", "tomorrow", "now-5x", "NOW-5d", "now-٣d",
        ] {
            assert!(absolute(bad, &now).is_none(), "{bad:?}");
        }
        // Out of range leaves the end open rather than failing.
        assert!(window("custom", "now-99999999y", "").is_unbounded());
    }

    #[test]
    fn custom_bounds_relative_to_each_other() {
        let w = window("custom", "to-5d", "now");
        assert_eq!(show(&w.from), "2026-08-11 00:00:00.000");
        let w = window("custom", "now-30d", "from+5d");
        assert_eq!(
            (show(&w.from), show(&w.until)),
            (
                "2026-07-17 00:00:00.000".into(),
                "2026-07-22 23:59:59.999".into()
            )
        );
        assert_eq!(
            show(&window("custom", "2026-08-01", "from+10d").until),
            "2026-08-11 23:59:59.999"
        );
        // Either sign; an inverted window simply contains nothing.
        assert_eq!(
            show(&window("custom", "now", "from-5d").until),
            "2026-08-11 23:59:59.999"
        );
        // Nothing to anchor to.
        assert!(window("custom", "to-5d", "from+5d").is_unbounded());
        assert!(window("custom", "from-5d", "to+5d").is_unbounded());
        assert!(window("custom", "to-5d", "").from.is_none());
    }

    #[test]
    fn pickers_read_the_url() {
        let p = Picker::from_query("history", "all", "", "", "");
        assert_eq!(
            (p.value(), p.label(), p.from(), p.until()),
            ("all".into(), "All time", "", "")
        );
        let p = Picker::from_query("overnight", "30", "this-year", "", "");
        assert_eq!((p.value(), p.label()), ("this-year".into(), "This year"));
        // Not offered, or a custom range with nothing in it: the default.
        for range in ["nonsense", "7.5", "14", "custom"] {
            assert_eq!(
                Picker::from_query("chart", "30", range, "", "").value(),
                "30",
                "{range}"
            );
        }
        let p = Picker::from_query("chart", "30", "custom", "now-5d", "now");
        assert_eq!(
            (p.value(), p.label(), p.from(), p.until()),
            ("custom".into(), "Custom range", "now-5d", "now")
        );
        assert_eq!(
            Picker::from_query("chart", "30", "custom", "", "2026-01-01").until(),
            "2026-01-01"
        );
        let long = "x".repeat(MAX_BOUND_LEN + 1);
        assert_eq!(
            Picker::from_query("chart", "30", "custom", &long, "now").value(),
            "30"
        );
        let w = Picker::from_query("overnight", "30", "7", "", "").window(&at("2026-08-16 14:30"));
        assert_eq!(
            w.from.unwrap().strftime("%Y-%m-%d").to_string(),
            "2026-08-10"
        );
        for p in PRESETS {
            assert_eq!(
                Picker::from_query("chart", "30", p.range, "", "").value(),
                p.range
            );
        }
    }
}

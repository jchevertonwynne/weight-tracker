//! Resolves the "range"/"from"/"until" query parameters shared by the chart,
//! history and overnight views into a concrete window.

use std::sync::LazyLock;

use jiff::{SignedDuration, Zoned};
use regex::Regex;

use crate::clock;

/// One entry in the picker's quick-range list. The list lives here rather
/// than as markup because the server has to answer the same questions the
/// popover does — which ranges exist, and what each is called — when it
/// renders a picker from the URL.
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

fn preset_label(range: &str) -> Option<&'static str> {
    PRESETS.iter().find(|p| p.range == range).map(|p| p.label)
}

/// What a custom range's button reads before static/app.js initialises; the
/// client immediately rewrites it from the bounds.
const CUSTOM_LABEL: &str = "Custom range";

/// Caps a custom bound coming off the URL. Bounds are free text, so length
/// is the only thing worth asserting, and an unbounded one would be echoed
/// into three attributes of the page. Matches the cap static/app.js applies
/// to a pasted range.
pub const MAX_BOUND_LEN: usize = 64;

/// The state of one instance of the shared time-range-picker partial — the
/// chart, the history filter and the overnight filter each embed one, with
/// different defaults.
#[derive(Clone, Debug)]
pub struct PickerConfig {
    /// The URL query parameter this instance reads and writes.
    pub param: &'static str,
    /// The preset shown when the URL says nothing about it.
    pub default: &'static str,
    /// The active preset ("30", "all", ...), or "custom".
    pub range: String,
    pub label: String,
    /// Custom bounds, empty unless range is "custom".
    pub from: String,
    pub until: String,
    pub presets: &'static [Preset],
}

/// One picker's state from the URL, falling back to `def` when the range is
/// absent — and when it is unrecognised: [`resolve`] treats anything it does
/// not know as unbounded, so a hand-edited link would otherwise show all
/// time under a button still claiming something else.
pub fn picker(
    param: &'static str,
    def: &'static str,
    range: &str,
    from: &str,
    until: &str,
) -> PickerConfig {
    let mut cfg = PickerConfig {
        param,
        default: def,
        range: def.to_string(),
        label: String::new(),
        from: String::new(),
        until: String::new(),
        presets: &PRESETS,
    };
    // Go's len() counts bytes, and so does this.
    if range == "custom"
        && (!from.is_empty() || !until.is_empty())
        && from.len() <= MAX_BOUND_LEN
        && until.len() <= MAX_BOUND_LEN
    {
        cfg.range = "custom".into();
        cfg.from = from.into();
        cfg.until = until.into();
        cfg.label = CUSTOM_LABEL.into();
        return cfg;
    }
    if preset_label(range).is_some() {
        cfg.range = range.into();
    }
    cfg.label = preset_label(&cfg.range).unwrap_or(&cfg.range).to_string();
    cfg
}

impl PickerConfig {
    /// What the picker is actually showing. Rendering content through this,
    /// rather than separately parsed parameters, is what keeps the content
    /// and the button describing it from disagreeing.
    pub fn window(&self, now: &Zoned) -> Window {
        resolve(&self.range, &self.from, &self.until, now)
    }
}

/// A resolved bound, either side of which may be open: both for "all time",
/// either for a custom range with that side left blank.
#[derive(Clone, Debug, Default)]
pub struct Window {
    pub from: Option<Zoned>,
    pub until: Option<Zoned>,
}

impl Window {
    pub fn contains(&self, t: &Zoned) -> bool {
        if let Some(from) = &self.from
            && t.timestamp() < from.timestamp()
        {
            return false;
        }
        if let Some(until) = &self.until
            && t.timestamp() > until.timestamp()
        {
            return false;
        }
        true
    }

    pub fn is_unbounded(&self) -> bool {
        self.from.is_none() && self.until.is_none()
    }
}

/// The "range" param as a window:
///
/// - a day count ("7", "30", ...) is an inclusive trailing window of whole
///   days ending now, open at the top so today's entries show;
/// - "this-year" runs from midnight on 1 January;
/// - "custom" reads from/until instead;
/// - "all", or anything unrecognised, is unbounded.
pub fn resolve(range: &str, from: &str, until: &str, today: &Zoned) -> Window {
    match range {
        "custom" => return custom_window(from, until, today),
        "this-year" => {
            let start = clock::date(i64::from(today.year()), 1, 1, 0, 0, 0, 0, today.time_zone());
            return Window {
                from: Some(start),
                until: None,
            };
        }
        _ => {}
    }
    // strconv.Atoi: an optional sign and decimal digits, nothing else.
    let Ok(days) = range.parse::<i64>() else {
        return Window::default();
    };
    if days <= 0 {
        return Window::default();
    }
    Window {
        from: clock::try_add_date(&clock::start_of_day(today), 0, 0, -days + 1),
        until: None,
    }
}

/// The subset of Grafana's relative syntax worth supporting: "now", or "now-"
/// a count and a unit. The units are Grafana's and case-sensitive the same
/// way — "m" is minutes and "M" months. Only "-": "now" only ever looks back.
static RELATIVE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^now(?:-([0-9]+)([smhdwMy]))?$").expect("a valid pattern"));

/// A bound relative to the other one: "to-5d" in from ("five days before
/// whatever until resolves to"), "from+5d" in until. Either sign, since
/// either field can sit either side of the other.
static CROSS_REF: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^(to|from)([+-])([0-9]+)([smhdwMy])$").expect("a valid pattern"));

/// Shifts `t` by `count` units in the direction of `sign`. Days and larger
/// are calendar arithmetic, so a month lands on the same day of the target
/// month (normalised as Go normalises it), and a day across a DST boundary
/// keeps its wall-clock time. None for a result out of range.
fn apply_offset(t: &Zoned, sign: &str, count: i64, unit: &str) -> Option<Zoned> {
    let n = if sign == "-" {
        count.checked_neg()?
    } else {
        count
    };
    let seconds = |per: i64| {
        let d = SignedDuration::from_secs(n.checked_mul(per)?);
        t.checked_add(d).ok()
    };
    match unit {
        "s" => seconds(1),
        "m" => seconds(60),
        "h" => seconds(3600),
        "d" => clock::try_add_date(t, 0, 0, n),
        "w" => clock::try_add_date(t, 0, 0, n.checked_mul(7)?),
        "M" => clock::try_add_date(t, 0, n, 0),
        "y" => clock::try_add_date(t, n, 0, 0),
        _ => None,
    }
}

/// "now-5d" against `now`.
fn parse_relative_time(value: &str, now: &Zoned) -> Option<Zoned> {
    let caps = RELATIVE.captures(value)?;
    match (caps.get(1), caps.get(2)) {
        (Some(n), Some(unit)) => apply_offset(now, "-", n.as_str().parse().ok()?, unit.as_str()),
        _ => Some(now.clone()),
    }
}

/// `value` as a reference to `keyword` ("to" or "from"), offset from the
/// other side's already-resolved value. None if it references something
/// else, or the other side never resolved.
fn parse_cross_ref(value: &str, keyword: &str, anchor: Option<&Zoned>) -> Option<Zoned> {
    let caps = CROSS_REF.captures(value.trim())?;
    if &caps[1] != keyword {
        return None;
    }
    apply_offset(anchor?, &caps[2], caps[3].parse().ok()?, &caps[4])
}

/// One side of a custom range: a date ("2026-01-01") or a relative
/// expression ("now-5d"). Cross-references need the other side, so
/// [`custom_window`] handles those.
fn parse_range_bound(value: &str, now: &Zoned) -> Option<Zoned> {
    let value = value.trim();
    if value.is_empty() {
        return None;
    }
    if let Some(t) = parse_relative_time(value, now) {
        return Some(t);
    }
    clock::parse_in(value, "%Y-%m-%d", now.time_zone())
}

/// A custom range's from/until. Each side is independently optional — blank
/// or unparseable leaves it open — and each accepts a date, a relative
/// expression, or a reference to the other side, resolved once that side is
/// known. A side that only makes sense through the other — blank, itself a
/// cross-reference — leaves the reference with nothing to anchor to.
///
/// Both ends snap to whole days, start to 00:00 and end to 23:59:59.999...:
/// a range is a span of dates, not instants, so every weigh-in on the named
/// days is included. Without it "now-5d" at 14:30 would drop that morning's
/// entry five days ago. The sub-day units round to the same boundaries.
fn custom_window(from: &str, until: &str, now: &Zoned) -> Window {
    let mut from_t = parse_range_bound(from, now);
    let mut until_t = parse_range_bound(until, now);
    // Second pass: whichever side did not resolve on its own gets a chance
    // to resolve against the side that did.
    if from_t.is_none() {
        from_t = parse_cross_ref(from, "to", until_t.as_ref());
    }
    if until_t.is_none() {
        until_t = parse_cross_ref(until, "from", from_t.as_ref());
    }
    Window {
        from: from_t.map(|t| clock::start_of_day(&t)),
        until: until_t.map(|t| clock::end_of_day(&t)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::at;

    fn fmt(t: &Option<Zoned>) -> String {
        t.as_ref()
            .expect("bounded")
            .strftime("%Y-%m-%d %H:%M:%S")
            .to_string()
    }

    fn fmt_ms(t: &Option<Zoned>) -> String {
        t.as_ref()
            .expect("bounded")
            .strftime("%Y-%m-%d %H:%M:%S%.3f")
            .to_string()
    }

    #[test]
    fn resolve_cases() {
        let today = at("2026-08-16 14:30");

        // A preset is an inclusive trailing window of whole days, open-ended.
        let w = resolve("7", "", "", &today);
        assert!(w.until.is_none());
        assert_eq!(fmt(&w.from), "2026-08-10 00:00:00");
        assert_eq!(
            fmt(&resolve("1", "", "", &today).from),
            "2026-08-16 00:00:00"
        );

        assert!(resolve("all", "", "", &today).is_unbounded());
        for p in ["", "nonsense", "0", "-5", "7.5"] {
            assert!(resolve(p, "", "", &today).is_unbounded(), "range={p:?}");
        }

        // Custom reads from/until, inclusive of the whole until day.
        let w = resolve("custom", "2026-08-01", "2026-08-10", &today);
        assert_eq!(fmt(&w.from), "2026-08-01 00:00:00");
        assert_eq!(fmt(&w.until), "2026-08-10 23:59:59");
        assert!(w.contains(&at("2026-08-10 23:30")));

        // Each side independently optional.
        let w = resolve("custom", "2026-08-01", "", &today);
        assert!(w.from.is_some() && w.until.is_none());
        let w = resolve("custom", "", "2026-08-10", &today);
        assert!(w.from.is_none() && w.until.is_some());
        assert!(resolve("custom", "not-a-date", "also-not", &today).is_unbounded());
    }

    #[test]
    fn window_contains() {
        let (from, until) = (at("2026-08-10 00:00"), at("2026-08-20 00:00"));
        let both = Window {
            from: Some(from.clone()),
            until: Some(until),
        };
        let lower = Window {
            from: Some(from),
            until: None,
        };
        for (name, w, t, want) in [
            ("inside a bounded window", &both, "2026-08-15 12:00", true),
            ("on the lower bound", &both, "2026-08-10 00:00", true),
            ("on the upper bound", &both, "2026-08-20 00:00", true),
            ("before a bounded window", &both, "2026-08-09 23:59", false),
            ("after a bounded window", &both, "2026-08-20 00:01", false),
            (
                "unbounded accepts anything",
                &Window::default(),
                "1999-01-01 00:00",
                true,
            ),
            (
                "lower bound only, before",
                &lower,
                "2026-08-09 00:00",
                false,
            ),
            (
                "lower bound only, long after",
                &lower,
                "2030-01-01 00:00",
                true,
            ),
        ] {
            assert_eq!(w.contains(&at(t)), want, "{name}");
        }
    }

    #[test]
    fn parse_relative_time_cases() {
        let now = at("2026-08-16 14:30");
        let secs = |s| clock::add(&now, SignedDuration::from_secs(s));
        for (value, want) in [
            ("now", now.clone()),
            ("now-30s", secs(-30)),
            ("now-45m", secs(-45 * 60)),
            ("now-6h", secs(-6 * 3600)),
            ("now-5d", clock::add_date(&now, 0, 0, -5)),
            ("now-2w", clock::add_date(&now, 0, 0, -14)),
            ("now-1M", clock::add_date(&now, 0, -1, 0)),
            ("now-1y", clock::add_date(&now, -1, 0, 0)),
            // Case-sensitive: m is minutes, so this is six minutes.
            ("now-6m", secs(-6 * 60)),
        ] {
            let got =
                parse_relative_time(value, &now).unwrap_or_else(|| panic!("{value} rejected"));
            assert_eq!(got.timestamp(), want.timestamp(), "{value}");
        }
        for bad in [
            "now+5d",
            "now-5",
            "now-d",
            "5d",
            "",
            "tomorrow",
            "2026-08-16",
            "now-5x",
            "NOW-5d",
            "now-٣d",
        ] {
            assert!(
                parse_relative_time(bad, &now).is_none(),
                "{bad:?} was accepted"
            );
        }
        // Out of range is unresolved, not a crash.
        assert!(parse_relative_time("now-99999999y", &now).is_none());
        assert!(resolve("custom", "now-99999999y", "", &now).is_unbounded());
    }

    #[test]
    fn relative_time_is_calendar_arithmetic() {
        // 31 March minus a month normalises as Go does, keeping the clock.
        let got = parse_relative_time("now-1M", &at("2026-03-31 09:00")).unwrap();
        assert_eq!(
            got.strftime("%Y-%m-%d %H:%M").to_string(),
            "2026-03-03 09:00"
        );
    }

    #[test]
    fn resolve_this_year() {
        let w = resolve("this-year", "", "", &at("2026-08-16 14:30"));
        assert!(w.until.is_none());
        assert_eq!(fmt(&w.from), "2026-01-01 00:00:00");
        assert!(w.contains(&at("2026-01-01 00:00")));
        assert!(!w.contains(&at("2025-12-31 23:59")));
    }

    #[test]
    fn custom_range_accepts_relative_expressions() {
        let now = at("2026-08-16 14:30");

        // The Grafana form, snapped to whole days at both ends.
        let w = resolve("custom", "now-5d", "now", &now);
        assert_eq!(fmt(&w.from), "2026-08-11 00:00:00");
        assert_eq!(fmt_ms(&w.until), "2026-08-16 23:59:59.999");
        assert!(w.contains(&at("2026-08-11 07:00")));
        assert!(w.contains(&at("2026-08-16 21:00")));
        assert!(!w.contains(&at("2026-08-10 21:00")));

        // A date and a relative expression can be mixed.
        let w = resolve("custom", "2026-01-01", "now-1M", &now);
        assert_eq!(fmt(&w.from), "2026-01-01 00:00:00");
        assert_eq!(fmt(&w.until), "2026-07-16 23:59:59");

        // A bare date covers its whole day at both ends.
        let w = resolve("custom", "2026-08-10", "2026-08-10", &now);
        assert!(w.contains(&at("2026-08-10 07:00")) && w.contains(&at("2026-08-10 21:00")));
        assert!(!w.contains(&at("2026-08-09 23:59")) && !w.contains(&at("2026-08-11 00:00")));

        // Sub-day units round to the same day boundaries.
        assert_eq!(
            fmt(&resolve("custom", "now-6h", "now", &now).from),
            "2026-08-16 00:00:00"
        );

        let w = resolve("custom", "now-90d", "", &now);
        assert!(w.from.is_some() && w.until.is_none());
        assert!(resolve("custom", "next tuesday", "whenever", &now).is_unbounded());
    }

    #[test]
    fn custom_range_cross_references() {
        let now = at("2026-08-16 14:30");

        let w = resolve("custom", "to-5d", "now", &now);
        assert_eq!(fmt(&w.from), "2026-08-11 00:00:00");
        assert_eq!(fmt_ms(&w.until), "2026-08-16 23:59:59.999");

        let w = resolve("custom", "now-30d", "from+5d", &now);
        assert_eq!(fmt(&w.from), "2026-07-17 00:00:00");
        assert_eq!(fmt(&w.until), "2026-07-22 23:59:59");

        let w = resolve("custom", "2026-08-01", "from+10d", &now);
        assert_eq!(fmt(&w.until), "2026-08-11 23:59:59");

        // Either sign either way; an inverted window just contains nothing.
        let w = resolve("custom", "now", "from-5d", &now);
        assert_eq!(fmt(&w.until), "2026-08-11 23:59:59");

        // Mutual and self references have nothing to anchor to.
        assert!(resolve("custom", "to-5d", "from+5d", &now).is_unbounded());
        assert!(resolve("custom", "from-5d", "to+5d", &now).is_unbounded());
        assert!(resolve("custom", "to-5d", "", &now).from.is_none());
    }

    #[test]
    fn picker_cases() {
        let cfg = picker("history", "all", "", "", "");
        assert_eq!(
            (cfg.range.as_str(), cfg.label.as_str()),
            ("all", "All time")
        );
        assert_eq!((cfg.from.as_str(), cfg.until.as_str()), ("", ""));

        let cfg = picker("overnight", "30", "this-year", "", "");
        assert_eq!(
            (cfg.range.as_str(), cfg.label.as_str()),
            ("this-year", "This year")
        );

        // Unrecognised falls back rather than widening the window.
        for p in ["nonsense", "7.5", "custom"] {
            assert_eq!(picker("chart", "30", p, "", "").range, "30", "{p}");
        }

        let cfg = picker("chart", "30", "custom", "now-5d", "now");
        assert_eq!(
            (cfg.range.as_str(), cfg.from.as_str(), cfg.until.as_str()),
            ("custom", "now-5d", "now")
        );
        let cfg = picker("chart", "30", "custom", "", "2026-01-01");
        assert_eq!(
            (cfg.range.as_str(), cfg.until.as_str()),
            ("custom", "2026-01-01")
        );

        let long = "x".repeat(MAX_BOUND_LEN + 1);
        assert_eq!(picker("chart", "30", "custom", &long, "now").range, "30");

        let cfg = picker("overnight", "30", "7", "", "");
        assert_eq!(
            cfg.window(&at("2026-08-16 14:30"))
                .from
                .unwrap()
                .strftime("%Y-%m-%d")
                .to_string(),
            "2026-08-10"
        );

        for p in PRESETS {
            assert!(!p.range.is_empty() && !p.label.is_empty());
            assert_eq!(picker("chart", "30", p.range, "", "").range, p.range);
        }
    }
}

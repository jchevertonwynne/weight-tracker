//! The JSON the client-side Chart.js instance renders directly — no
//! server-side pixel math, just points, a rolling trend, and clipped goal and
//! marker overlays.

use std::collections::HashMap;

use jiff::{Timestamp, Zoned};
use serde::Serialize;

use crate::db::{self, Entry, Goal, Marker};
use crate::timerange::{self, Window};
use crate::{clock, goals, markers, weight};

/// The trailing window, in elapsed days rather than sample count, that
/// smooths the raw series. Seven matches other weight trackers and a weekly
/// cadence of water and sodium.
const TREND_WINDOW_DAYS: f64 = 7.0;

/// The trailing span of readings the projection fits its rate to: long
/// enough that one noisy day cannot tilt it, short enough to reflect the
/// current trajectory.
const PROJECTION_FIT_DAYS: f64 = 28.0;
/// How far ahead the projection runs when no goal pulls it further — far
/// enough to be worth reading, before the band has said everything.
const PROJECTION_HORIZON_DAYS: f64 = 42.0;
/// The furthest a goal may stretch the horizon. Past this the band is too
/// wide to claim anything, so a goal that far off is left off.
const PROJECTION_MAX_DAYS: f64 = 180.0;
/// The largest share of the chart's width the projection may take, so it
/// never dwarfs the readings: a fraction f of the total spans f/(1-f) of the
/// visible data.
const PROJECTION_MAX_AREA_FRACTION: f64 = 0.25;
/// Prediction standard errors either side of the centre. ±1, as the
/// overnight chart's ±1 SD, so both make the same kind of claim.
const PROJECTION_SIGMA: f64 = 1.0;
/// Segments the curved band edges are drawn from; the interval widens as a
/// square root, so too few would cut the corner.
const PROJECTION_SAMPLES: usize = 24;
/// Too few readings, or all in a day or two, and the slope is noise dressed
/// as a direction.
const PROJECTION_MIN_POINTS: usize = 4;
const PROJECTION_MIN_SPAN_DAYS: f64 = 10.0;

/// One plotted point. Date and value are pre-formatted so the tooltip needs
/// no formatting of its own.
#[derive(Clone, Debug, Serialize)]
pub struct Point {
    /// Unix ms.
    pub x: i64,
    pub y: f64,
    /// "morning"/"evening"/"loss"/"gain"
    pub color: &'static str,
    pub date: String,
    pub value: String,
}

/// A plain point, for the trend and goal lines, which have no tooltips.
#[derive(Clone, Copy, Debug, Serialize)]
pub struct Xy {
    pub x: i64,
    pub y: f64,
}

#[derive(Clone, Debug, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Data {
    pub has_data: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub empty: String,
    pub is_bar: bool,
    #[serde(skip_serializing_if = "is_zero")]
    pub x_min: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub x_max: i64,
    /// null when there is no data, as the Go version's nil slice encoded.
    pub points: Option<Vec<Point>>,
    /// The rolling average for a single-period series. "all" splits it into
    /// a trend per period, since averaging morning and evening together is a
    /// line that is a rolling average of neither.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub trend: Vec<Xy>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub trend_morning: Vec<Xy>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub trend_evening: Vec<Xy>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub goals: Vec<Xy>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub markers: Vec<markers::Point>,
    /// Present only when asked for and there is enough recent data to fit
    /// one; the axis (x_max) is already widened to show all of it.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub projection: Option<ProjectionBand>,
}

fn is_zero(v: &i64) -> bool {
    *v == 0
}

/// The trend continued forward: a centre line at the current rate, inside a
/// band that widens with the horizon. The band is an ordinary-least-squares
/// prediction interval, widening for two honest reasons at once — a reading
/// scatters around the line, and the slope is only estimated — so it answers
/// "where is this heading, and how sure can the data be?".
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ProjectionBand {
    pub center: Vec<Xy>,
    pub upper: Vec<Xy>,
    pub lower: Vec<Xy>,
    /// The fitted rate, "-0.4 kg/week".
    pub rate_label: String,
}

#[derive(Clone, Debug)]
struct RawPoint {
    x: f64,
    t: Option<Zoned>,
    val: f64,
    class: &'static str,
}

/// Elapsed days since the epoch, from whole seconds as `t.Unix()` gave.
fn day_num(t: &Zoned) -> f64 {
    t.timestamp().as_second() as f64 / 86400.0
}

/// [`day_num`]'s inverse, to place projected points back on the time axis.
fn day_num_to_ms(x: f64) -> i64 {
    Timestamp::from_second((x * 86400.0).round() as i64)
        .map(|t| t.as_millisecond())
        .unwrap_or_default()
}

/// Fits a least-squares line to the last [`PROJECTION_FIT_DAYS`] of
/// `source` and extends it from the last trend value. Returns the band and
/// the axis end needed to show it, or None when there is too little recent
/// data to project honestly.
///
/// The centre is anchored at the last trend value, not the fitted line's own
/// endpoint, so it continues the drawn trend without a step; slope and band
/// width come from the fit.
fn build_projection(
    source: &[RawPoint],
    trend: &[RawPoint],
    all_goals: &[Goal],
    max_horizon_days: f64,
    today: &Zoned,
) -> Option<(ProjectionBand, i64)> {
    let last = trend.last()?;
    let cutoff = last.x - PROJECTION_FIT_DAYS;
    let fit: Vec<&RawPoint> = source.iter().filter(|p| p.x >= cutoff).collect();
    let n = fit.len();
    // n-2 degrees of freedom for the residual error, so three is the floor;
    // the span gate rejects readings clustered into a day or two.
    if n < PROJECTION_MIN_POINTS || fit[n - 1].x - fit[0].x < PROJECTION_MIN_SPAN_DAYS {
        return None;
    }
    let nf = n as f64;
    let mean_x = fit.iter().map(|p| p.x).sum::<f64>() / nf;
    let mean_y = fit.iter().map(|p| p.val).sum::<f64>() / nf;
    let (mut sxx, mut sxy) = (0.0, 0.0);
    for p in &fit {
        let dx = p.x - mean_x;
        sxx += dx * dx;
        sxy += dx * (p.val - mean_y);
    }
    if sxx == 0.0 {
        return None;
    }
    let slope = sxy / sxx;
    let intercept = mean_y - slope * mean_x;
    let sse: f64 = fit
        .iter()
        .map(|p| (p.val - (intercept + slope * p.x)).powi(2))
        .sum();
    let s = (sse / (nf - 2.0)).sqrt();

    let mut horizon = PROJECTION_HORIZON_DAYS;
    if let Some(goal) = goals::current(all_goals, today)
        && slope != 0.0
    {
        // Reach for the goal only when the rate is closing on it and it is
        // within the cap.
        let d = (db::grams_to_kg(goal.weight_g) - last.val) / slope;
        if d > horizon && d <= PROJECTION_MAX_DAYS {
            horizon = d;
        }
    }
    // Never outgrow its share of the chart — on a short range this
    // dominates, pulling six weeks down to a few days.
    if max_horizon_days > 0.0 && horizon > max_horizon_days {
        horizon = max_horizon_days;
    }

    let mut band = ProjectionBand {
        center: Vec::with_capacity(PROJECTION_SAMPLES + 1),
        upper: Vec::with_capacity(PROJECTION_SAMPLES + 1),
        lower: Vec::with_capacity(PROJECTION_SAMPLES + 1),
        rate_label: format!("{:+.1} kg/week", slope * 7.0),
    };
    for i in 0..=PROJECTION_SAMPLES {
        let d = horizon * i as f64 / PROJECTION_SAMPLES as f64;
        let x = last.x + d;
        let center = last.val + slope * d;
        let se = s * (1.0 + 1.0 / nf + (x - mean_x).powi(2) / sxx).sqrt();
        let half = PROJECTION_SIGMA * se;
        let ms = day_num_to_ms(x);
        band.center.push(Xy { x: ms, y: center });
        band.upper.push(Xy {
            x: ms,
            y: center + half,
        });
        band.lower.push(Xy {
            x: ms,
            y: center - half,
        });
    }
    Some((band, day_num_to_ms(last.x + horizon)))
}

fn empty_message(series: &str) -> &'static str {
    match series {
        "morning-delta" => {
            "Not enough data yet — log at least two morning weigh-ins to see day-over-day deltas."
        }
        "evening-delta" => {
            "Not enough data yet — log at least two evening weigh-ins to see day-over-day deltas."
        }
        "overnight" => {
            "Not enough data yet — log an evening weigh-in followed by the next morning's to see overnight changes."
        }
        "daily" => {
            "Not enough data yet — log a morning and evening weigh-in on the same day to see daily changes."
        }
        _ => "No entries in this range yet.",
    }
}

/// For each entry of `period` in `chrono` (ascending), the delta against the
/// previous entry of the same period, however long ago — a plain
/// day-over-day comparison, unlike the history list's cross-period deltas.
fn sequential_deltas(chrono: &[Entry], period: &str) -> HashMap<i64, i64> {
    let mut deltas = HashMap::new();
    let mut last: Option<&Entry> = None;
    for e in chrono.iter().filter(|e| weight::entry_period(e) == period) {
        if let Some(prev) = last {
            deltas.insert(e.id, e.weight_g - prev.weight_g);
        }
        last = Some(e);
    }
    deltas
}

/// For each point (ascending by x), the mean of every value with x in
/// [x - window_days, x]. One pass with a sliding window.
fn rolling_trend(series: &[RawPoint], window_days: f64) -> Vec<RawPoint> {
    let mut sum = 0.0;
    let mut start = 0;
    series
        .iter()
        .enumerate()
        .map(|(i, p)| {
            sum += p.val;
            while series[start].x < p.x - window_days {
                sum -= series[start].val;
                start += 1;
            }
            RawPoint {
                x: p.x,
                t: p.t.clone(),
                val: sum / (i - start + 1) as f64,
                class: "",
            }
        })
        .collect()
}

/// Trims a trend — computed over full history, so its window is never cut
/// short at the range's edge — to the visible window.
fn filter_by_window(pts: Vec<RawPoint>, window: &Window) -> Vec<RawPoint> {
    if window.is_unbounded() {
        return pts;
    }
    pts.into_iter()
        .filter(|p| p.t.as_ref().is_some_and(|t| window.contains(t)))
        .collect()
}

fn filter_by_class(pts: &[RawPoint], class: &str) -> Vec<RawPoint> {
    pts.iter().filter(|p| p.class == class).cloned().collect()
}

/// The rolling average trimmed to the window, or nothing below two points —
/// a one-point "trend" is not a line.
fn trend_xy(source: &[RawPoint], window: &Window) -> Vec<Xy> {
    let visible = filter_by_window(rolling_trend(source, TREND_WINDOW_DAYS), window);
    if visible.len() < 2 {
        return Vec::new();
    }
    visible
        .iter()
        .map(|p| Xy {
            x: p.t.as_ref().map(clock::ms).unwrap_or_default(),
            y: p.val,
        })
        .collect()
}

/// How far the x-axis runs. A bounded end of the window wins over the data,
/// so a range shows its whole span whether or not every day was logged. An
/// unbounded end falls back to the data — extended to the end of today when
/// the range has a start, so "last 30 days" reaches today however long ago
/// the last reading was. "All time" hugs the data at both ends.
fn axis_extent(window: &Window, first: &Zoned, last: &Zoned, now: &Zoned) -> (Zoned, Zoned) {
    let from = window.from.clone().unwrap_or_else(|| first.clone());
    let until = match &window.until {
        Some(u) => u.clone(),
        None if window.from.is_some() && now.timestamp() > last.timestamp() => {
            clock::end_of_day(now)
        }
        None => last.clone(),
    };
    // A window narrower than a single reading would collapse the axis.
    if from.timestamp() >= until.timestamp() {
        return (first.clone(), last.clone());
    }
    (from, until)
}

/// The chart for one series and range. With `show_projection`, a
/// single-period series (morning/evening) is extended with a projection band
/// and the axis widened for it; the delta bars and the split "all" view have
/// no single trend to continue.
#[allow(clippy::too_many_arguments)]
pub fn build(
    all_entries: &[Entry],
    all_goals: &[Goal],
    all_markers: &[Marker],
    range: &str,
    series: &str,
    from: &str,
    until: &str,
    show_projection: bool,
    today: &Zoned,
) -> Data {
    let (chrono, overnight, daily) = weight::chronological_with_deltas(all_entries);
    let window = timerange::resolve(range, from, until, today);
    let is_bar = matches!(
        series,
        "morning-delta" | "evening-delta" | "overnight" | "daily"
    );

    let deltas = match series {
        "morning-delta" => sequential_deltas(&chrono, "morning"),
        "evening-delta" => sequential_deltas(&chrono, "evening"),
        "overnight" => overnight,
        "daily" => daily,
        _ => HashMap::new(),
    };

    let mut pts = Vec::new();
    let mut trend_source = Vec::new();
    for e in &chrono {
        let period = if weight::entry_period(e) == "morning" {
            "morning"
        } else {
            "evening"
        };
        let visible = window.contains(&e.recorded_at);
        let point = |val: f64, class: &'static str| RawPoint {
            x: day_num(&e.recorded_at),
            t: Some(e.recorded_at.clone()),
            val,
            class,
        };
        if is_bar {
            if let Some(&d) = deltas.get(&e.id)
                && visible
            {
                pts.push(point(db::grams_to_kg(d), delta_class(d)));
            }
            continue;
        }
        if (series == "morning" || series == "evening") && period != series {
            continue;
        }
        // "morning", "evening", and "all" — the default for anything else.
        let p = point(db::grams_to_kg(e.weight_g), period);
        if visible {
            pts.push(p.clone());
        }
        trend_source.push(p);
    }

    let (Some(first), Some(last)) = (pts.first(), pts.last()) else {
        return Data {
            empty: empty_message(series).into(),
            ..Default::default()
        };
    };
    // The axis spans the range asked for, not just the part holding
    // readings: a 30-day range with a week of data used to draw a week-wide
    // chart, misrepresenting how dense the readings were and how long the
    // gap before them.
    let (axis_from, mut axis_until) = axis_extent(
        &window,
        first.t.as_ref().expect("a data point has a time"),
        last.t.as_ref().expect("a data point has a time"),
        today,
    );

    let mut data = Data {
        has_data: true,
        is_bar,
        x_min: clock::ms(&axis_from),
        x_max: clock::ms(&axis_until),
        points: Some(
            pts.iter()
                .map(|p| Point {
                    x: p.t.as_ref().map(clock::ms).unwrap_or_default(),
                    y: p.val,
                    color: p.class,
                    date: p.t.as_ref().map(clock::date_label).unwrap_or_default(),
                    value: if is_bar {
                        format!("{:+.1} kg", p.val)
                    } else {
                        format!("{:.1} kg", p.val)
                    },
                })
                .collect(),
        ),
        ..Default::default()
    };

    // The trend and goal lines only apply to the weight-value series.
    if !is_bar {
        if series == "all" {
            // A trend per period, rather than one smoothing over the very gap
            // between them that the raw lines show.
            data.trend_morning = trend_xy(&filter_by_class(&trend_source, "morning"), &window);
            data.trend_evening = trend_xy(&filter_by_class(&trend_source, "evening"), &window);
        } else {
            data.trend = trend_xy(&trend_source, &window);
            // Before the goal line is clipped, so both reach the same widened
            // axis and the cone visibly runs up to the goal it is heading
            // for.
            if show_projection {
                let full_trend = rolling_trend(&trend_source, TREND_WINDOW_DAYS);
                let visible_days = go_hours(
                    axis_until.timestamp().as_nanosecond() - axis_from.timestamp().as_nanosecond(),
                ) / 24.0;
                let max_horizon = visible_days * PROJECTION_MAX_AREA_FRACTION
                    / (1.0 - PROJECTION_MAX_AREA_FRACTION);
                if let Some((band, end_ms)) =
                    build_projection(&trend_source, &full_trend, all_goals, max_horizon, today)
                {
                    data.projection = Some(band);
                    if end_ms > data.x_max {
                        data.x_max = end_ms;
                        axis_until = clock::local(
                            Timestamp::from_millisecond(end_ms).expect("an instant in range"),
                        );
                    }
                }
            }
        }

        for seg in goals::clip_segments(&goals::build_segments(all_goals), &axis_from, &axis_until)
        {
            // Both ends of each segment, so consecutive goals join with a
            // vertical step in one plain line dataset.
            let y = db::grams_to_kg(seg.weight_g);
            data.goals.push(Xy {
                x: clock::ms(&seg.from),
                y,
            });
            if let Some(until) = &seg.until {
                data.goals.push(Xy {
                    x: clock::ms(until),
                    y,
                });
            }
        }
    }

    // Markers give context ("started new diet") whatever the series, bars
    // included.
    data.markers = markers::visible(all_markers, &axis_from, &axis_until);
    data
}

/// Go's `Duration.Hours()`, to the nanosecond: whole hours plus the
/// remainder, which is what keeps a range ending at 23:59:59.999999999 from
/// losing most of a second — and the projection's horizon cap with it.
fn go_hours(nanos: i128) -> f64 {
    const HOUR: i128 = 3_600_000_000_000;
    (nanos / HOUR) as f64 + (nanos % HOUR) as f64 / HOUR as f64
}

fn delta_class(delta_g: i64) -> &'static str {
    if delta_g < 0 { "loss" } else { "gain" }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testsupport::{at, entry, nearly_equal};
    use jiff::SignedDuration;

    fn raw(x: f64, val: f64) -> RawPoint {
        RawPoint {
            x,
            t: None,
            val,
            class: "",
        }
    }

    #[test]
    fn rolling_trend_cases() {
        // A running mean while every point is inside the window.
        let got = rolling_trend(
            &[
                raw(0.0, 80.0),
                raw(1.0, 82.0),
                raw(2.0, 84.0),
                raw(3.0, 86.0),
            ],
            7.0,
        );
        let vals: Vec<f64> = got.iter().map(|p| p.val).collect();
        for (g, w) in vals.iter().zip([80.0, 81.0, 82.0, 83.0]) {
            assert!(nearly_equal(*g, w), "{vals:?}");
        }
        // Points older than the window drop out.
        let got = rolling_trend(
            &[
                raw(0.0, 100.0),
                raw(1.0, 100.0),
                raw(9.0, 80.0),
                raw(10.0, 82.0),
            ],
            2.0,
        );
        assert!(nearly_equal(got[2].val, 80.0) && nearly_equal(got[3].val, 81.0));
        // Same-day readings all count.
        assert!(nearly_equal(
            rolling_trend(&[raw(0.0, 80.0), raw(0.0, 84.0)], 7.0)[1].val,
            82.0
        ));
        // x and t are kept.
        let t = at("2026-08-16 07:00");
        let got = rolling_trend(
            &[RawPoint {
                x: 5.0,
                t: Some(t.clone()),
                val: 80.0,
                class: "",
            }],
            7.0,
        );
        assert_eq!(
            (got[0].x, got[0].t.as_ref().map(|t| t.timestamp())),
            (5.0, Some(t.timestamp()))
        );
        assert!(rolling_trend(&[], 7.0).is_empty());
    }

    #[test]
    fn filter_by_window_cases() {
        let pts = || {
            ["2026-08-01 07:00", "2026-08-10 07:00", "2026-08-20 07:00"]
                .iter()
                .zip([84.0, 83.0, 82.0])
                .map(|(t, v)| RawPoint {
                    x: 0.0,
                    t: Some(at(t)),
                    val: v,
                    class: "",
                })
                .collect::<Vec<_>>()
        };
        assert_eq!(filter_by_window(pts(), &Window::default()).len(), 3);
        let got = filter_by_window(
            pts(),
            &Window {
                from: Some(at("2026-08-05 00:00")),
                until: None,
            },
        );
        assert_eq!(got.len(), 2);
        assert!(nearly_equal(got[0].val, 83.0));
        assert!(
            filter_by_window(
                pts(),
                &Window {
                    from: Some(at("2027-01-01 00:00")),
                    until: None
                }
            )
            .is_empty()
        );
    }

    #[test]
    fn sequential_deltas_cases() {
        let chrono = [
            entry(1, at("2026-08-14 07:00"), 84.0, ""),
            entry(2, at("2026-08-14 21:00"), 85.0, ""),
            entry(3, at("2026-08-16 07:00"), 83.0, ""), // skips the 15th
            entry(4, at("2026-08-16 21:00"), 84.5, ""),
        ];
        let m = sequential_deltas(&chrono, "morning");
        assert_eq!(m, HashMap::from([(3, -1000)]));
        assert_eq!(sequential_deltas(&chrono, "evening").get(&4), Some(&-500));
    }

    fn fixture() -> Vec<Entry> {
        vec![
            entry(1, at("2026-08-14 07:00"), 84.0, ""),
            entry(2, at("2026-08-14 21:00"), 85.0, ""),
            entry(3, at("2026-08-15 07:00"), 83.5, ""),
            entry(4, at("2026-08-15 21:00"), 84.2, ""),
            entry(5, at("2026-08-16 07:00"), 83.0, ""),
        ]
    }

    #[allow(clippy::too_many_arguments)]
    fn build_(
        e: &[Entry],
        g: &[Goal],
        range: &str,
        series: &str,
        from: &str,
        until: &str,
        proj: bool,
        today: &Zoned,
    ) -> Data {
        build(e, g, &[], range, series, from, until, proj, today)
    }

    #[test]
    fn build_cases() {
        let today = at("2026-08-16 23:00");
        let entries = fixture();

        let got = build_(&entries, &[], "30", "all", "", "", false, &today);
        assert!(got.has_data && !got.is_bar);
        assert_eq!(got.points.as_ref().unwrap().len(), 5);
        assert!(got.x_min < clock::ms(&entries[0].recorded_at));
        assert!(got.x_max > clock::ms(&entries[4].recorded_at));

        let got = build_(&entries, &[], "30", "morning", "", "", false, &today);
        let points = got.points.unwrap();
        assert_eq!(points.len(), 3);
        assert!(points.iter().all(|p| p.color == "morning"));
        assert_eq!(
            (points[0].value.as_str(), points[0].date.as_str()),
            ("84.0 kg", "Aug 14, 2026")
        );

        let got = build_(&entries, &[], "30", "morning-delta", "", "", false, &today);
        assert!(got.is_bar);
        let points = got.points.unwrap();
        assert_eq!(points.len(), 2, "the first morning has no predecessor");
        assert_eq!(
            (points[0].value.as_str(), points[0].color),
            ("-0.5 kg", "loss")
        );
        assert!(got.trend.is_empty());

        let goals = [Goal {
            id: 1,
            weight_g: 80000,
            effective_from: at("2026-08-01 00:00"),
        }];
        assert!(
            !build_(&entries, &goals, "30", "all", "", "", false, &today)
                .goals
                .is_empty()
        );
        assert!(
            build_(
                &entries,
                &goals,
                "30",
                "morning-delta",
                "",
                "",
                false,
                &today
            )
            .goals
            .is_empty()
        );

        // The trend is smoothed over history from before the visible range.
        let got = build_(
            &entries,
            &[],
            "custom",
            "all",
            "2026-08-15",
            "",
            false,
            &today,
        );
        assert_eq!(got.points.as_ref().unwrap().len(), 3);
        assert_eq!(got.trend_morning.len(), 2);
        assert!(nearly_equal(got.trend_morning[0].y, (84.0 + 83.5) / 2.0));
        assert!(
            got.trend_evening.is_empty(),
            "only one evening reading is visible"
        );

        let got = build_(
            &entries,
            &[],
            "custom",
            "all",
            "2026-08-16",
            "",
            false,
            &today,
        );
        assert_eq!(got.points.as_ref().unwrap().len(), 1);
        assert!(got.trend_morning.is_empty() && got.trend_evening.is_empty());

        let got = build_(&entries[..1], &[], "30", "all", "", "", false, &today);
        assert!(got.trend_morning.is_empty() && got.trend_evening.is_empty());

        let got = build_(
            &entries,
            &[],
            "custom",
            "all",
            "2027-01-01",
            "2027-02-01",
            false,
            &today,
        );
        assert!(!got.has_data && !got.empty.is_empty() && got.points.is_none());

        let got = build_(
            &entries[..1],
            &[],
            "30",
            "morning-delta",
            "",
            "",
            false,
            &today,
        );
        assert!(!got.has_data);
        assert_ne!(got.empty, empty_message("all"));

        let got = build_(&[], &[], "30", "all", "", "", false, &today);
        assert!(!got.has_data && !got.empty.is_empty());
    }

    #[test]
    fn delta_class_cases() {
        assert_eq!(
            (delta_class(-100), delta_class(100), delta_class(0)),
            ("loss", "gain", "gain")
        );
    }

    #[test]
    fn day_num_tracks_elapsed_time() {
        let a = at("2026-08-15 07:00");
        assert!(nearly_equal(
            day_num(&clock::add(&a, SignedDuration::from_hours(24))) - day_num(&a),
            1.0
        ));
        assert!(nearly_equal(
            day_num(&clock::add(&a, SignedDuration::from_hours(12))) - day_num(&a),
            0.5
        ));
    }

    #[test]
    fn axis_spans_the_requested_range() {
        let today = at("2026-08-16 14:00");
        let entries = [
            entry(1, at("2026-08-13 07:00"), 84.0, ""),
            entry(2, at("2026-08-14 07:00"), 83.5, ""),
            entry(3, at("2026-08-15 07:00"), 83.0, ""),
        ];
        let local = |ms| clock::local(Timestamp::from_millisecond(ms).unwrap());

        let got = build_(&entries, &[], "30", "morning", "", "", false, &today);
        assert_eq!(got.x_min, clock::ms(&at("2026-07-18 00:00")));
        let until = local(got.x_max);
        assert_eq!(
            (until.year(), until.month(), until.day(), until.hour()),
            (2026, 8, 16, 23)
        );
        let days = (got.x_max - got.x_min) as f64 / 86_400_000.0;
        assert!((29.9..=30.1).contains(&days), "{days}");

        let got = build_(
            &entries,
            &[],
            "custom",
            "morning",
            "2026-08-01",
            "2026-08-20",
            false,
            &today,
        );
        assert_eq!(got.x_min, clock::ms(&at("2026-08-01 00:00")));
        let until = local(got.x_max);
        assert_eq!((until.day(), until.hour()), (20, 23));

        let got = build_(&entries, &[], "all", "morning", "", "", false, &today);
        assert_eq!(got.x_min, clock::ms(&entries[0].recorded_at));
        assert_eq!(got.x_max, clock::ms(&entries[2].recorded_at));
    }

    #[test]
    fn projection() {
        // ~0.1 kg/day down, with alternating scatter so the fit has a real
        // residual and the band genuinely widens.
        let today = at("2026-06-01 12:00");
        let start = at("2025-08-01 07:00");
        let entries: Vec<Entry> = (0..300)
            .map(|i| {
                let noise = if i % 2 == 0 { -0.3 } else { 0.3 };
                entry(
                    i + 1,
                    clock::add_date(&start, 0, 0, i),
                    90.0 - 0.1 * i as f64 + noise,
                    "",
                )
            })
            .collect();

        assert!(
            build_(&entries, &[], "all", "morning", "", "", false, &today)
                .projection
                .is_none()
        );

        let got = build_(&entries, &[], "all", "morning", "", "", true, &today);
        let p = got.projection.as_ref().expect("a band");
        assert_eq!(p.center.len(), PROJECTION_SAMPLES + 1);
        assert!(
            p.center.last().unwrap().y < p.center[0].y,
            "centre did not descend"
        );
        let near = p.upper[0].y - p.lower[0].y;
        let far = p.upper.last().unwrap().y - p.lower.last().unwrap().y;
        assert!(far > near, "band did not widen: {near} {far}");
        assert!(got.x_max > clock::ms(&entries.last().unwrap().recorded_at));
        assert!(!p.rate_label.is_empty());

        // An active goal further than six weeks out extends the horizon.
        let goals = [Goal {
            id: 1,
            weight_g: db::kg_to_grams(55.0),
            effective_from: start.clone(),
        }];
        let without = build_(&entries, &[], "all", "morning", "", "", true, &today);
        let with = build_(&entries, &goals, "all", "morning", "", "", true, &today);
        assert!(with.x_max > without.x_max);
        assert!(nearly_equal(
            with.projection.unwrap().center.last().unwrap().y,
            55.0
        ));

        // Capped to a share of a short range.
        let got = build_(&entries, &[], "7", "morning", "", "", true, &today);
        let c = got.projection.expect("a band").center;
        let span = (c.last().unwrap().x - c[0].x) as f64 / 86_400_000.0;
        assert!(span > 0.0 && span <= 3.0, "{span}");

        let sparse = [
            entry(1, at("2026-02-01 07:00"), 90.0, ""),
            entry(2, at("2026-02-02 07:00"), 89.9, ""),
        ];
        assert!(
            build_(&sparse, &[], "all", "morning", "", "", true, &today)
                .projection
                .is_none()
        );
        assert!(
            build_(&entries, &[], "all", "morning-delta", "", "", true, &today)
                .projection
                .is_none()
        );
    }

    // The field names and nulls chart-app.js reads.
    #[test]
    fn json_shape() {
        let empty = serde_json::to_value(build_(
            &[],
            &[],
            "30",
            "all",
            "",
            "",
            false,
            &at("2026-08-16 12:00"),
        ))
        .unwrap();
        assert_eq!(
            empty,
            serde_json::json!({"hasData": false, "empty": "No entries in this range yet.", "isBar": false, "points": null})
        );
        let full = serde_json::to_value(build_(
            &fixture(),
            &[],
            "30",
            "all",
            "",
            "",
            false,
            &at("2026-08-16 23:00"),
        ))
        .unwrap();
        for key in ["hasData", "isBar", "xMin", "xMax", "points", "trendMorning"] {
            assert!(full.get(key).is_some(), "{key}");
        }
        assert!(full.get("projection").is_none() && full.get("trend").is_none());
    }
}

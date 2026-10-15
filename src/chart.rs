//! The trend chart's data. Chart.js draws it in the browser
//! (static/chart-app.js); this decides what is plotted: the readings or the
//! changes between them, a rolling trend, the goal line, markers, and
//! optionally a projection of where the trend is heading.

use jiff::{Timestamp, Zoned};
use serde::Serialize;

use crate::clock::Show as _;
use crate::goals;
use crate::markers;
use crate::model::{Goal, Grams, Marker, Period, current_goal};
use crate::timeline::Timeline;
use crate::timerange::{Range, Window};

/// The trend's trailing window, in days rather than readings — a week, the
/// cadence water and salt move on.
const TREND_DAYS: f64 = 7.0;

/// How far back the projection fits its rate: long enough that one odd day
/// cannot tilt it, short enough to be the current trajectory.
const FIT_DAYS: f64 = 28.0;
/// Too few readings, or all within a few days, and a slope is noise.
const FIT_MIN_READINGS: usize = 4;
const FIT_MIN_SPAN_DAYS: f64 = 10.0;
/// How far ahead the projection runs, unless a goal pulls it further.
const HORIZON_DAYS: f64 = 42.0;
/// The furthest a goal may pull it: past this the band claims nothing.
const MAX_HORIZON_DAYS: f64 = 180.0;
/// The largest share of the chart's width the projection may take, so a
/// short range is not dwarfed by its own forecast.
const MAX_SHARE: f64 = 0.25;
/// Prediction standard errors either side of the centre: ±1, the same claim
/// the overnight chart's ±1 SD makes.
const SIGMA: f64 = 1.0;
/// Segments the curved band edges are drawn with.
const SAMPLES: usize = 24;

/// What the chart plots, as the series select names it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Series {
    /// Every reading, with a trend for each period.
    All,
    Morning,
    Evening,
    /// Each morning against the morning before it.
    MorningDelta,
    EveningDelta,
    /// Each morning against the evening before.
    Overnight,
    /// Each evening against that day's morning.
    Daily,
}

impl Series {
    /// Anything unrecognised is every reading.
    pub fn parse(s: &str) -> Series {
        match s {
            "morning" => Series::Morning,
            "evening" => Series::Evening,
            "morning-delta" => Series::MorningDelta,
            "evening-delta" => Series::EveningDelta,
            "overnight" => Series::Overnight,
            "daily" => Series::Daily,
            _ => Series::All,
        }
    }

    /// Changes are bars; weights are lines.
    fn is_bar(self) -> bool {
        matches!(
            self,
            Series::MorningDelta | Series::EveningDelta | Series::Overnight | Series::Daily
        )
    }

    fn empty(self) -> &'static str {
        match self {
            Series::MorningDelta => {
                "Not enough data yet — log at least two morning weigh-ins to see day-over-day deltas."
            }
            Series::EveningDelta => {
                "Not enough data yet — log at least two evening weigh-ins to see day-over-day deltas."
            }
            Series::Overnight => {
                "Not enough data yet — log an evening weigh-in followed by the next morning's to see overnight changes."
            }
            Series::Daily => {
                "Not enough data yet — log a morning and evening weigh-in on the same day to see daily changes."
            }
            _ => "No entries in this range yet.",
        }
    }
}

/// How a point is coloured.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Class {
    Morning,
    Evening,
    Loss,
    Gain,
}

impl Class {
    fn of_change(change: Grams) -> Class {
        if change.is_loss() {
            Class::Loss
        } else {
            Class::Gain
        }
    }
}

impl From<Period> for Class {
    fn from(p: Period) -> Class {
        match p {
            Period::Morning => Class::Morning,
            Period::Evening => Class::Evening,
        }
    }
}

/// One plotted point, with its tooltip text ready-made.
#[derive(Clone, Debug, Serialize)]
pub struct Point {
    /// Unix ms.
    pub x: i64,
    pub y: f64,
    pub color: Class,
    pub date: String,
    pub value: String,
}

/// A point on a line with no tooltip: the trend, the goal, the projection.
#[derive(Clone, Copy, Debug, Serialize)]
pub struct Xy {
    pub x: i64,
    pub y: f64,
}

/// The /chart response, field for field what chart-app.js reads.
#[derive(Clone, Debug, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Chart {
    pub has_data: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub empty: Option<&'static str>,
    pub is_bar: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub x_min: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub x_max: Option<i64>,
    /// null when there is nothing to plot.
    pub points: Option<Vec<Point>>,
    /// The trend of a single-period series.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub trend: Vec<Xy>,
    /// "All" smooths each period on its own: one line through mornings and
    /// evenings together would be a rolling average of neither.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub trend_morning: Vec<Xy>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub trend_evening: Vec<Xy>,
    /// Pairs of points stepping between goals.
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub goals: Vec<Xy>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub markers: Vec<markers::Point>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub projection: Option<Projection>,
}

/// The trend carried forward: a centre line at the fitted rate inside a
/// least-squares prediction interval, which widens for two reasons at once
/// — a reading scatters around the line, and the slope itself is only an
/// estimate.
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Projection {
    pub center: Vec<Xy>,
    pub upper: Vec<Xy>,
    pub lower: Vec<Xy>,
    /// "-0.4 kg/week"
    pub rate_label: String,
}

/// A value to plot at a time, and its position in days for the arithmetic.
#[derive(Clone, Debug)]
struct Reading {
    at: Zoned,
    day: f64,
    kg: f64,
    class: Class,
}

impl Reading {
    fn new(at: &Zoned, kg: f64, class: Class) -> Reading {
        Reading {
            day: days_since_epoch(at),
            at: at.clone(),
            kg,
            class,
        }
    }
}

fn days_since_epoch(t: &Zoned) -> f64 {
    t.timestamp().as_second() as f64 / 86400.0
}

fn ms_of_day(day: f64) -> i64 {
    Timestamp::from_second((day * 86400.0).round() as i64).map_or(0, |t| t.as_millisecond())
}

/// The chart for `series` over `range`, as of `now`. With `projection`, a
/// single-period weight series is carried forward and the axis widened to
/// show it; the bars and the two-period view have no single trend to carry.
pub fn chart(
    timeline: &Timeline,
    goals: &[Goal],
    markers: &[Marker],
    series: Series,
    range: &Range,
    projection: bool,
    now: &Zoned,
) -> Chart {
    let window = range.window(now);
    let entries = timeline.entries().iter();
    let change = |e: &crate::model::Entry, g: Grams| {
        Reading::new(&e.recorded_at, g.kg(), Class::of_change(g))
    };
    let weight =
        |e: &crate::model::Entry| Reading::new(&e.recorded_at, e.weight.kg(), e.period().into());
    // Oldest first, and the whole history: the trend at the window's edge is
    // still averaged over the days before it.
    let readings: Vec<Reading> = match series {
        Series::All => entries.map(weight).collect(),
        Series::Morning => entries
            .filter(|e| e.period() == Period::Morning)
            .map(weight)
            .collect(),
        Series::Evening => entries
            .filter(|e| e.period() == Period::Evening)
            .map(weight)
            .collect(),
        Series::MorningDelta => timeline
            .sequential(Period::Morning)
            .map(|(e, g)| change(e, g))
            .collect(),
        Series::EveningDelta => timeline
            .sequential(Period::Evening)
            .map(|(e, g)| change(e, g))
            .collect(),
        Series::Overnight => entries
            .filter_map(|e| timeline.overnight(e.id).map(|g| change(e, g)))
            .collect(),
        Series::Daily => entries
            .filter_map(|e| timeline.daily(e.id).map(|g| change(e, g)))
            .collect(),
    };
    let visible: Vec<&Reading> = readings.iter().filter(|r| window.contains(&r.at)).collect();
    let (Some(first), Some(last)) = (visible.first(), visible.last()) else {
        return Chart {
            empty: Some(series.empty()),
            ..Default::default()
        };
    };

    let (from, mut until) = axis(&window, &first.at, &last.at, now);
    let mut chart = Chart {
        has_data: true,
        is_bar: series.is_bar(),
        points: Some(visible.iter().map(|r| point(r, series.is_bar())).collect()),
        ..Default::default()
    };

    if !series.is_bar() {
        if series == Series::All {
            let of = |class| {
                readings
                    .iter()
                    .filter(|r| r.class == class)
                    .cloned()
                    .collect::<Vec<_>>()
            };
            chart.trend_morning = trend_line(&of(Class::Morning), &window);
            chart.trend_evening = trend_line(&of(Class::Evening), &window);
        } else {
            chart.trend = trend_line(&readings, &window);
            if projection {
                // At most MAX_SHARE of the final width: a projection that is
                // a fraction f of the whole spans f / (1 - f) of the rest.
                let visible_days = until
                    .timestamp()
                    .duration_since(from.timestamp())
                    .as_secs_f64()
                    / 86400.0;
                let cap = visible_days * MAX_SHARE / (1.0 - MAX_SHARE);
                if let Some((band, end)) = project(&readings, goals, cap, now) {
                    chart.projection = Some(band);
                    if end > until {
                        until = end;
                    }
                }
            }
        }
        // After the projection widened the axis, so the goal line runs out to
        // meet it.
        for seg in goals::segments(goals, &from, &until) {
            let y = seg.weight.kg();
            chart.goals.push(Xy {
                x: seg.from.ms(),
                y,
            });
            chart.goals.push(Xy {
                x: seg.until.ms(),
                y,
            });
        }
    }

    // Context for the line, whatever is plotted.
    chart.markers = markers::visible(markers, &from, &until);
    chart.x_min = Some(from.ms());
    chart.x_max = Some(until.ms());
    chart
}

fn point(r: &Reading, is_bar: bool) -> Point {
    Point {
        x: r.at.ms(),
        y: r.kg,
        color: r.class,
        date: r.at.date_label(),
        value: if is_bar {
            format!("{:+.1} kg", r.kg)
        } else {
            format!("{:.1} kg", r.kg)
        },
    }
}

/// How far the x-axis runs. A bounded end of the range wins over the data,
/// so the chart is as wide as the range asked for — a month with a week of
/// readings shows the three empty weeks too. An open end follows the data,
/// except that a range with a start runs to the end of today: "last 30 days"
/// reaches today however long ago the last reading was. All time hugs the
/// data at both ends.
fn axis(window: &Window, first: &Zoned, last: &Zoned, now: &Zoned) -> (Zoned, Zoned) {
    let from = window.from.clone().unwrap_or_else(|| first.clone());
    let until = match &window.until {
        Some(until) => until.clone(),
        None if window.from.is_some() && now > last => {
            now.end_of_day().unwrap_or_else(|_| now.clone())
        }
        None => last.clone(),
    };
    if from < until {
        (from, until)
    } else {
        // A window narrower than one reading would collapse the axis.
        (first.clone(), last.clone())
    }
}

/// Each reading's mean over the trailing [`TREND_DAYS`], in one pass.
fn rolling_mean(readings: &[Reading]) -> Vec<(Zoned, f64, f64)> {
    let (mut sum, mut start) = (0.0, 0);
    readings
        .iter()
        .enumerate()
        .map(|(i, r)| {
            sum += r.kg;
            while readings[start].day < r.day - TREND_DAYS {
                sum -= readings[start].kg;
                start += 1;
            }
            (r.at.clone(), r.day, sum / (i - start + 1) as f64)
        })
        .collect()
}

/// The trend within `window`, or nothing if under two points survive — one
/// point is not a line.
fn trend_line(readings: &[Reading], window: &Window) -> Vec<Xy> {
    let line: Vec<Xy> = rolling_mean(readings)
        .into_iter()
        .filter(|(at, _, _)| window.contains(at))
        .map(|(at, _, y)| Xy { x: at.ms(), y })
        .collect();
    if line.len() < 2 { Vec::new() } else { line }
}

/// Fits a line to the last [`FIT_DAYS`] of `readings` and carries the trend
/// forward from its last value at that slope, `cap` days at most. The band
/// and where the axis must reach to show it; None with too little recent
/// data to project from honestly.
fn project(
    readings: &[Reading],
    goals: &[Goal],
    cap: f64,
    now: &Zoned,
) -> Option<(Projection, Zoned)> {
    let (_, last_day, last_trend) = rolling_mean(readings).pop()?;
    let fit: Vec<&Reading> = readings
        .iter()
        .filter(|r| r.day >= last_day - FIT_DAYS)
        .collect();
    let n = fit.len() as f64;
    if fit.len() < FIT_MIN_READINGS || fit[fit.len() - 1].day - fit[0].day < FIT_MIN_SPAN_DAYS {
        return None;
    }
    let mean_x = fit.iter().map(|r| r.day).sum::<f64>() / n;
    let mean_y = fit.iter().map(|r| r.kg).sum::<f64>() / n;
    let sxx: f64 = fit
        .iter()
        .map(|r| (r.day - mean_x) * (r.day - mean_x))
        .sum();
    let sxy: f64 = fit.iter().map(|r| (r.day - mean_x) * (r.kg - mean_y)).sum();
    if sxx == 0.0 {
        return None;
    }
    let slope = sxy / sxx;
    let intercept = mean_y - slope * mean_x;
    let sse: f64 = fit
        .iter()
        .map(|r| (r.kg - (intercept + slope * r.day)) * (r.kg - (intercept + slope * r.day)))
        .sum();
    // n - 2 degrees of freedom for the residual error.
    let s = (sse / (n - 2.0)).sqrt();

    let mut horizon = HORIZON_DAYS;
    if let Some(goal) = current_goal(goals, now)
        && slope != 0.0
    {
        // Reach for the goal only when the trend is closing on it, and not
        // too far off.
        let days = (goal.weight.kg() - last_trend) / slope;
        if days > horizon && days <= MAX_HORIZON_DAYS {
            horizon = days;
        }
    }
    if cap > 0.0 {
        horizon = horizon.min(cap);
    }

    let mut band = Projection {
        center: Vec::with_capacity(SAMPLES + 1),
        upper: Vec::with_capacity(SAMPLES + 1),
        lower: Vec::with_capacity(SAMPLES + 1),
        rate_label: format!("{:+.1} kg/week", slope * 7.0),
    };
    for i in 0..=SAMPLES {
        let ahead = horizon * i as f64 / SAMPLES as f64;
        let x = last_day + ahead;
        let center = last_trend + slope * ahead;
        let half = SIGMA * s * (1.0 + 1.0 / n + (x - mean_x) * (x - mean_x) / sxx).sqrt();
        let x = ms_of_day(x);
        band.center.push(Xy { x, y: center });
        band.upper.push(Xy {
            x,
            y: center + half,
        });
        band.lower.push(Xy {
            x,
            y: center - half,
        });
    }
    let end = Timestamp::from_millisecond(ms_of_day(last_day + horizon))
        .ok()?
        .to_zoned(now.time_zone().clone());
    Some((band, end))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::Entry;
    use crate::testsupport::{at, entry, goal, nearly_equal};

    fn reading(day: f64, kg: f64) -> Reading {
        Reading {
            at: at("2026-01-01 00:00"),
            day,
            kg,
            class: Class::Morning,
        }
    }

    #[test]
    fn rolling_mean_covers_elapsed_days() {
        let means = |rs: &[Reading]| {
            rolling_mean(rs)
                .into_iter()
                .map(|(_, _, y)| y)
                .collect::<Vec<_>>()
        };
        let got = means(&[
            reading(0.0, 80.0),
            reading(1.0, 82.0),
            reading(2.0, 84.0),
            reading(3.0, 86.0),
        ]);
        assert!(
            got.iter()
                .zip([80.0, 81.0, 82.0, 83.0])
                .all(|(g, w)| nearly_equal(*g, w)),
            "{got:?}"
        );
        // A week later the early readings have dropped out.
        let got = means(&[
            reading(0.0, 100.0),
            reading(1.0, 100.0),
            reading(9.0, 80.0),
            reading(10.0, 82.0),
        ]);
        assert!(nearly_equal(got[2], 80.0) && nearly_equal(got[3], 81.0));
        // Readings on the same day all count.
        assert!(nearly_equal(
            means(&[reading(0.0, 80.0), reading(0.0, 84.0)])[1],
            82.0
        ));
        assert!(rolling_mean(&[]).is_empty());
    }

    fn week() -> Timeline {
        Timeline::new(&[
            entry(1, "2026-08-14 07:00", 84.0),
            entry(2, "2026-08-14 21:00", 85.0),
            entry(3, "2026-08-15 07:00", 83.5),
            entry(4, "2026-08-15 21:00", 84.2),
            entry(5, "2026-08-16 07:00", 83.0),
        ])
    }

    fn build(
        t: &Timeline,
        goals: &[Goal],
        series: &str,
        range: Range,
        projection: bool,
        now: &str,
    ) -> Chart {
        chart(
            t,
            goals,
            &[],
            Series::parse(series),
            &range,
            projection,
            &at(now),
        )
    }

    fn days(n: i64) -> Range {
        Range::Days(n)
    }

    fn custom(from: &str, until: &str) -> Range {
        Range::parse("custom", from, until)
    }

    const NOW: &str = "2026-08-16 23:00";

    #[test]
    fn weights() {
        let t = week();
        let got = build(&t, &[], "all", days(30), false, NOW);
        assert!(got.has_data && !got.is_bar);
        assert_eq!(got.points.as_ref().unwrap().len(), 5);
        let got = build(&t, &[], "morning", days(30), false, NOW);
        let points = got.points.unwrap();
        assert_eq!(points.len(), 3);
        assert!(points.iter().all(|p| p.color == Class::Morning));
        assert_eq!(
            (points[0].value.as_str(), points[0].date.as_str()),
            ("84.0 kg", "Aug 14, 2026")
        );
    }

    #[test]
    fn changes_are_bars_without_trend_or_goal() {
        let got = build(
            &week(),
            &[goal(1, 80.0, "2026-08-01")],
            "morning-delta",
            days(30),
            false,
            NOW,
        );
        assert!(got.is_bar && got.trend.is_empty() && got.goals.is_empty());
        let points = got.points.unwrap();
        assert_eq!(points.len(), 2, "the first morning has nothing before it");
        assert_eq!(
            (points[0].value.as_str(), points[0].color),
            ("-0.5 kg", Class::Loss)
        );
        assert!(
            !build(
                &week(),
                &[goal(1, 80.0, "2026-08-01")],
                "all",
                days(30),
                false,
                NOW
            )
            .goals
            .is_empty()
        );
    }

    #[test]
    fn the_trend_reaches_back_before_the_window() {
        let got = build(&week(), &[], "all", custom("2026-08-15", ""), false, NOW);
        assert_eq!(got.points.as_ref().unwrap().len(), 3);
        // The first visible morning averages in the 14th, off the chart.
        assert_eq!(got.trend_morning.len(), 2);
        assert!(nearly_equal(got.trend_morning[0].y, (84.0 + 83.5) / 2.0));
        assert!(got.trend_evening.is_empty(), "one evening is not a line");
        let got = build(&week(), &[], "all", custom("2026-08-16", ""), false, NOW);
        assert!(got.trend_morning.is_empty() && got.trend_evening.is_empty());
    }

    #[test]
    fn empty_charts_say_why() {
        let got = build(
            &week(),
            &[],
            "all",
            custom("2027-01-01", "2027-02-01"),
            false,
            NOW,
        );
        assert!(!got.has_data && got.points.is_none());
        assert_eq!(got.empty, Some(Series::All.empty()));
        let one = Timeline::new(&[entry(1, "2026-08-14 07:00", 84.0)]);
        assert_eq!(
            build(&one, &[], "morning-delta", days(30), false, NOW).empty,
            Some(Series::MorningDelta.empty())
        );
        assert!(!build(&Timeline::default(), &[], "all", days(30), false, NOW).has_data);
    }

    #[test]
    fn the_axis_spans_the_range_asked_for() {
        let t = Timeline::new(&[
            entry(1, "2026-08-13 07:00", 84.0),
            entry(2, "2026-08-14 07:00", 83.5),
            entry(3, "2026-08-15 07:00", 83.0),
        ]);
        let now = "2026-08-16 14:00";
        let local = |ms: Option<i64>| {
            Timestamp::from_millisecond(ms.unwrap())
                .unwrap()
                .to_zoned(crate::clock::tz())
        };

        let got = build(&t, &[], "morning", days(30), false, now);
        assert_eq!(got.x_min, Some(at("2026-07-18 00:00").ms()));
        let end = local(got.x_max);
        assert_eq!((end.day(), end.hour()), (16, 23), "the end of today");

        let got = build(
            &t,
            &[],
            "morning",
            custom("2026-08-01", "2026-08-20"),
            false,
            now,
        );
        assert_eq!(got.x_min, Some(at("2026-08-01 00:00").ms()));
        let end = local(got.x_max);
        assert_eq!((end.day(), end.hour()), (20, 23));

        let got = build(&t, &[], "morning", Range::All, false, now);
        assert_eq!(
            (got.x_min, got.x_max),
            (
                Some(at("2026-08-13 07:00").ms()),
                Some(at("2026-08-15 07:00").ms())
            )
        );
    }

    /// A steady 0.1 kg/day fall with alternating scatter, so the fit has a
    /// real residual and the band genuinely widens.
    fn falling() -> Timeline {
        let start = at("2025-08-01 07:00");
        let entries: Vec<Entry> = (0..300)
            .map(|i| Entry {
                id: i + 1,
                recorded_at: start.checked_add(jiff::Span::new().days(i)).unwrap(),
                weight: Grams::from_kg(90.0 - 0.1 * i as f64 + if i % 2 == 0 { -0.3 } else { 0.3 }),
                period_override: None,
            })
            .collect();
        Timeline::new(&entries)
    }

    #[test]
    fn projection() {
        let t = falling();
        let now = "2026-06-01 12:00";
        assert!(
            build(&t, &[], "morning", Range::All, false, now)
                .projection
                .is_none()
        );

        let got = build(&t, &[], "morning", Range::All, true, now);
        let p = got.projection.as_ref().expect("a band");
        assert_eq!(p.center.len(), SAMPLES + 1);
        assert!(p.center[SAMPLES].y < p.center[0].y, "carries the fall on");
        let width = |i: usize| p.upper[i].y - p.lower[i].y;
        assert!(width(SAMPLES) > width(0), "widens with the horizon");
        assert!(got.x_max > Some(t.entries().last().unwrap().recorded_at.ms()));
        assert_eq!(p.rate_label, "-0.7 kg/week");

        // A goal past six weeks pulls the horizon out to meet it.
        let goals = [goal(1, 55.0, "2025-08-01")];
        let with = build(&t, &goals, "morning", Range::All, true, now);
        assert!(with.x_max > got.x_max);
        assert!(nearly_equal(
            with.projection.unwrap().center[SAMPLES].y,
            55.0
        ));

        // On a short range it stays a small share of the chart.
        let c = build(&t, &[], "morning", days(7), true, now)
            .projection
            .unwrap()
            .center;
        let span = (c[SAMPLES].x - c[0].x) as f64 / 86_400_000.0;
        assert!(span > 0.0 && span <= 3.0, "{span}");

        let sparse = Timeline::new(&[
            entry(1, "2026-02-01 07:00", 90.0),
            entry(2, "2026-02-02 07:00", 89.9),
        ]);
        assert!(
            build(&sparse, &[], "morning", Range::All, true, now)
                .projection
                .is_none()
        );
        assert!(
            build(&t, &[], "morning-delta", Range::All, true, now)
                .projection
                .is_none()
        );
    }

    // The JSON chart-app.js reads.
    #[test]
    fn json_shape() {
        let empty = serde_json::to_value(build(
            &Timeline::default(),
            &[],
            "all",
            days(30),
            false,
            NOW,
        ))
        .unwrap();
        assert_eq!(
            empty,
            serde_json::json!({"hasData": false, "empty": "No entries in this range yet.", "isBar": false, "points": null})
        );
        let full = serde_json::to_value(build(&week(), &[], "all", days(30), false, NOW)).unwrap();
        for key in ["hasData", "isBar", "xMin", "xMax", "points", "trendMorning"] {
            assert!(full.get(key).is_some(), "{key}");
        }
        assert_eq!(full["points"][0]["color"], "morning");
        assert!(full.get("projection").is_none() && full.get("trend").is_none());
    }
}

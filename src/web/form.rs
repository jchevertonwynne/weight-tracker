//! A request's fields, and the forms they make.

use std::collections::HashMap;

use axum::body::Bytes;
use axum::extract::{FromRequest, Request};
use axum::http::{Method, header};
use axum::response::{IntoResponse, Response};
use jiff::Zoned;

use super::error::{Result, bad_request};
use crate::clock;
use crate::db::EntryFields;
use crate::model::{Grams, Period};

/// The first value of each field, from a form-encoded body and the query
/// string together, the body winning.
///
/// htmx sends fields in the query string for GET and DELETE and in the body
/// for POST and PUT, so one extractor serves every route. The body is read
/// for POST, PUT and PATCH, and only when it says it is a form: anything
/// else has no body fields, and fails validation on whichever is missing.
#[derive(Debug, Default)]
pub struct Fields(HashMap<String, String>);

impl Fields {
    pub fn get(&self, key: &str) -> &str {
        self.0.get(key).map_or("", String::as_str)
    }

    pub fn from_query(query: Option<&str>) -> Fields {
        let mut fields = Fields::default();
        fields.add(query.unwrap_or_default().as_bytes());
        fields
    }

    fn add(&mut self, encoded: &[u8]) {
        for (k, v) in form_urlencoded::parse(encoded) {
            self.0
                .entry(k.into_owned())
                .or_insert_with(|| v.into_owned());
        }
    }

    /// The kilograms field, as grams.
    pub fn weight(&self) -> Result<Grams> {
        weight(self.get("weight_kg"))
    }

    /// A date input ("2026-08-16") as local midnight.
    pub fn date(&self, field: &str) -> Result<Zoned> {
        clock::parse(self.get(field), "%Y-%m-%d", &clock::tz())
            .ok_or_else(|| bad_request(format!("invalid {field}")))
    }

    /// A weigh-in's fields: the weight, separate date and time inputs (one
    /// datetime-local input means a two-screen native picker on some
    /// phones), and the period, "" to follow the clock.
    pub fn entry(&self) -> Result<EntryFields> {
        let weight = self.weight()?;
        let at = format!(
            "{}T{}",
            self.get("recorded_at_date"),
            self.get("recorded_at_time")
        );
        let recorded_at = clock::parse(&at, "%Y-%m-%dT%H:%M", &clock::tz())
            .ok_or_else(|| bad_request("invalid recorded_at"))?;
        let period_override = match self.get("period_override") {
            "" => None,
            p => Some(
                p.parse::<Period>()
                    .map_err(|_| bad_request("invalid period_override"))?,
            ),
        };
        Ok(EntryFields {
            recorded_at,
            weight,
            period_override,
        })
    }
}

impl<S: Send + Sync> FromRequest<S> for Fields {
    type Rejection = Response;

    async fn from_request(req: Request, state: &S) -> std::result::Result<Fields, Response> {
        let form_body = matches!(*req.method(), Method::POST | Method::PUT | Method::PATCH)
            && req
                .headers()
                .get(header::CONTENT_TYPE)
                .and_then(|v| v.to_str().ok())
                .and_then(|v| v.split(';').next())
                .is_some_and(|v| {
                    v.trim()
                        .eq_ignore_ascii_case("application/x-www-form-urlencoded")
                });
        let query = req.uri().query().map(str::to_string);
        let mut fields = Fields::default();
        if form_body {
            let body = Bytes::from_request(req, state)
                .await
                .map_err(IntoResponse::into_response)?;
            fields.add(&body);
        }
        fields.add(query.unwrap_or_default().as_bytes());
        Ok(fields)
    }
}

/// The heaviest weight taken: well clear of any person, low enough to catch
/// a slip or the wrong unit.
const MAX_KG: f64 = 1000.0;

/// Kilograms as typed into the form, checked here because the input's own
/// min and step are only advice to a browser.
fn weight(kg: &str) -> Result<Grams> {
    let kg: f64 = kg
        .parse()
        .map_err(|e| bad_request(format!("weight_kg is not a number: {e}")))?;
    if !kg.is_finite() {
        return Err(bad_request("weight_kg must be a finite number"));
    }
    if kg <= 0.0 || kg > MAX_KG {
        return Err(bad_request(format!(
            "weight_kg must be between 0 and {MAX_KG}, got {kg}"
        )));
    }
    let grams = Grams::from_kg(kg);
    if grams.0 <= 0 {
        return Err(bad_request(format!("weight_kg {kg} rounds to zero grams")));
    }
    Ok(grams)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn weights() {
        for ok in ["82.4", "0.1", "1000"] {
            assert!(weight(ok).is_ok(), "{ok}");
        }
        for bad in [
            "-50", "0", "abc", "", "5000", "NaN", "Inf", "-inf", "0.0004",
        ] {
            assert!(weight(bad).is_err(), "{bad:?}");
        }
    }
}

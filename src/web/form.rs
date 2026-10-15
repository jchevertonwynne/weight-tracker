//! Request fields as Go's `r.FormValue` read them.

use std::collections::HashMap;

use axum::body::Bytes;
use axum::extract::{FromRequest, Request};
use axum::http::{Method, header};
use axum::response::{IntoResponse, Response};

/// The first value of each field, from a form-encoded body and the URL's
/// query together, the body winning — what `r.FormValue` gave.
///
/// htmx puts included fields in the query string for GET and DELETE, and in
/// the body for POST and PUT, so one extractor covers every route. The body
/// is read only for POST, PUT and PATCH, and only when it says it is a form,
/// as `ParseForm` did: anything else has no body fields at all, and is then
/// refused by whatever field turns out missing.
#[derive(Debug, Default)]
pub struct Form(HashMap<String, String>);

impl Form {
    pub fn get(&self, key: &str) -> &str {
        self.0.get(key).map(String::as_str).unwrap_or_default()
    }

    /// The query string alone, as `r.URL.Query().Get` read it.
    pub fn from_query(query: Option<&str>) -> Self {
        let mut form = Form::default();
        form.add(query.unwrap_or_default().as_bytes());
        form
    }

    fn add(&mut self, encoded: &[u8]) {
        for (k, v) in form_urlencoded::parse(encoded) {
            self.0
                .entry(k.into_owned())
                .or_insert_with(|| v.into_owned());
        }
    }
}

impl<S: Send + Sync> FromRequest<S> for Form {
    type Rejection = Response;

    async fn from_request(req: Request, state: &S) -> Result<Self, Self::Rejection> {
        let reads_body = matches!(*req.method(), Method::POST | Method::PUT | Method::PATCH)
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
        let mut form = Form::default();
        if reads_body {
            let body = Bytes::from_request(req, state)
                .await
                .map_err(IntoResponse::into_response)?;
            form.add(&body);
        }
        form.add(query.unwrap_or_default().as_bytes());
        Ok(form)
    }
}

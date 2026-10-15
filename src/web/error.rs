//! How a handler fails.

use axum::extract::{FromRequestParts, Path};
use axum::http::StatusCode;
use axum::http::request::Parts;
use axum::response::{IntoResponse, Response};

#[derive(Debug)]
pub enum Error {
    /// The request was wrong, and the message says how.
    BadRequest(String),
    /// What it asked for is not there — a stale second tab, say.
    NotFound,
    /// Ours. Logged in the request's span, so it joins up with its trace; the
    /// client is told only that it happened.
    Internal(anyhow::Error),
}

pub type Result<T = Response> = std::result::Result<T, Error>;

pub fn bad_request(msg: impl Into<String>) -> Error {
    Error::BadRequest(msg.into())
}

impl<E: Into<anyhow::Error>> From<E> for Error {
    fn from(err: E) -> Error {
        Error::Internal(err.into())
    }
}

impl IntoResponse for Error {
    fn into_response(self) -> Response {
        match self {
            Error::BadRequest(msg) => homelab::http::error(StatusCode::BAD_REQUEST, &msg),
            Error::NotFound => homelab::http::plain_not_found(),
            Error::Internal(err) => {
                tracing::error!(error = format!("{err:#}"), "request failed");
                homelab::http::error(StatusCode::INTERNAL_SERVER_ERROR, "internal error")
            }
        }
    }
}

/// A row id from the path, or a 400.
pub struct Id(pub i64);

impl<S: Send + Sync> FromRequestParts<S> for Id {
    type Rejection = Error;

    async fn from_request_parts(parts: &mut Parts, state: &S) -> Result<Id> {
        let Path(raw) = Path::<String>::from_request_parts(parts, state)
            .await
            .map_err(|_| bad_request("invalid id"))?;
        raw.parse().map(Id).map_err(|_| bad_request("invalid id"))
    }
}

/// `Ok(true)` from an update or delete, or a 404.
pub fn found(changed: bool) -> Result<()> {
    if changed {
        Ok(())
    } else {
        Err(Error::NotFound)
    }
}

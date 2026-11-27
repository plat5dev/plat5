mod cache;
mod state;
mod validate;

pub use cache::{is_unexpired, JwtCache};
pub use state::{JwksUnavailable, JwtValidatorState};
pub use validate::{extract_kid, validate_token};

/// The audiences a token's `aud` must hold one of (`AUTH_ALLOWED_AUDIENCES`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Audiences {
    /// `*`: any audience.
    Any,
    OneOf(Vec<String>),
}

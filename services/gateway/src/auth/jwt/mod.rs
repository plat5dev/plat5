mod cache;
mod state;
mod validate;

pub use cache::JwtCache;
pub use state::JwtValidatorState;
pub use validate::validate_token;

/// The audiences a token's `aud` must hold one of (`AUTH_ALLOWED_AUDIENCES`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Audiences {
    /// `*`: any audience.
    Any,
    OneOf(Vec<String>),
}

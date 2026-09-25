/// Credential kind used for metrics, logs, and admission telemetry.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AuthType {
    Jwt,
    UserApiKey,
    MemberApiKey,
    MemberSession,
}

impl AuthType {
    pub fn as_str(self) -> &'static str {
        match self {
            AuthType::Jwt => "jwt",
            AuthType::UserApiKey => "user_apikey",
            AuthType::MemberApiKey => "member_apikey",
            AuthType::MemberSession => "member_session",
        }
    }
}

use pingora::http::ResponseHeader;
use pingora::Result;

/// CORS policy from `ALLOWED_ORIGINS`. Empty allowlist → `*`.
#[derive(Clone, Debug)]
pub struct CorsPolicy {
    allowed_origins: Vec<String>,
}

impl CorsPolicy {
    pub fn new(allowed_origins: Vec<String>) -> Self {
        Self { allowed_origins }
    }

    pub fn apply(
        &self,
        header: &mut ResponseHeader,
        request_origin: Option<&str>,
    ) -> Result<()> {
        if self.allowed_origins.is_empty() {
            header.insert_header("Access-Control-Allow-Origin", "*")?;
        } else if let Some(origin) = request_origin {
            if self.allowed_origins.iter().any(|allowed| allowed == origin) {
                header.insert_header("Access-Control-Allow-Origin", origin)?;
                header.insert_header("Vary", "Origin")?;
            }
        }
        header.insert_header(
            "Access-Control-Allow-Methods",
            "GET, POST, PUT, PATCH, DELETE, OPTIONS",
        )?;
        header.insert_header(
            "Access-Control-Allow-Headers",
            "Content-Type, Authorization, X-API-Key",
        )?;
        Ok(())
    }
}

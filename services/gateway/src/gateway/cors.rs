use pingora::http::ResponseHeader;
use pingora::Result;

pub fn apply_cors(
    allowed_origins: &[String],
    header: &mut ResponseHeader,
    request_origin: Option<&str>,
) -> Result<()> {
    if allowed_origins.is_empty() {
        header.insert_header("Access-Control-Allow-Origin", "*")?;
    } else if let Some(origin) = request_origin {
        if allowed_origins.iter().any(|allowed| allowed == origin) {
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

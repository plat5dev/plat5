use pingora::http::ResponseHeader;
use pingora::Result;

const ALLOW_METHODS: &str = "GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS";
const BASE_ALLOW_HEADERS: &str =
    "Authorization, Content-Type, X-Request-ID, traceparent, tracestate";
const EXPOSE_HEADERS: &str =
    "X-Request-ID, Retry-After, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset";

/// CORS from the control plane's `cors` document (docs/reference/gateway.md#cors).
/// `["*"]` allows any origin; an empty list allows none.
#[derive(Clone, Debug)]
pub struct CorsPolicy {
    allowed_origins: Vec<String>,
    allow_headers: String,
}

impl CorsPolicy {
    pub fn new(allowed_origins: Vec<String>, allowed_headers: &[String]) -> Self {
        let allow_headers = std::iter::once(BASE_ALLOW_HEADERS)
            .chain(allowed_headers.iter().map(String::as_str))
            .collect::<Vec<_>>()
            .join(", ");
        Self {
            allowed_origins,
            allow_headers,
        }
    }

    /// `Access-Control-Allow-Origin` for this request's `Origin`, and whether it
    /// varies by origin. None = no cross-origin request is allowed.
    fn allow_origin<'a>(&'a self, request_origin: Option<&'a str>) -> Option<(&'a str, bool)> {
        if self.allowed_origins.iter().any(|o| o == "*") {
            return Some(("*", false));
        }
        let origin = request_origin?;
        self.allowed_origins
            .iter()
            .any(|allowed| allowed == origin)
            .then_some((origin, true))
    }

    fn apply_origin(
        &self,
        header: &mut ResponseHeader,
        request_origin: Option<&str>,
    ) -> Result<bool> {
        let Some((origin, varies)) = self.allow_origin(request_origin) else {
            return Ok(false);
        };
        header.insert_header("Access-Control-Allow-Origin", origin)?;
        if varies {
            header.insert_header("Vary", "Origin")?;
        }
        Ok(true)
    }

    /// On every response but a preflight: the origin, and the headers a browser may read.
    pub fn apply(&self, header: &mut ResponseHeader, request_origin: Option<&str>) -> Result<()> {
        if self.apply_origin(header, request_origin)? {
            header.insert_header("Access-Control-Expose-Headers", EXPOSE_HEADERS)?;
        }
        Ok(())
    }

    /// On a preflight: the origin, and what a request may send.
    pub fn apply_preflight(
        &self,
        header: &mut ResponseHeader,
        request_origin: Option<&str>,
    ) -> Result<()> {
        self.apply_origin(header, request_origin)?;
        header.insert_header("Access-Control-Allow-Methods", ALLOW_METHODS)?;
        header.insert_header("Access-Control-Allow-Headers", self.allow_headers.as_str())?;
        header.insert_header("Access-Control-Max-Age", "86400")?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn header_of(h: &ResponseHeader, name: &str) -> Option<String> {
        h.headers.get(name).map(|v| v.to_str().unwrap().to_string())
    }

    fn response(policy: &CorsPolicy, origin: Option<&str>) -> ResponseHeader {
        let mut h = ResponseHeader::build(200, None).unwrap();
        policy.apply(&mut h, origin).unwrap();
        h
    }

    #[test]
    fn any_origin_is_written_as_a_star() {
        let p = CorsPolicy::new(vec!["*".into()], &[]);
        let h = response(&p, Some("https://x.example"));
        assert_eq!(
            header_of(&h, "Access-Control-Allow-Origin").as_deref(),
            Some("*")
        );
        assert_eq!(header_of(&h, "Vary"), None);
        assert_eq!(
            header_of(&h, "Access-Control-Expose-Headers").as_deref(),
            Some(EXPOSE_HEADERS)
        );
    }

    #[test]
    fn a_listed_origin_is_echoed_and_others_get_nothing() {
        let p = CorsPolicy::new(vec!["https://app.example.com".into()], &[]);
        let h = response(&p, Some("https://app.example.com"));
        assert_eq!(
            header_of(&h, "Access-Control-Allow-Origin").as_deref(),
            Some("https://app.example.com")
        );
        assert_eq!(header_of(&h, "Vary").as_deref(), Some("Origin"));
        for origin in [Some("https://evil.example"), None] {
            let h = response(&p, origin);
            assert_eq!(header_of(&h, "Access-Control-Allow-Origin"), None);
            assert_eq!(header_of(&h, "Access-Control-Expose-Headers"), None);
        }
        // An empty list allows no cross-origin requests.
        let none = CorsPolicy::new(vec![], &[]);
        assert_eq!(
            header_of(
                &response(&none, Some("https://app.example.com")),
                "Access-Control-Allow-Origin"
            ),
            None
        );
    }

    #[test]
    fn a_preflight_lists_methods_and_headers_with_the_configured_ones() {
        let p = CorsPolicy::new(vec!["*".into()], &["idempotency-key".into()]);
        let mut h = ResponseHeader::build(204, None).unwrap();
        p.apply_preflight(&mut h, Some("https://x.example"))
            .unwrap();
        assert_eq!(
            header_of(&h, "Access-Control-Allow-Methods").as_deref(),
            Some(ALLOW_METHODS)
        );
        assert_eq!(
            header_of(&h, "Access-Control-Allow-Headers").as_deref(),
            Some("Authorization, Content-Type, X-Request-ID, traceparent, tracestate, idempotency-key")
        );
        assert_eq!(
            header_of(&h, "Access-Control-Max-Age").as_deref(),
            Some("86400")
        );
        assert_eq!(
            header_of(&h, "Access-Control-Allow-Origin").as_deref(),
            Some("*")
        );
    }
}

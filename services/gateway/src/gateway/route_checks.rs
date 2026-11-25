//! `POST /access/routes` handler (docs/reference/gateway.md#route-checks).

use opentelemetry::trace::TraceContextExt;
use pingora::proxy::Session;
use pingora::Result;
use tracing::warn;
use tracing_opentelemetry::OpenTelemetrySpanExt;

use crate::control_plane::Snapshot;
use crate::cors::CorsPolicy;
use crate::error::ApiError;
use crate::pipeline::{parse_checks, GatewayPipeline, Rejection, MAX_CHECKS_BODY};

use super::context::GatewayContext;
use super::{client_ip, request_host, response, Gateway, ROUTE_CHECKS_PATH};

impl Gateway {
    /// Route checks (docs/reference/gateway.md#route-checks): the credential as
    /// a route takes it, the body, one count of each policy `defaults.rate_limit`
    /// names, then each check through the pipeline's own match, admit and
    /// permission steps. Never forwarded, never audited.
    pub(super) async fn handle_route_checks(
        &self,
        session: &mut Session,
        ctx: &mut GatewayContext,
        snapshot: &Snapshot,
    ) -> Result<bool> {
        let route = ROUTE_CHECKS_PATH.to_string();
        if let Some(ref span) = ctx.root_span {
            span.record("http.route", route.as_str());
            span.context().span().update_name(format!("POST {route}"));
        }
        ctx.route = Some(route);
        ctx.max_body_bytes = MAX_CHECKS_BODY;
        let cors = snapshot.cors.clone();

        let presented = self
            .authenticator
            .present(
                session.req_header(),
                &self.name_claims,
                &snapshot.policy_claims,
            )
            .await;
        ctx.credential.auth_type = match &presented {
            Ok(p) => Some(p.auth_type()),
            Err(err) => err.auth_type(),
        };
        let credential = match GatewayPipeline::check_credential(presented) {
            Ok(c) => c,
            Err(rejection) => return write_check_rejection(&cors, session, ctx, rejection).await,
        };

        if let Some(done) = self.reject_oversized_content_length(session, ctx).await? {
            return Ok(done);
        }
        let mut body = Vec::new();
        while let Some(chunk) = session.read_request_body().await? {
            if body.len() as u64 + chunk.len() as u64 > MAX_CHECKS_BODY {
                warn!(
                    max_body_size = MAX_CHECKS_BODY,
                    "route checks body too large"
                );
                return response::write_json_error(
                    &cors,
                    session,
                    ctx,
                    413,
                    ApiError::payload_too_large(MAX_CHECKS_BODY),
                )
                .await;
            }
            body.extend_from_slice(&chunk);
        }
        let checks = match parse_checks(&body) {
            Ok(checks) => checks,
            Err(fields) => {
                let fields: Vec<(String, String)> =
                    fields.into_iter().map(|f| (f.path, f.message)).collect();
                return response::write_json_error(
                    &cors,
                    session,
                    ctx,
                    422,
                    ApiError::validation(&fields),
                )
                .await;
            }
        };

        let ip = client_ip(session, &self.trusted_proxies);
        let host = request_host(session.req_header()).map(str::to_string);
        let checked = self
            .pipeline
            .check_routes_with_authorizer(
                &snapshot.routes,
                &snapshot.default_policies,
                snapshot.authorizer.as_ref(),
                &credential,
                &checks,
                &ip,
                host.as_deref(),
                ctx.request_id.as_deref().unwrap_or(""),
            )
            .await;
        let checked = match checked {
            Ok(c) => c,
            Err(rejection) => return write_check_rejection(&cors, session, ctx, rejection).await,
        };
        if checked.key_id.is_some() {
            ctx.credential.key_id = checked.key_id;
        }
        let answer = serde_json::json!({
            "checks": checked
                .allowed
                .iter()
                .map(|allowed| serde_json::json!({ "allowed": allowed }))
                .collect::<Vec<_>>()
        });
        let extras: Vec<(&'static str, String)> = checked
            .rate_limit
            .as_ref()
            .map(|info| response::rate_limit_headers(info).into_iter().collect())
            .unwrap_or_default();
        response::send_json(
            &cors,
            session,
            ctx,
            200,
            answer.to_string().into_bytes(),
            &extras,
        )
        .await?;
        Ok(true)
    }
}

/// A route checks call refused whole: its 401, 429 or 503.
async fn write_check_rejection(
    cors: &CorsPolicy,
    session: &mut Session,
    ctx: &GatewayContext,
    rejection: Rejection,
) -> Result<bool> {
    match rejection {
        Rejection::RateLimited { retry_after, info } => {
            response::write_rate_limited(cors, session, ctx, retry_after, info.as_ref()).await
        }
        Rejection::Unauthorized(err) => {
            response::write_json_error(
                cors,
                session,
                ctx,
                401,
                ApiError::unauthorized_any(err.details()),
            )
            .await
        }
        other => {
            response::write_json_error(
                cors,
                session,
                ctx,
                other.status(),
                ApiError::service_unavailable(),
            )
            .await
        }
    }
}

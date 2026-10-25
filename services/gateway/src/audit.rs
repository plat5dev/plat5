//! Audit log writes (docs/concepts/audit.md). On an audited route the gateway writes
//! an intent after the permission check and the fill, and waits on it, then an
//! outcome after, in the background. Its own refusals after admit are one
//! background write: an intent that carries its `rejected` outcome. Audit is on or
//! off for the whole deployment. Wire: docs/reference/api/internal.md#audit.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use reqwest::Method;
use serde::Serialize;
use serde_json::value::RawValue;
use tokio::sync::{mpsc, Semaphore};
use tracing::{error, warn, Instrument};
use tracing_opentelemetry::OpenTelemetrySpanExt;

use crate::auth::admit::NodeRef;
use crate::error::ErrorKind;
use crate::internal_http::InternalHttpClient;
use crate::metrics;

/// A service says what changed on this response header. The gateway stores it as
/// sent and strips it, from requests and responses, on every route.
pub const DETAILS_HEADER: &str = "X-Plat5-Audit-Details";
/// The id a create produced.
pub const CREATED_HEADER: &str = "X-Plat5-Audit-Created";
/// On a user route, the chain of nodes the service changed, root first, and the
/// person's member in that tree.
pub const NODES_HEADER: &str = "X-Plat5-Audit-Nodes";
pub const MEMBER_HEADER: &str = "X-Plat5-Audit-Member";
/// Every response header a service uses to talk to the audit log starts with
/// this. Stripped from every response.
pub const SERVICE_HEADER_PREFIX: &str = "x-plat5-audit-";
pub const MAX_DETAILS_BYTES: usize = 4096;
/// An id from a service header: one path segment of visible ASCII.
pub const MAX_ID_BYTES: usize = 128;
pub const MAX_NAMED_NODES: usize = 32;
pub const MAX_USER_AGENT_CHARS: usize = 512;

const INTENT_ATTEMPTS: u32 = 3;
const INTENT_ATTEMPT_TIMEOUT: Duration = Duration::from_millis(500);
/// The client waits on the intent, so every attempt fits in this.
const INTENT_BUDGET: Duration = Duration::from_secs(1);

const BACKGROUND_QUEUE: usize = 10_000;
const BACKGROUND_IN_FLIGHT: usize = 64;
const BACKGROUND_ATTEMPT_TIMEOUT: Duration = Duration::from_secs(2);
/// Wait before each background retry. About five minutes in all, then it's dropped.
const BACKGROUND_BACKOFF: &[Duration] = &[
    Duration::from_millis(200),
    Duration::from_secs(1),
    Duration::from_secs(5),
    Duration::from_secs(15),
    Duration::from_secs(30),
    Duration::from_secs(60),
    Duration::from_secs(60),
    Duration::from_secs(60),
    Duration::from_secs(60),
];

/// Whose credential was admitted, and which credential: the member on a kind
/// route, the user on a user route. A JWT has no key id.
#[derive(Debug, Clone, Serialize)]
pub struct Actor {
    pub member_id: Option<String>,
    pub user_id: Option<String>,
    /// `person` or `service_account`.
    pub principal: String,
    pub auth_type: &'static str,
    pub key_id: Option<String>,
}

/// Everything the gateway knows before forward. `request_id` is in the URL.
/// On a user route `nodes` is empty.
#[derive(Debug, Clone, Serialize)]
pub struct Intent {
    #[serde(skip)]
    pub request_id: String,
    pub occurred_at: String,
    pub nodes: Vec<NodeRef>,
    pub actor: Actor,
    pub service: String,
    pub method: String,
    pub route: String,
    pub params: BTreeMap<String, String>,
    pub ip: String,
    pub user_agent: Option<String>,
}

/// A gateway refusal after admit: the intent, written final.
#[derive(Debug, Serialize)]
pub struct Rejected {
    #[serde(flatten)]
    pub intent: Intent,
    pub outcome: OutcomeKind,
    pub status: u16,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum OutcomeKind {
    /// The gateway answered without calling the service.
    Rejected,
    /// The service answered.
    Responded,
    /// The gateway called the service and got no answer.
    NoResponse,
}

#[derive(Debug, Serialize)]
pub struct Outcome {
    pub outcome: OutcomeKind,
    /// What the gateway answered with. None when the client was gone first.
    pub status: Option<u16>,
    pub details: Option<Box<RawValue>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub created_id: Option<String>,
    /// On a user route, the chain the service named and the member in it.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub named_nodes: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub named_member_id: Option<String>,
}

/// An audited request between its intent and its outcome.
#[derive(Debug, Default)]
pub struct AuditState {
    /// Set when the upstream response arrives.
    pub upstream_status: Option<u16>,
    pub details: Option<Box<RawValue>>,
    pub created_id: Option<String>,
    /// The chain and member a service named on a user route's 2xx.
    pub named: Option<(Vec<String>, String)>,
}

/// How the request ended. `forwarded` = the gateway handed it to the upstream.
/// What a service said only counts when it answered.
pub fn outcome_for(state: AuditState, forwarded: bool, written_status: Option<u16>) -> Outcome {
    let (outcome, status) = match (state.upstream_status, forwarded) {
        (Some(status), _) => (OutcomeKind::Responded, Some(status)),
        (None, true) => (OutcomeKind::NoResponse, written_status),
        (None, false) => (OutcomeKind::Rejected, written_status),
    };
    if outcome != OutcomeKind::Responded {
        return Outcome {
            outcome,
            status,
            details: None,
            created_id: None,
            named_nodes: None,
            named_member_id: None,
        };
    }
    let (named_nodes, named_member_id) = state.named.unzip();
    Outcome {
        outcome,
        status,
        details: state.details,
        created_id: state.created_id,
        named_nodes,
        named_member_id,
    }
}

/// An id header: visible ASCII, one path segment, at most [`MAX_ID_BYTES`].
pub fn parse_id(value: &[u8]) -> Option<String> {
    let ok = !value.is_empty()
        && value.len() <= MAX_ID_BYTES
        && value
            .iter()
            .all(|b| (0x21..=0x7e).contains(b) && *b != b'/');
    ok.then(|| String::from_utf8_lossy(value).into_owned())
}

/// The named-nodes header: comma-separated ids, at most [`MAX_NAMED_NODES`], or None.
pub fn parse_nodes(value: &[u8]) -> Option<Vec<String>> {
    let ids: Vec<String> = value
        .split(|b| *b == b',')
        .map(|id| parse_id(id.trim_ascii()))
        .collect::<Option<_>>()?;
    (ids.len() <= MAX_NAMED_NODES).then_some(ids)
}

/// The details header as one JSON object of visible ASCII within the cap, or None.
pub fn parse_details(value: &[u8]) -> Option<Box<RawValue>> {
    if value.len() > MAX_DETAILS_BYTES || !value.iter().all(|b| (0x20..=0x7e).contains(b)) {
        return None;
    }
    let text = std::str::from_utf8(value).ok()?.trim();
    if !text.starts_with('{') {
        return None;
    }
    serde_json::from_str::<Box<RawValue>>(text).ok()
}

/// `User-Agent`, cut to [`MAX_USER_AGENT_CHARS`] characters.
pub fn user_agent(value: &[u8]) -> String {
    String::from_utf8_lossy(value)
        .chars()
        .take(MAX_USER_AGENT_CHARS)
        .collect()
}

/// RFC 3339, UTC, milliseconds: `2026-10-08T18:30:00.123Z`.
pub fn rfc3339_millis(t: SystemTime) -> String {
    let since = t.duration_since(UNIX_EPOCH).unwrap_or_default();
    let secs = since.as_secs() as i64;
    let (y, m, d) = civil_from_days(secs.div_euclid(86_400));
    let s = secs.rem_euclid(86_400);
    format!(
        "{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}.{:03}Z",
        s / 3600,
        s % 3600 / 60,
        s % 60,
        since.subsec_millis()
    )
}

/// Days since 1970-01-01 to (year, month, day). Howard Hinnant's algorithm.
fn civil_from_days(days: i64) -> (i64, u32, u32) {
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    let y = yoe + era * 400 + i64::from(m <= 2);
    (y, m, d)
}

/// The gateway did not see the intent written. The reason is already logged.
#[derive(Debug)]
pub struct IntentNotWritten;

/// A write the client doesn't wait on, queued and retried.
#[derive(Debug)]
enum Background {
    Outcome(Outcome),
    Rejected(Box<Rejected>),
}

impl Background {
    /// The `write` label on `audit_writes_total`.
    fn write(&self) -> &'static str {
        match self {
            Background::Outcome(_) => "outcome",
            Background::Rejected(_) => "rejected",
        }
    }
}

/// The deployment's audit writer. Off = no writes and no waits.
#[derive(Clone)]
pub struct Audit {
    inner: Option<Arc<Inner>>,
}

struct Inner {
    base_url: String,
    http: InternalHttpClient,
    background: mpsc::Sender<(String, Background, opentelemetry::Context)>,
}

impl Audit {
    pub fn off() -> Self {
        Self { inner: None }
    }

    /// On when `base_url` is set. Background writes run on `runtime`, with their own client.
    pub fn new(base_url: Option<String>, runtime: &tokio::runtime::Handle) -> Self {
        let Some(base_url) = base_url else {
            return Self::off();
        };
        let (tx, rx) = mpsc::channel(BACKGROUND_QUEUE);
        runtime.spawn(run_background(
            base_url.clone(),
            InternalHttpClient::new(),
            rx,
        ));
        Self {
            inner: Some(Arc::new(Inner {
                base_url,
                http: InternalHttpClient::new(),
                background: tx,
            })),
        }
    }

    pub fn enabled(&self) -> bool {
        self.inner.is_some()
    }

    /// Write the intent, retrying within [`INTENT_BUDGET`]. Err = the gateway did not
    /// see it written; the caller answers 503 and does not forward.
    pub async fn write_intent(&self, intent: &Intent) -> Result<(), IntentNotWritten> {
        let Some(inner) = &self.inner else {
            return Ok(());
        };
        let url = event_url(&inner.base_url, &intent.request_id);
        let start = Instant::now();
        let deadline = start + INTENT_BUDGET;
        let mut last_error = String::new();

        for _ in 0..INTENT_ATTEMPTS {
            let left = deadline.saturating_duration_since(Instant::now());
            if left.is_zero() {
                break;
            }
            let timeout = left.min(INTENT_ATTEMPT_TIMEOUT);
            match inner
                .http
                .send_json(Method::PUT, &url, intent, timeout)
                .await
            {
                Ok(200 | 201) => {
                    metrics::record_audit_write("intent", "ok");
                    metrics::record_audit_intent_duration("ok", start.elapsed().as_secs_f64());
                    return Ok(());
                }
                // A 4xx is the same on every try: a field audit can't store.
                Ok(status @ 400..=499) => {
                    last_error = format!("audit returned status {status}");
                    break;
                }
                Ok(status) => last_error = format!("audit returned status {status}"),
                Err(err) => last_error = format!("{err:?}"),
            }
        }

        error!(
            error_kind = ErrorKind::Network.as_str(),
            error_message = %last_error,
            request_id = %intent.request_id,
            "audit intent not written; not forwarding"
        );
        metrics::record_audit_write("intent", "failed");
        metrics::record_audit_intent_duration("failed", start.elapsed().as_secs_f64());
        Err(IntentNotWritten)
    }

    /// Queue the outcome. Never waits. A full queue drops it and the event stays pending.
    /// `parent` is the request's trace: the write lands in it, after the response.
    pub fn send_outcome(
        &self,
        request_id: String,
        outcome: Outcome,
        parent: opentelemetry::Context,
    ) {
        self.queue(request_id, Background::Outcome(outcome), parent);
    }

    /// Queue a refusal after admit, as one event written final. Never waits.
    pub fn send_rejected(&self, intent: Intent, status: u16, parent: opentelemetry::Context) {
        let request_id = intent.request_id.clone();
        let rejected = Rejected {
            intent,
            outcome: OutcomeKind::Rejected,
            status,
        };
        self.queue(request_id, Background::Rejected(Box::new(rejected)), parent);
    }

    fn queue(&self, request_id: String, write: Background, parent: opentelemetry::Context) {
        let Some(inner) = &self.inner else {
            return;
        };
        if let Err(err) = inner.background.try_send((request_id, write, parent)) {
            let (request_id, write, _) = err.into_inner();
            warn!(
                request_id = %request_id,
                write = write.write(),
                "audit queue full; write dropped"
            );
            metrics::record_audit_write(write.write(), "dropped");
        }
    }
}

fn event_url(base_url: &str, request_id: &str) -> String {
    format!("{base_url}/internal/events/{request_id}")
}

async fn run_background(
    base_url: String,
    http: InternalHttpClient,
    mut rx: mpsc::Receiver<(String, Background, opentelemetry::Context)>,
) {
    let permits = Arc::new(Semaphore::new(BACKGROUND_IN_FLIGHT));
    while let Some((request_id, write, parent)) = rx.recv().await {
        let Ok(permit) = permits.clone().acquire_owned().await else {
            return;
        };
        let http = http.clone();
        let url = event_url(&base_url, &request_id);
        let span = tracing::info_span!("audit.write", write = write.write());
        // Err only when the span is disabled: nothing to trace then.
        let _ = span.set_parent(parent);
        tokio::spawn(
            async move {
                write_background(&http, &url, &request_id, &write, BACKGROUND_BACKOFF).await;
                drop(permit);
            }
            .instrument(span),
        );
    }
}

/// Send a background write until audit takes it, refuses it, or the backoff runs
/// out. Every write is idempotent by `request_id`, so a retry after a lost reply is
/// harmless.
async fn write_background(
    http: &InternalHttpClient,
    url: &str,
    request_id: &str,
    write: &Background,
    backoff: &[Duration],
) {
    let label = write.write();
    let mut waits = backoff.iter();
    loop {
        let sent = match write {
            Background::Outcome(outcome) => {
                http.send_json(Method::PATCH, url, outcome, BACKGROUND_ATTEMPT_TIMEOUT)
                    .await
            }
            Background::Rejected(rejected) => {
                http.send_json(Method::PUT, url, rejected, BACKGROUND_ATTEMPT_TIMEOUT)
                    .await
            }
        };
        match sent {
            Ok(200..=299) => {
                metrics::record_audit_write(label, "ok");
                return;
            }
            // An outcome whose intent never landed (the 503 path): nothing to finish.
            Ok(404) if matches!(write, Background::Outcome(_)) => {
                metrics::record_audit_write(label, "not_found");
                return;
            }
            Ok(status @ 400..=499) => {
                error!(
                    error_kind = ErrorKind::Internal.as_str(),
                    status,
                    request_id,
                    write = label,
                    "audit refused the write"
                );
                metrics::record_audit_write(label, "failed");
                return;
            }
            _ => {}
        }
        let Some(wait) = waits.next() else {
            error!(
                error_kind = ErrorKind::Network.as_str(),
                request_id,
                write = label,
                "audit write not taken; giving up"
            );
            metrics::record_audit_write(label, "failed");
            return;
        };
        tokio::time::sleep(*wait).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;

    #[test]
    fn rfc3339_millis_formats_utc() {
        assert_eq!(rfc3339_millis(UNIX_EPOCH), "1970-01-01T00:00:00.000Z");
        let t = UNIX_EPOCH + Duration::from_millis(1_791_484_200_123);
        assert_eq!(rfc3339_millis(t), "2026-10-08T18:30:00.123Z");
        // Leap day and a year end.
        let leap = UNIX_EPOCH + Duration::from_secs(1_709_164_800);
        assert_eq!(rfc3339_millis(leap), "2024-02-29T00:00:00.000Z");
        let eoy = UNIX_EPOCH + Duration::from_millis(1_798_761_599_999);
        assert_eq!(rfc3339_millis(eoy), "2026-12-31T23:59:59.999Z");
    }

    #[test]
    fn details_is_one_ascii_json_object_within_the_cap() {
        let ok = br#" {"role":{"from":"developer","to":"admin"}} "#;
        assert_eq!(
            parse_details(ok).unwrap().get(),
            r#"{"role":{"from":"developer","to":"admin"}}"#
        );
        // Non-ASCII arrives escaped.
        assert!(parse_details(b"{\"name\":\"\\u00e9\"}").is_some());
        for bad in [
            &br#"[1,2]"#[..],
            br#""text""#,
            br#"{"a":"#,
            "{\"name\":\"é\"}".as_bytes(),
            b"{\"a\":\"\x01\"}",
        ] {
            assert!(
                parse_details(bad).is_none(),
                "{:?}",
                String::from_utf8_lossy(bad)
            );
        }
        let big = format!(r#"{{"a":"{}"}}"#, "x".repeat(MAX_DETAILS_BYTES));
        assert!(parse_details(big.as_bytes()).is_none());
    }

    #[test]
    fn user_agent_is_cut_by_characters() {
        assert_eq!(user_agent(b"curl/8.7.1"), "curl/8.7.1");
        let long = "é".repeat(MAX_USER_AGENT_CHARS + 10);
        assert_eq!(
            user_agent(long.as_bytes()).chars().count(),
            MAX_USER_AGENT_CHARS
        );
    }

    #[test]
    fn outcome_follows_how_the_request_ended() {
        let named = (
            vec!["01o".to_string(), "01p".to_string()],
            "01m".to_string(),
        );
        let responded = outcome_for(
            AuditState {
                upstream_status: Some(201),
                details: parse_details(br#"{"a":1}"#),
                created_id: Some("01o".into()),
                named: Some(named.clone()),
            },
            true,
            Some(201),
        );
        assert_eq!(responded.outcome, OutcomeKind::Responded);
        assert_eq!(responded.status, Some(201));
        assert!(responded.details.is_some());
        assert_eq!(responded.created_id.as_deref(), Some("01o"));
        assert_eq!(responded.named_nodes, Some(named.0.clone()));
        assert_eq!(responded.named_member_id.as_deref(), Some("01m"));

        // What a service said counts only when it answered.
        let cut = outcome_for(
            AuditState {
                upstream_status: None,
                details: None,
                created_id: Some("x".into()),
                named: Some(named),
            },
            true,
            Some(502),
        );
        assert!(cut.created_id.is_none() && cut.named_nodes.is_none());

        let no_response = outcome_for(AuditState::default(), true, Some(502));
        assert_eq!(no_response.outcome, OutcomeKind::NoResponse);
        assert_eq!(no_response.status, Some(502));

        let gone = outcome_for(AuditState::default(), true, None);
        assert_eq!(gone.status, None);

        let rejected = outcome_for(AuditState::default(), false, Some(503));
        assert_eq!(rejected.outcome, OutcomeKind::Rejected);
        assert_eq!(rejected.status, Some(503));
    }

    #[test]
    fn ids_are_one_visible_segment() {
        assert_eq!(parse_id(b"01j9zx").as_deref(), Some("01j9zx"));
        for bad in [
            &b""[..],
            b"a b",
            b"a/b",
            "é".as_bytes(),
            &[b'a'; MAX_ID_BYTES + 1],
        ] {
            assert!(parse_id(bad).is_none(), "{bad:?}");
        }
    }

    #[test]
    fn named_nodes_are_up_to_32_ids() {
        assert_eq!(
            parse_nodes(b"01o, 01p,01e"),
            Some(vec!["01o".into(), "01p".into(), "01e".into()])
        );
        let many = vec!["01x"; MAX_NAMED_NODES].join(",");
        assert_eq!(parse_nodes(many.as_bytes()).map(|v| v.len()), Some(32));
        let too_many = vec!["01x"; MAX_NAMED_NODES + 1].join(",");
        for bad in [
            &b""[..],
            b"01o,",
            b"01o,,01p",
            b"01o,a/b",
            too_many.as_bytes(),
        ] {
            assert!(
                parse_nodes(bad).is_none(),
                "{:?}",
                String::from_utf8_lossy(bad)
            );
        }
    }

    #[test]
    fn wire_shapes() {
        let mut tree = intent();
        tree.method = "PATCH".into();
        tree.nodes.push(NodeRef {
            kind: "project".into(),
            id: "01p".into(),
        });
        tree.route = "/orgs/{organization}/projects/{project}/bindings/{binding_id}".into();
        tree.params = BTreeMap::from([("binding_id".into(), "01b".into())]);
        let v = serde_json::to_value(&tree).unwrap();
        assert_eq!(
            v,
            serde_json::json!({
                "occurred_at": tree.occurred_at,
                "nodes": [{ "kind": "organization", "id": "01o" }, { "kind": "project", "id": "01p" }],
                "actor": { "member_id": "01m", "user_id": null, "principal": "service_account",
                           "auth_type": "apikey", "key_id": "01k" },
                "service": "s", "method": "PATCH",
                "route": "/orgs/{organization}/projects/{project}/bindings/{binding_id}",
                "params": { "binding_id": "01b" }, "ip": "127.0.0.1", "user_agent": null
            }),
            "request_id is in the URL"
        );

        let person = Intent {
            nodes: vec![],
            actor: Actor {
                member_id: None,
                user_id: Some("01u".into()),
                principal: "person".into(),
                auth_type: "jwt",
                key_id: None,
            },
            ..intent()
        };
        let v = serde_json::to_value(&person).unwrap();
        assert_eq!(v["nodes"], serde_json::json!([]));
        assert_eq!(
            v["actor"],
            serde_json::json!({ "member_id": null, "user_id": "01u", "principal": "person",
                                "auth_type": "jwt", "key_id": null })
        );

        let rejected = Rejected {
            intent: intent(),
            outcome: OutcomeKind::Rejected,
            status: 403,
        };
        let v = serde_json::to_value(&rejected).unwrap();
        assert_eq!(v["outcome"], "rejected");
        assert_eq!(v["status"], 403);
        assert_eq!(v["service"], "s");
        assert_eq!(v["actor"]["member_id"], "01m");

        let named = outcome_for(
            AuditState {
                upstream_status: Some(201),
                named: Some((vec!["01o".into()], "01m".into())),
                ..AuditState::default()
            },
            true,
            Some(201),
        );
        assert_eq!(
            serde_json::to_value(&named).unwrap(),
            serde_json::json!({ "outcome": "responded", "status": 201, "details": null,
                                "named_nodes": ["01o"], "named_member_id": "01m" })
        );

        let out = outcome_for(AuditState::default(), true, None);
        assert_eq!(
            serde_json::to_string(&out).unwrap(),
            r#"{"outcome":"no_response","status":null,"details":null}"#
        );
    }

    /// A one-shot HTTP server that answers each request with the next status in
    /// `statuses` (the last repeats), and counts requests.
    async fn fake_audit(statuses: Vec<u16>) -> (String, Arc<AtomicUsize>) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let hits = Arc::new(AtomicUsize::new(0));
        let counter = hits.clone();
        tokio::spawn(async move {
            loop {
                let Ok((mut sock, _)) = listener.accept().await else {
                    return;
                };
                let n = counter.fetch_add(1, Ordering::SeqCst);
                let status = *statuses.get(n).or(statuses.last()).unwrap();
                tokio::spawn(async move {
                    let mut buf = vec![0u8; 16 * 1024];
                    let _ = sock.read(&mut buf).await;
                    if status == 0 {
                        // Hang: the client's timeout decides.
                        tokio::time::sleep(Duration::from_secs(10)).await;
                        return;
                    }
                    let resp = format!(
                        "HTTP/1.1 {status} X\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"
                    );
                    let _ = sock.write_all(resp.as_bytes()).await;
                });
            }
        });
        (format!("http://{addr}"), hits)
    }

    fn intent() -> Intent {
        Intent {
            request_id: "req-1".into(),
            occurred_at: rfc3339_millis(SystemTime::now()),
            nodes: vec![NodeRef {
                kind: "organization".into(),
                id: "01o".into(),
            }],
            actor: Actor {
                member_id: Some("01m".into()),
                user_id: None,
                principal: "service_account".into(),
                auth_type: "apikey",
                key_id: Some("01k".into()),
            },
            service: "s".into(),
            method: "POST".into(),
            route: "/r".into(),
            params: BTreeMap::new(),
            ip: "127.0.0.1".into(),
            user_agent: None,
        }
    }

    fn audit_at(url: String) -> Audit {
        Audit::new(Some(url), &tokio::runtime::Handle::current())
    }

    #[tokio::test]
    async fn intent_retries_a_5xx_then_succeeds() {
        let (url, hits) = fake_audit(vec![503, 201]).await;
        assert!(audit_at(url).write_intent(&intent()).await.is_ok());
        assert_eq!(hits.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn intent_retry_finds_it_already_written() {
        let (url, _) = fake_audit(vec![500, 200]).await;
        assert!(audit_at(url).write_intent(&intent()).await.is_ok());
    }

    #[tokio::test]
    async fn intent_does_not_retry_a_4xx() {
        let (url, hits) = fake_audit(vec![422]).await;
        assert!(audit_at(url).write_intent(&intent()).await.is_err());
        assert_eq!(hits.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn intent_gives_up_within_the_budget() {
        let (url, hits) = fake_audit(vec![0]).await;
        let start = Instant::now();
        assert!(audit_at(url).write_intent(&intent()).await.is_err());
        let took = start.elapsed();
        assert!(
            took < INTENT_BUDGET + Duration::from_millis(250),
            "{took:?}"
        );
        assert_eq!(
            hits.load(Ordering::SeqCst),
            2,
            "two 500ms attempts fill the 1s budget"
        );
    }

    #[tokio::test]
    async fn off_never_calls_out() {
        assert!(Audit::off().write_intent(&intent()).await.is_ok());
        Audit::off().send_outcome(
            "req-1".into(),
            outcome_for(AuditState::default(), false, None),
            opentelemetry::Context::new(),
        );
        Audit::off().send_rejected(intent(), 403, opentelemetry::Context::new());
    }

    #[tokio::test]
    async fn outcome_retries_until_taken_and_stops_on_404() {
        let http = InternalHttpClient::new();
        let out = Background::Outcome(outcome_for(AuditState::default(), false, Some(503)));
        let quick = [Duration::from_millis(1); 4];

        let (url, hits) = fake_audit(vec![503, 503, 204]).await;
        write_background(&http, &format!("{url}/x"), "req-1", &out, &quick).await;
        assert_eq!(hits.load(Ordering::SeqCst), 3);

        let (url, hits) = fake_audit(vec![404]).await;
        write_background(&http, &format!("{url}/x"), "req-1", &out, &quick).await;
        assert_eq!(hits.load(Ordering::SeqCst), 1);

        let (url, hits) = fake_audit(vec![503]).await;
        write_background(&http, &format!("{url}/x"), "req-1", &out, &quick).await;
        assert_eq!(
            hits.load(Ordering::SeqCst),
            5,
            "first try plus one per backoff step"
        );
    }

    #[tokio::test]
    async fn a_rejected_write_retries_until_taken_and_stops_on_a_4xx() {
        let http = InternalHttpClient::new();
        let rejected = Background::Rejected(Box::new(Rejected {
            intent: intent(),
            outcome: OutcomeKind::Rejected,
            status: 403,
        }));
        let quick = [Duration::from_millis(1); 4];

        let (url, hits) = fake_audit(vec![503, 201]).await;
        write_background(&http, &format!("{url}/x"), "req-1", &rejected, &quick).await;
        assert_eq!(hits.load(Ordering::SeqCst), 2);

        let (url, hits) = fake_audit(vec![422]).await;
        write_background(&http, &format!("{url}/x"), "req-1", &rejected, &quick).await;
        assert_eq!(hits.load(Ordering::SeqCst), 1);
    }
}

//! Org audit log writes (docs/audit.md). On an audited route the gateway writes an
//! intent before forward and waits on it, then an outcome after, in the background.
//! Audit is on or off for the whole deployment.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use reqwest::Method;
use serde::Serialize;
use serde_json::value::RawValue;
use tokio::sync::{mpsc, Semaphore};
use tracing::{error, warn};

use crate::error::ErrorKind;
use crate::internal_http::InternalHttpClient;
use crate::metrics;

/// A service says what changed on this response header. The gateway stores it as
/// sent and strips it, from requests and responses, on every route.
pub const DETAILS_HEADER: &str = "X-Plat5-Audit-Details";
pub const MAX_DETAILS_BYTES: usize = 4096;
pub const MAX_USER_AGENT_CHARS: usize = 512;

const INTENT_ATTEMPTS: u32 = 3;
const INTENT_ATTEMPT_TIMEOUT: Duration = Duration::from_millis(500);
/// The client waits on the intent, so every attempt fits in this.
const INTENT_BUDGET: Duration = Duration::from_secs(1);

const OUTCOME_QUEUE: usize = 10_000;
const OUTCOME_IN_FLIGHT: usize = 64;
const OUTCOME_ATTEMPT_TIMEOUT: Duration = Duration::from_secs(2);
/// Wait before each outcome retry. About five minutes in all, then it stays pending.
const OUTCOME_BACKOFF: &[Duration] = &[
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

#[derive(Debug, Serialize)]
pub struct Actor {
    pub member_id: String,
    pub auth_type: &'static str,
    pub key_prefix: String,
}

/// Everything the gateway knows before forward. `request_id` is in the URL.
#[derive(Debug, Serialize)]
pub struct Intent {
    #[serde(skip)]
    pub request_id: String,
    pub occurred_at: String,
    pub organization_id: String,
    pub actor: Actor,
    pub service: String,
    pub method: String,
    pub route: String,
    pub params: HashMap<String, String>,
    pub ip: String,
    pub user_agent: Option<String>,
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
}

/// An audited request between its intent and its outcome.
#[derive(Debug, Default)]
pub struct AuditState {
    /// Set when the upstream response arrives.
    pub upstream_status: Option<u16>,
    pub details: Option<Box<RawValue>>,
}

/// How the request ended. `forwarded` = the gateway handed it to the upstream.
pub fn outcome_for(state: AuditState, forwarded: bool, written_status: Option<u16>) -> Outcome {
    match (state.upstream_status, forwarded) {
        (Some(status), _) => Outcome {
            outcome: OutcomeKind::Responded,
            status: Some(status),
            details: state.details,
        },
        (None, true) => Outcome {
            outcome: OutcomeKind::NoResponse,
            status: written_status,
            details: None,
        },
        (None, false) => Outcome {
            outcome: OutcomeKind::Rejected,
            status: written_status,
            details: None,
        },
    }
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

/// The deployment's audit writer. Off = no writes and no waits.
#[derive(Clone)]
pub struct Audit {
    inner: Option<Arc<Inner>>,
}

struct Inner {
    base_url: String,
    http: InternalHttpClient,
    outcomes: mpsc::Sender<(String, Outcome)>,
}

impl Audit {
    pub fn off() -> Self {
        Self { inner: None }
    }

    /// On when `base_url` is set. Outcome writes run on `runtime`, with their own client.
    pub fn new(
        base_url: Option<String>,
        internal_token: Option<String>,
        runtime: &tokio::runtime::Handle,
    ) -> Self {
        let Some(base_url) = base_url else {
            return Self::off();
        };
        let (tx, rx) = mpsc::channel(OUTCOME_QUEUE);
        runtime.spawn(run_outcomes(
            base_url.clone(),
            InternalHttpClient::new(internal_token.clone()),
            rx,
        ));
        Self {
            inner: Some(Arc::new(Inner {
                base_url,
                http: InternalHttpClient::new(internal_token),
                outcomes: tx,
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
                // A 4xx is the same on every try: a bug or a token mismatch.
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
    pub fn send_outcome(&self, request_id: String, outcome: Outcome) {
        let Some(inner) = &self.inner else {
            return;
        };
        if let Err(err) = inner.outcomes.try_send((request_id, outcome)) {
            let (request_id, _) = err.into_inner();
            warn!(
                request_id = %request_id,
                "audit outcome queue full; event stays pending"
            );
            metrics::record_audit_write("outcome", "dropped");
        }
    }
}

fn event_url(base_url: &str, request_id: &str) -> String {
    format!("{base_url}/internal/events/{request_id}")
}

async fn run_outcomes(
    base_url: String,
    http: InternalHttpClient,
    mut rx: mpsc::Receiver<(String, Outcome)>,
) {
    let permits = Arc::new(Semaphore::new(OUTCOME_IN_FLIGHT));
    while let Some((request_id, outcome)) = rx.recv().await {
        let Ok(permit) = permits.clone().acquire_owned().await else {
            return;
        };
        let http = http.clone();
        let url = event_url(&base_url, &request_id);
        tokio::spawn(async move {
            write_outcome(&http, &url, &request_id, &outcome, OUTCOME_BACKOFF).await;
            drop(permit);
        });
    }
}

/// PATCH the outcome until audit takes it, says there is no intent, or the backoff
/// runs out. Audit applies an outcome once, so a retry after a lost reply is harmless.
async fn write_outcome(
    http: &InternalHttpClient,
    url: &str,
    request_id: &str,
    outcome: &Outcome,
    backoff: &[Duration],
) {
    let mut waits = backoff.iter();
    loop {
        match http
            .send_json(Method::PATCH, url, outcome, OUTCOME_ATTEMPT_TIMEOUT)
            .await
        {
            Ok(200..=299) => {
                metrics::record_audit_write("outcome", "ok");
                return;
            }
            // The intent never landed (the 503 path): nothing to finish.
            Ok(404) => {
                metrics::record_audit_write("outcome", "not_found");
                return;
            }
            Ok(status @ 400..=499) => {
                error!(
                    error_kind = ErrorKind::Internal.as_str(),
                    status, request_id, "audit refused the outcome; event stays pending"
                );
                metrics::record_audit_write("outcome", "failed");
                return;
            }
            _ => {}
        }
        let Some(wait) = waits.next() else {
            error!(
                error_kind = ErrorKind::Network.as_str(),
                request_id, "audit outcome not written; event stays pending"
            );
            metrics::record_audit_write("outcome", "failed");
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
        let responded = outcome_for(
            AuditState {
                upstream_status: Some(200),
                details: parse_details(br#"{"a":1}"#),
            },
            true,
            Some(200),
        );
        assert_eq!(responded.outcome, OutcomeKind::Responded);
        assert_eq!(responded.status, Some(200));
        assert!(responded.details.is_some());

        let no_response = outcome_for(AuditState::default(), true, Some(502));
        assert_eq!(no_response.outcome, OutcomeKind::NoResponse);
        assert_eq!(no_response.status, Some(502));

        let gone = outcome_for(AuditState::default(), true, None);
        assert_eq!(gone.status, None);

        let rejected = outcome_for(AuditState::default(), false, Some(403));
        assert_eq!(rejected.outcome, OutcomeKind::Rejected);
        assert_eq!(rejected.status, Some(403));
    }

    #[test]
    fn wire_shapes() {
        let intent = Intent {
            request_id: "req-1".into(),
            occurred_at: "2026-10-08T18:30:00.123Z".into(),
            organization_id: "org-1".into(),
            actor: Actor {
                member_id: "m-1".into(),
                auth_type: "member_apikey",
                key_prefix: "plat5-mk-1-AbC9".into(),
            },
            service: "identity".into(),
            method: "PATCH".into(),
            route: "/org/members/{member_id}".into(),
            params: HashMap::from([("member_id".into(), "m-2".into())]),
            ip: "203.0.113.7".into(),
            user_agent: None,
        };
        let v = serde_json::to_value(&intent).unwrap();
        assert!(v.get("request_id").is_none(), "request_id is in the URL");
        assert_eq!(v["actor"]["auth_type"], "member_apikey");
        assert!(v["user_agent"].is_null());

        let out = Outcome {
            outcome: OutcomeKind::NoResponse,
            status: None,
            details: None,
        };
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
            organization_id: "org-1".into(),
            actor: Actor {
                member_id: "m-1".into(),
                auth_type: "member_apikey",
                key_prefix: "plat5-mk-1-AbC9".into(),
            },
            service: "s".into(),
            method: "POST".into(),
            route: "/r".into(),
            params: HashMap::new(),
            ip: "127.0.0.1".into(),
            user_agent: None,
        }
    }

    fn audit_at(url: String) -> Audit {
        Audit::new(
            Some(url),
            Some("tok".into()),
            &tokio::runtime::Handle::current(),
        )
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
        );
    }

    #[tokio::test]
    async fn outcome_retries_until_taken_and_stops_on_404() {
        let http = InternalHttpClient::new(None);
        let out = outcome_for(AuditState::default(), false, Some(403));
        let quick = [Duration::from_millis(1); 4];

        let (url, hits) = fake_audit(vec![503, 503, 204]).await;
        write_outcome(&http, &format!("{url}/x"), "req-1", &out, &quick).await;
        assert_eq!(hits.load(Ordering::SeqCst), 3);

        let (url, hits) = fake_audit(vec![404]).await;
        write_outcome(&http, &format!("{url}/x"), "req-1", &out, &quick).await;
        assert_eq!(hits.load(Ordering::SeqCst), 1);

        let (url, hits) = fake_audit(vec![503]).await;
        write_outcome(&http, &format!("{url}/x"), "req-1", &out, &quick).await;
        assert_eq!(
            hits.load(Ordering::SeqCst),
            5,
            "first try plus one per backoff step"
        );
    }
}

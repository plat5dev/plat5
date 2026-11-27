use std::collections::HashMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use jsonwebtoken::{encode, Algorithm, EncodingKey, Header};
use serde_json::{json, Value};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

use super::config::glob_match;
use super::*;

const ED25519_PRIVATE_DER: &[u8] = &[
    0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x04, 0x22, 0x04,
    0x20, 0xbf, 0xb5, 0xcd, 0x6d, 0x82, 0xec, 0x0e, 0x21, 0x15, 0xe7, 0x6b, 0x03, 0x48, 0x76,
    0xd0, 0x90, 0xf4, 0x3e, 0xb5, 0x28, 0x01, 0x8f, 0x36, 0x01, 0xf8, 0xae, 0xf9, 0x5d, 0xac,
    0xc5, 0xa1, 0xef,
];
const ED25519_PUBLIC_X: &str = "bXUj6dVFBJp4oH4PZBzpnTQAuK8oEtKDiftREsbKRFI";

fn now_secs() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs()
}

fn sign_eddsa(kid: &str, claims: &Value) -> String {
    let mut header = Header::new(Algorithm::EdDSA);
    header.kid = Some(kid.to_string());
    let key = EncodingKey::from_ed_der(ED25519_PRIVATE_DER);
    encode(&header, claims, &key).unwrap()
}

fn ed25519_jwks(kids: &[&str]) -> JwkSet {
    let keys: Vec<Value> = kids
        .iter()
        .map(|kid| {
            json!({
                "kty": "OKP",
                "crv": "Ed25519",
                "kid": kid,
                "use": "sig",
                "alg": "EdDSA",
                "x": ED25519_PUBLIC_X
            })
        })
        .collect();
    serde_json::from_value(json!({ "keys": keys })).unwrap()
}

#[test]
fn glob_matching_is_anchored_and_handles_multiple_stars() {
    assert!(glob_match("refs/heads/main", "refs/heads/main"));
    assert!(!glob_match("refs/heads/main", "refs/heads/main-2"));
    assert!(!glob_match("main", "refs/heads/main"));
    assert!(glob_match("refs/tags/v*", "refs/tags/v1.2.3"));
    assert!(glob_match("refs/tags/v*", "refs/tags/v"));
    assert!(!glob_match("refs/tags/v*", "refs/heads/v1"));
    assert!(glob_match(
        "repo:acme/*:ref:refs/heads/*",
        "repo:acme/platform:ref:refs/heads/main"
    ));
    assert!(!glob_match(
        "repo:acme/*:ref:refs/heads/*",
        "repo:other/platform:ref:refs/heads/main"
    ));
}

#[test]
fn auth_config_rejects_wildcards_duplicates_and_empty_claims() {
    // Rejects audience "*"
    let bad_aud = r#"
issuers:
  - name: gh
    issuer: https://token.actions.githubusercontent.com
    audiences: ["*"]
    rules:
      - access: read
        claims: { repository: ["acme/app"] }
"#;
    assert!(AuthConfig::parse_yaml_or_json(bad_aud).is_err());

    // Rejects bare "*" claim pattern
    let bad_glob = r#"
issuers:
  - name: gh
    issuer: https://token.actions.githubusercontent.com
    audiences: [https://cp.example.com]
    rules:
      - access: read
        claims: { repository: ["*"] }
"#;
    assert!(AuthConfig::parse_yaml_or_json(bad_glob).is_err());

    // Rejects empty claims object
    let empty_claims = r#"
issuers:
  - name: gh
    issuer: https://token.actions.githubusercontent.com
    audiences: [https://cp.example.com]
    rules:
      - access: read
        claims: {}
"#;
    assert!(AuthConfig::parse_yaml_or_json(empty_claims).is_err());

    // Rejects duplicate issuer name
    let dup_name = r#"
issuers:
  - name: idp
    issuer: https://a.example.com
    audiences: [cp]
    rules:
      - access: read
        claims: { sub: ["alice"] }
  - name: idp
    issuer: https://b.example.com
    audiences: [cp]
    rules:
      - access: read
        claims: { sub: ["bob"] }
"#;
    assert!(AuthConfig::parse_yaml_or_json(dup_name).is_err());
}

#[tokio::test]
async fn multi_issuer_read_and_apply_authorization() {
    let yaml = r#"
issuers:
  - name: github-actions
    issuer: https://token.actions.githubusercontent.com
    audiences: [https://cp.prod.example.com]
    max_token_lifetime: 1h
    rules:
      - access: read
        claims:
          repository: ["acme/*"]
      - access: apply
        claims:
          repository: [acme/platform]
          ref: ["refs/heads/main", "refs/tags/v*"]
          environment: [production]
  - name: okta
    issuer: https://acme.okta.com/oauth2/default
    audiences: [plat5-cp]
    subject_claim: email
    cli:
      client_id: 0oa12345
      scopes: [openid, email, groups]
      token_source: id_token
    rules:
      - access: read
        claims:
          groups: [platform-engineers]
      - access: apply
        claims:
          groups: [platform-admins]
          amr: [mfa]
"#;
    let cfg = AuthConfig::parse_yaml_or_json(yaml).unwrap();
    let mut jwks = HashMap::new();
    jwks.insert(
        "https://token.actions.githubusercontent.com".to_string(),
        ed25519_jwks(&["gh-1"]),
    );
    jwks.insert(
        "https://acme.okta.com/oauth2/default".to_string(),
        ed25519_jwks(&["okta-1"]),
    );
    let auth = Authenticator::with_static_jwks(None, cfg, jwks);
    assert!(auth.is_ready());
    assert!(!auth.has_admin_token());
    assert!(auth.has_oidc());

    let now = now_secs();

    // PR token: matches read rule (`acme/*`), NOT apply rule.
    let pr_tok = sign_eddsa(
        "gh-1",
        &json!({
            "iss": "https://token.actions.githubusercontent.com",
            "aud": "https://cp.prod.example.com",
            "sub": "repo:acme/platform:pull_request",
            "iat": now,
            "exp": now + 300,
            "repository": "acme/platform",
            "ref": "refs/pull/42/merge"
        }),
    );
    let actor = auth
        .authenticate(Some(&format!("Bearer {pr_tok}")), AccessLevel::Read)
        .await
        .unwrap();
    assert_eq!(actor.actor_type, "jwt");
    assert_eq!(actor.issuer_name.as_deref(), Some("github-actions"));
    assert_eq!(
        actor.subject.as_deref(),
        Some("repo:acme/platform:pull_request")
    );
    assert_eq!(actor.access, AccessLevel::Read);

    let err = auth
        .authenticate(Some(&format!("Bearer {pr_tok}")), AccessLevel::Apply)
        .await
        .unwrap_err();
    assert_eq!(
        err,
        AuthFailure::Forbidden {
            reason: "insufficient_access",
            required: AccessLevel::Apply
        }
    );

    // Main/tag production token: matches apply rule.
    let tag_tok = sign_eddsa(
        "gh-1",
        &json!({
            "iss": "https://token.actions.githubusercontent.com",
            "aud": "https://cp.prod.example.com",
            "sub": "repo:acme/platform:environment:production",
            "iat": now,
            "exp": now + 300,
            "repository": "acme/platform",
            "ref": "refs/tags/v1.4.0",
            "environment": "production"
        }),
    );
    let actor = auth
        .authenticate(Some(&format!("Bearer {tag_tok}")), AccessLevel::Apply)
        .await
        .unwrap();
    assert_eq!(actor.access, AccessLevel::Apply);

    // Exceeds max_token_lifetime (1h) -> 401.
    let long_tok = sign_eddsa(
        "gh-1",
        &json!({
            "iss": "https://token.actions.githubusercontent.com",
            "aud": "https://cp.prod.example.com",
            "sub": "repo:acme/platform:environment:production",
            "iat": now,
            "exp": now + 7200,
            "repository": "acme/platform",
            "ref": "refs/heads/main",
            "environment": "production"
        }),
    );
    assert_eq!(
        auth.authenticate(Some(&format!("Bearer {long_tok}")), AccessLevel::Read)
            .await
            .unwrap_err(),
        AuthFailure::Unauthorized {
            reason: "token_lifetime_exceeded"
        }
    );

    // Okta admin without MFA -> 403 (doesn't match read or apply).
    let no_mfa_tok = sign_eddsa(
        "okta-1",
        &json!({
            "iss": "https://acme.okta.com/oauth2/default",
            "aud": "plat5-cp",
            "email": "alice@acme.com",
            "exp": now + 300,
            "groups": ["platform-admins"],
            "amr": ["pwd"]
        }),
    );
    assert_eq!(
        auth.authenticate(Some(&format!("Bearer {no_mfa_tok}")), AccessLevel::Apply)
            .await
            .unwrap_err(),
        AuthFailure::Forbidden {
            reason: "claims_not_matched",
            required: AccessLevel::Apply
        }
    );

    // Okta admin with MFA -> allowed, subject is email.
    let mfa_tok = sign_eddsa(
        "okta-1",
        &json!({
            "iss": "https://acme.okta.com/oauth2/default",
            "aud": "plat5-cp",
            "email": "alice@acme.com",
            "exp": now + 300,
            "groups": ["platform-admins"],
            "amr": ["pwd", "mfa"]
        }),
    );
    let actor = auth
        .authenticate(Some(&format!("Bearer {mfa_tok}")), AccessLevel::Apply)
        .await
        .unwrap();
    assert_eq!(actor.issuer_name.as_deref(), Some("okta"));
    assert_eq!(actor.subject.as_deref(), Some("alice@acme.com"));

    // Public auth_info exposes cli metadata but never rules.
    let info = auth.auth_info();
    assert_eq!(info["methods"], json!(["oidc"]));
    assert_eq!(info["issuers"][1]["cli"]["client_id"], json!("0oa12345"));
    assert!(info["issuers"][1].get("rules").is_none());
}

#[tokio::test]
async fn oidc_discovery_and_unknown_kid_rotation() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let issuer = format!("http://{addr}");
    let jwks_body = Arc::new(std::sync::Mutex::new(
        serde_json::to_string(&ed25519_jwks(&["k1"])).unwrap(),
    ));
    let jwks_hits = Arc::new(AtomicUsize::new(0));

    let jwks_body_srv = jwks_body.clone();
    let jwks_hits_srv = jwks_hits.clone();
    let issuer_srv = issuer.clone();
    tokio::spawn(async move {
        loop {
            let Ok((mut sock, _)) = listener.accept().await else {
                return;
            };
            let body_store = jwks_body_srv.clone();
            let hits = jwks_hits_srv.clone();
            let iss = issuer_srv.clone();
            tokio::spawn(async move {
                let mut buf = [0u8; 2048];
                let n = sock.read(&mut buf).await.unwrap_or(0);
                let req = String::from_utf8_lossy(&buf[..n]);
                let payload = if req.starts_with("GET /.well-known/openid-configuration") {
                    json!({
                        "issuer": iss,
                        "jwks_uri": format!("{iss}/keys")
                    })
                    .to_string()
                } else {
                    hits.fetch_add(1, Ordering::SeqCst);
                    body_store.lock().unwrap().clone()
                };
                let resp = format!(
                    "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{}",
                    payload.len(),
                    payload
                );
                let _ = sock.write_all(resp.as_bytes()).await;
            });
        }
    });

    let jwks = jwks::IssuerJwks::with_refresh_cooldown(
        issuer.clone(),
        None,
        Duration::from_millis(50),
    );
    let initial = jwks.get_jwks().await.unwrap();
    assert!(initial.find("k1").is_some());
    assert_eq!(jwks_hits.load(Ordering::SeqCst), 1);

    // Rotate key to k2.
    *jwks_body.lock().unwrap() = serde_json::to_string(&ed25519_jwks(&["k1", "k2"])).unwrap();
    let refreshed = jwks.refresh_for_unknown_kid("k2").await.unwrap();
    assert!(refreshed.find("k2").is_some());
    assert_eq!(jwks_hits.load(Ordering::SeqCst), 2);
}

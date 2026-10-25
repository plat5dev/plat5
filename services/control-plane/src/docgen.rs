//! The config reference pages' field tables, generated from `schema/` so they
//! can't drift from it. A table sits between
//!
//! ```text
//! <!-- generated from schema: kinds.json#/$defs/kind/properties … -->
//! <!-- end generated -->
//! ```
//!
//! with one row per pointer (a pointer ending in `/properties` is a row per
//! property). `generated with types` adds a Type column. The test fails when a
//! table is stale, or when a link in it doesn't resolve from the page;
//! `UPDATE_DOCS=1 cargo test docs` rewrites them.

use std::path::{Path, PathBuf};

use serde_json::Value;

use crate::schema;

const OPEN: &str = "<!-- generated from schema";
const CLOSE: &str = "<!-- end generated -->";
const PAGES: &[&str] = &[
    "settings.md",
    "kinds.md",
    "roles.md",
    "rate-limits.md",
    "cors.md",
    "services.md",
];

fn docs_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../docs/reference/config")
}

/// The schema at `file#/json/pointer`.
fn resolve(target: &str) -> (String, Value) {
    let (file, pointer) = target.split_once('#').unwrap_or((target, ""));
    let doc = schema::file(file);
    let v = doc
        .pointer(pointer)
        .unwrap_or_else(|| panic!("schema/{target} doesn't exist"))
        .clone();
    (file.to_string(), v)
}

/// A `$ref` relative to `file`, resolved.
fn follow(file: &str, v: &Value) -> Option<(String, Value)> {
    let r = v.get("$ref")?.as_str()?;
    let target = if r.starts_with('#') {
        format!("{file}{r}")
    } else {
        r.to_string()
    };
    Some(resolve(&target))
}

fn description(file: &str, v: &Value) -> String {
    if let Some(d) = v.get("description").and_then(Value::as_str) {
        return d.to_string();
    }
    follow(file, v).map_or_else(String::new, |(f, t)| description(&f, &t))
}

fn type_of(file: &str, v: &Value) -> String {
    if let Some((f, t)) = follow(file, v) {
        let is_object = t.get("type").and_then(Value::as_str) == Some("object");
        return match (is_object, t.get("title").and_then(Value::as_str)) {
            (true, Some(title)) => title.to_string(),
            _ => type_of(&f, &t),
        };
    }
    if let Some(c) = v.get("const") {
        return format!("`{c}`");
    }
    for key in ["oneOf", "anyOf"] {
        if let Some(options) = v.get(key).and_then(Value::as_array) {
            return options
                .iter()
                .map(|o| type_of(file, o))
                .collect::<Vec<_>>()
                .join(" \\| ");
        }
    }
    match v.get("type").and_then(Value::as_str) {
        Some("array") => format!(
            "array<{}>",
            v.get("items").map_or("any".into(), |i| type_of(file, i))
        ),
        Some("object") => {
            if let Some(ap) = v.get("additionalProperties").filter(|a| a.is_object()) {
                format!("map<{}, {}>", key_type(file, v), type_of(file, ap))
            } else if let Some(props) = v.get("properties").and_then(Value::as_object) {
                format!(
                    "{{{}}}",
                    props.keys().cloned().collect::<Vec<_>>().join(", ")
                )
            } else {
                "object".into()
            }
        }
        Some("boolean") => "bool".into(),
        Some(t) => t.into(),
        None => "any".into(),
    }
}

/// A map's key: the title of its `propertyNames`, or `string`.
fn key_type(file: &str, map: &Value) -> String {
    map.get("propertyNames")
        .and_then(|names| follow(file, names))
        .and_then(|(_, names)| names.get("title")?.as_str().map(String::from))
        .unwrap_or_else(|| "string".into())
}

/// One table's rows: (field, type, description).
fn rows(pointers: &[&str]) -> Vec<(String, String, String)> {
    let mut out = Vec::new();
    for target in pointers {
        let (file, v) = resolve(target);
        if target.ends_with("/properties") {
            let parent = resolve(target.trim_end_matches("/properties")).1;
            let required: Vec<&str> = parent
                .get("required")
                .and_then(Value::as_array)
                .map(|r| r.iter().filter_map(Value::as_str).collect())
                .unwrap_or_default();
            for (name, prop) in v.as_object().expect("properties is an object") {
                let mut ty = type_of(&file, prop);
                if !required.contains(&name.as_str()) {
                    ty = format!("{ty}?");
                }
                out.push((format!("`{name}`"), ty, description(&file, prop)));
            }
        } else {
            let (_, shown) = follow(&file, &v)
                .filter(|_| v.get("title").is_none())
                .unwrap_or((file.clone(), v.clone()));
            let title = shown
                .get("title")
                .and_then(Value::as_str)
                .unwrap_or_else(|| panic!("{target} needs a title"));
            let field = if target.contains("/properties/") {
                format!("`{title}`")
            } else {
                title.to_string()
            };
            out.push((field, type_of(&file, &v), description(&file, &v)));
        }
    }
    out
}

fn table(spec: &str) -> String {
    let (with_types, pointers) = match spec.strip_prefix(" with types:") {
        Some(rest) => (true, rest),
        None => (
            false,
            spec.strip_prefix(':')
                .unwrap_or_else(|| panic!("bad marker: {spec}")),
        ),
    };
    let pointers: Vec<&str> = pointers.split_whitespace().collect();
    let mut out = String::new();
    if with_types {
        out.push_str("| Field | Type | Rule |\n|-------|------|------|\n");
    } else {
        out.push_str("| Field | Rule |\n|-------|------|\n");
    }
    for (field, ty, desc) in rows(&pointers) {
        let desc = desc.trim_end_matches('.').replace('|', "\\|");
        if with_types {
            out.push_str(&format!(
                "| {field} | `{}` | {desc} |\n",
                ty.replace('`', "")
            ));
        } else {
            out.push_str(&format!("| {field} | {desc} |\n"));
        }
    }
    out
}

/// The page with every generated table rewritten.
fn regenerate(page: &str) -> String {
    let mut out = String::new();
    let mut rest = page;
    while let Some(start) = rest.find(OPEN) {
        let line_end = start + rest[start..].find("-->").expect("marker closes") + 3;
        let spec = rest[start + OPEN.len()..line_end - 3].trim_end();
        let close = rest[line_end..].find(CLOSE).expect("generated table ends") + line_end;
        out.push_str(&rest[..line_end]);
        out.push('\n');
        out.push_str(&table(spec));
        rest = &rest[close..];
    }
    out.push_str(rest);
    out
}

/// The targets of the Markdown links in `text`.
fn links(text: &str) -> Vec<&str> {
    text.match_indices("](")
        .filter_map(|(at, _)| {
            let rest = &text[at + 2..];
            rest.find(')').map(|end| &rest[..end])
        })
        .collect()
}

/// A heading's anchor, as GitHub makes it.
fn anchor(heading: &str) -> String {
    heading
        .trim()
        .to_lowercase()
        .chars()
        .filter(|c| c.is_alphanumeric() || matches!(c, ' ' | '-' | '_'))
        .map(|c| if c == ' ' { '-' } else { c })
        .collect()
}

/// Links in a page's generated tables that don't resolve: a missing page, or
/// an anchor no heading there makes.
fn broken_links(page_path: &Path, page: &str) -> Vec<String> {
    let mut broken = Vec::new();
    let mut rest = page;
    while let Some(start) = rest.find(OPEN) {
        let close = rest[start..].find(CLOSE).expect("generated table ends") + start;
        for target in links(&rest[start..close]) {
            let (file, fragment) = target.split_once('#').unwrap_or((target, ""));
            let file = if file.is_empty() {
                page_path.to_path_buf()
            } else {
                page_path.parent().unwrap().join(file)
            };
            let Ok(text) = std::fs::read_to_string(&file) else {
                broken.push(target.to_string());
                continue;
            };
            let found = fragment.is_empty()
                || text
                    .lines()
                    .filter_map(|l| l.strip_prefix('#'))
                    .any(|h| anchor(h.trim_start_matches('#')) == fragment);
            if !found {
                broken.push(target.to_string());
            }
        }
        rest = &rest[close..];
    }
    broken
}

#[test]
fn docs_reference_tables_match_the_schema() {
    let update = std::env::var("UPDATE_DOCS").is_ok_and(|v| v == "1");
    let mut stale = Vec::new();
    for name in PAGES {
        let path = docs_dir().join(name);
        let page = std::fs::read_to_string(&path).unwrap();
        assert!(page.contains(OPEN), "{name} has no generated table");
        let fresh = regenerate(&page);
        let broken = broken_links(&path, &fresh);
        assert!(
            broken.is_empty(),
            "{name}: broken links from schema/: {broken:?}"
        );
        if fresh != page {
            if update {
                std::fs::write(&path, fresh).unwrap();
            } else {
                stale.push(*name);
            }
        }
    }
    assert!(
        stale.is_empty(),
        "docs/reference/config/{stale:?} don't match schema; run UPDATE_DOCS=1 cargo test docs"
    );
}

#[test]
fn broken_links_are_found() {
    let page = format!(
        "{OPEN}: x -->\n| [a](#nope) [b](settings.md) [c](../../concepts/access.md#lists) [d](#upstream) [e](gone.md) |\n{CLOSE}\n"
    );
    assert_eq!(
        broken_links(&docs_dir().join("services.md"), &page),
        ["#nope", "gone.md"]
    );
}

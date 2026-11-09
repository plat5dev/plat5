//! The duration format the config documents and the env share
//! (docs/reference/config/settings.md): a whole number and `s`, `m`, `h` or `d`.

use std::time::Duration;

/// A duration as the config writes one: a whole number and `s`, `m`, `h` or `d`.
pub fn parse_duration(raw: &str) -> Option<Duration> {
    let raw = raw.trim();
    let split = raw.find(|c: char| !c.is_ascii_digit())?;
    let (number, unit) = raw.split_at(split);
    let n: u64 = number.parse().ok()?;
    let secs = |mult: u64| n.checked_mul(mult).map(Duration::from_secs);
    match unit {
        "s" => secs(1),
        "m" => secs(60),
        "h" => secs(3600),
        "d" => secs(86_400),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn durations_are_a_number_and_a_unit() {
        assert_eq!(parse_duration("10s"), Some(Duration::from_secs(10)));
        assert_eq!(parse_duration("5m"), Some(Duration::from_secs(300)));
        assert_eq!(parse_duration("2h"), Some(Duration::from_secs(7200)));
        assert_eq!(parse_duration("30d"), Some(Duration::from_secs(2_592_000)));
        for bad in ["", "10", "s", "1.5s", "10 s", "-1s", "10w", "500ms"] {
            assert_eq!(parse_duration(bad), None, "{bad}");
        }
    }
}

//! RFC 3339 timestamps from identity (`good_until`), without a date crate.

use std::time::{Duration, SystemTime, UNIX_EPOCH};

/// `2026-11-01T00:00:00Z`, with optional fractional seconds and a `Z` or
/// `±hh:mm` offset. Fractions are dropped: an earlier instant only shortens a cache entry.
pub fn parse_rfc3339(s: &str) -> Option<SystemTime> {
    let b = s.as_bytes();
    if b.len() < 20 || b[4] != b'-' || b[7] != b'-' || !matches!(b[10], b'T' | b't') {
        return None;
    }
    if b[13] != b':' || b[16] != b':' {
        return None;
    }
    let num = |from: usize, to: usize| -> Option<i64> {
        let part = s.get(from..to)?;
        part.bytes()
            .all(|c| c.is_ascii_digit())
            .then(|| part.parse().ok())?
    };
    let (year, month, day) = (num(0, 4)?, num(5, 7)?, num(8, 10)?);
    let (hour, minute, second) = (num(11, 13)?, num(14, 16)?, num(17, 19)?);
    if !(1..=12).contains(&month)
        || !(1..=31).contains(&day)
        || hour > 23
        || minute > 59
        || second > 60
    {
        return None;
    }
    let mut rest = &s[19..];
    if let Some(frac) = rest.strip_prefix('.') {
        let digits = frac.bytes().take_while(u8::is_ascii_digit).count();
        if digits == 0 {
            return None;
        }
        rest = &frac[digits..];
    }
    let offset = match rest {
        "Z" | "z" => 0,
        _ => {
            let rb = rest.as_bytes();
            if rb.len() != 6 || rb[3] != b':' {
                return None;
            }
            let sign = match rb[0] {
                b'+' => 1,
                b'-' => -1,
                _ => return None,
            };
            let h: i64 = rest.get(1..3)?.parse().ok()?;
            let m: i64 = rest.get(4..6)?.parse().ok()?;
            sign * (h * 3600 + m * 60)
        }
    };
    let secs =
        days_from_civil(year, month, day) * 86_400 + hour * 3600 + minute * 60 + second - offset;
    let secs = u64::try_from(secs).ok()?;
    Some(UNIX_EPOCH + Duration::from_secs(secs))
}

/// Days since 1970-01-01 for a proleptic Gregorian date. Howard Hinnant's algorithm.
fn days_from_civil(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = y.div_euclid(400);
    let yoe = y - era * 400;
    let mp = (m + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + doe - 719_468
}

#[cfg(test)]
mod tests {
    use super::*;

    fn at(secs: u64) -> SystemTime {
        UNIX_EPOCH + Duration::from_secs(secs)
    }

    #[test]
    fn parses_utc_offsets_and_fractions() {
        assert_eq!(parse_rfc3339("1970-01-01T00:00:00Z"), Some(at(0)));
        assert_eq!(
            parse_rfc3339("2026-10-08T18:30:00.123Z"),
            Some(at(1_791_484_200))
        );
        assert_eq!(
            parse_rfc3339("2024-02-29T00:00:00Z"),
            Some(at(1_709_164_800))
        );
        assert_eq!(
            parse_rfc3339("2026-10-08T20:30:00+02:00"),
            Some(at(1_791_484_200))
        );
        assert_eq!(
            parse_rfc3339("2026-10-08T13:30:00-05:00"),
            Some(at(1_791_484_200))
        );
    }

    #[test]
    fn refuses_anything_else() {
        for bad in [
            "",
            "2026-10-08",
            "2026-10-08 18:30:00Z",
            "2026-13-08T18:30:00Z",
            "2026-10-08T18:30:00",
            "2026-10-08T18:30:00.Z",
            "2026-10-08T18:30:00+0200",
            "1969-12-31T23:59:59Z",
        ] {
            assert_eq!(parse_rfc3339(bad), None, "{bad}");
        }
    }
}

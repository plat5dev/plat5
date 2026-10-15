//! The client IP: what an audit event records, and what `public` routes and the
//! failed-auth limiter key on.
//!
//! The TCP peer, unless the peer is a trusted proxy (`TRUSTED_PROXIES`). Then
//! `X-Forwarded-For` is walked from the right, past trusted proxies, to the first
//! address that is not one. A client can put anything at the left of the header,
//! so the left is never read past an untrusted hop.

use std::net::IpAddr;

/// An address or a CIDR block.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Cidr {
    addr: IpAddr,
    prefix: u8,
}

impl Cidr {
    fn parse(raw: &str) -> Result<Self, String> {
        let (addr, prefix) = match raw.split_once('/') {
            Some((addr, prefix)) => (addr, Some(prefix)),
            None => (raw, None),
        };
        let addr: IpAddr = addr
            .parse()
            .map_err(|_| format!("{raw:?} is not an IP address or CIDR"))?;
        let addr = addr.to_canonical();
        let max = if addr.is_ipv4() { 32 } else { 128 };
        let prefix = match prefix {
            Some(p) => p
                .parse::<u8>()
                .ok()
                .filter(|p| *p <= max)
                .ok_or_else(|| format!("{raw:?} has an invalid prefix length"))?,
            None => max,
        };
        Ok(Self { addr, prefix })
    }

    fn contains(&self, ip: IpAddr) -> bool {
        match (self.addr, ip.to_canonical()) {
            (IpAddr::V4(net), IpAddr::V4(ip)) => {
                let mask = u32::MAX
                    .checked_shl(32 - u32::from(self.prefix))
                    .unwrap_or(0);
                u32::from(net) & mask == u32::from(ip) & mask
            }
            (IpAddr::V6(net), IpAddr::V6(ip)) => {
                let mask = u128::MAX
                    .checked_shl(128 - u32::from(self.prefix))
                    .unwrap_or(0);
                u128::from(net) & mask == u128::from(ip) & mask
            }
            _ => false,
        }
    }
}

/// The proxies whose `X-Forwarded-For` the gateway believes. Empty = none: the
/// client IP is the TCP peer.
#[derive(Clone, Debug, Default)]
pub struct TrustedProxies(Vec<Cidr>);

impl TrustedProxies {
    /// Comma-separated addresses and CIDR blocks.
    pub fn parse(raw: &str) -> Result<Self, String> {
        raw.split(',')
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .map(Cidr::parse)
            .collect::<Result<Vec<_>, _>>()
            .map(Self)
    }

    fn trusts(&self, ip: IpAddr) -> bool {
        self.0.iter().any(|c| c.contains(ip))
    }

    /// The client IP for a request from `peer` carrying these `X-Forwarded-For`
    /// values, in header order.
    pub fn client_ip<'a>(
        &self,
        peer: IpAddr,
        forwarded_for: impl IntoIterator<Item = &'a str>,
    ) -> IpAddr {
        let peer = peer.to_canonical();
        if !self.trusts(peer) {
            return peer;
        }
        let hops: Vec<&str> = forwarded_for
            .into_iter()
            .flat_map(|v| v.split(','))
            .map(str::trim)
            .collect();
        let mut client = peer;
        for hop in hops.iter().rev() {
            // A hop a trusted proxy wrote is an address. Anything else came from
            // further left, so stop at the last proxy we believed.
            let Ok(ip) = hop.parse::<IpAddr>() else {
                return client;
            };
            client = ip.to_canonical();
            if !self.trusts(client) {
                return client;
            }
        }
        client
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ip(s: &str) -> IpAddr {
        s.parse().unwrap()
    }

    fn trusted(raw: &str) -> TrustedProxies {
        TrustedProxies::parse(raw).unwrap()
    }

    #[test]
    fn untrusted_peer_ignores_the_header() {
        let t = TrustedProxies::default();
        assert_eq!(
            t.client_ip(ip("203.0.113.7"), ["6.6.6.6"]),
            ip("203.0.113.7")
        );
        let t = trusted("10.0.0.0/8");
        assert_eq!(
            t.client_ip(ip("203.0.113.7"), ["6.6.6.6"]),
            ip("203.0.113.7")
        );
    }

    #[test]
    fn trusted_peer_takes_the_rightmost_untrusted_hop() {
        let t = trusted("10.0.0.0/8");
        // The client sent 6.6.6.6; the proxy appended the address it saw.
        assert_eq!(
            t.client_ip(ip("10.0.0.2"), ["6.6.6.6, 198.51.100.4"]),
            ip("198.51.100.4")
        );
    }

    #[test]
    fn walks_past_a_chain_of_trusted_proxies() {
        let t = trusted("10.0.0.0/8, 172.16.0.5");
        assert_eq!(
            t.client_ip(
                ip("10.0.0.2"),
                ["6.6.6.6, 198.51.100.4, 172.16.0.5", "10.1.2.3"]
            ),
            ip("198.51.100.4")
        );
    }

    #[test]
    fn trusted_peer_without_a_header_is_the_client() {
        let t = trusted("10.0.0.0/8");
        assert_eq!(t.client_ip(ip("10.0.0.2"), []), ip("10.0.0.2"));
    }

    #[test]
    fn every_hop_trusted_is_the_leftmost() {
        let t = trusted("10.0.0.0/8");
        assert_eq!(
            t.client_ip(ip("10.0.0.2"), ["10.0.0.9, 10.0.0.3"]),
            ip("10.0.0.9")
        );
    }

    #[test]
    fn garbage_stops_at_the_last_trusted_proxy() {
        let t = trusted("10.0.0.0/8");
        assert_eq!(
            t.client_ip(ip("10.0.0.2"), ["unknown, 10.0.0.3"]),
            ip("10.0.0.3")
        );
        assert_eq!(t.client_ip(ip("10.0.0.2"), [""]), ip("10.0.0.2"));
    }

    #[test]
    fn v4_mapped_v6_peer_matches_v4_cidr() {
        let t = trusted("10.0.0.0/8");
        assert_eq!(
            t.client_ip(ip("::ffff:10.0.0.2"), ["198.51.100.4"]),
            ip("198.51.100.4")
        );
        assert_eq!(
            t.client_ip(ip("::ffff:203.0.113.7"), ["6.6.6.6"]),
            ip("203.0.113.7")
        );
    }

    #[test]
    fn v6_cidr() {
        let t = trusted("fd00::/8");
        assert_eq!(
            t.client_ip(ip("fd12::1"), ["2001:db8::9"]),
            ip("2001:db8::9")
        );
        assert_eq!(
            t.client_ip(ip("2001:db8::1"), ["6.6.6.6"]),
            ip("2001:db8::1")
        );
    }

    #[test]
    fn zero_prefix_trusts_everything() {
        let t = trusted("0.0.0.0/0");
        assert_eq!(
            t.client_ip(ip("203.0.113.7"), ["198.51.100.4"]),
            ip("198.51.100.4")
        );
    }

    #[test]
    fn parse_rejects_bad_entries() {
        assert!(TrustedProxies::parse("10.0.0.0/33").is_err());
        assert!(TrustedProxies::parse("not-an-ip").is_err());
        assert!(TrustedProxies::parse("10.0.0.0/x").is_err());
        assert!(TrustedProxies::parse("").unwrap().0.is_empty());
        assert_eq!(trusted(" 10.0.0.1 , ").0.len(), 1);
    }
}

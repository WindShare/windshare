use serde::Deserialize;
use serde_json::Value;
use unicode_normalization::is_nfc_stream_safe;

const MAX_KEY_ID_BYTES: usize = 32;
const MAX_URLS: usize = 8;
const MAX_URL_BYTES: usize = 512;
const MAX_DNS_NAME_BYTES: usize = 253;
const MAX_DNS_LABEL_BYTES: usize = 63;
const MAX_CREDENTIAL_BYTES: usize = 1024;

#[derive(Deserialize)]
pub struct Key {
    pub key_id: String,
    pub api_token: String,
}

impl Key {
    pub fn valid(&self) -> bool {
        (1..=MAX_KEY_ID_BYTES).contains(&self.key_id.len())
            && self
                .key_id
                .bytes()
                .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'-' | b'_'))
            && !self.api_token.trim().is_empty()
    }
}

pub struct Credentials<'a> {
    pub urls: Vec<&'a str>,
    pub username: &'a str,
    pub password: &'a str,
}

fn one_or_many(value: &Value) -> &[Value] {
    match value {
        Value::Array(many) => many,
        one => std::slice::from_ref(one),
    }
}

fn credential(value: &Value) -> Option<&str> {
    // Go's grant codec requires stream-safe NFC. Changing the provider's bytes would break auth.
    value
        .as_str()
        .filter(|v| (1..=MAX_CREDENTIAL_BYTES).contains(&v.len()) && is_nfc_stream_safe(v))
}

pub fn select(servers: &Value) -> Option<Credentials<'_>> {
    let mut selected: Option<Credentials<'_>> = None;
    for server in one_or_many(servers) {
        let (Some(username), Some(password)) = (
            credential(&server["username"]),
            credential(&server["credential"]),
        ) else {
            continue;
        };
        // A single ICE server configuration can carry only one credential pair.
        if selected
            .as_ref()
            .is_some_and(|c| c.username != username || c.password != password)
        {
            continue;
        }
        for url in one_or_many(&server["urls"])
            .iter()
            .filter_map(Value::as_str)
            .filter(|u| valid_url(u))
        {
            let c = selected.get_or_insert_with(|| Credentials {
                urls: Vec::new(),
                username,
                password,
            });
            if !c.urls.contains(&url) {
                c.urls.push(url);
                if c.urls.len() == MAX_URLS {
                    return selected;
                }
            }
        }
    }
    selected
}

fn valid_url(raw: &str) -> bool {
    let Some((scheme, tail)) = raw.split_once(':') else {
        return false;
    };
    if !matches!(scheme, "turn" | "turns") || raw.len() > MAX_URL_BYTES {
        return false;
    }
    let authority = match tail.split_once('?') {
        None => tail,
        Some((host, "transport=udp" | "transport=tcp")) => host,
        Some(_) => return false,
    };
    if authority.is_empty()
        || authority.ends_with(':')
        || !authority
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || b".-:[]".contains(&c))
    {
        return false;
    }
    let Ok(parsed) = url::Url::parse(&format!("https://{authority}")) else {
        return false;
    };
    if parsed.port() == Some(0) {
        return false;
    }
    // The wire accepts dotted-quad IPv4 and DNS labels. A WHATWG URL alone can rewrite a numeric
    // shorthand or accept an empty DNS label, displacing a later usable Cloudflare credential.
    let raw_host = authority.split(':').next().unwrap_or_default();
    match parsed.host() {
        Some(url::Host::Ipv6(_)) => true,
        Some(url::Host::Ipv4(ip)) => raw_host.parse::<std::net::Ipv4Addr>() == Ok(ip),
        Some(url::Host::Domain(domain)) => {
            raw_host.eq_ignore_ascii_case(domain) && valid_dns_name(domain)
        }
        None => false,
    }
}

fn valid_dns_name(domain: &str) -> bool {
    let name = domain.strip_suffix('.').unwrap_or(domain);
    name.len() <= MAX_DNS_NAME_BYTES
        && name.split('.').all(|label| {
            (1..=MAX_DNS_LABEL_BYTES).contains(&label.len())
                && !label.starts_with('-')
                && !label.ends_with('-')
                && (label.get(2..4) != Some("--") || label.starts_with("xn--"))
        })
}

#[cfg(test)]
#[path = "turn_credentials_tests.rs"]
mod tests;

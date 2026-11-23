use super::{AccessObject, Database, reply, reply_error};
use serde_json::{Value, json};
use std::collections::HashMap;
use windshare_relay_access::Digest;
use windshare_relay_console::http::{ConsoleError, required_id};
use worker::{Method, Response, Result};

const SETTING: &str = "setting:managed_turn";
const WINDOW_MS: u64 = 60_000;
const MAX_ISSUES: u8 = 20;
const MAX_WINDOWS: usize = 1024;
#[derive(Default)]
pub(super) struct Limits(HashMap<[u8; 32], (u64, u8)>);

impl Limits {
    fn admit(&mut self, key: [u8; 32], now: u64) -> bool {
        self.0
            .retain(|_, (start, _)| now.saturating_sub(*start) < WINDOW_MS);
        if self.0.len() >= MAX_WINDOWS && !self.0.contains_key(&key) {
            return false;
        }
        let (_, used) = self.0.entry(key).or_insert((now, 0));
        if *used >= MAX_ISSUES {
            return false;
        }
        *used += 1;
        true
    }
}
impl AccessObject {
    pub(super) fn turn_setting(db: &Database, method: &Method, body: &Value) -> Result<Response> {
        if *method == Method::Post {
            let Some(enabled) = body["enabled"].as_bool() else {
                return reply_error(ConsoleError::bad_request("invalid_request"));
            };
            db.put(SETTING, &enabled, None)?;
        }
        reply(
            &json!({"enabled":db.get::<bool>(SETTING)?.unwrap_or(true)}),
            200,
        )
    }

    pub(super) fn authorize_turn(&self, db: &Database, body: &Value, now: u64) -> Result<Response> {
        let Ok(key) = required_id::<32>(body, "digest") else {
            return reply_error(ConsoleError::bad_request("invalid_request"));
        };
        if !db.initialized()? {
            return reply_error(ConsoleError::forbidden("access_denied"));
        }
        let digest = Digest(key);
        if db
            .credential(digest)?
            .is_none_or(|mut c| c.authorize(digest, now).is_err())
        {
            return reply_error(ConsoleError::forbidden("access_denied"));
        }
        // Only an active credential may learn that the owner paused issuance.
        if !db.get::<bool>(SETTING)?.unwrap_or(true) {
            return reply_error(ConsoleError::forbidden("turn_disabled"));
        }
        if !self.turn_limits.borrow_mut().admit(key, now) {
            let mut response =
                reply_error(ConsoleError::too_many_requests("slow_down", WINDOW_MS))?;
            response.headers_mut().set("Retry-After", "60")?;
            return Ok(response);
        }
        reply(&json!({"ok":true}), 200)
    }
}

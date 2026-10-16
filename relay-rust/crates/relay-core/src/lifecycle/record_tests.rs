extern crate std;

use core::{num::NonZeroU64, ops::Range};
use std::{string::String, vec::Vec};

use serde::Deserialize;

use super::*;
use crate::{
    registration::RegisterInit,
    stop::StopInit,
    test_vectors::{bytes, relay_identity, session_case},
};

// Control frames open with an eight-byte header before their body.
const FRAME_HEADER_BYTES: usize = 8;
// Share ID, instance and key hash open the WS2X body.
const STOP_ROUTE_BYTES: usize = 12 + 16 + 16;
// Distinct bytes, so the layout test pins their order.
const GRACE_DEADLINE: u64 = 0x0102_0304_0506_0708;

#[derive(Deserialize)]
struct Vector {
    #[serde(rename = "relayIdentityB64")]
    relay_identity: String,
    #[serde(rename = "registerInitB64")]
    register_init: String,
    #[serde(rename = "stopInitB64")]
    stop_init: String,
}

struct Fixture {
    relay: RelayIdentity,
    register_init: Vec<u8>,
    stop_init: Vec<u8>,
}

impl Fixture {
    fn new() -> Self {
        let vector: Vector = session_case("fresh-relay-registration-proof");
        Self {
            relay: relay_identity(&vector.relay_identity),
            register_init: bytes(&vector.register_init),
            stop_init: bytes(&vector.stop_init),
        }
    }

    fn registration(&self) -> Registration {
        *RegisterInit::decode(&self.register_init)
            .unwrap()
            .registration()
    }

    fn operation(&self) -> RegistrationId {
        RegistrationId {
            relay: self.relay,
            share: self.registration().route().share_id(),
            incarnation: Incarnation::new([7; 16]).unwrap(),
            connection: ConnectionRef {
                id: NonZeroU64::new(0x0102).unwrap(),
                generation: NonZeroU64::new(3).unwrap(),
            },
            sequence: NonZeroU64::MAX,
        }
    }

    fn unresolved(&self) -> Record {
        Record::Unresolved(UnresolvedPublication {
            id: self.operation(),
            registration: self.registration(),
        })
    }

    fn route(&self) -> PublishedRoute {
        PublishedRoute {
            relay: self.relay,
            registration: self.registration(),
        }
    }

    fn published(&self) -> Record {
        Record::Published(OwnedRoute {
            route: self.route(),
            owner: self.operation().connection,
        })
    }

    fn grace(&self) -> Record {
        Record::Grace(GraceRoute {
            route: self.route(),
            deadline: UnixMillis(GRACE_DEADLINE),
        })
    }

    fn stopped(&self) -> Record {
        let stop = StopInit::decode(&self.stop_init).unwrap();
        Record::Stopped(StoppedShare {
            relay: stop.relay(),
            route: stop.route(),
            share_instance: stop.share_instance(),
            stop_id: stop.stop_id(),
        })
    }

    /// Format, kind and relay, then the shared frame's body from its share ID.
    fn prefix(&self, kind: u8, frame: &[u8], body_bytes: usize) -> Vec<u8> {
        let body = &frame[FRAME_HEADER_BYTES..FRAME_HEADER_BYTES + body_bytes];
        [&[FORMAT, kind][..], self.relay.as_bytes(), body].concat()
    }
}

#[test]
fn records_persist_registration_ownership_and_stop_facts_in_the_shared_frame_order() {
    let fixture = Fixture::new();
    let registration_body = fixture.register_init.len() - FRAME_HEADER_BYTES;
    let connection = [&0x0102_u64.to_be_bytes()[..], &3_u64.to_be_bytes()].concat();
    let operation = [&[7; 16][..], &connection, &u64::MAX.to_be_bytes()].concat();
    let stop = StopInit::decode(&fixture.stop_init).unwrap();
    let cases = [
        (
            fixture.unresolved(),
            [
                fixture.prefix(UNRESOLVED, &fixture.register_init, registration_body),
                operation,
            ]
            .concat(),
        ),
        (
            fixture.published(),
            [
                fixture.prefix(PUBLISHED, &fixture.register_init, registration_body),
                connection,
            ]
            .concat(),
        ),
        (
            fixture.grace(),
            [
                fixture.prefix(GRACE, &fixture.register_init, registration_body),
                GRACE_DEADLINE.to_be_bytes().to_vec(),
            ]
            .concat(),
        ),
        (
            fixture.stopped(),
            [
                fixture.prefix(STOPPED, &fixture.stop_init, STOP_ROUTE_BYTES),
                stop.stop_id().as_bytes().to_vec(),
            ]
            .concat(),
        ),
    ];
    for (record, expected) in cases {
        assert_eq!(record.relay(), fixture.relay);
        assert_eq!(record.share(), fixture.registration().route().share_id());
        let encoded = record.encode();
        assert_eq!(encoded.as_bytes(), expected, "{record:?}");
        assert_eq!(Record::decode(&expected), Ok(record));
    }
    assert_eq!(
        fixture.unresolved().encode().as_bytes().len(),
        MAX_RECORD_BYTES
    );
}

#[test]
fn stored_bytes_that_this_kernel_did_not_write_are_refused() {
    let fixture = Fixture::new();
    let unresolved = fixture.unresolved().encode().as_bytes().to_vec();
    let published = fixture.published().encode().as_bytes().to_vec();
    let grace = fixture.grace().encode().as_bytes().to_vec();
    let stopped = fixture.stopped().encode().as_bytes().to_vec();
    let edit = |bytes: &[u8], range: Range<usize>, value: u8| {
        let mut edited = bytes.to_vec();
        edited[range].fill(value);
        edited
    };
    let cases = [
        (Vec::new(), RecordError::Length),
        (
            unresolved[..unresolved.len() - 1].to_vec(),
            RecordError::Length,
        ),
        ([&published[..], &[0]].concat(), RecordError::Length),
        (grace[..grace.len() - 1].to_vec(), RecordError::Length),
        (edit(&stopped, 0..1, FORMAT + 1), RecordError::Format),
        (edit(&grace, 0..1, FORMAT - 1), RecordError::Format),
        (edit(&published, 1..2, 0), RecordError::Kind),
        (edit(&published, 1..2, STOPPED + 1), RecordError::Kind),
        // Relay identity, then a share ID no longer derived from its key hash.
        (edit(&published, 2..34, 0), RecordError::Field),
        (edit(&published, 34..35, 0xff), RecordError::Field),
        (edit(&stopped, 34..35, 0xff), RecordError::Field),
        // Instance, then the unresolved operation's incarnation, connection
        // slot, generation and sequence.
        (edit(&unresolved, 46..62, 0), RecordError::Field),
        (edit(&unresolved, 142..158, 0), RecordError::Field),
        (edit(&unresolved, 158..166, 0), RecordError::Field),
        (edit(&unresolved, 166..174, 0), RecordError::Field),
        (edit(&unresolved, 174..182, 0), RecordError::Field),
        // The owner's connection slot and generation, and the stop ID.
        (edit(&published, 142..150, 0), RecordError::Field),
        (edit(&published, 150..158, 0), RecordError::Field),
        (edit(&stopped, 78..94, 0), RecordError::Field),
    ];
    for (bytes, error) in cases {
        assert_eq!(Record::decode(&bytes), Err(error), "{bytes:02x?}");
    }
}

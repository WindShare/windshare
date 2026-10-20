use super::*;
use crate::access::{Attribution, RegistrationGrant};
use crate::{error_frame::ErrorFrame, identifiers::StopId};

const OWNER: Attribution = Attribution::Credential {
    principal: [4; 16],
    credential: [5; 16],
};

fn attributed(f: &Fixture, relay: &mut Relay) -> Record {
    let sender = connection(1);
    let _ = relay.handle(Input::Connected(sender), f.at);
    let init = RegisterInit::decode(&f.frame("registerInitB64")).unwrap();
    let turn = relay.handle(
        Input::Register(RegistrationGrant::new(
            sender,
            init,
            OWNER,
            UnixMillis(f.at.0 + REGISTRATION_BUDGET),
        )),
        f.at,
    );
    let _ = relay.handle(
        Input::Challenge {
            request: challenge(&turn),
            seed: challenge_seed(f.text("challengeIdB64"), f.text("challengeNonceB64")),
        },
        f.at,
    );
    let _ = f.control(relay, sender, "registerProofB64", f.at);
    let publication = work(&f.control(relay, sender, "descriptorUploadB64", f.at));
    let _ = complete(relay, &publication, f.at);
    next_record(&publication)
}
#[test]
fn raw_registration_cannot_bypass_host_authorization() {
    let f = Fixture::new();
    let mut relay = f.relay();
    let sender = connection(1);
    let _ = relay.handle(Input::Connected(sender), f.at);
    let turn = relay.handle(
        Input::Control {
            connection: sender,
            frame: &f.frame("registerInitB64"),
        },
        f.at,
    );
    assert!(turn.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Error(ErrorFrame::AccessDenied),
            ..
        }
    )));
    assert!(
        !turn
            .outputs
            .iter()
            .any(|output| matches!(output, Output::ChallengeNeeded(_) | Output::Work(_)))
    );
}
#[test]
fn grant_expires_and_cannot_authorize_another_connection_lifetime() {
    let f = Fixture::new();
    let mut relay = f.relay();
    let sender = connection(1);
    let _ = relay.handle(Input::Connected(sender), f.at);
    let init = RegisterInit::decode(&f.frame("registerInitB64")).unwrap();
    let expired = relay.handle(
        Input::Register(RegistrationGrant::new(sender, init, OWNER, f.at)),
        f.at,
    );
    assert!(expired.refusal.is_some());
    let wrong = relay.handle(
        Input::Register(RegistrationGrant::new(
            connection(2),
            init,
            OWNER,
            UnixMillis(f.at.0 + 1),
        )),
        f.at,
    );
    assert_eq!(wrong.refusal, Some(Error::UnknownConnection));
}
#[test]
fn administrator_termination_uses_the_durable_stop_barrier_and_exact_credential() {
    let f = Fixture::new();
    let mut relay = f.relay();
    let record = attributed(&f, &mut relay);
    let restored = Record::decode(record.encode().as_bytes()).unwrap();
    let Record::Published(owner) = restored else {
        panic!("committed registration");
    };
    assert_eq!(owner.route().attribution(), OWNER);
    let wrong = relay.handle(
        Input::Terminate {
            credential: [6; 16],
            stop_id: StopId::new([7; 16]).unwrap(),
        },
        f.at,
    );
    assert!(wrong.outputs.is_empty());
    let intent = work(&relay.handle(
        Input::Terminate {
            credential: [5; 16],
            stop_id: StopId::new([7; 16]).unwrap(),
        },
        f.at,
    ));
    assert!(matches!(next_record(&intent), Record::Stopping(_)));
    assert_eq!(
        Record::decode(next_record(&intent).encode().as_bytes()).unwrap(),
        next_record(&intent)
    );
    let commit = work(&complete(&mut relay, &intent, f.at));
    assert!(matches!(next_record(&commit), Record::Stopped(_)));
    let done = complete(&mut relay, &commit, f.at);
    assert!(!done.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Stopped(_),
            ..
        }
    )));
    let fresh = f.control(&mut relay, connection(1), "registerInitB64", f.at);
    assert!(
        !fresh
            .outputs
            .iter()
            .any(|output| matches!(output, Output::ChallengeNeeded(_)))
    );
}

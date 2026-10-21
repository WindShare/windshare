use super::*;
use crate::access::{Attribution, RegistrationDecision, RegistrationRequest};
use crate::{error_frame::ErrorFrame, identifiers::StopId};

const OWNER: Attribution = Attribution::Credential {
    principal: [4; 16],
    credential: [5; 16],
};

fn attributed(f: &Fixture, relay: &mut Relay) -> Record {
    let sender = connection(1);
    let _ = relay.handle(Input::Connected(sender), f.at);
    let request = RegistrationRequest::from_frame(sender, &f.frame("registerInitB64")).unwrap();
    let step = relay.handle(
        Input::Register(request.resolve(RegistrationDecision::Granted(OWNER), f.at)),
        f.at,
    );
    let _ = relay.handle(
        Input::Challenge {
            request: challenge(&step),
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
fn unsupported_ingress_replies_and_closes_only_its_source() {
    let f = Fixture::new();
    let mut relay = f.relay();
    for id in [1, 2] {
        let _ = relay.handle(Input::Connected(connection(id)), f.at);
    }
    let step = relay.handle(Input::Unsupported(connection(1)), f.at);
    assert!(step.outputs.iter().any(|output| matches!(
        output,
        Output::Send { connection: target, control: Control::Error(ErrorFrame::UnsupportedMode) }
            if *target == connection(1)
    )));
    assert!(
        step.outputs
            .iter()
            .any(|output| matches!(output, Output::Close(target) if *target == connection(1)))
    );
    let healthy = f.control(&mut relay, connection(2), "registerInitB64", f.at);
    assert!(
        healthy
            .outputs
            .iter()
            .any(|output| matches!(output, Output::ChallengeNeeded(_)))
    );
}

#[test]
fn raw_registration_cannot_bypass_host_authorization() {
    let f = Fixture::new();
    let mut relay = f.relay();
    let sender = connection(1);
    let _ = relay.handle(Input::Connected(sender), f.at);
    let step = relay.handle(
        Input::Control {
            connection: sender,
            frame: &f.frame("registerInitB64"),
        },
        f.at,
    );
    assert!(step.outputs.iter().any(|output| matches!(
        output,
        Output::Send {
            control: Control::Error(ErrorFrame::AccessDenied),
            ..
        }
    )));
    assert!(
        !step
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
    let request =
        |target| RegistrationRequest::from_frame(target, &f.frame("registerInitB64")).unwrap();
    let expired = relay.handle(
        Input::Register(
            request(sender).resolve(RegistrationDecision::Granted(OWNER), UnixMillis(0)),
        ),
        f.at,
    );
    assert!(expired.refusal.is_some());
    let wrong = relay.handle(
        Input::Register(request(connection(2)).resolve(RegistrationDecision::Granted(OWNER), f.at)),
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

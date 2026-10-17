use super::*;
use crate::test_support::Fixture;

#[test]
fn durable_transactions_replay_without_overwriting_other_authority() {
    let fixture = Fixture::new();
    let identity = fixture.identity();
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("authority.sqlite");
    let works = fixture.works();
    let database = Database::open(&path, identity.relay).unwrap();
    assert!(Database::open(&path, identity.relay).is_err());
    assert!(database.load(identity.share).unwrap().is_none());
    for work in &works[..2] {
        database.execute(identity.share, work).unwrap();
        database.execute(identity.share, work).unwrap();
    }
    assert!(matches!(
        database.load(identity.share).unwrap(),
        Some(Record::Published(_))
    ));
    let first_generation = database.generation();
    drop(database);

    let database = Database::open(&path, identity.relay).unwrap();
    assert!(database.generation() > first_generation);
    assert!(matches!(
        database.recoverable_records(10).unwrap().as_slice(),
        [Record::Published(_)]
    ));
    for work in &works[2..] {
        database.execute(identity.share, work).unwrap();
        database.execute(identity.share, work).unwrap();
    }
    assert!(database.execute(identity.share, &works[0]).is_err());
    assert!(database.execute(identity.share, &works[1]).is_err());
    let Storage::Publish { record, .. } = &works[0] else {
        panic!()
    };
    let Record::Unresolved(record) = **record else {
        panic!()
    };
    database
        .execute(identity.share, &Storage::RemovePublication(record.id()))
        .unwrap();
    drop(database);

    let database = Database::open(&path, identity.relay).unwrap();
    assert!(matches!(
        database.load(identity.share).unwrap(),
        Some(Record::Stopped(_))
    ));
    let connection = database.0.connection.lock().unwrap();
    let empty: bool = connection
        .query_row(
            "SELECT descriptor IS NULL AND publication IS NULL FROM shares",
            [],
            |row| row.get(0),
        )
        .unwrap();
    drop(connection);
    assert!(empty);
}

#[test]
fn publication_cleanup_is_operation_scoped_and_atomic() {
    let fixture = Fixture::new();
    let identity = fixture.identity();
    let directory = tempfile::tempdir().unwrap();
    let database =
        Database::open(&directory.path().join("authority.sqlite"), identity.relay).unwrap();
    let works = fixture.works();
    let Storage::Publish { record, .. } = &works[0] else {
        panic!()
    };
    let Record::Unresolved(unresolved) = **record else {
        panic!()
    };
    database.execute(identity.share, &works[0]).unwrap();
    let remove = Storage::RemovePublication(unresolved.id());
    database.execute(identity.share, &remove).unwrap();
    database.execute(identity.share, &remove).unwrap();
    assert!(database.load(identity.share).unwrap().is_none());

    database.execute(identity.share, &works[0]).unwrap();
    database.execute(identity.share, &works[1]).unwrap();
    assert!(database.execute(identity.share, &remove).is_err());
    database.execute(identity.share, &works[2]).unwrap();
    database.execute(identity.share, &remove).unwrap();
    assert!(matches!(
        database.load(identity.share).unwrap(),
        Some(Record::Stopping(_))
    ));
    database.execute(identity.share, &works[3]).unwrap();
}

#[test]
fn unknown_or_corrupt_authority_is_not_absence() {
    let fixture = Fixture::new();
    let identity = fixture.identity();
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("authority.sqlite");
    let database = Database::open(&path, identity.relay).unwrap();
    database
        .execute(identity.share, &fixture.works()[0])
        .unwrap();
    database
        .0
        .connection
        .lock()
        .unwrap()
        .execute("UPDATE shares SET descriptor=x'00'", [])
        .unwrap();
    assert!(database.load(identity.share).is_err());
    assert!(database.recoverable_records(10).is_err());
    drop(database);
    assert!(Database::open(&path, RelayIdentity::new([1; 32]).unwrap()).is_err());
}

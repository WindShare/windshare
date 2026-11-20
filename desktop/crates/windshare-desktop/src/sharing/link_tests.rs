use super::*;

#[test]
fn diagnostic_link_preserves_the_share_and_leaves_the_standard_link_intact() {
    let cases = [
        (
            "https://receiver.example/s/id#key",
            "https://receiver.example/s/id?trace=1#key",
        ),
        (
            "https://receiver.example/app/s/id?r=https%3A%2F%2Frelay.example&r=https%3A%2F%2Fbackup.example#key",
            "https://receiver.example/app/s/id?r=https%3A%2F%2Frelay.example&r=https%3A%2F%2Fbackup.example&trace=1#key",
        ),
    ];
    for (original, expected) in cases {
        let publication = SharePublication::new(original, None);
        let diagnostic = publication.diagnostic().unwrap();
        let standard = publication.standard();
        assert_eq!(diagnostic.url().as_ref(), expected);
        assert_eq!(standard.url().as_ref(), original);
        assert_eq!(publication.url(LinkKind::Backup), None);
    }
}

#[test]
fn backup_link_keeps_distinct_alternate_and_rejects_empty_or_duplicate_values() {
    let primary = "https://ws.example/id?n=cn1#key";
    let backup = "https://backup.example/id?n=cn1#key";

    let publication = SharePublication::new(primary, Some(backup.into()));
    assert!(publication.has_backup());
    assert_eq!(publication.url(LinkKind::Backup).as_deref(), Some(backup));
    assert!(publication.matches(primary, Some(backup)));
    assert!(!publication.matches(primary, None));

    let duplicate = SharePublication::new(primary, Some(primary.into()));
    assert!(!duplicate.has_backup());
    assert_eq!(duplicate.url(LinkKind::Backup), None);
    assert!(duplicate.matches(primary, None));
    assert!(duplicate.matches(primary, Some(primary)));

    let empty = SharePublication::new(primary, Some("".into()));
    assert!(!empty.has_backup());
    assert_eq!(empty.url(LinkKind::Backup), None);
    assert!(empty.matches(primary, None));
}

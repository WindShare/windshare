use super::*;

#[test]
fn browser_trace_decorates_primary_link_without_altering_the_standard_link() {
    let cases = [
        (
            "https://receiver.example/s/id#key",
            "https://receiver.example/s/id?trace=1#key",
        ),
        (
            "https://receiver.example/s/id?#key",
            "https://receiver.example/s/id?trace=1#key",
        ),
        (
            "https://receiver.example/app/s/id?r=https%3A%2F%2Frelay.example&r=https%3A%2F%2Fbackup.example#key",
            "https://receiver.example/app/s/id?r=https%3A%2F%2Frelay.example&r=https%3A%2F%2Fbackup.example&trace=1#key",
        ),
        (
            "https://receiver.example/s/id?trace=1#key",
            "https://receiver.example/s/id?trace=1#key",
        ),
    ];
    for (original, expected) in cases {
        let publication = SharePublication::new(original, None);
        assert!(!publication.is_traced(false));
        assert!(publication.is_traced(true));
        assert_eq!(publication.primary(false).url().as_ref(), original);
        assert_eq!(publication.primary(true).url().as_ref(), expected);
        assert_eq!(
            publication.url(LinkKind::Primary, false).as_deref(),
            Some(original)
        );
        assert_eq!(
            publication.url(LinkKind::Primary, true).as_deref(),
            Some(expected)
        );
        assert_eq!(publication.url(LinkKind::Backup, true), None);
    }
}

#[test]
fn browser_trace_decorates_backup_links_and_falls_back_when_fragment_is_missing() {
    let primary = "https://ws.example/id?n=cn1#key";
    let backup = "https://backup.example/id?n=cn1#key";

    let publication = SharePublication::new(primary, Some(backup.into()));
    assert!(publication.has_backup());
    assert!(!publication.is_backup_traced(false));
    assert!(publication.is_backup_traced(true));
    assert_eq!(
        publication.url(LinkKind::Backup, false).as_deref(),
        Some(backup)
    );
    assert_eq!(
        publication.url(LinkKind::Backup, true).as_deref(),
        Some("https://backup.example/id?n=cn1&trace=1#key")
    );
    assert!(publication.matches(primary, Some(backup)));
    assert!(!publication.matches(primary, None));

    let duplicate = SharePublication::new(primary, Some(primary.into()));
    assert!(!duplicate.has_backup());
    assert!(!duplicate.is_backup_traced(true));
    assert_eq!(duplicate.url(LinkKind::Backup, false), None);
    assert!(duplicate.matches(primary, None));
    assert!(duplicate.matches(primary, Some(primary)));

    let empty = SharePublication::new(primary, Some("".into()));
    assert!(!empty.has_backup());
    assert_eq!(empty.url(LinkKind::Backup, false), None);
    assert!(empty.matches(primary, None));

    let fragmentless = SharePublication::new(
        "https://ws.example/id",
        Some("https://backup.example/id".into()),
    );
    assert!(!fragmentless.is_traced(true));
    assert!(!fragmentless.is_backup_traced(true));
    assert_eq!(
        fragmentless.primary(true).url().as_ref(),
        "https://ws.example/id"
    );
    assert_eq!(
        fragmentless.url(LinkKind::Backup, true).as_deref(),
        Some("https://backup.example/id")
    );
}

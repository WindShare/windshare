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
        let publication = SharePublication::new(original);
        let diagnostic = publication.link(LinkKind::Diagnostic).unwrap();
        let standard = publication.link(LinkKind::Standard).unwrap();
        assert_eq!(diagnostic.url().as_ref(), expected);
        assert_eq!(standard.url().as_ref(), original);
    }
}

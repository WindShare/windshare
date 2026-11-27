use super::*;
use windshare_host::{Failure, SelectionCollision};

#[test]
fn sanitize_display_text_borrows_safe_names_and_escapes_controls_and_bidi() {
    let normal = sanitize_display_text("季度报告 2026 🎉.pdf");
    assert!(matches!(normal, Cow::Borrowed("季度报告 2026 🎉.pdf")));

    let joined = sanitize_display_text("family\u{200D}photo.png");
    assert!(matches!(joined, Cow::Borrowed("family\u{200D}photo.png")));

    assert_eq!(
        sanitize_display_text("line1\nline2\r\t\0\u{2028}\u{2029}"),
        r"line1\nline2\r\t\u{0000}\u{2028}\u{2029}"
    );
    assert_eq!(
        sanitize_display_text("report\u{202E}fdp.exe"),
        r"report\u{202E}fdp.exe"
    );
    assert_eq!(
        sanitize_display_text("\u{061C}\u{200E}\u{200F}\u{202A}\u{2066}\u{2069}"),
        r"\u{061C}\u{200E}\u{200F}\u{202A}\u{2066}\u{2069}"
    );
}

#[test]
fn item_display_name_rejects_empty_or_slash_only_and_sanitizes_leaf() {
    for empty in ["", "/", "///", r"\", r"\\\"] {
        assert_eq!(item_display_name(empty), None);
    }
    assert_eq!(
        item_display_name(r"C:\Users\alice\deck.pptx").as_deref(),
        Some("deck.pptx")
    );
    assert_eq!(
        item_display_name(r"C:\Users\alice\folder\").as_deref(),
        Some("folder")
    );
    assert_eq!(item_display_name(r"C:\").as_deref(), Some("C:"));
    assert_eq!(
        item_display_name("/tmp/bad\u{202E}name.txt").as_deref(),
        Some(r"bad\u{202E}name.txt")
    );
}

#[test]
fn distinguishing_display_names_expands_only_diverging_parent_suffix() {
    assert_eq!(
        distinguishing_display_names(r"C:\work\notes.txt", r"D:\backup\notes.txt"),
        Some((r"work\notes.txt".into(), r"backup\notes.txt".into()))
    );
    assert_eq!(
        distinguishing_display_names(r"C:\a\shared\notes.txt", r"C:\b\shared\notes.txt"),
        Some((r"a\shared\notes.txt".into(), r"b\shared\notes.txt".into()))
    );
    assert_eq!(
        distinguishing_display_names(r"C:\work\notes.txt", r"D:\work\notes.txt"),
        Some((r"C:\work\notes.txt".into(), r"D:\work\notes.txt".into()))
    );
    assert_eq!(
        distinguishing_display_names(r"C:\work\Report.pdf", r"C:\work\report.pdf"),
        Some(("Report.pdf".into(), "report.pdf".into()))
    );
    assert_eq!(
        distinguishing_display_names(r"C:\work\notes.txt", r"C:\work\notes.txt"),
        Some(("notes.txt".into(), "notes.txt".into()))
    );
    assert_eq!(
        distinguishing_display_names(r"C:\notes.txt", r"C:\sub\notes.txt"),
        Some((r"C:\notes.txt".into(), r"sub\notes.txt".into()))
    );
    assert_eq!(
        distinguishing_display_names("/a/notes.txt", "/x/a/notes.txt"),
        Some(("a/notes.txt".into(), "x/a/notes.txt".into()))
    );
    assert_eq!(
        distinguishing_display_names(r"\\?\C:\work\notes.txt", r"\\?\D:\backup\notes.txt"),
        Some((r"work\notes.txt".into(), r"backup\notes.txt".into()))
    );
    assert_eq!(
        distinguishing_display_names("C:\\work\\caf\u{00E9}.txt", "D:\\backup\\cafe\u{0301}.txt"),
        Some((
            "work\\caf\u{00E9}.txt".into(),
            "backup\\cafe\u{0301}.txt".into()
        ))
    );
    assert_eq!(distinguishing_display_names("", r"C:\work\notes.txt"), None);
    assert_eq!(distinguishing_display_names("///", r"\\\"), None);
}

#[test]
fn share_error_from_failure_drops_empty_paths() {
    let empty_source = Failure {
        code: "startup_failed".into(),
        message: "raw fallback".into(),
        engine_code: Some("source_cloud_placeholder".into()),
        source_path: Some(String::new()),
        selection_collision: None,
    };
    assert_eq!(
        ShareError::from_failure(&empty_source),
        ShareError::CloudPlaceholder { path: None }
    );

    let empty_collision = Failure {
        code: "startup_failed".into(),
        message: "collision fallback".into(),
        engine_code: Some("selection_collision".into()),
        source_path: None,
        selection_collision: Some(SelectionCollision {
            first_path: String::new(),
            second_path: r"C:\work\notes.txt".into(),
        }),
    };
    assert_eq!(
        ShareError::from_failure(&empty_collision),
        ShareError::Message("collision fallback".into())
    );
}

#[test]
fn share_error_maps_a_selected_symbolic_link_to_the_unsupported_wording() {
    let named_link = Failure {
        code: "startup_failed".into(),
        message: "raw fallback".into(),
        engine_code: Some("source_symbolic_link".into()),
        source_path: Some(r"C:\work\photos".into()),
        selection_collision: None,
    };
    let error = ShareError::from_failure(&named_link);
    assert_eq!(
        error,
        ShareError::SourceUnsupported {
            path: Some(r"C:\work\photos".into())
        }
    );
    assert!(
        Language::English
            .share_error(&error)
            .contains("instead of a symbolic link or junction")
    );
}

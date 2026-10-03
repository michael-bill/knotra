use super::*;

#[test]
fn secure_endpoint_rules() {
    assert!(endpoint("http://127.0.0.1:8080", None).is_ok());
    assert!(endpoint("http://[::1]:8080", None).is_ok());
    assert!(endpoint("http://engine.example", Some("secret")).is_err());
    assert!(endpoint("https://engine.example", None).is_err());
    assert!(endpoint("https://engine.example/base", Some("secret")).is_ok());
    for url in [
        "file:///etc/passwd",
        "http://user:pass@localhost",
        "http://localhost/?token=x",
        "http://localhost/#secret",
    ] {
        assert!(endpoint(url, None).is_err());
    }
}

#[test]
fn packages_preserve_bytes_and_reject_traversal_duplicates() {
    let mut package = Package {
        entrypoint: "pipeline.yaml".into(),
        source: "kind: Pipeline".into(),
        files: vec![
            PackageFile {
                path: "pipeline.yaml".into(),
                content: STANDARD.encode("kind: Pipeline"),
            },
            PackageFile {
                path: "data.bin".into(),
                content: STANDARD.encode([0, 255, 128]),
            },
        ],
    };
    assert!(package.validate().is_ok());
    package.files.push(PackageFile {
        path: "DATA.bin".into(),
        content: "AA==".into(),
    });
    assert!(package.validate().is_err());
    package.files.truncate(2);
    package.files[1].path = "../secret".into();
    assert!(package.validate().is_err());
}

#[test]
fn package_manifest_requires_matching_entrypoint_bytes() {
    let mut package = Package {
        entrypoint: "pipeline.yaml".into(),
        source: "kind: Pipeline".into(),
        files: vec![],
    };
    assert!(package.validate().is_err());
    package.files.push(PackageFile {
        path: "pipeline.yaml".into(),
        content: STANDARD.encode("kind: EngineProfile"),
    });
    assert!(package.validate().is_err());
    package.files[0].content = STANDARD.encode(&package.source);
    assert!(package.validate().is_ok());
}

#[test]
fn identifiers_cannot_change_api_routes() {
    let session = Session {
        base: Url::parse("http://localhost:8080/base/").unwrap(),
        token: None,
        key: "x".into(),
        client: Client::new(),
    };
    assert_eq!(
        session.url(&["runs", "a/b?x#y"]).unwrap().as_str(),
        "http://localhost:8080/base/v1/runs/a%2Fb%3Fx%23y"
    );
    assert!(session.url(&["runs", ".."]).is_err());
}

#[test]
fn manifest_path_folding_preserves_non_ascii_names() {
    let package = Package {
        entrypoint: "pipeline.yaml".into(),
        source: String::new(),
        files: vec![
            PackageFile {
                path: "pipeline.yaml".into(),
                content: STANDARD.encode("kind: Pipeline"),
            },
            PackageFile {
                path: "Ä.txt".into(),
                content: "AA==".into(),
            },
            PackageFile {
                path: "ä.txt".into(),
                content: "AA==".into(),
            },
        ],
    };
    assert!(package.validate().is_ok());
}

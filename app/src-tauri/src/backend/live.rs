use super::*;
use std::time::{SystemTime, UNIX_EPOCH};
use tauri::ipc::InvokeResponseBody;

fn operation(prefix: &str, nonce: u128) -> String {
    format!("desktop-{prefix}-{nonce}")
}

#[test]
#[ignore = "requires KNOTRA_E2E_ENDPOINT pointing to a real development engine"]
fn real_engine_commands_events_and_artifacts() {
    let address = std::env::var("KNOTRA_E2E_ENDPOINT").expect("set KNOTRA_E2E_ENDPOINT");
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_nanos();
    let directory = std::env::temp_dir().join(format!("knotra-native-{nonce}"));
    std::fs::create_dir(&directory).unwrap();
    let path = directory.join("workspace.sqlite");

    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap()
        .block_on(async {
            let mut session = Session {
                base: endpoint(&address, None).unwrap(),
                key: String::new(),
                token: None,
                client: Client::builder()
                    .redirect(reqwest::redirect::Policy::none())
                    .build()
                    .unwrap(),
            };
            let info = session
                .json(Method::GET, &["info"], None, None)
                .await
                .unwrap();
            assert_eq!(info["protocol"], "knotra.desktop/1");
            session.key = info["engineId"].as_str().unwrap().into();
            let store = store::Store::open(&path).unwrap();
            let bytes = [0, 255, 128, 13, 10, 0];
            let upload = Call::Upload {
                file: PackageFile {
                    path: format!("native-{nonce}.bin"),
                    content: STANDARD.encode(bytes),
                },
                media_type: "application/octet-stream".into(),
                operation_id: operation("upload", nonce),
            };
            let uploaded = execute(&store, &session, &upload).await.unwrap();
            assert_eq!(execute(&store, &session, &upload).await.unwrap(), uploaded);
            let artifact_id = uploaded["artifact"]["id"].as_str().unwrap();
            let downloaded = download(&session, artifact_id).await.unwrap();
            assert_eq!(
                STANDARD
                    .decode(downloaded["content"].as_str().unwrap())
                    .unwrap(),
                bytes
            );

            let source = r#"apiVersion: knotra/v1
kind: Pipeline
metadata: {name: native-review}
spec:
  inputs:
    source: {artifact: {mediaTypes: [application/octet-stream]}}
  nodes:
    review:
      type: human
      inputs:
        source: {artifact: {mediaTypes: [application/octet-stream]}, bind: {from: inputs.source}}
      human: {prompt: {text: Native review}}
      outputs:
        approved: {schema: {type: boolean, const: true}}
  outputs:
    approved: {schema: {type: boolean}, bind: {from: nodes.review.outputs.approved}}
"#;
            let package = Package {
                entrypoint: "pipeline.yaml".into(),
                source: source.into(),
                files: vec![PackageFile {
                    path: "pipeline.yaml".into(),
                    content: STANDARD.encode(source),
                }],
            };
            let artifacts = json!({"source": artifact_id});
            let checked = execute(
                &store,
                &session,
                &Call::Validate {
                    package: package.clone(),
                    profile: "local".into(),
                    inputs: json!({}),
                    artifacts: artifacts.clone(),
                },
            )
            .await
            .unwrap();
            assert_eq!(checked["valid"], true);
            let published = execute(
                &store,
                &session,
                &Call::Publish {
                    package,
                    operation_id: operation("publish", nonce),
                },
            )
            .await
            .unwrap();
            let start = Call::Start {
                definition_id: published["definition"]["id"].as_str().unwrap().into(),
                profile: "local".into(),
                inputs: json!({}),
                artifacts,
                operation_id: operation("start", nonce),
            };
            let started = execute(&store, &session, &start).await.unwrap();
            let run_id = started["run"]["id"].as_str().unwrap();
            // Reopening SQLite must recover the receipt without creating another run.
            drop(store);
            let store = Arc::new(store::Store::open(&path).unwrap());
            assert_eq!(execute(&store, &session, &start).await.unwrap(), started);

            let request = tokio::time::timeout(Duration::from_secs(30), async {
                loop {
                    let page = execute(&store, &session, &Call::Requests { cursor: None })
                        .await
                        .unwrap();
                    if let Some(request) =
                        page["items"].as_array().unwrap().iter().find(|request| {
                            request["runId"] == run_id && request["status"] == "open"
                        })
                    {
                        break request.clone();
                    }
                    tokio::time::sleep(Duration::from_millis(100)).await;
                }
            })
            .await
            .unwrap();
            let request_id = request["id"].as_str().unwrap();
            assert_eq!(
                request["inputs"]["artifacts"]["source"]["sha256"],
                uploaded["artifact"]["sha256"]
            );
            let invalid = execute(
                &store,
                &session,
                &Call::Respond {
                    request_id: request_id.into(),
                    outputs: json!({"approved": false}),
                    operation_id: operation("invalid", nonce),
                },
            )
            .await
            .unwrap_err();
            assert_eq!(invalid.status, Some(422));
            execute(
                &store,
                &session,
                &Call::Respond {
                    request_id: request_id.into(),
                    outputs: json!({"approved": true}),
                    operation_id: operation("respond", nonce),
                },
            )
            .await
            .unwrap();
            tokio::time::timeout(Duration::from_secs(30), async {
                loop {
                    let value = execute(
                        &store,
                        &session,
                        &Call::Run {
                            run_id: run_id.into(),
                        },
                    )
                    .await
                    .unwrap();
                    if value["run"]["status"] == "succeeded" {
                        assert_eq!(value["run"]["outputs"]["approved"], true);
                        break;
                    }
                    assert_ne!(value["run"]["status"], "failed", "{value}");
                    tokio::time::sleep(Duration::from_millis(100)).await;
                }
            })
            .await
            .unwrap();

            let observed = Arc::new(Mutex::new(Vec::new()));
            let delivered = observed.clone();
            let channel = Channel::new(move |body| {
                if let InvokeResponseBody::Json(text) = body {
                    let value: Value = serde_json::from_str(&text).unwrap();
                    if value["type"] == "event" {
                        delivered.lock().unwrap().push(value["event"].clone());
                    }
                }
                Ok(())
            });
            // The engine leaves SSE open after completion; stop after replay arrives.
            let _ = tokio::time::timeout(
                Duration::from_secs(2),
                stream(&session, &store, run_id, &channel),
            )
            .await;
            let events = store.events(&session.key, run_id).unwrap();
            assert!(events.len() > 4);
            assert_eq!(events.len(), observed.lock().unwrap().len());
            let cursor = store.cursor(&session.key, run_id).unwrap();
            assert!(cursor.is_some());
            let _ = tokio::time::timeout(
                Duration::from_secs(1),
                stream(&session, &store, run_id, &channel),
            )
            .await;
            assert_eq!(
                store.events(&session.key, run_id).unwrap().len(),
                events.len()
            );
            assert_eq!(store.cursor(&session.key, run_id).unwrap(), cursor);
            assert!(store.snapshot(&session.key).unwrap()["pending"]
                .as_array()
                .unwrap()
                .is_empty());
        });
    std::fs::remove_dir_all(directory).unwrap();
}

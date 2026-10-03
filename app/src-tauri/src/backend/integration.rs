use super::*;
use std::{
    io::{Read, Write},
    net::TcpListener,
};
use tauri::ipc::InvokeResponseBody;
fn runtime() -> tokio::runtime::Runtime {
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap()
}

fn fixture<F>(count: usize, handle: F) -> (Session, std::thread::JoinHandle<()>)
where
    F: Fn(usize, String) -> (u16, &'static str, Vec<u8>) + Send + 'static,
{
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let task = std::thread::spawn(move || {
        for index in 0..count {
            let (mut socket, _) = listener.accept().unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            let mut bytes = Vec::new();
            let mut length = None;
            loop {
                let mut buffer = [0; 4096];
                let size = socket.read(&mut buffer).unwrap();
                if size == 0 {
                    break;
                }
                bytes.extend_from_slice(&buffer[..size]);
                if let Some(end) = bytes.windows(4).position(|part| part == b"\r\n\r\n") {
                    let headers = String::from_utf8_lossy(&bytes[..end]).to_lowercase();
                    length = Some(
                        end + 4
                            + headers
                                .lines()
                                .find_map(|line| {
                                    line.strip_prefix("content-length: ")
                                        .and_then(|value| value.parse::<usize>().ok())
                                })
                                .unwrap_or(0),
                    );
                }
                if length.is_some_and(|length| bytes.len() >= length) {
                    break;
                }
            }
            let (status, kind, body) = handle(index, String::from_utf8(bytes).unwrap());
            let headers=format!("HTTP/1.1 {status} Test\r\nContent-Type: {kind}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",body.len());
            socket.write_all(headers.as_bytes()).unwrap();
            socket.write_all(&body).unwrap();
        }
    });
    (
        Session {
            base: Url::parse(&format!("http://{address}/")).unwrap(),
            key: "fixture".into(),
            token: Some("memory-only".into()),
            client: Client::builder()
                .redirect(reqwest::redirect::Policy::none())
                .build()
                .unwrap(),
        },
        task,
    )
}

#[test]
fn lost_receipt_reuses_exact_key_payload_and_only_one_logical_operation() {
    runtime().block_on(async {
        let deliveries = Arc::new(Mutex::new(Vec::<String>::new()));
        let recorded = deliveries.clone();
        let (session, task) = fixture(2, move |index, request| {
            recorded.lock().unwrap().push(request);
            (
                200,
                "application/json",
                if index == 0 {
                    b"{".to_vec()
                } else {
                    br#"{"run":{"id":"r1"}}"#.to_vec()
                },
            )
        });
        let store = store::Store::open(std::path::Path::new(":memory:")).unwrap();
        let call = Call::Start {
            definition_id: "d1".into(),
            profile: "p1".into(),
            inputs: json!({"a":null}),
            artifacts: json!({}),
            operation_id: "stable-operation".into(),
        };
        assert!(execute(&store, &session, &call).await.is_err());
        assert_eq!(
            store.snapshot("fixture").unwrap()["pending"]
                .as_array()
                .unwrap()
                .len(),
            1
        );
        assert_eq!(
            execute(&store, &session, &call).await.unwrap()["run"]["id"],
            "r1"
        );
        assert_eq!(
            execute(&store, &session, &call).await.unwrap()["run"]["id"],
            "r1"
        );
        assert!(store.snapshot("fixture").unwrap()["pending"]
            .as_array()
            .unwrap()
            .is_empty());
        task.join().unwrap();
        let sent = deliveries.lock().unwrap();
        assert_eq!(sent[0], sent[1]);
        assert!(sent[0]
            .to_lowercase()
            .contains("idempotency-key: stable-operation"));
        assert!(sent[0]
            .to_lowercase()
            .contains("authorization: bearer memory-only"));
        assert!(!store
            .snapshot("fixture")
            .unwrap()
            .to_string()
            .contains("memory-only"));
    });
}

#[test]
fn definitive_schema_rejection_keeps_request_retriable_with_new_operation() {
    runtime().block_on(async {
        let (session, task) = fixture(1, |_, request| {
            assert!(request.starts_with("POST /v1/requests/request-A/response"));
            (
                422,
                "application/json",
                br#"{"code":"OUTPUT_INVALID","message":"Invalid response","diagnostics":[]}"#
                    .to_vec(),
            )
        });
        let store = store::Store::open(std::path::Path::new(":memory:")).unwrap();
        let call = Call::Respond {
            request_id: "request-A".into(),
            outputs: json!({"feedback":false}),
            operation_id: "response-1".into(),
        };
        assert_eq!(
            execute(&store, &session, &call).await.unwrap_err().status,
            Some(422)
        );
        assert_eq!(
            execute(&store, &session, &call).await.unwrap_err().status,
            Some(422)
        );
        assert!(store.snapshot("fixture").unwrap()["pending"]
            .as_array()
            .unwrap()
            .is_empty());
        task.join().unwrap();
        assert!(store
            .prepare(
                "fixture",
                "response-2",
                &json!({"op":"respond","requestId":"request-A","outputs":{"feedback":"valid"}})
            )
            .unwrap()
            .is_none());
    });
}

#[test]
fn sse_resume_persists_before_delivery_and_deduplicates_replay() {
    runtime().block_on(async {
        let (session, task) = fixture(2, |index, request| {
            if index == 1 {
                assert!(request.to_lowercase().contains("last-event-id: e1"));
            }
            let one = "id: e1\ndata: {\"id\":\"e1\",\"runId\":\"r1\",\"message\":\"one\"}\n\n";
            let two = "id: e2\ndata: {\"id\":\"e2\",\"runId\":\"r1\",\"message\":\"two\"}\n\n";
            (
                200,
                "text/event-stream",
                if index == 0 {
                    format!("{one}{one}")
                } else {
                    format!("{one}{two}")
                }
                .into_bytes(),
            )
        });
        let store = Arc::new(store::Store::open(std::path::Path::new(":memory:")).unwrap());
        let received = Arc::new(Mutex::new(Vec::new()));
        let recorded = received.clone();
        let committed = store.clone();
        let channel = Channel::new(move |body| {
            if let InvokeResponseBody::Json(text) = body {
                let value: Value = serde_json::from_str(&text).unwrap();
                if value["type"] == "event" {
                    assert_eq!(
                        committed.cursor("fixture", "r1").unwrap().as_deref(),
                        value["event"]["id"].as_str()
                    );
                    recorded.lock().unwrap().push(value["event"].clone());
                }
            }
            Ok(())
        });
        stream(&session, &store, "r1", &channel).await.unwrap();
        stream(&session, &store, "r1", &channel).await.unwrap();
        task.join().unwrap();
        assert_eq!(received.lock().unwrap().len(), 2);
        assert_eq!(store.events("fixture", "r1").unwrap().len(), 2);
    });
}

#[test]
fn downloaded_binary_is_hash_checked_before_leaving_native_backend() {
    runtime().block_on(async {
        for corrupt in [false, true] {
            let bytes = vec![0, 255, 128, 10];
            let hash = if corrupt {
                "0".repeat(64)
            } else {
                format!("{:x}", Sha256::digest(&bytes))
            };
            let (session, task) = fixture(2, move |index, _| {
                if index == 0 {
                    (
                        200,
                        "application/json",
                        json!({"artifact":{"id":"a1","size":4,"sha256":hash}})
                            .to_string()
                            .into_bytes(),
                    )
                } else {
                    (200, "application/octet-stream", bytes.clone())
                }
            });
            let result = download(&session, "a1").await;
            task.join().unwrap();
            if corrupt {
                assert_eq!(result.unwrap_err().code, "integrity");
            } else {
                assert_eq!(
                    STANDARD
                        .decode(result.unwrap()["content"].as_str().unwrap())
                        .unwrap(),
                    [0, 255, 128, 10]
                );
            }
        }
    });
}

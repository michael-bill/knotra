mod sse;
mod store;
use crate::package::{valid_path, PackageFile};
use base64::{engine::general_purpose::STANDARD, Engine};
use futures_util::StreamExt;
use reqwest::{Client, Method, Url};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::{
    collections::{HashMap, HashSet},
    sync::{
        atomic::{AtomicU64, Ordering},
        Arc, Mutex,
    },
    time::Duration,
};
use tauri::{ipc::Channel, Manager, State};
pub type Result<T> = std::result::Result<T, Error>;
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Error {
    pub code: String,
    pub message: String,
    pub status: Option<u16>,
    pub diagnostics: Vec<Value>,
}

impl Error {
    fn new(code: &str, message: &str) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
            status: None,
            diagnostics: vec![],
        }
    }

    fn storage(_: impl std::fmt::Display) -> Self {
        Self::new(
            "storage",
            "Workspace database operation failed. Your data has not been replaced.",
        )
    }

    fn transport(_: impl std::fmt::Display) -> Self {
        Self::new(
            "transport",
            "Engine request failed. Its outcome may be unknown; reconcile the saved operation before sending a new command.",
        )
    }
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.message)
    }
}

impl std::error::Error for Error {}
#[derive(Clone)]
struct Session {
    base: Url,
    key: String,
    token: Option<String>,
    client: Client,
}

pub struct Backend {
    store: Arc<store::Store>,
    session: Mutex<Option<Session>>,
    revision: AtomicU64,
    watches: Mutex<HashMap<String, tauri::async_runtime::JoinHandle<()>>>,
}

impl Backend {
    pub fn new(app: &tauri::App) -> Result<Self> {
        let dir = app.path().app_data_dir().map_err(Error::storage)?;
        std::fs::create_dir_all(&dir).map_err(Error::storage)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700))
                .map_err(Error::storage)?;
        }
        Ok(Self {
            store: Arc::new(store::Store::open(&dir.join("workspace.sqlite"))?),
            session: Mutex::new(None),
            revision: AtomicU64::new(0),
            watches: Mutex::new(HashMap::new()),
        })
    }

    fn session(&self) -> Result<Session> {
        self.session
            .lock()
            .map_err(Error::storage)?
            .clone()
            .ok_or_else(|| Error::new("disconnected", "Connect to an engine in Settings first."))
    }

    fn stop(&self) -> Result<()> {
        for (_, task) in self.watches.lock().map_err(Error::storage)?.drain() {
            task.abort();
        }
        Ok(())
    }
}

fn endpoint(base: &str, token: Option<&str>) -> Result<Url> {
    let mut url = Url::parse(base).map_err(|_| Error::new("input", "Invalid engine URL."))?;
    if !matches!(url.scheme(), "http" | "https")
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || url.host_str().is_none()
    {
        return Err(Error::new(
            "input",
            "Use an HTTP(S) engine base URL without credentials, query or fragment.",
        ));
    }
    let local = url.host_str() == Some("localhost")
        || url
            .host_str()
            .and_then(|host| {
                host.trim_matches(['[', ']'])
                    .parse::<std::net::IpAddr>()
                    .ok()
            })
            .is_some_and(|ip| ip.is_loopback());
    if !local && (url.scheme() != "https" || token.is_none_or(|token| token.is_empty())) {
        return Err(Error::new(
            "input",
            "Remote engines require HTTPS and an access token.",
        ));
    }
    if token.is_some_and(|token| token.len() > 8192 || token.chars().any(|c| c.is_control())) {
        return Err(Error::new("input", "Invalid access token."));
    }
    let path = format!("{}/", url.path().trim_end_matches('/'));
    url.set_path(&path);
    Ok(url)
}

impl Session {
    fn url(&self, path: &[&str]) -> Result<Url> {
        let mut url = self.base.clone();
        let mut segments = url
            .path_segments_mut()
            .map_err(|_| Error::new("input", "Invalid engine URL."))?;
        segments.pop_if_empty();
        segments.extend(["v1"]);
        for part in path {
            if part.is_empty()
                || part.len() > 256
                || part.chars().any(|c| c.is_control())
                || matches!(*part, "." | "..")
            {
                return Err(Error::new("input", "Invalid API identifier."));
            }
            segments.push(part);
        }
        drop(segments);
        Ok(url)
    }

    fn request(&self, method: Method, path: &[&str]) -> Result<reqwest::RequestBuilder> {
        let request = self.client.request(method, self.url(path)?);
        Ok(if let Some(token) = &self.token {
            request.bearer_auth(token)
        } else {
            request
        })
    }

    async fn json(
        &self,
        method: Method,
        path: &[&str],
        body: Option<&Value>,
        operation: Option<&str>,
    ) -> Result<Value> {
        let mut request = self
            .request(method, path)?
            .timeout(Duration::from_secs(30))
            .header("Accept", "application/json");
        if let Some(body) = body {
            request = request.json(body);
        }
        if let Some(id) = operation {
            request = request.header("Idempotency-Key", id);
        }
        let response = request.send().await.map_err(Error::transport)?;
        let status = response.status();
        let bytes = limited(response, 96 * 1024 * 1024).await?;
        let value: Value = serde_json::from_slice(&bytes)
            .map_err(|_| Error::new("protocol", "Engine returned an invalid JSON response."))?;
        if !status.is_success() {
            return Err(Error {
                code: value["code"].as_str().unwrap_or("engine").into(),
                message: value["message"]
                    .as_str()
                    .unwrap_or("Engine rejected the request.")
                    .into(),
                status: Some(status.as_u16()),
                diagnostics: value["diagnostics"].as_array().cloned().unwrap_or_default(),
            });
        }
        if !value.is_object() {
            return Err(Error::new(
                "protocol",
                "Expected an engine response object.",
            ));
        }
        Ok(value)
    }
}

async fn limited(response: reqwest::Response, max: usize) -> Result<Vec<u8>> {
    if response
        .content_length()
        .is_some_and(|size| size > max as u64)
    {
        return Err(Error::new(
            "limit",
            "Engine response exceeds the client size limit.",
        ));
    }
    let mut stream = response.bytes_stream();
    let mut data = Vec::new();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(Error::transport)?;
        if data.len() + chunk.len() > max {
            return Err(Error::new(
                "limit",
                "Engine response exceeds the client size limit.",
            ));
        }
        data.extend_from_slice(&chunk);
    }
    Ok(data)
}

#[derive(Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct Package {
    entrypoint: String,
    source: String,
    files: Vec<PackageFile>,
}

impl Package {
    fn validate(&self) -> Result<()> {
        let mut paths = HashSet::new();
        if !valid_path(&self.entrypoint) {
            return Err(Error::new("input", "Invalid package entrypoint."));
        }
        if self.files.len() > 512 {
            return Err(Error::new("limit", "Package exceeds 512 files."));
        }
        let mut size = 0;
        let mut entrypoint_found = false;
        for file in &self.files {
            if !valid_path(&file.path) || !paths.insert(file.path.to_ascii_lowercase()) {
                return Err(Error::new("input", "Invalid or duplicate package path."));
            }
            let bytes = STANDARD
                .decode(&file.content)
                .map_err(|_| Error::new("input", "Invalid package file encoding."))?;
            size += bytes.len();
            if file.path == self.entrypoint {
                entrypoint_found = true;
                if !self.source.is_empty() && bytes != self.source.as_bytes() {
                    return Err(Error::new(
                        "input",
                        "Entrypoint bytes must match the package source.",
                    ));
                }
            }
        }
        if !entrypoint_found {
            return Err(Error::new(
                "input",
                "Package manifest must include its entrypoint.",
            ));
        }
        if size > 64 * 1024 * 1024 {
            return Err(Error::new("limit", "Package exceeds 64 MiB."));
        }
        Ok(())
    }
}

/// Closed command vocabulary. The webview cannot choose arbitrary HTTP methods or URLs.
#[derive(Clone, Serialize, Deserialize)]
#[serde(
    tag = "op",
    rename_all = "camelCase",
    rename_all_fields = "camelCase",
    deny_unknown_fields
)]
pub enum Call {
    Info,
    Definitions,
    Runs {
        cursor: Option<String>,
    },
    Run {
        run_id: String,
    },
    Requests {
        cursor: Option<String>,
    },
    Artifacts {
        cursor: Option<String>,
    },
    Resources,
    Profiles,
    Definition {
        definition_id: String,
    },
    Validate {
        package: Package,
        profile: String,
        inputs: Value,
        artifacts: Value,
    },
    Publish {
        package: Package,
        operation_id: String,
    },
    Start {
        definition_id: String,
        profile: String,
        inputs: Value,
        artifacts: Value,
        operation_id: String,
    },
    Cancel {
        run_id: String,
        operation_id: String,
    },
    Respond {
        request_id: String,
        outputs: Value,
        operation_id: String,
    },
    Resume {
        run_id: String,
        operation_id: String,
    },
    Resolve {
        run_id: String,
        instance_id: String,
        resolution: Value,
        operation_id: String,
    },
    Upload {
        file: PackageFile,
        media_type: String,
        operation_id: String,
    },
    Artifact {
        artifact_id: String,
    },
}

impl Call {
    fn operation(&self) -> Option<&str> {
        match self {
            Self::Publish { operation_id, .. }
            | Self::Start { operation_id, .. }
            | Self::Cancel { operation_id, .. }
            | Self::Respond { operation_id, .. }
            | Self::Resume { operation_id, .. }
            | Self::Resolve { operation_id, .. }
            | Self::Upload { operation_id, .. } => Some(operation_id),
            _ => None,
        }
    }

    fn route(&self) -> (Method, Vec<&str>, Option<Value>, Option<&str>) {
        match self {
            Self::Info => (Method::GET, vec!["info"], None, None),
            Self::Definitions => (Method::GET, vec!["definitions"], None, None),
            Self::Profiles => (Method::GET, vec!["profiles"], None, None),
            Self::Resources => (Method::GET, vec!["resources"], None, None),
            Self::Runs { cursor } => (Method::GET, vec!["runs"], None, cursor.as_deref()),
            Self::Requests { cursor } => (Method::GET, vec!["requests"], None, cursor.as_deref()),
            Self::Artifacts { cursor } => (Method::GET, vec!["artifacts"], None, cursor.as_deref()),
            Self::Definition { definition_id } => {
                (Method::GET, vec!["definitions", definition_id], None, None)
            }
            Self::Run { run_id } => (Method::GET, vec!["runs", run_id], None, None),
            Self::Validate {
                package,
                profile,
                inputs,
                artifacts,
            } => (
                Method::POST,
                vec!["packages", "validate"],
                Some(
                    json!({"package":package,"profile":profile,"inputs":inputs,"artifacts":artifacts}),
                ),
                None,
            ),
            Self::Publish { package, .. } => (
                Method::POST,
                vec!["definitions"],
                Some(json!({"package":package})),
                None,
            ),
            Self::Start {
                definition_id,
                profile,
                inputs,
                artifacts,
                ..
            } => (
                Method::POST,
                vec!["runs"],
                Some(
                    json!({"definitionId":definition_id,"profile":profile,"inputs":inputs,"artifacts":artifacts}),
                ),
                None,
            ),
            Self::Cancel { run_id, .. } => (
                Method::POST,
                vec!["runs", run_id, "cancel"],
                Some(json!({})),
                None,
            ),
            Self::Resume { run_id, .. } => (
                Method::POST,
                vec!["runs", run_id, "resume"],
                Some(json!({})),
                None,
            ),
            Self::Resolve {
                run_id,
                instance_id,
                resolution,
                ..
            } => (
                Method::POST,
                vec!["runs", run_id, "instances", instance_id, "resolve"],
                Some(resolution.clone()),
                None,
            ),
            Self::Respond {
                request_id,
                outputs,
                ..
            } => (
                Method::POST,
                vec!["requests", request_id, "response"],
                Some(json!({"outputs":outputs})),
                None,
            ),
            Self::Upload {
                file, media_type, ..
            } => (
                Method::POST,
                vec!["artifacts"],
                Some(json!({"name":file.path,"content":file.content,"mediaType":media_type})),
                None,
            ),
            Self::Artifact { artifact_id } => {
                (Method::GET, vec!["artifacts", artifact_id], None, None)
            }
        }
    }
}

#[tauri::command]
pub async fn workspace_load(backend: State<'_, Backend>) -> Result<Option<Value>> {
    let store = backend.store.clone();
    tauri::async_runtime::spawn_blocking(move || store.load())
        .await
        .map_err(Error::storage)?
}

#[tauri::command]
pub async fn workspace_save(backend: State<'_, Backend>, value: Value) -> Result<()> {
    let store = backend.store.clone();
    tauri::async_runtime::spawn_blocking(move || store.save(&value))
        .await
        .map_err(Error::storage)?
}

#[tauri::command]
pub async fn engine_connect(
    backend: State<'_, Backend>,
    url: String,
    token: Option<String>,
) -> Result<Value> {
    let revision = backend.revision.fetch_add(1, Ordering::SeqCst) + 1;
    backend.stop()?;
    *backend.session.lock().map_err(Error::storage)? = None;
    let base = endpoint(&url, token.as_deref())?;
    let mut session = Session {
        base,
        key: String::new(),
        token,
        client: Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(5))
            .build()
            .map_err(Error::transport)?,
    };
    let info = session.json(Method::GET, &["info"], None, None).await?;
    if info["protocol"] != "knotra.desktop/1"
        || !info["engineId"].is_string()
        || !info["principalId"].is_string()
        || !info["capabilities"].is_array()
    {
        return Err(Error::new(
            "incompatible",
            "Engine does not implement the knotra.desktop/1 contract.",
        ));
    }
    let valid_id = |value: &Value| {
        value.as_str().is_some_and(|id| {
            !id.is_empty() && id.len() <= 256 && !id.chars().any(|c| c.is_control())
        })
    };
    if !valid_id(&info["engineId"])
        || !valid_id(&info["principalId"])
        || !info["version"].is_string()
        || !info["capabilities"]
            .as_array()
            .unwrap()
            .iter()
            .all(Value::is_string)
    {
        return Err(Error::new(
            "protocol",
            "Invalid engine identity or capabilities.",
        ));
    }
    session.key = serde_json::to_string(&json!([
        session.base.as_str(),
        info["engineId"],
        info["principalId"]
    ]))
    .map_err(Error::storage)?;
    backend.store.cache(&session.key, "info", &info)?;
    let mut active = backend.session.lock().map_err(Error::storage)?;
    if revision != backend.revision.load(Ordering::SeqCst) {
        return Err(Error::new(
            "disconnected",
            "Connection was superseded while connecting.",
        ));
    }
    *active = Some(session);
    Ok(info)
}

#[tauri::command]
pub fn engine_disconnect(backend: State<'_, Backend>) -> Result<()> {
    backend.revision.fetch_add(1, Ordering::SeqCst);
    backend.stop()?;
    *backend.session.lock().map_err(Error::storage)? = None;
    Ok(())
}

#[tauri::command]
pub fn engine_cache(backend: State<'_, Backend>) -> Result<Value> {
    backend.store.snapshot(&backend.session()?.key)
}

#[tauri::command]
pub async fn engine_call(backend: State<'_, Backend>, call: Call) -> Result<Value> {
    let session = backend.session()?;
    execute(&backend.store, &session, &call).await
}

async fn execute(store: &store::Store, session: &Session, call: &Call) -> Result<Value> {
    match call {
        Call::Publish { package, .. } | Call::Validate { package, .. } => package.validate()?,
        Call::Respond { outputs, .. } if !outputs.is_object() => {
            return Err(Error::new(
                "input",
                "Human response must be a JSON object of output ports.",
            ))
        }
        Call::Upload { file, .. }
            if !valid_path(&file.path)
                || STANDARD
                    .decode(&file.content)
                    .map_err(|_| Error::new("input", "Invalid artifact encoding."))?
                    .len()
                    > 64 * 1024 * 1024 =>
        {
            return Err(Error::new("input", "Invalid artifact name or size."));
        }
        _ => {}
    }
    if let Call::Start {
        inputs, artifacts, ..
    }
    | Call::Validate {
        inputs, artifacts, ..
    } = call
    {
        if !inputs.is_object() || !artifacts.is_object() {
            return Err(Error::new(
                "input",
                "Inputs and artifact bindings must be objects.",
            ));
        }
    }
    let (method, path, body, cursor) = call.route();
    session.url(&path)?; // Reject malformed targets before creating a pending network command.
    let command = serde_json::to_value(call).map_err(Error::storage)?;
    if let Some(id) = call.operation() {
        if id.is_empty()
            || id.len() > 128
            || !id
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"-_".contains(&b))
        {
            return Err(Error::new("input", "Invalid operation ID."));
        }
        if let Some(cached) = store.prepare(&session.key, id, &command)? {
            if let Some(rejection) = cached.get("clientRejection") {
                return Err(serde_json::from_value(rejection.clone()).map_err(Error::storage)?);
            }
            return Ok(cached);
        }
    }
    let value = if let Some(cursor) = cursor {
        let response = session
            .request(method, &path)?
            .query(&[("cursor", cursor)])
            .timeout(Duration::from_secs(30))
            .send()
            .await
            .map_err(Error::transport)?;
        let status = response.status();
        let data = limited(response, 96 * 1024 * 1024).await?;
        if !status.is_success() {
            return Err(Error::new(
                "engine",
                "Engine rejected the pagination request.",
            ));
        }
        serde_json::from_slice(&data)
            .map_err(|_| Error::new("protocol", "Invalid page response."))?
    } else {
        match session
            .json(method, &path, body.as_ref(), call.operation())
            .await
        {
            Ok(value) => value,
            Err(error) => {
                if error.status.is_some_and(|status| {
                    (400..500).contains(&status) && status != 408 && status != 429
                }) {
                    if let Some(id) = call.operation() {
                        store.complete(&session.key, id, &json!({"clientRejection":error}))?;
                    }
                }
                return Err(error);
            }
        }
    };
    check_response(call, &value)?;
    if let Some(id) = call.operation() {
        store.complete(&session.key, id, &value)?;
    }
    if call.operation().is_none() && !matches!(call, Call::Validate { .. }) {
        let key = serde_json::to_string(call).map_err(Error::storage)?;
        store.cache(&session.key, &key, &value)?;
    }
    Ok(value)
}

fn check_response(call: &Call, value: &Value) -> Result<()> {
    let identifier = |value: &Value| {
        value.as_str().is_some_and(|id| {
            !id.is_empty() && id.len() <= 256 && !id.chars().any(|c| c.is_control())
        })
    };
    let page = || {
        value["items"].is_array()
            && (value["nextCursor"].is_null() || value["nextCursor"].is_string())
    };
    let valid = match call {
        Call::Validate { .. } => value["valid"].is_boolean() && value["diagnostics"].is_array(),
        Call::Publish { .. } | Call::Definition { .. } => identifier(&value["definition"]["id"]),
        Call::Start { .. } => identifier(&value["run"]["id"]),
        Call::Run { run_id } => value["run"]["id"] == *run_id,
        Call::Cancel { run_id, .. }
        | Call::Resume { run_id, .. }
        | Call::Resolve { run_id, .. } => value["accepted"] == true && value["runId"] == *run_id,
        Call::Respond { request_id, .. } => {
            value["accepted"] == true && value["requestId"] == *request_id
        }
        Call::Upload { .. } | Call::Artifact { .. } => {
            identifier(&value["artifact"]["id"])
                && value["artifact"]["size"].as_u64().is_some()
                && value["artifact"]["sha256"]
                    .as_str()
                    .is_some_and(|hash| hash.len() == 64)
        }
        Call::Runs { .. } | Call::Requests { .. } | Call::Artifacts { .. } => page(),
        Call::Profiles | Call::Resources | Call::Definitions => value["items"].is_array(),
        Call::Info => value["protocol"] == "knotra.desktop/1" && identifier(&value["engineId"]),
    };
    if !valid {
        return Err(Error::new(
            "protocol",
            "Engine returned an incompatible response. The command receipt was not confirmed.",
        ));
    }

    fn safe(value: &Value) -> bool {
        match value {
            Value::Number(number) => {
                number
                    .as_i64()
                    .is_none_or(|value| value.unsigned_abs() <= 9_007_199_254_740_991)
                    && number
                        .as_u64()
                        .is_none_or(|value| value <= 9_007_199_254_740_991)
            }
            Value::Array(values) => values.iter().all(safe),
            Value::Object(values) => values.values().all(safe),
            _ => true,
        }
    }
    if !safe(value) {
        return Err(Error::new(
            "precision",
            "Engine returned an integer outside JavaScript's safe range. The value was not rounded or accepted by the app.",
        ));
    }
    Ok(())
}

#[tauri::command]
pub async fn engine_download(backend: State<'_, Backend>, artifact_id: String) -> Result<Value> {
    let session = backend.session()?;
    download(&session, &artifact_id).await
}

async fn download(session: &Session, artifact_id: &str) -> Result<Value> {
    let metadata = session
        .json(Method::GET, &["artifacts", artifact_id], None, None)
        .await?;
    let descriptor = &metadata["artifact"];
    let size = descriptor["size"]
        .as_u64()
        .ok_or_else(|| Error::new("protocol", "Artifact is missing its byte size."))?;
    if size > 64 * 1024 * 1024 {
        return Err(Error::new("limit", "Artifact exceeds 64 MiB."));
    }
    let response = session
        .request(Method::GET, &["artifacts", artifact_id, "content"])?
        .timeout(Duration::from_secs(60))
        .send()
        .await
        .map_err(Error::transport)?;
    if !response.status().is_success() {
        return Err(Error::new("engine", "Artifact bytes are unavailable."));
    }
    let bytes = limited(response, 64 * 1024 * 1024).await?;
    let hash = format!("{:x}", Sha256::digest(&bytes));
    if bytes.len() as u64 != size || descriptor["sha256"] != hash {
        return Err(Error::new(
            "integrity",
            "Artifact size or SHA-256 checksum does not match engine metadata.",
        ));
    }
    Ok(json!({"artifact":descriptor,"content":STANDARD.encode(bytes)}))
}

#[tauri::command]
pub fn engine_events(backend: State<'_, Backend>, run_id: String) -> Result<Vec<Value>> {
    backend.store.events(&backend.session()?.key, &run_id)
}

#[tauri::command]
pub fn engine_unwatch(backend: State<'_, Backend>, run_id: String) -> Result<()> {
    if let Some(task) = backend
        .watches
        .lock()
        .map_err(Error::storage)?
        .remove(&run_id)
    {
        task.abort();
    }
    Ok(())
}

#[tauri::command]
pub fn engine_watch(
    backend: State<'_, Backend>,
    run_id: String,
    channel: Channel<Value>,
) -> Result<()> {
    let session = backend.session()?;
    session.url(&["runs", &run_id, "events"])?;
    let store = backend.store.clone();
    let id = run_id.clone();
    let task = tauri::async_runtime::spawn(async move {
        let mut delay = 1;
        loop {
            let result = stream(&session, &store, &id, &channel).await;
            let (message, code) = match result {
                Ok(()) => (
                    "Event stream ended. Reconnecting.".to_string(),
                    "reconnecting".to_string(),
                ),
                Err(e) => (e.message, e.code),
            };
            if channel.send(json!({"type":"connection","status":"reconnecting","message":message,"code":code})).is_err() {break;}
            tokio::time::sleep(Duration::from_secs(delay)).await;
            delay = (delay * 2).min(15);
        }
    });
    if let Some(old) = backend
        .watches
        .lock()
        .map_err(Error::storage)?
        .insert(run_id, task)
    {
        old.abort();
    }
    Ok(())
}

async fn stream(
    session: &Session,
    store: &store::Store,
    run: &str,
    channel: &Channel<Value>,
) -> Result<()> {
    let mut request = session
        .request(Method::GET, &["runs", run, "events"])?
        .header("Accept", "text/event-stream");
    if let Some(cursor) = store.cursor(&session.key, run)? {
        request = request.header("Last-Event-ID", cursor);
    }
    let response = tokio::time::timeout(Duration::from_secs(15), request.send())
        .await
        .map_err(|_| Error::new("transport", "Event connection timed out."))?
        .map_err(Error::transport)?;
    if !response.status().is_success()
        || !response.headers().get("content-type").is_some_and(|value| {
            value
                .to_str()
                .unwrap_or("")
                .starts_with("text/event-stream")
        })
    {
        return Err(Error::new(
            "protocol",
            "Engine did not return an event stream.",
        ));
    }
    channel
        .send(json!({"type":"connection","status":"connected"}))
        .map_err(Error::storage)?;
    let mut parser = sse::Parser::default();
    let mut bytes = response.bytes_stream();
    loop {
        let chunk = tokio::time::timeout(Duration::from_secs(45), bytes.next())
            .await
            .map_err(|_| Error::new("transport", "Event heartbeat timed out."))?;
        let Some(chunk) = chunk else { break };
        for frame in parser.push(&chunk.map_err(Error::transport)?)? {
            let event: Value = serde_json::from_str(&frame.data)
                .map_err(|_| Error::new("protocol", "Invalid event data."))?;
            if event["runId"] != run || event["id"] != frame.id {
                return Err(Error::new(
                    "protocol",
                    "Event identity does not match its stream.",
                ));
            }
            if store.event(&session.key, run, &frame.id, &event)? {
                channel
                    .send(json!({"type":"event","event":event}))
                    .map_err(Error::storage)?;
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests;

#[cfg(test)]
mod integration;

#[cfg(test)]
mod live;

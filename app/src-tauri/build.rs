fn main() {
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "open_package",
            "export_package",
            "export_file",
            "workspace_load",
            "workspace_save",
            "engine_connect",
            "engine_disconnect",
            "engine_call",
            "engine_cache",
            "engine_download",
            "engine_watch",
            "engine_unwatch",
            "engine_events",
        ]),
    ))
    .expect("failed to build Knotra desktop");
}

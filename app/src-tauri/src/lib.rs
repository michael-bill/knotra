mod backend;
mod package;
use backend::*;
use package::{OpenedPackage, PackageFile};
use tauri::Manager;
use tauri_plugin_dialog::DialogExt;
#[tauri::command]
async fn open_package(app: tauri::AppHandle) -> std::result::Result<Option<OpenedPackage>, String> {
    tauri::async_runtime::spawn_blocking(move || {
        app.dialog()
            .file()
            .add_filter("Knotra Pipeline", &["yaml", "yml"])
            .set_title("Open a pipeline entrypoint")
            .blocking_pick_file()
            .map(|f| package::read_package(&f.into_path().map_err(|e| e.to_string())?))
            .transpose()
    })
    .await
    .map_err(|e| e.to_string())?
}

#[tauri::command]
async fn export_package(
    app: tauri::AppHandle,
    entrypoint: String,
    source: String,
    files: Vec<PackageFile>,
    name: String,
) -> std::result::Result<Option<String>, String> {
    tauri::async_runtime::spawn_blocking(move || {
        app.dialog()
            .file()
            .set_title("Choose a parent folder for a new package")
            .blocking_pick_folder()
            .map(|f| {
                package::write_package(
                    &f.into_path().map_err(|e| e.to_string())?,
                    &entrypoint,
                    &source,
                    &files,
                    &name,
                )
                .map(|p| p.to_string_lossy().into_owned())
            })
            .transpose()
    })
    .await
    .map_err(|e| e.to_string())?
}

#[tauri::command]
async fn export_file(
    app: tauri::AppHandle,
    name: String,
    content: String,
) -> std::result::Result<(), String> {
    tauri::async_runtime::spawn_blocking(move || {
        if let Some(f) = app
            .dialog()
            .file()
            .set_title("Export file")
            .set_file_name(name)
            .blocking_save_file()
        {
            package::write_file(&f.into_path().map_err(|e| e.to_string())?, &content)?;
        }
        Ok(())
    })
    .await
    .map_err(|e| e.to_string())?
}

pub fn run() {
    tauri::Builder::default()
        .setup(|app| {
            app.manage(Backend::new(app)?);
            Ok(())
        })
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_opener::init())
        .invoke_handler(tauri::generate_handler![
            open_package,
            export_package,
            export_file,
            workspace_load,
            workspace_save,
            engine_connect,
            engine_disconnect,
            engine_call,
            engine_cache,
            engine_download,
            engine_watch,
            engine_unwatch,
            engine_events
        ])
        .run(tauri::generate_context!())
        .expect("error while running Knotra");
}

use base64::{engine::general_purpose::STANDARD, Engine};
use serde::{Deserialize, Serialize};
use std::{
    collections::HashSet,
    fs::{self, File, OpenOptions},
    io::{Read, Write},
    path::{Path, PathBuf},
};
use unicode_normalization::UnicodeNormalization;
const MAX_PACKAGE: usize = 64 * 1024 * 1024;
#[derive(Clone, Serialize, Deserialize)]
pub struct PackageFile {
    pub path: String,
    pub content: String,
}
#[derive(Serialize)]
pub struct OpenedPackage {
    pub entrypoint: String,
    pub source: String,
    pub files: Vec<PackageFile>,
}
fn err(e: impl std::fmt::Display) -> String {
    e.to_string()
}
pub fn valid_path(path: &str) -> bool {
    !path.is_empty()
        && path.chars().count() <= 1024
        && !path.chars().any(|c| c.is_control() || c == '\\')
        && !path
            .as_bytes()
            .get(1)
            .is_some_and(|c| *c == b':' && path.as_bytes()[0].is_ascii_alphabetic())
        && path
            .split('/')
            .all(|p| !p.is_empty() && p != "." && p != "..")
        && path.nfc().collect::<String>() == path
}
#[cfg(unix)]
fn read_relative(root: &Path, path: &str) -> Result<Vec<u8>, String> {
    use rustix::fs::{openat, Mode, OFlags};
    use std::os::unix::fs::MetadataExt;
    let mut dir = File::open(root).map_err(err)?;
    let parts: Vec<_> = path.split('/').collect();
    for part in &parts[..parts.len() - 1] {
        dir = File::from(
            openat(
                &dir,
                *part,
                OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::empty(),
            )
            .map_err(err)?,
        );
    }
    let file = File::from(
        openat(
            &dir,
            parts[parts.len() - 1],
            OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
            Mode::empty(),
        )
        .map_err(err)?,
    );
    let meta = file.metadata().map_err(err)?;
    if !meta.is_file() || meta.nlink() != 1 {
        return Err(format!("Not a regular single-link file: {path}"));
    }
    read_limited(file)
}
#[cfg(not(unix))]
fn read_relative(root: &Path, path: &str) -> Result<Vec<u8>, String> {
    let mut target = root.to_path_buf();
    for part in path.split('/') {
        target.push(part);
        if fs::symlink_metadata(&target)
            .map_err(err)?
            .file_type()
            .is_symlink()
        {
            return Err("Package symlink rejected".into());
        }
    }
    let actual = target.canonicalize().map_err(err)?;
    if !actual.starts_with(root) || !actual.is_file() {
        return Err("Invalid package file".into());
    }
    read_limited(File::open(actual).map_err(err)?)
}
fn read_limited(file: File) -> Result<Vec<u8>, String> {
    let mut bytes = Vec::new();
    file.take(MAX_PACKAGE as u64 + 1)
        .read_to_end(&mut bytes)
        .map_err(err)?;
    if bytes.len() > MAX_PACKAGE {
        return Err("Package file exceeds 64 MiB".into());
    }
    Ok(bytes)
}
fn imports(value: &serde_yaml::Value, found: &mut Vec<String>) {
    match value {
        serde_yaml::Value::Mapping(map) => {
            if value["type"].as_str() == Some("pipeline") {
                if let Some(path) = value["pipeline"]["file"].as_str() {
                    found.push(path.into());
                }
            }
            for (key, item) in map {
                if key.as_str() != Some("value") {
                    imports(item, found);
                }
            }
        }
        serde_yaml::Value::Sequence(items) => {
            for item in items {
                imports(item, found);
            }
        }
        _ => {}
    }
}
struct Loader<'a> {
    root: &'a Path,
    files: Vec<PackageFile>,
    names: HashSet<String>,
    loaded: HashSet<String>,
    active: HashSet<String>,
    total: usize,
}
impl Loader<'_> {
    fn load(&mut self, path: &str, bytes: &[u8], depth: usize) -> Result<(), String> {
        if depth > 32 || !self.active.insert(path.into()) {
            return Err("Recursive import or depth greater than 32".into());
        }
        if bytes.len() > 8 * 1024 * 1024 {
            return Err("Pipeline exceeds 8 MiB".into());
        }
        let value: serde_yaml::Value = serde_yaml::from_slice(bytes).map_err(err)?;
        if value["kind"].as_str() != Some("Pipeline") {
            return Err("Entrypoint and imports must be Pipeline documents".into());
        }
        let declared: Vec<String> = match &value["spec"]["files"] {
            serde_yaml::Value::Null => vec![],
            serde_yaml::Value::Sequence(items) => items
                .iter()
                .map(|v| {
                    v.as_str()
                        .map(str::to_owned)
                        .ok_or("spec.files must contain strings".to_string())
                })
                .collect::<Result<_, _>>()?,
            _ => return Err("spec.files must be a list".into()),
        };
        let mut unique = HashSet::new();
        for file in &declared {
            if !valid_path(file) || !unique.insert(file) {
                return Err(format!("Invalid or repeated package path: {file}"));
            }
            if !self.files.iter().any(|f| f.path == *file)
                && !self.loaded.contains(file)
                && !self.active.contains(file)
            {
                if !self.names.insert(file.to_ascii_lowercase()) {
                    return Err(format!("Case-folded path conflict: {file}"));
                }
                let bytes = read_relative(self.root, file)?;
                self.total += bytes.len();
                if self.total > MAX_PACKAGE || self.files.len() >= 511 {
                    return Err("Package exceeds 64 MiB or 512 files".into());
                }
                self.files.push(PackageFile {
                    path: file.clone(),
                    content: STANDARD.encode(bytes),
                });
            }
        }
        let mut children = Vec::new();
        imports(&value["spec"], &mut children);
        for child in children {
            if !declared.contains(&child) {
                return Err(format!("Import not declared in spec.files: {child}"));
            }
            if self.active.contains(&child) {
                return Err("Recursive import".into());
            }
            if self.loaded.contains(&child) {
                continue;
            }
            let file = self
                .files
                .iter()
                .find(|f| f.path == child)
                .ok_or("Missing imported file")?;
            let bytes = STANDARD.decode(&file.content).map_err(err)?;
            self.load(&child, &bytes, depth + 1)?;
        }
        self.active.remove(path);
        self.loaded.insert(path.into());
        Ok(())
    }
}
pub fn read_package(selected: &Path) -> Result<OpenedPackage, String> {
    let root = selected
        .parent()
        .ok_or("Missing package root")?
        .canonicalize()
        .map_err(err)?;
    let entrypoint = selected
        .file_name()
        .and_then(|s| s.to_str())
        .ok_or("Filename must be UTF-8")?
        .to_string();
    if !valid_path(&entrypoint) {
        return Err("Invalid entrypoint filename".into());
    }
    let bytes = read_relative(&root, &entrypoint)?;
    let source = String::from_utf8(bytes.clone()).map_err(err)?;
    let mut loader = Loader {
        root: &root,
        files: vec![],
        names: HashSet::from([entrypoint.to_ascii_lowercase()]),
        loaded: HashSet::new(),
        active: HashSet::new(),
        total: bytes.len(),
    };
    loader.load(&entrypoint, &bytes, 0)?;
    loader
        .files
        .sort_by(|a, b| a.path.as_bytes().cmp(b.path.as_bytes()));
    Ok(OpenedPackage {
        entrypoint,
        source,
        files: loader.files,
    })
}
pub fn write_package(
    parent: &Path,
    entrypoint: &str,
    source: &str,
    files: &[PackageFile],
    name: &str,
) -> Result<PathBuf, String> {
    if !valid_path(entrypoint) || !valid_path(name) || name.contains('/') {
        return Err("Invalid export name or entrypoint".into());
    }
    let mut paths = HashSet::new();
    let mut decoded = vec![];
    let mut size = 0;
    for file in std::iter::once(PackageFile {
        path: entrypoint.into(),
        content: STANDARD.encode(source),
    })
    .chain(files.iter().cloned())
    {
        if !valid_path(&file.path) || !paths.insert(file.path.to_ascii_lowercase()) {
            return Err("Invalid or conflicting export path".into());
        }
        let bytes = STANDARD.decode(file.content).map_err(err)?;
        size += bytes.len();
        if size > MAX_PACKAGE || paths.len() > 512 {
            return Err("Package exceeds limits".into());
        }
        decoded.push((file.path, bytes));
    }
    for path in &paths {
        let parts: Vec<_> = path.split('/').collect();
        for i in 1..parts.len() {
            if paths.contains(&parts[..i].join("/")) {
                return Err("File/directory conflict".into());
            }
        }
    }
    let timestamp = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_err(err)?
        .as_millis();
    let destination = parent.join(format!("{name}-{timestamp}"));
    fs::create_dir(&destination).map_err(err)?;
    let result = (|| {
        for (path, bytes) in decoded {
            let file = destination.join(path);
            fs::create_dir_all(file.parent().ok_or("Missing parent")?).map_err(err)?;
            OpenOptions::new()
                .write(true)
                .create_new(true)
                .open(file)
                .map_err(err)?
                .write_all(&bytes)
                .map_err(err)?;
        }
        Ok(destination.clone())
    })();
    if result.is_err() {
        let _ = fs::remove_dir_all(&destination);
    }
    result
}
pub fn write_file(path: &Path, content: &str) -> Result<(), String> {
    let bytes = STANDARD.decode(content).map_err(err)?;
    if bytes.len() > MAX_PACKAGE {
        return Err("File exceeds limit".into());
    }
    if let Ok(meta) = fs::symlink_metadata(path) {
        if !meta.is_file() || meta.file_type().is_symlink() {
            return Err("Destination must be a regular file".into());
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::MetadataExt;
            if meta.nlink() != 1 {
                return Err("Hard-link destination rejected".into());
            }
        }
    }
    let mut options = OpenOptions::new();
    options.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.custom_flags(rustix::fs::OFlags::NOFOLLOW.bits() as i32);
    }
    options
        .open(path)
        .map_err(err)?
        .write_all(&bytes)
        .map_err(err)
}
#[cfg(test)]
mod tests {
    use super::*;
    fn scratch() -> PathBuf {
        static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
        let p = std::env::temp_dir().join(format!(
            "knotra-{}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos(),
            NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
        ));
        fs::create_dir(&p).unwrap();
        p
    }
    #[test]
    fn rejects_bad_paths() {
        for p in ["../x", "/x", "a//b", "a\\b", "C:x", "a/./b", "a\nx"] {
            assert!(!valid_path(p));
        }
        assert!(valid_path("children/writer.yaml"));
    }
    #[test]
    fn preserves_package_bytes() {
        let root = scratch();
        let source = "kind: Pipeline\nspec:\n  files: [prompt.txt]\n";
        let files = vec![PackageFile {
            path: "prompt.txt".into(),
            content: STANDARD.encode([0, 1, 255]),
        }];
        let output = write_package(&root, "workflow.yaml", source, &files, "test").unwrap();
        let read = read_package(&output.join("workflow.yaml")).unwrap();
        assert_eq!(read.source, source);
        assert_eq!(read.files[0].content, files[0].content);
        fs::remove_dir_all(root).unwrap();
    }
    #[test]
    fn rejects_conflicting_exports() {
        let root = scratch();
        let files = vec![
            PackageFile {
                path: "A.txt".into(),
                content: STANDARD.encode("a"),
            },
            PackageFile {
                path: "a.txt".into(),
                content: STANDARD.encode("b"),
            },
        ];
        assert!(write_package(&root, "pipeline.yaml", "test", &files, "test").is_err());
        fs::remove_dir_all(root).unwrap();
    }
    #[cfg(unix)]
    #[test]
    fn rejects_links() {
        let root = scratch();
        fs::write(root.join("secret"), "private").unwrap();
        std::os::unix::fs::symlink(root.join("secret"), root.join("link")).unwrap();
        assert!(read_relative(&root, "link").is_err());
        fs::hard_link(root.join("secret"), root.join("hard")).unwrap();
        assert!(read_relative(&root, "hard").is_err());
        fs::remove_dir_all(root).unwrap();
    }
}

use super::{Error, Result};
use rusqlite::{params, Connection, OptionalExtension};
use serde_json::{json, Value};
use std::{path::Path, sync::Mutex};

/// Local drafts and read models only. The engine owns execution state.
pub struct Store(Mutex<Connection>);
impl Store {
    pub fn open(path: &Path) -> Result<Self> {
        let db = Connection::open(path).map_err(Error::storage)?;
        let version: u32 = db
            .query_row("PRAGMA user_version", [], |row| row.get(0))
            .map_err(Error::storage)?;
        if version > 1 {
            return Err(Error::new(
                "storage",
                "This workspace was created by a newer app. Its database has not been changed.",
            ));
        }
        db.busy_timeout(std::time::Duration::from_secs(5))
            .map_err(Error::storage)?;
        db.execute_batch("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
          CREATE TABLE IF NOT EXISTS local_state (id INTEGER PRIMARY KEY CHECK(id=1), json TEXT NOT NULL);
          CREATE TABLE IF NOT EXISTS cache (engine TEXT NOT NULL, key TEXT NOT NULL, json TEXT NOT NULL, PRIMARY KEY(engine,key));
          CREATE TABLE IF NOT EXISTS events (engine TEXT NOT NULL, run TEXT NOT NULL, id TEXT NOT NULL, json TEXT NOT NULL, PRIMARY KEY(engine,run,id));
          CREATE TABLE IF NOT EXISTS cursors (engine TEXT NOT NULL, run TEXT NOT NULL, id TEXT NOT NULL, PRIMARY KEY(engine,run));
          CREATE TABLE IF NOT EXISTS operations (engine TEXT NOT NULL, id TEXT NOT NULL, request TEXT NOT NULL, response TEXT, PRIMARY KEY(engine,id));
          PRAGMA user_version=1;").map_err(Error::storage)?;
        Ok(Self(Mutex::new(db)))
    }
    fn db(&self) -> Result<std::sync::MutexGuard<'_, Connection>> {
        self.0
            .lock()
            .map_err(|_| Error::new("storage", "Workspace database is unavailable."))
    }
    pub fn load(&self) -> Result<Option<Value>> {
        let text: Option<String> = self
            .db()?
            .query_row("SELECT json FROM local_state WHERE id=1", [], |row| {
                row.get(0)
            })
            .optional()
            .map_err(Error::storage)?;
        text.map(|text| serde_json::from_str(&text).map_err(Error::storage))
            .transpose()
    }
    pub fn save(&self, value: &Value) -> Result<()> {
        if value["version"] != 1 || !value["workspaces"].is_array() || !value["runs"].is_array() {
            return Err(Error::new("input", "Unsupported workspace backup."));
        }
        let text = serde_json::to_string(value).map_err(Error::storage)?;
        if text.len() > 96 * 1024 * 1024 {
            return Err(Error::new(
                "limit",
                "Workspace exceeds 96 MiB. Export old runs or split packages.",
            ));
        }
        self.db()?.execute("INSERT INTO local_state VALUES(1,?1) ON CONFLICT(id) DO UPDATE SET json=excluded.json", [text]).map_err(Error::storage)?;
        Ok(())
    }
    pub fn cache(&self, engine: &str, key: &str, value: &Value) -> Result<()> {
        self.db()?.execute("INSERT INTO cache VALUES(?1,?2,?3) ON CONFLICT(engine,key) DO UPDATE SET json=excluded.json", params![engine,key,value.to_string()]).map_err(Error::storage)?;
        Ok(())
    }
    pub fn snapshot(&self, engine: &str) -> Result<Value> {
        let db = self.db()?;
        let mut cache = serde_json::Map::new();
        let mut statement = db
            .prepare("SELECT key,json FROM cache WHERE engine=?1")
            .map_err(Error::storage)?;
        let rows = statement
            .query_map([engine], |row| {
                Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?))
            })
            .map_err(Error::storage)?;
        for row in rows {
            let (key, value) = row.map_err(Error::storage)?;
            cache.insert(key, serde_json::from_str(&value).map_err(Error::storage)?);
        }
        let mut statement = db.prepare("SELECT id,request FROM operations WHERE engine=?1 AND response IS NULL ORDER BY rowid").map_err(Error::storage)?;
        let pending = statement.query_map([engine], |row| Ok((row.get::<_,String>(0)?,row.get::<_,String>(1)?))).map_err(Error::storage)?.map(|row| { let (id, request) = row.map_err(Error::storage)?; Ok(json!({"id":id,"request":serde_json::from_str::<Value>(&request).map_err(Error::storage)?})) }).collect::<Result<Vec<_>>>()?;
        Ok(json!({"cache":cache,"pending":pending}))
    }
    // Record BEFORE sending. A lost response must reuse the same key and exact body after restart.
    pub fn prepare(&self, engine: &str, id: &str, request: &Value) -> Result<Option<Value>> {
        let db = self.db()?;
        let old: Option<(String, Option<String>)> = db
            .query_row(
                "SELECT request,response FROM operations WHERE engine=?1 AND id=?2",
                params![engine, id],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .optional()
            .map_err(Error::storage)?;
        if let Some((old, result)) = old {
            if serde_json::from_str::<Value>(&old).map_err(Error::storage)? != *request {
                return Err(Error::new(
                    "conflict",
                    "Operation ID already belongs to a different command.",
                ));
            }
            return result
                .map(|text| serde_json::from_str(&text).map_err(Error::storage))
                .transpose();
        }
        db.execute(
            "INSERT INTO operations(engine,id,request) VALUES(?1,?2,?3)",
            params![engine, id, request.to_string()],
        )
        .map_err(Error::storage)?;
        Ok(None)
    }
    pub fn complete(&self, engine: &str, id: &str, response: &Value) -> Result<()> {
        self.db()?
            .execute(
                "UPDATE operations SET response=?3 WHERE engine=?1 AND id=?2",
                params![engine, id, response.to_string()],
            )
            .map_err(Error::storage)?;
        Ok(())
    }
    pub fn cursor(&self, engine: &str, run: &str) -> Result<Option<String>> {
        self.db()?
            .query_row(
                "SELECT id FROM cursors WHERE engine=?1 AND run=?2",
                params![engine, run],
                |row| row.get(0),
            )
            .optional()
            .map_err(Error::storage)
    }
    pub fn event(&self, engine: &str, run: &str, id: &str, event: &Value) -> Result<bool> {
        let mut db = self.db()?;
        let tx = db.transaction().map_err(Error::storage)?;
        let fresh = tx
            .execute(
                "INSERT OR IGNORE INTO events VALUES(?1,?2,?3,?4)",
                params![engine, run, id, event.to_string()],
            )
            .map_err(Error::storage)?
            > 0;
        if fresh {
            tx.execute("INSERT INTO cursors VALUES(?1,?2,?3) ON CONFLICT(engine,run) DO UPDATE SET id=excluded.id",params![engine,run,id]).map_err(Error::storage)?;
        }
        tx.commit().map_err(Error::storage)?;
        Ok(fresh)
    }
    pub fn events(&self, engine: &str, run: &str) -> Result<Vec<Value>> {
        let db = self.db()?;
        let mut statement=db.prepare("SELECT json FROM (SELECT rowid,json FROM events WHERE engine=?1 AND run=?2 ORDER BY rowid DESC LIMIT 1000) ORDER BY rowid").map_err(Error::storage)?;
        let result = statement
            .query_map(params![engine, run], |row| row.get::<_, String>(0))
            .map_err(Error::storage)?
            .map(|row| serde_json::from_str(&row.map_err(Error::storage)?).map_err(Error::storage))
            .collect();
        result
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    fn store() -> Store {
        Store::open(Path::new(":memory:")).unwrap()
    }
    #[test]
    fn operation_recovery_rejects_changed_payload() {
        let s = store();
        let command = json!({"op":"respond","requestId":"r1","outputs":{"x":1}});
        assert!(s.prepare("a", "op1", &command).unwrap().is_none());
        assert!(s.prepare("a", "op1", &json!({"requestId":"r2"})).is_err());
        s.complete("a", "op1", &json!({"accepted":true})).unwrap();
        assert_eq!(
            s.prepare("a", "op1", &command).unwrap(),
            Some(json!({"accepted":true}))
        );
        assert!(s.prepare("b", "op1", &command).unwrap().is_none());
    }
    #[test]
    fn replay_never_duplicates_or_regresses_cursor() {
        let s = store();
        assert!(s.event("a", "run", "1", &json!({"message":"one"})).unwrap());
        assert!(s.event("a", "run", "2", &json!({"message":"two"})).unwrap());
        assert!(!s.event("a", "run", "1", &json!({"message":"one"})).unwrap());
        assert_eq!(s.cursor("a", "run").unwrap(), Some("2".into()));
        assert_eq!(s.events("a", "run").unwrap().len(), 2);
        assert!(s.events("b", "run").unwrap().is_empty());
    }
    #[test]
    fn survives_restart_without_touching_engine_state() {
        let path = std::env::temp_dir().join(format!(
            "knotra-store-{}-{}.sqlite",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        {
            let s = Store::open(&path).unwrap();
            s.save(&json!({"version":1,"workspaces":[],"runs":[]}))
                .unwrap();
            s.prepare("a", "op", &json!({"op":"cancel"})).unwrap();
            s.event("a", "r", "e1", &json!({"id":"e1"})).unwrap();
        }
        {
            let s = Store::open(&path).unwrap();
            assert!(s.load().unwrap().is_some());
            assert_eq!(
                s.snapshot("a").unwrap()["pending"]
                    .as_array()
                    .unwrap()
                    .len(),
                1
            );
            assert_eq!(s.cursor("a", "r").unwrap().unwrap(), "e1");
        }
        std::fs::remove_file(path).unwrap();
    }
}

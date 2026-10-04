use super::{Call, Error, Result};
use serde_json::Value;

// Protect cached read models using the contract's structural constraints.
// This is intentionally not a complete JSON Schema validator: formats and file
// content encodings are still validated by the frontend/execution boundary.
pub(super) fn check_read_shape(call: &Call, value: &Value) -> Result<()> {
    static SCHEMAS: std::sync::OnceLock<Value> = std::sync::OnceLock::new();
    let schemas = SCHEMAS.get_or_init(|| {
        let spec: Value =
            serde_json::from_str(include_str!("../../../../docs/api/desktop-v1.openapi.json"))
                .expect("bundled desktop contract is valid JSON");
        spec["components"]["schemas"].clone()
    });
    let model = match call {
        Call::Info => Some(("Info", value)),
        Call::Run { .. } => Some(("Run", &value["run"])),
        Call::Definition { definition_id } => {
            if value["definition"]["id"] != *definition_id {
                return Err(Error::new(
                    "protocol",
                    "Definition identity does not match the request.",
                ));
            }
            Some(("Definition", &value["definition"]))
        }
        Call::Artifact { artifact_id } => {
            if value["artifact"]["id"] != *artifact_id {
                return Err(Error::new(
                    "protocol",
                    "Artifact identity does not match the request.",
                ));
            }
            Some(("Artifact", &value["artifact"]))
        }
        Call::Runs { .. } => Some(("RunPage", value)),
        Call::Requests { .. } => Some(("HumanRequestPage", value)),
        Call::Artifacts { .. } => Some(("ArtifactPage", value)),
        Call::Definitions | Call::Profiles | Call::Resources => {
            let name = match call {
                Call::Definitions => "Definition",
                Call::Profiles => "Profile",
                _ => "Resource",
            };
            if !value["items"].as_array().is_some_and(|items| {
                items
                    .iter()
                    .all(|item| read_shape(&schemas[name], item, schemas))
            }) {
                return Err(Error::new(
                    "protocol",
                    "Engine returned an incompatible catalog. The last valid cache is preserved.",
                ));
            }
            None
        }
        _ => None,
    };
    if model.is_some_and(|(name, item)| !read_shape(&schemas[name], item, schemas)) {
        return Err(Error::new(
            "protocol",
            "Engine returned an incompatible read model. The last valid cache is preserved.",
        ));
    }
    Ok(())
}

fn read_shape(schema: &Value, value: &Value, schemas: &Value) -> bool {
    if let Some(valid) = schema.as_bool() {
        return valid;
    }
    if let Some(reference) = schema["$ref"].as_str() {
        return reference
            .strip_prefix("#/components/schemas/")
            .and_then(|name| schemas.get(name))
            .is_some_and(|target| read_shape(target, value, schemas));
    }
    let has_type = |kind: &str| match kind {
        "object" => value.is_object(),
        "array" => value.is_array(),
        "string" => value.is_string(),
        "boolean" => value.is_boolean(),
        "null" => value.is_null(),
        "number" => value.is_number(),
        "integer" => value
            .as_f64()
            .is_some_and(|number| number.is_finite() && number.fract() == 0.0),
        _ => false,
    };
    if schema["type"].as_str().is_some_and(|kind| !has_type(kind))
        || schema["type"]
            .as_array()
            .is_some_and(|kinds| !kinds.iter().any(|kind| kind.as_str().is_some_and(has_type)))
        || schema
            .get("const")
            .is_some_and(|expected| expected != value)
        || schema["enum"]
            .as_array()
            .is_some_and(|choices| !choices.contains(value))
        || schema["oneOf"].as_array().is_some_and(|choices| {
            choices
                .iter()
                .filter(|choice| read_shape(choice, value, schemas))
                .count()
                != 1
        })
        || schema["required"].as_array().is_some_and(|keys| {
            keys.iter()
                .any(|key| key.as_str().is_none_or(|key| value.get(key).is_none()))
        })
    {
        return false;
    }
    if let Some(properties) = value.as_object() {
        for (key, item) in properties {
            if let Some(property) = schema["properties"].get(key) {
                if !read_shape(property, item, schemas) {
                    return false;
                }
            } else if let Some(extra) = schema.get("additionalProperties") {
                if !read_shape(extra, item, schemas) {
                    return false;
                }
            }
        }
    }
    if let Some(items) = value.as_array() {
        if schema
            .get("items")
            .is_some_and(|item| items.iter().any(|value| !read_shape(item, value, schemas)))
            || schema["minItems"]
                .as_u64()
                .is_some_and(|min| (items.len() as u64) < min)
            || schema["maxItems"]
                .as_u64()
                .is_some_and(|max| (items.len() as u64) > max)
        {
            return false;
        }
    }
    if let Some(text) = value.as_str() {
        if schema["minLength"]
            .as_u64()
            .is_some_and(|min| (text.chars().count() as u64) < min)
            || schema["maxLength"]
                .as_u64()
                .is_some_and(|max| (text.chars().count() as u64) > max)
        {
            return false;
        }
        if schema["pattern"] == "^[a-f0-9]{64}$"
            && (text.len() != 64
                || !text
                    .bytes()
                    .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte)))
        {
            return false;
        }
    }
    if let Some(number) = value.as_f64() {
        if schema["minimum"].as_f64().is_some_and(|min| number < min)
            || schema["maximum"].as_f64().is_some_and(|max| number > max)
        {
            return false;
        }
    }
    true
}

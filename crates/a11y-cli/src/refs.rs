//! Persistent ref index for the desktop accessibility tree. A `snapshot`
//! invocation writes the latest mapping to `/tmp/a11y-cli-refs.json`; later
//! `click`/`type`/`fill` invocations read the same file to resolve `eN` back
//! into a `(bus_name, object_path)` pair.

use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
use atspi::object_ref::{ObjectRef, ObjectRefOwned};
use atspi::zbus::names::UniqueName;
use atspi::zbus::zvariant::ObjectPath;
use serde::{Deserialize, Serialize};

const DEFAULT_REFS_PATH: &str = "/tmp/a11y-cli-refs.json";

/// One row in the persisted refs file.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RefEntry {
    pub ref_id: String,
    pub bus_name: String,
    pub object_path: String,
    pub role: String,
    pub name: String,
    pub x: i32,
    pub y: i32,
    pub width: i32,
    pub height: i32,
}

impl RefEntry {
    /// Bounding box center, used as the RFB fallback target.
    pub fn center(&self) -> (i32, i32) {
        let cx = self.x.saturating_add(self.width / 2);
        let cy = self.y.saturating_add(self.height / 2);
        (cx, cy)
    }

    /// Rebuild an `ObjectRefOwned` so we can construct proxies again.
    pub fn to_object_ref(&self) -> Result<ObjectRefOwned> {
        let name = UniqueName::try_from(self.bus_name.clone())
            .with_context(|| format!("invalid bus name {:?}", self.bus_name))?;
        let path = ObjectPath::try_from(self.object_path.clone())
            .with_context(|| format!("invalid object path {:?}", self.object_path))?;
        Ok(ObjectRef::new_owned(name, path))
    }
}

#[derive(Debug, Serialize, Deserialize, Default)]
pub struct RefIndex {
    pub entries: Vec<RefEntry>,
}

fn refs_path() -> PathBuf {
    std::env::var("A11Y_CLI_REFS")
        .map(PathBuf::from)
        .unwrap_or_else(|_| PathBuf::from(DEFAULT_REFS_PATH))
}

pub fn write(entries: &[RefEntry]) -> Result<PathBuf> {
    let target = refs_path();
    write_to(&target, entries)?;
    Ok(target)
}

fn write_to(path: &Path, entries: &[RefEntry]) -> Result<()> {
    let index = RefIndex {
        entries: entries.to_vec(),
    };
    let data = serde_json::to_vec_pretty(&index).context("serialize refs index")?;
    let parent = path.parent();
    if let Some(parent) = parent {
        if !parent.as_os_str().is_empty() {
            std::fs::create_dir_all(parent)
                .with_context(|| format!("create parent directory for {}", path.display()))?;
        }
    }
    std::fs::write(path, data).with_context(|| format!("write refs index to {}", path.display()))
}

pub fn lookup(ref_id: &str) -> Result<RefEntry> {
    let target = refs_path();
    let data = std::fs::read(&target)
        .with_context(|| format!("read refs index at {}", target.display()))?;
    let index: RefIndex = serde_json::from_slice(&data).context("parse refs index")?;
    let normalized = normalize(ref_id);
    for entry in index.entries {
        if entry.ref_id == normalized {
            return Ok(entry);
        }
    }
    anyhow::bail!(
        "ref {ref_id} is not present in {} (run `a11y-cli snapshot` first)",
        target.display()
    )
}

/// Normalize ref ids like `e3`, `E03`, or `ref=e3` to the canonical `eN` form.
pub fn normalize(ref_id: &str) -> String {
    let trimmed = ref_id.trim();
    let lower = trimmed.to_ascii_lowercase();
    let without_prefix = lower.strip_prefix("ref=").unwrap_or(&lower);
    let without_e = without_prefix.strip_prefix('e').unwrap_or(without_prefix);
    match without_e.parse::<u32>() {
        Ok(idx) if idx > 0 => format!("e{idx}"),
        _ => trimmed.to_string(),
    }
}

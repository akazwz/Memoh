//! Implements `click`, `type`, and `fill` actions in terms of AT-SPI. Whenever
//! the AT-SPI invocation fails we still emit `fallback: { x, y }` so the Go
//! caller can replay the action via RFB pointer/key events.

use anyhow::Result;
use serde::Serialize;

use crate::connection;
use crate::refs::{self, RefEntry};

#[derive(Serialize)]
struct ActionResult {
    ok: bool,
    action: &'static str,
    #[serde(rename = "ref")]
    ref_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    detail: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    fallback: Option<Fallback>,
}

#[derive(Serialize)]
struct Fallback {
    x: i32,
    y: i32,
}

impl ActionResult {
    fn success(action: &'static str, entry: &RefEntry, detail: impl Into<String>) -> Self {
        Self {
            ok: true,
            action,
            ref_id: entry.ref_id.clone(),
            detail: Some(detail.into()),
            error: None,
            fallback: None,
        }
    }

    fn failure(action: &'static str, entry: &RefEntry, error: impl Into<String>) -> Self {
        let (x, y) = entry.center();
        Self {
            ok: false,
            action,
            ref_id: entry.ref_id.clone(),
            detail: None,
            error: Some(error.into()),
            fallback: Some(Fallback { x, y }),
        }
    }

    fn emit(&self) -> Result<()> {
        println!("{}", serde_json::to_string(self)?);
        Ok(())
    }
}

pub async fn click(ref_id: &str) -> Result<()> {
    let entry = refs::lookup(ref_id)?;
    let outcome = try_click(&entry).await;
    match outcome {
        Ok(detail) => ActionResult::success("click", &entry, detail).emit(),
        Err(err) => ActionResult::failure("click", &entry, format!("{err:#}")).emit(),
    }
}

async fn try_click(entry: &RefEntry) -> Result<String> {
    let conn = connection::open().await?;
    let object = entry.to_object_ref()?;
    let accessible = connection::accessible_for(&conn, &object).await?;
    let actions = connection::action_for(&conn, &accessible).await?;

    let descriptors = actions.get_actions().await?;
    if descriptors.is_empty() {
        anyhow::bail!("the target element does not expose any AT-SPI actions");
    }
    let preferred = preferred_action_index(&descriptors);
    let success = actions.do_action(preferred as i32).await?;
    if !success {
        anyhow::bail!("AT-SPI reported the action did not run");
    }
    let label = descriptors
        .get(preferred)
        .map(|action| action.name.to_string())
        .unwrap_or_else(|| "click".to_string());
    Ok(label)
}

fn preferred_action_index(descriptors: &[atspi::Action]) -> usize {
    for (idx, action) in descriptors.iter().enumerate() {
        let lower = action.name.to_ascii_lowercase();
        if lower.contains("click") || lower.contains("press") || lower.contains("activate") {
            return idx;
        }
    }
    0
}

pub async fn type_text(ref_id: &str, text: &str) -> Result<()> {
    let entry = refs::lookup(ref_id)?;
    let outcome = try_type(&entry, text).await;
    match outcome {
        Ok(_) => ActionResult::success(
            "type",
            &entry,
            format!("inserted {} chars", text.chars().count()),
        )
        .emit(),
        Err(err) => ActionResult::failure("type", &entry, format!("{err:#}")).emit(),
    }
}

async fn try_type(entry: &RefEntry, text: &str) -> Result<()> {
    let conn = connection::open().await?;
    let object = entry.to_object_ref()?;
    let accessible = connection::accessible_for(&conn, &object).await?;
    let editable = connection::editable_for(&conn, &accessible).await?;
    let text_proxy = connection::text_for(&conn, &accessible).await?;
    let caret = text_proxy.caret_offset().await.unwrap_or(-1);
    let position = if caret < 0 { 0 } else { caret };
    let length = i32::try_from(text.chars().count()).unwrap_or(i32::MAX);
    let inserted = editable.insert_text(position, text, length).await?;
    if !inserted {
        anyhow::bail!("editable text widget refused to insert");
    }
    Ok(())
}

pub async fn fill_text(ref_id: &str, text: &str) -> Result<()> {
    let entry = refs::lookup(ref_id)?;
    let outcome = try_fill(&entry, text).await;
    match outcome {
        Ok(_) => ActionResult::success(
            "fill",
            &entry,
            format!("set {} chars", text.chars().count()),
        )
        .emit(),
        Err(err) => ActionResult::failure("fill", &entry, format!("{err:#}")).emit(),
    }
}

async fn try_fill(entry: &RefEntry, text: &str) -> Result<()> {
    let conn = connection::open().await?;
    let object = entry.to_object_ref()?;
    let accessible = connection::accessible_for(&conn, &object).await?;
    let editable = connection::editable_for(&conn, &accessible).await?;
    let replaced = editable.set_text_contents(text).await?;
    if !replaced {
        anyhow::bail!("editable text widget refused to replace contents");
    }
    Ok(())
}

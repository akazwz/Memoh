//! `a11y-cli snapshot` subcommand — walk the desktop accessibility tree
//! iteratively, assign `eN` refs to visible nodes, persist the index, and
//! emit both human-readable lines and structured JSON with diagnostics so the
//! Go caller can tell whether an empty list means "no UI" or "everything
//! filtered out".

use anyhow::Result;
use atspi::object_ref::ObjectRefOwned;
use atspi::proxy::accessible::AccessibleProxy;
use atspi::{AccessibilityConnection, CoordType, Role, State, StateSet};
use serde::Serialize;

use crate::connection;
use crate::refs::{self, RefEntry};

/// Maximum number of applications we descend into before giving up. The
/// registry sits in front of every connected accessibility application; a
/// reasonable upper bound keeps the walk responsive on chatty desktops.
const MAX_APPS: usize = 32;
/// Maximum nodes inspected across the entire walk. AT-SPI trees can balloon
/// (LibreOffice Calc exposes ~2^31 cells), so we cap aggressively.
const MAX_VISITS: usize = 8000;

#[derive(Serialize)]
struct SnapshotItem {
    #[serde(rename = "ref")]
    ref_id: String,
    role: String,
    name: String,
    x: i32,
    y: i32,
    width: i32,
    height: i32,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    states: Vec<&'static str>,
}

#[derive(Serialize, Default)]
struct Diagnostics {
    apps: usize,
    visited: usize,
    accepted: usize,
    skipped_state: usize,
    skipped_role: usize,
    skipped_geometry: usize,
    errors: usize,
    #[serde(skip_serializing_if = "Option::is_none")]
    bus_address: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    display: Option<String>,
}

#[derive(Serialize)]
struct SnapshotOutput {
    ok: bool,
    truncated: bool,
    items: Vec<SnapshotItem>,
    lines: Vec<String>,
    refs_path: String,
    diagnostics: Diagnostics,
}

pub async fn run(limit: usize) -> Result<()> {
    let conn = connection::open().await?;
    let (entries, truncated, diagnostics) = collect(&conn, limit).await?;
    let refs_path = refs::write(&entries)?;

    let lines: Vec<String> = entries.iter().map(format_line).collect();
    let items: Vec<SnapshotItem> = entries
        .iter()
        .map(|entry| SnapshotItem {
            ref_id: entry.ref_id.clone(),
            role: entry.role.clone(),
            name: entry.name.clone(),
            x: entry.x,
            y: entry.y,
            width: entry.width,
            height: entry.height,
            states: Vec::new(),
        })
        .collect();

    let out = SnapshotOutput {
        ok: true,
        truncated,
        items,
        lines,
        refs_path: refs_path.display().to_string(),
        diagnostics,
    };
    println!("{}", serde_json::to_string(&out)?);
    Ok(())
}

fn format_line(entry: &RefEntry) -> String {
    let mut line = format!("- {}", entry.role);
    let name = entry.name.trim();
    if !name.is_empty() {
        line.push(' ');
        line.push_str(&json_quote(name));
    }
    line.push_str(&format!(" [ref={}]", entry.ref_id));
    if entry.width > 0 && entry.height > 0 {
        line.push_str(&format!(
            " @{},{} {}x{}",
            entry.x, entry.y, entry.width, entry.height
        ));
    }
    line
}

fn json_quote(value: &str) -> String {
    serde_json::to_string(value).unwrap_or_else(|_| format!("\"{value}\""))
}

async fn collect(
    conn: &AccessibilityConnection,
    limit: usize,
) -> Result<(Vec<RefEntry>, bool, Diagnostics)> {
    let registry = conn.root_accessible_on_registry().await?;
    let app_refs = registry.get_children().await?;

    let mut entries: Vec<RefEntry> = Vec::new();
    let mut diag = Diagnostics {
        apps: app_refs.len(),
        bus_address: connection::current_bus_address(),
        display: std::env::var("DISPLAY").ok(),
        ..Diagnostics::default()
    };
    let mut truncated = false;

    for app_obj in app_refs.into_iter().take(MAX_APPS) {
        if entries.len() >= limit {
            truncated = true;
            break;
        }
        let app_proxy = match connection::accessible_for(conn, &app_obj).await {
            Ok(proxy) => proxy,
            Err(_) => {
                diag.errors += 1;
                continue;
            }
        };

        // Iterative depth-first walk inside this application.
        let mut stack: Vec<AccessibleProxy<'_>> = vec![app_proxy];
        while let Some(node) = stack.pop() {
            if entries.len() >= limit {
                truncated = true;
                break;
            }
            if diag.visited >= MAX_VISITS {
                truncated = true;
                break;
            }
            diag.visited += 1;

            match describe(conn, &node, entries.len() + 1).await {
                Outcome::Keep(entry) => {
                    diag.accepted += 1;
                    entries.push(entry);
                }
                Outcome::SkipState => diag.skipped_state += 1,
                Outcome::SkipRole => diag.skipped_role += 1,
                Outcome::SkipGeometry => diag.skipped_geometry += 1,
                Outcome::Error => diag.errors += 1,
            }

            // Expand children even when the node itself was filtered, so
            // descendants still get a chance to surface.
            let child_objs = match node.get_children().await {
                Ok(values) => values,
                Err(_) => {
                    diag.errors += 1;
                    continue;
                }
            };
            for child_obj in child_objs.into_iter().rev() {
                if let Ok(child) = into_accessible(conn, child_obj).await {
                    stack.push(child);
                }
            }
        }
    }
    Ok((entries, truncated, diag))
}

async fn into_accessible<'a>(
    conn: &'a AccessibilityConnection,
    object: ObjectRefOwned,
) -> Result<AccessibleProxy<'a>> {
    connection::accessible_for(conn, &object).await
}

enum Outcome {
    Keep(RefEntry),
    SkipState,
    SkipRole,
    SkipGeometry,
    Error,
}

async fn describe(
    conn: &AccessibilityConnection,
    node: &AccessibleProxy<'_>,
    next_index: usize,
) -> Outcome {
    let states = match node.get_state().await {
        Ok(s) => s,
        Err(_) => return Outcome::Error,
    };
    if !is_on_screen(&states) {
        return Outcome::SkipState;
    }
    let role = match node.get_role().await {
        Ok(r) => r,
        Err(_) => return Outcome::Error,
    };
    if role_is_structural(role) {
        return Outcome::SkipRole;
    }

    let role_name = node
        .get_role_name()
        .await
        .unwrap_or_else(|_| format!("{role:?}").to_lowercase());
    let name = node.name().await.unwrap_or_default();

    // Geometry is best-effort: nodes with missing or zero extents (popups,
    // virtual children, lazily-laid-out widgets) are still useful to surface
    // when they expose a name. The Go side only needs geometry for the RFB
    // fallback path, which gracefully degrades to the AT-SPI Action route.
    let (x, y, width, height) = match connection::component_for(conn, node).await {
        Ok(component) => component
            .get_extents(CoordType::Screen)
            .await
            .unwrap_or((0, 0, 0, 0)),
        Err(_) => (0, 0, 0, 0),
    };

    // If both geometry and name are empty, the node carries no information the
    // model can act on. Skip it to keep the snapshot focused.
    if width <= 0 && height <= 0 && name.trim().is_empty() {
        return Outcome::SkipGeometry;
    }

    let inner = node.inner();
    Outcome::Keep(RefEntry {
        ref_id: format!("e{next_index}"),
        bus_name: inner.destination().to_string(),
        object_path: inner.path().to_string(),
        role: role_name,
        name,
        x,
        y,
        width,
        height,
    })
}

fn is_on_screen(states: &StateSet) -> bool {
    // AT-SPI exposes both Visible (will be drawn when its parent is) and
    // Showing (is actually on screen right now). Chromium tends to set only
    // Showing on many of its accessible nodes, while GTK apps set both — we
    // accept either to avoid false negatives.
    states.contains(State::Showing) || states.contains(State::Visible)
}

fn role_is_structural(role: Role) -> bool {
    // Blacklist: things that are pure structural noise, never actionable, and
    // whose subtrees are still walked (we filter the node itself, not its
    // descendants).
    matches!(
        role,
        Role::Filler
            | Role::Separator
            | Role::Invalid
            | Role::Unknown
            | Role::DesktopFrame
            | Role::Application
            | Role::DesktopIcon
    )
}

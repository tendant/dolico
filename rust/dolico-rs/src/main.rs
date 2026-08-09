//! `dolico-rs` -- the Rust extraction shim.
//!
//! Neither anydoc nor pdf-inspector exposes its document model as JSON:
//! `anydoc::to_document` is a Rust-only API and both CLIs emit Markdown. This
//! binary is the bridge. It calls both libraries and serializes into the
//! canonical schema, adding the page, bounding-box, confidence and provenance
//! fields that neither library carries.
//!
//! It is invoked as a subprocess by the Go orchestrator, one run per job:
//!
//! ```text
//! dolico-rs inspect --in doc.pdf --out inspect.json
//! dolico-rs extract --in doc.pdf --pages 1,3,5 --assets-dir ./assets --out doc.json
//! dolico-rs version
//! ```
//!
//! Exit codes are part of the contract, because the orchestrator routes on
//! them: 0 ok, 2 unsupported, 3 malformed, 4 encrypted, 1 internal.
//! On failure a JSON [`ErrorOutput`] goes to stdout so the caller gets
//! something structured rather than a message to scrape.

mod canonical;
mod md;
mod native;
mod pdf;

use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::ExitCode;

use clap::{Parser, Subcommand};

use canonical::{ErrorOutput, ExtractOutput, InspectOutput, PageKind, SCHEMA_VERSION};

/// Engine name recorded in provenance for natively-parsed formats.
pub const ENGINE_NATIVE: &str = "anydoc";
/// Engine name recorded in provenance for PDFs.
pub const ENGINE_PDF: &str = "pdf-inspector";

/// Exit codes. The orchestrator distinguishes "not my job" from "this file is
/// broken" from "I failed" purely by these, so they are load-bearing.
mod exit {
    pub const INTERNAL: u8 = 1;
    pub const UNSUPPORTED: u8 = 2;
    pub const MALFORMED: u8 = 3;
    pub const ENCRYPTED: u8 = 4;
}

#[derive(Debug)]
pub enum ShimError {
    Unsupported(String),
    Malformed(String),
    Encrypted,
    ResourceLimit(String),
    Io(std::io::Error),
    Internal(String),
}

impl ShimError {
    fn kind(&self) -> &'static str {
        match self {
            ShimError::Unsupported(_) => "unsupported",
            ShimError::Malformed(_) => "malformed",
            ShimError::Encrypted => "encrypted",
            ShimError::ResourceLimit(_) => "resource_limit",
            ShimError::Io(_) => "io",
            ShimError::Internal(_) => "internal",
        }
    }

    fn exit_code(&self) -> u8 {
        match self {
            ShimError::Unsupported(_) => exit::UNSUPPORTED,
            // A resource limit means this document cannot be processed as-is,
            // which is the same actionable outcome as malformed: do not retry.
            ShimError::Malformed(_) | ShimError::ResourceLimit(_) => exit::MALFORMED,
            ShimError::Encrypted => exit::ENCRYPTED,
            ShimError::Io(_) | ShimError::Internal(_) => exit::INTERNAL,
        }
    }
}

impl std::fmt::Display for ShimError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ShimError::Unsupported(m) => write!(f, "unsupported input: {m}"),
            ShimError::Malformed(m) => write!(f, "malformed document: {m}"),
            ShimError::Encrypted => write!(f, "document is encrypted"),
            ShimError::ResourceLimit(m) => write!(f, "resource limit exceeded: {m}"),
            ShimError::Io(e) => write!(f, "io error: {e}"),
            ShimError::Internal(m) => write!(f, "internal error: {m}"),
        }
    }
}

impl From<std::io::Error> for ShimError {
    fn from(e: std::io::Error) -> Self {
        ShimError::Io(e)
    }
}

impl From<anydoc::ConvertError> for ShimError {
    fn from(e: anydoc::ConvertError) -> Self {
        use anydoc::ConvertError as C;
        match e {
            C::Unsupported(m) => ShimError::Unsupported(m),
            C::Malformed { .. } | C::MissingPart { .. } => ShimError::Malformed(e.to_string()),
            C::Encrypted => ShimError::Encrypted,
            C::ResourceLimit { .. } => ShimError::ResourceLimit(e.to_string()),
            C::Io(io) => ShimError::Io(io),
            // ConvertError is #[non_exhaustive]: a variant added upstream must
            // not silently become "internal error" with no detail.
            other => ShimError::Malformed(other.to_string()),
        }
    }
}

impl From<pdf_inspector::PdfError> for ShimError {
    fn from(e: pdf_inspector::PdfError) -> Self {
        use pdf_inspector::PdfError as P;
        match e {
            P::Io(io) => ShimError::Io(io),
            P::Encrypted => ShimError::Encrypted,
            P::NotAPdf(m) => ShimError::Unsupported(format!("not a PDF: {m}")),
            P::Parse(m) => ShimError::Malformed(m),
            P::InvalidStructure => ShimError::Malformed("invalid PDF structure".into()),
        }
    }
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

#[derive(Parser)]
#[command(
    name = "dolico-rs",
    about = "Dolico Rust extraction shim (anydoc + pdf-inspector -> canonical JSON)",
    version
)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Classify a document without extracting text. PDFs get per-page
    /// text-versus-scanned classification; other formats report one section.
    Inspect {
        /// Input document.
        #[arg(long = "in")]
        input: PathBuf,
        /// Original filename, used for format detection when the input path
        /// has no meaningful extension. Required for signature-less formats
        /// (Markdown, plain text, CSV) read out of content-addressed storage.
        #[arg(long)]
        name: Option<String>,
        /// Write JSON here instead of stdout.
        #[arg(long)]
        out: Option<PathBuf>,
    },
    /// Extract a document into canonical JSON.
    Extract {
        /// Input document.
        #[arg(long = "in")]
        input: PathBuf,
        /// Original filename, used for format detection when the input path
        /// has no meaningful extension. See `inspect --name`.
        #[arg(long)]
        name: Option<String>,
        /// Comma-separated 1-indexed pages. Omit for all pages. PDFs only.
        #[arg(long, value_delimiter = ',')]
        pages: Option<Vec<u32>>,
        /// Directory to write embedded assets into. Without it, assets are
        /// skipped rather than inlined.
        #[arg(long = "assets-dir")]
        assets_dir: Option<PathBuf>,
        /// Write JSON here instead of stdout. Preferred for large documents:
        /// the orchestrator passes a temp path so multi-megabyte output never
        /// goes through a pipe.
        #[arg(long)]
        out: Option<PathBuf>,
    },
    /// Print the shim and engine versions as JSON.
    Version,
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    match run(cli) {
        Ok(()) => ExitCode::SUCCESS,
        Err(err) => {
            let payload = ErrorOutput {
                schema_version: SCHEMA_VERSION,
                kind: err.kind(),
                message: err.to_string(),
            };
            // Best-effort: if stdout is gone there is nothing useful to do,
            // and the exit code still carries the outcome.
            let _ = serde_json::to_writer(std::io::stdout(), &payload);
            let _ = writeln!(std::io::stdout());
            eprintln!("dolico-rs: {err}");
            ExitCode::from(err.exit_code())
        }
    }
}

fn run(cli: Cli) -> Result<(), ShimError> {
    match cli.command {
        Command::Version => {
            let payload = serde_json::json!({
                "schema_version": SCHEMA_VERSION,
                "shim_version": env!("CARGO_PKG_VERSION"),
                "engines": {
                    ENGINE_NATIVE: native::ANYDOC_VERSION,
                    ENGINE_PDF: pdf::PDF_INSPECTOR_VERSION,
                },
            });
            println!("{}", serde_json::to_string(&payload).unwrap());
            Ok(())
        }
        Command::Inspect { input, name, out } => {
            let started = std::time::Instant::now();
            let bytes = read_input(&input)?;
            let kind = detect_or_fail(&bytes, detection_path(&input, name.as_deref()))?;

            let (engine, metadata, pages, page_kind) = match kind {
                native::Kind::Pdf => {
                    let (m, p) = pdf::inspect(&bytes)?;
                    (ENGINE_PDF, m, p, PageKind::Paginated)
                }
                other => {
                    // Native formats have nothing cheap to inspect: anydoc has
                    // no detection-only mode, and the parse is milliseconds
                    // anyway. Report the shape without the blocks.
                    let (m, mut p, _) = native::extract(&bytes, other, None)?;
                    for page in &mut p {
                        page.blocks.clear();
                    }
                    (ENGINE_NATIVE, m, p, PageKind::Section)
                }
            };

            let payload = InspectOutput {
                schema_version: SCHEMA_VERSION,
                engine: engine.to_string(),
                engine_version: engine_version(engine).to_string(),
                page_count: pages.len() as u32,
                page_kind,
                metadata,
                pages,
                duration_ms: started.elapsed().as_millis() as u64,
            };
            write_json(&payload, out.as_deref())
        }
        Command::Extract {
            input,
            name,
            pages,
            assets_dir,
            out,
        } => {
            let started = std::time::Instant::now();
            let bytes = read_input(&input)?;
            let kind = detect_or_fail(&bytes, detection_path(&input, name.as_deref()))?;

            let (engine, metadata, extracted, assets) = match kind {
                native::Kind::Pdf => {
                    let (m, p) = pdf::extract(&bytes, pages.as_deref())?;
                    (ENGINE_PDF, m, p, Vec::new())
                }
                other => {
                    if pages.is_some() {
                        // Silently ignoring the filter would produce a whole
                        // document where the caller asked for one page, which
                        // the router would then merge into the wrong slots.
                        return Err(ShimError::Unsupported(
                            "--pages applies to PDFs only; native formats have no pagination"
                                .into(),
                        ));
                    }
                    let (m, p, a) = native::extract(&bytes, other, assets_dir.as_deref())?;
                    (ENGINE_NATIVE, m, p, a)
                }
            };

            let payload = ExtractOutput {
                schema_version: SCHEMA_VERSION,
                engine: engine.to_string(),
                engine_version: engine_version(engine).to_string(),
                metadata,
                pages: extracted,
                assets,
                duration_ms: started.elapsed().as_millis() as u64,
            };
            write_json(&payload, out.as_deref())
        }
    }
}

fn engine_version(engine: &str) -> &'static str {
    if engine == ENGINE_PDF {
        pdf::PDF_INSPECTOR_VERSION
    } else {
        native::ANYDOC_VERSION
    }
}

fn read_input(path: &Path) -> Result<Vec<u8>, ShimError> {
    std::fs::read(path).map_err(|e| {
        ShimError::Io(std::io::Error::new(
            e.kind(),
            format!("{}: {e}", path.display()),
        ))
    })
}

/// Which path format detection should read the extension from.
///
/// The orchestrator stores uploads by content digest, so `--in` points at a
/// name like `blobs/7b/7b21e6...` with no extension at all. Markdown, plain
/// text and CSV carry no content signature and can only be recognized by
/// extension, so without the original filename they are undetectable.
fn detection_path<'a>(input: &'a Path, name: Option<&'a str>) -> &'a Path {
    match name {
        Some(n) if !n.trim().is_empty() => Path::new(n),
        _ => input,
    }
}

fn detect_or_fail(bytes: &[u8], path: &Path) -> Result<native::Kind, ShimError> {
    native::detect(bytes, path).ok_or_else(|| {
        ShimError::Unsupported(format!(
            "unrecognized content and extension: {}",
            path.display()
        ))
    })
}

fn write_json<T: serde::Serialize>(payload: &T, out: Option<&Path>) -> Result<(), ShimError> {
    match out {
        Some(path) => {
            if let Some(parent) = path.parent().filter(|p| !p.as_os_str().is_empty()) {
                std::fs::create_dir_all(parent)?;
            }
            let file = std::fs::File::create(path)?;
            let mut w = std::io::BufWriter::new(file);
            serde_json::to_writer(&mut w, payload)
                .map_err(|e| ShimError::Internal(e.to_string()))?;
            w.flush()?;
            Ok(())
        }
        None => {
            let stdout = std::io::stdout();
            let mut w = std::io::BufWriter::new(stdout.lock());
            serde_json::to_writer(&mut w, payload)
                .map_err(|e| ShimError::Internal(e.to_string()))?;
            writeln!(w)?;
            w.flush()?;
            Ok(())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn convert_error_maps_to_exit_codes() {
        let unsupported: ShimError = anydoc::ConvertError::Unsupported("x".into()).into();
        assert_eq!(unsupported.exit_code(), exit::UNSUPPORTED);

        let encrypted: ShimError = anydoc::ConvertError::Encrypted.into();
        assert_eq!(encrypted.exit_code(), exit::ENCRYPTED);

        let malformed: ShimError = anydoc::ConvertError::Malformed {
            part: None,
            detail: "bad".into(),
        }
        .into();
        assert_eq!(malformed.exit_code(), exit::MALFORMED);
    }

    #[test]
    fn pdf_error_maps_to_exit_codes() {
        let not_pdf: ShimError = pdf_inspector::PdfError::NotAPdf("nope".into()).into();
        assert_eq!(not_pdf.exit_code(), exit::UNSUPPORTED);

        let encrypted: ShimError = pdf_inspector::PdfError::Encrypted.into();
        assert_eq!(encrypted.exit_code(), exit::ENCRYPTED);

        let broken: ShimError = pdf_inspector::PdfError::InvalidStructure.into();
        assert_eq!(broken.exit_code(), exit::MALFORMED);
    }

    #[test]
    fn detection_prefers_the_original_filename_over_the_digest_path() {
        let stored = Path::new("/var/blobs/7b/7b21e62dca83e20f");
        assert_eq!(
            detection_path(stored, Some("notes.md")),
            Path::new("notes.md")
        );
        // No hint, or a blank one, falls back to the stored path.
        assert_eq!(detection_path(stored, None), stored);
        assert_eq!(detection_path(stored, Some("  ")), stored);
    }

    #[test]
    fn resource_limit_is_not_retryable() {
        let err = ShimError::ResourceLimit("too big".into());
        assert_eq!(err.exit_code(), exit::MALFORMED);
        assert_eq!(err.kind(), "resource_limit");
    }
}

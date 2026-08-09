# Document Processing Platform Design (Go + Rust + Python)

## Executive Summary

This document proposes a production-ready architecture for a universal
document ingestion platform. The primary design goal is **high-quality
structured document extraction**, not OCR alone.

### Core principles

1.  Extract natively whenever possible.
2.  OCR only pages that require OCR.
3.  Route work per-page instead of per-document.
4.  Keep OCR engines replaceable.
5.  Normalize everything into one canonical document model.
6.  Preserve provenance and confidence.
7.  Cache aggressively using content hashes.

------------------------------------------------------------------------

# Technology Decisions

  -----------------------------------------------------------------------
  Component                  Language                 Reason
  -------------------------- ------------------------ -------------------
  API / Router / Scheduler   Go                       Fast development,
                                                      simple concurrency,
                                                      excellent
                                                      networking
                                                      ecosystem

  PDF Native Parsing         Rust (`pdf-inspector`)   Best-in-class
                                                      parsing performance
                                                      and existing
                                                      implementation

  OCR & Layout               Python (PaddleOCR /      Strong ML ecosystem
                             PP-Structure)            

  Storage                    MinIO/S3 + PostgreSQL    Durable object
                                                      storage + metadata

  Queue                      In-process initially,    Simple MVP,
                             NATS later               scalable upgrade
                                                      path
  -----------------------------------------------------------------------

## Why Go?

Go is recommended as the orchestration language because the system is
primarily a distributed service rather than a parsing library.

Advantages:

-   Simple concurrency (goroutines)
-   Excellent HTTP ecosystem
-   Single static binaries
-   Fast iteration
-   Easy deployment
-   Strong observability ecosystem

Rust should remain an implementation dependency where it already excels
instead of rewriting it.

------------------------------------------------------------------------

# High-Level Architecture

``` text
                  Client/API
                      |
                 Go API Service
                      |
                Document Router
                      |
      +---------------+----------------+
      |               |                |
      v               v                v
 Native Parser   PDF Inspector     OCR Workers
     (Go)           (Rust)          (Python)
      |               |                |
      +---------------+----------------+
                      |
             Canonical Document
                      |
        +-------------+-------------+
        |                           |
        v                           v
    Markdown                  JSON Structure
                      |
                      v
                  Search / RAG
```

------------------------------------------------------------------------

# Processing Pipeline

1.  Upload
2.  Inspect
3.  Route
4.  Extract
5.  Normalize
6.  Validate
7.  Store
8.  Chunk
9.  Index

Never OCR an entire document simply because it is a PDF.

------------------------------------------------------------------------

# Routing Strategy

## Native documents

-   DOCX
-   XLSX
-   PPTX
-   HTML
-   EPUB
-   Markdown
-   TXT

Use native parsers.

## PDFs

Run `pdf-inspector`.

For every page determine:

-   native text
-   scanned image
-   mixed
-   confidence

Only OCR pages requiring OCR.

------------------------------------------------------------------------

# OCR Pipeline

Tier 1

-   PaddleOCR

Tier 2

-   PP-StructureV3

Tier 3 (future)

-   Vision LLM fallback

Escalation should be confidence driven.

------------------------------------------------------------------------

# Canonical Data Model

Markdown is **not** the internal representation.

``` text
Document
 ├── metadata
 ├── pages
 │     └── blocks
 │            ├── heading
 │            ├── paragraph
 │            ├── table
 │            ├── image
 │            ├── formula
 │            └── code
 └── assets
```

Every block stores:

-   page
-   bounding box
-   confidence
-   extraction engine
-   provenance

------------------------------------------------------------------------

# Storage

Object Storage

-   original documents
-   rendered pages
-   extracted images
-   normalized JSON
-   markdown

PostgreSQL

-   jobs
-   pages
-   extraction metadata
-   engine versions

------------------------------------------------------------------------

# Cache

Use SHA-256 of original content.

Cache key:

    document hash
    engine
    engine version
    pipeline version
    configuration

------------------------------------------------------------------------

# Engine Interface

Each engine implements:

``` go
type Engine interface {
    Inspect(ctx context.Context, doc *Document) (*Inspection, error)
    Supports(*Inspection) SupportScore
    Extract(ctx context.Context, req *ExtractRequest) (*ExtractResult, error)
}
```

Implementations:

-   Native
-   PDF Inspector
-   PaddleOCR
-   PP-Structure
-   Vision

------------------------------------------------------------------------

# Deployment

## MVP

Docker Compose

-   api
-   worker
-   postgres
-   minio
-   paddle service
-   pdf-inspector

## Scale

-   NATS
-   CPU worker pool
-   GPU worker pool
-   Horizontal API scaling

------------------------------------------------------------------------

# Review

## Strengths

-   Avoids unnecessary OCR.
-   Engine-independent architecture.
-   Easy to replace OCR engines.
-   Clear separation between orchestration and ML.
-   Efficient caching.
-   Excellent fit for RAG.

## Improvements

1.  Add benchmarking framework comparing engines on representative
    datasets.
2.  Version the canonical schema independently of engine versions.
3.  Introduce per-page quality scoring rather than trusting engine
    confidence.
4.  Support partial document reprocessing after engine upgrades.
5.  Make rendering lazy and cache by page + DPI.
6.  Add document provenance and trace IDs for debugging.
7.  Build regression tests using golden documents.
8.  Expose structured JSON as the primary API; generate Markdown as a
    view.

------------------------------------------------------------------------

# Roadmap

## V1

-   Go API
-   Native parsers
-   pdf-inspector
-   PaddleOCR
-   Canonical JSON
-   Markdown export

## V1.5

-   PP-Structure
-   Better tables
-   Parallel page processing

## V2

-   MinerU strategy
-   Vision LLM fallback
-   Distributed workers
-   Intelligent quality scoring

------------------------------------------------------------------------

# Final Recommendation

Use:

-   **Go** for orchestration, routing, storage, APIs, scheduling and
    normalization.
-   **Rust** only where existing parsing libraries provide substantial
    value (e.g. pdf-inspector).
-   **Python** for OCR and ML inference.

This minimizes complexity while retaining the strengths of each
ecosystem and provides a clean, maintainable architecture that can scale
from a single Docker Compose deployment to a distributed GPU-backed
processing platform.

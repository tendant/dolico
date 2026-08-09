//! Markdown -> canonical blocks.
//!
//! This exists for two reasons, and it is the same parser for both:
//!
//!   * `.md` and `.txt` inputs, which anydoc does not handle (its `Format`
//!     enum covers office and ebook containers only).
//!   * PDF pages. pdf-inspector's per-page output is Markdown, not a block
//!     tree -- its positioned `TextItem`s are the raw layer below that, with
//!     no paragraph grouping or heading detection. Rather than reimplement its
//!     layout analysis we take the Markdown it produces and structure that.
//!
//! Scope is deliberately GitHub-flavored-Markdown-as-emitted, not a
//! CommonMark implementation: it handles what the two producers actually emit
//! and degrades to paragraphs for anything else. Reference links, HTML blocks,
//! setext headings and loose/tight list distinctions are out of scope.

use crate::canonical::{Block, BlockType, Cell, IdGen, List, ListItem, Provenance, Span, Table};

/// Parse Markdown into canonical blocks.
pub fn parse_blocks(md: &str, ids: &mut IdGen, prov: &Provenance) -> Vec<Block> {
    let lines: Vec<&str> = md.lines().collect();
    parse_lines(&lines, ids, prov)
}

fn parse_lines(lines: &[&str], ids: &mut IdGen, prov: &Provenance) -> Vec<Block> {
    let mut out = Vec::new();
    let mut i = 0;
    while i < lines.len() {
        let line = lines[i];
        if line.trim().is_empty() {
            i += 1;
            continue;
        }
        if let Some(fence) = code_fence(line) {
            let (block, next) = parse_code(lines, i, fence, ids, prov);
            out.push(block);
            i = next;
        } else if is_rule(line) {
            out.push(Block::new(ids.next(), BlockType::Rule, prov.clone()));
            i += 1;
        } else if let Some((level, text)) = atx_heading(line) {
            let mut b = Block::new(ids.next(), BlockType::Heading, prov.clone());
            let (plain, spans) = parse_inlines(text);
            b.level = Some(level);
            b.text = Some(plain);
            b.inline = non_trivial(spans);
            out.push(b);
            i += 1;
        } else if line.trim_start().starts_with('>') {
            let (block, next) = parse_quote(lines, i, ids, prov);
            out.push(block);
            i = next;
        } else if let Some((block, next)) = parse_table(lines, i, ids, prov) {
            out.push(block);
            i = next;
        } else if list_marker(line).is_some() {
            let (block, next) = parse_list(lines, i, ids, prov);
            out.push(block);
            i = next;
        } else {
            let (block, next) = parse_paragraph(lines, i, ids, prov);
            if let Some(block) = block {
                out.push(block);
            }
            i = next;
        }
    }
    out
}

// ---------------------------------------------------------------------------
// Block recognizers
// ---------------------------------------------------------------------------

fn atx_heading(line: &str) -> Option<(u8, &str)> {
    let t = line.trim_start();
    let hashes = t.len() - t.trim_start_matches('#').len();
    if hashes == 0 || hashes > 6 {
        return None;
    }
    let rest = &t[hashes..];
    // "#hashtag" is not a heading; ATX requires a space after the run.
    if !rest.is_empty() && !rest.starts_with(' ') {
        return None;
    }
    Some((hashes as u8, rest.trim().trim_end_matches('#').trim()))
}

fn is_rule(line: &str) -> bool {
    let t = line.trim();
    if t.len() < 3 {
        return false;
    }
    let c = t.as_bytes()[0];
    matches!(c, b'-' | b'*' | b'_') && t.bytes().all(|b| b == c)
}

/// Returns the fence string when this line opens a fenced code block.
fn code_fence(line: &str) -> Option<&str> {
    let t = line.trim_start();
    ["```", "~~~"]
        .into_iter()
        .find(|&f| t.starts_with(f))
        .map(|v| v as _)
}

fn parse_code(
    lines: &[&str],
    start: usize,
    fence: &str,
    ids: &mut IdGen,
    prov: &Provenance,
) -> (Block, usize) {
    let lang = lines[start].trim_start().trim_start_matches(fence).trim();
    let mut body = Vec::new();
    let mut i = start + 1;
    while i < lines.len() && !lines[i].trim_start().starts_with(fence) {
        body.push(lines[i]);
        i += 1;
    }
    // Unterminated fence: consume to end of input rather than dropping content.
    let next = if i < lines.len() { i + 1 } else { i };

    let mut b = Block::new(ids.next(), BlockType::Code, prov.clone());
    b.text = Some(body.join("\n"));
    b.lang = if lang.is_empty() {
        None
    } else {
        Some(lang.to_string())
    };
    (b, next)
}

fn parse_quote(lines: &[&str], start: usize, ids: &mut IdGen, prov: &Provenance) -> (Block, usize) {
    let mut inner = Vec::new();
    let mut i = start;
    while i < lines.len() {
        let t = lines[i].trim_start();
        if let Some(rest) = t.strip_prefix('>') {
            inner.push(rest.strip_prefix(' ').unwrap_or(rest));
            i += 1;
        } else if t.is_empty() {
            break;
        } else {
            // Lazy continuation: an unprefixed line still belongs to the quote.
            inner.push(lines[i]);
            i += 1;
        }
    }
    let mut b = Block::new(ids.next(), BlockType::Quote, prov.clone());
    b.quote = Some(parse_lines(&inner, ids, prov));
    (b, i)
}

/// A GFM pipe table: a header row, a delimiter row, then body rows.
fn parse_table(
    lines: &[&str],
    start: usize,
    ids: &mut IdGen,
    prov: &Provenance,
) -> Option<(Block, usize)> {
    if start + 1 >= lines.len() {
        return None;
    }
    if !lines[start].trim_start().starts_with('|') || !is_delimiter_row(lines[start + 1]) {
        return None;
    }
    let mut rows = vec![split_row(lines[start])];
    let mut i = start + 2;
    while i < lines.len() && lines[i].trim_start().starts_with('|') {
        rows.push(split_row(lines[i]));
        i += 1;
    }

    let grid = rows
        .iter()
        .map(|cells| {
            cells
                .iter()
                .map(|text| {
                    let mut cell = Cell {
                        row_span: Some(1),
                        col_span: Some(1),
                        ..Default::default()
                    };
                    if !text.trim().is_empty() {
                        let mut p = Block::new(ids.next(), BlockType::Paragraph, prov.clone());
                        let (plain, spans) = parse_inlines(text);
                        p.text = Some(plain);
                        p.inline = non_trivial(spans);
                        cell.blocks = vec![p];
                    }
                    cell
                })
                .collect()
        })
        .collect();

    let mut b = Block::new(ids.next(), BlockType::Table, prov.clone());
    // Markdown cannot express merged cells, so every slot is an origin.
    b.table = Some(Table {
        header_rows: 1,
        kind: Some("data"),
        grid,
    });
    Some((b, i))
}

fn is_delimiter_row(line: &str) -> bool {
    let t = line.trim();
    if !t.starts_with('|') {
        return false;
    }
    let inner = t.trim_matches('|');
    !inner.is_empty()
        && inner.split('|').all(|c| {
            let c = c.trim();
            !c.is_empty() && c.chars().all(|ch| ch == '-' || ch == ':')
        })
}

fn split_row(line: &str) -> Vec<String> {
    let t = line.trim();
    let t = t.strip_prefix('|').unwrap_or(t);
    let t = t.strip_suffix('|').unwrap_or(t);
    t.split('|').map(|c| c.trim().to_string()).collect()
}

/// Recognizes a list item: returns (indent, ordered, start ordinal, marker
/// style, content).
fn list_marker(line: &str) -> Option<(usize, bool, u64, &'static str, &str)> {
    let indent = line.len() - line.trim_start().len();
    let t = line.trim_start();
    let mut chars = t.chars();
    let first = chars.next()?;

    if matches!(first, '-' | '*' | '+') {
        let rest = &t[1..];
        // A rule ("---") is not a list item, and "*emphasis*" needs the space.
        if !rest.starts_with(' ') {
            return None;
        }
        return Some((indent, false, 1, "bullet", rest.trim_start()));
    }

    if first.is_ascii_digit() {
        let digits: String = t.chars().take_while(|c| c.is_ascii_digit()).collect();
        let rest = &t[digits.len()..];
        let delim = rest.chars().next()?;
        if delim != '.' && delim != ')' {
            return None;
        }
        let after = &rest[1..];
        if !after.starts_with(' ') {
            return None;
        }
        let n = digits.parse().unwrap_or(1);
        return Some((indent, true, n, "decimal", after.trim_start()));
    }

    None
}

fn parse_list(lines: &[&str], start: usize, ids: &mut IdGen, prov: &Provenance) -> (Block, usize) {
    let (base_indent, ordered, first_n, marker, _) = list_marker(lines[start]).unwrap();
    let mut items: Vec<ListItem> = Vec::new();
    let mut i = start;

    while i < lines.len() {
        let Some((indent, item_ordered, _, _, content)) = list_marker(lines[i]) else {
            break;
        };
        // A different indent or a switch between bullet and numbered starts a
        // new list rather than continuing this one.
        if indent != base_indent || item_ordered != ordered {
            break;
        }

        // Gather this item's continuation and nested lines: everything up to
        // the next marker at our own indent.
        let mut owned: Vec<String> = Vec::new();
        let (checked, content) = task_marker(content);
        owned.push(content.to_string());
        i += 1;
        while i < lines.len() {
            if lines[i].trim().is_empty() {
                // A blank line ends the item unless the list continues after it.
                if i + 1 < lines.len() && list_marker(lines[i + 1]).is_some() {
                    i += 1;
                    continue;
                }
                break;
            }
            let indent_here = lines[i].len() - lines[i].trim_start().len();
            if list_marker(lines[i]).is_some() && indent_here <= base_indent {
                break;
            }
            if indent_here <= base_indent && list_marker(lines[i]).is_none() {
                // Lazy continuation of the item's paragraph.
                owned.push(lines[i].trim_start().to_string());
                i += 1;
                continue;
            }
            // Nested content: strip one level of indentation.
            let strip = (base_indent + 2).min(indent_here);
            owned.push(lines[i][strip..].to_string());
            i += 1;
        }

        let refs: Vec<&str> = owned.iter().map(|s| s.as_str()).collect();
        items.push(ListItem {
            blocks: parse_lines(&refs, ids, prov),
            checked,
        });
    }

    let mut b = Block::new(ids.next(), BlockType::List, prov.clone());
    b.list = Some(List {
        ordered,
        marker: Some(marker),
        start: if ordered && first_n != 1 {
            Some(first_n)
        } else {
            None
        },
        items,
    });
    (b, i)
}

/// Splits a leading `[ ] ` / `[x] ` task checkbox off an item's content.
fn task_marker(content: &str) -> (Option<bool>, &str) {
    let lower = content.to_ascii_lowercase();
    if lower.starts_with("[ ] ") {
        (Some(false), content[4..].trim_start())
    } else if lower.starts_with("[x] ") {
        (Some(true), content[4..].trim_start())
    } else {
        (None, content)
    }
}

fn parse_paragraph(
    lines: &[&str],
    start: usize,
    ids: &mut IdGen,
    prov: &Provenance,
) -> (Option<Block>, usize) {
    let mut buf: Vec<&str> = Vec::new();
    let mut i = start;
    while i < lines.len() {
        let line = lines[i];
        if line.trim().is_empty()
            || is_rule(line)
            || code_fence(line).is_some()
            || atx_heading(line).is_some()
            || list_marker(line).is_some()
            || line.trim_start().starts_with('>')
        {
            break;
        }
        buf.push(line.trim());
        i += 1;
    }
    if buf.is_empty() {
        return (None, i.max(start + 1));
    }
    let joined = buf.join(" ");

    // A paragraph that is nothing but an image is an image block, which is
    // what makes figures addressable instead of buried in paragraph text.
    if let Some((alt, src)) = lone_image(&joined) {
        let mut b = Block::new(ids.next(), BlockType::Image, prov.clone());
        b.alt = Some(alt);
        b.asset_ref = Some(src);
        return (Some(b), i);
    }

    let mut b = Block::new(ids.next(), BlockType::Paragraph, prov.clone());
    let (plain, spans) = parse_inlines(&joined);
    b.text = Some(plain);
    b.inline = non_trivial(spans);
    (Some(b), i)
}

fn lone_image(s: &str) -> Option<(String, String)> {
    let s = s.trim();
    if !s.starts_with("![") || !s.ends_with(')') {
        return None;
    }
    let close = s.find("](")?;
    // Reject "![a](x) trailing text" and "![a](x) ![b](y)".
    if s[close + 2..].contains("](") {
        return None;
    }
    Some((
        s[2..close].to_string(),
        s[close + 2..s.len() - 1].to_string(),
    ))
}

// ---------------------------------------------------------------------------
// Inline parsing
// ---------------------------------------------------------------------------

/// Parse inline markup into plain text plus styled spans.
///
/// Returns the plain text unconditionally -- consumers that only want text
/// never have to walk the spans -- and the spans alongside it.
pub fn parse_inlines(s: &str) -> (String, Vec<Span>) {
    let mut spans: Vec<Span> = Vec::new();
    let mut plain = String::new();
    let b = s.as_bytes();
    let mut i = 0;
    let mut lit = String::new();

    macro_rules! flush {
        () => {
            if !lit.is_empty() {
                plain.push_str(&lit);
                spans.push(Span {
                    text: std::mem::take(&mut lit),
                    ..Default::default()
                });
            }
        };
    }

    while i < b.len() {
        // Escapes: a backslash makes the next byte literal.
        if b[i] == b'\\' && i + 1 < b.len() {
            lit.push(b[i + 1] as char);
            i += 2;
            continue;
        }
        if let Some((inner, next)) = delimited(s, i, "`") {
            flush!();
            plain.push_str(inner);
            spans.push(Span {
                text: inner.to_string(),
                code: true,
                ..Default::default()
            });
            i = next;
            continue;
        }
        let mut matched = false;
        for (delim, bold, italic, strike) in [
            ("***", true, true, false),
            ("**", true, false, false),
            ("~~", false, false, true),
            ("*", false, true, false),
            ("_", false, true, false),
        ] {
            if let Some((inner, next)) = delimited(s, i, delim) {
                flush!();
                let (sub_plain, sub_spans) = parse_inlines(inner);
                plain.push_str(&sub_plain);
                for mut sp in sub_spans {
                    sp.bold |= bold;
                    sp.italic |= italic;
                    sp.strike |= strike;
                    spans.push(sp);
                }
                i = next;
                matched = true;
                break;
            }
        }
        if matched {
            continue;
        }
        // An inline image contributes its alt text to the plain rendering; the
        // href is kept on the span so a consumer can still resolve the asset.
        if let Some((text, href, next, _is_image)) = link(s, i) {
            flush!();
            let (sub_plain, mut sub_spans) = parse_inlines(&text);
            plain.push_str(&sub_plain);
            for sp in &mut sub_spans {
                sp.href = Some(href.clone());
            }
            if sub_spans.is_empty() {
                spans.push(Span {
                    text: String::new(),
                    href: Some(href),
                    ..Default::default()
                });
            } else {
                spans.extend(sub_spans);
            }
            i = next;
            continue;
        }
        // Ordinary byte. Push whole UTF-8 characters, never partial ones.
        let ch_len = utf8_len(b[i]);
        lit.push_str(&s[i..i + ch_len]);
        i += ch_len;
    }
    flush!();
    (plain, spans)
}

/// Matches `<delim>inner<delim>` starting at `i`, returning the inner text and
/// the index just past the closing delimiter.
fn delimited<'a>(s: &'a str, i: usize, delim: &str) -> Option<(&'a str, usize)> {
    let rest = &s[i..];
    if !rest.starts_with(delim) {
        return None;
    }
    let after = &rest[delim.len()..];
    // Empty emphasis ("**" alone) is literal text, not a span.
    let end = after.find(delim)?;
    if end == 0 {
        return None;
    }
    Some((&after[..end], i + delim.len() + end + delim.len()))
}

/// Matches `[text](href)` or `![alt](src)` at `i`.
fn link(s: &str, i: usize) -> Option<(String, String, usize, bool)> {
    let rest = &s[i..];
    let (is_image, open) = if let Some(r) = rest.strip_prefix("![") {
        (true, r)
    } else {
        let r = rest.strip_prefix('[')?;
        (false, r)
    };
    let close = open.find("](")?;
    let text = &open[..close];
    let after = &open[close + 2..];
    let end = after.find(')')?;
    let href = &after[..end];
    let consumed = if is_image { 2 } else { 1 } + close + 2 + end + 1;
    Some((text.to_string(), href.to_string(), i + consumed, is_image))
}

fn utf8_len(b: u8) -> usize {
    match b {
        0x00..=0x7F => 1,
        0xC0..=0xDF => 2,
        0xE0..=0xEF => 3,
        _ => 4,
    }
}

/// Drops the span list when it carries no information the plain text does not
/// already have -- a single unstyled run -- so the JSON stays small.
fn non_trivial(spans: Vec<Span>) -> Option<Vec<Span>> {
    let informative = spans
        .iter()
        .any(|s| s.bold || s.italic || s.strike || s.code || s.underline || s.href.is_some());
    if informative { Some(spans) } else { None }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn prov() -> Provenance {
        Provenance::new("test", "0", "test")
    }

    fn parse(md: &str) -> Vec<Block> {
        let mut ids = IdGen::new(1);
        parse_blocks(md, &mut ids, &prov())
    }

    #[test]
    fn heading_levels_and_text() {
        let b = parse("## Hello world");
        assert_eq!(b.len(), 1);
        assert_eq!(b[0].kind, BlockType::Heading);
        assert_eq!(b[0].level, Some(2));
        assert_eq!(b[0].text.as_deref(), Some("Hello world"));
    }

    #[test]
    fn hashtag_is_not_a_heading() {
        let b = parse("#nothashtag");
        assert_eq!(b[0].kind, BlockType::Paragraph);
    }

    #[test]
    fn rule_beats_bullet_list() {
        let b = parse("---");
        assert_eq!(b[0].kind, BlockType::Rule);
    }

    #[test]
    fn paragraph_joins_wrapped_lines() {
        let b = parse("one\ntwo\n\nthree");
        assert_eq!(b.len(), 2);
        assert_eq!(b[0].text.as_deref(), Some("one two"));
        assert_eq!(b[1].text.as_deref(), Some("three"));
    }

    #[test]
    fn fenced_code_keeps_newlines_and_lang() {
        let b = parse("```go\nx := 1\ny := 2\n```");
        assert_eq!(b[0].kind, BlockType::Code);
        assert_eq!(b[0].lang.as_deref(), Some("go"));
        assert_eq!(b[0].text.as_deref(), Some("x := 1\ny := 2"));
    }

    #[test]
    fn unterminated_fence_keeps_content() {
        let b = parse("```\nkept\n");
        assert_eq!(b[0].text.as_deref(), Some("kept"));
    }

    #[test]
    fn gfm_table_grid_and_header() {
        let b = parse("| a | b |\n|---|---|\n| 1 | 2 |");
        assert_eq!(b[0].kind, BlockType::Table);
        let t = b[0].table.as_ref().unwrap();
        assert_eq!(t.header_rows, 1);
        assert_eq!(t.grid.len(), 2);
        assert_eq!(t.grid[0].len(), 2);
        assert_eq!(t.grid[1][1].blocks[0].text.as_deref(), Some("2"));
    }

    #[test]
    fn bullet_list_items() {
        let b = parse("- one\n- two");
        let l = b[0].list.as_ref().unwrap();
        assert!(!l.ordered);
        assert_eq!(l.items.len(), 2);
        assert_eq!(l.items[0].blocks[0].text.as_deref(), Some("one"));
    }

    #[test]
    fn ordered_list_records_start() {
        let b = parse("3. three\n4. four");
        let l = b[0].list.as_ref().unwrap();
        assert!(l.ordered);
        assert_eq!(l.start, Some(3));
        assert_eq!(l.items.len(), 2);
    }

    #[test]
    fn task_list_checkbox() {
        let b = parse("- [x] done\n- [ ] todo");
        let l = b[0].list.as_ref().unwrap();
        assert_eq!(l.items[0].checked, Some(true));
        assert_eq!(l.items[1].checked, Some(false));
        assert_eq!(l.items[0].blocks[0].text.as_deref(), Some("done"));
    }

    #[test]
    fn nested_list_nests() {
        let b = parse("- outer\n  - inner");
        let l = b[0].list.as_ref().unwrap();
        assert_eq!(l.items.len(), 1);
        let inner = l.items[0].blocks.iter().find(|b| b.kind == BlockType::List);
        assert!(inner.is_some(), "expected a nested list block");
    }

    #[test]
    fn blockquote_nests_blocks() {
        let b = parse("> quoted text");
        assert_eq!(b[0].kind, BlockType::Quote);
        let inner = b[0].quote.as_ref().unwrap();
        assert_eq!(inner[0].text.as_deref(), Some("quoted text"));
    }

    #[test]
    fn lone_image_becomes_image_block() {
        let b = parse("![a cat](cat.png)");
        assert_eq!(b[0].kind, BlockType::Image);
        assert_eq!(b[0].alt.as_deref(), Some("a cat"));
        assert_eq!(b[0].asset_ref.as_deref(), Some("cat.png"));
    }

    #[test]
    fn image_with_trailing_text_stays_a_paragraph() {
        let b = parse("![a](x.png) and more");
        assert_eq!(b[0].kind, BlockType::Paragraph);
    }

    #[test]
    fn inline_plain_text_drops_spans() {
        let (plain, spans) = parse_inlines("just words");
        assert_eq!(plain, "just words");
        assert!(non_trivial(spans).is_none());
    }

    #[test]
    fn inline_bold_and_code() {
        let (plain, spans) = parse_inlines("a **b** and `c`");
        assert_eq!(plain, "a b and c");
        assert!(spans.iter().any(|s| s.text == "b" && s.bold));
        assert!(spans.iter().any(|s| s.text == "c" && s.code));
    }

    #[test]
    fn inline_link_keeps_text_and_href() {
        let (plain, spans) = parse_inlines("see [docs](http://x/y)");
        assert_eq!(plain, "see docs");
        assert!(
            spans
                .iter()
                .any(|s| s.text == "docs" && s.href.as_deref() == Some("http://x/y"))
        );
    }

    #[test]
    fn inline_escape_is_literal() {
        let (plain, _) = parse_inlines(r"literal \*not italic\*");
        assert_eq!(plain, "literal *not italic*");
    }

    #[test]
    fn unmatched_delimiter_is_literal() {
        let (plain, _) = parse_inlines("2 * 3 = 6");
        assert_eq!(plain, "2 * 3 = 6");
    }

    #[test]
    fn multibyte_text_is_not_split() {
        let (plain, _) = parse_inlines("héllo — wörld 日本語");
        assert_eq!(plain, "héllo — wörld 日本語");
    }

    #[test]
    fn block_ids_are_unique_and_positional() {
        let b = parse("# a\n\ntext\n\n# c");
        let ids: Vec<&str> = b.iter().map(|b| b.id.as_str()).collect();
        assert_eq!(ids, vec!["p1-b0", "p1-b1", "p1-b2"]);
    }
}

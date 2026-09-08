# PaddleX: `general_ocr_pipeline` is None on the table-cell-split path

Filed against PaddleX 3.7.2 (paddleocr 3.7.0, paddle 3.3.1).

`PPStructureV3.predict()` raises `AttributeError: 'NoneType' object has no
attribute 'text_rec_model'` for any table whose cells need splitting, when the
table recogniser does not own its own OCR models. It is deterministic, not a
race.

## Where it lands

`paddlex/inference/pipelines/table_recognition/pipeline_v2.py:704`

```python
ocr_result = list(
    self.general_ocr_pipeline.text_rec_model(   # <- general_ocr_pipeline is None
        ori_img[y1:y2, x1:x2, :]
    )
)[0]
```

inside `split_ocr_bboxes_by_table_cells()`. There is no `None` check.

## Why the attribute is None

`__init__`, line 146:

```python
self.use_ocr_model = config.get("use_ocr_model", True)
self.general_ocr_pipeline = None
if self.use_ocr_model:
    self.general_ocr_pipeline = self.create_pipeline(general_ocr_config)
else:
    self.general_ocr_config_bak = config.get("SubPipelines", {}).get("GeneralOCR", None)
```

Under PP-StructureV3 the detection and recognition models are configured at the
top level and handed to the table sub-pipeline with `use_ocr_model: false`, so
`general_ocr_pipeline` stays `None` and only the config is kept for a later
rebuild.

## Why the rebuild that exists for this case never runs

`predict()` sets, unconditionally, at line 1171:

```python
self.cells_split_ocr = True
```

and then, at line 1221, guards the lazy rebuild with:

```python
elif self.general_ocr_pipeline is None and (
    (
        use_ocr_results_with_table_cells == True
        and self.cells_split_ocr == False        # <- can never be False here
    )
    or use_table_orientation_classify == True
):
    assert self.general_ocr_config_bak != None
    self.general_ocr_pipeline = self.create_pipeline(self.general_ocr_config_bak)
```

The first clause requires `self.cells_split_ocr == False`, which line 1171 has
just made impossible. The two conditions in the same method are mutually
exclusive. So unless the caller also passes
`use_table_orientation_classify=True` — an unrelated feature — the pipeline is
never rebuilt.

## Why the crash is then reached

Line 1066, in the same `predict()`:

```python
if use_ocr_results_with_table_cells == True:      # the default
    if self.cells_split_ocr == True:              # always true, per line 1171
        ...
        table_ocr_pred = self.split_ocr_bboxes_by_table_cells(   # -> line 704
            table_cells_result, table_ocr_pred, image_array
        )
```

`use_ocr_results_with_table_cells` defaults to `True`, `cells_split_ocr` is
`True`, so every table takes the branch that dereferences the `None`. Whether it
actually crashes depends on the data: `split_ocr_bboxes_by_table_cells` only
reaches line 704 for cells it decides to split.

The sibling call site at line 755 (`gen_ocr_with_table_cells`, the
`cells_split_ocr == False` branch) has the same exposure.

## Reproduction

Any PP-StructureV3 configuration where the table sub-pipeline has
`use_ocr_model: false`, on a document containing a table with cells the splitter
subdivides. Calling with `use_table_orientation_classify=True` masks it, because
the second clause of the line-1221 guard then builds the pipeline.

## Suggested fix

Either is sufficient; the first is the smaller change.

1. **Correct the guard at line 1221.** The `cells_split_ocr == False` clause
   looks like it was written before line 1171 became unconditional. Requiring
   `use_ocr_results_with_table_cells == True` alone — regardless of
   `cells_split_ocr` — builds the pipeline exactly when a later line will need
   it.

2. **Guard the dereference at 704 (and 755).** If `general_ocr_pipeline` is
   `None`, fall back to the text already found for that region by the overall
   OCR pass rather than re-reading the crop.

A `None` check at the call sites is worth having in either case: right now a
missing sub-pipeline surfaces as an `AttributeError` from inside a numpy slice
loop, several frames away from the configuration that caused it.

## Workaround in dolico

`python/ocr-service/dolico_ocr/structure.py` passes
`use_ocr_results_with_table_cells=False` to `predict()`, taking the branch that
reads the table from the page's own OCR result instead of per cell. Output on
our table fixtures is byte-identical, so on this configuration the per-cell
re-read was buying nothing.

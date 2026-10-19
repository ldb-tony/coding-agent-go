---
name: pdf
description: Extract text and tables from PDF files, fill PDF forms, and merge or split documents. Use when working with PDF documents.
---

# PDF Processing

Always inspect the file before processing it: `pdfinfo document.pdf` prints the page
count, page size, and whether the document is encrypted.

## Extract text

```bash
pdftotext -layout document.pdf -      # keep the visual layout for tables
pdftotext -f 3 -l 7 document.pdf -    # only pages 3..7
```

## Extract tables

`pdftotext -layout` keeps columns aligned; parse the result line by line instead of
assuming fixed-width fields. For bordered tables prefer `camelot` or `tabula`.

## Merge and split

```bash
pdfunite a.pdf b.pdf out.pdf
pdfseparate -f 2 -l 4 in.pdf page-%d.pdf
```

## Fill forms

Use `pdftk` for AcroForm fields: `pdftk form.pdf fill_form values.fdf output filled.pdf`.

## Common pitfalls

- Encrypted PDFs fail silently in most tools — detect this first and report it
  instead of returning empty output.
- Scanned PDFs contain no text layer; say so explicitly rather than returning an
  empty extraction, and suggest OCR (`ocrmypdf`).

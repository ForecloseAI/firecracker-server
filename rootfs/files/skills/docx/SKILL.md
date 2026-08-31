---
name: docx
description: Read, edit or write a Word document (.docx, and .doc after converting). Use whenever a Word file is the input or the output, including when the person sends one, asks what a document says, asks for a letter, report, contract or CV, or asks for something as a Word file.
---

# Working with Word documents

Python is on PATH already and has python-docx. Do not install anything.

## Read one

```python
import docx
d = docx.Document("/home/agent/workspace/uploads/x.docx")
for p in d.paragraphs:
    if p.text.strip():
        print(p.text)
```

Tables are separate from paragraphs and easy to miss. A contract's real content
is often entirely in tables:

```python
for t in d.tables:
    for row in t.rows:
        print([c.text for c in row.cells])
```

If you only read `paragraphs` and the document looked oddly empty, that is why.

## A legacy .doc

python-docx cannot open the old format. Convert first, then read the result:

```bash
soffice --headless --convert-to docx --outdir . old.doc
```

## Write one

```python
import docx
d = docx.Document()
d.add_heading("Quarterly summary", level=1)
d.add_paragraph("Revenue was up 12 percent.")
t = d.add_table(rows=1, cols=2)
t.style = "Table Grid"
t.rows[0].cells[0].text = "Item"
d.save("/home/agent/workspace/summary.docx")
```

Use real headings with `add_heading` rather than bold paragraphs. They become
the document's structure, so a table of contents and navigation work, and
whoever opens it can restyle the whole thing at once.

Give a table a style. The default has no borders and prints as floating text.

## Editing an existing document

Save to a NEW name. Never overwrite what the person sent until they have said
the result is right.

Editing paragraph text through `p.text = ...` throws away that paragraph's
formatting, because the formatting lives on the runs inside it. To keep it,
change the run:

```python
for p in d.paragraphs:
    for r in p.runs:
        r.text = r.text.replace("2025", "2026")
```

A phrase split across two runs, which happens whenever part of it is bold, will
not match. If a replacement silently does nothing, that is the reason. Print the
runs and see.

## Turning markdown into Word

Faster than building it paragraph by paragraph when there is no special
formatting:

```bash
pandoc notes.md -o notes.docx
```

## Showing the result

To see what it actually looks like, convert to PDF and render a page:

```bash
soffice --headless --convert-to pdf --outdir . out.docx
pdftoppm -png -r 100 -f 1 -l 1 out.pdf page
```

Do that when the layout matters, not routinely. It costs a lot of tokens to
look at an image.

## Before you finish

Say where the file is. The person cannot download it from the app yet, so put
the substance in the chat too, and offer to hand over the screen if they need to
see the document itself.

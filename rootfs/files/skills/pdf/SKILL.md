---
name: pdf
description: Read a PDF, pull out its text and tables, split or merge one, or render a page as an image you can look at. Use whenever a PDF is involved at all, including when the person sends a bill, statement, invoice, contract, report or scan, asks what a document says, or asks you to pull numbers out of one.
---

# Working with PDFs

Python is on PATH already and has what you need. Do not install anything.

## Read the text

```python
import pdfplumber
with pdfplumber.open("/home/agent/workspace/uploads/x.pdf") as pdf:
    print(f"{len(pdf.pages)} pages")
    for page in pdf.pages:
        print(page.extract_text() or "")
```

Check the page count first and read what you need. A long PDF dumped whole
fills your context and is re-sent on every later turn.

## Read the tables

`page.extract_table()` gives the first table on a page as a list of rows.
`page.extract_tables()` gives all of them.

```python
for row in page.extract_table() or []:
    print(row)
```

Tables in PDFs are guesswork by the library, not structure in the file. Print
what came back and check it against the page before you use the numbers. If the
columns came out wrong, read the text instead and pull the figures by hand.

## When there is no text

A scan is an image and `extract_text` returns nothing. That is a finding, not a
failure. Say the PDF is a scan and that you cannot read it. Do not guess at what
it probably says. If it matters enough, ask the person whether to install OCR,
which is `sudo /opt/agent/venv/bin/pip install pytesseract pdf2image` plus
`sudo apt-get install -y tesseract-ocr`.

## Look at a page

When the layout matters, or the text is confusing, render it and open the
image:

```bash
pdftoppm -png -r 100 -f 1 -l 1 input.pdf /home/agent/workspace/page
```

That writes `page-1.png`. Rendering costs a lot of tokens to look at, so do it
when reading has actually failed you, not first.

## Split, merge, rotate

```python
from pypdf import PdfReader, PdfWriter
w = PdfWriter()
for p in PdfReader("a.pdf").pages[0:5]:
    w.add_page(p)
w.write("first-five.pdf")
```

`PdfWriter.append("b.pdf")` merges a whole file in.

## Making a PDF

Two steps, and it has to be both. `pandoc notes.md -o notes.pdf` fails on this
machine with "pdflatex not found", because pandoc writes PDFs through LaTeX and
there is no LaTeX here. Go through Word format instead:

```bash
pandoc notes.md -o notes.docx
soffice --headless --convert-to pdf --outdir . notes.docx
```

The second command is also how you get a PDF of any Word, Excel or PowerPoint
file you already have. It takes a few seconds the first time it runs.

## Before you finish

Say where the file is. The person cannot download it from the app yet, so a
file they cannot see is not a delivered result. If they need to look at it,
offer to hand over the screen.

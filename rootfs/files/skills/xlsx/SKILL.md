---
name: xlsx
description: Read, edit or build a spreadsheet - .xlsx, .xlsm and .csv. Use whenever a spreadsheet is the input or the output, including when the person sends one, asks for a total or a breakdown from their data, asks you to add rows or a column, or asks for something as a spreadsheet.
---

# Working with spreadsheets

Python is on PATH already and has openpyxl and pandas. Do not install anything.

## Look before you touch

Open it and find out what is actually there. Sheet names, the header row, how
far the data goes. Guessing the shape is how you end up summing a column of
dates.

```python
import openpyxl
wb = openpyxl.load_workbook("/home/agent/workspace/uploads/x.xlsx")
print(wb.sheetnames)
ws = wb["Sheet1"]
print(ws.max_row, ws.max_column)
for row in ws.iter_rows(min_row=1, max_row=5, values_only=True):
    print(row)
```

The header is not always row 1. Some sheets have a title and a blank line
first. Print the first few rows and see.

## Read the numbers

For anything analytical use pandas, and say which header row you found:

```python
import pandas as pd
df = pd.read_excel("x.xlsx", sheet_name="Sheet1", header=0)
print(df.dtypes)
print(df.describe())
```

Check `dtypes` before you trust a total. A column that came in as `object`
usually has text in it somewhere, often a stray "N/A" or a currency symbol, and
summing it will either fail or quietly be wrong.

## Formulas stay formulas

`load_workbook(path)` gives you formulas. `load_workbook(path, data_only=True)`
gives you the values Excel last calculated, and `None` for any cell Excel has
not calculated yet.

When you write a formula, write the formula, not the number you worked out:

```python
ws["D2"] = "=B2*C2"
```

A pasted number looks identical and stops updating the moment anyone edits the
row. That is the mistake worth avoiding here. Only write a literal when the
person asked for a snapshot.

## Editing an existing file

Load, change, save to a NEW name. Never overwrite the file the person sent
until they have said the result is right.

```python
wb = openpyxl.load_workbook("in.xlsx")
ws = wb.active
ws.append(["2026-08-31", "Stationery", 420])
wb.save("/home/agent/workspace/out.xlsx")
```

openpyxl does not keep charts, images or pivot tables through a load and save.
If the file has any, say so before you edit it, and offer to make a new sheet
instead of rewriting theirs.

## Building one from scratch

```python
import pandas as pd
pd.DataFrame(rows).to_excel("out.xlsx", index=False, sheet_name="Summary")
```

Give it a header row and sensible column names. Format dates as dates and
numbers as numbers, not as text.

## CSV

`pd.read_csv` and `df.to_csv(index=False)`. Watch the encoding on anything
exported from a bank or an Indian government portal: try `encoding="utf-8-sig"`
when the first column name comes back with junk in front of it.

## A legacy .xls

openpyxl cannot open the old format. Convert first:

```bash
soffice --headless --convert-to xlsx --outdir . old.xls
```

## Before you finish

Say where the file is and what is in it. Give the person the headline numbers in
the chat as well, because they cannot download the file from the app yet.

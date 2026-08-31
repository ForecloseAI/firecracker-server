---
name: pptx
description: Read, edit or build a PowerPoint deck (.pptx, and .ppt after converting). Use whenever a slide deck is the input or the output, including when the person sends one, asks what is in a deck, or asks you to make slides, a pitch deck or a presentation.
---

# Working with slide decks

Python is on PATH already and has python-pptx. Do not install anything.

## Read one

```python
from pptx import Presentation
p = Presentation("/home/agent/workspace/uploads/x.pptx")
for i, slide in enumerate(p.slides, 1):
    print(f"--- slide {i} ---")
    for shape in slide.shapes:
        if shape.has_text_frame:
            print(shape.text_frame.text)
```

Speaker notes are where the argument often lives, and they are not in the
shapes:

```python
if slide.has_notes_slide:
    print(slide.notes_slide.notes_text_frame.text)
```

A legacy `.ppt` needs converting first:
`soffice --headless --convert-to pptx --outdir . old.ppt`.

## Build one

Start from a layout rather than placing text boxes by hand. The layouts carry
the theme, so text you put in a placeholder inherits the fonts and colours, and
text in a free-floating box does not.

```python
from pptx import Presentation
from pptx.util import Inches, Pt

p = Presentation()                 # or Presentation("their-template.pptx")
title_layout, bullets_layout = p.slide_layouts[0], p.slide_layouts[1]

s = p.slides.add_slide(title_layout)
s.shapes.title.text = "Quarterly review"
s.placeholders[1].text = "August 2026"

s = p.slides.add_slide(bullets_layout)
s.shapes.title.text = "What changed"
body = s.placeholders[1].text_frame
body.text = "Revenue up 12 percent"
body.add_paragraph().text = "Two new customers"

p.save("/home/agent/workspace/review.pptx")
```

If the person has a template or an existing deck, open THAT as the starting
point instead of a blank Presentation. Their branding comes free and the result
does not look like a default deck.

## Writing the slides themselves

Few words per slide. A slide is a headline plus three or four short lines, and
anything longer belongs in the notes. If you find yourself writing a paragraph
into a bullet, put it in `notes_slide` instead.

Default `Presentation()` is 4:3. For widescreen set
`p.slide_width, p.slide_height = Inches(13.333), Inches(7.5)` before adding
slides.

## Look at what you made

python-pptx will happily produce overlapping shapes and text that overflows its
box, and you cannot tell from the code. When layout matters, render and look:

```bash
soffice --headless --convert-to pdf --outdir . review.pptx
pdftoppm -png -r 80 -f 1 -l 3 review.pdf slide
```

Check the first few slides, not all of them.

## Before you finish

Send it with send_file, and say how many slides it has. Summarise the deck in
the chat as well: that is what they read on a phone, and the file is what they
open when they want the detail.

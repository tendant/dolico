# corpus-hard

Real scans, kept out of `testdata/` on purpose.

Everything in `testdata/` is generated, so its ground truth is *exact* — written
from the same constants that drew the fixture. Nothing here is. The expectation
below was transcribed by reading the scan, which makes it evidence rather than a
regression fixture, and means a mistake in the transcription is possible in a
way it is not for the generated corpus.

It exists to answer one question the synthetic corpus cannot: does a real bad
scan behave like `testdata/faded.pdf`, or was that fixture only difficult in a
way we invented?

Score it with:

```bash
make bench-hard      # forces escalation -- see below
```

## radio-1922.pdf

| | |
| --- | --- |
| Source | *Evening Star* (Washington, D.C.), 25 August 1922, image 10, column 1 |
| Held by | Library of Congress, Chronicling America / National Digital Newspaper Program |
| Identifier | LCCN `sn83045462`, batch `dlc_dalek_ver01` |
| Retrieved from | `https://tile.loc.gov/image-services/iiif/service:ndnp:dlc:batch_dlc_dalek_ver01:data:sn83045462:00280657165:1922082501:0721/pct:0.6,28.13,12.85,6.17/full/0/default.jpg` |
| Rights | Published in the United States in 1922: public domain. The Library of Congress states these newspaper pages carry no known restrictions. |
| Retrieved | 9 August 2026 |

**What was done to it.** One region of one page was requested from the IIIF
service — a nine-item radio schedule from the "BY RADIO TODAY" column — and
wrapped in a PDF at 200 DPI, so the page is 241.6 × 152.3 pt and the service's
default rasterization returns the original pixels rather than a resampling of
them. No enhancement, deskewing, or cleanup: the mottling, the broken serifs and
the film grain are the microfilm's own.

The region was chosen for three reasons: the content is mundane (broadcast
times, weather, market reports), every character in it is legible without
guessing, and it is short enough to transcribe reliably. An adjacent listing was
excluded because one digit in it is damaged past the point where "5" and "6" can
be told apart, and a transcription that guesses is worse than no fixture.

**A caveat on the numbers.** The expectation uses the em dashes the paper is set
in. An engine that emits `-` instead is charged for it by CER even though it
read the page correctly, so the absolute error rates here are less meaningful
than the comparison between tiers against the same expectation.

### What it measures

Both tiers, on this page, at the time it was added:

| | text |
| --- | --- |
| ground truth | `11:15 to 11:20 a.m.—Hog flash—Chicago and St. Louis. …` |
| Tier 2, `pp-structurev3` | `11:13to 11:20a.im.—-Ho8nasne Chicago and st..Louis. …` |
| Tier 3, `mineru` | `11:15 to 11:20 a.m.—Hog hash—Chicago and St. Louis. …` |

OCR mangles the times (`11:13` for `11:15`, `4t0` for `4 to`), loses word
boundaries, and turns "Hog flash" into "Ho8nasne". The vision tier misses one
letter on the whole page.

**And it reports 0.938 confidence while doing it.** That is why `bench-hard`
forces escalation with `DOLICO_OCR_THRESHOLD=0.99 DOLICO_VISION_THRESHOLD=0.98`
rather than using the production defaults: with the real thresholds this page
scores about 0.61 and Tier 3 is never called. The forced run measures what the
vision tier *can* recover; it is not what the pipeline would do.

That gap is the finding, not a workaround. Per-page quality scoring catches a
page OCR gave up on — `testdata/faded.pdf` is that case, and it escalates on the
real defaults — but it cannot catch a page OCR read wrongly and confidently,
because every signal available without a second opinion says the page is fine.
See the vision tier design doc for what would close it.

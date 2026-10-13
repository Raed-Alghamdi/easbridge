# EASBridge logo guide

The name is written **EASBridge** (EAS for Exchange ActiveSync, then Bridge) everywhere it is displayed. Lower case stays only for identifiers: the repository, file names, image names and the app id `io.github.easbridge`.

## The mark
A stone arch of five blocks on two legs. The centre block (the keystone) is the accent colour. It stands for the gateway between a closed protocol (ActiveSync) and open ones (IMAP, SMTP, CardDAV). Drawn on a 64 unit grid: block gaps are an even 3 units, legs are as wide as the arch ring.

## Files (all in `brand/`)
| Use | File |
|---|---|
| Mark | `mark.svg`, `mark-dark.svg`, `mark-black.svg`, `mark-white.svg` |
| Small mark (16 to 39 px) | `mark-favicon*.svg`, adaptive `favicon.svg` |
| Horizontal lockup | `lockup-horizontal*.svg` |
| Stacked lockup | `lockup-stacked*.svg` |
| README header | `readme-header-light.svg`, `readme-header-dark.svg` |
| GitHub social preview | `social-preview.png` (1280 x 640), source `social-preview.svg` |
| Web icons | `icons/web/` (favicon.ico built from the small mark) |

README snippet:
```html
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="brand/readme-header-dark.svg">
  <img alt="EASBridge" src="brand/readme-header-light.svg" width="450">
</picture>
```

## Colours
| Role | Light background | Dark background |
|---|---|---|
| Accent (keystone) | `#0F766E` (OKLCH 0.511 0.086 186.4) | `#3CCBB8` (OKLCH 0.763 0.122 182.1) |
| Ink | `#14202B` (OKLCH 0.238 0.027 246.9) | `#E8EEF2` (OKLCH 0.946 0.008 236.6) |
| Social preview background | | `#0F1519` |

One colour: use `-black` on light and `-white` on dark. The keystone then differs only by its gaps.

## Clear space
Keep free space of at least one leg width (about a quarter of the mark height) on every side of the mark or lockup. Nothing else may enter it.

## Minimum sizes
- Mark: full mark from 40 px high. Below that, use the small mark (`mark-favicon`), which has 3 blocks, 1 px gaps at 16 px and edges aligned to the 16 px pixel grid. The small mark is made for 16 and 32 px (also 48).
- Horizontal lockup: at least 28 px high for the mark part (about 130 px wide). Smaller than that, use the small mark alone.
- Stacked lockup: at least 64 px wide.
- Do not use the full mark at 16 px: the five-block gaps blur.

## Wordmark
JetBrains Mono Bold, converted to paths, "EASBridge" with a capital E, A, S and B. Never retype it in another font or change the spacing. See `SOURCES.md`.

## Misuse
- Do not recolour the keystone or use more than one accent colour.
- Do not use Microsoft blue, or place the mark on a blue that looks like Outlook or Exchange.
- Do not write the name in lower case or all caps ("easbridge", "EASBRIDGE") in display use.
- Do not stretch, rotate, outline, add shadows or gradients, or put the mark inside an envelope.
- Do not put the dark-background colours on light backgrounds or the reverse (the accent drops below 3:1 contrast).
- Do not add the five-block mark to favicons: use the small mark.

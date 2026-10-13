import math, os
from fontTools.ttLib import TTFont
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.pens.boundsPen import BoundsPen

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..')  # the brand folder
SCHEMES = {'light': ('#14202B', '#0F766E'), 'dark': ('#E8EEF2', '#3CCBB8'),
           'black': ('#000000', '#000000'), 'white': ('#FFFFFF', '#FFFFFF')}
NAME = 'EASBridge'

def load(path):
    f = TTFont(path); return f.getGlyphSet(), f.getBestCmap()
import os
FONT_DIR = os.environ.get('JETBRAINS_MONO_DIR', '/usr/share/fonts/TTF')  # a folder with JetBrainsMonoNerdFont-Bold.ttf and -Regular.ttf
BOLD = load(os.path.join(FONT_DIR, 'JetBrainsMonoNerdFont-Bold.ttf'))
REG = load(os.path.join(FONT_DIR, 'JetBrainsMonoNerdFont-Regular.ttf'))

def text_path(font, text, size, x0=0, base=0):
    """Returns (path d, ink width). Left ink edge placed at x0."""
    gs, cmap = font; s = size / 1000
    bp = BoundsPen(gs); x = 0
    for ch in text:
        g = cmap[ord(ch)]; gs[g].draw(TransformPen(bp, (s, 0, 0, -s, x, 0))); x += gs[g].width * s
    xmin, _, xmax, _ = bp.bounds
    d = []; x = x0 - xmin
    for ch in text:
        g = cmap[ord(ch)]; pen = SVGPathPen(gs, ntos=lambda v: f'{v:.2f}'.rstrip('0').rstrip('.'))
        gs[g].draw(TransformPen(pen, (s, 0, 0, -s, x, base))); d.append(pen.getCommands()); x += gs[g].width * s
    return ' '.join(d), xmax - xmin

def pt(cx, cy, rho, deg): return cx + rho * math.cos(math.radians(deg)), cy - rho * math.sin(math.radians(deg))

def edge(cx, cy, rho, deg, g, side):
    """Point on circle rho at the block edge angle, offset by g/2 perpendicular to the radius (side=-1 for block start, +1 for end)."""
    if g == 0 or deg in (0, 180): return pt(cx, cy, rho, deg)
    t = math.sqrt(rho * rho - (g / 2) ** 2); a = math.radians(deg)
    ux, uy = math.cos(a), -math.sin(a); vx, vy = -math.sin(a), -math.cos(a)
    return (cx + t * ux + side * g / 2 * vx, cy + t * uy + side * g / 2 * vy)

def arch(cx, cy, R, r, g, bounds, key, leg_bottom, leg_w=None):
    f = lambda v: f'{v:.2f}'.rstrip('0').rstrip('.')
    blocks = []
    for k in range(len(bounds) - 1):
        a0, a1 = bounds[k], bounds[k + 1]
        p0o, p0i = edge(cx, cy, R, a0, g, -1), edge(cx, cy, r, a0, g, -1)
        p1o, p1i = edge(cx, cy, R, a1, g, 1), edge(cx, cy, r, a1, g, 1)
        d = (f'M{f(p0o[0])} {f(p0o[1])}A{R} {R} 0 0 1 {f(p1o[0])} {f(p1o[1])}L{f(p1i[0])} {f(p1i[1])}'
             f'A{r} {r} 0 0 0 {f(p0i[0])} {f(p0i[1])}Z')
        blocks.append((d, k == key))
    top = cy + g
    l, r2 = cx - R, cx - r
    legs = (f'M{f(l)} {f(top)}H{f(r2)}V{f(leg_bottom)}H{f(l)}Z M{f(cx + r)} {f(top)}H{f(cx + R)}V{f(leg_bottom)}H{f(cx + r)}Z')
    return blocks, legs

def mark_body(kind, scheme):
    ink, acc = SCHEMES[scheme]
    if kind == 'full':
        blocks, legs = arch(32, 34, 29, 14, 3, [180, 144, 108, 72, 36, 0], 2, 59)
    else:  # favicon: 16px grid, every x4 unit is one pixel
        blocks, legs = arch(32, 36, 28, 12, 4, [180, 115, 65, 0], 1, 56)
    out = ''.join(f'<path fill="{acc if key else ink}" d="{d}"/>' for d, key in blocks)
    return out + f'<path fill="{ink}" d="{legs}"/>'

def svg(vb, body, title, extra='', w=None, h=None):
    x, y, ww, hh = vb
    size = f' width="{w}" height="{h}"' if w else ''
    return f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="{x} {y} {ww:.1f} {hh:.1f}"{size} role="img"><title>{title}</title>{extra}{body}</svg>\n'

WS = 62                                   # wordmark size, caps height 0.73*62 = 45
CAP = 0.73 * WS
BASE = 32 + CAP / 2                       # caps centred on the mark's centre line
GAP = 20
_, WW = text_path(BOLD, NAME, WS)
H_W = 64 + GAP + WW
VB_H = (0, -6, H_W, 76)
S_MARK = 128; S_GAP = 26
ST_W = WW + 0; ST_H = S_MARK + S_GAP + CAP + 0.21 * WS

def horizontal(scheme):
    ink, _ = SCHEMES[scheme]
    d, _ = text_path(BOLD, NAME, WS, 64 + GAP, BASE)
    return svg(VB_H, mark_body('full', scheme) + f'<path fill="{ink}" d="{d}"/>', f'{NAME} logo')

def stacked(scheme):
    ink, _ = SCHEMES[scheme]
    base = S_MARK + S_GAP + CAP
    d, _ = text_path(BOLD, NAME, WS, 0, base)
    m = f'<g transform="translate({(ST_W - S_MARK) / 2:.2f} 0) scale({S_MARK / 64})">{mark_body("full", scheme)}</g>'
    return svg((0, 0, ST_W, ST_H), m + f'<path fill="{ink}" d="{d}"/>', f'{NAME} logo, stacked')

def header(scheme):
    ink, _ = SCHEMES[scheme]; W, Hh = 900, 240; s = 2.0
    lw, lh = H_W * s, 76 * s
    d, _ = text_path(BOLD, NAME, WS, 64 + GAP, BASE)
    g = (f'<g transform="translate({(W - lw) / 2:.2f} {(Hh - lh) / 2 + 6 * s:.2f}) scale({s})">'
         f'{mark_body("full", scheme)}<path fill="{ink}" d="{d}"/></g>')
    return svg((0, 0, W, Hh), g, f'{NAME}: Exchange ActiveSync to IMAP, SMTP and CardDAV')

def write(name, s): open(f'{OUT}/{name}', 'w').write(s)
for sch in SCHEMES:
    suf = '' if sch == 'light' else '-' + sch
    write(f'mark{suf}.svg', svg((0, 0, 64, 64), mark_body('full', sch), f'{NAME} mark'))
    write(f'mark-favicon{suf}.svg', svg((0, 0, 64, 64), mark_body('fav', sch), f'{NAME} favicon mark'))
    write(f'lockup-horizontal{suf}.svg', horizontal(sch))
    write(f'lockup-stacked{suf}.svg', stacked(sch))
write('readme-header-light.svg', header('light')); write('readme-header-dark.svg', header('dark'))
# adaptive favicon for browsers
st = '<style>.i{fill:#14202B}.a{fill:#0F766E}@media(prefers-color-scheme:dark){.i{fill:#E8EEF2}.a{fill:#3CCBB8}}</style>'
b = mark_body('fav', 'light').replace('fill="#14202B"', 'class="i"').replace('fill="#0F766E"', 'class="a"')
write('favicon.svg', svg((0, 0, 64, 64), b, f'{NAME} favicon', st))

# social preview source (1280x640)
W, Hh, s = 1280, 640, 1.9
lw = H_W * s; tag = 'Exchange ActiveSync in. IMAP, SMTP and CardDAV out.'
td, tw = text_path(REG, tag, 30, 0, 0)
d, _ = text_path(BOLD, NAME, WS, 64 + GAP, BASE)
ink, acc = SCHEMES['dark']
soc = (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {Hh}"><rect width="{W}" height="{Hh}" fill="#0F1519"/>'
       f'<g transform="translate({(W - lw) / 2:.2f} {Hh / 2 - 38 * s - 28:.2f}) scale({s})">{mark_body("full", "dark")}<path fill="{ink}" d="{d}"/></g>'
       f'<g transform="translate({(W - tw) / 2:.2f} {Hh / 2 + 118:.2f})"><path fill="#9FB0BA" d="{td}"/></g></svg>')
open(f'{OUT}/social-preview.svg', 'w').write(soc)
print('wordmark width', WW, 'lockup', H_W)

"""Potflix identity pass 2 - Ribbon P + heavy wordmark (stdlib + Pillow only).

Run:  python design\\make_logo.py
Renders at 8x supersample, downsamples with Image.LANCZOS. Flat fills only.

Palette (exact):
  RED   #E50914  lit ribbon facets, badges
  DARK  #8E060C  receding ribbon facet (brand red scaled by 0.62, rounded)
  BLACK #0B0B0B  tiles, wordmark-on-light
  WHITE #FFFFFF  wordmark-on-black, badge glyphs
  LIGHT #F5F5F5  candidate-sheet light panel only (not brand)

Fonts live in design\\fonts\\ (OFL, alongside <family>-OFL.txt).
Chosen wordmark: Archivo Black (single-master display black, even color,
tight grotesque caps; condensed faces lose legibility across 7 glyphs on a
square tile, geometric faces read soft).

Outputs:
  design\\preview\\logo-candidates.png  (6-typeface labelled sheet)
  assets\\discord\\logo.png   1024x1024 chosen lockup on black rounded tile
  assets\\appicon.png          512x512 Ribbon P on black rounded tile
  assets\\discord\\play|pause|stop.png  1024x1024 red-disc badge set
  assets\\icon.ico             ONE 256x256 BMP/DIB frame, transparent corners
  design\\preview\\card-*.png  refreshed communication-aid cards
"""

import os
import struct
from PIL import Image, ImageDraw, ImageFont

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SS = 8  # supersample factor: the crispness source

# ---------------------------------------------------------------- palette
RED = (0xE5, 0x09, 0x14, 255)  # #E50914 brand red (lit facets)
DARK = (0x8E, 0x06, 0x0C, 255)  # #8E060C = round(#E50914 * 0.62)
BLACK = (0x0B, 0x0B, 0x0B, 255)  # #0B0B0B tiles
WHITE = (0xFF, 0xFF, 0xFF, 255)  # #FFFFFF wordmark / glyphs
LIGHT = (0xF5, 0xF5, 0xF5, 255)  # sheet light panel only
CARD = (0x23, 0x24, 0x28, 255)
CARD_HDR = (0xF2, 0xF3, 0xF5, 255)
CARD_DET = (0xDB, 0xDE, 0xE1, 255)
CARD_MUT = (0xB5, 0xBA, 0xC1, 255)

PALETTE_REPORT = (
    "RED=#E50914 DARK=#8E060C(round(#E50914*0.62)) BLACK=#0B0B0B WHITE=#FFFFFF"
)

# ---------------------------------------------------------------- type data
FDIR = os.path.join(ROOT, "design", "fonts")
FONTS = {  # key: file, expected family, variable->named instance, tracking em
    "archivo": dict(
        file="ArchivoBlack-Regular.ttf", family="Archivo Black", named=None, track=0.08
    ),
    "montserrat-black": dict(
        file="Montserrat-Variable.ttf", family="Montserrat", named=b"Black", track=0.10
    ),
    "poppins-black": dict(
        file="Poppins-Black.ttf", family="Poppins", named=None, track=0.08
    ),
    "poppins-xbold": dict(
        file="Poppins-ExtraBold.ttf", family="Poppins", named=None, track=0.08
    ),
    "anton": dict(file="Anton-Regular.ttf", family="Anton", named=None, track=0.06),
    "bebas": dict(
        file="BebasNeue-Regular.ttf", family="Bebas Neue", named=None, track=0.10
    ),
}
CHOSEN = "archivo"
INTER = dict(file="Inter-Variable.ttf", family="Inter")

# tight-but-even manual kern pairs (em units, applied on top of tracking)
KERN = {
    ("T", "F"): -0.020,
    ("F", "L"): -0.030,
    ("L", "I"): -0.010,
    ("P", "O"): -0.012,
    ("O", "T"): -0.012,
    ("I", "X"): +0.008,
}


def load_font(spec, size):
    """Truetype load + named-instance select + family assert (no silent fallback)."""
    path = os.path.join(FDIR, spec["file"])
    f = ImageFont.truetype(path, size)
    if spec.get("named"):
        f.set_variation_by_name(spec["named"])
    got = f.getname()[0]
    assert got == spec["family"], "font fallback: %s -> %s" % (path, got)
    return f


def load_inter(size, named):
    f = ImageFont.truetype(os.path.join(FDIR, INTER["file"]), size)
    f.set_variation_by_name(named)
    assert f.getname()[0] == "Inter"
    return f


# ---------------------------------------------------------------- primitives
def new(size, color=(0, 0, 0, 0)):
    return Image.new("RGBA", (int(size[0] * SS), int(size[1] * SS)), color)


def down(img, mode="RGBA"):
    w, h = img.size
    return img.resize((w // SS, h // SS), Image.LANCZOS).convert(mode)


def rr(d, box, r, fill):
    d.rounded_rectangle([b * SS for b in box], radius=int(r * SS), fill=fill)


def tile(size, bg):
    """Rounded-square tile on transparency (corners stay transparent)."""
    img = new((size, size))
    rr(ImageDraw.Draw(img), (0, 0, size, size), size * 0.225, bg)
    return img


# ---------------------------------------------------------------- Ribbon P
# Grid: box is 24u x 24u. All strokes 5u. Counter punched in tile bg.
# Facets: (1) front face RED, (2) receding ribbon plane DARK offset (+ex,+ex),
# (3) diagonal fold crease DARK across the bowl upright.
def ribbon_polys(x0, y0, W):
    u = W / 24.0
    X = lambda v: x0 + v * u
    Y = lambda v: y0 + v * u
    stem = [(X(4), Y(2)), (X(9), Y(2)), (X(9), Y(22)), (X(4), Y(22))]
    top = [(X(4), Y(2)), (X(20), Y(2)), (X(20), Y(7)), (X(4), Y(7))]
    upright = [(X(15), Y(7)), (X(20), Y(7)), (X(20), Y(11)), (X(15), Y(11))]
    mid = [(X(4), Y(11)), (X(20), Y(11)), (X(20), Y(16)), (X(4), Y(16))]
    counter = [(X(9), Y(7)), (X(15), Y(7)), (X(15), Y(11)), (X(9), Y(11))]
    crease = [(X(15), Y(9.0)), (X(20), Y(7.6)), (X(20), Y(9.1)), (X(15), Y(10.5))]
    return [stem, top, upright, mid], counter, crease, 1.2 * u


def ribbon_p(img, box):
    """Ribbon P mark. box=(x0,y0,x1,y1) in 1x units; img is SS-scale RGBA."""
    x0, y0, x1, y1 = box
    W = x1 - x0
    polys, counter, crease, ex = ribbon_polys(x0, y0, W)
    d = ImageDraw.Draw(img)

    def poly(pts, dx=0.0, dy=0.0, fill=None):
        d.polygon([((x + dx) * SS, (y + dy) * SS) for x, y in pts], fill=fill)

    for p in polys:  # receding ribbon plane
        poly(p, ex, ex, DARK)
    for p in polys:  # lit front face
        poly(p, 0, 0, RED)
    poly(counter, 0, 0, BLACK)  # bowl counter
    poly(crease, 0, 0, DARK)  # fold crease


# ---------------------------------------------------------------- wordmark
def tracked_width(font, text, track_em):
    w, size = 0.0, font.size
    for i, ch in enumerate(text):
        w += font.getlength(ch) + track_em * size
        if i + 1 < len(text):
            w += KERN.get((ch, text[i + 1]), 0.0) * size
    return w


def draw_wordmark(img, box, key, fill):
    """POTFLIX in heavy caps, glyph-by-glyph manual tracking. Returns family."""
    spec = FONTS[key]
    x0, y0, x1, y1 = box
    target = (x1 - x0) * 0.98
    probe = load_font(spec, 100)
    size = max(8, int(100 * target / tracked_width(probe, "POTFLIX", spec["track"])))
    font = load_font(spec, size * SS)
    fam = font.getname()[0]
    d = ImageDraw.Draw(img)
    total = tracked_width(font, "POTFLIX", spec["track"])
    cx = (x0 + x1) / 2 * SS
    ymid = (y0 + y1) / 2 * SS
    x = cx - total / 2
    for i, ch in enumerate("POTFLIX"):
        d.text((x, ymid), ch, font=font, fill=fill, anchor="lm")
        x += font.getlength(ch) + spec["track"] * font.size
        if i + 1 < 7:
            x += KERN.get((ch, "POTFLIX"[i + 1]), 0.0) * font.size
    return fam


# ---------------------------------------------------------------- artifacts
def render_mark_tile(size, margin_frac=0.21):
    t = tile(size, BLACK)
    m = size * margin_frac
    ribbon_p(t, (m, m, size - m, size - m))
    return down(t)


def render_lockup(size, key):
    """Chosen lockup: Ribbon P over POTFLIX on black rounded tile."""
    t = tile(size, BLACK)
    mw = size * 0.52
    ribbon_p(t, ((size - mw) / 2, size * 0.105, (size + mw) / 2, size * 0.105 + mw))
    fam = draw_wordmark(
        t, (size * 0.088, size * 0.70, size * 0.912, size * 0.87), key, WHITE
    )
    return down(t), fam


def render_badge(size, kind):
    img = new((size, size))
    d = ImageDraw.Draw(img)
    d.ellipse([0, 0, size * SS, size * SS], fill=RED)
    c, r = size / 2 * SS, size * 0.30 * SS
    if kind == "play":
        sh = r * 0.10  # optical shift right
        d.polygon(
            [
                (c - r * 0.55 + sh, c - r * 0.75),
                (c - r * 0.55 + sh, c + r * 0.75),
                (c + r * 0.75 + sh, c),
            ],
            fill=WHITE,
        )
    elif kind == "pause":
        w = r * 0.52
        d.rectangle(
            [c - r * 0.72, c - r * 0.72, c - r * 0.72 + w, c + r * 0.72], fill=WHITE
        )
        d.rectangle(
            [c + r * 0.72 - w, c - r * 0.72, c + r * 0.72, c + r * 0.72], fill=WHITE
        )
    else:
        d.rounded_rectangle(
            [c - r * 0.62, c - r * 0.62, c + r * 0.62, c + r * 0.62],
            radius=int(r * 0.12),
            fill=WHITE,
        )
    return down(img)


def candidate_sheet():
    """Labelled sheet: per typeface, mark tile + wordmark on black and light."""
    keys = [
        "archivo",
        "montserrat-black",
        "poppins-black",
        "poppins-xbold",
        "anton",
        "bebas",
    ]
    labels = {
        "archivo": "Archivo Black",
        "montserrat-black": "Montserrat Black (variable, Black instance)",
        "poppins-black": "Poppins Black",
        "poppins-xbold": "Poppins ExtraBold",
        "anton": "Anton Regular",
        "bebas": "Bebas Neue Regular",
    }
    RW, RH, HEAD = 1800, 600, 150  # 1x units
    rows = []
    fams = {}
    for key in keys:
        row = new((RW, RH), (0x1A, 0x1B, 0x1E, 255))
        d = ImageDraw.Draw(row)
        d.rectangle([0, 0, RW * SS, RH * SS], fill=(0x1A, 0x1B, 0x1E, 255))
        fl = load_inter(30 * SS, b"Bold")
        fs = load_inter(22 * SS, b"Regular")
        d.text(
            (40 * SS, 26 * SS),
            "%s  .  track %.2fem" % (labels[key], FONTS[key]["track"]),
            font=fl,
            fill=WHITE,
            anchor="lt",
        )
        spec = FONTS[key]
        d.text(
            (40 * SS, 66 * SS),
            "%s / %s" % (spec["file"], "named Black" if spec["named"] else "static"),
            font=fs,
            fill=CARD_MUT,
            anchor="lt",
        )
        for col, bg, wmfill in ((0, BLACK, WHITE), (1, LIGHT, BLACK)):
            px = RW / 2 * col
            d.rectangle([px * SS, 110 * SS, (px + RW / 2) * SS, RH * SS], fill=bg)
            t = tile(300, BLACK)
            ribbon_p(t, (300 * 0.21,) * 2 + (300 * 0.79,) * 2)
            row.alpha_composite(t, (int((px + 70) * SS), int(170 * SS)))
            fams[key] = draw_wordmark(
                row, (px + 400, 250, px + RW / 2 - 60, 420), key, wmfill
            )
        rows.append(down(row.convert("RGB")))
    head = new((RW, HEAD), (0x1A, 0x1B, 0x1E, 255))
    dh = ImageDraw.Draw(head)
    ft = load_inter(46 * SS, b"Bold")
    dh.text(
        (RW / 2 * SS, HEAD / 2 * SS),
        "POTFLIX LOGO CANDIDATES",
        font=ft,
        fill=WHITE,
        anchor="mm",
    )
    sheet = Image.new("RGB", (RW, HEAD + RH * len(rows)), (26, 27, 30))
    sheet.paste(down(head.convert("RGB")), (0, 0))
    for i, r in enumerate(rows):
        sheet.paste(r, (0, HEAD + RH * i))
    return sheet, fams


# ---------------------------------------------------------------- cards
def fmt(s):
    return "%d:%02d" % (s // 60, s % 60)


def card(
    w,
    h,
    header,
    details,
    state,
    bar=None,
    countup=None,
    buttons=False,
    large_img=None,
    small_img=None,
):
    img = new((w, h), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    rr(d, (0, 0, w, h), 10, CARD)
    fH = load_inter(30, b"Bold")
    fD = load_inter(26, b"Medium")
    fS = load_inter(24, b"Regular")
    fT = load_inter(22, b"Regular")
    th = h - 72
    lg = large_img.resize((th * SS, th * SS), Image.LANCZOS)
    img.alpha_composite(lg, (int(36 * SS), int(36 * SS)))
    sm = small_img.resize((int(th * 0.36 * SS), int(th * 0.36 * SS)), Image.LANCZOS)
    img.alpha_composite(sm, (int((36 + th * 0.64) * SS), int((36 + th * 0.64) * SS)))
    x, y = 36 + th + 32, 52
    for txt, fnt, col in (
        (header, fH, CARD_HDR),
        (details, fD, CARD_DET),
        (state, fS, CARD_MUT),
    ):
        if txt:
            d.text((x * SS, y * SS), txt, font=fnt, fill=col)
            y += 46
    if bar is not None:
        el, tot = bar
        y += 12
        bw = w - x - 120
        frac = max(0.0, min(1.0, el / float(tot)))
        d.rounded_rectangle(
            [x * SS, y * SS, (x + bw) * SS, (y + 12) * SS],
            radius=6 * SS,
            fill=(0x42, 0x44, 0x4A, 255),
        )
        if frac > 0:
            d.rounded_rectangle(
                [x * SS, y * SS, (x + bw * frac) * SS, (y + 12) * SS],
                radius=6 * SS,
                fill=RED,
            )
        y += 26
        d.text((x * SS, y * SS), fmt(el), font=fT, fill=CARD_MUT)
        d.text(((x + bw) * SS, y * SS), fmt(tot), font=fT, fill=CARD_MUT, anchor="ra")
    elif countup is not None:
        y += 12
        d.text((x * SS, y * SS), "%s elapsed" % fmt(countup), font=fT, fill=CARD_MUT)
    if buttons:
        y += 64
        fB = load_inter(24, b"Bold")
        bx = x
        for lab in ("Watch Episode", "View Series"):
            bw2 = d.textlength(lab, font=fB) + 56
            d.rounded_rectangle(
                [bx * SS, y * SS, (bx + bw2) * SS, (y + 48) * SS],
                radius=8 * SS,
                fill=(0x4E, 0x51, 0x5A, 255),
            )
            d.text(
                ((bx + bw2 / 2) * SS, (y + 24) * SS),
                lab,
                font=fB,
                fill=(0xC4, 0xC9, 0xCE, 255),
                anchor="mm",
            )
            d.line(
                [bx * SS, (y + 24) * SS, (bx + bw2) * SS, (y + 24) * SS],
                fill=RED,
                width=4 * SS,
            )
            bx += bw2 + 24
        d.text(
            (bx * SS + 16 * SS, (y + 24) * SS),
            "OWNER-EXCLUDED",
            font=fB,
            fill=RED,
            anchor="lm",
        )
    return down(img, "RGB")


# ---------------------------------------------------------------- ico
def write_ico(im256, path):
    """ONE 256x256 BMP/DIB ICO frame; hand-rolled DIB fallback if Pillow emits PNG."""
    rgba = im256.convert("RGBA")
    rgba.save(path, format="ICO", sizes=[(256, 256)], bitmap_format="bmp")
    with open(path, "rb") as f:
        data = f.read()
    if data[22:26] == b"\x89PNG":
        rgba.save(path, format="ICO", sizes=[(256, 256)])  # placeholder replaced below
        data = manual_bmp_ico(rgba)
        with open(path, "wb") as f:
            f.write(data)
    return verify_ico(path)


def manual_bmp_ico(px):
    w, h = px.size
    xor = bytearray()
    for y in range(h - 1, -1, -1):
        for x in range(w):
            r, g, b, a = px.getpixel((x, y))
            xor += bytes((b, g, r, a))
    andmask = bytearray()
    for y in range(h - 1, -1, -1):
        row = bytearray()
        bits = 0
        for x in range(w):
            bits = (bits << 1) | (1 if px.getpixel((x, y))[3] < 128 else 0)
            if x % 8 == 7:
                row.append(bits)
                bits = 0
        andmask += row
    dib = (
        struct.pack("<IIIHHIIIIII", 40, w, h * 2, 1, 32, 0, len(xor), 0, 0, 0, 0)
        + bytes(xor)
        + bytes(andmask)
    )
    return (
        struct.pack("<HHH", 0, 1, 1)
        + struct.pack("<BBBBHHII", 0, 0, 0, 0, 1, 32, len(dib), 22)
        + dib
    )


def verify_ico(path):
    with open(path, "rb") as f:
        data = f.read()
    assert data[0:4] == b"\x00\x00\x01\x00", "not an ICO"
    count = struct.unpack("<H", data[4:6])[0]
    assert count == 1, "want 1 frame, got %d" % count
    _w, _h = data[6], data[7]
    _pl, _bpp = struct.unpack("<HH", data[10:14])
    _sz, off = struct.unpack("<II", data[14:22])
    magic = data[off : off + 16]
    print(
        "ico frames=%d size=%dx%d bpp=%d frame_off=%d magic=%s"
        % (count, _w or 256, _h or 256, _bpp, off, magic.hex(" "))
    )
    assert magic[:4] == b"\x28\x00\x00\x00", (
        "frame is NOT BITMAPINFOHEADER: %s" % magic.hex(" ")
    )
    return magic


def check_mark_32():
    """Ribbon P glyph alone at 32px: nontrivial ink in every quadrant."""
    img = new((32, 32))
    ribbon_p(img, (0, 0, 32, 32))
    m = down(img)
    assert m.size == (32, 32)
    cov = []
    for qx, qy in ((0, 0), (16, 0), (0, 16), (16, 16)):
        n = sum(
            1
            for y in range(16)
            for x in range(16)
            if m.getpixel((qx + x, qy + y))[3] > 128
        )
        cov.append(n / 256.0)
    print("mark32 quadrant alpha coverage TL=%.3f TR=%.3f BL=%.3f BR=%.3f" % tuple(cov))
    assert all(c > 0.02 for c in cov), "mark fails 32px quadrant check"
    return cov


def main():
    P = lambda *a: os.path.join(ROOT, *a)
    os.makedirs(P("design", "preview"), exist_ok=True)
    os.makedirs(P("assets", "discord"), exist_ok=True)

    stale = P("design", "preview", "logo-concepts.png")
    if os.path.exists(stale):
        os.remove(stale)
        print("removed superseded %s" % stale)

    sheet, fams = candidate_sheet()
    sheet.save(P("design", "preview", "logo-candidates.png"))

    logo, lockfam = render_lockup(1024, CHOSEN)
    logo.save(P("assets", "discord", "logo.png"))
    render_mark_tile(512, 0.12).save(P("assets", "appicon.png"))
    kinds = {}
    for k in ("play", "pause", "stop"):
        b = render_badge(1024, k)
        b.save(P("assets", "discord", "%s.png" % k))
        kinds[k] = b
    write_ico(render_mark_tile(256, 0.12), P("assets", "icon.ico"))
    # go\embed.go resolves //go:embed relative to go\, so the compiled-in
    # tray icon is go\assets\icon.ico, not assets\icon.ico. Keep it byte-identical.
    with open(P("assets", "icon.ico"), "rb") as f:
        _icon_bytes = f.read()
    os.makedirs(P("go", "assets"), exist_ok=True)
    with open(P("go", "assets", "icon.ico"), "wb") as f:
        f.write(_icon_bytes)
    verify_ico(P("go", "assets", "icon.ico"))

    W, H = 1100, 340
    lg_btn = Image.open(P("assets", "discord", "logo.png")).convert("RGBA")
    cards = [
        (
            "card-before.png",
            "Watching PotFlix",
            "The Rising of the Shield Hero",
            'S1 E13 "The Shield Demon"',
            dict(small_kind="play", bar=(400, 1420)),
        ),
        (
            "card-after.png",
            "Watching The Rising of the Shield Hero",
            "The Shield Demon",
            "S1 E13",
            dict(small_kind="play", bar=(400, 1420)),
        ),
        (
            "card-after-movie.png",
            "Watching Dune Part Two (2024)",
            "Dune Part Two (2024)",
            "",
            dict(small_kind="play", bar=(400, 1420)),
        ),
        (
            "card-after-paused.png",
            "Watching The Rising of the Shield Hero",
            "The Shield Demon",
            "S1 E13 - Paused",
            dict(small_kind="pause", countup=400),
        ),
        (
            "card-netflix-reference.png",
            "Watching The Rising of the Shield Hero",
            "The Shield Demon",
            "S1 E13",
            dict(small_kind="play", bar=(400, 1420), buttons=True),
        ),
    ]
    for name, hdr, det, st, kw in cards:
        small = kinds[kw.pop("small_kind")]
        card(W, H, hdr, det, st, large_img=lg_btn, small_img=small, **kw).save(
            P("design", "preview", name)
        )

    # ---- PROOF ----
    from PIL import Image as I

    print("palette " + PALETTE_REPORT)
    for rel in [
        "design/preview/logo-candidates.png",
        "assets/discord/logo.png",
        "assets/appicon.png",
        "assets/discord/play.png",
        "assets/discord/pause.png",
        "assets/discord/stop.png",
        "design/preview/card-before.png",
        "design/preview/card-after.png",
        "design/preview/card-after-movie.png",
        "design/preview/card-after-paused.png",
        "design/preview/card-netflix-reference.png",
    ]:
        im = I.open(P(*rel.split("/")))
        print("%s %dx%d %s" % (rel, im.size[0], im.size[1], im.mode))
    for k, fam in list(fams.items()) + [("lockup:" + CHOSEN, lockfam)]:
        print("wordmark font %s -> getname family=%r" % (k, fam))
    check_mark_32()
    for rel in ["assets/icon.ico", "go/assets/icon.ico"]:
        with open(P(*rel.split("/")), "rb") as f:
            _d = f.read()
        _n = struct.unpack("<H", _d[4:6])[0]
        print("%s bytes=%d frames=%d magic=%s" % (rel, len(_d), _n, _d[0:4].hex(" ")))
    im = I.open(P("assets", "icon.ico"))
    print(
        "assets/icon.ico n_frames=%s size=%s" % (getattr(im, "n_frames", "?"), im.size)
    )
    # tray transparency: tile corners must be transparent, center opaque
    lg = I.open(P("assets", "discord", "logo.png"))
    print(
        "logo corners alpha=%s center alpha=%s"
        % (
            [lg.getpixel(p)[3] for p in [(0, 0), (1023, 0), (0, 1023), (1023, 1023)]],
            lg.getpixel((512, 512))[3],
        )
    )


if __name__ == "__main__":
    main()

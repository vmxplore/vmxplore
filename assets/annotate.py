#!/usr/bin/env python3
"""Draw the README's callout overlays onto a raw screenshot.

What it does, in order:
  1. Reads a raw screenshot and a list of callouts.
  2. Draws a leader line from each callout box to the point it describes,
     with a dot at the anchor.
  3. Draws the rounded box, its title, and its subtitle.
  4. Writes the annotated copy beside the original.

WHY this exists as a script rather than a one-off in an image editor: the
annotations have been redrawn by hand every time the UI moved, and the UI
moves constantly — the violet icon, the sectioned kldload tab and the
desktop selector all landed in one evening, invalidating every shot. Hand
placement also drifts in style between sessions. Encoding the callouts as
data means a new screenshot costs one command and looks identical to the
last one.

Inputs:  a PNG, and a SHOTS entry naming its callouts in image coordinates.
Outputs: <name>-annotated.png beside it.

Usage:
    python3 assets/annotate.py estate            # one shot
    python3 assets/annotate.py --list            # what is defined

Notes: coordinates are in PIXELS of the source image, origin top-left. They
are deliberately not fractions — a callout must point at a widget, and
widgets do not move proportionally when a window is resized. Re-take shots
at the same size (1400x773 historically) or update the numbers.
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

try:
    from PIL import Image, ImageDraw, ImageFont
except ImportError:  # pragma: no cover - dependency is the operator's
    sys.exit("needs Pillow:  pip install --user Pillow")

HERE = Path(__file__).resolve().parent
SHOTS_DIR = HERE / "screenshots"

# The house style, matched to the existing annotated shot.
VIOLET = (199, 125, 255)          # box border, leader line, anchor dot
BOX_FILL = (24, 20, 32)           # near-black, so it reads on a dark UI
TITLE = (255, 255, 255)
SUBTITLE = (176, 176, 190)
LINE_W = 2
RADIUS = 12
PAD = 14


def _font(size: int, bold: bool = False):
    """Resolve a real scalable font through fontconfig.

    WHY not hardcoded paths: the first version of this looked for DejaVu in
    four well-known directories, found none of them on a Fedora box, and
    silently fell back to Pillow's bitmap default — which renders thin, ignores
    the requested size, and drops any glyph outside ASCII. The em-dashes and
    middots in the callouts came out as empty boxes and the result shipped
    looking worse than the hand-made original it replaced.

    fc-match answers "what is the actual file for this family" on any system
    with fontconfig, which is every system that has fonts at all.
    """
    pattern = "sans-serif:bold" if bold else "sans-serif"
    try:
        path = subprocess.run(["fc-match", "-f", "%{file}", pattern],
                              capture_output=True, text=True,
                              timeout=10).stdout.strip()
        if path and Path(path).exists():
            return ImageFont.truetype(path, size)
    except (OSError, subprocess.SubprocessError):
        pass
    # Refuse rather than draw something worse than what it replaces.
    sys.exit("no scalable font found (install fontconfig + any TTF); "
             "refusing to render with the bitmap fallback")


# ─── the callouts ────────────────────────────────────────────────────────
#
# Each entry: (box_x, box_y, anchor_x, anchor_y, title, subtitle).
# box_* is the callout's top-left; anchor_* is the pixel it points at.
SHOTS: dict[str, list[tuple]] = {
    "estate": [
        (80, 225, 205, 105, "Estate tree",
         "grouped — off groups fold away"),
        (25, 320, 130, 125, "Two-line rows + batch",
         "state·CPU·IP·zvol·snaps — dot-click to select"),
        (625, 125, 590, 60, "Three consoles",
         "Serial · Graphics (native VNC) · kldload"),
        (680, 315, 580, 615, "Full dossier",
         "disks · IPs · dataset · lineage · snaps"),
        (935, 420, 880, 735, "Every verb — audited",
         "shows its exact virsh/zfs command first"),
    ],
    # New VM now carries the desktop selector, which is the thing worth
    # pointing at — it is the feature nothing else in this space has.
    "new-vm": [
        (520, 150, 300, 150, "Nine cloud images",
         "each verified against its vendor's checksum"),
        (520, 260, 300, 250, "…or a desktop",
         "GNOME · KDE · XFCE, installed on first boot"),
        (520, 400, 300, 430, "First-boot script",
         "runs as root — build your own appliance"),
    ],
    "ez-fleet": [
        (520, 150, 300, 150, "One golden, N clones",
         "zero-copy ZFS clones, blocks shared"),
        (520, 280, 300, 260, "A desktop costs one install",
         "the golden pays; every clone inherits it"),
    ],
    "kldload-tools": [
        (560, 120, 380, 120, "Sectioned, not a wall",
         "six groups — machines, images, storage, cluster…"),
        (560, 300, 380, 320, "Colour says what it does",
         "green builds · blue storage · gold reads · red destroys"),
    ],
}


def annotate(name: str) -> Path:
    src = SHOTS_DIR / f"{name}.png"
    if not src.exists():
        sys.exit(f"no such screenshot: {src}")
    if name not in SHOTS:
        sys.exit(f"no callouts defined for {name!r} — add them to SHOTS")

    im = Image.open(src).convert("RGB")
    d = ImageDraw.Draw(im)
    f_title = _font(19, bold=True)
    f_sub = _font(13)

    for bx, by, ax, ay, title, sub in SHOTS[name]:
        tw = d.textlength(title, font=f_title)
        sw = d.textlength(sub, font=f_sub)
        w = int(max(tw, sw)) + PAD * 2
        h = 52 + PAD

        # Leader first, so the box paints over its own end and the line
        # never appears to pierce the text.
        d.line((ax, ay, bx + w // 2, by + h // 2), fill=VIOLET, width=LINE_W)
        d.ellipse((ax - 5, ay - 5, ax + 5, ay + 5), fill=VIOLET)

        d.rounded_rectangle((bx, by, bx + w, by + h), radius=RADIUS,
                            fill=BOX_FILL, outline=VIOLET, width=LINE_W)
        d.text((bx + PAD, by + 8), title, font=f_title, fill=TITLE)
        d.text((bx + PAD, by + 33), sub, font=f_sub, fill=SUBTITLE)

    out = SHOTS_DIR / f"{name}-annotated.png"
    im.save(out)
    return out


def main() -> None:
    args = sys.argv[1:]
    if not args or args[0] in ("-h", "--help"):
        sys.exit(__doc__)
    if args[0] == "--list":
        for k, v in SHOTS.items():
            missing = "" if (SHOTS_DIR / f"{k}.png").exists() else "  (no source png)"
            print(f"  {k:16s} {len(v)} callouts{missing}")
        return
    for name in args:
        print(f"  wrote {annotate(name).relative_to(HERE.parent)}")


if __name__ == "__main__":
    main()

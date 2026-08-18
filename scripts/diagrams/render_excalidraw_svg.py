#!/usr/bin/env python3
"""Render an .excalidraw scene into a static SVG for embedding in Markdown.

GitHub cannot render .excalidraw files, so the editable scene under
docs/architecture/ is rendered to a committed SVG next to it.

    python3 scripts/diagrams/render_excalidraw_svg.py \
        docs/architecture/vigil-architecture.excalidraw \
        docs/architecture/vigil-architecture.svg

Supports the subset of the format the architecture diagrams use:
rectangles (optionally rounded), diamonds, ellipses, arrows and text,
plus the `label` shorthand for text centred inside a shape.
"""

import json
import sys
import xml.sax.saxutils as xml

FONT = "Segoe UI, Helvetica, Arial, sans-serif"
PAD = 40
CHAR_W = 0.55  # rough advance width as a fraction of the font size


def esc(text):
    return xml.escape(text)


def bounds(elements):
    xs, ys = [], []
    for el in elements:
        x, y = el.get("x", 0), el.get("y", 0)
        w, h = el.get("width", 0), el.get("height", 0)
        xs += [x, x + w]
        ys += [y, y + h]
        if el["type"] == "text":
            xs.append(x + len(max(el["text"].split("\n"), key=len)) * el.get("fontSize", 20) * CHAR_W)
            ys.append(y + el.get("fontSize", 20) * (el["text"].count("\n") + 1) * 1.25)
    return min(xs), min(ys), max(xs), max(ys)


def text_block(lines, cx, cy, size, color, anchor="middle"):
    line_h = size * 1.3
    top = cy - (len(lines) - 1) * line_h / 2
    out = []
    for i, line in enumerate(lines):
        out.append(
            f'<text x="{cx:.0f}" y="{top + i * line_h:.0f}" font-family="{FONT}" '
            f'font-size="{size}" fill="{color}" text-anchor="{anchor}" '
            f'dominant-baseline="central">{esc(line)}</text>'
        )
    return out


def shape(el):
    x, y = el["x"], el["y"]
    w, h = el.get("width", 0), el.get("height", 0)
    stroke = el.get("strokeColor", "#1e1e1e")
    fill = el.get("backgroundColor", "none")
    if fill == "transparent":
        fill = "none"
    sw = el.get("strokeWidth", 2)
    op = el.get("opacity", 100) / 100
    common = f'fill="{fill}" stroke="{stroke}" stroke-width="{sw}" opacity="{op}"'

    if el["type"] == "rectangle":
        r = 12 if el.get("roundness") else 0
        return [f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{r}" {common} />']
    if el["type"] == "ellipse":
        return [f'<ellipse cx="{x + w / 2}" cy="{y + h / 2}" rx="{w / 2}" ry="{h / 2}" {common} />']
    if el["type"] == "diamond":
        pts = f"{x + w / 2},{y} {x + w},{y + h / 2} {x + w / 2},{y + h} {x},{y + h / 2}"
        return [f'<polygon points="{pts}" {common} />']
    return []


def arrow(el):
    x, y = el["x"], el["y"]
    stroke = el.get("strokeColor", "#1e1e1e")
    sw = el.get("strokeWidth", 2)
    pts = [(x + dx, y + dy) for dx, dy in el["points"]]
    path = " ".join(f"{px:.0f},{py:.0f}" for px, py in pts)
    marker = ""
    if el.get("endArrowhead", "arrow"):
        marker += f' marker-end="url(#head-{stroke.lstrip("#")})"'
    if el.get("startArrowhead"):
        marker += f' marker-start="url(#tail-{stroke.lstrip("#")})"'
    out = [f'<polyline points="{path}" fill="none" stroke="{stroke}" stroke-width="{sw}"{marker} />']
    if label := el.get("label"):
        mx = sum(p[0] for p in pts) / len(pts)
        my = sum(p[1] for p in pts) / len(pts)
        size = label.get("fontSize", 16)
        width = len(label["text"]) * size * CHAR_W
        out.append(
            f'<rect x="{mx - width / 2 - 6:.0f}" y="{my - size:.0f}" width="{width + 12:.0f}" '
            f'height="{size * 1.7:.0f}" rx="4" fill="#ffffff" opacity="0.92" />'
        )
        out += text_block(label["text"].split("\n"), mx, my, size, stroke)
    return out


def markers(elements):
    colors = {el.get("strokeColor", "#1e1e1e") for el in elements if el["type"] == "arrow"}
    defs = []
    for c in colors:
        cid = c.lstrip("#")
        defs.append(
            f'<marker id="head-{cid}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" '
            f'markerHeight="6" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="{c}"/></marker>'
        )
        defs.append(
            f'<marker id="tail-{cid}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" '
            f'markerHeight="6" orient="auto"><path d="M0,0 L10,5 L0,10 z" fill="{c}"/></marker>'
        )
    return defs


def render(scene):
    elements = [el for el in scene["elements"] if el.get("type") in
                ("rectangle", "ellipse", "diamond", "arrow", "text")]
    minx, miny, maxx, maxy = bounds(elements)
    w, h = maxx - minx + 2 * PAD, maxy - miny + 2 * PAD

    body = []
    for el in elements:
        if el["type"] == "arrow":
            body += arrow(el)
            continue
        if el["type"] == "text":
            size = el.get("fontSize", 20)
            body += text_block(el["text"].split("\n"), el["x"], el["y"] + size / 2,
                               size, el.get("strokeColor", "#1e1e1e"), anchor="start")
            continue
        body += shape(el)
        if label := el.get("label"):
            body += text_block(
                label["text"].split("\n"),
                el["x"] + el["width"] / 2,
                el["y"] + el["height"] / 2,
                label.get("fontSize", 20),
                "#1e1e1e",
            )

    return "\n".join([
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{w:.0f}" height="{h:.0f}" '
        f'viewBox="{minx - PAD:.0f} {miny - PAD:.0f} {w:.0f} {h:.0f}" role="img" '
        f'aria-label="VIGIL runtime governance architecture">',
        "<defs>" + "".join(markers(elements)) + "</defs>",
        f'<rect x="{minx - PAD:.0f}" y="{miny - PAD:.0f}" width="{w:.0f}" height="{h:.0f}" fill="#ffffff"/>',
        *body,
        "</svg>",
    ])


def self_check():
    scene = {"elements": [
        {"type": "rectangle", "id": "a", "x": 0, "y": 0, "width": 100, "height": 50,
         "backgroundColor": "#a5d8ff", "label": {"text": "hi\nthere", "fontSize": 20}},
        {"type": "arrow", "id": "b", "x": 100, "y": 25, "width": 60, "height": 0,
         "points": [[0, 0], [60, 0]], "strokeColor": "#4a9eed", "label": {"text": "to"}},
        {"type": "diamond", "id": "c", "x": 160, "y": 0, "width": 80, "height": 50},
        {"type": "text", "id": "d", "x": 0, "y": 80, "text": "caption <&>", "fontSize": 16},
    ]}
    svg = render(scene)
    assert svg.startswith("<svg") and svg.endswith("</svg>"), "malformed document"
    assert svg.count("<text") == 4, "one text node per label line plus the caption"
    assert "caption &lt;&amp;&gt;" in svg, "text must be XML-escaped"
    assert "<polygon" in svg and "marker-end" in svg, "diamond and arrowhead must render"
    assert f'viewBox="{-PAD} {-PAD} ' in svg, "viewBox must include the padding margin"
    print("self-check ok")


def main():
    if len(sys.argv) == 2 and sys.argv[1] == "--self-check":
        return self_check()
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    with open(sys.argv[1]) as fh:
        scene = json.load(fh)
    with open(sys.argv[2], "w") as fh:
        fh.write(render(scene) + "\n")
    print(f"wrote {sys.argv[2]}")


if __name__ == "__main__":
    main()

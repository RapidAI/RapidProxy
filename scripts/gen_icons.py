#!/usr/bin/env python3
"""生成 RapidProxy 的图标资源（不依赖任何第三方库）。

产出：
  internal/tray/assets/tray.png    32x32 RGBA，macOS / Linux 托盘使用
  internal/tray/assets/tray.ico    多尺寸 ICO，Windows 托盘使用
  build/appicon.png                512x512，Wails 应用图标
  build/windows/icon.ico           多尺寸 ICO，Wails Windows 可执行文件图标

用法：python scripts/gen_icons.py
"""

import os
import struct
import zlib

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# 圆角方块渐变底色 + 白色 "R" 字形（多段折线近似）
COLOR_START = (0x3B, 0x82, 0xF6)
COLOR_END = (0x8B, 0x5C, 0xF6)
GLYPH_POLYLINES = [
    # 竖笔
    [(0.32, 0.30), (0.32, 0.70)],
    # 顶部横笔 + 圆弧碗部 + 中横
    [(0.32, 0.30), (0.60, 0.30), (0.665, 0.325), (0.69, 0.395),
     (0.665, 0.465), (0.60, 0.50), (0.32, 0.50)],
    # 撇腿
    [(0.56, 0.50), (0.675, 0.70)],
]
GLYPH_HALF_WIDTH = 0.055
CORNER_RADIUS = 0.22


def _inside_rounded_rect(x, y, size, radius):
    if x < 0 or y < 0 or x > size or y > size:
        return False
    cx = min(max(x, radius), size - radius)
    cy = min(max(y, radius), size - radius)
    dx = x - cx
    dy = y - cy
    return dx * dx + dy * dy <= radius * radius


def _distance_to_segment(px, py, ax, ay, bx, by):
    vx, vy = bx - ax, by - ay
    wx, wy = px - ax, py - ay
    length_sq = vx * vx + vy * vy
    t = 0.0 if length_sq == 0 else max(0.0, min(1.0, (wx * vx + wy * vy) / length_sq))
    dx, dy = px - (ax + t * vx), py - (ay + t * vy)
    return (dx * dx + dy * dy) ** 0.5


def _inside_glyph(x, y, size):
    half = GLYPH_HALF_WIDTH * size
    for polyline in GLYPH_POLYLINES:
        points = [(px * size, py * size) for px, py in polyline]
        for i in range(len(points) - 1):
            ax, ay = points[i]
            bx, by = points[i + 1]
            if _distance_to_segment(x, y, ax, ay, bx, by) <= half:
                return True
    return False


def render_rgba(size, supersample=4):
    """渲染 size x size 的 RGBA 图像，超采样后降采样得到抗锯齿效果。"""
    total = size * supersample
    scale = 1.0 / float(supersample * supersample)
    radius = CORNER_RADIUS * total
    rows = []
    for py in range(size):
        row = bytearray()
        for px in range(size):
            r = g = b = a = 0.0
            for sy in range(supersample):
                y = (py * supersample + sy) + 0.5
                for sx in range(supersample):
                    x = (px * supersample + sx) + 0.5
                    if not _inside_rounded_rect(x, y, total, radius):
                        continue
                    if _inside_glyph(x, y, total):
                        cr = cg = cb = 255
                    else:
                        t = (x + y) / (2.0 * total)
                        cr = COLOR_START[0] + (COLOR_END[0] - COLOR_START[0]) * t
                        cg = COLOR_START[1] + (COLOR_END[1] - COLOR_START[1]) * t
                        cb = COLOR_START[2] + (COLOR_END[2] - COLOR_START[2]) * t
                    r += cr
                    g += cg
                    b += cb
                    a += 255.0
            count = supersample * supersample
            alpha = a / count
            if alpha <= 0:
                row += bytes((0, 0, 0, 0))
                continue
            # 颜色按覆盖到的样本平均，未覆盖的样本不参与颜色平均
            covered = a / 255.0
            row += bytes((
                int(round(r / covered)),
                int(round(g / covered)),
                int(round(b / covered)),
                int(round(alpha)),
            ))
        rows.append(bytes(row))
    return rows


def encode_png(size, rows):
    raw = bytearray()
    for row in rows:
        raw.append(0)
        raw += row

    def chunk(tag, data):
        return (
            struct.pack(">I", len(data))
            + tag
            + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    header = struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0)
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", header)
        + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
        + chunk(b"IEND", b"")
    )


def encode_bmp_entry(size, rows):
    """把 RGBA 行编码成 ICO 内部的 BMP（DIB）结构：自下而上的 BGRA + AND 掩码。"""
    xor = bytearray()
    for row in reversed(rows):
        for i in range(0, len(row), 4):
            r, g, b, a = row[i], row[i + 1], row[i + 2], row[i + 3]
            xor += bytes((b, g, r, a))

    mask_row_bytes = ((size + 31) // 32) * 4
    and_mask = bytes(mask_row_bytes * size)

    header = struct.pack(
        "<IiiHHIIiiII",
        40,          # biSize
        size,        # biWidth
        size * 2,    # biHeight（XOR + AND）
        1,           # biPlanes
        32,          # biBitCount
        0,           # biCompression
        len(xor) + len(and_mask),
        0,
        0,
        0,
        0,
    )
    return header + bytes(xor) + and_mask


def encode_ico(entries):
    """entries: [(size, payload_bytes)]，按 ICO 规范打包。"""
    count = len(entries)
    directory = b""
    offset = 6 + 16 * count
    blobs = b""
    for size, payload in entries:
        width = 0 if size >= 256 else size
        directory += struct.pack(
            "<BBBBHHII", width, width, 0, 0, 1, 32, len(payload), offset
        )
        offset += len(payload)
        blobs += payload
    return struct.pack("<HHH", 0, 1, count) + directory + blobs


def write(path, data):
    full = os.path.join(ROOT, path)
    os.makedirs(os.path.dirname(full), exist_ok=True)
    with open(full, "wb") as fh:
        fh.write(data)
    print("wrote %-40s %6d bytes" % (path, len(data)))


def main():
    cache = {}

    def rows_for(size):
        if size not in cache:
            cache[size] = render_rgba(size)
        return cache[size]

    tray_sizes = [16, 24, 32, 48]
    write("internal/tray/assets/tray.png", encode_png(32, rows_for(32)))
    write(
        "internal/tray/assets/tray.ico",
        encode_ico([(s, encode_bmp_entry(s, rows_for(s))) for s in tray_sizes]),
    )

    write("build/appicon.png", encode_png(512, rows_for(512)))
    app_sizes = [16, 24, 32, 48, 64, 128, 256]
    entries = []
    for s in app_sizes:
        if s >= 256:
            entries.append((s, encode_png(s, rows_for(s))))
        else:
            entries.append((s, encode_bmp_entry(s, rows_for(s))))
    write("build/windows/icon.ico", encode_ico(entries))


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""生成 fnspeed 的应用图标：64x64 与 256x256。

设计：深色圆角底 + 上下双向箭头（绿=下行，蓝=上行），中间一道速度线。
纯 PIL 绘制，无外部素材。
"""
import os
from PIL import Image, ImageDraw

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

BG = (13, 17, 23)
BG2 = (22, 27, 34)
DOWN = (63, 185, 80)
UP = (88, 166, 255)


def make(size: int) -> Image.Image:
    S = size * 8  # 超采样后缩小，边缘更平滑
    img = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)

    # 圆角背景
    r = int(S * 0.22)
    d.rounded_rectangle([0, 0, S - 1, S - 1], radius=r, fill=BG)
    # 内描边
    d.rounded_rectangle(
        [S * 0.045, S * 0.045, S * 0.955, S * 0.955],
        radius=int(r * 0.85),
        outline=(48, 54, 61),
        width=max(2, int(S * 0.008)),
    )

    cx = S / 2
    # 上行箭头（向右）
    ay = S * 0.34
    aw, ah = S * 0.30, S * 0.13
    d.rounded_rectangle(
        [cx - aw - S * 0.02, ay - ah * 0.28, cx - S * 0.02, ay + ah * 0.28],
        radius=int(ah * 0.28), fill=UP,
    )
    d.polygon(
        [(cx - S * 0.02, ay - ah * 0.70), (cx + S * 0.20, ay), (cx - S * 0.02, ay + ah * 0.70)],
        fill=UP,
    )

    # 下行箭头（向左）
    by = S * 0.66
    d.rounded_rectangle(
        [cx + S * 0.02, by - ah * 0.28, cx + aw + S * 0.02, by + ah * 0.28],
        radius=int(ah * 0.28), fill=DOWN,
    )
    d.polygon(
        [(cx + S * 0.02, by - ah * 0.70), (cx - S * 0.20, by), (cx + S * 0.02, by + ah * 0.70)],
        fill=DOWN,
    )

    # 中间速度线
    d.line([(cx - S * 0.26, S / 2), (cx + S * 0.26, S / 2)],
           fill=(110, 118, 129), width=max(2, int(S * 0.012)))

    return img.resize((size, size), Image.LANCZOS)


def main():
    make(64).save(os.path.join(ROOT, "ICON.PNG"), "PNG")
    make(256).save(os.path.join(ROOT, "ICON_256.PNG"), "PNG")
    os.makedirs(os.path.join(ROOT, "ui"), exist_ok=True)
    make(256).save(os.path.join(ROOT, "ui", "icon.png"), "PNG")
    print("    已生成 ICON.PNG(64) / ICON_256.PNG(256) / ui/icon.png")


if __name__ == "__main__":
    main()

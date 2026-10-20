/** @jsxImportSource @opentui/solid */
import { RGBA } from "@opentui/core"
import type { TuiPlugin, TuiPluginModule } from "@opencode-ai/plugin/tui"

// 与 mars 主仓 internal/logo/logo.txt 保持一致的启动横幅。
const art = [
  "::::    ::::      :::     :::::::::   ::::::::",
  "+:+:+: :+:+:+   :+: :+:   :+:    :+: :+:    :+:",
  "+:+ +:+:+ +:+  +:+   +:+  +:+    +:+ +:+",
  "+#+  +:+  +#+ +#++:++#++: +#++:++#:  +#++:++#++",
  "+#+       +#+ +#+     +#+ +#+    +#+        +#+",
  "#+#       #+# #+#     #+# #+#    #+# #+#    #+#",
  "###       ### ###     ### ###    ###  ########",
]

// 与 mars 主仓一致：从上到下做 青色(0,255,255) → 洋红(255,0,255) 的线性渐变。
const from = [0, 255, 255]
const to = [255, 0, 255]

// fade 计算第 index 行的颜色；末行精确命中终点色。
function fade(index: number, max: number): RGBA {
  const ratio = max === 0 ? 1 : index / max
  const channel = (a: number, b: number) => Math.floor(a + (b - a) * ratio)
  return RGBA.fromInts(
    channel(from[0], to[0]),
    channel(from[1], to[1]),
    channel(from[2], to[2]),
    255,
  )
}

const tui: TuiPlugin = async (api) => {
  api.slots.register({
    slots: {
      home_logo() {
        return (
          <box flexDirection="column">
            {art.map((line, index) => (
              <text fg={fade(index, art.length - 1)}>{line}</text>
            ))}
          </box>
        )
      },
    },
  })
}

const plugin: TuiPluginModule & { id: string } = {
  id: "mars-logo",
  tui,
}

export default plugin

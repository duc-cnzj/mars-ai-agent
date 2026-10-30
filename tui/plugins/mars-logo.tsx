/** @jsxImportSource @opentui/solid */
import { RGBA } from "@opentui/core"
import { useTerminalDimensions } from "@opentui/solid"
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

// 启动页能力清单：四字标签 + 短描述，双色对齐，方便扫读。
// 长行交给 wrapMode="char" 折行（中文没有空格，word 模式折不开）。
const abilities = [
  { label: "资源总览", detail: "命名空间 / 项目 / Pod / 容器" },
  { label: "故障排查", detail: "反复重启 / OOM / 镜像拉取 / 调度 / 挂载 / 探针" },
  { label: "日志事件", detail: "容器日志 / 集群事件 / 操作记录" },
  { label: "指标拓扑", detail: "Pod CPU / 内存 · 访问地址 · 资源拓扑" },
  { label: "交付链路", detail: "Git 分支 / 提交 / CI 流水线" },
  { label: "定时休眠", detail: "查看 / 开关（不触发重部署）" },
  { label: "变更操作", detail: "进容器 / 重启 Pod / 改配置重部署（先确认）" },
]

// 与 mars 主仓一致：从上到下做 青色(0,255,255) → 洋红(255,0,255) 的线性渐变。
const from = [0, 255, 255]
const to = [255, 0, 255]

// artWidth 是横幅最宽行的列数（47）。必须对全部行取最大值：第 2/4/5/6 行都是 47，
// 只有首行是 46——用 art[0] 推导会低估 1 列，一旦改图让最宽行不在首行，窄终端上的
// 宽度护栏就会重新失效（box 比终端宽 → opentui/Bun 段错误）。
const artWidth = Math.max(...art.map((line) => line.length))

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
        const dimensions = useTerminalDimensions()
        const theme = () => api.theme.current
        // ⚠️ 宽度绝不能大于终端可用宽度：旧实现 Math.max(artWidth, width-6) 在窄于
        // 48 列的终端上会给出比父容器还宽的 box，opentui 直接触发 Bun 段错误
        // （40 列必现，44 列仍崩，46 列恢复），整个 TUI 进程崩溃而不是折行。
        // 现在按可用宽度收缩（下限 0），并在放不下时隐藏横幅——裁切后的 logo 只是
        // 噪声，文案清单才是要读的部分。
        const width = () => Math.max(0, Math.min(60, dimensions().width - 6))
        const fitsLogo = () => dimensions().width >= artWidth + 2
        return (
          <box flexDirection="column" alignItems="center">
            {fitsLogo() && art.map((line, index) => (
              <text fg={fade(index, art.length - 1)}>{line}</text>
            ))}
            <box width={width()} flexDirection="column" paddingTop={1}>
              {abilities.map((item) => (
                <box flexDirection="row" width="100%">
                  <text flexShrink={0} fg={theme().primary}>
                    {"▸ "}
                  </text>
                  <text flexShrink={0} width={10} fg={theme().text}>
                    {item.label}
                  </text>
                  <text flexShrink={1} wrapMode="char" fg={theme().textMuted}>
                    {item.detail}
                  </text>
                </box>
              ))}
            </box>
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

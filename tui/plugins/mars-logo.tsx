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
        // 文案区宽度：按最长一行留 2 列余量，最多 60 列；窄终端按可用宽度收缩。
        const width = () => Math.min(60, Math.max(art[0].length, dimensions().width - 6))
        return (
          <box flexDirection="column" alignItems="center">
            {art.map((line, index) => (
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

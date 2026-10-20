---
name: crashloop-triage
description: CrashLoopBackOff（Pod 反复重启）的标准排查步骤——先确认 Pod 状态与原因，再看事件，最后看容器日志，逐层收敛根因。当用户报告 Pod/容器反复重启、起不来、不断重启时使用。
---

# CrashLoopBackOff 排查套路

按由外到内的顺序收敛，每一步都先拿证据再下结论。

## 第 1 步：确认状态

调用 `check_pod_status`（入参 namespace + pod）。

- 返回**只有** `{running, reason}` 两个字段：
  - `running=true` → `reason` 为空。
  - `running=false` → `reason` 是 k8s 给出的人类可读原因（如 `CrashLoopBackOff`、`ImagePullBackOff`、`Evicted`、`pod not running.`）。
  - Pod **不存在**时同样返回 `running=false`，`reason` 含 `not found` 文本（这不是工具错误，多半是 Pod 名写错或 Pod 已被替换——回去用 `list_project_containers` 重新取当前 Pod 名）。
- ⚠️ 本工具**不返回**重启次数、容器退出码、容器级状态。不要编造这些数据。

按 reason 分流：

- `CrashLoopBackOff` / `BackOff` → 继续第 2、3 步。
- `ImagePullBackOff` / `ErrImagePull` → 镜像拉取问题，转镜像仓库/凭据排查。
- `Pending` → 调度或资源问题（节点资源、亲和性、PVC 未就绪），重点看第 2 步的 `FailedScheduling` / `FailedMount`。

## 第 2 步：看事件

调用 `list_namespace_events`（入参 namespace）。它返回 `{events:[{type, reason, note, object, count, last_seen, container}]}`。

- 返回的是**整个命名空间**的事件，**顺序不保证**：必须按 `object`（形如 `Pod/xxx-abc`）过滤出目标 Pod 的事件。
- `count` 是累计发生次数——它才是判断“反复重启/反复失败”的依据。
- `last_seen` 可能为空；k8s 事件默认只保留约 1 小时，事件缺失**不等于**历史上没出过问题。
- `container` 是关联字段路径（如 `spec.containers{app}`），不是容器名。

重点 reason：

- `OOMKilling` / `OOMKilled` —— 内存超过 limit 被杀；用 `get_pod_metrics` 对照实际用量（注意 `memory` 单位是 KB）。
- `Back-off restarting failed container` —— 容器反复启动失败，`count` 即重启次数。
- `FailedMount` / `FailedScheduling` / `Unhealthy` —— 挂载、调度、探针失败。

## 第 3 步：看日志

调用 `get_container_logs`（入参 namespace + pod；多容器 Pod **必须**传 container），定位第一条 FATAL / PANIC / 连接失败 / 启动参数错误。

- 返回 `{namespace, pod_name, container_name, log}`，其中 `log` **默认只包含最后 1000 行**（传 `tail_lines` 可调大/调小），不是全量。
- 超过 128 KiB 会被截断，末尾出现“...[日志过长，已截断，完整日志请在 mars 界面查看]”——看到该标记就说明日志不完整，不能把截断点当成日志结尾。
- `container_name` 只是回显入参；省略 container 时为空，不代表服务端替你选了哪个容器。
- Pod 处于 Pending 时加 `show_events=true`，`log` 里返回的是该 Pod 的**事件文本**（每行一条），不是容器日志。

容器刚崩溃、当次日志为空时：回到第 2 步用事件判断原因（OOM 看内存，启动即退看入口命令与配置），
不要因为日志为空就断言“没有错误”。

## 第 4 步：验证假设（需用户审批）

若怀疑是网络、依赖或配置问题，可用 `exec_command` 在容器内做**最小探活**（如 `["sh","-c","nc -z host port"]`）。

- 返回 `{output, exit_code?, truncated, timed_out, start_failed, error?}`：
  - `exit_code` **仅在命令跑完且退出码非 0 时出现**；字段缺失 = 退出码 0。
  - `timed_out=true` = 超过超时（缺省 60 秒）被终止；`start_failed=true` = exec 根本没启动（如命令不存在）。
  - 退出码非 0 **不是工具错误**，要自己看 `exit_code` 与标志位。
- 每次只验证一个假设，并先向用户说明将要执行的命令。该工具需要用户在客户端确认后才会执行。

## 可选动作（同样需用户审批）

根因定位后，按需提出（不要自动连续调用）：

- `restart_pod`：强制删除 Pod 交由控制器重建。**删除是立即的、重建是异步的**——返回 `deleted=true` 只代表删除请求被接受，需再用 `list_project_containers` / `check_pod_status` 确认新 Pod 就绪；裸 Pod 不会自动回来。
- `apply_project`：改配置/分支后重新部署。先 `get_project` 读当前配置，先 `dry_run=true` 预览，再正式部署。

## 汇报约定

- 给出：现象 → 证据（来自哪个工具、哪个字段）→ 根因假设 → 建议动作。
- 报告中必须带上对象定位信息：空间名 + 空间 id、项目名 + 项目 id、完整 Pod 名与容器名，方便用户直接操作。
- 没有证据时不要断言根因；不确定就明确说“需要进一步验证”。

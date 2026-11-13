# CPA Cursor Plugin Next

独立维护的 Cursor CPA 插件，重点修复工具回合计量。基于上游 `spancerxing/cliproxyapi-cursor-plugin` v0.1.1（`ad798f3350eb92774cde939c2f2aa452f21b688e`），保留 MIT 许可、NOTICE 和 magpie 来源；并非从零原创或官方产品。

工具回合结束后查询本次 Run 的真实 Dashboard 用量，经过账号、会话、模型、时间窗、事件去重和稳定采样校验。字节除以四的估算不再进入标准用量。迟到计量单独记账，不累加到下一轮上下文；包含内部工具发现步骤的 Run 累计值也不会当成单次上下文。

重要限制：CPA 8.0.38 和 8.0.43 会在 Responses 中省略未知用量，但宿主账本仍将其记成零且 `quality=complete`，并且没有补记回调。Dashboard 事件也没有已核实的最终标记。当前实现因此不能宣称每个工具回合都获得了永久精确的最终计量。详见[兼容说明](docs/host-compatibility.md)与[计量设计](docs/accounting-design.md)。

使用 `make test`、`make build` 构建，macOS 产物为 `dist/cursor-next.dylib`。插件 ID 为 `cursor-next`，provider 仍为 `cursor`。持久化计量需在插件配置里设置 `usage-journal`，目录权限为 0700、文件为 0600；不设置时仅保存在内存中。完整配置见[英文说明](README.md)。

当前 0.2.2 是仅修改插件的加固候选；已撤回的 0.2.1 跨组件方案不是前置依赖，也不是升级目标。构建不会自动替换现用插件或发布。先按[验证说明](VALIDATION.md)运行独立宿主，再按[安装与回退方案](docs/installation-and-rollback.md)切换。

0.2.0 参考 Magpie Cursor 0.2.23 重构原生传输与工具续接：HTTP/2 双向 Run、连接复用、工具结果继续同一次 Run、完整结果批次校验、重复请求回放、超时与取消，以及后台 Run 计量核对。Chat 流改为原始 JSON，由 CPA 统一包装 SSE，修复双重 `data:`。

原生模式需要显式配置 `transport: native` 和 `native-proxy-url: direct` 或批准使用的 HTTP(S)/SOCKS 代理。CPA 插件接口不提供已解析的代理/TLS 策略，因此空配置仍走原有宿主 HTTP。身份交换和模型目录仍使用宿主路径，两者应配置一致。

工具回合不再同步等待账单。整次 Run 的计量保留在独立账本，不伪装成每一步的上下文大小。会话仅保留当前工具轮次；永久丢失、结束或过期的执行句柄返回 410，并保留错误码及恢复提示，反复重试同一句柄不能恢复。仍在运行/提交或可纠正的契约冲突保留 409。恢复需先核对已完成工具的副作用，再以完整历史开启新的用户轮次。精确的逐步上下文计量、跨待处理工具回合的自动压缩及非流式即时断连取消仍有边界，详见[原生会话设计](docs/native-sessions.md)。

0.2.2 在工具整批输出前检查 required、指定工具、禁止并行及 strict JSON Schema；不合格批次直接拒绝，不修改参数后偷偷执行。JSON Schema 验证不等于任意 Lark grammar 的受约束解码。图片支持 data URL；公网 HTTP(S) 图片需显式 native 路由，拒绝私网/fake-IP 与不安全跳转，单图 8 MiB、请求附件合计 16 MiB、远程图最多八张。文本文件必须提供真实 base64 UTF-8 file_data，单文件 1 MiB；文件 ID、只有文件名、二进制文件、音频和视频明确拒绝。

不能实现的非默认采样、输出长度、stop、JSON 输出等控制返回 400，不再静默忽略。system/developer 文本仍按原顺序合并进 system，尚不能声称与原生独立角色层级等价。

`native-tool-result-timeout-seconds` 默认 120、范围 15–900；`native-run-lifetime-seconds` 默认 900、范围 60–3600，前者必须短于后者。实际工具交接截止时间还受 Run 剩余寿命限制。管理接口可读回生效传输与配置上限；Responses 可能过滤每轮 deadline 扩展，不能据此承诺 Codex 能看到每个截止时间。`recover-response-service-tier` 默认关闭，仅供已确认宿主翻译丢失字段时启用，绝不覆盖宿主已有的有效字段。

0.1.3 增加同一事件的初始值、最新值和修订记录；复查也要求稳定采样。
账本容量、写盘或路径故障会显示降级，不再让成功的模型回答变成 503。
可通过宿主鉴权保护的 `/v0/management/plugins/cursor-next/accounting` 查看健康状态、当前内存账本中的测量/未知数量、来源/原因统计，以及最近一次 Run 的真实测量与修订状态。此摘要只覆盖有界活跃账本，不扫描历史归档，也不返回账户、会话或事件标识；它不是日账单或账户历史总量。
内部多步 Run 的 Dashboard 与 TurnEnded 合计都不会冒充单步上下文；
Cursor 未提供可验证的逐步用量或最终标记，这一边界仍然存在。

`deferred` 表示后来已取得测量，不是仍未完成；复查失败不会删除已有测量。`stable` 不代表永久最终值，原生 TurnEnded 的 stable 也不表示经过 Dashboard 三次采样。

维护中的构建入口通过 `git -C` 读取插件自身仓库，包括 linked worktree，并显式嵌入 commit/dirty；关闭 Go 自动 VCS 发现，避免把外层 Workspace 误当成插件源码。管理接口 `build` 与实际库中的标记对应；打包生成独立 `.provenance.json`，将来源、Go 版本与库 SHA256 绑定。ZIP 仍只含根目录下的插件库。正式 tag 验证要求干净构建且源码 commit 与 tag 一致，不能用打包时的工作树 HEAD 冒充旧库的构建来源。没有显式源码标记的临时构建报告 unknown。

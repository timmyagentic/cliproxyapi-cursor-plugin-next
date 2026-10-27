# CPA Cursor Plugin Next

独立维护的 Cursor CPA 插件，重点修复工具回合计量。基于上游 `spancerxing/cliproxyapi-cursor-plugin` v0.1.1（`ad798f3350eb92774cde939c2f2aa452f21b688e`），保留 MIT 许可、NOTICE 和 magpie 来源；并非从零原创或官方产品。

工具回合结束后查询本次 Run 的真实 Dashboard 用量，经过账号、会话、模型、时间窗、事件去重和稳定采样校验。字节除以四的估算不再进入标准用量。迟到计量单独记账，不累加到下一轮上下文；包含内部工具发现步骤的 Run 累计值也不会当成单次上下文。

重要限制：CPA 8.0.38 和 8.0.43 会在 Responses 中省略未知用量，但宿主账本仍将其记成零且 `quality=complete`，并且没有补记回调。Dashboard 事件也没有已核实的最终标记。当前实现因此不能宣称每个工具回合都获得了永久精确的最终计量。详见[兼容说明](docs/host-compatibility.md)与[计量设计](docs/accounting-design.md)。

使用 `make test`、`make build` 构建，macOS 产物为 `dist/cursor-next.dylib`。插件 ID 为 `cursor-next`，provider 仍为 `cursor`。持久化计量需在插件配置里设置 `usage-journal`，目录权限为 0700、文件为 0600；不设置时仅保存在内存中。完整配置见[英文说明](README.md)。

这是 0.2.0 源码/构建候选，不会自动替换现用插件或发布。先按[验证说明](VALIDATION.md)运行独立宿主，再按[安装与回退方案](docs/installation-and-rollback.md)切换。

0.2.0 参考 Magpie Cursor 0.2.23 重构原生传输与工具续接：HTTP/2 双向 Run、连接复用、工具结果继续同一次 Run、完整结果批次校验、重复请求回放、超时与取消，以及后台 Run 计量核对。Chat 流改为原始 JSON，由 CPA 统一包装 SSE，修复双重 `data:`。

原生模式需要显式配置 `transport: native` 和 `native-proxy-url: direct` 或批准使用的 HTTP(S)/SOCKS 代理。CPA 插件接口不提供已解析的代理/TLS 策略，因此空配置仍走原有宿主 HTTP。身份交换和模型目录仍使用宿主路径，两者应配置一致。

工具回合不再同步等待账单。整次 Run 的计量保留在独立账本，不伪装成每一步的上下文大小。会话仅保留当前工具轮次；进程重启、过期、历史压缩/变更或不确定的结果提交会明确返回 409。新的普通用户轮次仍会导入完整历史。精确的逐步上下文计量、跨待处理工具回合的自动压缩及非流式即时断连取消仍有边界，详见[原生会话设计](docs/native-sessions.md)。

0.1.3 增加同一事件的初始值、最新值和修订记录；复查也要求稳定采样。
账本容量、写盘或路径故障会显示降级，不再让成功的模型回答变成 503。
可通过宿主鉴权保护的 `/v0/management/plugins/cursor-next/accounting` 查看健康状态。
内部多步 Run 的 Dashboard 与 TurnEnded 合计都不会冒充单步上下文；
Cursor 未提供可验证的逐步用量或最终标记，这一边界仍然存在。

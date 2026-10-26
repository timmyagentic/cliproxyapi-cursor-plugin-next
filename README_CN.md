# CPA Cursor Plugin Next

独立维护的 Cursor CPA 插件，重点修复工具回合计量。基于上游 `spancerxing/cliproxyapi-cursor-plugin` v0.1.1（`ad798f3350eb92774cde939c2f2aa452f21b688e`），保留 MIT 许可、NOTICE 和 magpie 来源；并非从零原创或官方产品。

工具回合结束后查询本次 Run 的真实 Dashboard 用量，经过账号、会话、模型、时间窗、事件去重和稳定采样校验。字节除以四的估算不再进入标准用量。迟到计量单独记账，不累加到下一轮上下文；包含内部工具发现步骤的 Run 累计值也不会当成单次上下文。

重要限制：CPA 8.0.38 和 8.0.43 会在 Responses 中省略未知用量，但宿主账本仍将其记成零且 `quality=complete`，并且没有补记回调。Dashboard 事件也没有已核实的最终标记。当前实现因此不能宣称每个工具回合都获得了永久精确的最终计量。详见[兼容说明](docs/host-compatibility.md)与[计量设计](docs/accounting-design.md)。

使用 `make test`、`make build` 构建，macOS 产物为 `dist/cursor-next.dylib`。插件 ID 为 `cursor-next`，provider 仍为 `cursor`。持久化计量需在插件配置里设置 `usage-journal`，目录权限为 0700、文件为 0600；不设置时仅保存在内存中。完整配置见[英文说明](README.md)。

这是 0.1.2 源码/构建候选，不会自动替换现用插件或发布。先按[验证说明](VALIDATION.md)运行独立宿主，再按[安装与回退方案](docs/installation-and-rollback.md)切换。

# 剧情与任务门栓回归矩阵

## 范围

该功能是只读的真实 PVF 离线回归工具，覆盖任务状态链、城镇区域、Gate、剧情副本拓扑和返回路线。它不修改 op 包体、不改变任务结算状态，也不在运行时替代现有门栓校验。

## 使用

在 `go-server` 目录执行：

```powershell
go run -buildvcs=false ./cmd/scratch/storygatematrix `
  -pvf F:\DNF\90ORIGON\PVF\Script-服务端.pvf `
  -start-quest 3145 -job 2 -grow-type 0 -level 90 `
  -json tmp\story-gate-3145.json `
  -markdown tmp\story-gate-3145.md
```

矩阵从 `quest.lst` 的前置关系开始遍历，只保留当前 job/grow_type 可用的分支。每个节点包含：

- PVF 任务类型、子类型、主/子任务归属、前置任务和后继任务；
- clear/condition 目标对真实 dungeon/map 的解析结果；
- dungeon 的 maze、起点、Boss、layered 声明、quest connection 和解析诊断；
- town PVF 中与任务或 dungeon 对应的 Gate，以及带任务条件的返回路线；
- 节点指纹和整条矩阵 hash，供 PVF 更新后的回归比较。

## 确定性与边界

- 任务、城镇和副本全部来自传入的 PVF；不使用合伙人派生 PVF、日期覆盖或硬编码地图/任务 ID。
- `MaxEntries` 默认 2048，超过上限会产生 warning，避免损坏的前置图导致无限遍历。
- 无法从 PVF 将任务目标连接到 dungeon 或 Gate 时保留诊断；工具不会猜测路线，也不会替运行时放行。
- `WriteJSON` 与 `WriteMarkdown` 只输出报告，不能写回 PVF 或数据库。

## 测试

```powershell
go test -buildvcs=false ./internal/modules/dnf/storygate
$env:DNFBRIDGE_REAL_PVF_SMOKE='F:\DNF\90ORIGON\PVF\Script-服务端.pvf'
go test -buildvcs=false -run TestRealPVFStoryGateMatrix ./internal/modules/dnf/storygate
```

真实回归测试当前以 `3145`、job `2`、grow_type `0`、等级 `90` 的矩阵 hash
`44df0b9ced5b68bce3f4b39a3b3e4630bed2d6df7aff8cf683036ede67e8974b` 为 reviewed
baseline。只有确认 PVF 有意变更后才更新该值，不能用服务端日志或手工地图测试绕过。


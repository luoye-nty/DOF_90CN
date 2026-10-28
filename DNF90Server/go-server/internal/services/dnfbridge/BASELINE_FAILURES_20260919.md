# dnfbridge 基线失败用例（2026-09-19 实测，与本任务无关）

在动多买多送/代币券之前，`go test ./internal/services/dnfbridge/` 就已经有 **5 个**确定性
失败。它们与商城、代币券、多买多送**没有任何关系**，是先行遗留：

| 用例 | 失败原因（实测） |
| --- | --- |
| `TestCurrentChannelCatalogFitsGuildMemberChannelByte` | 频道号 261/262/263、501..508 仍在公会成员行字节范围 1..254 之外，测试要求重编号 |
| `TestTowerSelectorProgressReadsDurableNextFloorAfterOp27Boundary` | 选择器包体与期望不符（`op5` 包体 `0100012b000001`） |
| `TestSelectorPageBackToVillageUsesLatestCurrentOp35Snapshot` | 选择器/回城快照不符 |
| `TestTownSetUserPositionResumesInterruptedFullReturnSuffix` | 城镇设点后续包序列不符 |
| `TestCurrentPartyUDPRelayLearnsActivePartySocketAfterSeededProbePort` | 组队 UDP 中继端口学习（1s 超时） |

这些看起来是"先写规格、后实现"的用例，或者是被更早的改动挂掉的。**不属于本次改动范围**，
本次不修（红线：改动不能顺手动别人的功能）。

## ⚠ 组队 UDP 中继这一族是 **flaky**，不要按单次结果判断回归

`TestCurrentPartyUDPRelay*` 这一族共用 1 秒超时，跑全包时会**随机**挂掉其中一个。
实测（同一份代码连跑 4 次）：

| 轮次 | 挂掉的 UDP 用例 |
| --- | --- |
| 1 | `...LearnsActivePartySocketAfterSeededProbePort` |
| 2 | `...LearnsActivePartySocketAfterSeededProbePort` + `...PreservesLargeRealtimeActorDatagram` |
| 3 | `...LearnsActivePartySocketAfterSeededProbePort` |
| 4 | `...ForwardsFirstHandshakeToRegisteredPeerPort` + `...LearnsActivePartySocketAfterSeededProbePort` |

其中只有 `...LearnsActivePartySocketAfterSeededProbePort` 每次都挂（属于上面的 5 个确定性失败）；
其余两个是**偶发**。判断回归时请以"非 UDP 用例的失败集"为准，或对 UDP 单独多跑几轮。

## 回归判据

改动后跑 `go test ./internal/services/dnfbridge/ -count=1`，
**失败集必须恰好等于上面这 5 个**（外加 UDP 家族的偶发项）。多出任何一个 = 我的改动破坏
了别的东西，必须回滚。

复现（只跑这 5 个，秒级）：

```powershell
cd <repo>\go-server
$pat = "TestTowerSelectorProgressReadsDurableNextFloorAfterOp27Boundary|TestCurrentChannelCatalogFitsGuildMemberChannelByte|TestCurrentPartyUDPRelayLearnsActivePartySocketAfterSeededProbePort|TestSelectorPageBackToVillageUsesLatestCurrentOp35Snapshot|TestTownSetUserPositionResumesInterruptedFullReturnSuffix"
go test ./internal/services/dnfbridge/ -count=1 -run $pat
```

## 已确认的"有意的包序列变化"（不是回归）

`initial_town_route_packets.go` 的选角进城尾部现在多一包 `class0/op53`（商城钱包）。
下面两个用例按既有风格显式吃掉这一包，而不是放宽 `len(rest)==0`：

- `TestDeferredSelectSceneTailResumesInitialTownRouteFromHeartbeatWithoutProgress36`
  （`initial_town_transition_order_test.go`）
- `TestCompletedSelectProgress36BindsRealActorBeforeTypedTownTransition`
  （`select_character_scene_gate_test.go`）

断言助手 `requireInitialTownCeraShopWalletProjection` 放在
`cera_shop_wallet_projection_test.go`，断言的是 class/opcode/13 字节/flag=1，
所以尾部再冒出来的任何**其它**包仍会被 `len(rest)==0` 抓到。

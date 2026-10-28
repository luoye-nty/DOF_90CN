# Current NoPack transport compatibility DLL

This project builds the production `90CN.dll` loaded by the existing
`ijl15.dll` plugin chain. Its production responsibility is limited to
connection bootstrap, channel-directory observation, packet codec
compatibility, DPROTO transport, bounded inbound routing, the validated
party-directory UI patch, scoped town co-presence compatibility, and narrowly
scoped current-client gaps that cannot be repaired by server packets alone:
allow-listed premium-contract op44 dispatch and persistent Aura state
mirroring. It also bridges the current client's native "disjoint all" selected
slot loop to one marked op26 batch command; item validation, rewards, inventory
mutation, and persistence remain server-owned. It also owns the optional Lua combat-power sidecar's window-thread
renderer and read-only copies of already accepted local actor stats and worn-object rows. Crystal Contract restore remains a native class-0/op898 server
notification and is not cached or applied by the DLL.

Character, inventory, equipment, avatar, creature, party, quest, contract,
mail, scene, camera, and appearance state remains owned by the Go server and
the current client's native handlers. The compatibility hooks do not grant or
persist state: they forward a native-format request or mirror an exact
server-authored Aura entitlement into the current actor's native UI state. The
optional Lua plugin can also copy a bounded text notice into the transport
DLL's main-window queue and consume confirmed client lifecycle events; it
cannot call the game UI from its worker thread.

## Production hooks

The production worker installs only the following current-EXE boundaries:

| Responsibility | Current-EXE RVA | Behavior |
| --- | --- | --- |
| TCLS direct-launch helpers | `0x015AA570`, `0x01AF7C40`, `0x01AF6970`, `0x01AF6A50`, `0x01AF6DE0`, `0x01AF6F50` | Preserves each native helper first. For the direct-launch marker only, missing native launch fields are filled from the already-parsed TCLS arguments. |
| Channel directory and connection | `0x0189FB00`, `0x014A5360`, `0x018A2440`, `0x014A2780`, `0x014A1FF0`, `0x018A0DA0` | Observes the native channel-directory lifecycle. The connection hook may redirect bootstrap port `10019` to the configured resident-channel port; it does not write HUD, clock, actor, scene, or camera state. |
| Ordinary party-directory UI | `0x02E6CF2C`, `0x02E6CF72`, `0x02E6D435`, `0x02E6D4B1`, `0x02BDD1A0` | Changes only the ordinary-channel controller's owner IDs in its guard, open, and both matching close paths from summary owner `0x17B` to full-directory owner `9`. After owner 9 opens successfully, it emits the exact native writer sequence `op98/u8(0)` found at current-EXE VA `0x032289E4`, so the server returns the full snapshot while the directory owner exists. Rows, create/join, and party state remain server-owned. |
| Equipment disjoint-all batching and title eligibility | `0x02C38260`, `0x01F30630`, `0x02C394DC`, native upper writer `0x03054640..0x030566F0` | The exact title-rejection jump in the native system-disassembler candidate scan is disabled while the independent reserved-part-31 check and every other stock eligibility test remain intact. Only while the exact native disjoint-all loop is iterating its selected set, captures those source slots and emits one marked op26 batch when the captured count exactly matches the UI-owned selection count. A one-item selection stays on the original sender; any capture/send failure replays every suppressed call through it. The server validates every slot again and commits the complete batch atomically; the DLL never computes a reward or mutates inventory. |
| Anton public-directory owner guard | `0x0194C39E` | Preserves the native class-0/op884 callback's dedicated raid connection index `15`. After validating the complete surrounding resolver/helper sequence, adds the null-owner check that neighboring op844 already has. If the local direct-route topology has no dedicated raid-service owner, the handler keeps its already-created empty native window and follows its existing no-owner cleanup path instead of dereferencing `owner+0x2B4`. It does not substitute, allocate, or fabricate an owner, directory row, actor, or raid state. |
| Town co-presence compatibility | `0x01E5CA00`, `0x02299A30`, `0x04D99C49`, `0x02294460`, `0x02294890` | Preserves native class-0 dispatch and actor lookup. It maintains only the measured remote actor context/visual lifecycle, and for the first current-character mode-0 create temporarily substitutes the native local owner when the wire owner is zero, preventing the current EXE from dereferencing a null current actor. Existing-actor equipment `op342` is left entirely to its native reader: the patch does not destroy/recreate the local paper-doll or replay mode0/mode1. Wire bytes are restored immediately; identity, position, equipment, party, and persistence remain server-owned. |
| Dungeon start-map derived-state synchronization | `0x019479A0`, `0x01949E40`; flags `0x04DB27B6`, `0x04EB8F54`, `0x04D9CEFC` | Runs only after the stock class-0 `op29` or altar `op211` reader. The current EXE already overwrites the direct Hell Party fog byte on every room, but its stock nonzero-only branch never clears two derived dungeon-controller bytes. The hook mirrors that accepted direct byte as zero or one, preventing a Hell Party run from locally blocking item clicks in the next ordinary or story dungeon. It does not manufacture a packet, permit an item, or alter server-owned dungeon state. |
| Authoritative consumable wait reset | active `0x01E5CA00` class-0 dispatcher, `0x0229A050`, `0x022906D0`, actor `+0x8BE` | After native dispatch of an actual server class-0 `op42`, resolves the current actor and calls the current EXE's own gate setter with zero. This closes the direct-route case where the outer dispatcher accepted the reset but the registered callback did not clear the newly rebound actor. It never runs on a click, never fabricates an acknowledgement and does not bypass item, cooldown, inventory or pending-operation validation. |
| Berserker Blood Frenzy red body gradient | `0x01FB9520`, actor `+0x4C8`; fixed status slot `1` | Caches the current local profession from the accepted class-0/op2 mode-0 row and runs only for male Slayer/Berserker (`job=0`, low grow nibble `3`) in a confirmed dungeon. Current-EXE live identity tracing proves actor `+0x4C8`, rather than the adjacent lifecycle visual at `+0x4C4`, is the final local-character renderer. During only that exact receiver's draw, the native pure-white alpha waveform at fixed status owner `+0x2B0` is converted into a white-to-bleeding-red RGB interpolation; actor alpha is preserved and the renderer's color/mask words are restored immediately after the original call. Other professions (including Dark Knight), monsters, remote actors, map presentation and recursive animation trees bypass the hook. NPK, packets and persistent state are untouched. |
| Local actor optional binding guard | `0x0228FEB0` | Preserves the native setter for every non-null receiver. If the current mode-1 bootstrap reaches the setter with a null optional child, it skips only the otherwise-invalid write to `child+0x538`; no actor, child object, equipment row, or packet state is created or changed by the DLL. |
| Creature rename null guard | `0x0198515F` | Preserves both earlier native name updates in class-0/op101. When the final optional op105-map lookup returns null, it skips only the unsafe `sub_34DEA20(this=0x14)` copy and returns through the native epilogue; a successful lookup continues through the unchanged native copy. |
| Town equipped-creature stat-map null guard | `0x02202336` | Preserves `sub_2602260`'s native `sub_25FCAF0` predicate for every resolved creature record. During a dungeon-to-town actor rebuild, a temporarily absent optional op105-map record returns the same false result consumed by the following native branch instead of entering the predicate with `this=0x38`; it does not create a record or alter pet state. |
| Premium-contract right click | `0x011280C0`, `0x01772FD0`, `0x0235A060`, `0x0235A140`, `0x01F31CE0` | Preserves the native path first and emits native-format op44 only when no native op44 was produced and the selected main-bag item is in the pinned runtime-PVF premium contract allow-list. |
| Aura state mirror | `0x01E5C8A0`, `0x02BDD1A0` | Consumes only exact marked `00 AURA` / `01 AURA` op863 state notifications. It resolves the current actor through the proved actor owner at `+0x4C8` and vtable resolver `+0xA98`, then clears/sets bit 3 of the native state dword at `+0x1AC` before avatar-panel owner `0xC9` is constructed. That is the same durable gate read by current-EXE `sub_26905A0`; the DLL never calls op863 result handler `sub_1D25590`, so opening the panel cannot replay the one-shot unlock animation. Crystal Contract op898 remains native class 0 and is not handled by the DLL. |
| All-day horse-joust entrance | `0x01E645B0`, `0x00B2D126` | Retains the native owner-609 scene-state and request flow. After verifying the current EXE's exact `75 34` branch at `sub_F2D0A0+0x86`, changes only it to `EB 34`, skipping the retired local weekend/10:00 early return. The native op1291 request, state validation, and UI construction still run; opening and betting state remain server-owned. |
| Anton/Luke raid shop date gates | `0x01C41F70`; Naen callers `0x02E5FEAE`/`0x00CD9E8E`, Erika callers `0x02E5FF29`/`0x00CD9DDA` | Preserves the native activity-date lookup everywhere except four exact current-EXE caller/entry pairs. NPC `326` and owner 1001 still require retired entry `181` for `itemshop/naen_raid.shp`; NPC `460` and owner 1002 require retired entry `282` for `itemshop/zelbaerica_raid.shp`. Those pairs are treated as active so the unchanged native shop path can issue op715. Item rows, limits, materials, purchases, and persistence remain native/server-owned. |
| Upper-body encoder | `0x02FAAFE0` | Copies the semantic plaintext body, restores packet length, and recomputes the checksum. |
| Upper-body decoder | `0x02FAB0A0` | Copies the server plaintext body into the native output contract. |
| Inbound route predicate | `0x030734E0` | Admits the bounded set of current-client opcodes implemented by the server, including the private-store creation result (`op85`) that releases the native scene/input gate before the editor, the native Anton public object page (`op753`), team protocol (`0x0248/0x024C/0x024F/0x0250/0x0251/0x0253/0x0255/0x0256/0x0257/0x027A`), and matching raid ACK/info IDs, then preserves the native predicate for every other opcode. |
| Outbound DPROTO boundary | `0x030738FA`, `0x0307391C` | Preserves the finalized inner-packet ownership contract while bypassing the incompatible protector wrapper for the measured routes. |
| Unsupported-command response capability | `0x01E628F0`, `0x01E62430`, native upper writer `0x03054640..0x030566F0` | After accepted class-0/op24 town entry, enumerates the current EXE's complete `CmdPacket` range `0..1518` through its native class-1 dispatch registry and sends a versioned 190-byte response-handler bitmap to the server on private C2S-only hole `op1456`. This is capability evidence only: the DLL does not reply to commands, fabricate packet bodies, or classify commands by name. The server uses it per session to close only unsupported interactive requests for which this exact EXE registered a response handler; unregistered one-way reports remain silent. |
| Lua notice main-thread bridge | Existing DNF main-window procedure | `Queue90CNClientNotice` copies at most 511 UTF-16 characters into a 32-entry queue and posts a private registered window message. The DNF UI thread drains the queue and paints a click-through popup owned by the game window and this DLL. No Lua worker thread calls USER32/GDI or game UI code. Read-only analysis plus live screenshots rejected `0x01E6F9C0`: it resolves registered sound/resources and does not render arbitrary text. |
| Lua combat-power sidecar | active `0x01E5CA00` town-compatible dispatcher, private class0/op413 type 1/version 4, `0x02BD4B40`, personal owner `0xD9`, existing DNF main-window procedure | After native class0/op2 dispatch accepts a complete local mode1/mode3 body, copies the verified 92-byte base-stat fields and boundedly parses its complete worn-object create list. The Go server resolves every occupied actor slot `0..32`, including the equipped creature and the pet owner's red/blue/green artifacts, against the authoritative runtime PVF and sends an exact 70-byte private body containing affixes, identity, three attacks, and the PVF equipment-grade score. `90CN.dll` consumes that body before native dispatch and exposes ordinary-data stat, equipment and damage-affix snapshots to Lua. Malformed lengths, versions, duplicate/out-of-range slots, incomplete rows and oversized counts fail closed. `Update90CNCombatPanel` accepts Lua's bounded V6 result; a 100-ms UI-thread timer shows an owned click-through sidecar only with personal owner `0xD9` and follows its root rectangle when dragged. White/yellow/critical/yellow-additional/critical-additional/all-attack categories remain separate in the ABI and renderer. |
| Trade counterpart column reveal | `0x0303B791`, `0x0303B8C4`, `0x0303F4F9`, `0x0303FCF9`, `0x0303FE0F`, `0x0303FEFB` | The current EXE's trade window renders the counterpart column from the trade manager's own second item list, which the server already fills with class-0/op15 rows from the moment the counterpart places each item, but all six trade-UI gate checks consult byte `[tradeManager+0x54]`, and an exhaustive scan proves no code in this EXE ever writes that byte: the only writer helper `sub_2340E50` has exactly one call site (`0x01D415FC`) whose receiver is a sub-object of `dword_51B2764` with vtable `0x456B36C`, never the trade manager with vtable `0x45B7ECC`, and the helper address appears in no data section, so no indirect or virtual call can reach it. Both identities were confirmed against live process memory during an active trade. Each hooked call site loads `dword_51B2794` into `ecx` immediately before the stock accessor, so the proxy reads no absolute address and instead derives the flag from the manager's own per-side state: `state[1] == 1` means the counterpart submitted "adding complete" through class-0/op17 (`sub_1D59D00` computes `side = wire charId != local charId` and calls `sub_2340B10`), which is exactly when the column must become visible. The proxy preserves the stock `al`-only register contract, keeps the seven call sites whose receiver is not the trade manager and every other trade state - including the reset that ends a trade - on the stock byte, reads no packet byte, and grants no item or trade state. Each of the six sites is patched with a five-byte `call` into the proxy, never a `jmp`: the proxy replaces the whole callee and ends in `ret`, so the patched instruction must push the return address itself. The other entry patches in this DLL use `jmp` because their proxies replay stolen prologue bytes and jump back to a resume address; copying that shape here leaves no return address for `ret` to pop and crashes the client the first time the trade window renders. |
| Trade window close notification | `0x0303B584` (init capture) plus the main-window 100 ms poll timer | Closing the trade window is a purely local act in this client: a live two-client capture with items placed on both sides ends at the offer ack, with no cancel, no state update and not even an op19 withdrawal afterwards. The client does own an unlock sender, `sub_23418C0(flag)`, which would write class-0/op24 with state 0 or 1, but its only call site in the whole binary is `0x0303B816` - the "adding complete" button - which passes 1, so the state-0 path is unreachable and the server has never received a client state 0. Server-side packet-timeline evidence then pins the remaining hook site down further: the gate-clearing call on the trade window's initialisation path fires exactly once per trade, 1-11 ms after the trade is created, and never on close, so it is an *open* signal and must not be used as a close signal - an earlier build that did so cancelled every trade the moment its window appeared. The window's own framework flag carries the state instead: `[tradeWindow+0x14]` is 1 while the window is shown and 0 once the player closes it, verified against live memory as the only field of the trade window object that changes across an open/close pair. The DLL therefore captures the window object from `esi` on the initialisation hook (the containing function stores `ecx` into `esi` at `0x0303B3E9` and never rewrites it), and the main window message hook's existing 100 ms timer polls `[window+0x14]` together with the trade manager's peer binding at `dword_51B2794+0x04`. When a peer is bound and the flag reads 0, the DLL emits the client's own native trade-state packet carrying state 0 (`WriteU16(0x18); WriteU8(0); Flush()`) through `sub_3454640`/`sub_3454EB0`/`sub_3454FC0`/`sub_34566F0` - byte-for-byte the sequence `sub_23418C0` and `sub_2340E20` already use, so no wire format is invented - once per trade, under a latch that resets when the trade ends. Both writers clean their single stack argument with `ret 4` and the flush takes none, so the sequence is stack-neutral. The server cancels a trade only when one exists for that character, and it separately ignores a state-0 report that arrives while a trade is still empty and younger than two seconds, so a window opened without a trade can never cancel anything. |
| Channel-follow ordinary-channel type gate | `0x02E4E516` (VA `0x0324E516`) | The current EXE's channel-follow selector `sub_324E3F0` ends in a channel-type whitelist: it accepts the target channel's `etc` type only when it is `0x16` (22), `3`, `0`, `0x21` (33) or `0x28` (40), or when its own `sub_226DC80` element predicate is true, and otherwise stores result status `1`, which the client renders as string `0x8224` - "目标角色处在普通或者非特殊区域". Every one of this server's 118 advertised channels is an ordinary type-`11` channel, so the stock client rejected every real follow request even after the server response itself was accepted. After confirming the exact `74 14` site and its two distinct outcomes in the already-accepted handler, the conditional jump is changed to `EB 14`, the same shape used for the retired horse-joust weekend gate: the unconditional jump always reaches the native accept exit, so an ordinary type runs the identical native channel-table lookup, native switch-request path and server validation as a whitelisted one. The patch reads no packet byte, replaces no helper, adds no branch target, and fabricates no channel, target, region or follow state; the server still owns target existence, region and channel, and an offline or missing target keeps its own server-authored state `100` (`0x8226`, "缺少目标角色。"). |
| Expert-job stall enchant result window | `0x01683263` (VA `0x01A83263`), `call sub_1B0F680` | The result window opened by the op601 reply renders whatever item the client global `0x518B798` names; the reply body itself carries only the outcome and the endurance, never a slot. That global has exactly one writer in the binary, `0x01A84EFD` inside `sub_1A84D20`, reached from `0x01A8E6DA` alone - event 30 of the UI class's dispatcher, "item dropped into the NPC enchant machine". The stall path (`0x01A83230` onwards) instead resolves the displayed item through `[esi+0x74]` and records `[esi+0x60]/[esi+0x74]/[esi+0x70]` through `sub_1B0F680` at `[window+0x238/+0x23C/+0x240]`, from which `sub_1B0F6B0` sends op601 - it never touches `0x518B798`, so the window resolved the stale key left by the previous machine enchant and showed a different item while the server enchanted the right one. The proxy on the recorder's only call site writes arg2 - the same value that becomes the op601 target slot and the key the window resolves the displayed item with - into `0x518B798`, then tail-jumps to `sub_1B0F680`. `pushad`/`popad` keep `ecx` (this) and all three arguments, and the site is patched with a five-byte `call`, never a `jmp`, so the callee's own `ret 0xC` returns to the original call site; the machine path does not pass through this site and keeps its stock behaviour. The server half of the fix lives in `expert_job_store.go` (`expert_job_store_enchant_before_ack`): the container refresh must precede the reply, because `sub_1D0E470` resolves the item and opens window `0x3D9` synchronously inside the reply handler. |

The DLL does not hook socket `send` or `recv`, build party records, or
grant/persist gameplay state. Its actor compatibility is limited to native
lookup/scene registration for server-created town actors and the scoped
current-character owner-zero remap described above. Existing-actor `op342`
equipment updates remain entirely native. Scene creation never consumes a
private expert-job envelope; real profession
mutation remains the native `op205` path and its `EJXP` trailer. It never
opens or rebuilds a panel. Its only command
fallbacks are the audited op44 writer for the exact premium-contract
allow-list and the native op98 mode-0 refresh issued only after the full
party-directory owner opens. The disjoint-all bridge emits only the native
UI's exact selected slots and leaves all item/reward authority to op26 on the
server. The `op1456` capability announcement is also not a gameplay fallback:
it contains only the current EXE's class-1 response-handler bitset, is never
reflected back to the client, and grants no state.

The `90cn-decode-bypass-v1` protocol profile, supported client executable,
asset hashes, and `90CN.dll` hash form one compatibility unit. Change and
validate them together.

## Configuration

Production `90CN.dll` reads **no** environment variable and **no** INI file. Every
behavioral knob is a compile-time constant in the `clientConfig` block at the top
of `90CN.cpp`; the Anton raid probe merged into this DLL keeps its own constants in
`raidProbeConfig` at the top of `raid_probe.cpp`. Change a value and rebuild.

| Constant (`clientConfig`) | Value | Was | Effect |
| --- | --- | --- | --- |
| `kMultiClientLaunch` | `true` | `DNF_MULTI_CLIENT=1` | Isolates the audited instance mutex/window so a second client can start. |
| `kResidentChannelID` | `11` | `DNF_RESIDENT_CHANNEL_ID` | Redirects bootstrap port `10019` to `10000 + channel ID`; `0` disables the redirect. |
| `kCipherPassthroughEnabled` | `true` | `DNF_CIPHER_PASSTHROUGH` | The upper-body plaintext codec bypass and the later DPROTO/route hooks. |
| `kDprotoCompatEnabled` | `true` | `DNF_DPROTO_COMPAT` | Outbound DPROTO wrapper compatibility. |
| `kSceneRouteCompatEnabled` | `true` | `DNF_ROUTE_COMPAT` | Inbound scene/UI route predicate admission. |
| `kQuestSingleTargetEnabled` | `true` | `DNF_QUEST_SINGLE_TARGET` | Exact-row task-completion sender. |
| `kOptionalLuaPluginEnabled` | `true` | `DNF_LUA_PLUGIN` | Loads a sibling `90CNLua.dll` when present. |
| `kProtocolTraceEnabled` | `false` | `DNF_PROTOCOL_TRACE=1` | Writes packet bodies to `90CN_protocol.log` during bounded transport diagnosis. |
| `kSocketTraceEnabled` | `false` | `90CN_socket_trace.ini` | Historical socket/item-panel trace hooks. |
| `kBuiltInRegionName` / `kBuiltInRegionID` | `奇迹暖暖` / `1` | `DNF_REGION_NAME`, `DNF_REGION_ID`, `90CN_region.ini` | The region name the patch publishes. |
| `kRemoteAuthTicket` / `kRemoteAuthHost` / `kRemoteAuthPorts` | empty | `DNF_REMOTE_AUTH_*` | LANA remote authentication; empty means disabled. |
| `kRemoteAuthLease` | empty | `DNF_LID_LEASE` | Launcher identity lease appended to a version 2 LANA answer; forwarded verbatim, never verified here. |

| Constant (`raidProbeConfig`, `raid_probe.cpp`) | Value | Was | Effect |
| --- | --- | --- | --- |
| `kApplyClaimPatch` | `true` | `patch_claim=1` | Stops the client from adopting a raid it merely browsed. |
| `kRescueTwentyPlayerButton` | `true` | `force_putong=1` | Opens the create window when the dead mode-select button is clicked. |

## Optional Lua plugin

The separately built `../client-lua/90CNLua.dll` can reuse the current EXE's
statically linked Lua 5.1.5 runtime. Production `90CN.dll` only discovers and
loads that sibling module (`kOptionalLuaPluginEnabled`); the plugin owns its Lua
hooks, script path, stack restoration, and hot reload. Removing the module, or
setting the constant to `false`, returns the transport DLL to its ordinary
behavior.

The persistent external script is `lua\init.lua` beside `90CNLua.dll`; the
game-owned monster AI states receive `lua\ai\init.lua` after their stock
scripts and native `AIBridge` registration complete.
`DNF_LUA_SCRIPT` may select an absolute path or one relative to the plugin
directory. The plugin owns a dedicated Lua thread and a plugin-owned Lua state;
timers and reloads stay on that same thread. `dnf90.notify(text)` resolves the
transport DLL's exported `Queue90CNClientNotice` interface and only submits a
copied message; the existing DNF main-window procedure paints and expires the
owned click-through popup. The transport DLL records only the proved
`ui_ready`, `ui_closed`, accepted typed class0/op24 `enter_town`, and
op28-to-first-op29-to-current-op3 `enter_dungeon` lifecycle boundaries in a
separate fixed queue. While that dungeon lifecycle is active, later complete
op29 packets publish deduplicated `dungeon_room_changed` events, and the final
accepted town op24 publishes `leave_dungeon` before `enter_town`. Later room
packets cannot replay `enter_dungeon`. The append-only event record also carries
the op28 dungeon ID and boss coordinate classification plus the fully parsed
op29 room, layer, and map context. `is_boss_room` is coordinate-backed rather
than a hard-coded map guess. The exported dequeue preserves the original
20-byte prefix for old consumers and returns the complete 60-byte record to the
current worker;
`90CNLua.dll` polls the exported `Dequeue90CNClientEvent` interface and invokes
callbacks on its Lua worker, never on the DNF UI thread. See
`../client-lua/README.md` for the build and compatibility contract.

The transport also exports a fixed 24-byte game-command ABI and a scalar-only
actor snapshot ABI. Commands are copied into a 64-entry queue and drained by
the existing DNF main-window procedure. Only the signature-validated current
character motion path, the native mood/ANI packet-handler path, and read-only
actor snapshot refresh are accepted; raw pointers, packets, arbitrary memory
writes, skill calls, and arbitrary ANI paths are not part of the ABI.

Boss mutation adds one bounded command to that ABI. The UI thread resolves an
opaque Lua monster handle to exactly one current type-3 op29 actor, sends the
exact `BMU1` class-1/op1456 private request, and accepts only an exact
class-0/op413/type-3/version-1 response tied to the same room generation,
token, object key, and monster template. The response is consumed before the
native dispatcher and copied into the fixed decision queue; the transport DLL
does not alter monster attributes itself.

The optional plugin also renders a persistent top-left local debug panel from
the already accepted character snapshot, equipment count, combat-power result,
and confirmed town/dungeon lifecycle events. It is click-through, hides when
the game is minimized or not foreground, and has no memory-write or packet
construction path. `dnf90.set_debug_panel_enabled(false)` hides it.

The same optional plugin may query stat, equipment and service-authored PVF
damage-affix snapshots, calculate a custom V4 score, and submit a
`DNF90CombatPanelState`. The transport DLL renders
that copied state only on the DNF main-window thread. Its native owner-open
check is read-only; it does not create, close, move, or mutate owner `0xC9`.
The formula keeps the six damage categories visible and uses the current EXE's
complete 33-slot worn snapshot. Unsupported TGP base-stat weights, ignore-defense
and armor-mastery coefficients remain explicitly outside the score instead of
being fabricated from item IDs or unproved game-memory offsets.

Production reads no INI file at all: there is nothing to copy into the game
directory and no example left to copy. The example files that used to document
`90CN.ini` and `90CN_socket_trace.ini` are gone with the mechanisms they
described.

## Diagnostics

The transport worker writes bounded lifecycle information to `90CN_trace.log`.
Full packet-boundary logging is off and writes `90CN_protocol.log` only when
`kProtocolTraceEnabled` is set to `true` and the DLL rebuilt. The vectored exception
observer records selected fault context and always returns
`EXCEPTION_CONTINUE_SEARCH`; it does not suppress or recover an exception.

The Anton raid probe merged into this DLL writes its own `RaidProbe.log` next to
the client, and takes no configuration: its two switches are the constants in
`raidProbeConfig` (`raid_probe.cpp`). A non-game 32-bit host can run its dry-run
self-test through the exported `Install90CNRaidProbe` entry point --
`probe\selftest_raidprobe.ps1` does exactly that.

These logs are diagnostic evidence only. They do not authorize client-side
gameplay fixes or make the DLL an owner of packet business semantics.

## Separate debugger test project

`90CN-debug.vcxproj` builds the independent `90CN-debug.dll` test harness.
It is not compiled into, loaded by, or required by production `90CN.dll`.
Its `DllMain` does not install hooks; a test host must explicitly call the
exported `Install90CNDebugCompat` entry.

The separate project exists only for local debugger-compatibility testing. It
must not be copied to or shipped with the game client.

Build and run its 32-bit smoke test with:

```text
msbuild 90CN-debug.vcxproj /t:Rebuild /p:Configuration=Release /p:Platform=Win32
msbuild 90CN-debug-smoke.vcxproj /t:Rebuild /p:Configuration=Release /p:Platform=Win32
Release\90CN-debug-smoke.exe Release\90CN-debug.dll
```

## Build and install

Build production `90CN.dll` as Release/Win32:

```text
msbuild 90CN.vcxproj /t:Rebuild /p:Configuration=Release /p:Platform=Win32
```

Install `Release\90CN.dll` as the game directory's `90CN.dll`. Do not deploy
`90CN-debug.dll` or either INI example.

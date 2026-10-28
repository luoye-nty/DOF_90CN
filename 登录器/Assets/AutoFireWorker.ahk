#Requires AutoHotkey v2.0
; ============================================================
; DNF 登录器 - 连发 Worker（DAF 同款 vkFFsc + SendEvent 实现）
; 用法: AutoHotkey64.exe AutoFireWorker.ahk "sc2D:10|sc2C:20"
;   参数: 以 | 分隔的键条目，每条 = 扫描码(带sc前缀):间隔ms
;   前台仅匹配窗口标题 "地下城与勇士"
; ============================================================

ProcessSetPriority("High")

; ---- 托盘 ----
; 悬浮提示写死为 LYHelper.exe（AHK 默认值是 A_ScriptName = 编译产物文件名，
; 写死可以避免以后改产物名时提示跟着漂）；托盘图标在编译时用 /icon 指定为 Assets\DNF.ico。
A_IconTip := "LYHelper.exe"

; 解除系统定时器精度限制，使 SetTimer 间隔可达 1ms（DAF 同款）
DllCall("Winmm\timeBeginPeriod", "UInt", 1)
try InstallKeybdHook()

; ---- 解析配置 ----
configStr := A_Args.Has(1) ? A_Args[1] : ""
if (configStr = "") {
    ExitApp 1
}

bindings := []
for item in StrSplit(configStr, "|") {
    if (item = "") {
        continue
    }
    parts := StrSplit(item, ":")
    if (parts.Length < 2) {
        continue
    }
    scHex := parts[1]
    ms := Integer(parts[2])
    if (ms < 1) {
        ms := 1
    }
    bindings.Push({ pressKey: "sc" SubStr(scHex, 3), keyCode: "vkFF" scHex, intervalMs: ms, busy: false })
}

if (bindings.Length = 0) {
    ExitApp 1
}

; ---- 前台判定：仅匹配 DNF 私服窗口标题 ----
IsDnfActive() {
    return WinActive("地下城与勇士")
}

; ---- DAF SendIP 同款：完整"按下-抬起"敲击 ----
SendTap(keyCode) {
    SendEvent("{Blind}{" keyCode " DownTemp}")
    DllCall("Sleep", "UInt", 8)
    SendEvent("{Blind}{" keyCode " Up}")
    DllCall("Sleep", "UInt", 2)
}

; ---- 单键 tick：前台 + 物理按下才发，busy 防重入 ----
Tick(b) {
    if !IsDnfActive() {
        return
    }
    if b.busy {
        return
    }
    b.busy := true
    try {
        if GetKeyState(b.pressKey, "P") {
            SendTap(b.keyCode)
        }
    } finally {
        b.busy := false
    }
}

; ---- 每键一个定时器 ----
for b in bindings {
    SetTimer(Tick.Bind(b), b.intervalMs)
}

OnExit((*) => DllCall("Winmm\timeEndPeriod", "UInt", 1))
loop {
    Sleep(1000)
}

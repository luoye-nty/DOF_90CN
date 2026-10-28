package main

// PVF 加密页（一键简化版）：选择标准原版 Script.pvf → 点“开始加密”。
// 自动完成：服务器探活 → 自动版本号(90-日期-N) → 随机新 key 加密 →
//   落盘 已加密Script.pvf（分发物）/ 未加密Script.pvf（源备份）→ 自动上报服务器。
// 上报可选携带“客户端更新包直链”（zip/7z/rar 压缩包）与“是否启用加密校验”：
//   登录器自检会按服务端最新 SHA 与本机 Script.pvf 比对，不一致时登录按钮变“更新”，
//   点击后按直链下载压缩包解压覆盖客户端根目录，形成自动更新闭环。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

const (
	pvfEncName   = "已加密Script.pvf" // 加密产物（待分发）
	pvfPlainName = "未加密Script.pvf" // 源文件标准原版备份
)

// pvfLocalRecord 本地最近一次加密结果（pvf_last_enc.json，供重新上报）。
type pvfLocalRecord struct {
	Version           string `json:"pvf_version"`
	KeySet            string `json:"key_set"`
	SHA256            string `json:"file_sha256"`
	OutPath           string `json:"out_path"`
	EncAt             string `json:"encrypted_at"`
	FromKey           string `json:"from_key,omitempty"`           // 源密钥：空=标准原版（简化后恒为空，字段保留兼容旧记录）
	DownloadURL       string `json:"download_url,omitempty"`       // 客户端更新包直链（zip/7z/rar）
	ClientDownloadURL string `json:"client_download_url,omitempty"` // 客户端整包直链（zip/7z/rar）
	Enabled           *bool  `json:"enabled"`                      // 是否启用 PVF 加密校验；nil=启用（兼容旧记录）
}

func (r *pvfLocalRecord) enabled() bool {
	if r == nil || r.Enabled == nil {
		return true
	}
	return *r.Enabled
}

func pvfLocalRecordPath() string {
	return filepath.Join(filepath.Dir(DefaultConfigPath()), "pvf_last_enc.json")
}

func loadPvfLocalRecord() *pvfLocalRecord {
	data, err := os.ReadFile(pvfLocalRecordPath())
	if err != nil {
		return nil
	}
	var rec pvfLocalRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil
	}
	return &rec
}

func savePvfLocalRecord(rec *pvfLocalRecord) {
	data, _ := json.MarshalIndent(rec, "", "  ")
	_ = os.WriteFile(pvfLocalRecordPath(), data, 0o600)
}

func buildPvfTab(a *App) decl.TabPage {
	a.pvLastEnc = loadPvfLocalRecord()
	pvURL := ""
	pvClientURL := ""
	pvEnabled := true
	if a.pvLastEnc != nil {
		pvURL = a.pvLastEnc.DownloadURL
		pvClientURL = a.pvLastEnc.ClientDownloadURL
		pvEnabled = a.pvLastEnc.enabled()
	}
	return decl.TabPage{
		Title:   "PVF 加密",
		MinSize: decl.Size{Width: 900, Height: 520},
		Layout:  decl.VBox{},
		Children: []decl.Widget{
			decl.GroupBox{
				Title:  "一键加密并发布（加密 + 自动上报）",
				Layout: decl.VBox{},
				Children: []decl.Widget{
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "源 Script.pvf（标准原版）："},
							decl.LineEdit{
								AssignTo: &a.pvSrcLE,
								ReadOnly: true,
							},
							decl.PushButton{
								AssignTo:  &a.pvBrowseSrcBtn,
								Text:      "浏览...",
								OnClicked: func() { a.onPvfBrowseSrc() },
							},
							decl.PushButton{
								AssignTo:  &a.pvEncBtn,
								Text:      "开始加密",
								OnClicked: func() { a.onPvfEncrypt() },
							},
							decl.PushButton{
								AssignTo:  &a.pvReportBtn,
								Text:      "重新上报",
								OnClicked: func() { a.onPvfReport() },
							},
						},
					},
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "客户端更新包直链（zip/7z/rar，可选）："},
							decl.LineEdit{
								AssignTo: &a.pvUrlLE,
								Text:     pvURL,
								MinSize:  decl.Size{Width: 420, Height: 0},
								ToolTipText: "登录器检测到本机 Script.pvf 与服务端不一致时，按此直链下载压缩包并解压覆盖客户端根目录。留空则登录器会提示需要更新但无法自动下载。",
							},
							decl.CheckBox{
								AssignTo: &a.pvEnableChk,
								Text:     "启用 PVF 加密校验（登录器启动按最新哈希比对）",
								Checked:  pvEnabled,
								ToolTipText: "取消勾选并上报后，本次发布不启用加密校验：登录器不比对哈希、直接放行登录（适用于暂时只分发文件不强制换钥的场景）。",
							},
							decl.PushButton{
								AssignTo:  &a.pvSaveUrlBtn,
								Text:      "保存更新包",
								OnClicked: func() { a.onPvfSaveURLs() },
								ToolTipText: "把更新包直链与加密校验开关随最近一次加密记录上报到服务器（无需重新加密 PVF）",
							},
						},
					},
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "客户端整包直链（zip/7z/rar，可选）："},
							decl.LineEdit{
								AssignTo: &a.pvClientLE,
								Text:     pvClientURL,
								MinSize:  decl.Size{Width: 420, Height: 0},
								ToolTipText: "整包客户端压缩包直链（含 DNF.exe 等完整客户端）。登录器本机尚未配置客户端目录时，登录按钮会变为“下载客户端”，按此直链下载并解压到玩家选择目录。留空则登录器提示需要手动准备客户端。",
							},
							decl.PushButton{
								AssignTo:  &a.pvSaveClientBtn,
								Text:      "保存整包",
								OnClicked: func() { a.onPvfSaveURLs() },
								ToolTipText: "把整包直链与加密校验开关随最近一次加密记录上报到服务器（无需重新加密 PVF）",
							},
						},
					},
					decl.TextEdit{
						AssignTo: &a.pvResult,
						ReadOnly: true,
						VScroll:  true,
						MinSize:  decl.Size{Width: 0, Height: 200},
					},
					decl.Label{Text: "说明：选择本地改好的标准原版 Script.pvf，点“开始加密”。工具自动探活服务器、生成版本号（90-日期-N）、随机新密钥并加密。"},
					decl.Label{Text: "在源文件同目录生成“已加密Script.pvf”（分发给玩家替换）与“未加密Script.pvf”（源备份），随后自动上报服务器。"},
					decl.Label{Text: "勾选“启用 PVF 加密校验”后，登录器自检发现本机 Script.pvf 与服务端不一致时，登录按钮会变为“更新”；若填写了更新包直链，玩家点击“更新”即可自动下载解压覆盖客户端目录。"},
					decl.Label{Text: "若填写“客户端整包直链”，玩家本机尚无客户端时登录按钮会变为“下载客户端”，可一键下载整包并解压到自选目录。"},
					decl.Label{Text: "上报失败时产物保留，可点“重新上报”补报。"},

				},
			},
		},
	}
}

func (a *App) pvfSetBusy(b bool) {
	a.pvBusy = b
	if a.pvEncBtn != nil {
		a.pvEncBtn.SetEnabled(!b)
	}
	if a.pvBrowseSrcBtn != nil {
		a.pvBrowseSrcBtn.SetEnabled(!b)
	}
	if a.pvSaveUrlBtn != nil {
		a.pvSaveUrlBtn.SetEnabled(!b)
	}
	if a.pvSaveClientBtn != nil {
		a.pvSaveClientBtn.SetEnabled(!b)
	}
}

// onPvfSaveURLs 把更新包/整包直链与加密校验开关，随最近一次加密记录上报到服务器。
func (a *App) onPvfSaveURLs() {
	if a.pvBusy {
		return
	}
	rec := a.pvLastEnc
	if rec == nil {
		a.showInfo("暂无记录", "请先执行一次“开始加密”，生成版本记录后再保存直链。")
		return
	}
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	url := strings.TrimSpace(a.pvUrlLE.Text())
	clientURL := strings.TrimSpace(a.pvClientLE.Text())
	if url != "" && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		a.showInfo("参数错误", "更新包直链必须以 http:// 或 https:// 开头")
		return
	}
	if clientURL != "" && !strings.HasPrefix(clientURL, "http://") && !strings.HasPrefix(clientURL, "https://") {
		a.showInfo("参数错误", "客户端整包直链必须以 http:// 或 https:// 开头")
		return
	}
	enabled := true
	if a.pvEnableChk != nil {
		enabled = a.pvEnableChk.Checked()
	}
	a.pvfSetBusy(true)
	pvfAppendLog(a.pvResult, "保存直链：正在上报服务器…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		id, err := c.PvfReport(ctx, rec.Version, rec.SHA256, rec.KeySet, url, clientURL, enabled)
		a.sync(func() {
			a.pvfSetBusy(false)
			if err != nil {
				if !a.handleAPIError(err) {
					pvfAppendLog(a.pvResult, "保存直链失败："+err.Error())
					a.showError("保存直链失败", err)
				}
				return
			}
			rec.DownloadURL = url
			rec.ClientDownloadURL = clientURL
			rec.Enabled = &enabled
			savePvfLocalRecord(rec)
			pvfAppendLog(a.pvResult, fmt.Sprintf("直链已保存并上报（记录 ID %d）：更新包 %s / 整包 %s", id, url, clientURL))
			a.showInfo("保存成功", fmt.Sprintf("直链已保存并上报服务器（记录 ID %d）。", id))
		})
	}()
}

func pvfAppendLog(ed *walk.TextEdit, s string) {
	if ed == nil {
		return
	}
	prev := ed.Text()
	if prev != "" && !strings.HasSuffix(prev, "\r\n") {
		prev += "\r\n"
	}
	ed.SetText(prev + s)
}

// onPvfBrowseSrc 选择源 Script.pvf（标准原版）。
func (a *App) onPvfBrowseSrc() {
	if a.pvBusy {
		return
	}
	dlg := new(walk.FileDialog)
	dlg.Title = "选择源 Script.pvf（标准原版）"
	dlg.Filter = "PVF 文件 (*.pvf)|*.pvf|所有文件 (*.*)|*.*"
	ok, err := dlg.ShowOpen(a.mw)
	if err != nil || !ok {
		return
	}
	a.pvSrcLE.SetText(dlg.FilePath)
}

// onPvfEncrypt 一键加密入口：校验源文件后进入后台自动流水。
func (a *App) onPvfEncrypt() {
	if a.pvBusy {
		return
	}
	src := strings.TrimSpace(a.pvSrcLE.Text())
	if src == "" {
		a.showInfo("参数不完整", "请先选择源 Script.pvf（标准原版）")
		return
	}
	if fi, err := os.Stat(src); err != nil || fi.IsDir() {
		a.showInfo("参数错误", "源文件不存在或不是文件")
		return
	}
	enabled := true
	if a.pvEnableChk != nil {
		enabled = a.pvEnableChk.Checked()
	}
	a.pvfSetBusy(true)
	pvfAppendLog(a.pvResult, "开始加密："+src)
	go a.pvfEncryptWorker(src, enabled)
}

// pvfEncryptWorker 自动流水：探活 → 版本号 → 加密 → 备份源 → 上报。
// 直链不再随加密一起上报：保留最近一次已保存的直链，避免覆盖服务器上已有的链接。
func (a *App) pvfEncryptWorker(src string, enabled bool) {
	t0 := time.Now()

	// 1) 服务器探活 + 拉取已发布列表（兼自动版本号数据源）
	c := a.snapshotClient()
	if c == nil {
		a.sync(func() {
			a.pvfSetBusy(false)
			pvfAppendLog(a.pvResult, "加密已取消：无法连接服务器（未登录）")
			a.showInfo("无法连接服务器", "无法连接服务器，不允许加密。请确认运维工具在线并已登录后重试。")
		})
		return
	}
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 20*time.Second)
	list, err := c.PvfList(probeCtx)
	probeCancel()
	if err != nil {
		a.sync(func() {
			a.pvfSetBusy(false)
			pvfAppendLog(a.pvResult, "加密已取消：无法连接服务器（"+err.Error()+"）")
			a.showInfo("无法连接服务器", "无法连接服务器，不允许加密。请确认运维工具在线并已登录后重试。")
		})
		return
	}

	// 2) 自动版本号（本地记录 + 服务器当天已发布列表取最大序号 +1）
	ver := autoPvfVersion(list, a.pvLastEnc)
	dir := filepath.Dir(src)
	dstEnc := filepath.Join(dir, pvfEncName)
	dstPlain := filepath.Join(dir, pvfPlainName)
	a.sync(func() {
		pvfAppendLog(a.pvResult, fmt.Sprintf("版本： %s\r\n输出： %s", ver, dstEnc))
	})

	// 3) 标准密钥加密（源必须为标准原版）
	keySet, sha, err := protectPVFFile(src, dstEnc, "", "")
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "parse src with old keys") || strings.Contains(msg, "header magic mismatch") {
			msg = "源文件格式未受支持或已被加密，请重新选择。"
		}
		a.sync(func() {
			a.pvfSetBusy(false)
			pvfAppendLog(a.pvResult, "加密失败："+msg)
			a.showError("加密失败", fmt.Errorf("%s", msg))
		})
		return
	}

	// 4) 备份源为标准原版副本（已有则直接覆盖）
	if err := copyFile(src, dstPlain); err != nil {
		a.sync(func() {
			pvfAppendLog(a.pvResult, "警告：备份未加密Script.pvf 失败（"+err.Error()+"）")
		})
	}

	// 5) 保存本地记录（先落盘，供上报失败后补报）
	// 直链沿用最近一次已保存的值，不在加密时从输入框读取/覆盖。
	prevURL := ""
	prevClientURL := ""
	if a.pvLastEnc != nil {
		prevURL = a.pvLastEnc.DownloadURL
		prevClientURL = a.pvLastEnc.ClientDownloadURL
	}
	rec := &pvfLocalRecord{
		Version:           ver,
		KeySet:            keySet,
		SHA256:            sha,
		OutPath:           dstEnc,
		EncAt:             time.Now().Format("2006-01-02 15:04:05"),
		FromKey:           "",
		DownloadURL:       prevURL,
		ClientDownloadURL: prevClientURL,
		Enabled:           &enabled,
	}
	a.pvLastEnc = rec
	savePvfLocalRecord(rec)

	// 6) 自动上报（直链沿用已保存值）
	reportCtx, reportCancel := context.WithTimeout(context.Background(), 20*time.Second)
	id, rerr := c.PvfReport(reportCtx, ver, sha, keySet, prevURL, prevClientURL, enabled)
	reportCancel()
	elapsed := time.Since(t0).Round(time.Millisecond).String()
	if rerr != nil {
		a.sync(func() {
			a.pvfSetBusy(false)
			if a.pvReportBtn != nil {
				a.pvReportBtn.SetEnabled(true)
			}
			pvfAppendLog(a.pvResult, fmt.Sprintf("加密完成但上报失败：%v\r\n产物已保留： %s\r\n可点“重新上报”补报。", rerr, dstEnc))
			a.showError("加密完成，上报失败", fmt.Errorf("加密产物已生成（%s），但上报服务器失败：%v。可稍后点“重新上报”补报。", dstEnc, rerr))
		})
		return
	}

	// 7) 成功收尾
	a.sync(func() {
		a.pvfSetBusy(false)
		if a.pvReportBtn != nil {
			a.pvReportBtn.SetEnabled(false)
		}
		pvfAppendLog(a.pvResult, fmt.Sprintf("加密并上报完成，耗时 %s\r\n版本： %s\r\n分发产物： %s\r\nSHA256： %s\r\n已上报服务器（记录 ID %d）", elapsed, ver, dstEnc, sha, id))
		a.showInfo("加密并上报完成", fmt.Sprintf("已完成加密并上报服务器（记录 ID %d）。\n\n版本： %s\n分发产物： %s\n源备份： %s\nSHA256： %s\n\n将“已加密Script.pvf”分发给玩家替换即可。", id, ver, dstEnc, dstPlain, sha))
	})
}

// autoPvfVersion 自动生成版本号 90-YYYYMMDD-N：
// N 取本地记录与服务器当天已发布列表中最大序号 +1；无当天记录则从 1 开始。
func autoPvfVersion(server []PvfPublished, local *pvfLocalRecord) string {
	prefix := "90-" + time.Now().Format("20060102") + "-"
	maxN := 0
	scan := func(ver string) {
		if !strings.HasPrefix(ver, prefix) {
			return
		}
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(ver, prefix), "%d", &n); err == nil && n > maxN {
			maxN = n
		}
	}
	if local != nil {
		scan(local.Version)
	}
	for i := range server {
		scan(server[i].PVFVersion)
	}
	return prefix + fmt.Sprintf("%d", maxN+1)
}

// copyFile 复制文件（目标存在则覆盖）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// onPvfReport 重新上报最近一次本地加密记录（兜底补报）。
func (a *App) onPvfReport() {
	if a.pvBusy {
		return
	}
	rec := a.pvLastEnc
	if rec == nil {
		a.showInfo("暂无记录", "暂无本地加密记录，请先执行“开始加密”。")
		return
	}
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	a.pvReportBtn.SetEnabled(false)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		id, err := c.PvfReport(ctx, rec.Version, rec.SHA256, rec.KeySet, rec.DownloadURL, rec.ClientDownloadURL, rec.enabled())
		a.sync(func() {
			if err != nil {
				a.pvReportBtn.SetEnabled(true)
				if !a.handleAPIError(err) {
					a.showError("上报失败", err)
				}
				return
			}
			pvfAppendLog(a.pvResult, fmt.Sprintf("已重新上报服务器（记录 ID %d）：版本 %s", id, rec.Version))
			a.showInfo("上报成功", fmt.Sprintf("已重新上报服务器（记录 ID %d）。登录器将能按该 SHA256 匹配下发 key。", id))
		})
	}()
}

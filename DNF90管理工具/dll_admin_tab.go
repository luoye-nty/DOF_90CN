package main

// 服务器选项页「可加载 DLL」分组逻辑。
//
// 设计口径（已与需求确认）：
//   - 管理工具只算「文件名 + SHA256」并上传，**不上传 dll 本体**（服主自行随客户端分发）；
//   - 登录器（外网版）在客户端根目录（DNF.exe 同目录）校验同名文件哈希，通过才注入；
//   - 每条可选择校验失败策略：拒绝启动游戏（登录器弹窗）或跳过注入；
//   - 保存是整表替换：表格里没有的行等于删除。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lxn/walk"
)

// dllPolicyLabels / dllPolicyValues 校验失败策略（下拉文案 ↔ 协议值）。
var (
	dllPolicyLabels = []string{"跳过注入（照常进游戏）", "拒绝启动游戏（登录器弹窗提示）"}
	dllPolicyValues = []string{"skip", "block"}
)

// dllMaxItems 与运维工具侧 store.dllMaxItems 同口径。
const dllMaxItems = 50

// dlTableModel 可加载 DLL 表模型（启用勾选框 + 文件名 + SHA256 + 策略）。
type dlTableModel struct {
	walk.TableModelBase
	items   []LauncherDLL
	checked []bool
}

func (m *dlTableModel) RowCount() int { return len(m.items) }

func (m *dlTableModel) reset(items []LauncherDLL) {
	m.items = items
	m.checked = make([]bool, len(items))
	for i, item := range items {
		m.checked[i] = item.Enabled
	}
}

// Checked 实现 walk.ItemChecker。
func (m *dlTableModel) Checked(index int) bool {
	if index < 0 || index >= len(m.checked) {
		return false
	}
	return m.checked[index]
}

// SetChecked 实现 walk.ItemChecker：只存状态并回写 items（保存时直接用它）。
func (m *dlTableModel) SetChecked(index int, checked bool) error {
	if index < 0 || index >= len(m.checked) {
		return nil
	}
	m.checked[index] = checked
	m.items[index].Enabled = checked
	return nil
}

func (m *dlTableModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	item := m.items[row]
	switch col {
	case 0:
		return "" // 勾选框列
	case 1:
		return item.FileName
	case 2:
		return item.SHA256
	case 3:
		return dllPolicyLabel(item.OnMismatch)
	}
	return nil
}

// dllPolicyLabel 协议值 → 下拉文案。
func dllPolicyLabel(value string) string {
	for i, v := range dllPolicyValues {
		if v == value {
			return dllPolicyLabels[i]
		}
	}
	return dllPolicyLabels[0]
}

// dlPolicyValue 下拉索引 → 协议值。
func (a *App) dlPolicyValue() string {
	if a.dlPolicyCb == nil {
		return dllPolicyValues[0]
	}
	index := a.dlPolicyCb.CurrentIndex()
	if index < 0 || index >= len(dllPolicyValues) {
		return dllPolicyValues[0]
	}
	return dllPolicyValues[index]
}

// dlSetPolicy 按协议值回填下拉。
func (a *App) dlSetPolicy(value string) {
	if a.dlPolicyCb == nil {
		return
	}
	for i, v := range dllPolicyValues {
		if v == value {
			a.dlPolicyCb.SetCurrentIndex(i)
			return
		}
	}
	a.dlPolicyCb.SetCurrentIndex(0)
}

// dlMsg 状态行文案（灰色常规 / 红色错误）。
func (a *App) dlMsg(text string, isErr bool) {
	if a.dlMsgL == nil {
		return
	}
	a.dlMsgL.SetText(text)
	if isErr {
		a.dlMsgL.SetTextColor(walk.RGB(190, 30, 30))
	} else {
		a.dlMsgL.SetTextColor(walk.RGB(120, 120, 120))
	}
}

// dlApplyTable 把内存列表刷进表格。
func (a *App) dlApplyTable() {
	if a.dlTable == nil {
		return
	}
	model, ok := a.dlTable.Model().(*dlTableModel)
	if !ok || model == nil {
		model = new(dlTableModel)
	}
	model.reset(a.dlItems)
	a.dlTable.SetModel(model)
}

// onDllPickFile 选本地 dll 并算出 SHA256（只上传文件名 + 哈希）。
func (a *App) onDllPickFile() {
	dlg := new(walk.FileDialog)
	dlg.Title = "选择要由登录器自动加载的 DLL"
	dlg.Filter = "DLL 文件 (*.dll)|*.dll|所有文件 (*.*)|*.*"
	ok, err := dlg.ShowOpen(a.mw)
	if err != nil || !ok {
		return
	}
	path := strings.TrimSpace(dlg.FilePath)
	if path == "" {
		return
	}
	name := filepath.Base(path)
	sum, err := dllFileSHA256(path)
	if err != nil {
		a.dlMsg("读取文件失败："+err.Error(), true)
		return
	}
	a.dlPickedPath = path
	if a.dlNameLE != nil {
		a.dlNameLE.SetText(name)
	}
	if a.dlHashLE != nil {
		a.dlHashLE.SetText(sum)
	}
	a.dlMsg(fmt.Sprintf("已计算 %s 的 SHA256（体积 %s）", name, dllFileSizeText(path)), false)
}

// dllFileSHA256 计算文件 SHA-256（小写 hex）。
func dllFileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// dllFileSizeText 文件体积文案（便于核对选错了大文件）。
func dllFileSizeText(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "未知"
	}
	size := info.Size()
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

// dlReadInputs 读取输入区并做与服务端同口径的本地预校验。
func (a *App) dlReadInputs() (LauncherDLL, error) {
	item := LauncherDLL{Enabled: true, OnMismatch: a.dlPolicyValue()}
	if a.dlNameLE != nil {
		item.FileName = strings.TrimSpace(a.dlNameLE.Text())
	}
	if a.dlHashLE != nil {
		item.SHA256 = strings.ToLower(strings.TrimSpace(a.dlHashLE.Text()))
	}
	if item.FileName == "" {
		return item, fmt.Errorf("请先点「选择本地 DLL…」或手动填文件名")
	}
	if strings.ContainsAny(item.FileName, `\/:`) {
		return item, fmt.Errorf("文件名不能带路径（dll 要放在客户端根目录）")
	}
	if !strings.HasSuffix(strings.ToLower(item.FileName), ".dll") {
		return item, fmt.Errorf("必须是 .dll 文件")
	}
	if len(item.SHA256) != 64 || !isHexString(item.SHA256) {
		return item, fmt.Errorf("SHA256 必须是 64 位十六进制（点「选择本地 DLL…」会自动算）")
	}
	return item, nil
}

// isHexString 纯 hex 判断。
func isHexString(text string) bool {
	for i := 0; i < len(text); i++ {
		c := text[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return text != ""
}

// onDllAdd 追加一条待保存条目。
func (a *App) onDllAdd() {
	item, err := a.dlReadInputs()
	if err != nil {
		a.dlMsg("无法添加："+err.Error(), true)
		return
	}
	if len(a.dlItems) >= dllMaxItems {
		a.dlMsg(fmt.Sprintf("最多 %d 条", dllMaxItems), true)
		return
	}
	for _, exist := range a.dlItems {
		if strings.EqualFold(exist.FileName, item.FileName) {
			a.dlMsg("列表里已经有同名 dll（请用「更新选中行」改它）", true)
			return
		}
	}
	a.dlItems = append(a.dlItems, item)
	a.dlApplyTable()
	if a.dlTable != nil {
		a.dlTable.SetCurrentIndex(len(a.dlItems) - 1)
	}
	a.dlMsg(fmt.Sprintf("已加入列表（共 %d 条），别忘了点「保存全部到服务器」", len(a.dlItems)), false)
}

// onDllUpdate 用输入区覆盖选中行（启用状态保留）。
func (a *App) onDllUpdate() {
	if a.dlTable == nil {
		return
	}
	index := a.dlTable.CurrentIndex()
	if index < 0 || index >= len(a.dlItems) {
		a.dlMsg("请先在表格里选中一行，再点「更新选中行」", true)
		return
	}
	item, err := a.dlReadInputs()
	if err != nil {
		a.dlMsg("无法更新："+err.Error(), true)
		return
	}
	item.ID = a.dlItems[index].ID
	item.Enabled = a.dlItems[index].Enabled
	a.dlItems[index] = item
	a.dlApplyTable()
	if a.dlTable != nil {
		a.dlTable.SetCurrentIndex(index)
	}
	a.dlMsg(fmt.Sprintf("已更新第 %d 行，别忘了点「保存全部到服务器」", index+1), false)
}

// onDllDelete 删除选中行。
func (a *App) onDllDelete() {
	if a.dlTable == nil {
		return
	}
	index := a.dlTable.CurrentIndex()
	if index < 0 || index >= len(a.dlItems) {
		a.dlMsg("请先在表格里选中一行，再点「删除选中行」", true)
		return
	}
	a.dlItems = append(a.dlItems[:index], a.dlItems[index+1:]...)
	a.dlApplyTable()
	a.dlMsg(fmt.Sprintf("已删除，剩余 %d 条；点「保存全部到服务器」后正式生效", len(a.dlItems)), false)
}

// onDllPick 双击表格某行：回填输入区（含策略下拉）。
func (a *App) onDllPick() {
	if a.dlTable == nil {
		return
	}
	index := a.dlTable.CurrentIndex()
	if index < 0 || index >= len(a.dlItems) {
		return
	}
	item := a.dlItems[index]
	if a.dlNameLE != nil {
		a.dlNameLE.SetText(item.FileName)
	}
	if a.dlHashLE != nil {
		a.dlHashLE.SetText(item.SHA256)
	}
	a.dlSetPolicy(item.OnMismatch)
	a.dlMsg(fmt.Sprintf("已回填第 %d 行到输入区（改动后点「更新选中行」）", index+1), false)
}

// onDllRefresh 拉取服务端清单并覆盖本页。
func (a *App) onDllRefresh(silent bool) {
	c := a.snapshotClient()
	if c == nil {
		if !silent {
			a.showInfo("未连接", "请先登录")
		}
		return
	}
	if a.dlBusy {
		return
	}
	a.dlBusy = true
	go func() {
		defer a.sync(func() { a.dlBusy = false })
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		items, err := c.DLLsGet(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				if !silent {
					a.showError("读取可加载 DLL 清单失败", err)
				}
				a.sync(func() { a.dlMsg("读取失败："+err.Error(), true) })
			}
			return
		}
		a.sync(func() {
			a.dlItems = items
			a.dlLoaded = true
			a.dlApplyTable()
			a.dlMsg(dllSummary(len(items)), false)
		})
	}()
}

// onDllSave 整表保存到服务器。
func (a *App) onDllSave() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	if a.dlBusy {
		a.showInfo("操作进行中", "正在与服务器通信，请稍候")
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再保存")
		return
	}
	items := append([]LauncherDLL(nil), a.dlItems...)
	enabled, blocking := 0, 0
	for _, item := range items {
		if item.Enabled {
			enabled++
		}
		if item.OnMismatch == "block" {
			blocking++
		}
	}
	tip := fmt.Sprintf("将把当前 %d 条 dll（启用 %d 条，其中 %d 条校验不过会拒绝启动游戏）整批写入服务器。\n\n表格里没有的条目等于删除。是否继续？",
		len(items), enabled, blocking)
	if len(items) == 0 {
		// 空表 = 整表清空：这一档清空后登录器（外网版）的"服务端指定 DLL"校验与注入全部失效，
		// 而且没有任何界面提示。所以空表要单独再确认一次（正常保存流程不受影响）。
		tip += "\n\n⚠ 当前列表是**空的**：确认后服务器上的 DLL 清单会被整表清空，"
		tip += "登录器的 DLL 校验/注入会随之全部失效。"
	}
	if !a.confirm("保存可加载 DLL 清单", tip) {
		a.endOp()
		return
	}
	if len(items) == 0 && !a.confirm("清空 DLL 清单？",
		"再确认一次：表格里一条都没有，保存会把服务器上的 DLL 清单全部删除。\n\n确定要清空吗？") {
		a.endOp()
		return
	}
	a.dlBusy = true
	a.dlMsg("正在保存…", false)
	go func() {
		defer a.sync(func() { a.dlBusy = false; a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		saved, err := c.DLLsApply(ctx, items)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("保存可加载 DLL 清单失败", err)
			}
			a.sync(func() { a.dlMsg("保存失败："+err.Error(), true) })
			return
		}
		a.sync(func() {
			a.dlItems = saved
			a.dlApplyTable()
			a.dlMsg("已保存："+dllSummary(len(saved)), false)
		})
	}()
}

// dllSummary 列表摘要文案。
func dllSummary(count int) string {
	if count == 0 {
		return "服务器上暂无可加载 DLL（点「选择本地 DLL…」后加入列表再保存）"
	}
	return fmt.Sprintf("已加载 %d 条可加载 DLL", count)
}

//go:build paid

// 远程数据库备份桥接实现（设计 P1 三端备份闭环）：
// 由 internal/manage Bridge 闭包调用，与 GUI「数据库备份」页共享同一套
// backupCfg/mutateAutoBackup/backupDir/stopServerAll/EnsureDatabaseReady/mysqlFromInstance 链路，
// 保证免费版与赞助版、本地与远程行为一致。本文件仅赞助版编译。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dnf90ops/internal/backup"
	"dnf90ops/internal/config"
	"dnf90ops/internal/manage"
	"dnf90ops/internal/serverdir"
)

// manageBackupStatus 返回自动备份配置 + 备份目录 + 运行态快照（管理工具 GET backup/config）。
func (a *App) manageBackupStatus() manage.BackupStatus {
	ab := a.backupCfg()
	st := manage.BackupStatus{
		Enabled:       ab.Enabled,
		IntervalHours: ab.IntervalHours,
		Keep:          ab.Keep,
		BaseAt:        ab.BaseAt,
		BackupDir:     a.backupDir(),
		MySQLMode:     true, // 备份仅 MySQL 模式提供
	}
	if ab.Enabled && ab.BaseAt != "" {
		if base, err := time.Parse(time.RFC3339, ab.BaseAt); err == nil {
			// 到点还没执行时，NextRunAt 算出来的"下一个未来时刻"会让人误以为一切正常，
			// 这里把"已到点未执行"单独标出来（管理工具据此提示）。
			if slot, due := dueAutoBackupSlot(ab, time.Now()); due {
				st.Overdue = true
				st.DueAt = slot.Format(time.RFC3339)
			}
			st.NextRunAt = backup.NextRunAt(base, ab.IntervalHours, time.Now()).Format(time.RFC3339)
		}
	}
	root := strings.TrimSpace(a.rootEdit.Text())
	if root == "" {
		root = strings.TrimSpace(a.cfg.ServerRoot)
	}
	if root != "" {
		ins := serverdir.Inspect(root)
		pmRun, _ := a.pm.Status()
		st.ServerRunning = (ins != nil && ins.Running) || pmRun || a.svcOn.Load()
	} else {
		st.ServerRunning = a.svcOn.Load()
	}
	st.AuthRunning = a.svcOn.Load()
	if a.svcOn.Load() && a.st != nil {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		st.MySQLAlive = a.st.Ping(cctx) == nil
		cancel()
	}
	return st
}

// manageBackupApply 应用自动备份配置（enabled / intervalHours / keep），基准时刻重置为 now。
// 与 GUI「保存设置」onBackupSave 完全同链路：mutateAutoBackup（UI 线程 + 落盘）。
func (a *App) manageBackupApply(ctx context.Context, enabled bool, intervalHours, keep int) (manage.BackupStatus, error) {
	if intervalHours < 1 || intervalHours > config.AutoBackupIntervalMax {
		return manage.BackupStatus{}, fmt.Errorf("非法备份间隔 %d（允许 1~%d 小时）", intervalHours, config.AutoBackupIntervalMax)
	}
	if keep < 1 || keep > config.AutoBackupKeepMax {
		return manage.BackupStatus{}, fmt.Errorf("非法保留份数 %d（允许 1~%d 份）", keep, config.AutoBackupKeepMax)
	}
	a.mutateAutoBackup(func(ab *config.AutoBackupConfig) {
		ab.Enabled = enabled
		ab.IntervalHours = intervalHours
		ab.Keep = keep
		ab.BaseAt = time.Now().Format(time.RFC3339)
	})
	a.ulog("远程管理：已%s自动备份（间隔 %d 小时，保留 %d 份，基准=当前时刻）",
		map[bool]string{true: "启用", false: "停用"}[enabled], intervalHours, keep)
	// 应用新上限后立刻清理一次，避免调小份数后旧备份一直占盘。
	if enabled {
		go a.pruneBackups()
	}
	return a.manageBackupStatus(), nil
}

// manageBackupList 列出服务器端备份目录全部备份（新→旧）。
func (a *App) manageBackupList() ([]manage.BackupFileEntry, error) {
	entries, err := backup.ListBackups(a.backupDir())
	if err != nil {
		return nil, err
	}
	out := make([]manage.BackupFileEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, manage.BackupFileEntry{
			Name:    filepath.Base(e.File),
			Size:    e.Size,
			ModTime: e.ModTime.Format(time.RFC3339),
		})
	}
	return out, nil
}

// manageBackupNow 立即执行一次在线备份（不停止服务端，与 GUI「立即备份」按钮同链路）。
// busy 在 UI 线程抢占，防止与 GUI 手动备份/还原/启停并发。
func (a *App) manageBackupNow(ctx context.Context) (manage.BackupNowResult, error) {
	res := manage.BackupNowResult{}
	if a.mw == nil {
		return res, errors.New("GUI 尚未就绪")
	}
	// 原子占位。旧写法用 a.mw.Synchronize 里检查再赋值，而 Synchronize 只是 PostMessage（异步），
	// 紧随其后的 acquireErr 判断很可能在闭包执行前就跑完了 —— 互斥形同虚设。
	if !a.tryBusy() {
		return res, errors.New("工具正忙（有其他任务执行中），请稍后再试")
	}
	defer a.clearBusy()

	mcfg, err := a.mysqlFromInstance()
	if err != nil {
		return res, err
	}
	dr, err := backup.DumpMySQL(mcfg, a.backupDir(), func(line string) {
		a.ulog("%s", line)
	})
	if err != nil {
		return res, err
	}
	a.pruneBackups()
	res.ZipName = filepath.Base(dr.ZipPath)
	res.TableCount = dr.TableCount
	res.RowCount = dr.RowCount
	a.ulog("远程管理：立即备份完成 %s（%d 张表，%d 行）", res.ZipName, res.TableCount, res.RowCount)
	return res, nil
}

// manageBackupRestore 远程覆盖还原：运行中先自动一键停止（认证/DNF90Server/MySQL），
// 幂等拉起便携 MySQL 后 RestoreMySQL。zipName 仅接受文件名；busy 在 UI 线程抢占，
// 防止与管理工具请求期间的 GUI 手动启停/备份并发。
func (a *App) manageBackupRestore(ctx context.Context, zipName string, logf func(string)) (manage.BackupRestoreResult, error) {
	log := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		a.ulog("%s", line)
		if logf != nil {
			logf(line)
		}
	}
	res := manage.BackupRestoreResult{ZipName: zipName}
	if zipName == "" || filepath.Base(zipName) != zipName || strings.ContainsAny(zipName, `/\`) {
		return res, errors.New("非法备份文件名")
	}
	zipFile := filepath.Join(a.backupDir(), zipName)
	if fi, err := os.Stat(zipFile); err != nil || fi.IsDir() {
		return res, errors.New("备份文件不存在")
	}
	root := strings.TrimSpace(a.rootEdit.Text())
	if root == "" {
		root = strings.TrimSpace(a.cfg.ServerRoot)
	}
	if root == "" {
		return res, errors.New("未选择服务端目录，无法还原")
	}
	if a.mw == nil {
		return res, errors.New("GUI 尚未就绪")
	}
	// 抢占 busy，避免与 GUI 任务（启停/备份/还原）并发。
	// 原子占位。旧写法用 a.mw.Synchronize 里检查再赋值，而 Synchronize 只是 PostMessage（异步），
	// 紧随其后的 acquireErr 判断很可能在闭包执行前就跑完了 —— 互斥形同虚设。
	if !a.tryBusy() {
		return res, errors.New("工具正忙（有其他任务执行中），请稍后再试")
	}
	defer a.clearBusy()

	ins := serverdir.Inspect(root)
	pmRun, _ := a.pm.Status()
	if (ins != nil && ins.Running) || pmRun || a.svcOn.Load() {
		log("检测到服务端正在运行，先自动停止（认证服务/DNF90Server/MySQL）...")
		if err := a.stopServerAll(); err != nil {
			return res, fmt.Errorf("关闭服务端失败: %v", err)
		}
		res.Stopped = true
		log("服务端已停止，开始还原数据库...")
	} else {
		log("服务端当前未运行，直接开始还原数据库...")
	}
	if err := serverdir.EnsureDatabaseReady(root, "mysql", func(line string) {
		log("%s", line)
	}, 180*time.Second); err != nil {
		return res, fmt.Errorf("启动便携 MySQL 失败: %v", err)
	}
	mcfg, err := a.mysqlFromInstance()
	if err != nil {
		return res, err
	}
	rr, err := backup.RestoreMySQL(mcfg, zipFile, func(line string) { log("%s", line) })
	if err != nil {
		return res, err
	}
	res.TableCount = rr.TableCount
	res.RowCount = rr.RowCount
	log("还原完成：%s（%d 张表，%d 行）", zipName, rr.TableCount, rr.RowCount)
	return res, nil
}

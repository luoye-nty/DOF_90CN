package store

// 游戏角色（dnf_characters）的只读查询。
//
// 为什么单独一个文件：dnf_characters 是**游戏库**的表（不是登录器的 launcher_* 表），
// 运维工具直查它只有两类用途 ——
//  ① 服务端 GM 接口不可用时的只读兜底（redeem.go 里的 GameCharacter / 黑钻现状，用的是别的口径）；
//  ② 批量任务需要"全服唯一目标"这种走回环口拿不到的集合（本文件）。
//
// 全部**只读**：改游戏数据一律走服务端 GM 接口 —— 直写游戏库会被在线角色的内存快照
// 在下次存档时覆盖回去（邮件"复活"就是这么来的），这是本项目一贯的边界。

import (
	"context"
	"database/sql"
	"errors"
)

// ServerFirstCharacter 全服创建最早的角色（"全服第一个角色"）。
//
// AccountID 是游戏侧口径的账号主键（dnf:<sha256(用户名)>，与 launcher_accounts.account_id 同源）。
// 要显示成登录用户名得由调用方自己去翻译（manage 侧用 AccountNamesByID）：
// 翻译是"锦上添花"，这里不为了好看去 JOIN 登录器库 —— 两个库甚至可能不在同一个 MySQL 实例上。
type ServerFirstCharacter struct {
	CharacterID string `json:"character_id"`
	Name        string `json:"name"`
	AccountID   string `json:"account_id"`
	Level       int    `json:"level"`
}

// serverFirstCharacterSQL "全服第一个角色"的查询口径。
//
// 排序三级，缺一不可：
//  1. (created_at IS NULL) —— created_at 可空（老数据/直接导入的行没有它）。MySQL 里
//     NULL 排最前，不先按"是否为空"分组的话，一行 created_at 为空的老数据会永远压在所有
//     有时间的角色前面，而它很可能根本不是最早的；
//  2. created_at ASC —— 正常口径：创建最早的；
//  3. CAST(character_id AS UNSIGNED) ASC —— 兜底：character_id 是 VARCHAR，但全局单调递增，
//     同一时刻创建（或 created_at 都为空）时用它定序。**这一级不能省**：不加它，同秒创建的
//     两个角色谁先谁后由存储引擎决定，两次调用可能给出不同的"全服第一个角色"——
//     那是给错人发东西。
//
// delete_flag = 0：已删除的角色不参与（否则会把目标定到一个玩家根本看不到的角色上）。
const serverFirstCharacterSQL = `SELECT character_id, name, account_id, level
		 FROM dnf_characters
		 WHERE delete_flag = 0
		 ORDER BY (created_at IS NULL), created_at ASC, CAST(character_id AS UNSIGNED) ASC
		 LIMIT 1`

// ServerFirstCharacter 查全服创建最早的角色（只读）。
//
// 查不到返回 (零值, false, nil)："全服还没角色"是业务状态而不是故障，
// 由调用方决定怎么提示运营（批量任务里它会变成一条可读的 failed 原因）。
func (s *Store) ServerFirstCharacter(ctx context.Context) (ServerFirstCharacter, bool, error) {
	var out ServerFirstCharacter
	err := s.db.QueryRowContext(ctx, serverFirstCharacterSQL).
		Scan(&out.CharacterID, &out.Name, &out.AccountID, &out.Level)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ServerFirstCharacter{}, false, nil
		}
		return ServerFirstCharacter{}, false, err
	}
	return out, true, nil
}

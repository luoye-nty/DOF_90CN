// 本文件把「持久化能力接口」的名字放在仓储层，供服务层使用。
//
// 为什么需要它：投影读缓存（internal/services/dnfbridge/projection_read_cache.go）要在一次扇出里
// 包住仓储实现，并在转发时保留这些可选能力（字段级保存、批量字段保存、连通性自检、TTL）。
// 这些接口定义在 internal/platform/db，而 Source Boundaries 的规则 2
// （internal/architecture/dependency_test.go:99-104）规定服务层不得 import platform/db，
// 只能经 internal/modules/dnf/repository 访问持久化。仓储层本来就在用这些类型
// （各仓储接口内嵌 db.Store[Record]），所以由它把名字交给服务层正是这条规则的既定路径。
//
// 这里刻意用**类型别名**而不是新定义的接口：别名与原类型完全等价，服务层里的类型断言、
// 泛型实例化与转发行为一字不变（改前改后语义相同，只是名字换了归属）。
package repository

import (
	"context"

	"longheng.io/server/internal/platform/db"
)

// Store 是按 key 读写记录的持久化存储。
type Store[T any] = db.Store[T]

// FieldStore 能只保存一条记录的指定字段。
type FieldStore[T any, F comparable] = db.FieldStore[T, F]

// BatchFieldStore 能批量保存多条记录的指定字段。
type BatchFieldStore[T any, F comparable] = db.BatchFieldStore[T, F]

// FieldSave 是一次「记录 + 要保存的字段」。
type FieldSave[T any, F comparable] = db.FieldSave[T, F]

// Checker 是存储的连通性自检能力（仓储健康检查）。
type Checker = db.Checker

// Expirer 是给 key 设置 TTL 的能力。
type Expirer = db.Expirer

// SaveFields 只保存给定字段；normalize 为 nil 时不做字段规整。
func SaveFields[T any, F comparable](
	ctx context.Context,
	store Store[T],
	record T,
	normalize func([]F) []F,
	fields ...F,
) error {
	return db.SaveFields(ctx, store, record, normalize, fields...)
}

// 下面的编译期断言钉住「这些名字是**别名**，不是新定义的类型」。
//
// 为什么要钉：服务层（projection_read_cache.go）用 `x.(dnfrepo.FieldStore[T, F])` 这样的类型断言
// 来判断某个仓储实现是否支持字段级保存。别名两边可互相赋值，断言照旧命中；一旦有人把它们改成
// `type FieldStore[T any, F comparable] interface{ ... }`（新类型），断言就不再命中**仓储实现**，
// 投影读缓存会静默失效——不报错、不崩溃，只是每次扇出都重新读库（性能回退，且很难发现）。
// 有了这两组双向赋值断言，那种改动会**编译不过**。
var (
	_ Store[struct{}]                                                   = (db.Store[struct{}])(nil)
	_ db.Store[struct{}]                                                = (Store[struct{}])(nil)
	_ FieldStore[struct{}, int]                                         = (db.FieldStore[struct{}, int])(nil)
	_ db.FieldStore[struct{}, int]                                      = (FieldStore[struct{}, int])(nil)
	_ BatchFieldStore[struct{}, int]                                    = (db.BatchFieldStore[struct{}, int])(nil)
	_ db.BatchFieldStore[struct{}, int]                                 = (BatchFieldStore[struct{}, int])(nil)
	_ Checker                                                           = (db.Checker)(nil)
	_ db.Checker                                                        = (Checker)(nil)
	_ Expirer                                                           = (db.Expirer)(nil)
	_ db.Expirer                                                        = (Expirer)(nil)
	_ FieldSave[struct{}, int]                                          = db.FieldSave[struct{}, int]{}
	_ func(context.Context, Store[struct{}], struct{}, func([]int) []int, ...int) error = SaveFields[struct{}, int]
)


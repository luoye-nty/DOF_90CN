// 来源: DNF90GM工具_独立版 vendor/longheng.io/server/internal (modules/dnf/pvf) 于 2026-09-06 复制。
// 维护纪律: 以 GM 工具源码为唯一事实源; 同步升级时逐文件对比; import path 保持本端 module 内部路径。
// Package pvf 提供 DNF 项目层 PVF 文本解析和内存索引。
//
// 平台层 internal/platform/pvf 只负责 PVF 文件归档、解密、解压和按路径读文本。
// 本包负责把这些文本解析成 DNF 项目可消费的通用节点、section 和 lst 引用索引。
package pvf

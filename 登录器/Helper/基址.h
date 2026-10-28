/*
 * 基址.h - 登录器内嵌 Helper DLL 精简基址（对应 90 版本）
 *
 * 仅保留顺图 + 拾取所需的地址，与轮回DNFHelperProject - DLL版/src/基址.h 同源
 */

#pragma once
#include <windows.h>

/* ──── 人物/地图/物件 ──── */
#define ADDR_PLAYER_BASE 0x052B5BB0   /* 人物基址 */
#define OFF_MAP          0xBC         /* 人物→地图对象 */
#define OFF_HEAD         0xC0         /* 地图→链表头 */
#define OFF_TAIL         0xC4         /* 地图→链表尾 */
#define OFF_OBJ_TYPE     0x98         /* 物件类型偏移 */

/* ──── 顺图 过图CALL: 三级指针链获取pass对象 ──── */
#define CALL_DOOR_PASS      0x017C9D80   /* 过门CALL (thiscall, ecx=pass, [esp+4]=direction) */
#define ADDR_PASS_CHAIN_1   0x051B275C   /* 一级入口 (商店基址-8) */
#define OFF_PASS_CHAIN_2    0x020A028    /* 二级偏移 (时间基址常量) */
#define OFF_PASS            0x8C         /* 三级偏移 → pass对象 */

/* ──── op43 拾取发包三件套 ──── */
#define ADDR_SEND_BASE   0x052EA4BC   /* 发包对象指针 */
#define CALL_BUF         0x03454EB0   /* 缓冲CALL thiscall, ecx=[ADDR_SEND_BASE], push opcode */
#define CALL_ENC1        0x03454FC0   /* 加密CALL 长度1 */
#define CALL_ENC2        0x03454FF0   /* 加密CALL 长度2 */
#define CALL_ENC4        0x03455020   /* 加密CALL 长度4 */
#define CALL_SEND        0x034566F0   /* 发包CALL 无参 */

/* ──── 解密表 ──── */
#define ADDR_DECRYPT     0x052F7450   /* 解密基址（加密表基址） */

/* ──── 系统公告 ──── */
#define ADDR_SHOP_BASE    0x051B2764   /* 商店管理器基址 */
#define CALL_FORMAT_MSG   0x0181E750   /* 商店管理器::格式化公告 */
#define CALL_HORN_NOTIFY  0x02728160   /* 公告对象::发送() */

/* ──── 聊天栏消息投递 ──── */
#define CALL_MSG_INIT      0x01F2D8E0   /* 消息结构体初始化 ecx=dst */
#define CALL_MSG_DISPATCH  0x02727950   /* 消息投递 thiscall ret 4 */

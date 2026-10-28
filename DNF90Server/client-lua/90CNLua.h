#pragma once

#include <windows.h>

// 接口名称：Install90CNLua
// 中文说明：启动一次性的内置 Lua 运行时初始化和脚本工作线程。
// 调用方式：DLL_PROCESS_ATTACH 时会自动调用，外部加载器也可以主动调用。
// 返回值：TRUE 表示初始化流程已经启动或此前已经启动；FALSE 表示启动失败。
EXTERN_C BOOL WINAPI Install90CNLua();

// 接口名称：Request90CNLuaReload
// 中文说明：请求专用 Lua 工作线程重新加载外部脚本。
// 线程约束：本接口只登记请求，绝不会在调用本接口的线程中执行 Lua。
// 返回值：TRUE 表示重载请求已经登记。
EXTERN_C BOOL WINAPI Request90CNLuaReload();

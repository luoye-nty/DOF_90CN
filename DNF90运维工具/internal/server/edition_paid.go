//go:build paid

package server

// ServerEdition 标识当前运维工具构建的服务端版本：
//   - paid（-tags paid 构建）：赞助版运维工具，仅接受外网版（EXTERNAL）登录器连接
const ServerEdition = "paid"

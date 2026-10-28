//go:build !paid

package server

// ServerEdition 标识当前运维工具构建的服务端版本：
//   - free（默认，无 -tags paid）：免费版运维工具，仅接受免费版登录器连接
const ServerEdition = "free"

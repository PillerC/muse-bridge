// Package version 保存构建版本号。
//
// 单独成包是为了让 upgrade 包能读到版本，而不必反过来依赖 cmd/bridge。
// 版本号由 scripts/build.sh 通过 -ldflags -X 注入；没注入时是 dev，
// 升级器看到 dev 会认为"这是自己编译的版本"，不参与在线升级比较。
package version

// Version 由构建脚本注入，形如 v1.2.3、2249451 或 2249451-dirty。
var Version = "dev"

// Commit 是构建时的提交哈希，可选，由构建脚本注入。
var Commit = ""

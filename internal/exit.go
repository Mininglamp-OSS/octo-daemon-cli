package internal

import "fmt"

// ExitError 是 daemon 各层向上抛的"带 exit code 语义的错误"。main.go 顶层
// 统一映射到 os.Exit。约定见 plan §二.2.1：
//
//	0   SIGINT/SIGTERM 正常退出
//	1   运行期意外错误 / 未识别错误
//	2   启动期 fatal（config / 锁）
//	75  预留的 service manager respawn 请求（当前无 producer）
//	78  API key 永久失效（403）
type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("exit code %d", e.Code)
	}
	return e.Message
}

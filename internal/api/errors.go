package api

import "net/http"

// ErrorCode 是错误应答里稳定的代码（m5-plan v2.7 决定 0、design 11.1 v0.28）：界面和命令行的分支只看它，
// 不看文字（文字照旧返回，给人看）。代码表集中在这里，随类型生成进 Swift（ApiErrorCode）；只增不改。
type ErrorCode string

const (
	CodeNeedsAuthorization       ErrorCode = "needs_authorization"        // 403：需要本人授权（敏感操作，或这一次按敏感处理）
	CodeNeedsConfirm             ErrorCode = "needs_confirm"              // 409：两步确认（应答里有预览和确认码）
	CodeConfirmInvalid           ErrorCode = "confirm_invalid"            // 403：确认码无效、过期、用过，或目标变了
	CodeBusy                     ErrorCode = "busy"                       // 409：有别的操作在进行
	CodeInUse                    ErrorCode = "in_use"                     // 409：正在使用，不能删（来源正在作为主代理、出口被规则引用）
	CodeViaHostRequired          ErrorCode = "via_host_required"          // 422：未启用时抓取订阅，要显式选择经本机网络
	CodeLANUnconfirmed           ErrorCode = "lan_unconfirmed"            // 422：主代理的第一跳是局域网节点、还没有确认（node 是节点 ID）
	CodeSwitchRolledBack         ErrorCode = "switch_rolled_back"         // 更换主代理失败，已经回到原来的主代理
	CodeSwitchRollbackIncomplete ErrorCode = "switch_rollback_incomplete" // 更换主代理失败，恢复原来的主代理没有完成（稍后自动重试）
	CodeNoBaseline               ErrorCode = "no_baseline"                // 没有出口基准（要先应用主代理）
	CodeUnsupported              ErrorCode = "unsupported"                // 501：后台现在的模式不提供
	CodeInvalid                  ErrorCode = "invalid"                    // 400、422：校验不通过（field 是哪一项，能给时给）
	CodeNotFound                 ErrorCode = "not_found"                  // 404
	CodeTooLarge                 ErrorCode = "too_large"                  // 413：请求体太大
	CodeNotEnabled               ErrorCode = "not_enabled"                // 409：只有在启用状态下才能做
	CodeUnavailable              ErrorCode = "unavailable"                // 503：暂时不可用（状态文件损坏、订阅数到上限等）
	CodeInternal                 ErrorCode = "internal"                   // 500 及其他
	CodeUninstalled              ErrorCode = "uninstalled"                // 409：已经一键卸载过（M6-5a），要继续使用请重新安装
)

// ErrorCodes 是全部代码（生成 Swift 的枚举、测试核对用）。
var ErrorCodes = []ErrorCode{
	CodeNeedsAuthorization, CodeNeedsConfirm, CodeConfirmInvalid, CodeBusy, CodeInUse, CodeViaHostRequired, CodeLANUnconfirmed,
	CodeSwitchRolledBack, CodeSwitchRollbackIncomplete, CodeNoBaseline, CodeUnsupported, CodeInvalid, CodeNotFound, CodeTooLarge,
	CodeNotEnabled, CodeUnavailable, CodeInternal, CodeUninstalled,
}

// ErrorBody 是所有出错应答的形式。error 是给人看的文字（“做什么失败了：原因。下一步”），code 是稳定的代码；
// node、field 是少量结构化数据（局域网待确认的节点 ID、校验不通过的是哪一项）。
type ErrorBody struct {
	Error string    `json:"error"`
	Code  ErrorCode `json:"code"`
	Node  string    `json:"node,omitempty"`
	Field string    `json:"field,omitempty"`
}

// DefaultCode 是没有显式给代码时按状态码给的代码。
func DefaultCode(status int) ErrorCode {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return CodeInvalid
	case http.StatusForbidden:
		return CodeNeedsAuthorization
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeBusy
	case http.StatusRequestEntityTooLarge:
		return CodeTooLarge
	case http.StatusNotImplemented:
		return CodeUnsupported
	case http.StatusServiceUnavailable:
		return CodeUnavailable
	}
	return CodeInternal
}

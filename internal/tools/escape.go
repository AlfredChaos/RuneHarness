package tools

import "strings"

// EscapeXML 转义进入 XML 文本节点的三个元字符。注入消息的结构化块
// （<task_notification>、<scheduled_task>）共用，防内容里的标签文本
// 把块截断。
func EscapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

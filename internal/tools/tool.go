// Package tools 定义工具抽象与注册表。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Spec 是暴露给模型的工具元数据（JSON Schema 描述）。
type Spec struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Tool 是一个可被模型调用的能力。
// Run 的 rawArgs 是模型生成的 JSON 参数，由实现方自行反序列化与校验。
type Tool interface {
	Spec() Spec
	Run(ctx context.Context, rawArgs json.RawMessage) (string, error)
}

// Registry 按名字管理一组工具。
type Registry struct {
	byName map[string]Tool
	specs  []Spec
}

// NewRegistry 注册一组工具。
func NewRegistry(tt ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool, len(tt))}
	for _, t := range tt {
		r.byName[t.Spec().Name] = t
		r.specs = append(r.specs, t.Spec())
	}
	return r
}

// Specs 返回所有工具的元数据，供组装 LLM 请求。
func (r *Registry) Specs() []Spec {
	return r.specs
}

// Has 报告指定名字的工具是否已注册。
func (r *Registry) Has(name string) bool {
	_, ok := r.byName[name]
	return ok
}

// Result 是一次工具调用的结果。
// Output 是回填给模型的内容；IsError 标记该结果是否为失败，供 harness
// 与 hook 区分（OpenAI 兼容协议的 tool message 没有 is_error 字段，
// 模型侧仍靠 "error:" 前缀文本判断）。
type Result struct {
	Output  string
	IsError bool
}

// Call 执行一次工具调用。
// 所有失败（含 panic）都转成错误结果返回给模型，由模型自行纠错。
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (res Result) {
	t, ok := r.byName[name]
	if !ok {
		return Result{Output: "error: unknown tool " + name, IsError: true}
	}
	defer func() {
		if p := recover(); p != nil {
			res = Result{Output: fmt.Sprintf("error: tool %s panicked: %v", name, p), IsError: true}
		}
	}()
	out, err := t.Run(ctx, args)
	if err != nil {
		return Result{Output: "error: " + err.Error(), IsError: true}
	}
	return Result{Output: out}
}

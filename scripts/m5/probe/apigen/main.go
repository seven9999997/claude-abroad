// apigen 是 M5-1 探查 E5 的原型（m5-plan v2 第 5 节）：从 Go 的接口类型直接生成 Swift 的 Codable 类型（候选二），
// 同时生成 OpenAPI 3.1 的 components/schemas（给人看、给以后别的客户端用，也是候选一的输入）。
//
//	go run ./scripts/m5/probe/apigen -swift out/APITypes.swift -openapi out/openapi.json
//
// 规则：json 标签决定字段名，"-" 跳过；omitempty、指针、切片、map 一律是 Swift 的可选（Go 的 nil 切片会编成 null）；
// 实现了 json.Marshaler 或 encoding.TextMarshaler 的类型（netip.Addr、time.Time 等）当作字符串；
// 结构体按“包名 + 类型名”命名，避免两个包里的同名类型冲突；嵌入的结构体字段展开。
package main

import (
	"encoding"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	"claude-abroad/internal/daemon"
)

// roots 是这次原型生成的接口类型（M5-2 改成从路由表取）。
var roots = []reflect.Type{
	reflect.TypeOf(daemon.Status{}),
	reflect.TypeOf(daemon.ConfirmNeeded{}),
	reflect.TypeOf(daemon.RegionRequest{}),
	reflect.TypeOf(probeEvent{}),
}

// probeEvent 是事件推送的原型（M5-2 定稿前的示意）。
type probeEvent struct {
	Seq   int       `json:"seq"`
	Kind  string    `json:"kind"`
	State string    `json:"state,omitempty"`
	At    time.Time `json:"at"`
}

var (
	marshaler     = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshaler = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	swiftKeywords = map[string]bool{"protocol": true, "default": true, "class": true, "struct": true, "enum": true, "func": true,
		"var": true, "let": true, "in": true, "is": true, "self": true, "Type": true, "extension": true, "import": true, "return": true,
		"case": true, "switch": true, "where": true, "operator": true, "internal": true, "public": true, "private": true}
)

type field struct {
	JSON     string
	Swift    string
	Type     string // Swift 类型（不含 ?）
	Optional bool
	Schema   map[string]any
}

type gen struct {
	structs map[string][]field
	order   []string
}

func name(t reflect.Type) string {
	pkg := t.PkgPath()
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	n := t.Name()
	if pkg == "main" {
		pkg = "probe"
	}
	r := []rune(pkg)
	r[0] = unicode.ToUpper(r[0])
	m := []rune(n)
	m[0] = unicode.ToUpper(m[0])
	return string(r) + string(m)
}

func camel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	out := strings.Join(parts, "")
	if out == "" || unicode.IsDigit(rune(out[0])) {
		out = "f" + out
	}
	if swiftKeywords[out] {
		out = "`" + out + "`"
	}
	return out
}

// swiftType 返回 Swift 类型和 OpenAPI schema；遇到结构体时登记它。
func (g *gen) swiftType(t reflect.Type) (string, map[string]any) {
	if t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler) || t.Implements(textMarshaler) || reflect.PointerTo(t).Implements(textMarshaler) {
		if t == reflect.TypeOf(time.Time{}) {
			return "String", map[string]any{"type": "string", "format": "date-time"}
		}
		return "String", map[string]any{"type": "string"}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return g.swiftType(t.Elem())
	case reflect.String:
		return "String", map[string]any{"type": "string"}
	case reflect.Bool:
		return "Bool", map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if t == reflect.TypeOf(time.Duration(0)) {
			return "Int64", map[string]any{"type": "integer", "format": "int64"}
		}
		return "Int64", map[string]any{"type": "integer", "format": "int64"}
	case reflect.Float32, reflect.Float64:
		return "Double", map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return "String", map[string]any{"type": "string", "format": "byte"}
		}
		et, es := g.swiftType(t.Elem())
		return "[" + et + "]", map[string]any{"type": "array", "items": es}
	case reflect.Map:
		vt, vs := g.swiftType(t.Elem())
		return "[String: " + vt + "]", map[string]any{"type": "object", "additionalProperties": vs}
	case reflect.Struct:
		n := name(t)
		g.addStruct(t)
		return n, map[string]any{"$ref": "#/components/schemas/" + n}
	case reflect.Interface:
		return "JSONValue", map[string]any{}
	}
	panic(fmt.Sprintf("不支持的类型 %v", t))
}

func (g *gen) fields(t reflect.Type) []field {
	var out []field
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		parts := strings.Split(tag, ",")
		if f.Anonymous && parts[0] == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = append(out, g.fields(ft)...)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		jn := parts[0]
		if jn == "" {
			jn = f.Name
		}
		omit := false
		for _, p := range parts[1:] {
			if p == "omitempty" || p == "omitzero" {
				omit = true
			}
		}
		st, sc := g.swiftType(f.Type)
		k := f.Type.Kind()
		opt := omit || k == reflect.Pointer || k == reflect.Slice || k == reflect.Map || k == reflect.Interface
		out = append(out, field{JSON: jn, Swift: camel(jn), Type: st, Optional: opt, Schema: sc})
	}
	return out
}

func (g *gen) addStruct(t reflect.Type) {
	n := name(t)
	if _, ok := g.structs[n]; ok {
		return
	}
	g.structs[n] = nil // 先占位，防止递归
	g.order = append(g.order, n)
	g.structs[n] = g.fields(t)
}

func (g *gen) swift() string {
	var b strings.Builder
	b.WriteString("// 由 scripts/m5/probe/apigen 生成，不要手改（M5-1 探查 E5 的原型）。\n\nimport Foundation\n\n")
	b.WriteString(`/// 任意 JSON 值（接口里少数 any 类型的字段）。
public enum JSONValue: Codable, Sendable, Equatable {
    case string(String), number(Double), bool(Bool), object([String: JSONValue]), array([JSONValue]), null
    public init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null } else if let v = try? c.decode(Bool.self) { self = .bool(v) }
        else if let v = try? c.decode(Double.self) { self = .number(v) } else if let v = try? c.decode(String.self) { self = .string(v) }
        else if let v = try? c.decode([JSONValue].self) { self = .array(v) } else { self = .object(try c.decode([String: JSONValue].self)) }
    }
    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .string(let v): try c.encode(v)
        case .number(let v): try c.encode(v)
        case .bool(let v): try c.encode(v)
        case .object(let v): try c.encode(v)
        case .array(let v): try c.encode(v)
        case .null: try c.encodeNil()
        }
    }
}

`)
	names := append([]string(nil), g.order...)
	sort.Strings(names)
	for _, n := range names {
		fs := g.structs[n]
		fmt.Fprintf(&b, "public struct %s: Codable, Sendable {\n", n)
		for _, f := range fs {
			q := ""
			if f.Optional {
				q = "?"
			}
			fmt.Fprintf(&b, "    public var %s: %s%s\n", f.Swift, f.Type, q)
		}
		if len(fs) > 0 {
			b.WriteString("    enum CodingKeys: String, CodingKey {\n")
			for _, f := range fs {
				fmt.Fprintf(&b, "        case %s = %q\n", f.Swift, f.JSON)
			}
			b.WriteString("    }\n")
		}
		b.WriteString("}\n\n")
	}
	return b.String()
}

func (g *gen) openapi() map[string]any {
	schemas := map[string]any{}
	for n, fs := range g.structs {
		props := map[string]any{}
		var req []string
		for _, f := range fs {
			props[f.JSON] = f.Schema
			if !f.Optional {
				req = append(req, f.JSON)
			}
		}
		sort.Strings(req)
		s := map[string]any{"type": "object", "properties": props}
		if len(req) > 0 {
			s["required"] = req
		}
		schemas[n] = s
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]any{"title": "abroadd 本地接口（M5-1 原型）", "version": "0"},
		"paths": map[string]any{
			"/v1/status": map[string]any{"get": map[string]any{"responses": map[string]any{"200": map[string]any{
				"description": "状态", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/DaemonStatus"}}}}}}},
		},
		"components": map[string]any{"schemas": schemas},
	}
}

func main() {
	sw := flag.String("swift", "", "Swift 输出文件")
	oa := flag.String("openapi", "", "OpenAPI 输出文件")
	flag.Parse()
	g := &gen{structs: map[string][]field{}}
	for _, t := range roots {
		g.addStruct(t)
	}
	if *sw != "" {
		if err := os.WriteFile(*sw, []byte(g.swift()), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if *oa != "" {
		b, _ := json.MarshalIndent(g.openapi(), "", "  ")
		if err := os.WriteFile(*oa, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Printf("生成了 %d 个结构体\n", len(g.structs))
}

package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// Config 是 adapter 专有配置（来自 YAML/JSON/API），值为标量。
type Config map[string]any

// Clone 深拷贝。
func (c Config) Clone() Config {
	out := make(Config, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// FieldType ConfigSchema 字段类型。
type FieldType string

const (
	FieldString   FieldType = "string"
	FieldInt      FieldType = "int"
	FieldBool     FieldType = "bool"
	FieldDuration FieldType = "duration" // "5s" 形式
	FieldList     FieldType = "list"     // []string
)

// SchemaField 单个配置字段的声明（FR1.1a 动态表单）。
type SchemaField struct {
	Name     string    `json:"name"`
	Label    string    `json:"label"`
	Type     FieldType `json:"type"`
	Required bool      `json:"required"`
	Default  any       `json:"default,omitempty"`
	Secret   bool      `json:"secret,omitempty"` // 密钥字段：回显脱敏、留空不修改
	Help     string    `json:"help,omitempty"`
	Options  []string  `json:"options,omitempty"` // type=string 时的可选值枚举
}

// Schema 完整配置表单声明。
type Schema struct {
	Fields []SchemaField `json:"fields"`
}

// Field 按 name 查找。
func (s Schema) Field(name string) (SchemaField, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return SchemaField{}, false
}

// FieldError 指明具体字段的配置错误（U-F1-02：错误信息含字段名）。
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string { return fmt.Sprintf("config field %q: %s", e.Field, e.Reason) }

// NewFieldError 构造字段错误。
func NewFieldError(field, format string, args ...any) *FieldError {
	return &FieldError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// CoerceConfig 按 schema 规范化并校验配置：
//   - required 字段缺失/空 → FieldError
//   - 类型转换（YAML/JSON 加载后数字为 float64）→ 目标 Go 类型
//   - 枚举（Options）校验
//   - 未声明的字段忽略（宽容，便于向后兼容）
//
// 返回仅含声明字段且类型规范化的新 Config（缺失的取 Default）。
func CoerceConfig(s Schema, cfg Config) (Config, error) {
	out := make(Config, len(s.Fields))
	for _, f := range s.Fields {
		raw, present := cfg[f.Name]
		if !present || raw == nil || isEmptyString(raw) {
			if f.Default != nil {
				raw = f.Default
			} else if f.Required {
				return nil, &FieldError{Field: f.Name, Reason: "required"}
			} else {
				continue
			}
		}
		v, err := coerceValue(f, raw)
		if err != nil {
			return nil, err
		}
		out[f.Name] = v
	}
	return out, nil
}

func isEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

func coerceValue(f SchemaField, raw any) (any, error) {
	field := func(reason string, args ...any) error {
		return &FieldError{Field: f.Name, Reason: fmt.Sprintf(reason, args...)}
	}
	switch f.Type {
	case FieldString:
		switch v := raw.(type) {
		case string:
			if len(f.Options) > 0 && !containsStr(f.Options, v) {
				return nil, field("must be one of %s", strings.Join(f.Options, "|"))
			}
			return v, nil
		default:
			return nil, field("want string")
		}
	case FieldInt:
		switch v := raw.(type) {
		case int:
			return v, nil
		case int64:
			return int(v), nil
		case float64: // YAML/JSON 数字统一先变 float64
			return int(v), nil
		case string:
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, field("want int")
			}
			return n, nil
		default:
			return nil, field("want int")
		}
	case FieldBool:
		switch v := raw.(type) {
		case bool:
			return v, nil
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return nil, field("want bool")
			}
			return b, nil
		default:
			return nil, field("want bool")
		}
	case FieldDuration:
		switch v := raw.(type) {
		case string:
			d, err := time.ParseDuration(strings.TrimSpace(v))
			if err != nil {
				return nil, field("want duration like \"5s\"")
			}
			return d.String(), nil
		case float64: // 秒数
			return (time.Duration(v) * time.Second).String(), nil
		default:
			return nil, field("want duration")
		}
	case FieldList:
		switch v := raw.(type) {
		case []string:
			return append([]string(nil), v...), nil
		case []any:
			list := make([]string, 0, len(v))
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, field("want list of strings")
				}
				list = append(list, s)
			}
			return list, nil
		case string:
			if strings.TrimSpace(v) == "" {
				return []string{}, nil
			}
			parts := strings.Fields(v)
			return parts, nil
		default:
			return nil, field("want list")
		}
	default:
		return raw, nil
	}
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ResolveEnv 将配置值中的 ${ENV_VAR} 引用替换为环境变量实际值（F1.3 密钥载入解析）。
// 未定义的环境变量替换为空串并保持原样由必填校验拒绝。
func ResolveEnv(cfg Config) Config {
	out := cfg.Clone()
	for k, v := range out {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
			out[k] = os.Getenv(strings.TrimSuffix(strings.TrimPrefix(s, "${"), "}"))
		}
	}
	return out
}

// MaskedValue 密钥脱敏占位值。
const MaskedValue = "***"

// MaskConfig 返回脱敏副本：Secret 字段已设置 → MaskedValue，未设置 → 空（C-F1-04 回显）。
func MaskConfig(cfg Config, s Schema) Config {
	out := cfg.Clone()
	for _, f := range s.Fields {
		if !f.Secret {
			continue
		}
		if v, ok := out[f.Name]; ok && !isEmptyString(v) {
			out[f.Name] = MaskedValue
		}
	}
	return out
}

// MergeConfigSecrets 编辑配置时合并密钥：新值空或为脱敏占位 → 沿用旧值（PUT 留空表示不修改）。
func MergeConfigSecrets(newCfg, oldCfg Config, s Schema) Config {
	out := newCfg.Clone()
	for _, f := range s.Fields {
		if !f.Secret {
			continue
		}
		nv, ok := out[f.Name]
		if !ok || isEmptyString(nv) || nv == MaskedValue {
			if ov, exists := oldCfg[f.Name]; exists {
				out[f.Name] = ov
			}
		}
	}
	return out
}

// ToJSON 序列化配置（稳定字段序）。
func (c Config) ToJSON() (string, error) {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]any, len(c))
	for _, k := range keys {
		ordered[k] = c[k]
	}
	b, err := json.Marshal(ordered)
	if err != nil {
		return "", fmt.Errorf("adapter: marshal config: %w", err)
	}
	return string(b), nil
}

// ConfigFromJSON 反序列化配置。
func ConfigFromJSON(s string) (Config, error) {
	if strings.TrimSpace(s) == "" {
		return Config{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("adapter: unmarshal config: %w", err)
	}
	return Config(m), nil
}

// CapabilitiesToJSON / CapabilitiesFromJSON 存储层 JSON 互转。
func CapabilitiesToJSON(c Capabilities) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("adapter: marshal capabilities: %w", err)
	}
	return string(b), nil
}

func CapabilitiesFromJSON(s string) (Capabilities, error) {
	var c Capabilities
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return c, fmt.Errorf("adapter: unmarshal capabilities: %w", err)
	}
	return c, nil
}

// ---- 取值辅助（adapter 实现用）----

// GetStr 取 string 字段（缺省 fallback）。
func GetStr(cfg Config, key, fallback string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

// GetInt 取 int 字段。
func GetInt(cfg Config, key string, fallback int) int {
	switch v := cfg[key].(type) {
	case int:
		if v != 0 || fallback == 0 {
			if _, exists := cfg[key]; exists {
				return v
			}
		}
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// GetBool 取 bool 字段。
func GetBool(cfg Config, key string, fallback bool) bool {
	if v, ok := cfg[key].(bool); ok {
		return v
	}
	return fallback
}

// GetDuration 取 duration 字段（规范化为字符串存储）。
func GetDuration(cfg Config, key string, fallback time.Duration) time.Duration {
	switch v := cfg[key].(type) {
	case string:
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	case float64:
		return time.Duration(v) * time.Second
	}
	return fallback
}

// GetList 取 list 字段。
func GetList(cfg Config, key string) []string {
	switch v := cfg[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, it := range v {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

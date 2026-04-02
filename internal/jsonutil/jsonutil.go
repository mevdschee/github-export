package jsonutil

import (
	"encoding/json"
	"fmt"
	"strconv"
)

func Str(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", val)
	}
}

func Int(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return int64(val)
	case json.Number:
		n, _ := val.Int64()
		return n
	default:
		return 0
	}
}

func Bool(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	b, _ := v.(bool)
	return b
}

func Map(m map[string]any, key string) map[string]any {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	sub, _ := v.(map[string]any)
	return sub
}

func List(m map[string]any, key string) []any {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	list, _ := v.([]any)
	return list
}

func Logins(m map[string]any, key string) []string {
	list := List(m, key)
	var logins []string
	for _, item := range list {
		if u, ok := item.(map[string]any); ok {
			if login := Str(u, "login"); login != "" {
				logins = append(logins, login)
			}
		}
	}
	return logins
}

func LabelNames(m map[string]any, key string) []string {
	list := List(m, key)
	var names []string
	for _, item := range list {
		if l, ok := item.(map[string]any); ok {
			if name := Str(l, "name"); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

func UserLogin(m map[string]any, key string) string {
	u := Map(m, key)
	if u == nil {
		return ""
	}
	return Str(u, "login")
}

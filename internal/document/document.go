package document

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func YamlScalar(v any) string {
	switch val := v.(type) {
	case string:
		if val == "" {
			return `""`
		}
		b, _ := yaml.Marshal(val)
		return strings.TrimSpace(string(b))
	case bool:
		if val {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(val, 10)
	case int:
		return strconv.Itoa(val)
	case float64:
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	default:
		b, _ := yaml.Marshal(val)
		return strings.TrimSpace(string(b))
	}
}

type Writer struct {
	buf strings.Builder
}

func (d *Writer) KV(key string, value any) {
	if value == nil {
		return
	}
	if s, ok := value.(string); ok && s == "" {
		return
	}
	fmt.Fprintf(&d.buf, "%s: %s\n", key, YamlScalar(value))
}

func (d *Writer) KVIndent(indent, key string, value any) {
	if value == nil {
		return
	}
	if s, ok := value.(string); ok && s == "" {
		return
	}
	fmt.Fprintf(&d.buf, "%s%s: %s\n", indent, key, YamlScalar(value))
}

func (d *Writer) List(key string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(&d.buf, "%s:\n", key)
	for _, item := range items {
		fmt.Fprintf(&d.buf, "  - %s\n", YamlScalar(item))
	}
}

func (d *Writer) Buf() *strings.Builder {
	return &d.buf
}

func (d *Writer) String() string {
	return d.buf.String()
}

func WriteFirstDoc(w io.Writer, frontmatter, body string) {
	fmt.Fprint(w, "---\n")
	fmt.Fprint(w, frontmatter)
	fmt.Fprint(w, "---\n")
	body = strings.TrimSpace(body)
	if body != "" {
		fmt.Fprint(w, "\n")
		fmt.Fprint(w, body)
		fmt.Fprint(w, "\n")
	}
}

func WriteSubDoc(w io.Writer, frontmatter, body string) {
	fmt.Fprint(w, "\n---\n")
	fmt.Fprint(w, frontmatter)
	fmt.Fprint(w, "---\n")
	body = strings.TrimSpace(body)
	if body != "" {
		fmt.Fprint(w, "\n")
		fmt.Fprint(w, body)
		fmt.Fprint(w, "\n")
	}
}

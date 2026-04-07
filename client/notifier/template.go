package notifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

// templateData wraps Notification with access to the raw payload for templating.
type templateData struct {
	Notification
	payload map[string]interface{}
}

// PayloadField extracts a dot-separated key from the raw GitHub webhook payload.
// Example: {{PayloadField "pull_request.title"}} or {{PayloadField "commits.0.message"}}
func (d *templateData) PayloadField(key string) string {
	if d.payload == nil {
		return ""
	}
	return extractNestedField(d.payload, key)
}

func extractNestedField(data interface{}, key string) string {
	parts := strings.SplitN(key, ".", 2)
	current := parts[0]

	switch v := data.(type) {
	case map[string]interface{}:
		val, ok := v[current]
		if !ok {
			return ""
		}
		if len(parts) == 1 {
			return fieldToString(val)
		}
		return extractNestedField(val, parts[1])
	case []interface{}:
		// Support numeric index: "commits.0.message"
		idx := 0
		for _, ch := range current {
			if ch < '0' || ch > '9' {
				return ""
			}
			idx = idx*10 + int(ch-'0')
		}
		if idx >= len(v) {
			return ""
		}
		if len(parts) == 1 {
			return fieldToString(v[idx])
		}
		return extractNestedField(v[idx], parts[1])
	default:
		return ""
	}
}

func fieldToString(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case float64:
		// Render integers without decimal point.
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%g", val)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// ResolveTemplate renders a Go template string with notification data.
// Provides {{.PayloadField "key.path"}} for accessing raw webhook JSON.
func ResolveTemplate(tmplStr string, n Notification) string {
	data := &templateData{Notification: n}

	if len(n.RawPayload) > 0 {
		var m map[string]interface{}
		if err := json.Unmarshal(n.RawPayload, &m); err == nil {
			data.payload = m
		}
	}

	tmpl, err := template.New("notify").Parse(tmplStr)
	if err != nil {
		return tmplStr
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return tmplStr
	}
	return buf.String()
}

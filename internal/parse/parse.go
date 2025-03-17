package parse

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
)

func ParseETagToVersion(etag string) string {
	if len(etag) == 0 {
		return etag
	}
	// Strip the double quotes in the etag if present
	if etag[0] == '"' {
		etag = etag[1:]
	}
	if len(etag) > 0 && etag[len(etag)-1] == '"' {
		etag = etag[:len(etag)-1]
	}

	return etag
}

func ParseName(name string) map[string]string {
	repository := name
	tag := "latest"
	if strings.Contains(name, ":") {
		idx := strings.LastIndex(name, ":")
		repository = name[:idx]
		tag = name[idx+1:]
	}
	return map[string]string{
		"name":       name,
		"repository": repository,
		"tag":        tag,
	}
}

func ExpandPattern(pattern string, params map[string]string) (res string, err error) {
	tmpl, err := template.New("path").Option("missingkey=error").Parse(pattern)
	if err != nil {
		slog.Error(fmt.Sprintf("Invalid template [%s]: %s", pattern, err.Error()))
		return
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, params)
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to template [%s]: %s", pattern, err.Error()))
		return
	}
	return buf.String(), nil
}

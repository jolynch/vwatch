package util

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
)

func ParseName(name string) map[string]string {
	repository := name
	tag := ""
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

func ExpandPattern(pattern string, params map[string]string) string {
	tmpl, err := template.New("path").Parse(pattern)
	if err != nil {
		slog.Error(fmt.Sprintf("Invalid template [%s]: %s", pattern, err.Error()))
		return pattern
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, params)
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to template [%s]: %s", pattern, err.Error()))
		return pattern
	}
	return buf.String()
}

package conf

import (
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestFromEnv(t *testing.T) {
	expectedDefault := Config{
		ListenClient:          "127.0.0.1:8080",
		ListenServer:          "127.0.0.1:8008",
		ReplicateWith:         "",
		ReplicateEvery:        1 * time.Second,
		ReplicateResolveEvery: 10 * time.Second,
		ReplicateLimitBytes:   1 * 1024 * 1024,
		FillFrom:              "",
		FillPath:              "/v1/versions/{{.name}}",
		FillExpiry:            10 * time.Second,
		FillStrategy:          FillWatch,
		FillHeaders:           make(http.Header),
		BlockFor:              10 * time.Second,
		JitterPerWatch:        1 * time.Millisecond,
		LogLevel:              slog.LevelInfo,
		DataLimitBytes:        4 * 1024,
		DataLimitError:        true,
	}

	actualDefault := FromEnv()
	// Random
	expectedDefault.NodeId = actualDefault.NodeId

	if !reflect.DeepEqual(expectedDefault, actualDefault) {
		t.Error("Config default not correct")
	}

	repr := actualDefault.PrettyRepr()
	if len(repr) == 0 {
		t.Errorf("Expected a valid string representation")
	}
}

func TestFillHeaders(t *testing.T) {
	accept := "application/json"
	content := "application/text"
	t.Setenv("VWATCH_FILL_HEADERS_ACCEPT", fmt.Sprintf("Accept: %s", accept))
	t.Setenv("VWATCH_FILL_HEADERS_CONTENT", fmt.Sprintf("Content-Type: %s", content))

	config := FromEnv()
	if config.FillHeaders.Get("Accept") != accept {
		t.Errorf("Accept header not parsed properly")
	}
	if config.FillHeaders.Get("Content-Type") != content {
		t.Errorf("Content-Type header not parsed properly")
	}
}

func TestFillLogLevel(t *testing.T) {
	t.Setenv("VWATCH_LOG_LEVEL", "DEBUG")
	config := FromEnv()
	if config.LogLevel != slog.LevelDebug {
		t.Errorf("Expected debug logging level")
	}
}

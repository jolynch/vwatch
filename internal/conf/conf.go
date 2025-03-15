package conf

import (
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jolynch/vwatch/internal/repl"
)

type Config struct {
	Listen                string        `json:"listen"`
	ReplicateWith         string        `json:"replicate-with"`
	ReplicateEvery        time.Duration `json:"replicate-every"`
	ReplicateResolveEvery time.Duration `json:"replicate-resolve-every"`
	ReplicateLimitBytes   uint64        `json:"replicate-limit-bytes"`
	FillFrom              string        `json:"fill-from"`
	FillPath              string        `json:"fill-path"`
	FillExpiry            time.Duration `json:"fill-expiry"`
	FillStrategy          string        `json:"fill-strategy"`
	BlockFor              time.Duration `json:"block-for"`
	JitterFor             time.Duration `json:"jitter-for"`
	LogLevel              slog.Level    `json:"log-level"`
	DataLimitBytes        uint64        `json:"data-limit-bytes"`
}

func (config Config) PrettyRepr() string {
	var str strings.Builder
	// Json requires us to define a custom type which does not play nicely with flags ...
	str.WriteString("Config{\n")
	val := reflect.ValueOf(config)
	typ := reflect.Indirect(val)
	elems := make([]string, 0)
	maxLen := 0
	for i := range typ.NumField() {
		maxLen = max(maxLen, len(typ.Type().Field(i).Name))
	}
	for i := range typ.NumField() {
		key := fmt.Sprintf("%*s", -maxLen, typ.Type().Field(i).Name)
		elems = append(elems, fmt.Sprintf("  %s: %s", key, repr(val.Field(i).Interface())))
	}
	str.WriteString(strings.Join(elems, ",\n"))

	str.WriteString("\n}\n")
	return str.String()
}

func repr(val any) (result string) {
	switch v := val.(type) {
	case uint64:
		result = fmt.Sprintf("%d", v)
	case slog.Level:
		result = fmt.Sprintf("\"%s\"", v.String())
	default:
		result = fmt.Sprintf("\"%s\"", v)
	}
	return result
}

func FromEnv() Config {
	return Config{
		Listen:                EnvString("VWATCH_LISTEN", "127.0.0.1:8080"),
		ReplicateWith:         EnvString("VWATCH_REPLICATE_WITH", ""),
		ReplicateEvery:        EnvDuration("VWATCH_REPLICATE_EVERY", 1*time.Second),
		ReplicateResolveEvery: EnvDuration("VWATCH_REPLICATE_RESOLVE_EVERY", 10*time.Second),
		// Default replication limit is 1MiB at a time
		ReplicateLimitBytes: EnvUint("VWATCH_REPLICATE_LIMIT_BYTES", 1*1024*1024),
		FillFrom:            EnvString("VWATCH_FILL_FROM", ""),
		FillPath:            EnvString("VWATCH_FILL_PATH", "/version/{{.name}}"),
		FillExpiry:          EnvDuration("VWATCH_FILL_EXPIRY", 10*time.Second),
		FillStrategy:        EnvString("VWATCH_FILL_STRATEGY", repl.FillWatch),
		BlockFor:            EnvDuration("VWATCH_BLOCK_FOR", 10*time.Second),
		JitterFor:           EnvDuration("VWATCH_JITTER_FOR", 1*time.Second),
		LogLevel:            EnvLogLevel("VWATCH_LOG_LEVEL", slog.LevelInfo),
		// Default limit on data size is 4KiB, not a database!
		DataLimitBytes: EnvUint("VWATCH_DATA_LIMIT_BYTES", 4*1024),
	}
}

func EnvString(key string, fallback string) string {
	v, ok := os.LookupEnv(key)
	if ok {
		return v
	}
	return fallback
}

func EnvDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			slog.Error(fmt.Sprintf("Invalid duration %s=%s: %s", key, v, err.Error()))
			os.Exit(2)
		}
		return d
	}
	return fallback
}

func EnvUint(key string, fallback uint64) uint64 {
	v, ok := os.LookupEnv(key)
	if ok {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			slog.Error(fmt.Sprintf("Invalid uint %s=%s: %s", key, v, err.Error()))
			os.Exit(2)
		}
		return n
	}
	return fallback
}

func EnvLogLevel(key string, fallback slog.Level) slog.Level {
	v, ok := os.LookupEnv(key)
	if ok {
		slevel, err := LogLevel(v, fallback)
		if err != nil {
			slog.Error(fmt.Sprintf("Invalid LogLevel %s=%s: %s", key, v, err.Error()))
			os.Exit(2)
		}
		return slevel
	}
	return fallback
}

func LogLevel(value string, fallback slog.Level) (level slog.Level, err error) {
	level = slog.Level(0)
	err = level.UnmarshalText([]byte(value))
	if err != nil {
		return
	}
	return
}

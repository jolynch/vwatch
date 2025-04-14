package headers

import "fmt"

const (
	ETag             = "ETag"
	LastModified     = "Last-Modified"
	XLastSync        = "X-Last-Sync"
	ContentType      = "Content-Type"
	ContentTypeJson  = "application/json"
	ContentTypeBytes = "application/octet-stream"
	ServerTiming     = "Server-Timing"
)

// So we can pass header flags on the command line
type HeaderFlags []string

func (flags *HeaderFlags) String() string {
	return fmt.Sprintf("%v", *flags)
}

func (flags *HeaderFlags) Set(v string) error {
	*flags = append(*flags, v)
	return nil
}

package params

import (
	"fmt"
	"strings"
)

// DefaultFilePart is the multipart part name a --file path without "part=" is sent as.
const DefaultFilePart = "file"

// FormField is one multipart form field.
type FormField struct {
	Name  string
	Value string
}

// FilePart is one file to send as a multipart part.
type FilePart struct {
	Name string // part name
	Path string // local file path
}

// ParseFormFields parses repeated --field name=value flags, keeping their order. The value may itself contain "=".
func ParseFormFields(args []string) ([]FormField, error) {
	fields := make([]FormField, 0, len(args))
	for _, arg := range args {
		name, value, ok := strings.Cut(arg, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("--field %q: expected name=value", arg)
		}
		fields = append(fields, FormField{Name: name, Value: value})
	}
	return fields, nil
}

// ParseFileParts parses repeated --file flags: a bare path is sent as part "file", part=path as that part. A path
// that itself contains "=" can be given as file=path.
func ParseFileParts(args []string) ([]FilePart, error) {
	parts := make([]FilePart, 0, len(args))
	for _, arg := range args {
		name, path := DefaultFilePart, arg
		if before, after, ok := strings.Cut(arg, "="); ok && isPartName(before) {
			name, path = before, after
		}
		if strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("--file %q: expected a path or part=path", arg)
		}
		parts = append(parts, FilePart{Name: name, Path: path})
	}
	return parts, nil
}

// isPartName reports whether s looks like a multipart part name rather than the start of a file path.
func isPartName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

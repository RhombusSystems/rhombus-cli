package params

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// ParamMeta describes a parameter for skeleton generation.
type ParamMeta struct {
	Name     string // original camelCase name
	FlagName string // kebab-case flag name
	Type     string // "string", "integer", "number", "boolean", "array", "object"
	Required bool
	Example  any
}

// CollectFlags returns a map of flag-name → value for all flags that were explicitly set.
func CollectFlags(cmd *cobra.Command) map[string]string {
	flags := make(map[string]string)
	cmd.Flags().Visit(func(f *pflag.Flag) {
		// Skip meta-flags
		if f.Name == "cli-input-json" || f.Name == "generate-cli-skeleton" {
			return
		}
		flags[f.Name] = f.Value.String()
	})
	return flags
}

// BuildBody builds the JSON request body from flags and optional cli-input-json.
func BuildBody(flags map[string]string, cliInputJSON string) (map[string]any, error) {
	body := make(map[string]any)

	// Load cli-input-json first (flags override)
	if cliInputJSON != "" {
		data, err := loadJSON(cliInputJSON)
		if err != nil {
			return nil, fmt.Errorf("--cli-input-json: %w", err)
		}
		for k, v := range data {
			body[k] = v
		}
	}

	// Apply flags (override cli-input-json values)
	for flagName, value := range flags {
		key := kebabToCamel(flagName)
		body[key] = coerceValue(value)
	}

	return body, nil
}

// PrintSkeleton prints a JSON skeleton for the given parameters.
func PrintSkeleton(params []ParamMeta) error {
	skeleton := make(map[string]any)
	for _, p := range params {
		skeleton[p.Name] = exampleValue(p)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "    ")
	return enc.Encode(skeleton)
}

func loadJSON(input string) (map[string]any, error) {
	var data []byte
	var err error

	if strings.HasPrefix(input, "file://") {
		path := strings.TrimPrefix(input, "file://")
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading file %s: %w", path, err)
		}
	} else {
		data = []byte(input)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return result, nil
}

// kebabToCamel converts "group-uuid" → "groupUuid"
func kebabToCamel(s string) string {
	parts := strings.Split(s, "-")
	for i := 1; i < len(parts); i++ {
		if len(parts[i]) > 0 {
			runes := []rune(parts[i])
			runes[0] = unicode.ToUpper(runes[0])
			parts[i] = string(runes)
		}
	}
	return strings.Join(parts, "")
}

// coerceValue tries to parse as JSON (for arrays/objects/numbers/bools), falls back to string.
func coerceValue(s string) any {
	// Try JSON parse
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	return s
}

func exampleValue(p ParamMeta) any {
	if p.Example != nil {
		return p.Example
	}
	switch p.Type {
	case "string":
		return ""
	case "integer", "number":
		return 0
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	default:
		return ""
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

type Param struct {
	Name        string // original camelCase: "groupUuid"
	FlagName    string // kebab-case: "group-uuid"
	Type        string // "string"|"integer"|"number"|"boolean"|"array"|"object"
	Description string
	Required    bool
	Example     any
}

type Operation struct {
	OperationID string // "addUsersToAccessControlGroup"
	CommandName string // "add-users-to-access-control-group"
	GoFuncName  string // "AddUsersToAccessControlGroup"
	Path        string // "/api/accesscontrol/addUsersToAccessControlGroup"
	Summary     string
	Description string
	Params      []Param
}

type Service struct {
	Tag         string // "Camera Webservice"
	ServiceName string // "camera"
	FuncName    string // "Camera"
	FileName    string // "camera.go"
	Operations  []Operation
}

type openAPISpec struct {
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
}

type operationObj struct {
	OperationID string   `json:"operationId"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	RequestBody struct {
		Content map[string]struct {
			Schema struct {
				Ref string `json:"$ref"`
			} `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
}

type schemaObj struct {
	Type       string                     `json:"type"`
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
	AllOf      []json.RawMessage          `json:"allOf"`
}

type propertyObj struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Format      string `json:"format"`
	Ref         string `json:"$ref"`
	Example     any    `json:"example"`
	Items       *struct {
		Type string `json:"type"`
		Ref  string `json:"$ref"`
	} `json:"items"`
}

func ParseSpec(specPath string) ([]Service, error) {
	data, err := os.ReadFile(specPath)
	if err != nil {
		return nil, fmt.Errorf("reading spec: %w", err)
	}

	var spec openAPISpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parsing spec: %w", err)
	}

	// Group operations by tag
	serviceMap := make(map[string]*Service)

	for path, methods := range spec.Paths {
		for _, opRaw := range methods {
			var op operationObj
			if err := json.Unmarshal(opRaw, &op); err != nil {
				continue
			}
			if op.OperationID == "" {
				continue
			}

			tag := "Default"
			if len(op.Tags) > 0 {
				tag = op.Tags[0]
			}

			svc, ok := serviceMap[tag]
			if !ok {
				svc = &Service{
					Tag:         tag,
					ServiceName: TagToServiceName(tag),
					FuncName:    TagToFuncName(tag),
					FileName:    TagToFileName(tag),
				}
				serviceMap[tag] = svc
			}

			operation := Operation{
				OperationID: op.OperationID,
				CommandName: OperationIDToCommandName(op.OperationID),
				GoFuncName:  OperationIDToGoFuncName(op.OperationID),
				Path:        path,
				Summary:     sanitizeString(op.Summary),
				Description: sanitizeString(op.Description),
			}

			// Resolve request body schema to get params
			if schemaRef := getSchemaRef(op); schemaRef != "" {
				params := resolveParams(spec.Components.Schemas, schemaRef)
				operation.Params = params
			}

			svc.Operations = append(svc.Operations, operation)
		}
	}

	// Convert map to sorted slice
	var services []Service
	for _, svc := range serviceMap {
		// Sort operations by command name
		sort.Slice(svc.Operations, func(i, j int) bool {
			return svc.Operations[i].CommandName < svc.Operations[j].CommandName
		})
		services = append(services, *svc)
	}
	sort.Slice(services, func(i, j int) bool {
		return services[i].ServiceName < services[j].ServiceName
	})

	return services, nil
}

func getSchemaRef(op operationObj) string {
	if content, ok := op.RequestBody.Content["application/json"]; ok {
		return content.Schema.Ref
	}
	return ""
}

func resolveParams(schemas map[string]json.RawMessage, ref string) []Param {
	name := strings.TrimPrefix(ref, "#/components/schemas/")
	raw, ok := schemas[name]
	if !ok {
		return nil
	}

	var schema schemaObj
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}

	requiredSet := make(map[string]bool)
	for _, r := range schema.Required {
		requiredSet[r] = true
	}

	var params []Param
	for propName, propRaw := range schema.Properties {
		var prop propertyObj
		if err := json.Unmarshal(propRaw, &prop); err != nil {
			continue
		}

		p := Param{
			Name:        propName,
			FlagName:    CamelToKebab(propName),
			Description: sanitizeString(prop.Description),
			Required:    requiredSet[propName],
			Example:     prop.Example,
		}

		// Determine type
		if prop.Ref != "" {
			// Referenced type — resolve to check if it's an enum or object
			refType := resolveRefType(schemas, prop.Ref)
			p.Type = refType
		} else if prop.Type == "array" {
			p.Type = "array"
		} else if prop.Type != "" {
			p.Type = prop.Type
		} else {
			p.Type = "string"
		}

		params = append(params, p)
	}

	sort.Slice(params, func(i, j int) bool {
		return params[i].FlagName < params[j].FlagName
	})

	return params
}

func resolveRefType(schemas map[string]json.RawMessage, ref string) string {
	name := strings.TrimPrefix(ref, "#/components/schemas/")
	raw, ok := schemas[name]
	if !ok {
		return "string"
	}

	// Quick check: is it an enum?
	var check struct {
		Type string   `json:"type"`
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(raw, &check); err != nil {
		return "string"
	}

	if len(check.Enum) > 0 {
		return "string" // Enums are passed as strings
	}
	if check.Type == "object" || check.Type == "" {
		return "object"
	}
	return check.Type
}

func sanitizeString(s string) string {
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "`", "'")
	return s
}

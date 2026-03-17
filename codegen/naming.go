package main

import (
	"strings"
	"unicode"
)

// TagToServiceName converts "Camera Webservice" → "camera", "Access Control Webservice" → "access-control"
func TagToServiceName(tag string) string {
	tag = strings.TrimSuffix(tag, " Webservice")
	words := strings.Fields(tag)
	for i := range words {
		words[i] = strings.ToLower(words[i])
	}
	name := strings.Join(words, "-")
	// Avoid collision with cobra's built-in "help" command
	if name == "help" {
		name = "help-service"
	}
	return name
}

// TagToFileName converts "Camera Webservice" → "camera.go", "Access Control Webservice" → "access_control.go"
func TagToFileName(tag string) string {
	tag = strings.TrimSuffix(tag, " Webservice")
	words := strings.Fields(tag)
	for i := range words {
		words[i] = strings.ToLower(words[i])
	}
	return strings.Join(words, "_") + ".go"
}

// TagToFuncName converts "Camera Webservice" → "Camera", "Access Control Webservice" → "AccessControl"
func TagToFuncName(tag string) string {
	tag = strings.TrimSuffix(tag, " Webservice")
	words := strings.Fields(tag)
	for i := range words {
		if len(words[i]) > 0 {
			runes := []rune(words[i])
			runes[0] = unicode.ToUpper(runes[0])
			words[i] = string(runes)
		}
	}
	return strings.Join(words, "")
}

// OperationIDToCommandName converts "addUsersToAccessControlGroup" → "add-users-to-access-control-group"
func OperationIDToCommandName(opID string) string {
	var result []rune
	for i, r := range opID {
		if unicode.IsUpper(r) && i > 0 {
			result = append(result, '-')
		}
		result = append(result, unicode.ToLower(r))
	}
	return string(result)
}

// OperationIDToGoFuncName converts "addUsersToAccessControlGroup" → "AddUsersToAccessControlGroup"
func OperationIDToGoFuncName(opID string) string {
	if len(opID) == 0 {
		return opID
	}
	runes := []rune(opID)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// CamelToKebab converts "groupUuid" → "group-uuid"
func CamelToKebab(s string) string {
	var result []rune
	for i, r := range s {
		if unicode.IsUpper(r) && i > 0 {
			result = append(result, '-')
		}
		result = append(result, unicode.ToLower(r))
	}
	return string(result)
}

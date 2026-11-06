package params

import (
	"reflect"
	"testing"
)

func TestParseFormFields(t *testing.T) {
	got, err := ParseFormFields([]string{"opportunityId=006Ab", "poNumber=PO-17", "note=a=b", "empty="})
	if err != nil {
		t.Fatal(err)
	}
	want := []FormField{{"opportunityId", "006Ab"}, {"poNumber", "PO-17"}, {"note", "a=b"}, {"empty", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	for _, bad := range []string{"noequals", "=value", " =value"} {
		if _, err := ParseFormFields([]string{bad}); err == nil {
			t.Errorf("ParseFormFields(%q): want an error", bad)
		}
	}
}

func TestParseFileParts(t *testing.T) {
	got, err := ParseFileParts([]string{"po.pdf", "attachment=./docs/po.pdf", "/tmp/a=b.pdf", "file=/tmp/a=b.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	want := []FilePart{
		{"file", "po.pdf"},
		{"attachment", "./docs/po.pdf"},
		{"file", "/tmp/a=b.pdf"}, // "/tmp/a" is not a part name, so the whole argument is the path
		{"file", "/tmp/a=b.pdf"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	for _, bad := range []string{"", "file=", "file=  "} {
		if _, err := ParseFileParts([]string{bad}); err == nil {
			t.Errorf("ParseFileParts(%q): want an error", bad)
		}
	}
}

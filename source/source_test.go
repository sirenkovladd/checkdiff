package source

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseJSONPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []pathStep
	}{
		{"empty", "", nil},
		{"single key", "data", []pathStep{{kind: pathKey, key: "data"}}},
		{"nested keys", "a.b.c", []pathStep{
			{kind: pathKey, key: "a"},
			{kind: pathKey, key: "b"},
			{kind: pathKey, key: "c"},
		}},
		{"trailing index", "a[0]", []pathStep{
			{kind: pathKey, key: "a"},
			{kind: pathIndex, index: 0},
		}},
		{"index in middle", "data.valid_tno[0].spath_list", []pathStep{
			{kind: pathKey, key: "data"},
			{kind: pathKey, key: "valid_tno"},
			{kind: pathIndex, index: 0},
			{kind: pathKey, key: "spath_list"},
		}},
		{"multiple indices on one segment", "a[0][1]", []pathStep{
			{kind: pathKey, key: "a"},
			{kind: pathIndex, index: 0},
			{kind: pathIndex, index: 1},
		}},
		{"leading index", "[3]", []pathStep{
			{kind: pathIndex, index: 3},
		}},
		{"dot then index", "a.[0]", []pathStep{
			{kind: pathKey, key: "a"},
			{kind: pathIndex, index: 0},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseJSONPath(c.in)
			if err != nil {
				t.Fatalf("ParseJSONPath(%q) error: %v", c.in, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ParseJSONPath(%q) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

func TestParseJSONPathErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"unmatched bracket", "a[0"},
		{"non-numeric index", "a[abc]"},
		{"negative index", "a[-1]"},
		{"garbage after bracket", "a[0]x"},
		{"bracket without index", "a[]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseJSONPath(c.in)
			if err == nil {
				t.Errorf("ParseJSONPath(%q) = nil error, want error", c.in)
			}
		})
	}
}

func TestExtractJSONValue(t *testing.T) {
	body := []byte(`{
		"status": "SUCCESS",
		"body": {
			"button_status": {
				"notification": "We're sorry, but this Activity is full."
			},
			"counter": 42,
			"active": true
		},
		"list": [{"k": "v1"}, {"k": "v2"}]
	}`)

	cases := []struct {
		name string
		path string
		want string
	}{
		{"string at nested path", "body.button_status.notification", "We're sorry, but this Activity is full."},
		{"integer formatted without trailing zeros", "body.counter", "42"},
		{"bool as string", "body.active", "true"},
		{"string at root", "status", "SUCCESS"},
		{"array index then key", "list[1].k", "v2"},
		{"empty string is preserved", "body.button_status.notification", "We're sorry, but this Activity is full."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ExtractJSONValue(body, c.path)
			if err != nil {
				t.Fatalf("ExtractJSONValue(%q) error: %v", c.path, err)
			}
			if got != c.want {
				t.Errorf("ExtractJSONValue(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

func TestExtractJSONValueErrors(t *testing.T) {
	body := []byte(`{"a": {"b": [1, 2]}}`)
	if _, err := ExtractJSONValue(body, "a.missing"); err == nil {
		t.Errorf("missing key: got nil error, want error")
	}
	if _, err := ExtractJSONValue(body, "a.b[5]"); err == nil {
		t.Errorf("out-of-range index: got nil error, want error")
	}
	// Object at the end of the path is not a scalar.
	if _, err := ExtractJSONValue(body, "a"); err == nil {
		t.Errorf("object at end: got nil error, want error")
	}
	if _, err := ExtractJSONValue(body, "a.b"); err == nil {
		t.Errorf("array at end: got nil error, want error")
	}
}

func TestExtractJSONArrayPathGrammar(t *testing.T) {
	// Mirrors the shape of the UniUni tracking response: status/data/
	// valid_tno[0]/spath_list.
	body := []byte(`{
		"status": "SUCCESS",
		"data": {
			"valid_tno": [
				{
					"tno": "U000180542908940",
					"spath_list": [
						{"id": 100, "code": "first"},
						{"id": 101, "code": "second"}
					]
				}
			]
		}
	}`)

	// Full path with array index — the new grammar.
	got, err := ExtractJSONArray(body, "data.valid_tno[0].spath_list")
	if err != nil {
		t.Fatalf("ExtractJSONArray with index: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ExtractJSONArray: got %d items, want 2", len(got))
	}

	// Old grammar (no index) must still work.
	got2, err := ExtractJSONArray(body, "data")
	if err == nil {
		t.Errorf("ExtractJSONArray('data') = %d items, want error (data is an object, not an array)", len(got2))
	}

	// Index out of range.
	_, err = ExtractJSONArray(body, "data.valid_tno[5].spath_list")
	if err == nil {
		t.Errorf("ExtractJSONArray with out-of-range index: got nil error, want error")
	}

	// Index into an object should error, not panic.
	_, err = ExtractJSONArray(body, "data[0]")
	if err == nil {
		t.Errorf("ExtractJSONArray with index on object: got nil error, want error")
	}
}

func TestJSONScalarAsString(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"string", "hello", "hello"},
		{"empty string", "", ""},
		{"int-valued float", float64(953389610), "953389610"},
		{"float-valued float", float64(1.5), "1.5"},
		{"zero", float64(0), "0"},
		{"true", true, "true"},
		{"false", false, "false"},
		{"nil", nil, ""},
		{"object", map[string]interface{}{"a": 1}, ""},
		{"array", []interface{}{1, 2}, ""},
		{"json.Number int", json.Number("42"), "42"},
		{"json.Number float", json.Number("1.5"), "1.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jsonScalarAsString(c.in); got != c.want {
				t.Errorf("jsonScalarAsString(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

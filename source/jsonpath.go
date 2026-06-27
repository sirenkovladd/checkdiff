package source

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ExtractJSONValue navigates a path through a JSON document and
// returns the scalar value at the end of the path as a string.
// The path grammar is the same as ExtractJSONArray (object keys
// separated by ".", array indices in "[N]"), but the leaf can
// be a string, number, or bool rather than an array. Returns
// an empty string and an error if the path doesn't resolve or
// the leaf is a non-scalar (object/array). The non-scalar
// error is deliberate: if a user configures a json_value
// source pointing at an object, they almost certainly
// misconfigured the path, and silently returning "" would
// hide the bug behind a "no diff" notification.
//
// Used by the json_value fetcher for sources that track a
// single field's value (e.g. an API status string) rather
// than an array of items. Diff semantics: any change in the
// returned string is a notification.
func ExtractJSONValue(body []byte, path string) (string, error) {
	var current interface{}
	if err := json.Unmarshal(body, &current); err != nil {
		return "", fmt.Errorf("parse JSON: %w", err)
	}
	steps, err := ParseJSONPath(path)
	if err != nil {
		return "", err
	}
	for i, step := range steps {
		switch step.kind {
		case pathKey:
			m, ok := current.(map[string]interface{})
			if !ok {
				return "", fmt.Errorf("path %q: step %d: expected object before key %q", path, i+1, step.key)
			}
			v, ok := m[step.key]
			if !ok {
				return "", fmt.Errorf("path %q: step %d: key %q not found", path, i+1, step.key)
			}
			current = v
		case pathIndex:
			arr, ok := current.([]interface{})
			if !ok {
				return "", fmt.Errorf("path %q: step %d: expected array before index [%d]", path, i+1, step.index)
			}
			if step.index < 0 || step.index >= len(arr) {
				return "", fmt.Errorf("path %q: step %d: index [%d] out of range (len=%d)", path, i+1, step.index, len(arr))
			}
			current = arr[step.index]
		}
	}
	switch current.(type) {
	case map[string]interface{}, []interface{}:
		return "", fmt.Errorf("path %q: leaf is %T, not a scalar (json_value source needs a string/number/bool at the end of the path)", path, current)
	}
	return jsonScalarAsString(current), nil
}

// ExtractJSONArray navigates a path through a JSON document and
// returns the raw JSON of each element of the array found at
// the end. The path grammar is intentionally tiny:
//
//	""          - the root must be an array
//	"a"         - root must be {"a": [...]}
//	"a.b"       - root must be {"a": {"b": [...]}}
//	"a[0]"      - root must be {"a": [...]}, take index 0
//	"a[0].b"    - root must be {"a": [{"b": [...]}]}
//
// Segments separated by "." are object keys. A "[N]" suffix on
// a segment indexes into the array reached at that step.
// Multiple "[N]" suffixes on a single segment (e.g. "a[0][1]")
// traverse nested arrays. Indexing past the end of an array is
// an error. It does not support wildcards, filters, or negative
// indices.
func ExtractJSONArray(body []byte, path string) ([]json.RawMessage, error) {
	var current interface{}
	if err := json.Unmarshal(body, &current); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	steps, err := ParseJSONPath(path)
	if err != nil {
		return nil, err
	}
	for i, step := range steps {
		switch step.kind {
		case pathKey:
			m, ok := current.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("path %q: step %d: expected object before key %q", path, i+1, step.key)
			}
			v, ok := m[step.key]
			if !ok {
				return nil, fmt.Errorf("path %q: step %d: key %q not found", path, i+1, step.key)
			}
			current = v
		case pathIndex:
			arr, ok := current.([]interface{})
			if !ok {
				return nil, fmt.Errorf("path %q: step %d: expected array before index [%d]", path, i+1, step.index)
			}
			if step.index < 0 || step.index >= len(arr) {
				return nil, fmt.Errorf("path %q: step %d: index [%d] out of range (len=%d)", path, i+1, step.index, len(arr))
			}
			current = arr[step.index]
		}
	}
	arr, ok := current.([]interface{})
	if !ok {
		return nil, fmt.Errorf("path %q: expected array at the end", path)
	}
	out := make([]json.RawMessage, 0, len(arr))
	for _, v := range arr {
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

type pathStepKind int

const (
	pathKey   pathStepKind = iota // look up `key` in the current object
	pathIndex                     // take element `index` of the current array
)

type pathStep struct {
	kind  pathStepKind
	key   string
	index int
}

// ParseJSONPath parses a dot-separated path with optional
// "[N]" index suffixes into a sequence of steps. See
// ExtractJSONArray for the grammar.
func ParseJSONPath(path string) ([]pathStep, error) {
	if path == "" {
		return nil, nil
	}
	var steps []pathStep
	for _, segment := range strings.Split(path, ".") {
		// Split "key[N1][N2]..." into key + indices.
		i := strings.IndexByte(segment, '[')
		var key string
		var indices []int
		if i < 0 {
			key = segment
		} else {
			key = segment[:i]
			rest := segment[i:]
			for len(rest) > 0 {
				if rest[0] != '[' {
					return nil, fmt.Errorf("path %q: malformed segment %q (expected '[')", path, segment)
				}
				end := strings.IndexByte(rest, ']')
				if end < 0 {
					return nil, fmt.Errorf("path %q: malformed segment %q (unmatched '[')", path, segment)
				}
				idx, err := strconv.Atoi(rest[1:end])
				if err != nil {
					return nil, fmt.Errorf("path %q: invalid index %q in segment %q", path, rest[1:end], segment)
				}
				if idx < 0 {
					return nil, fmt.Errorf("path %q: negative index %d in segment %q", path, idx, segment)
				}
				indices = append(indices, idx)
				rest = rest[end+1:]
			}
		}
		if key != "" {
			steps = append(steps, pathStep{kind: pathKey, key: key})
		}
		for _, idx := range indices {
			steps = append(steps, pathStep{kind: pathIndex, index: idx})
		}
	}
	return steps, nil
}

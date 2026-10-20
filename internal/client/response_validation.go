package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
)

const maxResponseBytes = 16 << 20

// strictJSON rejects ambiguous duplicate properties and trailing documents,
// including exact duplicates in nested objects. Only top-level keys are folded
// for case-insensitive struct decoding; nested map keys remain case-sensitive.
// Errors never contain response data.
func strictJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return fmt.Errorf("JSON nesting limit exceeded")
		}
		token, err := d.Token()
		if err != nil {
			return fmt.Errorf("invalid JSON")
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return fmt.Errorf("invalid JSON object")
				}
				name, ok := key.(string)
				if depth == 0 {
					name = strings.ToLower(name)
				}
				if !ok || seen[name] {
					return fmt.Errorf("ambiguous JSON object")
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			close, err := d.Token()
			if err != nil || close != json.Delim('}') {
				return fmt.Errorf("invalid JSON object")
			}
		case json.Delim('['):
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			close, err := d.Token()
			if err != nil || close != json.Delim(']') {
				return fmt.Errorf("invalid JSON array")
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

// Decode transactionally: a type error must not partially overwrite the caller.
func decodeResponse(raw []byte, out any) error {
	dst := reflect.ValueOf(out)
	if dst.Kind() != reflect.Pointer || dst.IsNil() {
		return fmt.Errorf("response destination must be a pointer")
	}
	next := reflect.New(dst.Elem().Type())
	if err := json.Unmarshal(raw, next.Interface()); err != nil {
		return fmt.Errorf("invalid response type")
	}
	dst.Elem().Set(next.Elem())
	return nil
}

// A status code alone cannot distinguish a missing record from a missing
// route, reverse proxy failure, or authentication layer hiding a resource.
// Only the expected resource's structured tRPC error may erase state.
func confirmedNotFound(method, path string, query url.Values, payload []byte, status int, raw []byte) bool {
	if strictJSON(raw) != nil {
		return false
	}
	var envelope struct {
		Code string                     `json:"code"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return false
	}
	expected := "NOT_FOUND"
	if path == "/port.one" && status == http.StatusBadRequest {
		expected = "BAD_REQUEST"
	} else if status != http.StatusNotFound {
		return false
	}
	// Read the protocol keys exactly: nested objects may legitimately contain
	// case-distinct keys, so struct field folding must not reinterpret metadata.
	var envelopeStatus int
	if json.Unmarshal(envelope.Data["httpStatus"], &envelopeStatus) != nil || envelope.Code != expected || stringField(envelope.Data, "code") != expected || envelopeStatus != status || stringField(envelope.Data, "path") != strings.TrimPrefix(path, "/") {
		return false
	}
	lookup := method == http.MethodGet && strings.HasSuffix(path, ".one")
	deletion := method == http.MethodPost && (strings.HasSuffix(path, ".delete") || strings.HasSuffix(path, ".remove") || path == "/user.deleteApiKey")
	if !lookup && !deletion {
		return false
	}
	var id string
	if lookup {
		_, id = lookupIdentity(path, query)
	} else {
		var body map[string]json.RawMessage
		if json.Unmarshal(payload, &body) != nil {
			return false
		}
		for key, value := range body {
			if strings.HasSuffix(key, "Id") {
				var v string
				if json.Unmarshal(value, &v) == nil && v != "" {
					if id != "" {
						return false
					}
					id = v
				}
			}
		}
	}
	if id == "" {
		return false
	}
	return true
}

func lookupIdentity(path string, query url.Values) (string, string) {
	var field, id string
	for key, values := range query {
		if strings.HasSuffix(key, "Id") && len(values) == 1 && values[0] != "" {
			if id != "" {
				return "", ""
			}
			field, id = key, values[0]
		}
	}
	if path == "/organization.one" {
		field = "id"
	}
	return field, id
}

func stringField(object map[string]json.RawMessage, key string) string {
	var v string
	_ = json.Unmarshal(object[key], &v)
	return v
}
func responseProblem(method, path, reason string) error {
	return fmt.Errorf("%s %s: invalid API response (%s); prior state must be retained", method, path, reason)
}
func validateResponse(method, path string, query url.Values, raw []byte) error {
	if strictJSON(raw) != nil {
		return responseProblem(method, path, "malformed or ambiguous JSON")
	}
	if method != http.MethodGet || !strings.HasSuffix(path, ".one") {
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return responseProblem(method, path, "expected resource object")
	}
	field, id := lookupIdentity(path, query)
	if field == "" || id == "" || stringField(object, field) != id {
		return responseProblem(method, path, "resource identity mismatch")
	}
	return nil
}

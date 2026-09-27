package agentinstall

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// jsonObject is a JSON object that remembers key order, so rewriting a client
// configuration keeps every unrelated key exactly where the user put it.
type jsonObject struct {
	keys   []string
	values map[string]json.RawMessage
}

func newJSONObject() *jsonObject {
	return &jsonObject{values: map[string]json.RawMessage{}}
}

func (o *jsonObject) has(key string) bool {
	_, ok := o.values[key]
	return ok
}

func (o *jsonObject) get(key string) (json.RawMessage, bool) {
	value, ok := o.values[key]
	return value, ok
}

func (o *jsonObject) set(key string, value json.RawMessage) {
	if !o.has(key) {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

func (o *jsonObject) remove(key string) bool {
	if !o.has(key) {
		return false
	}
	delete(o.values, key)
	for index, existing := range o.keys {
		if existing == key {
			o.keys = append(o.keys[:index], o.keys[index+1:]...)
			break
		}
	}
	return true
}

// decodeJSONObject parses a JSON object while preserving key order. Line and
// block comments are tolerated because several clients document JSONC files,
// but they cannot be round-tripped through the standard library encoder.
func decodeJSONObject(data []byte) (*jsonObject, error) {
	stripped := bytes.TrimSpace(stripJSONComments(data))
	object := newJSONObject()
	if len(stripped) == 0 {
		return object, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(stripped))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return nil, fmt.Errorf("expected a JSON object")
	}
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		if keyErr != nil {
			return nil, keyErr
		}
		key, keyOK := keyToken.(string)
		if !keyOK {
			return nil, fmt.Errorf("expected an object key")
		}
		var value json.RawMessage
		if valueErr := decoder.Decode(&value); valueErr != nil {
			return nil, valueErr
		}
		object.set(key, value)
	}
	if _, err = decoder.Token(); err != nil {
		return nil, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing content")
	}
	return object, nil
}

// compact renders the object as compact JSON in key order.
func (o *jsonObject) compact() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for index, key := range o.keys {
		if index > 0 {
			buffer.WriteByte(',')
		}
		encoded, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buffer.Write(encoded)
		buffer.WriteByte(':')
		if err = json.Compact(&buffer, o.values[key]); err != nil {
			return nil, err
		}
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

// render renders the object using the supplied indentation and a trailing
// newline, matching the conventions of hand-edited configuration files.
func (o *jsonObject) render(indent string) ([]byte, error) {
	compacted, err := o.compact()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err = json.Indent(&out, compacted, "", indent); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// detectIndent reports the indentation string used by an existing document so
// rewrites do not reformat the whole file.
func detectIndent(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || len(trimmed) == len(line) {
			continue
		}
		return line[:len(line)-len(trimmed)]
	}
	return "  "
}

// stripJSONComments removes // and /* */ comments outside of string literals.
func stripJSONComments(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false
	for index := 0; index < len(data); index++ {
		character := data[index]
		if inString {
			out = append(out, character)
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}
			continue
		}
		switch {
		case character == '"':
			inString = true
			out = append(out, character)
		case character == '/' && index+1 < len(data) && data[index+1] == '/':
			for index < len(data) && data[index] != '\n' {
				index++
			}
			if index < len(data) {
				out = append(out, '\n')
			}
		case character == '/' && index+1 < len(data) && data[index+1] == '*':
			index += 2
			for index+1 < len(data) && !(data[index] == '*' && data[index+1] == '/') {
				index++
			}
			index++
		default:
			out = append(out, character)
		}
	}
	return out
}

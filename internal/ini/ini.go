// INI parser matching Python's configparser with interpolation off
package ini

import (
	"fmt"
	"regexp"
	"strings"
)

// Section whose values every other section inherits
const DefaultSection = "DEFAULT"

// Parsed INI file
type File struct {
	sections map[string]map[string]string
	defaults map[string]string
	headers  []Header
}

// A section header and the line it's on, counted from 1
type Header struct {
	Name string
	Line int
}

// List every section header in file order, [DEFAULT] included, so text edits agree with the parser
func (f *File) Headers() []Header { return f.headers }

// Error for a problem at one line of the source
type Error struct {
	Source string
	Line   int
	Msg    string
}

// Format the error the way configparser words it
func (e *Error) Error() string {
	return fmt.Sprintf("While reading from '%s' [line %2d]: %s", e.Source, e.Line, e.Msg)
}

// Key, the first = or : delimiter, and value
var optRe = regexp.MustCompile(`^(.*?)\s*([=:])\s*(.*)$`)

// Whitespace trimmed around lines, keys and values
const space = " \t\n\r\f\v"

// Parse INI text, naming it source in error messages
func Parse(text, source string) (*File, error) {
	raw := map[string]map[string][]string{}
	defaultsRaw := map[string][]string{}
	var order []string
	var headers []Header
	var cur map[string][]string
	var sectName, optName string
	indentLevel := 0
	seen := map[string]bool{}

	// One pass over the lines, any line ending accepted
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, line := range lines {
		n := i + 1

		// Values end up in shortcuts.vdf, where a NUL ends a string and would corrupt the whole file
		if strings.ContainsRune(line, 0) {
			return nil, &Error{source, n, "contains a NUL byte"}
		}
		value := strings.Trim(line, space)
		comment := strings.HasPrefix(value, "#") || strings.HasPrefix(value, ";")
		if comment || value == "" {
			// Blank lines can sit inside a multi-line value, comments never do
			if !comment && cur != nil && optName != "" {
				cur[optName] = append(cur[optName], "")
			}
			continue
		}

		// Lines indented deeper than their key continue its value
		indent := len(line) - len(strings.TrimLeft(line, space))
		if cur != nil && optName != "" && indent > indentLevel {
			cur[optName] = append(cur[optName], value)
			continue
		}
		indentLevel = indent

		// The header runs to the last ] on the line
		if strings.HasPrefix(value, "[") && strings.LastIndex(value, "]") > 1 {
			name := value[1:strings.LastIndex(value, "]")]
			switch {
			case name == DefaultSection:
				cur = defaultsRaw
			case seen[name]:
				return nil, &Error{source, n, fmt.Sprintf("section '%s' already exists", name)}
			default:
				cur = map[string][]string{}
				raw[name] = cur
				order = append(order, name)
			}
			seen[name] = true
			headers = append(headers, Header{name, n})
			sectName, optName = name, ""
			continue
		}

		// key = value inside a section, each key once per section
		if cur == nil {
			return nil, &Error{source, n, fmt.Sprintf("file contains no section headers, found %q", line)}
		}
		m := optRe.FindStringSubmatch(value)
		if m == nil || m[1] == "" {
			return nil, &Error{source, n, fmt.Sprintf("can't parse %q, expected key = value", line)}
		}
		optName = strings.ToLower(strings.TrimRight(m[1], space))
		key := sectName + "\x00" + optName
		if seen[key] {
			return nil, &Error{source, n, fmt.Sprintf("option '%s' in section '%s' already exists", optName, sectName)}
		}
		seen[key] = true
		cur[optName] = []string{strings.Trim(m[3], space)}
	}

	// Values joined, sections looked up by name
	f := &File{sections: map[string]map[string]string{}, defaults: join(defaultsRaw), headers: headers}
	for _, name := range order {
		f.sections[name] = join(raw[name])
	}
	return f, nil
}

// Join multi-line values with newlines, dropping trailing blank lines
func join(m map[string][]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = strings.TrimRight(strings.Join(v, "\n"), space)
	}
	return out
}

// Check whether a section exists, [DEFAULT] not counting as one
func (f *File) Has(name string) bool {
	_, ok := f.sections[name]
	return ok
}

// Copy a section's values with [DEFAULT] filled in, empty when the section doesn't exist
func (f *File) Section(name string) map[string]string {
	out := map[string]string{}
	s, ok := f.sections[name]
	if !ok {
		return out
	}
	for k, v := range f.defaults {
		out[k] = v
	}
	for k, v := range s {
		out[k] = v
	}
	return out
}

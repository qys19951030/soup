package soup

import (
	"fmt"
	"net/url"
	"strings"
)

// ValueMode selects how a field reads data from a matched element.
type ValueMode int

const (
	// ValueText reads the element text through Root.Text.
	ValueText ValueMode = iota
	// ValueHTML reads the serialized element HTML through Root.HTML.
	ValueHTML
	// ValueAttr reads the value of Field.Attribute from the element.
	ValueAttr
)

// Field describes one piece of data to extract from a parsed page.
type Field struct {
	// Name is the key under which the value is returned.
	Name string
	// Tag is the element tag name to match; an empty string matches any tag.
	Tag string
	// Attrs are attribute key/value conditions, all of which must match.
	Attrs map[string]string
	// Mode selects text, HTML or attribute extraction.
	Mode ValueMode
	// Attribute is the attribute name used when Mode is ValueAttr.
	Attribute string
	// Multiple returns every matching element as a string slice instead of
	// only the first match.
	Multiple bool
	// Required fails the extraction when no value is found.
	Required bool
	// AbsoluteURL resolves an attribute value against the page URL. It only
	// applies to ValueAttr fields.
	AbsoluteURL bool
}

// Extractor executes a fixed set of field descriptions against parsed pages.
type Extractor struct {
	fields []Field
}

// NewExtractor builds an Extractor from field descriptions.
func NewExtractor(fields []Field) *Extractor {
	return &Extractor{fields: fields}
}

// MissingFieldError reports a required field that produced no value.
type MissingFieldError struct {
	Field string
	Page  string
}

func (e *MissingFieldError) Error() string {
	return fmt.Sprintf("required field %q was not found on page %q", e.Field, e.Page)
}

// matches reports whether an element satisfies every attribute condition of
// the field description. Exact equality is required for every condition.
func (f Field) matches(node Root) bool {
	attributes := node.Attrs()
	for key, value := range f.Attrs {
		actual, ok := attributes[key]
		if !ok || actual != value {
			return false
		}
	}
	return true
}

// Extract runs every field description against root, resolving relative URLs
// against pageURL. Single-value fields return a string; multi-value fields
// return a []string. A missing required field produces *MissingFieldError.
func (e *Extractor) Extract(root Root, pageURL string) (map[string]interface{}, error) {
	result := make(map[string]interface{}, len(e.fields))
	for _, field := range e.fields {
		values, err := e.extractField(root, pageURL, field)
		if err != nil {
			return nil, err
		}
		if len(values) == 0 {
			if field.Required {
				return nil, &MissingFieldError{Field: field.Name, Page: pageURL}
			}
			continue
		}
		if field.Multiple {
			result[field.Name] = values
		} else {
			result[field.Name] = values[0]
		}
	}
	return result, nil
}

// extractField collects every valid value for one field description.
func (e *Extractor) extractField(root Root, pageURL string, field Field) ([]string, error) {
	nodes := root.FindAll(field.Tag)
	var raw []string
	for _, node := range nodes {
		if !field.matches(node) {
			continue
		}
		value, ok, err := readFieldValue(node, pageURL, field)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		raw = append(raw, value)
	}
	return cleanValues(raw), nil
}

// readFieldValue reads one element according to the field mode, reporting
// whether a usable value was available.
func readFieldValue(node Root, pageURL string, field Field) (string, bool, error) {
	switch field.Mode {
	case ValueText:
		return node.Text(), true, nil
	case ValueHTML:
		return node.HTML(), true, nil
	case ValueAttr:
		value, ok := nodeAttr(node.Pointer, field.Attribute)
		if !ok {
			return "", false, nil
		}
		if field.AbsoluteURL {
			resolved, err := resolveURL(value, pageURL)
			if err != nil {
				return "", false, err
			}
			return resolved, true, nil
		}
		return value, true, nil
	default:
		return "", false, fmt.Errorf("unknown value mode %d for field %q", field.Mode, field.Name)
	}
}

// resolveURL resolves raw against pageURL and returns an absolute URL string.
func resolveURL(raw, pageURL string) (string, error) {
	base, err := url.Parse(pageURL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

// cleanValues trims whitespace, drops empty strings and removes duplicates
// while preserving document order.
func cleanValues(values []string) []string {
	seen := make(map[string]bool, len(values))
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		cleaned = append(cleaned, trimmed)
	}
	return cleaned
}

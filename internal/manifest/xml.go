// Package manifest ports package-xml.ts, manifest-xml.ts, windows-path.ts and
// game-locale.ts: bounded reading of Windows game package manifests.
package manifest

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Node is a parsed XML element. Attribute order is kept for case-insensitive lookup.
type Node struct {
	Name       string
	Attributes []Attribute
	Children   []*Node
	Text       string
}

type Attribute struct{ Name, Value string }

var (
	entityPattern = regexp.MustCompile(`&[^;\s<]*;|&`)
	numericEntity = regexp.MustCompile(`(?i)^&#(?:x[0-9a-f]+|[0-9]+);$`)
	token         = regexp.MustCompile(`^(?:<!--[\s\S]*?-->|<\?xml\s[\s\S]*?\?>|<!\[CDATA\[[\s\S]*?\]\]>|</?[A-Za-z_](?:[^<>"']|"[^"<]*"|'[^'<]*')*>|[^<]+)`)
	closingTag    = regexp.MustCompile(`^</([A-Za-z_][\w.:-]*)\s*>$`)
	openingTag    = regexp.MustCompile(`^<([A-Za-z_][\w.:-]*)([\s\S]*?)(/?)>$`)
	attribute     = regexp.MustCompile(`^\s+([A-Za-z_][\w.:-]*)\s*=\s*(?:"([^"<]*)"|'([^'<]*)')`)
)

var namedEntities = map[string]string{"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&apos;": "'"}

func decode(text string) (string, error) {
	var failure error
	out := entityPattern.ReplaceAllStringFunc(text, func(entity string) string {
		if value, ok := namedEntities[entity]; ok {
			return value
		}
		if !numericEntity.MatchString(entity) {
			failure = errors.New("Unsupported XML entity.")
			return ""
		}
		var code int64
		var err error
		if entity[2] == 'x' || entity[2] == 'X' {
			code, err = strconv.ParseInt(entity[3:len(entity)-1], 16, 64)
		} else {
			code, err = strconv.ParseInt(entity[2:len(entity)-1], 10, 64)
		}
		if err != nil || code <= 0 || code > 0x10ffff || (code >= 0xd800 && code <= 0xdfff) {
			failure = errors.New("Invalid XML character.")
			return ""
		}
		return string(rune(code))
	})
	return out, failure
}

// Parse is a bounded manifest XML reader. No DTDs, external entities or
// processing instructions. It reads package identity/relationships and never
// proves account ownership.
func Parse(xml string) (*Node, error) {
	if xml == "" || utf8.RuneCountInString(xml) > 1_048_576 || strings.ContainsRune(xml, 0) {
		return nil, errors.New("Unsupported package manifest.")
	}
	// MicrosoftGame.config is often saved with a UTF-8 BOM; JS trim() dropped
	// it, strings.TrimSpace does not.
	xml = strings.TrimPrefix(xml, "\ufeff")
	container := &Node{}
	stack := []*Node{container}
	nodes := 0
	for offset := 0; offset < len(xml); {
		part := token.FindString(xml[offset:])
		if part == "" {
			return nil, errors.New("Malformed package XML.")
		}
		offset += len(part)
		parent := stack[len(stack)-1]
		switch {
		case strings.HasPrefix(part, "<!--"):
			if strings.Contains(part[4:len(part)-3], "--") {
				return nil, errors.New("Invalid XML comment.")
			}
		case strings.HasPrefix(part, "<?"):
			if len(container.Children) > 0 || len(stack) != 1 {
				return nil, errors.New("Misplaced XML declaration.")
			}
		case strings.HasPrefix(part, "<![CDATA["):
			parent.Text += part[9 : len(part)-3]
		case !strings.HasPrefix(part, "<"):
			text, err := decode(part)
			if err != nil {
				return nil, err
			}
			parent.Text += text
		case strings.HasPrefix(part, "</"):
			closing := closingTag.FindStringSubmatch(part)
			if len(stack) < 2 || closing == nil || closing[1] != parent.Name {
				return nil, errors.New("Mismatched package XML tag.")
			}
			stack = stack[:len(stack)-1]
		default:
			opening := openingTag.FindStringSubmatch(part)
			nodes++
			if opening == nil || nodes > 4096 || len(stack) > 32 {
				return nil, errors.New("Package XML limit exceeded.")
			}
			node := &Node{Name: opening[1]}
			seen := map[string]bool{}
			for remaining := opening[2]; strings.TrimSpace(remaining) != ""; {
				match := attribute.FindStringSubmatchIndex(remaining)
				if match == nil {
					return nil, errors.New("Invalid package XML attribute.")
				}
				name := remaining[match[2]:match[3]]
				if seen[name] {
					return nil, errors.New("Invalid package XML attribute.")
				}
				seen[name] = true
				raw := ""
				if match[4] >= 0 {
					raw = remaining[match[4]:match[5]]
				} else {
					raw = remaining[match[6]:match[7]]
				}
				value, err := decode(raw)
				if err != nil {
					return nil, err
				}
				node.Attributes = append(node.Attributes, Attribute{name, value})
				remaining = remaining[match[1]:]
			}
			parent.Children = append(parent.Children, node)
			if opening[3] == "" {
				stack = append(stack, node)
			}
		}
	}
	if len(stack) != 1 || len(container.Children) != 1 || strings.TrimSpace(container.Text) != "" {
		return nil, errors.New("Invalid package XML root.")
	}
	return container.Children[0], nil
}

func localName(name string) string {
	if index := strings.LastIndexByte(name, ':'); index >= 0 {
		return name[index+1:]
	}
	return name
}

// ChildrenNamed returns direct children with the given local name.
func ChildrenNamed(node *Node, name string) []*Node {
	var out []*Node
	for _, child := range node.Children {
		if localName(child.Name) == name {
			out = append(out, child)
		}
	}
	return out
}

func Single(node *Node, name string) (*Node, error) {
	found := ChildrenNamed(node, name)
	if len(found) != 1 {
		return nil, fmt.Errorf("Package manifest requires one %s.", name)
	}
	return found[0], nil
}

// Elements matches Windows manifest names with optional namespace prefixes,
// case-insensitively, in document order. Only parsed elements are visited,
// never markup inside comments or CDATA text.
func Elements(node *Node, name string) []*Node {
	var out []*Node
	var walk func(*Node)
	walk = func(n *Node) {
		if strings.EqualFold(localName(n.Name), name) {
			out = append(out, n)
		}
		for _, child := range n.Children {
			walk(child)
		}
	}
	if node != nil {
		walk(node)
	}
	return out
}

// AttributeValue looks up an attribute case-insensitively.
func AttributeValue(node *Node, name string) (string, bool) {
	if node == nil {
		return "", false
	}
	for _, attribute := range node.Attributes {
		if strings.EqualFold(attribute.Name, name) {
			return attribute.Value, true
		}
	}
	return "", false
}

// Attr returns the attribute of the first matching element, or "".
func Attr(root *Node, element, attribute string) string {
	found := Elements(root, element)
	if len(found) == 0 {
		return ""
	}
	value, _ := AttributeValue(found[0], attribute)
	return value
}

// Text returns the trimmed text of the first matching element, or "".
func Text(root *Node, element string) string {
	found := Elements(root, element)
	if len(found) == 0 {
		return ""
	}
	return strings.TrimSpace(found[0].Text)
}

// Attr returns an attribute by exact name, like node.attributes[name] in TypeScript.
func (n *Node) Attr(name string) string {
	for _, attribute := range n.Attributes {
		if attribute.Name == name {
			return attribute.Value
		}
	}
	return ""
}

// LocalName returns the element name without its namespace prefix.
func (n *Node) LocalName() string { return localName(n.Name) }

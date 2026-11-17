// GraphQL v4 (POST /api/graphql): a small parser and executor for the documents
// gh and github-mcp-server actually send — viewer, repository fields, __type
// probes, issue/pull-request reads, the five mutations, node(id:) and batching.
// Unknown fields answer HTTP 200 with an `errors` array naming the field.

package main

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ------------------------------------------------------------------ parser

type gqlValue struct {
	Kind   string // string, int, float, boolean, null, enum, variable, list, object
	Str    string
	Num    float64
	Bool   bool
	Name   string // variable name
	Items  []gqlValue
	Fields []gqlObjField
}

type gqlObjField struct {
	Name  string
	Value gqlValue
}

type gqlArg struct {
	Name  string
	Value gqlValue
}

type gqlField struct {
	Alias        string
	Name         string
	Args         []gqlArg
	Selection    []gqlField
	HasSelection bool
	FragmentType string
}

type gqlOperation struct {
	Kind      string // query | mutation
	Name      string
	Variables map[string]gqlValue
	Selection []gqlField
}

type gqlParseError struct{ msg string }

func (e *gqlParseError) Error() string { return e.msg }

// lexer scans one GraphQL document.
type lexer struct {
	text string
	pos  int
}

func (l *lexer) skipIgnored() {
	for {
		for l.pos < len(l.text) && (l.text[l.pos] == ' ' || l.text[l.pos] == '\t' || l.text[l.pos] == '\n' || l.text[l.pos] == '\r' || l.text[l.pos] == ',') {
			l.pos++
		}
		if l.pos < len(l.text) && l.text[l.pos] == '#' {
			for l.pos < len(l.text) && l.text[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		break
	}
}

func (l *lexer) peek() byte {
	l.skipIgnored()
	if l.pos >= len(l.text) {
		return 0
	}
	return l.text[l.pos]
}

func (l *lexer) consume(expected string) error {
	l.skipIgnored()
	if !strings.HasPrefix(l.text[l.pos:], expected) {
		return &gqlParseError{fmt.Sprintf("expected %s at %d", expected, l.pos)}
	}
	l.pos += len(expected)
	return nil
}

func (l *lexer) tryConsume(expected string) bool {
	l.skipIgnored()
	if !strings.HasPrefix(l.text[l.pos:], expected) {
		return false
	}
	l.pos += len(expected)
	return true
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

func (l *lexer) readName() (string, error) {
	l.skipIgnored()
	start := l.pos
	if l.pos >= len(l.text) || !isNameStart(l.text[l.pos]) {
		return "", &gqlParseError{fmt.Sprintf("expected a name at %d", l.pos)}
	}
	for l.pos < len(l.text) && isNameChar(l.text[l.pos]) {
		l.pos++
	}
	return l.text[start:l.pos], nil
}

func (l *lexer) readString() (string, error) {
	l.skipIgnored()
	if l.pos >= len(l.text) || l.text[l.pos] != '"' {
		return "", &gqlParseError{fmt.Sprintf("expected a string at %d", l.pos)}
	}
	l.pos++
	var out strings.Builder
	for {
		if l.pos >= len(l.text) {
			return "", &gqlParseError{"unterminated string"}
		}
		ch := l.text[l.pos]
		if ch == '"' {
			l.pos++
			return out.String(), nil
		}
		if ch == '\\' {
			next := byte(0)
			if l.pos+1 < len(l.text) {
				next = l.text[l.pos+1]
			}
			if next == 'u' {
				if l.pos+6 > len(l.text) {
					return "", &gqlParseError{"unterminated unicode escape"}
				}
				code, err := strconv.ParseUint(l.text[l.pos+2:l.pos+6], 16, 32)
				if err != nil {
					return "", &gqlParseError{"bad unicode escape"}
				}
				l.pos += 6
				// Surrogate pair: a high surrogate followed by \uXXXX low.
				if code >= 0xD800 && code <= 0xDBFF && l.pos+6 <= len(l.text) &&
					l.text[l.pos] == '\\' && l.text[l.pos+1] == 'u' {
					low, err := strconv.ParseUint(l.text[l.pos+2:l.pos+6], 16, 32)
					if err == nil && low >= 0xDC00 && low <= 0xDFFF {
						l.pos += 6
						r := 0x10000 + ((code - 0xD800) << 10) + (low - 0xDC00)
						out.WriteRune(rune(r))
						continue
					}
				}
				out.WriteRune(rune(code))
				continue
			}
			var esc rune
			switch next {
			case '"':
				esc = '"'
			case '\\':
				esc = '\\'
			case '/':
				esc = '/'
			case 'b':
				esc = '\b'
			case 'f':
				esc = '\f'
			case 'n':
				esc = '\n'
			case 'r':
				esc = '\r'
			case 't':
				esc = '\t'
			default:
				esc = rune(next)
			}
			out.WriteRune(esc)
			l.pos += 2
			continue
		}
		out.WriteByte(ch)
		l.pos++
	}
}

func (l *lexer) readNumber() (gqlValue, error) {
	l.skipIgnored()
	start := l.pos
	if l.pos < len(l.text) && l.text[l.pos] == '-' {
		l.pos++
	}
	for l.pos < len(l.text) && l.text[l.pos] >= '0' && l.text[l.pos] <= '9' {
		l.pos++
	}
	isFloat := false
	if l.pos < len(l.text) && l.text[l.pos] == '.' {
		isFloat = true
		l.pos++
		for l.pos < len(l.text) && l.text[l.pos] >= '0' && l.text[l.pos] <= '9' {
			l.pos++
		}
	}
	if l.pos < len(l.text) && (l.text[l.pos] == 'e' || l.text[l.pos] == 'E') {
		isFloat = true
		l.pos++
		if l.pos < len(l.text) && (l.text[l.pos] == '+' || l.text[l.pos] == '-') {
			l.pos++
		}
		for l.pos < len(l.text) && l.text[l.pos] >= '0' && l.text[l.pos] <= '9' {
			l.pos++
		}
	}
	raw := l.text[start:l.pos]
	if raw == "" || raw == "-" {
		return gqlValue{}, &gqlParseError{fmt.Sprintf("expected a number at %d", start)}
	}
	num, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return gqlValue{}, &gqlParseError{fmt.Sprintf("expected a number at %d", start)}
	}
	if isFloat {
		return gqlValue{Kind: "float", Num: num}, nil
	}
	return gqlValue{Kind: "int", Num: num}, nil
}

func parseValue(l *lexer) (gqlValue, error) {
	ch := l.peek()
	switch {
	case ch == '$':
		_ = l.consume("$")
		name, err := l.readName()
		if err != nil {
			return gqlValue{}, err
		}
		return gqlValue{Kind: "variable", Name: name}, nil
	case ch == '"':
		s, err := l.readString()
		if err != nil {
			return gqlValue{}, err
		}
		return gqlValue{Kind: "string", Str: s}, nil
	case ch == '[':
		_ = l.consume("[")
		items := []gqlValue{}
		for l.peek() != ']' {
			item, err := parseValue(l)
			if err != nil {
				return gqlValue{}, err
			}
			items = append(items, item)
		}
		_ = l.consume("]")
		return gqlValue{Kind: "list", Items: items}, nil
	case ch == '{':
		_ = l.consume("{")
		fields := []gqlObjField{}
		for l.peek() != '}' {
			name, err := l.readName()
			if err != nil {
				return gqlValue{}, err
			}
			if err := l.consume(":"); err != nil {
				return gqlValue{}, err
			}
			value, err := parseValue(l)
			if err != nil {
				return gqlValue{}, err
			}
			fields = append(fields, gqlObjField{Name: name, Value: value})
		}
		_ = l.consume("}")
		return gqlValue{Kind: "object", Fields: fields}, nil
	case ch == '-' || (ch >= '0' && ch <= '9'):
		return l.readNumber()
	default:
		name, err := l.readName()
		if err != nil {
			return gqlValue{}, err
		}
		switch name {
		case "true":
			return gqlValue{Kind: "boolean", Bool: true}, nil
		case "false":
			return gqlValue{Kind: "boolean", Bool: false}, nil
		case "null":
			return gqlValue{Kind: "null"}, nil
		default:
			return gqlValue{Kind: "enum", Str: name}, nil
		}
	}
}

func parseSelectionSet(l *lexer) ([]gqlField, error) {
	if err := l.consume("{"); err != nil {
		return nil, err
	}
	fields := []gqlField{}
	for l.peek() != '}' {
		if strings.HasPrefix(l.text[l.pos:], "...") {
			_ = l.consume("...")
			fragmentType := ""
			if l.peek() != '{' {
				keyword, err := l.readName()
				if err != nil {
					return nil, err
				}
				if keyword == "on" {
					ft, err := l.readName()
					if err != nil {
						return nil, err
					}
					fragmentType = ft
				} else {
					return nil, &gqlParseError{fmt.Sprintf("unsupported fragment spread %s", keyword)}
				}
			}
			inner, err := parseSelectionSet(l)
			if err != nil {
				return nil, err
			}
			for _, field := range inner {
				field.FragmentType = fragmentType
				fields = append(fields, field)
			}
			continue
		}
		first, err := l.readName()
		if err != nil {
			return nil, err
		}
		alias := ""
		name := first
		if l.tryConsume(":") {
			alias = first
			name, err = l.readName()
			if err != nil {
				return nil, err
			}
		}
		args := []gqlArg{}
		if l.peek() == '(' {
			_ = l.consume("(")
			for l.peek() != ')' {
				argName, err := l.readName()
				if err != nil {
					return nil, err
				}
				if err := l.consume(":"); err != nil {
					return nil, err
				}
				value, err := parseValue(l)
				if err != nil {
					return nil, err
				}
				args = append(args, gqlArg{Name: argName, Value: value})
			}
			_ = l.consume(")")
		}
		var selection []gqlField
		hasSelection := false
		if l.peek() == '{' {
			selection, err = parseSelectionSet(l)
			if err != nil {
				return nil, err
			}
			hasSelection = true
		}
		fields = append(fields, gqlField{Alias: alias, Name: name, Args: args, Selection: selection, HasSelection: hasSelection})
	}
	_ = l.consume("}")
	return fields, nil
}

func parseOperation(text string) (gqlOperation, error) {
	l := &lexer{text: text}
	kind := "query"
	if l.peek() == '{' {
		selection, err := parseSelectionSet(l)
		if err != nil {
			return gqlOperation{}, err
		}
		return gqlOperation{Kind: "query", Variables: map[string]gqlValue{}, Selection: selection}, nil
	}
	keyword, err := l.readName()
	if err != nil {
		return gqlOperation{}, err
	}
	if keyword != "query" && keyword != "mutation" {
		return gqlOperation{}, &gqlParseError{fmt.Sprintf("unsupported operation %s", keyword)}
	}
	kind = keyword
	var name string
	if c := l.peek(); c != 0 && isNameStart(c) {
		name, err = l.readName()
		if err != nil {
			return gqlOperation{}, err
		}
	}
	variables := map[string]gqlValue{}
	if l.peek() == '(' {
		_ = l.consume("(")
		for l.peek() != ')' {
			if err := l.consume("$"); err != nil {
				return gqlOperation{}, err
			}
			varName, err := l.readName()
			if err != nil {
				return gqlOperation{}, err
			}
			if err := l.consume(":"); err != nil {
				return gqlOperation{}, err
			}
			// Skip the type (a name, list brackets, or !) and an optional default value.
			depth := 0
		typeLoop:
			for {
				ch := l.peek()
				switch {
				case ch == '[':
					_ = l.consume("[")
					depth++
				case ch == ']':
					_ = l.consume("]")
					depth--
				case ch == '!':
					_ = l.consume("!")
				case ch != 0 && isNameStart(ch):
					if _, err := l.readName(); err != nil {
						return gqlOperation{}, err
					}
				default:
					break typeLoop
				}
				if depth == 0 {
					next := l.peek()
					if next != '[' && next != '!' && !(next != 0 && isNameStart(next)) {
						break typeLoop
					}
				}
			}
			if l.tryConsume("=") {
				value, err := parseValue(l)
				if err != nil {
					return gqlOperation{}, err
				}
				variables[varName] = value
			} else {
				variables[varName] = gqlValue{Kind: "null"}
			}
		}
		_ = l.consume(")")
	}
	selection, err := parseSelectionSet(l)
	if err != nil {
		return gqlOperation{}, err
	}
	return gqlOperation{Kind: kind, Name: name, Variables: variables, Selection: selection}, nil
}

// ------------------------------------------------------------------ values

func evalValue(value gqlValue, variables map[string]interface{}) interface{} {
	switch value.Kind {
	case "string", "enum":
		return value.Str
	case "int", "float":
		return value.Num
	case "boolean":
		return value.Bool
	case "null":
		return nil
	case "variable":
		if v, ok := variables[value.Name]; ok {
			return v
		}
		return nil
	case "list":
		out := make([]interface{}, 0, len(value.Items))
		for _, item := range value.Items {
			out = append(out, evalValue(item, variables))
		}
		return out
	case "object":
		out := map[string]interface{}{}
		for _, field := range value.Fields {
			out[field.Name] = evalValue(field.Value, variables)
		}
		return out
	}
	return nil
}

func argValue(field gqlField, name string, variables map[string]interface{}) interface{} {
	for _, arg := range field.Args {
		if arg.Name == name {
			return evalValue(arg.Value, variables)
		}
	}
	return nil
}

// ------------------------------------------------------------------ schema

var typeFields = map[string][]string{
	"Issue":                         {"id", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "labels", "comments", "url", "repository"},
	"PullRequest":                   {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRef", "baseRef", "files", "author", "createdAt", "updatedAt"},
	"Repository":                    {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "url", "createdAt"},
	"SearchType":                    {},
	"User":                          {"id", "login", "name", "email"},
	"IssueConnection":               {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection":         {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFile":        {"path", "additions", "deletions", "changes"},
	"PageInfo":                      {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"Ref":                           {"name", "target"},
	"CommentEdge":                   {"node", "cursor"},
	"IssueComment":                  {"id", "body", "author", "createdAt"},
	"Query":                         {"viewer", "repository", "node", "__type", "__schema"},
	"Mutation":                      {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
}

var typeKinds = map[string]string{
	"SearchType":           "ENUM",
	"IssueState":           "ENUM",
	"PullRequestState":     "ENUM",
	"MergeableState":       "ENUM",
	"OrderDirection":       "ENUM",
	"IssueOrderField":      "ENUM",
	"RepositoryPermission": "ENUM",
}

var enumValues = map[string][]string{
	"SearchType":           {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE", "REGISTRY_PACKAGE", "WIKI"},
	"IssueState":           {"OPEN", "CLOSED"},
	"PullRequestState":     {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":       {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"OrderDirection":       {"ASC", "DESC"},
	"IssueOrderField":      {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"RepositoryPermission": {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
}

// ------------------------------------------------------------------ executor

type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

// gqlObject is one node in the execution tree: a typename, its own
// properties, and the store entity behind it.
type gqlObject struct {
	Typename string
	Props    map[string]interface{}
	Repo     *Repo
	Issue    *Issue
	Pull     *Pull
}

func newGQLObject(typename string) *gqlObject {
	return &gqlObject{Typename: typename, Props: map[string]interface{}{}}
}

func b64cursor(index int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + itoa(index)))
}

func parseCursor(cursor interface{}) int {
	s, ok := cursor.(string)
	if !ok || s == "" {
		return -1
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return -1
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return -1
	}
	n, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil {
		return -1
	}
	return n
}

// GraphQLExecutor executes documents against one forge.
type GraphQLExecutor struct {
	Forge *Forge
}

func (e *GraphQLExecutor) execute(document string, variables map[string]interface{}) map[string]interface{} {
	operation, err := parseOperation(document)
	if err != nil {
		return map[string]interface{}{"data": nil, "errors": []interface{}{map[string]interface{}{"message": err.Error()}}}
	}
	root := newGQLObject("Query")
	if operation.Kind == "mutation" {
		root.Typename = "Mutation"
	}
	data, err := e.resolveSelection(root, operation.Selection, variables)
	if err != nil {
		if ge, ok := err.(*gqlError); ok {
			return map[string]interface{}{"data": nil, "errors": []interface{}{map[string]interface{}{"message": ge.msg}}}
		}
		panic(err)
	}
	return map[string]interface{}{"data": data, "errors": []interface{}{}}
}

func (e *GraphQLExecutor) resolveSelection(parent *gqlObject, selection []gqlField, variables map[string]interface{}) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	for _, field := range selection {
		if field.FragmentType != "" && field.FragmentType != parent.Typename {
			continue
		}
		key := field.Name
		if field.Alias != "" {
			key = field.Alias
		}
		value, err := e.resolveField(parent, field, variables)
		if err != nil {
			return nil, err
		}
		if field.HasSelection && value != nil {
			if arr, ok := value.([]*gqlObject); ok {
				mapped := make([]interface{}, 0, len(arr))
				for _, item := range arr {
					if item == nil {
						mapped = append(mapped, nil)
						continue
					}
					resolved, err := e.resolveSelection(item, field.Selection, variables)
					if err != nil {
						return nil, err
					}
					mapped = append(mapped, resolved)
				}
				out[key] = mapped
			} else if obj, ok := value.(*gqlObject); ok {
				resolved, err := e.resolveSelection(obj, field.Selection, variables)
				if err != nil {
					return nil, err
				}
				out[key] = resolved
			} else {
				out[key] = value
			}
		} else {
			out[key] = value
		}
	}
	return out, nil
}

func (e *GraphQLExecutor) unknownField(parent *gqlObject, field gqlField) (interface{}, error) {
	return nil, &gqlError{fmt.Sprintf("Cannot query field %q on type %q.", field.Name, parent.Typename)}
}

func (e *GraphQLExecutor) resolveField(parent *gqlObject, field gqlField, variables map[string]interface{}) (interface{}, error) {
	// Payloads, connections and page info carry their fields as own properties.
	if !strings.HasPrefix(field.Name, "__") {
		if v, ok := parent.Props[field.Name]; ok {
			return v, nil
		}
	}
	switch parent.Typename {
	case "Query":
		return e.resolveQuery(parent, field, variables)
	case "Mutation":
		return e.resolveMutation(parent, field, variables)
	case "Repository":
		return e.resolveRepository(parent, field, variables)
	case "Issue":
		return e.resolveIssue(parent, field, variables)
	case "PullRequest":
		return e.resolvePullRequest(parent, field, variables)
	case "User":
		switch field.Name {
		case "login":
			return parent.Props["login"], nil
		case "id":
			return parent.Props["id"], nil
		case "name":
			return parent.Props["name"], nil
		case "email":
			return parent.Props["email"], nil
		}
		return e.unknownField(parent, field)
	case "__Type":
		switch field.Name {
		case "name":
			return parent.Props["name"], nil
		case "kind":
			if v, ok := parent.Props["kind"]; ok {
				return v, nil
			}
			return "OBJECT", nil
		case "fields":
			if v, ok := parent.Props["fields"]; ok {
				return v, nil
			}
			return nil, nil
		case "enumValues":
			if v, ok := parent.Props["enumValues"]; ok {
				return v, nil
			}
			return nil, nil
		}
		return e.unknownField(parent, field)
	case "__Field", "__EnumValue":
		if field.Name == "name" {
			return parent.Props["name"], nil
		}
		return e.unknownField(parent, field)
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection":
		switch field.Name {
		case "nodes":
			if v, ok := parent.Props["nodes"]; ok {
				if nodes, ok := v.([]*gqlObject); ok {
					return nodes, nil
				}
			}
			return []*gqlObject{}, nil
		case "edges":
			var nodes []*gqlObject
			if v, ok := parent.Props["nodes"]; ok {
				nodes, _ = v.([]*gqlObject)
			}
			edges := make([]*gqlObject, 0, len(nodes))
			for i, node := range nodes {
				edge := newGQLObject("Edge")
				edge.Props["node"] = node
				edge.Props["cursor"] = b64cursor(i)
				edges = append(edges, edge)
			}
			return edges, nil
		case "pageInfo":
			return parent.Props["pageInfo"], nil
		case "totalCount":
			if v, ok := parent.Props["nodes"]; ok {
				if nodes, ok := v.([]*gqlObject); ok {
					return len(nodes), nil
				}
			}
			return 0, nil
		}
		return e.unknownField(parent, field)
	case "PageInfo":
		switch field.Name {
		case "hasNextPage":
			if v, ok := parent.Props["hasNextPage"]; ok {
				return v, nil
			}
			return false, nil
		case "hasPreviousPage":
			if v, ok := parent.Props["hasPreviousPage"]; ok {
				return v, nil
			}
			return false, nil
		case "endCursor":
			return parent.Props["endCursor"], nil
		case "startCursor":
			return parent.Props["startCursor"], nil
		}
		return e.unknownField(parent, field)
	case "Ref":
		if field.Name == "name" {
			return parent.Props["name"], nil
		}
		return e.unknownField(parent, field)
	case "PullRequestChangedFile":
		switch field.Name {
		case "path":
			return parent.Props["path"], nil
		case "additions":
			if v, ok := parent.Props["additions"]; ok {
				return v, nil
			}
			return 0, nil
		case "deletions":
			if v, ok := parent.Props["deletions"]; ok {
				return v, nil
			}
			return 0, nil
		case "changes":
			if v, ok := parent.Props["changes"]; ok {
				return v, nil
			}
			return 0, nil
		}
		return e.unknownField(parent, field)
	case "CommentEdge":
		switch field.Name {
		case "node":
			return parent.Props["node"], nil
		case "cursor":
			return parent.Props["cursor"], nil
		}
		return e.unknownField(parent, field)
	case "IssueComment":
		switch field.Name {
		case "id":
			return parent.Props["id"], nil
		case "body":
			return parent.Props["body"], nil
		}
		return e.unknownField(parent, field)
	case "Edge":
		switch field.Name {
		case "node":
			return parent.Props["node"], nil
		case "cursor":
			return parent.Props["cursor"], nil
		}
		return e.unknownField(parent, field)
	}
	return e.unknownField(parent, field)
}

func (e *GraphQLExecutor) resolveQuery(parent *gqlObject, field gqlField, variables map[string]interface{}) (interface{}, error) {
	switch field.Name {
	case "viewer":
		obj := newGQLObject("User")
		obj.Props["login"] = VIEWER.Login
		obj.Props["id"] = gid("User", VIEWER.Login)
		obj.Props["name"] = VIEWER.Name
		obj.Props["email"] = VIEWER.Email
		return obj, nil
	case "repository":
		owner := argValue(field, "owner", variables)
		name := argValue(field, "name", variables)
		ownerStr, ok1 := owner.(string)
		nameStr, ok2 := name.(string)
		if !ok1 || !ok2 {
			return nil, nil
		}
		repo := e.Forge.getRepo(ownerStr, nameStr)
		if repo == nil || ownerStr != VIEWER.Login {
			return nil, nil
		}
		return repoObject(repo), nil
	case "node":
		id := argValue(field, "id", variables)
		idStr, ok := id.(string)
		if !ok {
			return nil, nil
		}
		kind, parts, ok := parseGid(idStr)
		if !ok {
			return nil, nil
		}
		switch kind {
		case "Repository":
			n, err := strconv.Atoi(parts[0])
			if err != nil {
				return nil, nil
			}
			repo := e.Forge.getRepoByID(n)
			if repo == nil {
				return nil, nil
			}
			return repoObject(repo), nil
		case "Issue":
			n, err := strconv.Atoi(parts[0])
			if err != nil {
				return nil, nil
			}
			repo, issue := e.Forge.getIssueByID(n)
			if issue == nil {
				return nil, nil
			}
			return issueObject(repo, issue), nil
		case "PullRequest":
			n, err := strconv.Atoi(parts[0])
			if err != nil {
				return nil, nil
			}
			repo := e.Forge.getRepoByID(n)
			if repo == nil {
				return nil, nil
			}
			num, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, nil
			}
			pull := e.Forge.getPull(repo, num)
			if pull == nil {
				return nil, nil
			}
			return pullObject(repo, pull), nil
		}
		return nil, nil
	case "__type":
		name := argValue(field, "name", variables)
		nameStr, ok := name.(string)
		if !ok {
			return nil, nil
		}
		fields, hasFields := typeFields[nameStr]
		enums, hasEnums := enumValues[nameStr]
		if !hasFields && !hasEnums {
			return nil, nil
		}
		obj := newGQLObject("__Type")
		obj.Props["name"] = nameStr
		kind := "OBJECT"
		if k, ok := typeKinds[nameStr]; ok {
			kind = k
		}
		obj.Props["kind"] = kind
		fieldObjs := []*gqlObject{}
		for _, f := range fields {
			fo := newGQLObject("__Field")
			fo.Props["name"] = f
			fieldObjs = append(fieldObjs, fo)
		}
		obj.Props["fields"] = fieldObjs
		enumObjs := []*gqlObject{}
		for _, ev := range enums {
			eo := newGQLObject("__EnumValue")
			eo.Props["name"] = ev
			enumObjs = append(enumObjs, eo)
		}
		obj.Props["enumValues"] = enumObjs
		return obj, nil
	case "__schema":
		obj := newGQLObject("__Schema")
		types := []*gqlObject{}
		// Deterministic order: the schema table's declaration order is not
		// kept in a Go map, so sort the type names.
		names := make([]string, 0, len(typeFields))
		for name := range typeFields {
			names = append(names, name)
		}
		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				if names[j] < names[i] {
					names[i], names[j] = names[j], names[i]
				}
			}
		}
		for _, name := range names {
			to := newGQLObject("__Type")
			to.Props["name"] = name
			types = append(types, to)
		}
		obj.Props["types"] = types
		return obj, nil
	}
	return e.unknownField(parent, field)
}

func (e *GraphQLExecutor) resolveMutation(parent *gqlObject, field gqlField, variables map[string]interface{}) (interface{}, error) {
	input := argValue(field, "input", variables)
	record, _ := input.(map[string]interface{})
	if record == nil {
		record = map[string]interface{}{}
	}
	switch field.Name {
	case "createIssue":
		kind, parts, ok := parseGid(asString(record["repositoryId"]))
		var repo *Repo
		if ok && kind == "Repository" {
			if n, err := strconv.Atoi(parts[0]); err == nil {
				repo = e.Forge.getRepoByID(n)
			}
		}
		if repo == nil {
			return nil, &gqlError{"repository not found"}
		}
		title := asString(record["title"])
		created, errMsg := e.Forge.createIssue(repo, title, asString(record["body"]), nil)
		if errMsg != "" {
			return nil, &gqlError{errMsg}
		}
		payload := newGQLObject("CreateIssuePayload")
		payload.Props["issue"] = issueObject(repo, created)
		return payload, nil
	case "closeIssue":
		kind, parts, ok := parseGid(asString(record["issueId"]))
		var repo *Repo
		var issue *Issue
		if ok && kind == "Issue" {
			if n, err := strconv.Atoi(parts[0]); err == nil {
				repo, issue = e.Forge.getIssueByID(n)
			}
		}
		if issue == nil {
			return nil, &gqlError{"issue not found"}
		}
		issue.State = "closed"
		_, iso := e.Forge.Clock.Next()
		issue.UpdatedAt = iso
		payload := newGQLObject("CloseIssuePayload")
		payload.Props["issue"] = issueObject(repo, issue)
		return payload, nil
	case "createPullRequest":
		kind, parts, ok := parseGid(asString(record["repositoryId"]))
		var repo *Repo
		if ok && kind == "Repository" {
			if n, err := strconv.Atoi(parts[0]); err == nil {
				repo = e.Forge.getRepoByID(n)
			}
		}
		if repo == nil {
			return nil, &gqlError{"repository not found"}
		}
		created, errMsg, _ := e.Forge.createPull(repo, asString(record["headRefName"]), asString(record["baseRefName"]), asString(record["title"]), asString(record["body"]))
		if errMsg != "" {
			return nil, &gqlError{errMsg}
		}
		payload := newGQLObject("CreatePullRequestPayload")
		payload.Props["pullRequest"] = pullObject(repo, created)
		return payload, nil
	case "mergePullRequest":
		kind, parts, ok := parseGid(asString(record["pullRequestId"]))
		var repo *Repo
		var pull *Pull
		if ok && kind == "PullRequest" {
			if n, err := strconv.Atoi(parts[0]); err == nil {
				repo = e.Forge.getRepoByID(n)
			}
			if repo != nil {
				if num, err := strconv.Atoi(parts[1]); err == nil {
					pull = e.Forge.getPull(repo, num)
				}
			}
		}
		if repo == nil || pull == nil {
			return nil, &gqlError{"pull request not found"}
		}
		if pull.Merged {
			return nil, &gqlError{"pull request already merged"}
		}
		if _, _, merged := e.Forge.mergePull(repo, pull); !merged {
			return nil, &gqlError{"pull request is conflicted"}
		}
		payload := newGQLObject("MergePullRequestPayload")
		payload.Props["pullRequest"] = pullObject(repo, pull)
		return payload, nil
	case "addComment":
		kind, parts, ok := parseGid(asString(record["subjectId"]))
		var repo *Repo
		var issue *Issue
		if ok && kind == "Issue" {
			if n, err := strconv.Atoi(parts[0]); err == nil {
				repo, issue = e.Forge.getIssueByID(n)
			}
		}
		if issue == nil {
			return nil, &gqlError{"issue not found"}
		}
		bodyText := asString(record["body"])
		_, iso := e.Forge.Clock.Next()
		comment := &Comment{ID: e.Forge.NextCommentID, Body: bodyText, CreatedAt: iso, UserLogin: VIEWER.Login}
		e.Forge.NextCommentID++
		issue.Comments = append(issue.Comments, comment)
		node := newGQLObject("IssueComment")
		node.Props["id"] = gid("IssueComment", repo.ID, issue.Number, comment.ID)
		node.Props["body"] = comment.Body
		edge := newGQLObject("CommentEdge")
		edge.Props["node"] = node
		edge.Props["cursor"] = b64cursor(len(issue.Comments) - 1)
		payload := newGQLObject("AddCommentPayload")
		payload.Props["commentEdge"] = edge
		return payload, nil
	}
	return e.unknownField(parent, field)
}

func (e *GraphQLExecutor) resolveRepository(parent *gqlObject, field gqlField, variables map[string]interface{}) (interface{}, error) {
	repo := parent.Repo
	switch field.Name {
	case "id":
		return parent.Props["id"], nil
	case "databaseId":
		return repo.ID, nil
	case "name":
		return repo.Name, nil
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name, nil
	case "owner":
		obj := newGQLObject("User")
		obj.Props["login"] = repo.Owner
		obj.Props["id"] = gid("User", repo.Owner)
		return obj, nil
	case "defaultBranchRef":
		tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if !ok {
			return nil, nil
		}
		_ = tip
		obj := newGQLObject("Ref")
		obj.Props["name"] = repo.DefaultBranch
		return obj, nil
	case "viewerPermission":
		return "ADMIN", nil
	case "hasIssuesEnabled":
		return true, nil
	case "description":
		return repo.Description, nil
	case "issue":
		number := argValue(field, "number", variables)
		n, ok := number.(float64)
		if !ok {
			return nil, nil
		}
		issue := e.Forge.getIssue(repo, int(n))
		if issue == nil {
			return nil, nil
		}
		return issueObject(repo, issue), nil
	case "issues":
		first := argValue(field, "first", variables)
		after := argValue(field, "after", variables)
		states := argValue(field, "states", variables)
		orderBy := argValue(field, "orderBy", variables)
		list := append([]*Issue(nil), repo.Issues...)
		if arr, ok := states.([]interface{}); ok && len(arr) > 0 {
			wanted := map[string]bool{}
			for _, s := range arr {
				wanted[strings.ToLower(asString(s))] = true
			}
			filtered := []*Issue{}
			for _, issue := range list {
				if wanted[issue.State] {
					filtered = append(filtered, issue)
				}
			}
			list = filtered
		}
		direction := 1
		if order, ok := orderBy.(map[string]interface{}); ok && order != nil {
			if order["direction"] == "DESC" {
				direction = -1
			}
		}
		// Sort by createdSeq, ties by number, in the requested direction.
		for i := 0; i < len(list); i++ {
			for j := i + 1; j < len(list); j++ {
				swap := false
				if list[i].CreatedSeq != list[j].CreatedSeq {
					swap = (list[i].CreatedSeq-list[j].CreatedSeq)*direction > 0
				} else {
					swap = (list[i].Number-list[j].Number)*direction > 0
				}
				if swap {
					list[i], list[j] = list[j], list[i]
				}
			}
		}
		nodes := make([]*gqlObject, 0, len(list))
		for _, issue := range list {
			nodes = append(nodes, issueObject(repo, issue))
		}
		return connection(nodes, first, after, "IssueConnection"), nil
	case "pullRequests":
		first := argValue(field, "first", variables)
		after := argValue(field, "after", variables)
		nodes := []*gqlObject{}
		for _, pull := range repo.Pulls {
			if pull.State == "open" {
				nodes = append(nodes, pullObject(repo, pull))
			}
		}
		return connection(nodes, first, after, "PullRequestConnection"), nil
	case "pullRequest":
		number := argValue(field, "number", variables)
		n, ok := number.(float64)
		if !ok {
			return nil, nil
		}
		pull := e.Forge.getPull(repo, int(n))
		if pull == nil {
			return nil, nil
		}
		return pullObject(repo, pull), nil
	}
	return e.unknownField(parent, field)
}

func (e *GraphQLExecutor) resolveIssue(parent *gqlObject, field gqlField, variables map[string]interface{}) (interface{}, error) {
	issue := parent.Issue
	switch field.Name {
	case "id":
		return parent.Props["id"], nil
	case "number":
		return issue.Number, nil
	case "title":
		return issue.Title, nil
	case "state":
		return strings.ToUpper(issue.State), nil
	case "body":
		return issue.Body, nil
	}
	return e.unknownField(parent, field)
}

func (e *GraphQLExecutor) resolvePullRequest(parent *gqlObject, field gqlField, variables map[string]interface{}) (interface{}, error) {
	pull := parent.Pull
	repo := parent.Repo
	switch field.Name {
	case "id":
		return parent.Props["id"], nil
	case "number":
		return pull.Number, nil
	case "title":
		return pull.Title, nil
	case "body":
		return pull.Body, nil
	case "state":
		if pull.Merged {
			return "MERGED", nil
		}
		return strings.ToUpper(pull.State), nil
	case "merged":
		return pull.Merged, nil
	case "headRefName":
		return pull.HeadRef, nil
	case "baseRefName":
		return pull.BaseRef, nil
	case "mergeable":
		if e.Forge.pullConflicts(repo, pull) {
			return "CONFLICTING", nil
		}
		return "MERGEABLE", nil
	case "files":
		first := argValue(field, "first", variables)
		files := e.Forge.pullFiles(repo, pull)
		nodes := make([]*gqlObject, 0, len(files))
		for _, f := range files {
			obj := newGQLObject("PullRequestChangedFile")
			obj.Props["path"] = f.Filename
			obj.Props["additions"] = f.Additions
			obj.Props["deletions"] = f.Deletions
			obj.Props["changes"] = f.Changes
			nodes = append(nodes, obj)
		}
		return connection(nodes, first, nil, "PullRequestChangedFileConnection"), nil
	}
	return e.unknownField(parent, field)
}

func repoObject(repo *Repo) *gqlObject {
	obj := newGQLObject("Repository")
	obj.Props["id"] = gid("Repository", repo.ID)
	obj.Repo = repo
	return obj
}

func issueObject(repo *Repo, issue *Issue) *gqlObject {
	obj := newGQLObject("Issue")
	obj.Props["id"] = gid("Issue", issue.ID)
	obj.Repo = repo
	obj.Issue = issue
	return obj
}

func pullObject(repo *Repo, pull *Pull) *gqlObject {
	obj := newGQLObject("PullRequest")
	obj.Props["id"] = gid("PullRequest", repo.ID, pull.Number)
	obj.Repo = repo
	obj.Pull = pull
	return obj
}

func connection(nodes []*gqlObject, first, after interface{}, typename string) *gqlObject {
	start := parseCursor(after) + 1
	limit := len(nodes)
	if f, ok := first.(float64); ok && !math.IsInf(f, 0) && !math.IsNaN(f) {
		limit = int(f)
		if limit < 0 {
			limit = 0
		}
	}
	end := start + limit
	if end > len(nodes) {
		end = len(nodes)
	}
	if start > len(nodes) {
		start = len(nodes)
		end = len(nodes)
	}
	page := nodes[start:end]
	hasNextPage := start+limit < len(nodes)
	var endCursor, startCursor interface{}
	if len(page) > 0 {
		endCursor = b64cursor(start + len(page) - 1)
		startCursor = b64cursor(start)
	}
	obj := newGQLObject(typename)
	obj.Props["nodes"] = page
	pageInfo := newGQLObject("PageInfo")
	pageInfo.Props["hasNextPage"] = hasNextPage
	pageInfo.Props["hasPreviousPage"] = start > 0
	pageInfo.Props["startCursor"] = startCursor
	pageInfo.Props["endCursor"] = endCursor
	obj.Props["pageInfo"] = pageInfo
	return obj
}

// ------------------------------------------------------------------ HTTP

// handleGraphQL answers POST /api/graphql: one operation {query, variables}
// or a batch (a JSON array of operations, answered in order). Auth is decided
// by the caller.
func handleGraphQL(req *Request, ctx *Ctx) Response {
	raw := strings.TrimSpace(string(req.Body))
	var body interface{}
	if raw == "" {
		body = map[string]interface{}{}
	} else {
		parsed, ok := parseJSONBody(req.Body)
		if !ok {
			return jsonResponse(400, map[string]interface{}{"errors": []interface{}{map[string]interface{}{"message": "Problems parsing JSON"}}}, nil)
		}
		body = parsed
	}
	executor := &GraphQLExecutor{Forge: ctx.Forge}
	runOne := func(operation interface{}) map[string]interface{} {
		record, ok := operation.(map[string]interface{})
		if !ok {
			return map[string]interface{}{"data": nil, "errors": []interface{}{map[string]interface{}{"message": "Invalid GraphQL operation"}}}
		}
		query := asString(record["query"])
		variables := map[string]interface{}{}
		if v, ok := record["variables"].(map[string]interface{}); ok {
			variables = v
		}
		return executor.execute(query, variables)
	}
	var answer interface{}
	if arr, ok := body.([]interface{}); ok {
		out := make([]map[string]interface{}, 0, len(arr))
		for _, op := range arr {
			out = append(out, runOne(op))
		}
		answer = out
	} else {
		answer = runOne(body)
	}
	return jsonResponse(200, answer, nil)
}

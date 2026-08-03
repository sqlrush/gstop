package sqlshape

import (
	"fmt"
	"strings"
)

// Identifier retains whether SQL name matching is case-sensitive. Unquoted
// identifiers are folded to lower case while quoted identifiers keep their
// exact decoded spelling.
type Identifier struct {
	Value  string
	Quoted bool
}

// RelationRef is an explicitly schema-qualified relation found in executable
// SQL text. It is evidence for diagnosis only and is never used to rewrite SQL.
type RelationRef struct {
	Schema Identifier
	Table  Identifier
	Alias  *Identifier
}

// RelationEvidence keeps qualified and unqualified relation occurrences
// separate so callers can reject only ambiguities relevant to one plan access.
type RelationEvidence struct {
	Qualified   []RelationRef
	Unqualified []RelationRef
}

type relationTokenKind uint8

const (
	relationIdentifier relationTokenKind = iota + 1
	relationPunctuation
)

type relationToken struct {
	kind        relationTokenKind
	identifier  Identifier
	punctuation byte
}

// LeadingKeyword returns the first executable, unquoted SQL word. Comments
// and literal bodies are ignored. Malformed lexical input fails closed.
func LeadingKeyword(sqlText string) (string, error) {
	tokens, err := lexRelations(sqlText)
	if err != nil {
		return "", err
	}
	for _, token := range tokens {
		if token.kind == relationIdentifier && !token.identifier.Quoted {
			return strings.ToUpper(token.identifier.Value), nil
		}
	}
	return "", nil
}

// SchemaQualifiedRelations extracts only explicit schema.table references from
// relation-bearing SQL clauses. It deliberately does not infer search_path.
func SchemaQualifiedRelations(sqlText string) ([]RelationRef, error) {
	evidence, err := RelationEvidenceFor(sqlText)
	if err != nil {
		return nil, err
	}
	if len(evidence.Unqualified) > 0 {
		return nil, fmt.Errorf("unqualified relation prevents safe schema binding")
	}
	return evidence.Qualified, nil
}

// RelationEvidenceFor extracts relation occurrences for access-specific,
// fail-closed schema binding. Multiple executable statements are rejected
// because one plan cannot safely be associated with their combined scope.
func RelationEvidenceFor(sqlText string) (RelationEvidence, error) {
	tokens, err := lexRelations(sqlText)
	if err != nil {
		return RelationEvidence{}, err
	}
	if hasMultipleExecutableStatements(tokens) {
		return RelationEvidence{}, fmt.Errorf("multiple SQL statements cannot provide one plan relation scope")
	}

	evidence := RelationEvidence{}
	depth := 0
	selectSeen := make(map[int]bool)
	inFromList := make(map[int]bool)
	insertSeen := make(map[int]bool)
	deleteSeen := make(map[int]bool)
	mergeSeen := make(map[int]bool)
	dmlSeen := make(map[int]bool)

	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if token.kind == relationPunctuation {
			switch token.punctuation {
			case '(':
				depth++
			case ')':
				if depth > 0 {
					delete(selectSeen, depth)
					delete(inFromList, depth)
					delete(insertSeen, depth)
					delete(deleteSeen, depth)
					delete(mergeSeen, depth)
					delete(dmlSeen, depth)
					depth--
				}
			case ',':
				if inFromList[depth] {
					next, relationErr := collectRelationEvidence(tokens, index+1, true, &evidence)
					if relationErr != nil {
						return RelationEvidence{}, relationErr
					}
					if next > index+1 {
						index = next - 1
					}
				}
			case ';':
				selectSeen = make(map[int]bool)
				inFromList = make(map[int]bool)
				insertSeen = make(map[int]bool)
				deleteSeen = make(map[int]bool)
				mergeSeen = make(map[int]bool)
				dmlSeen = make(map[int]bool)
				depth = 0
			}
			continue
		}
		if token.kind != relationIdentifier || token.identifier.Quoted {
			continue
		}

		word := token.identifier.Value
		switch word {
		case "select":
			selectSeen[depth] = true
			inFromList[depth] = false
		case "update":
			if mergeSeen[depth] {
				continue
			}
			dmlSeen[depth] = true
			next, relationErr := collectRelationEvidence(tokens, index+1, false, &evidence)
			if relationErr != nil {
				return RelationEvidence{}, relationErr
			}
			if next > index+1 {
				index = next - 1
			}
		case "insert":
			insertSeen[depth] = true
			dmlSeen[depth] = true
		case "delete":
			deleteSeen[depth] = true
			dmlSeen[depth] = true
		case "merge":
			mergeSeen[depth] = true
			dmlSeen[depth] = true
		case "into":
			if insertSeen[depth] || mergeSeen[depth] {
				next, relationErr := collectRelationEvidence(tokens, index+1, false, &evidence)
				if relationErr != nil {
					return RelationEvidence{}, relationErr
				}
				if next > index+1 {
					index = next - 1
				}
			}
		case "from":
			if selectSeen[depth] || deleteSeen[depth] || dmlSeen[depth] {
				inFromList[depth] = true
				next, relationErr := collectRelationEvidence(tokens, index+1, true, &evidence)
				if relationErr != nil {
					return RelationEvidence{}, relationErr
				}
				if next > index+1 {
					index = next - 1
				}
			}
		case "join":
			if selectSeen[depth] || dmlSeen[depth] {
				inFromList[depth] = true
				next, relationErr := collectRelationEvidence(tokens, index+1, true, &evidence)
				if relationErr != nil {
					return RelationEvidence{}, relationErr
				}
				if next > index+1 {
					index = next - 1
				}
			}
		case "using":
			if mergeSeen[depth] || deleteSeen[depth] {
				next, relationErr := collectRelationEvidence(tokens, index+1, true, &evidence)
				if relationErr != nil {
					return RelationEvidence{}, relationErr
				}
				if next > index+1 {
					index = next - 1
				}
			}
		case "analyze", "vacuum":
			next, relationErr := collectRelationEvidence(tokens, index+1, false, &evidence)
			if relationErr != nil {
				return RelationEvidence{}, relationErr
			}
			if next > index+1 {
				index = next - 1
			}
		case "on":
			inFromList[depth] = false
		case "where", "group", "having", "order", "limit", "offset", "union", "intersect", "except", "window", "returning":
			inFromList[depth] = false
		}
	}
	return evidence, nil
}

func collectRelationEvidence(
	tokens []relationToken,
	start int,
	rejectFunction bool,
	evidence *RelationEvidence,
) (int, error) {
	if ref, next, ok := parseQualifiedRelation(tokens, start, rejectFunction); ok {
		evidence.Qualified = append(evidence.Qualified, ref)
		return next, nil
	}
	if ref, next, ok := parseUnqualifiedRelation(tokens, start, rejectFunction); ok {
		evidence.Unqualified = append(evidence.Unqualified, ref)
		return next, nil
	}
	return start, nil
}

func parseUnqualifiedRelation(
	tokens []relationToken,
	start int,
	rejectFunction bool,
) (RelationRef, int, bool) {
	index := start
	for index < len(tokens) && isUnquotedWord(tokens[index], "only", "lateral", "verbose") {
		index++
	}
	if index >= len(tokens) || tokens[index].kind != relationIdentifier {
		return RelationRef{}, start, false
	}
	if index+1 < len(tokens) && tokens[index+1].kind == relationPunctuation {
		if tokens[index+1].punctuation == '.' {
			return RelationRef{}, start, false
		}
		if rejectFunction && tokens[index+1].punctuation == '(' {
			return RelationRef{}, start, false
		}
	}
	ref := RelationRef{Table: tokens[index].identifier}
	index++
	if index < len(tokens) && isUnquotedWord(tokens[index], "as") {
		index++
		if index < len(tokens) && tokens[index].kind == relationIdentifier {
			alias := tokens[index].identifier
			ref.Alias = &alias
			index++
		}
	} else if index < len(tokens) && tokens[index].kind == relationIdentifier && !isRelationBoundary(tokens[index]) {
		alias := tokens[index].identifier
		ref.Alias = &alias
		index++
	}
	return ref, index, true
}

func hasMultipleExecutableStatements(tokens []relationToken) bool {
	seenCode := false
	endedStatement := false
	for _, token := range tokens {
		if token.kind == relationPunctuation && token.punctuation == ';' {
			if seenCode {
				endedStatement = true
			}
			continue
		}
		if endedStatement {
			return true
		}
		seenCode = true
	}
	return false
}

func parseQualifiedRelation(tokens []relationToken, start int, rejectFunction bool) (RelationRef, int, bool) {
	index := start
	for index < len(tokens) && isUnquotedWord(tokens[index], "only", "lateral", "verbose") {
		index++
	}
	if index+2 >= len(tokens) || tokens[index].kind != relationIdentifier ||
		tokens[index+1].kind != relationPunctuation || tokens[index+1].punctuation != '.' ||
		tokens[index+2].kind != relationIdentifier {
		return RelationRef{}, start, false
	}
	ref := RelationRef{
		Schema: tokens[index].identifier,
		Table:  tokens[index+2].identifier,
	}
	index += 3
	if rejectFunction && index < len(tokens) && tokens[index].kind == relationPunctuation && tokens[index].punctuation == '(' {
		return RelationRef{}, start, false
	}
	if index < len(tokens) && isUnquotedWord(tokens[index], "as") {
		index++
		if index < len(tokens) && tokens[index].kind == relationIdentifier {
			alias := tokens[index].identifier
			ref.Alias = &alias
			index++
		}
	} else if index < len(tokens) && tokens[index].kind == relationIdentifier && !isRelationBoundary(tokens[index]) {
		alias := tokens[index].identifier
		ref.Alias = &alias
		index++
	}
	return ref, index, true
}

func isRelationBoundary(token relationToken) bool {
	if token.kind != relationIdentifier || token.identifier.Quoted {
		return false
	}
	switch token.identifier.Value {
	case "where", "join", "inner", "left", "right", "full", "cross", "natural", "on", "using",
		"group", "having", "order", "limit", "offset", "union", "intersect", "except", "window",
		"returning", "set", "values", "when":
		return true
	default:
		return false
	}
}

func isUnquotedWord(token relationToken, words ...string) bool {
	if token.kind != relationIdentifier || token.identifier.Quoted {
		return false
	}
	for _, word := range words {
		if token.identifier.Value == word {
			return true
		}
	}
	return false
}

func lexRelations(sqlText string) ([]relationToken, error) {
	tokens := make([]relationToken, 0, len(sqlText)/4)
	for index := 0; index < len(sqlText); {
		switch {
		case isSpace(sqlText[index]):
			index++
		case index+1 < len(sqlText) && sqlText[index:index+2] == "--":
			index += 2
			for index < len(sqlText) && sqlText[index] != '\n' {
				index++
			}
		case index+1 < len(sqlText) && sqlText[index:index+2] == "/*":
			next, err := scanRelationBlockComment(sqlText, index)
			if err != nil {
				return nil, err
			}
			index = next
		case sqlText[index] == '\'':
			next, err := scanRelationQuoted(sqlText, index, '\'')
			if err != nil {
				return nil, err
			}
			index = next
		case (sqlText[index] == 'e' || sqlText[index] == 'E') && index+1 < len(sqlText) && sqlText[index+1] == '\'' && relationPrefixBoundary(sqlText, index):
			next, err := scanRelationQuoted(sqlText, index+1, '\'')
			if err != nil {
				return nil, err
			}
			index = next
		case sqlText[index] == '"':
			identifier, next, err := scanQuotedIdentifier(sqlText, index)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, relationToken{kind: relationIdentifier, identifier: identifier})
			index = next
		case sqlText[index] == '$':
			delimiter := relationDollarDelimiter(sqlText, index)
			if delimiter == "" {
				index++
				continue
			}
			contentStart := index + len(delimiter)
			closeOffset := strings.Index(sqlText[contentStart:], delimiter)
			if closeOffset < 0 {
				return nil, fmt.Errorf("unterminated dollar-quoted string")
			}
			index = contentStart + closeOffset + len(delimiter)
		case isWordStart(sqlText[index]):
			start := index
			index++
			for index < len(sqlText) && isWordPart(sqlText[index]) {
				index++
			}
			tokens = append(tokens, relationToken{
				kind:       relationIdentifier,
				identifier: Identifier{Value: strings.ToLower(sqlText[start:index])},
			})
		case strings.ContainsRune(".,();", rune(sqlText[index])):
			tokens = append(tokens, relationToken{kind: relationPunctuation, punctuation: sqlText[index]})
			index++
		default:
			index++
		}
	}
	return tokens, nil
}

func scanRelationQuoted(sqlText string, start int, quote byte) (int, error) {
	for index := start + 1; index < len(sqlText); index++ {
		if sqlText[index] == '\\' && quote == '\'' && index+1 < len(sqlText) {
			index++
			continue
		}
		if sqlText[index] != quote {
			continue
		}
		if index+1 < len(sqlText) && sqlText[index+1] == quote {
			index++
			continue
		}
		return index + 1, nil
	}
	return 0, fmt.Errorf("unterminated quoted string")
}

func scanQuotedIdentifier(sqlText string, start int) (Identifier, int, error) {
	var value strings.Builder
	for index := start + 1; index < len(sqlText); index++ {
		if sqlText[index] != '"' {
			value.WriteByte(sqlText[index])
			continue
		}
		if index+1 < len(sqlText) && sqlText[index+1] == '"' {
			value.WriteByte('"')
			index++
			continue
		}
		return Identifier{Value: value.String(), Quoted: true}, index + 1, nil
	}
	return Identifier{}, 0, fmt.Errorf("unterminated quoted identifier")
}

func scanRelationBlockComment(sqlText string, start int) (int, error) {
	depth := 1
	for index := start + 2; index < len(sqlText); {
		switch {
		case index+1 < len(sqlText) && sqlText[index:index+2] == "/*":
			depth++
			index += 2
		case index+1 < len(sqlText) && sqlText[index:index+2] == "*/":
			depth--
			index += 2
			if depth == 0 {
				return index, nil
			}
		default:
			index++
		}
	}
	return 0, fmt.Errorf("unterminated block comment")
}

func relationDollarDelimiter(sqlText string, start int) string {
	if start >= len(sqlText) || sqlText[start] != '$' {
		return ""
	}
	for index := start + 1; index < len(sqlText); index++ {
		if sqlText[index] == '$' {
			if index == start+1 {
				return "$$"
			}
			return sqlText[start : index+1]
		}
		if !isWordPart(sqlText[index]) || index == start+1 && isDigit(sqlText[index]) {
			return ""
		}
	}
	return ""
}

func relationPrefixBoundary(sqlText string, index int) bool {
	return index == 0 || !isWordPart(sqlText[index-1])
}

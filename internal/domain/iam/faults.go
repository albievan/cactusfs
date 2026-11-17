package iam

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// A policy document that is refused is refused for every fault it has, each
// with where it is (ADR-0032): a builder shows a fault at its field, and a
// person who sent a document learns of all that is wrong with it at once.
// The error of a parser still reads as its first fault, word for word as it
// did before the faults were located, and wraps the same sentinels.

// MaxPolicyFaults is how many faults of one document are collected. A
// document with more is refused all the same; PolicyError.Truncated says so.
const MaxPolicyFaults = 100

// PolicyFault is one thing wrong with a policy document, and where it is.
type PolicyFault struct {
	// Pointer is where in the document as sent the fault is, as an RFC 6901
	// JSON pointer: "" is the whole document, "/Statement/2/Action/0" the
	// first action of the third statement. A field that may be one string
	// or a list (Action, Resource, a Principal's AWS, a condition's values)
	// is pointed at with an index when it is written as a list and without
	// one when it is written as a single string. A field that is missing is
	// pointed at by the object it is missing from.
	Pointer string
	// Statement is the index of the statement the fault is in, counted from
	// 0 as the reasons count; NoStatement when it is in none.
	Statement int
	// Reason is what is wrong, in the words of the parser. The reason of
	// the first fault is the text of the parser's error.
	Reason string
	// Offset is the byte offset in the document at which a fault of JSON
	// syntax - or data after the document - was met; NoOffset for every
	// other fault.
	Offset int64
}

// NoStatement is the PolicyFault.Statement of a fault that is in no
// statement.
const NoStatement = -1

// NoOffset is the PolicyFault.Offset of a fault that is not one of JSON
// syntax.
const NoOffset = -1

// PolicyError is the error of a parser that refused a policy document. It
// reads as its first fault, and wraps the sentinels the parser documents.
//
// Its faults quote and point into the document. They are for the caller who
// sent that document: take them only from the parse a use case makes of a
// document it is putting or validating, never from an error that also wraps
// domain.ErrIntegrity - the parse of a document already stored, which the
// caller may not be allowed to read.
type PolicyError struct {
	faults    []PolicyFault
	truncated bool
	sentinels []error
}

// Error returns the reason of the first fault. A PolicyError is made only
// with one; one made otherwise reads as an invalid argument.
func (e *PolicyError) Error() string {
	if len(e.faults) == 0 {
		return domain.ErrInvalidArgument.Error()
	}
	return e.faults[0].Reason
}

// Unwrap returns the sentinels the error wraps.
func (e *PolicyError) Unwrap() []error { return e.sentinels }

// Faults returns every fault collected, the first first: at least one, at
// most MaxPolicyFaults.
func (e *PolicyError) Faults() []PolicyFault { return slices.Clone(e.faults) }

// Truncated reports whether the document had more faults than were
// collected.
func (e *PolicyError) Truncated() bool { return e.truncated }

// faultList collects the faults of one document, in the order the parser
// meets them.
//
// A document is read first with a list that does not locate: no tree is
// built and no pointer made, so that a document that is accepted costs what
// it did before its faults were located. Only a document that is refused is
// read again, with a list that locates (parseLocated). The two readings make
// the same checks in the same order; only where a fault is said to be
// differs.
type faultList struct {
	// locate says that the tree is built and the faults are located.
	locate bool
	// stored says that the document is one already stored, read with the
	// tolerance of strayClose.
	stored bool
	// decoded says that the document was decoded, and end is the byte
	// offset just after it.
	decoded bool
	end     int64
	// tree is the document as read, once it could be read; pointers are
	// held against it.
	tree      *jsonNode
	faults    []PolicyFault
	truncated bool
}

// parseLocated parses a document with parse, first without locating its
// faults and, when it is refused, again to locate them. Should the second
// reading find no fault, which the two make the same checks to prevent, the
// faults of the first are kept: a document is never accepted by the second.
//
// stored is the tolerance of the fault lists: true for a document already
// stored (strayClose).
func parseLocated[T any](stored bool, parse func(faults *faultList) T) (T, *faultList) {
	quick := &faultList{stored: stored}
	result := parse(quick)
	if !quick.failed() {
		return result, quick
	}
	located := &faultList{locate: true, stored: stored}
	var zero T
	if parse(located); !located.failed() {
		return zero, quick
	}
	return zero, located
}

// add records a fault at pointer whose reason is err's text.
func (l *faultList) add(pointer string, err error) { l.addAt(pointer, NoOffset, err) }

// addAt records a fault at pointer met at a byte offset of the document.
func (l *faultList) addAt(pointer string, offset int64, err error) {
	if len(l.faults) == MaxPolicyFaults {
		l.truncated = true
		return
	}
	pointer = l.clamp(pointer)
	l.faults = append(l.faults, PolicyFault{Pointer: pointer, Statement: l.statementOf(pointer), Reason: err.Error(), Offset: offset})
}

// decodedTo records that the document was decoded, and that it ends at the
// byte offset end.
func (l *faultList) decodedTo(end int64) {
	l.decoded, l.end = true, end
}

// strayClose records the fault "data after the document" when the document
// was decoded and what follows it, after white space, begins with a closing
// bracket - "}" or "]" (PLAN 9.2a). It is called last, so that a document
// refused for something else reads as it did before.
//
// Every other byte after a document but white space is refused before: by
// the walk when a value begins there (readDocument), and right after the
// document is decoded when not (json.Decoder.More). A closing bracket was
// the one text the parsers took after a document before PLAN 9.2a: the walk
// cannot read a token there, and More reports none. A document already
// stored (stored) keeps that tolerance, so that a policy stored before it is
// read as it was; one being put never has it.
func (l *faultList) strayClose(data []byte) {
	if !l.decoded || l.stored {
		return
	}
	at, found := afterDocument(data, l.end)
	if found && (data[at] == '}' || data[at] == ']') {
		l.addAt("", at, invalidPolicy("data after the document"))
	}
}

// afterDocument returns the byte offset of the first byte of data from end
// on that is not JSON white space (RFC 8259: space, tab, line feed, carriage
// return); found is false when there is none.
func afterDocument(data []byte, end int64) (at int64, found bool) {
	for i := max(end, 0); i < int64(len(data)); i++ {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
		default:
			return i, true
		}
	}
	return 0, false
}

// failed reports whether a fault was recorded.
func (l *faultList) failed() bool { return len(l.faults) > 0 }

// err returns the error of the faults, wrapping sentinels; nil when there
// are none.
func (l *faultList) err(sentinels ...error) error {
	if !l.failed() {
		return nil
	}
	return &PolicyError{faults: l.faults, truncated: l.truncated, sentinels: sentinels}
}

// clamp returns the longest beginning of pointer that is there in the
// document: a field that is missing is pointed at by its object.
func (l *faultList) clamp(pointer string) string {
	if l.tree == nil || pointer == "" {
		return pointer
	}
	node, kept := l.tree, 0
	tokens := strings.Split(pointer[1:], "/")
	for _, token := range tokens {
		if node = node.child(unescapePointer(token)); node == nil {
			break
		}
		kept++
	}
	if kept == 0 {
		return ""
	}
	return "/" + strings.Join(tokens[:kept], "/")
}

// statementOf returns the index of the statement pointer is in.
func (l *faultList) statementOf(pointer string) int {
	rest, ok := strings.CutPrefix(pointer, "/Statement/")
	if !ok {
		return NoStatement
	}
	index, _, _ := strings.Cut(rest, "/")
	n, err := strconv.Atoi(index)
	if err != nil || n < 0 || strconv.Itoa(n) != index {
		return NoStatement
	}
	if l.tree != nil {
		if statements := l.tree.child("Statement"); statements == nil || statements.kind != kindArray {
			return NoStatement
		}
	}
	return n
}

// item returns the pointer of the i-th value of the field at pointer: with
// the index when the field is written as a list, without when it is one
// string. Before the document is read every field counts as a list, as
// MarshalJSON writes them.
func (l *faultList) item(pointer string, i int) string {
	if l.tree != nil {
		if node := l.tree.lookup(pointer); node == nil || node.kind != kindArray {
			return pointer
		}
	}
	return pointer + "/" + strconv.Itoa(i)
}

// escapePointer escapes a field name for a JSON pointer (RFC 6901).
func escapePointer(name string) string {
	return strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
}

// unescapePointer reads a field name of a JSON pointer (RFC 6901).
func unescapePointer(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
}

// jsonKind is the kind of a JSON value.
type jsonKind int

const (
	kindNull jsonKind = iota
	kindString
	kindNumber
	kindBool
	kindObject
	kindArray
)

// describe names a kind of value for a reason.
func (k jsonKind) describe() string {
	switch k {
	case kindString:
		return "a string"
	case kindNumber:
		return "a number"
	case kindBool:
		return "a boolean"
	case kindObject:
		return "an object"
	case kindArray:
		return "a list"
	default:
		return "null"
	}
}

// jsonNode is a value of a document as it was written: the order of the
// fields of an object and whether a field is one string or a list are kept.
type jsonNode struct {
	kind jsonKind
	// text is the value of a string.
	text string
	// names are the fields of an object, in order; elems their values, or
	// the items of a list.
	names []string
	elems []*jsonNode
}

// child returns the field name of an object or the item of a list whose
// index name is; nil when there is none.
func (n *jsonNode) child(name string) *jsonNode {
	if n == nil {
		return nil
	}
	switch n.kind {
	case kindObject:
		if i := slices.Index(n.names, name); i >= 0 {
			return n.elems[i]
		}
	case kindArray:
		i, err := strconv.Atoi(name)
		if err == nil && i >= 0 && i < len(n.elems) && strconv.Itoa(i) == name {
			return n.elems[i]
		}
	}
	return nil
}

// fieldNames returns the fields of an object, in order; none of a value
// that is no object, or of none.
func (n *jsonNode) fieldNames() []string {
	if n == nil || n.kind != kindObject {
		return nil
	}
	return n.names
}

// statements returns the items of the Statement of a document, when it is a
// list: each an object, or another value the parser will refuse.
func (n *jsonNode) statements() []*jsonNode {
	if n == nil {
		return nil
	}
	statements := n.child("Statement")
	if statements == nil || statements.kind != kindArray {
		return nil
	}
	return statements.elems
}

// lookup returns the value at pointer; nil when there is none.
func (n *jsonNode) lookup(pointer string) *jsonNode {
	if pointer == "" {
		return n
	}
	node := n
	for _, token := range strings.Split(pointer[1:], "/") {
		if node = node.child(unescapePointer(token)); node == nil {
			return nil
		}
	}
	return node
}

// walker reads a document token by token, as checkFields always has: it
// refuses a field given twice in one object, and, where the fields are
// fixed, one not spelled exactly as the policy language spells it. It keeps
// going after such a fault, and stops at a fault of JSON syntax. Only when
// the fault list locates does it build the tree and the pointers.
type walker struct {
	decoder *json.Decoder
	fields  func(where string) []string
	faults  *faultList
	broken  bool
}

// readDocument reads data, recording the faults of its form, and reports
// whether it had none. When the fault list locates, the tree is the document
// as read; otherwise it is nil.
func readDocument(faults *faultList, data []byte, fields func(where string) []string) (*jsonNode, bool) {
	w := &walker{decoder: json.NewDecoder(bytes.NewReader(data)), fields: fields, faults: faults}
	before := len(faults.faults)
	root := w.value("", "")
	if !w.broken {
		end := w.decoder.InputOffset()
		if _, err := w.decoder.Token(); err == nil {
			// The fault is where the data after the document begins.
			offset, _ := afterDocument(data, end)
			faults.addAt("", offset, invalidPolicy("data after the document"))
		}
	}
	if len(faults.faults) > before || faults.truncated {
		return nil, false
	}
	return root, true
}

// fieldSet holds the names of the fields of one object met so far: in a
// list while there are few, in a map once there are more, so that an object
// of many fields is checked in linear time.
type fieldSet struct {
	list [maxFieldList]string
	n    int
	set  map[string]struct{}
}

// maxFieldList is how many names a fieldSet keeps in its list.
const maxFieldList = 16

// add records name, and reports whether it was there already.
func (s *fieldSet) add(name string) bool {
	if s.set != nil {
		if _, ok := s.set[name]; ok {
			return true
		}
		s.set[name] = struct{}{}
		return false
	}
	if slices.Contains(s.list[:s.n], name) {
		return true
	}
	if s.n == maxFieldList {
		s.set = make(map[string]struct{}, 2*maxFieldList)
		for _, known := range s.list {
			s.set[known] = struct{}{}
		}
		s.set[name] = struct{}{}
		return false
	}
	s.list[s.n] = name
	s.n++
	return false
}

// child returns the pointer of a field or an item of the value at pointer;
// empty when the fault list does not locate, so that a document that is
// accepted is read without making a pointer.
func (w *walker) child(pointer, token string) string {
	if !w.faults.locate {
		return ""
	}
	return pointer + "/" + token
}

// fault records a fault of the form of the document.
func (w *walker) fault(pointer string, err error) { w.faults.add(pointer, invalidPolicy("%v", err)) }

// syntax records a fault of JSON syntax, and stops the walk.
func (w *walker) syntax(pointer string, err error) {
	offset := w.decoder.InputOffset()
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		offset = syntax.Offset
	}
	w.faults.addAt(pointer, offset, invalidPolicy("%v", err))
	w.broken = true
}

// value reads one value. where is the place of the value with every index
// written "[]", as fieldsAt asks; pointer is its JSON pointer.
func (w *walker) value(where, pointer string) *jsonNode {
	if strings.Count(where, "/") > maxPolicyDepth {
		w.fault(pointer, errors.New("nested too deeply"))
		w.skip(pointer)
		return nil
	}
	token, err := w.decoder.Token()
	if err != nil {
		w.syntax(pointer, err)
		return nil
	}
	if delim, ok := token.(json.Delim); ok {
		return w.container(delim, where, pointer)
	}
	if !w.faults.locate {
		return nil
	}
	var node *jsonNode
	switch value := token.(type) {
	case string:
		node = &jsonNode{kind: kindString, text: value}
	case bool:
		node = &jsonNode{kind: kindBool}
	case nil:
		node = &jsonNode{kind: kindNull}
	default:
		node = &jsonNode{kind: kindNumber}
	}
	return node
}

// container reads the fields of an object or the items of a list whose
// opening delimiter was read, and its closing one.
func (w *walker) container(delim json.Delim, where, pointer string) *jsonNode {
	var node *jsonNode
	if w.faults.locate {
		node = &jsonNode{kind: kindArray}
	}
	switch delim {
	case '{':
		if node != nil {
			node.kind = kindObject
		}
		var seen fieldSet
		for w.decoder.More() {
			token, err := w.decoder.Token()
			if err != nil {
				w.syntax(pointer, err)
				return nil
			}
			name, _ := token.(string)
			at := ""
			if w.faults.locate {
				at = w.child(pointer, escapePointer(name))
			}
			if allowed := w.fields(where); seen.add(name) {
				w.fault(at, fmt.Errorf("field %q appears twice", name))
			} else if allowed != nil && !slices.Contains(allowed, name) {
				w.fault(at, fmt.Errorf("unknown field %q", name))
			}
			child := w.value(where+"/"+name, at)
			if w.broken {
				return nil
			}
			if node != nil {
				node.names = append(node.names, name)
				node.elems = append(node.elems, child)
			}
		}
	case '[':
		for i := 0; w.decoder.More(); i++ {
			at := ""
			if w.faults.locate {
				at = w.child(pointer, strconv.Itoa(i))
			}
			child := w.value(where+"/[]", at)
			if w.broken {
				return nil
			}
			if node != nil {
				node.elems = append(node.elems, child)
			}
		}
	}
	// The closing bracket.
	if _, err := w.decoder.Token(); err != nil {
		w.syntax(pointer, err)
		return nil
	}
	return node
}

// skip reads one value without looking at it: one nested too deeply.
func (w *walker) skip(pointer string) {
	depth := 0
	for {
		token, err := w.decoder.Token()
		if err != nil {
			w.syntax(pointer, err)
			return
		}
		if delim, ok := token.(json.Delim); ok {
			if delim == '{' || delim == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			return
		}
	}
}

// typeFault is a value of a document that encoding/json cannot decode into
// the wire form of a parser. custom marks one refused by oneOrMany, whose
// error stops decoding; any other is a type error, after which decoding
// goes on.
type typeFault struct {
	pointer string
	custom  bool
	err     error
}

// shape is what a value of the wire form of a parser may be.
type shape int

const (
	// shapeAny is a json.RawMessage: anything.
	shapeAny shape = iota
	// shapeString is a string, or null.
	shapeString
	// shapeStrings is a oneOrMany: a string, null, or a list of strings
	// and nulls.
	shapeStrings
	// shapeConditions is the Condition of an identity policy: an object of
	// objects of oneOrMany, or null.
	shapeConditions
)

// typeFaults lists, in document order, the values of a document that do not
// fit the wire form of a parser: an object or null, whose Version has the
// shape version, whose Statement is a list of objects or nulls, and whose
// statements' fields have the shapes fieldShape gives.
func typeFaults(root *jsonNode, version shape, fieldShape func(name string) shape) []typeFault {
	var out []typeFault
	if root == nil || root.kind == kindNull {
		return nil
	}
	if root.kind != kindObject {
		out = append(out, typeFault{pointer: "", err: invalidPolicy("the document: want an object, not %s", root.kind.describe())})
		return out
	}
	for i, name := range root.names {
		value, at := root.elems[i], "/"+escapePointer(name)
		switch name {
		case "Version":
			out = append(out, shapeFaults(at, name, value, version)...)
		case "Statement":
			if value.kind == kindNull {
				continue
			}
			if value.kind != kindArray {
				out = append(out, typeFault{pointer: at, err: invalidPolicy("Statement: want a list, not %s", value.kind.describe())})
				continue
			}
			for j, statement := range value.elems {
				here := at + "/" + strconv.Itoa(j)
				switch statement.kind {
				case kindNull:
				case kindObject:
					for k, field := range statement.names {
						out = append(out, shapeFaults(here+"/"+escapePointer(field), field, statement.elems[k], fieldShape(field))...)
					}
				default:
					out = append(out, typeFault{pointer: here, err: invalidPolicy("a statement: want an object, not %s", statement.kind.describe())})
				}
			}
		}
	}
	return out
}

// shapeFaults lists the faults of a value that should have shape s.
func shapeFaults(pointer, name string, n *jsonNode, s shape) []typeFault {
	switch s {
	case shapeString:
		if n.kind != kindString && n.kind != kindNull {
			return []typeFault{{pointer: pointer, err: invalidPolicy("%s: want a string, not %s", name, n.kind.describe())}}
		}
	case shapeStrings:
		if !stringsShape(n) {
			return []typeFault{{pointer: pointer, custom: true, err: invalidPolicy("%v", errWantStrings)}}
		}
	case shapeConditions:
		if n.kind == kindNull {
			return nil
		}
		if n.kind != kindObject {
			return []typeFault{{pointer: pointer, err: invalidPolicy("%s: want an object, not %s", name, n.kind.describe())}}
		}
		var out []typeFault
		for i, operator := range n.names {
			tests, at := n.elems[i], pointer+"/"+escapePointer(operator)
			switch tests.kind {
			case kindNull:
			case kindObject:
				for j, key := range tests.names {
					out = append(out, shapeFaults(at+"/"+escapePointer(key), key, tests.elems[j], shapeStrings)...)
				}
			default:
				out = append(out, typeFault{pointer: at, err: invalidPolicy("%s: want an object, not %s", operator, tests.kind.describe())})
			}
		}
		return out
	}
	return nil
}

// stringsShape reports whether a value decodes as a oneOrMany.
func stringsShape(n *jsonNode) bool {
	switch n.kind {
	case kindString, kindNull:
		return true
	case kindArray:
		for _, item := range n.elems {
			if item.kind != kindString && item.kind != kindNull {
				return false
			}
		}
		return true
	}
	return false
}

// decodeFault returns the pointer of the fault encoding/json reports of a
// document it could not decode: the first that a oneOrMany refused, which
// stops decoding, or else the first type error. found is false when none of
// faults is either, and the fault is then the whole document's.
func decodeFault(faults []typeFault) (pointer string, found bool) {
	for _, f := range faults {
		if f.custom {
			return f.pointer, true
		}
	}
	if len(faults) > 0 {
		return faults[0].pointer, true
	}
	return "", false
}

// addDecodeFaults records the faults of a document encoding/json could not
// decode: first the one its error is about, with its error's text, then
// every other value that does not fit.
func addDecodeFaults(l *faultList, decodeErr error, faults []typeFault) {
	first, _ := decodeFault(faults)
	l.add(first, invalidPolicy("%v", decodeErr))
	for _, f := range faults {
		if f.pointer != first {
			l.add(f.pointer, f.err)
		}
	}
}

// Package tablesort is the shared, table-agnostic column-sort component (#1242):
// a per-table Spec (allowlist of column key -> fixed SQL expression, default
// order, stable id column), URL parse/validate, ORDER BY and keyset helpers
// with NULLs last in both directions, and a cursor codec that carries the sort
// value. It is a leaf package (net/url, net/http only) so both the query layer
// and the web layer can import it. The Work Queue is its first consumer.
//
// Nothing a caller supplies is ever interpolated into SQL: a request names a
// column key, which selects a fixed expression from the Spec; every cursor
// value is a bound parameter.
package tablesort

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Shared column vocabulary: every table that sorts names its columns from this
// set, so one log validator (KnownKey) serves them all.
const (
	KeyArtist      = "artist"
	KeyAlbum       = "album"
	KeyTitle       = "title"
	KeyStatus      = "status"
	KeyNextAttempt = "next_attempt"
	KeyMisses      = "misses"
	KeyAttempts    = "attempts"
	KeyUpdated     = "updated"
)

var vocabulary = map[string]bool{
	KeyArtist: true, KeyAlbum: true, KeyTitle: true, KeyStatus: true,
	KeyNextAttempt: true, KeyMisses: true, KeyAttempts: true, KeyUpdated: true,
}

// KnownKey reports whether key is in the shared vocabulary. Adding a column to
// a new table means adding its key here, once.
func KnownKey(key string) bool { return vocabulary[key] }

// ValidDir reports whether dir is a direction value.
func ValidDir(dir string) bool { return dir == "asc" || dir == "desc" }

// Column is one sortable column. Expr is a fixed SQL fragment.
type Column struct {
	Expr string
	// Integer columns carry integer cursor values, the rest text.
	Integer bool
	// DescFirst is the direction a header click requests first (newest/largest).
	DescFirst bool
}

// Spec is one table's sort definition. ID names the stable tie-break column.
type Spec struct {
	Columns map[string]Column
	ID      string
	Default Order
}

// Order is a resolved sort. The zero Order means "by ID ascending, unsorted".
type Order struct {
	Key  string
	Desc bool
}

// Resolve validates sort/dir against the spec. An unknown or empty sort falls
// back to the default column; an unknown dir to that column's natural
// direction.
func (s Spec) Resolve(sort, dir string) Order {
	o := s.Default
	if c, ok := s.Columns[sort]; ok {
		o = Order{Key: sort, Desc: c.DescFirst}
	}
	switch dir {
	case "asc":
		o.Desc = false
	case "desc":
		o.Desc = true
	}
	return o
}

// ParseValues validates the sort/dir in v. It returns the resolved order and
// the query values to re-emit: only a value that was present AND valid, so an
// invalid one is dropped rather than reflected.
func ParseValues(v url.Values, s Spec) (Order, url.Values) {
	keep := url.Values{}
	sort, dir := v.Get("sort"), v.Get("dir")
	if _, ok := s.Columns[sort]; !ok {
		sort = ""
	} else {
		keep.Set("sort", sort)
	}
	if !ValidDir(dir) {
		dir = ""
	} else {
		keep.Set("dir", dir)
	}
	return s.Resolve(sort, dir), keep
}

// ParseSort is ParseValues over a request's query string.
func ParseSort(r *http.Request, s Spec) (Order, url.Values) {
	return ParseValues(r.URL.Query(), s)
}

func (s Spec) column(o Order) (Column, bool) {
	c, ok := s.Columns[o.Key]
	return c, ok
}

// OrderBy is the ORDER BY body: NULLs last in both directions, then the value,
// then the id in the same direction. An unsorted Order orders by id ascending.
func (s Spec) OrderBy(o Order) string {
	c, ok := s.column(o)
	if !ok {
		return s.ID + " ASC"
	}
	dir := "ASC"
	if o.Desc {
		dir = "DESC"
	}
	return fmt.Sprintf("(%[1]s) IS NULL, %[1]s %[2]s, %[3]s %[2]s", c.Expr, dir, s.ID)
}

// SelectExpr is the expression that yields a row's own sort value for its
// cursor: text is cast so a DATETIME column scans as the string it compares as.
func (s Spec) SelectExpr(o Order) string {
	c, ok := s.column(o)
	switch {
	case !ok:
		return "NULL"
	case c.Integer:
		return c.Expr
	}
	return "CAST(" + c.Expr + " AS TEXT)"
}

// Cursor is a keyset position: the last row's id and its encoded sort value
// ("n" NULL, "i<int>", "t<text>"). The zero Cursor is the top of the list.
type Cursor struct {
	ID  int64
	Val string
}

// EncodeValue renders a scanned sort value (nil, int64, string, []byte).
func EncodeValue(v any) string {
	switch x := v.(type) {
	case int64:
		return "i" + strconv.FormatInt(x, 10)
	case string:
		return "t" + x
	case []byte:
		return "t" + string(x)
	}
	return "n"
}

// maxCursorRunes bounds an encoded cursor.
const maxCursorRunes = 600

// Encode renders the cursor for a URL ("<id>:<value>"); the zero Cursor is "".
func (c Cursor) Encode() string {
	if c.ID == 0 {
		return ""
	}
	return strconv.FormatInt(c.ID, 10) + ":" + c.Val
}

// DecodeCursor parses and validates raw against the active column. ok is false
// for anything forged or mismatched (callers fall back to the first page); an
// empty raw is the zero Cursor with ok true.
func (s Spec) DecodeCursor(o Order, raw string) (Cursor, bool) {
	if raw == "" {
		return Cursor{}, true
	}
	if utf8.RuneCountInString(raw) > maxCursorRunes || !utf8.ValidString(raw) {
		return Cursor{}, false
	}
	idPart, val, found := strings.Cut(raw, ":")
	id, err := strconv.ParseInt(idPart, 10, 64)
	if !found || err != nil || id <= 0 || val == "" {
		return Cursor{}, false
	}
	col, sorted := s.column(o)
	switch {
	case val == "n":
	case sorted && col.Integer && val[0] == 'i':
		if _, err := strconv.ParseInt(val[1:], 10, 64); err != nil {
			return Cursor{}, false
		}
	case sorted && !col.Integer && val[0] == 't':
	default:
		return Cursor{}, false
	}
	return Cursor{ID: id, Val: val}, true
}

// Keyset is the WHERE predicate (with leading AND) selecting rows strictly after
// c under o, plus its bound args; "" for the first page. The cursor must have
// passed DecodeCursor. NULL rows come last in either direction, so a non-NULL
// cursor has not reached them yet.
func (s Spec) Keyset(o Order, c Cursor) (string, []any) {
	if c.ID == 0 {
		return "", nil
	}
	col, ok := s.column(o)
	if !ok {
		return fmt.Sprintf(" AND %s > ?", s.ID), []any{c.ID}
	}
	op := ">"
	if o.Desc {
		op = "<"
	}
	if c.Val == "n" {
		return fmt.Sprintf(" AND ((%s) IS NULL AND %s %s ?)", col.Expr, s.ID, op), []any{c.ID}
	}
	var v any = c.Val[1:]
	if col.Integer {
		n, _ := strconv.ParseInt(c.Val[1:], 10, 64)
		v = n
	}
	return fmt.Sprintf(" AND (%[1]s %[3]s ? OR (%[1]s = ? AND %[2]s %[3]s ?) OR (%[1]s) IS NULL)",
		col.Expr, s.ID, op), []any{v, v, c.ID}
}

// Toggle is the order a click on key's header requests: the active column
// flips direction, any other column starts in its natural direction.
func (s Spec) Toggle(active Order, key string) Order {
	if key == active.Key {
		return Order{Key: key, Desc: !active.Desc}
	}
	return Order{Key: key, Desc: s.Columns[key].DescFirst}
}

// AriaSort is the aria-sort value for key's header: "ascending" or
// "descending" on the active column, "" elsewhere.
func AriaSort(active Order, key string) string {
	switch {
	case key != active.Key:
		return ""
	case active.Desc:
		return "descending"
	}
	return "ascending"
}

// HeaderHref is a header link: base plus the preserved params (never a cursor,
// a re-sort starts at the top) plus the requested sort and dir.
func HeaderHref(base string, keep url.Values, o Order) string {
	v := url.Values{}
	for k, vs := range keep {
		v[k] = append([]string(nil), vs...)
	}
	v.Set("sort", o.Key)
	v.Set("dir", map[bool]string{false: "asc", true: "desc"}[o.Desc])
	return base + "?" + v.Encode()
}

// LoggableCursor reports whether raw is safe to write to a request log: a bare
// id or "<id>:n" / "<id>:i<int>". A text-valued cursor carries library
// metadata and is not loggable.
func LoggableCursor(raw string) bool {
	idPart, val, found := strings.Cut(raw, ":")
	if n, err := strconv.ParseInt(idPart, 10, 64); err != nil || n < 0 {
		return false
	}
	if !found {
		return true
	}
	if val == "n" {
		return true
	}
	if len(val) > 1 && val[0] == 'i' {
		_, err := strconv.ParseInt(val[1:], 10, 64)
		return err == nil
	}
	return false
}

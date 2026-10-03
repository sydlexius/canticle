package tablesort

import (
	"database/sql"
	"fmt"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

var testSpec = Spec{
	Columns: map[string]Column{
		KeyTitle:    {Expr: "name COLLATE NOCASE"},
		KeyMisses:   {Expr: "n", Integer: true, DescFirst: true},
		KeyUpdated:  {Expr: "ts", DescFirst: true},
		KeyArtist:   {Expr: "name"},
		KeyAttempts: {Expr: "n", Integer: true},
	},
	ID:      "id",
	Default: Order{Key: KeyUpdated, Desc: true},
}

type fixtureRow struct {
	id   int64
	name sql.NullString
	n    sql.NullInt64
}

// fixture has duplicate keys and NULLs in both columns, so ties and the NULL
// group are exercised under every sort.
func fixture(t *testing.T) (*sql.DB, []fixtureRow) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=private")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT, n INTEGER, ts TEXT)`); err != nil {
		t.Fatal(err)
	}
	var rows []fixtureRow
	for i := int64(1); i <= 23; i++ {
		r := fixtureRow{id: i}
		if i%5 != 0 {
			r.name = sql.NullString{String: []string{"b", "A", "a", "c"}[i%4], Valid: true}
		}
		if i%7 != 0 {
			r.n = sql.NullInt64{Int64: i % 3, Valid: true}
		}
		if _, err := db.Exec(`INSERT INTO t VALUES (?, ?, ?, ?)`, i, r.name, r.n, r.name); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, r)
	}
	return db, rows
}

// page reads one keyset page of the fixture table.
func page(t *testing.T, db *sql.DB, o Order, c Cursor, limit int) (ids []int64, last Cursor) {
	t.Helper()
	where, kargs := testSpec.Keyset(o, c)
	q := "SELECT id, " + testSpec.SelectExpr(o) + " FROM t WHERE 1=1" + where + " ORDER BY " + testSpec.OrderBy(o) + " LIMIT ?"
	rs, err := db.Query(q, append(kargs, limit)...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rs.Close() }()
	for rs.Next() {
		var id int64
		var sv any
		if err := rs.Scan(&id, &sv); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		last = Cursor{ID: id, Val: EncodeValue(sv)}
	}
	return ids, last
}

// want orders the fixture in Go: NULLs last in both directions, ties by id in
// the sort direction. It is the oracle the SQL must agree with.
func want(rows []fixtureRow, key string, desc bool) []int64 {
	val := func(r fixtureRow) (string, bool) {
		switch key {
		case KeyMisses, KeyAttempts:
			return fmt.Sprintf("%05d", r.n.Int64), r.n.Valid
		case KeyTitle:
			return strings.ToLower(r.name.String), r.name.Valid
		}
		return r.name.String, r.name.Valid
	}
	out := append([]fixtureRow(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool {
		a, aok := val(out[i])
		b, bok := val(out[j])
		if aok != bok {
			return aok
		}
		if a != b {
			return (a < b) != desc
		}
		return (out[i].id < out[j].id) != desc
	})
	ids := make([]int64, len(out))
	for i, r := range out {
		ids[i] = r.id
	}
	return ids
}

func TestPagingEverySortBothDirectionsVisitsEachRowOnce(t *testing.T) {
	db, rows := fixture(t)
	for _, key := range []string{KeyTitle, KeyMisses, KeyArtist, KeyAttempts} {
		for _, desc := range []bool{false, true} {
			o := Order{Key: key, Desc: desc}
			var got []int64
			var cur Cursor
			for i := 0; i < 30; i++ {
				ids, last := page(t, db, o, cur, 4)
				if len(ids) == 0 {
					break
				}
				got = append(got, ids...)
				// Round-trip through the URL form, as a real "Show more" does.
				dec, ok := testSpec.DecodeCursor(o, last.Encode())
				if !ok {
					t.Fatalf("%s desc=%v: own cursor %q rejected", key, desc, last.Encode())
				}
				cur = dec
			}
			if fmt.Sprint(got) != fmt.Sprint(want(rows, key, desc)) {
				t.Errorf("%s desc=%v:\n got %v\nwant %v", key, desc, got, want(rows, key, desc))
			}
		}
	}
}

func TestNullsSortLastInBothDirections(t *testing.T) {
	db, _ := fixture(t)
	for _, desc := range []bool{false, true} {
		ids, _ := page(t, db, Order{Key: KeyMisses, Desc: desc}, Cursor{}, 100)
		seenNull := false
		for _, id := range ids {
			isNull := id%7 == 0
			if seenNull && !isNull {
				t.Fatalf("desc=%v: non-NULL row %d after a NULL row: %v", desc, id, ids)
			}
			seenNull = seenNull || isNull
		}
		if !seenNull {
			t.Fatalf("desc=%v: fixture produced no NULL rows", desc)
		}
	}
}

func TestUnsortedOrderIsIDAscending(t *testing.T) {
	db, _ := fixture(t)
	ids, last := page(t, db, Order{}, Cursor{}, 5)
	if fmt.Sprint(ids) != "[1 2 3 4 5]" {
		t.Fatalf("ids = %v", ids)
	}
	ids, _ = page(t, db, Order{}, Cursor{ID: last.ID}, 2)
	if fmt.Sprint(ids) != "[6 7]" {
		t.Fatalf("after 5: %v", ids)
	}
}

func TestResolveFallsBackToDefault(t *testing.T) {
	cases := []struct {
		sort, dir string
		want      Order
	}{
		{"", "", Order{Key: KeyUpdated, Desc: true}},
		{"id;drop table t", "", Order{Key: KeyUpdated, Desc: true}},
		{"nope", "asc", Order{Key: KeyUpdated}},
		{KeyTitle, "", Order{Key: KeyTitle}},
		{KeyMisses, "", Order{Key: KeyMisses, Desc: true}},
		{KeyTitle, "sideways", Order{Key: KeyTitle}},
		{KeyTitle, "desc", Order{Key: KeyTitle, Desc: true}},
		{KeyStatus, "", Order{Key: KeyUpdated, Desc: true}}, // vocabulary key this spec lacks
	}
	for _, c := range cases {
		if got := testSpec.Resolve(c.sort, c.dir); got != c.want {
			t.Errorf("Resolve(%q,%q) = %+v, want %+v", c.sort, c.dir, got, c.want)
		}
	}
}

func TestParseSortKeepsOnlyValidValues(t *testing.T) {
	for raw, wantKeep := range map[string]string{
		"sort=title&dir=desc":     "dir=desc&sort=title",
		"sort=id%3Bdrop&dir=up":   "",
		"sort=title&dir=sideways": "sort=title",
		"":                        "",
	} {
		req := httptest.NewRequest("GET", "/x?"+raw, nil)
		_, keep := ParseSort(req, testSpec)
		if got := keep.Encode(); got != wantKeep {
			t.Errorf("ParseSort(%q) keep = %q, want %q", raw, got, wantKeep)
		}
	}
	o, _ := ParseValues(url.Values{"sort": {KeyTitle}, "dir": {"desc"}}, testSpec)
	if o != (Order{Key: KeyTitle, Desc: true}) {
		t.Errorf("order = %+v", o)
	}
}

func TestCursorRoundTripAndForgery(t *testing.T) {
	text, intO := Order{Key: KeyTitle}, Order{Key: KeyMisses}
	for _, c := range []struct {
		o Order
		c Cursor
	}{
		{text, Cursor{ID: 9, Val: "tHello: world"}},
		{text, Cursor{ID: 9, Val: "n"}},
		{intO, Cursor{ID: 3, Val: "i-7"}},
	} {
		got, ok := testSpec.DecodeCursor(c.o, c.c.Encode())
		if !ok || got != c.c {
			t.Errorf("round trip %+v -> %+v ok=%v", c.c, got, ok)
		}
	}
	if got, ok := testSpec.DecodeCursor(text, ""); !ok || got != (Cursor{}) {
		t.Errorf("empty cursor = %+v ok=%v, want zero, true", got, ok)
	}
	for _, bad := range []string{
		"abc", "5", "5:", "0:n", "-1:n", "x:n", "5:z", "5:iabc", "5:i",
		"5:i3", // integer value on a text column
		"5:t" + strings.Repeat("x", MaxCursorBytes), // one byte over the cap
	} {
		if _, ok := testSpec.DecodeCursor(text, bad); ok {
			t.Errorf("forged cursor %q accepted for a text column", bad)
		}
	}
	if _, ok := testSpec.DecodeCursor(intO, "5:tabc"); ok {
		t.Error("text value accepted for an integer column")
	}
	if _, ok := testSpec.DecodeCursor(Order{}, "5:tabc"); ok {
		t.Error("typed value accepted for an unsorted listing")
	}
}

// DecodeCursor accepts everything EncodeValue can produce, up to the byte cap:
// a value far past the old 600-rune cap and one that is not valid UTF-8 both
// round-trip, since a tag can hold either and the value is only a bound parameter.
func TestCursorRoundTripsLongAndInvalidUTF8Values(t *testing.T) {
	text := Order{Key: KeyTitle}
	for name, val := range map[string]string{
		"long":       EncodeValue(strings.Repeat("\u00e9", 620)),
		"invalidUTF": EncodeValue("bad\xff\xfe tag"),
		"atCap":      "t" + strings.Repeat("x", MaxCursorBytes-len("9:t")),
	} {
		c := Cursor{ID: 9, Val: val}
		got, ok := testSpec.DecodeCursor(text, c.Encode())
		if !ok || got != c {
			t.Errorf("%s: round trip failed ok=%v", name, ok)
		}
	}
}

// A text cursor value full of SQL is just a bound string: the query still runs
// and returns rows, never an injection.
func TestCursorValueIsBoundNotInterpolated(t *testing.T) {
	db, _ := fixture(t)
	o := Order{Key: KeyTitle}
	c, ok := testSpec.DecodeCursor(o, "3:t'; DROP TABLE t; --")
	if !ok {
		t.Fatal("text cursor rejected")
	}
	page(t, db, o, c, 5)
	if _, err := db.Exec(`SELECT 1 FROM t`); err != nil {
		t.Fatalf("table gone: %v", err)
	}
}

func TestHeaderHelpers(t *testing.T) {
	active := Order{Key: KeyUpdated, Desc: true}
	if got := testSpec.Toggle(active, KeyUpdated); got != (Order{Key: KeyUpdated}) {
		t.Errorf("toggle active = %+v", got)
	}
	if got := testSpec.Toggle(active, KeyMisses); got != (Order{Key: KeyMisses, Desc: true}) {
		t.Errorf("toggle other (desc-first) = %+v", got)
	}
	if got := testSpec.Toggle(active, KeyTitle); got != (Order{Key: KeyTitle}) {
		t.Errorf("toggle other (asc-first) = %+v", got)
	}
	for _, c := range []struct {
		o    Order
		key  string
		want string
	}{
		{active, KeyUpdated, "descending"}, {Order{Key: KeyUpdated}, KeyUpdated, "ascending"}, {active, KeyTitle, ""},
	} {
		if got := AriaSort(c.o, c.key); got != c.want {
			t.Errorf("AriaSort(%+v,%s) = %q, want %q", c.o, c.key, got, c.want)
		}
	}
	keep := url.Values{"q": {"a b"}, "after": {"stale"}}
	got := HeaderHref("/t", url.Values{"q": keep["q"]}, Order{Key: KeyTitle, Desc: true})
	if got != "/t?dir=desc&q=a+b&sort=title" {
		t.Errorf("HeaderHref = %q", got)
	}
}

func TestVocabularyAndLoggableCursor(t *testing.T) {
	if !KnownKey(KeyNextAttempt) || KnownKey("id;drop") || KnownKey("") {
		t.Error("KnownKey wrong")
	}
	if !ValidDir("asc") || !ValidDir("desc") || ValidDir("sideways") {
		t.Error("ValidDir wrong")
	}
	for raw, ok := range map[string]bool{
		"12": true, "12:n": true, "12:i-4": true, "12:tSecret Artist": false,
		"x": false, "-1": false, "12:i": false, "12:iabc": false, "": false,
	} {
		if got := LoggableCursor(raw); got != ok {
			t.Errorf("LoggableCursor(%q) = %v, want %v", raw, got, ok)
		}
	}
}

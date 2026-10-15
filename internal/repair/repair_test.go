package repair

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fakeRepair is a Repair with a fixed Info.
type fakeRepair struct{ info Info }

func (f fakeRepair) Info() Info                                      { return f.info }
func (f fakeRepair) Pending(context.Context, Options) (int64, error) { return 0, nil }
func (f fakeRepair) Run(context.Context, Options) (Result, error) {
	return NewResult(f.info.Counts...), nil
}

func validInfo(key Key) Info {
	return Info{Key: key, Title: "Settle tiers", Counts: []CountKey{"checked", "settled"}}
}

// TestImportsNothingInternal: the package is a leaf, so internal/web and
// internal/commands can both import it.
func TestImportsNothingInternal(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(path, "github.com/sydlexius/canticle/") {
				t.Errorf("%s imports %s; the package must import nothing internal", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no package files parsed")
	}
}

// TestResultHasNoFreeText: Result is built only from slices and structs whose
// leaves are CountKey or int64, so it has nowhere to carry a path or a name.
// Any other leaf type fails, including []byte, [N]byte and [N]rune.
func TestResultHasNoFreeText(t *testing.T) {
	allowed := map[reflect.Type]bool{
		reflect.TypeFor[CountKey](): true,
		reflect.TypeFor[int64]():    true,
	}
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, at string) {
		switch typ.Kind() {
		case reflect.Slice:
			walk(typ.Elem(), at+"[]")
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				walk(f.Type, at+"."+f.Name)
			}
		default:
			if !allowed[typ] {
				t.Errorf("%s has type %s; a Result may hold only CountKey and int64", at, typ)
			}
		}
	}
	walk(reflect.TypeFor[Result](), "Result")
}

func TestKeyValid(t *testing.T) {
	cases := []struct {
		in   Key
		want bool
	}{
		{"tier-unknown", true},
		{"a", true},
		{"tag-reread2", true},
		{"", false},
		{"Tier", false},
		{"2fix", false},
		{"-fix", false},
		{"tier_unknown", false},
		{"a/b", false},
		{"../etc", false},
		{"tier unknown", false},
		{Key(strings.Repeat("a", maxIdentLen)), true},
		{Key(strings.Repeat("a", maxIdentLen+1)), false},
	}
	for _, c := range cases {
		if got := c.in.Valid(); got != c.want {
			t.Errorf("Key(%q).Valid() = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCountKeyValid(t *testing.T) {
	cases := []struct {
		in   CountKey
		want bool
	}{
		{"settled", true},
		{"reset_for_refetch", true},
		{"rows2", true},
		{"", false},
		{"Settled", false},
		{"_x", false},
		{"9rows", false},
		{"with-hyphen", false},
		{"/music/a.flac", false},
		{"a.lrc", false},
		{"two words", false},
		{"café", false},
		{CountKey(strings.Repeat("a", maxIdentLen+1)), false},
	}
	for _, c := range cases {
		if got := c.in.Valid(); got != c.want {
			t.Errorf("CountKey(%q).Valid() = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestInfoValidate(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Info)
		ok   bool
	}{
		{"valid", func(*Info) {}, true},
		{"no counts", func(i *Info) { i.Counts = nil }, true},
		{"bad key", func(i *Info) { i.Key = "Bad Key" }, false},
		{"empty title", func(i *Info) { i.Title = "" }, false},
		{"bad count key", func(i *Info) { i.Counts = []CountKey{"ok", "not/ok"} }, false},
		{"duplicate count key", func(i *Info) { i.Counts = []CountKey{"ok", "ok"} }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := validInfo("tier-unknown")
			c.mod(&info)
			err := info.Validate()
			if c.ok && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !c.ok && !errors.Is(err, ErrInvalidInfo) {
				t.Fatalf("Validate() = %v, want ErrInvalidInfo", err)
			}
		})
	}
}

func TestResultCounts(t *testing.T) {
	r := NewResult("checked", "settled", "skipped")
	r.Add("settled", 3)
	r.Add("checked", 5)
	r.Add("settled", 2)
	r.Add("reset", 1)

	want := []Count{{Key: "checked", N: 5}, {Key: "settled", N: 5}, {Key: "skipped", N: 0}, {Key: "reset", N: 1}}
	if got := r.Counts(); !slices.Equal(got, want) {
		t.Fatalf("Counts() = %v, want %v", got, want)
	}
	if n, ok := r.Get("skipped"); !ok || n != 0 {
		t.Errorf("Get(skipped) = %d, %v; want 0, true", n, ok)
	}
	if _, ok := r.Get("absent"); ok {
		t.Error("Get(absent) reported present")
	}

	// Counts is a copy: editing it leaves the Result alone.
	got := r.Counts()
	got[0].N = 99
	if n, _ := r.Get("checked"); n != 5 {
		t.Errorf("editing Counts() changed the Result: checked = %d", n)
	}

	var zero Result
	zero.Add("checked", 1)
	if n, ok := zero.Get("checked"); !ok || n != 1 {
		t.Errorf("zero Result Add: got %d, %v", n, ok)
	}
}

func TestResultCheck(t *testing.T) {
	info := validInfo("tier-unknown")
	cases := []struct {
		name string
		res  func() Result
		ok   bool
	}{
		{"empty", func() Result { return Result{} }, true},
		{"declared", func() Result { r := NewResult(info.Counts...); r.Add("settled", 4); return r }, true},
		{"undeclared", func() Result { r := NewResult(info.Counts...); r.Add("other", 1); return r }, false},
		{"path as key", func() Result { var r Result; r.Add("/music/x.flac", 1); return r }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.res().Check(info)
			if c.ok && err != nil {
				t.Fatalf("Check() = %v, want nil", err)
			}
			if !c.ok {
				if !errors.Is(err, ErrInvalidResult) {
					t.Fatalf("Check() = %v, want ErrInvalidResult", err)
				}
				if strings.Contains(err.Error(), "music") || strings.Contains(err.Error(), "other") {
					t.Errorf("Check() error echoes the offending key: %v", err)
				}
			}
		})
	}
}

func TestOptionsReport(t *testing.T) {
	Options{}.Report(1, 2) // nil Progress must not panic

	var gotDone, gotTotal int64
	o := Options{Progress: func(d, n int64) { gotDone, gotTotal = d, n }}
	o.Report(3, 10)
	if gotDone != 3 || gotTotal != 10 {
		t.Errorf("Report forwarded (%d, %d), want (3, 10)", gotDone, gotTotal)
	}
}

func TestCheckOptions(t *testing.T) {
	cases := []struct {
		name        string
		needsBackup bool
		opts        Options
		wantErr     bool
	}{
		{"dry run, needs backup, none given", true, Options{}, false},
		{"apply, needs backup, none given", true, Options{Apply: true}, true},
		{"apply, needs backup, given", true, Options{Apply: true, Backup: "b.jsonl"}, false},
		{"apply, no backup needed", false, Options{Apply: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := validInfo("tier-unknown")
			info.NeedsBackup = c.needsBackup
			err := CheckOptions(info, c.opts)
			if c.wantErr != errors.Is(err, ErrBackupRequired) {
				t.Fatalf("CheckOptions() = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("CheckOptions() = %v, want nil", err)
			}
		})
	}
}

func TestRegistryRegister(t *testing.T) {
	first := fakeRepair{validInfo("tier-unknown")}
	cases := []struct {
		name    string
		add     Repair
		wantErr error
		wantLen int
	}{
		{"duplicate key", fakeRepair{Info{Key: "tier-unknown", Title: "Other"}}, ErrDuplicateKey, 1},
		{"second key", fakeRepair{validInfo("tag-reread")}, nil, 2},
		{"nil repair", nil, ErrNilRepair, 1},
		{"invalid info", fakeRepair{Info{Key: "Bad", Title: "x"}}, ErrInvalidInfo, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := NewRegistry()
			if err := g.Register(first); err != nil {
				t.Fatalf("first Register: %v", err)
			}
			err := g.Register(c.add)
			if c.wantErr == nil && err != nil {
				t.Fatalf("Register() = %v, want nil", err)
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("Register() = %v, want %v", err, c.wantErr)
			}
			if n := len(g.Keys()); n != c.wantLen {
				t.Errorf("len(Keys()) = %d, want %d", n, c.wantLen)
			}
			got, ok := g.Get("tier-unknown")
			if !ok || got.Info().Title != first.info.Title {
				t.Errorf("Get(tier-unknown) = %v, %v; the first registration must stay", got, ok)
			}
		})
	}
}

func TestRegistryLookup(t *testing.T) {
	var g Registry // the zero value is usable
	if _, ok := g.Get("tier-unknown"); ok {
		t.Fatal("empty registry reported a repair")
	}
	if len(g.Keys()) != 0 {
		t.Fatal("empty registry has keys")
	}
	for _, k := range []Key{"tier-unknown", "tag-reread", "detector-stats"} {
		if err := g.Register(fakeRepair{validInfo(k)}); err != nil {
			t.Fatalf("Register(%s): %v", k, err)
		}
	}
	want := []Key{"tier-unknown", "tag-reread", "detector-stats"}
	keys := g.Keys()
	if !slices.Equal(keys, want) {
		t.Fatalf("Keys() = %v, want registration order %v", keys, want)
	}
	keys[0] = "changed"
	if g.Keys()[0] != "tier-unknown" {
		t.Error("editing Keys() changed the registry")
	}
	r, ok := g.Get("tag-reread")
	if !ok || r.Info().Key != "tag-reread" {
		t.Errorf("Get(tag-reread) = %v, %v", r, ok)
	}
	if _, ok := g.Get("unknown"); ok {
		t.Error("Get(unknown) reported a repair")
	}
}

// TestNoPackageLevelRegistry: the registry is a value its owner injects; the
// only package-level variables, in any non-test file, are errors.New sentinels
// (or the blank identifier, as in a compile-time interface assertion).
func TestNoPackageLevelRegistry(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if id.Name == "_" {
						continue // a compile-time assertion holds no state
					}
					if i >= len(vs.Values) || !isErrorsNew(vs.Values[i]) {
						t.Errorf("%s: package-level var %s is not an errors.New sentinel; the registry must be an injected value", name, id.Name)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no package files parsed")
	}
}

// isErrorsNew reports whether e is a call to errors.New.
func isErrorsNew(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "errors" && sel.Sel.Name == "New"
}

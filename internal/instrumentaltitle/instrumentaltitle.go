// Package instrumentaltitle classifies a track's title and album for an
// instrumental qualifier such as "(Instrumental)" or "- Off Vocal" (#1145). It is
// pure: no I/O, no clock, no logging, and it imports nothing internal except
// normalize.
//
// Two invariants are load-bearing. The exclusion wins: a title naming a
// cappella, karaoke or a sing-along product ("Performance Tracks", "Minus
// One", "Sing-Along", "Play-Along", "Originally Performed By"), or an album
// naming karaoke or such a product ("Backing Tracks" too, there), in any
// spelling the exclusion tables know, is None whatever else it carries. An a
// cappella recording is vocals only, so its lyrics are correct, and a track sold
// for singing over must get its lyrics: that is what it is for. "Backing Track"
// in a title is the exception, an instrumental qualifier: there it labels a
// studio outtake, not a product. The tables deliberately over-match
// (a title about a chapel in a Romance language loses its verdict too), since a
// false None only preserves today's behavior while a false Strong costs a track
// its lyrics. And
// a Verdict never carries any part of the input: Family and Position are closed
// sets of constants, safe to count and print where a title is private.
package instrumentaltitle

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sydlexius/canticle/internal/normalize"
)

// Tier is how much a qualifier can be trusted, ordered None < Weak < Strong.
type Tier int

const (
	// None means no signal, an excluded (a cappella, karaoke or sing-along
	// product) title, or a karaoke or sing-along album.
	None Tier = iota
	// Weak is a signal that needs corroboration (the audio detector). Its
	// Position is descriptive only: a Weak verdict is not actionable on its own
	// in any position, bracket and suffix included.
	Weak
	// Strong is a per-track qualifier: a strong term alone in a bracket pair or
	// a dash-suffix, beside packaging words only, qualifying a real title, with
	// no other qualifier segment contradicting it.
	Strong
)

// String returns the stable lower-case name of the tier, for counts and logs.
func (t Tier) String() string {
	switch t {
	case None:
		return "none"
	case Weak:
		return "weak"
	case Strong:
		return "strong"
	}
	return "Tier(" + strconv.Itoa(int(t)) + ")"
}

// Family names which term matched. The set is closed.
type Family string

// Families. FamilyInst covers "(Inst)" and its katakana form; FamilyNoVocal
// covers "no vocal(s)" and "without vocal(s)"; FamilyArrangement is
// "orchestral version" / "piano version".
const (
	FamilyNone         Family = ""
	FamilyInstrumental Family = "instrumental"
	FamilyInst         Family = "inst"
	FamilyBackingTrack Family = "backing_track"
	FamilyBGM          Family = "bgm"
	FamilyOffVocal     Family = "off_vocal"
	FamilyMR           Family = "mr"
	FamilyNoVocal      Family = "no_vocal"
	FamilyArrangement  Family = "arrangement"
	FamilyScore        Family = "score"
)

// Position names where the term sat. The set is closed.
type Position string

// Positions. PositionInline is anywhere in the title outside a clean qualifier
// segment; PositionAlbum means the title was clean and the album carried it.
//
// A Strong PositionSuffix verdict is actionable only after the caller has
// checked that the text before the separator is not the track's own artist:
// Classify never sees the artist.
const (
	PositionNone    Position = ""
	PositionBracket Position = "bracket"
	PositionSuffix  Position = "suffix"
	PositionInline  Position = "inline"
	PositionAlbum   Position = "album"
)

// Verdict is the classification of one title/album pair. The zero value is
// "no signal".
type Verdict struct {
	Tier     Tier
	Family   Family
	Position Position
}

// term is a token-sequence needle; each seq element is a set of alternatives.
// Strong terms come first so the reported family prefers them.
type term struct {
	family Family
	strong bool
	seq    [][]string
}

// kana is a substring needle (Japanese is not space-delimited). An occurrence
// directly followed by notBefore does not count. Longer needles come first.
type kana struct {
	family    Family
	needle    string
	notBefore string
}

// contra is a word that says the recording has vocals. hard ones count in any
// other qualifier segment, the rest only in another bracket pair (a dash-suffix
// is often a subtitle, where "sing" and "not" are ordinary words). sub needles
// match inside a token, for unspaced Japanese.
type contra struct {
	needle    string
	hard, sub bool
}

// Segment analysis is skipped above these bounds. No real title is this long or
// this bracketed, and the fallback (the inline pass) is Weak at most.
const (
	maxRunes = 1024
	maxSpans = 16
)

var (
	terms = []term{
		{FamilyInstrumental, true, [][]string{{"instrumental", "instrumentals"}}},
		{FamilyBackingTrack, true, [][]string{{"backing"}, {"track"}}},
		{FamilyBGM, true, [][]string{{"bgm"}}},
		{FamilyOffVocal, true, [][]string{{"off"}, {"vocal", "vocals"}}},
		{FamilyMR, true, [][]string{{"mr"}, {"ver", "version"}}},
		{FamilyNoVocal, true, [][]string{{"no", "without"}, {"vocal", "vocals"}}},
		{FamilyArrangement, false, [][]string{{"orchestral", "piano"}, {"ver", "version"}}},
		{FamilyScore, false, [][]string{{"score"}}},
	}
	// The trailing long-vowel mark makes the katakana abbreviation the start of
	// the loanword for "install", which is not a signal.
	kanas = []kana{
		{FamilyInstrumental, "インストゥルメンタル", ""},
		{FamilyInstrumental, "インストルメンタル", ""},
		{FamilyOffVocal, "オフボーカル", ""},
		{FamilyOffVocal, "オフ・ボーカル", ""},
		{FamilyOffVocal, "オフ ボーカル", ""},
		{FamilyOffVocal, "オフヴォーカル", ""},
		{FamilyOffVocal, "オフ・ヴォーカル", ""},
		{FamilyOffVocal, "オフ ヴォーカル", ""},
		{FamilyInst, "インスト", "ー"},
	}
	// packaging is the closed vocabulary (plus a year, 1900-2099) allowed beside
	// a strong term in a Strong segment. "tv size" alone is therefore no signal.
	packaging = []string{
		"version", "ver", "mix", "edit", "original", "album", "single", "track", "tv", "size",
		"バージョン", "ヴァージョン", "版",
	}
	// negations are a contradiction only inside a segment that also holds a
	// term ("(Not Instrumental)"); elsewhere they are ordinary words.
	negations = []string{"not", "non"}
	contras   = []contra{
		{"vocal", true, false}, {"vocals", true, false}, {"vox", true, false}, {"sung", true, false},
		{"ボーカル", true, true}, {"ヴォーカル", true, true}, {"歌入", true, true}, {"歌あり", true, true},
		{"guide", false, false}, {"chorus", false, false}, {"sing", false, false}, {"singalong", false, false},
		{"lyric", false, false}, {"lyrics", false, false}, {"hook", false, false}, {"voice", false, false},
		{"singer", false, false}, {"rap", false, false},
		{"コーラス", false, true}, {"ガイド", false, true},
	}
	instToken = "inst"

	// The exclusions. Over-matching is the safe direction: it
	// withholds a verdict. Stems match anywhere in the text; glued needles match
	// after everything but letters and digits is removed, so a middle dot or a
	// space inside the word does not hide it; whole tokens are the abbreviation.
	// Two glued rows mean "unaccompanied", which also names solo-instrument
	// works: those lose their verdict too, by choice. The karaoke stems cover the
	// common misspellings.
	aCappellaStems = []string{"capel", "cappel", "kapel", "kappel", "капелл", "акапел"}
	aCappellaGlued = []string{
		"アカペラ", "アカペッラ", "アカッペラ", "あかぺら", "아카펠라", "阿卡贝拉", "阿卡貝拉", "清唱", "無伴奏", "无伴奏",
	}
	aCappellaTokens = []string{"acap", "acaps"}
	// A spaced Cyrillic a cappella is the particle plus a word on this stem; the
	// bare stem is not excluded because it is also a common unrelated word.
	aCappellaParticle, aCappellaAfter = "\u0430", "капел"
	karaokeStems                      = []string{"karaok", "kareok", "karoke", "karoak", "караоке"}
	karaokeGlued                      = []string{"カラオケ", "からおけ", "가라오케", "卡拉ok"}
	// The sing-along products and the karaoke label phrases, excluded exactly
	// as karaoke is: a recording sold for singing or playing over. Each row is a
	// phrase, one set of alternatives per word. Only the product form counts,
	// the words directly adjacent (see adjacent) or glued into one token
	// ("splittrax", "singalong"): "Performance", "Split", "Accompaniment" and
	// "Sing" on their own are ordinary words.
	//
	// The sing-along phrases below count in a title or an album.
	singAlongSet = phraseSet{rows: [][][]string{
		{{"accompaniment", "performance", "split"}, {"track", "tracks", "trax"}},
		{{"minus"}, {"one", "1"}},
		{{"sing", "play"}, {"along", "alongs"}},
		{{"sing", "play"}, {"a"}, {"long", "longs"}},
		{{"originally"}, {"performed"}, {"by"}},
		{{"in"}, {"the"}, {"style"}, {"of"}},
	}}
	// albumPhrases count in an album name only. On a release, "Backing Tracks"
	// names a sing-along product. In a track title, "Backing Track" labels a
	// studio outtake that really is instrumental, so there it stays the
	// backing_track qualifier in terms.
	albumPhrases = phraseSet{rows: [][][]string{
		{{"backing"}, {"track", "tracks", "trax"}},
	}}

	yearToken = regexp.MustCompile(`^(19|20)[0-9]{2}$`)
	// dressing is a token that numbers a track rather than names it: digits, a
	// vinyl side with or without its position, or a numbering word with or
	// without its number.
	dressing = regexp.MustCompile(`^([0-9]+|[a-h][0-9]{0,2}|(track|tr|cd|disc|disk|no|vol|side|pt|part)[0-9]*)$`)
	// dotted is a run of dotted initials ("b.g.m.", "m.r."), read as one word.
	dotted       = regexp.MustCompile(`\b(?:[a-z]\.)+[a-z]\b\.?`)
	packagingSet = map[string]bool{}

	closers = map[rune]rune{
		'(': ')', '[': ']', '{': '}', '【': '】', '〔': '〕', '〈': '〉', '《': '》', '「': '」', '『': '』',
	}
	closerSet = map[rune]bool{}
)

// init folds every needle through normalize.NormalizeKey, the same function
// Classify applies to its input. The fold strips the voicing marks from
// katakana, so a literal needle would never match a folded title.
func init() {
	fold := func(list []string) {
		for i := range list {
			list[i] = normalize.NormalizeKey(list[i])
		}
	}
	for _, t := range terms {
		for _, alts := range t.seq {
			fold(alts)
		}
	}
	for i := range kanas {
		kanas[i].needle = normalize.NormalizeKey(kanas[i].needle)
		kanas[i].notBefore = normalize.NormalizeKey(kanas[i].notBefore)
	}
	for i := range contras {
		contras[i].needle = normalize.NormalizeKey(contras[i].needle)
	}
	for _, p := range packaging {
		packagingSet[normalize.NormalizeKey(p)] = true
	}
	for _, list := range [][]string{aCappellaStems, aCappellaGlued, aCappellaTokens, karaokeStems, karaokeGlued} {
		fold(list)
	}
	singAlongSet.build(fold)
	albumPhrases.build(fold)
	instToken = normalize.NormalizeKey(instToken)
	for _, c := range closers {
		closerSet[c] = true
	}
}

// phraseSet is a table of product phrases and every phrase written as one
// glued token ("splittrax", "singalong").
type phraseSet struct {
	rows  [][][]string
	glued map[string]bool
}

// build folds the rows and fills the glued set; init calls it once.
func (ps *phraseSet) build(fold func([]string)) {
	ps.glued = map[string]bool{}
	for _, phrase := range ps.rows {
		joined := []string{""}
		for _, alts := range phrase {
			fold(alts)
			var next []string
			for _, head := range joined {
				for _, alt := range alts {
					next = append(next, head+alt)
				}
			}
			joined = next
		}
		for _, j := range joined {
			ps.glued[j] = true
		}
	}
}

// matches reports whether a phrase of the set starts at ws[i], or ws[i] is a
// glued phrase.
func (ps *phraseSet) matches(s string, ws []word, i int) bool {
	if ps.glued[ws[i].text] {
		return true
	}
	return slices.ContainsFunc(ps.rows, func(phrase [][]string) bool { return phraseAt(s, ws, i, phrase) })
}

// prepare normalizes one field and joins dotted initials.
func prepare(s string) string {
	return dotted.ReplaceAllStringFunc(normalize.NormalizeKey(s), func(m string) string {
		return strings.ReplaceAll(m, ".", "")
	})
}

// Classify reports the instrumental signal carried by a title and its album.
// The title decides alone when it carries anything; a clean title falls back to
// the album, whose signal is always Weak. An a cappella, karaoke or sing-along
// title is None outright, and so is any track on a karaoke or sing-along album:
// such a product is sold for its lyrics, whatever its titles say, a "(Backing
// Track)" title included. An a cappella album cancels only the album's own
// signal.
func Classify(title, album string) Verdict {
	t, a := prepare(title), prepare(album)
	titleOut, _ := excluded(t, false)
	albumOut, singAlongAlbum := excluded(a, true)
	if titleOut || singAlongAlbum {
		return Verdict{}
	}
	if v := classifyField(t); v.Tier != None {
		return v
	}
	if albumOut {
		return Verdict{}
	}
	if v := classifyField(a); v.Tier != None {
		return Verdict{Tier: Weak, Family: v.Family, Position: PositionAlbum}
	}
	return Verdict{}
}

// tokens splits on everything that is not a letter or digit, so
// "instrumentalist" is one token and never the term.
func tokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// word is one token of a field with its byte offsets, so the text between two
// tokens can be read.
type word struct {
	text       string
	start, end int
}

// words is tokens with offsets.
func words(s string) (out []word) {
	start := -1
	for i, r := range s {
		in := unicode.IsLetter(r) || unicode.IsDigit(r)
		if in && start < 0 {
			start = i
		} else if !in && start >= 0 {
			out, start = append(out, word{s[start:i], start, i}), -1
		}
	}
	if start >= 0 {
		out = append(out, word{s[start:], start, len(s)})
	}
	return out
}

// adjacent is the one rule for what may stand between the words of a product
// phrase: whitespace only, or a single dash of any kind (see isDash, plus the
// non-breaking hyphen) with no space around it. Any other punctuation ends the phrase, so "Live Performance - Track 3" and
// "Performance, Track 2" are not products.
func adjacent(gap string) bool {
	if r, n := utf8.DecodeRuneInString(gap); n == len(gap) && n > 0 && (isDash(r) || r == '\u2011') {
		return true
	}
	return (gap != "" && strings.TrimFunc(gap, unicode.IsSpace) == "")
}

// phraseAt reports whether the phrase starts at ws[i], its words adjacent.
func phraseAt(s string, ws []word, i int, phrase [][]string) bool {
	if i+len(phrase) > len(ws) {
		return false
	}
	for j, alts := range phrase {
		if !slices.Contains(alts, ws[i+j].text) || (j > 0 && !adjacent(s[ws[i+j-1].end:ws[i+j].start])) {
			return false
		}
	}
	return true
}

// excluded is the a cappella, karaoke and sing-along check: out for any of
// them, singAlong for karaoke, the sing-along products and the karaoke label
// phrases. album says the field is an album name, where the album-only phrases
// count as well. It runs on the whole field, whatever its length. Format runes
// (zero-width space, soft hyphen) are dropped first so they cannot split the
// word.
func excluded(s string, album bool) (out, singAlong bool) {
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	glued := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, s)
	has := func(text string, needles []string) bool {
		return slices.ContainsFunc(needles, func(n string) bool { return strings.Contains(text, n) })
	}
	if has(s, karaokeStems) || has(glued, karaokeGlued) {
		return true, true
	}
	ws := words(s)
	for i := range ws {
		if singAlongSet.matches(s, ws, i) || (album && albumPhrases.matches(s, ws, i)) {
			return true, true
		}
	}
	if has(s, aCappellaStems) || has(glued, aCappellaGlued) {
		return true, false
	}
	for i, w := range ws {
		if slices.Contains(aCappellaTokens, w.text) {
			return true, false
		}
		if w.text == aCappellaParticle && i+1 < len(ws) && strings.HasPrefix(ws[i+1].text, aCappellaAfter) {
			return true, false
		}
	}
	return false, false
}

// span is one matched bracket pair, as rune indexes of its two delimiters.
type span struct{ open, close int }

// bracketSpans pairs brackets with a stack. ok is false when the structure is
// not well-formed (an unclosed opener, a stray closer, or a crossed pair): such
// a field has no qualifier segment at all, bracket or suffix, so it is Weak at
// most.
func bracketSpans(rs []rune) (out []span, ok bool) {
	if len(rs) > maxRunes {
		return nil, false
	}
	var stack []int
	for i, r := range rs {
		if _, opener := closers[r]; opener {
			stack = append(stack, i)
		} else if closerSet[r] {
			n := len(stack)
			if n == 0 || closers[rs[stack[n-1]]] != r {
				return nil, false
			}
			out = append(out, span{stack[n-1], i})
			stack = stack[:n-1]
		}
	}
	return out, len(stack) == 0 && len(out) <= maxSpans
}

// coverage marks every rune inside a bracket pair, delimiters included.
func coverage(rs []rune, spans []span) []bool {
	covered := make([]bool, len(rs))
	for _, sp := range spans {
		for i := sp.open; i <= sp.close; i++ {
			covered[i] = true
		}
	}
	return covered
}

func isTilde(r rune) bool { return r == '~' || r == '〜' }

// isDash covers hyphen-minus, hyphen, en dash, em dash, horizontal bar, minus
// and the box-drawing horizontal.
func isDash(r rune) bool {
	return r == '-' || r == '‐' || r == '–' || r == '—' || r == '―' || r == '−' || r == '─'
}

// dashSuffix finds the last separator outside every bracket pair: the prefix is
// rs[:sep] and the suffix rs[sep+1:end]. A dash needs whitespace on its left,
// so a hyphenated name is not a separator; a tilde or wave dash does not.
// Trailing tildes are dropped first: they close a "~qualifier~" pair rather
// than open an empty suffix.
func dashSuffix(rs []rune, covered []bool) (sep, end int, ok bool) {
	end = len(rs)
	for end > 0 && (isTilde(rs[end-1]) || unicode.IsSpace(rs[end-1])) {
		end--
	}
	for i := end - 1; i >= 0; i-- {
		if covered[i] {
			continue
		}
		if isTilde(rs[i]) || (isDash(rs[i]) && i > 0 && unicode.IsSpace(rs[i-1])) {
			return i, end, true
		}
	}
	return 0, 0, false
}

// cut replaces each real occurrence of the needle with a space. An occurrence
// that starts the excluded longer word is left in place, where it reads as an
// ordinary (non-packaging) token.
func cut(text string, k kana) (out string, hit bool) {
	var b strings.Builder
	for {
		i := strings.Index(text, k.needle)
		if i < 0 {
			break
		}
		b.WriteString(text[:i])
		text = text[i+len(k.needle):]
		if k.notBefore != "" && strings.HasPrefix(text, k.notBefore) {
			b.WriteString(k.needle + k.notBefore)
			text = text[len(k.notBefore):]
			continue
		}
		b.WriteByte(' ')
		hit = true
	}
	return b.String() + text, hit
}

// found is what scan saw in one stretch of text. strong: a strong term matched.
// clean: every token left over is packaging vocabulary or a year. named: some
// left-over token could name a song (not packaging, not track numbering).
// hard/soft: a left-over token says the recording has vocals.
type found struct {
	fam                              Family
	strong, clean, named, hard, soft bool
}

// realBase reports whether text names something: a track number alone does
// not, so "03 - Off Vocal" is a song named Off Vocal, not a qualified title.
func realBase(text string) bool {
	return slices.ContainsFunc(tokens(text), func(tok string) bool { return !dressing.MatchString(tok) })
}

// scan looks for terms in one stretch of normalized text. The bare token "inst"
// is a term only when segment is set and it leaves the segment clean: inline it
// abbreviates too many words.
func scan(text string, segment bool) (f found) {
	note := func(fam Family, s bool) {
		if f.fam == FamilyNone {
			f.fam = fam
		}
		f.strong = f.strong || s
	}
	for _, k := range kanas {
		var hit bool
		if text, hit = cut(text, k); hit {
			note(k.family, true)
		}
	}
	toks := tokens(text)
	used := make([]bool, len(toks))
	for _, t := range terms {
	next:
		for i := 0; i+len(t.seq) <= len(toks); i++ {
			for j, alts := range t.seq {
				if !slices.Contains(alts, toks[i+j]) {
					continue next
				}
			}
			for j := range t.seq {
				used[i+j] = true
			}
			note(t.family, t.strong)
		}
	}
	inst, negated := false, false
	f.clean = true
	for i, tok := range toks {
		switch {
		case used[i] || packagingSet[tok] || yearToken.MatchString(tok):
			continue
		case tok == instToken:
			inst = true
			continue
		}
		f.clean = false
		f.named = f.named || !dressing.MatchString(tok)
		negated = negated || slices.Contains(negations, tok)
		for _, c := range contras {
			if tok == c.needle || (c.sub && strings.Contains(tok, c.needle)) {
				f.hard = f.hard || c.hard
				f.soft = f.soft || !c.hard
			}
		}
	}
	if inst && segment && f.clean {
		note(FamilyInst, true)
	} else if inst {
		f.clean = false
	}
	f.soft = f.soft || (negated && f.fam != FamilyNone)
	return f
}

// classifyField classifies one normalized field on its own. A qualifier segment
// is Strong only when all of these hold; otherwise a term in it is Weak:
//
//   - it is clean (the term beside packaging words only);
//   - it qualifies a real title: text outside every bracket pair that is more
//     than a track number, or an earlier bracket pair that holds a name and no
//     term (a title written in quotes). A field that is nothing but the
//     segment is a song named with the word;
//   - no other qualifier segment says the recording has vocals. The title
//     itself is not consulted, so a song named with "sing" is unaffected.
//
// The first segment to reach the best tier wins; the inline pass over the whole
// field is the Weak fallback, and the only pass an oversized or malformed field
// gets.
func classifyField(s string) Verdict {
	var best Verdict
	consider := func(f found, pos Position, ok bool) {
		tier := None
		switch {
		case f.strong && f.clean && ok:
			tier = Strong
		case f.fam != FamilyNone:
			tier = Weak
		}
		if tier > best.Tier {
			best = Verdict{Tier: tier, Family: f.fam, Position: pos}
		}
	}
	rs := []rune(s)
	if spans, ok := bracketSpans(rs); ok {
		covered := coverage(rs, spans)
		var base strings.Builder
		for i, r := range rs {
			if !covered[i] {
				base.WriteRune(r)
			}
		}
		hasBase := realBase(base.String())
		// quoted is where the first bracket pair holding a name and no term ends.
		segs, quoted, vocal := make([]found, len(spans)), len(rs), false
		for i, sp := range spans {
			segs[i] = scan(string(rs[sp.open+1:sp.close]), true)
			vocal = vocal || segs[i].hard || segs[i].soft
			if segs[i].fam == FamilyNone && segs[i].named {
				quoted = min(quoted, sp.close)
			}
		}
		sep, end, hasSuffix := dashSuffix(rs, covered)
		var suffix found
		if hasSuffix {
			suffix = scan(string(rs[sep+1:end]), true)
			vocal = vocal || suffix.hard
		}
		for i, sp := range spans {
			consider(segs[i], PositionBracket, !vocal && (hasBase || quoted < sp.open))
		}
		if hasSuffix {
			consider(suffix, PositionSuffix, !vocal && realBase(string(rs[:sep])))
		}
	}
	consider(scan(s, false), PositionInline, false)
	return best
}

// KaraokeExcluded reports whether Classify withholds its verdict because the
// title or the album names karaoke or a sing-along product (the name predates
// the products; they are excluded by the same path). "Backing Track" counts in
// the album only. Classify returns the same
// None for that as for "no signal"; a caller that must tell them apart (to keep
// an audio detector from settling such a track ahead of the lyric providers)
// asks here, so the exclusion tables stay in one place.
func KaraokeExcluded(title, album string) bool {
	_, t := excluded(prepare(title), false)
	_, a := excluded(prepare(album), true)
	return t || a
}

// SuffixBaseWithin reports whether the text before title's dash-suffix
// separator could be name: the two are equal, or either holds the other as a
// contiguous run of tokens. It is the check PositionSuffix asks of the caller:
// "Artist - Off Vocal" is a song named Off Vocal whose title tag carries the
// artist, not a qualified title.
//
// Both sides are compared as normalized tokens (case, width, diacritics and
// dotted initials folded), with "&", "&amp;" and "and" as one token and track
// numbering around the prefix ignored. Numbers are never stripped before the
// first comparison, so a name that starts with a number still matches.
// Containment runs in BOTH directions: the prefix inside the name covers a
// multi-artist tag, and the name inside the prefix covers a title tag that
// credits more than the tag does ("Artist feat. Guest - Off Vocal"). A leading
// "The" on either side needs no rule of its own: the shorter side is then a run
// inside the longer one.
//
// The trade is deliberate: a genuine "Artist - Title - Instrumental" title is
// true here as well, so the caller declines it and the track is simply fetched
// like any other. Every doubtful case resolves toward true, which only ever
// withholds a verdict. What it cannot see is a respelling in another script (a
// katakana prefix against a romanized tag): that stays false, an accepted
// residual. It is false for a name with no tokens or a title with no separator.
func SuffixBaseWithin(title, name string) bool {
	want := tokens(nameText(name))
	rs := []rune(nameText(title))
	spans, ok := bracketSpans(rs)
	if len(want) == 0 || !ok {
		return false
	}
	covered := coverage(rs, spans)
	sep, _, ok := dashSuffix(rs, covered)
	if !ok {
		return false
	}
	// Judged with and without bracketed text: either reading may be the name.
	outside := slices.Clone(rs[:sep])
	for i := range outside {
		if covered[i] {
			outside[i] = ' '
		}
	}
	for _, base := range [][]rune{rs[:sep], outside} {
		have := tokens(string(base))
		// As written first, so a name that is itself numbering still matches;
		// then with the track numbering around it dropped.
		lo, hi := 0, len(have)
		for lo < hi && dressing.MatchString(have[lo]) {
			lo++
		}
		for lo < hi && dressing.MatchString(have[hi-1]) {
			hi--
		}
		for _, h := range [][]string{have, have[lo:hi]} {
			if runWithin(h, want) || runWithin(want, h) {
				return true
			}
		}
	}
	return false
}

// PlaceholderArtist reports whether name is no artist at all: empty, or one of
// the fillers a tagger writes when it has none ("Various Artists", "VA",
// "V.A.", "Unknown Artist", "Unknown"). A caller of SuffixBaseWithin must not
// count such a name as an artist it compared against: with no real artist known
// the dash-suffix check has not been made, and the answer is not to act.
func PlaceholderArtist(name string) bool {
	toks := tokens(prepare(name))
	return len(toks) == 0 || slices.Contains(placeholderArtists, strings.Join(toks, " "))
}

// Actionable reports the verdict of Classify(title, album) and whether the
// title rule may act on it. Only a Strong verdict is acted on, and a bracketed
// one needs nothing more. A Strong dash-suffix is acted on only once the text
// before the separator is known not to be the track's artist ("Artist - Off
// Vocal" is a song named Off Vocal whose title tag carries the artist), which
// Classify cannot tell because it never sees the artist. Both tagged artists
// are compared, the track artist and the album artist: a compilation carries
// the performer in one and someone else in the other. (The resolved artist the
// providers are asked for is always one of those two, so it is not a third
// name.) With no real artist to compare against (none, or only a placeholder
// such as "Various Artists") the answer is no: doubt withholds the verdict.
//
// Decide on ok alone. v is Classify's verdict unchanged, for reporting only: a
// Strong v with ok false is a title the rule declines. Two callers agree only
// when they pass the same inputs, so album must be the one the worker would
// see (the audio file's tag where it re-reads it; a karaoke album turns a
// Strong title into None), and whether the rule is on at all is the caller's
// check.
func Actionable(title, album, artist, albumArtist string) (v Verdict, ok bool) {
	v = Classify(title, album)
	if v.Tier != Strong {
		return v, false
	}
	if v.Position != PositionSuffix {
		return v, true
	}
	known := false
	for _, name := range []string{artist, albumArtist} {
		if PlaceholderArtist(name) {
			continue
		}
		known = true
		if SuffixBaseWithin(title, name) {
			return v, false
		}
	}
	return v, known
}

// placeholderArtists are PlaceholderArtist's fillers, as joined tokens.
var placeholderArtists = []string{"various artists", "various", "va", "unknown artist", "unknown"}

// ampersand folds every spelling of "&" to the word, spaced so it is a token.
// It runs on prepared text: lower case, and a full-width sign already folded.
var ampersand = strings.NewReplacer("&amp;", " and ", "&", " and ")

// nameText is prepare for a name comparison: the ampersand becomes its word.
func nameText(s string) string { return ampersand.Replace(prepare(s)) }

// runWithin reports whether the non-empty run occurs contiguously in in.
func runWithin(run, in []string) bool {
	for i := 0; len(run) > 0 && i+len(run) <= len(in); i++ {
		if slices.Equal(in[i:i+len(run)], run) {
			return true
		}
	}
	return false
}

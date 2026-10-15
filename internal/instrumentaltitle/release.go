package instrumentaltitle

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sydlexius/canticle/internal/normalize"
)

// The tables of InstrumentalRelease. A wrong yes would one day cost every track
// of a release its lyrics, so each table only withholds the answer and each
// over-matches by choice.
var (
	// releaseMarkers is every text an album's closing marker may hold, whole:
	// one more word and it names something else ("Instrumental Demos", "Deluxe /
	// Instrumental"). releaseOpeners are the only bracket pairs that may hold it
	// (a full-width pair folds to these). scoreStem anywhere in the album
	// withholds the answer: a score album can be sung.
	releaseMarkers = []string{
		"instrumental", "instrumentals", "instrumental version", "instrumental edition", "instrumental album",
	}
	releaseOpeners = "(["
	scoreStem      = "score"
	// The voice tables (voiced). voiceWords are whole tokens: a spoken or sung
	// part, or another version of the song. A token holding a voiceStem, or
	// starting with a voiceHead, counts too: the heads are prefix-only because
	// "sing" sits inside every "-sing" gerund ("Rising") and "sung" inside
	// "Unsung". voiceGlued match anywhere in the text, for scripts written
	// without spaces. No kana needle is among them: the fold strips the voicing
	// marks, which turns a kana word into another word.
	voiceWords = []string{
		"voc", "live", "demo", "demos", "remix", "cover", "acoustic", "bonus", "rap", "raps", "rapped", "rapper",
	}
	voiceStems = []string{
		"vocal", "lyric", "voice", "choir", "choral", "chorus", "duet", "narrat", "speech", "spoken", "skit",
	}
	voiceHeads = []string{"sing", "sang", "sung"}
	voiceGlued = []string{"歌", "唱", "보컬", "라이브"}
	// A guest credit (credited): a creditWord anywhere, a creditJoin between two
	// words, or a creditLead followed by a word.
	creditWords = []string{"feat", "featuring", "ft", "f", "w"}
	creditJoins = []string{"x", "vs"}
	creditLeads = []string{"with", "con", "avec", "mit"}
	// segmentWords are the only words a bracket pair or a dash-suffix of the
	// title may hold, beside a year, "version" after "album", and a number after
	// a part word. A qualifier is where a sung version is named, in more words
	// than any deny-list holds, so there the list is of what may stay.
	segmentWords = []string{
		"remaster", "remastered", "remasters", "reprise", "album", "interlude", "intro", "outro", "prelude", "mono", "stereo",
	}
	partWords  = []string{"pt", "part", "parts"}
	partNumber = regexp.MustCompile(`^([0-9]{1,3}|[ivx]{1,4})$`)
	// A list-shaped album base ("A / A (Instrumental)", "A and A (Instrumental)")
	// is a single or a split release: the marker names its last item only.
	listMarks = "/+&;,|、"
	listWords = []string{"and", "plus", "bw", "vs", "with"}
	// The sing-over products the exclusion does not cover. Either set in an
	// album name withholds the answer; coverSet in a title does too. A cast
	// album or a musical's instrumental is sold to sing over, so it keeps its
	// lyrics as a sing-along does (#1515): "cast", "broadway" and "musical" are
	// whole words, so "Broadcast", "Castle" and "Musically" do not count.
	coverSet    = phraseSet{rows: [][][]string{{{"made"}, {"famous"}, {"by"}}, {{"tribute"}, {"to"}}}}
	singOverSet = phraseSet{rows: [][][]string{
		{{"accompaniment"}}, {{"playback", "playbacks"}}, {{"trax"}}, {{"for"}, {"singers"}},
		{{"cast"}, {"recording"}}, {{"cover"}, {"version", "versions"}},
		{{"cast"}}, {{"broadway"}}, {{"musical", "musicals"}},
	}}
)

// init folds the tables through normalize.NormalizeKey, as the package's other
// init does for its own (the fold strips the voicing marks from katakana).
func init() {
	fold := func(list []string) {
		for i := range list {
			list[i] = normalize.NormalizeKey(list[i])
		}
	}
	for _, list := range [][]string{
		releaseMarkers, voiceWords, voiceStems, voiceHeads, voiceGlued, creditWords, creditJoins, creditLeads,
		segmentWords, partWords, listWords,
	} {
		fold(list)
	}
	coverSet.build(fold)
	singOverSet.build(fold)
}

// PositionRelease is the position of a verdict the album decided: a plain
// title on an instrumental release (InstrumentalRelease). Only Settles returns
// it. It is not PositionAlbum, which is Classify's Weak album signal and is
// never acted on.
const PositionRelease Position = "release"

// Settles reports the verdict a caller may act on for a track, and whether it
// may act. It is Actionable, plus the album-level verdict: when the title rule
// declines and the track is a plain title on an instrumental release
// (InstrumentalRelease), the answer is Strong FamilyInstrumental at
// PositionRelease, and ok.
//
// The title decides first. A verdict from the title keeps its own family and
// position, and a title the release rule withholds (one that carries any signal
// of its own, Weak or a declined Strong included, or names a voice or another
// version) gets exactly what Actionable gives. Karaoke and a sing-along
// product, in either field, and an a cappella title win over both. An a
// cappella album withholds only the album-level verdict: a Strong title on it
// still settles, as Classify lets such an album cancel only its own signal. A
// score album is never a release: some are sung.
//
// ok says the rule may act. For a PositionRelease verdict the caller must also
// hold it to a row that is that release's own (the edition row of #1525): the
// queue row, the lyric cache and the lyric block are keyed by artist and
// title, so the sung twin shares them otherwise. Every other verdict is
// decided on ok alone, as with Actionable. Classify, Actionable and
// KaraokeExcluded do not know this rule.
func Settles(title, album, artist, albumArtist string) (v Verdict, ok bool) {
	if v, ok = Actionable(title, album, artist, albumArtist); ok {
		return v, true
	}
	if InstrumentalRelease(title, album, artist, albumArtist) {
		return Verdict{Tier: Strong, Family: FamilyInstrumental, Position: PositionRelease}, true
	}
	return v, false
}

// InstrumentalRelease reports whether a track is a plain-titled track on a
// release whose album name ends in an instrumental marker, such as
// "Low Tide (Instrumental)" or "Low Tide - Instrumentals".
//
// Settles turns the answer into a verdict; this is the count-only form the
// title repair's dry run reads. Classify and Actionable do not know this rule.
//
// Every doubt is a no. All of these must hold:
//
//   - neither field is excluded (a cappella, karaoke, a sing-along product);
//   - the title carries no instrumental signal of its own (Classify(title, "")
//     is None) and may not name a sung version (releaseTitle);
//   - the album ends in a marker and its base passes releaseMarked;
//   - neither artist tag carries a guest credit (credited): a guest is a voice.
//     A band named with "&" or "and" is not a credit;
//   - no artist tag could be the text before a dash in the album name
//     (SuffixBaseWithin): "Artist - Instrumental" is an album named with the
//     word. When the marker itself follows a dash, a real artist must be known
//     for that comparison to have been made (PlaceholderArtist); the bracket
//     form needs none.
func InstrumentalRelease(title, album, artist, albumArtist string) bool {
	t, a := prepare(title), prepare(album)
	titleOut, _ := excluded(t, false)
	albumOut, _ := excluded(a, true)
	if titleOut || albumOut || classifyField(t).Tier != None || !releaseTitle(t) {
		return false
	}
	base, dash, ok := releaseShape(a)
	if !ok || !releaseMarked(a, base) {
		return false
	}
	known := !dash
	for _, name := range []string{artist, albumArtist} {
		if credited(guardTokens(stripFormat(prepare(name)))) {
			return false
		}
		if PlaceholderArtist(name) {
			continue
		}
		known = true
		if SuffixBaseWithin(album, name) {
			return false
		}
	}
	return known
}

// in reports whether a phrase of the set occurs anywhere in s.
func (ps *phraseSet) in(s string) bool {
	ws := words(s)
	for i := range ws {
		if ps.matches(s, ws, i) {
			return true
		}
	}
	return false
}

// releaseShape splits an album name that ends in an instrumental marker into
// what precedes the marker and whether the marker follows a dash. All of these
// must hold; the bare trailing word ("Low Tide Instrumentals") and the word
// inline are never a marker:
//
//   - the marker is the last thing in the name: one "(...)" or "[...]" pair
//     closing it, or the text after the last dash (isDash, whitespace on its
//     left, as for a title's dash-suffix; a tilde is not a dash here);
//   - the marker's whole content is one of releaseMarkers, its words apart by
//     whitespace only;
//   - the name holds no other bracket of any kind, and its brackets are
//     well-formed.
func releaseShape(album string) (base string, dash, ok bool) {
	rs := []rune(strings.TrimRightFunc(album, unicode.IsSpace))
	spans, wellFormed := bracketSpans(rs)
	if !wellFormed || len(spans) > 1 {
		return "", false, false
	}
	var lo int
	hi := len(rs)
	if len(spans) == 1 {
		lo, hi = spans[0].open, spans[0].close
		if hi != len(rs)-1 || !strings.ContainsRune(releaseOpeners, rs[lo]) {
			return "", false, false
		}
	} else {
		sep, _, has := dashSuffix(rs, make([]bool, len(rs)))
		if !has || !isDash(rs[sep]) {
			return "", false, false
		}
		lo, dash = sep, true
	}
	marker := strings.Join(strings.Fields(string(rs[lo+1:hi])), " ")
	return string(rs[:lo]), dash, slices.Contains(releaseMarkers, marker)
}

// releaseMarked reports whether an album of the release shape, with the given
// base, marks the whole release instrumental. The base must name something
// (realBase), must not read as a list of items (listMarks, listWords) and must
// hold no word of a voice (voiced). The name holds "score" nowhere and no
// sing-over product (coverSet, singOverSet). Format runes are dropped before
// those word checks, as excluded drops them, so they cannot split a word.
func releaseMarked(album, base string) bool {
	whole := stripFormat(album)
	if strings.Contains(whole, scoreStem) || coverSet.in(whole) || singOverSet.in(whole) {
		return false
	}
	base = stripFormat(base)
	listed := strings.ContainsAny(base, listMarks) ||
		slices.ContainsFunc(tokens(base), func(tok string) bool { return slices.Contains(listWords, tok) })
	return realBase(base) && !listed && !voiced(base)
}

// stripFormat drops the format runes (zero-width space, soft hyphen), which
// would otherwise split a word in two.
func stripFormat(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

// guardTokens is tokens, each split again where Latin text meets another
// script, so a word glued to unspaced text ("...feat.") is still read whole.
// The split is ASCII against non-ASCII, so a Latin letter that does not
// decompose ("ø") also splits a word; this only withholds, and is to be
// revisited with the acting change (#1525).
func guardTokens(s string) (out []string) {
	for _, tok := range tokens(s) {
		start := 0
		for i, r := range tok {
			if i > 0 && (r < utf8.RuneSelf) != (tok[i-1] < utf8.RuneSelf) {
				out, start = append(out, tok[start:i]), i
			}
		}
		out = append(out, tok[start:])
	}
	return out
}

// credited reports a guest credit among toks (see creditWords).
func credited(toks []string) bool {
	for i, tok := range toks {
		after := i+1 < len(toks)
		if slices.Contains(creditWords, tok) || (after && slices.Contains(creditLeads, tok)) ||
			(after && i > 0 && slices.Contains(creditJoins, tok)) {
			return true
		}
	}
	return false
}

// voiced reports whether s, a title or the base of an album name, says a voice
// is on the recording or that it is another version of the song: any contra,
// hard or soft, a guest credit (credited) or an ampersand, or any row of the
// voice tables. The caller drops format runes first.
func voiced(s string) bool {
	toks := guardTokens(s)
	has := func(list []string, match func(tok, n string) bool) bool {
		return slices.ContainsFunc(toks, func(tok string) bool {
			return slices.ContainsFunc(list, func(n string) bool { return match(tok, n) })
		})
	}
	return strings.Contains(s, "&") || credited(toks) ||
		slices.ContainsFunc(voiceGlued, func(n string) bool { return strings.Contains(s, n) }) ||
		has(voiceWords, func(tok, n string) bool { return tok == n }) ||
		has(voiceStems, strings.Contains) || has(voiceHeads, strings.HasPrefix) ||
		slices.ContainsFunc(toks, func(tok string) bool {
			return slices.ContainsFunc(contras, func(c contra) bool { return tok == c.needle || (c.sub && strings.Contains(tok, c.needle)) })
		})
}

// releaseTitle reports whether a title could be a plain track of an
// instrumental release: it names something, holds no word of a voice (voiced)
// and no cover phrase, and every bracket pair and the dash-suffix hold nothing
// but allowed words (segmentWords and the rest of that allow-list). Brackets
// that are not well-formed cannot be read: no.
func releaseTitle(title string) bool {
	title = stripFormat(title)
	rs := []rune(title)
	spans, ok := bracketSpans(rs)
	if !ok || len(tokens(title)) == 0 || voiced(title) || coverSet.in(title) {
		return false
	}
	segs := make([]string, 0, len(spans)+1)
	for _, sp := range spans {
		segs = append(segs, string(rs[sp.open+1:sp.close]))
	}
	if sep, end, has := dashSuffix(rs, coverage(rs, spans)); has {
		segs = append(segs, string(rs[sep+1:end]))
	}
	return !slices.ContainsFunc(segs, func(seg string) bool {
		toks := guardTokens(seg)
		for i, tok := range toks {
			prev := ""
			if i > 0 {
				prev = toks[i-1]
			}
			switch {
			case slices.Contains(segmentWords, tok), slices.Contains(partWords, tok), yearToken.MatchString(tok):
			case tok == "version" && prev == "album":
			case partNumber.MatchString(tok) && (slices.Contains(partWords, prev) || partNumber.MatchString(prev)):
			default:
				return true
			}
		}
		return len(toks) == 0
	})
}

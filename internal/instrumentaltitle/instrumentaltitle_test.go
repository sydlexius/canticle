package instrumentaltitle

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/normalize"
)

// Every title, album and artist below is invented.

// TestNormalizeKeyFoldPinned pins what normalize.NormalizeKey does to each
// Japanese needle. The fold strips voicing marks, so several needles change;
// the package folds its needles through the same function, and this test fails
// loudly if that fold ever makes two needles collide or eats the long-vowel
// mark the "install" exclusion depends on.
func TestNormalizeKeyFoldPinned(t *testing.T) {
	pins := []struct{ in, want string }{
		{"カラオケ", "カラオケ"},
		{"インスト", "インスト"},
		{"オフボーカル", "オフホーカル"},   // dakuten stripped
		{"オフヴォーカル", "オフウォーカル"}, // dakuten stripped
		{"アカペラ", "アカヘラ"},       // handakuten stripped
		{"あかぺら", "あかへら"},
		{"ア･ｶﾍﾟﾗ", "ア・カヘラ"}, // half-width middle dot folds to the full-width one
		{"ー", "ー"}, // long-vowel mark survives
		{"ｲﾝｽﾄｰﾙ", "インストール"},  // half-width folds, mark included
		{"ｵﾌﾎﾞｰｶﾙ", "オフホーカル"}, // half-width with a separate voicing mark
		{"オフボーカル", "オフホーカル"},
		{"（Ｉｎｓｔ）［］｛｝", "(inst)[]{}"},
		{"【】〔〕〈〉《》「」『』", "【】〔〕〈〉《》「」『』"},
		{"a　－–—−～〜", "a -–—−~〜"},
		{"a\xffb\x00c", "ab\x00c"}, // invalid bytes are dropped, NUL is kept
	}
	for _, p := range pins {
		if got := normalize.NormalizeKey(p.in); got != p.want {
			t.Errorf("NormalizeKey(%q) = %q, want %q", p.in, got, p.want)
		}
	}
	seen := map[string]Family{}
	for _, k := range kanas {
		if prev, dup := seen[k.needle]; dup && prev != k.family {
			t.Errorf("folded needle %q serves both %q and %q", k.needle, prev, k.family)
		}
		seen[k.needle] = k.family
		if slices.Contains(slices.Concat(aCappellaGlued, karaokeGlued), k.needle) {
			t.Errorf("folded needle %q collides with the exclusion", k.needle)
		}
	}
}

type tc struct {
	title, album string
	want         Verdict
}

func run(t *testing.T, cases []tc) {
	t.Helper()
	for _, c := range cases {
		if got := Classify(c.title, c.album); got != c.want {
			t.Errorf("Classify(%q, %q) = %+v, want %+v", c.title, c.album, got, c.want)
		}
	}
}

func strong(f Family, p Position) Verdict { return Verdict{Strong, f, p} }
func weak(f Family, p Position) Verdict   { return Verdict{Weak, f, p} }

// TestStrongFamilies covers every Strong family in bracket and dash-suffix form.
func TestStrongFamilies(t *testing.T) {
	families := []struct {
		q   string
		fam Family
	}{
		{"Instrumental", FamilyInstrumental},
		{"Instrumentals", FamilyInstrumental},
		{"インストゥルメンタル", FamilyInstrumental},
		{"インストルメンタル", FamilyInstrumental},
		{"Inst", FamilyInst},
		{"Inst.", FamilyInst},
		{"インスト", FamilyInst},
		{"インスト・バージョン", FamilyInst},
		{"インスト版", FamilyInst},
		{"インスト\x00", FamilyInst}, // a NUL is a separator like any other
		{"オフボーカル\x00", FamilyOffVocal},
		{"Backing Track", FamilyBackingTrack},
		{"BGM", FamilyBGM},
		{"B.G.M.", FamilyBGM},
		{"Off Vocal", FamilyOffVocal},
		{"Off-Vocals", FamilyOffVocal},
		{"オフボーカル", FamilyOffVocal},
		{"オフヴォーカル", FamilyOffVocal},
		{"オフ・ボーカル", FamilyOffVocal},
		{"オフ　ヴォーカル", FamilyOffVocal},
		{"オフボーカル・バージョン", FamilyOffVocal},
		{"MR Ver.", FamilyMR},
		{"M.R. Ver", FamilyMR},
		{"MR Version", FamilyMR},
		{"No Vocal", FamilyNoVocal},
		{"No Vocals", FamilyNoVocal},
		{"Without Vocal", FamilyNoVocal},
		{"Without Vocals", FamilyNoVocal},
	}
	var cases []tc
	for _, f := range families {
		cases = append(cases,
			tc{"Glass Harbor (" + f.q + ")", "", strong(f.fam, PositionBracket)},
			tc{"Glass Harbor - " + f.q, "", strong(f.fam, PositionSuffix)},
		)
	}
	run(t, cases)
}

// TestDelimiterVariants covers every bracket pair and every separator, plus the
// whitespace rule that tells a dash separator from a hyphenated name.
func TestDelimiterVariants(t *testing.T) {
	var cases []tc
	for _, b := range []string{"()", "[]", "{}", "（）", "［］", "｛｝", "【】", "〔〕", "〈〉", "《》", "「」", "『』", "｢｣"} {
		r := []rune(b)
		cases = append(cases, tc{"Paper Kites " + string(r[0]) + "Instrumental" + string(r[1]), "", strong(FamilyInstrumental, PositionBracket)})
	}
	for _, d := range []string{"-", "‐", "–", "—", "―", "−", "－", "─"} {
		cases = append(cases,
			tc{"Paper Kites " + d + " Instrumental", "", strong(FamilyInstrumental, PositionSuffix)},
			tc{"Paper Kites " + d + "Instrumental" + d, "", strong(FamilyInstrumental, PositionSuffix)},
			// No whitespace on the left: a hyphenated name, not a separator.
			tc{"Paper" + d + "Instrumental", "", weak(FamilyInstrumental, PositionInline)},
		)
	}
	for _, d := range []string{"~", "～", "〜"} {
		cases = append(cases,
			tc{"Paper Kites" + d + "Instrumental", "", strong(FamilyInstrumental, PositionSuffix)},
			tc{"Paper Kites " + d + "Instrumental" + d, "", strong(FamilyInstrumental, PositionSuffix)},
		)
	}
	run(t, cases)
}

// TestJapaneseForms covers half-width, full-width, composed and decomposed
// input, and the loanword that only starts like the abbreviation.
func TestJapaneseForms(t *testing.T) {
	run(t, []tc{
		{"夜の灯台 (ｲﾝｽﾄ)", "", strong(FamilyInst, PositionBracket)},
		{"夜の灯台 (ｵﾌﾎﾞｰｶﾙ)", "", strong(FamilyOffVocal, PositionBracket)},
		{"夜の灯台 (オフボーカル)", "", strong(FamilyOffVocal, PositionBracket)},  // decomposed
		{"夜の灯台 (オフヴォーカル)", "", strong(FamilyOffVocal, PositionBracket)}, // decomposed
		{"夜の灯台（Ｉｎｓｔｒｕｍｅｎｔａｌ）", "", strong(FamilyInstrumental, PositionBracket)},
		{"夜の灯台 ―インスト―", "", strong(FamilyInst, PositionSuffix)},
		{"夜の灯台 ─インスト─", "", strong(FamilyInst, PositionSuffix)},
		{"夜の灯台―インスト―", "", weak(FamilyInst, PositionInline)},
		{"夜の灯台 (インストver)", "", strong(FamilyInst, PositionBracket)}, // no space before packaging
		{"夜の灯台 (インスト Version)", "", strong(FamilyInst, PositionBracket)},
		// Unspaced Japanese beside the needle is a non-packaging token: Weak.
		{"夜の灯台 (新インスト曲)", "", weak(FamilyInst, PositionBracket)},
		{"夜の灯台 (オフボーカル新版)", "", weak(FamilyOffVocal, PositionBracket)},
		{"夜のインスト灯台", "", weak(FamilyInst, PositionInline)},
		// The "install" loanword, full-width and half-width, is not a signal.
		{"夜の灯台 (インストール)", "", Verdict{}},
		{"ｲﾝｽﾄｰﾙ手順", "", Verdict{}},
		// The loanword does not hide a real occurrence beside it, and stays a
		// non-packaging token itself.
		{"夜の灯台 (インストール インスト)", "", weak(FamilyInst, PositionBracket)},
		{"夜の灯台 (インストー インスト)", "", weak(FamilyInst, PositionBracket)},
	})
}

// TestWeakAndNone covers the tier boundary: position, packaging vocabulary and
// the terms that are Weak wherever they sit.
func TestWeakAndNone(t *testing.T) {
	run(t, []tc{
		{"", "", Verdict{}},
		{"Glass Harbor", "Low Tide Sessions", Verdict{}},
		{"Instrumental", "", weak(FamilyInstrumental, PositionInline)},
		{"Instrumentals", "", weak(FamilyInstrumental, PositionInline)},
		{"Instrumental Weather", "", weak(FamilyInstrumental, PositionInline)},
		{"Glass Harbor Minus One", "", Verdict{}}, // a sing-along product: excluded
		{"Instrumentalist", "", Verdict{}},
		{"Glass Harbor (Instrumentalist)", "", Verdict{}},
		// "inst" is a term only alone in a segment; inline it abbreviates anything.
		{"Inst Harbor", "", Verdict{}},
		{"Inst", "", Verdict{}},
		{"Inst Version", "Inst Mix", Verdict{}},
		{"Glass Harbor (Inst Rehearsal)", "", Verdict{}},
		// Packaging words and a year keep a segment Strong.
		{"Glass Harbor (Original Instrumental Version)", "", strong(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor [2019 Instrumental Mix]", "", strong(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor - TV Size Inst", "", strong(FamilyInst, PositionSuffix)},
		{"Glass Harbor (Backing Track Album Edit)", "", strong(FamilyBackingTrack, PositionBracket)},
		// A non-packaging token, or a number that is not a year, makes it Weak.
		{"Glass Harbor (Live Instrumental)", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Instrumental 12345)", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor - Instrumental Demo", "", weak(FamilyInstrumental, PositionSuffix)},
		{"Glass Harbor - Instrumental - Demo", "", weak(FamilyInstrumental, PositionInline)},
		// A segment that qualifies nothing is a song named with the word.
		{"(Instrumental)", "", weak(FamilyInstrumental, PositionBracket)},
		{"[BGM] (Off Vocal)", "", weak(FamilyBGM, PositionBracket)},
		{"~Instrumental~", "", weak(FamilyInstrumental, PositionSuffix)},
		// Weak terms stay Weak in any position.
		{"Glass Harbor (Orchestral Version)", "", weak(FamilyArrangement, PositionBracket)},
		{"Glass Harbor - Piano Ver.", "", weak(FamilyArrangement, PositionSuffix)},
		{"Glass Harbor (Original Score)", "", weak(FamilyScore, PositionBracket)},
		{"Final Score", "", weak(FamilyScore, PositionInline)},
		{"Glass Harbor (Piano)", "", Verdict{}},
		{"Mr Harbor", "", Verdict{}},
		{"Glass Harbor (TV Size)", "", Verdict{}},
		{"Glass Harbor - TV Size", "TV Size Collection", Verdict{}},
	})
}

// TestRealBase: a segment is Strong only when it qualifies a real title. A
// track number is not one, in any of its dressings; a title written inside an
// earlier bracket pair is.
func TestRealBase(t *testing.T) {
	var cases []tc
	for _, n := range []string{
		"03", "03.", "#3", "A1", "b2", "1-03", "Track 03", "TR03", "CD1 03", "Disc 2 Track 5", "１２",
		"No. 5", "Vol 2", "Side A", "Pt. 2", "Part 2",
	} {
		cases = append(cases,
			tc{n + " - Off Vocal", "", weak(FamilyOffVocal, PositionSuffix)},
			tc{n + " - Minus One", "", Verdict{}}, // a sing-along product: excluded
			tc{n + " (Instrumental)", "", weak(FamilyInstrumental, PositionBracket)},
			tc{"(BGM) " + n, "", weak(FamilyBGM, PositionBracket)},
			// The same number in front of a name is only a number.
			tc{n + " Glass Harbor - Off Vocal", "", strong(FamilyOffVocal, PositionSuffix)},
			tc{n + " Glass Harbor (Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		)
	}
	run(t, append(cases, []tc{
		// "3)" is a stray closer, so the field has no segment at all.
		{"3) - BGM", "", weak(FamilyBGM, PositionInline)},
		{"3) (Instrumental)", "", weak(FamilyInstrumental, PositionInline)},
		// Not a track position: a letter outside the vinyl sides, or three digits.
		{"K9 (Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		{"A100 - BGM", "", strong(FamilyBGM, PositionSuffix)},
		// A packaging word is still a name when it stands outside the brackets.
		{"Original (Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		// The artist-prefix shape cannot be told apart here: the caller's check.
		{"Tin Lantern Band - Off Vocal", "", strong(FamilyOffVocal, PositionSuffix)},
		{"Lecture - Inst. 2020", "", strong(FamilyInst, PositionSuffix)},
		// A title in its own bracket pair, before the qualifier, is a base.
		{"「Glass Harbor」(Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		{"(Glass Harbor) (Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		{"03 「Glass Harbor」 [BGM]", "", strong(FamilyBGM, PositionBracket)},
		// After it, it is not: the quoted word may be the song's own name.
		{"「BGM」(Remix)", "", weak(FamilyBGM, PositionBracket)},
		{"(Glass Harbor Instrumental) [BGM]", "", weak(FamilyInstrumental, PositionBracket)},
		{"(Instrumental) (Glass Harbor)", "", weak(FamilyInstrumental, PositionBracket)},
		// Nor is a pair holding only packaging, a number, or another term.
		{"(Album Version) (Instrumental)", "", weak(FamilyInstrumental, PositionBracket)},
		{"(03) (Instrumental)", "", weak(FamilyInstrumental, PositionBracket)},
		{"(Glass Harbor (Instrumental))", "", weak(FamilyInstrumental, PositionBracket)},
	}...))
}

// TestContradiction: a clean segment is capped at Weak when another qualifier
// segment says the recording has vocals, and only then.
func TestContradiction(t *testing.T) {
	run(t, []tc{
		{"Glass Harbor (Off Vocal Version) [With Lead Vocal]", "", weak(FamilyOffVocal, PositionBracket)},
		{"Glass Harbor (MR Version) (Vocal Version)", "", weak(FamilyMR, PositionBracket)},
		{"Glass Harbor (Instrumental) (Vocal Mix)", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Vocal Version) - Instrumental", "", weak(FamilyInstrumental, PositionSuffix)},
		{"Glass Harbor (Not Instrumental) [BGM]", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor [BGM] (Non-Instrumental)", "", weak(FamilyBGM, PositionBracket)},
		{"Glass Harbor (Instrumental) - Vocals Up", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor - Instrumental (Sung Demo)", "", weak(FamilyInstrumental, PositionSuffix)},
		{"Glass Harbor (Vocal [Instrumental])", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Off Vocal) [Guide Melody]", "", weak(FamilyOffVocal, PositionBracket)},
		{"Glass Harbor (Backing Track) [Chorus On]", "", weak(FamilyBackingTrack, PositionBracket)},
		{"Glass Harbor (Sing-Along) (Backing Track)", "", Verdict{}}, // a sing-along product: excluded
		{"Glass Harbor (Vox) [Instrumental]", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Sung) [Instrumental]", "", weak(FamilyInstrumental, PositionBracket)},
		// "sung" is a hard word: it counts in a dash-suffix too.
		{"Glass Harbor (Instrumental) - Sung", "", weak(FamilyInstrumental, PositionBracket)},
		// A second bracket naming lyrics or a voice, with no "vocal" in it.
		{"Glass Harbor (Instrumental) [With Lyrics]", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Instrumental) [Lyric Version]", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Instrumental) (with Hook)", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Instrumental) (with Singer)", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Instrumental) (with Voice)", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Instrumental) (with Rap)", "", weak(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Singalong) (Backing Track)", "", Verdict{}}, // a sing-along product: excluded
		{"夜の灯台 (インスト) [ガイド入り]", "", weak(FamilyInst, PositionBracket)},
		{"夜の灯台 (インスト) [新ボーカル]", "", weak(FamilyInst, PositionBracket)},
		{"夜の灯台 (インスト) [ガイドボーカル入り]", "", weak(FamilyInst, PositionBracket)},
		{"夜の灯台 (インスト) (歌入り)", "", weak(FamilyInst, PositionBracket)},
		{"夜の灯台 (インスト) (コーラス入り)", "", weak(FamilyInst, PositionBracket)},
		{"夜の灯台 (オフボーカル) ～歌あり～", "", weak(FamilyOffVocal, PositionBracket)},
		// The base is not consulted, and a dash-suffix only for the hard words.
		{"Sing With Me (Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		{"Vocal Point (Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		{"Voice of the Harbor (Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		{"Rap Sheet Blues (Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		{"Hook Line (Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor - Sing Me Home (Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		// A credit bracket, a subtitle with a negation and no term, and a "vocal"
		// consumed by its own off/no term are not contradictions.
		{"Glass Harbor (feat. Nobody) [Instrumental]", "", strong(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (With You) [Instrumental]", "", strong(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (with Tin Lantern Band) [Instrumental]", "", strong(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Lead Single) [Instrumental]", "", strong(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Non-Stop Mix) [Instrumental]", "", strong(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Not Alone) - BGM", "", strong(FamilyBGM, PositionSuffix)},
		{"Glass Harbor (Off Vocal) [Instrumental]", "", strong(FamilyOffVocal, PositionBracket)},
		{"Glass Harbor (No Vocals) - BGM", "", strong(FamilyNoVocal, PositionBracket)},
		{"夜の灯台 (オフボーカル) [Instrumental]", "", strong(FamilyOffVocal, PositionBracket)},
	})
}

// TestSegmentEdges covers the inputs the design leaves open.
func TestSegmentEdges(t *testing.T) {
	run(t, []tc{
		// Two segments that disagree on cleanliness: the clean one decides.
		{"Glass Harbor (feat. Nobody Instrumental) [Instrumental]", "", strong(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Live) (BGM)", "", strong(FamilyBGM, PositionBracket)},
		// A term both inline and in a clean segment: the segment decides.
		{"Instrumental Weather (Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		// Nested pairs: the inner pair is its own segment.
		{"Glass Harbor (Remaster [Instrumental])", "", strong(FamilyInstrumental, PositionBracket)},
		// A field whose brackets are not well-formed has no segment at all,
		// bracket or suffix: unclosed, stray closer, crossed, over-closed.
		{"Glass Harbor (Instrumental", "", weak(FamilyInstrumental, PositionInline)},
		{"Glass Harbor Instrumental)", "", weak(FamilyInstrumental, PositionInline)},
		{"Glass (Harbor [Instrumental)", "", weak(FamilyInstrumental, PositionInline)},
		{"Glass Harbor (Instrumental]", "", weak(FamilyInstrumental, PositionInline)},
		{"Glass Harbor (Live [Instrumental)]", "", weak(FamilyInstrumental, PositionInline)},
		{"Glass Harbor ((Instrumental)", "", weak(FamilyInstrumental, PositionInline)},
		{"Glass Harbor (Instrumental))", "", weak(FamilyInstrumental, PositionInline)},
		{"Glass Harbor (Live - Instrumental", "", weak(FamilyInstrumental, PositionInline)},
		{"Glass Harbor :) - Instrumental", "", weak(FamilyInstrumental, PositionInline)},
		// A separator inside a bracket pair is not a title-level suffix.
		{"Glass Harbor (Demo - Instrumental)", "", weak(FamilyInstrumental, PositionBracket)},
		// An unspaced hyphen in the name does not move the split point.
		{"Glass-Harbor Twin-Set - Instrumental", "", strong(FamilyInstrumental, PositionSuffix)},
		{"Glass-Harbor - Twin-Instrumental", "", weak(FamilyInstrumental, PositionSuffix)},
		// Mixed script.
		{"夜の灯台 Glass Harbor 【Off Vocal】", "", strong(FamilyOffVocal, PositionBracket)},
		{"夜の灯台 - Instrumental", "", strong(FamilyInstrumental, PositionSuffix)},
		// Case and accents fold; a NUL is a separator, not needle text.
		{"Glass Harbor (INSTRUMÉNTAL)", "", strong(FamilyInstrumental, PositionBracket)},
		{"Glass Harbor (Instrumental\x00)", "", strong(FamilyInstrumental, PositionBracket)},
	})
}

// TestBounds asserts the property the bounds buy, not a wall-clock number: a
// field too long or too bracketed for segment analysis is Weak at most, and an
// a cappella one is still None.
func TestBounds(t *testing.T) {
	pairs := func(n int) string { return "Glass Harbor " + strings.Repeat("()", n) + " (Instrumental)" }
	long := strings.Repeat("Glass Harbor ", maxRunes/13+1) // just over the bound
	run(t, []tc{
		{pairs(maxSpans - 1), "", strong(FamilyInstrumental, PositionBracket)},
		{pairs(maxSpans), "", weak(FamilyInstrumental, PositionInline)},
		{pairs(64000), "", weak(FamilyInstrumental, PositionInline)},
		{"Glass Harbor " + strings.Repeat("(", 4000) + "Instrumental" + strings.Repeat(")", 4000), "", weak(FamilyInstrumental, PositionInline)},
		{long[:maxRunes-14] + "(Instrumental)", "", strong(FamilyInstrumental, PositionBracket)},
		{long + "(Instrumental)", "", weak(FamilyInstrumental, PositionInline)},
		{long + "- BGM", "", weak(FamilyBGM, PositionInline)},
		{long + "(Instrumental) a cappella", "", Verdict{}},
		{long + "(Instrumental) karaoke", "", Verdict{}},
		{"Glass Harbor", long + "(Instrumental)", weak(FamilyInstrumental, PositionAlbum)},
	})
}

// aCappellaSpellings and karaokeSpellings are every spelling the exclusion is
// pinned to catch.
var aCappellaSpellings = []string{
	"A Cappella", "a capella", "A Cappela", "Acappella", "Acapella", "Accapella", "ACAPELLA",
	"A-Cappella", "A. Cappella", "Ａ　Ｃａｐｐｅｌｌａ", "à cappella", "a_cappella", "semi-acapella",
	// Plural and variant endings, no article, K spellings, glued, abbreviated.
	"acapellas", "a cappellas", "acapelle", "a cappelle", "acappelle", "acapell", "acapellla",
	"cappella", "capella", "alla cappella", "akapella", "a kapella", "a-kapella", "a kappella",
	"acapellamix", "diyacapella", "acap", "Acaps", "A.C.A.P.",
	// Japanese: composed, half-width, decomposed, split, hiragana, geminated.
	"アカペラ", "ｱｶﾍﾟﾗ", "アカペラ", "ア・カペラ", "ア･ｶﾍﾟﾗ", "ア カペラ", "あかぺら", "アカペッラ", "アカッペラ",
	// Other scripts, and "unaccompanied".
	"아카펠라", "акапелла", "Акапела", "а капелла", "а капела", "阿卡贝拉", "阿卡貝拉", "清唱", "無伴奏", "无伴奏",
	// Hidden format runes (zero-width space, soft hyphen, joiner), written as
	// escapes to stay visible.
	"a cap\u200bpella", "acap\u00adella", "a\u200d capella",
}

// A karaoke track must get its lyrics. The misspellings are excluded too, and
// so is the unrelated "Karaoka": over-matching is the accepted direction.
var karaokeSpellings = []string{
	"Karaoke", "KARAOKE", "Karaoké", "karaokes", "Karaoke Version", "Karaoke Instrumental", "nokaraoke",
	"Kareoke Version", "Karoke", "Karoakes", "Karaoki", "Karaoka",
	"カラオケ", "ｶﾗｵｹ", "カラ・オケ", "カラオケ版", "からおけ", "卡拉OK", "караоке", "가라오케", "kara\u200boke",
}

// A sing-along product must get its lyrics exactly as karaoke does: the product
// form of each term, spaced, hyphenated, glued, and with the phrases a product
// name carries beside it. "Backing Track" is not here: it is a product on an
// album only (TestBackingTrack).
var singAlongSpellings = []string{
	"Accompaniment Track", "Accompaniment Tracks", "Accompaniment Trax", "ACCOMPANIMENT-TRACK",
	"Performance Track", "Performance Tracks", "Performance Trax", "PerformanceTrax",
	"Split Track", "Split Tracks", "Split-Trax", "SplitTrax",
	"Minus One", "Minus-One", "MinusOne", "Ｍｉｎｕｓ　Ｏｎｅ", "Minus‐One",
	"Performance Track with Background Vocals", "Split Track Instrumental",
	"Minus 1", "Minus-1", "Minus1",
	"Sing Along", "Sing-Along", "Singalong", "Sing-Alongs", "SING ALONGS", "Play Along", "Play-Along", "Playalong",
	// The phrases a karaoke label prints beside the title.
	"Originally Performed by Tin Lantern Band", "In the Style of Tin Lantern Band", "in-the-style-of Tin Lantern Band",
}

// excludedSpellings is a cappella first: TestExclusion reads every spelling
// after those as one that takes the title's verdict with it from the album.
var excludedSpellings = slices.Concat(aCappellaSpellings, karaokeSpellings, singAlongSpellings)

// TestExclusion: a cappella, karaoke and the sing-along products beat
// everything in the title, in every position. In the album, a cappella cancels
// only the album's own signal, while karaoke and a sing-along product take the
// title's verdict with them.
func TestExclusion(t *testing.T) {
	var cases []tc
	for i, s := range excludedSpellings {
		onAlbum := strong(FamilyInstrumental, PositionBracket)
		if i >= len(aCappellaSpellings) {
			onAlbum = Verdict{}
		}
		cases = append(cases,
			tc{"Glass Harbor (" + s + ")", "", Verdict{}},
			tc{"Glass Harbor (" + s + ") [Instrumental]", "", Verdict{}},
			tc{"Glass Harbor (" + s + ") - Off Vocal", "", Verdict{}},
			tc{"Glass Harbor - " + s, "", Verdict{}},
			tc{"Glass Harbor " + s, "Low Tide (Instrumental)", Verdict{}},
			tc{"Glass Harbor", "Low Tide " + s, Verdict{}},
			tc{s + " Glass Harbor (BGM)", "Instrumental Edition", Verdict{}},
			tc{"Glass Harbor - " + s + " (Inst)", "", Verdict{}},
			tc{"Glass Harbor ~" + s + "~ (Off Vocal)", "", Verdict{}},
			tc{"Glass Harbor", "Low Tide (Instrumental) " + s, Verdict{}},
			tc{"Glass Harbor (Instrumental)", "Low Tide " + s, onAlbum},
		)
	}
	// Words that merely resemble it are not the exclusion.
	for _, s := range []string{"A Chapel", "Madcap Elegy", "Cape Lookout", "Scalpel", "Escape Lane", "Капель", "А капля", "Acapulco", "ア カラ ペラ", "Karate", "Kara OK"} {
		cases = append(cases, tc{s + " (Instrumental)", "", strong(FamilyInstrumental, PositionBracket)})
	}
	run(t, append(cases, []tc{
		{"Glass Harbor (Karaoke Instrumental)", "", Verdict{}},
		{"Glass Harbor - Karaoke Version", "", Verdict{}},
		{"Glass Harbor", "Low Tide Karaoke Hits", Verdict{}},
		{"Glass Harbor (Karaoke) [With Lead Vocal]", "Low Tide (Instrumental)", Verdict{}},
		{"Glass Harbor (Instrumental Version)", "Low Tide Karaoke Hits", Verdict{}},
		{"Glass Harbor (In the Style of Tin Lantern Band) [Instrumental Version]", "Karaoke Hits Vol 3", Verdict{}},
		{"Glass Harbor (Instrumental Version)", "Low Tide A Cappella Hits", strong(FamilyInstrumental, PositionBracket)},
		// An album naming both is a karaoke album: karaoke is checked first.
		{"Glass Harbor (Instrumental)", "A Cappella Karaoke", Verdict{}},
	}...))
}

// TestAlbumSignal: the album tier is Weak always, and only for a clean title.
func TestAlbumSignal(t *testing.T) {
	run(t, []tc{
		{"Glass Harbor", "Low Tide (Instrumental)", weak(FamilyInstrumental, PositionAlbum)},
		{"Glass Harbor", "Low Tide (Instrumentals)", weak(FamilyInstrumental, PositionAlbum)},
		{"Glass Harbor", "Low Tide - Minus One", Verdict{}}, // a sing-along album
		{"Glass Harbor", "Low Tide - Karaoke", Verdict{}},
		{"Glass Harbor", "Instrumental Works", weak(FamilyInstrumental, PositionAlbum)},
		{"Glass Harbor", "Low Tide (Original Score)", weak(FamilyScore, PositionAlbum)},
		{"Glass Harbor", "Low Tide (Inst)", weak(FamilyInst, PositionAlbum)},
		{"Glass Harbor", "ｶﾗｵｹ全集", Verdict{}},
		{"Glass Harbor", "インスト全集", weak(FamilyInst, PositionAlbum)},
		// The title's own verdict wins over the album's, except on a karaoke album.
		{"Glass Harbor (BGM)", "Low Tide (Karaoke)", Verdict{}},
		{"Glass Harbor (BGM)", "Low Tide (Minus One)", Verdict{}},
		{"Glass Harbor (BGM)", "Low Tide (Instrumentals)", strong(FamilyBGM, PositionBracket)},
		{"Final Score", "Low Tide (Instrumental)", weak(FamilyScore, PositionInline)},
	})
}

// TestSingAlongAndLabelExclusion: a sing-along or play-along release, and a
// track carrying a karaoke label phrase, is excluded exactly as karaoke is,
// from the title or from the album.
func TestSingAlongAndLabelExclusion(t *testing.T) {
	for _, c := range []struct{ title, album string }{
		{"Glass Harbor", "Low Tide Sing-Along (Instrumental)"},
		{"Glass Harbor", "Singalong Favorites (Instrumental Version)"},
		{"Glass Harbor", "Low Tide (Instrumental) (Sing-Along)"},
		{"Glass Harbor", "Playalong Favorites - Instrumentals"},
		{"Glass Harbor - Sing-Along Version", "Low Tide (Instrumental)"},
		{"Glass Harbor (Play Along) [Instrumental]", ""},
		{"Glass Harbor (Minus 1)", ""},
		{"Glass Harbor (Instrumental)", "Quiet Shelf (Performance Tracks)"},
		{"Glass Harbor", "Low Tide (Originally Performed by Tin Lantern Band) [Instrumental]"},
		{"Glass Harbor (Originally Performed by Tin Lantern Band) [Instrumental]", ""},
		{"Glass Harbor (In the Style of Tin Lantern Band)", "Low Tide (Instrumental)"},
		{"Glass Harbor (Instrumental)", "In the Style of Tin Lantern Band"},
	} {
		if got := Classify(c.title, c.album); got != (Verdict{}) {
			t.Errorf("Classify(%q, %q) = %+v, want no verdict for a sing-along or karaoke-label name", c.title, c.album, got)
		}
		if !KaraokeExcluded(c.title, c.album) {
			t.Errorf("KaraokeExcluded(%q, %q) = false for a sing-along or karaoke-label name", c.title, c.album)
		}
	}
}

// TestProductPhraseAdjacency: the words of a product phrase count only when
// nothing but whitespace or one unspaced hyphen stands between them. Any other
// punctuation makes them two separate things, and the verdict stands. So does
// it for a head word on its own, in another order, or apart from its partner.
func TestProductPhraseAdjacency(t *testing.T) {
	bracket := strong(FamilyInstrumental, PositionBracket)
	for _, c := range []tc{
		{"Live Performance - Track 3", "", Verdict{}},
		{"Performance, Track 2 (Instrumental)", "", bracket},
		{"Split - Tracks From The Vault", "", Verdict{}},
		{"Glass Harbor (Instrumental)", "Live Performance - Track 3", bracket},
		{"Glass Harbor (Instrumental)", "Split - Tracks From The Vault", bracket},
		{"Glass Harbor (Instrumental)", "Backing: Tracks", bracket},
		{"Glass Harbor (Instrumental)", "Split / Trax", bracket},
		{"Glass Harbor (Instrumental)", "Minus, One", bracket},
		{"Glass Harbor (Instrumental)", "Minus - 1", bracket},
		{"Glass Harbor (Instrumental)", "Performance -Tracks", bracket},
		{"Glass Harbor (Instrumental)", "Play. Along the River", bracket},
		{"Glass Harbor (Instrumental)", "In the Style. Of Course", bracket},
		{"Glass Harbor", "Performance, Tracks (Instrumentals)", weak(FamilyInstrumental, PositionAlbum)},
		// Bare words.
		{"Glass Harbor (Instrumental)", "The Performance", bracket},
		{"The Performance (Instrumental)", "", bracket},
		{"Split (Instrumental)", "Tin Lantern Band / Paper Kites Split EP", bracket},
		{"Sonata for Violin with Piano Accompaniment (Instrumental)", "", bracket},
		{"Glass Harbor (Instrumental)", "Backing Vocals", bracket},
		{"Glass Harbor (Instrumental)", "One Minus Two", bracket},
		{"Track Performance (Instrumental)", "Tracks Split", bracket},
		{"Glass Harbor (Instrumental)", "Performance of the Track", bracket},
		{"Sing (Instrumental)", "Along the River", bracket},
		{"Glass Harbor", "The Performance", Verdict{}},
	} {
		if got := Classify(c.title, c.album); got != c.want {
			t.Errorf("Classify(%q, %q) = %+v, want %+v", c.title, c.album, got, c.want)
		}
		if KaraokeExcluded(c.title, c.album) {
			t.Errorf("KaraokeExcluded(%q, %q) = true, want false: not a product phrase", c.title, c.album)
		}
	}
}

// TestBackingTrack: in a track title "Backing Track" is an instrumental
// qualifier, since there it labels a studio outtake, and the plural or glued
// form is nothing at all. In an album name every form is a sing-along product,
// and that exclusion beats the title's qualifier.
func TestBackingTrack(t *testing.T) {
	bracket := strong(FamilyBackingTrack, PositionBracket)
	for _, c := range []struct {
		title, album string
		want         Verdict
		excluded     bool
	}{
		{"Glass Harbor (Backing Track)", "", bracket, false},
		{"Glass Harbor - Backing Track", "", strong(FamilyBackingTrack, PositionSuffix), false},
		{"Glass Harbor (Instrumental Backing Track)", "", strong(FamilyInstrumental, PositionBracket), false},
		{"Glass Harbor (Take 14 / Instrumental Backing Track)", "", weak(FamilyInstrumental, PositionBracket), false},
		{"Glass Harbor (Takes 1 & 2 / Backing Track)", "", weak(FamilyBackingTrack, PositionBracket), false},
		{"Glass Harbor (Backing Track)", "Low Tide", bracket, false},
		{"Glass Harbor (Backing Track)", "Low Tide (Instrumental)", bracket, false},
		// Only the singular is the qualifier; no title form is an exclusion.
		{"Glass Harbor (Backing Tracks)", "", Verdict{}, false},
		{"Glass Harbor (Backing Trax) [Instrumental]", "", strong(FamilyInstrumental, PositionBracket), false},
		{"Glass Harbor (BackingTrack) [Instrumental]", "", strong(FamilyInstrumental, PositionBracket), false},
		// The album: a sing-along product, whatever the title says.
		{"Glass Harbor", "Low Tide (Backing Tracks)", Verdict{}, true},
		{"Glass Harbor", "Low Tide Backing Track (Instrumental)", Verdict{}, true},
		{"Glass Harbor (Instrumental)", "Low Tide Backing Trax", Verdict{}, true},
		{"Glass Harbor (Instrumental)", "Low Tide BackingTracks", Verdict{}, true},
		{"Glass Harbor (Instrumental)", "Low Tide Backing-Track", Verdict{}, true},
		{"Glass Harbor (Backing Track)", "Low Tide (Backing Tracks)", Verdict{}, true},
		{"Glass Harbor (Backing Track)", "Quiet Shelf (Performance Tracks)", Verdict{}, true},
		// A title carrying another excluded phrase beside it is excluded.
		{"Glass Harbor (Sing-Along) (Backing Track)", "", Verdict{}, true},
		{"Glass Harbor (Backing Track) [Minus One]", "", Verdict{}, true},
		{"Glass Harbor (Karaoke Backing Track)", "", Verdict{}, true},
	} {
		if got := Classify(c.title, c.album); got != c.want {
			t.Errorf("Classify(%q, %q) = %+v, want %+v", c.title, c.album, got, c.want)
		}
		if got := KaraokeExcluded(c.title, c.album); got != c.excluded {
			t.Errorf("KaraokeExcluded(%q, %q) = %v, want %v", c.title, c.album, got, c.excluded)
		}
	}
}

// TestSingALongAndDashes: the three-word "sing a long" spellings and any one
// dash between the words of a product phrase are exclusions; other separators
// are not.
func TestSingALongAndDashes(t *testing.T) {
	for _, c := range []struct {
		title, album string
		excluded     bool
	}{
		{"Glass Harbor (Instrumental)", "Low Tide Sing-A-Long", true},
		{"Glass Harbor (Instrumental)", "Low Tide Sing A Long", true},
		{"Glass Harbor (Instrumental)", "Low Tide Sing-a-long", true},
		{"Glass Harbor (Instrumental)", "Low Tide Play-A-Long", true},
		{"Glass Harbor (Instrumental)", "Low Tide Sing-A-Longs", true},
		{"Glass Harbor (Sing-A-Long)", "", true},
		// Deliberate over-match: an ordinary title with the words adjacent costs
		// one normal lookup, the safe direction.
		{"Sing a Long Road Home", "", true},
		{"Sing, a Long Night", "", false},
		// Any one dash rune.
		{"Glass Harbor (Minus\u2013One)", "", true},
		{"Glass Harbor (Instrumental)", "Low Tide Sing\u2013Along", true},
		{"Glass Harbor (Minus\u2014One)", "", true},
		{"Glass Harbor (Minus\u2212One)", "", true},
		{"Glass Harbor (Minus\u2011One)", "", true},
		{"Glass Harbor (Minus\u2010One)", "", true},
		{"Glass Harbor (Minus-One)", "", true},
		// Not a single dash with nothing around it.
		{"Glass Harbor (Minus_One)", "", false},
		{"Glass Harbor (Minus.One)", "", false},
		{"Glass Harbor (Minus--One)", "", false},
		{"Glass Harbor (Minus, One)", "", false},
		{"Glass Harbor (Minus - One)", "", false},
		{"Glass Harbor (Minus/One)", "", false},
	} {
		if got := KaraokeExcluded(c.title, c.album); got != c.excluded {
			t.Errorf("KaraokeExcluded(%q, %q) = %v, want %v", c.title, c.album, got, c.excluded)
		}
		if c.excluded {
			if got := Classify(c.title, c.album); got != (Verdict{}) {
				t.Errorf("Classify(%q, %q) = %+v, want no verdict", c.title, c.album, got)
			}
		}
	}
}

// TestAcceptedTierRises pins two inputs whose tier rose against main (Weak to
// Strong) when the qualifier scan stopped reading "Minus/One" as a term. They
// are accepted because neither is a realistic tag.
func TestAcceptedTierRises(t *testing.T) {
	for _, c := range []struct {
		title string
		want  Verdict
	}{
		{"Glass Harbor (Backing Track) [Not Minus/One]", strong(FamilyBackingTrack, PositionBracket)},
		{"(Minus/One) [Backing Track]", strong(FamilyBackingTrack, PositionBracket)},
	} {
		if got := Classify(c.title, ""); got != c.want {
			t.Errorf("Classify(%q) = %+v, want %+v", c.title, got, c.want)
		}
	}
}

func TestTierString(t *testing.T) {
	if got := fmt.Sprintf("%v %v %v %v", None, Weak, Strong, Tier(7)); got != "none weak strong Tier(7)" {
		t.Errorf("tiers print as %q", got)
	}
}

// FuzzClassify checks that no input panics, that a verdict is always drawn from
// the closed sets (so it can never carry any part of the title), that Strong is
// only ever a bracket or a suffix, and that every pinned a cappella, karaoke or
// sing-along spelling turns any title into None.
func FuzzClassify(f *testing.F) {
	for _, s := range []string{
		"", " ", "(", ")", ")(", "((((", "]]]]", "([{【〔〈《「『", "』」》〉〕】}])", "()", "(())[]{}",
		"-", " - ", "~", "~~~~", " -", "- -- - ", "〜〜", "インスト", "インストー", "ー", "インストインストール",
		"a", "a a a cappella", "inst", "(inst)", "mr", "mr ver", "off", "no", "\xff\xfe(\x00instrumental)",
		"゙゚", "Glass Harbor (Instrumental) - Karaoke ~BGM~", "2019", "(2019)", "a.b.c.", "03 - (x) (bgm) [with vocal]",
	} {
		f.Add(s, "", uint8(0))
		f.Add("Glass Harbor", s, uint8(1))
		f.Add(s, s, uint8(2))
	}
	families := map[Family]bool{}
	for _, tm := range terms {
		families[tm.family] = true
	}
	families[FamilyInst] = true
	positions := map[Position]bool{PositionBracket: true, PositionSuffix: true, PositionInline: true, PositionAlbum: true}
	f.Fuzz(func(t *testing.T, title, album string, pick uint8) {
		v := Classify(title, album)
		switch {
		case v.Tier == None:
			if v != (Verdict{}) {
				t.Fatalf("None verdict carries detail: %+v", v)
			}
		case v.Tier != Weak && v.Tier != Strong, !families[v.Family], !positions[v.Position]:
			t.Fatalf("verdict outside the closed sets: %+v", v)
		case v.Tier == Strong && v.Position != PositionBracket && v.Position != PositionSuffix:
			t.Fatalf("Strong outside a qualifier segment: %+v", v)
		}
		spelling := excludedSpellings[int(pick)%len(excludedSpellings)]
		if got := Classify(title+" "+spelling, album); got != (Verdict{}) {
			t.Fatalf("title with %q appended classified %+v", spelling, got)
		}
	})
}

// TestKaraokeExcluded pins the one distinction Classify's None hides: karaoke
// and the sing-along products (title or album) are reported, an a cappella
// title and a clean one are not.
func TestKaraokeExcluded(t *testing.T) {
	for _, tc := range []struct {
		title, album string
		want         bool
	}{
		{"Glass Harbor (Karaoke Version)", "", true},
		{"Glass Harbor - カラオケ", "", true},
		{"Glass Harbor", "Paper Kites Karaoke", true},
		{"Glass Harbor (Performance Track)", "", true},
		{"Glass Harbor (Instrumental)", "Low Tide Split-Trax", true},
		{"Glass Harbor", "Low Tide - Minus 1", true},
		{"Glass Harbor (A Cappella)", "", false},
		{"Glass Harbor (Instrumental)", "Paper Kites", false},
	} {
		if got := KaraokeExcluded(tc.title, tc.album); got != tc.want {
			t.Errorf("KaraokeExcluded(%q, %q) = %v, want %v", tc.title, tc.album, got, tc.want)
		}
		if tc.want && Classify(tc.title, tc.album) != (Verdict{}) {
			t.Errorf("Classify(%q, %q) is not None for a karaoke exclusion", tc.title, tc.album)
		}
	}
}

// TestSuffixBaseWithin: the artist check a Strong dash-suffix needs. Track
// numbering beside the name is ignored, but a number that is part of the name
// is compared. The name inside the prefix counts as much as the prefix inside
// the name, so every doubtful shape is true (the caller then does not act).
func TestSuffixBaseWithin(t *testing.T) {
	for _, tc := range []struct {
		title, name string
		want        bool
	}{
		{"Tin Lantern Band - Minus One", "Tin Lantern Band", true},
		{"Tin Lantern Band - 03 - Minus One", "Tin Lantern Band", true},
		{"03 Tin Lantern Band - Minus One", "tin lantern band", true},
		{"3 Tin Lanterns - Instrumental", "3 Tin Lanterns", true},
		{"03 - 3 Tin Lanterns - Instrumental", "3 Tin Lanterns", true},
		{"Tin Lantern Band (Live) - Minus One", "Tin Lantern Band", true},
		{"808 - Minus One", "808", true}, // the name is itself numbering
		{"Tin Lantern Band - Minus One", "Tin Lantern Band feat. Paper Kites", true},
		{"Tïn Läntern Band - Minus One", "TIN LANTERN BAND", true},                      // diacritics, case
		{"Ｔｉｎ Lantern Band - Minus One", "Tin Lantern Band", true},                      // full-width
		{"T.L.B. - Minus One", "TLB", true},                                             // dotted initials
		{"03 Tin Lantern Band - Minus One", "Tin Lantern Band feat. Paper Kites", true}, // numbering beside one artist of several
		// A leading article on either side changes nothing (containment).
		{"The Tin Lantern Band - Minus One", "Tin Lantern Band", true},
		{"Tin Lantern Band - Minus One", "The Tin Lantern Band", true},
		{"03 The Tin Lantern Band - Minus One", "Tin Lantern Band", true},
		{"The The - Minus One", "The The", true},
		{"The - Minus One", "The", true}, // the article is the whole name
		// The name inside the prefix: the title credits more than the tag does.
		{"Tin Lantern Band feat. Paper Kites - Minus One", "Tin Lantern Band", true},
		{"Tin Lantern Band Glass Harbor - Minus One", "Tin Lantern Band", true},
		{"Tin Lantern Band - Glass Harbor - Instrumental", "Tin Lantern Band", true}, // the stated trade
		// Every spelling of the ampersand is one token.
		{"Tin & Lantern - Minus One", "Tin and Lantern", true},
		{"Tin &amp; Lantern - Minus One", "Tin & Lantern", true},
		{"Tin and Lantern - Minus One", "Tin ＆ Lantern", true},
		{"Tin And Lantern - Minus One", "TIN &AMP; LANTERN", true},
		{"Tin Lantern - Minus One", "Tin & Lantern", false}, // not a contiguous run of it
		{"Glass Harbor - Minus One", "Tin Lantern Band", false},
		{"Lantern Tin Band - Minus One", "Tin Lantern Band", false}, // same tokens, another order
		{"3 Glass Harbor - Instrumental", "3 Tin Lanterns", false},
		{"Glass Harbor (Instrumental)", "Glass Harbor", false}, // no separator
		{"Tin Lantern Band - Minus One", "", false},
		// Another script cannot be matched: the accepted residual.
		{"ティン - Minus One", "Tin", false},
	} {
		if got := SuffixBaseWithin(tc.title, tc.name); got != tc.want {
			t.Errorf("SuffixBaseWithin(%q, %q) = %v, want %v", tc.title, tc.name, got, tc.want)
		}
	}
}

// TestPlaceholderArtist: a filler is no artist, a real name is one, including
// a real name that merely contains a filler word.
func TestPlaceholderArtist(t *testing.T) {
	for name, want := range map[string]bool{
		"": true, "  ": true, "Various Artists": true, "various artists": true, "Various": true,
		"VA": true, "V.A.": true, "v.a": true, "Unknown Artist": true, "[Unknown]": true, "Unknown": true,
		"Tin Lantern Band": false, "Unknown Lanterns": false, "Various Lanterns": false, "Va Lantern": false,
	} {
		if got := PlaceholderArtist(name); got != want {
			t.Errorf("PlaceholderArtist(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestActionable: the one answer the worker gate and the bulk repair share. A
// bracketed Strong title needs no artist; a Strong dash-suffix needs a real
// artist that is not the text before the separator, whether the track artist
// or the album artist carries it. The verdict is Classify's own either way.
func TestActionable(t *testing.T) {
	const suffix = "Tin Lantern Band - Off Vocal"
	for _, tc := range []struct {
		name                              string
		title, album, artist, albumArtist string
		want                              bool
	}{
		{"bracket, no artist", "Glass Harbor (Instrumental)", "", "", "", true},
		{"bracket, artist named in the title", "Glass Harbor (Instrumental)", "", "Glass Harbor", "", true},
		{"suffix, unrelated artist", suffix, "", "Paper Kites", "", true},
		{"suffix, unrelated artist and album artist", suffix, "", "Paper Kites", "Glass Harbor", true},
		{"suffix, unrelated artist beside a placeholder", suffix, "", "Paper Kites", "Various Artists", true},
		{"suffix, artist equals the prefix", suffix, "", "Tin Lantern Band", "", false},
		{"suffix, artist holds the prefix", suffix, "", "Tin Lantern Band feat. Paper Kites", "", false},
		{"suffix, artist inside the prefix", suffix, "", "Lantern Band", "", false},
		{"suffix, album artist equals the prefix", suffix, "", "Paper Kites", "Tin Lantern Band", false},
		{"suffix, album artist holds the prefix", suffix, "", "Paper Kites", "The Tin Lantern Band", false},
		{"suffix, album artist inside the prefix", suffix, "", "Paper Kites", "Tin Lantern", false},
		{"suffix, artist is the prefix on a compilation", suffix, "", "Tin Lantern Band", "Copper Finch Trio", false},
		{"suffix, only the album artist, the prefix", suffix, "", "", "Tin Lantern Band", false},
		{"suffix, only the album artist, unrelated", suffix, "", "", "Paper Kites", true},
		{"suffix, no artist", suffix, "", "", "", false},
		{"suffix, placeholders only", suffix, "", "Unknown Artist", "Various Artists", false},
		{"weak", "Glass Harbor", "Glass Harbor (Instrumentals)", "Paper Kites", "", false},
		{"backing track title", "Glass Harbor (Backing Track)", "", "Paper Kites", "", true},
		{"backing track title on a sing-along album", "Glass Harbor (Backing Track)", "Low Tide (Backing Tracks)", "Paper Kites", "", false},
		{"sing-along title", "Glass Harbor (Performance Track)", "", "Paper Kites", "", false},
		{"sing-along album", "Glass Harbor (Instrumental)", "Low Tide Performance Tracks", "Paper Kites", "", false},
		{"a cappella", "Glass Harbor (A Cappella Instrumental)", "", "Paper Kites", "", false},
		{"karaoke title", "Glass Harbor (Karaoke Instrumental)", "", "Paper Kites", "", false},
		{"karaoke album", "Glass Harbor (Instrumental)", "Karaoke Hits", "Paper Kites", "", false},
	} {
		v, ok := Actionable(tc.title, tc.album, tc.artist, tc.albumArtist)
		if ok != tc.want {
			t.Errorf("%s: Actionable(%q, %q, %q, %q) ok = %v, want %v", tc.name, tc.title, tc.album, tc.artist, tc.albumArtist, ok, tc.want)
		}
		if want := Classify(tc.title, tc.album); v != want {
			t.Errorf("%s: Actionable verdict = %+v, want Classify's %+v", tc.name, v, want)
		}
	}
	// The shapes the table rests on are pinned: a row that stopped being a
	// Strong suffix (or Weak) would be false for the wrong reason.
	if v, _ := Actionable(suffix, "", "", ""); v.Tier != Strong || v.Position != PositionSuffix {
		t.Fatalf("Actionable(%q) verdict = %+v, want a Strong suffix", suffix, v)
	}
	if v, _ := Actionable("Glass Harbor", "Glass Harbor (Instrumentals)", "", ""); v.Tier != Weak {
		t.Fatalf("the weak case is %+v, want Weak", v)
	}
}

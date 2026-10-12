package instrumentaltitle

import (
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// Every title, album and artist below is invented.

const relArtist = "Tin Lantern Band"

// relCheck runs InstrumentalRelease over every title with every album, under a
// known artist that is nowhere in either.
func relCheck(t *testing.T, want bool, titles, albums []string) {
	t.Helper()
	for _, title := range titles {
		for _, a := range albums {
			if got := InstrumentalRelease(title, a, relArtist, ""); got != want {
				t.Errorf("InstrumentalRelease(%q, %q) = %v, want %v", title, a, got, want)
			}
		}
	}
}

// TestInstrumentalReleaseShapes pins the album names that end in a marker and
// the near misses that do not.
func TestInstrumentalReleaseShapes(t *testing.T) {
	plain := []string{"Quiet Shelf"}
	// Every marker, in both accepted bracket pairs (a full-width pair folds to
	// them) and after every dash rune, in any case, width and spacing.
	relCheck(t, true, plain, []string{
		"Low Tide (Instrumental)", "Low Tide [Instrumentals]", "Low Tide (Instrumental Version)",
		"Low Tide [Instrumental Edition]", "Low Tide (Instrumental Album)", "LOW TIDE (INSTRUMENTAL)",
		"Low Tide（Ｉｎｓｔｒｕｍｅｎｔａｌ）", "Low Tide ［Instrumental］", "Low Tide(Instrumental)", "Low Tide (Instrumental) ",
		"Low Tide ( Instrumental  Version )", "Low Tide\u00a0(Instrumental\u00a0Version)", "Low Tide (Instrumentál)",
		"Low Tide - Instrumental", "Low Tide ‐ Instrumentals", "Low Tide – Instrumental Version",
		"Low Tide — Instrumental Edition", "Low Tide ― Instrumental Album", "Low Tide − Instrumental",
		"Low Tide ─ Instrumental", "Low Tide － Instrumental", "Low Tide -Instrumental",
		"Glass Harbor - Low Tide - Instrumental", "Low ~ Tide - Instrumental",
	})
	relCheck(t, false, plain, []string{
		// Bracket pairs that are not accepted, and separators that are not dashes.
		"Low Tide {Instrumental}", "Low Tide 【Instrumental】", "Low Tide 〔Instrumental〕", "Low Tide 「Instrumental」",
		"Low Tide ~ Instrumental", "Low Tide 〜 Instrumental", "Low Tide ~Instrumental~", "Low Tide- Instrumental",
		"Low Tide: Instrumental", "Low Tide / Instrumental",
		// The bare trailing word, the word inline, and no signal at all.
		"Low Tide Instrumentals", "Low Tide Instrumental", "A Bluegrass Instrumental Tribute to Glass Harbor", "Low Tide", "",
		// Other words inside the marker, or its words joined by anything but space.
		"Low Tide (Instrumental and Vocal Versions)", "Low Tide (Deluxe / Instrumental)", "Low Tide (Instrumental Demos)",
		"Low Tide (Instrumental Mix)", "Low Tide (Original Instrumental)", "Low Tide (The Instrumentals)",
		"Low Tide (Instrumental 2019)", "Low Tide (Instrumental-Version)", "Low Tide (Instrumental\u200bVersion)",
		"Low Tide - Deluxe Instrumental",
		// The marker is not last, or another bracket stands beside it.
		"Low Tide (Instrumental) [Deluxe]", "Low Tide (Instrumental) Deluxe", "Low Tide [Deluxe] (Instrumental)",
		"Low Tide (Deluxe) - Instrumental", "Low (A) Tide (B) - Instrumental", "Low Tide ((Instrumental))", "Low Tide (Instrumental).", "Low Tide (Instrumental)\u200b",
		// Malformed brackets.
		"Low Tide (Instrumental", "Low Tide ) (Instrumental)", "Low Tide (Instrumental]",
		// The whole name is the word, or only numbering precedes it.
		"Instrumental", "(Instrumental)", " - Instrumental", "03 - Instrumental", "Disc 2 (Instrumental)",
		// A score, also when a format rune splits the word.
		"Low Tide Score (Instrumental)", "Underscore (Instrumental)", "Low Tide (Original Score)",
		"Low Tide Sc\u200bore (Instrumental)", "Low Tide Sco\u00adres - Instrumental",
		// The exclusion comes first.
		"Low Tide Karaoke (Instrumental)", "Low Tide A Cappella (Instrumental)", "Low Tide Sing-Along - Instrumental",
		"Low Tide Backing Tracks (Instrumental)",
	})
}

// TestInstrumentalReleaseAlbumGuard pins the album bases that withhold the
// answer under a good marker: a list of items, a sing-over product and a word
// of a voice. The near misses beside them are still a release.
func TestInstrumentalReleaseAlbumGuard(t *testing.T) {
	plain := []string{"Quiet Shelf"}
	relCheck(t, false, plain, []string{
		// A list: a single or a split release, the marker naming its last item.
		"Quiet Shelf / Quiet Shelf (Instrumental)", "Quiet Shelf b/w Quiet Shelf (Instrumental)",
		"Quiet Shelf; Quiet Shelf (Instrumental)", "Glass Harbor, Glass Harbor (Instrumental)",
		"Glass Harbor + Glass Harbor (Instrumental)", "Glass Harbor & Glass Harbor (Instrumental)",
		"Quiet Shelf / Paper Lantern - Instrumental", "Low Tide and Low Tide (Instrumentals)",
		"Low Tide | Paper Lantern [Instrumental]", "Low Tide plus Paper Lantern (Instrumental)",
		"Low Tide bw Paper Lantern (Instrumental)", "Low Tide vs Paper Lantern (Instrumental)",
		"Low Tide with Paper Lantern (Instrumental)", "Low Tide、Paper Lantern (Instrumental)",
		"Low Tide ／ Paper Lantern (Instrumental)", "Low Tide ＆ Paper Lantern - Instrumental",
		// The accepted cost: an album named with the word is withheld too.
		"Salt and Stone (Instrumental)",
		// Sing-over products the exclusion does not cover.
		"Hits Made Famous by Glass Harbor (Instrumental)", "Low Tide Playback (Instrumental)",
		"Worship Trax (Instrumental)", "Songs for Singers (Instrumental)",
		"Low Tide Original Cast Recording (Instrumental)", "A Tribute to Glass Harbor (Instrumental)",
		"Low Tide Accompaniment - Instrumental", "Low Tide Playbacks [Instrumental]", "Low Tide Cover Versions (Instrumental)",
		// A word of a voice, inflected, and one a format rune splits.
		"Glass Harbor Sings Low Tide (Instrumental)", "Voices (Instrumental)", "Duets (Instrumental)",
		"Vocal Hits (Instrumental)", "Low Tide Choral Works (Instrumental)", "Low Tide Remix - Instrumental",
		"Low Tide Live (Instrumental)", "Low Tide Demos - Instrumental", "Low Tide feat. Glass Harbor (Instrumental)",
		"Low Tide Vo\u200bcals (Instrumental)",
	})
	relCheck(t, true, plain, []string{
		"Sandy Stone (Instrumental)", "Saltand Stone (Instrumental)", "Low Tide Playbook (Instrumental)",
		"Low Tide Recording (Instrumental)", "Famous Low Tide (Instrumental)", "Low Tide Rising (Instrumental)",
		"Low Tide Trap - Instrumental",
	})
}

// TestInstrumentalReleaseTitleGuard pins what the track title does on a good
// release: a plain one counts; any word outside the allow-list in a bracket
// pair or a dash-suffix, a word of a voice anywhere, an excluded title and a
// title with a signal of its own do not.
func TestInstrumentalReleaseTitleGuard(t *testing.T) {
	albums := []string{"Low Tide (Instrumental)", "Low Tide - Instrumentals"}
	// A plain title, and a bracket pair or dash-suffix holding allowed words only.
	relCheck(t, true, []string{
		"Quiet Shelf", "Quiet Shelf (Reprise)", "Quiet Shelf (Remastered)", "Quiet Shelf (Album Version)",
		"Quiet Shelf, Pt. 2", "03", "Quiet Shelf (2019 Remaster)", "Quiet Shelf [Remasters]", "Quiet Shelf (Pt. 2)",
		"Quiet Shelf (Part II)", "Quiet Shelf (Parts 1 2)", "Quiet Shelf (Interlude)", "Quiet Shelf [Intro]",
		"Quiet Shelf (Outro)", "Quiet Shelf (Prelude)", "Quiet Shelf (Mono)", "Quiet Shelf (Stereo)", "Quiet Shelf (Album)",
		"Quiet Shelf - Reprise", "Quiet Shelf - 2004 Remaster", "Quiet Shelf (Reprise) [Mono]",
		// Words that only look like a deny-list row.
		"Alive", "Olive", "Deliver", "Demons", "Demonstration", "Hooked", "Trap", "Wrap", "Defeat", "Rising",
		"Unsung", "Missing Shelf", "Stay Close With", "X Shelf", "Shelf Vs", "Contact", "Mitten", "Covered",
		// No kana needle: the fold would turn one into another word, so only the
		// bracketed form of these is withheld (below).
		"棚ライブ", "棚デモ",
	}, albums)
	// Any other word in a bracket pair or a dash-suffix: it may name a sung version.
	relCheck(t, false, []string{
		"Quiet Shelf (Edit)", "Quiet Shelf (Radio Edit)", "Quiet Shelf (Extended)", "Quiet Shelf (Version)",
		"Quiet Shelf (Single Version)", "Quiet Shelf (Stay Close)", "Quiet Shelf (Paper Lantern Version)",
		"Quiet Shelf (English)", "Quiet Shelf (II)", "Quiet Shelf (2)", "Quiet Shelf (Sped Up)", "Quiet Shelf (Original)",
		"Quiet Shelf ()", "Quiet Shelf (Reprise) (Paper)", "Quiet Shelf [Reprise Paper]", "Quiet Shelf - Paper Lantern",
		"Quiet - Shelf", "Quiet ~ Shelf", "Quiet Shelf (Version Album)", "Quiet Shelf 【Paper】", "棚 (ライブ)", "棚 [デモ]",
	}, albums)
	// The deny-list, over the whole title. Single, Singleton, Invoice and
	// Unspoken are its accepted cost: a stem that withholds the answer.
	relCheck(t, false, []string{
		"Quiet Shelf (Vocal)", "Vocal Shelf", "Quiet Vocalist", "Quiet Shelf (Voc)", "Quiet Shelf (Vox)", "Quiet Shelf (Sung)",
		"Quiet Shelf (ボーカル入り)", "Quiet Shelf (feat. Glass Harbor)", "Quiet Shelf featuring Glass Harbor",
		"Quiet Shelf ft Glass Harbor", "Quiet Shelf (with Glass Harbor)", "Quiet Shelf - with Glass Harbor",
		"Quiet Shelf (Live)", "Live at the Quiet Shelf", "Quiet Shelf (Demo)", "Quiet Shelf (Lyric Video)", "Quiet Lyrical",
		"Sing Quiet Shelf", "Quiet Shelf (Guide)", "Quiet Shelf (", "Quiet Shelf )", "((((", "", "   ", "...",
		"Single", "Singleton", "Invoice", "Unspoken", "Quiet Singer", "Shelf Sings", "Sang Shelf", "Sung Shelf", "Quiet Voices",
		"Choirboy", "Choral Shelf", "Quiet Choruses", "Rap Shelf", "Quiet Raps", "Rapped Shelf", "Quiet Rapper", "Duets",
		"Narrated Shelf", "Quiet Narration", "Speeches", "Spoken Shelf", "Skits", "Quiet Lyricist",
		"Quiet Shelf f. Glass Harbor", "Quiet Shelf w/ Glass Harbor", "Quiet x Shelf", "Quiet vs Shelf", "Quiet & Shelf",
		"Quiet with Shelf", "Stay Close With Them", "Live Shelf", "Demo Shelf", "Quiet Demos", "Quiet Remix", "Cover Shelf",
		"Acoustic Shelf", "Bonus Shelf", "Quiet con Shelf", "Quiet avec Shelf", "Quiet mit Shelf",
		"Quiet Shelf Made Famous by Glass Harbor", "Tribute to Quiet Shelf",
		// Scripts written without spaces, and a credit glued to them.
		"静かな歌", "合唱の棚", "棚 보컬", "棚라이브", "グラスハーバーfeat.ペーパー", "棚ft棚",
		// The vocal words of the title rule (contras), outside any bracket.
		"Off the Hook", "Guide Shelf", "Quiet Shelf Vox", "棚ボーカル",
		// A format rune splitting the word.
		"Glass Harbor (Vo\u200bcal)", "Vo\u00adcal Shelf", "Quiet Shelf fe\u200bat Glass Harbor", "Li\u200bve Shelf",
		// Excluded titles, and titles with a signal of their own, Weak included.
		"Quiet Shelf (A Cappella)", "Quiet Shelf (Karaoke)", "Quiet Shelf Singalong", "Quiet Shelf (Performance Track)",
		"Quiet Shelf Karaoke", "Quiet Shelf Minus One",
		"Quiet Shelf (BGM)", "Quiet Shelf - Off Vocal", "Quiet Shelf (Instrumental)", "Final Score", "Instrumental Jam",
		"Quiet Shelf (Piano Version)",
	}, albums)
}

// TestInstrumentalReleaseArtists pins the artist rules: a guest credit in
// either tag is a no; the dash form needs a real artist that is not the text
// before a dash; the bracket form needs none.
func TestInstrumentalReleaseArtists(t *testing.T) {
	const bracket, dash = "Low Tide (Instrumental)", "Low Tide - Instrumental"
	for _, tc := range []struct {
		name, album, artist, albumArtist string
		want                             bool
	}{
		{"bracket form, no artist", bracket, "", "", true},
		{"bracket form, placeholders only", "Low Tide [Instrumentals]", "Various Artists", "Unknown", true},
		{"dash form, unrelated artist", dash, "Glass Harbor", "", true},
		{"dash form, only the album artist, unrelated", dash, "", "Glass Harbor", true},
		{"dash form, no artist", dash, "", "", false},
		{"dash form, placeholders only", dash, "Various Artists", "VA", false},
		{"dash form, the album is the artist then the word", "Glass Harbor - Instrumental", "Glass Harbor", "", false},
		{"dash form, album artist before the dash", "Glass Harbor - Instrumentals", "Wren Hollow", "The Glass Harbor", false},
		{"bracket form, artist before a dash in the base", "Glass Harbor - Low Tide (Instrumental)", "Glass Harbor", "", false},
		{"artist with a guest", bracket, "Tin Lantern feat. Wren Hollow", "", false},
		{"album artist with a guest", bracket, "Tin Lantern", "Tin Lantern with Wren Hollow", false},
		{"artist w/ a guest", bracket, "Tin Lantern w/ Wren Hollow", "", false},
		{"artist x a guest", bracket, "Tin Lantern x Wren Hollow", "", false},
		{"artist vs a guest", dash, "Tin Lantern vs Wren Hollow", "", false},
		{"album artist ft a guest", bracket, "", "Tin Lantern ft. Wren Hollow", false},
		{"band named with an ampersand", bracket, "Tin Lantern & Sons", "", true},
		{"band named with and", dash, "Tin Lantern and Sons", "", true},
	} {
		if got := InstrumentalRelease("Quiet Shelf", tc.album, tc.artist, tc.albumArtist); got != tc.want {
			t.Errorf("%s: InstrumentalRelease(%q, %q, %q) = %v, want %v", tc.name, tc.album, tc.artist, tc.albumArtist, got, tc.want)
		}
	}
}

// TestInstrumentalReleaseLeavesTheClassifierAlone: the release rule is not in
// Classify, Actionable or KaraokeExcluded. An album that is a release is the
// Weak album signal it always was, never acted on, and the sing-over phrases
// are no exclusion.
func TestInstrumentalReleaseLeavesTheClassifierAlone(t *testing.T) {
	for _, a := range []string{"Low Tide (Instrumental)", "Low Tide - Instrumentals", "Low Tide [Instrumental Album]"} {
		if !InstrumentalRelease("Quiet Shelf", a, relArtist, "") {
			t.Fatalf("fixture drifted: %q is not a release", a)
		}
		want := weak(FamilyInstrumental, PositionAlbum)
		if got := Classify("Quiet Shelf", a); got != want {
			t.Errorf("Classify(plain title, %q) = %+v, want %+v", a, got, want)
		}
		if v, ok := Actionable("Quiet Shelf", a, relArtist, ""); ok || v != want {
			t.Errorf("Actionable(plain title, %q) = %+v, %v; want %+v, never acted on", a, v, ok, want)
		}
	}
	for _, a := range []string{"Low Tide Playback (Instrumental)", "Hits Made Famous by Glass Harbor (Instrumental)", "Worship Trax (Instrumental)"} {
		if KaraokeExcluded("Quiet Shelf", a) || KaraokeExcluded(a, "") {
			t.Errorf("KaraokeExcluded reports %q: the sing-over phrases withhold the release answer only", a)
		}
		if got, want := Classify("Quiet Shelf (Instrumental)", a), strong(FamilyInstrumental, PositionBracket); got != want {
			t.Errorf("Classify(strong title, %q) = %+v, want %+v", a, got, want)
		}
	}
}

func FuzzInstrumentalRelease(f *testing.F) {
	for _, a := range []string{
		"Low Tide (Instrumental)", "Low Tide - Instrumentals", "Karaoke [Instrumental Album]", "x (Instrumental) ~",
		"a / a (Instrumental)", "a, b + c & d; e | f - Instrumental", "", "((((", "Low Tide Sc\u200bore (Instrumental)",
	} {
		for _, title := range []string{"Quiet Shelf", "Quiet Shelf (Vocal)", "Quiet Shelf (Pt. 2) [Radio Edit]", "Quiet Shelf (Vo\u200bcal) (2019 Remaster)", "", "\xff("} {
			f.Add(title, a, "", "")
			f.Add(title, a, relArtist, "Various Artists")
			f.Add(title, a, "Low Tide", "a feat. b")
		}
	}
	// The whole vocabulary a bracket pair of a release title may hold.
	segment := regexp.MustCompile(`^((19|20)[0-9]{2}|[0-9]{1,3}|[ivx]{1,4}|remaster|remastered|remasters|reprise|album|version|pt|part|parts|interlude|intro|outro|prelude|mono|stereo)$`)
	f.Fuzz(func(t *testing.T, title, album, artist, albumArtist string) {
		if !InstrumentalRelease(title, album, artist, albumArtist) {
			return
		}
		if again := InstrumentalRelease(title, album, artist, albumArtist); !again {
			t.Fatalf("InstrumentalRelease(%q, %q) is not deterministic", title, album)
		}
		if v := Classify(title, ""); v != (Verdict{}) {
			t.Fatalf("release over title %q, which classifies %+v alone", title, v)
		}
		// The dry run counts a release among its Weak album rows only.
		if v := Classify(title, album); v != weak(FamilyInstrumental, PositionAlbum) {
			t.Fatalf("release over %q, %q, which classifies %+v", title, album, v)
		}
		if KaraokeExcluded(title, album) {
			t.Fatalf("release over a karaoke-excluded pair %q, %q", title, album)
		}
		rs := []rune(prepare(title))
		spans, _ := bracketSpans(rs)
		for _, sp := range spans {
			for _, tok := range tokens(string(rs[sp.open+1 : sp.close])) {
				if !segment.MatchString(tok) {
					t.Fatalf("release over title %q: bracket token %q is outside the allow-list", title, tok)
				}
			}
		}
		// The marker closes the name, so everything before its opener is base.
		a := strings.TrimRightFunc(prepare(album), unicode.IsSpace)
		cut := strings.LastIndexAny(a, "([")
		if !strings.HasSuffix(a, ")") && !strings.HasSuffix(a, "]") {
			cut = strings.LastIndexFunc(a, isDash)
		}
		if cut < 0 || strings.ContainsAny(a[:cut], "/+&;,|") {
			t.Fatalf("release over album %q: its base holds a list separator", album)
		}
	})
}

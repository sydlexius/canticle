package langguard

import "testing"

func TestScriptOf(t *testing.T) {
	cases := []struct {
		r    rune
		want string
	}{
		{'a', Latin}, {'Z', Latin},
		{'好', Han}, {'山', Han},
		{'あ', Kana}, {'ア', Kana},
		{'한', Hangul}, {'글', Hangul},
		{'Я', Other}, // Cyrillic -> Other (foreign under a Latin allowlist)
		{'5', ""}, {' ', ""}, {':', ""}, {'，', ""},
		{'・', ""}, // U+30FB katakana middle dot (punctuation, not a letter)
		{'゠', ""}, // U+30A0 katakana-hiragana double hyphen (punctuation)
	}
	for _, c := range cases {
		if got := ScriptOf(c.r); got != c.want {
			t.Errorf("ScriptOf(%q) = %q; want %q", c.r, got, c.want)
		}
	}
}

package langguard

import "testing"

func TestIsCreditLine(t *testing.T) {
	credit := []string{
		"作词 : Max Martin/Taylor Swift",
		"制作人 : Steve Mac",
		"网易云音乐",
		"Produced by Steve Mac",
		"Mixing : Serban Ghenea",
	}
	for _, s := range credit {
		if !IsCreditLine(s) {
			t.Errorf("IsCreditLine(%q) = false; want true", s)
		}
	}
	lyric := []string{
		"Nice to meet you, where you been?",
		"I could show you incredible things",
		"夜に駆ける",
		"ла-ла-ла",
		"歌词写得真好", // contains 词 but is a lyric line, not a credit
		"曲折的人生路", // contains 曲 but is a lyric line, not a credit
	}
	for _, s := range lyric {
		if IsCreditLine(s) {
			t.Errorf("IsCreditLine(%q) = true; want false", s)
		}
	}
}

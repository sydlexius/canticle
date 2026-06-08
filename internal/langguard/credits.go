package langguard

import (
	"regexp"
	"strings"
)

// creditTokens are role/attribution markers (CJK + Latin) that identify a
// non-lyric credit line. Seed list; extend as real provider files reveal more.
var creditTokens = []string{
	"作词", "作曲", "编曲", "制作人", "制作", "混音", "母带", "和声", "监制",
	"录音", "出品", "网易云音乐", "纯音乐", "演唱",
	"lyrics", "lyricist", "composer", "arranger", "produced by", "producer",
	"mixing", "mastering", "written by", "performed by", "arranged by",
	"composed by",
}

// roleColon matches a short "role : value" line (CJK or ASCII colon), the
// structural shape of a credit line not caught by the token list.
var roleColon = regexp.MustCompile(`^\s*[^：:]{1,20}\s*[：:]\s*\S`)

// IsCreditLine reports whether a single timestamp-free lyric line is a
// credit/attribution line rather than lyric content.
func IsCreditLine(line string) bool {
	low := strings.ToLower(line)
	for _, t := range creditTokens {
		if strings.Contains(line, t) || strings.Contains(low, t) {
			return true
		}
	}
	return len([]rune(line)) < 40 && roleColon.MatchString(line)
}

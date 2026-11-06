package arbiter

import (
	"strings"
	"unicode"
)

// Scorer 相关性打分器接口（F3.3：可替换为 gse 分词或向量方案）。
type Scorer interface {
	// Score 返回消息与成员画像（关键词）的词面相关度 [0,1]。
	Score(msg, profile string) float64
}

// DefaultScorer 默认实现：CJK rune-bigram + 英文词 的 Jaccard 词面重叠
// （gse 分词可经 Scorer 接口替换，见 TODO.md）。
var DefaultScorer Scorer = bigramScorer{}

type bigramScorer struct{}

func (bigramScorer) Score(msg, profile string) float64 {
	return jaccard(tokens(msg), tokens(profile))
}

// tokens 分词：CJK 相邻字组成 bigram；连续字母/数字作为单词；统一小写。
func tokens(s string) map[string]bool {
	out := make(map[string]bool)
	runes := []rune(strings.ToLower(s))
	var word []rune
	flush := func() {
		if len(word) > 1 {
			out[strings.TrimSpace(string(word))] = true
		} else if len(word) == 1 && !unicode.IsLetter(word[0]) {
			// 单字符标点忽略
		} else if len(word) == 1 {
			// 单字母词贡献过小，忽略
		}
		word = word[:0]
	}
	for i, r := range runes {
		switch {
		case unicode.Is(unicode.Han, r):
			flush()
			if i+1 < len(runes) && unicode.Is(unicode.Han, runes[i+1]) {
				out[string([]rune{r, runes[i+1]})] = true
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			word = append(word, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

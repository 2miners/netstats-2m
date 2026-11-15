// Package camel implements lodash 4 _.camelCase, which the JS client used to
// derive the node id from INSTANCE_NAME. The server keys nodes by that id, so it
// must stay byte-for-byte the same (e.g. "2Miners.com Ethereum PoW SOLO 🇩🇪" →
// "2MinersComEthereumPoWSolo🇩🇪").
package camel

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// deburr strips combining marks (lodash _.deburr for Latin letters).
func deburr(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		return s
	}
	return out
}

func isRegional(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

func isEmojiish(r rune) bool {
	return isRegional(r) || unicode.Is(unicode.So, r) || unicode.Is(unicode.Sk, r) && r > 0x2000
}

func isJoiner(r rune) bool { return r == 0x200D || r == 0xFE0F || r == 0xFE0E || (r >= 0x1F3FB && r <= 0x1F3FF) }

// words splits like lodash's unicodeWords for the inputs that matter here:
// letter runs split at lower→upper and ACRONYMWord boundaries, digit runs,
// and emoji (a regional-indicator pair, or a symbol with its joiners).
func words(s string) []string {
	rs := []rune(s)
	var out []string
	i := 0
	for i < len(rs) {
		r := rs[i]
		switch {
		case unicode.IsDigit(r):
			j := i
			for j < len(rs) && unicode.IsDigit(rs[j]) {
				j++
			}
			out = append(out, string(rs[i:j]))
			i = j
		case unicode.IsLetter(r):
			j := i
			for j < len(rs) && unicode.IsLetter(rs[j]) {
				j++
			}
			out = append(out, splitLetters(rs[i:j])...)
			i = j
		case isRegional(r):
			j := i + 1
			if j < len(rs) && isRegional(rs[j]) {
				j++
			}
			out = append(out, string(rs[i:j]))
			i = j
		case isEmojiish(r):
			j := i + 1
			for j < len(rs) && (isJoiner(rs[j]) || (j > 0 && rs[j-1] == 0x200D && isEmojiish(rs[j]))) {
				j++
			}
			out = append(out, string(rs[i:j]))
			i = j
		default:
			i++
		}
	}
	return out
}

func splitLetters(rs []rune) []string {
	var out []string
	start := 0
	for k := 1; k < len(rs); k++ {
		prev, cur := rs[k-1], rs[k]
		// fooBar → foo|Bar
		if !unicode.IsUpper(prev) && unicode.IsUpper(cur) {
			out = append(out, string(rs[start:k]))
			start = k
			continue
		}
		// XMLHttp → XML|Http (an upper followed by a lower starts a word)
		if unicode.IsUpper(prev) && unicode.IsUpper(cur) && k+1 < len(rs) && !unicode.IsUpper(rs[k+1]) {
			out = append(out, string(rs[start:k]))
			start = k
		}
	}
	return append(out, string(rs[start:]))
}

func upperFirst(w string) string {
	rs := []rune(w)
	if len(rs) == 0 {
		return w
	}
	rs[0] = unicode.ToUpper(rs[0])
	return string(rs)
}

// Case is lodash _.camelCase.
func Case(s string) string {
	var b strings.Builder
	for i, w := range words(deburr(s)) {
		w = strings.ToLower(w)
		if i == 0 {
			b.WriteString(w)
		} else {
			b.WriteString(upperFirst(w))
		}
	}
	return b.String()
}

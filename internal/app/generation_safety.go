package app

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxGenerationRecentContent = 20

var (
	ErrGenerationUnsafe     = errors.New("unsafe generation output")
	ErrGenerationRepetition = errors.New("repeated generation output")
)

// ValidateGenerationSafety is deliberately a heuristic gate, not comprehensive
// moderation or a prompt-injection guarantee. It rejects concealed controls,
// prompt delimiters, selected unsupported authority/access claims and selected
// overt harmful instructions. Paraphrases, other languages and confusables can
// evade semantic rules; quality/safety evaluation is still a release requirement.
// The caller supplies bounded, visible recent agent content, never private data.
func ValidateGenerationSafety(result GenerationResult, recent []Content) error {
	if result.Digest() == "" || len(recent) > MaxGenerationRecentContent {
		return ErrGenerationOutput
	}
	if result.Decision() == GenerationSkip {
		return nil
	}
	content := result.content
	texts := []string{content.Body}
	if content.Code != nil {
		texts = append(texts, content.Code.Language, content.Code.Filename, content.Code.Source)
	}
	for _, text := range texts {
		for _, r := range text {
			if unicode.Is(unicode.Cf, r) || unicode.IsControl(r) && r != '\n' && r != '\t' {
				return ErrGenerationUnsafe
			}
		}
		lower := strings.ToLower(text)
		if strings.Contains(lower, "<|") || strings.Contains(lower, "[inst]") || strings.Contains(lower, "[/inst]") {
			return ErrGenerationUnsafe
		}
		compact := strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				return unicode.ToLower(r)
			}
			return -1
		}, text)
		for _, phrase := range []string{
			"officialrepresentative", "officialspokesperson", "onbehalfof", "iamhuman", "imahuman",
			"ihaveliveaccess", "ihaverealtimeaccess", "icanaccessyour", "iaccessedyour", "icheckedyour", "ijustran", "ijustbrowsed", "isearchedtheweb",
			"killyourself", "stealpasswords", "stealcredentials", "sendmeyourpassword", "sendmeyourapikey",
		} {
			if strings.Contains(compact, phrase) {
				return ErrGenerationUnsafe
			}
		}
	}
	body := generationWords(content.Body)
	for _, previous := range recent {
		if _, err := NewContent(previous.Body, previous.Code); err != nil {
			return ErrGenerationOutput
		}
		words := generationWords(previous.Body)
		if strings.Join(body, " ") == strings.Join(words, " ") || similarGenerationWords(body, words) {
			return ErrGenerationRepetition
		}
		if content.Code != nil && previous.Code != nil && utf8.RuneCountInString(content.Code.Source) >= 40 && strings.Join(strings.Fields(content.Code.Source), " ") == strings.Join(strings.Fields(previous.Code.Source), " ") {
			return ErrGenerationRepetition
		}
	}
	return nil
}

func generationWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

// Multiset adjacent-word Dice similarity >= 80%, for bodies of at least eight
// words. Bounded 320-rune bodies keep work small; this is not semantic similarity.
func similarGenerationWords(a, b []string) bool {
	if len(a) < 8 || len(b) < 8 {
		return false
	}
	pairs := make(map[[2]string]int, len(a)-1)
	for i := 1; i < len(a); i++ {
		pairs[[2]string{a[i-1], a[i]}]++
	}
	shared := 0
	for i := 1; i < len(b); i++ {
		pair := [2]string{b[i-1], b[i]}
		if pairs[pair] > 0 {
			shared++
			pairs[pair]--
		}
	}
	return 10*shared >= 4*(len(a)+len(b)-2)
}

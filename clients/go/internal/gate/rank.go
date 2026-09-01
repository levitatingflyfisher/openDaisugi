package gate

import "strings"

// rankRefusal is rank_rule.REFUSAL.
const rankRefusal = "only the owner records a ranking vote. Record it yourself."

// rankWord is rank_rule._WORD: the ASCII characters a word is made of.
func rankWord(c rune) bool {
	return c < 0x80 && (('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') ||
		strings.ContainsRune("_./:=+@%,-", c))
}

// rankWords is rank_rule._words: backslashes and quotes dropped, then the
// runs of word characters. Any other character, non-ASCII included, ends
// a word.
func rankWords(text string) []string {
	text = strings.NewReplacer("\\", "", "'", "", "\"", "").Replace(text)
	var out []string
	var cur strings.Builder
	for _, c := range text {
		if rankWord(c) {
			cur.WriteRune(c)
		} else if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func rankHead(w string) bool {
	last := w[strings.LastIndex(w, "/")+1:]
	return last == "daisugi" || last == "opendaisugi" || strings.HasPrefix(last, "opendaisugi.")
}

// namesRankRecord is rank_rule.names_rank_record: a daisugi word, then
// rank, then record, in that order.
func namesRankRecord(text string) bool {
	state := 0
	for _, w := range rankWords(text) {
		switch {
		case state == 0 && rankHead(w):
			state = 1
		case state == 1 && w == "rank":
			state = 2
		case state == 2 && w == "record":
			return true
		}
	}
	return false
}

// rankRecordHit is rank_rule.rank_record_hit: a shell call whose command
// is a string that names rank record.
func rankRecordHit(rec *record) bool {
	if rec == nil || rec.StepType != "shell" {
		return false
	}
	c, ok := rec.CommandRaw.(string)
	return ok && namesRankRecord(c)
}

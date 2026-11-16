package agent

// memoryPreambleAck is the fixed assistant line that acknowledges the history
// preamble, so the conversation keeps alternating user/assistant.
const memoryPreambleAck = "Entendido, lo tengo presente."

// shapeHistory keeps at most exchanges earlier exchanges before the current
// message (the last one) and, when preamble returns text, opens the
// conversation with it. An exchange starts at a user message; the kept
// history always starts with one. exchanges <= 0 keeps every exchange.
func shapeHistory(messages []map[string]interface{}, exchanges int, preamble func(keptExchanges int) string) []map[string]interface{} {
	last := len(messages) - 1
	if last < 0 {
		return messages
	}
	var userIdx []int
	for i := 0; i < last; i++ {
		if messages[i]["role"] == "user" {
			userIdx = append(userIdx, i)
		}
	}
	start := last
	if n := len(userIdx); n > 0 {
		start = userIdx[0]
		if exchanges > 0 && n > exchanges {
			start = userIdx[n-exchanges]
		}
	}
	kept := 0
	for _, i := range userIdx {
		if i >= start {
			kept++
		}
	}
	out := messages[start:]
	if preamble == nil {
		return out
	}
	text := preamble(kept)
	if text == "" {
		return out
	}
	shaped := make([]map[string]interface{}, 0, len(out)+2)
	shaped = append(shaped,
		map[string]interface{}{"role": "user", "content": text},
		map[string]interface{}{"role": "assistant", "content": memoryPreambleAck},
	)
	return append(shaped, out...)
}

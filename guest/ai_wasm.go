//go:build wasm || wasip1

package guest

// hostCapabilityQuery is the raw capability_query host import.
//
//go:wasmimport metacore_host capability_query
func hostCapabilityQuery(reqPtr, reqLen uint32) uint64

// AIComplete runs one ai.complete call through the host. The error is only
// for caller bugs (*AIError: forbidden, validation_error); no Pulse, a
// reached cap, a rate limit or the hub being down come back as an AIResult
// whose Status is not "ok":
//
//	res, err := guest.AIComplete(guest.AIRequest{
//	    Feature: "ticket_triage", Prompt: text, Tier: "fast",
//	    ResponseFormat: &guest.AIResponseFormat{Type: "json_object"},
//	    IdempotencyKey: "ticket:" + id + ":triage:v1",
//	})
//	if err != nil { /* fix the manifest or the request */ }
//	if !res.OK() { /* keep going without AI; res.Message says why */ }
func AIComplete(req AIRequest) (AIResult, error) {
	body, err := encodeAIQuery(req)
	if err != nil {
		return AIResult{}, err
	}
	ptr, n := stringRef(string(body))
	return decodeAIEnvelope(readPacked(hostCapabilityQuery(ptr, n)))
}

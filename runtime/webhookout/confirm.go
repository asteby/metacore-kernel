package webhookout

import (
	"fmt"
	"strings"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// ConfirmTextKey is the payload field that carries what the user typed for an
// action with confirm_typed.
const ConfirmTextKey = "confirm_text"

// VerifyConfirmTyped enforces Action.confirm_typed ON THE SERVER: the text in
// payload.confirm_text must equal the CURRENT value of ct.Field in the
// acted-on record (compared after trimming surrounding whitespace, otherwise
// exact and case-sensitive). A nil ct means the action does not require it.
// An empty or missing record value can never be confirmed, so a misconfigured
// action fails closed instead of accepting an empty string.
//
// Hosts call it for ANY handler type before dispatching; Client.Do calls it
// too when Request.ConfirmTyped is set.
func VerifyConfirmTyped(ct *v3.ConfirmTyped, record, payload map[string]any) error {
	if ct == nil {
		return nil
	}
	want, ok := scalarString(record[ct.Field])
	if !ok || want == "" {
		return fmt.Errorf("%w: record has no value in %q to confirm", ErrConfirmMismatch, ct.Field)
	}
	got, _ := scalarString(payload[ConfirmTextKey])
	if got == "" {
		return fmt.Errorf("%w: type the value of %q to confirm", ErrConfirmMismatch, ct.Field)
	}
	if strings.TrimSpace(got) != want {
		return fmt.Errorf("%w: the text typed does not match %q", ErrConfirmMismatch, ct.Field)
	}
	return nil
}

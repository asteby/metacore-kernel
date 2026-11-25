package contract

import (
	"regexp"
	"strings"
)

// The folio goes into an issue title, an HTML comment and an idempotency key,
// so it is a closed character list (no '<', '>', spaces or '@'). These are
// the exact expressions the Hub enforces; they must not drift. The patterns
// are exported for ports to other languages (the PHP client copies them).
const (
	FolioPattern     = `^[A-Za-z0-9][A-Za-z0-9._/-]{0,63}$`
	MessageIDPattern = `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`
	PriorityPattern  = `^[A-Za-z0-9_-]{1,32}$`
)

var (
	folioRe     = regexp.MustCompile(FolioPattern)
	messageIDRe = regexp.MustCompile(MessageIDPattern)
	priorityRe  = regexp.MustCompile(PriorityPattern)
)

// ValidFolio reports whether s is an acceptable ticket folio (1-64 chars).
// The value is not trimmed: callers trim first if they want to be lenient.
func ValidFolio(s string) bool { return folioRe.MatchString(s) }

// ValidMessageID reports whether s is an acceptable message id or
// client_message_id (1-128 chars).
func ValidMessageID(s string) bool { return messageIDRe.MatchString(s) }

// ValidPriority reports whether s is an acceptable free-form ticket priority.
func ValidPriority(s string) bool { return priorityRe.MatchString(s) }

// ValidAuthorType reports whether s is reporter or agent, the author types a
// /message request accepts.
func ValidAuthorType(s string) bool { return s == AuthorReporter || s == AuthorAgent }

func lowerTrim(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

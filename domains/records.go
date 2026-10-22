package domains

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// RecordType is a DNS record type this package reasons about.
type RecordType string

const (
	TypeA     RecordType = "A"
	TypeAAAA  RecordType = "AAAA"
	TypeCNAME RecordType = "CNAME"
	TypeMX    RecordType = "MX"
	TypeTXT   RecordType = "TXT"
)

// Valid reports whether t is supported.
func (t RecordType) Valid() bool {
	switch t {
	case TypeA, TypeAAAA, TypeCNAME, TypeMX, TypeTXT:
		return true
	}
	return false
}

// MatchMode says how an observed TXT value satisfies a required one.
type MatchMode string

const (
	// MatchExact (default): equal after trimming quotes/whitespace.
	MatchExact MatchMode = ""
	// MatchContains: the observed value contains the required one (an SPF
	// that must include a mechanism among the tenant's own).
	MatchContains MatchMode = "contains"
	// MatchPrefix: the observed value starts with the required one (a DMARC
	// policy whose tag tail the tenant tunes).
	MatchPrefix MatchMode = "prefix"
)

// Record is one required DNS record.
type Record struct {
	Type RecordType `json:"type"`
	// Name is the absolute owner name, lower-case, without trailing dot
	// ("_dmarc.example.com"). Use Rel/Abs helpers to build it.
	Name string `json:"name"`
	// Value is the record data: TXT text (unquoted), MX target host, CNAME
	// target, IPv4/IPv6 literal.
	Value string `json:"value"`
	// Priority is the MX preference (required, 0..65535, for MX only).
	Priority int `json:"priority,omitempty"`
	// TTL in seconds; 0 lets the provider choose (auto).
	TTL int `json:"ttl,omitempty"`
	// Purpose is a free label for the UI and diagnostics: "ownership", "mx",
	// "spf", "dkim", "dmarc", "mta_sts", "tls_rpt", "return_path".
	Purpose string `json:"purpose,omitempty"`
	// Match applies to TXT only; see MatchMode.
	Match MatchMode `json:"match,omitempty"`
	// Needle is what contains/prefix matching looks for, when it differs from
	// Value. Value is what the platform WRITES (a complete SPF or DMARC
	// record); Needle is the part it REQUIRES ("include:_spf.mail.example",
	// "v=DMARC1") so a tenant's own extra mechanisms or tuned policy still
	// verify. Empty = Value.
	Needle string `json:"needle,omitempty"`
	// Unique marks a name+type that must hold exactly one value (SPF, DMARC,
	// a DKIM selector, a CNAME): applying UPDATES an existing different value
	// instead of adding a second one.
	Unique bool `json:"unique,omitempty"`
	// Optional records are recommended: reported, but they never keep a
	// RecordSetCheck from being AllOK.
	Optional bool `json:"optional,omitempty"`
}

// Key identifies the record slot (type + owner name).
func (r Record) Key() string { return string(r.Type) + "|" + r.Name }

// Canonical is the comparable value form: MX "10 host", TXT trimmed, hosts
// lower-cased without trailing dot, IPs in canonical text.
func (r Record) Canonical() string {
	switch r.Type {
	case TypeMX:
		return strconv.Itoa(r.Priority) + " " + canonHost(r.Value)
	case TypeCNAME:
		return canonHost(r.Value)
	case TypeA, TypeAAAA:
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Value)); err == nil {
			return a.Unmap().String()
		}
		return strings.TrimSpace(r.Value)
	}
	return canonTXT(r.Value)
}

func (r Record) String() string { return fmt.Sprintf("%s %s %s", r.Type, r.Name, r.Canonical()) }

func canonHost(v string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(v), "."))
}

// canonTXT trims whitespace and one pair of surrounding quotes (providers such
// as Cloudflare return TXT content quoted).
func canonTXT(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	return strings.TrimSpace(v)
}

// Abs joins a relative label onto a domain: Abs("example.com","_dmarc") ->
// "_dmarc.example.com"; "@" or "" is the apex.
func Abs(domain, rel string) string {
	rel = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(rel)), ".")
	domain = strings.ToLower(domain)
	if rel == "" || rel == "@" {
		return domain
	}
	return rel + "." + domain
}

// Validate checks one record.
func (r Record) Validate() error {
	if !r.Type.Valid() {
		return fmt.Errorf("record type %q is not supported (A|AAAA|CNAME|MX|TXT)", r.Type)
	}
	if r.Name == "" || r.Name != strings.ToLower(strings.TrimSuffix(r.Name, ".")) || len(r.Name) > MaxHostLen {
		return fmt.Errorf("record name %q must be a lower-case absolute name without trailing dot", r.Name)
	}
	for _, l := range strings.Split(r.Name, ".") {
		if l == "" || len(l) > 63 || strings.ContainsAny(l, " \t\"") {
			return fmt.Errorf("record name %q has an invalid label", r.Name)
		}
	}
	if strings.TrimSpace(r.Value) == "" {
		return fmt.Errorf("record %s %s: value is required", r.Type, r.Name)
	}
	if r.TTL < 0 || (r.TTL != 0 && r.TTL < 60) || r.TTL > 86400*7 {
		return fmt.Errorf("record %s %s: ttl must be 0 (auto) or 60..604800", r.Type, r.Name)
	}
	if r.Priority != 0 && r.Type != TypeMX {
		return fmt.Errorf("record %s %s: priority is only valid for MX", r.Type, r.Name)
	}
	if r.Match != MatchExact && r.Type != TypeTXT {
		return fmt.Errorf("record %s %s: match is only valid for TXT", r.Type, r.Name)
	}
	if r.Needle != "" && r.Match != MatchContains && r.Match != MatchPrefix {
		return fmt.Errorf("record %s %s: needle requires match contains or prefix", r.Type, r.Name)
	}
	switch r.Match {
	case MatchExact, MatchContains, MatchPrefix:
	default:
		return fmt.Errorf("record %s %s: match %q must be contains|prefix or empty", r.Type, r.Name, r.Match)
	}
	switch r.Type {
	case TypeMX:
		if r.Priority < 0 || r.Priority > 65535 {
			return fmt.Errorf("record MX %s: priority must be 0..65535", r.Name)
		}
		if err := checkTarget(r.Value); err != nil {
			return fmt.Errorf("record MX %s: %w", r.Name, err)
		}
	case TypeCNAME:
		if err := checkTarget(r.Value); err != nil {
			return fmt.Errorf("record CNAME %s: %w", r.Name, err)
		}
	case TypeA:
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Value)); err != nil || !a.Is4() {
			return fmt.Errorf("record A %s: %q is not an IPv4 address", r.Name, r.Value)
		}
	case TypeAAAA:
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Value)); err != nil || !a.Is6() || a.Is4In6() {
			return fmt.Errorf("record AAAA %s: %q is not an IPv6 address", r.Name, r.Value)
		}
	case TypeTXT:
		if len(canonTXT(r.Value)) > 4096 {
			return fmt.Errorf("record TXT %s: value exceeds 4096 bytes", r.Name)
		}
		if strings.ContainsAny(r.Value, "\r\n") {
			return fmt.Errorf("record TXT %s: value cannot contain line breaks", r.Name)
		}
	}
	return nil
}

func checkTarget(v string) error {
	h := canonHost(v)
	if h == "" || strings.ContainsAny(h, " \t/") {
		return fmt.Errorf("target %q is not a host name", v)
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return fmt.Errorf("target %q must be a host name, not an IP", v)
	}
	return nil
}

// RecordSet is what a domain purpose REQUIRES in DNS.
type RecordSet struct {
	// Domain is the apex the set was generated for ("example.com").
	Domain  string   `json:"domain"`
	Records []Record `json:"records"`
}

// Validate checks every record, that names live under Domain, that there are
// no duplicate (type,name,value) entries, and that a CNAME owns its name alone
// (RFC 1034 §3.6.2).
func (s RecordSet) Validate() error {
	if err := ValidateHost(s.Domain); err != nil {
		return fmt.Errorf("recordset domain: %w", err)
	}
	if len(s.Records) == 0 {
		return errors.New("recordset has no records")
	}
	seen := map[string]struct{}{}
	byName := map[string]map[RecordType]int{}
	for i, r := range s.Records {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("records[%d]: %w", i, err)
		}
		if r.Name != s.Domain && !strings.HasSuffix(r.Name, "."+s.Domain) {
			return fmt.Errorf("records[%d]: name %q is outside domain %q", i, r.Name, s.Domain)
		}
		id := r.Key() + "|" + r.Canonical()
		if _, dup := seen[id]; dup {
			return fmt.Errorf("records[%d]: duplicate %s", i, r)
		}
		seen[id] = struct{}{}
		if byName[r.Name] == nil {
			byName[r.Name] = map[RecordType]int{}
		}
		byName[r.Name][r.Type]++
	}
	for name, types := range byName {
		if types[TypeCNAME] > 0 && len(types) > 1 {
			return fmt.Errorf("name %q mixes a CNAME with other record types", name)
		}
		if types[TypeCNAME] > 1 {
			return fmt.Errorf("name %q has more than one CNAME", name)
		}
	}
	return nil
}

// Slots groups the records by Key, in first-appearance order.
func (s RecordSet) Slots() map[string][]Record {
	out := map[string][]Record{}
	for _, r := range s.Records {
		out[r.Key()] = append(out[r.Key()], r)
	}
	return out
}

// SortedKeys returns the slot keys sorted (stable iteration for tests/UIs).
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

package contract

import (
	"strconv"
	"strings"
)

// Contract versions. v2 is v1 plus optional fields, so a v1 client keeps
// working unchanged against a v2 server.
const (
	ContractV1 = 1
	ContractV2 = 2

	// ContractCurrent is the highest version this package describes.
	ContractCurrent = ContractV2
)

// HeaderContract is the optional request header that negotiates the contract
// version. A missing or malformed header means v1.
const HeaderContract = "X-Support-Contract"

// ParseContract reads a contract version from a header value. Anything that
// is not a positive integer (empty, text, zero, negative) yields ContractV1;
// a version above ContractCurrent is returned as is so the server can decide
// whether to answer contract_upgrade_required or downgrade.
func ParseContract(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < ContractV1 {
		return ContractV1
	}
	return n
}

// EffectiveContract resolves the version of a request from its header value
// and its body field "contract" (0 when absent). The header wins when it is
// present.
func EffectiveContract(header string, body int) int {
	if strings.TrimSpace(header) != "" {
		return ParseContract(header)
	}
	if body >= ContractV1 {
		return body
	}
	return ContractV1
}

package capability

import "time"

// Well-known region capability keys. Region packs (region_mx …) provide them
// from their own postal tables; the kernel ships no data.
const (
	// RegionPostalLocate resolves a postal code to the centroid of its area
	// plus city and state. It is an approximation for maps, distance-based
	// shipping and store pickers; never a delivery address.
	RegionPostalLocate = "region.postal.locate"
)

// PostalPrecisions is the closed vocabulary of region.postal.locate
// precision: the point is always the centroid of the postal code area.
var PostalPrecisions = []string{"postal_centroid"}

// RegionPostalTimeout is the region.postal.locate deadline: a table lookup on
// interactive paths (checkout address, store picker).
const RegionPostalTimeout = 2 * time.Second

// Error codes region.postal.locate providers report (NewCodedError).
const (
	// ErrCodeInvalidInput: the provider rejected the input (a code that is
	// well-formed but not valid for its country).
	ErrCodeInvalidInput = "invalid_input"
	// ErrCodeNotFound: the postal code is not in the provider's table.
	ErrCodeNotFound = "not_found"
)

func init() {
	for _, c := range regionContracts() {
		mustRegister(c)
	}
}

func regionContracts() []Contract {
	return []Contract{
		{
			Key: RegionPostalLocate, Kind: KindQuery, Cardinality: One, Timeout: RegionPostalTimeout,
			Doc: "postal code to the centroid of its area, with city and state",
			Input: schema(
				pattern(req("postal_code", TypeString, "5-digit postal code (MX código postal)"), `^[0-9]{5}$`),
			),
			Output: schema(
				req("lat", TypeNumber, "WGS84 latitude of the postal code centroid, decimal degrees [-90, 90]"),
				req("lng", TypeNumber, "WGS84 longitude of the postal code centroid, decimal degrees [-180, 180]"),
				opt("city", TypeString, "city or municipality; omitted when the postal table has none"),
				req("state", TypeString, "state (name or code, as the region pack publishes it)"),
				enum(req("precision", TypeString, "always postal_centroid: draw it as an area, never as a rooftop pin"), PostalPrecisions),
			),
			Errors: []string{ErrCodeInvalidInput, ErrCodeNotFound},
		},
	}
}

func pattern(f Field, re string) Field { f.Pattern = re; return f }

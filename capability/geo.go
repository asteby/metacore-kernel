package capability

import "time"

// Well-known geo capability keys. Contracts only: the kernel ships no
// geocoder or router. A provider addon (centroides de CP de region_mx,
// a self-hosted Nominatim / OSRM / Valhalla) declares them in
// provides_capabilities and the host selects it per org like any other
// capability. Without a provider, consumers degrade on their own (points by
// postal code, straight lines between stops). Coordinates are always WGS84
// decimal degrees, latitude and longitude as separate numbers (never a
// "lat,lng" string, never [lng, lat] outside GeoJSON). Reference:
// docs/capabilities-geo.md.
const (
	GeoGeocode = "geo.geocode"
	GeoRoute   = "geo.route"
	// GeoAddressSuggest is the type-ahead of an address form: a few
	// normalized candidates (street, number, colonia, postal code) for what the
	// shopper has typed so far. It is a SUGGESTION source, never an authority:
	// the region pack's postal catalog (region.postal.locate) is the only one
	// that may lock a state or city. Separate from geo.geocode because its
	// deadline, volume (one call per pause in typing) and caching differ.
	GeoAddressSuggest = "geo.address.suggest"
)

// Geo vocabularies.
var (
	// GeoPrecisions is how exact a geocoded point is, finest first. A postal
	// code centroid is "postal_code"; consumers must not draw it as a
	// rooftop pin.
	GeoPrecisions = []string{"rooftop", "street", "neighborhood", "postal_centroid", "locality", "region", "country"}
	// RouteProfiles is the travel mode of a route.
	RouteProfiles = []string{"driving", "truck", "cycling", "walking"}
)

// Geo deadlines: both run on interactive paths (an address form, a map
// panel), so a slow provider must give up quickly.
const (
	GeocodeTimeout = 3 * time.Second
	RouteTimeout   = 5 * time.Second
	// AddressSuggestTimeout is shorter on purpose: type-ahead that answers
	// after a second is worse than no type-ahead, and the form keeps working
	// without it.
	AddressSuggestTimeout = 1 * time.Second
)

// MaxAddressSuggestResults is the most candidates a geo.address.suggest
// caller may ask for; providers clamp larger limits to it.
const MaxAddressSuggestResults = 8

// MinAddressSuggestQuery is the shortest query (in characters, without the
// surrounding blanks) a provider is asked about. The host rejects shorter
// ones before invoking anyone, so no third party sees a half-typed word.
const MinAddressSuggestQuery = 3

// MaxRouteWaypoints is the largest waypoint list a geo.route caller may send.
const MaxRouteWaypoints = 25

var (
	// GeoPointSchema is one WGS84 coordinate pair.
	GeoPointSchema = schema(
		req("lat", TypeLatitude, "WGS84 latitude"),
		req("lng", TypeLongitude, "WGS84 longitude"),
	)

	// GeoPlaceSchema is one geocoding match.
	GeoPlaceSchema = schema(
		req("lat", TypeLatitude, "WGS84 latitude"),
		req("lng", TypeLongitude, "WGS84 longitude"),
		enum(req("precision", TypeString, "how exact the point is; a postal code centroid is postal_centroid"), GeoPrecisions),
		opt("label", TypeString, "formatted address / place name"),
		opt("postal_code", TypeString, ""),
		opt("street", TypeString, "street name without the house number"),
		opt("house_number", TypeString, "house or building number as written (a string: 12-B, S/N)"),
		opt("neighborhood", TypeString, "colonia / district"),
		opt("city", TypeString, "city / municipio"),
		opt("state", TypeString, "state / province (code or name)"),
		opt("country", TypeCountry, "ISO 3166-1 alpha-2"),
		opt("place_id", TypeString, "opaque provider id of the match; stable enough to re-resolve it, never to be parsed"),
		opt("confidence", TypeNumber, "0..1, provider-defined"),
		nums(opt("bbox", TypeArray, "[west, south, east, north] in degrees, when the match is an area")),
	)

	// RouteLegSchema is the stretch between two consecutive waypoints.
	RouteLegSchema = schema(
		req("distance_m", TypeNumber, "metres"),
		req("duration_s", TypeNumber, "seconds"),
	)
)

func object(f Field, of *Schema) Field { f.Type = TypeObject; f.Of = of; return f }

func nums(f Field) Field { f.Type = TypeArray; f.Elem = TypeNumber; return f }

func init() {
	for _, c := range geoContracts() {
		mustRegister(c)
	}
}

func geoContracts() []Contract {
	return []Contract{
		{
			Key: GeoGeocode, Kind: KindQuery, Cardinality: One, Timeout: GeocodeTimeout,
			Doc: "text or postal code to coordinates (send query and/or postal_code)",
			Input: schema(
				opt("query", TypeString, "free-text address or place; one of query / postal_code is needed"),
				opt("postal_code", TypeString, "postal code; alone it resolves to the code's centroid"),
				opt("neighborhood", TypeString, ""),
				opt("city", TypeString, ""),
				opt("state", TypeString, ""),
				opt("country", TypeCountry, "ISO 3166-1 alpha-2; restricts the search"),
				opt("limit", TypeInteger, "max results (provider default 1, at most 10)"),
				opt("locale", TypeString, "language of the labels (es, en…)"),
			),
			Output: schema(
				list(req("results", TypeArray, "best match first; empty when nothing matched"), GeoPlaceSchema),
			),
		},
		{
			Key: GeoAddressSuggest, Kind: KindQuery, Cardinality: One, Timeout: AddressSuggestTimeout,
			Doc: "address candidates for a partially typed street (type-ahead of an address form)",
			Input: schema(
				pattern(req("query", TypeString, "what the shopper typed: at least 3 characters once trimmed"), `(?s)^\s*\S.+\S\s*$`),
				opt("country", TypeCountry, "ISO 3166-1 alpha-2; restricts the search (the market's country)"),
				opt("postal_code", TypeString, "postal code already chosen in the form; biases and filters the candidates"),
				object(opt("near", TypeObject, "bias point (e.g. the postal code centroid); never a filter"), GeoPointSchema),
				opt("limit", TypeInteger, "max candidates, 1..8 (providers clamp; default 5)"),
				opt("locale", TypeString, "language of the labels (es, en…)"),
			),
			Output: schema(
				list(req("results", TypeArray, "best candidate first, at most 8; empty when nothing matched"), GeoPlaceSchema),
			),
		},
		{
			Key: GeoRoute, Kind: KindQuery, Cardinality: One, Timeout: RouteTimeout,
			Doc: "road distance, duration and optional geometry through ordered waypoints",
			Input: schema(
				list(req("waypoints", TypeArray, "origin first, destination last; 2..25 points"), GeoPointSchema),
				enum(opt("profile", TypeString, "default driving"), RouteProfiles),
				opt("geometry", TypeBoolean, "return the route geometry (default false: distance and duration only)"),
			),
			Output: schema(
				req("distance_m", TypeNumber, "total metres"),
				req("duration_s", TypeNumber, "total seconds"),
				opt("geometry", TypeObject, "GeoJSON LineString {type, coordinates: [[lng, lat], …]}, only when requested"),
				list(opt("legs", TypeArray, "one per consecutive waypoint pair"), RouteLegSchema),
				opt("approximate", TypeBoolean, "true when the provider fell back to straight lines (no road network)"),
			),
		},
	}
}

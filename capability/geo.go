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
)

// Geo vocabularies.
var (
	// GeoPrecisions is how exact a geocoded point is, finest first. A postal
	// code centroid is "postal_code"; consumers must not draw it as a
	// rooftop pin.
	GeoPrecisions = []string{"rooftop", "street", "neighborhood", "postal_code", "locality", "region", "country"}
	// RouteProfiles is the travel mode of a route.
	RouteProfiles = []string{"driving", "truck", "cycling", "walking"}
)

// Geo deadlines: both run on interactive paths (an address form, a map
// panel), so a slow provider must give up quickly.
const (
	GeocodeTimeout = 3 * time.Second
	RouteTimeout   = 5 * time.Second
)

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
		enum(req("precision", TypeString, "how exact the point is; a postal code centroid is postal_code"), GeoPrecisions),
		opt("label", TypeString, "formatted address / place name"),
		opt("postal_code", TypeString, ""),
		opt("neighborhood", TypeString, "colonia / district"),
		opt("city", TypeString, "city / municipio"),
		opt("state", TypeString, "state / province (code or name)"),
		opt("country", TypeCountry, "ISO 3166-1 alpha-2"),
		opt("confidence", TypeNumber, "0..1, provider-defined"),
		nums(opt("bbox", TypeArray, "[west, south, east, north] in degrees, when the match is an area")),
	)

	// RouteLegSchema is the stretch between two consecutive waypoints.
	RouteLegSchema = schema(
		req("distance_m", TypeNumber, "metres"),
		req("duration_s", TypeNumber, "seconds"),
	)
)

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

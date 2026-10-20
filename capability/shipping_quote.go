package capability

// Shipping quote and label contracts, additive evolution (shipping system,
// wave 0). Reference: docs/capabilities-commerce.md, "Cotización y guía de
// envíos", and docs/adr/0001-contratos-envios-aditivos.md.
//
// Same rule as the embedded payment contracts: commerce.shipping.rates and
// commerce.shipping.label keep their keys (never a version suffix) and evolve
// ONLY by optional input fields, optional output fields, enum values appended
// at the end and new shared shapes. Nothing is removed, renamed, retyped or
// made required, so a provider (envia, skydropx, noventa9minutos,
// shipping_flat...) built against the earlier shape keeps validating, and a
// host may ignore every new output field. The events
// commerce.shipping.tracking_updated and commerce.fulfillment.updated are
// untouched.
//
// What the kernel does NOT decide here: the cart owns the final price (the
// 'free' policies coupon, market_policy and shop_threshold, the shipping tax)
// and the immutable quote snapshot; the contract only carries the provider's
// list price, its own free reason and the facts the snapshot freezes.

const (
	// DatePattern is a calendar date, YYYY-MM-DD (the shape; not a calendar check).
	DatePattern = `^[0-9]{4}-[0-9]{2}-[0-9]{2}$`
	// QuoteHashPattern is the digest of a quote: "sha256:" and 64 lower-case hex.
	QuoteHashPattern = `^sha256:[0-9a-f]{64}$`
)

var (
	// DeliveryTypes is the closed set of rates[].delivery_type values (the
	// kernel's own: door, branch, same_day, on_demand, pickup). Hosts translate
	// to it; "home" and "pickup_point" are not kernel values.
	DeliveryTypes = []string{"door", "branch", "same_day", "on_demand", "pickup"}

	// ShippingFreeReasons is the closed set of reasons a shipping price is zero
	// or below its list price, as the order records it. The cart decides
	// which applies (precedence: coupon, market_policy, shop_threshold,
	// rate_free_over, rate_free). Append only.
	ShippingFreeReasons = []string{"none", "rate_free", "rate_free_over", "market_policy", "shop_threshold", "coupon", "staff_override", "pickup_free"}

	// ShippingRateFreeReasons is the subset a PROVIDER may answer in
	// rates[].free_reason: the cart-side policies (coupon, market_policy,
	// shop_threshold) are decided by the host, never by a carrier. Append only.
	ShippingRateFreeReasons = []string{"none", "rate_free", "rate_free_over", "staff_override", "pickup_free"}

	// ShippingPurposes is the direction of a label.
	ShippingPurposes = []string{"outbound", "return"}

	// ShippingOriginSchema is one candidate ship-from location: an Address plus
	// the location id in the host (multi-origin quote).
	ShippingOriginSchema = &Schema{Fields: append([]Field{
		req("location_id", TypeString, "location id in the host"),
	}, AddressSchema.Fields...)}

	// ShippingRuleRefSchema points at the provider rule behind a price.
	ShippingRuleRefSchema = schema(
		req("kind", TypeString, "rule family, e.g. shipping_flat_rate"),
		req("id", TypeString, "rule id at the provider"),
		opt("version", TypeString, "rule version / revision, when the provider versions rules"),
	)

	// DeliveryWindowSchema is the promised delivery range, calendar dates.
	DeliveryWindowSchema = schema(
		pat(req("from", TypeString, "earliest promised date, YYYY-MM-DD"), DatePattern),
		pat(req("to", TypeString, "latest promised date, YYYY-MM-DD"), DatePattern),
	)

	// PickupPointSchema is a place the buyer collects the parcel.
	PickupPointSchema = schema(
		req("id", TypeString, "stable id at the provider"),
		req("name", TypeString, ""),
		obj(opt("address", TypeObject, ""), AddressSchema),
		opt("hours", TypeString, "opening hours, free text"),
		opt("lat", TypeLatitude, ""),
		opt("lng", TypeLongitude, ""),
	)

	// RateInsuranceSchema is the insurance a rate offers or includes.
	RateInsuranceSchema = schema(
		opt("available", TypeBoolean, "the buyer can add insurance (label insurance_value)"),
		opt("included", TypeBoolean, "cost already includes insurance"),
		opt("cost", TypeDecimal, "price of the insurance, in the rate currency"),
	)
)

// pat sets the regular expression a string field must match.
func pat(f Field, pattern string) Field { f.Pattern = pattern; return f }

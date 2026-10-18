package capability

import "time"

// Well-known commerce and fiscal capability keys. The storefront core
// (cart, checkout, order) only ever names these keys; which addon answers
// (mercadopago, stripe, skydropx, envia, region_mx, factura_com …) is an
// install + per-market selection decision. Money is ALWAYS a decimal string
// next to an ISO 4217 currency. The reference with every field lives in
// docs/capabilities-commerce.md.
const (
	CommercePaymentsSession   = "commerce.payments.session"
	CommercePaymentsConfirm   = "commerce.payments.confirm"
	CommercePaymentsRefund    = "commerce.payments.refund"
	CommercePaymentsStatus    = "commerce.payments.status"
	CommerceTaxCalculate      = "commerce.tax.calculate"
	CommerceTaxCommit         = "commerce.tax.commit"
	CommerceTaxReverse        = "commerce.tax.reverse"
	CommerceShippingRates     = "commerce.shipping.rates"
	CommerceShippingLabel     = "commerce.shipping.label"
	CommerceShippingTrack     = "commerce.shipping.track"
	CommerceShippingCancel    = "commerce.shipping.cancel"
	CommerceFulfillmentCreate = "commerce.fulfillment.create"
	CommerceFulfillmentCancel = "commerce.fulfillment.cancel"
	CommerceSearchQuery       = "commerce.search.query"
	CommerceDiscountsEvaluate = "commerce.discounts.evaluate"
	FiscalInvoiceCreate       = "fiscal.invoice.create"
	FiscalInvoiceCancel       = "fiscal.invoice.cancel"
)

// CommercePaymentsConfirmed is the normalized payment-update event every
// payment provider emits from its own webhook (or staff action, for manual
// payments): {provider, result}, where result is the output of
// commerce.payments.confirm (result.kind: payment | refund | dispute). Only
// providers of a commerce.payments.* capability may emit it; the host stamps
// the real publisher as "emitter" on delivery, and the consumer re-reads the
// payment with commerce.payments.status pinned to the session's provider
// before acting on it.
const CommercePaymentsConfirmed = "commerce.payments.confirmed"

// CommerceShippingTrackingUpdated is the normalized parcel-status event every
// carrier emits when a shipment it created moves (webhook or poll). Only
// providers of a commerce.shipping.* capability may emit it (the domain is
// reserved), so a non-carrier cannot forge a "delivered" notice. The payload
// is TrackingUpdatedSchema; dedupe on emitter + event_id.
const CommerceShippingTrackingUpdated = "commerce.shipping.tracking_updated"

// CommerceFulfillmentUpdated is the normalized status event every warehouse /
// 3PL emits when a fulfillment it accepted (commerce.fulfillment.create)
// moves. Only providers of a commerce.fulfillment.* capability may emit it.
// The payload is FulfillmentUpdatedSchema; dedupe on emitter + event_id.
const CommerceFulfillmentUpdated = "commerce.fulfillment.updated"

// Normalized status vocabularies shared by providers.
var (
	// PaymentStatuses is the normalized payment lifecycle.
	PaymentStatuses = []string{"pending", "requires_action", "authorized", "paid", "failed", "cancelled", "expired", "refunded", "partially_refunded"}
	// TrackingStatuses is the normalized parcel lifecycle every carrier
	// status is mapped onto.
	TrackingStatuses = []string{"created", "picked_up", "in_transit", "out_for_delivery", "delivered", "exception", "returned", "cancelled"}
	// FulfillmentStatuses is the normalized fulfillment lifecycle: the
	// statuses commerce.fulfillment.create answers (accepted, on_hold,
	// rejected) followed by the parcel lifecycle of the shipment the
	// warehouse dispatches.
	FulfillmentStatuses = append([]string{"accepted", "on_hold", "rejected"}, TrackingStatuses...)
)

// Default per-provider deadlines. Fan-out queries are tighter: a slow carrier
// must never hold the checkout.
const (
	DefaultQueryTimeout = 8 * time.Second
	// MutatingTimeout is the deadline of idempotent (money / irreversible)
	// contracts: carriers and tax/fiscal authorities are slow, and giving up
	// early only produces an indeterminate outcome.
	MutatingTimeout      = 30 * time.Second
	ShippingRatesTimeout = 4 * time.Second
	SearchTimeout        = 2 * time.Second
	DiscountsTimeout     = 2 * time.Second
)

// IdempotencyDoc documents the required key of mutating contracts.
const IdempotencyDoc = "required; stable per business operation (reuse it on every retry); the provider must dedupe on it"

func req(name string, t FieldType, doc string) Field {
	return Field{Name: name, Type: t, Required: true, Doc: doc}
}

func opt(name string, t FieldType, doc string) Field {
	return Field{Name: name, Type: t, Doc: doc}
}

func enum(f Field, values []string) Field { f.Enum = values; return f }

func obj(f Field, s *Schema) Field { f.Of = s; return f }

func list(f Field, s *Schema) Field { f.Type = TypeArray; f.Of = s; return f }

func strs(f Field) Field { f.Type = TypeArray; f.Elem = TypeString; return f }

func schema(fields ...Field) *Schema { return &Schema{Fields: fields} }

// Shared shapes. Addresses are region-neutral: region packs (region_mx) add
// validation (CP → estado/municipio/colonia) on top, never new required keys.
var (
	// AddressSchema is a postal address / contact point.
	AddressSchema = &Schema{Open: false, Fields: []Field{
		opt("name", TypeString, "contact name"),
		opt("company", TypeString, "company name"),
		opt("line1", TypeString, "street and number"),
		opt("line2", TypeString, "interior, suite"),
		opt("neighborhood", TypeString, "colonia / district"),
		opt("city", TypeString, "city / municipio"),
		opt("state", TypeString, "state / province (code or name)"),
		req("postal_code", TypeString, "postal code"),
		req("country", TypeCountry, "ISO 3166-1 alpha-2"),
		opt("phone", TypeString, "E.164 preferred"),
		opt("email", TypeString, "contact email"),
		opt("reference", TypeString, "delivery reference / between streets"),
	}}

	// BuyerSchema is the paying customer.
	BuyerSchema = schema(
		opt("id", TypeString, "customer id in OPS"),
		opt("email", TypeString, ""),
		opt("name", TypeString, ""),
		opt("phone", TypeString, ""),
		opt("tax_id", TypeString, "RFC / VAT id"),
	)

	// LineSchema is an order/cart line as payments and tax see it.
	LineSchema = schema(
		req("id", TypeString, "line id, echoed back in per-line outputs"),
		opt("sku", TypeString, ""),
		opt("title", TypeString, ""),
		req("quantity", TypeInteger, ""),
		req("unit_price", TypeDecimal, "unit price in the request currency"),
		opt("discount", TypeDecimal, "line discount amount (total, not per unit)"),
		opt("tax_code", TypeString, "provider/fiscal product tax code"),
		opt("product_ref", TypeString, "product/variant id"),
	)

	// ParcelSchema is one package to ship. Dimensions in cm, weight in kg,
	// both as decimal strings.
	ParcelSchema = schema(
		req("weight_kg", TypeDecimal, ""),
		opt("length_cm", TypeDecimal, ""),
		opt("width_cm", TypeDecimal, ""),
		opt("height_cm", TypeDecimal, ""),
		opt("quantity", TypeInteger, "identical parcels (default 1)"),
		opt("content", TypeString, "content description (customs/carrier)"),
	)

	// ShipItemSchema is a line handed to a carrier or a warehouse.
	ShipItemSchema = schema(
		req("sku", TypeString, ""),
		req("quantity", TypeInteger, ""),
		opt("title", TypeString, ""),
		opt("id", TypeString, "order line id"),
		opt("unit_price", TypeDecimal, ""),
		opt("product_code", TypeString, "fiscal product key (SAT ClaveProdServ, carta porte)"),
	)

	// TrackingEventSchema is one normalized carrier scan.
	TrackingEventSchema = schema(
		req("at", TypeString, "RFC 3339 timestamp"),
		enum(req("status", TypeString, ""), TrackingStatuses),
		opt("description", TypeString, "carrier wording"),
		opt("location", TypeString, ""),
		opt("lat", TypeLatitude, "WGS84 latitude of the scan, only when the carrier reports coordinates"),
		opt("lng", TypeLongitude, "WGS84 longitude of the scan, only when the carrier reports coordinates"),
	)

	// TrackingUpdatedSchema is the payload of commerce.shipping.tracking_updated.
	TrackingUpdatedSchema = schema(
		req("provider", TypeString, "addon key of the carrier (informative: subscribers trust the host-stamped emitter)"),
		req("event_id", TypeString, "stable per observed status change; subscribers dedupe on emitter + event_id"),
		enum(req("status", TypeString, ""), TrackingStatuses),
		req("occurred_at", TypeString, "RFC 3339 time of the scan / status change"),
		opt("order_ref", TypeString, "the order_ref sent to commerce.shipping.label; absent when the carrier cannot tell"),
		opt("shipment_id", TypeString, "provider shipment id (commerce.shipping.label shipment_id)"),
		opt("carrier", TypeString, "e.g. fedex, estafeta, dhl"),
		opt("tracking_number", TypeString, ""),
		opt("tracking_url", TypeString, ""),
		opt("label_url", TypeString, "set once an asynchronous label became available"),
		opt("label_ready", TypeBoolean, "this update is the one that made the label available"),
		opt("provider_status", TypeString, "carrier wording / code of the status, untranslated"),
		opt("description", TypeString, ""),
		opt("location", TypeString, ""),
		opt("test_mode", TypeBoolean, "update from the provider sandbox"),
	)

	// FulfillmentUpdatedSchema is the payload of commerce.fulfillment.updated.
	FulfillmentUpdatedSchema = schema(
		req("provider", TypeString, "addon key of the warehouse / 3PL (informative: subscribers trust the host-stamped emitter)"),
		req("event_id", TypeString, "stable per observed status change; subscribers dedupe on emitter + event_id"),
		req("fulfillment_id", TypeString, "commerce.fulfillment.create fulfillment_id"),
		req("order_ref", TypeString, "the order_ref sent to commerce.fulfillment.create"),
		enum(req("status", TypeString, ""), FulfillmentStatuses),
		req("occurred_at", TypeString, "RFC 3339"),
		enum(opt("previous_status", TypeString, ""), FulfillmentStatuses),
		opt("warehouse", TypeString, "provider warehouse code"),
		opt("carrier", TypeString, ""),
		opt("tracking_number", TypeString, ""),
		opt("tracking_url", TypeString, ""),
		opt("provider_status", TypeString, "provider wording / code of the status, untranslated"),
		opt("description", TypeString, ""),
		opt("test_mode", TypeBoolean, "update from the provider sandbox"),
	)

	// TaxLineSchema is one computed tax component.
	TaxLineSchema = schema(
		req("name", TypeString, "e.g. IVA, State tax"),
		req("rate", TypeDecimal, "0.16"),
		req("amount", TypeDecimal, ""),
		opt("jurisdiction", TypeString, ""),
	)
)

// IdempotentContracts are the mutating contracts: `idempotency_key` is
// required and a timeout is indeterminate (see Contract.Idempotent).
var IdempotentContracts = map[string]bool{
	CommercePaymentsSession:   true,
	CommercePaymentsRefund:    true,
	CommerceTaxCommit:         true,
	CommerceTaxReverse:        true,
	CommerceShippingLabel:     true,
	CommerceFulfillmentCreate: true,
	FiscalInvoiceCreate:       true,
}

// PinnedContracts are the follow-up contracts that must name the provider
// (Query.Provider) that created the payment / shipment / invoice / tax
// transaction: webhooks, status, refunds, cancels, tracking, commit, reverse.
var PinnedContracts = map[string]bool{
	CommercePaymentsConfirm:   true,
	CommercePaymentsStatus:    true,
	CommercePaymentsRefund:    true,
	CommerceTaxCommit:         true,
	CommerceTaxReverse:        true,
	CommerceShippingCancel:    true,
	CommerceShippingTrack:     true,
	CommerceFulfillmentCancel: true,
	FiscalInvoiceCancel:       true,
}

func init() {
	for _, c := range commerceContracts() {
		mustRegister(c)
	}
	for _, e := range commerceEvents() {
		mustRegisterEvent(e)
	}
}

// commerceEvents are the well-known commerce event contracts. Registered after
// the query contracts because the payment event reuses the confirm output.
func commerceEvents() []EventContract {
	return []EventContract{
		{
			Name: CommerceShippingTrackingUpdated, EmittedBy: "commerce.shipping",
			Doc:     "a carrier observed a new normalized status of a shipment it created (webhook or poll)",
			Payload: TrackingUpdatedSchema,
		},
		{
			Name: CommerceFulfillmentUpdated, EmittedBy: "commerce.fulfillment",
			Doc:     "a warehouse / 3PL observed a new normalized status of a fulfillment it accepted (webhook or poll)",
			Payload: FulfillmentUpdatedSchema,
		},
		{
			Name: CommercePaymentsConfirmed, EmittedBy: "commerce.payments",
			Doc: "a provider verified a payment, refund or dispute update (webhook or staff action)",
			Payload: schema(
				req("provider", TypeString, "addon key of the provider (informative: subscribers trust the host-stamped emitter)"),
				obj(req("result", TypeObject, "output of commerce.payments.confirm; result.kind tells payment / refund / dispute"), Contracts[CommercePaymentsConfirm].Output),
			),
		},
	}
}

func commerceContracts() []Contract {
	cs := commerceContractList()
	// commerce.payments.status answers exactly what confirm answers.
	var confirm *Schema
	for _, c := range cs {
		if c.Key == CommercePaymentsConfirm {
			confirm = c.Output
		}
	}
	for i := range cs {
		if cs[i].Key == CommercePaymentsStatus {
			cs[i].Output = confirm
		}
		if IdempotentContracts[cs[i].Key] {
			cs[i].Idempotent = true
			if cs[i].Timeout == 0 {
				cs[i].Timeout = MutatingTimeout
			}
		}
		cs[i].PinRequired = PinnedContracts[cs[i].Key]
	}
	return cs
}

func commerceContractList() []Contract {
	return []Contract{
		// ---------------------------------------------------------- payments
		{
			Key: CommercePaymentsSession, Kind: KindQuery, Cardinality: One,
			Doc: "create a payment session (checkout/preference/intent) for an order",
			Input: schema(
				req("order_ref", TypeString, "order id/number in OPS (external_reference)"),
				req("amount", TypeDecimal, "total to charge"),
				req("currency", TypeCurrency, ""),
				req("return_url", TypeString, "where the buyer lands after paying"),
				opt("cancel_url", TypeString, ""),
				opt("notify_url", TypeString, "webhook URL the host exposes for commerce.payments.confirm"),
				obj(opt("buyer", TypeObject, ""), BuyerSchema),
				strs(opt("methods", TypeArray, "allowed method families: card / msi / oxxo / spei / wallet / bnpl / bank_transfer / cash")),
				opt("method_hint", TypeString, "preselected method family"),
				obj(opt("breakdown", TypeObject, "amount components (all decimals)"), schema(
					opt("subtotal", TypeDecimal, ""),
					opt("shipping", TypeDecimal, ""),
					opt("discount", TypeDecimal, ""),
					opt("tax", TypeDecimal, ""),
				)),
				obj(opt("shipping_address", TypeObject, ""), AddressSchema),
				opt("market", TypeString, "market code the order was placed in"),
				list(opt("lines", TypeArray, ""), LineSchema),
				opt("description", TypeString, "statement / title"),
				opt("installments", TypeInteger, "max interest-free installments (MSI)"),
				opt("expires_at", TypeString, "RFC 3339"),
				req("idempotency_key", TypeString, IdempotencyDoc),
				opt("metadata", TypeObject, ""),
			),
			Output: schema(
				req("session_id", TypeString, "provider session/preference/intent id"),
				enum(req("status", TypeString, ""), []string{"created", "pending", "requires_action", "authorized", "paid", "failed"}),
				enum(opt("action_type", TypeString, "what the storefront does next"), []string{"redirect", "embed", "voucher", "none"}),
				opt("redirect_url", TypeString, "hosted checkout URL"),
				opt("client_secret", TypeString, "token for an embedded (iframe) widget"),
				opt("embed_url", TypeString, "isolated iframe URL"),
				opt("expires_at", TypeString, "RFC 3339"),
				opt("provider_ref", TypeString, "payment id when known already"),
				obj(opt("instructions", TypeObject, "offline payment instructions (OXXO reference, SPEI CLABE, barcode_url, deadline)"), &Schema{Open: true}),
				opt("test_mode", TypeBoolean, "session created against the provider sandbox"),
			),
		},
		{
			Key: CommercePaymentsConfirm, Kind: KindQuery, Cardinality: One,
			Doc: "verify and normalize a provider webhook/notification into a payment status",
			Input: schema(
				req("payload", TypeAny, "webhook body (decoded JSON, or the raw string)"),
				opt("headers", TypeObject, "request headers, lowercase keys (signature verification)"),
				opt("query", TypeObject, "query-string parameters"),
				opt("raw_body", TypeString, "exact body bytes as text, when the signature covers them"),
			),
			Output: schema(
				req("order_ref", TypeString, "the order_ref sent to commerce.payments.session"),
				enum(req("status", TypeString, ""), PaymentStatuses),
				req("amount", TypeDecimal, "amount of this status (paid/refunded)"),
				req("currency", TypeCurrency, ""),
				req("provider_ref", TypeString, "provider payment id"),
				opt("event_id", TypeString, "provider event id (dedupe on provider+event_id)"),
				enum(opt("kind", TypeString, "default payment"), []string{"payment", "refund", "dispute"}),
				opt("method", TypeString, "card / msi / oxxo / spei / wallet / bnpl / bank_transfer / cash"),
				opt("installments", TypeInteger, ""),
				opt("paid_at", TypeString, "RFC 3339"),
				opt("occurred_at", TypeString, "RFC 3339"),
				opt("amount_refunded", TypeDecimal, ""),
				opt("fee", TypeDecimal, "provider fee"),
				opt("net_amount", TypeDecimal, "amount minus fee"),
				opt("test_mode", TypeBoolean, "event from the provider sandbox"),
			),
		},
		{
			Key: CommercePaymentsStatus, Kind: KindQuery, Cardinality: One,
			Doc: "poll a payment's normalized status (return-URL fallback, reconciliation); same output as confirm",
			Input: schema(
				opt("provider_ref", TypeString, "provider payment/order id"),
				opt("order_ref", TypeString, "lookup by external reference when provider_ref is unknown"),
				opt("session_id", TypeString, ""),
			),
		},
		{
			Key: CommercePaymentsRefund, Kind: KindQuery, Cardinality: One,
			Doc: "refund (fully or partially) a captured payment",
			Input: schema(
				req("provider_ref", TypeString, "provider payment id"),
				req("amount", TypeDecimal, ""),
				req("currency", TypeCurrency, ""),
				opt("order_ref", TypeString, ""),
				opt("reason", TypeString, ""),
				req("idempotency_key", TypeString, IdempotencyDoc),
			),
			Output: schema(
				req("refund_id", TypeString, ""),
				enum(req("status", TypeString, "manual_required: cash/BNPL refunds the merchant settles by hand"), []string{"pending", "succeeded", "failed", "manual_required"}),
				req("amount", TypeDecimal, ""),
				req("currency", TypeCurrency, ""),
				opt("instructions", TypeString, "what the merchant must do when manual_required"),
			),
		},
		// --------------------------------------------------------------- tax
		{
			Key: CommerceTaxCalculate, Kind: KindQuery, Cardinality: One,
			Doc: "compute taxes for cart lines and shipping",
			Input: schema(
				req("currency", TypeCurrency, ""),
				list(req("lines", TypeArray, ""), LineSchema),
				obj(opt("ship_to", TypeObject, ""), AddressSchema),
				obj(opt("ship_from", TypeObject, ""), AddressSchema),
				opt("shipping_amount", TypeDecimal, "shipping charged to the buyer"),
				opt("prices_include_tax", TypeBoolean, "unit prices already include tax"),
				obj(opt("customer", TypeObject, ""), BuyerSchema),
				opt("order_ref", TypeString, ""),
			),
			Output: schema(
				req("currency", TypeCurrency, ""),
				req("tax_included", TypeBoolean, "true when line prices already contain the tax"),
				req("total_tax", TypeDecimal, ""),
				list(req("lines", TypeArray, ""), schema(
					req("id", TypeString, "input line id"),
					req("tax", TypeDecimal, "tax amount of the line"),
					opt("taxable_amount", TypeDecimal, ""),
					list(opt("breakdown", TypeArray, ""), TaxLineSchema),
				)),
				opt("shipping_tax", TypeDecimal, ""),
				opt("calculation_id", TypeString, "provider calculation id, passed to commerce.tax.commit"),
			),
		},
		{
			Key: CommerceTaxCommit, Kind: KindQuery, Cardinality: One,
			Doc: "record the taxes of a paid order with the provider (no-op for stateless providers)",
			Input: schema(
				req("order_ref", TypeString, ""),
				opt("calculation_id", TypeString, ""),
				opt("currency", TypeCurrency, ""),
				list(opt("lines", TypeArray, ""), LineSchema),
				opt("total_tax", TypeDecimal, ""),
				obj(opt("ship_to", TypeObject, ""), AddressSchema),
				opt("paid_at", TypeString, "RFC 3339"),
				req("idempotency_key", TypeString, IdempotencyDoc),
			),
			Output: schema(
				enum(req("status", TypeString, ""), []string{"committed", "skipped"}),
				opt("transaction_id", TypeString, ""),
			),
		},
		{
			Key: CommerceTaxReverse, Kind: KindQuery, Cardinality: One,
			Doc: "reverse (fully or partially) a committed tax transaction after a refund/return",
			Input: schema(
				req("order_ref", TypeString, ""),
				opt("transaction_id", TypeString, ""),
				list(opt("lines", TypeArray, "partial reversal; absent = full"), LineSchema),
				opt("amount", TypeDecimal, ""),
				opt("currency", TypeCurrency, ""),
				opt("reason", TypeString, ""),
				req("idempotency_key", TypeString, IdempotencyDoc),
			),
			Output: schema(
				enum(req("status", TypeString, ""), []string{"reversed", "skipped"}),
				opt("transaction_id", TypeString, ""),
			),
		},
		// ---------------------------------------------------------- shipping
		{
			Key: CommerceShippingRates, Kind: KindQuery, Cardinality: All, Timeout: ShippingRatesTimeout,
			Doc: "quote delivery options; every eligible carrier addon answers in parallel",
			Input: schema(
				obj(req("origin", TypeObject, ""), AddressSchema),
				obj(req("destination", TypeObject, ""), AddressSchema),
				list(req("parcels", TypeArray, ""), ParcelSchema),
				req("currency", TypeCurrency, "currency of declared_value and of the wanted rates"),
				opt("declared_value", TypeDecimal, "insured / declared value"),
				opt("cod_amount", TypeDecimal, "cash on delivery to collect"),
				list(opt("items", TypeArray, ""), ShipItemSchema),
				opt("order_ref", TypeString, "cart/order id"),
			),
			Output: schema(
				list(req("rates", TypeArray, ""), schema(
					req("id", TypeString, "opaque rate id, passed to commerce.shipping.label"),
					req("carrier", TypeString, "e.g. fedex, estafeta, dhl, local"),
					req("service", TypeString, "human service name"),
					req("cost", TypeDecimal, "price to the buyer"),
					req("currency", TypeCurrency, ""),
					opt("service_code", TypeString, ""),
					opt("eta_days_min", TypeInteger, ""),
					opt("eta_days_max", TypeInteger, ""),
					opt("pickup", TypeBoolean, "store/branch pickup option"),
					enum(opt("delivery_type", TypeString, ""), []string{"door", "branch", "same_day", "on_demand", "pickup"}),
					opt("taxes_included", TypeBoolean, "cost already includes VAT/IVA"),
					opt("expires_at", TypeString, "RFC 3339"),
				)),
			),
		},
		{
			Key: CommerceShippingLabel, Kind: KindQuery, Cardinality: One,
			Doc: "buy a label for a quoted rate",
			Input: schema(
				req("rate_id", TypeString, "id of a commerce.shipping.rates rate from this provider"),
				req("order_ref", TypeString, ""),
				obj(opt("origin", TypeObject, ""), AddressSchema),
				obj(opt("destination", TypeObject, ""), AddressSchema),
				list(opt("parcels", TypeArray, ""), ParcelSchema),
				list(opt("items", TypeArray, ""), ShipItemSchema),
				opt("declared_value", TypeDecimal, ""),
				opt("currency", TypeCurrency, ""),
				enum(opt("label_format", TypeString, ""), []string{"pdf", "zpl", "png"}),
				opt("label_size", TypeString, "4x6 / letter"),
				opt("cod_amount", TypeDecimal, ""),
				req("idempotency_key", TypeString, "stable per shipment; a duplicate label costs money"),
			),
			Output: schema(
				enum(req("label_status", TypeString, "label_pending resolves by webhook/poll; tracking_number and label_url are set once created"), []string{"created", "label_pending"}),
				req("carrier", TypeString, ""),
				opt("tracking_number", TypeString, ""),
				opt("label_url", TypeString, ""),
				opt("shipment_id", TypeString, ""),
				opt("label_format", TypeString, ""),
				opt("tracking_url", TypeString, ""),
				opt("cost", TypeDecimal, ""),
				opt("currency", TypeCurrency, ""),
				enum(opt("status", TypeString, "tracking status"), TrackingStatuses),
			),
		},
		{
			Key: CommerceShippingCancel, Kind: KindQuery, Cardinality: One,
			Doc: "void a label / cancel a shipment not yet picked up",
			Input: schema(
				req("shipment_id", TypeString, ""),
				opt("tracking_number", TypeString, ""),
				opt("order_ref", TypeString, ""),
				opt("reason", TypeString, ""),
			),
			Output: schema(
				enum(req("status", TypeString, ""), []string{"cancelled", "rejected"}),
				opt("refund_amount", TypeDecimal, "label cost credited back"),
				opt("currency", TypeCurrency, ""),
				opt("message", TypeString, "why it could not be cancelled"),
			),
		},
		{
			Key: CommerceShippingTrack, Kind: KindQuery, Cardinality: One,
			Doc: "normalized tracking of a shipment",
			Input: schema(
				req("tracking_number", TypeString, ""),
				opt("carrier", TypeString, ""),
				opt("shipment_id", TypeString, ""),
			),
			Output: schema(
				enum(req("status", TypeString, ""), TrackingStatuses),
				opt("carrier", TypeString, ""),
				opt("tracking_url", TypeString, ""),
				opt("eta", TypeString, "RFC 3339 date"),
				opt("delivered_at", TypeString, "RFC 3339"),
				list(opt("events", TypeArray, "newest first"), TrackingEventSchema),
			),
		},
		// ------------------------------------------------------- fulfillment
		{
			Key: CommerceFulfillmentCreate, Kind: KindQuery, Cardinality: One,
			Doc: "send an order to a warehouse / 3PL for picking and shipping",
			Input: schema(
				req("order_ref", TypeString, ""),
				list(req("items", TypeArray, ""), ShipItemSchema),
				obj(req("destination", TypeObject, ""), AddressSchema),
				opt("service_level", TypeString, "standard / express / … or a rate id"),
				opt("carrier", TypeString, ""),
				opt("warehouse", TypeString, "provider warehouse code"),
				obj(opt("buyer", TypeObject, ""), BuyerSchema),
				opt("declared_value", TypeDecimal, ""),
				opt("currency", TypeCurrency, ""),
				opt("notes", TypeString, ""),
				req("idempotency_key", TypeString, IdempotencyDoc),
			),
			Output: schema(
				req("fulfillment_id", TypeString, ""),
				enum(req("status", TypeString, ""), []string{"accepted", "on_hold", "rejected"}),
				opt("warehouse", TypeString, ""),
				opt("tracking_number", TypeString, ""),
				opt("carrier", TypeString, ""),
				opt("tracking_url", TypeString, ""),
				opt("message", TypeString, "reason when on_hold/rejected"),
			),
		},
		{
			Key: CommerceFulfillmentCancel, Kind: KindQuery, Cardinality: One,
			Doc: "cancel a fulfillment that has not shipped",
			Input: schema(
				req("fulfillment_id", TypeString, ""),
				opt("order_ref", TypeString, ""),
				opt("reason", TypeString, ""),
			),
			Output: schema(
				enum(req("status", TypeString, ""), []string{"cancelled", "rejected"}),
				opt("message", TypeString, "why it could not be cancelled"),
			),
		},
		// ------------------------------------------------------------ search
		{
			Key: CommerceSearchQuery, Kind: KindQuery, Cardinality: One, Timeout: SearchTimeout,
			Doc: "product search / suggestions (hot path: providers should answer from an index, not per-keystroke WASM)",
			Input: schema(
				opt("q", TypeString, "free text; empty = browse"),
				opt("filters", TypeObject, "facet -> value(s)"),
				opt("sort", TypeString, "relevance / price_asc / price_desc / newest / …"),
				opt("limit", TypeInteger, ""),
				opt("cursor", TypeString, ""),
				opt("locale", TypeString, ""),
				opt("collection", TypeString, "restrict to a collection/category handle"),
				opt("suggest", TypeBoolean, "type-ahead mode"),
			),
			Output: schema(
				list(req("hits", TypeArray, ""), schema(
					req("id", TypeString, "product id"),
					opt("handle", TypeString, ""),
					opt("title", TypeString, ""),
					opt("variant_id", TypeString, ""),
					opt("score", TypeNumber, ""),
					opt("highlight", TypeObject, "field -> highlighted snippet"),
				)),
				opt("total_hits", TypeInteger, "total matches (may be an estimate)"),
				opt("next_cursor", TypeString, ""),
				list(opt("facets", TypeArray, ""), schema(
					req("field", TypeString, ""),
					list(req("values", TypeArray, ""), schema(
						req("value", TypeString, ""),
						req("count", TypeInteger, ""),
					)),
				)),
				strs(opt("suggestions", TypeArray, "query completions")),
			),
		},
		{
			Key: CommerceDiscountsEvaluate, Kind: KindQuery, Cardinality: All, Timeout: DiscountsTimeout,
			Doc: "every discount engine proposes adjustments for a cart",
			Input: schema(
				req("currency", TypeCurrency, ""),
				list(req("lines", TypeArray, ""), LineSchema),
				strs(opt("codes", TypeArray, "coupon codes entered by the buyer")),
				obj(opt("buyer", TypeObject, ""), BuyerSchema),
				opt("subtotal", TypeDecimal, ""),
			),
			Output: schema(
				list(req("discounts", TypeArray, ""), schema(
					req("id", TypeString, ""),
					req("title", TypeString, ""),
					req("amount", TypeDecimal, "total discount"),
					req("currency", TypeCurrency, ""),
					opt("code", TypeString, "coupon code that produced it"),
					opt("line_id", TypeString, "line it applies to; absent = order level"),
					opt("combinable", TypeBoolean, ""),
				)),
				list(opt("rejected_codes", TypeArray, ""), schema(
					req("code", TypeString, ""),
					opt("reason", TypeString, ""),
				)),
			),
		},
		// ------------------------------------------------------------ fiscal
		{
			Key: FiscalInvoiceCreate, Kind: KindQuery, Cardinality: One,
			Doc: "issue a fiscal invoice for an order (CFDI 4.0 in MX via the fiscal_mexico family)",
			Input: schema(
				req("order_ref", TypeString, ""),
				req("currency", TypeCurrency, ""),
				obj(req("customer", TypeObject, ""), &Schema{Fields: []Field{
					req("tax_id", TypeString, "RFC / VAT id"),
					req("name", TypeString, "legal name"),
					opt("email", TypeString, ""),
					opt("postal_code", TypeString, "fiscal domicile postal code"),
					opt("tax_regime", TypeString, "e.g. SAT régimen fiscal"),
					opt("use", TypeString, "e.g. SAT uso CFDI"),
					opt("country", TypeCountry, ""),
				}}),
				list(req("lines", TypeArray, ""), schema(
					req("id", TypeString, ""),
					req("description", TypeString, ""),
					req("quantity", TypeDecimal, "decimal: fiscal quantities can be fractional"),
					req("unit_price", TypeDecimal, "before tax"),
					opt("discount", TypeDecimal, ""),
					opt("product_code", TypeString, "fiscal product key (SAT ClaveProdServ)"),
					opt("unit_code", TypeString, "fiscal unit key (SAT ClaveUnidad)"),
					opt("sku", TypeString, ""),
					list(opt("taxes", TypeArray, ""), TaxLineSchema),
				)),
				opt("payment_form", TypeString, "e.g. SAT forma de pago (04, 03, 01)"),
				opt("payment_method", TypeString, "e.g. PUE / PPD"),
				opt("series", TypeString, ""),
				opt("subtotal", TypeDecimal, ""),
				opt("total", TypeDecimal, ""),
				opt("issued_at", TypeString, "RFC 3339"),
				opt("notes", TypeString, ""),
				req("idempotency_key", TypeString, IdempotencyDoc),
			),
			Output: schema(
				req("invoice_id", TypeString, "provider id"),
				enum(req("status", TypeString, ""), []string{"issued", "pending", "failed"}),
				opt("fiscal_uuid", TypeString, "folio fiscal (CFDI UUID)"),
				opt("series", TypeString, ""),
				opt("folio", TypeString, ""),
				opt("total", TypeDecimal, ""),
				opt("currency", TypeCurrency, ""),
				opt("pdf_url", TypeString, ""),
				opt("xml_url", TypeString, ""),
				opt("message", TypeString, "authority/provider message when failed"),
			),
		},
		{
			Key: FiscalInvoiceCancel, Kind: KindQuery, Cardinality: One,
			Doc: "cancel an issued fiscal invoice",
			Input: schema(
				req("invoice_id", TypeString, ""),
				opt("reason", TypeString, "e.g. SAT motivo 01 / 02 / 03 / 04"),
				opt("substitution_id", TypeString, "replacing invoice fiscal_uuid (motivo 01)"),
			),
			Output: schema(
				enum(req("status", TypeString, ""), []string{"cancelled", "pending", "rejected"}),
				opt("message", TypeString, ""),
			),
		},
	}
}

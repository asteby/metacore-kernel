package capability

// Embedded payment (the buyer pays on the storefront page) and payment
// notification health contracts. Reference: docs/capabilities-commerce.md.
//
// Evolution rules (the package's, applied here): a contract key is never
// versioned by suffix; it evolves ONLY by adding optional input fields,
// optional output fields, enum values appended at the end, and new contracts.
// Nothing is removed, renamed, retyped or made required, so a provider built
// against an earlier shape keeps validating. The browser never decides money:
// the host sets amount and currency of commerce.payments.submit from its own
// document, and the provider rejects any other amount.

var (
	// PaymentActionSchema is what the storefront does after a submit.
	PaymentActionSchema = schema(
		enum(req("type", TypeString, "none: nothing to do; challenge: show the bank challenge (3DS) inside the page; voucher: show the cash / transfer voucher; redirect: send the buyer to redirect_url"), PaymentActionTypes),
		obj(opt("challenge", TypeObject, "type challenge: what the provider's SDK needs to mount the bank challenge"), schema(
			opt("url", TypeString, "challenge URL (external_resource_url)"),
			opt("creq", TypeString, "challenge request, as the provider returns it"),
		)),
		obj(opt("voucher", TypeObject, "type voucher: the offline payment slip (never a card number)"), schema(
			opt("payment_point", TypeString, "cash point or bank (oxxo, 7eleven, spei...)"),
			opt("reference", TypeString, "payment reference to give at the point"),
			opt("voucher_url", TypeString, "hosted voucher page (https)"),
			opt("barcode", TypeString, "barcode content"),
			opt("clabe", TypeString, "bank transfer account (CLABE)"),
			opt("amount", TypeDecimal, "exact amount to pay"),
			opt("currency", TypeCurrency, ""),
			opt("expires_at", TypeString, "RFC 3339; after it the voucher no longer pays"),
		)),
		opt("redirect_url", TypeString, "type redirect: hosted page the buyer must open"),
	)

	// ClientPointSchema is one selectable cash point of the cash option.
	ClientPointSchema = schema(
		req("id", TypeString, "stable id of the row the buyer picks"),
		req("payment_method_id", TypeString, "provider method id sent back in submit provider_data"),
		opt("payment_point", TypeString, "provider point id when several points share one method id; absent when the method is the point"),
		req("label", TypeString, "name shown to the buyer"),
		opt("logo_url", TypeString, ""),
		enum(opt("group", TypeString, "main (default): first list; more: behind the 'more' toggle"), ClientPointGroups),
	)

	// ClientOptionSchema is one payment option the storefront paints.
	ClientOptionSchema = schema(
		enum(req("key", TypeString, "which option"), ClientOptionKeys),
		opt("family", TypeString, "method family as in commerce.payments.capabilities methods (card, msi, oxxo, spei, wallet, bnpl...)"),
		req("redirect", TypeBoolean, "true: the option leaves the page (create the session with flow redirect and method_hint = key)"),
		opt("label", TypeString, "provider default label; the storefront owns the final text"),
		opt("max_installments", TypeInteger, "card: highest installment count offered (the provider re-checks it on submit)"),
		list(opt("points", TypeArray, "cash: selectable points"), ClientPointSchema),
		opt("label_max", TypeString, "credits: the provider's own wording of the maximum term, when it knows it"),
	)

	// ClientCSPSchema lists the origins the provider's SDK needs, so the
	// storefront builds its Content-Security-Policy without knowing the
	// provider. Origins only (https://host), never full URLs.
	ClientCSPSchema = schema(
		strs(opt("script", TypeArray, "script-src origins")),
		strs(opt("frame", TypeArray, "frame-src origins")),
		strs(opt("connect", TypeArray, "connect-src origins")),
		strs(opt("img", TypeArray, "img-src origins")),
		strs(opt("form", TypeArray, "form-action origins")),
	)

	// ClientSchema is what a provider declares to paint its payment in the
	// storefront. PUBLIC data only: never a secret key or access token.
	ClientSchema = schema(
		req("kind", TypeString, "storefront adapter that knows the provider SDK (e.g. mercadopago_bricks, stripe_elements)"),
		opt("sdk_url", TypeString, "SDK script to load, https; absent when the options need no script"),
		opt("public_key", TypeString, "public key / publishable identifier of the SDK"),
		opt("locale", TypeString, "locale the SDK is painted in (es-MX)"),
		opt("market", TypeString, "market code the answer is for"),
		list(req("options", TypeArray, "payment options, in the order to paint them"), ClientOptionSchema),
		obj(opt("csp", TypeObject, "origins the SDK needs"), ClientCSPSchema),
		opt("test_mode", TypeBoolean, "the public key belongs to the provider sandbox"),
	)

	// NotificationCheckSchema is one connector health check.
	NotificationCheckSchema = schema(
		req("key", TypeString, "stable check id (credentials, mode, webhook_secret, notification_url...)"),
		req("ok", TypeBoolean, ""),
		enum(opt("level", TypeString, "severity when ok is false (default warning)"), HealthLevels),
		opt("message", TypeString, "what is wrong or how to fix it; never a secret"),
	)
)

// openSchema returns a copy of s that accepts undeclared fields (typed fields
// stay validated): provider-specific public extras next to the normalized ones.
func openSchema(s *Schema) *Schema {
	c := *s
	c.Open = true
	return &c
}

// submitOutput is the confirm output plus action, decline_code and retryable.
func submitOutput(confirm *Schema) *Schema {
	out := &Schema{Fields: append([]Field(nil), confirm.Fields...)}
	out.Fields = append(out.Fields,
		obj(opt("action", TypeObject, "what the storefront does next; absent = none"), PaymentActionSchema),
		enum(opt("decline_code", TypeString, "neutral rejection reason when status is failed"), DeclineCodes),
		opt("retryable", TypeBoolean, "failed: the buyer may try again with the same checkout (another card, fixing a field)"),
	)
	return out
}

// embeddedPaymentContracts are appended to the commerce contracts. Outputs
// that reuse the confirm shape (submit, notification.resolve) are filled in by
// commerceContracts.
func embeddedPaymentContracts() []Contract {
	return []Contract{
		{
			Key: CommercePaymentsClient, Kind: KindQuery, Cardinality: One,
			Doc: "what this provider declares so the storefront paints its payment in the page (options, public key, SDK, CSP origins, market, mode); read only, no secrets",
			Input: schema(
				req("market", TypeString, "market code"),
				req("currency", TypeCurrency, ""),
				opt("amount", TypeDecimal, "cart total, when the options depend on it (installments)"),
				opt("locale", TypeString, "preferred locale (es-MX)"),
			),
			Output: ClientSchema,
		},
		{
			Key: CommercePaymentsSubmit, Kind: KindQuery, Cardinality: One,
			Doc: "charge what the buyer's browser tokenized with the provider SDK (embedded payment); the host sets amount and currency, the provider rejects any other amount; pinned to the session's provider and idempotent",
			Input: schema(
				req("session_id", TypeString, "commerce.payments.session session_id"),
				req("order_ref", TypeString, "order id/number in OPS (external_reference)"),
				req("amount", TypeDecimal, "total to charge, set by the host from its own document"),
				req("currency", TypeCurrency, ""),
				enum(req("method", TypeString, "family the buyer chose"), SubmitMethods),
				obj(req("provider_data", TypeObject, "opaque: what the provider SDK handed the browser (card token, method id, issuer, installments, payer id, cash point). Never a card number"), &Schema{Open: true}),
				req("idempotency_key", TypeString, IdempotencyDoc+"; one per attempt, not per checkout"),
				opt("device_session_id", TypeString, "anti-fraud device fingerprint from the provider SDK; pass through, never store or log"),
				obj(opt("buyer", TypeObject, "from the host's record, not the browser"), BuyerSchema),
				obj(opt("shipping_address", TypeObject, "from the host's record, not the browser"), AddressSchema),
				list(opt("lines", TypeArray, ""), LineSchema),
				opt("market", TypeString, "market code the order was placed in; the provider uses that market's configuration"),
				obj(opt("metadata", TypeObject, ""), &Schema{Open: true, Fields: []Field{
					opt("checkout_id", TypeString, "checkout id in OPS"),
					opt("attempt", TypeInteger, "attempt number of the checkout"),
				}}),
				opt("expires_at", TypeString, "RFC 3339 expiry of a cash / transfer voucher"),
				opt("return_url", TypeString, "where a challenge returns the buyer"),
				opt("notify_url", TypeString, "webhook URL the host exposes (as in commerce.payments.session)"),
			),
			// Output: submitOutput(confirm), filled in by commerceContracts.
			Errors: []string{"invalid_input", "price_mismatch", "method_not_allowed", "installments_exceeded"},
		},
		{
			Key: CommercePaymentsNotificationInspect, Kind: KindQuery, Cardinality: One,
			Doc: "read a provider notification WITHOUT network or writes: verdict on its signature and which resource it names; the host calls it right after persisting the notification",
			Input: schema(
				req("method", TypeString, "HTTP method of the notification"),
				opt("query", TypeObject, "query-string parameters"),
				opt("headers", TypeObject, "request headers, lowercase keys"),
				opt("raw_body_b64", TypeString, "exact body bytes, base64 (the signature may cover them)"),
			),
			Output: schema(
				enum(req("auth", TypeString, "signature verdict"), NotificationAuths),
				opt("auth_reason", TypeString, "why (signature_mismatch, timestamp_out_of_tolerance, matched_previous...)"),
				opt("resource_kind", TypeString, "what the notification names (payment, refund, dispute...)"),
				opt("resource_id", TypeString, "id of that resource at the provider"),
				opt("event_id", TypeString, "provider event id (dedupe)"),
				opt("livemode", TypeBoolean, "false = sandbox notification"),
			),
		},
		{
			Key: CommercePaymentsNotificationResolve, Kind: KindQuery, Cardinality: One,
			Doc: "read the resource a notification names through the provider API, by id (the notification body is never trusted); same output as commerce.payments.status; the host rate-limits and dedupes it when auth is not firma_ok",
			Input: schema(
				req("resource_kind", TypeString, "as inspect answered"),
				req("resource_id", TypeString, "must have the shape of the provider's ids, else invalid_input"),
				enum(opt("auth", TypeString, "inspect verdict, for the provider's own logging"), NotificationAuths),
			),
			// Output: the confirm output, filled in by commerceContracts.
			Errors: []string{"invalid_input", "not_found"},
		},
		{
			Key: CommercePaymentsHealth, Kind: KindQuery, Cardinality: One,
			Doc: "connector self-diagnosis (credentials, mode, webhook secret, notification URL) without revealing any secret",
			Input: schema(
				opt("market", TypeString, "market code, when the configuration differs per market"),
			),
			Output: schema(
				list(req("checks", TypeArray, ""), NotificationCheckSchema),
				opt("test_mode", TypeBoolean, "the configured credentials are sandbox credentials"),
			),
		},
	}
}

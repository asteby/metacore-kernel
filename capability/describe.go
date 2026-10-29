package capability

import (
	"sort"
	"strings"
)

// Text is human copy per locale ("es", "en", "es-MX"…). Admin screens show it
// instead of a capability key; the key itself only belongs in a "technical
// details" hint.
type Text map[string]string

// Get returns the copy for locale: the exact locale, then its base language
// ("es-MX" → "es"), then Spanish (the ecosystem default), then English, then
// "" when the text is empty.
func (t Text) Get(locale string) string {
	if len(t) == 0 {
		return ""
	}
	locale = strings.TrimSpace(locale)
	if v := t[locale]; v != "" {
		return v
	}
	if i := strings.IndexAny(locale, "-_"); i > 0 {
		if v := t[strings.ToLower(locale[:i])]; v != "" {
			return v
		}
	}
	for _, l := range []string{"es", "en"} {
		if v := t[l]; v != "" {
			return v
		}
	}
	return ""
}

// Domain is the business area a family of contracts belongs to ("Pagos",
// "Envíos"). Admin screens group capabilities by domain and choose one
// provider per domain and market instead of one per contract.
type Domain struct {
	// Key is the key prefix shared by the domain's contracts
	// ("commerce.payments").
	Key         string
	Title       Text
	Description Text
	// Icon is a lucide icon name ("CreditCard").
	Icon string
	// Order sorts domains on screen (lower first).
	Order int
}

// Domains is the registry of well-known capability domains.
var Domains = map[string]Domain{}

// RegisterDomain adds or replaces a domain (hosts may ship their own).
func RegisterDomain(d Domain) {
	Domains[d.Key] = d
}

// DomainOf returns the domain key of a capability key: the longest registered
// domain that prefixes it, else its first two segments ("acme.loyalty.earn"
// → "acme.loyalty"), else the key itself.
func DomainOf(key string) string {
	best := ""
	for k := range Domains {
		if (key == k || strings.HasPrefix(key, k+".")) && len(k) > len(best) {
			best = k
		}
	}
	if best != "" {
		return best
	}
	parts := strings.Split(key, ".")
	if len(parts) > 2 {
		return strings.Join(parts[:2], ".")
	}
	return key
}

// DomainKeys lists the registered domains in screen order.
func DomainKeys() []string {
	out := make([]string, 0, len(Domains))
	for k := range Domains {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := Domains[out[i]], Domains[out[j]]
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return out[i] < out[j]
	})
	return out
}

type contractText struct {
	title, description Text
	checkout           bool
}

// describeContract fills a contract's human copy from the built-in catalog
// when the contract does not bring its own.
func describeContract(c *Contract) {
	d, ok := contractTexts[c.Key]
	if !ok {
		return
	}
	if len(c.Title) == 0 {
		c.Title = d.title
	}
	if len(c.Description) == 0 {
		c.Description = d.description
	}
	if d.checkout {
		c.Checkout = true
	}
}

func init() {
	for _, d := range builtinDomains {
		RegisterDomain(d)
	}
}

var builtinDomains = []Domain{
	{Key: "commerce.payments", Icon: "CreditCard", Order: 10,
		Title:       Text{"es": "Pagos", "en": "Payments"},
		Description: Text{"es": "Quién cobra los pedidos, confirma los pagos y hace los reembolsos.", "en": "Who charges orders, confirms payments and issues refunds."}},
	{Key: "commerce.shipping", Icon: "Truck", Order: 20,
		Title:       Text{"es": "Envíos", "en": "Shipping"},
		Description: Text{"es": "Cotización de envíos, guías y rastreo de paquetes.", "en": "Shipping quotes, labels and package tracking."}},
	{Key: "commerce.fulfillment", Icon: "PackageCheck", Order: 30,
		Title:       Text{"es": "Surtido de pedidos", "en": "Fulfillment"},
		Description: Text{"es": "Almacén o 3PL que prepara y despacha los pedidos.", "en": "Warehouse or 3PL that picks and ships orders."}},
	{Key: "commerce.tax", Icon: "Percent", Order: 40,
		Title:       Text{"es": "Impuestos", "en": "Taxes"},
		Description: Text{"es": "Cálculo y registro de impuestos de cada venta.", "en": "Tax calculation and recording for each sale."}},
	{Key: "fiscal.invoice", Icon: "FileText", Order: 50,
		Title:       Text{"es": "Facturación", "en": "Invoicing"},
		Description: Text{"es": "Emisión y cancelación de facturas fiscales.", "en": "Issuing and cancelling fiscal invoices."}},
	{Key: "messaging.whatsapp", Icon: "MessageCircle", Order: 60,
		Title:       Text{"es": "Mensajería", "en": "Messaging"},
		Description: Text{"es": "Envío de mensajes de WhatsApp a clientes.", "en": "Sending WhatsApp messages to customers."}},
	{Key: "commerce.discounts", Icon: "BadgePercent", Order: 70,
		Title:       Text{"es": "Descuentos", "en": "Discounts"},
		Description: Text{"es": "Motores de promociones que proponen descuentos al carrito.", "en": "Promotion engines that propose cart discounts."}},
	{Key: "commerce.search", Icon: "Search", Order: 80,
		Title:       Text{"es": "Búsqueda", "en": "Search"},
		Description: Text{"es": "Búsqueda y sugerencias de productos en la tienda.", "en": "Product search and suggestions in the store."}},
	{Key: "geo", Icon: "Map", Order: 90,
		Title:       Text{"es": "Mapas y rutas", "en": "Maps and routes"},
		Description: Text{"es": "Ubicar direcciones y calcular distancias.", "en": "Locate addresses and compute distances."}},
	{Key: "region", Icon: "MapPin", Order: 100,
		Title:       Text{"es": "Datos regionales", "en": "Regional data"},
		Description: Text{"es": "Códigos postales y datos de cada país.", "en": "Postal codes and per-country data."}},
}

// contractTexts is the built-in human copy of every well-known contract.
// A package-level var (not init) so Register and the Contracts literal can
// read it whatever the file init order.
var contractTexts = map[string]contractText{
	MessagingWhatsAppSend: {
		title:       Text{"es": "Enviar mensaje de WhatsApp", "en": "Send WhatsApp message"},
		description: Text{"es": "Manda un mensaje o archivo a un número de WhatsApp.", "en": "Sends a message or file to a WhatsApp number."},
	},
	CommercePaymentsSession: {checkout: true,
		title:       Text{"es": "Cobrar en línea", "en": "Charge online"},
		description: Text{"es": "Crea el cobro del pedido (tarjeta, efectivo, transferencia…) y lleva al comprador a pagar.", "en": "Creates the order charge (card, cash, transfer…) and takes the buyer to pay."},
	},
	CommercePaymentsConfirm: {checkout: true,
		title:       Text{"es": "Confirmar pagos", "en": "Confirm payments"},
		description: Text{"es": "Recibe los avisos del proveedor y marca el pedido como pagado.", "en": "Receives the provider's notifications and marks the order as paid."},
	},
	CommercePaymentsStatus: {checkout: true,
		title:       Text{"es": "Consultar estado del pago", "en": "Check payment status"},
		description: Text{"es": "Pregunta al proveedor si un pago ya se acreditó.", "en": "Asks the provider whether a payment went through."},
	},
	CommercePaymentsRefund: {
		title:       Text{"es": "Reembolsar", "en": "Refund"},
		description: Text{"es": "Devuelve todo o parte de un pago cobrado.", "en": "Returns all or part of a captured payment."},
	},
	CommercePaymentsRefundStatus: {
		title:       Text{"es": "Consultar reembolso", "en": "Check refund"},
		description: Text{"es": "Revisa en qué va un reembolso ya solicitado.", "en": "Checks the progress of a refund already requested."},
	},
	CommercePaymentsCancel: {
		title:       Text{"es": "Cancelar cobro", "en": "Cancel charge"},
		description: Text{"es": "Anula un cobro que todavía no se ha pagado.", "en": "Voids a charge that has not been paid yet."},
	},
	CommercePaymentsCapabilities: {
		title:       Text{"es": "Funciones del proveedor", "en": "Provider features"},
		description: Text{"es": "Qué métodos, límites y reembolsos admite el proveedor.", "en": "Which methods, limits and refunds the provider supports."},
	},
	CommercePaymentsReconcile: {
		title:       Text{"es": "Conciliar pagos", "en": "Reconcile payments"},
		description: Text{"es": "Trae los pagos del proveedor para cuadrarlos con los pedidos.", "en": "Fetches the provider's payments to match them with orders."},
	},
	CommercePaymentsTerminalCreate: {
		title:       Text{"es": "Cobrar en terminal", "en": "Charge on terminal"},
		description: Text{"es": "Envía el monto a una terminal o equipo de cobro en tienda.", "en": "Sends the amount to an in-store card terminal or cash device."},
	},
	CommercePaymentsTerminalDevices: {
		title:       Text{"es": "Listar terminales", "en": "List terminals"},
		description: Text{"es": "Muestra las terminales de la cuenta del proveedor para vincularlas.", "en": "Lists the provider account's terminals to link them."},
	},
	CommerceTaxCalculate: {
		title:       Text{"es": "Calcular impuestos", "en": "Calculate taxes"},
		description: Text{"es": "Calcula los impuestos del carrito y del envío.", "en": "Computes taxes for the cart and shipping."},
	},
	CommerceTaxCommit: {
		title:       Text{"es": "Registrar impuestos", "en": "Record taxes"},
		description: Text{"es": "Registra ante el proveedor los impuestos de un pedido pagado.", "en": "Records a paid order's taxes with the provider."},
	},
	CommerceTaxReverse: {
		title:       Text{"es": "Revertir impuestos", "en": "Reverse taxes"},
		description: Text{"es": "Revierte impuestos registrados tras una devolución.", "en": "Reverses recorded taxes after a return."},
	},
	CommerceShippingRates: {
		title:       Text{"es": "Cotizar envíos", "en": "Quote shipping"},
		description: Text{"es": "Muestra al comprador las opciones y precios de envío.", "en": "Shows the buyer shipping options and prices."},
	},
	CommerceShippingLabel: {
		title:       Text{"es": "Generar guía", "en": "Create label"},
		description: Text{"es": "Compra la guía de la paquetería elegida.", "en": "Buys the label for the chosen carrier."},
	},
	CommerceShippingCancel: {
		title:       Text{"es": "Cancelar guía", "en": "Cancel label"},
		description: Text{"es": "Cancela una guía que aún no se recolecta.", "en": "Cancels a label not yet picked up."},
	},
	CommerceShippingTrack: {
		title:       Text{"es": "Rastrear envío", "en": "Track shipment"},
		description: Text{"es": "Consulta dónde va el paquete.", "en": "Checks where the package is."},
	},
	CommerceFulfillmentCreate: {
		title:       Text{"es": "Enviar a surtir", "en": "Send to fulfillment"},
		description: Text{"es": "Manda el pedido al almacén o 3PL para prepararlo.", "en": "Sends the order to the warehouse or 3PL."},
	},
	CommerceFulfillmentCancel: {
		title:       Text{"es": "Cancelar surtido", "en": "Cancel fulfillment"},
		description: Text{"es": "Detiene un surtido que aún no sale.", "en": "Stops a fulfillment that has not shipped."},
	},
	CommerceSearchQuery: {
		title:       Text{"es": "Buscar productos", "en": "Search products"},
		description: Text{"es": "Resultados y sugerencias del buscador de la tienda.", "en": "Results and suggestions for the store search."},
	},
	CommerceDiscountsEvaluate: {
		title:       Text{"es": "Aplicar promociones", "en": "Apply promotions"},
		description: Text{"es": "Propone descuentos para el carrito.", "en": "Proposes discounts for the cart."},
	},
	FiscalInvoiceCreate: {
		title:       Text{"es": "Emitir factura", "en": "Issue invoice"},
		description: Text{"es": "Timbra la factura fiscal de un pedido.", "en": "Issues the fiscal invoice for an order."},
	},
	FiscalInvoiceCancel: {
		title:       Text{"es": "Cancelar factura", "en": "Cancel invoice"},
		description: Text{"es": "Cancela una factura ya emitida.", "en": "Cancels an issued invoice."},
	},
	GeoGeocode: {
		title:       Text{"es": "Ubicar direcciones", "en": "Geocode addresses"},
		description: Text{"es": "Convierte una dirección o código postal en coordenadas.", "en": "Turns an address or postal code into coordinates."},
	},
	GeoRoute: {
		title:       Text{"es": "Calcular rutas", "en": "Compute routes"},
		description: Text{"es": "Distancia y tiempo por carretera entre puntos.", "en": "Road distance and time between points."},
	},
	RegionPostalLocate: {
		title:       Text{"es": "Ubicar código postal", "en": "Locate postal code"},
		description: Text{"es": "Ciudad, estado y ubicación de un código postal.", "en": "City, state and location of a postal code."},
	},
}

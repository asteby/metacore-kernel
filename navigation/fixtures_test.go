package navigation

import (
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func ip(v int) *int { return &v }

// legacyContribs is a fleet slice that uses NONE of the anchor fields: the
// shapes of the real cases (products / link_catalog / products-tires on
// products; channel_orders / storefront_api; mail / link-inbox; fiscal; pos /
// caja), as they are published today with free-text targets.
func legacyContribs() []Contribution {
	return []Contribution{
		{AddonKey: "products", Groups: []manifest.NavGroup{{Title: "nav.catalog", Icon: "Package", Target: "sidebar.catalog", Items: []manifest.NavItem{
			{Title: "nav.products", URL: "/m/products", Model: "Product", Permission: "product.index"},
			{Title: "nav.products.board", URL: "/m/products", Model: "Product", ViewType: "kanban", GroupBy: "status"},
		}}}},
		{AddonKey: "link_catalog", Groups: []manifest.NavGroup{{Title: "nav.link_catalog", Icon: "Link", Target: "sidebar.catalog", Items: []manifest.NavItem{
			{Title: "nav.products", URL: "/m/products", Model: "Product"},
			{Title: "nav.link_catalog.sync", URL: "/addons/link_catalog/sync"},
		}}}},
		{AddonKey: "products-tires", Groups: []manifest.NavGroup{{Title: "nav.tires", Icon: "Circle", Target: "sidebar.operations", Items: []manifest.NavItem{
			{Title: "nav.tires", URL: "/m/products", Model: "Product", Filter: map[string]string{"kind": "tire"}},
		}}}},
		{AddonKey: "channel_orders", Groups: []manifest.NavGroup{{Title: "nav.channel_orders", Icon: "Inbox", Target: "sidebar.sales_orders", Items: []manifest.NavItem{
			{Title: "nav.channel_orders", URL: "/m/channel_orders", Model: "ChannelOrder"},
		}}}},
		{AddonKey: "storefront_api", Groups: []manifest.NavGroup{{Title: "nav.storefront", Icon: "Store", Target: "sidebar.sales", Items: []manifest.NavItem{
			{Title: "nav.channel_orders", URL: "/m/channel_orders/", Model: ""},
			{Title: "nav.storefront.tokens", URL: "/addons/storefront_api/tokens"},
		}}}},
		{AddonKey: "mail", Groups: []manifest.NavGroup{{Title: "nav.mail", Icon: "Mail", Target: "sidebar.tickets", Items: []manifest.NavItem{
			{Title: "nav.messages", URL: "/m/messages", Model: "Message"},
		}}}},
		{AddonKey: "link-inbox", Groups: []manifest.NavGroup{{Title: "", Items: []manifest.NavItem{
			{Title: "nav.inbox", URL: "/m/messages?folder=inbox"},
		}}}},
		{AddonKey: "fiscal_mexico", Groups: []manifest.NavGroup{{Title: "nav.fiscal", Icon: "Receipt", Target: "sidebar.finance", Items: []manifest.NavItem{
			{Title: "nav.fiscal.documents", URL: "/m/fiscal_documents", Model: "FiscalDocument"},
			{Title: "nav.fiscal.pending", URL: "/m/fiscal_documents", Model: "FiscalDocument", Filter: map[string]string{"status": "pending"}, LockedScope: true, Permission: "fiscaldocument.stamp"},
		}}}},
		{AddonKey: "pos", Groups: []manifest.NavGroup{{Title: "nav.pos", Icon: "Monitor", Target: "sidebar.sales", Items: []manifest.NavItem{
			{Title: "nav.pos.register", URL: "/pos"},
			{Title: "nav.pos.sessions", URL: "/m/pos_sessions", Model: "PosSession"},
		}}}},
		{AddonKey: "caja", Groups: []manifest.NavGroup{{Title: "nav.caja", Icon: "Wallet", Target: "sidebar.finance", Items: []manifest.NavItem{
			{Title: "nav.cash_registers", URL: "/m/cash_registers", Model: "CashRegister"},
			{Title: "nav.caja.cuts", Items: []manifest.NavItem{{Title: "nav.caja.cuts.open", URL: "/m/cash_cuts?f_status=eq:open"}}},
		}}}},
	}
}

func legacyCore() []Group {
	return []Group{
		{Title: "sidebar.sales", Icon: "ShoppingCart", Target: "sidebar.sales", Items: []Item{{Title: "core.orders", URL: "/m/sales_orders", Owner: "core"}}},
		{Title: "sidebar.finance", Icon: "Landmark", Target: "sidebar.finance"},
	}
}

// anchoredFleet is the same fleet migrated to anchors and attach_to (RFC
// SDK-0001 §4.3 and §9).
func anchoredFleet() []Addon {
	return []Addon{
		{Key: "customers", FirstParty: true,
			Anchors: []v3.NavAnchor{
				{Key: "sales.order", Section: "sales", Title: "nav.sales.orders", Icon: "ShoppingCart", Model: "SalesOrder", Order: ip(10),
					Accepts: []string{"tab", "view", "action", "panel", "filter", "mode"}, Permission: "salesorder.index", Provisional: true},
				{Key: "customers.customer", Section: "customers", Title: "nav.customers", Icon: "Users", Model: "Customer", Order: ip(10),
					Accepts: []string{"tab", "action", "panel", "filter"}, Permission: "customer.index"},
			},
			Groups: []manifest.NavGroup{{Key: "customers.sales", Section: "sales", Order: ip(10), Title: "nav.sales.orders", Items: []manifest.NavItem{
				{Key: "pending_credit", Title: "nav.sales.pending_credit", AttachTo: "sales.order", As: "filter",
					Filter: map[string]string{"state": "credit_review"}, LockedScope: true, Permission: "salesorder.authorize_credit"},
			}}},
		},
		{Key: "products", FirstParty: true,
			Anchors: []v3.NavAnchor{{Key: "catalog.product", Section: "catalog", Title: "nav.products", Icon: "Package", Model: "Product", Order: ip(10),
				Accepts: []string{"tab", "view", "action", "panel", "filter"}, Permission: "product.index"}},
			Groups: []manifest.NavGroup{{Key: "products.catalog", Section: "catalog", Title: "nav.catalog", Items: []manifest.NavItem{
				{Key: "board", Title: "nav.products.board", AttachTo: "catalog.product", As: "view", Model: "Product", ViewType: "kanban", GroupBy: "status"},
				// Still listed as a plain entry: deduped against the anchor.
				{Key: "list", Title: "nav.products", URL: "/m/products", Model: "Product"},
			}}},
		},
		{Key: "link_catalog", FirstParty: true, Groups: []manifest.NavGroup{{Key: "link_catalog.catalog", Section: "catalog", Title: "nav.link_catalog", Items: []manifest.NavItem{
			// Same board as the owner: the owner wins the dedupe (A5).
			{Key: "board", Title: "nav.link_catalog.board", AttachTo: "catalog.product", As: "view", Model: "Product", ViewType: "kanban", GroupBy: "status", Order: ip(1)},
			{Key: "sync", Title: "nav.link_catalog.sync", AttachTo: "catalog.product", As: "action", Actions: []string{"sync_catalog"}},
		}}}},
		{Key: "products-tires", Groups: []manifest.NavGroup{{Key: "products-tires.catalog", Section: "catalog", Title: "nav.tires", Items: []manifest.NavItem{
			{Key: "tires", Title: "nav.tires", AttachTo: "catalog.product", As: "filter", Model: "Product", Filter: map[string]string{"kind": "tire"}},
		}}}},
		{Key: "fiscal_mexico", FirstParty: true, Groups: []manifest.NavGroup{{Key: "fiscal_mexico.sales", Section: "sales", Title: "nav.fiscal", Items: []manifest.NavItem{
			{Key: "invoices_tab", Title: "nav.fiscal.invoices", AttachTo: "sales.order", As: "tab", Model: "Invoice", Permission: "invoice.index", Fallback: "hide", Order: ip(30)},
			{Key: "stamp", Title: "actions.stamp", AttachTo: "sales.order", As: "action", Actions: []string{"stamp_invoice"}, Permission: "invoice.stamp"},
		}}}},
		{Key: "pos", FirstParty: true, Groups: []manifest.NavGroup{{Key: "pos.main", Section: "sales", Order: ip(5), Title: "nav.pos", Items: []manifest.NavItem{
			{Key: "register", Title: "nav.pos.register", AttachTo: "sales.order", As: "mode", Page: "register", Permission: "screen.pos.access", Fallback: "section"},
		}}}},
		{Key: "caja", Groups: []manifest.NavGroup{{Key: "caja.finance", Section: "finance", Title: "nav.caja", Items: []manifest.NavItem{
			{Key: "registers", Title: "nav.cash_registers", URL: "/m/cash_registers", Model: "CashRegister"},
		}}}},
		{Key: "channel_orders", FirstParty: true, Groups: []manifest.NavGroup{{Key: "channel_orders.sales", Section: "sales", Title: "nav.channel_orders", Items: []manifest.NavItem{
			{Key: "orders", Title: "nav.channel_orders", AttachTo: "sales.order", As: "view", Model: "ChannelOrder"},
		}}}},
		{Key: "storefront_api", Groups: []manifest.NavGroup{{Key: "storefront_api.sales", Section: "sales", Title: "nav.storefront", Items: []manifest.NavItem{
			// Same resource as channel_orders: first-party wins (A5).
			{Key: "orders", Title: "nav.storefront.orders", AttachTo: "sales.order", As: "view", Model: "ChannelOrder"},
		}}}},
		{Key: "mail", FirstParty: true, Groups: []manifest.NavGroup{{Key: "mail.inbox", Section: "communication", Title: "nav.mail", Items: []manifest.NavItem{
			{Key: "messages", Title: "nav.messages", AttachTo: "communication.inbox", As: "view", Model: "Message"},
		}}}},
		{Key: "link-inbox", Groups: []manifest.NavGroup{{Key: "link-inbox.inbox", Section: "communication", Title: "nav.inbox", Items: []manifest.NavItem{
			// communication.inbox has no owner installed: both fall back to
			// own entries of their group (view -> section).
			{Key: "messages", Title: "nav.inbox", AttachTo: "communication.inbox", As: "view", Model: "Message"},
			{Key: "assign", Title: "nav.inbox.assign", AttachTo: "communication.inbox", As: "panel", Page: "assign"},
		}}}},
	}
}

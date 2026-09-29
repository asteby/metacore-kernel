package i18n

import (
	"strings"
	"unicode"
)

// Humanize turns an i18n key or a Go/JSON field name into a readable label
// for lang. It is the last resort used by HumanizeMissing (and
// host.AppConfig.I18nHumanizeMissing) so metadata never shows a raw key.
//
// The subject of the key is picked as follows:
//
//   - the segment after the last container segment (fields, columns,
//     filters, actions, options, placeholders, messages):
//     "models.branches.modal.fields.postal_code" → "postal_code";
//   - otherwise, for model-level keys (table.title, modal.create_title, …),
//     the model segment: "models.branches.table.title" → "branches";
//   - otherwise the last segment.
//
// The subject is split on "_", "-" and camelCase boundaries ("CreatedAt",
// "created_at" → created, at). Spanish ("es", "es-MX", …) uses a small
// built-in dictionary of common field names ("postal_code" → "Código
// postal", "created_at" → "Fecha de creación", "name" → "Nombre"); words
// outside it, and every other language, get the English sentence-case form
// ("Postal code", "Created at"). Well-known acronyms stay upper-case (ID,
// URL, RFC, SKU…). A trailing "_id" is dropped ("customer_id" → "Customer").
func Humanize(key, lang string) string {
	subject := humanizeSubject(key)
	words := splitWords(subject)
	if len(words) > 1 && words[len(words)-1] == "id" {
		words = words[:len(words)-1]
	}
	if len(words) > 1 && (words[0] == "is" || words[0] == "has") {
		words = words[1:]
	}
	if len(words) == 0 {
		return key
	}
	if isSpanish(lang) {
		if v, ok := esPhrases[strings.Join(words, "_")]; ok {
			return v
		}
	}
	for i, w := range words {
		if acronyms[w] {
			words[i] = strings.ToUpper(w)
		}
	}
	if !acronyms[words[0]] {
		words[0] = upperFirst(words[0])
	}
	return strings.Join(words, " ")
}

var containerSegments = map[string]bool{
	"fields": true, "columns": true, "filters": true, "actions": true,
	"options": true, "placeholders": true, "messages": true, "labels": true,
	"custom_actions": true, "tabs": true, "sections": true, "steps": true,
}

var structuralSegments = map[string]bool{
	"models": true, "table": true, "modal": true, "title": true,
	"create_title": true, "edit_title": true, "delete_title": true,
	"search_placeholder": true, "placeholder": true, "label": true,
	"tooltip": true, "description": true, "confirm_message": true,
}

func humanizeSubject(key string) string {
	segs := strings.Split(key, ".")
	if len(segs) == 1 {
		return key
	}
	for i := len(segs) - 2; i >= 0; i-- {
		if containerSegments[segs[i]] && segs[i+1] != "" {
			return segs[i+1]
		}
	}
	if segs[0] == "models" && len(segs) > 1 && segs[1] != "" {
		return segs[1]
	}
	for i := len(segs) - 1; i >= 0; i-- {
		if !structuralSegments[segs[i]] && segs[i] != "" {
			return segs[i]
		}
	}
	return segs[len(segs)-1]
}

// splitWords lower-cases and splits on "_", "-", spaces and camelCase
// boundaries (keeping acronym runs together: "HTTPServer" → http, server).
func splitWords(s string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case r == '_' || r == '-' || unicode.IsSpace(r):
			flush()
			continue
		case unicode.IsUpper(r) && len(cur) > 0:
			prevLower := unicode.IsLower(cur[len(cur)-1]) || unicode.IsDigit(cur[len(cur)-1])
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if prevLower || (unicode.IsUpper(cur[len(cur)-1]) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return words
}

func upperFirst(s string) string {
	rs := []rune(s)
	if len(rs) == 0 {
		return s
	}
	rs[0] = unicode.ToUpper(rs[0])
	return string(rs)
}

func isSpanish(lang string) bool {
	l := normalizeLang(lang)
	return l == "es" || strings.HasPrefix(l, "es-")
}

var acronyms = map[string]bool{
	"id": true, "url": true, "uri": true, "rfc": true, "sku": true, "api": true,
	"ip": true, "iva": true, "curp": true, "clabe": true, "sat": true, "cfdi": true,
	"uuid": true, "html": true, "json": true, "pdf": true, "csv": true, "vin": true,
	"sms": true, "otp": true, "eta": true, "gps": true,
}

// esPhrases maps common snake_case field names (after dropping a trailing
// "_id" and a leading "is_"/"has_") to Spanish labels.
var esPhrases = map[string]string{
	"id": "ID", "name": "Nombre", "first_name": "Nombre", "last_name": "Apellido",
	"full_name": "Nombre completo", "title": "Título", "subtitle": "Subtítulo",
	"description": "Descripción", "notes": "Notas", "note": "Nota", "comments": "Comentarios",
	"email": "Correo electrónico", "phone": "Teléfono", "mobile": "Celular", "whatsapp": "WhatsApp",
	"address": "Dirección", "street": "Calle", "city": "Ciudad", "state": "Estado",
	"country": "País", "postal_code": "Código postal", "zip": "Código postal", "zip_code": "Código postal",
	"lat": "Latitud", "latitude": "Latitud", "lng": "Longitud", "lon": "Longitud", "longitude": "Longitud",
	"status": "Estatus", "type": "Tipo", "kind": "Tipo", "category": "Categoría", "tags": "Etiquetas",
	"code": "Código", "slug": "Identificador", "sku": "SKU", "barcode": "Código de barras",
	"price": "Precio", "cost": "Costo", "amount": "Monto", "total": "Total", "subtotal": "Subtotal",
	"tax": "Impuesto", "discount": "Descuento", "fee": "Tarifa", "currency": "Moneda",
	"quantity": "Cantidad", "qty": "Cantidad", "stock": "Existencias", "unit": "Unidad",
	"date": "Fecha", "start_date": "Fecha de inicio", "end_date": "Fecha de fin",
	"due_date": "Fecha de vencimiento", "created_at": "Fecha de creación",
	"updated_at": "Última actualización", "deleted_at": "Fecha de eliminación",
	"created_by": "Creado por", "updated_by": "Actualizado por",
	"active": "Activo", "enabled": "Habilitado", "visible": "Visible", "default": "Predeterminado",
	"sort": "Orden", "order": "Orden", "position": "Posición", "priority": "Prioridad",
	"image": "Imagen", "image_url": "Imagen", "logo": "Logo", "logo_url": "Logo", "photo": "Foto",
	"color": "Color", "icon": "Ícono", "url": "URL", "website": "Sitio web", "link": "Enlace",
	"user": "Usuario", "customer": "Cliente", "client": "Cliente", "supplier": "Proveedor",
	"vendor": "Proveedor", "product": "Producto", "products": "Productos", "organization": "Organización",
	"company": "Empresa", "branch": "Sucursal", "branches": "Sucursales", "warehouse": "Almacén",
	"role": "Rol", "password": "Contraseña", "username": "Usuario", "language": "Idioma",
	"timezone": "Zona horaria", "hours": "Horario", "schedule": "Horario", "brand": "Marca",
	"model": "Modelo", "year": "Año", "size": "Tamaño", "weight": "Peso", "width": "Ancho",
	"height": "Alto", "length": "Largo", "legal_name": "Razón social", "tax_id": "RFC",
	"rfc": "RFC", "reference": "Referencia", "number": "Número", "folio": "Folio",
	"payment_method": "Método de pago", "method": "Método", "source": "Origen", "channel": "Canal",
	"message": "Mensaje", "subject": "Asunto", "body": "Contenido", "content": "Contenido",
	"settings": "Configuración", "store_settings": "Configuración de la tienda",
	"orders": "Pedidos", "customers": "Clientes", "users": "Usuarios", "delivery_zones": "Zonas de envío",
}

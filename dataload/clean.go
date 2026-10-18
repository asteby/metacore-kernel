package dataload

import (
	"regexp"
	"strings"
)

func validCleanRule(r string) bool {
	if arg, ok := strings.CutPrefix(r, CleanDropLines+":"); ok {
		_, err := regexp.Compile(arg)
		return err == nil && arg != ""
	}
	return knownClean[r]
}

func cleanRuleError(r string) Message {
	if arg, ok := strings.CutPrefix(r, CleanDropLines+":"); ok {
		if arg == "" {
			return msg(CodeCleanDropLinesEmpty)
		}
		if _, err := regexp.Compile(arg); err != nil {
			return msg(CodeCleanDropLinesInvalid, "pattern", arg, "error", err.Error())
		}
	}
	return msg(CodeCleanUnknown, "rule", r)
}

func dropLines(pattern, s string) string {
	re := compileCached(pattern)
	lines := strings.Split(s, "\n")
	out := lines[:0:0]
	for _, l := range lines {
		if !re.MatchString(l) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

var (
	reDropBlocks = regexp.MustCompile(`(?is)<(script|style|iframe|object|embed)\b.*?</\s*(script|style|iframe|object|embed)\s*>`)
	reComments   = regexp.MustCompile(`(?s)<!--.*?-->`)
	reAnyTag     = regexp.MustCompile(`(?s)<(/?)([a-zA-Z][a-zA-Z0-9]*)\b([^>]*)>`)
	reHref       = regexp.MustCompile(`(?is)\bhref\s*=\s*("([^"]*)"|'([^']*)')`)
	reBlockStart = regexp.MustCompile(`(?i)^\s*<(p|ul|ol|li|h[1-6]|table|blockquote|div)\b`)
)

var allowedTags = map[string]bool{
	"p": true, "br": true, "ul": true, "ol": true, "li": true, "strong": true, "b": true,
	"em": true, "i": true, "h2": true, "h3": true, "h4": true, "a": true,
}

// sanitizeHTML conserva solo etiquetas de texto de la lista blanca; descarta
// atributos (salvo href http/https) y el contenido de scripts y estilos. Las
// etiquetas no permitidas se quitan pero su texto se conserva.
func sanitizeHTML(s string) string {
	s = reComments.ReplaceAllString(s, "")
	s = reDropBlocks.ReplaceAllString(s, "")
	s = reAnyTag.ReplaceAllStringFunc(s, func(tag string) string {
		m := reAnyTag.FindStringSubmatch(tag)
		closing, name, attrs := m[1] == "/", strings.ToLower(m[2]), m[3]
		if !allowedTags[name] {
			return ""
		}
		if closing {
			return "</" + name + ">"
		}
		if name == "br" {
			return "<br>"
		}
		if name == "a" {
			if h := reHref.FindStringSubmatch(attrs); h != nil {
				href := strings.TrimSpace(h[2] + h[3])
				if l := strings.ToLower(href); (strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")) && !strings.ContainsAny(href, "\"'<> ") {
					return `<a href="` + href + `">`
				}
			}
			return "<a>"
		}
		return "<" + name + ">"
	})
	// Un "<" que no abrió etiqueta válida no debe quedar como marcado.
	s = strings.ReplaceAll(s, "<", "&lt;")
	for _, name := range []string{"p", "br", "ul", "ol", "li", "strong", "b", "em", "i", "h2", "h3", "h4", "a"} {
		s = strings.ReplaceAll(s, "&lt;"+name+">", "<"+name+">")
		s = strings.ReplaceAll(s, "&lt;/"+name+">", "</"+name+">")
	}
	s = regexp.MustCompile(`&lt;a href="(https?://[^"<> ']*)">`).ReplaceAllString(s, `<a href="$1">`)
	return strings.TrimSpace(s)
}

var (
	reListOpen  = regexp.MustCompile(`(?i)<(ul|ol)\b`)
	reListClose = regexp.MustCompile(`(?i)</(ul|ol)>`)
	reBlockEnd  = regexp.MustCompile(`(?i)^\s*</(p|ul|ol|li|h[1-6]|table|blockquote|div)>`)
)

// autoParagraphs envuelve en <p> las líneas de texto sueltas (las consecutivas
// forman un solo párrafo con <br>); deja tal cual las líneas que ya son HTML de
// bloque y todo lo que está dentro de una lista.
func autoParagraphs(s string) string {
	var out, buf []string
	flush := func() {
		if len(buf) > 0 {
			out = append(out, "<p>"+strings.Join(buf, "<br>")+"</p>")
			buf = nil
		}
	}
	depth := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}
		raw := depth > 0 || reBlockStart.MatchString(line) || reBlockEnd.MatchString(line)
		depth += len(reListOpen.FindAllString(line, -1)) - len(reListClose.FindAllString(line, -1))
		if depth < 0 {
			depth = 0
		}
		if raw {
			flush()
			out = append(out, line)
			continue
		}
		buf = append(buf, line)
	}
	flush()
	return strings.Join(out, "\n")
}

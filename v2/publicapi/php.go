package publicapi

import (
	"net/http"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// PHP's index.php makes the trailing slash optional by adding one to every
// REQUEST_URI before routing:
//
//	$uri = preg_replace('#([^/])\?#', '\1/?', $uri);
//	$uri = preg_replace('#([^/])$#', '\1/', $uri);
//
// The second line runs on the whole URI, so a query string gains a slash
// too: "/a?x=1" becomes "/a/?x=1/". PHP parses $_GET before the rewrite, so
// handlers never see that slash, but error messages and apiResponseError's
// "url" do.
var (
	phpSlashBeforeQuery = regexp.MustCompile(`([^/])\?`)
	phpSlashAtEnd       = regexp.MustCompile(`([^/])$`)
)

// phpRequestURI is the REQUEST_URI PHP saw: r's after PHP's rewrite.
func phpRequestURI(r *http.Request) string {
	uri := r.RequestURI
	if uri == "" {
		uri = r.URL.RequestURI()
	}
	uri = phpSlashBeforeQuery.ReplaceAllString(uri, "$1/?")
	return phpSlashAtEnd.ReplaceAllString(uri, "$1/")
}

// phpPathInfo is Symfony's Request::getPathInfo after the rewrite: the raw,
// still percent-encoded, path with a trailing slash.
func phpPathInfo(r *http.Request) string {
	p, _, _ := strings.Cut(phpRequestURI(r), "?")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// phpFormats is Symfony 3.0's Request::$formats, in its order.
var phpFormats = []struct {
	format string
	types  []string
}{
	{"html", []string{"text/html", "application/xhtml+xml"}},
	{"txt", []string{"text/plain"}},
	{"js", []string{"application/javascript", "application/x-javascript", "text/javascript"}},
	{"css", []string{"text/css"}},
	{"json", []string{"application/json", "application/x-json"}},
	{"jsonld", []string{"application/ld+json"}},
	{"xml", []string{"text/xml", "application/xml", "application/x-xml"}},
	{"rdf", []string{"application/rdf+xml"}},
	{"atom", []string{"application/atom+xml"}},
	{"rss", []string{"application/rss+xml"}},
	{"form", []string{"application/x-www-form-urlencoded"}},
}

// phpContentFormat is Symfony's Request::getContentType: the format name of
// a Content-Type ("json" for "application/json; charset=utf-8"), or "" for a
// missing or unknown one. Matching is case-sensitive, as in Symfony.
func phpContentFormat(contentType string) string {
	canonical := ""
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		canonical = strings.TrimSpace(contentType[:i])
	}
	for _, f := range phpFormats {
		for _, t := range f.types {
			if t == contentType || (canonical != "" && t == canonical) {
				return f.format
			}
		}
	}
	return ""
}

const hexDigits = "0123456789abcdef"

// appendPHPString appends s to b as PHP's json_encode encodes a string:
// slashes escaped and non-ASCII as lowercase \uXXXX. hex adds the
// JSON_HEX_TAG|JSON_HEX_APOS|JSON_HEX_QUOT|JSON_HEX_AMP flags Symfony's
// JsonResponse uses, whose escapes PHP writes in uppercase.
func appendPHPString(b []byte, s string, hex bool) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			i += size
			if r >= 0x10000 {
				r1, r2 := utf16.EncodeRune(r)
				b = appendUnicodeEscape(b, r1)
				b = appendUnicodeEscape(b, r2)
			} else {
				b = appendUnicodeEscape(b, r)
			}
			continue
		}
		i++
		switch {
		case c == '"' && hex:
			b = append(b, `\u0022`...)
		case c == '"':
			b = append(b, `\"`...)
		case c == '\\':
			b = append(b, `\\`...)
		case c == '/':
			b = append(b, `\/`...)
		case c == '\b':
			b = append(b, `\b`...)
		case c == '\f':
			b = append(b, `\f`...)
		case c == '\n':
			b = append(b, `\n`...)
		case c == '\r':
			b = append(b, `\r`...)
		case c == '\t':
			b = append(b, `\t`...)
		case c < 0x20:
			b = appendUnicodeEscape(b, rune(c))
		case hex && c == '<':
			b = append(b, `\u003C`...)
		case hex && c == '>':
			b = append(b, `\u003E`...)
		case hex && c == '&':
			b = append(b, `\u0026`...)
		case hex && c == '\'':
			b = append(b, `\u0027`...)
		default:
			b = append(b, c)
		}
	}
	return append(b, '"')
}

func appendUnicodeEscape(b []byte, r rune) []byte {
	return append(b, '\\', 'u',
		hexDigits[r>>12&0xf], hexDigits[r>>8&0xf], hexDigits[r>>4&0xf], hexDigits[r&0xf])
}

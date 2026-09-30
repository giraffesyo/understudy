package modules

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math"
	"math/big"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// This file ports the request-body encoders the uri module uses: Python's
// json.dumps, urllib.parse.urlencode and module_utils.urls'
// prepare_multipart (an email.mime multipart rendered with the HTTP policy).

// pyStrValue is str(v) for a JSON-shaped value.
func pyStrValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return pyFloatRepr(t)
	case []any, map[string]any:
		return pyReprValue(v)
	}
	return fmt.Sprintf("%v", v)
}

// pyReprValue is repr(v) for a JSON-shaped value.
func pyReprValue(v any) string {
	switch t := v.(type) {
	case string:
		return pyStrRepr(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = pyReprValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyStrRepr(k) + ": " + pyReprValue(t[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return pyStrValue(v)
}

// pyFloatRepr is repr(float): positional between 1e-4 and 1e16,
// scientific outside.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	if a := math.Abs(f); f == 0 || (a >= 1e-4 && a < 1e16) {
		return pyFloat(f)
	}
	return strconv.FormatFloat(f, 'e', -1, 64)
}

// pyJSONDumps is json.dumps(v) with its default separators and
// ensure_ascii. Plain maps are ordered by key (the controller pre-renders
// bodies whose insertion order matters).
func pyJSONDumps(v any) string {
	var b strings.Builder
	var write func(v any)
	write = func(v any) {
		switch t := v.(type) {
		case nil:
			b.WriteString("null")
		case bool:
			if t {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
		case string:
			b.WriteString(pyJSONString(t))
		case int64:
			b.WriteString(strconv.FormatInt(t, 10))
		case int:
			b.WriteString(strconv.Itoa(t))
		case float64:
			b.WriteString(pyFloatRepr(t))
		case []any:
			b.WriteByte('[')
			for i, e := range t {
				if i > 0 {
					b.WriteString(", ")
				}
				write(e)
			}
			b.WriteByte(']')
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(pyJSONString(k))
				b.WriteString(": ")
				write(t[k])
			}
			b.WriteByte('}')
		default:
			b.WriteString(pyJSONString(fmt.Sprintf("%v", t)))
		}
	}
	write(v)
	return b.String()
}

// pyJSONString quotes s like json.dumps with ensure_ascii=True.
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20 || r >= 0x7f && r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			case r > 0xffff:
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyQuotePlus is urllib.parse.quote_plus over UTF-8.
func pyQuotePlus(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.IndexByte("_.-~", c) >= 0:
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// uriFormURLEncoded is the uri module's form_urlencoded: a dict or a list
// of pairs becomes urlencode(..., doseq=True); list values repeat the key
// and None values are dropped.
func uriFormURLEncoded(body any) (any, error) {
	var pairs [][2]any
	switch t := body.(type) {
	case string:
		return t, nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			pairs = append(pairs, [2]any{k, t[k]})
		}
	case []any:
		for _, item := range t {
			var kv []any
			switch it := item.(type) {
			case []any:
				kv = it
			case string:
				for _, r := range it {
					kv = append(kv, string(r))
				}
			case map[string]any:
				keys := make([]string, 0, len(it))
				for k := range it {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					kv = append(kv, k)
				}
			default:
				return nil, &uriTypeError{fmt.Sprintf("cannot unpack non-iterable %s object", pyTypeName(item))}
			}
			if len(kv) < 2 {
				return nil, fmt.Errorf("not enough values to unpack (expected 2, got %d)", len(kv))
			}
			if len(kv) > 2 {
				return nil, fmt.Errorf("too many values to unpack (expected 2)")
			}
			pairs = append(pairs, [2]any{kv[0], kv[1]})
		}
	default:
		return body, nil
	}
	var out []string
	for _, p := range pairs {
		key := pyStrValue(p[0])
		var values []any
		switch v := p[1].(type) {
		case []any:
			values = v
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				values = append(values, k)
			}
		default:
			values = []any{v}
		}
		for _, v := range values {
			if v == nil {
				continue
			}
			out = append(out, pyQuotePlus(key)+"="+pyQuotePlus(pyStrValue(v)))
		}
	}
	return strings.Join(out, "&"), nil
}

// uriTypeError is a Python TypeError escaping the module (a crash).
type uriTypeError struct{ msg string }

func (e *uriTypeError) Error() string { return e.msg }

// multipartFileKey carries a controller file's content (base64) for a
// form-multipart field whose filename the uri action resolved locally.
const multipartFileKey = "_understudy_content_b64"

// pyMimeTypes is mimetypes' default table (types_map plus the common
// non-strict entries) for the extensions prepare_multipart guesses from.
var pyMimeTypes = map[string]string{
	".js": "text/javascript", ".mjs": "text/javascript", ".json": "application/json",
	".webmanifest": "application/manifest+json", ".doc": "application/msword", ".dot": "application/msword",
	".wiz": "application/msword", ".nq": "application/n-quads", ".nt": "application/n-triples",
	".bin": "application/octet-stream", ".a": "application/octet-stream", ".dll": "application/octet-stream",
	".exe": "application/octet-stream", ".o": "application/octet-stream", ".obj": "application/octet-stream",
	".so": "application/octet-stream", ".oda": "application/oda", ".ogx": "application/ogg",
	".pdf": "application/pdf", ".p7c": "application/pkcs7-mime", ".ps": "application/postscript",
	".ai": "application/postscript", ".eps": "application/postscript", ".trig": "application/trig",
	".m3u": "application/vnd.apple.mpegurl", ".m3u8": "application/vnd.apple.mpegurl",
	".xls": "application/vnd.ms-excel", ".xlb": "application/vnd.ms-excel",
	".eot": "application/vnd.ms-fontobject", ".ppt": "application/vnd.ms-powerpoint",
	".pot": "application/vnd.ms-powerpoint", ".ppa": "application/vnd.ms-powerpoint",
	".pps": "application/vnd.ms-powerpoint", ".pwz": "application/vnd.ms-powerpoint",
	".odg": "application/vnd.oasis.opendocument.graphics", ".odp": "application/vnd.oasis.opendocument.presentation",
	".ods": "application/vnd.oasis.opendocument.spreadsheet", ".odt": "application/vnd.oasis.opendocument.text",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".rar":  "application/vnd.rar", ".wasm": "application/wasm", ".7z": "application/x-7z-compressed",
	".bcpio": "application/x-bcpio", ".cpio": "application/x-cpio", ".csh": "application/x-csh",
	".deb": "application/x-debian-package", ".dvi": "application/x-dvi", ".gtar": "application/x-gtar",
	".hdf": "application/x-hdf", ".h5": "application/x-hdf5", ".latex": "application/x-latex",
	".mif": "application/x-mif", ".cdf": "application/x-netcdf", ".nc": "application/x-netcdf",
	".p12": "application/x-pkcs12", ".php": "application/x-httpd-php", ".pfx": "application/x-pkcs12",
	".ram": "application/x-pn-realaudio", ".pyc": "application/x-python-code", ".pyo": "application/x-python-code",
	".rpm": "application/x-rpm", ".sh": "application/x-sh", ".shar": "application/x-shar",
	".swf": "application/x-shockwave-flash", ".sv4cpio": "application/x-sv4cpio", ".sv4crc": "application/x-sv4crc",
	".tar": "application/x-tar", ".tcl": "application/x-tcl", ".tex": "application/x-tex",
	".texi": "application/x-texinfo", ".texinfo": "application/x-texinfo", ".roff": "application/x-troff",
	".t": "application/x-troff", ".tr": "application/x-troff", ".man": "application/x-troff-man",
	".me": "application/x-troff-me", ".ms": "application/x-troff-ms", ".ustar": "application/x-ustar",
	".src": "application/x-wais-source", ".xsl": "application/xml", ".rdf": "application/xml",
	".wsdl": "application/xml", ".xpdl": "application/xml", ".yaml": "application/yaml", ".yml": "application/yaml",
	".zip": "application/zip", ".3gp": "audio/3gpp", ".3gpp": "audio/3gpp", ".3g2": "audio/3gpp2",
	".3gpp2": "audio/3gpp2", ".aac": "audio/aac", ".adts": "audio/aac", ".loas": "audio/aac",
	".ass": "audio/aac", ".au": "audio/basic", ".snd": "audio/basic", ".flac": "audio/flac",
	".mka": "audio/matroska", ".m4a": "audio/mp4", ".mp4a": "audio/mp4", ".mp3": "audio/mpeg",
	".mp2": "audio/mpeg", ".ogg": "audio/ogg", ".opus": "audio/opus", ".aif": "audio/x-aiff",
	".aifc": "audio/x-aiff", ".aiff": "audio/x-aiff", ".ra": "audio/x-pn-realaudio", ".wav": "audio/vnd.wave",
	".otf": "font/otf", ".ttf": "font/ttf", ".weba": "audio/webm", ".woff": "font/woff", ".woff2": "font/woff2",
	".avif": "image/avif", ".bmp": "image/bmp", ".emf": "image/emf", ".fits": "image/fits",
	".g3": "image/g3fax", ".gif": "image/gif", ".ief": "image/ief", ".jp2": "image/jp2",
	".jpg": "image/jpeg", ".jpe": "image/jpeg", ".jpeg": "image/jpeg", ".jfif": "image/jpeg",
	".jpm": "image/jpm", ".jpx": "image/jpx", ".heic": "image/heic", ".heif": "image/heif",
	".png": "image/png", ".svg": "image/svg+xml", ".t38": "image/t38", ".tiff": "image/tiff",
	".tif": "image/tiff", ".tfx": "image/tiff-fx", ".ico": "image/vnd.microsoft.icon", ".webp": "image/webp",
	".wmf": "image/wmf", ".ras": "image/x-cmu-raster", ".pnm": "image/x-portable-anymap",
	".pbm": "image/x-portable-bitmap", ".pgm": "image/x-portable-graymap", ".ppm": "image/x-portable-pixmap",
	".rgb": "image/x-rgb", ".xbm": "image/x-xbitmap", ".xpm": "image/x-xpixmap", ".xwd": "image/x-xwindowdump",
	".eml": "message/rfc822", ".mht": "message/rfc822", ".mhtml": "message/rfc822", ".nws": "message/rfc822",
	".gltf": "model/gltf+json", ".glb": "model/gltf-binary", ".stl": "model/stl", ".css": "text/css",
	".csv": "text/csv", ".html": "text/html", ".htm": "text/html", ".md": "text/markdown",
	".markdown": "text/markdown", ".n3": "text/n3", ".txt": "text/plain", ".bat": "text/plain",
	".c": "text/plain", ".h": "text/plain", ".ksh": "text/plain", ".pl": "text/plain", ".srt": "text/plain",
	".rtx": "text/richtext", ".rtf": "text/rtf", ".tsv": "text/tab-separated-values", ".vtt": "text/vtt",
	".py": "text/x-python", ".rst": "text/x-rst", ".etx": "text/x-setext", ".sgm": "text/x-sgml",
	".sgml": "text/x-sgml", ".vcf": "text/x-vcard", ".xml": "text/xml", ".mkv": "video/matroska",
	".mk3d": "video/matroska-3d", ".mp4": "video/mp4", ".mpeg": "video/mpeg", ".m1v": "video/mpeg",
	".mpa": "video/mpeg", ".mpe": "video/mpeg", ".mpg": "video/mpeg", ".mov": "video/quicktime",
	".qt": "video/quicktime", ".webm": "video/webm", ".avi": "video/vnd.avi", ".movie": "video/x-sgi-movie",
	".mid": "audio/midi", ".midi": "audio/midi", ".pct": "image/pict", ".pic": "image/pict",
	".pict": "image/pict", ".xul": "text/xul",
}

// pyGuessType is mimetypes.guess_type(name, strict=False)[0] ("" if
// unknown). Compression suffixes (.gz, .bz2, ...) are encodings, not types.
func pyGuessType(name string) string {
	base := path.Base(name)
	if name == "" {
		return ""
	}
	suffixMap := map[string]string{".svgz": ".svg.gz", ".tgz": ".tar.gz", ".taz": ".tar.gz",
		".tz": ".tar.gz", ".tbz2": ".tar.bz2", ".txz": ".tar.xz"}
	ext := path.Ext(base)
	root := strings.TrimSuffix(base, ext)
	for {
		if full, ok := suffixMap[ext]; ok {
			root += full
			ext = path.Ext(root)
			root = strings.TrimSuffix(root, ext)
			continue
		}
		break
	}
	switch ext {
	case ".gz", ".Z", ".bz2", ".xz", ".br", ".zst":
		ext = path.Ext(root)
	}
	if t, ok := pyMimeTypes[ext]; ok {
		return t
	}
	if t, ok := pyMimeTypes[strings.ToLower(ext)]; ok {
		return t
	}
	return ""
}

// mimeParam renders one Content-Disposition parameter the way
// email.message.Message.set_param does (always quoted; RFC 2231 for
// non-ASCII values).
func mimeParam(name, value string) string {
	ascii := true
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if !ascii {
		var b strings.Builder
		for _, c := range []byte(value) {
			if c < 0x80 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_.-~", c) >= 0) {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
		return name + "*=utf-8''" + b.String()
	}
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return name + `="` + value + `"`
}

// crlfLines normalizes every line ending to CRLF (email.generator's
// _write_lines under the HTTP policy).
func crlfLines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	parts := strings.Split(s, "\n")
	var b strings.Builder
	for _, p := range parts[:len(parts)-1] {
		b.WriteString(p)
		b.WriteString("\r\n")
	}
	b.WriteString(parts[len(parts)-1])
	return b.String()
}

// mimeBoundary is email.generator's boundary: 15 '=' + 19 random digits + "==".
func mimeBoundary() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	return fmt.Sprintf("===============%019d==", n.Int64())
}

// prepareMultipart is module_utils.urls.prepare_multipart: the
// Content-Type header (with its boundary) and the body.
func prepareMultipart(fields any) (string, []byte, error) {
	m, ok := fields.(map[string]any)
	if !ok {
		return "", nil, fmt.Errorf("Mapping is required, cannot be type %s", pyTypeName(fields))
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	boundary := mimeBoundary()
	var body strings.Builder
	for _, field := range names {
		value := m[field]
		var mainType, subType, content, filename string
		var hasContent, hasFile bool
		encoding := "base64"
		var fileData []byte
		var fileGiven bool
		switch v := value.(type) {
		case string:
			mainType, subType, content, hasContent = "text", "plain", v, v != ""
		case map[string]any:
			if f, ok := v["filename"]; ok && f != nil {
				filename = pyStrValue(f)
				hasFile = filename != ""
			}
			if e, ok := v["multipart_encoding"]; ok && e != nil && pyStrValue(e) != "" {
				encoding = pyStrValue(e)
			}
			if c, ok := v["content"]; ok && c != nil {
				content = pyStrValue(c)
				hasContent = content != ""
			}
			if b64, ok := v[multipartFileKey].(string); ok {
				fileData, _ = base64.StdEncoding.DecodeString(b64)
				fileGiven = true
			}
			if !hasFile && !hasContent {
				return "", nil, fmt.Errorf("at least one of filename or content must be provided")
			}
			mime := ""
			if mt, ok := v["mime_type"]; ok && mt != nil {
				mime = pyStrValue(mt)
			}
			if mime == "" {
				mime = pyGuessType(filename)
				if mime == "" {
					mime = "application/octet-stream"
				}
			}
			mainType, subType, _ = strings.Cut(mime, "/")
		default:
			return "", nil, fmt.Errorf("value must be a string, or mapping, cannot be type %s", pyTypeName(value))
		}

		var headers []string
		var payload string
		if !hasContent && hasFile {
			if encoding != "base64" && encoding != "7or8bit" {
				return "", nil, fmt.Errorf("multipart_encoding must be one of dict_keys(['base64', '7or8bit']).")
			}
			data := fileData
			if !fileGiven {
				var err error
				if data, err = os.ReadFile(filename); err != nil {
					return "", nil, &uriOSError{err}
				}
			}
			if encoding == "base64" {
				enc := base64.StdEncoding.EncodeToString(data)
				var lines []string
				for len(enc) > 76 {
					lines = append(lines, enc[:76])
					enc = enc[76:]
				}
				lines = append(lines, enc)
				payload = strings.Join(lines, "\n") + "\n"
				headers = append(headers, "Content-Transfer-Encoding: base64")
			} else {
				cte := "7bit"
				for _, c := range data {
					if c >= 0x80 {
						cte = "8bit"
						break
					}
				}
				payload = string(data)
				headers = append(headers, "Content-Transfer-Encoding: "+cte)
			}
			headers = append(headers, "Content-Type: "+mainType+"/"+subType)
		} else {
			headers = append(headers, "Content-Type: "+mainType+"/"+subType)
			payload = content
		}
		disp := "Content-Disposition: form-data; " + mimeParam("name", field)
		if filename != "" {
			disp += "; " + mimeParam("filename", path.Base(filename))
		}
		headers = append(headers, disp)

		body.WriteString("--" + boundary + "\r\n")
		for _, h := range headers {
			body.WriteString(h + "\r\n")
		}
		body.WriteString("\r\n")
		body.WriteString(crlfLines(payload))
		body.WriteString("\r\n")
	}
	if len(names) == 0 {
		body.WriteString("\r\n")
	}
	body.WriteString("--" + boundary + "--\r\n")
	return `multipart/form-data; boundary="` + boundary + `"`, []byte(body.String()), nil
}

// uriOSError is an OSError escaping prepare_multipart (a module crash).
type uriOSError struct{ err error }

func (e *uriOSError) Error() string { return pyOSErrorStr(e.err) }

// pyOSErrorStr renders an OSError raised for a str path.
func pyOSErrorStr(err error) string {
	return strings.Replace(pyOSError(err), ": b'", ": '", 1)
}

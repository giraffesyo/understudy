package modules

import (
	"bufio"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// pgpKeyInfo is one primary key or subkey: its 8-byte key ID and full
// fingerprint, both upper-case hex (rpm_key's LibRPM.identify_keys).
type pgpKeyInfo struct {
	KeyID       string
	Fingerprint string
}

var pgpPubkeyRe = regexp.MustCompile(`(?s)-----BEGIN PGP PUBLIC KEY BLOCK-----.*?-----END PGP PUBLIC KEY BLOCK-----`)

// isPGPPubkey is rpm_key's is_pubkey: the data contains an armored public
// key block.
func isPGPPubkey(data []byte) bool { return pgpPubkeyRe.Match(data) }

// pgpDearmor decodes the first ASCII-armored block in data (the part of
// librpm's pgpParsePkts the modules rely on). Binary (non-armored) input is
// returned unchanged.
func pgpDearmor(data []byte) ([]byte, error) {
	s := string(data)
	start := strings.Index(s, "-----BEGIN PGP ")
	if start < 0 {
		if len(data) > 0 && data[0]&0x80 != 0 {
			return data, nil
		}
		return nil, fmt.Errorf("no PGP armor found")
	}
	sc := bufio.NewScanner(strings.NewReader(s[start:]))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	sc.Scan() // BEGIN line
	inHeaders := true
	var b64 strings.Builder
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if strings.HasPrefix(line, "-----END PGP ") {
			break
		}
		if inHeaders {
			if line == "" {
				inHeaders = false
				continue
			}
			if strings.Contains(line, ": ") {
				continue
			}
			inHeaders = false
		}
		if strings.HasPrefix(line, "=") && len(line) == 5 {
			continue // CRC24 checksum
		}
		b64.WriteString(line)
	}
	out, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		return nil, fmt.Errorf("invalid PGP armor: %v", err)
	}
	return out, nil
}

// pgpPacketHeader parses an RFC 9580 packet header at off, returning the
// tag, body length and header length (tag<0 on error or unsupported).
func pgpPacketHeader(pkt []byte, off int) (tag, bodyLen, hdrLen int) {
	n := len(pkt)
	if off >= n {
		return -1, 0, 0
	}
	b := pkt[off]
	if b&0x40 != 0 {
		tag = int(b & 0x3f)
		if off+1 >= n {
			return -1, 0, 0
		}
		l := int(pkt[off+1])
		switch {
		case l < 192:
			return tag, l, 2
		case l < 224:
			if off+2 >= n {
				return -1, 0, 0
			}
			return tag, ((l - 192) << 8) + int(pkt[off+2]) + 192, 3
		case l == 255:
			if off+5 >= n {
				return -1, 0, 0
			}
			return tag, int(pkt[off+2])<<24 | int(pkt[off+3])<<16 | int(pkt[off+4])<<8 | int(pkt[off+5]), 6
		}
		return -1, 0, 0
	}
	tag = int(b>>2) & 0x0f
	switch b & 0x03 {
	case 0:
		if off+1 >= n {
			return -1, 0, 0
		}
		return tag, int(pkt[off+1]), 2
	case 1:
		if off+2 >= n {
			return -1, 0, 0
		}
		return tag, int(pkt[off+1])<<8 | int(pkt[off+2]), 3
	case 2:
		if off+4 >= n {
			return -1, 0, 0
		}
		return tag, int(pkt[off+1])<<24 | int(pkt[off+2])<<16 | int(pkt[off+3])<<8 | int(pkt[off+4]), 5
	}
	return -1, 0, 0
}

// pgpIdentifyKeys returns the key ID and fingerprint of the primary key
// and every subkey in an armored (or binary) key, in packet order.
func pgpIdentifyKeys(data []byte) ([]pgpKeyInfo, error) {
	pkt, err := pgpDearmor(data)
	if err != nil {
		return nil, fmt.Errorf("Unable to parse PGP key armor")
	}
	var keys []pgpKeyInfo
	for off := 0; off < len(pkt); {
		tag, bodyLen, hdrLen := pgpPacketHeader(pkt, off)
		if tag < 0 {
			break
		}
		body := off + hdrLen
		if (tag == 6 || tag == 14) && body < len(pkt) && body+bodyLen <= len(pkt) {
			b := pkt[body : body+bodyLen]
			switch b[0] {
			case 4:
				h := sha1.New()
				h.Write([]byte{0x99, byte(bodyLen >> 8), byte(bodyLen)})
				h.Write(b)
				fp := strings.ToUpper(hex.EncodeToString(h.Sum(nil)))
				keys = append(keys, pgpKeyInfo{KeyID: fp[len(fp)-16:], Fingerprint: fp})
			case 6:
				h := sha256.New()
				h.Write([]byte{0x9b, byte(bodyLen >> 24), byte(bodyLen >> 16), byte(bodyLen >> 8), byte(bodyLen)})
				h.Write(b)
				fp := strings.ToUpper(hex.EncodeToString(h.Sum(nil)))
				keys = append(keys, pgpKeyInfo{KeyID: fp[:16], Fingerprint: fp})
			default:
				return nil, fmt.Errorf("Unhandled key version %#02x", b[0])
			}
		}
		off += hdrLen + bodyLen
	}
	return keys, nil
}

// fetchURLStatus is module_utils.urls.fetch_url reduced to what the
// repository/key modules need: GET with Ansible's user agent, optional
// certificate validation, and the HTTP status for error messages.
func fetchURLStatus(url string, validateCerts bool, timeout time.Duration) ([]byte, int, error) {
	if path, ok := strings.CutPrefix(url, "file://"); ok {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, -1, err
		}
		return b, 200, nil
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if !validateCerts {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := &http.Client{Timeout: timeout, Transport: tr}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, -1, err
	}
	req.Header.Set("User-Agent", "ansible-httpget")
	resp, err := client.Do(req)
	if err != nil {
		return nil, -1, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return body, resp.StatusCode, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, strings.TrimPrefix(resp.Status, fmt.Sprintf("%d ", resp.StatusCode)))
	}
	return body, resp.StatusCode, nil
}

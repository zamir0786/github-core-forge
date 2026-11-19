package main

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func jsonMarshal(v interface{}) ([]byte, error) { return json.Marshal(v) }

func sha1Hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

func unb64(text string) []byte {
	b, _ := base64.StdEncoding.DecodeString(text)
	return b
}

func toB64URL(text string) string {
	s := base64.StdEncoding.EncodeToString([]byte(text))
	s = strings.ReplaceAll(s, "+", "-")
	s = strings.ReplaceAll(s, "/", "_")
	return strings.TrimRight(s, "=")
}

func fromB64URL(text string) string {
	s := strings.ReplaceAll(text, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}

const gitHexLower = "0123456789abcdef"

func gitHexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func pkt(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	n := len(payload) + 4
	out[0] = gitHexLower[(n>>12)&0xf]
	out[1] = gitHexLower[(n>>8)&0xf]
	out[2] = gitHexLower[(n>>4)&0xf]
	out[3] = gitHexLower[n&0xf]
	copy(out[4:], payload)
	return out
}

func pktString(payload string) []byte { return pkt([]byte(payload)) }

var flushBytes = []byte("0000")
var delimBytes = []byte("0001")

func pktLines(parts ...[]byte) []byte {
	out := make([]byte, 0, 256)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

type Packet struct {
	Kind    string
	Payload []byte
	Raw     []byte
	Next    int
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func readPacket(buf []byte, offset int) Packet {
	if offset+4 > len(buf) {
		return Packet{Kind: "bad", Payload: []byte{}, Raw: buf[offset:], Next: len(buf)}
	}
	h0, ok0 := gitHexNibble(buf[offset])
	h1, ok1 := gitHexNibble(buf[offset+1])
	h2, ok2 := gitHexNibble(buf[offset+2])
	h3, ok3 := gitHexNibble(buf[offset+3])
	if !ok0 || !ok1 || !ok2 || !ok3 {
		return Packet{Kind: "bad", Payload: []byte{}, Raw: buf[offset : offset+4], Next: offset + 4}
	}
	length := int(h0)<<12 | int(h1)<<8 | int(h2)<<4 | int(h3)
	if length == 0 {
		return Packet{Kind: "flush", Payload: []byte{}, Raw: buf[offset : offset+4], Next: offset + 4}
	}
	if length == 1 {
		return Packet{Kind: "delim", Payload: []byte{}, Raw: buf[offset : offset+4], Next: offset + 4}
	}
	if length < 4 || offset+length > len(buf) {
		return Packet{Kind: "bad", Payload: []byte{}, Raw: buf[offset:], Next: len(buf)}
	}
	return Packet{Kind: "data", Payload: buf[offset+4 : offset+length], Raw: buf[offset : offset+length], Next: offset + length}
}

func packetsOf(buf []byte) [][]byte {
	out := make([][]byte, 0)
	for off := 0; off < len(buf); {
		p := readPacket(buf, off)
		off = p.Next
		if p.Kind == "data" {
			out = append(out, p.Payload)
		} else if p.Kind == "bad" {
			break
		}
	}
	return out
}

const epochMS int64 = 1766000000000

// The trial clock is fixed for the whole process. Parsing the environment on
// every tick() made issue creation and git mutation needlessly expensive.
var trialNowMS = parseTrialNowMS()

func parseTrialNowMS() int64 {
	raw := os.Getenv("MAKEFUN_TRIAL_NOW")
	if raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t.UnixMilli()
		}
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			if n < 1e12 {
				return n * 1000
			}
			return n
		}
	}
	return epochMS
}

func nowMS() int64 { return trialNowMS }

func tick() int64 {
	GlobalStore.ClockTick++
	return nowMS() + int64(GlobalStore.ClockTick)*1000
}

func iso(ms int64) string {
	var buf [20]byte
	return string(time.UnixMilli(ms).UTC().AppendFormat(buf[:0], "2006-01-02T15:04:05Z"))
}

func isoNow() string { return iso(tick()) }

func gitIdent(name, email string, ms int64) string {
	return fmt.Sprintf("%s <%s> %d +0000", name, email, ms/1000)
}

func safeDecode(text string) string {
	s, err := url.QueryUnescape(text)
	if err != nil {
		return text
	}
	return s
}

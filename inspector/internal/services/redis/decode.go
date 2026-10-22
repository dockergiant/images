package redis

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	LayoutSymfony = "symfony"
	LayoutCm      = "cm"

	symfonyTags     = "cache:tags:"
	symfonyIDTags   = "cache:id_tags:"
	symfonyAllIDs   = "cache:all_ids"
	cmKey           = "zc:k:"
	cmTagIDs        = "zc:ti:"
	cmTags          = "zc:tags"
	cmIDs           = "zc:ids"
	symfonyCompress = "CACHE_COMPRESSION"

	maxPreview  = 64 << 10
	maxHexBytes = 512
	maxInflate  = 64 << 20
)

type Entry struct {
	ID          string         `json:"id"`
	Key         string         `json:"key"`
	Layout      string         `json:"layout"`
	Tags        []string       `json:"tags"`
	MTime       int64          `json:"mtime,omitempty"`
	Expire      int64          `json:"expire,omitempty"`
	Infinite    bool           `json:"infinite"`
	DataForm    string         `json:"dataForm"`
	Compression string         `json:"compression,omitempty"`
	Size        int            `json:"size"`
	DataSize    int            `json:"dataSize"`
	Preview     string         `json:"preview"`
	Truncated   bool           `json:"truncated,omitempty"`
	Link        string         `json:"link,omitempty"`
	URL         string         `json:"url,omitempty"`
	Status      int            `json:"status,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
}

func DetectLayout(keys []string) string {
	cm := false
	for _, k := range keys {
		if strings.HasPrefix(k, symfonyTags) || strings.HasPrefix(k, symfonyIDTags) || k == symfonyAllIDs {
			return LayoutSymfony
		}
		if strings.HasPrefix(k, cmKey) || strings.HasPrefix(k, cmTagIDs) || k == cmTags {
			cm = true
		}
	}
	if cm {
		return LayoutCm
	}
	return ""
}

func DecodeSymfony(raw []byte) (Entry, error) {
	e := Entry{Layout: LayoutSymfony, Size: len(raw), Tags: []string{}}
	if bytes.HasPrefix(raw, []byte("\x00\x00\x00\x02")) {
		e.DataForm = "igbinary"
		e.DataSize = len(raw)
		e.Preview = hexPreview(raw)
		return e, nil
	}
	v, err := phpUnserialize(raw)
	if err != nil {
		return e, fmt.Errorf("not a Symfony cache value: %w", err)
	}
	data := v
	if arr, ok := v.(phpArray); ok {
		if d, ok := arr.Get("data"); ok {
			if _, ok := arr.Get("mtime"); ok {
				data = d
				e.MTime = toInt(arr.Lookup("mtime"))
				e.Expire = toInt(arr.Lookup("expire"))
				if tags, ok := arr.Get("tags"); ok {
					if list, ok := tags.(phpArray); ok {
						for _, kv := range list {
							if s, ok := kv.Value.(string); ok {
								e.Tags = append(e.Tags, s)
							}
						}
					}
				}
			}
		}
	}
	e.Infinite = e.Expire == 0
	s, ok := data.(string)
	if !ok {
		e.DataForm = "php"
		e.DataSize = len(raw)
		e.setPreview(raw)
		return e, nil
	}
	classify(&e, []byte(s))
	return e, nil
}

func DecodeCm(hash map[string][]byte) (Entry, error) {
	d, ok := hash["d"]
	if !ok {
		return Entry{}, errors.New("not a Cm cache entry: field d missing")
	}
	e := Entry{Layout: LayoutCm, Size: len(d), Tags: []string{}}
	for _, t := range strings.Split(string(hash["t"]), ",") {
		if t != "" {
			e.Tags = append(e.Tags, t)
		}
	}
	e.MTime, _ = strconv.ParseInt(string(hash["m"]), 10, 64)
	e.Infinite = string(hash["i"]) == "1"
	classify(&e, d)
	return e, nil
}

var cmLibs = map[string]string{"gz": "gzip", "zc": "gzip", "sn": "snappy", "lz": "lzf", "l4": "lz4", "zs": "zstd"}

var (
	encryptedRe = regexp.MustCompile(`^\d{1,3}:\d:[A-Za-z0-9+/=]+$`)
	linkRe      = regexp.MustCompile(`^l:([0-9a-fA-F]+)$`)
)

func classify(e *Entry, data []byte) {
	switch {
	case bytes.HasPrefix(data, []byte(symfonyCompress)):
		e.Compression = "gzip"
		out, err := inflate(data[len(symfonyCompress):])
		if err != nil {
			e.DataForm = "compressed"
			e.DataSize = len(data)
			return
		}
		data = out
	case len(data) >= 5 && string(data[2:5]) == ":\x1f\x8b":
		lib, ok := cmLibs[string(data[:2])]
		if !ok {
			lib = string(data[:2])
		}
		e.Compression = lib
		if lib != "gzip" {
			e.DataForm = "compressed"
			e.DataSize = len(data)
			return
		}
		out, err := inflate(data[5:])
		if err != nil {
			e.DataForm = "compressed"
			e.DataSize = len(data)
			return
		}
		data = out
	}
	e.DataSize = len(data)

	trimmed := bytes.TrimSpace(data)
	switch {
	case encryptedRe.Match(trimmed):
		e.DataForm = "encrypted"
	case linkRe.Match(trimmed):
		e.DataForm = "link"
		e.Link = string(linkRe.FindSubmatch(trimmed)[1])
		e.setPreview(trimmed)
	case len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid(trimmed):
		e.DataForm = "json"
		if page(e, trimmed) {
			return
		}
		e.setPreview(data)
	case isSerialized(data):
		e.DataForm = "php"
		e.setPreview(data)
	case printable(string(data)) || utf8.Valid(data) && printableText(data):
		e.DataForm = "text"
		e.setPreview(data)
	default:
		e.DataForm = "binary"
		e.Preview = hexPreview(data)
		e.Truncated = len(data) > maxHexBytes
	}
}

func printableText(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			return false
		}
	}
	return true
}

func page(e *Entry, data []byte) bool {
	var p struct {
		Content          *string         `json:"content"`
		StatusCode       int             `json:"status_code"`
		Context          json.RawMessage `json:"context"`
		ContextVariables json.RawMessage `json:"context_variables"`
	}
	if json.Unmarshal(data, &p) != nil || p.Content == nil {
		return false
	}
	e.DataForm = "page"
	e.Status = p.StatusCode
	ctx := p.Context
	if len(ctx) == 0 {
		ctx = p.ContextVariables
	}
	var c map[string]any
	if json.Unmarshal(ctx, &c) == nil {
		if inner, ok := c["data"].(map[string]any); ok {
			c = inner
		}
		e.Context = c
	}
	e.setPreview([]byte(*p.Content))
	return true
}

func (e *Entry) setPreview(b []byte) {
	if len(b) > maxPreview {
		cut := maxPreview
		for cut > 0 && !utf8.RuneStart(b[cut]) {
			cut--
		}
		b = b[:cut]
		e.Truncated = true
	}
	e.Preview = strings.ToValidUTF8(string(b), "�")
}

func hexPreview(b []byte) string {
	if len(b) > maxHexBytes {
		b = b[:maxHexBytes]
	}
	return hex.Dump(b)
}

func inflate(b []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, maxInflate))
	if err != nil {
		return nil, err
	}
	return out, nil
}

func isSerialized(b []byte) bool {
	if len(b) < 2 || !strings.ContainsRune("aibdsNO", rune(b[0])) {
		return false
	}
	_, err := phpUnserialize(b)
	return err == nil
}

func toInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

var idCleaner = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// cleanID mirrors Magento's identifier normalisation (Zend _unifyId and the
// Symfony adapter's cleanIdentifier), so a computed cache id finds its key.
func cleanID(id string) string {
	id = strings.ReplaceAll(strings.ToUpper(id), ".", "__")
	return idCleaner.ReplaceAllString(id, "_")
}

type phpKV struct {
	Key   string
	Value any
}

type phpArray []phpKV

func (a phpArray) Get(key string) (any, bool) {
	for _, kv := range a {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return nil, false
}

func (a phpArray) Lookup(key string) any {
	v, _ := a.Get(key)
	return v
}

func (a phpArray) MarshalJSON() ([]byte, error) {
	list := true
	for i, kv := range a {
		if kv.Key != strconv.Itoa(i) {
			list = false
			break
		}
	}
	var b bytes.Buffer
	if list {
		b.WriteByte('[')
	} else {
		b.WriteByte('{')
	}
	for i, kv := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		if !list {
			k, _ := json.Marshal(kv.Key)
			b.Write(k)
			b.WriteByte(':')
		}
		v, err := json.Marshal(kv.Value)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	if list {
		b.WriteByte(']')
	} else {
		b.WriteByte('}')
	}
	return b.Bytes(), nil
}

var errPHP = errors.New("invalid PHP serialize data")

const maxPHPDepth = 64

func phpUnserialize(b []byte) (any, error) {
	p := &phpParser{b: b}
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	if p.i != len(p.b) {
		return nil, errPHP
	}
	return v, nil
}

type phpParser struct {
	b []byte
	i int
}

func (p *phpParser) until(c byte) (string, error) {
	j := bytes.IndexByte(p.b[p.i:], c)
	if j < 0 {
		return "", errPHP
	}
	s := string(p.b[p.i : p.i+j])
	p.i += j + 1
	return s, nil
}

func (p *phpParser) expect(s string) error {
	if !bytes.HasPrefix(p.b[p.i:], []byte(s)) {
		return errPHP
	}
	p.i += len(s)
	return nil
}

func (p *phpParser) value(depth int) (any, error) {
	if depth > maxPHPDepth || p.i+1 >= len(p.b) {
		return nil, errPHP
	}
	t := p.b[p.i]
	if t == 'N' {
		return nil, p.expect("N;")
	}
	if p.b[p.i+1] != ':' {
		return nil, errPHP
	}
	p.i += 2
	switch t {
	case 'b':
		s, err := p.until(';')
		if err != nil || s != "0" && s != "1" {
			return nil, errPHP
		}
		return s == "1", nil
	case 'i':
		s, err := p.until(';')
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, errPHP
		}
		return n, nil
	case 'd':
		s, err := p.until(';')
		if err != nil {
			return nil, err
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, errPHP
		}
		return f, nil
	case 's':
		s, err := p.str()
		if err != nil {
			return nil, err
		}
		return s, p.expect(";")
	case 'a':
		s, err := p.until(':')
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > len(p.b) {
			return nil, errPHP
		}
		if err := p.expect("{"); err != nil {
			return nil, err
		}
		arr := make(phpArray, 0, n)
		for k := 0; k < n; k++ {
			key, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			var ks string
			switch kv := key.(type) {
			case int64:
				ks = strconv.FormatInt(kv, 10)
			case string:
				ks = kv
			default:
				return nil, errPHP
			}
			val, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, phpKV{Key: ks, Value: val})
		}
		return arr, p.expect("}")
	}
	return nil, errPHP
}

func (p *phpParser) str() (string, error) {
	s, err := p.until(':')
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || p.i+n+2 > len(p.b) {
		return "", errPHP
	}
	if p.b[p.i] != '"' || p.b[p.i+1+n] != '"' {
		return "", errPHP
	}
	out := string(p.b[p.i+1 : p.i+1+n])
	p.i += n + 2
	return out, nil
}

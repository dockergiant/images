package varnish

import (
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

type statCounter struct {
	Flag   string          `json:"flag"`
	Format string          `json:"format"`
	Value  json.RawMessage `json:"value"`
}

func ParseStat(data []byte) (map[string]uint64, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	body := top
	if raw, ok := top["counters"]; ok {
		body = nil
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
	}
	if body == nil {
		return nil, errors.New("varnishstat: no counters")
	}
	out := make(map[string]uint64, len(body))
	for name, raw := range body {
		if len(raw) == 0 || raw[0] != '{' {
			continue
		}
		var c statCounter
		if err := json.Unmarshal(raw, &c); err != nil || c.Value == nil {
			continue
		}
		if c.Format == "b" {
			continue
		}
		v, err := strconv.ParseUint(string(c.Value), 10, 64)
		if err != nil {
			f, ferr := strconv.ParseFloat(string(c.Value), 64)
			if ferr != nil || f < 0 {
				continue
			}
			v = uint64(f)
		}
		out[name] = v
	}
	return out, nil
}

func Rates(prev, cur map[string]uint64, dt time.Duration, names ...string) map[string]float64 {
	out := map[string]float64{}
	if dt <= 0 {
		return out
	}
	for _, n := range names {
		a, ok1 := prev[n]
		b, ok2 := cur[n]
		if !ok1 || !ok2 {
			continue
		}
		if b < a {
			out[n] = 0
			continue
		}
		out[n] = float64(b-a) / dt.Seconds()
	}
	return out
}

func delta(prev, cur map[string]uint64, name string) uint64 {
	a, b := prev[name], cur[name]
	if b < a {
		return b
	}
	return b - a
}

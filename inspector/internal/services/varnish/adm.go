package varnish

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type AdmEnvelope struct {
	Version int
	Argv    []string
	Time    time.Time
	Payload []json.RawMessage
}

func ParseAdm(data []byte) (AdmEnvelope, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err != nil {
		return AdmEnvelope{}, fmt.Errorf("varnishadm -j: %w", err)
	}
	if len(arr) < 3 {
		return AdmEnvelope{}, errors.New("varnishadm -j: short reply")
	}
	var env AdmEnvelope
	if err := json.Unmarshal(arr[0], &env.Version); err != nil {
		return AdmEnvelope{}, fmt.Errorf("varnishadm -j version: %w", err)
	}
	if err := json.Unmarshal(arr[1], &env.Argv); err != nil {
		return AdmEnvelope{}, fmt.Errorf("varnishadm -j argv: %w", err)
	}
	var ts float64
	if err := json.Unmarshal(arr[2], &ts); err != nil {
		return AdmEnvelope{}, fmt.Errorf("varnishadm -j time: %w", err)
	}
	env.Time = floatTime(ts)
	env.Payload = arr[3:]
	return env, nil
}

type Ban struct {
	Time      time.Time `json:"time"`
	Refs      int       `json:"refs"`
	Completed bool      `json:"completed"`
	Spec      string    `json:"spec"`
}

func ParseBans(data []byte) ([]Ban, error) {
	env, err := ParseAdm(data)
	if err != nil {
		return nil, err
	}
	out := make([]Ban, 0, len(env.Payload))
	for _, raw := range env.Payload {
		var b struct {
			Time      float64 `json:"time"`
			Refs      int     `json:"refs"`
			Completed bool    `json:"completed"`
			Spec      string  `json:"spec"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("ban.list: %w", err)
		}
		out = append(out, Ban{Time: floatTime(b.Time), Refs: b.Refs, Completed: b.Completed, Spec: b.Spec})
	}
	return out, nil
}

type VCL struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	State       string `json:"state"`
	Temperature string `json:"temperature"`
	Busy        int    `json:"busy"`
}

func ParseVCLs(data []byte) ([]VCL, error) {
	env, err := ParseAdm(data)
	if err != nil {
		return nil, err
	}
	out := make([]VCL, 0, len(env.Payload))
	for _, raw := range env.Payload {
		var v VCL
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("vcl.list: %w", err)
		}
		out = append(out, v)
	}
	return out, nil
}

type Backend struct {
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	AdminHealth string    `json:"adminHealth"`
	Probe       string    `json:"probe"`
	Good        int       `json:"good"`
	Window      int       `json:"window"`
	LastChange  time.Time `json:"lastChange"`
}

func (b Backend) Healthy() bool {
	if b.AdminHealth == "sick" {
		return false
	}
	return b.Probe != "sick"
}

type backendJSON struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	AdminHealth  string            `json:"admin_health"`
	ProbeMessage []json.RawMessage `json:"probe_message"`
	LastChange   float64           `json:"last_change"`
}

func (j backendJSON) backend(name string) Backend {
	b := Backend{Name: name, Type: j.Type, AdminHealth: j.AdminHealth, LastChange: floatTime(j.LastChange)}
	if len(j.ProbeMessage) == 3 {
		_ = json.Unmarshal(j.ProbeMessage[0], &b.Good)
		_ = json.Unmarshal(j.ProbeMessage[1], &b.Window)
		_ = json.Unmarshal(j.ProbeMessage[2], &b.Probe)
	}
	return b
}

func ParseBackends(data []byte) ([]Backend, error) {
	env, err := ParseAdm(data)
	if err != nil {
		return nil, err
	}
	var out []Backend
	for _, raw := range env.Payload {
		var named backendJSON
		if err := json.Unmarshal(raw, &named); err == nil && named.Name != "" {
			out = append(out, named.backend(named.Name))
			continue
		}
		var byName map[string]backendJSON
		if err := json.Unmarshal(raw, &byName); err != nil {
			return nil, fmt.Errorf("backend.list: %w", err)
		}
		for name, j := range byName {
			out = append(out, j.backend(name))
		}
	}
	slices.SortFunc(out, func(a, b Backend) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

type Storage struct {
	Name    string `json:"name"`
	Storage string `json:"storage"`
}

func ParseStorages(data []byte) ([]Storage, error) {
	env, err := ParseAdm(data)
	if err != nil {
		return nil, err
	}
	out := make([]Storage, 0, len(env.Payload))
	for _, raw := range env.Payload {
		var s Storage
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("storage.list: %w", err)
		}
		out = append(out, s)
	}
	return out, nil
}

type Param struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Units       string `json:"units,omitempty"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

func ParseParams(data []byte) ([]Param, error) {
	env, err := ParseAdm(data)
	if err != nil {
		return nil, err
	}
	out := make([]Param, 0, len(env.Payload))
	for _, raw := range env.Payload {
		var p struct {
			Name        string          `json:"name"`
			Value       json.RawMessage `json:"value"`
			Units       string          `json:"units"`
			Default     json.RawMessage `json:"default"`
			Description string          `json:"description"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("param.show: %w", err)
		}
		out = append(out, Param{Name: p.Name, Value: jsonScalar(p.Value), Units: p.Units, Default: jsonScalar(p.Default), Description: p.Description})
	}
	return out, nil
}

func jsonScalar(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

type VCLFile struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

var vclShowHeader = regexp.MustCompile(`(?m)^// VCL\.SHOW (\d+) (\d+) (.*)\n`)

func SplitVCLShow(text string) []VCLFile {
	locs := vclShowHeader.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		return []VCLFile{{Source: text}}
	}
	out := make([]VCLFile, 0, len(locs))
	for i, loc := range locs {
		start := loc[1]
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if n, err := strconv.Atoi(text[loc[4]:loc[5]]); err == nil && start+n <= end {
			end = start + n
		}
		out = append(out, VCLFile{Path: text[loc[6]:loc[7]], Source: text[start:end]})
	}
	return out
}

func panicState(exit int, stdout, stderr string) (bool, string) {
	text := strings.TrimSpace(stdout)
	if exit != 0 && strings.Contains(stderr+stdout, "error code 300") {
		return false, text
	}
	return text != "", text
}

package procmap

import (
	"bufio"
	"encoding/hex"
	"errors"
	"io"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	stateEstablished uint8 = 0x01
	stateListen      uint8 = 0x0A
)

type Socket struct {
	Local  netip.AddrPort
	Remote netip.AddrPort
	State  uint8
	Inode  uint64
	PIDs   []int
}

type Walk struct {
	Procs   map[int]string
	Sockets []Socket
}

func ParseWalk(r io.Reader) (Walk, error) {
	w := Walk{Procs: map[int]string{}}
	owners := map[uint64][]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		tag, rest, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		switch tag {
		case "T":
			if s, ok := parseTCP(rest); ok {
				w.Sockets = append(w.Sockets, s)
			}
		case "P":
			pid, cmd, _ := strings.Cut(rest, " ")
			if n, err := strconv.Atoi(pid); err == nil && n > 0 {
				w.Procs[n] = strings.TrimSpace(cmd)
			}
		case "F":
			f := strings.Fields(rest)
			if len(f) != 2 {
				continue
			}
			pid, err1 := strconv.Atoi(f[0])
			inode, err2 := strconv.ParseUint(f[1], 10, 64)
			if err1 == nil && err2 == nil && pid > 0 && inode > 0 {
				owners[inode] = append(owners[inode], pid)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return w, err
	}
	for i := range w.Sockets {
		pids := owners[w.Sockets[i].Inode]
		slices.Sort(pids)
		w.Sockets[i].PIDs = slices.Compact(pids)
	}
	return w, nil
}

func parseTCP(line string) (Socket, bool) {
	f := strings.Fields(line)
	if len(f) < 10 || !strings.HasSuffix(f[0], ":") {
		return Socket{}, false
	}
	local, err := decodeAddr(f[1])
	if err != nil {
		return Socket{}, false
	}
	remote, err := decodeAddr(f[2])
	if err != nil {
		return Socket{}, false
	}
	state, err := strconv.ParseUint(f[3], 16, 8)
	if err != nil {
		return Socket{}, false
	}
	inode, err := strconv.ParseUint(f[9], 10, 64)
	if err != nil || inode == 0 {
		return Socket{}, false
	}
	return Socket{Local: local, Remote: remote, State: uint8(state), Inode: inode}, true
}

// The kernel prints each 32-bit word of the address in host byte order;
// every architecture the image ships for (amd64, arm64) is little-endian.
func decodeAddr(s string) (netip.AddrPort, error) {
	h, p, ok := strings.Cut(s, ":")
	if !ok || len(p) != 4 || (len(h) != 8 && len(h) != 32) {
		return netip.AddrPort{}, errors.New("bad address " + strconv.Quote(s))
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return netip.AddrPort{}, err
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	var addr netip.Addr
	if len(raw) == 4 {
		addr = netip.AddrFrom4([4]byte(raw))
	} else {
		addr = netip.AddrFrom16([16]byte(raw)).Unmap()
	}
	return netip.AddrPortFrom(addr, uint16(port)), nil
}

func Role(cmd string) string {
	switch {
	case strings.HasPrefix(cmd, "php-fpm: pool"):
		return "fpm-worker"
	case strings.HasPrefix(cmd, "php-fpm: master"):
		return "fpm-master"
	case strings.Contains(cmd, "queue:consumers:start"):
		return "consumer"
	case strings.Contains(cmd, "cron:run"):
		return "cron"
	}
	return "cli"
}

var secretFlag = regexp.MustCompile(`(?i)(^|[_-])(password|pwd|passwd|secret|token|key|apikey|signature|cert)([_-]|$)`)

// The command line reaches the browser in the Redis and RabbitMQ tabs, and
// bin/magento setup:install or a mysql client carries credentials in argv.
func scrub(cmd string) string {
	f := strings.Fields(cmd)
	changed := false
	for i := 0; i < len(f); i++ {
		tok := f[i]
		if !strings.HasPrefix(tok, "--") {
			if strings.HasPrefix(tok, "-p") && len(tok) > 2 {
				f[i], changed = "-p***", true
			}
			continue
		}
		name, _, hasValue := strings.Cut(tok[2:], "=")
		if !secretFlag.MatchString(name) {
			continue
		}
		switch {
		case hasValue:
			f[i], changed = "--"+name+"=***", true
		case i+1 < len(f) && !strings.HasPrefix(f[i+1], "-"):
			f[i+1], changed = "***", true
			i++
		}
	}
	if !changed {
		return cmd
	}
	return strings.Join(f, " ")
}
